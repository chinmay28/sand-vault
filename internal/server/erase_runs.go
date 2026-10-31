package server

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The deletes running right now, so the browser can draw one — and so that
// one can outlive the page that started it.
//
// This is the relocation watch's shape (see relocate_watch.go) applied to the
// other slow thing a vault does. Erasing a file is a round trip to every
// account holding one of its parts, bounded by the slowest of them, and a
// folder of a few hundred films is that a few hundred times over: minutes,
// sometimes an hour, of a dialog that cannot be closed. Nothing about that
// needs a page open. The erasing is the machine's work, the index is written
// by the machine, and the only thing the browser contributed was the request
// holding the connection.
//
// So a delete can be asked to detach. A detached one runs on a context of its
// own, is answered the moment it is accepted, reports here as it goes, and is
// remembered for a while after it ends so the outcome is still there when
// somebody comes back to look. The lifetime is the process, not the request,
// and not a restart: what a delete had erased when it was cut short has left
// the index already, the rest is exactly where it was, and asking again is
// how it resumes. There is nothing worth writing down.
//
// Where it departs from a relocation is in how many may run. A relocation is
// refused past a ceiling, because a third move competing for the same clouds
// only slows the first two. A delete is more often several in a row — three
// files from a menu, then a folder — and refusing the fourth would hand the
// person a question they did not ask. So detached deletes queue: a few erase
// at once, the rest wait their turn and are listed as waiting, and a stop
// while waiting is a run that never touched anything.
//
// Nothing in this file is ever read by a delete. It is written to, and read
// back out to be shown.

// maxRunningErasures is how many detached deletes erase parts at once. Each
// already works a few files abreast (vault.eraseWindow), so two of them is
// eight files' worth of requests in flight per account — about where a cloud
// starts answering with rate limits rather than deletions.
const maxRunningErasures = 2

// maxWaitingErasures is how many detached deletes may be waiting for a turn.
// A ceiling against a loop rather than against a person: nobody clicks Delete
// thirty times before the first one has finished, and a script that does has
// the foreground endpoints.
const maxWaitingErasures = 32

// eraseRunTimeout is the most a detached delete may run. The same ceiling a
// relocation gets, for the same reason: it is resumable at file granularity,
// so a timeout costs the files in flight and nothing else, and what remains is
// exactly as it was.
const eraseRunTimeout = 6 * time.Hour

// eraseReport is what a delete came to, foreground or detached.
type eraseReport struct {
	// Deleted counts the files whose parts were erased and whose records left
	// the index — the files named directly and the ones inside the folders
	// named, together.
	Deleted int `json:"deleted"`

	// Folders counts the folders removed whole. A folder whose delete was
	// stopped part-way is not among them: what it had lost is gone, and the
	// folder itself is still there holding the rest.
	Folders int `json:"folders"`

	// Missing lists the IDs and paths that named nothing — most often a
	// selection deleted twice, where the second time is not an error.
	Missing []string `json:"missing"`

	// Warnings are parts left behind on accounts and folders that could not
	// be taken, one line each.
	Warnings []string `json:"warnings"`
}

