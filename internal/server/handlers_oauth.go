package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// oauthCallbackPath is where providers send the browser back to. It is part of
// the redirect URI registered with each provider, so it is fixed.
const oauthCallbackPath = "/api/providers/oauth/callback"

// oauthRedirectEnv pins the redirect URI for deployments where the address the
// browser uses is not the address this process sees — behind a reverse proxy,
// or where a provider will only accept one fixed URI.
const oauthRedirectEnv = "SAND_OAUTH_REDIRECT"

// handleOAuthStart opens a sign-in: it works out which app credentials to use,
// remembers the secrets that will tie the redirect back to this request, and
// hands the browser the URL to send the account holder to.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind         provider.Kind `json:"kind"`
		ClientID     string        `json:"client_id"`
		ClientSecret string        `json:"client_secret"`
		RedirectURI  string        `json:"redirect_uri"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	spec, ok := provider.SpecFor(req.Kind)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider kind %q", req.Kind), "BAD_REQUEST")
		return
	}
	if spec.OAuth == nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s is not connected by signing in", spec.Label), "BAD_REQUEST")
		return
	}

	// What the user typed wins over what the deployment was started with, so a
	// second Google account with its own OAuth app is still possible.
	clientID, clientSecret := spec.OAuth.EnvClient()
	if v := strings.TrimSpace(req.ClientID); v != "" {
		clientID, clientSecret = v, strings.TrimSpace(req.ClientSecret)
	}
	if clientID == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"no %s app is configured — create one and paste its client ID, or start SAND with %s set",
			spec.Label, spec.OAuth.ClientIDEnv), "OAUTH_NO_CLIENT")
		return
	}
	if spec.OAuth.SecretRequired && clientSecret == "" {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s also needs the app's client secret", spec.Label), "OAUTH_NO_CLIENT")
		return
	}

	redirectURI, err := oauthRedirectURI(r, req.RedirectURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	var verifier, challenge string
	if spec.OAuth.PKCE {
		if verifier, challenge, err = provider.NewPKCE(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL")
			return
		}
	}

	flow, err := s.oauthFlows.start(&oauthFlow{
		Kind:         spec.Kind,
		Verifier:     verifier,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
		Session:      sessionToken(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":      flow.ID,
		"auth_url":     spec.OAuth.AuthCodeURL(clientID, redirectURI, flow.State, challenge),
		"redirect_uri": redirectURI,
		"expires_in":   int(oauthFlowTTL.Seconds()),
	})
}

// handleOAuthCallback is where the provider sends the browser back to. It runs
// without a session: this is a cross-site navigation, so the SameSite=Strict
// cookie is not sent, and the unguessable state parameter is what proves the
// redirect belongs to a sign-in this server started.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	flow, ok := s.oauthFlows.byState(query.Get("state"))
	if !ok {
		// Either it expired while the user was signing in, or it is not ours.
		renderOAuthPage(w, http.StatusBadRequest, "", false,
			"This sign-in has expired. Start it again from SAND.")
		return
	}

	if denied := query.Get("error"); denied != "" {
		message := query.Get("error_description")
		if message == "" {
			message = denied
		}
		s.oauthFlows.finish(flow.ID, nil, "", message)
		renderOAuthPage(w, http.StatusOK, flow.ID, false, message)
		return
	}

	code := query.Get("code")
	if code == "" {
		s.oauthFlows.finish(flow.ID, nil, "", "the provider sent no authorization code")
		renderOAuthPage(w, http.StatusBadRequest, flow.ID, false,
			"The provider sent no authorization code.")
		return
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	if err := s.completeOAuthFlow(ctx, flow, code); err != nil {
		renderOAuthPage(w, http.StatusOK, flow.ID, false, err.Error())
		return
	}
	renderOAuthPage(w, http.StatusOK, flow.ID, true, "")
}

// handleOAuthExchange is the way back in when the redirect cannot reach this
// server — signing in from a phone against a vault bound to localhost, say.
// The user pastes the URL the browser was left on and the exchange happens
// from here instead.
func (s *Server) handleOAuthExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string `json:"flow_id"`
		URL    string `json:"url"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	flow, ok := s.oauthFlows.byID(req.FlowID)
	if !ok || !sameSecret(flow.Session, sessionToken(r)) {
		writeError(w, http.StatusNotFound, "that sign-in is no longer in progress", "NOT_FOUND")
		return
	}

	parsed, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		writeError(w, http.StatusBadRequest, "that does not look like a URL", "BAD_REQUEST")
		return
	}
	query := parsed.Query()
	if denied := query.Get("error"); denied != "" {
		writeError(w, http.StatusBadRequest, denied, "OAUTH_DENIED")
		return
	}
	if !sameSecret(flow.State, query.Get("state")) {
		writeError(w, http.StatusBadRequest,
			"that URL belongs to a different sign-in — start again", "OAUTH_STATE")
		return
	}
	code := query.Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest,
			"that URL has no authorization code in it", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	if err := s.completeOAuthFlow(ctx, flow, code); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "OAUTH_FAILED")
		return
	}
	s.writeOAuthStatus(w, flow.ID)
}

