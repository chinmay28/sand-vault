package vault

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// replicatedFile is one file uploaded onto the first three of a replica
// vault's accounts and replicated, with the parts and the replica both whole.
func replicatedFile(t *testing.T, v *Vault, accounts []string, path string, payload []byte) *Entry {
	t.Helper()
	dir, name := splitEntryPath(path)
	entry, _, err := v.Upload(context.Background(), MainScope, dir, name, payload, UploadOptions{Accounts: accounts})
	if err != nil {
		t.Fatalf("Upload %s: %v", path, err)
	}
	// A copy, because the row Upload hands back is the live one and a rebuild
	// rewrites it in place — the tests below want the file as it was.
	return copyEntry(entry)
}

func splitEntryPath(path string) (dir, name string) {
	dir = CleanDir(path)
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			return CleanDir(dir[:i]), dir[i+1:]
		}
	}
	return "/", dir
}

func replicaOf(t *testing.T, v *Vault, id string) *Replica {
	t.Helper()
	v.mu.RLock()
	defer v.mu.RUnlock()
	e := v.manifest.ByID(id)
	if e == nil {
		t.Fatalf("no entry %s", id)
	}
	return e.Replica
}

func TestRebuildFromReplicaPutsTheFileBackOnChosenClouds(t *testing.T) {
	// Four accounts, the replica policy over the first three. The file's
	// parts go on the first three too; then two of those parts folders are
	// emptied, which is a file the parts cannot rebuild and the replica can.
	v, roots, replicaRoots := replicaVault(t, 4, 3, archive.Scheme{Data: 2, Total: 3})
	ids := providerIDs(t, v)
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("a file worth keeping twice "), 400)
	entry := replicatedFile(t, v, ids[:3], "/keep.bin", payload)
	replicate(t, v)
	before := replicaOf(t, v, entry.ID)
	if before == nil {
		t.Fatal("the file was not replicated")
	}

	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])
	v.chunks.clear()
	if _, _, err := v.Fetch(context.Background(), entry.ID); err == nil {
		t.Fatal("the parts still read with two of three accounts emptied")
	}

	// Onto the three accounts that are not the two that lost their parts —
	// one of which is the account its third part is on.
	targets := []string{ids[2], ids[3], ids[0]}
	plan, err := v.PlanRelocationFrom(MainScope, "/keep.bin", targets, archive.Scheme{}, ReadReplica)
	if err != nil {
		t.Fatalf("PlanRelocationFrom: %v", err)
	}
	if plan.Source != ReadReplica {
		t.Errorf("plan.Source = %q, want replica", plan.Source)
	}
	if plan.Recoded != 1 || plan.NoReplica != 0 || plan.Unchanged != 0 || plan.Moves != 0 {
		t.Fatalf("plan = recoded %d, no replica %d, unchanged %d, moves %d; want one rebuild",
			plan.Recoded, plan.NoReplica, plan.Unchanged, plan.Moves)
	}
	row := plan.Files[0]
	if !row.Recode || !row.FromReplica || row.NoReplica {
		t.Fatalf("row = %+v, want a rebuild from the replica", row)
	}
	if row.Bytes <= entry.Size {
		t.Errorf("a rebuild is priced at %d bytes for a %d byte file; it comes down and goes back up", row.Bytes, entry.Size)
	}
	if plan.RecodeBytes != row.Bytes {
		t.Errorf("plan.RecodeBytes = %d, want the row's %d", plan.RecodeBytes, row.Bytes)
	}

	report, err := v.RelocateFrom(context.Background(), MainScope, "/keep.bin", targets, archive.Scheme{}, ReadReplica, nil)
	if err != nil {
		t.Fatalf("RelocateFrom: %v", err)
	}
	if !report.Done() || report.Recoded != 1 || report.Relocated != 1 || report.Source != ReadReplica {
		t.Fatalf("report = %+v", report)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ArchiveID == entry.ArchiveID {
		t.Error("the parts keep the archive ID they had; a rebuild writes a fresh set")
	}
	if after.Redundancy() != after.Scheme().Total || after.Scheme() != (archive.Scheme{Data: 2, Total: 3}) {
		t.Errorf("rebuilt as %s with %d parts, want a full 2-of-3", after.Scheme(), after.Redundancy())
	}
	wanted := map[string]bool{}
	for _, id := range targets {
		wanted[id] = true
	}
	for _, s := range after.Shards {
		if !wanted[s.ProviderID] {
			t.Errorf("part %d landed on %s, which was not chosen", s.Part, s.ProviderName)
		}
	}
	if after.Hash != entry.Hash {
		t.Errorf("hash changed from %s to %s", entry.Hash, after.Hash)
	}

	// The replica is not touched by any of it.
	if got := replicaOf(t, v, entry.ID); got == nil || got.ArchiveID != before.ArchiveID {
		t.Errorf("the replica changed under the rebuild: %+v → %+v", before, got)
	}
	for _, root := range replicaRoots[:3] {
		if sandObjectsIn(t, root) != before.ChunkCount {
			t.Errorf("%s holds %d replica objects after the rebuild, want %d", root, sandObjectsIn(t, root), before.ChunkCount)
		}
	}

	// And the file reads back from its parts again — with the chunk cache
	// cleared, so it is the clouds answering and not what was read before.
	v.chunks.clear()
	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the rebuild: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("the rebuilt file reads back as %d bytes, want %d", len(data), len(payload))
	}

	// The old part on the account that kept it is gone: the index stopped
	// naming it, and the rebuild erased it. The account holds the new part
	// and nothing else.
	if got := objectsIn(t, roots[2]); got != after.ChunkCount {
		t.Errorf("%s holds %d objects, want the %d of the new part only", roots[2], got, after.ChunkCount)
	}

	// Running the same rebuild again is not a no-op: the replica was chosen
	// over the parts, and the parts it writes are new every time.
	plan, err = v.PlanRelocationFrom(MainScope, entry.ID, targets, archive.Scheme{}, ReadReplica)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Recoded != 1 {
		t.Errorf("a second plan rebuilds %d files, want 1 — a replica rebuild does not ask whether the parts look whole", plan.Recoded)
	}
}

