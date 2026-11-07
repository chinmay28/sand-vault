package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The share is a second way into the vault, so it is not there unless it was
// asked for.
func TestWebDAVIsOffUnlessEnabled(t *testing.T) {
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand")}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/dav/", nil))
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusMultiStatus {
		t.Errorf("the share answered on a server that did not enable it: %d", w.Code)
	}
}

// Enabled, it answers on both the bare prefix and everything under it — a
// client asking about the share itself sends the former.
func TestWebDAVAnswersOnBothPrefixForms(t *testing.T) {
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand"), WebDAV: true}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	for _, path := range []string{"/dav", "/dav/", "/dav/films/"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("PROPFIND %s = %d, want 401 from the share's auth", path, w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got == "" {
			t.Errorf("PROPFIND %s returned no Basic challenge", path)
		}
	}
}

func TestWebDAVPrefixIsConfigurable(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		VaultPath:    filepath.Join(dir, "vault.sand"),
		WebDAV:       true,
		WebDAVPrefix: "share/",
	}
	if got := s.webdavPrefix(); got != "/share" {
		t.Errorf("webdavPrefix() = %q, want %q", got, "/share")
	}

	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/share/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("PROPFIND on the configured prefix = %d, want 401", w.Code)
	}
}

// A mounted share has no browser session, so without counting its requests as
// use the vault would lock itself halfway through a film.
func TestWebDAVActivityHoldsOffTheAutoLock(t *testing.T) {
	s := &Server{IdleTimeout: time.Minute}

	if s.externalActive() {
		t.Error("a share nobody has used reports as active")
	}

	s.noteExternalActivity()
	if !s.externalActive() {
		t.Error("a share used just now does not report as active")
	}

	s.externalMu.Lock()
	s.externalActivity = time.Now().Add(-2 * time.Minute)
	s.externalMu.Unlock()
	if s.externalActive() {
		t.Error("a share last used past the idle timeout still reports as active")
	}
}

// The browser needs to know the share exists to offer a way to mount it, and
// must not be told about one that is not being served.
func TestVaultStatusReportsTheShare(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		prefix  string
		want    string
	}{
		{name: "off", enabled: false},
		{name: "on", enabled: true, want: "/dav/"},
		{name: "on with a custom path", enabled: true, prefix: "/share", want: "/share/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := &Server{
				VaultPath:    filepath.Join(dir, "vault.sand"),
				WebDAV:       tc.enabled,
				WebDAVPrefix: tc.prefix,
			}
			handler, err := s.Handler()
			if err != nil {
				t.Fatalf("Handler: %v", err)
			}

			v, err := s.Vault()
			if err != nil {
				t.Fatalf("Vault: %v", err)
			}
			if err := v.Init("a perfectly ordinary password", vault.PolicyStrict); err != nil {
				t.Fatalf("Init: %v", err)
			}
			t.Cleanup(v.AwaitBackupSync)
			token, err := s.sessions.issue()
			if err != nil {
				t.Fatalf("issue: %v", err)
			}

			req := httptest.NewRequest("GET", "/api/vault", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			var status struct {
				Unlocked bool `json:"unlocked"`
				WebDAV   *struct {
					Path string `json:"path"`
				} `json:"webdav"`
			}
			if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
				t.Fatalf("decoding the status: %v", err)
			}
			if !status.Unlocked {
				t.Fatal("the session did not see an unlocked vault, so the test proves nothing")
			}

			if tc.want == "" {
				if status.WebDAV != nil {
					t.Errorf("a server without a share reported one at %q", status.WebDAV.Path)
				}
				return
			}
			if status.WebDAV == nil {
				t.Fatal("a server serving a share did not report it")
			}
			if status.WebDAV.Path != tc.want {
				t.Errorf("share path = %q, want %q", status.WebDAV.Path, tc.want)
			}
		})
	}
}

