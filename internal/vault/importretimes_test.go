package vault

import (
	"testing"
	"time"
)

// The batch holds its corrections back until there are enough of them to be
// worth an index write. That is the whole reason it exists — the index is
// sealed and written whole, so a write per file made a re-import of a folder
// already in the vault slower than the import that fetched it.
func TestRetimeBatchHoldsUntilItIsWorthAWrite(t *testing.T) {
	v, _ := newTestVault(t, 3)
	entry := put(t, v, "/photos", "hike.jpg", UploadOptions{})
	was := entry.ModifiedAt
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	batch := importRetimes{v: v, scope: MainScope}
	batch.add(FileTime{Path: "/photos/hike.jpg", Mod: taken},
		ImportResult{Path: "hike.jpg", Dest: "/photos/hike.jpg", Retimed: true})

	if failed := batch.flushIfFull(); failed != nil {
		t.Fatalf("a batch of one wrote something: %+v", failed)
	}
	if !entry.ModifiedAt.Equal(was) {
		t.Errorf("the time was put back before the batch was written")
	}

	if failed := batch.flush(); failed != nil {
		t.Fatalf("flush: %+v", failed)
	}
	if !SameModTime(entry.ModifiedAt, taken) {
		t.Errorf("the entry says %s, want the source's %s", entry.ModifiedAt, taken)
	}
	if len(batch.want) != 0 || len(batch.held) != 0 {
		t.Errorf("the batch still holds %d files after being written", len(batch.want))
	}
}

// Full is full: the write happens without waiting for the end of the import,
// which is what bounds how many corrections a kill in the middle throws away.
func TestRetimeBatchWritesWhenFull(t *testing.T) {
	v, _ := newTestVault(t, 3)
	entry := put(t, v, "/photos", "hike.jpg", UploadOptions{})
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	batch := importRetimes{v: v, scope: MainScope}
	batch.add(FileTime{Path: "/photos/hike.jpg", Mod: taken},
		ImportResult{Path: "hike.jpg", Dest: "/photos/hike.jpg", Retimed: true})
	// The rest name nothing, which Retime ignores as it ignores any path it
	// does not hold — they are here to fill the batch, not to be corrected.
	for i := 1; i < retimeBatchSize; i++ {
		batch.add(FileTime{Path: "/photos/gone.jpg", Mod: taken}, ImportResult{})
	}

	if failed := batch.flushIfFull(); failed != nil {
		t.Fatalf("a full batch failed: %+v", failed)
	}
	if len(batch.want) != 0 {
		t.Errorf("a full batch was not emptied by the write: %d left", len(batch.want))
	}
	if !SameModTime(entry.ModifiedAt, taken) {
		t.Errorf("a full batch was not written: the entry says %s", entry.ModifiedAt)
	}
	if failed := batch.flush(); failed != nil {
		t.Fatalf("flushing an empty batch wrote something: %+v", failed)
	}
}

// A write that fails is not a set of files that were retimed. Retime puts the
// entries back as they were, so the batch hands its files back with the error
// on them — a summary that counted the correction anyway would be claiming a
// time that is not in the index.
func TestRetimeBatchHandsBackWhatItCouldNotWrite(t *testing.T) {
	v, _ := newTestVault(t, 3)
	put(t, v, "/photos", "hike.jpg", UploadOptions{})

	batch := importRetimes{v: v, scope: MainScope}
	batch.add(FileTime{Path: "/photos/hike.jpg", Mod: time.Now()},
		ImportResult{Path: "hike.jpg", Dest: "/photos/hike.jpg", Retimed: true, Reason: "already imported"})

	// Nothing can be written to a locked vault: the key the index is sealed
	// under is gone.
	v.Lock()

	failed := batch.flush()
	if len(failed) != 1 {
		t.Fatalf("a failed write handed back %d files, want the one it held", len(failed))
	}
	if failed[0].Error == "" {
		t.Error("the file came back without the error that stopped it")
	}
	if failed[0].Retimed {
		t.Error("a file whose time was not written still says it was retimed")
	}
	if failed[0].Dest != "/photos/hike.jpg" {
		t.Errorf("the file came back as %q, want the one that was held", failed[0].Dest)
	}
	if len(batch.want) != 0 {
		t.Error("a batch that could not be written held on to it")
	}
}
