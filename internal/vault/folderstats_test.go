package vault

import (
	"context"
	"testing"
)

func statsOf(t *testing.T, v *Vault, dir string) *FolderStats {
	t.Helper()
	s, err := v.FolderStats(MainScope, dir)
	if err != nil {
		t.Fatalf("FolderStats %s: %v", dir, err)
	}
	return s
}

// The whole reason the figures exist: a folder's weight is in the levels below
// it, and its own listing can only ever show one of them.
func TestFolderStatsCountsEverythingBeneath(t *testing.T) {
	v := tree(t,
		"/films/here.mkv",
		"/films/2023/one.mkv",
		"/films/2023/corfu/two.mkv",
		"/photos/elsewhere.jpg",
	)

	s := statsOf(t, v, "/films")
	if s.Files != 3 {
		t.Errorf("counted %d files under /films, want 3", s.Files)
	}
	if s.Folders != 2 {
		t.Errorf("counted %d folders under /films, want 2 (/films/2023 and /films/2023/corfu)", s.Folders)
	}

	// The bytes are the file contents the helper wrote: each file holds its own
	// path, so the total is the three path lengths and nothing from /photos.
	want := int64(len("/films/here.mkv") + len("/films/2023/one.mkv") + len("/films/2023/corfu/two.mkv"))
	if s.Bytes != want {
		t.Errorf("/films weighs %d bytes, want %d", s.Bytes, want)
	}
	if s.Newest == nil {
		t.Error("/films holds three files and reports no newest one")
	}
}

// The root is a folder like any other, and the one place where "everything
// under it" is the whole vault.
func TestFolderStatsAtTheRootCountsTheWholeVault(t *testing.T) {
	v := tree(t, "/films/here.mkv", "/photos/one.jpg")

	s := statsOf(t, v, "/")
	if s.Files != 2 {
		t.Errorf("counted %d files at the root, want 2", s.Files)
	}
	if s.Folders != 2 {
		t.Errorf("counted %d folders at the root, want 2 — the root is not one of them", s.Folders)
	}
}

// What the parts weigh is a different question from what the folder holds, and
// the erasure coding is the difference. A folder that reported one figure for
// both would be reporting the wrong one for whichever question was asked.
func TestFolderStatsWeighsTheStoredPartsSeparately(t *testing.T) {
	v := tree(t, "/films/one.mkv", "/films/two.mkv")

	s := statsOf(t, v, "/films")
	if s.Stored <= s.Bytes {
		t.Errorf("parts weigh %d against %d bytes of files, want more — they are erasure coded", s.Stored, s.Bytes)
	}
}

// One line per account, however many parts of however many files it holds.
func TestFolderStatsNamesEachCloudOnce(t *testing.T) {
	v := tree(t, "/films/one.mkv", "/films/two.mkv", "/films/2023/three.mkv")

	s := statsOf(t, v, "/films")
	if len(s.Clouds) != 3 {
		t.Fatalf("/films lives on %v, want the three accounts once each", s.Clouds)
	}
	seen := map[string]bool{}
	for _, name := range s.Clouds {
		if seen[name] {
			t.Errorf("account %q named twice in %v", name, s.Clouds)
		}
		seen[name] = true
	}
	for _, want := range []string{"cloud-a", "cloud-b", "cloud-c"} {
		if !seen[want] {
			t.Errorf("%v does not name %s", s.Clouds, want)
		}
	}
}

// An empty folder says so in every figure, and names no date. A zero time read
// out as a date is the year 1, which is worse than saying nothing.
func TestFolderStatsOfAnEmptyFolderNamesNoDate(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/empty"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	s := statsOf(t, v, "/empty")
	if s.Files != 0 || s.Folders != 0 || s.Bytes != 0 || s.Stored != 0 {
		t.Errorf("empty folder reports %+v, want every figure zero", s)
	}
	if len(s.Clouds) != 0 {
		t.Errorf("empty folder lives on %v, want no clouds", s.Clouds)
	}
	if s.Newest != nil {
		t.Errorf("empty folder reports a newest file of %v, want none", s.Newest)
	}
}

// A folder that is not there is an error rather than a folder holding nothing:
// the two mean different things, and a menu asking about a folder that has just
// been deleted should not be told it is empty.
func TestFolderStatsRefusesAFolderThatIsNotThere(t *testing.T) {
	v := tree(t, "/films/one.mkv")

	if _, err := v.FolderStats(MainScope, "/nowhere"); err == nil {
		t.Error("FolderStats /nowhere succeeded, want an error")
	}
}

// The name shown is the one the account answers to now. Renaming an account is
// not supposed to leave a folder still naming the old one.
func TestFolderStatsNamesCloudsAsTheyAreNamedNow(t *testing.T) {
	v := tree(t, "/films/one.mkv")
	ctx := context.Background()

	providers, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	name := "the attic"
	if _, err := v.UpdateProvider(ctx, providers[0].ID, ProviderEdit{Name: &name}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	s := statsOf(t, v, "/films")
	for _, cloud := range s.Clouds {
		if cloud == providers[0].Name {
			t.Errorf("%v still names the account %q it had before the rename", s.Clouds, providers[0].Name)
		}
	}
}
