package server

import (
	"errors"
	"testing"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// What is running from one machine, and nothing that is not.
func TestImportWatchListsWhatIsRunning(t *testing.T) {
	w := newImportWatch()

	if got := w.forSource("vps"); len(got) != 0 {
		t.Fatalf("a fresh watch listed %d imports", len(got))
	}

	ticket, err := w.start("vps", "/media", "", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	elsewhere, err := w.start("nas", "/photos", "", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	running := w.forSource("vps")
	if len(running) != 1 {
		t.Fatalf("listed %d imports from the vps, want 1", len(running))
	}
	if running[0].Dest != "/media" {
		t.Errorf("landing at %q, want /media", running[0].Dest)
	}
	// Nothing has moved yet: a selection is walked before a byte of it does,
	// and the zero value is what says so.
	if running[0].At.Name != "" {
		t.Errorf("an import that has not started reported %+v", running[0].At)
	}

	ticket.update(vault.ImportProgress{
		File: 1, Files: 3, Name: "one.mp4",
		Stage: vault.StageFetching, Done: 2048, Size: 8192,
	})
	if at := w.forSource("vps")[0].At; at.Name != "one.mp4" || at.Done != 2048 {
		t.Errorf("progress came back as %+v", at)
	}
	if w.running() != 2 {
		t.Errorf("running() said %d, want 2", w.running())
	}

	// A foreground import that is over is not a running one, whether it
	// finished, failed or was cancelled: its answer went back down its own
	// request, so there is nothing left here to show.
	ticket.done()
	if got := w.forSource("vps"); len(got) != 0 {
		t.Errorf("a finished foreground import is still listed: %+v", got)
	}
	if got := w.forSource("nas"); len(got) != 1 {
		t.Errorf("forgetting one import took another with it: %+v", got)
	}
	elsewhere.done()
}

// The entries handed out are copies. The import goroutine writes to its own the
// moment the lock is released, and a handler encoding one to JSON must not be
// reading it as it changes.
func TestImportWatchHandsOutCopies(t *testing.T) {
	w := newImportWatch()
	ticket, err := w.start("vps", "/media", "", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer ticket.done()

	ticket.update(vault.ImportProgress{Name: "one.mp4", Done: 10})
	snapshot := w.forSource("vps")[0]
	ticket.update(vault.ImportProgress{Name: "two.mp4", Done: 20})

	if snapshot.At.Name != "one.mp4" || snapshot.At.Done != 10 {
		t.Errorf("a listing changed under the caller: %+v", snapshot.At)
	}
}

// A detached import's result outlives it, because there is no request left for
// it to be the answer to.
func TestImportWatchKeepsADetachedResult(t *testing.T) {
	w := newImportWatch()
	ticket, err := w.start("vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	ticket.finish(&vault.ImportSummary{Imported: 3, Skipped: 1}, nil, false)

	runs := w.forSource("vps")
	if len(runs) != 1 {
		t.Fatalf("a finished detached import is not listed: %+v", runs)
	}
	if !runs[0].Done || runs[0].FinishedAt == nil {
		t.Errorf("finished run came back as %+v", runs[0])
	}
	if runs[0].Summary == nil || runs[0].Summary.Imported != 3 {
		t.Errorf("summary came back as %+v", runs[0].Summary)
	}
	// It is over, so it is not holding the vault open any more.
	if w.running() != 0 {
		t.Errorf("running() said %d after the only import finished", w.running())
	}

	// Dismissing it is what forgetting it takes — it will not go on its own
	// while somebody might still come back to read it.
	if !w.stop(runs[0].ID) {
		t.Fatal("dismissing a finished run said there was no such run")
	}
	if got := w.forSource("vps"); len(got) != 0 {
		t.Errorf("a dismissed run is still listed: %+v", got)
	}
}

// Stopping a running one cancels it and leaves it listed until its own
// goroutine comes back and says how it ended. A run that vanished the instant
// it was asked to stop would claim the transfer had let go before it had.
func TestImportWatchStopCancels(t *testing.T) {
	w := newImportWatch()
	cancelled := false
	ticket, err := w.start("vps", "/media", "", true, func() { cancelled = true })
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	runs := w.forSource("vps")
	if !w.stop(runs[0].ID) {
		t.Fatal("stopping a running import said there was no such run")
	}
	if !cancelled {
		t.Error("stopping an import did not cancel its context")
	}
	if got := w.forSource("vps"); len(got) != 1 {
		t.Errorf("a stopping import went missing before it had stopped: %+v", got)
	}

	ticket.finish(&vault.ImportSummary{Imported: 1}, nil, true)
	runs = w.forSource("vps")
	if len(runs) != 1 || !runs[0].Cancelled {
		t.Errorf("a stopped import came back as %+v", runs)
	}
	// What it did get is still worth saying: those files are in the vault, and
	// the next run will skip them.
	if runs[0].Summary == nil || runs[0].Summary.Imported != 1 {
		t.Errorf("a stopped import lost what it had already brought in: %+v", runs[0].Summary)
	}
}

// An error is kept the same way a summary is, since nobody was watching when it
// happened.
func TestImportWatchKeepsAFailure(t *testing.T) {
	w := newImportWatch()
	ticket, err := w.start("vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ticket.finish(nil, errors.New("the machine stopped answering"), false)

	runs := w.forSource("vps")
	if len(runs) != 1 || runs[0].Error == "" {
		t.Fatalf("a failed detached import came back as %+v", runs)
	}
	if runs[0].Summary != nil {
		t.Errorf("a failure carried a summary too: %+v", runs[0].Summary)
	}
}

// Detached imports are capped. A foreground one is bounded by somebody sitting
// in front of it; a detached one is not, and they only slow each other down.
func TestImportWatchCapsDetachedImports(t *testing.T) {
	w := newImportWatch()
	for i := 0; i < maxDetachedImports; i++ {
		if _, err := w.start("vps", "/media", "", true, func() {}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, err := w.start("vps", "/media", "", true, func() {}); err == nil {
		t.Error("a fifth detached import was allowed to start")
	}
	// The cap is on detached ones only: a request somebody is watching is not
	// what runs away with the machine.
	if _, err := w.start("vps", "/media", "", false, func() {}); err != nil {
		t.Errorf("a foreground import was refused: %v", err)
	}
}

// Locking the vault stops every transfer: the keys they are sealing chunks with
// are about to leave memory.
func TestImportWatchStopAll(t *testing.T) {
	w := newImportWatch()
	stopped := 0
	for i := 0; i < 3; i++ {
		if _, err := w.start("vps", "/media", "", true, func() { stopped++ }); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	w.stopAll()
	if stopped != 3 {
		t.Errorf("stopAll cancelled %d of 3", stopped)
	}
}
