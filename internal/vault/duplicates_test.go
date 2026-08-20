package vault

import (
	"context"
	"strconv"
	"testing"
)

// grow stores a file at a full path with the exact bytes given, so a test can
// say which two files are the same file and which two merely weigh the same.
func grow(t *testing.T, v *Vault, full string, body []byte) {
	t.Helper()

	dir, name := CleanDir(full[:lastSlash(full)]), full[lastSlash(full)+1:]
	if err := v.Mkdir(MainScope, dir); err != nil {
		t.Fatalf("Mkdir %s: %v", dir, err)
	}
	if _, _, err := v.Upload(context.Background(), MainScope, dir, name, body, UploadOptions{}); err != nil {
		t.Fatalf("Upload %s: %v", full, err)
	}
}

func duplicatesOf(t *testing.T, v *Vault, dir string) *Duplicates {
	t.Helper()

	d, err := v.Duplicates(MainScope, dir)
	if err != nil {
		t.Fatalf("Duplicates %s: %v", dir, err)
	}
	return d
}

// names lists what a group holds, in the order the group offers it — the
// suggested survivor first.
func names(g DuplicateGroup) []string {
	out := make([]string, 0, len(g.Files))
	for _, f := range g.Files {
		out = append(out, f.Dir+"/"+f.Name)
	}
	return out
}

func only(t *testing.T, set DuplicateSet) DuplicateGroup {
	t.Helper()

	if len(set.Groups) != 1 {
		t.Fatalf("found %d groups, want exactly 1: %+v", len(set.Groups), set.Groups)
	}
	return set.Groups[0]
}

// The whole point of the content question: the same bytes are the same file,
// wherever they are and whatever they are called.
func TestDuplicatesFindsTheSameBytesUnderDifferentNames(t *testing.T) {
	v, _ := newTestVault(t, 3)
	body := []byte("the same photograph, twice")
	grow(t, v, "/photos/corfu.jpg", body)
	grow(t, v, "/photos/2023/phone/DSC_9912.jpg", body)

	g := only(t, duplicatesOf(t, v, "/photos").Content)
	if !g.Certain {
		t.Error("two files with one hash are not reported as certain")
	}
	if len(g.Files) != 2 {
		t.Fatalf("group holds %d files, want 2", len(g.Files))
	}
	// The shallowest copy is the one suggested for keeping, and it leads.
	if got := names(g); got[0] != "/photos/corfu.jpg" {
		t.Errorf("group keeps %s, want the copy already in the folder", got[0])
	}
	if !g.Files[0].Keep || g.Files[1].Keep {
		t.Error("the group marks the wrong copy to keep")
	}
	if g.Waste != int64(len(body)) {
		t.Errorf("removing the extra copy frees %d bytes, want %d", g.Waste, len(body))
	}
}

// Same length, different bytes. It is the question somebody asks out loud and
// the one that can be wrong, so the answer has to say which it is.
func TestDuplicatesSeparatesSameSizeFromSameFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/docs/one.txt", []byte("aaaaaaaaaa"))
	grow(t, v, "/docs/two.txt", []byte("bbbbbbbbbb"))

	d := duplicatesOf(t, v, "/docs")
	if len(d.Content.Groups) != 0 {
		t.Errorf("two different files were reported as the same content: %+v", d.Content.Groups)
	}

	g := only(t, d.Size)
	if g.Certain {
		t.Error("a size match with two different hashes is reported as certain")
	}
	if len(g.Files) != 2 {
		t.Errorf("size group holds %d files, want 2", len(g.Files))
	}
}

// A file of nothing is the same nothing as every other, which says nothing
// about what a folder is holding — so the loose question leaves them out, and
// the one that can prove it keeps them.
func TestDuplicatesLeavesEmptyFilesOutOfTheSizeQuestion(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/empties/a.log", nil)
	grow(t, v, "/empties/b.log", nil)

	d := duplicatesOf(t, v, "/empties")
	if len(d.Size.Groups) != 0 {
		t.Errorf("empty files were grouped by size: %+v", d.Size.Groups)
	}
	if len(d.Content.Groups) != 1 {
		t.Errorf("empty files are the same bytes and should still group by content: %+v", d.Content.Groups)
	}
}

