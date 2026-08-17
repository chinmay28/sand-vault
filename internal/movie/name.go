package movie

import (
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Reading a film's name off a filename is guesswork, and the shape of the
// guess is the same one every media server makes: what people actually store is
// "The.Thing.1982.REMASTERED.1080p.BluRay.x265-RARBG.mkv", and the film in it
// is "The Thing" from 1982. Everything after the year is how the file was made
// rather than what it is, and the separators are dots because the name came off
// a network where spaces were once inconvenient.
//
// So: cut at the year if there is one, cut at the first token that describes an
// encoding if there is not, and treat dots, underscores and brackets as
// punctuation. What is left is a title to search for. It is wrong sometimes —
// which is exactly why a match can be corrected by hand and why the details
// view always says what it searched for.

// Guess is what a name suggests a film is called. A Year of zero means the name
// did not say.
type Guess struct {
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
}

// Empty reports whether the guess is too thin to search on.
func (g Guess) Empty() bool { return strings.TrimSpace(g.Title) == "" }

// String renders the guess the way it is searched for and shown: "The Thing
// (1982)", or just the title when there is no year.
func (g Guess) String() string {
	if g.Year > 0 {
		return g.Title + " (" + strconv.Itoa(g.Year) + ")"
	}
	return g.Title
}

// releaseTags are the tokens that describe how a file was encoded rather than
// what it holds. Everything from the first of them onwards is dropped when the
// name carries no year to cut at instead.
//
// The list is deliberately short of anything that could be a word in a title.
// "Cut", "Part", "Limited" and "Extended" all appear in real film names — "The
// Final Cut" would become "The Final" — and losing a genuine tag costs a
// slightly noisier search, while eating a title word costs the match.
var releaseTags = map[string]bool{
	// Resolution and range
	"480p": true, "576p": true, "720p": true, "1080p": true, "1440p": true,
	"2160p": true, "4320p": true, "4k": true, "8k": true, "uhd": true,
	"hdr": true, "hdr10": true, "sdr": true,

	// Source. Nothing hyphenated is listed, because the hyphen is gone by the
	// time these are compared — see clean, and see the "web dl" pair below.
	"bluray": true, "bdrip": true, "brrip": true, "bdremux": true,
	"dvdrip": true, "dvdscr": true, "webrip": true, "webdl": true,
	"hdtv": true, "pdtv": true, "hdrip": true, "remux": true, "telesync": true,
	"telecine": true, "workprint": true,

	// Video codec
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true,
	"avc": true, "xvid": true, "divx": true, "10bit": true, "8bit": true,

	// Audio
	"aac": true, "ac3": true, "eac3": true, "dts": true, "dtshd": true,
	"truehd": true, "atmos": true, "dd5": true, "ddp5": true, "flac": true,
	"mp3": true, "opus": true,

	// Provenance
	"proper": true, "repack": true, "rerip": true, "internal": true,
	"remastered": true, "unrated": true,
}

// isTag reports whether the token at i describes the encoding.
//
// The pair test is what "WEB-DL" costs: the hyphen became a space long before
// this, and "web" on its own cannot be a tag — Charlotte has one, and cutting
// there would leave the search asking for "Charlottes".
func isTag(tokens []string, i int) bool {
	token := strings.ToLower(tokens[i])
	if releaseTags[token] {
		return true
	}
	return token == "web" && i+1 < len(tokens) && strings.EqualFold(tokens[i+1], "dl")
}

// genericNames are filenames that name the medium rather than the film. A disc
// ripper produces them by the hundred, and they are the reason the folder a
// file sits in is worth consulting at all.
var genericNames = map[string]bool{
	"movie": true, "film": true, "video": true, "main": true, "title": true,
	"fullmovie": true, "index": true, "untitled": true,
	// What a DVD rip is called before anyone renames it.
	"vts": true, "track": true, "disc": true, "dvd": true,
}

// Parse reads a film title and year out of one name — a filename or a folder
// name, with or without an extension.
func Parse(name string) Guess {
	cleaned := clean(stripExtension(name))
	tokens := strings.Fields(cleaned)
	if len(tokens) == 0 {
		return Guess{}
	}

	// The year, if the name has one. The last plausible one wins: "2012.2009"
	// is the 2009 film called 2012, and a name ending in a year that is part of
	// the title is far rarer than one where the year trails the title. The first
	// token is never it, for the same reason — a film called 1917 is a film
	// called 1917.
	yearAt, year := -1, 0
	for i := len(tokens) - 1; i > 0; i-- {
		if y, ok := parseYear(tokens[i]); ok {
			yearAt, year = i, y
			break
		}
	}

	end := len(tokens)
	if yearAt > 0 {
		end = yearAt
	} else {
		// No year to cut at, so cut at the first token that describes the
		// encoding instead. Never the first token: a tag cannot be the whole
		// title, and a film named after one would lose everything.
		for i := 1; i < len(tokens); i++ {
			if isTag(tokens, i) {
				end = i
				break
			}
		}
	}

	title := strings.Join(tokens[:end], " ")
	return Guess{Title: strings.TrimSpace(title), Year: year}
}

// ParseIn reads a name the way a browser sees it: a file inside a folder.
//
// The folder is not decoration. Both the layout media servers ask for and the
// one a disc ripper produces put the film's name on the folder and something
// useless on the file — "Blade Runner (1982)/title00.mkv" — so a guess made
// from the filename alone would search for "title00". The file still wins when
// it says anything at all, because it is the more specific of the two; the
// folder fills in what it left out.
func ParseIn(dir, name string) Guess {
	fromFile := Parse(name)
	fromDir := Parse(path.Base(strings.TrimSuffix(dir, "/")))

	if weak(fromFile) {
		// Nothing on the file and nothing on the folder: better an empty guess
		// than a search for "0002" — a lookup leaves this machine, and a query
		// nobody could answer is not worth sending.
		if fromDir.Empty() {
			return Guess{}
		}
		return fromDir
	}
	// A file that named the film but not the year, in a folder that named both:
	// "Blade Runner (1982)/Blade.Runner.1080p.mkv".
	if fromFile.Year == 0 && fromDir.Year > 0 &&
		normalizeTitle(fromFile.Title) == normalizeTitle(fromDir.Title) {
		fromFile.Year = fromDir.Year
	}
	return fromFile
}

// weak reports whether a guess is too thin to search on: nothing at all, a
// name that describes the medium rather than the film, or a number that is a
// track off a disc rather than a title.
func weak(g Guess) bool {
	title := strings.TrimSpace(g.Title)
	if title == "" {
		return true
	}
	if genericNames[strings.ToLower(title)] {
		return true
	}

	letters := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return r
		}
		return -1
	}, strings.ToLower(title))

	if letters == "" {
		// A number on its own is a film often enough — 1917, 2012, 300, 1408 —
		// so the test is not "is it numeric" but "is it numbered": a track is
		// zero-padded and a title is not.
		return strings.HasPrefix(title, "0") || len(strings.Fields(title)) > 1
	}
	// "title00", "VTS_01_1": letters that are a generic word with a number
	// stuck to them.
	return genericNames[letters] && letters != title
}

