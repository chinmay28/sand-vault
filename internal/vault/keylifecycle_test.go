package vault

import (
	"context"
	"os"
	"testing"
)

// The tests in this file are about what happens to a data key generation
// between the moment nothing in memory names it and the moment the vault file
// says so — and about the transfers, rotations and recoveries that can land in
// that gap.

// blockVaultFile makes writeStore's final rename fail by putting a directory
// where the vault file is. The providers' roots are untouched, so uploads and
// shard erasures still work; only the index write fails.
func blockVaultFile(t *testing.T, v *Vault) func() {
	t.Helper()
	data, err := os.ReadFile(v.Path())
	if err != nil {
		t.Fatalf("reading vault: %v", err)
	}
	if err := os.Remove(v.Path()); err != nil {
		t.Fatalf("removing vault: %v", err)
	}
	if err := os.Mkdir(v.Path(), 0700); err != nil {
		t.Fatalf("blocking vault: %v", err)
	}
	return func() {
		os.Remove(v.Path())
		if err := os.WriteFile(v.Path(), data, 0600); err != nil {
			t.Fatalf("restoring vault: %v", err)
		}
	}
}

// relock closes and reopens the main vault, so that what is read back is what
// the file holds rather than what memory did.
func relock(t *testing.T, v *Vault, password string) {
	t.Helper()
	v.AwaitBackupSync()
	v.Lock()
	if err := v.Unlock(password); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

// The prune runs on every write, and a write can fail. The caller then puts
// its entry back — and the entry names a key the prune must not have thrown
// away in the meantime, or the file is readable by nothing, and the next
// successful write bakes that in.
func TestFailedWriteKeepsTheKeyARolledBackOverwriteNames(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "a.txt", []byte("old key"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.ChangePassword(ctx, testPassword, "new password", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	v.AwaitBackupSync()
	if retiredCount(v) != 1 {
		t.Fatalf("retired = %d, want 1", retiredCount(v))
	}

	unblock := blockVaultFile(t, v)
	// Overwrite the only file still on the old key: commitUpload removes the
	// old entry, persists (the prune lets go of the old key), the write
	// fails, and the old entry is put back.
	if _, _, err := v.Upload(ctx, MainScope, "/", "a.txt", []byte("new key"), UploadOptions{Overwrite: true}); err == nil {
		t.Fatal("Upload should fail while the vault file cannot be written")
	}
	unblock()

	got, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry after rollback: %v", err)
	}
	if got.KeyID == v.dataKeyID {
		t.Fatalf("the rolled-back entry should still be on the old key")
	}
	if retiredCount(v) != 1 {
		t.Errorf("retired keys in memory after the failed write = %d, want 1", retiredCount(v))
	}

	// Any later successful write persists the vault — with the key.
	if err := v.Mkdir(MainScope, "/later"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	relock(t, v, "new password")
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("the file is unreadable after the failed overwrite: %v", err)
	}
	if string(data) != "old key" {
		t.Errorf("Fetch = %q, want the original contents", data)
	}
}

func TestFailedWriteKeepsTheKeyARolledBackMigrationNames(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "a.txt", []byte("old key"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.ChangePassword(ctx, testPassword, "new password", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	v.AwaitBackupSync()

	unblock := blockVaultFile(t, v)
	report, err := v.MigrateFiles(ctx, nil)
	if err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if report.Migrated != 0 {
		t.Fatalf("Migrated = %d, want 0 while the vault file cannot be written", report.Migrated)
	}
	unblock()

	if err := v.Mkdir(MainScope, "/later"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	relock(t, v, "new password")
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Fatalf("the file is unreadable after the failed migration: %v", err)
	}
	// And the migration still has the key to finish with.
	if report, err := v.MigrateFiles(ctx, nil); err != nil || !report.Done() {
		t.Fatalf("MigrateFiles after the failed attempt: %+v, %v", report, err)
	}
}

// The same gap in a sub vault: its retired keys are pruned on the same write,
// and its record is sealed before the file is written.
func TestFailedWriteKeepsTheKeyASubVaultRollbackNames(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	id := createSubVault(t, v, "Taxes", subPassword)

	entry, _, err := v.Upload(ctx, Scope(id), "/", "a.txt", []byte("old sub key"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.ChangeSubVaultPassword(ctx, id, subPassword, "new sub password", false); err != nil {
		t.Fatalf("ChangeSubVaultPassword: %v", err)
	}
	v.AwaitBackupSync()

	unblock := blockVaultFile(t, v)
	if _, _, err := v.Upload(ctx, Scope(id), "/", "a.txt", []byte("new sub key"), UploadOptions{Overwrite: true}); err == nil {
		t.Fatal("Upload should fail while the vault file cannot be written")
	}
	unblock()

	if err := v.Mkdir(MainScope, "/later"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	relock(t, v, testPassword)
	if err := v.UnlockSubVault(id, "new sub password"); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("the sub vault's file is unreadable after the failed overwrite: %v", err)
	}
	if string(data) != "old sub key" {
		t.Errorf("Fetch = %q, want the original contents", data)
	}
}

// RotateDataKey retires the active key, and on an empty vault the prune lets
// go of it in the same write. A failed write rolls the active key back, and
// what comes back has to be the key — not a slice the prune had wiped through
// an alias.
func TestRotateDataKeyRollbackKeepsTheActiveKeyIntact(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	v.AwaitBackupSync()
	before := activeDataKey(v)

	unblock := blockVaultFile(t, v)
	if _, err := v.RotateDataKey(); err == nil {
		t.Fatal("RotateDataKey should fail while the vault file cannot be written")
	}
	unblock()

	if got := activeDataKey(v); string(got) != string(before) {
		t.Fatalf("after the failed rotation the in-memory data key changed (id %q)", v.dataKeyID)
	}

	// The proof that matters: a file sealed now opens after a fresh unlock,
	// which reads the key back from the file rather than from memory.
	entry, _, err := v.Upload(ctx, MainScope, "/", "after.txt", []byte("sealed after the rollback"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	relock(t, v, testPassword)
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Fatalf("a file uploaded after the failed rotation is unreadable: %v", err)
	}
}
