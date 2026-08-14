package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// readBackup reads the manifest backup a local-folder account is holding.
func readBackup(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, BackupKey))
	if err != nil {
		t.Fatalf("reading the backup at %s: %v", root, err)
	}
	return data
}

func TestBackupLandsOnEveryAccount(t *testing.T) {
	v, roots := newTestVault(t, 3)
	if _, _, err := v.Upload(context.Background(), "/", "notes.txt", []byte("hello"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()
	if warnings, err := v.SyncManifestBackup(context.Background(), false); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}

	// Each account must hold a copy that stands on its own — the same index and
	// the same account list, so recovering from any one of them is equivalent.
	// The bytes differ between pushes (every seal draws a fresh nonce), so it is
	// the contents that have to match, not the ciphertext.
	for i, root := range roots {
		snapshot, err := OpenBackup(readBackup(t, root), testPassword)
		if err != nil {
			t.Fatalf("account %d: OpenBackup: %v", i, err)
		}
		if len(snapshot.Manifest.Entries) != 1 || snapshot.Manifest.Entries[0].Name != "notes.txt" {
			t.Errorf("account %d describes %+v, want the one stored file", i, snapshot.Manifest.Entries)
		}
		if len(snapshot.Accounts) != 3 {
			t.Errorf("account %d names %d accounts, want 3", i, len(snapshot.Accounts))
		}
		if len(snapshot.Manifest.Entries) > 0 && len(snapshot.Manifest.Entries[0].Shards) != 3 {
			t.Errorf("account %d records %d shards, want the full placement map",
				i, len(snapshot.Manifest.Entries[0].Shards))
		}
	}
}

func TestBackupOpensWithThePasswordAlone(t *testing.T) {
	v, roots := newTestVault(t, 3)
	if _, _, err := v.Upload(context.Background(), "/", "notes.txt", []byte("hello"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.SyncManifestBackup(context.Background(), false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	blob := readBackup(t, roots[0])

	// The point of the envelope: no vault file is involved in opening it.
	if _, err := OpenBackup(blob, testPassword); err != nil {
		t.Fatalf("OpenBackup with the right password: %v", err)
	}
	if _, err := OpenBackup(blob, "not the password"); err != ErrWrongPassword {
		t.Fatalf("OpenBackup with a wrong password = %v, want ErrWrongPassword", err)
	}
}

func TestBackupCarriesNoCredentials(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(filepath.Join(dir, "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := v.AddProvider(context.Background(), provider.Config{
			Kind: provider.KindLocal,
			Name: "cloud-" + string(rune('a'+i)),
			Options: map[string]string{
				"path":          filepath.Join(dir, "cloud", string(rune('a'+i))),
				"refresh_token": "super-secret-refresh-token",
			},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	if _, err := v.SyncManifestBackup(context.Background(), false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}

	blob := readBackup(t, filepath.Join(dir, "cloud", "a"))
	snapshot, err := OpenBackup(blob, testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	plain, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// A copy of this file sits in every account, so a credential inside it
	// would let one compromised account reach all the others.
	if strings.Contains(string(plain), "super-secret-refresh-token") {
		t.Fatal("the backup carries an account credential")
	}
}

func TestBackupRefusedOnRedundantPolicyWithTooFewAccounts(t *testing.T) {
	v, roots := newTestVault(t, 2)
	if err := v.SetPolicy(PolicyRedundant); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	v.AwaitBackupSync()

	_, err := v.SyncManifestBackup(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "manifest backup refused") {
		t.Fatalf("SyncManifestBackup = %v, want a refusal", err)
	}
	for _, root := range roots {
		if _, statErr := os.Stat(filepath.Join(root, BackupKey)); statErr == nil {
			t.Fatal("a backup was written despite the refusal")
		}
	}

	// A third account restores the guarantee that one account is never enough
	// to rebuild a file, so the backup becomes safe to write again.
	if _, err := v.AddProvider(context.Background(), provider.Config{
		Kind:    provider.KindLocal,
		Name:    "cloud-c",
		Options: map[string]string{"path": filepath.Join(t.TempDir(), "c")},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if _, err := v.SyncManifestBackup(context.Background(), false); err != nil {
		t.Fatalf("SyncManifestBackup after connecting a third account: %v", err)
	}
	readBackup(t, roots[0])
}

func TestBackupCanBeTurnedOff(t *testing.T) {
	v, roots := newTestVault(t, 3)
	if !v.BackupEnabled() {
		t.Fatal("the manifest backup should be on by default")
	}
	if _, err := v.SetBackupEnabled(context.Background(), false); err != nil {
		t.Fatalf("SetBackupEnabled: %v", err)
	}
	if v.BackupEnabled() {
		t.Fatal("BackupEnabled should report the vault's setting")
	}
	if _, err := v.SyncManifestBackup(context.Background(), false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(roots[0], BackupKey)); err == nil {
		t.Fatal("a backup was written while the setting was off")
	}

	// The setting has to survive a lock and unlock, not just live in memory.
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if v.BackupEnabled() {
		t.Fatal("the setting did not survive a lock and unlock")
	}
}

func TestForgetBackupsErasesEveryCopy(t *testing.T) {
	v, roots := newTestVault(t, 3)
	if _, err := v.SyncManifestBackup(context.Background(), false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	v.AwaitBackupSync()
	if _, err := v.ForgetBackups(context.Background()); err != nil {
		t.Fatalf("ForgetBackups: %v", err)
	}
	for i, root := range roots {
		if _, err := os.Stat(filepath.Join(root, BackupKey)); err == nil {
			t.Errorf("account %d still holds a backup", i)
		}
	}
}

// The whole point of the feature: the vault file is gone, the accounts are
// still there, and a password rebuilds everything.
func TestRecoverAfterLosingTheVault(t *testing.T) {
	ctx := context.Background()
	original, roots := newTestVault(t, 3)

	if err := original.Mkdir("/work/reports"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	payload := bytes.Repeat([]byte("the quarterly numbers "), 500)
	if _, _, err := original.Upload(ctx, "/work/reports", "q4.txt", payload, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := original.Upload(ctx, "/", "readme.md", []byte("top level"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if warnings, err := original.SyncManifestBackup(ctx, false); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}
	original.Lock()

	// A new machine: a fresh vault, a different password, and the same three
	// folders reconnected — which gives every account a brand new ID.
	fresh, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fresh.Init("a completely different password", PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for i, root := range roots {
		if _, err := fresh.AddProvider(ctx, provider.Config{
			Kind:    provider.KindLocal,
			Name:    "reconnected-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}

	dryRun, err := fresh.Recover(ctx, snapshot, true)
	if err != nil {
		t.Fatalf("Recover(dry run): %v", err)
	}
	if dryRun.Files != 2 || dryRun.Recoverable != 2 {
		t.Fatalf("dry run = %+v, want 2 files, both recoverable", dryRun)
	}
	if listing, err := fresh.List("/"); err == nil && len(listing.Files) > 0 {
		t.Fatal("a dry run should not have changed the vault")
	}

	report, err := fresh.Recover(ctx, snapshot, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if report.Files != 2 || report.Recoverable != 2 {
		t.Fatalf("report = %+v, want 2 files, both recoverable", report)
	}
	if report.Unreachable != 0 {
		t.Errorf("no shard should be unreachable, got %d", report.Unreachable)
	}
	// Every account was reconnected under a new ID, so every shard record had
	// to be re-pointed by asking the accounts what they hold.
	if report.Relocated == 0 {
		t.Error("expected shards to be re-pointed at the reconnected accounts")
	}

	// The tree is back.
	listing, err := fresh.List("/work/reports")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "q4.txt" {
		t.Fatalf("recovered listing = %+v", listing.Files)
	}

	// And so are the contents, which is what the recovered data key buys.
	entry := listing.Files[0]
	got, _, err := fresh.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after recovery: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the recovered file does not match the original")
	}
}

func TestRecoverRefusesAVaultThatAlreadyHoldsFiles(t *testing.T) {
	ctx := context.Background()
	source, _ := newTestVault(t, 3)
	if _, _, err := source.Upload(ctx, "/", "a.txt", []byte("a"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := source.SyncManifestBackup(ctx, false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	accounts, _ := source.Providers()
	snapshot, err := source.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}

	target, _ := newTestVault(t, 3)
	if _, _, err := target.Upload(ctx, "/", "b.txt", []byte("b"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	_, err = target.Recover(ctx, snapshot, false)
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("Recover = %v, want a refusal to overwrite a populated vault", err)
	}
}

func TestSnapshotShardPasswordOpensStoredParts(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)
	payload := bytes.Repeat([]byte("recoverable without a vault "), 200)
	entry, _, err := v.Upload(ctx, "/", "offline.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.SyncManifestBackup(ctx, false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}

	snapshot, err := OpenBackup(readBackup(t, roots[0]), testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	password, err := snapshot.ShardPassword()
	if err != nil {
		t.Fatalf("ShardPassword: %v", err)
	}

	// Read the parts straight off the "accounts", with no vault involved, the
	// way an offline restore would.
	var blobs [][]byte
	for _, root := range roots {
		for _, shard := range entry.Shards {
			data, err := os.ReadFile(filepath.Join(root, shard.Key))
			if err == nil {
				blobs = append(blobs, data)
			}
		}
	}
	if len(blobs) < archive.MinPartsToRestore {
		t.Fatalf("found %d parts on disk, need %d", len(blobs), archive.MinPartsToRestore)
	}

	decoded, err := archive.DecodeBytes(blobs[:archive.MinPartsToRestore], password)
	if err != nil {
		t.Fatalf("DecodeBytes with the recovered secret: %v", err)
	}
	if !bytes.Equal(decoded.Data, payload) {
		t.Fatal("the offline restore does not match the original")
	}
	if decoded.Filename != "offline.txt" {
		t.Errorf("filename = %q, want offline.txt", decoded.Filename)
	}
}

func TestRecoverRollsBackWhenTheVaultCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	source, roots := newTestVault(t, 3)
	if _, _, err := source.Upload(ctx, "/", "a.txt", []byte("a"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := source.SyncManifestBackup(ctx, false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	source.AwaitBackupSync()
	snapshot, err := OpenBackup(readBackup(t, roots[0]), testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}

	target, _ := newTestVault(t, 3)
	// Connecting the accounts scheduled a backup push, and its local-folder
	// roots live under the same directory as the vault file. Let it finish
	// before that directory is removed, or the two race.
	target.AwaitBackupSync()

	// Make the vault file unwritable by replacing its directory with a file, so
	// the atomic write cannot create its temporary neighbour.
	dir := filepath.Dir(target.Path())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clearing the vault directory: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("blocking the vault directory: %v", err)
	}
	t.Cleanup(func() { os.Remove(dir) })

	if _, err := target.Recover(ctx, snapshot, false); err == nil {
		t.Fatal("Recover should fail when the vault cannot be written")
	}

	// The failure must leave nothing half-applied: a retry has to be possible,
	// which it would not be if the recovered entries were still in memory.
	listing, err := target.List("/")
	if err != nil {
		t.Fatalf("List after the failed recovery: %v", err)
	}
	if len(listing.Files) != 0 {
		t.Errorf("the failed recovery left %d file(s) in the index", len(listing.Files))
	}
}
