package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/chinmay28/sand-vault/internal/compress"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/tuning"
)

func resetSharedTuning(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		compress.Configure(compress.Options{Level: compress.DefaultLevel, Threads: compress.DefaultThreads})
		provider.ConfigureTransport(provider.DefaultConnsPerHost)
	})
}

// Opening the vault applies a tuning: the vault, the compressor and the
// provider layer all run the numbers the profile resolved to.
func TestOpeningTheVaultAppliesTheTuning(t *testing.T) {
	resetSharedTuning(t)
	c := newTestClient(t)
	c.setup("pw", 0)

	w, body := c.json(http.MethodGet, "/api/machine", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/machine: %d %v", w.Code, body)
	}
	if body["profile"] != "auto" {
		t.Errorf("a fresh install runs profile %v, want auto", body["profile"])
	}
	if body["pinned"] != false {
		t.Error("nothing pinned the profile, but the page is told it is")
	}
	effective := body["effective"].(map[string]any)
	v := c.server.vault
	if got := v.Tuning().UploadWindow; float64(got) != effective["upload_window"] {
		t.Errorf("vault runs an upload window of %d, page says %v", got, effective["upload_window"])
	}
	if got := compress.Current(); float64(got.Level) != effective["compress_level"] {
		t.Errorf("compressor at level %d, page says %v", got.Level, effective["compress_level"])
	}
	if got := provider.ConnsPerHost(); float64(got) != effective["conns_per_host"] {
		t.Errorf("transport keeps %d connections per host, page says %v", got, effective["conns_per_host"])
	}
	if knobs := body["knobs"].([]any); len(knobs) != len(tuning.Knobs()) {
		t.Errorf("page is told about %d knobs, want %d", len(knobs), len(tuning.Knobs()))
	}
	if profiles := body["profiles"].([]any); len(profiles) != 4 {
		t.Errorf("page is told about %d profiles, want 4", len(profiles))
	}
}

// Choosing a profile with overrides saves the choice beside the vault,
// applies it at once, and reports the override as one.
func TestChoosingAProfileSavesAndApplies(t *testing.T) {
	resetSharedTuning(t)
	c := newTestClient(t)
	c.setup("pw", 0)

	w, body := c.json(http.MethodPut, "/api/machine", map[string]any{
		"profile":   "pi5",
		"overrides": map[string]any{"read_ahead": 4, "compress_level": 2},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/machine: %d %v", w.Code, body)
	}
	if body["profile"] != "pi5" {
		t.Errorf("profile = %v", body["profile"])
	}
	overrides := body["overrides"].(map[string]any)
	if overrides["read_ahead"] != float64(4) || overrides["compress_level"] != float64(2) {
		t.Errorf("overrides reported as %v", overrides)
	}
	defaults := body["defaults"].(map[string]any)
	if defaults["read_ahead"] != float64(1) {
		t.Errorf("the pi5 default read-ahead is reported as %v, want 1", defaults["read_ahead"])
	}

	v := c.server.vault
	if v.Tuning().ReadAhead != 4 {
		t.Errorf("vault read-ahead = %d, want the override", v.Tuning().ReadAhead)
	}
	if compress.Current().Level != 2 {
		t.Errorf("compressor level = %d, want the override", compress.Current().Level)
	}

	// On disk, as the choice rather than the numbers.
	saved, err := tuning.Load(tuning.PathFor(v.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Profile != tuning.ProfilePi5 || saved.Overrides["read_ahead"] != 4 {
		t.Errorf("saved %+v", saved)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path()), tuning.FileName)); err != nil {
		t.Errorf("no %s beside the vault: %v", tuning.FileName, err)
	}

	// A fresh server over the same vault comes up on the saved choice.
	again := &Server{VaultPath: v.Path()}
	if _, err := again.Handler(); err != nil {
		t.Fatal(err)
	}
	if again.vault.Tuning().ReadAhead != 4 {
		t.Errorf("after reopening, read-ahead = %d, want the saved override", again.vault.Tuning().ReadAhead)
	}
}

// Reset puts everything back to automatic with nothing overridden.
func TestResetReturnsToAutomatic(t *testing.T) {
	resetSharedTuning(t)
	c := newTestClient(t)
	c.setup("pw", 0)

	if w, body := c.json(http.MethodPut, "/api/machine", map[string]any{
		"profile": "custom", "machine": map[string]any{"cpus": 8, "memory_bytes": 32 << 30},
		"overrides": map[string]any{"erase_window": 6},
	}); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %v", w.Code, body)
	}
	w, body := c.json(http.MethodDelete, "/api/machine", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /api/machine: %d %v", w.Code, body)
	}
	if body["profile"] != "auto" {
		t.Errorf("after reset, profile = %v", body["profile"])
	}
	if overrides := body["overrides"].(map[string]any); len(overrides) != 0 {
		t.Errorf("after reset, overrides = %v", overrides)
	}
	saved, _ := tuning.Load(tuning.PathFor(c.server.vault.Path()))
	if saved.Profile != "" || len(saved.Overrides) != 0 {
		t.Errorf("reset left %+v on disk", saved)
	}
}

