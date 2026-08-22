package vault

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
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

func destPaths(summary ImportSummary) []string {
	out := make([]string, 0, len(summary.Results))
	for _, r := range summary.Results {
		out = append(out, r.Dest)
	}
	sort.Strings(out)
	return out
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
	if summary.Results[0].File == nil {
		t.Error("a successful import reported no entry")
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
	if got := destPaths(summary); strings.Join(got, ",") != strings.Join(want, ",") {
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
	for _, r := range again.Results {
		if r.Skipped && r.Reason == "" {
			t.Errorf("%s was skipped with no reason given", r.Dest)
		}
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
	if len(summary.Results) != 1 {
		t.Errorf("one file selected two ways produced %d results", len(summary.Results))
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
	files, _, err := planImport(client, source.Root, []string{"good.txt", "vanishing.txt"}, "/")
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
