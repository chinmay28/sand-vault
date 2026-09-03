package vault

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/provider/s3test"
)

// The scenario this file exists for: a bucket that keeps every version, the
// way Backblaze B2 does out of the box. Every rewrite of the index backup and
// every deleted part goes on being stored beneath what a listing shows, the
// usage bar says one thing and the bill another, and the only way out is to
// erase the history version by version — without touching the current
// version of anything.

// newVersionedVault opens a vault on the given number of versioned buckets,
// all on one stub server. The buckets are named after their index.
func newVersionedVault(t *testing.T, buckets int) (*Vault, *s3test.Server, []provider.Config) {
	t.Helper()

	stub := s3test.New()
	t.Cleanup(stub.Close)

	v, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)

	configs := make([]provider.Config, buckets)
	for i := range configs {
		name := "bucket-" + string(rune('a'+i))
		cfg, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    provider.KindS3,
			Name:    name,
			Options: stub.Options(name),
		})
		if err != nil {
			t.Fatalf("AddProvider %s: %v", name, err)
		}
		configs[i] = cfg
	}
	return v, stub, configs
}

// versionScan runs a scan and fails the test rather than the caller.
func versionScan(t *testing.T, v *Vault) *VersionScan {
	t.Helper()

	v.AwaitBackupSync()
	scan, err := v.ScanForStaleVersions(context.Background())
	if err != nil {
		t.Fatalf("ScanForStaleVersions: %v", err)
	}
	return scan
}

