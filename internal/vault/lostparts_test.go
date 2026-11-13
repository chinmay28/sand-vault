package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/provider"
)

// rootOfAccount is the folder standing in for one connected account.
func rootOfAccount(t *testing.T, v *Vault, id string) string {
	t.Helper()
	v.mu.RLock()
	defer v.mu.RUnlock()
	cfg, ok := v.configForLocked(id)
	if !ok {
		t.Fatalf("no account %s", id)
	}
	return cfg.Options["path"]
}

// emptyFolder removes everything in a folder and keeps the folder, which is
// what "I emptied it by accident" leaves behind.
func emptyFolder(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// partsOnDisk is every part object under the roots, by path, as a hash of its
// bytes: what the accounts are holding, to be compared before and after.
func partsOnDisk(t *testing.T, roots []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if _, _, ours := partOfKey(e.Name()); !ours {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			out[filepath.Join(root, e.Name())] = hex.EncodeToString(sum[:])
		}
	}
	return out
}

func lostScan(t *testing.T, v *Vault) *LostPartsScan {
	t.Helper()
	scan, err := v.ScanForLostParts(context.Background(), LostScanOptions{})
	if err != nil {
		t.Fatalf("ScanForLostParts: %v", err)
	}
	return scan
}

func restoreLost(t *testing.T, v *Vault, opts LostRestoreOptions) *LostRestoreReport {
	t.Helper()
	report, err := v.RestoreLostParts(context.Background(), opts)
	if err != nil {
		t.Fatalf("RestoreLostParts: %v", err)
	}
	return report
}

// shardOf is the record of one part of a file.
func shardOf(t *testing.T, e *Entry, part int) Shard {
	t.Helper()
	for _, s := range e.Shards {
		if s.Part == part {
			return s
		}
	}
	t.Fatalf("%s has no part %d", e.Path(), part)
	return Shard{}
}

// mustFetch reads a file with nothing cached and holds it to its bytes.
func mustFetch(t *testing.T, v *Vault, id string, want []byte) {
	t.Helper()
	v.chunks.clear()
	got, _, err := v.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the file read back differently")
	}
}

func TestLostPartsScanFindsNothingOnAVaultThatIsWhole(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	upload(t, v, "film.bin", noise(20_000, 1))
	before := partsOnDisk(t, roots)

	scan := lostScan(t, v)
	if scan.Found || scan.Parts != 0 || len(scan.Items) != 0 {
		t.Fatalf("a whole vault scanned as %+v", scan.LostSummary)
	}
	if len(scan.Accounts) != 3 {
		t.Fatalf("scan names %d accounts, want 3", len(scan.Accounts))
	}
	for _, a := range scan.Accounts {
		if a.Recorded == 0 || a.Lost != 0 || a.Emptied || a.Error != "" {
			t.Errorf("account %s = %+v", a.Name, a)
		}
	}

	report := restoreLost(t, v, LostRestoreOptions{})
	if report.Parts != 0 || report.Objects != 0 || report.Failed != 0 {
		t.Errorf("a restore with nothing lost reported %+v", report)
	}
	if after := partsOnDisk(t, roots); !reflect.DeepEqual(after, before) {
		t.Error("a restore with nothing lost changed what the accounts hold")
	}
}

