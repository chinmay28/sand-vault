package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// indexedVault returns an unlocked vault whose index holds the given paths. A
// search only ever reads the index, so nothing has to be scattered to test it:
// a path ending in "/" is a folder, anything else a file.
func indexedVault(t *testing.T, paths ...string) *Vault {
	t.Helper()

	m := newManifest()
	for i, p := range paths {
		if strings.HasSuffix(p, "/") {
			if err := m.Mkdir(strings.TrimSuffix(p, "/")); err != nil {
				t.Fatalf("Mkdir %s: %v", p, err)
			}
			continue
		}
		dir, name := splitFolder(CleanDir(p))
		m.add(&Entry{ID: fmt.Sprintf("id-%d", i), Dir: dir, Name: name, Size: int64(i)})
	}
	return &Vault{manifest: m, dataKey: []byte("unlocked")}
}

func search(t *testing.T, v *Vault, opts SearchOptions) *SearchResults {
	t.Helper()

	res, err := v.Search(opts)
	if err != nil {
		t.Fatalf("Search(%+v): %v", opts, err)
	}
	return res
}

// hitPaths flattens a result set into the paths it found, in order.
func hitPaths(res *SearchResults) []string {
	out := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		out = append(out, hit.Path)
	}
	return out
}

func assertPaths(t *testing.T, res *SearchResults, want ...string) {
	t.Helper()

	got := hitPaths(res)
	if len(got) != len(want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("found %v, want %v", got, want)
		}
	}
}

func TestSearchFindsFilesAndFolders(t *testing.T) {
	v := indexedVault(t,
		"/taxes/",
		"/taxes/tax-return-2023.pdf",
		"/photos/holiday.jpg",
		"/notes.txt",
	)

	res := search(t, v, SearchOptions{Query: "TAX"})
	assertPaths(t, res, "/taxes", "/taxes/tax-return-2023.pdf")
}

func TestSearchMatchesAnywhereInTheName(t *testing.T) {
	v := indexedVault(t, "/report-final.pdf", "/final.pdf", "/draft.txt")

	res := search(t, v, SearchOptions{Query: "final"})
	// Exact name first, then the one that merely contains it.
	assertPaths(t, res, "/final.pdf", "/report-final.pdf")
}

func TestSearchRanksExactThenPrefixThenSubstring(t *testing.T) {
	v := indexedVault(t, "/a/my-notes.txt", "/notes.txt.bak", "/notes", "/notes-old.txt")

	res := search(t, v, SearchOptions{Query: "notes"})
	assertPaths(t, res,
		"/notes",          // exact
		"/notes-old.txt",  // prefix
		"/notes.txt.bak",  // prefix
		"/a/my-notes.txt", // substring, and one folder deeper
	)
}

func TestSearchPrefersShallowerPaths(t *testing.T) {
	v := indexedVault(t, "/a/b/c/deep.txt", "/shallow.txt", "/a/mid.txt")

	res := search(t, v, SearchOptions{Query: ".txt"})
	assertPaths(t, res, "/shallow.txt", "/a/mid.txt", "/a/b/c/deep.txt")
}

func TestSearchScopesToASubtree(t *testing.T) {
	v := indexedVault(t,
		"/work/report.pdf",
		"/work/2024/report.pdf",
		"/personal/report.pdf",
	)

	res := search(t, v, SearchOptions{Query: "report", Dir: "/work"})
	assertPaths(t, res, "/work/report.pdf", "/work/2024/report.pdf")
	if res.Scope != "/work" {
		t.Errorf("Scope = %q, want /work", res.Scope)
	}
}

func TestSearchScopeExcludesTheFolderItself(t *testing.T) {
	v := indexedVault(t, "/work/", "/work/work-log.txt")

	res := search(t, v, SearchOptions{Query: "work", Dir: "/work"})
	assertPaths(t, res, "/work/work-log.txt")
}

func TestSearchRejectsAScopeThatDoesNotExist(t *testing.T) {
	v := indexedVault(t, "/notes.txt")

	_, err := v.Search(SearchOptions{Query: "notes", Dir: "/nowhere"})
	if err == nil || !strings.Contains(err.Error(), "no such folder") {
		t.Fatalf("Search in a missing folder = %v, want a no-such-folder error", err)
	}
}

func TestSearchKindNarrowsTheResults(t *testing.T) {
	v := indexedVault(t, "/backup/", "/backup/backup.tar")

	files := search(t, v, SearchOptions{Query: "backup", Kind: SearchFiles})
	assertPaths(t, files, "/backup/backup.tar")

	folders := search(t, v, SearchOptions{Query: "backup", Kind: SearchFolders})
	assertPaths(t, folders, "/backup")

	if _, err := v.Search(SearchOptions{Query: "backup", Kind: "files"}); err == nil {
		t.Error("an unknown kind should be rejected")
	}
}

func TestSearchWildcardsMatchWholeNames(t *testing.T) {
	v := indexedVault(t, "/holiday.jpg", "/holiday.jpeg", "/a/beach.jpg", "/jpg-notes.txt")

	res := search(t, v, SearchOptions{Query: "*.jpg"})
	assertPaths(t, res, "/holiday.jpg", "/a/beach.jpg")

	single := search(t, v, SearchOptions{Query: "holiday.jpe?"})
	assertPaths(t, single, "/holiday.jpeg")
}

