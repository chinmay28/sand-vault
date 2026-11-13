package vault

import (
	"testing"

	"github.com/chinmay28/SandHarbor/internal/movie"
)

// filmsOf builds the inputs byFilm takes from names alone, with sizes rising
// in the order given — so a test can say which copy is the big one.
func filmsOf(dirsAndNames ...[2]string) ([]DuplicateFile, []filmClue) {
	files := make([]DuplicateFile, 0, len(dirsAndNames))
	clues := make([]filmClue, 0, len(dirsAndNames))
	for i, dn := range dirsAndNames {
		e := &Entry{ID: dn[1] + "#" + dn[0], Name: dn[1], Dir: dn[0], Size: int64(100 * (i + 1))}
		files = append(files, DuplicateFile{ID: e.ID, Name: e.Name, Dir: e.Dir, Size: e.Size, Ext: Extension(e.Name)})
		clues = append(clues, filmClueOf(e, nil))
	}
	return files, clues
}

// The pair this question exists for: a scene release and a tidy name of one
// film, which the name question is right to keep apart and this one has to
// bring together — through the vault, the way the browser asks.
func TestDuplicatesFindsOneFilmUnderAReleaseNameAndATidyOne(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/Movies/unsorted/The.Lord.of.the.Rings.The.Two.Towers.2002.EXTENDED.PROPER.1080p.BluRay.H264.AAC-LAMA.mkv", make([]byte, 4500))
	grow(t, v, "/Movies/imported/The Lord of the Rings - The Two Towers (2002).mkv", make([]byte, 797))
	grow(t, v, "/Movies/unsorted/The.Lord.of.the.Rings.The.Return.of.the.King.2003.EXTENDED.1080p.BluRay.x264-LAMA.mkv", make([]byte, 4100))
	grow(t, v, "/Movies/imported/The Lord of the Rings - The Return of the King (2003).mkv", make([]byte, 893))
	grow(t, v, "/Movies/unsorted/overlord-(2018)-720p-bluray.mkv", make([]byte, 1100))

	d := duplicatesOf(t, v, "/Movies")
	if d.Videos != 5 {
		t.Errorf("asked the film question of %d files, want 5", d.Videos)
	}
	if len(d.Name.Groups) != 0 {
		t.Errorf("the name question grouped names that share nothing: %+v", d.Name.Groups)
	}
	if len(d.Film.Groups) != 2 {
		t.Fatalf("found %d films, want the two with a copy each: %+v", len(d.Film.Groups), d.Film.Groups)
	}

	// Most reclaimable first — the King's small copy is the bigger spare — and
	// in each, the big encode is the one worth keeping.
	if got := d.Film.Groups[0].Title; got != "The Lord of the Rings The Return of the King (2003)" {
		t.Errorf("the first group is called %q", got)
	}
	g := d.Film.Groups[1]
	if g.Title != "The Lord of the Rings The Two Towers (2002)" {
		t.Errorf("the second group is called %q", g.Title)
	}
	if len(g.Files) != 2 {
		t.Fatalf("group holds %v, want both copies", names(g))
	}
	if !g.Files[0].Keep || g.Files[0].Size != 4500 {
		t.Errorf("the group keeps %s (%d bytes), want the larger encode", g.Files[0].Name, g.Files[0].Size)
	}
	if g.Certain {
		t.Error("two different encodes are reported as certainly identical")
	}
	if g.Waste != 797 {
		t.Errorf("removing the small copy frees %d bytes, want 797", g.Waste)
	}
	if d.Film.Extra != 2 || d.Film.Waste != 797+893 {
		t.Errorf("the set counts %d spare / %d bytes, want 2 / %d", d.Film.Extra, d.Film.Waste, 797+893)
	}
}

// A stored match is the strongest clue there is: two files with one database
// id are one film whatever they are called, and a matched file answers to
// its stored title as well as its own name's — so a matched copy still meets
// an unmatched one.
func TestDuplicatesJoinsFilmsThroughTheStoredMatch(t *testing.T) {
	v, _ := newTestVault(t, 3)
	grow(t, v, "/films/ttt-extended.mkv", make([]byte, 400))
	grow(t, v, "/films/two.towers.mkv", make([]byte, 300))
	grow(t, v, "/films/The Lord of the Rings - The Two Towers (2002).mp4", make([]byte, 200))
	grow(t, v, "/films/Alien.1979.mkv", make([]byte, 100))

	towers := &movie.Info{TMDBID: 121, Title: "The Lord of the Rings: The Two Towers", Year: 2002}
	for _, name := range []string{"ttt-extended.mkv", "two.towers.mkv"} {
		e, err := v.EntryByPath(MainScope, "/films/"+name)
		if err != nil {
			t.Fatalf("%s was not stored: %v", name, err)
		}
		if err := v.SetMovie(e.ID, towers); err != nil {
			t.Fatalf("SetMovie: %v", err)
		}
	}

	g := only(t, duplicatesOf(t, v, "/films").Film)
	if len(g.Files) != 3 {
		t.Fatalf("group holds %v, want the two matched copies and the tidy name", names(g))
	}
	if g.Title != "The Lord of the Rings: The Two Towers (2002)" {
		t.Errorf("a group with a matched copy is called %q, want the database's title", g.Title)
	}
	if g.Files[0].Name != "ttt-extended.mkv" {
		t.Errorf("the group keeps %s, want the largest", g.Files[0].Name)
	}
}