// The case this was written for: a cloud's folder emptied by hand. Every part
// the index records there is found missing, remade from the other clouds and
// written back where it was — and the index is not touched to do it.
func TestAnEmptiedCloudIsFoundAndItsPartsPutBack(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	big, small := noise(40_000, 1), noise(900, 2)
	film := upload(t, v, "film.bin", big)
	note := upload(t, v, "note.txt", small)

	victim := shardOf(t, film, 2).ProviderID
	victimRoot := rootOfAccount(t, v, victim)
	emptyFolder(t, victimRoot)

	scan := lostScan(t, v)
	if !scan.Found || scan.Parts != 2 || scan.Files != 2 || scan.Restorable != 2 {
		t.Fatalf("scan = %+v, want two restorable parts of two files", scan.LostSummary)
	}
	wantObjects := film.ChunkCount + note.ChunkCount
	if scan.Objects != wantObjects {
		t.Errorf("scan counts %d lost objects, want %d", scan.Objects, wantObjects)
	}
	for _, a := range scan.Accounts {
		emptied := a.ProviderID == victim
		if a.Emptied != emptied {
			t.Errorf("account %s: emptied = %v, want %v", a.Name, a.Emptied, emptied)
		}
		if emptied && (a.Parts != 2 || a.Lost != wantObjects || a.Files != 2) {
			t.Errorf("the emptied account = %+v", a)
		}
	}
	for _, row := range scan.Items {
		if row.ProviderID != victim || !row.Restorable || row.Path == "" || row.Objects != row.Of {
			t.Errorf("row = %+v", row)
		}
	}

	var last LostRestoreProgress
	report := restoreLost(t, v, LostRestoreOptions{OnProgress: func(p LostRestoreProgress) { last = p }})
	if report.Parts != 2 || report.Files != 2 || report.Objects != wantObjects || report.Failed != 0 || report.Withheld != 0 {
		t.Fatalf("restore = %+v", report)
	}
	if report.Stopped || report.Bytes == 0 || len(report.On) != 1 {
		t.Errorf("restore = %+v", report)
	}
	if last.Stage != RestoreStageRestoring || last.Objects != wantObjects || last.Done != wantObjects || last.PartsDone != 2 {
		t.Errorf("last progress = %+v", last)
	}

	if again := lostScan(t, v); again.Found {
		t.Fatalf("after the restore the scan still finds %+v", again.LostSummary)
	}

	// The index was right all along about where the parts belong, and was
	// not written.
	for _, was := range []*Entry{film, note} {
		now, err := v.Entry(was.ID)
		if err != nil {
			t.Fatalf("Entry: %v", err)
		}
		if now.ArchiveID != was.ArchiveID || !reflect.DeepEqual(now.Shards, was.Shards) {
			t.Errorf("%s: the restore changed its record", was.Path())
		}
	}

	// What was put back is a real part: with either other cloud emptied in
	// turn, the files have to be read through it.
	for _, part := range []int{1, 3} {
		other := rootOfAccount(t, v, shardOf(t, film, part).ProviderID)
		held := t.TempDir()
		if err := os.CopyFS(held, os.DirFS(other)); err != nil {
			t.Fatal(err)
		}
		emptyFolder(t, other)
		mustFetch(t, v, film.ID, big)
		mustFetch(t, v, note.ID, small)
		if err := os.CopyFS(other, os.DirFS(held)); err != nil {
			t.Fatal(err)
		}
	}
}

// One object gone from a long file is one object remade, and nothing else on
// any account is written.
func TestOneLostObjectCostsOneObject(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	payload := noise(40_000, 3)
	film := upload(t, v, "film.bin", payload)
	if film.ChunkCount < 4 {
		t.Fatalf("fixture has %d chunks", film.ChunkCount)
	}

	gone := shardFile(t, roots, film, 2, 1)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	before := partsOnDisk(t, roots)

	scan := lostScan(t, v)
	if scan.Parts != 1 || scan.Objects != 1 {
		t.Fatalf("scan = %+v, want one object of one part", scan.LostSummary)
	}
	if row := scan.Items[0]; row.Part != 1 || row.Objects != 1 || row.Of != film.ChunkCount || !row.Restorable {
		t.Fatalf("row = %+v", row)
	}
	for _, a := range scan.Accounts {
		if a.Emptied {
			t.Errorf("%s reads as emptied over one object", a.Name)
		}
	}

	report := restoreLost(t, v, LostRestoreOptions{})
	if report.Parts != 1 || report.Objects != 1 {
		t.Fatalf("restore = %+v", report)
	}

	after := partsOnDisk(t, roots)
	if _, back := after[gone]; !back {
		t.Fatal("the lost object is not back where it was")
	}
	delete(after, gone)
	if !reflect.DeepEqual(after, before) {
		t.Error("the restore wrote something other than the one object that was lost")
	}

	// And it is read through: part 2's account emptied leaves parts 1 and 3.
	emptyFolder(t, rootOfAccount(t, v, shardOf(t, film, 2).ProviderID))
	mustFetch(t, v, film.ID, payload)
}

