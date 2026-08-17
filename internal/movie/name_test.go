package movie

import (
	"fmt"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		title string
		year  int
	}{
		// The shape nearly everything arrives in.
		{"The.Thing.1982.REMASTERED.1080p.BluRay.x265-RARBG.mkv", "The Thing", 1982},
		{"Blade Runner (1982).mkv", "Blade Runner", 1982},
		{"Arrival.2016.2160p.UHD.BluRay.x265.HDR-TERMiNAL.mp4", "Arrival", 2016},
		{"Spirited_Away_2001_1080p_BluRay.mkv", "Spirited Away", 2001},
		{"Dune Part Two [2024] [WEBRip].mkv", "Dune Part Two", 2024},

		// No year, so the cut is at the first tag instead.
		{"Whiplash.1080p.WEB-DL.mkv", "Whiplash", 0},
		{"The Lighthouse BluRay x264.mkv", "The Lighthouse", 0},

		// Nothing to cut at at all.
		{"Nosferatu.mkv", "Nosferatu", 0},
		{"holiday video.mp4", "holiday video", 0},

		// A title that is a year, which is why the first token is never read as
		// one — and which still finds the real year when there is one after it.
		{"1917.2019.1080p.BluRay.mkv", "1917", 2019},
		{"2012.mkv", "2012", 0},
		{"2012.2009.BluRay.mkv", "2012", 2009},

		// Punctuation that is part of the title rather than a separator.
		{"Charlotte's Web.mkv", "Charlottes Web", 0},
		{"Charlottes.Web.WEB-DL.mkv", "Charlottes Web", 0},
		{"Wall-E.2008.mkv", "Wall E", 2008},

		// An extension that is not one.
		{"The Hateful Eight", "The Hateful Eight", 0},
		// 2049 is part of the title rather than a release date, and the
		// plausible-year window is what says so.
		{"Blade Runner 2049", "Blade Runner 2049", 0},

		{"", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.name)
			if got.Title != tc.title || got.Year != tc.year {
				t.Errorf("Parse(%q) = %q/%d, want %q/%d", tc.name, got.Title, got.Year, tc.title, tc.year)
			}
		})
	}
}

// A year further in the future than anything could have been released is part
// of a title or a piece of noise, not a release date.
func TestParseIgnoresImplausibleYears(t *testing.T) {
	far := fmt.Sprintf("Some Film %d 1080p.mkv", time.Now().Year()+40)
	if got := Parse(far); got.Year != 0 {
		t.Errorf("Parse(%q) read a year of %d — it is not a release date", far, got.Year)
	}
	if got := Parse("Metropolis 1799 BluRay.mkv"); got.Year != 0 {
		t.Errorf("year = %d, want none — cinema did not exist", got.Year)
	}
}

func TestParseIn(t *testing.T) {
	cases := []struct {
		dir, file string
		title     string
		year      int
	}{
		// The file says everything; the folder is not consulted.
		{"/films", "The.Thing.1982.1080p.mkv", "The Thing", 1982},

		// The layout a disc ripper produces, and the one media servers ask for:
		// the film is named on the folder and the file is a track number.
		{"/films/Blade Runner (1982)", "title00.mkv", "Blade Runner", 1982},
		{"/films/Blade Runner (1982)", "movie.mkv", "Blade Runner", 1982},
		{"/films/Blade Runner (1982)", "VTS_01_1.VOB", "Blade Runner", 1982},

		// The file names the film but not the year, and the folder has it.
		{"/films/Blade Runner (1982)", "Blade.Runner.1080p.BluRay.mkv", "Blade Runner", 1982},

		// A file that names a different film keeps its own name: a folder can
		// hold more than one thing.
		{"/films/Blade Runner (1982)", "Alien.1979.mkv", "Alien", 1979},

		// Nothing anywhere.
		{"/", "0002.mkv", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.dir+"/"+tc.file, func(t *testing.T) {
			got := ParseIn(tc.dir, tc.file)
			if got.Title != tc.title || got.Year != tc.year {
				t.Errorf("ParseIn(%q, %q) = %q/%d, want %q/%d",
					tc.dir, tc.file, got.Title, got.Year, tc.title, tc.year)
			}
		})
	}
}

func TestGuessString(t *testing.T) {
	if got := (Guess{Title: "The Thing", Year: 1982}).String(); got != "The Thing (1982)" {
		t.Errorf("String() = %q", got)
	}
	if got := (Guess{Title: "Nosferatu"}).String(); got != "Nosferatu" {
		t.Errorf("String() = %q", got)
	}
	if !(Guess{}).Empty() {
		t.Error("an empty guess reports itself as searchable")
	}
}

func TestBestPrefersTheYearTheFilenameGave(t *testing.T) {
	// What the database answers for "The Thing": the popular remake first.
	candidates := []Candidate{
		{TMDBID: 2, Title: "The Thing", Year: 2011},
		{TMDBID: 1, Title: "The Thing", Year: 1982},
	}

	if best := Best(candidates, Guess{Title: "The Thing", Year: 1982}); best == nil || best.TMDBID != 1 {
		t.Errorf("best = %+v, want the 1982 film", best)
	}
	// Without a year there is nothing to overrule the database's own order.
	if best := Best(candidates, Guess{Title: "The Thing"}); best == nil || best.TMDBID != 2 {
		t.Errorf("best = %+v, want the first result", best)
	}
	if best := Best(nil, Guess{Title: "The Thing"}); best != nil {
		t.Errorf("best = %+v, want nothing to choose", best)
	}
}

func TestBestPrefersAnExactTitle(t *testing.T) {
	candidates := []Candidate{
		{TMDBID: 9, Title: "Making Alien", Year: 1979},
		{TMDBID: 348, Title: "Alien", Year: 1979},
	}
	if best := Best(candidates, Guess{Title: "Alien", Year: 1979}); best == nil || best.TMDBID != 348 {
		t.Errorf("best = %+v, want the film rather than the documentary about it", best)
	}
}

// A release date a year either side of what the filename said is the same film
// released in two countries, and must not be thrown out.
func TestBestToleratesAYearEitherWay(t *testing.T) {
	candidates := []Candidate{
		{TMDBID: 5, Title: "Something Else", Year: 1994},
		{TMDBID: 6, Title: "Parasite", Year: 2019},
	}
	if best := Best(candidates, Guess{Title: "Parasite", Year: 2020}); best == nil || best.TMDBID != 6 {
		t.Errorf("best = %+v, want Parasite", best)
	}
}
