package movie

import (
	"strings"
	"unicode"
)

// A filename often says more than the film's title: the franchise in front of
// it ("james-bond-a-view-to-a-kill"), a running number before or after it
// ("007 - Goldfinger", "Casino Royale 21"), something tacked on at the end. The
// database searches for every word it is given, so a name like that finds
// nothing, or something wrong.
//
// So a lookup that finds nothing that fits searches again on less of the name.
// First it strips the numbers off both ends, then it tries shorter runs of the
// words that are left. Because a shorter search finds more, a candidate from a
// relaxed search is accepted only when it fits the whole name: its title has to
// appear in the name as whole words, and its year cannot contradict the one
// the name gave.

// maxVariants bounds how many extra searches one file may cost. Each is a
// request to a third party, and a sweep makes them for every film in a folder.
const maxVariants = 6

// Variants are the relaxed forms of a guess, in the order worth trying them:
// numbers stripped off both ends first, then shorter and shorter runs of the
// words that are left, keeping either the end or the start of the title. The
// guess itself is never among them, and none of them repeats another.
//
// A number stripped off the end of a title is kept as the year when it is a
// plausible one and the guess has no year of its own.
func (g Guess) Variants() []Guess {
	words := strings.Fields(g.Title)
	year := g.Year

	// Numbers off both ends, and any punctuation the parser left there.
	for len(words) > 0 && !hasLetter(words[0]) {
		if y, ok := parseYear(words[0]); ok && year == 0 {
			year = y
		}
		words = words[1:]
	}
	for len(words) > 0 && !hasLetter(words[len(words)-1]) {
		if y, ok := parseYear(words[len(words)-1]); ok && year == 0 {
			year = y
		}
		words = words[:len(words)-1]
	}

	seen := map[string]bool{strings.ToLower(g.String()): true}
	var out []Guess
	add := func(ws []string) {
		if len(out) >= maxVariants || !searchable(ws) {
			return
		}
		v := Guess{Title: strings.Join(ws, " "), Year: year}
		if key := strings.ToLower(v.String()); !seen[key] {
			seen[key] = true
			out = append(out, v)
		}
	}

	add(words)
	// Shorter runs, longest first. Dropping words from the front comes first,
	// because a franchise or a collection name in front of the title is far
	// more common than words after it that the parser did not already cut.
	for n := len(words) - 1; n >= 1; n-- {
		add(words[len(words)-n:])
		add(words[:n])
	}
	return out
}

// searchable reports whether a run of words is worth a search on its own:
// something with letters in it, and no single short word or name for the
// medium, which would match hundreds of films.
func searchable(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if len(words) > 1 {
		return true
	}
	w := strings.ToLower(words[0])
	return hasLetter(w) && len([]rune(w)) >= 4 && !genericNames[w]
}

// Fits reports whether a candidate is plausibly the film a name means even
// though a search was made on only part of it: its title, or its original
// title, appears among the name's words as whole words, and its year is within
// one of the guess's when both are known.
func (g Guess) Fits(c Candidate) bool {
	if g.Year > 0 && c.Year > 0 {
		if diff := g.Year - c.Year; diff > 1 || diff < -1 {
			return false
		}
	}
	name := " " + plainTitle(g.Title) + " "
	for _, title := range []string{c.Title, c.Original} {
		if t := plainTitle(title); t != "" && strings.Contains(name, " "+t+" ") {
			return true
		}
	}
	return false
}

// fitting is the candidates that fit a guess, in the order they came.
func fitting(candidates []Candidate, g Guess) []Candidate {
	var out []Candidate
	for _, c := range candidates {
		if g.Fits(c) {
			out = append(out, c)
		}
	}
	return out
}

// plainTitle is normalizeTitle with apostrophes dropped rather than turned
// into a break, as clean drops them: a stored "Charlotte's" and a parsed
// "Charlottes" have to compare equal.
func plainTitle(title string) string {
	return normalizeTitle(strings.Map(func(r rune) rune {
		if r == '\'' || r == '’' || r == '`' {
			return -1
		}
		return r
	}, title))
}

func hasLetter(s string) bool {
	return strings.IndexFunc(s, unicode.IsLetter) >= 0
}
