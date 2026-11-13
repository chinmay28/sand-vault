package vault

import (
	"slices"
	"strconv"

	"github.com/chinmay28/SandHarbor/internal/movie"
)

// The same film, twice, under names that share nothing.
//
// A films folder is the one place the name question fails on purpose. The
// same film arrives as "The.Lord.of.the.Rings.The.Two.Towers.2002.EXTENDED.
// PROPER.1080p.BluRay.H264.AAC-LAMA.mkv" and again, tidied by hand or by a
// media server, as "The Lord of the Rings: The Two Towers (2002).mkv" — and
// those two names are not a copy marker, a separator or a typo apart. They are
// forty edits apart, with different numbers in them, and the name question is
// right to keep them separate: by its rules they are not the same name. They
// are the same film, which is a fourth question.
//
// It is answered from two things the vault already has, and nothing is looked
// up. A file matched against the film database carries that film's id, and
// two files with one id are one film whatever they are called. A file that
// was never matched — because the folder never opted in, or because the match
// has not run yet — is read the way the lookup would read it (see
// movie.Parse): a title and, nearly always, a year, which is what a scene
// release and a tidy name turn out to agree on once the encoding tags and the
// punctuation come off. The two sources join: a matched file answers to its
// stored title as well as its own name's, so a matched copy and an unmatched
// one still meet.
//
// Only videos are asked. The group never crosses into subtitles or posters,
// and it does cross extensions: an .mkv and an .mp4 of one film are the pair
// this exists to find. Every group is a resemblance rather than a proof — two
// encodes of one film are different bytes — so each says whether the hashes
// agree, and the copy suggested for keeping is the largest, which is usually
// the better encode. That is the opposite of what the other questions suggest,
// and it is deliberate: "report (2).pdf" is a spare, but a 4 GB Blu-ray rip is
// not a spare of its 800 MB download.

// filmClue is what can be told about which film a file is without asking
// anyone. A clue with no Titles is a file that is not a film, or not one
// anybody could name.
type filmClue struct {
	// TMDB is the database id a stored match carries, 0 when there is none.
	TMDB int

	// Titles are the comparison keys the file answers to (see movie.TitleKey):
	// its own name's, and the stored title and original title when matched.
	Titles []string

	// Year is the stored match's year when there is one, else the name's, else
	// zero.
	Year int

	// Label is the film as it should be called in a group — the stored title
	// and year when matched, the name's guess otherwise.
	Label string
}

// filmClueOf reads a clue off a file: the stored match when there is one, the
// name otherwise. A file that is not a video has no clue at all.
func filmClueOf(e *Entry, info *movie.Info) filmClue {
	if !IsVideo(e) {
		return filmClue{}
	}

	guess := filmGuess(e.Dir, e.Name)
	if info == nil {
		if guess.Empty() {
			return filmClue{}
		}
		return filmClue{
			Titles: []string{movie.TitleKey(guess.Title)},
			Year:   guess.Year,
			Label:  guess.String(),
		}
	}

	clue := filmClue{TMDB: info.TMDBID, Year: info.Year, Label: info.Title}
	if clue.Year == 0 {
		// A record without a date is rare but allowed; the name may still say.
		clue.Year = guess.Year
	}
	if clue.Year > 0 {
		clue.Label += " (" + strconv.Itoa(clue.Year) + ")"
	}
	for _, title := range []string{info.Title, info.Original, guess.Title} {
		if key := movie.TitleKey(title); key != "" && !slices.Contains(clue.Titles, key) {
			clue.Titles = append(clue.Titles, key)
		}
	}
	return clue
}

// filmGuess reads a film off a file's name the way the lookup does, with one
// more refusal. The lookup lets a folder name a file that could not name
// itself — "Blade Runner (1982)/title00.mkv" — and so does this, but only
// when the folder names a film and not merely a place: two disc rips in a
// folder called "unsorted" would otherwise both be called "unsorted" and
// offered as copies of each other. A folder that is a film has a year on it.
func filmGuess(dir, name string) movie.Guess {
	guess := movie.ParseIn(dir, name)
	if movie.Parse(name).Weak() && guess.Year == 0 {
		return movie.Guess{}
	}
	if guess.Weak() {
		return movie.Guess{}
	}
	return guess
}

