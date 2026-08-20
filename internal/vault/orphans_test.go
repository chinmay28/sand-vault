package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// disconnectAndReconnect plays out the thing this whole file exists for: an
// account is dropped, the vault carries on without it, and the same storage is
// wired back up as a fresh account with an ID of its own.
func disconnectAndReconnect(t *testing.T, v *Vault, root, name string) string {
	t.Helper()

	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, cfg := range accounts {
		if cfg.Options["path"] != root {
			continue
		}
		if err := v.RemoveProvider(cfg.ID, true); err != nil {
			t.Fatalf("RemoveProvider: %v", err)
		}
		break
	}

	fresh, err := v.AddProvider(context.Background(), provider.Config{
		Kind:    provider.KindLocal,
		Name:    name,
		Options: map[string]string{"path": root},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return fresh.ID
}

// orphanScan runs a scan and fails the test rather than the caller.
func orphanScan(t *testing.T, v *Vault) *OrphanScan {
	t.Helper()

	scan, err := v.ScanForOrphans(context.Background())
	if err != nil {
		t.Fatalf("ScanForOrphans: %v", err)
	}
	return scan
}

func TestOrphansAppearWhenAFileIsDeletedWhileAnAccountIsAway(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	doomed, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("this one gets deleted"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	keeper, _, err := v.Upload(ctx, MainScope, "/", "kept.txt", []byte("this one stays"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Nothing is abandoned yet, and a scan of a healthy vault has to say so —
	// a tidy-up that cries wolf on every load is worse than none.
	if scan := orphanScan(t, v); scan.Found {
		t.Fatalf("a vault nobody has disturbed reports %d abandoned archive(s): %+v",
			scan.Archives, scan.Items)
	}

	// The cloud goes away, the file is deleted without it, and the cloud comes
	// back as somebody new. The delete could not reach it, and nothing will
	// ever try again: the ID it was erased under no longer exists.
	away := roots[0]
	if err := v.RemoveProvider(providerHolding(t, v, away), true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if _, err := v.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	returned, err := v.AddProvider(ctx, provider.Config{
		Kind:    provider.KindLocal,
		Name:    "drive-personal-again",
		Options: map[string]string{"path": away},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	scan := orphanScan(t, v)
	if !scan.Found {
		t.Fatalf("the deleted file's part was left on %s and the scan did not notice", away)
	}
	if len(scan.Blocked) > 0 {
		t.Fatalf("the sweep was withheld from an ordinary vault: %v", scan.Blocked)
	}
	if scan.Archives != 1 {
		t.Fatalf("found %d abandoned archive(s), want exactly the deleted file's: %+v",
			scan.Archives, scan.Items)
	}
	if got := scan.Items[0].ArchiveID; got != doomed.ArchiveID {
		t.Errorf("blamed archive %s, want the deleted file's %s", got, doomed.ArchiveID)
	}
	if got := scan.Items[0].ProviderID; got != returned.ID {
		t.Errorf("blamed account %s, want the reconnected %s", got, returned.ID)
	}
	if !scan.Items[0].Deletable || scan.DeletableBytes == 0 {
		t.Errorf("the row is not offered for deletion: %+v", scan.Items[0])
	}

	// And the file that is still stored is not implicated, on that account or
	// on either of the two that never went anywhere.
	for _, item := range scan.Items {
		if item.ArchiveID == keeper.ArchiveID {
			t.Fatalf("a file that is still stored was reported as abandoned: %+v", item)
		}
	}
}

func TestOrphanSweepErasesTheAbandonedPartAndNothingElse(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	payload := []byte("the file that survives the tidy-up")
	doomed, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("deleted while a cloud was away"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	keeper, _, err := v.Upload(ctx, MainScope, "/", "kept.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	away := roots[0]
	if err := v.RemoveProvider(providerHolding(t, v, away), true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if _, err := v.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	disconnectAndReconnect(t, v, away, "back-again")

	before := storedObjects(t, roots)
	if !holdsArchive(before, doomed.ArchiveID) {
		t.Fatalf("the abandoned part is not on the accounts to begin with")
	}

	// Asked first without committing, because that is what the browser shows
	// in the confirmation.
	preview, err := v.SweepOrphans(ctx, nil, true)
	if err != nil {
		t.Fatalf("SweepOrphans dry run: %v", err)
	}
	if preview.Deleted == 0 || preview.Bytes == 0 {
		t.Fatalf("a dry run promised nothing: %+v", preview)
	}
	if after := storedObjects(t, roots); len(after) != len(before) {
		t.Fatalf("a dry run erased %d object(s)", len(before)-len(after))
	}

	report, err := v.SweepOrphans(ctx, nil, false)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if report.Archives != 1 || report.Deleted != preview.Deleted || report.Bytes != preview.Bytes {
		t.Fatalf("the sweep did not do what the dry run said: %+v vs %+v", report, preview)
	}
	if len(report.Warnings) > 0 || len(report.Skipped) > 0 {
		t.Errorf("sweep complained: %v %v", report.Warnings, report.Skipped)
	}

	after := storedObjects(t, roots)
	if holdsArchive(after, doomed.ArchiveID) {
		t.Errorf("the abandoned archive is still on the accounts")
	}
	if !holdsArchive(after, keeper.ArchiveID) {
		t.Errorf("the sweep took the parts of a file that is still stored")
	}

	// The proof that matters: the file that was never in question still opens.
	got, _, err := v.Fetch(ctx, keeper.ID)
	if err != nil {
		t.Fatalf("Fetch after the sweep: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("the surviving file came back changed")
	}

	// And a second sweep has nothing left to do.
	if scan := orphanScan(t, v); scan.Found {
		t.Errorf("still %d abandoned archive(s) after the sweep: %+v", scan.Archives, scan.Items)
	}
}

func TestOrphanScanLeavesALockedSubVaultsFilesAlone(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	sub, err := v.CreateSubVault("private", "a second password entirely")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	scope := Scope(sub.ID)
	hidden, _, err := v.Upload(ctx, scope, "/", "diary.txt", []byte("nobody's business"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload into the sub vault: %v", err)
	}
	if err := v.LockSubVault(sub.ID); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	// A sub vault that is shut is unreadable by design, and its files must not
	// therefore look abandoned — the main vault keeps an inventory of the
	// archives it owns for exactly this question.
	scan := orphanScan(t, v)
	for _, item := range scan.Items {
		if item.ArchiveID == hidden.ArchiveID {
			t.Fatalf("a locked sub vault's file was reported as abandoned: %+v", item)
		}
	}
	if scan.Found {
		t.Fatalf("found %d abandoned archive(s) in a vault that has lost nothing: %+v",
			scan.Archives, scan.Items)
	}
}

func TestOrphanScanWillNotSweepAVaultWaitingToBeRecovered(t *testing.T) {
	roots := storedVault(t)

	// The reinstalled machine: an empty vault, its own password, and three
	// clouds full of somebody else's parts. Every one of them is unaccounted
	// for, and every one of them is exactly what a recovery needs.
	fresh := reconnect(t, "a completely different password", roots)

	scan := orphanScan(t, fresh)
	if !scan.Found {
		t.Fatalf("the parts on the accounts were not noticed at all")
	}
	if len(scan.Blocked) == 0 {
		t.Fatalf("an empty vault was offered a sweep of the parts a recovery would use")
	}
	if scan.Deletable != 0 || scan.DeletableBytes != 0 {
		t.Errorf("%d object(s) were marked deletable on a vault waiting to be recovered", scan.Deletable)
	}
	for _, item := range scan.Items {
		if item.Deletable {
			t.Fatalf("a row is deletable despite the block: %+v", item)
		}
	}

	if _, err := fresh.SweepOrphans(context.Background(), nil, false); err == nil {
		t.Fatal("SweepOrphans ran anyway")
	}
	if live := storedObjects(t, roots); len(live) == 0 {
		t.Fatal("the parts were erased despite the refusal")
	}
}

func TestOrphanScanWillNotSweepAnAccountAnotherVaultIsUsing(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	if _, _, err := v.Upload(ctx, MainScope, "/", "ours.txt", []byte("this vault's own"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	// A second vault has been writing to the same folder — the state somebody
	// is in who pointed two installs at one bucket. Its parts are accounted for
	// by an index, just not by this one, and the mark it leaves is the backup
	// this vault's key cannot open.
	other, err := Open(filepath.Join(t.TempDir(), "other.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := other.Init("the other vault's password", PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(other.AwaitBackupSync)
	for i, root := range roots {
		if _, err := other.AddProvider(ctx, provider.Config{
			Kind:    provider.KindLocal,
			Name:    "shared-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	theirs, _, err := other.Upload(ctx, MainScope, "/", "theirs.txt", []byte("the other vault's own"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload from the other vault: %v", err)
	}
	other.AwaitBackupSync()
	if warnings, err := other.SyncManifestBackup(ctx, true); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}
	other.Lock()

	scan := orphanScan(t, v)
	if !scan.Found {
		t.Fatalf("the other vault's parts were not noticed")
	}
	foreign := 0
	for _, account := range scan.Accounts {
		if account.Foreign {
			foreign++
		}
	}
	if foreign == 0 {
		t.Fatal("no account was marked as carrying another vault's index")
	}
	for _, item := range scan.Items {
		if item.Deletable {
			t.Fatalf("another vault's parts were offered for deletion: %+v", item)
		}
		if item.Reason == "" {
			t.Errorf("a row was withheld without saying why: %+v", item)
		}
	}
	if scan.Deletable != 0 {
		t.Errorf("%d object(s) marked deletable on accounts another vault is using", scan.Deletable)
	}

	// Sweeping everything is a no-op rather than a disaster.
	if _, err := v.SweepOrphans(ctx, nil, false); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if !holdsArchive(storedObjects(t, roots), theirs.ArchiveID) {
		t.Fatal("the other vault's file was erased")
	}
}

func TestOrphanScanIgnoresObjectsSandDidNotWrite(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	if _, _, err := v.Upload(ctx, MainScope, "/", "ours.txt", []byte("stored"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// A folder somebody else is also using. None of this is SAND's to count,
	// let alone to erase.
	strangers := map[string][]byte{
		"holiday.jpg":            []byte("not ours"),
		"notes.sand":             []byte("named like ours, shaped like nothing"),
		"backup-2024-p1.sand":    []byte("close, but the archive is not hex"),
		"deadbeefcafe-p1.sandry": []byte("nearly the suffix"),
	}
	for name, body := range strangers {
		if err := os.WriteFile(filepath.Join(roots[0], name), body, 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	scan := orphanScan(t, v)
	if scan.Found {
		t.Fatalf("files SAND never wrote were counted as abandoned: %+v", scan.Items)
	}

	if _, err := v.SweepOrphans(ctx, nil, false); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	for name := range strangers {
		if _, err := os.Stat(filepath.Join(roots[0], name)); err != nil {
			t.Errorf("the sweep erased %s, which is not ours: %v", name, err)
		}
	}
}

func TestOrphanSweepRefusesATargetThatIsNoLongerAbandoned(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	live, _, err := v.Upload(ctx, MainScope, "/", "kept.txt", []byte("very much in use"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}

	// The window this closes: a client that was shown a figure and comes back
	// naming an archive the vault has since started pointing at again. The
	// scan is re-run inside the sweep, so the name means nothing on its own.
	report, err := v.SweepOrphans(ctx, []OrphanTarget{
		{ProviderID: accounts[0].ID, ArchiveID: live.ArchiveID},
	}, false)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if report.Deleted != 0 || report.Archives != 0 {
		t.Fatalf("the sweep erased %d object(s) of a stored file", report.Deleted)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], live.ArchiveID) {
		t.Fatalf("the refusal was not reported: %+v", report.Skipped)
	}
	if !holdsArchive(storedObjects(t, roots), live.ArchiveID) {
		t.Fatal("the stored file's parts are gone")
	}
	if _, _, err := v.Fetch(ctx, live.ID); err != nil {
		t.Fatalf("Fetch after the refused sweep: %v", err)
	}
}

func TestOrphanScanWithholdsTheSweepWhenAnAccountWillNotAnswer(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	doomed, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("deleted while away"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	away := roots[0]
	if err := v.RemoveProvider(providerHolding(t, v, away), true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if _, err := v.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	disconnectAndReconnect(t, v, away, "back-again")

	// A fourth account that cannot be reached at all, standing in for a cloud
	// that is offline or whose token has expired. Part of the arithmetic is
	// missing, and the arithmetic is the only evidence there is — so the
	// answer is withheld rather than guessed.
	v.mu.Lock()
	v.providers = append(v.providers, provider.Config{
		ID:   "offline-account",
		Kind: provider.Kind("a-backend-this-build-does-not-have"),
		Name: "unreachable-cloud",
	})
	v.mu.Unlock()

	scan := orphanScan(t, v)
	if len(scan.Blocked) == 0 {
		t.Fatal("a sweep was offered while an account could not be listed")
	}
	if !strings.Contains(strings.Join(scan.Blocked, " "), "unreachable-cloud") {
		t.Errorf("the block does not name the silent account: %v", scan.Blocked)
	}
	if scan.Deletable != 0 {
		t.Errorf("%d object(s) marked deletable with an account silent", scan.Deletable)
	}
	if len(scan.Warnings) == 0 {
		t.Error("the account that would not answer was not mentioned")
	}
	// The archive really is abandoned — this is a refusal to act on a partial
	// picture, not a failure to see it.
	if !scan.Found {
		t.Error("the abandoned archive went unreported as well as unswept")
	}

	if _, err := v.SweepOrphans(ctx, nil, false); err == nil {
		t.Fatal("SweepOrphans ran with an account silent")
	}
	if !holdsArchive(storedObjects(t, roots), doomed.ArchiveID) {
		t.Error("the sweep erased something despite refusing")
	}
}

func TestOrphanGuardNamesEveryReasonToHoldOff(t *testing.T) {
	owned := map[string]*ownedArchive{"6f1b8c2a3d4e5f60718293a4b5c6d7e8": {}}
	healthy := []OrphanAccount{{Name: "drive", Orphans: 2}, {Name: "bucket"}}

	if blocked := orphanGuard(owned, healthy); len(blocked) != 0 {
		t.Errorf("an ordinary vault was blocked: %v", blocked)
	}

	// A vault that holds nothing, with parts on its accounts, is the opening
	// scene of a recovery rather than a mess to tidy.
	if blocked := orphanGuard(nil, healthy); len(blocked) != 1 {
		t.Errorf("an empty vault holding unaccounted-for parts was not blocked: %v", blocked)
	}
	// Unless there is genuinely nothing out there, in which case there is
	// nothing to be wrong about.
	if blocked := orphanGuard(nil, []OrphanAccount{{Name: "drive"}}); len(blocked) != 0 {
		t.Errorf("an empty vault with empty accounts was blocked: %v", blocked)
	}
	// Or unless the accounts carry this vault's own index, which is somebody
	// who has deleted their last file rather than somebody who has lost a
	// vault — and who may tidy up after themselves.
	emptied := []OrphanAccount{{Name: "drive", Orphans: 2, Backup: true}, {Name: "bucket", Backup: true}}
	if blocked := orphanGuard(nil, emptied); len(blocked) != 0 {
		t.Errorf("a vault that had emptied its own accounts was blocked: %v", blocked)
	}
	// But a foreign index anywhere settles it the other way, however many of
	// the others are ours.
	mixed := []OrphanAccount{{Name: "drive", Orphans: 2, Backup: true}, {Name: "bucket", Backup: true, Foreign: true}}
	if blocked := orphanGuard(nil, mixed); len(blocked) != 1 {
		t.Errorf("an empty vault beside another vault's index was not blocked: %v", blocked)
	}

	silent := []OrphanAccount{{Name: "drive", Orphans: 2}, {Name: "bucket", Error: "connection refused"}}
	blocked := orphanGuard(owned, silent)
	if len(blocked) != 1 || !strings.Contains(blocked[0], "bucket") {
		t.Errorf("a silent account did not block the sweep by name: %v", blocked)
	}
}

func TestOrphanSweepTakesTheRowsThePreviewCouldNotShow(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	if _, _, err := v.Upload(ctx, MainScope, "/", "ours.txt", []byte("stored"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// More abandoned archives than a scan will list. They stand in for a year
	// of deletes against a cloud that was never connected while they happened,
	// and they are written straight onto the account because that is exactly
	// what such a delete leaves: part objects with no index anywhere.
	const strays = orphanArchivePreview + 17
	for i := 0; i < strays; i++ {
		name := fmt.Sprintf("%032x-p1.sand", i+1)
		if err := os.WriteFile(filepath.Join(roots[0], name), []byte("abandoned"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	scan := orphanScan(t, v)
	if scan.Archives != strays {
		t.Fatalf("counted %d abandoned archive(s), want %d", scan.Archives, strays)
	}
	if len(scan.Items) != orphanArchivePreview || scan.ItemsTruncated != strays-orphanArchivePreview {
		t.Fatalf("listed %d row(s) with %d truncated, want %d and %d",
			len(scan.Items), scan.ItemsTruncated, orphanArchivePreview, strays-orphanArchivePreview)
	}
	if scan.Deletable != strays {
		t.Errorf("offered %d object(s) for deletion, want every one of the %d", scan.Deletable, strays)
	}

	// The cap bounds what is shown, not what is swept. A sweep that stopped at
	// the preview would leave the rest behind without saying so.
	report, err := v.SweepOrphans(ctx, nil, false)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if report.Archives != strays {
		t.Errorf("swept %d archive(s), want %d", report.Archives, strays)
	}
	if scan := orphanScan(t, v); scan.Found {
		t.Errorf("%d archive(s) survived the sweep", scan.Archives)
	}
}

// providerHolding returns the ID of the connected account pointed at a folder.
func providerHolding(t *testing.T, v *Vault, root string) string {
	t.Helper()

	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, cfg := range accounts {
		if cfg.Options["path"] == root {
			return cfg.ID
		}
	}
	t.Fatalf("no connected account is pointed at %s", root)
	return ""
}

// holdsArchive reports whether any of the listed object keys belongs to an
// archive.
func holdsArchive(objects map[string]bool, archiveID string) bool {
	for key := range objects {
		if id, ours := archiveOfKey(key); ours && id == archiveID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Shards a disconnect mislaid, and putting them back
// ---------------------------------------------------------------------------

// degradedByDisconnect plays out the case this repair exists for: a cloud is
// disconnected, which drops the index records naming it, and the same storage
// is connected back as a new account still holding the objects. Returns the
// file, the folder, and the ID of the account it came back as.
func degradedByDisconnect(t *testing.T, v *Vault, roots []string, payload []byte) (*Entry, Shard, string, string) {
	t.Helper()
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "important.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.Redundancy() != 3 {
		t.Fatalf("the file went up on %d shard(s), want 3", entry.Redundancy())
	}

	// Which account actually holds shard 1 is the placement's choice, so ask
	// rather than assume.
	dropped := entry.Shards[0]
	away := dropped.ProviderID
	root := ""
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, cfg := range accounts {
		if cfg.ID == away {
			root = cfg.Options["path"]
		}
	}
	if root == "" {
		t.Fatal("could not find the folder behind the account holding a shard")
	}

	if err := v.RemoveProvider(away, true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	back, err := v.AddProvider(ctx, provider.Config{
		Kind:    provider.KindLocal,
		Name:    "reconnected",
		Options: map[string]string{"path": root},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return entry, dropped, root, back.ID
}

func TestADisconnectMislaysAShardAndTheScanFindsIt(t *testing.T) {
	v, roots := newTestVault(t, 3)
	entry, _, root, back := degradedByDisconnect(t, v, roots, []byte("the file that lost a spare"))

	// The state the app reports as "missing a spare part": the record is gone,
	// and the object it named is sitting on a connected account.
	if got := entry.Redundancy(); got != 2 {
		t.Fatalf("the file has %d shard(s) recorded after the disconnect, want 2", got)
	}
	if len(storedObjects(t, []string{root})) == 0 {
		t.Fatal("the disconnect erased the object as well as the record")
	}

	scan := orphanScan(t, v)

	// It is emphatically not an orphan — the file is right there in the tree —
	// so the sweep must never see it.
	if scan.Found {
		t.Errorf("a mislaid shard was reported as abandoned: %+v", scan.Items)
	}
	if scan.Deletable != 0 {
		t.Errorf("%d object(s) of a stored file were offered for deletion", scan.Deletable)
	}

	if len(scan.Strays) != 1 {
		t.Fatalf("found %d mislaid shard(s), want 1: %+v", len(scan.Strays), scan.Strays)
	}
	stray := scan.Strays[0]
	switch {
	case stray.ArchiveID != entry.ArchiveID:
		t.Errorf("blamed archive %s, want the file's %s", stray.ArchiveID, entry.ArchiveID)
	case stray.ProviderID != back:
		t.Errorf("blamed account %s, want the reconnected %s", stray.ProviderID, back)
	case stray.FileID != entry.ID:
		t.Errorf("pointed at file %s, want %s", stray.FileID, entry.ID)
	case stray.Path != "/important.txt":
		t.Errorf("named the file %q", stray.Path)
	case stray.Have != 2 || stray.Want != 3:
		t.Errorf("said the file has %d of %d shards, want 2 of 3", stray.Have, stray.Want)
	case !stray.Reattachable:
		t.Errorf("the shard is not offered for reattachment: %s", stray.Reason)
	}
	if scan.Reattachable != 1 || scan.StrayFiles != 1 || scan.StrayBytes == 0 {
		t.Errorf("totals do not agree with the row: %d reattachable, %d file(s), %d byte(s)",
			scan.Reattachable, scan.StrayFiles, scan.StrayBytes)
	}
}

func TestReattachPutsTheShardBackWithoutMovingAByte(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)
	payload := []byte("the file that lost a spare")
	entry, dropped, _, back := degradedByDisconnect(t, v, roots, payload)

	before := storedObjects(t, roots)

	preview, err := v.ReattachShards(ctx, true)
	if err != nil {
		t.Fatalf("ReattachShards dry run: %v", err)
	}
	if preview.Shards != 1 || preview.Files != 1 {
		t.Fatalf("the dry run promised %d shard(s) across %d file(s), want 1 and 1", preview.Shards, preview.Files)
	}
	if got := v.entryByID(t, entry.ID).Redundancy(); got != 2 {
		t.Fatalf("a dry run changed the index: %d shard(s)", got)
	}

	report, err := v.ReattachShards(ctx, false)
	if err != nil {
		t.Fatalf("ReattachShards: %v", err)
	}
	if report.Shards != preview.Shards || report.Bytes != preview.Bytes {
		t.Fatalf("the repair did not do what the dry run said: %+v vs %+v", report, preview)
	}
	if len(report.Restored) != 1 || report.Restored[0] != "/important.txt" {
		t.Errorf("the file was not reported as restored: %+v", report.Restored)
	}
	if len(report.Skipped) > 0 || len(report.Warnings) > 0 {
		t.Errorf("the repair complained: %v %v", report.Skipped, report.Warnings)
	}

	// The whole point: not a byte was written to any account.
	if after := storedObjects(t, roots); len(after) != len(before) {
		t.Errorf("the repair changed the accounts: %d object(s), was %d", len(after), len(before))
	}

	// The file is back to full spread, on the account that was holding it all
	// along, and still reads.
	fixed := v.entryByID(t, entry.ID)
	if got := fixed.Redundancy(); got != 3 {
		t.Fatalf("the file has %d shard(s), want 3", got)
	}
	found := false
	for _, sh := range fixed.Shards {
		if sh.ProviderID == back {
			found = true
			// The very key the dropped record carried, which is the whole
			// claim: nothing was re-derived differently, and nothing moved.
			if sh.Key != dropped.Key {
				t.Errorf("recorded key %q, want the one it had, %q", sh.Key, dropped.Key)
			}
			if sh.Part != dropped.Part {
				t.Errorf("recorded shard %d, want %d", sh.Part, dropped.Part)
			}
			if sh.ProviderName != "reconnected" {
				t.Errorf("recorded the account as %q", sh.ProviderName)
			}
		}
	}
	if !found {
		t.Error("no shard was recorded on the reconnected account")
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the repair: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("the file came back changed")
	}

	// And a second run has nothing left to do.
	again, err := v.ReattachShards(ctx, false)
	if err != nil {
		t.Fatalf("ReattachShards again: %v", err)
	}
	if again.Shards != 0 {
		t.Errorf("the repair ran twice over the same shard: %+v", again)
	}
	if scan := orphanScan(t, v); len(scan.Strays) != 0 {
		t.Errorf("%d shard(s) still reported as mislaid", len(scan.Strays))
	}
}

func TestReattachRefusesAShardStrictPlacementWouldNotHaveMade(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "crowded.txt", []byte("two shards, one cloud"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// A second shard's object copied onto an account that already holds one of
	// this file's — what an interrupted relocation leaves. The bytes being
	// there is not the question; recording them would have the vault act on a
	// placement the strict policy promises never to make.
	host, other := entry.Shards[0], entry.Shards[1]
	hostRoot := ""
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, cfg := range accounts {
		if cfg.ID == host.ProviderID {
			hostRoot = cfg.Options["path"]
		}
	}
	otherRoot := ""
	for _, cfg := range accounts {
		if cfg.ID == other.ProviderID {
			otherRoot = cfg.Options["path"]
		}
	}
	blob, err := os.ReadFile(filepath.Join(otherRoot, other.Key))
	if err != nil {
		t.Fatalf("reading a part: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostRoot, other.Key), blob, 0o600); err != nil {
		t.Fatalf("planting a copy: %v", err)
	}

	// Recorded on its own account already, so it is not mislaid at all.
	scan := orphanScan(t, v)
	if len(scan.Strays) != 0 || scan.Found {
		t.Fatalf("a spare copy of a recorded shard was reported: strays %+v, orphans %+v",
			scan.Strays, scan.Items)
	}

	// Now drop the real record, so the copy becomes the only candidate — and
	// the one the policy will not have.
	v.mu.Lock()
	for _, e := range v.manifest.Entries {
		if e.ID != entry.ID {
			continue
		}
		kept := e.Shards[:0]
		for _, sh := range e.Shards {
			if sh.ProviderID != other.ProviderID {
				kept = append(kept, sh)
			}
		}
		e.Shards = kept
	}
	v.mu.Unlock()

	// Two candidates for the same missing shard now: the copy on the account
	// that already holds shard 1, which strict placement will not have, and the
	// original on the account whose record was just dropped, which it will.
	scan = orphanScan(t, v)
	if len(scan.Strays) != 2 {
		t.Fatalf("found %d candidate(s), want 2: %+v", len(scan.Strays), scan.Strays)
	}
	crowded, roomy := (*StrayShard)(nil), (*StrayShard)(nil)
	for i := range scan.Strays {
		switch scan.Strays[i].ProviderID {
		case host.ProviderID:
			crowded = &scan.Strays[i]
		case other.ProviderID:
			roomy = &scan.Strays[i]
		}
	}
	if crowded == nil || roomy == nil {
		t.Fatalf("the candidates are not the two accounts expected: %+v", scan.Strays)
	}
	if crowded.Reattachable {
		t.Errorf("a shard strict placement forbids was offered: %+v", crowded)
	}
	if !strings.Contains(crowded.Reason, "strict placement") {
		t.Errorf("the refusal does not say why: %q", crowded.Reason)
	}
	if !roomy.Reattachable {
		t.Errorf("the shard on the account that has room was refused: %s", roomy.Reason)
	}

	report, err := v.ReattachShards(ctx, false)
	if err != nil {
		t.Fatalf("ReattachShards: %v", err)
	}
	if report.Shards != 1 {
		t.Fatalf("reattached %d shard(s), want the one with room: %+v", report.Shards, report)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "strict placement") {
		t.Errorf("the refusal was not reported: %+v", report.Skipped)
	}
	fixed := v.entryByID(t, entry.ID)
	for _, sh := range fixed.Shards {
		if sh.ProviderID == host.ProviderID && sh.Part == other.Part {
			t.Errorf("the crowded copy was recorded after all: %+v", sh)
		}
	}
	if got := fixed.Redundancy(); got != 3 {
		t.Errorf("the file has %d distinct shard(s), want 3", got)
	}
}

func TestReattachLeavesAThumbnailPackAlone(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "picture.jpg", []byte("a picture of something"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetThumb(ctx, entry.ID, []byte("a small jpeg standing in for one")); err != nil {
		t.Fatalf("SetThumb: %v", err)
	}

	// A pack's shard record dropped the way a disconnect drops one. A pack is
	// derived from a file that is still stored, so it is drawn again rather
	// than mended — and its objects must not read as abandoned either.
	v.mu.Lock()
	var packArchive string
	for _, pack := range v.manifest.Thumbs {
		if pack == nil || len(pack.Shards) == 0 {
			continue
		}
		packArchive, _, _ = partOfKey(pack.Shards[0].Key)
		pack.Shards = pack.Shards[1:]
	}
	v.mu.Unlock()
	if packArchive == "" {
		t.Fatal("no thumbnail pack was stored")
	}

	scan := orphanScan(t, v)
	for _, stray := range scan.Strays {
		if stray.ArchiveID == packArchive {
			t.Errorf("a thumbnail pack's shard was offered for reattachment: %+v", stray)
		}
	}
	for _, item := range scan.Items {
		if item.ArchiveID == packArchive {
			t.Errorf("a thumbnail pack's shard was reported as abandoned: %+v", item)
		}
	}

	if report, err := v.ReattachShards(ctx, false); err != nil {
		t.Fatalf("ReattachShards: %v", err)
	} else if report.Shards != 0 {
		t.Errorf("it reattached %d pack shard(s)", report.Shards)
	}
	if _, err := v.SweepOrphans(ctx, nil, false); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if !holdsArchive(storedObjects(t, roots), packArchive) {
		t.Error("the sweep erased a live thumbnail pack")
	}
}

func TestReattachSkipsAChunkedShardThatIsOnlyPartlyThere(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	// Big enough to be cut into several chunks, so a shard is several objects
	// and half of it can go missing.
	payload := make([]byte, 3*int(v.uploadChunkSize()))
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/", "film.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.ChunkCount < 2 {
		t.Fatalf("the file went up in %d chunk(s), want several", entry.ChunkCount)
	}

	victim := entry.Shards[0]
	root := ""
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, cfg := range accounts {
		if cfg.ID == victim.ProviderID {
			root = cfg.Options["path"]
		}
	}

	// The record goes, and so does one of the shard's chunks — a half-finished
	// erase, or an account that lost an object. What is left cannot stand in
	// for the shard, and saying it can would tell the file it has a spare.
	if err := os.Remove(filepath.Join(root, ChunkShardKey(entry.ArchiveID, 1, victim.Part))); err != nil {
		t.Fatalf("removing a chunk: %v", err)
	}
	v.mu.Lock()
	for _, e := range v.manifest.Entries {
		if e.ID != entry.ID {
			continue
		}
		kept := e.Shards[:0]
		for _, sh := range e.Shards {
			if sh.ProviderID != victim.ProviderID {
				kept = append(kept, sh)
			}
		}
		e.Shards = kept
	}
	v.mu.Unlock()

	scan := orphanScan(t, v)
	if len(scan.Strays) != 1 {
		t.Fatalf("found %d mislaid shard(s), want 1: %+v", len(scan.Strays), scan.Strays)
	}
	if scan.Strays[0].Reattachable {
		t.Fatalf("a shard missing a chunk was offered: %+v", scan.Strays[0])
	}
	if !strings.Contains(scan.Strays[0].Reason, "chunks") {
		t.Errorf("the refusal does not say why: %q", scan.Strays[0].Reason)
	}
	if report, err := v.ReattachShards(ctx, false); err != nil {
		t.Fatalf("ReattachShards: %v", err)
	} else if report.Shards != 0 {
		t.Errorf("it was reattached anyway: %+v", report)
	}
}

func TestAChunkedShardComesBackWholeWithTheRightKeyAndSize(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	payload := make([]byte, 3*int(v.uploadChunkSize()))
	for i := range payload {
		payload[i] = byte(i * 11)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/", "film.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	victim := entry.Shards[0]

	v.mu.Lock()
	for _, e := range v.manifest.Entries {
		if e.ID != entry.ID {
			continue
		}
		kept := e.Shards[:0]
		for _, sh := range e.Shards {
			if sh.ProviderID != victim.ProviderID {
				kept = append(kept, sh)
			}
		}
		e.Shards = kept
	}
	v.mu.Unlock()

	report, err := v.ReattachShards(ctx, false)
	if err != nil {
		t.Fatalf("ReattachShards: %v", err)
	}
	if report.Shards != 1 {
		t.Fatalf("reattached %d shard(s), want 1: %+v", report.Shards, report)
	}

	fixed := v.entryByID(t, entry.ID)
	for _, sh := range fixed.Shards {
		if sh.ProviderID != victim.ProviderID {
			continue
		}
		// One record describes the whole shard: chunk zero's key, and the
		// weight of every chunk together. See putChunkParts.
		if want := ChunkShardKey(entry.ArchiveID, 0, sh.Part); sh.Key != want {
			t.Errorf("recorded key %q, want chunk zero's %q", sh.Key, want)
		}
		if sh.Size != victim.Size {
			t.Errorf("recorded %d byte(s), want the shard's %d", sh.Size, victim.Size)
		}
	}

	// The proof it is a usable record and not just a plausible one.
	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the repair: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("the file came back changed")
	}
}

func TestReattachTakesOneCopyOfAShardAndSaysSoAboutTheOther(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "twice.txt", []byte("copied and forgotten"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	victim := entry.Shards[0]
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	roots := map[string]string{}
	for _, cfg := range accounts {
		roots[cfg.ID] = cfg.Options["path"]
	}

	// The same shard sitting on two accounts, which is what an interrupted
	// relocation leaves behind, with the record for neither.
	blob, err := os.ReadFile(filepath.Join(roots[victim.ProviderID], victim.Key))
	if err != nil {
		t.Fatalf("reading a part: %v", err)
	}
	spare := ""
	for _, cfg := range accounts {
		used := false
		for _, sh := range entry.Shards {
			if sh.ProviderID == cfg.ID {
				used = true
			}
		}
		if !used {
			spare = cfg.ID
		}
	}
	if spare == "" {
		// Every account holds a shard, so plant the copy on the one holding
		// the shard we are about to un-record — still two copies, one record.
		t.Skip("no account free of this file's shards")
	}
	if err := os.WriteFile(filepath.Join(roots[spare], victim.Key), blob, 0o600); err != nil {
		t.Fatalf("planting a copy: %v", err)
	}

	v.mu.Lock()
	for _, e := range v.manifest.Entries {
		if e.ID != entry.ID {
			continue
		}
		kept := e.Shards[:0]
		for _, sh := range e.Shards {
			if sh.ProviderID != victim.ProviderID {
				kept = append(kept, sh)
			}
		}
		e.Shards = kept
	}
	v.mu.Unlock()

	scan := orphanScan(t, v)
	if len(scan.Strays) != 2 {
		t.Fatalf("found %d copies, want 2: %+v", len(scan.Strays), scan.Strays)
	}
	if scan.Reattachable != 1 {
		t.Fatalf("%d copies were offered, want exactly one — recording a shard twice is not "+
			"redundancy: %+v", scan.Reattachable, scan.Strays)
	}

	report, err := v.ReattachShards(ctx, false)
	if err != nil {
		t.Fatalf("ReattachShards: %v", err)
	}
	if report.Shards != 1 {
		t.Fatalf("reattached %d, want 1: %+v", report.Shards, report)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "same shard") {
		t.Errorf("the second copy was not explained: %+v", report.Skipped)
	}
	if got := v.entryByID(t, entry.ID).Redundancy(); got != 3 {
		t.Errorf("the file has %d distinct shard(s), want 3", got)
	}
}

// entryByID reads one entry back out of the live index.
func (v *Vault) entryByID(t *testing.T, id string) *Entry {
	t.Helper()

	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, e := range v.manifest.Entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no entry with id %s", id)
	return nil
}
