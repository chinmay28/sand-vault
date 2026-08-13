package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// OAuthSpec describes how a backend's browser sign-in works: where to send the
// user, how to turn the code they come back with into tokens, and which
// options those tokens are stored under.
//
// Everything the server needs to drive the flow lives here rather than in the
// HTTP layer, so connecting a new OAuth backend is a matter of registering one
// more spec. The fields with JSON tags are the ones the browser is told about;
// the rest never leave the process.
type OAuthSpec struct {
	// SignInLabel is the button the user actually clicks, e.g. "Continue with
	// Google".
	SignInLabel string `json:"sign_in_label"`

	// ConsoleURL and ConsoleSteps point at the provider's developer console for
	// the case where no app credentials have been configured for SAND and the
	// user has to register one themselves. Each step is rendered as its own
	// numbered line, between "open the console" and "paste the client ID".
	ConsoleURL   string   `json:"console_url,omitempty"`
	ConsoleSteps []string `json:"console_steps,omitempty"`

	// ClientIDField and ClientSecretField name the option keys the app
	// credentials live under, so the connect form knows which of the spec's
	// fields it is collecting up front and which the sign-in fills in.
	ClientIDField     string `json:"client_id_field"`
	ClientSecretField string `json:"client_secret_field"`

	// SecretRequired is false for backends that accept a public client, where
	// PKCE stands in for the client secret.
	SecretRequired bool `json:"secret_required"`

	// Configured reports that this deployment already has app credentials for
	// the backend, so the user goes straight to the provider's consent screen.
	// Filled in per call from the environment; never set on a registered spec.
	Configured bool `json:"configured"`

	// --- server-side only ------------------------------------------------

	AuthURL  string   `json:"-"`
	TokenURL string   `json:"-"`
	Scopes   []string `json:"-"`

	// AuthParams are extra query parameters the authorize URL needs. This is
	// where "please issue a refresh token" lives, which every provider spells
	// differently.
	AuthParams map[string]string `json:"-"`

	// PKCE adds a proof key to the exchange. Harmless where a client secret is
	// also in play, and the only thing binding the code to us where there is
	// no secret at all.
	PKCE bool `json:"-"`

	// RefreshTokenField and AccessTokenField are the option keys the exchanged
	// tokens are written to.
	RefreshTokenField string `json:"-"`
	AccessTokenField  string `json:"-"`

	// ClientIDEnv and ClientSecretEnv name environment variables holding app
	// credentials for the whole deployment. Set them and nobody connecting an
	// account ever sees an OAuth client ID.
	ClientIDEnv     string `json:"-"`
	ClientSecretEnv string `json:"-"`
}

// EnvClient returns the app credentials this deployment was started with, if
// any.
func (o *OAuthSpec) EnvClient() (clientID, clientSecret string) {
	if o == nil {
		return "", ""
	}
	return strings.TrimSpace(os.Getenv(o.ClientIDEnv)), strings.TrimSpace(os.Getenv(o.ClientSecretEnv))
}

// configured reports whether sign-in can start without the user supplying app
// credentials first.
func (o *OAuthSpec) configured() bool {
	if o == nil {
		return false
	}
	id, secret := o.EnvClient()
	if id == "" {
		return false
	}
	return secret != "" || !o.SecretRequired
}

// AuthCodeURL is where the browser is sent to ask the account holder for
// consent. challenge is the PKCE challenge, ignored by specs that do not use
// one.
func (o *OAuthSpec) AuthCodeURL(clientID, redirectURI, state, challenge string) string {
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("state", state)
	if len(o.Scopes) > 0 {
		params.Set("scope", strings.Join(o.Scopes, " "))
	}
	for k, v := range o.AuthParams {
		params.Set(k, v)
	}
	if o.PKCE && challenge != "" {
		params.Set("code_challenge", challenge)
		params.Set("code_challenge_method", "S256")
	}

	sep := "?"
	if strings.Contains(o.AuthURL, "?") {
		sep = "&"
	}
	return o.AuthURL + sep + params.Encode()
}

// Tokens is what comes back from a successful code exchange.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// Exchange trades an authorization code for tokens. verifier is the PKCE
// verifier matching the challenge sent to AuthCodeURL, empty where unused.
func (o *OAuthSpec) Exchange(ctx context.Context, clientID, clientSecret, code, redirectURI, verifier string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}

	tok, err := postTokenForm(ctx, o.TokenURL, form)
	if err != nil {
		return Tokens{}, err
	}
	if tok.RefreshToken == "" {
		// Without one the account would go dark the moment the access token
		// expires, which is an hour later and far away from this screen.
		return Tokens{}, fmt.Errorf("%s returned no refresh token — if you have connected this app "+
			"to the account before, revoke it in the account's security settings and sign in again",
			hostOf(o.TokenURL))
	}
	return tok, nil
}

