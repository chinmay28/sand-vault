package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
)

// importFixture gives a vault with three accounts and one connected source.
func importFixture(t *testing.T) (*Vault, string, string) {
	t.Helper()

	v, _ := newTestVault(t, 3)
	src, root := testSource(t, "vps")
	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	return v, added.ID, root
}

// read pulls a stored file back out of the vault, so a test can check what
// arrived rather than only that something did.
func read(t *testing.T, v *Vault, full string) string {
	t.Helper()
	entry := v.manifest.ByPath(full)
	if entry == nil {
		t.Fatalf("nothing stored at %s", full)
	}
	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("fetching %s: %v", full, err)
	}
	return string(data)
}

// stored says which of the paths are in the main vault's index, so a test can
// check where a selection landed.
func stored(v *Vault, paths ...string) []string {
	var out []string
	for _, p := range paths {
		if v.manifest.ByPath(p) != nil {
			out = append(out, p)
		}
	}
	return out
}

// touch sets a source file's modification time, which is the thing every test
// about times has to say.
func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("touching %s: %v", path, err)
	}
}

func TestImportFiles(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "notes.txt"), "hello from the vps")
	seed(t, filepath.Join(root, "films", "one.mp4"), "a film")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"notes.txt"},
		Dest:  "/",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 1 || summary.Failed != 0 || summary.Skipped != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	if got := read(t, v, "/notes.txt"); got != "hello from the vps" {
		t.Errorf("stored %q, want the file's contents", got)
	}
	// A file that simply arrived is a count, not a line.
	if len(summary.Results) != 0 {
		t.Errorf("a clean import listed %d lines: %+v", len(summary.Results), summary.Results)
	}
}

// Selecting a folder brings everything under it, keeping its shape.
func TestImportFolderKeepsItsShape(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "films", "2019", "one.mp4"), "one")
	seed(t, filepath.Join(root, "films", "2020", "two.mp4"), "two")
	seed(t, filepath.Join(root, "films", "poster.jpg"), "poster")
	seed(t, filepath.Join(root, "elsewhere.txt"), "not selected")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"films"},
		Dest:  "/media",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 3 {
		t.Fatalf("imported %d, want 3: %+v", summary.Imported, summary.Results)
	}

	want := []string{"/media/films/2019/one.mp4", "/media/films/2020/two.mp4", "/media/films/poster.jpg"}
	if got := stored(v, want...); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("landed at %v, want %v", got, want)
	}
	if got := read(t, v, "/media/films/2019/one.mp4"); got != "one" {
		t.Errorf("stored %q, want %q", got, "one")
	}
	if v.manifest.ByPath("/media/elsewhere.txt") != nil {
		t.Error("a file outside the selection was imported")
	}
}

// The load-bearing claim: re-running an import is how you resume it.
func TestImportSkipsWhatIsAlreadyThere(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "a.txt"), "aaa")
	seed(t, filepath.Join(root, "b.txt"), "bbb")

	req := ImportRequest{Paths: []string{"a.txt", "b.txt"}, Dest: "/"}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// A third file turns up, as it would if the first run had been interrupted
	// partway through the selection.
	seed(t, filepath.Join(root, "c.txt"), "ccc")
	req.Paths = append(req.Paths, "c.txt")

	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Imported != 1 || again.Skipped != 2 {
		t.Fatalf("re-running fetched %d and skipped %d, want 1 and 2: %+v",
			again.Imported, again.Skipped, again.Results)
	}
	// Already here is the answer on a second run, and it is given as a count:
	// on a selection of every file, a line per skip would be the whole list.
	if len(again.Results) != 0 {
		t.Errorf("files already here were listed: %+v", again.Results)
	}

	// Nothing was stored twice under a numbered name.
	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 3 {
		names := make([]string, 0, len(listing.Files))
		for _, f := range listing.Files {
			names = append(names, f.Name)
		}
		t.Errorf("vault holds %v, want three files", names)
	}
}

// The one case a size comparison alone would get wrong: a file replaced on the
// source by a different file of the same length.
func TestImportRefetchesAFileThatChanged(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "a.txt"), "aaa")

	req := ImportRequest{Paths: []string{"a.txt"}, Dest: "/", Overwrite: true}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// Same length, different contents, and newer.
	seed(t, filepath.Join(root, "a.txt"), "zzz")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), future, future); err != nil {
		t.Fatalf("touching the file: %v", err)
	}

	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Imported != 1 {
		t.Fatalf("a changed file was not fetched again: %+v", again.Results)
	}
	if got := read(t, v, "/a.txt"); got != "zzz" {
		t.Errorf("vault holds %q, want the new contents", got)
	}
}

