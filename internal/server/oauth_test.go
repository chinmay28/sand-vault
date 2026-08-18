package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// stubBackend is a provider kind that exists only for these tests: it speaks
// the OAuth flow against a local token endpoint and stores nothing.
type stubBackend struct{ cfg provider.Config }

func (s stubBackend) Config() provider.Config { return s.cfg }
func (s stubBackend) Put(context.Context, string, []byte) error {
	return nil
}
func (s stubBackend) Get(context.Context, string) ([]byte, error) {
	return nil, provider.ErrNotFound
}
func (s stubBackend) Stat(context.Context, string) (provider.ObjectInfo, error) {
	return provider.ObjectInfo{}, provider.ErrNotFound
}
func (s stubBackend) Delete(context.Context, string) error { return nil }
func (s stubBackend) List(context.Context, string) ([]provider.ObjectInfo, error) {
	return nil, nil
}
func (s stubBackend) Ping(context.Context) error { return nil }
func (s stubBackend) Account(context.Context) (string, error) {
	return "alice@example.test", nil
}

// registerStubBackend wires a sign-in backend up to a stub token endpoint and
// returns its kind. The registry is process-wide, so the kind is named for
// tests and simply re-registered by each one that needs it.
func registerStubBackend(t *testing.T, tokenURL string) provider.Kind {
	t.Helper()

	kind := provider.Kind("stub-signin")
	provider.Register(provider.Spec{
		Kind:        kind,
		Label:       "Stub Cloud",
		Description: "A backend that exists only in tests.",
		Fields: []provider.FieldSpec{
			{Key: "client_id", Label: "Client ID", Required: true, Advanced: true},
			{Key: "client_secret", Label: "Client secret", Secret: true, Advanced: true},
			{Key: "refresh_token", Label: "Refresh token", Secret: true, Required: true, Advanced: true},
			{Key: "folder", Label: "Folder", Default: "sand"},
		},
		OAuth: &provider.OAuthSpec{
			SignInLabel:       "Continue with Stub",
			AuthURL:           "https://stub.example.test/authorize",
			TokenURL:          tokenURL,
			Scopes:            []string{"files"},
			PKCE:              true,
			ClientIDField:     "client_id",
			ClientSecretField: "client_secret",
			RefreshTokenField: "refresh_token",
			ClientIDEnv:       "SAND_STUB_CLIENT_ID",
			ClientSecretEnv:   "SAND_STUB_CLIENT_SECRET",
		},
	}, func(cfg provider.Config) (provider.Provider, error) {
		return stubBackend{cfg: cfg}, nil
	})
	return kind
}

// stubTokenEndpoint stands in for the provider's token endpoint and records
// what was sent to it.
func stubTokenEndpoint(t *testing.T) (*httptest.Server, *url.Values) {
	t.Helper()

	got := &url.Values{}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		*got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt-stored","expires_in":3600}`))
	}))
	t.Cleanup(stub.Close)
	return stub, got
}

// stateFrom pulls the state parameter back out of an authorize URL.
func stateFrom(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}
	return u.Query().Get("state")
}