func TestSearchWildcardsAreNotRegularExpressions(t *testing.T) {
	v := indexedVault(t, "/report(final).pdf", "/reportXfinalY.pdf")

	res := search(t, v, SearchOptions{Query: "report(*)*"})
	assertPaths(t, res, "/report(final).pdf")
}

func TestSearchOnAPathMatchesTheWholePath(t *testing.T) {
	v := indexedVault(t,
		"/photos/2024/beach.jpg",
		"/photos/beach.jpg",
		"/scans/beach.jpg",
	)

	res := search(t, v, SearchOptions{Query: "photos/beach"})
	assertPaths(t, res, "/photos/beach.jpg")

	glob := search(t, v, SearchOptions{Query: "photos/*/*.jpg"})
	assertPaths(t, glob, "/photos/2024/beach.jpg")
}

func TestSearchFindsFoldersImpliedByStoredFiles(t *testing.T) {
	// No folder was ever created explicitly here — /a and /a/deep exist only
	// because something was stored inside them.
	v := indexedVault(t, "/a/deep/file.txt")

	res := search(t, v, SearchOptions{Query: "deep", Kind: SearchFolders})
	assertPaths(t, res, "/a/deep")
}

func TestSearchReportsEachFoldersParent(t *testing.T) {
	v := indexedVault(t, "/a/deep/file.txt")

	res := search(t, v, SearchOptions{Query: "deep"})
	if len(res.Hits) != 1 {
		t.Fatalf("expected one hit, got %v", hitPaths(res))
	}
	hit := res.Hits[0]
	if hit.Dir != "/a" || hit.Name != "deep" || hit.Type != "folder" {
		t.Errorf("hit = %+v, want a folder named deep in /a", hit)
	}
	if hit.File != nil {
		t.Error("a folder hit should not carry a file entry")
	}
}

func TestSearchCarriesTheIndexEntryForFiles(t *testing.T) {
	v := indexedVault(t, "/notes.txt")

	res := search(t, v, SearchOptions{Query: "notes"})
	if len(res.Hits) != 1 || res.Hits[0].File == nil {
		t.Fatalf("expected one file hit carrying its entry, got %+v", res.Hits)
	}
	if res.Hits[0].File.Name != "notes.txt" {
		t.Errorf("entry name = %q, want notes.txt", res.Hits[0].File.Name)
	}
}

func TestSearchLimitTruncatesButStillCounts(t *testing.T) {
	paths := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		paths = append(paths, fmt.Sprintf("/log-%02d.txt", i))
	}
	v := indexedVault(t, paths...)

	res := search(t, v, SearchOptions{Query: "log", Limit: 5})
	if len(res.Hits) != 5 {
		t.Fatalf("got %d hits, want 5", len(res.Hits))
	}
	if !res.Truncated {
		t.Error("a capped search should report that it was truncated")
	}
	if res.Matched != 20 {
		t.Errorf("Matched = %d, want 20", res.Matched)
	}
	// The cap keeps the best matches, which is only true if it is applied
	// after the ranking.
	if res.Hits[0].Path != "/log-00.txt" {
		t.Errorf("first hit = %q, want /log-00.txt", res.Hits[0].Path)
	}
}

func TestSearchWithNoMatchesIsEmptyRatherThanAnError(t *testing.T) {
	v := indexedVault(t, "/notes.txt")

	res := search(t, v, SearchOptions{Query: "nothing-like-this"})
	if len(res.Hits) != 0 || res.Truncated {
		t.Errorf("expected no hits, got %+v", res)
	}
}

func TestSearchNeedsSomethingToLookFor(t *testing.T) {
	v := indexedVault(t, "/notes.txt")

	for _, query := range []string{"", "   "} {
		if _, err := v.Search(SearchOptions{Query: query}); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("Search(%q) = %v, want ErrEmptyQuery", query, err)
		}
	}
}

func TestSearchNeedsAnUnlockedVault(t *testing.T) {
	v := indexedVault(t, "/notes.txt")
	v.dataKey = nil

	if _, err := v.Search(SearchOptions{Query: "notes"}); !errors.Is(err, ErrLocked) {
		t.Errorf("searching a locked vault = %v, want ErrLocked", err)
	}
}

func TestSearchFindsWhatWasActuallyUploaded(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdir(MainScope, "/receipts"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(context.Background(), MainScope, "/receipts", "coffee.txt", []byte("2.80"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	res := search(t, v, SearchOptions{Query: "coffee"})
	if len(res.Hits) != 1 {
		t.Fatalf("expected one hit, got %v", hitPaths(res))
	}
	hit := res.Hits[0]
	if hit.Path != "/receipts/coffee.txt" {
		t.Errorf("Path = %q, want /receipts/coffee.txt", hit.Path)
	}
	// The hit carries enough to draw a row: size and where the parts went.
	if hit.File == nil || hit.File.ID != entry.ID || len(hit.File.Shards) != 3 {
		t.Errorf("hit entry = %+v, want the uploaded entry with three shards", hit.File)
	}
}
