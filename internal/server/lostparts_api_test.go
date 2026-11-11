package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// emptyAccountFolder removes everything in one account's folder and keeps the
// folder: a cloud somebody emptied by hand.
func emptyAccountFolder(t *testing.T, root string) (parts int) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "-p") {
			parts++
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	return parts
}

// The whole feature over HTTP: a cloud's folder is emptied, the stray-parts
// scan the app already runs says so, the scan lists what is gone, the restore
// puts it back, and the files read with another cloud taken away.
func TestLostPartsAreFoundAndPutBackOverHTTP(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	body := []byte(strings.Repeat("worth keeping ", 800))
	first := c.upload("first.txt", "/", body)
	c.upload("second.txt", "/", []byte("and this too"))
	c.server.vault.AwaitBackupSync()

	// Nothing lost: no run yet, an empty answer, and nothing on the banner's
	// scan.
	if w, status := c.json(http.MethodGet, "/api/vault/lost-parts/scan", nil); w.Code != http.StatusOK || status["id"] != nil {
		t.Fatalf("status before any scan: %d %v", w.Code, status)
	}
	if _, scan := c.json(http.MethodGet, "/api/vault/orphans", nil); scan["lost"] != nil {
		t.Fatalf("a whole vault's stray scan carries lost parts: %v", scan["lost"])
	}

	lost := emptyAccountFolder(t, roots[1])
	if lost != 2 {
		t.Fatalf("the emptied account held %d parts, want 2", lost)
	}

	_, strays := c.json(http.MethodGet, "/api/vault/orphans", nil)
	summary, _ := strays["lost"].(map[string]any)
	if summary == nil || summary["parts"].(float64) != 2 || summary["restorable"].(float64) != 2 {
		t.Fatalf("the stray scan's lost = %v", strays["lost"])
	}

	w, _ := c.json(http.MethodPost, "/api/vault/lost-parts/scan", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("scan start: %d %s", w.Code, w.Body.String())
	}
	scan, _ := c.await("/api/vault/lost-parts/scan")["result"].(map[string]any)
	if scan == nil || scan["found"] != true || scan["parts"].(float64) != 2 || scan["files"].(float64) != 2 {
		t.Fatalf("scan = %v", scan)
	}
	items := scan["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	for _, item := range items {
		row := item.(map[string]any)
		if row["restorable"] != true || row["path"] == "" || row["provider_name"] == "" {
			t.Errorf("row = %v", row)
		}
	}
	emptied := 0
	for _, a := range scan["accounts"].([]any) {
		if a.(map[string]any)["emptied"] == true {
			emptied++
		}
	}
	if emptied != 1 {
		t.Errorf("%d accounts read as emptied, want 1", emptied)
	}

	if w, _ := c.json(http.MethodPost, "/api/vault/lost-parts/restore", map[string]any{"provider_ids": "nope"}); w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed restore: %d", w.Code)
	}
	w, _ = c.json(http.MethodPost, "/api/vault/lost-parts/restore", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("restore start: %d %s", w.Code, w.Body.String())
	}
	done := c.await("/api/vault/lost-parts/restore")
	report, _ := done["result"].(map[string]any)
	if report == nil || report["parts"].(float64) != 2 || report["objects"].(float64) != 2 || report["failed"].(float64) != 0 {
		t.Fatalf("restore = %v (%v)", report, done["error"])
	}

	// The scan it made stale is gone, and a fresh one finds nothing.
	if _, status := c.json(http.MethodGet, "/api/vault/lost-parts/scan", nil); status["id"] != nil {
		t.Errorf("the scan from before the restore is still being served: %v", status)
	}
	c.json(http.MethodPost, "/api/vault/lost-parts/scan", nil)
	if again, _ := c.await("/api/vault/lost-parts/scan")["result"].(map[string]any); again == nil || again["found"] != false {
		t.Fatalf("scan after the restore = %v", again)
	}

	// Read through the parts that were put back.
	emptyAccountFolder(t, roots[0])
	got := c.do(http.MethodGet, "/api/files/"+first["id"].(string)+"/content", nil, "")
	if got.Code != http.StatusOK || got.Body.String() != string(body) {
		t.Fatalf("reading through the restored part: %d", got.Code)
	}
}

func TestLostPartsNeedTheVaultUnlocked(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.json(http.MethodPost, "/api/vault/lock", nil)
	for _, route := range [][2]string{
		{http.MethodPost, "/api/vault/lost-parts/scan"},
		{http.MethodGet, "/api/vault/lost-parts/scan"},
		{http.MethodPost, "/api/vault/lost-parts/restore"},
		{http.MethodGet, "/api/vault/lost-parts/restore"},
		{http.MethodPost, "/api/vault/lost-parts/restore/stop"},
	} {
		if w, _ := c.json(route[0], route[1], nil); w.Code < 400 {
			t.Errorf("%s %s on a locked vault: %d", route[0], route[1], w.Code)
		}
	}
}

// One restore at a time: the same ask joins, a different one is refused, a
// stop lets the run finish with what it had, and a lock takes it away.
func TestLostRestoreWatchJoinsRefusesAndStops(t *testing.T) {
	jw := newJobWatch[vault.LostRestoreProgress, vault.LostRestoreReport]()
	started := make(chan struct{})
	run := func(ctx context.Context, onProgress func(vault.LostRestoreProgress)) (*vault.LostRestoreReport, error) {
		onProgress(vault.LostRestoreProgress{Stage: vault.RestoreStageRestoring, Objects: 4, Done: 1})
		close(started)
		<-ctx.Done()
		return &vault.LostRestoreReport{Parts: 1, Stopped: true, StoppedBy: "it was stopped"}, nil
	}

	first, outcome := jw.start("a", run, time.Minute)
	if outcome != jobStarted || !first.Running {
		t.Fatalf("start = %+v, %v", first, outcome)
	}
	<-started
	if joined, outcome := jw.start("a", run, time.Minute); outcome != jobJoined || joined.ID != first.ID {
		t.Fatalf("the same ask = %+v, %v", joined, outcome)
	}
	if _, outcome := jw.start("b", run, time.Minute); outcome != jobBusy {
		t.Fatalf("another ask while one runs: %v", outcome)
	}
	if at := jw.status().Progress; at == nil || at.Objects != 4 || at.Done != 1 {
		t.Fatalf("progress = %+v", at)
	}

	jw.cancel()
	awaitScan(t, "the stopped restore to finish", func() bool { return !jw.running() })
	if done := jw.status(); done.Result == nil || !done.Result.Stopped || done.Result.Parts != 1 {
		t.Fatalf("a stopped restore = %+v", done)
	}

	jw.stopAll()
	if gone := jw.status(); gone.ID != "" {
		t.Fatalf("after a lock the watch still holds %+v", gone)
	}
}