// The names a browser and a file manager actually produce.
func TestDuplicatesFindsTheMarksACopyIsMadeWith(t *testing.T) {
	v, _ := newTestVault(t, 3)
	for i, name := range []string{"report.pdf", "report (1).pdf", "report (2).pdf", "report - Copy.pdf", "Report_copy.pdf"} {
		grow(t, v, "/papers/"+name, []byte{byte(i)})
	}

	g := only(t, duplicatesOf(t, v, "/papers").Name)
	if len(g.Files) != 5 {
		t.Fatalf("found %d names alike, want all 5: %v", len(g.Files), names(g))
	}
	if g.Files[0].Name != "report.pdf" {
		t.Errorf("the group keeps %q, want the name without a mark on it", g.Files[0].Name)
	}
	if g.Certain {
		t.Error("five different files are reported as certainly identical")
	}
}

// The trap the whole reduction exists to avoid: two photographs in sequence are
// one character apart and are not copies of anything.
func TestDuplicatesDoesNotCallConsecutiveNumbersCopies(t *testing.T) {
	v, _ := newTestVault(t, 3)
	for i, name := range []string{"IMG_0001.jpg", "IMG_0002.jpg", "IMG_0003.jpg", "Holiday 2023.mkv", "Holiday 2024.mkv"} {
		grow(t, v, "/camera/"+name, []byte{byte(i)})
	}

	if groups := duplicatesOf(t, v, "/camera").Name.Groups; len(groups) != 0 {
		t.Errorf("numbered files were called copies of each other: %+v", groups)
	}
}

// How a name was typed is not what it says.
func TestDuplicatesIgnoresCaseAndSeparators(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/mix/Vacation Photo.jpg", []byte("a"))
	grow(t, v, "/mix/copies/vacation-photo.jpg", []byte("bb"))
	grow(t, v, "/mix/deeper/still/VACATION_PHOTO.jpg", []byte("ccc"))

	g := only(t, duplicatesOf(t, v, "/mix").Name)
	if len(g.Files) != 3 {
		t.Fatalf("found %d, want 3: %v", len(g.Files), names(g))
	}
	if g.Files[0].Depth != 0 {
		t.Errorf("the group keeps a copy %d folders down, want the shallowest", g.Files[0].Depth)
	}
}

// A typo is a minimal difference too, which is the other half of what "similar
// names" means.
func TestDuplicatesJoinsNamesAFewEditsApart(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/talks/presentation notes.txt", []byte("a"))
	grow(t, v, "/talks/presentaton notes.txt", []byte("bb"))
	// Short names are left alone: at three letters every word is one edit from
	// another word, and "cat" is not a copy of "bat".
	grow(t, v, "/talks/cat.txt", []byte("ccc"))
	grow(t, v, "/talks/bat.txt", []byte("dddd"))

	g := only(t, duplicatesOf(t, v, "/talks").Name)
	if len(g.Files) != 2 {
		t.Fatalf("found %d, want the two spellings of one name: %v", len(g.Files), names(g))
	}
}

// A film and its subtitles share a stem and are the opposite of duplicates.
func TestDuplicatesNeverGroupsAcrossExtensions(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/films/Corfu.mkv", []byte("a"))
	grow(t, v, "/films/Corfu.srt", []byte("bb"))

	if groups := duplicatesOf(t, v, "/films").Name.Groups; len(groups) != 0 {
		t.Errorf("a film was grouped with its subtitles: %+v", groups)
	}
}