// A name without a year beside a name with one is the same film when the
// title has been seen with one year only. Beside two years it is a question
// nobody can answer, and it stays out of both.
func TestDuplicatesLendsAYearToANameWithoutOne(t *testing.T) {
	files, clues := filmsOf([2]string{"/d", "Dune.mkv"}, [2]string{"/d", "Dune.2021.1080p.mkv"})
	g := only(t, byFilm(files, clues))
	if len(g.Files) != 2 {
		t.Errorf("Dune and Dune (2021) were not called one film: %v", names(g))
	}

	files, clues = filmsOf([2]string{"/d", "Dune.mkv"}, [2]string{"/d", "Dune.2021.1080p.mkv"}, [2]string{"/d", "Dune.1984.mkv"})
	if set := byFilm(files, clues); len(set.Groups) != 0 {
		t.Errorf("a yearless Dune was handed to one of two Dunes: %+v", set.Groups)
	}
}

// Two films in one year are two films, and a sequel is not its original.
func TestDuplicatesKeepsDifferentFilmsApart(t *testing.T) {
	files, clues := filmsOf(
		[2]string{"/f", "Heat.1995.mkv"}, [2]string{"/f", "Se7en.1995.mkv"},
		[2]string{"/f", "Alien.1979.mkv"}, [2]string{"/f", "Aliens.1986.mkv"},
		[2]string{"/f", "The Thing (1982).mkv"}, [2]string{"/f", "The Thing (2011).mkv"},
	)
	if set := byFilm(files, clues); len(set.Groups) != 0 {
		t.Errorf("different films were called copies: %+v", set.Groups)
	}
}

// The group crosses extensions — an .mkv and an .mp4 of one film are the pair
// to find — and never crosses into what is not a video at all.
func TestDuplicatesFilmCrossesVideoExtensionsAndNothingElse(t *testing.T) {
	files, clues := filmsOf(
		[2]string{"/f", "Arrival.2016.mkv"},
		[2]string{"/f", "Arrival (2016).mp4"},
		[2]string{"/f", "Arrival (2016).srt"},
		[2]string{"/f", "Arrival (2016).jpg"},
		[2]string{"/f", "Arrival (2016).nfo"},
	)
	g := only(t, byFilm(files, clues))
	if len(g.Files) != 2 {
		t.Errorf("group holds %v, want the two videos only", names(g))
	}
	for _, c := range clues[2:] {
		if len(c.Titles) != 0 {
			t.Errorf("a file that is not a video was read as a film: %+v", c)
		}
	}
}

// A folder may name a file that could not name itself — but only a folder
// that is a film. Two disc rips in a folder called "unsorted" are not copies
// of "unsorted".
func TestDuplicatesLetsAFilmFolderNameItsTracksButNotAPlace(t *testing.T) {
	files, clues := filmsOf(
		[2]string{"/unsorted", "VTS_01_1.VOB"}, [2]string{"/unsorted", "VTS_02_1.VOB"},
		[2]string{"/unsorted", "movie.mkv"}, [2]string{"/unsorted/again", "movie.mkv"},
	)
	if set := byFilm(files, clues); len(set.Groups) != 0 {
		t.Errorf("tracks in a folder named for a place were called one film: %+v", set.Groups)
	}

	files, clues = filmsOf(
		[2]string{"/films/Blade Runner (1982)", "title00.mkv"},
		[2]string{"/films", "Blade.Runner.1982.Final.Cut.1080p.mkv"},
	)
	g := only(t, byFilm(files, clues))
	if len(g.Files) != 2 {
		t.Errorf("a track in a film's folder was not called that film: %v", names(g))
	}
}

// Same name, same film, twice over — still found here as well as under the
// name question, because somebody reading this tab should see every copy.
func TestDuplicatesFilmFindsThePlainCopyToo(t *testing.T) {
	files, clues := filmsOf([2]string{"/f", "Whiplash.2014.mkv"}, [2]string{"/f/backup", "Whiplash.2014.mkv"})
	g := only(t, byFilm(files, clues))
	if len(g.Files) != 2 {
		t.Errorf("group holds %v, want both", names(g))
	}
}

func TestFilmClueOfReadsTheMatchOverTheName(t *testing.T) {
	e := &Entry{Name: "two.towers.mkv", Dir: "/f"}
	c := filmClueOf(e, &movie.Info{TMDBID: 121, Title: "The Lord of the Rings: The Two Towers", Original: "The Lord of the Rings: The Two Towers", Year: 2002})
	if c.TMDB != 121 || c.Year != 2002 || c.Label != "The Lord of the Rings: The Two Towers (2002)" {
		t.Errorf("clue = %+v", c)
	}
	// The stored title, once — it is also the original — and the name's own.
	if len(c.Titles) != 2 || c.Titles[0] != "lord of the rings the two towers" || c.Titles[1] != "two towers" {
		t.Errorf("titles = %q", c.Titles)
	}

	// A stored record with no date takes the year off the name.
	c = filmClueOf(&Entry{Name: "Heat.1995.mkv", Dir: "/f"}, &movie.Info{TMDBID: 949, Title: "Heat"})
	if c.Year != 1995 || c.Label != "Heat (1995)" {
		t.Errorf("a dateless match did not take the name's year: %+v", c)
	}

	if c := filmClueOf(&Entry{Name: "notes.txt", Dir: "/f"}, nil); len(c.Titles) != 0 {
		t.Errorf("a text file was read as a film: %+v", c)
	}
	if c := filmClueOf(&Entry{Name: "clip.bin", Dir: "/f", MIME: "video/mp4"}, nil); len(c.Titles) != 1 {
		t.Errorf("a file the MIME type calls a video was not read: %+v", c)
	}
}
