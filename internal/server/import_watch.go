package server

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/google/uuid"
)

// The imports running right now, so the browser can draw one — and so that one
// can outlive the page that started it.
//
// An import is one long POST. Left in the foreground it is exactly that: the
// request holds the connection open for as long as the transfer takes, and
// closing the tab cancels the transfer, because the handler's context is the
// request's. That is the right default — it is what makes "nothing is running
// that you did not ask for" true — but it is the wrong answer for one 18 GB
// film, where the page has to stay open for an hour to get a file that the
// machine could perfectly well fetch on its own.
//
// So an import can be asked to detach. A detached one runs on a context of its
// own, reports here exactly as a foreground one does, and is remembered for a
// while after it ends so the summary is still there when somebody comes back to
// look. Its lifetime is the *process*, not the request: closing the page keeps
// it, and restarting SAND does not. That line is deliberate. Making it survive
// a restart would mean writing down what is in flight, and an import has
// nothing worth writing down — re-running one is how it resumes, at whole-file
// granularity, which is a property of the vault rather than of any bookkeeping
// here. See vault.ImportFromSource.
//
// Nothing in this file is ever read by an import. It is written to, and read
// back out to be shown.

// maxDetachedImports is how many imports may be running detached at once.
//
// A foreground import is bounded by somebody sitting in front of it. A detached
// one is not, and four selections walking four machines at once is already more
// than a home connection has to give — past that they only slow each other
// down while looking like progress.
const maxDetachedImports = 4

// finishedImportTTL is how long a detached import's result is kept after it
// ends, for somebody who was not watching when it did.
//
// Long enough to make a cup of tea and come back, short enough that a browser
// opened tomorrow is not shown yesterday's news as though it were current. It
// is also dismissable, which is the path that actually matters — this is the
// backstop for the page that was never reopened.
const finishedImportTTL = 30 * time.Minute

// maxFinishedImports caps how many results are held at once, oldest dropped
// first, so a night of hourly imports cannot grow the map without bound.
const maxFinishedImports = 8

// rateSample is how far apart two progress reports have to be before the speed
// between them is worth believing.
//
// Progress arrives every few megabytes, which on a fast local transfer can be
// several times a second — dividing a small distance by a small time is how a
// speed reading ends up swinging between 40 MB/s and 900 MB/s while the
// transfer itself is perfectly steady.
const rateSample = 500 * time.Millisecond

// rateWeight is how much of the newest sample goes into the speed shown.
//
// A quarter, so the number settles within a few seconds of a real change and
// does not jitter with the ordinary unevenness of a network. It is a moving
// average rather than an average since the file started, deliberately: what
// somebody watching wants is what it is doing now, not what it managed
// overall — the second is only interesting once it is over.
const rateWeight = 0.25

// staleRateAfter is how long a speed survives with nothing moving behind it.
//
// A transfer that has stalled must not go on claiming 20 MB/s: past this the
// speed is dropped rather than held, and the bytes count — which has not moved
// either — is what says where it got to. Long enough to ride out a slow chunk
// on a wide erasure scheme.
const staleRateAfter = 12 * time.Second

