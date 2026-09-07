package vault

import (
	"context"
	"strings"
	"testing"
	"time"
)

// dated stores one file per given name and time, all in one folder, and returns
// the vault holding them.
func dated(t *testing.T, dir string, files map[string]time.Time) *Vault {
	t.Helper()

	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, dir); err != nil {
		t.Fatalf("Mkdir %s: %v", dir, err)
	}
	for name, when := range files {
		fileDated(t, v, dir, name, when)
	}
	return v
}

// fileDated puts one file in a folder with a modified time of its own, creating
// the folder if it is not there.
func fileDated(t *testing.T, v *Vault, dir, name string, when time.Time) *Entry {
	t.Helper()

	if err := v.Mkdir(MainScope, dir); err != nil {
		t.Fatalf("Mkdir %s: %v", dir, err)
	}
	e, _, err := v.Upload(context.Background(), MainScope, dir, name,
		[]byte(dir+"/"+name), UploadOptions{ModifiedAt: when})
	if err != nil {
		t.Fatalf("Upload %s/%s: %v", dir, name, err)
	}
	return e
}

func at(iso string) time.Time {
	when, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		panic(err)
	}
	return when
}

func planOf(t *testing.T, v *Vault, dir string, opts DateSortOptions) *DatePlan {
	t.Helper()

	plan, err := v.DateSort(MainScope, dir, opts)
	if err != nil {
		t.Fatalf("DateSort %s: %v", dir, err)
	}
	return plan
}

// where each file in a plan would land, as "folder/name".
func landings(plan *DatePlan) map[string]string {
	out := map[string]string{}
	for _, m := range plan.Moves {
		out[m.Name] = JoinPath(m.To, m.As)
	}
	return out
}

// The whole of it: a flat folder becomes a year and a month.
func TestDateSortFilesAFlatFolderByYearAndMonth(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{
		"one.jpg":   at("2026-01-14T09:00:00Z"),
		"two.jpg":   at("2026-01-30T09:00:00Z"),
		"three.jpg": at("2025-07-04T09:00:00Z"),
	})

	plan := planOf(t, v, "/photos", DateSortOptions{})

	want := map[string]string{
		"one.jpg":   "/photos/2026/January/one.jpg",
		"two.jpg":   "/photos/2026/January/two.jpg",
		"three.jpg": "/photos/2025/July/three.jpg",
	}
	for name, to := range want {
		if got := landings(plan)[name]; got != to {
			t.Errorf("%s lands at %q, want %q", name, got, to)
		}
	}

	// Oldest first, so a run reads as a calendar filling up rather than as
	// index order.
	if len(plan.Folders) != 2 {
		t.Fatalf("plan makes %d folders, want 2", len(plan.Folders))
	}
	if plan.Folders[0].Label != "2025/July" || plan.Folders[1].Label != "2026/January" {
		t.Errorf("folders read %q then %q, want 2025/July then 2026/January",
			plan.Folders[0].Label, plan.Folders[1].Label)
	}
	if plan.Folders[1].Files != 2 {
		t.Errorf("2026/January takes %d files, want 2", plan.Folders[1].Files)
	}
	if plan.Folders[0].Exists || plan.Folders[1].Exists {
		t.Error("folders that are not there yet are reported as existing")
	}
}

// By year alone is the same act with one level instead of two.
func TestDateSortCanFileByYearAlone(t *testing.T) {
	v := dated(t, "/scans", map[string]time.Time{
		"jan.pdf": at("2026-01-14T09:00:00Z"),
		"dec.pdf": at("2026-12-30T09:00:00Z"),
	})

	plan := planOf(t, v, "/scans", DateSortOptions{Grain: ByYear})

	if len(plan.Folders) != 1 || plan.Folders[0].Path != "/scans/2026" {
		t.Fatalf("filing by year made %+v, want one /scans/2026", plan.Folders)
	}
	if plan.Folders[0].Month != 0 {
		t.Errorf("a year folder names month %d, want none", plan.Folders[0].Month)
	}
	if len(plan.Moves) != 2 {
		t.Errorf("plan moves %d files, want 2", len(plan.Moves))
	}
}

