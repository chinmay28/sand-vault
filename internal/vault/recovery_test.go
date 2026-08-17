package vault

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// reconnect builds a fresh vault on a new machine and connects it to the
// account folders a previous vault was using — which is what a reinstall looks
// like from the vault's point of view: the same data, brand new account IDs.
//
// Only the roots named are connected, so a test can model somebody who has got
// two of their three clouds back and not the third.
func reconnect(t *testing.T, password string, roots []string) *Vault {
	t.Helper()

	fresh, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fresh.Init(password, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// This vault pushes backups of its own in the background, into the very
	// folders the test is about to clean up.
	t.Cleanup(fresh.AwaitBackupSync)

	for i, root := range roots {
		if _, err := fresh.AddProvider(context.Background(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "reconnected-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider %d: %v", i, err)
		}
	}
	return fresh
}

// storedVault returns three account folders holding two files and the index
// backup that describes them, with the vault that wrote them locked away.
func storedVault(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()

	original, roots := newTestVault(t, 3)
	if err := original.Mkdir(MainScope, "/work"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := original.Upload(ctx, MainScope, "/work", "q4.txt", []byte("the quarterly numbers"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := original.Upload(ctx, MainScope, "/", "readme.md", []byte("top level"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if warnings, err := original.SyncManifestBackup(ctx, false); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}
	original.Lock()
	return roots
}

func TestScanNoticesAVaultLeftOnTheAccounts(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	// The new machine: empty vault, its own password, the same three clouds.
	fresh := reconnect(t, "a completely different password", roots)

	scan, err := fresh.ScanForRecovery(ctx)
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if !scan.VaultEmpty {
		t.Error("a vault holding no files should report itself empty")
	}
	if !scan.Available {
		t.Fatalf("scan found nothing to recover: %+v", scan.Sources)
	}
	if len(scan.Sources) != 3 {
		t.Fatalf("scanned %d accounts, want 3", len(scan.Sources))
	}
	if scan.Parts == 0 || scan.Bytes == 0 {
		t.Errorf("scan counted %d part(s), %d byte(s) — want the stored parts",
			scan.Parts, scan.Bytes)
	}
	for _, source := range scan.Sources {
		if source.Error != "" {
			t.Errorf("%s: %s", source.Name, source.Error)
		}
		// Every account carries the same copy, which is what makes any one of
		// them enough to recover from.
		if !source.Backup || !source.Foreign {
			t.Errorf("%s: backup=%v foreign=%v, want a foreign backup on every account",
				source.Name, source.Backup, source.Foreign)
		}
		if source.Parts == 0 {
			t.Errorf("%s holds no parts", source.Name)
		}
	}
}

func TestScanDoesNotOfferAVaultItsOwnBackup(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, err := v.SyncManifestBackup(ctx, false); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}

	scan, err := v.ScanForRecovery(ctx)
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if scan.Available {
		t.Error("a vault should not be offered a recovery from its own backup")
	}
	for _, source := range scan.Sources {
		if !source.Backup {
			t.Errorf("%s should be holding this vault's backup", source.Name)
		}
		if source.Foreign {
			t.Errorf("%s: this vault's own backup was read as another vault's", source.Name)
		}
	}
}

func TestScanIgnoresWhatIsNotOurs(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	// Somebody else's file, sharing the folder SAND was pointed at.
	if err := os.WriteFile(filepath.Join(roots[0], "holiday.jpg"), []byte("not ours"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fresh := reconnect(t, "another password", roots)
	scan, err := fresh.ScanForRecovery(ctx)
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}

	var counted int
	for _, source := range scan.Sources {
		counted += source.Parts
	}
	if counted != scan.Parts {
		t.Fatalf("scan totals %d parts but its sources add to %d", scan.Parts, counted)
	}

	// The stranger's photo is in the folder and is not one of ours: it does not
	// end in .sand, so it is not counted and — more to the point — the account
	// it sits on is not reported as holding one part more than it does.
	before := scan.Parts
	if err := os.Remove(filepath.Join(roots[0], "holiday.jpg")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	again, err := fresh.ScanForRecovery(ctx)
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if again.Parts != before {
		t.Errorf("removing a file SAND never wrote changed the count: %d then %d",
			before, again.Parts)
	}
}

func TestScanFindsNothingOnEmptyAccounts(t *testing.T) {
	fresh := reconnect(t, "a password", []string{t.TempDir(), t.TempDir()})

	scan, err := fresh.ScanForRecovery(context.Background())
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if scan.Available || scan.Parts != 0 {
		t.Errorf("empty accounts should offer nothing: %+v", scan)
	}
	if len(scan.Warnings) != 0 {
		t.Errorf("an account with nothing on it is not a warning: %v", scan.Warnings)
	}
}

func TestRecoveryReportNamesWhatDidNotComeBack(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	// One cloud out of three has been reconnected, which is one short of what
	// it takes to rebuild any file: two parts of three.
	fresh := reconnect(t, "another password", roots[:1])
	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}

	report, err := fresh.Recover(ctx, snapshot, true)
	if err != nil {
		t.Fatalf("Recover(dry run): %v", err)
	}

	if report.Files != 2 || report.Recoverable != 0 {
		t.Fatalf("report = %d file(s), %d recoverable; want neither openable",
			report.Files, report.Recoverable)
	}
	if report.Lost != 2 || report.Complete() {
		t.Errorf("report says %d lost, complete=%v; want both files reported missing",
			report.Lost, report.Complete())
	}
	if report.Bytes == 0 || report.LostBytes != report.Bytes {
		t.Errorf("report weighs %d bytes with %d lost; want the whole vault counted as lost",
			report.Bytes, report.LostBytes)
	}
	if report.RecoverableBytes != 0 {
		t.Errorf("nothing is openable, so no bytes are recoverable, got %d", report.RecoverableBytes)
	}

	// The list of what is missing is the point of the report.
	if len(report.Missing) != 2 {
		t.Fatalf("report names %d missing file(s), want 2", len(report.Missing))
	}
	for _, file := range report.Missing {
		if file.PartsFound != 1 || file.PartsNeeded != 2 {
			t.Errorf("%s: found %d of %d parts, want 1 of 2",
				file.Path, file.PartsFound, file.PartsNeeded)
		}
		if len(file.Accounts) != 2 {
			t.Errorf("%s: blames %v, want the two accounts that were not reconnected",
				file.Path, file.Accounts)
		}
	}

	// And so is the instruction that would change the outcome.
	if len(report.MissingAccounts) != 2 {
		t.Fatalf("report names %d missing account(s), want the 2 still to be connected: %+v",
			len(report.MissingAccounts), report.MissingAccounts)
	}
	for _, account := range report.MissingAccounts {
		if !account.Blocking {
			t.Errorf("%s holds parts of files that cannot be opened without it, so it blocks",
				account.Name)
		}
		if account.Files != 2 || account.Parts != 2 {
			t.Errorf("%s: %d file(s) over %d part(s), want 2 and 2",
				account.Name, account.Files, account.Parts)
		}
		// Named as the lost vault knew it, since that is the name the user is
		// looking for when they go to reconnect it.
		if account.Name == "" || account.Kind != string(provider.KindLocal) {
			t.Errorf("missing account is unidentifiable: %+v", account)
		}
	}
}

func TestRecoveryReportsDegradedFilesAsRecovered(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	// Two clouds of three: enough to rebuild every file, with nothing to spare.
	fresh := reconnect(t, "another password", roots[:2])
	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}

	report, err := fresh.Recover(ctx, snapshot, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if report.Recoverable != 2 || !report.Complete() {
		t.Fatalf("report = %+v, want both files back", report)
	}
	if report.RecoverableBytes != report.Bytes || report.LostBytes != 0 {
		t.Errorf("report weighs %d of %d bytes recoverable, %d lost; want all of it back",
			report.RecoverableBytes, report.Bytes, report.LostBytes)
	}
	if report.Degraded != 2 {
		t.Errorf("report says %d degraded, want both files marked as having no spare part",
			report.Degraded)
	}
	if len(report.Missing) != 0 {
		t.Errorf("nothing was lost, so nothing should be named: %+v", report.Missing)
	}
	// The third account still holds a part of every file, so it is still worth
	// naming — as a spare, not as a blocker.
	if len(report.MissingAccounts) != 1 {
		t.Fatalf("want the one account still to be reconnected: %+v", report.MissingAccounts)
	}
	if report.MissingAccounts[0].Blocking {
		t.Error("an account holding only spare parts does not block anything")
	}

	// And the vault really did take the files on.
	listing, err := fresh.List(MainScope, "/work")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "q4.txt" {
		t.Fatalf("recovered listing = %+v", listing.Files)
	}
	got, _, err := fresh.Fetch(ctx, listing.Files[0].ID)
	if err != nil {
		t.Fatalf("Fetch after recovery: %v", err)
	}
	if string(got) != "the quarterly numbers" {
		t.Fatalf("recovered file = %q", got)
	}
}

// The report tells someone which accounts to connect. This is that advice being
// taken: a recovery run with one cloud of three, then the other two connected
// and the job finished.
func TestResumingARecoveryOnceTheRestOfTheCloudsAreBack(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	fresh := reconnect(t, "another password", roots[:1])
	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}
	first, err := fresh.Recover(ctx, snapshot, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if first.Lost != 2 {
		t.Fatalf("first pass = %+v, want both files stranded", first)
	}

	// The index knows about them and cannot reach them, which is exactly the
	// state a partial recovery is supposed to leave behind.
	stats, err := fresh.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Files != 2 || stats.Stranded != 2 || stats.Unresolved != 4 {
		t.Fatalf("stats = %d file(s), %d stranded, %d unresolved shard(s); want 2, 2 and 4",
			stats.Files, stats.Stranded, stats.Unresolved)
	}
	// And a second Recover is not the way out of it: adopting the snapshot
	// again would replace the data key those files now depend on.
	if _, err := fresh.Recover(ctx, snapshot, false); err == nil {
		t.Error("recovering a second time into a vault holding files should be refused")
	}

	// The user goes and finds their other two clouds.
	for i, root := range roots[1:] {
		if _, err := fresh.AddProvider(ctx, provider.Config{
			Kind:    provider.KindLocal,
			Name:    "late-" + string(rune('b'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	preview, err := fresh.Reconcile(ctx, true)
	if err != nil {
		t.Fatalf("Reconcile(dry run): %v", err)
	}
	if preview.Recoverable != 2 || !preview.Complete() {
		t.Fatalf("preview = %+v, want both files reachable again", preview)
	}
	if before, _ := fresh.Stats(); before.Stranded != 2 {
		t.Error("a dry run should not have re-pointed anything")
	}

	report, err := fresh.Reconcile(ctx, false)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Recoverable != 2 || report.Lost != 0 {
		t.Fatalf("report = %+v, want both files back", report)
	}
	if report.Relocated == 0 {
		t.Error("the parts on the late accounts had to be re-pointed at them")
	}
	if len(report.MissingAccounts) != 0 {
		t.Errorf("nothing is missing any more: %+v", report.MissingAccounts)
	}

	after, err := fresh.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if after.Stranded != 0 || after.Unresolved != 0 {
		t.Errorf("stats still report %d stranded over %d unresolved shard(s)",
			after.Stranded, after.Unresolved)
	}

	// The proof: a file that could not be opened before this now opens.
	listing, err := fresh.List(MainScope, "/work")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _, err := fresh.Fetch(ctx, listing.Files[0].ID)
	if err != nil {
		t.Fatalf("Fetch after resuming: %v", err)
	}
	if string(got) != "the quarterly numbers" {
		t.Fatalf("recovered file = %q", got)
	}

	// And the re-pointing survives a reload, rather than living in memory.
	reopened, err := Open(fresh.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.Unlock("another password"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(reopened.AwaitBackupSync)
	if persisted, err := reopened.Stats(); err != nil || persisted.Unresolved != 0 {
		t.Errorf("reopened vault reports %d unresolved shard(s) (%v)", persisted.Unresolved, err)
	}
}

func TestResumeIsOfferedOnlyWhileSomethingIsOutOfReach(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("hello"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	scan, err := v.ScanForRecovery(ctx)
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if scan.Resumable || scan.Unresolved != 0 || scan.Stranded != 0 {
		t.Errorf("a healthy vault has nothing to resume: %+v", scan)
	}
}

func TestScanStopsOfferingOnceRecovered(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	fresh := reconnect(t, "another password", roots)
	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}
	if _, err := fresh.Recover(ctx, snapshot, false); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// The recovering vault claims the accounts, which is what stops the copies
	// on them still reading as somebody else's.
	if warnings, err := fresh.SyncManifestBackup(ctx, true); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}

	scan, err := fresh.ScanForRecovery(ctx)
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if scan.Available {
		t.Error("a recovered vault should not be offered the same recovery again")
	}
	if scan.VaultEmpty {
		t.Error("the vault holds the recovered files, so it is not empty")
	}
	for _, source := range scan.Sources {
		if source.Foreign {
			t.Errorf("%s still reads as another vault's after being claimed", source.Name)
		}
	}
}

func TestRecoveryTruncatesAVeryLongListOfMissingFiles(t *testing.T) {
	ctx := context.Background()

	// More missing files than the report will name one by one. The counts stay
	// exact; only the list is cut, and it has to say by how much.
	snapshot := &Snapshot{
		Version:  backupVersion,
		Policy:   PolicyStrict,
		Manifest: newManifest(),
	}
	const files = maxMissingListed + 7
	for i := 0; i < files; i++ {
		snapshot.Manifest.Entries = append(snapshot.Manifest.Entries, &Entry{
			ID:   string(rune('a'+i%26)) + string(rune('0'+i%10)),
			Dir:  "/",
			Name: "gone-" + string(rune('a'+i%26)),
			Size: 100,
			Shards: []Shard{
				{Part: 1, ProviderID: "old-a", ProviderName: "cloud-a", Key: "missing-a"},
				{Part: 2, ProviderID: "old-b", ProviderName: "cloud-b", Key: "missing-b"},
			},
		})
	}
	snapshot.DataKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, DataKeySize))

	fresh := reconnect(t, "a password", []string{t.TempDir()})
	report, err := fresh.Recover(ctx, snapshot, true)
	if err != nil {
		t.Fatalf("Recover(dry run): %v", err)
	}

	if report.Lost != files {
		t.Errorf("report says %d lost, want every one of the %d", report.Lost, files)
	}
	if len(report.Missing) != maxMissingListed {
		t.Errorf("report names %d files, want the cap of %d", len(report.Missing), maxMissingListed)
	}
	if report.MissingTruncated != files-maxMissingListed {
		t.Errorf("report hid %d files without saying so — want %d",
			report.MissingTruncated, files-maxMissingListed)
	}
}
