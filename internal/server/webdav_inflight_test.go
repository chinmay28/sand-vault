package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// A request over the share counts as use for as long as it runs, not only for
// the moment it began. A PUT of a film takes longer than the idle timeout, and
// the auto-lock taking the keys away partway through is what this guards.
func TestWebDAVRequestInFlightHoldsOffTheAutoLock(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		VaultPath:   filepath.Join(dir, "vault.sand"),
		WebDAV:      true,
		IdleTimeout: time.Minute,
	}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	v, err := s.Vault()
	if err != nil {
		t.Fatalf("Vault: %v", err)
	}
	const password = "a perfectly ordinary password"
	if err := v.Init(password, vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)
	for i := 0; i < 3; i++ {
		if _, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "cloud-" + string(rune('a'+i)),
			Options: map[string]string{"path": filepath.Join(dir, "cloud", string(rune('a'+i)))},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if s.externalActive() {
		t.Fatal("a share nobody has used reports as active")
	}

	// A PUT whose body stays open is a request still running.
	pr, pw := io.Pipe()
	req, err := http.NewRequest("PUT", srv.URL+"/dav/film.mkv", pr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.SetBasicAuth("anyone", password)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Errorf("PUT: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()
	if _, err := pw.Write([]byte("the first bytes of a long film")); err != nil {
		t.Fatalf("writing the body: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.dav.InFlight() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.dav.InFlight() != 1 {
		t.Fatalf("InFlight = %d while a PUT is running, want 1", s.dav.InFlight())
	}

	// Age the one-off note of activity past the idle timeout: the request
	// began long ago by the auto-lock's reckoning, and is still going.
	s.externalMu.Lock()
	s.externalActivity = time.Now().Add(-2 * time.Minute)
	s.externalMu.Unlock()
	if !s.externalActive() {
		t.Error("a share in the middle of a request does not report as active")
	}

	pw.Close()
	resp := <-done
	if resp == nil {
		t.FailNow()
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("PUT = %d, want 201", resp.StatusCode)
	}
	for s.dav.InFlight() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.externalActive() {
		t.Error("the share still reports as active once its request has finished and the note has aged out")
	}
}