// The property that makes the tool safe to press twice: a file already where
// its date says it belongs is left alone, so the second run is a no-op and a
// stalled first run is finished by running it again.
func TestDateSortLeavesFilesAlreadyFiledAlone(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{"loose.jpg": at("2026-03-02T09:00:00Z")})
	fileDated(t, v, "/photos/2026/March", "filed.jpg", at("2026-03-09T09:00:00Z"))

	plan := planOf(t, v, "/photos", DateSortOptions{Deep: true})

	if len(plan.Moves) != 1 || plan.Moves[0].Name != "loose.jpg" {
		t.Fatalf("plan moves %+v, want only loose.jpg", plan.Moves)
	}
	if plan.Settled != 1 {
		t.Errorf("plan counts %d files already filed, want 1", plan.Settled)
	}
	// And the folder it is already in is not offered as one to make.
	if !plan.Folders[0].Exists {
		t.Error("/photos/2026/March is reported as a folder still to create")
	}
	// Both files counted against the folder they end up sharing.
	if plan.Folders[0].Files != 2 {
		t.Errorf("2026/March holds %d files after the sort, want 2", plan.Folders[0].Files)
	}
}

// Running the plan and asking again must produce nothing left to do — the
// same-answer-twice property, checked against the vault rather than argued.
func TestDateSortIsFinishedAfterItHasBeenRun(t *testing.T) {
	ctx := context.Background()
	v := dated(t, "/photos", map[string]time.Time{
		"a.jpg": at("2026-01-14T09:00:00Z"),
		"b.jpg": at("2025-07-04T09:00:00Z"),
	})

	plan := planOf(t, v, "/photos", DateSortOptions{})
	for _, move := range plan.Moves {
		if err := v.Mkdir(MainScope, move.To); err != nil {
			t.Fatalf("Mkdir %s: %v", move.To, err)
		}
		if _, err := v.Move(ctx, move.ID, move.To, move.As); err != nil {
			t.Fatalf("Move %s: %v", move.Name, err)
		}
	}

	again := planOf(t, v, "/photos", DateSortOptions{Deep: true})
	if len(again.Moves) != 0 {
		t.Errorf("sorting an already sorted folder moves %+v, want nothing", again.Moves)
	}
	if again.Settled != 2 {
		t.Errorf("second plan counts %d files settled, want 2", again.Settled)
	}
}

// Two files of the same name is the normal case for this tool, not an edge one:
// a camera restarts its numbering, and IMG_0001.jpg arrives from three folders.
func TestDateSortNeverLandsOneNameOnAnother(t *testing.T) {
	v, _ := newTestVault(t, 3)
	fileDated(t, v, "/roll/card-a", "IMG_0001.jpg", at("2026-05-02T09:00:00Z"))
	fileDated(t, v, "/roll/card-b", "IMG_0001.jpg", at("2026-05-03T09:00:00Z"))
	fileDated(t, v, "/roll/card-c", "img_0001.JPG", at("2026-05-04T09:00:00Z"))

	plan := planOf(t, v, "/roll", DateSortOptions{Deep: true})

	seen := map[string]bool{}
	for _, move := range plan.Moves {
		full := JoinPath(move.To, move.As)
		if seen[strings.ToLower(full)] {
			t.Fatalf("two files land on %s", full)
		}
		seen[strings.ToLower(full)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("plan lands %d files, want 3", len(seen))
	}
	// Numbered the way a file manager numbers a collision, and compared without
	// case: a folder holding both a.jpg and A.jpg is not one the sort has made
	// easier to read.
	if !seen[strings.ToLower("/roll/2026/May/IMG_0001 (2).jpg")] {
		t.Errorf("the second IMG_0001.jpg was not numbered: %+v", landings(plan))
	}
}

// A name a file is landing beside is spoken for even when that file is not part
// of the sort at all.
func TestDateSortRespectsNamesAlreadyInTheDestination(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{"note.txt": at("2026-02-11T09:00:00Z")})
	fileDated(t, v, "/photos/2026/February", "note.txt", at("2026-02-01T09:00:00Z"))

	// Shallow: the file already in /photos/2026/February is not being sorted,
	// but its name is still taken.
	plan := planOf(t, v, "/photos", DateSortOptions{})

	if len(plan.Moves) != 1 {
		t.Fatalf("plan moves %+v, want one file", plan.Moves)
	}
	if plan.Moves[0].As == "note.txt" {
		t.Error("the moved file would land on the note.txt already there")
	}
	if plan.Moves[0].As != "note (2).txt" {
		t.Errorf("it lands as %q, want %q", plan.Moves[0].As, "note (2).txt")
	}
}