func TestRebuildFromReplicaRebuildsAFolderAndLeavesFilesWithoutOne(t *testing.T) {
	v, roots, _ := replicaVault(t, 4, 3, archive.Scheme{Data: 2, Total: 3})
	ids := providerIDs(t, v)
	if err := v.Mkdir(MainScope, "/photos"); err != nil {
		t.Fatal(err)
	}
	// The folder is replicated, one file in it is kept out.
	if err := v.SetReplicate("/photos", on(true)); err != nil {
		t.Fatal(err)
	}
	a := replicatedFile(t, v, ids[:3], "/photos/a.bin", bytes.Repeat([]byte("alpha "), 500))
	b := replicatedFile(t, v, ids[:3], "/photos/b.bin", bytes.Repeat([]byte("bravo "), 300))
	c := replicatedFile(t, v, ids[:3], "/photos/c.bin", bytes.Repeat([]byte("charlie "), 200))
	if err := v.SetReplicate("/photos/c.bin", on(false)); err != nil {
		t.Fatal(err)
	}
	if made := replicate(t, v); made != 2 {
		t.Fatalf("replicated %d files, want 2", made)
	}

	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])
	v.chunks.clear()

	targets := []string{ids[2], ids[3], ids[1]}
	plan, err := v.PlanRelocationFrom(MainScope, "/photos", targets, archive.Scheme{}, ReadReplica)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Folder || plan.Total != 3 || plan.Recoded != 2 || plan.NoReplica != 1 || plan.Unchanged != 0 {
		t.Fatalf("plan = folder %v, total %d, recoded %d, no replica %d, unchanged %d",
			plan.Folder, plan.Total, plan.Recoded, plan.NoReplica, plan.Unchanged)
	}
	rows := map[string]*FilePlan{}
	for i := range plan.Files {
		rows[plan.Files[i].Path] = &plan.Files[i]
	}
	if row := rows["/photos/c.bin"]; row == nil || !row.NoReplica || row.Changed() {
		t.Errorf("c.bin row = %+v, want left alone for want of a replica", row)
	}
	saidSo := false
	for _, w := range plan.Warnings {
		if bytes.Contains([]byte(w), []byte("/photos/c.bin has no replica")) {
			saidSo = true
		}
	}
	if !saidSo {
		t.Errorf("the file left alone is not named in the warnings: %v", plan.Warnings)
	}

	var seen []string
	report, err := v.RelocateFrom(context.Background(), MainScope, "/photos", targets, archive.Scheme{}, ReadReplica,
		func(path string, done, total int) { seen = append(seen, path) })
	if err != nil {
		t.Fatalf("RelocateFrom: %v", err)
	}
	if !report.Done() || report.Recoded != 2 || report.NoReplica != 1 || report.Total != 3 {
		t.Fatalf("report = %+v", report)
	}
	if len(seen) != 3 {
		t.Errorf("progress reported %d files, want 3: %v", len(seen), seen)
	}

	v.chunks.clear()
	for _, e := range []*Entry{a, b} {
		data, _, err := v.Fetch(context.Background(), e.ID)
		if err != nil {
			t.Errorf("%s after the rebuild: %v", e.Path(), err)
			continue
		}
		if len(data) != int(e.Size) {
			t.Errorf("%s reads back as %d bytes, want %d", e.Path(), len(data), e.Size)
		}
	}
	// c.bin was never touched: its parts are still the two that were
	// emptied plus one, and it still does not read.
	after, err := v.Entry(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ArchiveID != c.ArchiveID {
		t.Errorf("c.bin was rewritten without a replica to rewrite it from")
	}
	if _, _, err := v.Fetch(context.Background(), c.ID); err == nil {
		t.Errorf("c.bin reads back, which it should not — nothing put it back")
	}
}