// importRun is one import, running or lately finished.
type importRun struct {
	ID string `json:"id"`

	// Source is the machine it is pulling from, Dest the vault folder it is
	// landing in, and Vault the sub vault that folder is inside, if any.
	Source string `json:"source"`
	Dest   string `json:"dest"`
	Vault  string `json:"vault,omitempty"`

	// Detached says this one outlives the page that started it.
	Detached bool `json:"detached,omitempty"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// At is where it has got to, and is the zero value until the first file is
	// picked up — a selection of ten thousand files spends a moment being
	// walked before anything moves.
	At vault.ImportProgress `json:"at"`

	// Rate is how fast the current stage is moving, in bytes per second, or
	// zero when there is nothing to say yet — the file has just started, or
	// nothing has moved for a while.
	//
	// Per stage rather than per file, because the two stages are two different
	// speeds: coming down is the source's upstream and going up is this
	// machine's, and averaging them would describe neither. It is what the
	// stage is doing now rather than what it has averaged, since a number
	// that spent the last minute wrong keeps saying so long after it stops
	// being true.
	Rate float64 `json:"rate,omitempty"`

	// Done, and what it came to. A finished run is only ever a detached one:
	// a foreground import's answer goes back down the request that started it,
	// and is forgotten here the moment that request ends.
	Done      bool                 `json:"done,omitempty"`
	Summary   *vault.ImportSummary `json:"summary,omitempty"`
	Error     string               `json:"error,omitempty"`
	Cancelled bool                 `json:"cancelled,omitempty"`
}

// importWatch holds them.
type importWatch struct {
	mu   sync.Mutex
	runs map[string]*watchedImport

	// now is time.Now everywhere but in the tests that have to drive the speed
	// reading, which is arithmetic over timestamps and untestable against a
	// clock that will not hold still.
	now func() time.Time
}

// watchedImport is a run plus the handle to stop it, and what the speed reading
// is measured against.
type watchedImport struct {
	run    importRun
	cancel func()

	// The last sample the speed was worked out from: which stage of which file
	// it was, how much had gone, and when. A new stage starts the measurement
	// over rather than carrying a number across a boundary it does not
	// describe.
	stage string
	done  int64
	at    time.Time
}

func newImportWatch() *importWatch {
	return &importWatch{runs: map[string]*watchedImport{}, now: time.Now}
}

// errTooManyImports is refusing to detach one more.
type errTooManyImports struct{}

func (errTooManyImports) Error() string {
	return "too many imports are already running in the background; wait for one to finish or stop it"
}

// start registers an import and hands back the handle to report it with.
//
// cancel is what stopping it calls, and is the detached import's answer to a
// question a foreground one never has to ask: a request that has gone away
// takes its transfer with it, and a detached one has to be stopped on purpose.
func (w *importWatch) start(source, dest, scope string, detached bool, cancel func()) (*importTicket, error) {
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
		if running >= maxDetachedImports {
			return nil, errTooManyImports{}
		}
	}

	id := uuid.NewString()
	w.runs[id] = &watchedImport{
		run: importRun{
			ID: id, Source: source, Dest: dest, Vault: scope,
			Detached: detached, StartedAt: w.now(),
		},
		cancel: cancel,
	}
	return &importTicket{watch: w, id: id}, nil
}

// importTicket is one running import's handle on its own entry.
type importTicket struct {
	watch *importWatch
	id    string
}

// update records where the import has got to, and how fast it is getting
// there. It is called from the goroutine running the import, on every few
// megabytes, and does nothing but take a lock and do a little arithmetic.
func (t *importTicket) update(at vault.ImportProgress) {
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

// measure folds one progress report into the speed reading.
func (e *watchedImport) measure(at vault.ImportProgress, now time.Time) {
	stage := fmt.Sprintf("%d/%s", at.File, at.Stage)

	// A new file, or the same file's other half. Neither continues the last
	// measurement: the file that just started has moved nothing yet, and the
	// scatter is a different pipe from the fetch.
	if stage != e.stage {
		e.stage, e.done, e.at = stage, at.Done, now
		e.run.Rate = 0
		return
	}

	elapsed := now.Sub(e.at).Seconds()
	moved := at.Done - e.done
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
	e.done, e.at = at.Done, now
}

// done forgets the import. It is what a foreground import ends with, whether it
// finished, failed or was cancelled: the answer went back down its own request,
// and a progress bar for something that is no longer running is worse than none.
func (t *importTicket) done() {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()
	delete(t.watch.runs, t.id)
}

// finish is what a detached import ends with instead: the result stays, because
// there is no longer a request for it to be the answer to.
func (t *importTicket) finish(summary *vault.ImportSummary, err error, cancelled bool) {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()

	entry, ok := t.watch.runs[t.id]
	if !ok {
		// Dismissed while it ran, which is somebody saying they are not
		// interested in how it turned out.
		return
	}
	at := time.Now()
	entry.run.Done, entry.run.FinishedAt, entry.run.Cancelled = true, &at, cancelled
	entry.run.Summary = summary
	if err != nil {
		entry.run.Error = err.Error()
	}
	entry.cancel = nil
	t.watch.trimLocked()
}

// forSource answers with what one machine has running or lately finished,
// oldest first. Copies, not pointers: the import goroutine goes on writing to
// its entry the moment the lock is released, and a handler encoding one of
// these to JSON would otherwise be reading it as it changed.
func (w *importWatch) forSource(source string) []importRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked()

	out := make([]importRun, 0, len(w.runs))
	for _, entry := range w.runs {
		if entry.run.Source == source {
			out = append(out, w.snapshotLocked(entry))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// running is how many imports are moving bytes right now, detached or not.
//
// The auto-lock asks: a transfer is use of the vault as much as a browser
// clicking around is, and locking the keys out from under one would fail it for
// no reason. Progress also touches the external-activity clock, which covers
// the same ground from the other side; this is the one that does not depend on
// a tick having landed recently.
func (w *importWatch) running() int {
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

// forRun answers with one run by ID, or nil if there is no such thing. A copy,
// for the same reason forSource hands out copies.
func (w *importWatch) forRun(id string) *importRun {
	w.mu.Lock()
	defer w.mu.Unlock()

	entry, ok := w.runs[id]
	if !ok {
		return nil
	}
	run := w.snapshotLocked(entry)
	return &run
}

// snapshotLocked copies one run for handing out, dropping a speed that nothing
// has moved behind for a while. A stalled transfer saying it is doing 20 MB/s
// is worse than one saying nothing: the bytes have stopped either way, and only
// one of the two answers admits it.
func (w *importWatch) snapshotLocked(entry *watchedImport) importRun {
	run := entry.run
	if !run.Done && !entry.at.IsZero() && w.now().Sub(entry.at) > staleRateAfter {
		run.Rate = 0
	}
	if run.Done {
		run.Rate = 0
	}
	return run
}

// stop cancels a running import and forgets a finished one, which are the same
// gesture from the outside: this is not something I want to look at any more.
// It reports whether there was anything by that name.
func (w *importWatch) stop(id string) bool {
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

	// Outside the lock: cancelling wakes the import's own goroutine, which
	// comes back here to record how it ended. The entry stays until it does,
	// so a stopped import is still listed — as stopping — rather than
	// vanishing before it has actually let go of the connection.
	if cancel != nil {
		cancel()
	}
	return true
}

// stopAll cancels every running import. Locking the vault calls it: the keys
// those transfers are being sealed with are about to leave memory, and a
// transfer that carried on would only fail further in, having spent the
// bandwidth first.
func (w *importWatch) stopAll() {
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

// sweepLocked drops finished runs nobody came back for.
func (w *importWatch) sweepLocked() {
	for id, entry := range w.runs {
		if entry.run.Done && entry.run.FinishedAt != nil && time.Since(*entry.run.FinishedAt) > finishedImportTTL {
			delete(w.runs, id)
		}
	}
}

// trimLocked keeps the number of remembered results down, oldest first.
func (w *importWatch) trimLocked() {
	finished := make([]importRun, 0, len(w.runs))
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
