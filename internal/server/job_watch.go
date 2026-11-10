package server

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// jobWatch runs one long job at a time apart from the request that asked for
// it, and holds what it found until the next run replaces it.
//
// The shape is the stray-parts scan's (see orphan_scan.go), and the reason is
// the same for every job put on one: the work is a listing of every account,
// which takes minutes on a slow cloud, and a page that is navigated away from
// cancels the request it was waiting on — so the minutes were spent for
// nothing, and in the case of a repair, the repair never happened. A job here
// runs on a context of its own; the page polls for where it has got to while
// it runs and what came of it once it is done, and a page that comes back
// finds either.
//
// P is what the job reports as it goes and R is what it answers with. Both are
// handed out by value inside the status, so a receiver that keeps one is not
// watching it change — the job must likewise not keep a P it has reported.
//
// It lives in memory for as long as the process does and is never written
// down: nothing in it is worth writing, and stopAll drops it whole when the
// keys it was read with leave memory.
type jobWatch[P, R any] struct {
	mu  sync.Mutex
	run *jobRun[P, R]
}

// jobRun is one run, from the moment it was asked for until it is replaced.
type jobRun[P, R any] struct {
	id        string
	scope     string
	startedAt time.Time
	cancel    context.CancelFunc

	// Everything below is guarded by the watch's mutex.
	progress   P
	finishedAt time.Time
	result     *R
	err        string
}

// jobStatus is what a poll is answered with: the run that is going, with where
// it has got to, or the last one that ran, with what came of it — or neither,
// when nothing has been asked since the vault was opened.
type jobStatus[P, R any] struct {
	// ID names the run, so a page can tell a fresh run from the one it was
	// already watching. Empty when there is no run at all.
	ID      string `json:"id,omitempty"`
	Running bool   `json:"running"`

	// Scope is what the run was asked about, as its starter named it — for
	// the reattach, the one account it lists, or empty for every account.
	Scope string `json:"scope,omitempty"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Progress is where a running job has got to. Absent once it is done.
	Progress *P `json:"progress,omitempty"`

	// Result is what a finished job answered with, and Error why it answered
	// nothing. A run that was stopped — by a lock — is gone altogether rather
	// than reported.
	Result *R     `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// jobFunc is the job itself, given the context to run under and the place to
// report progress to. A parameter, so the watch can be tested without a vault.
type jobFunc[P, R any] func(ctx context.Context, onProgress func(P)) (*R, error)

// startOutcome says what a start did.
type startOutcome int

const (
	// jobStarted is a fresh run.
	jobStarted startOutcome = iota
	// jobJoined is a run already going with the same scope: the question is
	// the same question, and asking it twice at once answers it no better.
	jobJoined
	// jobBusy is a run already going with a different scope. Nothing was
	// started; the status is the run in the way.
	jobBusy
)

func newJobWatch[P, R any]() *jobWatch[P, R] {
	return &jobWatch[P, R]{}
}

// start begins a run under scope, or joins the one already going if it was
// asked about the same thing. The answer is the status of whichever run is
// now going, and the outcome says which it is. The run gets a context of its
// own, derived from Background and bounded by timeout.
func (jw *jobWatch[P, R]) start(scope string, run jobFunc[P, R], timeout time.Duration) (jobStatus[P, R], startOutcome) {
	jw.mu.Lock()
	defer jw.mu.Unlock()

	if jw.run != nil && jw.run.finishedAt.IsZero() {
		if jw.run.scope == scope {
			return jw.statusLocked(), jobJoined
		}
		return jw.statusLocked(), jobBusy
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	current := &jobRun[P, R]{
		id:        uuid.NewString(),
		scope:     scope,
		startedAt: time.Now(),
		cancel:    cancel,
	}
	jw.run = current

	go func() {
		defer cancel()
		result, err := run(ctx, func(at P) {
			jw.mu.Lock()
			if jw.run == current {
				current.progress = at
			}
			jw.mu.Unlock()
		})

		jw.mu.Lock()
		defer jw.mu.Unlock()
		// A run that was stopped is already gone from the watch, and what it
		// found — or the error it stopped with — is nobody's business now.
		if jw.run != current {
			return
		}
		current.finishedAt = time.Now()
		if err != nil {
			current.err = err.Error()
			return
		}
		current.result = result
	}()

	return jw.statusLocked(), jobStarted
}

// status is the run as it stands.
func (jw *jobWatch[P, R]) status() jobStatus[P, R] {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	return jw.statusLocked()
}

func (jw *jobWatch[P, R]) statusLocked() jobStatus[P, R] {
	run := jw.run
	if run == nil {
		return jobStatus[P, R]{}
	}
	started := run.startedAt
	status := jobStatus[P, R]{
		ID:        run.id,
		Running:   run.finishedAt.IsZero(),
		Scope:     run.scope,
		StartedAt: &started,
	}
	if status.Running {
		at := run.progress
		status.Progress = &at
		return status
	}
	finished := run.finishedAt
	status.FinishedAt = &finished
	status.Result = run.result
	status.Error = run.err
	return status
}

// stopAll cancels a running job and forgets whatever the last one found.
// Locking the vault calls it: a result read with keys that are leaving memory
// is nobody's to read, and a job still going would finish for nobody.
func (jw *jobWatch[P, R]) stopAll() {
	jw.mu.Lock()
	run := jw.run
	jw.run = nil
	jw.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// cancel ends a running job's context and keeps the run: the job is expected
// to notice and finish with whatever it had, which then stands as its result.
// For a job whose half-done answer is still an answer.
func (jw *jobWatch[P, R]) cancel() {
	jw.mu.Lock()
	run := jw.run
	jw.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// running reports whether a job is going right now.
func (jw *jobWatch[P, R]) running() bool {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	return jw.run != nil && jw.run.finishedAt.IsZero()
}

// forgetFinished drops the last run's answer if that run is over, and leaves a
// running one alone. For when something else has made the answer wrong — a
// reattach that wrote records back makes the last scan's list of mislaid parts
// a list of parts that are no longer mislaid.
func (jw *jobWatch[P, R]) forgetFinished() {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	if jw.run != nil && !jw.run.finishedAt.IsZero() {
		jw.run = nil
	}
}