// eraseRun is one delete, waiting, running or lately finished.
type eraseRun struct {
	ID string `json:"id"`

	// Label names what was picked, for the card: a file or folder's path, or
	// "12 items" for a selection.
	Label string `json:"label"`

	// Detached says this one outlives the page that started it.
	Detached bool `json:"detached,omitempty"`

	// Waiting says it has not begun: another detached delete has the slot,
	// and this one starts when that one lets go.
	Waiting bool `json:"waiting,omitempty"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// At is how many of the doomed files have had their parts erased, out of
	// how many were picked — folders counted by what they held. The zero value
	// until the first report lands.
	At folderErase `json:"at"`

	// Done, and what it came to. A finished run is only ever a detached one:
	// a foreground delete's answer goes back down the request that started
	// it, and is forgotten here the moment that request ends.
	Done      bool         `json:"done,omitempty"`
	Report    *eraseReport `json:"report,omitempty"`
	Error     string       `json:"error,omitempty"`
	Cancelled bool         `json:"cancelled,omitempty"`
}

// eraseRuns holds them.
type eraseRuns struct {
	mu   sync.Mutex
	runs map[string]*watchedErase

	// slots is the turnstile detached deletes go through: one token per run
	// erasing right now, maxRunningErasures of them.
	slots chan struct{}

	// now is time.Now everywhere but in tests.
	now func() time.Time
}

// watchedErase is a run plus the handle to stop it.
type watchedErase struct {
	run    eraseRun
	cancel func()
}

func newEraseRuns() *eraseRuns {
	return &eraseRuns{
		runs:  map[string]*watchedErase{},
		slots: make(chan struct{}, maxRunningErasures),
		now:   time.Now,
	}
}

// errTooManyErasures is refusing to queue one more.
type errTooManyErasures struct{}

func (errTooManyErasures) Error() string {
	return "too many deletes are already waiting to run in the background; wait for one to finish or stop it"
}

// start registers a delete and hands back the handle to report it with. A
// detached one is listed as waiting until acquire gives it a turn; a
// foreground one never waits — somebody is sitting in front of it, and it
// held the connection open just as it always did.
func (w *eraseRuns) start(label string, detached bool, cancel func()) (*eraseTicket, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked()

	if detached {
		waiting := 0
		for _, entry := range w.runs {
			if entry.run.Detached && entry.run.Waiting {
				waiting++
			}
		}
		if waiting >= maxWaitingErasures {
			return nil, errTooManyErasures{}
		}
	}

	id := uuid.NewString()
	w.runs[id] = &watchedErase{
		run:    eraseRun{ID: id, Label: label, Detached: detached, Waiting: detached, StartedAt: w.now()},
		cancel: cancel,
	}
	return &eraseTicket{watch: w, id: id}, nil
}

// eraseTicket is one delete's handle on its own entry. Its methods are called
// from the goroutine running the delete, and from nowhere else.
type eraseTicket struct {
	watch *eraseRuns
	id    string

	// holding says acquire has handed this run a slot that finish or done
	// has yet to give back.
	holding bool
}

// acquire waits for a turn to erase, or for ctx to end — whichever is first.
// A detached delete calls it before touching a part; a foreground one has no
// need to and does not. The error is ctx.Err() when the wait was cut short,
// in which case the run has done nothing and should finish as cancelled.
func (t *eraseTicket) acquire(ctx context.Context) error {
	if t == nil || t.holding {
		return nil
	}
	// A stop that arrived before the turn did is honoured whether or not a
	// slot happens to be free: select would otherwise take the slot as
	// readily as the cancellation.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case t.watch.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	t.holding = true

	t.watch.mu.Lock()
	if entry, ok := t.watch.runs[t.id]; ok {
		entry.run.Waiting = false
	}
	t.watch.mu.Unlock()
	return nil
}

// release gives a slot back, once.
func (t *eraseTicket) release() {
	if t.holding {
		t.holding = false
		<-t.watch.slots
	}
}

// update records how many files have been erased out of how many. Called
// per file from the goroutines doing the erasing, in order; it does nothing
// but take a lock and store two numbers.
func (t *eraseTicket) update(done, total int) {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()

	entry, ok := t.watch.runs[t.id]
	if !ok {
		return
	}
	entry.run.At = folderErase{Done: done, Total: total}
}

// done forgets the run — what a foreground delete ends with, however it came
// out: its answer went back down its own request.
func (t *eraseTicket) done() {
	if t == nil {
		return
	}
	t.release()
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()
	delete(t.watch.runs, t.id)
}

// finish is what a detached delete ends with instead: the result stays,
// because there is no longer a request for it to be the answer to.
func (t *eraseTicket) finish(report *eraseReport, err error, cancelled bool) {
	if t == nil {
		return
	}
	t.release()
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()

	entry, ok := t.watch.runs[t.id]
	if !ok {
		// Dismissed while it ran.
		return
	}
	at := t.watch.now()
	entry.run.Done, entry.run.Waiting, entry.run.FinishedAt, entry.run.Cancelled = true, false, &at, cancelled
	entry.run.Report = report
	if err != nil {
		entry.run.Error = err.Error()
	}
	entry.cancel = nil
	t.watch.trimLocked()
}

// all answers with every run, waiting, running or lately finished, oldest
// first. Copies, not pointers: the goroutines doing the erasing go on writing
// to their entries the moment the lock is released.
func (w *eraseRuns) all() []eraseRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked()

	out := make([]eraseRun, 0, len(w.runs))
	for _, entry := range w.runs {
		out = append(out, entry.run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// forRun answers with one run by ID, or nil. A copy, as above.
func (w *eraseRuns) forRun(id string) *eraseRun {
	w.mu.Lock()
	defer w.mu.Unlock()

	entry, ok := w.runs[id]
	if !ok {
		return nil
	}
	run := entry.run
	return &run
}

// running is how many deletes are not yet over, waiting ones included. The
// auto-lock asks, as it asks the transfers: a delete is use of the vault, and
// locking the keys out from under one would fail it for no reason.
func (w *eraseRuns) running() int {
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

// stop cancels a waiting or running delete and forgets a finished one — the
// same gesture from the outside. It reports whether there was anything by
// that name.
func (w *eraseRuns) stop(id string) bool {
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
	// stopped run is still listed — as stopping — rather than vanishing
	// before the files in flight have finished their round.
	if cancel != nil {
		cancel()
	}
	return true
}

// stopAll cancels every waiting or running delete. Locking the vault calls
// it: the index the next write would go to is about to leave memory, and
// every file already erased has left it.
func (w *eraseRuns) stopAll() {
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

// sweepLocked drops finished runs nobody came back for, on the transfers'
// clock: long enough to make a cup of tea, short enough not to be yesterday's
// news presented as current.
func (w *eraseRuns) sweepLocked() {
	for id, entry := range w.runs {
		if entry.run.Done && entry.run.FinishedAt != nil && w.now().Sub(*entry.run.FinishedAt) > finishedTransferTTL {
			delete(w.runs, id)
		}
	}
}

// trimLocked keeps the number of remembered results down, oldest first.
func (w *eraseRuns) trimLocked() {
	finished := make([]eraseRun, 0, len(w.runs))
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
