package server

import (
	"testing"

	"github.com/chinmay28/sand-vault/internal/vault"
)

func TestEraseWatchWindowLifecycle(t *testing.T) {
	ew := newEraseWatch()
	key := eraseKey(vault.MainScope, "/photos")

	if _, ok := ew.get(key); ok {
		t.Fatal("a fresh watch answered for a delete nobody started")
	}

	ew.set(key, folderErase{Done: 3, Total: 79})
	at, ok := ew.get(key)
	if !ok || at.Done != 3 || at.Total != 79 {
		t.Fatalf("get = %+v, %v, want 3 of 79", at, ok)
	}

	ew.clear(key)
	if _, ok := ew.get(key); ok {
		t.Fatal("the window stayed up after the delete finished")
	}
}

// Two vaults can hold the same folder path, and a watcher of one must not be
// answered with the other's count. The raw path also goes through CleanDir on
// both ends, so the DELETE and the poll agree on the name however it was
// spelled.
func TestEraseKeySeparatesScopesAndCleansPaths(t *testing.T) {
	if eraseKey(vault.MainScope, "/photos") == eraseKey(vault.Scope("sub"), "/photos") {
		t.Error("the same path in two vaults shares a key")
	}
	if eraseKey(vault.MainScope, "photos/") != eraseKey(vault.MainScope, "/photos") {
		t.Error("two spellings of one folder got two keys")
	}
}
