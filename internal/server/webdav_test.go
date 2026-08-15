package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
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