// What the buttons are counted from.
func TestDuplicatesCountsWhatWouldGoAndWhatItComesTo(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ten, twenty := make([]byte, 10), make([]byte, 20)
	grow(t, v, "/tally/a.bin", ten)
	grow(t, v, "/tally/one/a.bin", ten)
	grow(t, v, "/tally/two/a.bin", ten)
	grow(t, v, "/tally/b.bin", twenty)
	grow(t, v, "/tally/one/b.bin", twenty)
	grow(t, v, "/tally/lonely.bin", []byte("only one of these"))

	d := duplicatesOf(t, v, "/tally")
	if d.Scanned != 6 {
		t.Errorf("scanned %d files, want 6", d.Scanned)
	}
	if len(d.Content.Groups) != 2 {
		t.Fatalf("found %d groups, want 2 — the lonely file is not a duplicate of anything", len(d.Content.Groups))
	}
	// Most reclaimable first: two spare copies of ten bytes beat one of twenty.
	if d.Content.Groups[0].Waste != 20 || d.Content.Groups[1].Waste != 20 {
		t.Errorf("groups waste %d and %d, want 20 each", d.Content.Groups[0].Waste, d.Content.Groups[1].Waste)
	}
	if d.Content.Files != 5 || d.Content.Extra != 3 {
		t.Errorf("%d files in groups of which %d spare, want 5 and 3", d.Content.Files, d.Content.Extra)
	}
	if d.Content.Waste != 40 {
		t.Errorf("clearing the spares frees %d bytes, want 40", d.Content.Waste)
	}
}

// Every file with a hash is grouped by it; a file stored before the vault kept
// one is not grouped with the other files that have none, because "we do not
// know what either of these is" is not a reason to put them together.
func TestDuplicatesByContentSkipsFilesWithNoHash(t *testing.T) {
	files := []DuplicateFile{
		{ID: "1", Name: "a", Size: 10},
		{ID: "2", Name: "b", Size: 10},
		{ID: "3", Name: "c", Size: 10, Hash: "abc"},
		{ID: "4", Name: "d", Size: 10, Hash: "abc"},
	}

	content := byContent(files)
	if len(content.Groups) != 1 || len(content.Groups[0].Files) != 2 {
		t.Fatalf("byContent grouped %+v, want just the pair that has a hash", content.Groups)
	}

	// The size question takes all four, and reports that it cannot prove it.
	size := bySize(files)
	if len(size.Groups) != 1 || len(size.Groups[0].Files) != 4 {
		t.Fatalf("bySize grouped %+v, want all four", size.Groups)
	}
	if size.Groups[0].Certain {
		t.Error("a size group holding files with no hash is reported as certain")
	}
}

// A group is certain when its files are provably one file, whichever question
// found them — including the name question, which is otherwise a guess.
func TestDuplicatesNameGroupSaysWhenTheBytesAgree(t *testing.T) {
	v, _ := newTestVault(t, 3)
	body := []byte("the same thing under two names")
	grow(t, v, "/same/notes.txt", body)
	grow(t, v, "/same/backup/notes (1).txt", body)

	if g := only(t, duplicatesOf(t, v, "/same").Name); !g.Certain {
		t.Error("a name group whose files share one hash does not say so")
	}
}

func TestDuplicatesRefusesAFolderThatIsNotThere(t *testing.T) {
	v := tree(t, "/a/one.txt")

	if _, err := v.Duplicates(MainScope, "/nowhere"); err == nil {
		t.Fatal("looking for duplicates in a folder that does not exist succeeded")
	}
}

func TestDuplicatesRefusesALockedVault(t *testing.T) {
	v := tree(t, "/a/one.txt")
	v.Lock()

	if _, err := v.Duplicates(MainScope, "/"); err == nil {
		t.Fatal("looking for duplicates in a locked vault succeeded")
	}
}

