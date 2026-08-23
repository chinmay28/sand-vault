package server

import (
	"testing"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// What is running from one machine, and nothing that is not.
func TestImportWatchListsWhatIsRunning(t *testing.T) {
	w := newImportWatch()

	if got := w.forSource("vps"); len(got) != 0 {
		t.Fatalf("a fresh watch listed %d imports", len(got))
	}

	ticket := w.start("vps", "/media", "")
	elsewhere := w.start("nas", "/photos", "")

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

	// A finished import is not a running one, whether it finished, failed or
	// was cancelled — the handler forgets it either way.
	ticket.done()
	if got := w.forSource("vps"); len(got) != 0 {
		t.Errorf("a finished import is still listed: %+v", got)
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
	ticket := w.start("vps", "/media", "")
	defer ticket.done()

	ticket.update(vault.ImportProgress{Name: "one.mp4", Done: 10})
	snapshot := w.forSource("vps")[0]
	ticket.update(vault.ImportProgress{Name: "two.mp4", Done: 20})

	if snapshot.At.Name != "one.mp4" || snapshot.At.Done != 10 {
		t.Errorf("a listing changed under the caller: %+v", snapshot.At)
	}
}