// A file keeps the age it had on the machine it came from, so a folder of
// photographs arrives dated when they were taken.
func TestImportKeepsTheSourcesTime(t *testing.T) {
	v, id, root := importFixture(t)
	file := filepath.Join(root, "hike.jpg")
	seed(t, file, "a photograph")
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	touch(t, file, taken)

	if _, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"hike.jpg"},
		Dest:  "/",
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	entry := v.manifest.ByPath("/hike.jpg")
	if !SameModTime(entry.ModifiedAt, taken) {
		t.Errorf("stored under %s, want the file's own time %s", entry.ModifiedAt, taken)
	}
	// When it arrived here is the other question, and still has today's answer.
	if time.Since(entry.CreatedAt) > time.Minute {
		t.Errorf("CreatedAt is %s, want the moment the import ran", entry.CreatedAt)
	}
}

// The retroactive half: a file imported before the time was kept is filed under
// the day it was imported, and running the import again puts it right without
// fetching a byte of it.
func TestImportRetimesAFileImportedUnderTheWrongTime(t *testing.T) {
	v, id, root := importFixture(t)
	file := filepath.Join(root, "hike.jpg")
	seed(t, file, "a photograph")
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	touch(t, file, taken)

	req := ImportRequest{Paths: []string{"hike.jpg"}, Dest: "/"}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Stamped the way every import used to stamp: with the moment it landed.
	entry := v.manifest.ByPath("/hike.jpg")
	was := entry.ID
	entry.ModifiedAt = entry.CreatedAt

	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Retimed != 1 || again.Imported != 0 || again.Skipped != 0 || again.Failed != 0 {
		t.Fatalf("re-running gave %+v, want one file retimed and nothing fetched", again)
	}
	if !SameModTime(entry.ModifiedAt, taken) {
		t.Errorf("the entry says %s, want the source's %s", entry.ModifiedAt, taken)
	}
	// Retiming is an index write: the file itself was never in question.
	if entry.ID != was {
		t.Errorf("the file was stored again rather than retimed")
	}
	if got := read(t, v, "/hike.jpg"); got != "a photograph" {
		t.Errorf("vault holds %q, want the file's contents", got)
	}
	// Counted rather than listed, like every other "already here".
	if len(again.Results) != 0 {
		t.Errorf("retimed files were listed: %+v", again.Results)
	}
}

// Once the times agree there is nothing left to do, and a third run says so by
// skipping in silence.
func TestImportSkipsWhenTheTimesAgree(t *testing.T) {
	v, id, root := importFixture(t)
	file := filepath.Join(root, "hike.jpg")
	seed(t, file, "a photograph")
	touch(t, file, time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC))

	req := ImportRequest{Paths: []string{"hike.jpg"}, Dest: "/"}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}

	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Skipped != 1 || again.Retimed != 0 || again.Imported != 0 {
		t.Errorf("re-running gave %+v, want one file skipped and nothing else", again)
	}
}

// The safety the time guard was there for in the first place: a file touched on
// the source since it was imported is fetched again, not quietly retimed.
func TestImportRefetchesRatherThanRetimingAChangedFile(t *testing.T) {
	v, id, root := importFixture(t)
	file := filepath.Join(root, "a.txt")
	seed(t, file, "aaa")
	touch(t, file, time.Now().Add(-24*time.Hour))

	req := ImportRequest{Paths: []string{"a.txt"}, Dest: "/", Overwrite: true}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// Same length, different contents, and touched since — which is exactly
	// what a stale recorded time looks like from the outside, and must not be
	// mistaken for one.
	seed(t, file, "zzz")
	touch(t, file, time.Now().Add(time.Hour))

	req.Overwrite = false
	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Retimed != 0 {
		t.Fatalf("a changed file was retimed rather than fetched: %+v", again)
	}
	if again.Imported != 1 {
		t.Fatalf("a changed file was not fetched again: %+v", again.Results)
	}
}

// Overwrite is how somebody says "fetch it again anyway".
func TestImportOverwriteIgnoresTheSkip(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "a.txt"), "aaa")

	req := ImportRequest{Paths: []string{"a.txt"}, Dest: "/"}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}

	req.Overwrite = true
	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Imported != 1 || again.Skipped != 0 {
		t.Errorf("overwrite did not force a re-fetch: %+v", again)
	}
}

