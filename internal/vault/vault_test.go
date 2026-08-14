package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

const testPassword = "correct horse battery staple"

// newTestVault creates an unlocked vault backed by n local-folder accounts,
// standing in for n separate cloud accounts.
func newTestVault(t *testing.T, accounts int) (*Vault, []string) {
	t.Helper()

	dir := t.TempDir()
	v, err := Open(filepath.Join(dir, "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// The manifest backup is pushed in the background, so let it finish before
	// the temporary directories it writes into are cleaned up.
	t.Cleanup(v.AwaitBackupSync)

	roots := make([]string, accounts)
	for i := 0; i < accounts; i++ {
		root := filepath.Join(dir, "cloud", string(rune('a'+i)))
		roots[i] = root
		_, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "cloud-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		})
		if err != nil {
			t.Fatalf("AddProvider %d: %v", i, err)
		}
	}
	return v, roots
}

func TestUploadFetchRoundTrip(t *testing.T) {
	v, _ := newTestVault(t, 3)

	payload := []byte("the quick brown fox jumps over the lazy dog\n")
	entry, warnings, err := v.Upload(context.Background(), "/", "notes.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if got := len(entry.Shards); got != 3 {
		t.Fatalf("expected 3 shards, got %d", got)
	}
	if entry.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", entry.Size, len(payload))
	}
	if !strings.HasPrefix(entry.MIME, "text/plain") {
		t.Errorf("MIME = %q, want text/plain", entry.MIME)
	}

	data, fetched, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("round-trip mismatch: got %q, want %q", data, payload)
	}
	if fetched.Path() != "/notes.txt" {
		t.Errorf("Path = %q, want /notes.txt", fetched.Path())
	}
}

func TestShardsLandOnDistinctAccounts(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), "/", "spread.bin", []byte("abcdefgh"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	seen := map[string]bool{}
	for _, s := range entry.Shards {
		if seen[s.ProviderID] {
			t.Fatalf("account %s holds more than one part — a single compromised account could rebuild the file", s.ProviderName)
		}
		seen[s.ProviderID] = true
	}
	if len(seen) != 3 {
		t.Errorf("parts spread over %d accounts, want 3", len(seen))
	}
}

func TestFetchSurvivesOneAccountGoingDark(t *testing.T) {
	v, roots := newTestVault(t, 3)

	payload := make([]byte, 64*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	entry, _, err := v.Upload(context.Background(), "/", "big.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Wipe the account holding part 1 entirely, as if the provider had gone
	// offline or closed the account.
	victim := entry.Shards[0].ProviderName
	for _, root := range roots {
		candidate := filepath.Join(root, filepath.FromSlash(entry.Shards[0].Key))
		if _, err := os.Stat(candidate); err == nil {
			if err := os.RemoveAll(root); err != nil {
				t.Fatalf("removing account root: %v", err)
			}
			break
		}
	}

	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch with one account down (%s): %v", victim, err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("data reconstructed from the two surviving parts does not match the original")
	}
}

func TestFetchFailsWithOnlyOnePartLeft(t *testing.T) {
	v, roots := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), "/", "fragile.txt", []byte("hello"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Take out two of the three accounts.
	wiped := 0
	for _, root := range roots {
		if wiped == 2 {
			break
		}
		if err := os.RemoveAll(root); err == nil {
			wiped++
		}
	}

	if _, _, err := v.Fetch(context.Background(), entry.ID); err == nil {
		t.Fatal("expected Fetch to fail with only one part reachable")
	}
}

func TestHealthReportsMissingShard(t *testing.T) {
	v, roots := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), "/", "watched.txt", []byte("payload"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	health, err := v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Recoverable {
		t.Fatal("freshly uploaded file reported as unrecoverable")
	}
	for _, s := range health.Shards {
		if !s.Present {
			t.Errorf("part %d reported missing: %s", s.Part, s.Error)
		}
	}

	// Delete a single shard file and re-check.
	for _, root := range roots {
		candidate := filepath.Join(root, filepath.FromSlash(entry.Shards[0].Key))
		if _, err := os.Stat(candidate); err == nil {
			os.Remove(candidate)
			break
		}
	}

	health, err = v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health after damage: %v", err)
	}
	if !health.Recoverable {
		t.Error("file should still be recoverable from the remaining two parts")
	}
	missing := 0
	for _, s := range health.Shards {
		if !s.Present {
			missing++
		}
	}
	if missing != 1 {
		t.Errorf("expected exactly 1 missing part, got %d", missing)
	}
}

func TestStrictPolicyRefusesSingleAccount(t *testing.T) {
	v, _ := newTestVault(t, 1)

	_, _, err := v.Upload(context.Background(), "/", "lonely.txt", []byte("data"), UploadOptions{})
	if err == nil {
		t.Fatal("expected strict placement to refuse a single connected account")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("error should explain the account requirement, got: %v", err)
	}
}