// Shallow is the flat-folder case the tool exists for: the folders somebody has
// already made are left exactly as they are.
func TestDateSortShallowTouchesOnlyTheFilesInTheFolder(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{"loose.jpg": at("2026-01-14T09:00:00Z")})
	fileDated(t, v, "/photos/Corfu", "kept.jpg", at("2026-01-15T09:00:00Z"))

	shallow := planOf(t, v, "/photos", DateSortOptions{})
	if len(shallow.Moves) != 1 || shallow.Moves[0].Name != "loose.jpg" {
		t.Fatalf("a shallow sort moves %+v, want only loose.jpg", shallow.Moves)
	}

	deep := planOf(t, v, "/photos", DateSortOptions{Deep: true})
	if len(deep.Moves) != 2 {
		t.Fatalf("a deep sort moves %d files, want 2", len(deep.Moves))
	}
}

// What a deep sort leaves behind, so the browser can offer to clear it up —
// and, just as much, what it must not offer: the folders it just filled.
func TestDateSortNamesTheFoldersItWouldEmpty(t *testing.T) {
	v, _ := newTestVault(t, 3)
	fileDated(t, v, "/photos/Corfu/2023", "one.jpg", at("2026-01-14T09:00:00Z"))
	fileDated(t, v, "/photos/Keep", "two.jpg", at("2026-01-15T09:00:00Z"))
	if err := v.Mkdir(MainScope, "/photos/Never used"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	plan := planOf(t, v, "/photos", DateSortOptions{Deep: true})

	want := []string{"/photos/Corfu/2023", "/photos/Corfu", "/photos/Keep", "/photos/Never used"}
	if len(plan.Emptied) != len(want) {
		t.Fatalf("plan empties %v, want %v", plan.Emptied, want)
	}
	// Deepest first: each is removed on its own and non-recursively, so a
	// parent can only go after its children have.
	if plan.Emptied[0] != "/photos/Corfu/2023" {
		t.Errorf("the emptied folders lead with %s, want the deepest", plan.Emptied[0])
	}
	for _, folder := range plan.Emptied {
		if folder == "/photos/2026" || folder == "/photos/2026/January" {
			t.Errorf("%s is offered for removal, and the sort just filled it", folder)
		}
	}
}

// A shallow sort empties nothing, because it moves nothing out of a folder
// anybody made.
func TestDateSortShallowEmptiesNothing(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{"loose.jpg": at("2026-01-14T09:00:00Z")})
	fileDated(t, v, "/photos/Corfu", "kept.jpg", at("2026-01-15T09:00:00Z"))

	if plan := planOf(t, v, "/photos", DateSortOptions{}); len(plan.Emptied) != 0 {
		t.Errorf("a shallow sort would empty %v, want nothing", plan.Emptied)
	}
}