// Without a session the share is not mentioned, the same way the stats are not.
func TestVaultStatusHidesTheShareFromStrangers(t *testing.T) {
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand"), WebDAV: true}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	v, _ := s.Vault()
	if err := v.Init("a perfectly ordinary password", vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/vault", nil))
	if strings.Contains(w.Body.String(), "webdav") {
		t.Errorf("an unauthenticated status mentions the share: %s", w.Body.String())
	}
}

// The browser takes its state straight from what unlock hands back and does not
// ask again, so anything missing from that answer is missing from the app until
// something else happens to refetch. The share went unmentioned there for
// exactly that reason; every door into an open vault has to describe it.
func TestEveryUnlockedAnswerDescribesTheShare(t *testing.T) {
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand"), WebDAV: true}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	v, err := s.Vault()
	if err != nil {
		t.Fatalf("Vault: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)

	const password = "a perfectly ordinary password"

	share := func(t *testing.T, what string, body *bytes.Buffer) {
		t.Helper()
		var status struct {
			Unlocked bool `json:"unlocked"`
			WebDAV   *struct {
				Path string `json:"path"`
			} `json:"webdav"`
		}
		if err := json.NewDecoder(body).Decode(&status); err != nil {
			t.Fatalf("%s: decoding: %v", what, err)
		}
		if !status.Unlocked {
			t.Fatalf("%s did not report an unlocked vault", what)
		}
		if status.WebDAV == nil {
			t.Errorf("%s does not mention the share, so the app never learns about it", what)
			return
		}
		if status.WebDAV.Path != "/dav/" {
			t.Errorf("%s reports the share at %q, want /dav/", what, status.WebDAV.Path)
		}
	}

	// Creating a vault hands the app its first status.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, jsonRequest("POST", "/api/vault/init",
		`{"password":"`+password+`","policy":"strict"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("init = %d: %s", w.Code, w.Body)
	}
	share(t, "init", w.Body)

	v.Lock()

	// And unlocking hands it the next one, which is the path a returning user
	// actually takes.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, jsonRequest("POST", "/api/vault/unlock", `{"password":"`+password+`"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("unlock = %d: %s", w.Code, w.Body)
	}
	share(t, "unlock", w.Body)
}

func jsonRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// The sub vaults' shares come and go with the vault's: off together, on
// together, and beside each other rather than nested.
func TestSubVaultSharesAreServedBesideTheVaultShare(t *testing.T) {
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand")}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/sub/Taxes/", nil))
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusServiceUnavailable || w.Code == http.StatusMultiStatus {
		t.Errorf("a sub vault share answered on a server that did not enable WebDAV: %d", w.Code)
	}

	s = &Server{VaultPath: filepath.Join(dir, "vault.sand"), WebDAV: true}
	handler, err = s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	v, _ := s.Vault()
	if err := v.Init("a perfectly ordinary password", vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	if _, err := v.CreateSubVault("Taxes", "the sub vault's own password"); err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}

	// A sub vault's share asks for its password; the root of the shares names
	// nothing.
	for _, path := range []string{"/sub/Taxes", "/sub/Taxes/", "/sub/Taxes/Papers/"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("PROPFIND %s = %d, want 401 from the share's auth", path, w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got == "" {
			t.Errorf("PROPFIND %s returned no Basic challenge", path)
		}
	}
	for _, path := range []string{"/sub", "/sub/"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404", path, w.Code)
		}
	}

	// And with the vault locked the share is unavailable rather than asking
	// for a password that could not help.
	v.Lock()
	w = httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/sub/Taxes/", nil)
	req.SetBasicAuth("anyone", "the sub vault's own password")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("PROPFIND on a sub vault share while the vault is locked = %d, want 503", w.Code)
	}
}

func TestWebDAVSubPrefixIsConfigurable(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		VaultPath:       filepath.Join(dir, "vault.sand"),
		WebDAV:          true,
		WebDAVSubPrefix: "vaults/",
	}
	if got := s.webdavSubPrefix(); got != "/vaults" {
		t.Errorf("webdavSubPrefix() = %q, want %q", got, "/vaults")
	}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	v, _ := s.Vault()
	if err := v.Init("a perfectly ordinary password", vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/vaults/Taxes/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("PROPFIND on the configured sub vault prefix = %d, want 401", w.Code)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/sub/Taxes/", nil))
	if w.Code == http.StatusUnauthorized {
		t.Errorf("the default sub vault prefix still answers after being moved: %d", w.Code)
	}
}

// One share's prefix inside the other's would have a folder in one shadow the
// other, so the server refuses to start that way rather than serving a
// surprise.
func TestWebDAVPrefixesMayNotNest(t *testing.T) {
	for _, tc := range []struct{ main, sub string }{
		{"/dav", "/dav"},
		{"/dav", "/dav/sub"},
		{"/sub/vault", "/sub"},
		{"dav/", "/dav/"},
	} {
		dir := t.TempDir()
		s := &Server{
			VaultPath:       filepath.Join(dir, "vault.sand"),
			WebDAV:          true,
			WebDAVPrefix:    tc.main,
			WebDAVSubPrefix: tc.sub,
		}
		if _, err := s.Handler(); err == nil {
			t.Errorf("Handler accepted the share at %q with the sub vault shares at %q", tc.main, tc.sub)
		}
	}

	// Sharing a leading run of letters is not nesting.
	dir := t.TempDir()
	s := &Server{
		VaultPath:       filepath.Join(dir, "vault.sand"),
		WebDAV:          true,
		WebDAVPrefix:    "/dav",
		WebDAVSubPrefix: "/dav-sub",
	}
	if _, err := s.Handler(); err != nil {
		t.Errorf("Handler refused /dav beside /dav-sub: %v", err)
	}
}

// The browser draws a mount line per sub vault from the status, so the status
// has to say where each one's share is — by label where the label can be a
// path segment, and by ID where it cannot.
func TestVaultStatusReportsTheSubVaultShares(t *testing.T) {
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand"), WebDAV: true}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	v, _ := s.Vault()
	if err := v.Init("a perfectly ordinary password", vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)

	plain, err := v.CreateSubVault("Taxes", "one")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	spaced, err := v.CreateSubVault("Tax Papers", "two")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	slashed, err := v.CreateSubVault("2024/2025", "three")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}

	token, err := s.sessions.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/vault", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var status struct {
		WebDAV *struct {
			Path      string            `json:"path"`
			SubVaults map[string]string `json:"sub_vaults"`
		} `json:"webdav"`
	}
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decoding the status: %v", err)
	}
	if status.WebDAV == nil {
		t.Fatal("the status does not mention the share")
	}
	want := map[string]string{
		plain.ID:   "/sub/Taxes/",
		spaced.ID:  "/sub/Tax%20Papers/",
		slashed.ID: "/sub/" + slashed.ID + "/",
	}
	for id, path := range want {
		if got := status.WebDAV.SubVaults[id]; got != path {
			t.Errorf("share path for %s = %q, want %q", id, got, path)
		}
	}

	// And nothing about them to a stranger.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/vault", nil))
	if strings.Contains(w.Body.String(), "sub_vaults") || strings.Contains(w.Body.String(), "Taxes") {
		t.Errorf("an unauthenticated status mentions the sub vault shares: %s", w.Body.String())
	}
}