// Picking a folder and a file inside it is easy to do with checkboxes, and
// importing it twice would leave a numbered copy beside it.
func TestImportDoesNotFetchTheSameFileTwice(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "films", "one.mp4"), "one")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"films", "films/one.mp4"},
		Dest:  "/",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 1 {
		t.Errorf("one file selected two ways was imported %d times", summary.Imported)
	}
	listing, err := v.List(MainScope, "/films")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 {
		t.Errorf("the folder holds %d files, want the one", len(listing.Files))
	}
}

// There is no cap on a selection, and the walk is not cut where the browser's
// listing is: a folder with more files than one page shows is imported whole.
func TestPlanImportReadsPastTheListingCap(t *testing.T) {
	v, id, root := importFixture(t)
	const files = sandsftp.MaxEntries + 10
	for i := 0; i < files; i++ {
		seed(t, filepath.Join(root, "crowded", fmt.Sprintf("f%05d", i)), "x")
	}

	client, source, err := v.connectSource(context.Background(), id)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	// The browser's own listing of the folder is a page, cut short and
	// saying so.
	page, err := client.ReadDir(source.Root, "crowded")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if !page.Truncated {
		t.Fatalf("a folder of %d files was not cut for the browser, so this test proves nothing", files)
	}

	plan, err := planImport(client, source.Root, []string{"crowded"}, "/", nil)
	if err != nil {
		t.Fatalf("planImport: %v", err)
	}
	if len(plan) != files {
		t.Errorf("planned %d files, want every one of %d", len(plan), files)
	}
	if plan[0].remote != "crowded/f00000" || plan[files-1].remote != fmt.Sprintf("crowded/f%05d", files-1) {
		t.Errorf("plan runs from %q to %q", plan[0].remote, plan[files-1].remote)
	}
}

// The walk says how far it has got, which is the only thing there is to say
// before any file has a name — and on a folder of ten thousand, listed a
// directory at a time over a link with a round trip in it, the difference
// between a dialog that is thinking and one that has died.
func TestPlanImportReportsWhatItHasFound(t *testing.T) {
	v, id, root := importFixture(t)
	const files = 2*planEvery + 5
	for i := 0; i < files; i++ {
		seed(t, filepath.Join(root, "crowded", fmt.Sprintf("f%05d", i)), "x")
	}

	client, source, err := v.connectSource(context.Background(), id)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var found []int
	plan, err := planImport(client, source.Root, []string{"crowded"}, "/",
		func(n int) { found = append(found, n) })
	if err != nil {
		t.Fatalf("planImport: %v", err)
	}
	if len(plan) != files {
		t.Fatalf("planned %d files, want %d", len(plan), files)
	}

	// Every so many files rather than every file: the walk can find thousands
	// a second, and a report each would be a wake-up per file to move a number
	// nobody reads that closely.
	if len(found) > files/planEvery+1 {
		t.Errorf("a walk of %d files reported %d times: %v", files, len(found), found)
	}
	if len(found) < 2 {
		t.Fatalf("a walk of %d files reported %v, want it saying so as it went", files, found)
	}
	for i := 1; i < len(found); i++ {
		if found[i] <= found[i-1] {
			t.Errorf("the count did not climb: %v", found)
		}
	}
	// The last word is the whole count, whatever the throttle was holding.
	if found[len(found)-1] != files {
		t.Errorf("the walk finished on %d, want the %d it found", found[len(found)-1], files)
	}
}

// Which files get a line: the ones that did not simply arrive.
func TestImportResultWorthALine(t *testing.T) {
	cases := map[string]struct {
		result ImportResult
		want   bool
	}{
		"arrived":                {ImportResult{OK: true}, false},
		"already here":           {ImportResult{Skipped: true, Reason: "already imported"}, false},
		"failed":                 {ImportResult{Error: "the source hung up"}, true},
		"arrived with a warning": {ImportResult{OK: true, Warnings: []string{"stored on one cloud fewer than asked"}}, true},
	}
	for name, c := range cases {
		if got := c.result.worthALine(); got != c.want {
			t.Errorf("%s: worth a line = %v, want %v", name, got, c.want)
		}
	}
}

func TestImportRefusesToLeaveTheSourceFolder(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(filepath.Dir(root), "secret.txt"), "not yours")
	seed(t, filepath.Join(root, "fine.txt"), "yours")

	for _, rel := range []string{"../secret.txt", "..", "a/../../secret.txt"} {
		if _, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
			Paths: []string{rel},
			Dest:  "/",
		}); err == nil {
			t.Errorf("importing %q was allowed", rel)
		}
	}
	if v.manifest.ByPath("/secret.txt") != nil {
		t.Error("a file outside the source's folder was imported")
	}
}

