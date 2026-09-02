package server

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/google/uuid"
)

// The transfers running right now between this vault and a machine somebody
// has a login on, so the browser can draw one — and so that one can outlive
// the page that started it.
//
// A transfer is an import or an export, and the watch does not much care
// which: both are one long POST that works a selection a file at a time,
// both report the same progress, and both can be detached. What differs is
// which way the bytes go and what the summary at the end is called, and the
// run records both so the dialog can say.
//
// A transfer is one long POST. Left in the foreground it is exactly that: the
// request holds the connection open for as long as the transfer takes, and
// closing the tab cancels the transfer, because the handler's context is the
// request's. That is the right default — it is what makes "nothing is running
// that you did not ask for" true — but it is the wrong answer for one 18 GB
// film, where the page has to stay open for an hour to get a file that the
// machine could perfectly well fetch on its own.
//
// So a transfer can be asked to detach. A detached one runs on a context of
// its own, reports here exactly as a foreground one does, and is remembered
// for a while after it ends so the summary is still there when somebody comes
// back to look. Its lifetime is the *process*, not the request: closing the
// page keeps it, and restarting SAND does not. That line is deliberate. Making
// it survive a restart would mean writing down what is in flight, and a
// transfer has nothing worth writing down — re-running one is how it resumes,
// at whole-file granularity, which is a property of the vault rather than of
// any bookkeeping here. See vault.ImportFromSource and vault.ExportToSource.
//
// Nothing in this file is ever read by a transfer. It is written to, and read
// back out to be shown.

// transferKind is which way the bytes go.
type transferKind string

const (
	// transferImport brings a machine's files into the vault.
	transferImport transferKind = "import"

	// transferExport writes the vault's files out onto a machine.
	transferExport transferKind = "export"
)

// maxDetachedTransfers is how many transfers may be running detached at once,
// imports and exports together.
//
// A foreground transfer is bounded by somebody sitting in front of it. A
// detached one is not, and four selections walking four machines at once is
// already more than a home connection has to give — past that they only slow
// each other down while looking like progress.
const maxDetachedTransfers = 4

// finishedTransferTTL is how long a detached transfer's result is kept after
// it ends, for somebody who was not watching when it did.
//
// Long enough to make a cup of tea and come back, short enough that a browser
// opened tomorrow is not shown yesterday's news as though it were current. It
// is also dismissable, which is the path that actually matters — this is the
// backstop for the page that was never reopened.
const finishedTransferTTL = 30 * time.Minute

// maxFinishedTransfers caps how many results are held at once, oldest dropped
// first, so a night of hourly transfers cannot grow the map without bound.
const maxFinishedTransfers = 8

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