// completeOAuthFlow turns an authorization code into stored credentials and
// asks the account who it belongs to, so the connection can name itself.
func (s *Server) completeOAuthFlow(ctx context.Context, flow *oauthFlow, code string) error {
	spec, ok := provider.SpecFor(flow.Kind)
	if !ok || spec.OAuth == nil {
		err := fmt.Errorf("unknown provider kind %q", flow.Kind)
		s.oauthFlows.finish(flow.ID, nil, "", err.Error())
		return err
	}

	tokens, err := spec.OAuth.Exchange(ctx, flow.ClientID, flow.ClientSecret,
		code, flow.RedirectURI, flow.Verifier)
	if err != nil {
		s.oauthFlows.finish(flow.ID, nil, "", err.Error())
		return err
	}

	options := spec.OAuth.Options(flow.ClientID, flow.ClientSecret, tokens)
	account, rotated := probeAccount(ctx, spec.Kind, options)
	for key, value := range rotated {
		options[key] = value
	}

	s.oauthFlows.finish(flow.ID, options, account, "")
	return nil
}

// probeAccount asks a backend whose account it is now holding credentials for,
// and reports back anything the backend rotated while answering.
//
// That second return matters more than it looks: Box and Microsoft retire a
// refresh token the moment it is spent, so this one call can invalidate the
// token that just came out of the exchange. Storing the pre-probe value would
// connect an account that is already dead.
//
// A backend that cannot name its account, or one that will not answer right
// now, simply goes unnamed — the connection still works and the user can type
// a name.
func probeAccount(ctx context.Context, kind provider.Kind, options map[string]string) (string, map[string]string) {
	p, err := provider.New(provider.Config{Kind: kind, Options: options})
	if err != nil {
		return "", nil
	}

	var mu sync.Mutex
	rotated := map[string]string{}
	if rotator, ok := p.(provider.CredentialRotator); ok {
		rotator.OnCredentialChange(func(updates map[string]string) {
			mu.Lock()
			defer mu.Unlock()
			for key, value := range updates {
				rotated[key] = value
			}
		})
	}

	identifier, ok := p.(provider.Identifier)
	if !ok {
		return "", nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	account, probeErr := identifier.Account(probeCtx)

	mu.Lock()
	defer mu.Unlock()
	if probeErr != nil {
		account = ""
	}
	return account, rotated
}

// handleOAuthStatus reports where a sign-in has got to. The browser polls it
// while the provider's window is open, which is also what makes the flow
// survive a popup blocker sending the user through a full page redirect.
func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.oauthFlows.byID(r.PathValue("id"))
	if !ok || !sameSecret(flow.Session, sessionToken(r)) {
		writeError(w, http.StatusNotFound, "that sign-in is no longer in progress", "NOT_FOUND")
		return
	}
	s.writeOAuthStatus(w, flow.ID)
}

