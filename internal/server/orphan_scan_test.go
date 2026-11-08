package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// A scan that waits to be let go, so a test can look at the watch mid-run.
type heldScan struct {
	release chan struct{}
	ctx     chan context.Context
}

func newHeldScan() *heldScan {
	return &heldScan{release: make(chan struct{}), ctx: make(chan context.Context, 1)}
}

func (h *heldScan) run(ctx context.Context, onProgress func(vault.OrphanScanProgress)) (*vault.OrphanScan, error) {
	h.ctx <- ctx
	onProgress(vault.OrphanScanProgress{Stage: vault.ScanStageListing, Roots: 2, Listed: 1, Waiting: []string{"slow"}})
	select {
	case <-h.release:
		return &vault.OrphanScan{Found: true, Archives: 1}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func awaitScan(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScanWatchRunsOneScanAndKeepsItsAnswer(t *testing.T) {
	sw := newScanWatch()
	if status := sw.status(); status.ID != "" || status.Running {
		t.Fatalf("a fresh watch reported a run: %+v", status)
	}

	held := newHeldScan()
	first, outcome := sw.start("", held.run, time.Minute)
	if outcome != jobStarted || first.ID == "" || !first.Running || first.StartedAt == nil {
		t.Fatalf("start = %+v, %v; want a fresh running run", first, outcome)
	}
	<-held.ctx

	// A second ask while the first is going joins it rather than listing
	// every account twice.
	joined, outcome := sw.start("", held.run, time.Minute)
	if outcome != jobJoined || joined.ID != first.ID {
		t.Fatalf("a second start made a new run: %+v, outcome=%v", joined, outcome)
	}

	// Where it has got to is what the scan last said.
	awaitScan(t, "the first progress report", func() bool {
		at := sw.status().Progress
		return at != nil && at.Listed == 1
	})
	if at := sw.status().Progress; at.Roots != 2 || len(at.Waiting) != 1 || at.Waiting[0] != "slow" {
		t.Errorf("progress = %+v, want 1 of 2 with slow waiting", at)
	}

	close(held.release)
	awaitScan(t, "the scan to finish", func() bool { return !sw.running() })
	done := sw.status()
	if done.ID != first.ID || done.Running || done.FinishedAt == nil || done.Progress != nil {
		t.Fatalf("finished status = %+v, want the same run, not running, with a finish time", done)
	}
	if done.Result == nil || !done.Result.Found || done.Error != "" {
		t.Fatalf("finished status carries %+v / %q, want the scan's answer", done.Result, done.Error)
	}

	// The answer stays until something replaces it — and a new start does.
	if again := sw.status(); again.Result == nil {
		t.Fatal("the answer went away on a second look")
	}
	next := newHeldScan()
	fresh, outcome := sw.start("", next.run, time.Minute)
	if outcome != jobStarted || fresh.ID == first.ID || !fresh.Running || fresh.Result != nil {
		t.Fatalf("a start after the finish = %+v, %v; want a fresh run with no answer yet", fresh, outcome)
	}
	<-next.ctx
	close(next.release)
	awaitScan(t, "the second scan to finish", func() bool { return !sw.running() })
}

func TestScanWatchReportsAScanThatFailed(t *testing.T) {
	sw := newScanWatch()
	sw.start("", func(ctx context.Context, _ func(vault.OrphanScanProgress)) (*vault.OrphanScan, error) {
		return nil, errors.New("the index is locked")
	}, time.Minute)
	awaitScan(t, "the failing scan to finish", func() bool { return !sw.running() })
	status := sw.status()
	if status.Running || status.Result != nil || status.Error != "the index is locked" {
		t.Fatalf("status = %+v, want the error and no scan", status)
	}
}

// Locking stops the scan and drops it altogether: the keys it read with are
// going, and so are the file paths in what it found. A run that is cancelled
// must not come back from the dead to leave its error, either.
func TestScanWatchStopAllCancelsAndForgets(t *testing.T) {
	sw := newScanWatch()
	held := newHeldScan()
	sw.start("", held.run, time.Minute)
	ctx := <-held.ctx

	sw.stopAll()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the running scan was not cancelled")
	}
	// Give the cancelled run every chance to write itself back in.
	time.Sleep(20 * time.Millisecond)
	if status := sw.status(); status.ID != "" || status.Error != "" || status.Result != nil {
		t.Fatalf("after stopAll the watch still reports %+v", status)
	}

	// Stopping with nothing running is nothing.
	sw.stopAll()
	if sw.running() {
		t.Error("running after stopping an empty watch")
	}
}

// A scan that overruns its ceiling ends as a failed one, not a running one.
func TestScanWatchTimesOut(t *testing.T) {
	sw := newScanWatch()
	held := newHeldScan()
	sw.start("", held.run, 10*time.Millisecond)
	<-held.ctx
	awaitScan(t, "the scan to time out", func() bool { return !sw.running() })
	if status := sw.status(); status.Error == "" || status.Result != nil {
		t.Fatalf("a timed-out scan reported %+v, want an error", status)
	}
}

// The same scan over HTTP: started, left to run, watched to the end, and
// found waiting by a page that comes back — then gone with the lock.
func TestStrayScanRunsDetachedAndIsFoundOnReturn(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	doomed := c.upload("gone.txt", "/", []byte("deleted while a cloud was away"))
	c.abandonPartsOn(roots[0], doomed["id"].(string), "drive-personal-again")

	// Nothing has been asked yet.
	w, body := c.json(http.MethodGet, "/api/vault/orphans/scan", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status before any scan: %d %s", w.Code, w.Body.String())
	}
	if running, _ := body["running"].(bool); running || body["id"] != nil {
		t.Fatalf("a scan was reported before one was asked for: %v", body)
	}

	w, body = c.json(http.MethodPost, "/api/vault/orphans/scan", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("the started run has no id: %v", body)
	}

	// Polled the way the page polls it, until it is done.
	var done map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, body = c.json(http.MethodGet, "/api/vault/orphans/scan", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("poll: %d %s", w.Code, w.Body.String())
		}
		if body["id"] != id {
			t.Fatalf("the poll answered for a different run: %v", body)
		}
		if running, _ := body["running"].(bool); !running {
			done = body
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scan did not finish: %v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	scan, _ := done["result"].(map[string]any)
	if scan == nil {
		t.Fatalf("the finished run carries no scan: %v", done)
	}
	if found, _ := scan["found"].(bool); !found {
		t.Errorf("the detached scan missed the abandoned parts: %v", scan)
	}
	if done["finished_at"] == nil || done["progress"] != nil {
		t.Errorf("a finished run should carry a finish time and no progress: %v", done)
	}

	// A page coming back later finds the same answer, and asking again
	// while nothing is running starts a fresh run.
	if _, again := c.json(http.MethodGet, "/api/vault/orphans/scan", nil); again["id"] != id {
		t.Errorf("the answer did not wait for the page to come back: %v", again)
	}
	w, fresh := c.json(http.MethodPost, "/api/vault/orphans/scan", nil)
	if w.Code != http.StatusAccepted || fresh["id"] == id {
		t.Errorf("a second ask after the finish did not start afresh: %d %v", w.Code, fresh)
	}
	awaitScan(t, "the second scan", func() bool { return !c.server.strayScans.running() })

	// Locking takes the result with it.
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if status := c.server.strayScans.status(); status.ID != "" {
		t.Errorf("the scan's answer survived the lock: %+v", status)
	}
}

// The reattach over HTTP the way the browser runs it: started detached,
// polled to the end, its report waiting for a page that comes back, the
// stray scan's now-wrong answer forgotten — and the repair actually made.
func TestReattachRunsDetachedAndIsFoundOnReturn(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	file := c.upload("important.txt", "/", []byte("the file that lost a spare"))
	back := c.degradeByDisconnect(file, roots, "drive-personal-again")

	// A finished stray scan that lists the mislaid part.
	c.json(http.MethodPost, "/api/vault/orphans/scan", nil)
	awaitScan(t, "the stray scan", func() bool { return !c.server.strayScans.running() })
	if c.server.strayScans.status().Result == nil {
		t.Fatal("the stray scan left no answer to forget")
	}

	w, body := c.json(http.MethodGet, "/api/vault/orphans/reattaching", nil)
	if w.Code != http.StatusOK || body["id"] != nil {
		t.Fatalf("status before any reattach: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodPost, "/api/vault/orphans/reattaching", map[string]any{"provider_id": back})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	id, _ := body["id"].(string)
	if id == "" || body["scope"] != back {
		t.Fatalf("the started run = %v, want an id scoped to %s", body, back)
	}

	var done map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = c.json(http.MethodGet, "/api/vault/orphans/reattaching", nil)
		if body["id"] != id {
			t.Fatalf("the poll answered for a different run: %v", body)
		}
		if running, _ := body["running"].(bool); !running {
			done = body
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reattach did not finish: %v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	report, _ := done["result"].(map[string]any)
	if report == nil {
		t.Fatalf("the finished run carries no report: %v", done)
	}
	if shards, _ := report["shards"].(float64); shards != 1 {
		t.Errorf("recorded %v shard(s), want 1", report["shards"])
	}
	if c.degraded() != 0 {
		t.Errorf("the vault still reports %d degraded file(s)", c.degraded())
	}
	if status := c.server.strayScans.status(); status.ID != "" {
		t.Errorf("the stray scan still offers parts that were just recorded: %+v", status)
	}
	if _, again := c.json(http.MethodGet, "/api/vault/orphans/reattaching", nil); again["id"] != id {
		t.Errorf("the report did not wait for the page to come back: %v", again)
	}

	// Locking takes the report with it.
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if status := c.server.reattaches.status(); status.ID != "" {
		t.Errorf("the reattach's report survived the lock: %+v", status)
	}
}

func TestReattachStartNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.cookies = nil
	if w, _ := c.json(http.MethodPost, "/api/vault/orphans/reattaching", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/vault/orphans/reattaching without a session: %d, want 401", w.Code)
	}
}

// A reattach for another cloud while one runs is turned away, and the one
// running is left alone.
func TestReattachStartRefusesAnotherScopeWhileOneRuns(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	run, release, began := heldReattach()
	if _, outcome := c.server.reattaches.start("cloud-a", run, time.Minute); outcome != jobStarted {
		t.Fatalf("seeding the running reattach: %v", outcome)
	}
	<-began
	defer close(release)

	w, _ := c.json(http.MethodPost, "/api/vault/orphans/reattaching", map[string]any{"provider_id": "cloud-b"})
	if w.Code != http.StatusConflict {
		t.Fatalf("a second scope while one runs: %d %s, want 409", w.Code, w.Body.String())
	}
	if status := c.server.reattaches.status(); !status.Running || status.Scope != "cloud-a" {
		t.Errorf("the running reattach was disturbed: %+v", status)
	}
}

func heldReattach() (func(context.Context, func(vault.OrphanScanProgress)) (*vault.ReattachReport, error), chan struct{}, chan struct{}) {
	release := make(chan struct{})
	began := make(chan struct{}, 1)
	return func(ctx context.Context, _ func(vault.OrphanScanProgress)) (*vault.ReattachReport, error) {
		began <- struct{}{}
		select {
		case <-release:
			return &vault.ReattachReport{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, release, began
}