// transferRun is one transfer, running or lately finished.
type transferRun struct {
	ID string `json:"id"`

	// Kind is which way it is going.
	Kind transferKind `json:"kind"`

	// Source is the machine at the far end. Dest is where files are landing:
	// the vault folder for an import, the folder on the machine (relative to
	// the source's root) for an export. Vault is the sub vault the vault side
	// is in, if any.
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
	At vault.TransferProgress `json:"at"`

	// Rate is how fast the current stage is moving, in bytes per second, or
	// zero when there is nothing to say yet — the file has just started, or
	// nothing has moved for a while.
	//
	// Per stage rather than per file, because an import's two stages are two
	// different speeds: coming down is the source's upstream and going up is
	// this machine's, and averaging them would describe neither. It is what
	// the stage is doing now rather than what it has averaged, since a number
	// that spent the last minute wrong keeps saying so long after it stops
	// being true.
	Rate float64 `json:"rate,omitempty"`

	// Done, and what it came to. A finished run is only ever a detached one:
	// a foreground transfer's answer goes back down the request that started
	// it, and is forgotten here the moment that request ends.
	//
	// Summary is a *vault.ImportSummary or a *vault.ExportSummary, by Kind.
	// It is held loosely because the watch has no reason to look inside it —
	// it is carried for the dialog, which reads the kind and knows the shape.
	Done      bool   `json:"done,omitempty"`
	Summary   any    `json:"summary,omitempty"`
	Error     string `json:"error,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// transferWatch holds them.
type transferWatch struct {
	mu   sync.Mutex
	runs map[string]*watchedTransfer

	// now is time.Now everywhere but in the tests that have to drive the speed
	// reading, which is arithmetic over timestamps and untestable against a
	// clock that will not hold still.
	now func() time.Time
}

// watchedTransfer is a run plus the handle to stop it, and what the speed
// reading is measured against.
type watchedTransfer struct {
	run    transferRun
	cancel func()

	// The last sample the speed was worked out from: which stage of which file
	// it was, how much had gone, and when. A new stage starts the measurement
	// over rather than carrying a number across a boundary it does not
	// describe.
	stage string
	done  int64
	at    time.Time
}

func newTransferWatch() *transferWatch {
	return &transferWatch{runs: map[string]*watchedTransfer{}, now: time.Now}
}

// errTooManyTransfers is refusing to detach one more.
type errTooManyTransfers struct{}

func (errTooManyTransfers) Error() string {
	return "too many transfers are already running in the background; wait for one to finish or stop it"
}

// start registers a transfer and hands back the handle to report it with.
//
// cancel is what stopping it calls, and is the detached transfer's answer to a
// question a foreground one never has to ask: a request that has gone away
// takes its transfer with it, and a detached one has to be stopped on purpose.
func (w *transferWatch) start(kind transferKind, source, dest, scope string, detached bool, cancel func()) (*transferTicket, error) {
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
		if running >= maxDetachedTransfers {
			return nil, errTooManyTransfers{}
		}
	}

	id := uuid.NewString()
	w.runs[id] = &watchedTransfer{
		run: transferRun{
			ID: id, Kind: kind, Source: source, Dest: dest, Vault: scope,
			Detached: detached, StartedAt: w.now(),
		},
		cancel: cancel,
	}
	return &transferTicket{watch: w, id: id}, nil
}

// transferTicket is one running transfer's handle on its own entry.
type transferTicket struct {
	watch *transferWatch
	id    string
}

// update records where the transfer has got to, and how fast it is getting
// there. It is called from the goroutine running the transfer, on every few
// megabytes, and does nothing but take a lock and do a little arithmetic.
func (t *transferTicket) update(at vault.TransferProgress) {
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
func (e *watchedTransfer) measure(at vault.TransferProgress, now time.Time) {
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

// done forgets the transfer. It is what a foreground transfer ends with,
// whether it finished, failed or was cancelled: the answer went back down its
// own request, and a progress bar for something that is no longer running is
// worse than none.
func (t *transferTicket) done() {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()
	delete(t.watch.runs, t.id)
}

// finish is what a detached transfer ends with instead: the result stays,
// because there is no longer a request for it to be the answer to.
func (t *transferTicket) finish(summary any, err error, cancelled bool) {
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

// forSource answers with what one machine has running or lately finished in
// one direction, oldest first. Copies, not pointers: the transfer goroutine
// goes on writing to its entry the moment the lock is released, and a handler
// encoding one of these to JSON would otherwise be reading it as it changed.
func (w *transferWatch) forSource(source string, kind transferKind) []transferRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked()

	out := make([]transferRun, 0, len(w.runs))
	for _, entry := range w.runs {
		if entry.run.Source == source && entry.run.Kind == kind {
			out = append(out, w.snapshotLocked(entry))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// running is how many transfers are moving bytes right now, detached or not.
//
// The auto-lock asks: a transfer is use of the vault as much as a browser
// clicking around is, and locking the keys out from under one would fail it for
// no reason. Progress also touches the external-activity clock, which covers
// the same ground from the other side; this is the one that does not depend on
// a tick having landed recently.
func (w *transferWatch) running() int {
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
func (w *transferWatch) forRun(id string) *transferRun {
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
func (w *transferWatch) snapshotLocked(entry *watchedTransfer) transferRun {
	run := entry.run
	if !run.Done && !entry.at.IsZero() && w.now().Sub(entry.at) > staleRateAfter {
		run.Rate = 0
	}
	if run.Done {
		run.Rate = 0
	}
	return run
}

// stop cancels a running transfer and forgets a finished one, which are the
// same gesture from the outside: this is not something I want to look at any
// more. It reports whether there was anything by that name — in the direction
// asked, so a dialog cannot stop an import by naming it as an export.
func (w *transferWatch) stop(id string, kind transferKind) bool {
	w.mu.Lock()
	entry, ok := w.runs[id]
	if !ok || entry.run.Kind != kind {
		w.mu.Unlock()
		return false
	}
	cancel := entry.cancel
	if entry.run.Done {
		delete(w.runs, id)
	}
	w.mu.Unlock()

	// Outside the lock: cancelling wakes the transfer's own goroutine, which
	// comes back here to record how it ended. The entry stays until it does,
	// so a stopped transfer is still listed — as stopping — rather than
	// vanishing before it has actually let go of the connection.
	if cancel != nil {
		cancel()
	}
	return true
}

// stopAll cancels every running transfer. Locking the vault calls it: the keys
// those transfers are being sealed or opened with are about to leave memory,
// and a transfer that carried on would only fail further in, having spent the
// bandwidth first.
func (w *transferWatch) stopAll() {
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
func (w *transferWatch) sweepLocked() {
	for id, entry := range w.runs {
		if entry.run.Done && entry.run.FinishedAt != nil && time.Since(*entry.run.FinishedAt) > finishedTransferTTL {
			delete(w.runs, id)
		}
	}
}

// trimLocked keeps the number of remembered results down, oldest first.
func (w *transferWatch) trimLocked() {
	finished := make([]transferRun, 0, len(w.runs))
	for _, entry := range w.runs {
		if entry.run.Done {
			finished = append(finished, entry.run)
		}
	}
	if len(finished) <= maxFinishedTransfers {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].StartedAt.Before(finished[j].StartedAt) })
	for _, run := range finished[:len(finished)-maxFinishedTransfers] {
		delete(w.runs, run.ID)
	}
}