// redirect drives the provider's callback the way a browser would: a plain GET
// with no session cookie, because the redirect is a cross-site navigation.
func (c *testClient) redirect(query string) *httptest.ResponseRecorder {
	c.t.Helper()

	req := httptest.NewRequest(http.MethodGet, oauthCallbackPath+"?"+query, nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

func TestSignInConnectsAnAccountWithoutLeavingTheApp(t *testing.T) {
	tokenStub, sentToToken := stubTokenEndpoint(t)
	kind := registerStubBackend(t, tokenStub.URL)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	w, start := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{
		"kind":          kind,
		"client_id":     "client-abc",
		"client_secret": "secret-abc",
		"redirect_uri":  "http://example.test/somewhere",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %v", w.Code, start)
	}

	// Whatever the browser proposed, the redirect only ever points at our own
	// callback.
	if got := start["redirect_uri"]; got != "http://example.test"+oauthCallbackPath {
		t.Errorf("redirect_uri = %v", got)
	}
	authURL, _ := start["auth_url"].(string)
	if !strings.HasPrefix(authURL, "https://stub.example.test/authorize?") {
		t.Fatalf("auth_url = %q", authURL)
	}
	if !strings.Contains(authURL, "code_challenge=") {
		t.Error("the authorize URL should carry a PKCE challenge")
	}
	flowID, _ := start["flow_id"].(string)

	// Nothing has come back yet.
	_, pending := c.json(http.MethodGet, "/api/providers/oauth/"+flowID, nil)
	if pending["status"] != "pending" {
		t.Errorf("status before the redirect = %v", pending["status"])
	}

	page := c.redirect(url.Values{
		"code":  {"the-code"},
		"state": {stateFrom(t, authURL)},
	}.Encode())
	if page.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "ACCOUNT AUTHORIZED") {
		t.Errorf("callback page did not report success: %s", page.Body.String())
	}

	if sentToToken.Get("code") != "the-code" {
		t.Errorf("token exchange sent code %q", sentToToken.Get("code"))
	}
	if sentToToken.Get("code_verifier") == "" {
		t.Error("the PKCE verifier never reached the token endpoint")
	}
	if sentToToken.Get("redirect_uri") != "http://example.test"+oauthCallbackPath {
		t.Errorf("token exchange sent redirect_uri %q", sentToToken.Get("redirect_uri"))
	}

	_, ready := c.json(http.MethodGet, "/api/providers/oauth/"+flowID, nil)
	if ready["status"] != "ready" {
		t.Fatalf("status after the redirect = %v", ready)
	}
	if ready["account"] != "alice@example.test" {
		t.Errorf("account = %v, want the signed-in identity", ready["account"])
	}
	if ready["suggested_name"] != "alice@example.test" {
		t.Errorf("suggested_name = %v", ready["suggested_name"])
	}

	w, created := c.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": flowID,
		"name":    "",
		"options": map[string]string{"folder": "shards"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("complete: %d %v", w.Code, created)
	}

	connected, _ := created["provider"].(map[string]any)
	if connected["name"] != "alice@example.test" {
		t.Errorf("the account named itself %v", connected["name"])
	}
	options, _ := connected["options"].(map[string]any)
	if options["folder"] != "shards" {
		t.Errorf("the folder chosen in the form was dropped: %v", options)
	}
	if options["refresh_token"] == "rt-stored" {
		t.Error("the refresh token was handed back to the browser unredacted")
	}

	// The flow is spent.
	w, _ = c.json(http.MethodGet, "/api/providers/oauth/"+flowID, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("a completed flow is still readable: %d", w.Code)
	}

	// And the vault really holds it, with the token the exchange produced.
	_, list := c.json(http.MethodGet, "/api/providers", nil)
	providers, _ := list["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("connected accounts = %d, want 1", len(providers))
	}
}

// TestSignInStoresACredentialRotatedWhileNamingTheAccount covers a way to
// connect an account that is already dead: Box and Microsoft retire a refresh
// token as it is spent, and asking the account its name spends one.
func TestSignInStoresACredentialRotatedWhileNamingTheAccount(t *testing.T) {
	tokenStub, _ := stubTokenEndpoint(t)
	kind := provider.Kind("stub-rotating")

	provider.Register(provider.Spec{
		Kind:        kind,
		Label:       "Rotating Cloud",
		Description: "A backend that retires its refresh token as it is spent.",
		Fields: []provider.FieldSpec{
			{Key: "client_id", Label: "Client ID", Required: true, Advanced: true},
			{Key: "refresh_token", Label: "Refresh token", Secret: true, Required: true, Advanced: true},
		},
		OAuth: &provider.OAuthSpec{
			SignInLabel:       "Continue with Rotating",
			AuthURL:           "https://rotating.example.test/authorize",
			TokenURL:          tokenStub.URL,
			ClientIDField:     "client_id",
			RefreshTokenField: "refresh_token",
			ClientIDEnv:       "SAND_ROTATING_CLIENT_ID",
		},
	}, func(cfg provider.Config) (provider.Provider, error) {
		built.record(cfg)
		return &rotatingBackend{cfg: cfg}, nil
	})

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	_, start := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{
		"kind": kind, "client_id": "client-abc",
	})
	flowID := start["flow_id"].(string)
	c.redirect("code=the-code&state=" + url.QueryEscape(stateFrom(t, start["auth_url"].(string))))

	w, created := c.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": flowID, "name": "rotating", "options": map[string]string{},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("complete: %d %v", w.Code, created)
	}

	// The account is built twice: once to ask its name, and once by the vault
	// as it connects. The second build is the one that gets stored, and it has
	// to carry the token the backend rotated to — not the one the exchange
	// produced and the identity probe then spent.
	if got := built.last(); got != "rt-rotated" {
		t.Errorf("the connected account carries refresh token %q, want the rotated one", got)
	}
}

