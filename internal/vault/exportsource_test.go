package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// store puts one file in the vault at dir/name, for the export tests to send
// back out.
func store(t *testing.T, v *Vault, dir, name, content string) *Entry {
	t.Helper()
	if err := v.ensureFolder(MainScope, dir, map[string]bool{}); err != nil {
		t.Fatalf("making %s: %v", dir, err)
	}
	entry, _, err := v.Upload(context.Background(), MainScope, dir, name, []byte(content), UploadOptions{})
	if err != nil {
		t.Fatalf("storing %s/%s: %v", dir, name, err)
	}
	return entry
}

// onDisk reads a file the export was meant to have written.
func onDisk(t *testing.T, root string, rel ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
	if err != nil {
		t.Fatalf("nothing on the machine at %s: %v", filepath.Join(rel...), err)
	}
	return string(data)
}

func remotePaths(summary ExportSummary) []string {
	out := make([]string, 0, len(summary.Results))
	for _, r := range summary.Results {
		out = append(out, r.Dest)
	}
	sort.Strings(out)
	return out
}

func TestExportFiles(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "notes.txt", "hello from the vault")

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/notes.txt"},
		Dest:  "out",
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if summary.Exported != 1 || summary.Failed != 0 || summary.Skipped != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	if summary.Bytes != int64(len("hello from the vault")) {
		t.Errorf("counted %d bytes, want the file's length", summary.Bytes)
	}
	if got := onDisk(t, root, "out", "notes.txt"); got != "hello from the vault" {
		t.Errorf("the machine holds %q, want the file's contents", got)
	}
	if summary.Results[0].Dest != "out/notes.txt" {
		t.Errorf("landed at %q, want out/notes.txt", summary.Results[0].Dest)
	}
	// Written for the owner alone: these are the user's files in the clear.
	info, _ := os.Stat(filepath.Join(root, "out", "notes.txt"))
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the file is readable by others: %v", info.Mode())
	}
}

// Picking a folder sends everything under it, keeping its shape.
func TestExportFolderKeepsItsShape(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/media/films/2019", "one.mp4", "one")
	store(t, v, "/media/films/2020", "two.mp4", "two")
	store(t, v, "/media/films", "poster.jpg", "poster")
	store(t, v, "/media", "elsewhere.txt", "not selected")

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/media/films"},
		Dest:  "backup",
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if summary.Exported != 3 {
		t.Fatalf("exported %d, want 3: %+v", summary.Exported, summary.Results)
	}
	want := []string{"backup/films/2019/one.mp4", "backup/films/2020/two.mp4", "backup/films/poster.jpg"}
	if got := remotePaths(summary); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("landed at %v, want %v", got, want)
	}
	if got := onDisk(t, root, "backup", "films", "2019", "one.mp4"); got != "one" {
		t.Errorf("stored %q, want one", got)
	}
	if _, err := os.Stat(filepath.Join(root, "backup", "elsewhere.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file outside the selection was exported")
	}
}

// The root has no name to become a folder, so its contents go straight into
// the destination.
func TestExportOfTheRootLandsInTheDestination(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "a")
	store(t, v, "/deep", "b.txt", "b")

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/"}, Dest: "all",
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := []string{"all/a.txt", "all/deep/b.txt"}
	if got := remotePaths(summary); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("landed at %v, want %v", got, want)
	}
	onDisk(t, root, "all", "deep", "b.txt")
}

// An empty destination is the source's own folder.
func TestExportIntoTheSourceRoot(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "a")

	if _, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/a.txt"},
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	onDisk(t, root, "a.txt")
}

// The load-bearing claim: re-running an export is how you resume it.
func TestExportSkipsWhatIsAlreadyThere(t *testing.T) {
	v, id, root := importFixture(t)
	a := store(t, v, "/", "a.txt", "aaa")
	store(t, v, "/", "b.txt", "bbb")

	req := ExportRequest{Paths: []string{"/a.txt", "/b.txt"}, Dest: ""}
	if _, err := v.ExportToSource(context.Background(), MainScope, id, req); err != nil {
		t.Fatalf("first export: %v", err)
	}

	// The copy carries the vault's own time, which is what the second run
	// recognises it by.
	info, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(a.ModifiedAt.Truncate(time.Second)) {
		t.Errorf("stamped %v, want the vault's %v", info.ModTime(), a.ModifiedAt)
	}

	// A third file turns up, as it would if the first run had been
	// interrupted partway through the selection.
	store(t, v, "/", "c.txt", "ccc")
	req.Paths = append(req.Paths, "/c.txt")

	again, err := v.ExportToSource(context.Background(), MainScope, id, req)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if again.Exported != 1 || again.Skipped != 2 {
		t.Fatalf("re-running sent %d and skipped %d, want 1 and 2: %+v",
			again.Exported, again.Skipped, again.Results)
	}
	for _, r := range again.Results {
		if r.Skipped && r.Reason != "already there" {
			t.Errorf("%s was skipped for %q", r.Dest, r.Reason)
		}
	}
	if again.Bytes != 3 {
		t.Errorf("counted %d bytes on the second run, want the one file that moved", again.Bytes)
	}
}

// A file that is there and is not the same file is left exactly as it was,
// and the line says why. Replace is how somebody asks otherwise.
func TestExportWillNotClobberADifferentFile(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "from the vault")
	seed(t, filepath.Join(root, "a.txt"), "something else entirely")

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/a.txt"},
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if summary.Skipped != 1 || summary.Exported != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	if !strings.Contains(summary.Results[0].Reason, "different file") {
		t.Errorf("reason = %q", summary.Results[0].Reason)
	}
	if got := onDisk(t, root, "a.txt"); got != "something else entirely" {
		t.Errorf("the file on the machine was changed to %q", got)
	}

	replaced, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/a.txt"}, Overwrite: true,
	})
	if err != nil {
		t.Fatalf("export with overwrite: %v", err)
	}
	if replaced.Exported != 1 {
		t.Fatalf("overwriting did not send the file: %+v", replaced.Results)
	}
	if got := onDisk(t, root, "a.txt"); got != "from the vault" {
		t.Errorf("after replacing, the machine holds %q", got)
	}
}

