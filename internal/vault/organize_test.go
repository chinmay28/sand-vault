package vault

import (
	"context"
	"testing"
)

// tree stores a file at each of the given full paths, creating the folders
// above them, and returns the vault it built them in.
func tree(t *testing.T, paths ...string) *Vault {
	t.Helper()
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	for _, full := range paths {
		dir, name := CleanDir(full[:lastSlash(full)]), full[lastSlash(full)+1:]
		if err := v.Mkdir(MainScope, dir); err != nil {
			t.Fatalf("Mkdir %s: %v", dir, err)
		}
		if _, _, err := v.Upload(ctx, MainScope, dir, name, []byte(full), UploadOptions{}); err != nil {
			t.Fatalf("Upload %s: %v", full, err)
		}
	}
	return v
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func surveyOf(t *testing.T, v *Vault, dir string) *Survey {
	t.Helper()
	s, err := v.Survey(MainScope, dir)
	if err != nil {
		t.Fatalf("Survey %s: %v", dir, err)
	}
	return s
}

func folder(t *testing.T, s *Survey, path string) SurveyFolder {
	t.Helper()
	for _, f := range s.Folders {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("survey of %s has no folder %s", s.Path, path)
	return SurveyFolder{}
}

// The whole point of the answer: how deep each file sits, which is what a
// flatten moves on and what "this folder" versus "everything under it" means
// everywhere else.
func TestSurveyReportsHowDeepEachFileSits(t *testing.T) {
	v := tree(t,
		"/films/here.mkv",
		"/films/2023/one.mkv",
		"/films/2023/corfu/two.mkv",
	)

	s := surveyOf(t, v, "/films")
	if len(s.Files) != 3 {
		t.Fatalf("survey found %d files, want 3", len(s.Files))
	}

	depths := map[string]int{}
	for _, f := range s.Files {
		depths[f.Name] = f.Depth
	}
	for name, want := range map[string]int{"here.mkv": 0, "one.mkv": 1, "two.mkv": 2} {
		if depths[name] != want {
			t.Errorf("%s is %d folders down, want %d", name, depths[name], want)
		}
	}

	// Shallowest first, so a flatten moves the file that keeps its name before
	// the deeper one that has to be numbered around it.
	if s.Files[0].Name != "here.mkv" {
		t.Errorf("survey leads with %s, want the file already in the folder", s.Files[0].Name)
	}
}

// A folder is only safe to remove when nothing is under it at all — not merely
// when nothing is directly in it.
func TestSurveyCountsWhatIsBelowAFolderAndNotJustInIt(t *testing.T) {
	v := tree(t, "/a/b/c/deep.txt")
	if err := v.Mkdir(MainScope, "/a/empty/emptier"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	s := surveyOf(t, v, "/a")

	b := folder(t, s, "/a/b")
	if b.Files != 0 {
		t.Errorf("/a/b holds %d files directly, want 0", b.Files)
	}
	if b.Total != 1 {
		t.Errorf("/a/b holds %d files below it, want 1 — removing it would take one", b.Total)
	}

	for _, path := range []string{"/a/empty", "/a/empty/emptier"} {
		if f := folder(t, s, path); f.Total != 0 {
			t.Errorf("%s counts %d files below it, want 0", path, f.Total)
		}
	}
}

// A folder nobody has put anything in is exactly what the empty-folder tool is
// looking for, so it has to be in the answer at all.
func TestSurveyListsFoldersNoFileHasEverBeenPutIn(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/plans/2027"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	s := surveyOf(t, v, "/plans")
	if len(s.Folders) != 1 || s.Folders[0].Path != "/plans/2027" {
		t.Fatalf("survey of an empty tree found %+v, want /plans/2027", s.Folders)
	}
	if len(s.Files) != 0 {
		t.Errorf("survey of an empty tree found %d files", len(s.Files))
	}
}

// The folder being organized is never one of the things an organizer may
// remove, so it is not among the folders it is offered.
func TestSurveyLeavesOutTheFolderBeingSurveyed(t *testing.T) {
	v := tree(t, "/a/b/one.txt")

	for _, f := range surveyOf(t, v, "/a").Folders {
		if f.Path == "/a" {
			t.Fatal("the surveyed folder is listed among the folders under it")
		}
	}
	// And from the root, which is the one folder that can never be removed at
	// all, every folder in the vault is under it.
	if len(surveyOf(t, v, "/").Folders) != 2 {
		t.Errorf("survey of the root found %d folders, want /a and /a/b", len(surveyOf(t, v, "/").Folders))
	}
}

func TestSurveyGroupsFilesByExtension(t *testing.T) {
	v := tree(t,
		"/mixed/film.MKV",
		"/mixed/subs/film.srt",
		"/mixed/README",
		"/mixed/.hidden",
	)

	kinds := map[string]int{}
	for _, f := range surveyOf(t, v, "/mixed").Files {
		kinds[f.Ext]++
	}

	// Lowercased, so .MKV and .mkv are one kind to tick rather than two.
	if kinds[".mkv"] != 1 {
		t.Errorf("found %d .mkv files, want 1 — an upper-case extension is the same kind", kinds[".mkv"])
	}
	if kinds[".srt"] != 1 {
		t.Errorf("found %d .srt files, want 1", kinds[".srt"])
	}
	// A name with no extension and a name that is nothing but one are both
	// "no extension": .hidden is a hidden file, not a hidden-file file.
	if kinds[""] != 2 {
		t.Errorf("found %d files with no extension, want 2 (README and .hidden)", kinds[""])
	}
}

func TestSurveyCountsWhatAFolderComesTo(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	if err := v.Mkdir(MainScope, "/big/inner"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, size := range []int{100, 250} {
		body := make([]byte, size)
		name := "f" + string(rune('a'+size%26)) + ".bin"
		if _, _, err := v.Upload(ctx, MainScope, "/big/inner", name, body, UploadOptions{}); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	}

	inner := folder(t, surveyOf(t, v, "/big"), "/big/inner")
	if inner.Bytes != 350 {
		t.Errorf("/big/inner comes to %d bytes, want 350", inner.Bytes)
	}
	if inner.Files != 2 || inner.Total != 2 {
		t.Errorf("/big/inner holds %d directly and %d below, want 2 and 2", inner.Files, inner.Total)
	}
}

func TestSurveyRefusesAFolderThatIsNotThere(t *testing.T) {
	v := tree(t, "/a/one.txt")

	if _, err := v.Survey(MainScope, "/nowhere"); err == nil {
		t.Fatal("surveying a folder that does not exist succeeded")
	}
}

// A locked vault knows nothing, and a survey is a read of the index like any
// other.
func TestSurveyRefusesALockedVault(t *testing.T) {
	v := tree(t, "/a/one.txt")
	v.Lock()

	if _, err := v.Survey(MainScope, "/"); err == nil {
		t.Fatal("surveying a locked vault succeeded")
	}
}

func TestExtensionIsTheKindAndNotTheDot(t *testing.T) {
	for name, want := range map[string]string{
		"film.mkv":      ".mkv",
		"FILM.MKV":      ".mkv",
		"a.tar.gz":      ".gz",
		"README":        "",
		".gitignore":    "",
		"trailing.":     "",
		"in a folder/x": "",
	} {
		if got := Extension(name); got != want {
			t.Errorf("Extension(%q) = %q, want %q", name, got, want)
		}
	}
}
