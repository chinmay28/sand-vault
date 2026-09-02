package server

import (
	"errors"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// What is running from one machine, and nothing that is not.
func TestImportWatchListsWhatIsRunning(t *testing.T) {
	w := newTransferWatch()

	if got := w.forSource("vps", transferImport); len(got) != 0 {
		t.Fatalf("a fresh watch listed %d imports", len(got))
	}

	ticket, err := w.start(transferImport, "vps", "/media", "", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	elsewhere, err := w.start(transferImport, "nas", "/photos", "", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	running := w.forSource("vps", transferImport)
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

	ticket.update(vault.TransferProgress{
		File: 1, Files: 3, Name: "one.mp4",
		Stage: vault.StageFetching, Done: 2048, Size: 8192,
	})
	if at := w.forSource("vps", transferImport)[0].At; at.Name != "one.mp4" || at.Done != 2048 {
		t.Errorf("progress came back as %+v", at)
	}
	if w.running() != 2 {
		t.Errorf("running() said %d, want 2", w.running())
	}

	// A foreground import that is over is not a running one, whether it
	// finished, failed or was cancelled: its answer went back down its own
	// request, so there is nothing left here to show.
	ticket.done()
	if got := w.forSource("vps", transferImport); len(got) != 0 {
		t.Errorf("a finished foreground import is still listed: %+v", got)
	}
	if got := w.forSource("nas", transferImport); len(got) != 1 {
		t.Errorf("forgetting one import took another with it: %+v", got)
	}
	elsewhere.done()
}

// The entries handed out are copies. The import goroutine writes to its own the
// moment the lock is released, and a handler encoding one to JSON must not be
// reading it as it changes.
func TestImportWatchHandsOutCopies(t *testing.T) {
	w := newTransferWatch()
	ticket, err := w.start(transferImport, "vps", "/media", "", false, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer ticket.done()

	ticket.update(vault.TransferProgress{Name: "one.mp4", Done: 10})
	snapshot := w.forSource("vps", transferImport)[0]
	ticket.update(vault.TransferProgress{Name: "two.mp4", Done: 20})

	if snapshot.At.Name != "one.mp4" || snapshot.At.Done != 10 {
		t.Errorf("a listing changed under the caller: %+v", snapshot.At)
	}
}

// A detached import's result outlives it, because there is no request left for
// it to be the answer to.
func TestImportWatchKeepsADetachedResult(t *testing.T) {
	w := newTransferWatch()
	ticket, err := w.start(transferImport, "vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	ticket.finish(&vault.ImportSummary{Imported: 3, Skipped: 1}, nil, false)

	runs := w.forSource("vps", transferImport)
	if len(runs) != 1 {
		t.Fatalf("a finished detached import is not listed: %+v", runs)
	}
	if !runs[0].Done || runs[0].FinishedAt == nil {
		t.Errorf("finished run came back as %+v", runs[0])
	}
	if summary, _ := runs[0].Summary.(*vault.ImportSummary); summary == nil || summary.Imported != 3 {
		t.Errorf("summary came back as %+v", runs[0].Summary)
	}
	// It is over, so it is not holding the vault open any more.
	if w.running() != 0 {
		t.Errorf("running() said %d after the only import finished", w.running())
	}

	// Dismissing it is what forgetting it takes — it will not go on its own
	// while somebody might still come back to read it.
	if !w.stop(runs[0].ID, transferImport) {
		t.Fatal("dismissing a finished run said there was no such run")
	}
	if got := w.forSource("vps", transferImport); len(got) != 0 {
		t.Errorf("a dismissed run is still listed: %+v", got)
	}
}

// Stopping a running one cancels it and leaves it listed until its own
// goroutine comes back and says how it ended. A run that vanished the instant
// it was asked to stop would claim the transfer had let go before it had.
func TestImportWatchStopCancels(t *testing.T) {
	w := newTransferWatch()
	cancelled := false
	ticket, err := w.start(transferImport, "vps", "/media", "", true, func() { cancelled = true })
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	runs := w.forSource("vps", transferImport)
	if !w.stop(runs[0].ID, transferImport) {
		t.Fatal("stopping a running import said there was no such run")
	}
	if !cancelled {
		t.Error("stopping an import did not cancel its context")
	}
	if got := w.forSource("vps", transferImport); len(got) != 1 {
		t.Errorf("a stopping import went missing before it had stopped: %+v", got)
	}

	ticket.finish(&vault.ImportSummary{Imported: 1}, nil, true)
	runs = w.forSource("vps", transferImport)
	if len(runs) != 1 || !runs[0].Cancelled {
		t.Errorf("a stopped import came back as %+v", runs)
	}
	// What it did get is still worth saying: those files are in the vault, and
	// the next run will skip them.
	if summary, _ := runs[0].Summary.(*vault.ImportSummary); summary == nil || summary.Imported != 1 {
		t.Errorf("a stopped import lost what it had already brought in: %+v", runs[0].Summary)
	}
}

// An error is kept the same way a summary is, since nobody was watching when it
// happened.
func TestImportWatchKeepsAFailure(t *testing.T) {
	w := newTransferWatch()
	ticket, err := w.start(transferImport, "vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ticket.finish(nil, errors.New("the machine stopped answering"), false)

	runs := w.forSource("vps", transferImport)
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
	w := newTransferWatch()
	for i := 0; i < maxDetachedTransfers; i++ {
		if _, err := w.start(transferImport, "vps", "/media", "", true, func() {}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, err := w.start(transferImport, "vps", "/media", "", true, func() {}); err == nil {
		t.Error("a fifth detached import was allowed to start")
	}
	// The cap is on detached ones only: a request somebody is watching is not
	// what runs away with the machine.
	if _, err := w.start(transferImport, "vps", "/media", "", false, func() {}); err != nil {
		t.Errorf("a foreground import was refused: %v", err)
	}
}

// Locking the vault stops every transfer: the keys they are sealing chunks with
// are about to leave memory.
func TestImportWatchStopAll(t *testing.T) {
	w := newTransferWatch()
	stopped := 0
	for i := 0; i < 3; i++ {
		if _, err := w.start(transferImport, "vps", "/media", "", true, func() { stopped++ }); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	w.stopAll()
	if stopped != 3 {
		t.Errorf("stopAll cancelled %d of 3", stopped)
	}
}

// The speed reading: measured over the reports, per stage, and dropped rather
// than held once nothing is moving behind it.
func TestImportWatchMeasuresSpeed(t *testing.T) {
	w := newTransferWatch()
	clock := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }

	ticket, err := w.start(transferImport, "vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := w.forSource("vps", transferImport)[0].ID

	fetching := func(done int64) vault.TransferProgress {
		return vault.TransferProgress{File: 1, Files: 1, Name: "big.mp4", Stage: vault.StageFetching, Done: done, Size: 1 << 30}
	}

	// The first report of a stage is the starting line, not a measurement:
	// nothing has moved yet to divide by anything.
	ticket.update(fetching(0))
	if rate := w.forRun(id).Rate; rate != 0 {
		t.Errorf("the first report claimed %v bytes/s", rate)
	}

	// Two seconds and 20 MB later.
	clock = clock.Add(2 * time.Second)
	ticket.update(fetching(20 << 20))
	if rate := w.forRun(id).Rate; rate != float64(10<<20) {
		t.Errorf("speed came back as %v, want %v bytes/s", rate, float64(10<<20))
	}

	// A report too soon after the last one is not measured against it — a few
	// megabytes over a few milliseconds is how a speed reading ends up
	// swinging wildly while the transfer is steady.
	clock = clock.Add(50 * time.Millisecond)
	ticket.update(fetching(24 << 20))
	if rate := w.forRun(id).Rate; rate != float64(10<<20) {
		t.Errorf("a sample taken too soon moved the speed to %v", rate)
	}

	// A slower second later: the reading moves towards it rather than jumping.
	clock = clock.Add(2 * time.Second)
	ticket.update(fetching(28 << 20))
	rate := w.forRun(id).Rate
	if rate >= float64(10<<20) || rate <= float64(4<<20) {
		t.Errorf("speed came back as %v, want it moved part of the way down from 10 MB/s", rate)
	}

	// The other half of the same file is a different pipe, and starts over.
	clock = clock.Add(time.Second)
	ticket.update(vault.TransferProgress{
		File: 1, Files: 1, Name: "big.mp4", Stage: vault.StageScattering, Done: 0, Size: 1 << 30,
	})
	if got := w.forRun(id).Rate; got != 0 {
		t.Errorf("the scatter inherited the fetch's speed: %v", got)
	}

	// Nothing for a while: a stalled transfer must stop claiming a speed.
	clock = clock.Add(time.Second)
	ticket.update(vault.TransferProgress{
		File: 1, Files: 1, Name: "big.mp4", Stage: vault.StageScattering, Done: 8 << 20, Size: 1 << 30,
	})
	if got := w.forRun(id).Rate; got == 0 {
		t.Fatal("the scatter never picked up a speed")
	}
	clock = clock.Add(staleRateAfter + time.Second)
	if got := w.forRun(id).Rate; got != 0 {
		t.Errorf("a stalled transfer still claims %v bytes/s", got)
	}
	ticket.done()
}

// A finished run has no speed. Whatever it was doing, it is not doing it now.
func TestImportWatchDropsTheSpeedWhenItEnds(t *testing.T) {
	w := newTransferWatch()
	ticket, err := w.start(transferImport, "vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := w.forSource("vps", transferImport)[0].ID

	ticket.update(vault.TransferProgress{File: 1, Files: 1, Name: "a.bin", Stage: vault.StageFetching})
	time.Sleep(2 * rateSample)
	ticket.update(vault.TransferProgress{File: 1, Files: 1, Name: "a.bin", Stage: vault.StageFetching, Done: 8 << 20, Size: 8 << 20})
	if w.forRun(id).Rate == 0 {
		t.Fatal("no speed was measured at all")
	}

	ticket.finish(&vault.ImportSummary{Imported: 1}, nil, false)
	if got := w.forRun(id).Rate; got != 0 {
		t.Errorf("a finished import still reports %v bytes/s", got)
	}
}

// Imports and exports share the watch and are told apart by kind: a machine's
// exports are not listed among its imports, and one cannot be stopped by
// naming it as the other.
func TestTransferWatchKeepsTheDirectionsApart(t *testing.T) {
	w := newTransferWatch()

	in, err := w.start(transferImport, "vps", "/media", "", true, func() {})
	if err != nil {
		t.Fatalf("start import: %v", err)
	}
	out, err := w.start(transferExport, "vps", "backup", "", true, func() {})
	if err != nil {
		t.Fatalf("start export: %v", err)
	}

	imports := w.forSource("vps", transferImport)
	exports := w.forSource("vps", transferExport)
	if len(imports) != 1 || imports[0].Kind != transferImport || imports[0].Dest != "/media" {
		t.Errorf("imports listed as %+v", imports)
	}
	if len(exports) != 1 || exports[0].Kind != transferExport || exports[0].Dest != "backup" {
		t.Errorf("exports listed as %+v", exports)
	}
	if w.running() != 2 {
		t.Errorf("running() said %d, want 2", w.running())
	}

	if w.stop(out.id, transferImport) {
		t.Error("an export was stopped by naming it as an import")
	}
	out.finish(&vault.ExportSummary{Exported: 2}, nil, false)
	if !w.stop(out.id, transferExport) {
		t.Error("a finished export could not be dismissed")
	}
	if got := w.forSource("vps", transferExport); len(got) != 0 {
		t.Errorf("a dismissed export is still listed: %+v", got)
	}
	if got := w.forSource("vps", transferImport); len(got) != 1 {
		t.Errorf("dismissing an export took the import with it: %+v", got)
	}

	// The cap on detached transfers counts both directions together: they
	// share the same connection and the same clouds.
	for i := 0; i < maxDetachedTransfers; i++ {
		if _, err := w.start(transferExport, "nas", "x", "", true, func() {}); err != nil {
			if i < maxDetachedTransfers-1 {
				t.Fatalf("refused detached transfer %d of %d: %v", i+1, maxDetachedTransfers, err)
			}
		}
	}
	if _, err := w.start(transferImport, "nas", "/y", "", true, func() {}); err == nil {
		t.Error("an import was detached past the cap the exports had already reached")
	}
	in.done()
}
