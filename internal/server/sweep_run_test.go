package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// A sweep's scope is what it was asked to erase, whatever order it was named
// in: the same set joins, a different set does not, and "everything" is its
// own scope.
func TestSweepScopeNamesTheSetNotTheOrder(t *testing.T) {
	a := vault.OrphanTarget{ProviderID: "p1", ArchiveID: "a1"}
	b := vault.OrphanTarget{ProviderID: "p2", ArchiveID: "a2"}
	c := vault.OrphanTarget{ProviderID: "p1", ArchiveID: "a2"}

	if got := sweepScope(nil); got != "" {
		t.Errorf("everything has scope %q, want empty", got)
	}
	if sweepScope([]vault.OrphanTarget{a, b}) != sweepScope([]vault.OrphanTarget{b, a}) {
		t.Error("the same targets in another order are a different scope")
	}
	if sweepScope([]vault.OrphanTarget{a, b}) != sweepScope([]vault.OrphanTarget{a, b, a}) {
		t.Error("a target named twice changed the scope")
	}
	if sweepScope([]vault.OrphanTarget{a, b}) == sweepScope([]vault.OrphanTarget{a, c}) {
		t.Error("different targets share a scope")
	}
}

// The sweep over HTTP the way the browser runs it: started detached, polled
// to the end, its report waiting for a page that comes back, the stray scan's
// now-wrong answer forgotten, the parts actually gone — and the report gone
// with the lock.
func TestSweepRunsDetachedAndIsFoundOnReturn(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	doomed := c.upload("gone.txt", "/", []byte("deleted while a cloud was away"))
	c.abandonPartsOn(roots[0], doomed["id"].(string), "drive-personal-again")
	before := sandFilesIn(t, roots[0])

	c.json(http.MethodPost, "/api/vault/orphans/scan", nil)
	awaitScan(t, "the stray scan", func() bool { return !c.server.strayScans.running() })

	w, body := c.json(http.MethodPost, "/api/vault/orphans/sweeping", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("the started run has no id: %v", body)
	}

	var done map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = c.json(http.MethodGet, "/api/vault/orphans/sweeping", nil)
		if body["id"] != id {
			t.Fatalf("the poll answered for a different run: %v", body)
		}
		if running, _ := body["running"].(bool); !running {
			done = body
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep did not finish: %v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	report, _ := done["result"].(map[string]any)
	if report == nil {
		t.Fatalf("the finished run carries no report: %v (error %v)", done, done["error"])
	}
	if deleted, _ := report["deleted"].(float64); deleted < 1 {
		t.Errorf("erased %v object(s), want at least 1", report["deleted"])
	}
	if after := sandFilesIn(t, roots[0]); after >= before {
		t.Errorf("the abandoned parts are still there: %d object(s), was %d", after, before)
	}
	if status := c.server.strayScans.status(); status.ID != "" {
		t.Errorf("the stray scan still offers what was just erased: %+v", status)
	}

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if status := c.server.sweeps.status(); status.ID != "" {
		t.Errorf("the sweep's report survived the lock: %+v", status)
	}
}

// Another sweep while one runs is turned away, and the one running is left
// alone; the same ask joins it.
func TestSweepStartJoinsTheSameAndRefusesAnother(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	release := make(chan struct{})
	began := make(chan struct{}, 1)
	held := func(ctx context.Context, _ func(sweepProgress)) (*vault.OrphanSweepReport, error) {
		began <- struct{}{}
		select {
		case <-release:
			return &vault.OrphanSweepReport{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	first, _ := c.server.sweeps.start("", held, time.Minute)
	<-began
	defer close(release)

	w, body := c.json(http.MethodPost, "/api/vault/orphans/sweeping", map[string]any{"targets": []any{}})
	if w.Code != http.StatusOK || body["id"] != first.ID {
		t.Errorf("the same ask did not join: %d %v", w.Code, body)
	}
	w, _ = c.json(http.MethodPost, "/api/vault/orphans/sweeping", map[string]any{
		"targets": []map[string]string{{"provider_id": "p", "archive_id": "a"}},
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("another sweep while one runs: %d %s, want 409", w.Code, w.Body.String())
	}
	if status := c.server.sweeps.status(); !status.Running || status.ID != first.ID {
		t.Errorf("the running sweep was disturbed: %+v", status)
	}
}

func TestSweepStartNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.cookies = nil
	if w, _ := c.json(http.MethodPost, "/api/vault/orphans/sweeping", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/vault/orphans/sweeping without a session: %d, want 401", w.Code)
	}
}