// The viewer's clock, not the server's: a file the browser shows as the last
// evening of December must not be filed under January.
func TestDateSortFilesByTheViewersClock(t *testing.T) {
	// 23:40 on 31 December in a zone seven hours west, stored as 06:40 on the
	// first of January.
	v := dated(t, "/photos", map[string]time.Time{"newyear.jpg": at("2026-01-01T06:40:00Z")})

	utc := planOf(t, v, "/photos", DateSortOptions{})
	if utc.Moves[0].To != "/photos/2026/January" {
		t.Errorf("with no offset it files under %s, want /photos/2026/January", utc.Moves[0].To)
	}

	west := planOf(t, v, "/photos", DateSortOptions{Offset: -7 * 60})
	if west.Moves[0].To != "/photos/2025/December" {
		t.Errorf("seven hours west it files under %s, want /photos/2025/December", west.Moves[0].To)
	}

	// The time reported back is the stored one, so the browser is never shown a
	// date it did not itself produce.
	if !west.Moves[0].Modified.Equal(at("2026-01-01T06:40:00Z")) {
		t.Errorf("the plan reports %v, want the time as stored", west.Moves[0].Modified)
	}
}

// A file with no time at all is left where it is rather than swept into a
// folder named for a date nobody claimed.
func TestDateSortLeavesAFileWithNoTimeWhereItIs(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{
		"dated.jpg": at("2026-01-14T09:00:00Z"),
		"blank.jpg": at("2026-01-15T09:00:00Z"),
	})
	// An entry written before a file could carry a time of its own.
	v.manifest.ByPath("/photos/blank.jpg").ModifiedAt = time.Time{}

	plan := planOf(t, v, "/photos", DateSortOptions{})

	if plan.Undated != 1 {
		t.Errorf("plan counts %d files with no time, want 1", plan.Undated)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].Name != "dated.jpg" {
		t.Errorf("plan moves %+v, want only dated.jpg", plan.Moves)
	}
}

func TestDateSortRefusesWhatItCannotAnswer(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{"one.jpg": at("2026-01-14T09:00:00Z")})

	if _, err := v.DateSort(MainScope, "/nowhere", DateSortOptions{}); err == nil {
		t.Error("sorting a folder that is not there succeeded")
	}
	if _, err := v.DateSort(MainScope, "/photos", DateSortOptions{Grain: "week"}); err == nil {
		t.Error("an unknown grain was accepted")
	}
	if _, err := v.DateSort(MainScope, "/photos", DateSortOptions{Offset: 5000}); err == nil {
		t.Error("an offset no clock on earth has was accepted")
	}

	v.Lock()
	if _, err := v.DateSort(MainScope, "/photos", DateSortOptions{}); err == nil {
		t.Error("sorting a locked vault succeeded")
	}
}

// Nothing travels, and the plan says so in the one figure that matters.
func TestDateSortCountsWhatMovesAndNotWhatStays(t *testing.T) {
	v := dated(t, "/photos", map[string]time.Time{"one.jpg": at("2026-01-14T09:00:00Z")})
	fileDated(t, v, "/photos/2026/January", "already.jpg", at("2026-01-20T09:00:00Z"))

	plan := planOf(t, v, "/photos", DateSortOptions{Deep: true})

	var moving int64
	for _, move := range plan.Moves {
		moving += move.Size
	}
	if plan.Bytes != moving {
		t.Errorf("plan reports %d bytes moving, want %d", plan.Bytes, moving)
	}
	if plan.Folders[0].Bytes <= moving {
		t.Errorf("the folder comes to %d bytes, want more than the %d arriving",
			plan.Folders[0].Bytes, moving)
	}
}

func TestDateLabelNamesTheMonthRatherThanNumberingIt(t *testing.T) {
	when := at("2026-09-07T12:00:00Z")
	if got := dateLabel(when, ByMonth); got != "2026/September" {
		t.Errorf("dateLabel by month = %q, want 2026/September", got)
	}
	if got := dateLabel(when, ByYear); got != "2026" {
		t.Errorf("dateLabel by year = %q, want 2026", got)
	}
}
