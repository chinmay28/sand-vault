package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthCodeURLCarriesEverythingTheProviderNeeds(t *testing.T) {
	spec := &OAuthSpec{
		AuthURL:    "https://accounts.example.test/authorize",
		Scopes:     []string{"files.read", "offline_access"},
		AuthParams: map[string]string{"access_type": "offline"},
		PKCE:       true,
	}

	raw := spec.AuthCodeURL("client-123", "http://sand.test/api/providers/oauth/callback", "state-abc", "challenge-xyz")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}
	q := u.Query()

	cases := map[string]string{
		"response_type":         "code",
		"client_id":             "client-123",
		"redirect_uri":          "http://sand.test/api/providers/oauth/callback",
		"state":                 "state-abc",
		"scope":                 "files.read offline_access",
		"access_type":           "offline",
		"code_challenge":        "challenge-xyz",
		"code_challenge_method": "S256",
	}
	for key, want := range cases {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestExchangeRequiresARefreshToken(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// An access token on its own goes dark within the hour.
		w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
	}))
	defer stub.Close()

	spec := &OAuthSpec{TokenURL: stub.URL}
	_, err := spec.Exchange(context.Background(), "id", "secret", "code", "http://sand.test/cb", "")
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("Exchange = %v, want a missing-refresh-token error", err)
	}
}

func TestExchangeSendsTheProofKeyAndReturnsTokens(t *testing.T) {
	var got url.Values

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
	}))
	defer stub.Close()

	spec := &OAuthSpec{
		TokenURL:          stub.URL,
		ClientIDField:     "client_id",
		ClientSecretField: "client_secret",
		RefreshTokenField: "refresh_token",
	}

	tokens, err := spec.Exchange(context.Background(), "id", "secret", "the-code", "http://sand.test/cb", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tokens.RefreshToken != "rt" || tokens.AccessToken != "at" {
		t.Errorf("tokens = %+v", tokens)
	}

	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"redirect_uri":  "http://sand.test/cb",
		"client_id":     "id",
		"client_secret": "secret",
		"code_verifier": "verifier",
	}
	for key, value := range want {
		if got.Get(key) != value {
			t.Errorf("form %s = %q, want %q", key, got.Get(key), value)
		}
	}

	options := spec.Options("id", "secret", tokens)
	if options["refresh_token"] != "rt" || options["client_id"] != "id" {
		t.Errorf("Options = %v", options)
	}
}

// TestTokenSourceStoresARotatedRefreshToken covers the failure mode that makes
// Box and Microsoft accounts go dark: the provider retires the refresh token
// as it is spent, and the replacement has to be handed back for storage.
func TestTokenSourceStoresARotatedRefreshToken(t *testing.T) {
	var seen []string

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		seen = append(seen, r.PostForm.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		// Every refresh mints a new one, and the access token it comes with is
		// already too close to expiry to reuse, so the next call refreshes
		// again — with the replacement, if it was stored.
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt-next","expires_in":1}`))
	}))
	defer stub.Close()

	rotated := make(chan string, 4)
	ts := &tokenSource{tokenURL: stub.URL, clientID: "id", refreshToken: "rt-first"}
	ts.setRotationSink(func(token string) { rotated <- token })

	ctx := context.Background()
	if _, err := ts.accessToken(ctx); err != nil {
		t.Fatalf("first accessToken: %v", err)
	}
	if got := <-rotated; got != "rt-next" {
		t.Errorf("rotation reported %q, want rt-next", got)
	}

	if _, err := ts.accessToken(ctx); err != nil {
		t.Fatalf("second accessToken: %v", err)
	}
	if len(seen) != 2 || seen[0] != "rt-first" || seen[1] != "rt-next" {
		t.Errorf("refresh tokens sent = %v, want [rt-first rt-next]", seen)
	}
}

// TestTokenSourceOmitsAnEmptySecret keeps public clients working: a token
// endpoint handed client_secret= rejects the request rather than ignoring it.
func TestTokenSourceOmitsAnEmptySecret(t *testing.T) {
	var hasSecret bool

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		_, hasSecret = r.PostForm["client_secret"]
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
	}))
	defer stub.Close()

	ts := &tokenSource{tokenURL: stub.URL, clientID: "id", refreshToken: "rt"}
	if _, err := ts.accessToken(context.Background()); err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if hasSecret {
		t.Error("an empty client secret was sent to the token endpoint")
	}
}

func TestOAuthSpecsReportWhetherAnAppIsConfigured(t *testing.T) {
	spec, ok := SpecFor(KindGDrive)
	if !ok || spec.OAuth == nil {
		t.Fatal("Google Drive should be connectable by signing in")
	}
	if spec.OAuth.Configured {
		t.Error("no client credentials are set in this environment, so Configured should be false")
	}

	t.Setenv("SAND_GOOGLE_CLIENT_ID", "client")
	t.Setenv("SAND_GOOGLE_CLIENT_SECRET", "secret")

	spec, _ = SpecFor(KindGDrive)
	if !spec.OAuth.Configured {
		t.Error("client credentials in the environment should make the backend one-click")
	}

	// The registered spec must not have been mutated by resolving a copy.
	t.Setenv("SAND_GOOGLE_CLIENT_ID", "")
	t.Setenv("SAND_GOOGLE_CLIENT_SECRET", "")
	spec, _ = SpecFor(KindGDrive)
	if spec.OAuth.Configured {
		t.Error("Configured leaked across calls")
	}
}