// builtConfigs records the configs a backend was constructed from, which is
// how a test sees what the vault actually stored: the config itself is
// redacted on its way back out over HTTP.
type builtConfigs struct {
	mu      sync.Mutex
	configs []provider.Config
}

func (b *builtConfigs) record(cfg provider.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configs = append(b.configs, cfg)
}

func (b *builtConfigs) last() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.configs) == 0 {
		return ""
	}
	return b.configs[len(b.configs)-1].Option("refresh_token")
}

var built = &builtConfigs{}

// rotatingBackend hands back a new refresh token the first time it is asked
// anything, the way Box does.
type rotatingBackend struct {
	stubBackend
	cfg provider.Config

	sink func(map[string]string)
}

func (r *rotatingBackend) Config() provider.Config { return r.cfg }
func (r *rotatingBackend) OnCredentialChange(fn func(map[string]string)) {
	r.sink = fn
}
func (r *rotatingBackend) Account(context.Context) (string, error) {
	if r.sink != nil {
		r.sink(map[string]string{"refresh_token": "rt-rotated"})
	}
	return "alice@example.test", nil
}

func TestCallbackRejectsAnUnknownState(t *testing.T) {
	tokenStub, _ := stubTokenEndpoint(t)
	registerStubBackend(t, tokenStub.URL)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	page := c.redirect("code=whatever&state=not-a-real-state")
	if page.Code != http.StatusBadRequest {
		t.Errorf("callback with a forged state = %d, want 400", page.Code)
	}
	if !strings.Contains(page.Body.String(), "expired") {
		t.Errorf("page = %s", page.Body.String())
	}
}

func TestSignInIsBoundToTheSessionThatStartedIt(t *testing.T) {
	tokenStub, _ := stubTokenEndpoint(t)
	kind := registerStubBackend(t, tokenStub.URL)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	_, start := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{
		"kind":      kind,
		"client_id": "client-abc",
	})
	flowID, _ := start["flow_id"].(string)

	// A second browser, unlocking the same vault, gets its own session — and
	// no view of someone else's half-finished sign-in.
	other := &testClient{t: t, handler: c.handler, origin: c.origin}
	w, _ := other.json(http.MethodPost, "/api/vault/unlock",
		map[string]any{"password": "correct horse battery staple"})
	if w.Code != http.StatusOK {
		t.Fatalf("unlock in a second session: %d", w.Code)
	}

	if w, _ := other.json(http.MethodGet, "/api/providers/oauth/"+flowID, nil); w.Code != http.StatusNotFound {
		t.Errorf("another session could read the flow: %d", w.Code)
	}
	w, _ = other.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": flowID, "name": "stolen", "options": map[string]string{},
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("another session could complete the flow: %d", w.Code)
	}
}

func TestCompleteRefusesAnUnfinishedSignIn(t *testing.T) {
	tokenStub, _ := stubTokenEndpoint(t)
	kind := registerStubBackend(t, tokenStub.URL)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	_, start := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{
		"kind":      kind,
		"client_id": "client-abc",
	})

	w, _ := c.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": start["flow_id"], "name": "early", "options": map[string]string{},
	})
	if w.Code != http.StatusConflict {
		t.Errorf("completing a pending flow = %d, want 409", w.Code)
	}
}