// Options maps a completed exchange onto the option keys the backend reads its
// credentials from, ready to become a Config.
func (o *OAuthSpec) Options(clientID, clientSecret string, tok Tokens) map[string]string {
	out := map[string]string{}
	if o.ClientIDField != "" {
		out[o.ClientIDField] = clientID
	}
	if o.ClientSecretField != "" && clientSecret != "" {
		out[o.ClientSecretField] = clientSecret
	}
	if o.RefreshTokenField != "" {
		out[o.RefreshTokenField] = tok.RefreshToken
	}
	if o.AccessTokenField != "" && tok.AccessToken != "" {
		out[o.AccessTokenField] = tok.AccessToken
	}
	return out
}

// NewPKCE mints a verifier and the challenge derived from it.
func NewPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// postTokenForm posts a form to a token endpoint and reads the standard
// response out of it.
func postTokenForm(ctx context.Context, tokenURL string, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("reaching %s: %w", hostOf(tokenURL), err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return Tokens{}, httpError("token exchange", resp)
	}

	body, err := readAllBody(resp)
	if err != nil {
		return Tokens{}, err
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Tokens{}, fmt.Errorf("parsing token response: %w", err)
	}
	if payload.AccessToken == "" {
		return Tokens{}, fmt.Errorf("%s returned no access token", hostOf(tokenURL))
	}
	return Tokens{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ExpiresIn:    payload.ExpiresIn,
	}, nil
}

// hostOf reduces a URL to its host for error messages.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// tokenSource exchanges a long-lived refresh token for short-lived access
// tokens and caches the result until shortly before it expires. Every OAuth
// backend uses the same refresh grant, so they share this.
type tokenSource struct {
	tokenURL     string
	clientID     string
	clientSecret string
	refreshToken string

	// scopes is sent with the refresh where the provider insists on it.
	scopes []string

	// staticToken short-circuits refreshing entirely, for backends that also
	// accept a directly supplied access token.
	staticToken string

	mu        sync.Mutex
	token     string
	expiresAt time.Time

	// onRotate is called when the provider hands back a new refresh token and
	// retires the old one. Box and Microsoft both do this on every refresh, so
	// the new value has to reach the vault or the account dies quietly an hour
	// later.
	onRotate func(refreshToken string)
}

// token returns a valid access token, refreshing if the cached one is missing
// or within a minute of expiring.
func (ts *tokenSource) accessToken(ctx context.Context) (string, error) {
	if ts.staticToken != "" && ts.refreshToken == "" {
		return ts.staticToken, nil
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.token != "" && time.Now().Before(ts.expiresAt.Add(-time.Minute)) {
		return ts.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", ts.refreshToken)
	form.Set("client_id", ts.clientID)
	// Public clients have no secret, and sending an empty one is rejected
	// outright by some token endpoints rather than ignored.
	if ts.clientSecret != "" {
		form.Set("client_secret", ts.clientSecret)
	}
	if len(ts.scopes) > 0 {
		form.Set("scope", strings.Join(ts.scopes, " "))
	}

	tok, err := postTokenForm(ctx, ts.tokenURL, form)
	if err != nil {
		return "", fmt.Errorf("refreshing access token: %w", err)
	}

	ts.token = tok.AccessToken
	if tok.ExpiresIn > 0 {
		ts.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		ts.expiresAt = time.Now().Add(time.Hour)
	}

	if tok.RefreshToken != "" && tok.RefreshToken != ts.refreshToken {
		ts.refreshToken = tok.RefreshToken
		if ts.onRotate != nil {
			// Called inline, and holding this lock: a sink must record the new
			// token and return. Anything that could block — writing the vault
			// file, say — is the sink's job to hand off.
			ts.onRotate(tok.RefreshToken)
		}
	}
	return ts.token, nil
}

// authorize attaches a bearer token to a request.
func (ts *tokenSource) authorize(ctx context.Context, req *http.Request) error {
	token, err := ts.accessToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// setRotationSink installs the callback fired when the refresh token changes.
func (ts *tokenSource) setRotationSink(fn func(string)) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.onRotate = fn
}

// oauthBase is embedded by every backend that authenticates with OAuth. It
// carries the token source and answers the vault's request to be told when the
// stored credentials change underneath it.
type oauthBase struct {
	base
	tokens *tokenSource

	// refreshField is the option key the refresh token is stored under, so a
	// rotated token can be written back to the right place.
	refreshField string
}

// OnCredentialChange registers a sink for credential updates that have to be
// persisted. It satisfies CredentialRotator. The sink runs inline on whatever
// goroutine was refreshing, so it must not block.
func (o *oauthBase) OnCredentialChange(fn func(map[string]string)) {
	if fn == nil || o.refreshField == "" {
		return
	}
	o.tokens.setRotationSink(func(token string) {
		fn(map[string]string{o.refreshField: token})
	})
}
