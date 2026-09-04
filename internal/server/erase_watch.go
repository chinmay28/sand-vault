package server

import (
	"context"
	"sync"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Where a recursive folder delete has got to, while it is still getting there.
//
// The same bargain as the import watch: this is a window onto a request that
// is running, and nothing more. It is not job state — it lives in memory for
// as long as the DELETE does, is never written down, and losing it costs
// nothing, because the delete it describes went with it.
//
// What it is for is the case the confirm dialog had nothing to say about: a
// folder holding hundreds of files. Erasing one file's parts is a round trip
// to every account holding one, and a big folder is that many times over —
// minutes of a button saying "Deleting…" with no way to tell it apart from a
// hang. The DELETE request itself can only answer at the end, so where it has
// got to is served beside it, keyed by the folder being deleted.
//
// The orphan sweep is the same slow shape — one POST, a delete per abandoned
// object, an answer only at the end — and counts itself through here too,
// under a key no folder can produce. See handlers_orphans.go.

// folderErase is one recursive delete, mid-flight: how many of the doomed
// files have had their parts erased, out of how many the folder held.
type folderErase struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type eraseWatch struct {
	mu     sync.Mutex
	byPath map[string]folderErase
	// stops holds, for each delete that can be stopped, the cancel of the
	// context it runs under. A DELETE on the window calls it — see stop.
	stops map[string]context.CancelFunc
}

func newEraseWatch() *eraseWatch {
	return &eraseWatch{byPath: map[string]folderErase{}, stops: map[string]context.CancelFunc{}}
}

// open registers a running delete as one that can be stopped: the cancel
// given is what stop calls. Paired with clear, which takes it down again.
func (ew *eraseWatch) open(key string, stop context.CancelFunc) {
	ew.mu.Lock()
	ew.stops[key] = stop
	ew.mu.Unlock()
}

// stop asks a running delete to stop, and says whether there was one to
// ask. Stopping is what vault.DeleteMany and Rmdir make of a done context:
// no further file started, the ones in flight finished, the index written
// for what went. The request itself then answers, and says it was stopped.
func (ew *eraseWatch) stop(key string) bool {
	ew.mu.Lock()
	cancel, ok := ew.stops[key]
	ew.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// eraseKey names one delete: the folder, inside whichever vault it is in. Two
// vaults can hold the same path, and a watcher of one must not be answered
// with the other's count.
func eraseKey(scope vault.Scope, path string) string {
	return string(scope) + "\x00" + vault.CleanDir(path)
}

// batchEraseKey names one batch delete (POST /api/files/delete) by the token
// the browser gave it. The leading NUL keeps it apart from every folder key,
// which starts with a scope.
func batchEraseKey(batch string) string {
	return "\x00files\x00" + batch
}

func (ew *eraseWatch) set(key string, at folderErase) {
	ew.mu.Lock()
	ew.byPath[key] = at
	ew.mu.Unlock()
}

// clear takes a finished delete's window down. Without it a second delete of a
// folder recreated under the same name would open on the last one's count.
func (ew *eraseWatch) clear(key string) {
	ew.mu.Lock()
	delete(ew.byPath, key)
	delete(ew.stops, key)
	ew.mu.Unlock()
}

func (ew *eraseWatch) get(key string) (folderErase, bool) {
	ew.mu.Lock()
	at, ok := ew.byPath[key]
	ew.mu.Unlock()
	return at, ok
}