func TestRebuildFromReplicaFailsCleanlyWhenTheReplicaCannotBeRead(t *testing.T) {
	v, roots, replicaRoots := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	ids := providerIDs(t, v)
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := replicatedFile(t, v, ids, "/gone.bin", bytes.Repeat([]byte("gone "), 400))
	replicate(t, v)

	// Both copies lose two of three accounts; nothing can be read.
	emptyCloud(t, roots[0])
	emptyCloud(t, replicaRoots[0])
	emptyCloud(t, replicaRoots[1])
	v.chunks.clear()

	report, err := v.RelocateFrom(context.Background(), MainScope, entry.ID, ids, archive.Scheme{}, ReadReplica, nil)
	if err != nil {
		t.Fatalf("RelocateFrom: %v", err)
	}
	if report.Failed != 1 || report.Recoded != 0 || report.Done() {
		t.Fatalf("report = %+v, want the one file failed and nothing rebuilt", report)
	}
	if len(report.Warnings) == 0 {
		t.Error("a failed rebuild says nothing about why")
	}

	// The file is exactly as it was: same archive ID, same parts recorded,
	// the one part still on disk still there.
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ArchiveID != entry.ArchiveID || len(after.Shards) != len(entry.Shards) {
		t.Errorf("a failed rebuild changed the file: %+v → %+v", entry, after)
	}
	for i := 1; i < 3; i++ {
		if got := objectsIn(t, roots[i]); got != entry.ChunkCount {
			t.Errorf("%s holds %d objects after the failed rebuild, want its %d untouched", roots[i], got, entry.ChunkCount)
		}
	}
}

func TestRebuildFromReplicaRefusesWhatItCannotMean(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	ids := providerIDs(t, v)
	entry := replicatedFile(t, v, ids, "/plain.bin", []byte("never replicated"))

	// "either" is a way to read, not a copy to trust.
	if _, err := v.PlanRelocationFrom(MainScope, entry.ID, ids, archive.Scheme{}, ReadEither); err == nil {
		t.Error("planning a rebuild from either copy was allowed")
	}
	if _, err := v.RelocateFrom(context.Background(), MainScope, entry.ID, ids, archive.Scheme{}, ReadEither, nil); err == nil {
		t.Error("a rebuild from either copy was allowed")
	}
	// Empty is the parts, exactly as it is for a read.
	plan, err := v.PlanRelocationFrom(MainScope, entry.ID, ids, archive.Scheme{}, "")
	if err != nil {
		t.Fatalf("an empty source: %v", err)
	}
	if plan.Source != "" || plan.Recoded != 0 || plan.Unchanged != 1 {
		t.Errorf("an empty source planned %+v, want a move that finds the file in place", plan)
	}

	// A file with no replica is skipped by the plan, and the rebuild itself
	// says why when asked for it directly.
	plan, err = v.PlanRelocationFrom(MainScope, entry.ID, ids, archive.Scheme{}, ReadReplica)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NoReplica != 1 || plan.Recoded != 0 {
		t.Errorf("plan = %+v, want the file counted as having no replica", plan)
	}
	if _, _, _, err := v.migrateFileFrom(context.Background(), MainScope, entry.ID, ids, archive.Scheme{}, ReadReplica, nil); !errors.Is(err, ErrNoReplica) {
		t.Errorf("rebuilding a file with no replica: %v, want ErrNoReplica", err)
	}
	if _, _, _, err := v.migrateFileFrom(context.Background(), MainScope, entry.ID, ids, archive.Scheme{}, ReadEither, nil); err == nil {
		t.Error("migrating from either was allowed")
	}
}

func TestRebuildFromReplicaWaitsForTheReEncryptionSlot(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	ids := providerIDs(t, v)
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := replicatedFile(t, v, ids, "/busy.bin", bytes.Repeat([]byte("busy "), 300))
	replicate(t, v)

	v.migrateMu.Lock()
	report, err := v.RelocateFrom(context.Background(), MainScope, entry.ID, ids, archive.Scheme{}, ReadReplica, nil)
	v.migrateMu.Unlock()
	if err != nil {
		t.Fatalf("RelocateFrom: %v", err)
	}
	if report.Deferred != 1 || report.Recoded != 0 || report.Failed != 0 {
		t.Errorf("report = %+v, want the file deferred", report)
	}
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ArchiveID != entry.ArchiveID {
		t.Error("a deferred rebuild rewrote the file")
	}
}
