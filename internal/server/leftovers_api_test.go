package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The other half of the same housekeeping, over HTTP: not what a cloud is
// holding, but what SAND has left in the folder its own vault file lives in.
// An upload is spooled to disk in full before it is sent, so a process killed
// mid-upload leaves the whole file behind — see internal/vault/leftovers.go.

// abandonSpool writes a file into the vault's directory under one of SAND's
// temporary names and backdates it out of the settling window, which is what a
// spool nothing is writing to any more looks like.
func (c *testClient) abandonSpool(name string, size int, age time.Duration) string {
	c.t.Helper()

	path := filepath.Join(filepath.Dir(c.server.VaultPath), name)
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0600); err != nil {
		c.t.Fatalf("writing %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		c.t.Fatalf("backdating %s: %v", name, err)
	}
	return path
}

func TestOrphanScanReportsTheWorkingFilesLeftBeside(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)

	c.abandonSpool(".sand-upload-1611628659", 4096, 5*time.Hour)
	c.abandonSpool(".sand-upload-3236273287", 1024, time.Minute)

	scan := c.orphanScan()
	local, ok := scan["leftovers"].(map[string]any)
	if !ok {
		t.Fatalf("the scan carries no local half: %v", scan)
	}
	if found, _ := local["found"].(bool); !found {
		t.Fatalf("the spools were not reported: %v", local)
	}
	if files, _ := local["files"].(float64); files != 2 {
		t.Fatalf("reported %v file(s), want both", local["files"])
	}
	if bytesHeld, _ := local["bytes"].(float64); bytesHeld != 5120 {
		t.Fatalf("reported %v bytes, want 5120", local["bytes"])
	}
	// Only the settled one is offered: the other was written to a minute ago,
	// which is what an upload running right now looks like from outside.
	if deletable, _ := local["deletable"].(float64); deletable != 1 {
		t.Fatalf("offered %v file(s) for deletion, want only the settled one", local["deletable"])
	}
	if freed, _ := local["deletable_bytes"].(float64); freed != 4096 {
		t.Fatalf("offered %v bytes, want 4096", local["deletable_bytes"])
	}
	// The clouds are clean, and the two answers do not run into each other.
	if found, _ := scan["found"].(bool); found {
		t.Fatalf("a spool on this disk was counted as an abandoned part: %v", scan)
	}
}

func TestLeftoverSweepErasesWhatTheDryRunPromised(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)

	spool := c.abandonSpool(".sand-upload-1671795871", 8192, 5*time.Hour)
	live := c.abandonSpool(".sand-upload-4177015723", 2048, 2*time.Minute)

	_, preview := c.json(http.MethodPost, "/api/vault/orphans/leftovers", map[string]any{"dry_run": true})
	if deleted, _ := preview["deleted"].(float64); deleted != 1 {
		t.Fatalf("the dry run promised %v file(s), want 1", preview["deleted"])
	}
	if _, err := os.Stat(spool); err != nil {
		t.Fatalf("the dry run erased the spool: %v", err)
	}

	w, report := c.json(http.MethodPost, "/api/vault/orphans/leftovers", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	if deleted, _ := report["deleted"].(float64); deleted != 1 {
		t.Fatalf("the sweep erased %v file(s), want 1", report["deleted"])
	}
	if freed, _ := report["bytes"].(float64); freed != 8192 {
		t.Fatalf("the sweep freed %v bytes, want 8192", report["bytes"])
	}
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Fatalf("the settled spool is still there: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the spool that is still being written was erased: %v", err)
	}
	if _, err := os.Stat(c.server.VaultPath); err != nil {
		t.Fatalf("the vault file did not survive the sweep: %v", err)
	}
}

func TestLeftoverSweepNamingOneFileLeavesTheRest(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)

	going := c.abandonSpool(".sand-upload-1000000001", 512, 5*time.Hour)
	staying := c.abandonSpool(".sand-convert-1000000002", 256, 5*time.Hour)

	w, report := c.json(http.MethodPost, "/api/vault/orphans/leftovers", map[string]any{
		"names": []string{filepath.Base(going)},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	if deleted, _ := report["deleted"].(float64); deleted != 1 {
		t.Fatalf("the sweep erased %v file(s), want only the one named", report["deleted"])
	}
	if _, err := os.Stat(staying); err != nil {
		t.Fatalf("a file that was not named was erased: %v", err)
	}
}

func TestLeftoverSweepReachesNothingOutsideTheVaultsFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)

	// A name from outside, a name inside that is not SAND's to erase, and a
	// path that tries to climb out of the folder. Only what a fresh scan finds
	// under one of SAND's own temporary names is ever acted on, so all three
	// come back as skipped and nothing moves.
	elsewhere := filepath.Join(c.t.TempDir(), "precious.txt")
	if err := os.WriteFile(elsewhere, []byte("not SAND's"), 0600); err != nil {
		t.Fatalf("writing the bystander: %v", err)
	}

	w, report := c.json(http.MethodPost, "/api/vault/orphans/leftovers", map[string]any{
		"names": []string{elsewhere, "vault.sand", "../../etc/passwd"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	if deleted, _ := report["deleted"].(float64); deleted != 0 {
		t.Fatalf("the sweep erased %v file(s) it should not have found", report["deleted"])
	}
	if skipped, _ := report["skipped"].([]any); len(skipped) != 3 {
		t.Fatalf("the sweep said nothing about what it refused: %v", report)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("a file outside the vault's folder was erased: %v", err)
	}
	if _, err := os.Stat(c.server.VaultPath); err != nil {
		t.Fatalf("the vault file did not survive: %v", err)
	}
}

func TestLeftoverSweepNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.cookies = nil

	w, _ := c.json(http.MethodPost, "/api/vault/orphans/leftovers", map[string]any{"dry_run": true})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/vault/orphans/leftovers without a session: %d, want 401", w.Code)
	}
}