// Remaking is per chunk, so two parts that have each lost a different chunk
// can mend one another even though neither is whole.
func TestPartsThatLostDifferentChunksMendEachOther(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	payload := noise(40_000, 4)
	film := upload(t, v, "film.bin", payload)

	for _, at := range [][2]int{{0, 1}, {3, 2}} {
		if err := os.Remove(shardFile(t, roots, film, at[0], at[1])); err != nil {
			t.Fatal(err)
		}
	}

	scan := lostScan(t, v)
	if scan.Parts != 2 || scan.Restorable != 2 {
		t.Fatalf("scan = %+v, want two restorable parts", scan.LostSummary)
	}
	report := restoreLost(t, v, LostRestoreOptions{})
	if report.Parts != 2 || report.Objects != 2 || report.Failed != 0 {
		t.Fatalf("restore = %+v", report)
	}

	emptyFolder(t, rootOfAccount(t, v, shardOf(t, film, 3).ProviderID))
	mustFetch(t, v, film.ID, payload)
}

// A chunk two of whose three shards are gone cannot give either back, and a
// restore says so rather than writing anything.
func TestALostPartThatCannotBeRemadeIsWithheld(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	film := upload(t, v, "film.bin", noise(40_000, 5))
	whole := upload(t, v, "whole.bin", noise(5_000, 6))

	for _, part := range []int{1, 2} {
		if err := os.Remove(shardFile(t, roots, film, 1, part)); err != nil {
			t.Fatal(err)
		}
	}
	// A second file, mendable, on one of the same accounts: the one that
	// cannot be helped must not take it down with it.
	if err := os.Remove(shardFile(t, roots, whole, 0, 1)); err != nil {
		t.Fatal(err)
	}
	before := partsOnDisk(t, roots)

	scan := lostScan(t, v)
	if scan.Parts != 3 || scan.Restorable != 1 {
		t.Fatalf("scan = %+v, want three parts, one restorable", scan.LostSummary)
	}
	for _, row := range scan.Items {
		if row.FileID == film.ID && (row.Restorable || !strings.Contains(row.Reason, "only 1")) {
			t.Errorf("row = %+v", row)
		}
	}

	report := restoreLost(t, v, LostRestoreOptions{})
	if report.Parts != 1 || report.Withheld != 2 || report.Failed != 0 || len(report.Failures) != 2 {
		t.Fatalf("restore = %+v", report)
	}
	after := partsOnDisk(t, roots)
	if len(after) != len(before)+1 {
		t.Errorf("the restore wrote %d objects, want 1", len(after)-len(before))
	}
}