// A copy that is older than the vault's — the file was replaced in the vault
// since it was exported — is not mistaken for the current one.
func TestExportNoticesAStaleCopy(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "aaa")
	seed(t, filepath.Join(root, "a.txt"), "old")
	long := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), long, long); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/a.txt"},
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if summary.Skipped != 1 || !strings.Contains(summary.Results[0].Reason, "older") {
		t.Errorf("a stale same-size copy was reported as %+v", summary.Results[0])
	}
}

// Nothing outside the source's folder can be written, however the destination
// is spelled — and the refusal comes before a connection is made.
func TestExportRefusesToLeaveTheSourceFolder(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "aaa")

	for _, dest := range []string{"..", "../", "../escape", "x/../.."} {
		_, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
			Paths: []string{"/a.txt"}, Dest: dest,
		})
		if err == nil {
			t.Errorf("exporting into %q was allowed", dest)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file was written above the source's folder")
	}
}

// A file stored before chunking cannot be streamed, and this path will not
// rebuild it in memory to make up for that. It fails on its own line, naming
// the cure, and the rest of the selection still goes.
func TestExportRefusesAPreChunkingFileAndSendsTheRest(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "new.txt", "chunked")
	addWholeEntry(t, v, "old-1", "old.bin", []byte("stored whole"))

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/"},
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if summary.Exported != 1 || summary.Failed != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	for _, r := range summary.Results {
		if r.Path == "/old.bin" && !strings.Contains(r.Error, "converted") {
			t.Errorf("the old-format file failed with %q, which does not say to convert it", r.Error)
		}
	}
	onDisk(t, root, "new.txt")
	if _, err := os.Stat(filepath.Join(root, "old.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the old-format file was written anyway")
	}
}

func TestExportRefusesAnEmptyOrUnknownSelection(t *testing.T) {
	v, id, _ := importFixture(t)
	if err := v.Mkdir(MainScope, "/empty"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := map[string][]string{
		"nothing named":    {},
		"an empty folder":  {"/empty"},
		"a missing path":   {"/nowhere.txt"},
		"an empty string":  {""},
		"a folder and air": {"/empty", "/gone"},
	}
	for name, paths := range cases {
		if _, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{Paths: paths}); err == nil {
			t.Errorf("exporting %s was allowed", name)
		}
	}
}

// Picking a folder and a file inside it sends the file once.
func TestExportSendsAFileOnceHoweverItWasPicked(t *testing.T) {
	v, id, _ := importFixture(t)
	store(t, v, "/photos", "a.jpg", "a")

	summary, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths: []string{"/photos", "/photos/a.jpg"},
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(summary.Results) != 1 || summary.Exported != 1 {
		t.Errorf("a file picked twice was sent %d times: %+v", len(summary.Results), summary.Results)
	}
}

// The progress reported is a window onto the request: which file of how many,
// the one stage an export has, and what became of the files before it.
func TestExportReportsProgress(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "aaa")
	store(t, v, "/", "b.txt", strings.Repeat("b", 3000))
	seed(t, filepath.Join(root, "a.txt"), "aaa")

	var seen []TransferProgress
	_, err := v.ExportToSource(context.Background(), MainScope, id, ExportRequest{
		Paths:      []string{"/a.txt", "/b.txt"},
		OnProgress: func(at TransferProgress) { seen = append(seen, at) },
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("nothing was reported")
	}
	// a.txt is already there, so it is passed over in silence; b.txt is the
	// second of two, with one skipped before it.
	for _, at := range seen {
		if at.Name != "b.txt" || at.File != 2 || at.Files != 2 || at.Stage != StageSending {
			t.Errorf("reported %+v", at)
		}
		if at.Skipped != 1 || at.Completed != 0 {
			t.Errorf("tallies before b.txt were %d done, %d skipped: want 0 and 1", at.Completed, at.Skipped)
		}
		if at.Path != "/b.txt" || at.Dest != "b.txt" {
			t.Errorf("reported paths %q → %q", at.Path, at.Dest)
		}
	}
	last := seen[len(seen)-1]
	if last.Done != 3000 || last.Size != 3000 {
		t.Errorf("the last report said %d of %d, want the whole file", last.Done, last.Size)
	}
}

// A cancelled export stops after the file it is on, keeps what landed whole,
// and does not report a failure for every file it never reached.
func TestExportStopsWhenCancelled(t *testing.T) {
	v, id, root := importFixture(t)
	store(t, v, "/", "a.txt", "aaa")
	store(t, v, "/", "b.txt", "bbb")

	ctx, cancel := context.WithCancel(context.Background())
	summary, err := v.ExportToSource(ctx, MainScope, id, ExportRequest{
		Paths: []string{"/a.txt", "/b.txt"},
		OnProgress: func(TransferProgress) {
			// Pulled the moment the first file is picked up.
			cancel()
		},
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// The first file was picked up and then pulled out from under; the
	// second was never reached and has no line, failed or otherwise.
	if len(summary.Results) != 1 {
		t.Errorf("a cancelled export reported %d files, want the one it was on: %+v",
			len(summary.Results), summary.Results)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file the export never reached landed on the machine")
	}
}