// TestPastedRedirectFinishesTheFlow covers the case where the provider's
// redirect cannot reach this server — the user copies the URL over by hand.
func TestPastedRedirectFinishesTheFlow(t *testing.T) {
	tokenStub, _ := stubTokenEndpoint(t)
	kind := registerStubBackend(t, tokenStub.URL)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	_, start := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{
		"kind": kind, "client_id": "client-abc",
	})
	flowID, _ := start["flow_id"].(string)
	state := stateFrom(t, start["auth_url"].(string))

	// A URL from a different sign-in is refused.
	w, _ := c.json(http.MethodPost, "/api/providers/oauth/exchange", map[string]any{
		"flow_id": flowID,
		"url":     "http://localhost:8123" + oauthCallbackPath + "?code=x&state=someone-elses",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("mismatched state = %d, want 400", w.Code)
	}

	w, done := c.json(http.MethodPost, "/api/providers/oauth/exchange", map[string]any{
		"flow_id": flowID,
		"url":     "http://localhost:8123" + oauthCallbackPath + "?code=pasted&state=" + state,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("exchange: %d %v", w.Code, done)
	}
	if done["status"] != "ready" {
		t.Errorf("status after pasting = %v", done)
	}
}

func TestStartRejectsBackendsThatDoNotSignIn(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	w, body := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{"kind": "local"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("start on a local folder = %d, want 400", w.Code)
	}
	if !strings.Contains(body["error"].(string), "signing in") {
		t.Errorf("error = %v", body["error"])
	}
}

func TestStartSaysWhenNoAppIsConfigured(t *testing.T) {
	tokenStub, _ := stubTokenEndpoint(t)
	kind := registerStubBackend(t, tokenStub.URL)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	w, body := c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{"kind": kind})
	if w.Code != http.StatusBadRequest || body["code"] != "OAUTH_NO_CLIENT" {
		t.Fatalf("start without a client ID = %d %v", w.Code, body)
	}
	if !strings.Contains(body["error"].(string), "SAND_STUB_CLIENT_ID") {
		t.Errorf("the error should name the environment variable: %v", body["error"])
	}

	// Configured on the server, the same call needs nothing typed at all.
	t.Setenv("SAND_STUB_CLIENT_ID", "from-the-environment")
	w, body = c.json(http.MethodPost, "/api/providers/oauth/start", map[string]any{"kind": kind})
	if w.Code != http.StatusOK {
		t.Fatalf("start with a configured app = %d %v", w.Code, body)
	}
	if !strings.Contains(body["auth_url"].(string), "client_id=from-the-environment") {
		t.Errorf("auth_url = %v", body["auth_url"])
	}
}

func TestOAuthEndpointsNeedAnUnlockedVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)
	c.json(http.MethodPost, "/api/vault/lock", nil)

	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/providers/oauth/start", map[string]any{"kind": "gdrive"}},
		{http.MethodPost, "/api/providers/oauth/complete", map[string]any{"flow_id": "x", "name": "", "options": map[string]string{}}},
		{http.MethodGet, "/api/providers/oauth/x", nil},
	} {
		w, _ := c.json(call.method, call.path, call.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s while locked = %d, want 401", call.method, call.path, w.Code)
		}
	}
}

func TestRedirectURIPrefersThePinnedValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/providers/oauth/start", nil)
	req.Host = "sand.local:8123"

	got, err := oauthRedirectURI(req, "")
	if err != nil || got != "http://sand.local:8123"+oauthCallbackPath {
		t.Errorf("derived redirect = %q, %v", got, err)
	}

	req.Header.Set("X-Forwarded-Proto", "https")
	if got, _ := oauthRedirectURI(req, ""); !strings.HasPrefix(got, "https://") {
		t.Errorf("a TLS-terminated request should produce an https redirect, got %q", got)
	}

	t.Setenv(oauthRedirectEnv, "https://vault.example.com/api/providers/oauth/callback")
	if got, _ := oauthRedirectURI(req, "http://somewhere-else.test/x"); got != "https://vault.example.com/api/providers/oauth/callback" {
		t.Errorf("the pinned redirect should win, got %q", got)
	}
}

// TestProviderSpecsDescribeTheSignInFlow is what the connect dialog reads to
// decide between a button and a form.
func TestProviderSpecsDescribeTheSignInFlow(t *testing.T) {
	c := newTestClient(t)

	w := c.do(http.MethodGet, "/api/providers/specs", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("specs: %d", w.Code)
	}

	var payload struct {
		Specs []struct {
			Kind  provider.Kind `json:"kind"`
			Label string        `json:"label"`
			OAuth *struct {
				SignInLabel    string `json:"sign_in_label"`
				Configured     bool   `json:"configured"`
				SecretRequired bool   `json:"secret_required"`
			} `json:"oauth"`
			Presets []struct {
				Key string `json:"key"`
			} `json:"presets"`
		} `json:"specs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parsing specs: %v", err)
	}

	seen := map[provider.Kind]bool{}
	for _, spec := range payload.Specs {
		seen[spec.Kind] = true
	}
	for _, kind := range []provider.Kind{"gdrive", "dropbox", "onedrive", "box", "icloud", "proton", "local", "s3", "webdav"} {
		if !seen[kind] {
			t.Errorf("the connect dialog is not offered %s", kind)
		}
	}

	// The sign-in backends come first, so the dialog leads with them.
	if payload.Specs[0].OAuth == nil {
		t.Errorf("specs lead with %s, which cannot be signed in to", payload.Specs[0].Kind)
	}

	// No spec may leak the token endpoint or app credentials to the browser.
	if strings.Contains(w.Body.String(), "token") && strings.Contains(w.Body.String(), "oauth2.googleapis.com") {
		t.Error("provider specs exposed a token endpoint to the browser")
	}
}
