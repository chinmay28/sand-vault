package server

import (
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The imports running right now, so the browser can draw one.
//
// An import is one long POST — the request holds the connection open for as
// long as the transfer takes, and cancelling the request cancels the transfer.
// This is a second view onto that request while it is in flight, and it is
// deliberately nothing more: it lives in a map for exactly as long as the
// handler does, it is never written to the vault, and a restart takes it with
// the import it described.
//
// That is the point worth being careful about. SAND's import has no job state
// by design — an interrupted import leaves whole files behind and re-running it
// is how you resume (see vault.ImportFromSource) — and a progress bar is not a
// reason to grow one. Nothing here is consulted by an import; it is only read
// back out to be shown.
//
// It is keyed by source rather than by a token the browser made up, because
// "what is this machine doing right now" is a question with a true answer that
// does not depend on who is asking: a second tab watching the same import sees
// it too.

// importRun is one import in flight.
type importRun struct {
	// Source is the machine it is pulling from, Dest the vault folder it is
	// landing in, and Vault the sub vault that folder is inside, if any.
	Source string `json:"source"`
	Dest   string `json:"dest"`
	Vault  string `json:"vault,omitempty"`

	StartedAt time.Time `json:"started_at"`

	// At is where it has got to, and is the zero value until the first file is
	// picked up — a selection of ten thousand files spends a moment being
	// walked before anything moves.
	At vault.ImportProgress `json:"at"`
}

// importWatch holds them, one entry per running import.
type importWatch struct {
	mu   sync.Mutex
	next int64
	runs map[int64]*importRun
}

func newImportWatch() *importWatch { return &importWatch{runs: map[int64]*importRun{}} }

// start registers an import and hands back the handle to report it with. The
// caller must call done, which is what keeps the map the size of what is
// actually running.
func (w *importWatch) start(source, dest, scope string) *importTicket {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.next++
	id := w.next
	w.runs[id] = &importRun{
		Source: source, Dest: dest, Vault: scope,
		StartedAt: time.Now(),
	}
	return &importTicket{watch: w, id: id}
}

// importTicket is one running import's handle on its own entry.
type importTicket struct {
	watch *importWatch
	id    int64
}

// update records where the import has got to. It is called from the goroutine
// running the import, on every few megabytes, and does nothing but take a lock
// and copy a small struct.
func (t *importTicket) update(at vault.ImportProgress) {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()
	if run, ok := t.watch.runs[t.id]; ok {
		run.At = at
	}
}

// done forgets the import, whether it finished, failed or was cancelled. A
// progress bar for something that is no longer running is worse than none.
func (t *importTicket) done() {
	if t == nil {
		return
	}
	t.watch.mu.Lock()
	defer t.watch.mu.Unlock()
	delete(t.watch.runs, t.id)
}

// forSource answers with what is running from one machine, oldest first.
//
// Copies, not pointers: the import goroutine goes on writing to its entry the
// moment the lock is released, and a handler encoding one of these to JSON
// would otherwise be reading it as it changed.
func (w *importWatch) forSource(source string) []importRun {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]importRun, 0, len(w.runs))
	for _, run := range w.runs {
		if run.Source == source {
			out = append(out, *run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}