func TestRedundantPolicyDoublesUp(t *testing.T) {
	v, _ := newTestVault(t, 2)

	if err := v.SetPolicy(PolicyRedundant); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	entry, _, err := v.Upload(context.Background(), "/", "doubled.txt", []byte("data"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(entry.Shards) != 3 {
		t.Fatalf("redundant policy should store all 3 parts, got %d", len(entry.Shards))
	}

	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != "data" {
		t.Errorf("round-trip mismatch: %q", data)
	}
}

func TestFoldersAndListing(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir("/photos/2024"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, "/photos/2024", "a.txt", []byte("a"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(ctx, "/", "root.txt", []byte("r"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	root, err := v.List("/")
	if err != nil {
		t.Fatalf("List /: %v", err)
	}
	if len(root.Folders) != 1 || root.Folders[0] != "photos" {
		t.Errorf("root folders = %v, want [photos]", root.Folders)
	}
	if len(root.Files) != 1 || root.Files[0].Name != "root.txt" {
		t.Errorf("root files = %v, want [root.txt]", root.Files)
	}

	nested, err := v.List("/photos/2024")
	if err != nil {
		t.Fatalf("List /photos/2024: %v", err)
	}
	if len(nested.Files) != 1 || nested.Files[0].Name != "a.txt" {
		t.Errorf("nested files = %v", nested.Files)
	}
	if nested.Parent != "/photos" {
		t.Errorf("Parent = %q, want /photos", nested.Parent)
	}
}

func TestUploadCollisionMakesUniqueName(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, "/", "dup.txt", []byte("first"), UploadOptions{}); err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	second, _, err := v.Upload(ctx, "/", "dup.txt", []byte("second"), UploadOptions{})
	if err != nil {
		t.Fatalf("second Upload: %v", err)
	}
	if second.Name != "dup (2).txt" {
		t.Errorf("collision name = %q, want %q", second.Name, "dup (2).txt")
	}
}

func TestUploadOverwriteReplaces(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, "/", "same.txt", []byte("first"), UploadOptions{}); err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	second, _, err := v.Upload(ctx, "/", "same.txt", []byte("second"), UploadOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("overwrite Upload: %v", err)
	}
	if second.Name != "same.txt" {
		t.Errorf("Name = %q, want same.txt", second.Name)
	}

	listing, err := v.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 {
		t.Fatalf("expected 1 file after overwrite, got %d", len(listing.Files))
	}

	data, _, err := v.Fetch(ctx, second.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != "second" {
		t.Errorf("content = %q, want second", data)
	}
}

func TestDeleteRemovesShards(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "temp.txt", []byte("bye"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	warnings, err := v.Delete(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	for _, shard := range entry.Shards {
		for _, root := range roots {
			candidate := filepath.Join(root, filepath.FromSlash(shard.Key))
			if _, err := os.Stat(candidate); err == nil {
				t.Errorf("shard %s still present at %s", shard.Key, candidate)
			}
		}
	}

	if _, err := v.Entry(entry.ID); err == nil {
		t.Error("entry still listed after delete")
	}
}

func TestMoveRenamesWithoutTouchingShards(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir("/archive"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, "/", "movable.txt", []byte("content"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := append([]Shard(nil), entry.Shards...)

	moved, err := v.Move(entry.ID, "/archive", "renamed.txt")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.Path() != "/archive/renamed.txt" {
		t.Errorf("Path = %q, want /archive/renamed.txt", moved.Path())
	}
	for i, s := range moved.Shards {
		if s.Key != before[i].Key {
			t.Errorf("shard %d key changed on move: %s -> %s", i, before[i].Key, s.Key)
		}
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after move: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("content = %q", data)
	}
}

func TestRmdirRecursive(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir("/docs/reports"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, "/docs/reports", "q1.txt", []byte("q1"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := v.Rmdir(ctx, "/docs", false); err == nil {
		t.Error("expected non-recursive Rmdir to refuse a non-empty folder")
	}
	if _, err := v.Rmdir(ctx, "/docs", true); err != nil {
		t.Fatalf("recursive Rmdir: %v", err)
	}

	listing, err := v.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Folders) != 0 || len(listing.Files) != 0 {
		t.Errorf("root not empty after recursive delete: %+v", listing)
	}
}

func TestLockUnlockPersistsIndex(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "persist.txt", []byte("durable"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	v.Lock()
	if _, err := v.List("/"); !errors.Is(err, ErrLocked) {
		t.Errorf("List on locked vault = %v, want ErrLocked", err)
	}

	// Re-open from disk entirely, as a fresh process would.
	reopened, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.Unlock("wrong password"); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("Unlock with wrong password = %v, want ErrWrongPassword", err)
	}
	if err := reopened.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	data, fetched, err := reopened.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after reopen: %v", err)
	}
	if string(data) != "durable" || fetched.Name != "persist.txt" {
		t.Errorf("unexpected entry after reopen: %q %q", data, fetched.Name)
	}
}

func TestVaultFileLeaksNoFilenames(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if _, _, err := v.Upload(context.Background(), "/", "top-secret-plans.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	raw, err := os.ReadFile(v.Path())
	if err != nil {
		t.Fatalf("reading vault file: %v", err)
	}
	if bytes.Contains(raw, []byte("top-secret-plans")) {
		t.Error("vault file contains a plaintext filename")
	}
	if bytes.Contains(raw, []byte(testPassword)) {
		t.Error("vault file contains the plaintext password")
	}
}

func TestChangePasswordKeepsFilesReadable(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "keeper.txt", []byte("still here"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	report, err := v.ChangePassword(ctx, testPassword, "a brand new password", true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if !report.Done() {
		t.Fatalf("migration left %d file(s) behind: %v", report.Remaining, report.Warnings)
	}

	reopened, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.Unlock(testPassword); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("old password should no longer work, got %v", err)
	}
	if err := reopened.Unlock("a brand new password"); err != nil {
		t.Fatalf("Unlock with new password: %v", err)
	}

	data, _, err := reopened.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after password change: %v", err)
	}
	if string(data) != "still here" {
		t.Errorf("content = %q", data)
	}
}

func TestRemoveProviderGuardsRecoverability(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "guarded.txt", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	first := entry.Shards[0].ProviderID
	second := entry.Shards[1].ProviderID

	// Dropping one of three accounts still leaves two parts: allowed.
	if err := v.RemoveProvider(first, false); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	// The entry should no longer claim a part on the disconnected account.
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if len(after.Shards) != 2 {
		t.Errorf("entry kept %d shards after a disconnect, want 2", len(after.Shards))
	}
	for _, s := range after.Shards {
		if s.ProviderID == first {
			t.Error("entry still references the disconnected account")
		}
	}

	// Dropping a second would leave one reachable part: refused without force.
	if err := v.RemoveProvider(second, false); err == nil {
		t.Fatal("expected removal to be refused when it would strand a file")
	}
	if err := v.RemoveProvider(second, true); err != nil {
		t.Fatalf("forced RemoveProvider: %v", err)
	}
}

func TestCleanDir(t *testing.T) {
	cases := map[string]string{
		"":           "/",
		"/":          "/",
		"photos":     "/photos",
		"/photos/":   "/photos",
		"/a/b/../c":  "/a/c",
		"a\\b":       "/a/b",
		"/../escape": "/escape",
		"/a//b///c/": "/a/b/c",
	}
	for in, want := range cases {
		if got := CleanDir(in); got != want {
			t.Errorf("CleanDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeNameRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "/", "   "} {
		if _, err := SanitizeName(bad); err == nil {
			t.Errorf("SanitizeName(%q) should have failed", bad)
		}
	}
	got, err := SanitizeName("../../etc/passwd")
	if err != nil {
		t.Fatalf("SanitizeName: %v", err)
	}
	if got != "passwd" {
		t.Errorf("SanitizeName stripped to %q, want passwd", got)
	}
}

func TestBuildPlanRotatesAcrossAccounts(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}

	first, err := BuildPlan(ids, PolicyStrict, 0)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	second, err := BuildPlan(ids, PolicyStrict, 1)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if first[1] == second[1] {
		t.Error("different seeds should start the rotation on different accounts")
	}
	for _, plan := range []Plan{first, second} {
		seen := map[string]bool{}
		for _, id := range plan {
			if seen[id] {
				t.Fatal("strict plan reused an account")
			}
			seen[id] = true
		}
	}
}

func TestShardKeyIsFlat(t *testing.T) {
	key := ShardKey("6f1b8c2a3d4e5f60718293a4b5c6d7e8", 2)
	if want := "6f1b8c2a3d4e5f60718293a4b5c6d7e8-p2.sand"; key != want {
		t.Errorf("ShardKey = %q, want %q", key, want)
	}
	// A key with no directory components is what keeps the layout identical on
	// backends that have folders and on Google Drive, which does not.
	if strings.Contains(key, "/") {
		t.Errorf("ShardKey %q should not nest directories", key)
	}
}

func TestUploadedShardKeysMatchShardKey(t *testing.T) {
	v, _ := newTestVault(t, 3)
	entry, _, err := v.Upload(context.Background(), "/", "notes.txt", []byte("hello"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, shard := range entry.Shards {
		if want := ShardKey(entry.ArchiveID, shard.Part); shard.Key != want {
			t.Errorf("part %d stored under %q, want %q", shard.Part, shard.Key, want)
		}
	}
}