// writeOAuthStatus answers with a flow's current state.
func (s *Server) writeOAuthStatus(w http.ResponseWriter, id string) {
	flow, ok := s.oauthFlows.byID(id)
	if !ok {
		writeError(w, http.StatusNotFound, "that sign-in is no longer in progress", "NOT_FOUND")
		return
	}

	status := "pending"
	switch {
	case flow.Done && flow.Err != "":
		status = "error"
	case flow.Done:
		status = "ready"
	}

	payload := map[string]any{
		"flow_id": flow.ID,
		"kind":    flow.Kind,
		"status":  status,
		"account": flow.Account,
	}
	if flow.Err != "" {
		payload["error"] = flow.Err
	}
	if status == "ready" {
		payload["suggested_name"] = s.suggestAccountName(flow)
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleOAuthComplete turns a finished sign-in into a connected account.
func (s *Server) handleOAuthComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string            `json:"flow_id"`
		Name    string            `json:"name"`
		Options map[string]string `json:"options"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	flow, ok := s.oauthFlows.byID(req.FlowID)
	if !ok || !sameSecret(flow.Session, sessionToken(r)) {
		writeError(w, http.StatusNotFound, "that sign-in is no longer in progress", "NOT_FOUND")
		return
	}
	if !flow.Done || flow.Err != "" {
		writeError(w, http.StatusConflict, "that sign-in has not finished", "OAUTH_PENDING")
		return
	}

	// Settings the user chose in the form — a folder, usually — plus the
	// credentials the sign-in produced. Credentials win: nothing typed into
	// the form may overwrite them.
	options := map[string]string{}
	for key, value := range req.Options {
		options[key] = value
	}
	for key, value := range flow.Options {
		options[key] = value
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = s.suggestAccountName(flow)
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	v, _ := s.Vault()
	cfg, err := v.AddProvider(ctx, provider.Config{
		Kind:    flow.Kind,
		Name:    name,
		Options: options,
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	s.oauthFlows.drop(flow.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"provider": cfg})
}

// suggestAccountName names a connection after the account it points at, and
// keeps clear of the names already taken.
func (s *Server) suggestAccountName(flow *oauthFlow) string {
	label := string(flow.Kind)
	if spec, ok := provider.SpecFor(flow.Kind); ok {
		label = spec.Label
	}
	base := label
	if flow.Account != "" {
		base = flow.Account
	}

	taken := map[string]bool{}
	if v, err := s.Vault(); err == nil {
		if accounts, err := v.Providers(); err == nil {
			for _, cfg := range accounts {
				taken[strings.ToLower(cfg.Name)] = true
			}
		}
	}

	candidate := base
	for i := 2; taken[strings.ToLower(candidate)]; i++ {
		candidate = fmt.Sprintf("%s (%d)", base, i)
	}
	return candidate
}

// oauthRedirectURI works out where the provider should send the browser back
// to. A deployment can pin it, the browser can propose the origin it is
// actually being used from, and failing both it is derived from this request.
func oauthRedirectURI(r *http.Request, requested string) (string, error) {
	if pinned := strings.TrimSpace(os.Getenv(oauthRedirectEnv)); pinned != "" {
		return pinned, nil
	}

	if requested = strings.TrimSpace(requested); requested != "" {
		u, err := url.Parse(requested)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("invalid redirect URI %q", requested)
		}
		// Only ever our own callback: this URI is what the provider will send
		// an authorization code to.
		u.Path = oauthCallbackPath
		u.RawQuery = ""
		u.Fragment = ""
		return u.String(), nil
	}

	scheme := "http"
	if isSecureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + oauthCallbackPath, nil
}

// oauthPage is what the provider's window lands on: it hands the result back
// to the app that opened it and gets out of the way. Everything is inline —
// the page makes no requests of its own.
var oauthPage = template.Must(template.New("oauth").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SAND Vault — {{if .OK}}connected{{else}}sign-in failed{{end}}</title>
<style>
  body { margin: 0; min-height: 100vh; display: flex; align-items: center;
         justify-content: center; background: #0a0e17; color: #e2e8f0;
         font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; padding: 24px; }
  .card { max-width: 420px; text-align: center; }
  .mark { font-size: 34px; margin-bottom: 14px; color: {{if .OK}}#22c55e{{else}}#ef4444{{end}}; }
  h1 { font-size: 15px; letter-spacing: 1px; margin: 0 0 10px; }
  p { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
      font-size: 13px; line-height: 1.6; color: #94a3b8; margin: 0 0 16px; word-break: break-word; }
  a { color: #f59e0b; font-size: 12px; }
</style>
</head>
<body>
  <div class="card">
    <div class="mark">{{if .OK}}✓{{else}}✗{{end}}</div>
    <h1>{{if .OK}}ACCOUNT AUTHORIZED{{else}}SIGN-IN FAILED{{end}}</h1>
    <p>{{if .OK}}You can close this window — SAND is finishing up.{{else}}{{.Message}}{{end}}</p>
    <a href="/">Return to SAND Vault</a>
  </div>
<script>
  (function () {
    var result = { source: "sand-oauth", flow: {{.FlowID}}, ok: {{.OK}} };
    if (window.opener) {
      try { window.opener.postMessage(result, window.location.origin) } catch (e) {}
      window.setTimeout(function () { window.close() }, {{.CloseDelay}});
      return;
    }
    // Not a popup: the sign-in took over the tab, so hand it back to the app,
    // which picks the flow up again from where it left off.
    window.setTimeout(function () { window.location.replace("/") }, {{.CloseDelay}});
  })();
</script>
</body>
</html>`))

// renderOAuthPage writes the callback page.
func renderOAuthPage(w http.ResponseWriter, status int, flowID string, ok bool, message string) {
	// A failure is left on screen long enough to read; a success is over.
	closeDelay := 400
	if !ok {
		closeDelay = 6000
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	_ = oauthPage.Execute(w, struct {
		OK         bool
		FlowID     string
		Message    string
		CloseDelay int
	}{OK: ok, FlowID: flowID, Message: message, CloseDelay: closeDelay})
}