// A restore limited to one account leaves the others' lost parts alone.
func TestRestoreCanBeLimitedToOneAccount(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	a := upload(t, v, "a.bin", noise(3_000, 7))
	b := upload(t, v, "b.bin", noise(3_000, 8))

	if err := os.Remove(shardFile(t, roots, a, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shardFile(t, roots, b, 0, 2)); err != nil {
		t.Fatal(err)
	}
	only := shardOf(t, a, 1).ProviderID
	if only == shardOf(t, b, 2).ProviderID {
		t.Skip("the two parts landed on one account")
	}

	report := restoreLost(t, v, LostRestoreOptions{Accounts: []string{only}})
	if report.Parts != 1 {
		t.Fatalf("restore = %+v", report)
	}
	scan := lostScan(t, v)
	if scan.Parts != 1 || scan.Items[0].FileID != b.ID {
		t.Fatalf("after a restore of one account the scan finds %+v", scan.Items)
	}
}

// The account is asked for each object by name before it is written, and one
// that is there is left as it is whatever a listing said.
func TestRestoreNeverWritesOverAnObjectThatIsThere(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	film := upload(t, v, "film.bin", noise(10_000, 9))
	before := partsOnDisk(t, roots)

	s := shardOf(t, film, 1)
	row := &LostPart{
		ProviderID: s.ProviderID, ProviderName: s.ProviderName,
		FileID: film.ID, ArchiveID: film.ArchiveID, Part: 1,
		Objects: 2, Of: film.ChunkCount, missing: []int{0, 1},
	}
	// Called from the goroutines doing the objects, as documented.
	var dealt atomic.Int32
	out, err := v.restoreLostPart(context.Background(), row, func(int64) { dealt.Add(1) })
	if err != nil {
		t.Fatalf("restoreLostPart: %v", err)
	}
	if out.written != 0 || out.present != 2 || dealt.Load() != 2 {
		t.Errorf("outcome = %+v, dealt %d", out, dealt.Load())
	}
	if after := partsOnDisk(t, roots); !reflect.DeepEqual(after, before) {
		t.Error("an object that was there was written over")
	}
}

// A file erased, or moved to other clouds, between the scan and the write is
// one whose part must not be put back: no record would point at it.
func TestRestoreLeavesAloneAFileThatHasMovedOn(t *testing.T) {
	v, roots := chunkedVault(t, 4, 4096)
	ctx := context.Background()
	gone := upload(t, v, "gone.bin", noise(6_000, 10))
	moved := upload(t, v, "moved.bin", noise(6_000, 11))

	// Two rows as a scan would have made them, one object lost from each.
	rowFor := func(e *Entry) *LostPart {
		s := shardOf(t, e, 1)
		return &LostPart{
			ProviderID: s.ProviderID, ProviderName: s.ProviderName, Path: e.Path(),
			FileID: e.ID, ArchiveID: e.ArchiveID, Part: 1,
			Objects: 1, Of: e.ChunkCount, missing: []int{0},
		}
	}
	rows := []*LostPart{rowFor(gone), rowFor(moved)}
	if err := os.Remove(shardFile(t, roots, gone, 0, 1)); err != nil {
		t.Fatal(err)
	}

	if _, err := v.Delete(ctx, gone.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Onto the accounts that are not the one part 1 is on, so its record
	// there goes — and then the object goes too, as if it had been lost.
	var elsewhere []string
	for _, cfg := range mustProviders(t, v) {
		if cfg.ID != rows[1].ProviderID {
			elsewhere = append(elsewhere, cfg.ID)
		}
	}
	report, err := v.Relocate(ctx, MainScope, moved.ID, elsewhere, moved.Scheme(), nil)
	if err != nil || report.Relocated != 1 {
		t.Fatalf("Relocate = %+v, %v", report, err)
	}
	before := partsOnDisk(t, roots)

	for _, row := range rows {
		out, err := v.restoreLostPart(ctx, row, func(int64) {})
		if !errors.Is(err, errNoLongerRecorded) || out.written != 0 {
			t.Errorf("%s: outcome %+v, err %v", row.Path, out, err)
		}
	}
	if after := partsOnDisk(t, roots); !reflect.DeepEqual(after, before) {
		t.Error("a part was put back for a file that had moved on")
	}
	if again := lostScan(t, v); again.Found {
		t.Errorf("scan after = %+v", again.Items)
	}
}

func mustProviders(t *testing.T, v *Vault) []provider.Config {
	t.Helper()
	configs, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	return configs
}

// An account that will not answer is named and not judged: nothing on it is
// reported lost, and a part that could only be remade through it is withheld
// with that as the reason.
func TestAnAccountThatWillNotAnswerIsNotJudged(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	film := upload(t, v, "film.bin", noise(10_000, 12))

	dark := shardOf(t, film, 3).ProviderID
	if err := os.RemoveAll(rootOfAccount(t, v, dark)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shardFile(t, roots, film, 0, 1)); err != nil {
		t.Fatal(err)
	}

	scan := lostScan(t, v)
	if scan.Parts != 1 || scan.Restorable != 0 {
		t.Fatalf("scan = %+v, want one part, withheld", scan.LostSummary)
	}
	row := scan.Items[0]
	if row.Part != 1 || !strings.Contains(row.Reason, "could not be listed") {
		t.Errorf("row = %+v", row)
	}
	if len(scan.Warnings) != 1 {
		t.Errorf("warnings = %q", scan.Warnings)
	}
	for _, a := range scan.Accounts {
		if a.ProviderID == dark && (a.Error == "" || a.Lost != 0 || a.Emptied) {
			t.Errorf("the dark account = %+v", a)
		}
	}
}

// A file from before chunking has no per-chunk shard to remake. It is found
// and said, with the way out, rather than left off the list.
func TestALostPartOfAFileStoredWholeIsReportedAndWithheld(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	entry := addWholeEntry(t, v, "legacy-old", "old.bin", noise(3_000, 13))

	s := shardOf(t, entry, 2)
	if err := os.Remove(filepath.Join(rootOfAccount(t, v, s.ProviderID), s.Key)); err != nil {
		t.Fatal(err)
	}

	scan := lostScan(t, v)
	if scan.Parts != 1 || scan.Restorable != 0 {
		t.Fatalf("scan = %+v", scan.LostSummary)
	}
	if row := scan.Items[0]; row.Of != 1 || !strings.Contains(row.Reason, "before chunking") {
		t.Errorf("row = %+v", row)
	}
	report := restoreLost(t, v, LostRestoreOptions{})
	if report.Withheld != 1 || report.Objects != 0 {
		t.Errorf("restore = %+v", report)
	}
}

// An open sub vault's files are scanned and mended with its own key, and
// named by nothing but their archive. A locked one's are not judged at all.
func TestLostPartsReachIntoAnOpenSubVaultAndNotALockedOne(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	ctx := context.Background()
	sub := createSubVault(t, v, "papers", subPassword)
	payload := noise(9_000, 14)
	entry, _, err := v.Upload(ctx, Scope(sub), "/", "deed.pdf", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()
	entry = copyEntry(entry)

	if err := os.Remove(shardFile(t, roots, entry, 1, 2)); err != nil {
		t.Fatal(err)
	}

	if err := v.LockSubVault(sub); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}
	if scan := lostScan(t, v); scan.Found {
		t.Fatalf("a locked sub vault's file was judged: %+v", scan.Items)
	}

	if err := v.UnlockSubVault(sub, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	scan := lostScan(t, v)
	if scan.Parts != 1 || scan.Restorable != 1 {
		t.Fatalf("scan = %+v", scan.LostSummary)
	}
	if row := scan.Items[0]; row.Scope != Scope(sub) || row.Path != "" {
		t.Errorf("row = %+v: a sub vault's file is named by its archive alone", row)
	}

	report := restoreLost(t, v, LostRestoreOptions{})
	if report.Parts != 1 || report.Objects != 1 {
		t.Fatalf("restore = %+v", report)
	}
	emptyFolder(t, rootOfAccount(t, v, shardOf(t, entry, 1).ProviderID))
	mustFetch(t, v, entry.ID, payload)
}

// The stray-parts scan lists the same accounts, so it carries the finding in
// figures — which is how the browser comes to offer the repair unasked.
func TestTheStrayPartsScanCarriesWhatIsLost(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	film := upload(t, v, "film.bin", noise(10_000, 15))

	if scan := orphanScan(t, v); scan.Lost != nil {
		t.Fatalf("a whole vault's stray scan carries %+v", scan.Lost)
	}

	if err := os.Remove(shardFile(t, roots, film, 0, 3)); err != nil {
		t.Fatal(err)
	}
	scan := orphanScan(t, v)
	if scan.Lost == nil || scan.Lost.Parts != 1 || scan.Lost.Restorable != 1 || scan.Lost.Files != 1 {
		t.Fatalf("stray scan's lost = %+v", scan.Lost)
	}
	if len(scan.Lost.On) != 1 || scan.Lost.On[0] != shardOf(t, film, 3).ProviderName {
		t.Errorf("lost on %q", scan.Lost.On)
	}
	if scan.Found || len(scan.Strays) != 0 {
		t.Errorf("a lost part read as a stray or an orphan: %+v", scan)
	}
}

func TestLostPartsNeedAnUnlockedVault(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	v.Lock()
	if _, err := v.ScanForLostParts(context.Background(), LostScanOptions{}); !errors.Is(err, ErrLocked) {
		t.Errorf("scan of a locked vault: %v", err)
	}
	if _, err := v.RestoreLostParts(context.Background(), LostRestoreOptions{}); !errors.Is(err, ErrLocked) {
		t.Errorf("restore on a locked vault: %v", err)
	}
}

// The judgement on its own, where the listings can be whatever the case
// needs: a part recorded twice, and an account that was never listed.
func TestJudgeLostParts(t *testing.T) {
	const arch = "0123456789abcdef0123456789abcdef"
	entry := func(shards ...Shard) *Entry {
		return &Entry{
			ID: "f1", Dir: "/", Name: "a.bin", ArchiveID: arch,
			ChunkSize: 4096, ChunkCount: 2, DataShards: 2, TotalShards: 3, Shards: shards,
		}
	}
	rec := func(part int, account string) Shard {
		return Shard{Part: part, ProviderID: account, ProviderName: account, Key: ChunkShardKey(arch, 0, part), Size: 200}
	}
	holding := func(pairs ...[2]int) heldKeys {
		keys := heldKeys{}
		for _, p := range pairs {
			keys[ChunkShardKey(arch, p[0], p[1])] = struct{}{}
		}
		return keys
	}
	accountsFor := func(ids ...string) map[string]*LostAccount {
		out := map[string]*LostAccount{}
		for _, id := range ids {
			out[id] = &LostAccount{ProviderID: id, Name: id}
		}
		return out
	}

	t.Run("a part on an account that was not listed is not lost", func(t *testing.T) {
		files := []lostFile{{entry: entry(rec(1, "a"), rec(2, "b"), rec(3, "c"))}}
		held := map[string]heldKeys{
			"a": holding([2]int{0, 1}, [2]int{1, 1}),
			"b": holding([2]int{0, 2}, [2]int{1, 2}),
		}
		if lost := judgeLostParts(files, held, accountsFor("a", "b")); len(lost) != 0 {
			t.Errorf("lost = %+v", lost)
		}
	})

	t.Run("each chunk is judged on its own", func(t *testing.T) {
		files := []lostFile{{entry: entry(rec(1, "a"), rec(2, "b"), rec(3, "c"))}}
		accounts := accountsFor("a", "b", "c")
		lost := judgeLostParts(files, map[string]heldKeys{
			"a": holding([2]int{1, 1}),               // chunk 0 gone
			"b": holding([2]int{0, 2}),               // chunk 1 gone
			"c": holding([2]int{0, 3}, [2]int{1, 3}), // whole
		}, accounts)
		if len(lost) != 2 {
			t.Fatalf("lost = %+v", lost)
		}
		for _, row := range lost {
			if !row.Restorable || row.Objects != 1 || row.Of != 2 || row.Bytes != 100 {
				t.Errorf("row = %+v", row)
			}
		}
		if accounts["a"].Recorded != 2 || accounts["c"].Recorded != 2 {
			t.Errorf("recorded = %d, %d", accounts["a"].Recorded, accounts["c"].Recorded)
		}
	})

	t.Run("a second copy of the same part is not a second part", func(t *testing.T) {
		// Part 1 on two accounts, part 2 lost, part 3 never written: the two
		// copies of part 1 are one shard, and one shard does not remake.
		files := []lostFile{{entry: entry(rec(1, "a"), rec(1, "c"), rec(2, "b"))}}
		lost := judgeLostParts(files, map[string]heldKeys{
			"a": holding([2]int{0, 1}, [2]int{1, 1}),
			"c": holding([2]int{0, 1}, [2]int{1, 1}),
			"b": holding(),
		}, accountsFor("a", "b", "c"))
		if len(lost) != 1 || lost[0].Restorable {
			t.Fatalf("lost = %+v", lost)
		}
	})
}
