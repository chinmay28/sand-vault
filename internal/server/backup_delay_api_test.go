package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The pacing of the index pushes is a setting of the vault: read with the
// status, set on its own route, and kept across a lock.
func TestBackupDelayIsReportedAndSet(t *testing.T) {
	c := newTestClient(t)
	c.setup("a password", 1)

	// The test harness switches the pacing off; put the shipped default back
	// so the status has something to report.
	if err := c.server.vault.SetBackupDelay(5 * time.Second); err != nil {
		t.Fatalf("SetBackupDelay: %v", err)
	}
	w, body := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if got, _ := body["manifest_backup_delay_seconds"].(float64); got != 5 {
		t.Errorf("status reports a delay of %v, want 5", body["manifest_backup_delay_seconds"])
	}

	w, body = c.json(http.MethodPost, "/api/vault/backup/delay", map[string]any{"seconds": 30})
	if w.Code != http.StatusOK {
		t.Fatalf("set delay: %d %s", w.Code, w.Body.String())
	}
	if got, _ := body["manifest_backup_delay_seconds"].(float64); got != 30 {
		t.Errorf("the answer reports a delay of %v, want 30", body["manifest_backup_delay_seconds"])
	}
	if on, _ := body["manifest_backup"].(bool); !on {
		t.Error("setting the delay switched the backup off")
	}
	if got := c.server.vault.BackupDelay(); got != 30*time.Second {
		t.Errorf("the vault holds %s, want 30s", got)
	}

	// Zero is a choice — push at once — not an omission.
	w, body = c.json(http.MethodPost, "/api/vault/backup/delay", map[string]any{"seconds": 0})
	if w.Code != http.StatusOK {
		t.Fatalf("set delay to zero: %d %s", w.Code, w.Body.String())
	}
	if got, _ := body["manifest_backup_delay_seconds"].(float64); got != 0 {
		t.Errorf("the answer reports a delay of %v, want 0", body["manifest_backup_delay_seconds"])
	}
}

func TestBackupDelayRefusesBadInput(t *testing.T) {
	c := newTestClient(t)
	c.setup("a password", 1)
	if err := c.server.vault.SetBackupDelay(5 * time.Second); err != nil {
		t.Fatalf("SetBackupDelay: %v", err)
	}

	for _, payload := range []map[string]any{
		{},                  // no seconds at all
		{"seconds": -1},     // negative
		{"seconds": 3601},   // over an hour
		{"seconds": "soon"}, // not a number
		{"seconds": 2.5},    // not whole
	} {
		w, body := c.json(http.MethodPost, "/api/vault/backup/delay", payload)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%v answered %d, want 400", payload, w.Code)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Errorf("%v was refused without a reason: %v", payload, body)
		}
	}
	if got := c.server.vault.BackupDelay(); got != 5*time.Second {
		t.Errorf("a refused request changed the delay to %s", got)
	}
}

// An orderly stop writes the index that is waiting out its delay, and does
// not wait forever for it.
func TestShutdownFlushWritesTheWaitingIndex(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("a password", 3)
	v := c.server.vault
	v.AwaitBackupSync()
	if err := v.SetBackupDelay(time.Hour); err != nil {
		t.Fatalf("SetBackupDelay: %v", err)
	}
	v.AwaitBackupSync()

	// A change inside the gap: the push for it is an hour away.
	c.upload("late.txt", "/", []byte("the last thing before the restart"))
	before := backupModTime(t, roots[0])

	if !flushBackupsWithin(v, 30*time.Second) {
		t.Fatal("the flush did not settle a push to a local folder within 30s")
	}
	if !backupModTime(t, roots[0]).After(before) {
		t.Error("the stop did not rewrite the copy of the index on the account")
	}
}

func backupModTime(t *testing.T, root string) time.Time {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, vault.BackupKey))
	if err != nil {
		t.Fatalf("stat the index backup: %v", err)
	}
	return info.ModTime()
}