// byFilm groups the videos that are one film. clues[i] describes files[i].
func byFilm(files []DuplicateFile, clues []filmClue) DuplicateSet {
	// A name without a year beside names with one. "Dune.mkv" next to
	// "Dune.2021.mkv" is one film; next to "Dune.1984.mkv" and
	// "Dune.2021.mkv" it is a question nobody can answer, so it stays apart
	// from both. The year a title was seen with is borrowed when there is
	// exactly one.
	years := map[string]map[int]bool{}
	for _, c := range clues {
		if c.Year == 0 {
			continue
		}
		for _, title := range c.Titles {
			if years[title] == nil {
				years[title] = map[int]bool{}
			}
			years[title][c.Year] = true
		}
	}
	yearOf := func(c filmClue) int {
		if c.Year > 0 {
			return c.Year
		}
		seen := map[int]bool{}
		for _, title := range c.Titles {
			for y := range years[title] {
				seen[y] = true
			}
		}
		if len(seen) == 1 {
			for y := range seen {
				return y
			}
		}
		return 0
	}

	// Union-find over files, joined by every identity they share: a database
	// id, or a title with its year. Keys are exact, so there is no chain to
	// cap — a film is not one edit from another film.
	parent := make([]int, len(files))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	join := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			// Toward the lower index, so the lead of a group is the file first
			// walked and the answer does not depend on map order.
			if rb < ra {
				ra, rb = rb, ra
			}
			parent[rb] = ra
		}
	}

	first := map[string]int{}
	for i, c := range clues {
		if len(c.Titles) == 0 {
			continue
		}
		keys := make([]string, 0, len(c.Titles)+1)
		if c.TMDB > 0 {
			keys = append(keys, "id:"+strconv.Itoa(c.TMDB))
		}
		year := strconv.Itoa(yearOf(c))
		for _, title := range c.Titles {
			keys = append(keys, "title:"+title+"\x00"+year)
		}
		for _, key := range keys {
			if j, seen := first[key]; seen {
				join(i, j)
			} else {
				first[key] = i
			}
		}
	}

	by := map[string][]int{}
	order := []string{}
	for i, c := range clues {
		if len(c.Titles) == 0 {
			continue
		}
		lead := strconv.Itoa(find(i))
		if _, seen := by[lead]; !seen {
			order = append(order, lead)
		}
		by[lead] = append(by[lead], i)
	}

	set := collectWith(files, by, order, func(lead string) string {
		return "film:" + lead
	}, keepLargest)

	// What to call each group: the database's title where any copy was
	// matched, the first copy's own guess otherwise.
	at := make(map[string]int, len(files))
	for i, f := range files {
		at[f.ID] = i
	}
	for g := range set.Groups {
		set.Groups[g].Title = filmLabel(set.Groups[g].Files, func(f DuplicateFile) filmClue {
			return clues[at[f.ID]]
		})
	}
	return set
}

// filmLabel names a group: the stored film if any copy was matched, else what
// the suggested survivor's name said.
func filmLabel(members []DuplicateFile, clueOf func(DuplicateFile) filmClue) string {
	for _, f := range members {
		if c := clueOf(f); c.TMDB > 0 {
			return c.Label
		}
	}
	for _, f := range members {
		if c := clueOf(f); c.Label != "" {
			return c.Label
		}
	}
	return ""
}

// keepLargest prefers the bigger copy of a film, which is usually the better
// encode — the suggestion is the opposite of the plainest-name rule the other
// questions use, because a 4 GB Blu-ray rip is not a spare of its 800 MB
// download. Equal sizes fall through to the usual order.
func keepLargest(a, b DuplicateFile) bool {
	return a.Size > b.Size
}
