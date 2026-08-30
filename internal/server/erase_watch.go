package server

import (
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

// folderErase is one recursive delete, mid-flight: how many of the doomed
// files have had their parts erased, out of how many the folder held.
type folderErase struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type eraseWatch struct {
	mu     sync.Mutex
	byPath map[string]folderErase
}

func newEraseWatch() *eraseWatch {
	return &eraseWatch{byPath: map[string]folderErase{}}
}

// eraseKey names one delete: the folder, inside whichever vault it is in. Two
// vaults can hold the same path, and a watcher of one must not be answered
// with the other's count.
func eraseKey(scope vault.Scope, path string) string {
	return string(scope) + "\x00" + vault.CleanDir(path)
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
	ew.mu.Unlock()
}

func (ew *eraseWatch) get(key string) (folderErase, bool) {
	ew.mu.Lock()
	at, ok := ew.byPath[key]
	ew.mu.Unlock()
	return at, ok
}
