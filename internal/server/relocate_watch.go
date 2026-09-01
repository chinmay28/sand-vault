package server

import (
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/google/uuid"
)

// The relocations running right now, so the browser can draw one — and so that
// one can outlive the page that started it.
//
// This is the import watch's shape applied to the other long transfer in the
// app (see import_watch.go for the reasoning it carries): a relocation is one
// long POST, held open by default so that closing the tab cancels it, and a
// folder of films moving between clouds is exactly the request nobody should
// have to keep a page open for. So a relocation can be asked to detach — the
// web client always asks — runs on a context of its own when it is, reports
// here either way, and is remembered for a while after it ends so the outcome
// is still there when somebody comes back to look.
//
// The lifetime line is the same one imports drew, for the same reason:
// process, not request, and not restart. A relocation commits file by file,
// so re-running the same move is how it resumes, and there is nothing worth
// writing down.

// maxDetachedRelocations is how many relocations may be running detached at
// once. Lower than the imports' ceiling on purpose: an import loads the source
// machine and this machine, where a relocation reads from and writes to the
// same set of connected clouds — the accounts every other detached relocation
// is using too — and each run already keeps several part objects in flight
// (see vault.relocateWindow).
const maxDetachedRelocations = 2

// relocateRun is one relocation, running or lately finished.
type relocateRun struct {
	ID string `json:"id"`

	// Label names what was picked, for the card: a file or folder's path, or
	// "12 items" for a selection.
	Label string `json:"label"`

	// Detached says this one outlives the page that started it.
	Detached bool `json:"detached,omitempty"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// At is where it has got to — which file, and how many bytes have crossed
	// between accounts against the plan's total. The zero value until the
	// first report lands.
	At vault.RelocationProgress `json:"at"`

	// Rate is how fast bytes are crossing, in bytes per second, or zero when
	// there is nothing honest to say — the run has just started, or nothing
	// has moved for a while. A file being rebuilt reports its bytes only when
	// it commits, so the rate goes quiet rather than lying through one.
	Rate float64 `json:"rate,omitempty"`

	// Done, and what it came to. A finished run is only ever a detached one:
	// a foreground relocation's answer goes back down the request that
	// started it, and is forgotten here the moment that request ends.
	Done      bool                    `json:"done,omitempty"`
	Report    *vault.RelocationReport `json:"report,omitempty"`
	Error     string                  `json:"error,omitempty"`
	Cancelled bool                    `json:"cancelled,omitempty"`
}

// relocateWatch holds them.
type relocateWatch struct {
	mu   sync.Mutex
	runs map[string]*watchedRelocation

	// now is time.Now everywhere but in tests driving the speed reading.
	now func() time.Time
}

// watchedRelocation is a run plus the handle to stop it, and the last sample
// the speed was measured against.
type watchedRelocation struct {
	run    relocateRun
	cancel func()

	bytes int64
	at    time.Time
}

func newRelocateWatch() *relocateWatch {
	return &relocateWatch{runs: map[string]*watchedRelocation{}, now: time.Now}
}

// errTooManyRelocations is refusing to detach one more.
type errTooManyRelocations struct{}

func (errTooManyRelocations) Error() string {
	return "too many moves are already running in the background; wait for one to finish or stop it"
}

// start registers a relocation and hands back the handle to report it with.
func (w *relocateWatch) start(label string, detached bool, cancel func()) (*relocateTicket, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked()

	if detached {
		running := 0
		for _, entry := range w.runs {
			if entry.run.Detached && !entry.run.Done {
				running++
			}
		}
		if running >= maxDetachedRelocations {
			return nil, errTooManyRelocations{}
		}
	}

	id := uuid.NewString()
	w.runs[id] = &watchedRelocation{
		run:    relocateRun{ID: id, Label: label, Detached: detached, StartedAt: w.now()},
		cancel: cancel,
	}
	return &relocateTicket{watch: w, id: id}, nil
}

// relocateTicket is one running relocation's handle on its own entry.
type relocateTicket struct {
	watch *relocateWatch
	id    string
}

// update records where the relocation has got to. It is called from the
// goroutines doing the copying, per object landed, and does nothing but take a
// lock and do a little arithmetic.
func (t *relocateTicket) update(at vault.RelocationProgress) {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()

	entry, ok := t.watch.runs[t.id]
	if !ok {
		return
	}
	entry.run.At = at
	entry.measure(at, t.watch.now())
}

// measure folds one progress report into the speed reading. One continuous
// measurement over the whole run, because unlike an import's two stages this
// is one kind of work throughout: bytes crossing between accounts.
func (e *watchedRelocation) measure(at vault.RelocationProgress, now time.Time) {
	if e.at.IsZero() {
		e.bytes, e.at = at.Bytes, now
		return
	}

	elapsed := now.Sub(e.at).Seconds()
	moved := at.Bytes - e.bytes
	if elapsed < rateSample.Seconds() || moved <= 0 {
		// Too soon, or nothing new. Leave the sample where it is so the next
		// report measures against a wider window rather than a noisier one.
		return
	}

	sample := float64(moved) / elapsed
	if e.run.Rate == 0 {
		e.run.Rate = sample
	} else {
		e.run.Rate = e.run.Rate*(1-rateWeight) + sample*rateWeight
	}
	e.bytes, e.at = at.Bytes, now
}

// done forgets the run — what a foreground relocation ends with, however it
// came out: its answer went back down its own request.
func (t *relocateTicket) done() {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()
	delete(t.watch.runs, t.id)
}

// finish is what a detached relocation ends with instead: the result stays,
// because there is no longer a request for it to be the answer to.
func (t *relocateTicket) finish(report *vault.RelocationReport, err error, cancelled bool) {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()

	entry, ok := t.watch.runs[t.id]
	if !ok {
		// Dismissed while it ran.
		return
	}
	at := time.Now()
	entry.run.Done, entry.run.FinishedAt, entry.run.Cancelled = true, &at, cancelled
	entry.run.Report = report
	if err != nil {
		entry.run.Error = err.Error()
	}
	entry.cancel = nil
	t.watch.trimLocked()
}

// all answers with every run, running or lately finished, oldest first.
// Copies, not pointers — the goroutines doing the copying go on writing to
// their entries the moment the lock is released.
func (w *relocateWatch) all() []relocateRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked()

	out := make([]relocateRun, 0, len(w.runs))
	for _, entry := range w.runs {
		out = append(out, w.snapshotLocked(entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// forRun answers with one run by ID, or nil. A copy, as above.
func (w *relocateWatch) forRun(id string) *relocateRun {
	w.mu.Lock()
	defer w.mu.Unlock()

	entry, ok := w.runs[id]
	if !ok {
		return nil
	}
	run := w.snapshotLocked(entry)
	return &run
}

// running is how many relocations are moving bytes right now, detached or not.
// The auto-lock asks, exactly as it asks the imports: a transfer is use of the
// vault, and locking the keys out from under one would fail it for no reason.
func (w *relocateWatch) running() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := 0
	for _, entry := range w.runs {
		if !entry.run.Done {
			n++
		}
	}
	return n
}

// snapshotLocked copies one run for handing out, dropping a speed nothing has
// moved behind for a while — a stalled transfer claiming 20 MB/s is worse
// than one saying nothing.
func (w *relocateWatch) snapshotLocked(entry *watchedRelocation) relocateRun {
	run := entry.run
	if run.Done || (!entry.at.IsZero() && w.now().Sub(entry.at) > staleRateAfter) {
		run.Rate = 0
	}
	return run
}

// stop cancels a running relocation and forgets a finished one — the same
// gesture from the outside. It reports whether there was anything by that name.
func (w *relocateWatch) stop(id string) bool {
	w.mu.Lock()
	entry, ok := w.runs[id]
	if !ok {
		w.mu.Unlock()
		return false
	}
	cancel := entry.cancel
	if entry.run.Done {
		delete(w.runs, id)
	}
	w.mu.Unlock()

	// Outside the lock: cancelling wakes the run's own goroutine, which comes
	// back here to record how it ended. The entry stays until it does, so a
	// stopped run is still listed — as stopping — rather than vanishing before
	// it has let go of the accounts.
	if cancel != nil {
		cancel()
	}
	return true
}

// stopAll cancels every running relocation. Locking the vault calls it: the
// index the next commit would write to is about to leave memory, and every
// file already committed stays moved.
func (w *relocateWatch) stopAll() {
	w.mu.Lock()
	cancels := make([]func(), 0, len(w.runs))
	for _, entry := range w.runs {
		if entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
	}
	w.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// sweepLocked drops finished runs nobody came back for, on the imports' own
// clock: long enough to make a cup of tea, short enough not to be yesterday's
// news presented as current.
func (w *relocateWatch) sweepLocked() {
	for id, entry := range w.runs {
		if entry.run.Done && entry.run.FinishedAt != nil && time.Since(*entry.run.FinishedAt) > finishedImportTTL {
			delete(w.runs, id)
		}
	}
}

// trimLocked keeps the number of remembered results down, oldest first.
func (w *relocateWatch) trimLocked() {
	finished := make([]relocateRun, 0, len(w.runs))
	for _, entry := range w.runs {
		if entry.run.Done {
			finished = append(finished, entry.run)
		}
	}
	if len(finished) <= maxFinishedImports {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].StartedAt.Before(finished[j].StartedAt) })
	for _, run := range finished[:len(finished)-maxFinishedImports] {
		delete(w.runs, run.ID)
	}
}
