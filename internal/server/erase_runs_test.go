package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

// What is running, with where it has got to — and nothing once a foreground
// delete has answered.
func TestEraseRunsListsWhatIsRunning(t *testing.T) {
	w := newEraseRuns()

	if got := w.all(); len(got) != 0 {
		t.Fatalf("a fresh watch listed %d runs", len(got))
	}

	ticket, err := w.start("/photos", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	other, err := w.start("3 items", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	runs := w.all()
	if len(runs) != 2 {
		t.Fatalf("listed %d runs, want 2", len(runs))
	}
	if runs[0].Label != "/photos" || runs[0].Detached || runs[0].Waiting {
		t.Errorf("the foreground run came back as %+v", runs[0])
	}
	if runs[0].At != (folderErase{}) {
		t.Errorf("a run that has not started reported %+v", runs[0].At)
	}

	ticket.update(3, 79)
	if at := w.forRun(ticket.id).At; at.Done != 3 || at.Total != 79 {
		t.Errorf("progress came back as %+v", at)
	}
	if w.running() != 2 {
		t.Errorf("running() = %d, want 2", w.running())
	}

	ticket.done()
	if w.forRun(ticket.id) != nil {
		t.Error("a finished foreground delete is still listed")
	}
	if w.forRun(other.id) == nil {
		t.Error("forgetting one run took another with it")
	}
	if w.running() != 1 {
		t.Errorf("running() = %d after one finished, want 1", w.running())
	}
}

// A detached delete waits its turn, is listed as waiting while it does, and
// keeps its result once it is over.
func TestEraseRunsQueuesDetachedDeletes(t *testing.T) {
	w := newEraseRuns()
	ctx := context.Background()

	tickets := make([]*eraseTicket, 0, maxRunningErasures+1)
	for i := 0; i <= maxRunningErasures; i++ {
		ticket, err := w.start("run", true, func() {})
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if !w.forRun(ticket.id).Waiting {
			t.Errorf("run %d is not listed as waiting before it has a turn", i)
		}
		tickets = append(tickets, ticket)
	}

	// The first few get a slot at once.
	for i := 0; i < maxRunningErasures; i++ {
		if err := tickets[i].acquire(ctx); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if w.forRun(tickets[i].id).Waiting {
			t.Errorf("run %d is still listed as waiting with a slot", i)
		}
	}

	// The one past the ceiling does not, until one of them lets go.
	last := tickets[maxRunningErasures]
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := last.acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire past the ceiling = %v, want to wait", err)
	}
	if !w.forRun(last.id).Waiting {
		t.Error("a run that could not get a slot stopped being listed as waiting")
	}

	tickets[0].finish(&eraseReport{Deleted: 4}, nil, false)
	if err := last.acquire(ctx); err != nil {
		t.Fatalf("acquire after a slot freed: %v", err)
	}

	// Acquiring twice is one slot, not two.
	if err := last.acquire(ctx); err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	// The finished one is still listed, with what it came to.
	first := w.forRun(tickets[0].id)
	if first == nil || !first.Done || first.Report == nil || first.Report.Deleted != 4 {
		t.Fatalf("the finished run came back as %+v", first)
	}
	if first.FinishedAt == nil || first.Waiting {
		t.Errorf("a finished run reads %+v", first)
	}
	// And is not counted as running.
	if w.running() != maxRunningErasures {
		t.Errorf("running() = %d, want %d", w.running(), maxRunningErasures)
	}
}

// Stopping a waiting delete is a cancel that reaches it before it has touched
// anything; stopping a running one cancels it and keeps it listed until it
// records how it ended; stopping a finished one forgets it.
func TestEraseRunsStop(t *testing.T) {
	w := newEraseRuns()

	ctx, cancel := context.WithCancel(context.Background())
	ticket, err := w.start("/films", true, cancel)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !w.stop(ticket.id) {
		t.Fatal("stop of a waiting run said there was nothing to stop")
	}
	if err := ticket.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire after a stop = %v, want cancelled", err)
	}
	// Still listed: the run has yet to say how it ended.
	if w.forRun(ticket.id) == nil {
		t.Fatal("a stopped run vanished before it recorded its ending")
	}
	ticket.finish(nil, nil, true)
	got := w.forRun(ticket.id)
	if got == nil || !got.Done || !got.Cancelled {
		t.Fatalf("a stopped run came back as %+v", got)
	}

	// The same gesture on a finished run dismisses it.
	if !w.stop(ticket.id) {
		t.Error("dismissing a finished run said there was nothing by that name")
	}
	if w.forRun(ticket.id) != nil {
		t.Error("a dismissed run is still listed")
	}
	if w.stop(ticket.id) {
		t.Error("stopping a run that is gone said it stopped something")
	}
}