func TestNameKeyReducesANameToWhatACopyWouldShare(t *testing.T) {
	same := [][2]string{
		{"report.pdf", "report (1).pdf"},
		{"report.pdf", "Report - Copy.pdf"},
		{"report.pdf", "report copy 2.pdf"},
		{"report.pdf", "report (1) (2).pdf"},
		{"IMG_0001.jpg", "img 0001.jpg"},
		{"Vacation Photo.jpg", "vacation-photo.jpg"},
		{"holiday 2023 12.mkv", "holiday2023-12.mkv"},
	}
	for _, pair := range same {
		if nameKey(pair[0]) != nameKey(pair[1]) {
			t.Errorf("%q and %q reduce differently: %q vs %q",
				pair[0], pair[1], nameKey(pair[0]), nameKey(pair[1]))
		}
	}

	differ := [][2]string{
		{"IMG_0001.jpg", "IMG_0002.jpg"},
		{"holiday 2023.mkv", "holiday 2024.mkv"},
		{"a1b2.txt", "a12b.txt"},
		// The word has to be a word: a photocopy is not a copy of a photo.
		{"photocopy.pdf", "photo.pdf"},
		{"film.mkv", "film.srt"},
		// A file actually called "copy" keeps its name rather than being
		// reduced to nothing.
		{"copy.txt", "notes.txt"},
	}
	for _, pair := range differ {
		if nameKey(pair[0]) == nameKey(pair[1]) {
			t.Errorf("%q and %q reduce to the same key %q", pair[0], pair[1], nameKey(pair[0]))
		}
	}
}

func TestWithinEditsStaysInsideItsAllowance(t *testing.T) {
	cases := []struct {
		a, b    string
		allowed int
		want    bool
	}{
		{"presentation", "presentaton", 1, true},
		{"presentation", "presentatons", 1, false},
		{"presentation", "presentatons", 2, true},
		{"cat", "bat", 0, false},
		{"cat", "cat", 0, true},
		{"", "", 0, true},
		{"abcdefgh", "abcdefghij", 1, false},
	}
	for _, c := range cases {
		if got := withinEdits([]rune(c.a), []rune(c.b), c.allowed); got != c.want {
			t.Errorf("withinEdits(%q, %q, %d) = %v, want %v", c.a, c.b, c.allowed, got, c.want)
		}
	}
}

// Similarity chains. In a folder of machine-made names every one is a letter
// from a dozen others, and joining them transitively would offer the whole
// folder as one group of duplicates. A run that wide is a naming scheme, and
// what survives it is the names that matched exactly.
func TestDuplicatesBreaksUpARunOfNamesTooAlikeToBeCopies(t *testing.T) {
	files := []DuplicateFile{}
	for i, stem := range []string{"aaqrstuv", "abqrstuv", "acqrstuv", "adqrstuv", "aeqrstuv", "afqrstuv", "agqrstuv", "ahqrstuv", "aiqrstuv", "ajqrstuv"} {
		files = append(files, DuplicateFile{ID: stem, Name: stem + ".txt", Size: int64(i + 1)})
	}
	// Except for one pair that really is a copy, which has to come through it.
	files = append(files,
		DuplicateFile{ID: "keep", Name: "quarterly summary.pdf", Size: 100},
		DuplicateFile{ID: "copy", Name: "quarterly summary (1).pdf", Size: 100},
	)

	set := byName(files)
	if set.Crowded != 1 {
		t.Errorf("%d runs were called too wide, want 1", set.Crowded)
	}
	if len(set.Groups) != 1 {
		t.Fatalf("found %d groups, want only the real copy: %+v", len(set.Groups), set.Groups)
	}
	if len(set.Groups[0].Files) != 2 || set.Groups[0].Files[0].ID != "keep" {
		t.Errorf("the surviving group is %+v, want the quarterly summary and its copy", set.Groups[0].Files)
	}
}

// The cap is on chains of different names, never on how many times one name
// repeats: "report (1).pdf" through "report (40).pdf" is one name forty times.
func TestDuplicatesKeepsALongRunOfTheSameNameWhole(t *testing.T) {
	files := []DuplicateFile{{ID: "0", Name: "report.pdf", Size: 10}}
	for i := 1; i <= 40; i++ {
		files = append(files, DuplicateFile{
			ID:   strconv.Itoa(i),
			Name: "report (" + strconv.Itoa(i) + ").pdf",
			Size: 10,
		})
	}

	set := byName(files)
	if len(set.Groups) != 1 || len(set.Groups[0].Files) != 41 {
		t.Fatalf("found %+v, want one group of 41", set.Groups)
	}
	if set.Crowded != 0 {
		t.Errorf("one name repeated forty times was called a naming scheme")
	}
}