// What will not fit or is out of range is refused, and nothing changes.
func TestABadChoiceIsRefusedAndNothingChanges(t *testing.T) {
	resetSharedTuning(t)
	c := newTestClient(t)
	c.setup("pw", 0)
	before := c.server.vault.Tuning()

	cases := []map[string]any{
		{"profile": "mainframe"},
		{"profile": "custom"}, // no machine
		{"profile": "pi5", "overrides": map[string]any{"upload_window": 0}},
		{"profile": "pi5", "overrides": map[string]any{"no_such_knob": 1}},
		{"profile": "pi5", "overrides": map[string]any{"chunk_cache_bytes": 12 << 30}},
	}
	for _, body := range cases {
		w, resp := c.json(http.MethodPut, "/api/machine", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT %v: %d %v, want 400", body, w.Code, resp)
		}
	}
	if c.server.vault.Tuning() != before {
		t.Error("a refused choice changed the tuning")
	}
}

// A profile pinned from the command line cannot be changed from the page,
// but its knobs can.
func TestAPinnedProfileRefusesToMove(t *testing.T) {
	resetSharedTuning(t)
	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand"), Profile: "pi5"}
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	c := &testClient{t: t, handler: handler, server: s, origin: "http://example.test"}
	c.setup("pw", 0)

	w, body := c.json(http.MethodGet, "/api/machine", nil)
	if w.Code != http.StatusOK || body["profile"] != "pi5" || body["pinned"] != true {
		t.Fatalf("GET: %d %v", w.Code, body)
	}
	if w, body := c.json(http.MethodPut, "/api/machine", map[string]any{"profile": "workstation"}); w.Code != http.StatusConflict {
		t.Errorf("changing a pinned profile: %d %v, want 409", w.Code, body)
	}
	w, body = c.json(http.MethodPut, "/api/machine", map[string]any{
		"profile": "pi5", "overrides": map[string]any{"read_ahead": 2},
	})
	if w.Code != http.StatusOK {
		t.Errorf("overriding a knob under a pinned profile: %d %v", w.Code, body)
	}
	if s.vault.Tuning().ReadAhead != 2 {
		t.Errorf("read-ahead = %d, want 2", s.vault.Tuning().ReadAhead)
	}
}

// A machine.json that cannot be applied does not stop the server: it runs
// on defaults and says so.
func TestABrokenMachineFileIsReportedNotFatal(t *testing.T) {
	resetSharedTuning(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, tuning.FileName), []byte(`{"profile":"pi5","overrides":{"upload_window":0}}`), 0o600)
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand")}
	if _, err := s.Handler(); err != nil {
		t.Fatalf("a broken machine file stopped the server: %v", err)
	}
	if s.vault.Tuning().UploadWindow < 1 {
		t.Error("the broken override was applied")
	}
}
