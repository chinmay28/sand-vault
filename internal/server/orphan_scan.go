package server

import (
	"context"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/google/uuid"
)

// The stray-parts scan as something that runs on its own, rather than inside
// the request that asked for it.
//
// GET /api/vault/orphans is one request that answers at the end, and for the
// banner over the file list that is the right shape: the app asks when the
// set of connected clouds changes and nobody is waiting on it. The settings
// panel is different. Somebody opened it to be told what is true now, the
// answer is a full listing of every account — minutes, on a slow cloud — and
// a page that is navigated away from cancels the request it was waiting on,
// so the minutes were spent for nothing and are spent again next time.
//
// So the panel's scan is started here, on a context of its own, and the page
// polls for it: where it has got to while it runs, and what it found once it
// is done. Closing the page keeps it; a page that comes back finds either the
// scan still going or the result waiting. One scan at a time — a second ask
// while one is running joins it, since the question is the same question —
// and the result is held until the next scan replaces it or the vault is
// locked, which takes the keys the result was read with and the file paths
// in it along with them.
//
// It lives in memory for as long as the process does and is never written
// down, for the reason the transfer watch gives: there is nothing worth
// writing. Running the scan again is how it resumes.

// strayScanTimeout bounds a listing of one account, the reattach that
// re-lists it, and the detached scan of every account — which gets the same
// ceiling, since it too is as slow as its slowest cloud. A listing that is cut
// off reads as "nothing there", which is the one wrong answer, so the bound is
// generous.
const strayScanTimeout = 10 * time.Minute

// scanWatch holds the scan that is running, or the last one that ran.
type scanWatch struct {
	mu  sync.Mutex
	run *scanRun
}

// scanRun is one scan, from the moment it was asked for until it is replaced.
type scanRun struct {
	id        string
	startedAt time.Time
	cancel    context.CancelFunc

	// Everything below is guarded by the watch's mutex.
	progress   vault.OrphanScanProgress
	finishedAt time.Time
	scan       *vault.OrphanScan
	err        string
}

// scanStatus is what a poll is answered with: the scan that is running, with
// where it has got to, or the last one that ran, with what it found — or
// neither, when nothing has been asked since the vault was opened.
type scanStatus struct {
	// ID names the run, so a page can tell a fresh scan from the one it was
	// already watching. Empty when there is no run at all.
	ID      string `json:"id,omitempty"`
	Running bool   `json:"running"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Progress is where a running scan has got to. Absent once it is done.
	Progress *vault.OrphanScanProgress `json:"progress,omitempty"`

	// Scan is what a finished scan found, and Error why one found nothing.
	// A scan that was stopped — by a lock — is gone altogether rather than
	// reported, since what it had read was read with keys that are gone.
	Scan  *vault.OrphanScan `json:"scan,omitempty"`
	Error string            `json:"error,omitempty"`
}

// scanFunc is the scan itself, given the context to run under and the place
// to report progress to. It is a parameter so the watch can be tested without
// a vault behind it.
type scanFunc func(ctx context.Context, onProgress func(vault.OrphanScanProgress)) (*vault.OrphanScan, error)

func newScanWatch() *scanWatch {
	return &scanWatch{}
}

// start begins a scan, or joins the one already running.
//
// The answer is the status either way, and started says which: true for a
// fresh run, false for one that was already going — the caller answers 202
// for the first and 200 for the second. The scan runs on a context of its own
// under the timeout given, and reports here as it goes; nothing it reads is
// read back by it.
func (sw *scanWatch) start(run scanFunc, timeout time.Duration) (status scanStatus, started bool) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.run != nil && sw.run.finishedAt.IsZero() {
		return sw.statusLocked(), false
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	current := &scanRun{
		id:        uuid.NewString(),
		startedAt: time.Now(),
		cancel:    cancel,
	}
	sw.run = current

	go func() {
		defer cancel()
		scan, err := run(ctx, func(at vault.OrphanScanProgress) {
			sw.mu.Lock()
			if sw.run == current {
				current.progress = at
			}
			sw.mu.Unlock()
		})

		sw.mu.Lock()
		defer sw.mu.Unlock()
		// A run that was stopped is already gone from the watch, and what it
		// found — or the error it stopped with — is nobody's business now.
		if sw.run != current {
			return
		}
		current.finishedAt = time.Now()
		if err != nil {
			current.err = err.Error()
			return
		}
		current.scan = scan
	}()

	return sw.statusLocked(), true
}

// status is the run as it stands.
func (sw *scanWatch) status() scanStatus {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.statusLocked()
}

func (sw *scanWatch) statusLocked() scanStatus {
	run := sw.run
	if run == nil {
		return scanStatus{}
	}
	status := scanStatus{
		ID:        run.id,
		Running:   run.finishedAt.IsZero(),
		StartedAt: &run.startedAt,
	}
	if status.Running {
		at := run.progress
		status.Progress = &at
		return status
	}
	finished := run.finishedAt
	status.FinishedAt = &finished
	status.Scan = run.scan
	status.Error = run.err
	return status
}

// stopAll cancels a running scan and forgets whatever the last one found.
// Locking the vault calls it: the result names file paths and was read with
// keys that are about to leave memory, and a scan still going would come
// back with the same, to nobody.
func (sw *scanWatch) stopAll() {
	sw.mu.Lock()
	run := sw.run
	sw.run = nil
	sw.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// running reports whether a scan is going right now.
func (sw *scanWatch) running() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.run != nil && sw.run.finishedAt.IsZero()
}
