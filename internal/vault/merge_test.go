package vault

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The merge is a pure function over two indexes, so it is tested as one: no
// clouds, no vault file, just the rules at the top of merge.go.

var (
	t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

// row builds a row at a path with a stamp.
func row(id, dir, name string, at time.Time) *Entry {
	return &Entry{ID: id, Dir: dir, Name: name, Size: 1, CreatedAt: at, IndexedAt: at,
		Shards: []Shard{{Part: 1, ProviderID: "a"}, {Part: 2, ProviderID: "b"}, {Part: 3, ProviderID: "c"}}}
}

// index builds a manifest with a clock and rows.
func index(clock map[string]uint64, rows ...*Entry) *Manifest {
	m := newManifest()
	m.Clock = clock
	m.Entries = append(m.Entries, rows...)
	return m
}

func ids(m *Manifest) []string {
	out := []string{}
	for _, e := range m.Entries {
		out = append(out, e.ID)
	}
	sort.Strings(out)
	return out
}

func TestCompareClocks(t *testing.T) {
	cases := []struct {
		a, b map[string]uint64
		want clockOrder
	}{
		{nil, nil, clocksEqual},
		{map[string]uint64{"pc": 3}, map[string]uint64{"pc": 3}, clocksEqual},
		{map[string]uint64{"pc": 4}, map[string]uint64{"pc": 3}, clockAfter},
		{map[string]uint64{"pc": 3}, map[string]uint64{"pc": 3, "pi": 1}, clockBefore},
		{map[string]uint64{"pc": 4}, map[string]uint64{"pc": 3, "pi": 1}, clocksConcurrent},
		{map[string]uint64{"pc": 0}, nil, clocksEqual},
	}
	for _, c := range cases {
		if got := compareClocks(c.a, c.b); got != c.want {
			t.Errorf("compareClocks(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A copy that is simply a later version of this one is taken whole, and one
// that is an earlier version is left alone.
func TestMergeTakesALaterVersionWholeAndLeavesAnEarlierOne(t *testing.T) {
	older := index(map[string]uint64{"pc": 1}, row("x", "/", "x.txt", t0))
	newer := index(map[string]uint64{"pc": 1, "pi": 2}, row("x", "/", "x.txt", t0), row("y", "/", "y.txt", t1))

	merged, rep := mergeIndexes(older, newer)
	if rep.Outcome != MergeAdopted || rep.Added != 1 || rep.Removed != 0 {
		t.Fatalf("older ← newer: %+v", rep)
	}
	if got := ids(merged); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("adopted rows %v, want [x y]", got)
	}

	merged, rep = mergeIndexes(newer, older)
	if rep.Outcome != MergeAhead || rep.Changed() {
		t.Fatalf("newer ← older: %+v", rep)
	}
	if merged != newer {
		t.Error("an earlier copy should leave this machine's index untouched")
	}

	_, rep = mergeIndexes(newer, newer)
	if rep.Outcome != MergeCurrent || rep.Changed() {
		t.Errorf("the same version twice: %+v", rep)
	}
}

// Two machines that each added a file end up with both — and both machines
// compute the same result, whichever is "local".
func TestMergeOfDivergedIndexesKeepsBothSidesAdditions(t *testing.T) {
	base := row("shared", "/", "shared.txt", t0)
	pc := index(map[string]uint64{"pc": 2, "pi": 1}, base, row("from-pc", "/", "pc.txt", t1))
	pi := index(map[string]uint64{"pc": 1, "pi": 2}, base, row("from-pi", "/", "pi.txt", t1))

	onPC, rep := mergeIndexes(pc, pi)
	if rep.Outcome != MergeMerged || rep.Added != 1 || rep.Removed != 0 || rep.Updated != 0 {
		t.Fatalf("pc ← pi: %+v", rep)
	}
	onPi, _ := mergeIndexes(pi, pc)
	want := []string{"from-pc", "from-pi", "shared"}
	if got := ids(onPC); !reflect.DeepEqual(got, want) {
		t.Errorf("on the pc: %v, want %v", got, want)
	}
	if got := ids(onPi); !reflect.DeepEqual(got, want) {
		t.Errorf("on the pi: %v, want %v", got, want)
	}
	if !reflect.DeepEqual(onPC.Clock, map[string]uint64{"pc": 2, "pi": 2}) {
		t.Errorf("merged clock %v, want the max of both", onPC.Clock)
	}
}

// A file deleted on one machine stays deleted when the other still lists it,
// and a file added on one machine is not mistaken for one the other deleted.
func TestMergeHonoursTombstones(t *testing.T) {
	gone := row("gone", "/", "gone.txt", t0)
	kept := row("kept", "/", "kept.txt", t0)

	pc := index(map[string]uint64{"pc": 2, "pi": 1}, kept, row("new", "/", "new.txt", t1))
	pc.Tombstones = map[string]time.Time{"gone": t1}
	pi := index(map[string]uint64{"pc": 1, "pi": 2}, kept, gone)
	// The pi renamed the file the pc deleted, after the delete: the delete
	// still wins, because the parts are gone.
	gone.Name, gone.IndexedAt = "renamed.txt", t2

	for name, pair := range map[string][2]*Manifest{"pc": {pc, pi}, "pi": {pi, pc}} {
		merged, _ := mergeIndexes(pair[0], pair[1])
		if got := ids(merged); !reflect.DeepEqual(got, []string{"kept", "new"}) {
			t.Errorf("on the %s: %v, want [kept new]", name, got)
		}
		if _, still := merged.Tombstones["gone"]; !still {
			t.Errorf("on the %s: the tombstone was dropped, so the file would come back on the next fold", name)
		}
	}
}

// The same row changed on both machines: the later write stands.
func TestMergeOfOneRowChangedOnBothSidesTakesTheLaterWrite(t *testing.T) {
	onPC := row("f", "/", "pc-name.txt", t1)
	onPi := row("f", "/", "pi-name.txt", t2)
	pc := index(map[string]uint64{"pc": 2, "pi": 1}, onPC)
	pi := index(map[string]uint64{"pc": 1, "pi": 2}, onPi)

	merged, rep := mergeIndexes(pc, pi)
	if rep.Updated != 1 || merged.Entries[0].Name != "pi-name.txt" {
		t.Errorf("the pc took %q (%+v), want the pi's later write", merged.Entries[0].Name, rep)
	}
	merged, rep = mergeIndexes(pi, pc)
	if rep.Updated != 0 || merged.Entries[0].Name != "pi-name.txt" {
		t.Errorf("the pi took %q (%+v), want its own later write", merged.Entries[0].Name, rep)
	}
}

// A row from before stamps existed merges as if stamped when it was created.
func TestMergeFallsBackToCreatedAtForAnUnstampedRow(t *testing.T) {
	old := row("f", "/", "old.txt", t0)
	old.IndexedAt = time.Time{}
	fresh := row("f", "/", "new.txt", t1)
	pc := index(map[string]uint64{"pc": 2, "pi": 1}, old)
	pi := index(map[string]uint64{"pc": 1, "pi": 2}, fresh)
	merged, _ := mergeIndexes(pc, pi)
	if merged.Entries[0].Name != "new.txt" {
		t.Errorf("took %q, want the stamped, later row", merged.Entries[0].Name)
	}
}

// Both machines uploaded a "notes.txt" to the same folder: both survive, and
// the later one is renamed the way an upload beside an existing file is. The
// same one is renamed on both machines.
func TestMergeRenamesTheLaterOfTwoFilesAtOnePath(t *testing.T) {
	first := row("first", "/docs", "notes.txt", t1)
	second := row("second", "/docs", "notes.txt", t2)
	pc := index(map[string]uint64{"pc": 2, "pi": 1}, first)
	pi := index(map[string]uint64{"pc": 1, "pi": 2}, second)

	for name, pair := range map[string][2]*Manifest{"pc": {pc, pi}, "pi": {pi, pc}} {
		merged, rep := mergeIndexes(pair[0], pair[1])
		byID := map[string]string{}
		for _, e := range merged.Entries {
			byID[e.ID] = e.Name
		}
		if byID["first"] != "notes.txt" || byID["second"] != "notes (2).txt" {
			t.Errorf("on the %s: %v, want the later file renamed to notes (2).txt", name, byID)
		}
		if len(rep.Renamed) != 1 || !strings.Contains(rep.Renamed[0], "notes (2).txt") {
			t.Errorf("on the %s: renames reported as %v", name, rep.Renamed)
		}
	}
	// The rename did not reach into the other side's index.
	if second.Name != "notes.txt" {
		t.Error("the merge renamed the remote's own row in place")
	}
}

// Folders made by hand merge by union, less what the other side deleted.
func TestMergeFoldersUnionLessDeletions(t *testing.T) {
	pc := index(map[string]uint64{"pc": 2, "pi": 1})
	pc.Folders = []string{"/a", "/both", "/deleted-on-pi"}
	pi := index(map[string]uint64{"pc": 1, "pi": 2})
	pi.Folders = []string{"/b", "/both"}
	pi.FolderTombstones = map[string]time.Time{"/deleted-on-pi": t1}

	merged, _ := mergeIndexes(pc, pi)
	if want := []string{"/a", "/b", "/both"}; !reflect.DeepEqual(merged.Folders, want) {
		t.Errorf("folders %v, want %v", merged.Folders, want)
	}
}

// The register of machines is unioned whatever the clocks say — including
// when this machine's copy is otherwise ahead.
func TestMergeAlwaysUnionsTheRegister(t *testing.T) {
	pc := index(map[string]uint64{"pc": 3})
	pc.Instances = map[string]*Instance{"pc": {Name: "pc", LastSeen: t2, Accounts: []string{"a"}}}
	pi := index(map[string]uint64{"pc": 1})
	pi.Instances = map[string]*Instance{"pi": {Name: "pi", LastSeen: t1, Accounts: []string{"b"}}}

	merged, rep := mergeIndexes(pc, pi)
	if rep.Outcome != MergeAhead || !rep.RegisterChanged || !rep.Changed() {
		t.Fatalf("report %+v", rep)
	}
	if merged.Instances["pi"] == nil || merged.Instances["pc"] == nil {
		t.Errorf("register %v, want both machines", merged.Instances)
	}
	if pc.Instances["pi"] != nil {
		t.Error("the fold wrote into this machine's own index rather than returning a copy")
	}

	// And a register already holding the other machine is not a change.
	_, rep = mergeIndexes(merged, pi)
	if rep.RegisterChanged {
		t.Error("a register that already lists the machine reported a change")
	}
}

// Per-file records follow their file; a thumbnail pack rewritten later wins.
func TestMergeCarriesRecordsWithTheirFiles(t *testing.T) {
	pc := index(map[string]uint64{"pc": 2, "pi": 1}, row("kept", "/", "k.txt", t0))
	pc.Tombstones = map[string]time.Time{"gone": t1}
	pc.Thumbs = map[string]*ThumbPack{"/": {IDs: []string{"kept"}, UpdatedAt: t0}}
	pi := index(map[string]uint64{"pc": 1, "pi": 2}, row("kept", "/", "k.txt", t0), row("gone", "/", "g.txt", t0))
	pi.Thumbs = map[string]*ThumbPack{"/": {IDs: []string{"kept", "gone"}, UpdatedAt: t1}}
	pi.FolderArt = map[string]string{"/a": "gone", "/b": "kept"}

	merged, _ := mergeIndexes(pc, pi)
	if got := merged.Thumbs["/"].IDs; !reflect.DeepEqual(got, []string{"kept", "gone"}) {
		t.Errorf("thumbs %v, want the later pack", got)
	}
	if !reflect.DeepEqual(merged.FolderArt, map[string]string{"/b": "kept"}) {
		t.Errorf("folder art %v, want only the record whose file survived", merged.FolderArt)
	}
}

// The stamp is set by the write, for exactly the rows the write changed.
func TestIndexWriteStampsTheRowsItChanged(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := t.Context()
	a, _, err := v.Upload(ctx, MainScope, "/", "a.txt", []byte("a"), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := v.Upload(ctx, MainScope, "/", "b.txt", []byte("b"), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.IndexedAt.IsZero() || b.IndexedAt.IsZero() {
		t.Fatal("new rows were not stamped")
	}
	// Upload hands back the index's own row, so the stamps are copied out.
	aBefore, bBefore := a.IndexedAt, b.IndexedAt

	time.Sleep(5 * time.Millisecond)
	if _, err := v.Move(ctx, b.ID, "/", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	a2, _ := v.Entry(a.ID)
	b2, _ := v.Entry(b.ID)
	if !a2.IndexedAt.Equal(aBefore) {
		t.Errorf("a row the write did not touch was restamped: %s → %s", aBefore, a2.IndexedAt)
	}
	if !b2.IndexedAt.After(bBefore) {
		t.Errorf("the renamed row was not restamped: %s then %s", bBefore, b2.IndexedAt)
	}

	v.mu.RLock()
	clock := v.manifest.Clock[v.instance]
	v.mu.RUnlock()
	if clock == 0 {
		t.Error("the write did not count itself in the clock")
	}
}

// A delete leaves a tombstone; the stamp of a row that comes back clears it.
func TestDeleteLeavesATombstoneAndReAddingClearsIt(t *testing.T) {
	v, _ := newTestVault(t, 3)
	e, _, err := v.Upload(t.Context(), MainScope, "/", "a.txt", []byte("a"), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Delete(t.Context(), e.ID); err != nil {
		t.Fatal(err)
	}
	v.mu.RLock()
	_, stoned := v.manifest.Tombstones[e.ID]
	v.mu.RUnlock()
	if !stoned {
		t.Fatal("deleting a file left no tombstone")
	}

	m := newManifest()
	m.remove("x")
	m.add(&Entry{ID: "x", Dir: "/", Name: "x"})
	if _, still := m.Tombstones["x"]; still {
		t.Error("a row added back is still marked deleted")
	}
	if err := m.Mkdir("/f"); err != nil {
		t.Fatal(err)
	}
	m.removeFolders("/f")
	if _, ok := m.FolderTombstones["/f"]; !ok {
		t.Error("removing a folder left no tombstone")
	}
	if err := m.Mkdir("/f"); err != nil {
		t.Fatal(err)
	}
	if _, still := m.FolderTombstones["/f"]; still {
		t.Error("a folder made again is still marked deleted")
	}
}

func TestTombstonesArePrunedAfterTheirLifetime(t *testing.T) {
	now := time.Now()
	stones := map[string]time.Time{
		"fresh": now.Add(-time.Hour),
		"stale": now.Add(-tombstoneLifetime - time.Hour),
	}
	pruneTombstones(stones, now)
	if _, ok := stones["fresh"]; !ok {
		t.Error("a fresh tombstone was pruned")
	}
	if _, ok := stones["stale"]; ok {
		t.Error("a tombstone past its lifetime was kept")
	}
}