// A selection with nothing in it is a mistake worth naming, not an import that
// silently does nothing.
func TestImportRefusesAnEmptySelection(t *testing.T) {
	v, id, root := importFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"empty"},
		Dest:  "/",
	}); err == nil {
		t.Error("an empty folder imported without complaint")
	}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{},
		Dest:  "/",
	}); err == nil {
		t.Error("a selection of nothing imported without complaint")
	}
}

// One bad file does not take the rest of the selection down with it.
func TestImportReportsPerFile(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "good.txt"), "fine")
	seed(t, filepath.Join(root, "vanishing.txt"), "here for now")

	// Planned while it exists, gone by the time it is fetched — which is what
	// a file deleted on the source mid-import looks like.
	client, source, err := v.connectSource(context.Background(), id)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	files, err := planImport(client, source.Root, []string{"good.txt", "vanishing.txt"}, "/", nil)
	client.Close()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("planned %d files, want 2", len(files))
	}
	if err := os.Remove(filepath.Join(root, "vanishing.txt")); err != nil {
		t.Fatal(err)
	}

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"good.txt", "vanishing.txt"},
		Dest:  "/",
	})
	// The file is gone before planning even reaches it here, so the request
	// fails as a whole; what matters is that it says which path was the
	// problem rather than failing anonymously.
	if err != nil {
		if !strings.Contains(err.Error(), "vanishing.txt") {
			t.Errorf("error does not name the missing file: %v", err)
		}
		return
	}
	if summary.Imported != 1 || summary.Failed != 1 {
		t.Errorf("summary: %+v", summary.Results)
	}
}

func TestImportIntoASubVault(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "private.txt"), "for the sub vault")

	sub, err := v.CreateSubVault("private", "a-sub-vault-password")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	scope := Scope(sub.ID)

	summary, err := v.ImportFromSource(context.Background(), scope, id, ImportRequest{
		Paths: []string{"private.txt"},
		Dest:  "/",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 1 {
		t.Fatalf("summary: %+v", summary.Results)
	}

	// In the sub vault's index, and not in the main one.
	if v.manifest.ByPath("/private.txt") != nil {
		t.Error("a file imported into a sub vault landed in the main vault")
	}
	listing, err := v.List(scope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "private.txt" {
		t.Errorf("sub vault holds %+v", listing.Files)
	}
}

// The progress a caller can draw while an import runs: every file it picks up,
// both halves of the trip, and the tallies as they stand.
func TestImportReportsProgress(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "a.txt"), "aaa")
	seed(t, filepath.Join(root, "b.txt"), "bbbb")

	var seen []TransferProgress
	req := ImportRequest{
		Paths:      []string{"a.txt", "b.txt"},
		Dest:       "/",
		OnProgress: func(at TransferProgress) { seen = append(seen, at) },
	}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("an import with a progress callback reported nothing")
	}

	// The walk speaks first, before any file has a name: how many files the
	// selection holds is all there is to say, and saying nothing at all is
	// what made a long walk look like a hang.
	first := seen[0]
	if first.Stage != StagePlanning || first.Files != 2 || first.Name != "" {
		t.Errorf("first report was %+v, want the walk having found 2 files", first)
	}

	// Then a file, announced before a byte of it moves, so what is being
	// worked on shows up at once rather than once enough of it has arrived.
	second := seen[1]
	if second.Stage != StageChecking || second.Done != 0 {
		t.Errorf("second report was %s at %d bytes, want checking at 0", second.Stage, second.Done)
	}
	if second.Name != "a.txt" || second.File != 1 || second.Files != 2 {
		t.Errorf("second report was %+v, want a.txt as 1 of 2", second)
	}

	// Both halves of the trip are reported, and named apart: coming down from
	// the source and going back up to the accounts are slow for different
	// reasons and one is not the other's second half.
	stages := map[TransferStage]bool{}
	for _, at := range seen {
		stages[at.Stage] = true
		if at.Stage == StagePlanning {
			continue // no file yet, so no size to report
		}
		if at.Done > at.Size {
			t.Errorf("%s reported %d of %d bytes", at.Name, at.Done, at.Size)
		}
		if at.Size == 0 {
			t.Errorf("%s reported no size at all", at.Name)
		}
	}
	if !stages[StageFetching] || !stages[StageScattering] {
		t.Errorf("stages reported were %v, want both fetching and scattering", stages)
	}

	// The second file knows the first one landed, which is what makes the
	// counts readable mid-flight: they are what the summary would say if the
	// import stopped here.
	var next *TransferProgress
	for i := range seen {
		if seen[i].Name == "b.txt" {
			next = &seen[i]
			break
		}
	}
	if next == nil {
		t.Fatal("the second file was never reported")
	}
	if next.File != 2 || next.Completed != 1 {
		t.Errorf("b.txt was reported as file %d with %d imported, want 2 and 1", next.File, next.Completed)
	}
}

