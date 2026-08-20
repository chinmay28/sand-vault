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
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
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
	// The manifest backup is pushed in the background, and so is the conversion
	// of any file still stored whole, so let both finish before the temporary
	// directories they write into are cleaned up. The read history saves itself
	// on a goroutine too, into the directory the vault file is in.
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)

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
	entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", payload, UploadOptions{})
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

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "spread.bin", []byte("abcdefgh"), UploadOptions{})
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

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "big.bin", payload, UploadOptions{})
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

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "fragile.txt", []byte("hello"), UploadOptions{})
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

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "watched.txt", []byte("payload"), UploadOptions{})
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

	_, _, err := v.Upload(context.Background(), MainScope, "/", "lonely.txt", []byte("data"), UploadOptions{})
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

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "doubled.txt", []byte("data"), UploadOptions{})
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

	if err := v.Mkdir(MainScope, "/photos/2024"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, MainScope, "/photos/2024", "a.txt", []byte("a"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(ctx, MainScope, "/", "root.txt", []byte("r"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	root, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List /: %v", err)
	}
	if len(root.Folders) != 1 || root.Folders[0] != "photos" {
		t.Errorf("root folders = %v, want [photos]", root.Folders)
	}
	if len(root.Files) != 1 || root.Files[0].Name != "root.txt" {
		t.Errorf("root files = %v, want [root.txt]", root.Files)
	}

	nested, err := v.List(MainScope, "/photos/2024")
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

	if _, _, err := v.Upload(ctx, MainScope, "/", "dup.txt", []byte("first"), UploadOptions{}); err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	second, _, err := v.Upload(ctx, MainScope, "/", "dup.txt", []byte("second"), UploadOptions{})
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

	if _, _, err := v.Upload(ctx, MainScope, "/", "same.txt", []byte("first"), UploadOptions{}); err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	second, _, err := v.Upload(ctx, MainScope, "/", "same.txt", []byte("second"), UploadOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("overwrite Upload: %v", err)
	}
	if second.Name != "same.txt" {
		t.Errorf("Name = %q, want same.txt", second.Name)
	}

	listing, err := v.List(MainScope, "/")
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

	entry, _, err := v.Upload(ctx, MainScope, "/", "temp.txt", []byte("bye"), UploadOptions{})
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

	if err := v.Mkdir(MainScope, "/archive"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/", "movable.txt", []byte("content"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := append([]Shard(nil), entry.Shards...)

	moved, err := v.Move(context.Background(), entry.ID, "/archive", "renamed.txt")
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

func TestFoldersListsEveryFolderOnce(t *testing.T) {
	v, _ := newTestVault(t, 3)

	for _, dir := range []string{"/archive", "/photos/2024"} {
		if err := v.Mkdir(MainScope, dir); err != nil {
			t.Fatalf("Mkdir %s: %v", dir, err)
		}
	}
	// A folder recorded only by the file sitting in it, which is what a vault
	// rebuilt by a recovery is full of. It is a folder you can walk into, so it
	// is a folder something can be moved into, and it has to be on the list.
	v.manifest.Entries = append(v.manifest.Entries, &Entry{
		ID: "implied", Dir: "/photos/2023/june", Name: "old.jpg",
	})

	folders, err := v.Folders(MainScope)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	want := []string{"/", "/archive", "/photos", "/photos/2023", "/photos/2023/june", "/photos/2024"}
	if strings.Join(folders, " ") != strings.Join(want, " ") {
		t.Errorf("Folders() = %v, want %v", folders, want)
	}

	v.Lock()
	if _, err := v.Folders(MainScope); !errors.Is(err, ErrLocked) {
		t.Errorf("a locked vault answered %v, want ErrLocked — folder names are index too", err)
	}
}

func TestRmdirRecursive(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir(MainScope, "/docs/reports"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, MainScope, "/docs/reports", "q1.txt", []byte("q1"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := v.Rmdir(ctx, MainScope, "/docs", false); err == nil {
		t.Error("expected non-recursive Rmdir to refuse a non-empty folder")
	}
	if _, err := v.Rmdir(ctx, MainScope, "/docs", true); err != nil {
		t.Fatalf("recursive Rmdir: %v", err)
	}

	listing, err := v.List(MainScope, "/")
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

	entry, _, err := v.Upload(ctx, MainScope, "/", "persist.txt", []byte("durable"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	v.Lock()
	if _, err := v.List(MainScope, "/"); !errors.Is(err, ErrLocked) {
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
	// Unlocking pushes the manifest backup in the background, and this second
	// vault has its own goroutine doing it — waiting on the first one is not
	// enough to keep it from writing into directories being cleaned up.
	t.Cleanup(reopened.AwaitBackupSync)

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

	if _, _, err := v.Upload(context.Background(), MainScope, "/", "top-secret-plans.txt", []byte("x"), UploadOptions{}); err != nil {
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

	entry, _, err := v.Upload(ctx, MainScope, "/", "keeper.txt", []byte("still here"), UploadOptions{})
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
	t.Cleanup(reopened.AwaitBackupSync)

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

	entry, _, err := v.Upload(ctx, MainScope, "/", "guarded.txt", []byte("x"), UploadOptions{})
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

	first, err := BuildPlan(ids, PolicyStrict, archive.SchemeDefault, 0)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	second, err := BuildPlan(ids, PolicyStrict, archive.SchemeDefault, 1)
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
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", []byte("hello"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !entry.Chunked() {
		t.Fatal("an upload should store the file in chunks")
	}
	// A shard names its first chunk; the rest follow from the same function, so
	// nothing has to record a key per chunk.
	for _, shard := range entry.Shards {
		if want := ChunkShardKey(entry.ArchiveID, 0, shard.Part); shard.Key != want {
			t.Errorf("part %d stored under %q, want %q", shard.Part, shard.Key, want)
		}
	}
}

func TestUpdateProviderRenamesAndRecolours(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "labelled.txt", []byte("hello"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	held := entry.Shards[0].ProviderID

	name, colour := "the blue one", "#38BDF8"
	updated, err := v.UpdateProvider(t.Context(), held, ProviderEdit{Name: &name, Color: &colour})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if updated.Name != name {
		t.Errorf("Name = %q, want %q", updated.Name, name)
	}
	// Stored the way every other colour in the vault is stored, whatever case
	// it was typed in.
	if updated.Color != "#38bdf8" {
		t.Errorf("Color = %q, want #38bdf8", updated.Color)
	}

	// The index records the name of the account holding each part, and that is
	// what the file list and the health read-out show — so a rename has to
	// reach it, or the vault keeps answering with a name nothing is called.
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	for _, shard := range after.Shards {
		if shard.ProviderID == held && shard.ProviderName != name {
			t.Errorf("shard still names the account %q, want %q", shard.ProviderName, name)
		}
	}

	// And all of it survives a lock and an unlock, which is the only proof the
	// change reached the file rather than just the map in memory.
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	found := false
	for _, cfg := range accounts {
		if cfg.ID != held {
			continue
		}
		found = true
		if cfg.Name != name || cfg.Color != "#38bdf8" {
			t.Errorf("reopened as %q/%q, want %q/#38bdf8", cfg.Name, cfg.Color, name)
		}
	}
	if !found {
		t.Error("the edited account is missing after a reopen")
	}
}

func TestUpdateProviderRejectsNonsense(t *testing.T) {
	v, _ := newTestVault(t, 2)

	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	first, second := accounts[0], accounts[1]

	blank := "   "
	if _, err := v.UpdateProvider(t.Context(), first.ID, ProviderEdit{Name: &blank}); err == nil {
		t.Error("a blank name should be refused")
	}

	// Two accounts answering to one name is what the connect path already
	// refuses; renaming must not be the way around it.
	taken := strings.ToUpper(second.Name)
	if _, err := v.UpdateProvider(t.Context(), first.ID, ProviderEdit{Name: &taken}); err == nil {
		t.Error("a name another account already answers to should be refused")
	}

	notAColour := "cerulean"
	if _, err := v.UpdateProvider(t.Context(), first.ID, ProviderEdit{Name: &notAColour, Color: &notAColour}); err == nil {
		t.Error("a colour that is not a colour should be refused")
	}
	// Refused whole: the name in that same call must not have landed either.
	reread, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if reread[0].Name != first.Name {
		t.Errorf("a rejected edit renamed the account to %q", reread[0].Name)
	}

	// An account keeping its own name is not a clash with itself.
	same := first.Name
	if _, err := v.UpdateProvider(t.Context(), first.ID, ProviderEdit{Name: &same}); err != nil {
		t.Errorf("renaming an account to what it is already called: %v", err)
	}

	// And "" is a colour choice — the one that hands the pick back to the
	// browser — rather than an invalid one.
	auto := ""
	if _, err := v.UpdateProvider(t.Context(), first.ID, ProviderEdit{Color: &auto}); err != nil {
		t.Errorf("clearing a colour: %v", err)
	}

	if _, err := v.UpdateProvider(t.Context(), "no-such-account", ProviderEdit{Name: &same}); err == nil {
		t.Error("editing an account that is not connected should be refused")
	}
}

func TestUpdateProviderLeavesTheOtherFieldAlone(t *testing.T) {
	v, _ := newTestVault(t, 1)

	accounts, _ := v.Providers()
	id, original := accounts[0].ID, accounts[0].Name

	colour := "#abc"
	if _, err := v.UpdateProvider(t.Context(), id, ProviderEdit{Color: &colour}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	renamed := "still coloured"
	updated, err := v.UpdateProvider(t.Context(), id, ProviderEdit{Name: &renamed})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	// The shorthand is expanded on the way in, so nothing downstream ever has
	// to compare a three-digit colour with a six-digit one.
	if updated.Color != "#aabbcc" {
		t.Errorf("Color = %q, want #aabbcc — a rename should not disturb it", updated.Color)
	}
	if updated.Name != renamed || original == renamed {
		t.Errorf("Name = %q, want %q", updated.Name, renamed)
	}
}

// rotatingBackend stands in for the clouds that retire a credential as it is
// spent — Box and Microsoft both do — and does it on the one call an edit makes
// before it is stored. It fails the ping on request, so the two cases that
// matter can be told apart: a set of settings that works, and one that does not
// but has already burnt a token on the way to being refused.
type rotatingBackend struct {
	cfg    provider.Config
	notify func(map[string]string)
}

func (b *rotatingBackend) Config() provider.Config { return b.cfg }
func (b *rotatingBackend) Put(context.Context, string, []byte) error {
	return nil
}
func (b *rotatingBackend) Get(context.Context, string) ([]byte, error) {
	return nil, provider.ErrNotFound
}
func (b *rotatingBackend) Stat(context.Context, string) (provider.ObjectInfo, error) {
	return provider.ObjectInfo{}, provider.ErrNotFound
}
func (b *rotatingBackend) Delete(context.Context, string) error { return nil }
func (b *rotatingBackend) List(context.Context, string) ([]provider.ObjectInfo, error) {
	return nil, nil
}
func (b *rotatingBackend) OnCredentialChange(fn func(map[string]string)) { b.notify = fn }

func (b *rotatingBackend) Ping(context.Context) error {
	if b.notify != nil {
		b.notify(map[string]string{"token": "rotated-by-" + b.cfg.Options["token"]})
	}
	if b.cfg.Options["refuse"] == "yes" {
		return errors.New("the provider says no")
	}
	return nil
}

func registerRotatingBackend(t *testing.T) provider.Kind {
	t.Helper()

	kind := provider.Kind("rotating-on-ping")
	provider.Register(provider.Spec{
		Kind:        kind,
		Label:       "Rotating Cloud",
		Description: "A backend that retires its token as it is spent.",
		Fields: []provider.FieldSpec{
			{Key: "token", Label: "Token", Secret: true, Required: true},
			{Key: "refuse", Label: "Refuse the ping"},
		},
	}, func(cfg provider.Config) (provider.Provider, error) {
		return &rotatingBackend{cfg: cfg}, nil
	})
	return kind
}

// storedOption reads what an account is really connected with, past the
// redaction every other route applies.
func storedOption(t *testing.T, v *Vault, id, key string) string {
	t.Helper()

	v.mu.RLock()
	defer v.mu.RUnlock()
	cfg, ok := v.configForLocked(id)
	if !ok {
		t.Fatalf("no account %s", id)
	}
	return cfg.Options[key]
}

// A settings edit is checked against the backend before it is stored, and the
// check itself can cost a credential. Whichever way it goes, the account must
// end up holding one coherent set: the new settings if the ping worked, and the
// ones it already had if it did not.
func TestUpdateProviderDoesNotStrandACredentialSpentOnARefusedEdit(t *testing.T) {
	v, _ := newTestVault(t, 1)
	kind := registerRotatingBackend(t)
	ctx := context.Background()

	cfg, err := v.AddProvider(ctx, provider.Config{
		Kind:    kind,
		Name:    "rotating",
		Options: map[string]string{"token": "first"},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	// Connecting spends a token too, and that replacement is written back — a
	// live account rotating its own credentials is the mechanism working. Let
	// it settle, and take what it settles on as what the account is working on.
	working := ""
	for i := 0; i < 100 && working != "rotated-by-first"; i++ {
		working = storedOption(t, v, cfg.ID, "token")
		time.Sleep(5 * time.Millisecond)
	}
	if working != "rotated-by-first" {
		t.Fatalf("connecting did not store the token it rotated: %q", working)
	}

	// Settings the backend refuses — after its ping has already retired the
	// token it was handed. Nothing about that may reach the stored account: it
	// is still working on the credentials it has, and half of a rejected edit
	// would break it.
	refused := map[string]string{"token": "second", "refuse": "yes"}
	if _, err := v.UpdateProvider(ctx, cfg.ID, ProviderEdit{Options: refused}); err == nil {
		t.Fatal("a backend that refused the ping was stored anyway")
	}

	// The write that would have done the damage is asynchronous in the shape
	// this guards against, so give it every chance to happen before saying it
	// did not.
	for i := 0; i < 20; i++ {
		if got := storedOption(t, v, cfg.ID, "token"); got != working {
			t.Fatalf("token = %q after a refused edit, want %q — the one it was working on",
				got, working)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := storedOption(t, v, cfg.ID, "refuse"); got != "" {
		t.Errorf("refuse = %q — a refused edit's settings were stored", got)
	}

	// And the other half: an edit that works keeps what the check rotated,
	// rather than storing a token the backend has already retired.
	accepted := map[string]string{"token": "third"}
	if _, err := v.UpdateProvider(ctx, cfg.ID, ProviderEdit{Options: accepted}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if got := storedOption(t, v, cfg.ID, "token"); got != "rotated-by-third" {
		t.Errorf("token = %q, want the one the check left behind", got)
	}
}
