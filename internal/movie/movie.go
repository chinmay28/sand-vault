// Package movie turns a video file's name into a guess at what film it is, and
// looks that guess up against a film database.
//
// It is the only part of SAND that ever talks to anything but the user's own
// cloud accounts, which is why it is a package of its own and why nothing
// reaches it unless a folder has been opted in by name. A lookup sends a title
// guessed from a filename — and this machine's address, and the user's own API
// key — to a third party that has nothing to do with the vault. That is a real
// disclosure, small but real, and it is the user's to make rather than a
// default.
//
// Everything a lookup brings back is stored in the vault like anything else:
// the details go into the encrypted index, the poster becomes the file's stored
// thumbnail. So the browser still fetches from nowhere but the local server,
// and a vault that has been matched once never contacts the database again.
package movie

import (
	"strings"
	"time"
	"unicode"
)

// Credit is one name on a film, and what they did on it.
type Credit struct {
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

// Info is what is known about one film.
//
// It lives in the vault's index, which is encrypted at rest and replicated to
// the connected accounts as part of the manifest backup — so it is deliberately
// text and nothing else. A few hundred bytes per film is a rounding error next
// to the index it sits in; the poster, which is not, is stored as the file's
// thumbnail instead and travels the same path every other picture does.
type Info struct {
	TMDBID   int    `json:"tmdb_id"`
	IMDBID   string `json:"imdb_id,omitempty"`
	Title    string `json:"title"`
	Original string `json:"original_title,omitempty"`

	// Year is the release year, pulled out of Released because it is what the
	// list and the title line actually show.
	Year     int    `json:"year,omitempty"`
	Released string `json:"released,omitempty"` // full release date, as given

	Tagline  string   `json:"tagline,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Runtime  int      `json:"runtime,omitempty"` // minutes
	Genres   []string `json:"genres,omitempty"`

	Rating float64 `json:"rating,omitempty"` // out of 10
	Votes  int     `json:"votes,omitempty"`

	Directors []string `json:"directors,omitempty"`
	Cast      []Credit `json:"cast,omitempty"`

	// PosterPath is the database's own path for the artwork, kept so the
	// picture can be fetched again without a second search. That is not
	// hypothetical: changing the vault password drops every stored thumbnail,
	// because they are sealed under the key being retired and regenerating
	// derived data is cheaper than re-encrypting it. A photograph regenerates
	// from the file it is a picture of; a poster has no such source, so this is
	// what stands in for one.
	PosterPath string `json:"poster_path,omitempty"`

	// Query is what was actually searched for, and File is the name it was
	// guessed from. Both are shown in the details view, because the one thing a
	// reader needs to judge a match by is what the match was made on.
	Query string `json:"query,omitempty"`
	File  string `json:"file,omitempty"`

	// Manual marks a film somebody picked by hand after the guess got it wrong.
	// A later sweep leaves those alone: the whole point of correcting a match is
	// that it stays corrected.
	Manual bool `json:"manual,omitempty"`

	MatchedAt time.Time `json:"matched_at"`
}

// Candidate is one result of a search — enough to choose between films, and no
// more. The full record is a second request, made only for the one chosen.
type Candidate struct {
	TMDBID     int     `json:"tmdb_id"`
	Title      string  `json:"title"`
	Original   string  `json:"original_title,omitempty"`
	Year       int     `json:"year,omitempty"`
	Released   string  `json:"released,omitempty"`
	Overview   string  `json:"overview,omitempty"`
	Rating     float64 `json:"rating,omitempty"`
	Votes      int     `json:"votes,omitempty"`
	PosterPath string  `json:"poster_path,omitempty"`
}

// Best picks the candidate a guess most likely meant, or nil when there is
// nothing to pick from.
//
// The database already returns its results in a sensible order — roughly by
// popularity — so this does not try to re-rank them. It only overrules that
// order where the filename knows something the search did not use: the year,
// which separates a remake from the film it remade, and an exact title, which
// separates a film from the documentary about it. Ties go to whichever came
// first, which is the database's own opinion.
func Best(candidates []Candidate, guess Guess) *Candidate {
	if len(candidates) == 0 {
		return nil
	}

	wanted := normalizeTitle(guess.Title)
	bestAt, bestScore := 0, -1

	for i, c := range candidates {
		score := 0
		if guess.Year > 0 && c.Year > 0 {
			switch diff := guess.Year - c.Year; {
			case diff == 0:
				score += 4
			case diff == 1 || diff == -1:
				// A film released in December in one country and January in
				// another is one film, and the filename may have been named
				// after either.
				score += 2
			default:
				score -= 3
			}
		}
		if wanted != "" {
			switch {
			case normalizeTitle(c.Title) == wanted, normalizeTitle(c.Original) == wanted:
				score += 3
			case strings.HasPrefix(normalizeTitle(c.Title), wanted):
				score++
			}
		}
		if score > bestScore {
			bestAt, bestScore = i, score
		}
	}

	chosen := candidates[bestAt]
	return &chosen
}

// normalizeTitle reduces a title to what two spellings of the same film have in
// common: lower case, letters and digits, single spaces. It is only ever used
// to compare one title with another.
func normalizeTitle(title string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}