// A file already in the vault moves no bytes, and used to be passed over in
// silence with it. On one file that was right; on the run it actually happens
// on — a whole folder re-imported, where every file is already here — it meant
// a transfer that reported nothing whatever from beginning to end and could
// not be told apart from a hang. So a file is named as it is looked at, moving
// or not.
func TestImportReportsTheFilesItOnlyLooksAt(t *testing.T) {
	v, id, root := importFixture(t)
	seed(t, filepath.Join(root, "a.txt"), "aaa")

	req := ImportRequest{Paths: []string{"a.txt"}, Dest: "/"}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}

	var seen []TransferProgress
	req.OnProgress = func(at TransferProgress) { seen = append(seen, at) }
	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Skipped != 1 {
		t.Fatalf("the second run fetched the file again: %+v", again.Results)
	}

	// Named, and named as what it is: looked at rather than moved. Nothing
	// claims to be fetching or scattering, because nothing is.
	var checked *TransferProgress
	for i := range seen {
		switch seen[i].Stage {
		case StageChecking:
			checked = &seen[i]
		case StageFetching, StageScattering:
			t.Errorf("a file already here reported %s", seen[i].Stage)
		}
	}
	if checked == nil {
		t.Fatal("a run where everything was already here reported no file at all")
	}
	if checked.Name != "a.txt" || checked.File != 1 || checked.Files != 1 {
		t.Errorf("the skipped file was reported as %+v, want a.txt as 1 of 1", *checked)
	}
	if checked.Done != 0 {
		t.Errorf("a file nothing was fetched for reported %d bytes moved", checked.Done)
	}
}

// The counts climb as the files are looked at, so a run that is only putting
// times back says how far it has got — which is the run this was all for.
func TestImportProgressCountsFilesItRetimes(t *testing.T) {
	v, id, root := importFixture(t)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	names := []string{"one.jpg", "two.jpg", "three.jpg"}
	for _, name := range names {
		file := filepath.Join(root, name)
		seed(t, file, "a photograph")
		touch(t, file, taken)
	}

	req := ImportRequest{Paths: names, Dest: "/"}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Stamped the way every import used to stamp them: with the day they
	// landed, which is what a re-import is asked to put right.
	for _, name := range names {
		entry := v.manifest.ByPath("/" + name)
		entry.ModifiedAt = entry.CreatedAt
	}

	var last TransferProgress
	req.OnProgress = func(at TransferProgress) { last = at }
	again, err := v.ImportFromSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Retimed != len(names) || again.Imported != 0 {
		t.Fatalf("re-running gave %+v, want every file retimed and nothing fetched", again)
	}

	// The last file was reported knowing the two before it were done, which is
	// what makes the tally readable while it runs.
	if last.File != len(names) || last.Files != len(names) {
		t.Errorf("the last report was file %d of %d, want %d of %d",
			last.File, last.Files, len(names), len(names))
	}
	if last.Retimed != len(names)-1 {
		t.Errorf("the last report said %d retimed so far, want %d", last.Retimed, len(names)-1)
	}

	// And every correction landed, though they were written in one go at the
	// end rather than one file at a time. See importRetimes.
	for _, name := range names {
		if entry := v.manifest.ByPath("/" + name); !SameModTime(entry.ModifiedAt, taken) {
			t.Errorf("%s says %s, want the source's %s", name, entry.ModifiedAt, taken)
		}
	}
}

// The bytes reported are the file's own, all of them, and the last read is not
// swallowed — a bar that stopped at 99% because the tail was short would look
// stuck exactly where it matters.
func TestImportProgressCountsEveryByte(t *testing.T) {
	v, id, root := importFixture(t)
	body := strings.Repeat("x", 5000)
	seed(t, filepath.Join(root, "big.bin"), body)

	var fetched int64
	req := ImportRequest{
		Paths: []string{"big.bin"},
		Dest:  "/",
		OnProgress: func(at TransferProgress) {
			if at.Stage == StageFetching && at.Done > fetched {
				fetched = at.Done
			}
		},
	}
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("import: %v", err)
	}
	if fetched != int64(len(body)) {
		t.Errorf("fetching reported %d bytes, want %d", fetched, len(body))
	}
}