// A failure is kept beside the result, so whoever comes back is told what
// went wrong rather than shown nothing.
func TestEraseRunsKeepsAFailure(t *testing.T) {
	w := newEraseRuns()

	ticket, err := w.start("/films", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ticket.finish(&eraseReport{Deleted: 2}, errors.New("the index could not be written"), false)

	got := w.forRun(ticket.id)
	if got == nil || !got.Done {
		t.Fatalf("the failed run came back as %+v", got)
	}
	if got.Error != "the index could not be written" || got.Report == nil || got.Report.Deleted != 2 {
		t.Errorf("failure recorded as %+v", got)
	}
}

// Past the ceiling on waiting runs, one more is refused rather than queued
// without bound.
func TestEraseRunsCapsTheQueue(t *testing.T) {
	w := newEraseRuns()

	for i := 0; i < maxWaitingErasures; i++ {
		if _, err := w.start("run", true, func() {}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, err := w.start("one more", true, func() {}); !errors.As(err, &errTooManyErasures{}) {
		t.Errorf("start past the ceiling = %v, want refused", err)
	}
	// A foreground delete is never refused: somebody is sitting in front of
	// it, and it held its own connection open.
	if _, err := w.start("foreground", false, func() {}); err != nil {
		t.Errorf("a foreground delete was refused: %v", err)
	}
}

// Locking the vault stops every delete that is still going, and leaves the
// finished ones alone.
func TestEraseRunsStopAll(t *testing.T) {
	w := newEraseRuns()

	stopped := 0
	for i := 0; i < 3; i++ {
		if _, err := w.start("run", true, func() { stopped++ }); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	finished, _ := w.start("over", true, func() { t.Error("a finished run was cancelled") })
	finished.finish(&eraseReport{}, nil, false)

	w.stopAll()
	if stopped != 3 {
		t.Errorf("stopAll cancelled %d runs, want 3", stopped)
	}
}

// A finished result is forgotten after a while, and the number kept at once
// is capped — oldest first.
func TestEraseRunsForgetsOldResults(t *testing.T) {
	w := newEraseRuns()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	ticket, _ := w.start("old", true, func() {})
	ticket.finish(&eraseReport{}, nil, false)
	if len(w.all()) != 1 {
		t.Fatal("a result just finished is not listed")
	}

	now = now.Add(finishedTransferTTL + time.Minute)
	if got := w.all(); len(got) != 0 {
		t.Errorf("an old result is still listed: %+v", got)
	}

	for i := 0; i < maxFinishedTransfers+3; i++ {
		now = now.Add(time.Second)
		ticket, _ := w.start("many", true, func() {})
		ticket.finish(&eraseReport{Deleted: i}, nil, false)
	}
	got := w.all()
	if len(got) != maxFinishedTransfers {
		t.Fatalf("kept %d results, want %d", len(got), maxFinishedTransfers)
	}
	if got[0].Report.Deleted != 3 {
		t.Errorf("the oldest kept result is %d, want the newest %d kept", got[0].Report.Deleted, maxFinishedTransfers)
	}
}
