package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// shardPath finds where a part landed among the local folders standing in for
// cloud accounts, or "" if no account is holding it.
func shardPath(roots []string, key string) string {
	for _, root := range roots {
		full := filepath.Join(root, key)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return ""
}

// storedParts reads the blobs backing a set of shards, straight off the
// accounts, the way an offline restore would.
func storedParts(t *testing.T, roots []string, shards []Shard) [][]byte {
	t.Helper()

	var blobs [][]byte
	for _, shard := range shards {
		full := shardPath(roots, shard.Key)
		if full == "" {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("reading part %d: %v", shard.Part, err)
		}
		blobs = append(blobs, data)
	}
	return blobs
}

// activeSecret is the secret the vault is currently sealing parts with.
func activeSecret(v *Vault) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return shardPasswordFor(v.dataKey)
}

func retiredCount(v *Vault) int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.retired)
}

func TestChangePasswordRotatesTheKeyThatOpensTheParts(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	payload := bytes.Repeat([]byte("everything under the old key "), 200)
	entry, _, err := v.Upload(ctx, "/", "rotated.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	before := *entry
	before.Shards = append([]Shard(nil), entry.Shards...)
	oldSecret := activeSecret(v)

	// The old secret opens the parts as they stand — which is exactly what
	// must stop being true.
	if _, err := archive.DecodeBytes(storedParts(t, roots, before.Shards), oldSecret); err != nil {
		t.Fatalf("the stored parts should open under the key in force: %v", err)
	}

	report, err := v.ChangePassword(ctx, testPassword, "an altogether better password", true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if report.Migrated != 1 || !report.Done() {
		t.Fatalf("report = %+v, want the one file migrated and nothing left", report)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.ArchiveID == before.ArchiveID {
		t.Error("the file kept its archive ID, so its parts were not rewritten")
	}
	if after.KeyID == before.KeyID {
		t.Error("the file is still recorded under the old key generation")
	}

	// The parts the old password could open are gone from the accounts, not
	// merely unreferenced.
	for _, shard := range before.Shards {
		if full := shardPath(roots, shard.Key); full != "" {
			t.Errorf("part %d is still on an account at %s", shard.Part, full)
		}
	}

	newParts := storedParts(t, roots, after.Shards)
	if len(newParts) != archive.PartCount {
		t.Fatalf("found %d parts on the accounts, want %d", len(newParts), archive.PartCount)
	}
	if _, err := archive.DecodeBytes(newParts, oldSecret); err == nil {
		t.Error("the old key still opens the stored parts — the rotation bought nothing")
	}
	decoded, err := archive.DecodeBytes(newParts, activeSecret(v))
	if err != nil {
		t.Fatalf("the new key does not open the parts it wrote: %v", err)
	}
	if !bytes.Equal(decoded.Data, payload) {
		t.Error("the re-encrypted file does not match the original")
	}

	// And the file is still a file, read the ordinary way.
	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("Fetch after the password change does not match the original")
	}
	if retiredCount(v) != 0 {
		t.Errorf("the vault is still holding %d old key(s) with nothing left on them", retiredCount(v))
	}
}

func TestDeferredMigrationKeepsFilesReadableAndResumes(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	first, _, err := v.Upload(ctx, "/", "one.txt", []byte("the first file"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	second, _, err := v.Upload(ctx, "/", "two.txt", []byte("the second file"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	report, err := v.ChangePassword(ctx, testPassword, "deferred for now", false)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if report.Migrated != 0 || report.Remaining != 2 {
		t.Fatalf("report = %+v, want nothing migrated and both files outstanding", report)
	}

	// The password really did change, and the files really are still readable:
	// the old key travelled into the rewritten vault file rather than being
	// dropped with the password that used to wrap it.
	reopened, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.Unlock(testPassword); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("the old password still unlocks the vault, got %v", err)
	}
	if err := reopened.Unlock("deferred for now"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(reopened.AwaitBackupSync)

	stats, err := reopened.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 2 {
		t.Errorf("Stats.Pending = %d, want 2", stats.Pending)
	}
	for _, entry := range []*Entry{first, second} {
		data, _, err := reopened.Fetch(ctx, entry.ID)
		if err != nil {
			t.Fatalf("Fetch %s before migrating: %v", entry.Name, err)
		}
		if len(data) == 0 {
			t.Errorf("%s came back empty", entry.Name)
		}
	}

	resumed, err := reopened.MigrateFiles(ctx, nil)
	if err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if resumed.Migrated != 2 || !resumed.Done() {
		t.Fatalf("resumed = %+v, want both files migrated", resumed)
	}
	if got := reopened.PendingMigration(); got != 0 {
		t.Errorf("PendingMigration = %d, want 0", got)
	}
	if retiredCount(reopened) != 0 {
		t.Error("the old key is still held after everything moved off it")
	}

	// Nothing on disk should still be sealed under a key nobody needs.
	final, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := final.Unlock("deferred for now"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(final.AwaitBackupSync)
	final.mu.RLock()
	retired := len(final.store.RetiredKeys)
	final.mu.RUnlock()
	if retired != 0 {
		t.Errorf("the vault file still carries %d retired key(s)", retired)
	}
}

func TestMigrationLeavesUnreadableFilesOnTheOldKey(t *testing.T) {
	// Two accounts under the strict policy means two parts per file, so losing
	// one is enough to put a file out of reach.
	v, roots := newTestVault(t, 2)
	ctx := context.Background()

	readable, _, err := v.Upload(ctx, "/", "reachable.txt", []byte("this one is fine"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	stranded, _, err := v.Upload(ctx, "/", "stranded.txt", []byte("this one is not"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Take one of its parts away, as an offline account would.
	missing := shardPath(roots, stranded.Shards[0].Key)
	if missing == "" {
		t.Fatal("could not find a part to remove")
	}
	saved, err := os.ReadFile(missing)
	if err != nil {
		t.Fatalf("reading the part: %v", err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatalf("removing the part: %v", err)
	}

	report, err := v.ChangePassword(ctx, testPassword, "changed regardless", true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if report.Migrated != 1 || report.Remaining != 1 {
		t.Fatalf("report = %+v, want the reachable file moved and the other left", report)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "/stranded.txt") {
		t.Errorf("warnings do not name the file that was left behind: %v", report.Warnings)
	}

	// A file that could not move keeps its key, so it is still readable the
	// moment its parts come back.
	if retiredCount(v) != 1 {
		t.Fatalf("the vault holds %d old key(s), want the one the stranded file needs", retiredCount(v))
	}
	if _, _, err := v.Fetch(ctx, readable.ID); err != nil {
		t.Errorf("the migrated file is not readable: %v", err)
	}

	if err := os.WriteFile(missing, saved, 0600); err != nil {
		t.Fatalf("restoring the part: %v", err)
	}
	resumed, err := v.MigrateFiles(ctx, nil)
	if err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if resumed.Migrated != 1 || !resumed.Done() {
		t.Fatalf("resumed = %+v, want the recovered file migrated", resumed)
	}
	data, _, err := v.Fetch(ctx, stranded.ID)
	if err != nil {
		t.Fatalf("Fetch after the retry: %v", err)
	}
	if string(data) != "this one is not" {
		t.Errorf("content = %q", data)
	}
	if retiredCount(v) != 0 {
		t.Error("the old key outlived the last file on it")
	}
}

func TestDeletingTheLastFileOnAnOldKeyDropsIt(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "doomed.txt", []byte("not for long"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.ChangePassword(ctx, testPassword, "still deferred", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if retiredCount(v) != 1 {
		t.Fatalf("the vault holds %d old key(s), want 1", retiredCount(v))
	}

	// Deleting the file is another way for a key to stop being needed, and it
	// must not survive as a spare copy of a secret nothing points at.
	if _, err := v.Delete(ctx, entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if retiredCount(v) != 0 {
		t.Error("the old key is still held after its last file was deleted")
	}
}

func TestChangePasswordOnAnEmptyVaultKeepsNoOldKey(t *testing.T) {
	v, _ := newTestVault(t, 3)

	report, err := v.ChangePassword(context.Background(), testPassword, "nothing to move", true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if report.Pending != 0 || !report.Done() {
		t.Fatalf("report = %+v, want nothing to migrate", report)
	}
	if retiredCount(v) != 0 {
		t.Error("an empty vault kept its previous data key")
	}
}

func TestChangePasswordRejectsTheWrongCurrentPassword(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, "/", "untouched.txt", []byte("safe"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := v.ChangePassword(ctx, "not the password", "a new one", true); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("ChangePassword = %v, want ErrWrongPassword", err)
	}

	// Nothing was written, so the vault still answers to the password it had.
	reopened, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock with the original password: %v", err)
	}
	t.Cleanup(reopened.AwaitBackupSync)
	if got := reopened.PendingMigration(); got != 0 {
		t.Errorf("PendingMigration = %d, want 0", got)
	}
}

func TestRecoveryAfterAnInterruptedPasswordChange(t *testing.T) {
	ctx := context.Background()
	original, roots := newTestVault(t, 3)

	old := bytes.Repeat([]byte("written before the change "), 100)
	if _, _, err := original.Upload(ctx, "/", "before.txt", old, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// The password changes but the migration does not run, so what follows is
	// a vault whose files sit on two different keys.
	if _, err := original.ChangePassword(ctx, testPassword, "half way through", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	fresh := []byte("written after the change")
	if _, _, err := original.Upload(ctx, "/", "after.txt", fresh, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	original.AwaitBackupSync()
	if warnings, err := original.SyncManifestBackup(ctx, true); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}
	original.Lock()

	// Lose the vault file and rebuild from an account's copy. The backup has to
	// carry both keys, or the older file comes back unreadable.
	target, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := target.Init("the recovering vault's own password", PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(target.AwaitBackupSync)
	for i, root := range roots {
		if _, err := target.AddProvider(ctx, provider.Config{
			Kind:    provider.KindLocal,
			Name:    "reconnected-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	accounts, err := target.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := target.FetchBackup(ctx, accounts[0].ID, "half way through")
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}
	if len(snapshot.Keys) != 2 {
		t.Fatalf("the backup carries %d key(s), want both generations", len(snapshot.Keys))
	}
	if _, err := target.Recover(ctx, snapshot, false); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	for name, want := range map[string][]byte{"/before.txt": old, "/after.txt": fresh} {
		entry := entryAt(t, target, name)
		got, _, err := target.Fetch(ctx, entry.ID)
		if err != nil {
			t.Fatalf("Fetch %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s does not match what was stored", name)
		}
	}

	// The recovered vault knows one file is still behind, and can finish it.
	if got := target.PendingMigration(); got != 1 {
		t.Errorf("PendingMigration = %d, want the one unmigrated file", got)
	}
	report, err := target.MigrateFiles(ctx, nil)
	if err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if report.Migrated != 1 || !report.Done() {
		t.Fatalf("report = %+v, want the outstanding file migrated", report)
	}
	entry := entryAt(t, target, "/before.txt")
	got, _, err := target.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after migrating: %v", err)
	}
	if !bytes.Equal(got, old) {
		t.Error("the migrated file does not match what was stored")
	}
}

func TestBackupsAreReplacedAfterAPasswordChangeEvenIfThePushFailed(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, "/", "indexed.txt", []byte("in the index"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	// The copies on the accounts are sealed under the password about to be
	// replaced, and carry the data key about to be retired.
	if _, err := OpenBackup(readBackup(t, roots[0]), testPassword); err != nil {
		t.Fatalf("OpenBackup before the change: %v", err)
	}

	if _, err := v.ChangePassword(ctx, testPassword, "the new one", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	v.AwaitBackupSync()

	// Reopening stands in for the machine that changed the password having
	// been offline at the time: the vault has to know, from the file alone,
	// that what is on the accounts is stale rather than a foreign vault's.
	reopened, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.Unlock("the new one"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(reopened.AwaitBackupSync)

	warnings, err := reopened.SyncManifestBackup(ctx, false)
	if err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("the vault refused to replace its own stale backups: %v", warnings)
	}

	for i, root := range roots {
		if _, err := OpenBackup(readBackup(t, root), testPassword); err == nil {
			t.Errorf("account %d still holds a backup the old password opens", i)
		}
		snapshot, err := OpenBackup(readBackup(t, root), "the new one")
		if err != nil {
			t.Fatalf("account %d: OpenBackup with the new password: %v", i, err)
		}
		// And it carries the key the files are actually on now.
		secret, err := snapshot.ShardPassword()
		if err != nil {
			t.Fatalf("account %d: ShardPassword: %v", i, err)
		}
		if secret != activeSecret(reopened) {
			t.Errorf("account %d carries a stale data key", i)
		}
	}

	// Once they are current, the standing overwrite instruction is spent.
	reopened.mu.RLock()
	forced := reopened.store.BackupNeedsForce
	reopened.mu.RUnlock()
	if forced {
		t.Error("the vault is still set to overwrite backups it has already replaced")
	}
}

// entryAt looks a file up by path, failing the test if it is not there.
func entryAt(t *testing.T, v *Vault, path string) *Entry {
	t.Helper()

	listing, err := v.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range listing.Files {
		if e.Path() == path {
			return e
		}
	}
	t.Fatalf("no entry at %s", path)
	return nil
}

func TestMigrationProgressReportsEveryFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, _, err := v.Upload(ctx, "/", name, []byte(name), UploadOptions{}); err != nil {
			t.Fatalf("Upload %s: %v", name, err)
		}
	}
	if _, err := v.ChangePassword(ctx, testPassword, "watch it go", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	var seen []string
	report, err := v.MigrateFiles(ctx, func(path string, done, total int) {
		if total != 3 {
			t.Errorf("progress reported a total of %d, want 3", total)
		}
		if done != len(seen)+1 {
			t.Errorf("progress jumped to %d after %d file(s)", done, len(seen))
		}
		seen = append(seen, path)
	})
	if err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if report.Migrated != 3 || len(seen) != 3 {
		t.Fatalf("migrated %d file(s), progress saw %d", report.Migrated, len(seen))
	}
}