// stripExtension drops a trailing ".mkv" and the like, and only that: a name
// ending in ".2019" or "Dr. No" keeps every character it came with.
func stripExtension(name string) string {
	ext := path.Ext(name)
	if len(ext) < 2 || len(ext) > 5 {
		return name
	}
	for _, r := range ext[1:] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return name
		}
	}
	if _, isYear := parseYear(ext[1:]); isYear {
		return name
	}
	return strings.TrimSuffix(name, ext)
}

// clean turns a scene name into words: punctuation becomes a space, so that
// "The.Thing" is two words and a year in brackets is a token of its own.
//
// Brackets are opened rather than dropped with their contents, because the year
// is very often what is inside them — "The Thing [1982]" — and everything else
// they tend to hold is a release tag the cut above removes anyway.
//
// Apostrophes are the exception: they vanish instead of becoming a space, since
// "Charlotte's" is one word and "Charlotte s" is a search for two.
func clean(name string) string {
	var b strings.Builder

	for _, r := range name {
		switch {
		case r == '\'' || r == '’' || r == '`' || r == '"':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}

	return strings.Join(strings.Fields(b.String()), " ")
}

// parseYear reads a token that is a plausible release year and nothing else.
// The upper bound moves with the clock, because a film announced for next year
// is a film somebody has a file of.
func parseYear(token string) (int, bool) {
	if len(token) != 4 {
		return 0, false
	}
	year, err := strconv.Atoi(token)
	if err != nil {
		return 0, false
	}
	if year < 1888 || year > time.Now().Year()+2 {
		return 0, false
	}
	return year, true
}