// rowsFor picks the rows a scan reported for one key, across accounts.
func rowsFor(scan *VersionScan, key string) []VersionKey {
	var rows []VersionKey
	for _, row := range scan.Items {
		if row.Key == key {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestStaleVersionsPileUpUnderTheIndexBackupAndDeletedParts(t *testing.T) {
	ctx := context.Background()
	v, stub, configs := newVersionedVault(t, 3)

	doomed, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("this one gets deleted"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	keeper, _, err := v.Upload(ctx, MainScope, "/", "kept.txt", []byte("this one stays"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	scan := versionScan(t, v)
	if !scan.Found {
		t.Fatal("a versioned bucket that has seen uploads and a delete reports nothing stale")
	}
	if scan.Versioned != 3 {
		t.Errorf("%d account(s) could be asked for versions, want all 3", scan.Versioned)
	}
	if len(scan.Warnings) > 0 {
		t.Errorf("unexpected warnings: %v", scan.Warnings)
	}

	// The index backup was rewritten on every change — three accounts
	// connected, two uploads, a delete — so each bucket holds a stack of them
	// with exactly one that matters.
	backups := rowsFor(scan, BackupKey)
	if len(backups) != 3 {
		t.Fatalf("the index backup has stale versions on %d account(s), want all 3: %+v", len(backups), scan.Items)
	}
	for _, row := range backups {
		if row.Versions < 1 || !row.Deletable || row.What != "the index backup" {
			t.Errorf("index backup row is not what it should be: %+v", row)
		}
	}

	// The deleted file's parts are a marker over their data on every account
	// that held one, and nothing points at them any more.
	deletedRows := 0
	for _, row := range scan.Items {
		id, _, ours := partOfKey(row.Key)
		if !ours || id != doomed.ArchiveID {
			continue
		}
		deletedRows++
		if row.Markers != 1 || row.Versions != 1 || !row.Deletable {
			t.Errorf("a deleted part should be one marker over one version, deletable: %+v", row)
		}
		if row.What != "a part of a deleted file" {
			t.Errorf("a deleted part is described as %q", row.What)
		}
	}
	if deletedRows != 3 {
		t.Errorf("the deleted file left stale versions on %d account(s), want 3", deletedRows)
	}

	// And the file that is still stored is not implicated at all: one live
	// version per part, nothing beneath it.
	for _, row := range scan.Items {
		if id, _, ours := partOfKey(row.Key); ours && id == keeper.ArchiveID {
			t.Errorf("a stored file's part was reported as stale: %+v", row)
		}
	}

	// The account rows add up, and say what the bill sees.
	for _, account := range scan.Accounts {
		if !account.Versioned || account.Error != "" {
			t.Errorf("%s could not be asked: %+v", account.Name, account)
		}
		if account.Current < 2 {
			// The index backup and at least one part of the kept file.
			t.Errorf("%s reports %d current object(s), want at least the backup and a part", account.Name, account.Current)
		}
		if account.Stale == 0 || account.Deletable != account.Stale {
			t.Errorf("%s should have stale versions and all of them deletable: %+v", account.Name, account)
		}
		if account.Markers != 1 {
			t.Errorf("%s holds %d marker(s), want the deleted part's one", account.Name, account.Markers)
		}
	}
	if scan.Deletable != scan.Stale || scan.DeletableBytes != scan.StaleBytes {
		t.Errorf("the totals disagree with the rows: %+v", scan)
	}

	// A dry run promises exactly what the scan showed, and erases nothing.
	before := 0
	for _, cfg := range configs {
		before += len(stub.Versions(cfg.Name))
	}
	preview, err := v.SweepStaleVersions(ctx, nil, true, nil)
	if err != nil {
		t.Fatalf("SweepStaleVersions dry run: %v", err)
	}
	if preview.Deleted != scan.Deletable || preview.Bytes != scan.DeletableBytes || preview.Markers != scan.Markers {
		t.Errorf("dry run promised %+v, the scan showed %d version(s) / %d bytes / %d marker(s)",
			preview, scan.Deletable, scan.DeletableBytes, scan.Markers)
	}
	after := 0
	for _, cfg := range configs {
		after += len(stub.Versions(cfg.Name))
	}
	if after != before {
		t.Fatalf("a dry run erased something: %d version(s) before, %d after", before, after)
	}

	// The sweep itself, counted as it goes.
	var progress [][2]int
	report, err := v.SweepStaleVersions(ctx, nil, false, func(done, total int) {
		progress = append(progress, [2]int{done, total})
	})
	if err != nil {
		t.Fatalf("SweepStaleVersions: %v", err)
	}
	if len(report.Warnings) > 0 || len(report.Skipped) > 0 {
		t.Errorf("the sweep complained: warnings %v, skipped %v", report.Warnings, report.Skipped)
	}
	if report.Deleted != preview.Deleted || report.Bytes != preview.Bytes {
		t.Errorf("the sweep erased %+v, the dry run promised %+v", report, preview)
	}
	if len(progress) == 0 || progress[0] != [2]int{0, preview.Deleted} {
		t.Errorf("progress did not open with (0, total): %v", progress)
	}
	if last := progress[len(progress)-1]; last[0] != last[1] || last[0] != preview.Deleted {
		t.Errorf("progress did not close on (total, total): %v", progress)
	}

	// What is left on every bucket is exactly the current object of each key:
	// no markers, no history, the deleted file entirely gone.
	for _, cfg := range configs {
		versions := stub.Versions(cfg.Name)
		objects := stub.Objects(cfg.Name)
		if len(versions) != len(objects) {
			t.Errorf("%s still stores %d version(s) of %d object(s): %+v", cfg.Name, len(versions), len(objects), versions)
		}
		for _, ver := range versions {
			if ver.DeleteMarker {
				t.Errorf("%s still carries a delete marker on %s", cfg.Name, ver.Key)
			}
			if id, _, ours := partOfKey(ver.Key); ours && id == doomed.ArchiveID {
				t.Errorf("%s still holds a part of the deleted file: %s", cfg.Name, ver.Key)
			}
		}
	}

	// And nothing that mattered was touched.
	if data, _, err := v.Fetch(ctx, keeper.ID); err != nil || string(data) != "this one stays" {
		t.Fatalf("the kept file no longer reads back after the sweep: %q, %v", data, err)
	}
	if again := versionScan(t, v); again.Found {
		t.Errorf("a second scan straight after the sweep still finds %d stale version(s): %+v", again.Stale, again.Items)
	}
}

func TestStaleVersionsHoldBackAPartDeletedBehindTheIndexsBack(t *testing.T) {
	ctx := context.Background()
	v, stub, configs := newVersionedVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "kept.txt", []byte("still in the index"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	// Somebody deletes one part from the bucket's console. The index still
	// points at it; the bucket now has a marker over the only copy.
	var victim Shard
	for _, sh := range entry.Shards {
		if sh.ProviderID == configs[0].ID {
			victim = sh
		}
	}
	if victim.Key == "" {
		t.Fatalf("no part of the file landed on %s: %+v", configs[0].Name, entry.Shards)
	}
	console, err := provider.New(provider.Config{Kind: provider.KindS3, Options: stub.Options(configs[0].Name)})
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	if err := console.Delete(ctx, victim.Key); err != nil {
		t.Fatalf("Delete behind the index's back: %v", err)
	}

	scan := versionScan(t, v)
	rows := rowsFor(scan, victim.Key)
	if len(rows) != 1 {
		t.Fatalf("the deleted-from-outside part has %d row(s), want 1: %+v", len(rows), scan.Items)
	}
	if rows[0].Deletable || !strings.Contains(rows[0].Reason, "only copies left") {
		t.Errorf("the only copy of a part the index points at is offered for erasing: %+v", rows[0])
	}
	if !strings.Contains(rows[0].What, "deleted by something other than SAND") {
		t.Errorf("the row does not say what happened: %+v", rows[0])
	}

	// The sweep leaves it exactly where it is, whatever else it erases.
	if _, err := v.SweepStaleVersions(ctx, nil, false, nil); err != nil {
		t.Fatalf("SweepStaleVersions: %v", err)
	}
	kept := 0
	for _, ver := range stub.Versions(configs[0].Name) {
		if ver.Key == victim.Key && !ver.DeleteMarker {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("the only copy of the part was erased: %+v", stub.Versions(configs[0].Name))
	}
}

func TestStaleVersionsAreLeftAloneOnAnotherVaultsBucket(t *testing.T) {
	ctx := context.Background()
	stub := s3test.New()
	defer stub.Close()

	// Another vault owns the bucket: its index backup is there, and so is a
	// part it deleted.
	other, err := Open(filepath.Join(t.TempDir(), "other.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := other.Init("somebody-else's-password", PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := other.AddProvider(ctx, provider.Config{
		Kind: provider.KindS3, Name: "shared", Options: stub.Options("shared"),
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	other.AwaitBackupSync()
	theirs, err := provider.New(provider.Config{Kind: provider.KindS3, Options: stub.Options("shared")})
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	if err := theirs.Put(ctx, "0123456789abcdef-p1.sand", []byte("their part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := theirs.Delete(ctx, "0123456789abcdef-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// This vault connects the same bucket alongside its own.
	v, _ := newTestVault(t, 2)
	shared, err := v.AddProvider(ctx, provider.Config{
		Kind: provider.KindS3, Name: "borrowed", Options: stub.Options("shared"),
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	scan := versionScan(t, v)
	var account VersionAccount
	for _, a := range scan.Accounts {
		if a.ProviderID == shared.ID {
			account = a
		}
	}
	if !account.Versioned || !account.Foreign || !account.Backup {
		t.Fatalf("the borrowed bucket is not recognised as another vault's: %+v", account)
	}
	if account.Stale == 0 {
		t.Fatalf("the other vault's deleted part is not counted as stale: %+v", account)
	}
	if account.Deletable != 0 || scan.Deletable != 0 {
		t.Errorf("another vault's history is offered for erasing: %+v", account)
	}
	for _, row := range scan.Items {
		if row.Deletable || !strings.Contains(row.Reason, "did not write") {
			t.Errorf("row on the borrowed bucket is deletable or unexplained: %+v", row)
		}
	}

	// The local accounts cannot be asked, and say so without it being an
	// error.
	for _, a := range scan.Accounts {
		if a.ProviderID != shared.ID && (a.Versioned || a.Error != "") {
			t.Errorf("a local folder claims to keep versions: %+v", a)
		}
	}

	before := len(stub.Versions("shared"))
	report, err := v.SweepStaleVersions(ctx, nil, false, nil)
	if err != nil {
		t.Fatalf("SweepStaleVersions: %v", err)
	}
	if report.Deleted != 0 || len(stub.Versions("shared")) != before {
		t.Errorf("the sweep touched another vault's bucket: %+v", report)
	}
}

func TestStaleVersionSweepCanBeAimedAtOneAccount(t *testing.T) {
	ctx := context.Background()
	v, stub, configs := newVersionedVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("deleted on all three"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Delete(ctx, entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	scan := versionScan(t, v)

	var only VersionAccount
	for _, a := range scan.Accounts {
		if a.ProviderID == configs[0].ID {
			only = a
		}
	}
	report, err := v.SweepStaleVersions(ctx, []string{configs[0].ID, "no-such-account"}, false, nil)
	if err != nil {
		t.Fatalf("SweepStaleVersions: %v", err)
	}
	if report.Deleted != only.Deletable {
		t.Errorf("aimed at %s, the sweep erased %d, want its %d", configs[0].Name, report.Deleted, only.Deletable)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "no-such-account") {
		t.Errorf("an unknown account was not reported: %v", report.Skipped)
	}

	// The aimed-at bucket is clean; the other two are as they were.
	for _, ver := range stub.Versions(configs[0].Name) {
		if ver.DeleteMarker {
			t.Errorf("%s still carries %+v", configs[0].Name, ver)
		}
	}
	again := versionScan(t, v)
	for _, a := range again.Accounts {
		switch {
		case a.ProviderID == configs[0].ID && a.Stale != 0:
			t.Errorf("%s was swept and still reports %d stale", a.Name, a.Stale)
		case a.ProviderID != configs[0].ID && a.Stale == 0:
			t.Errorf("%s was not asked for and was swept anyway", a.Name)
		}
	}
}

// The decision itself, against every shape of bucket, with no I/O.
func TestClassifyVersions(t *testing.T) {
	ver := func(key, id string, size int64, latest, marker bool) provider.ObjectVersion {
		return provider.ObjectVersion{Key: key, VersionID: id, Size: size, Latest: latest, DeleteMarker: marker}
	}
	owns := func(id string) bool { return id == "0000000000000000" }
	const stored, deleted = "0000000000000000-p1.sand", "ffffffffffffffff-p2.sand"

	t.Run("an unversioned bucket has nothing stale", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(BackupKey, "null", 10, true, false),
			ver(stored, "null", 100, true, false),
		}, owns, "")
		if len(rows) != 0 || doomed.count() != 0 {
			t.Errorf("rows %+v, doomed %+v", rows, doomed)
		}
		if account.Current != 2 || account.CurrentBytes != 110 || account.Stale != 0 {
			t.Errorf("account %+v", account)
		}
	})

	t.Run("superseded backups and deleted parts go, data before markers", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(BackupKey, "3", 30, true, false),
			ver(BackupKey, "2", 20, false, false),
			ver(BackupKey, "1", 10, false, false),
			ver(deleted, "m", 0, true, true),
			ver(deleted, "d", 500, false, false),
			ver(stored, "s", 100, true, false),
		}, owns, "")
		if len(rows) != 2 {
			t.Fatalf("rows %+v", rows)
		}
		for _, row := range rows {
			if !row.Deletable {
				t.Errorf("held back: %+v", row)
			}
		}
		if account.Current != 2 || account.CurrentBytes != 130 {
			t.Errorf("current %d / %d, want the backup and the stored part", account.Current, account.CurrentBytes)
		}
		if account.Stale != 4 || account.StaleBytes != 530 || account.Markers != 1 || account.Deletable != 4 {
			t.Errorf("account %+v", account)
		}
		if len(doomed.data) != 3 || len(doomed.markers) != 1 || doomed.markers[0].VersionID != "m" {
			t.Errorf("doomed %+v", doomed)
		}
	})

	t.Run("a marker over a part the index still wants is held", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(stored, "m", 0, true, true),
			ver(stored, "d", 100, false, false),
		}, owns, "")
		if len(rows) != 1 || rows[0].Deletable || rows[0].Reason == "" {
			t.Fatalf("rows %+v", rows)
		}
		if doomed.count() != 0 || account.Deletable != 0 || account.Stale != 2 {
			t.Errorf("doomed %+v, account %+v", doomed, account)
		}
	})

	t.Run("a marker beneath a live backup is stale", func(t *testing.T) {
		// The backup was switched off, its object deleted, and switched back
		// on: a marker in the middle of the stack.
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(BackupKey, "3", 30, true, false),
			ver(BackupKey, "m", 0, false, true),
			ver(BackupKey, "1", 10, false, false),
		}, owns, "")
		if len(rows) != 1 || rows[0].Versions != 1 || rows[0].Markers != 1 || !rows[0].Deletable {
			t.Fatalf("rows %+v", rows)
		}
		if len(doomed.data) != 1 || len(doomed.markers) != 1 {
			t.Errorf("doomed %+v", doomed)
		}
	})

	t.Run("an orphan's current version is not this sweep's, its history is", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(deleted, "2", 200, true, false),
			ver(deleted, "1", 100, false, false),
		}, owns, "")
		if len(rows) != 1 || !rows[0].Deletable || rows[0].What != "a part no file points at" {
			t.Fatalf("rows %+v", rows)
		}
		if doomed.count() != 1 || doomed.data[0].VersionID != "1" {
			t.Errorf("doomed %+v", doomed)
		}
		if account.Current != 1 || account.CurrentBytes != 200 {
			t.Errorf("account %+v", account)
		}
	})

	t.Run("somebody else's files are counted and never touched", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver("photos/cat.jpg", "2", 2000, true, false),
			ver("photos/cat.jpg", "1", 1000, false, false),
			ver("notes.txt", "m", 0, true, true),
			ver("notes.txt", "1", 50, false, false),
		}, owns, "")
		if len(rows) != 0 || doomed.count() != 0 {
			t.Errorf("rows %+v, doomed %+v", rows, doomed)
		}
		if account.Other != 3 || account.OtherBytes != 1050 || account.Current != 0 || account.Stale != 0 {
			t.Errorf("account %+v", account)
		}
	})

	t.Run("a hold reports every row and dooms nothing", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(BackupKey, "2", 20, true, false),
			ver(BackupKey, "1", 10, false, false),
		}, owns, "another vault's")
		if len(rows) != 1 || rows[0].Deletable || rows[0].Reason != "another vault's" {
			t.Fatalf("rows %+v", rows)
		}
		if doomed.count() != 0 || account.Stale != 1 || account.Deletable != 0 {
			t.Errorf("doomed %+v, account %+v", doomed, account)
		}
	})

	t.Run("a bucket that marks nothing current is not guessed at", func(t *testing.T) {
		account := &VersionAccount{Name: "b"}
		rows, doomed := classifyVersions(account, []provider.ObjectVersion{
			ver(BackupKey, "2", 20, false, false),
			ver(BackupKey, "1", 10, false, false),
		}, owns, "")
		if len(rows) != 1 || rows[0].Deletable || !strings.Contains(rows[0].Reason, "current") {
			t.Fatalf("rows %+v", rows)
		}
		if doomed.count() != 0 {
			t.Errorf("doomed %+v", doomed)
		}
	})
}
