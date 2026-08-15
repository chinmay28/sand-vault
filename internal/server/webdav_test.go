package server

import (
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

	if s.davActive() {
		t.Error("a share nobody has used reports as active")
	}

	s.noteDAVActivity()
	if !s.davActive() {
		t.Error("a share used just now does not report as active")
	}

	s.davMu.Lock()
	s.davActivity = time.Now().Add(-2 * time.Minute)
	s.davMu.Unlock()
	if s.davActive() {
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
