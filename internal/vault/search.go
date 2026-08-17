package vault

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ErrEmptyQuery is returned when a search is asked for without anything to
// look for.
var ErrEmptyQuery = errors.New("search needs something to look for")

// DefaultSearchLimit caps a search that does not ask for a limit of its own.
// A vault is an index in memory, so the cost is in rendering thousands of rows
// rather than in finding them.
const DefaultSearchLimit = 200

// SearchKind narrows a search to one sort of result.
type SearchKind string

const (
	SearchAll     SearchKind = "all"
	SearchFiles   SearchKind = "file"
	SearchFolders SearchKind = "folder"
)

// Valid reports whether k is a kind the search understands.
func (k SearchKind) Valid() bool {
	switch k {
	case SearchAll, SearchFiles, SearchFolders:
		return true
	}
	return false
}

// SearchOptions describes one search over the browser namespace.
type SearchOptions struct {
	// Query is what to look for. A query containing "/" is matched against the
	// full path; anything else against the name alone. A query containing "*"
	// or "?" is a wildcard pattern; anything else is a case-insensitive
	// substring.
	Query string

	// Dir restricts the search to one subtree. Empty means the whole vault.
	Dir string

	// Kind restricts the results to files or to folders. Empty means both.
	Kind SearchKind

	// Limit caps how many hits come back. Zero means DefaultSearchLimit.
	Limit int
}

// SearchHit is one match: a stored file, or a folder in the namespace.
type SearchHit struct {
	Type string `json:"type"` // "file" or "folder"
	Path string `json:"path"` // full path of the hit
	Dir  string `json:"dir"`  // parent folder, normalized
	Name string `json:"name"` // the segment that matched

	// File carries the index entry when Type is "file", so a result row can
	// show size, placement and health without a second call.
	File *Entry `json:"file,omitempty"`

	// score orders the results and never leaves the package.
	score int
}

// SearchResults is one query's worth of hits.
type SearchResults struct {
	Query string      `json:"query"`
	Scope string      `json:"scope"`
	Hits  []SearchHit `json:"hits"`

	// Matched is how many hits the query found, which is larger than the
	// number returned when Truncated is set.
	Matched   int  `json:"matched"`
	Truncated bool `json:"truncated"`

	// Thumbs names the hits that have a stored thumbnail, exactly as a listing
	// does — a result row is the same row, and draws the same picture.
	Thumbs []string `json:"thumbs"`

	// Movies titles the hits that have been matched against the film database,
	// for the same reason: a poster in a search result should be captioned the
	// same way it is in the folder it came from.
	Movies map[string]MovieBrief `json:"movies,omitempty"`

	// FolderArt names the picture each folder hit is drawn with, by path — and
	// again for the same reason. A folder found by searching is the same folder
	// row, and it should look like itself.
	FolderArt map[string]FolderArt `json:"folder_art,omitempty"`
}

// Search finds files and folders whose name — or whose path, for a query that
// contains a slash — matches the query.
//
// Names and folder structure are only ever readable while the vault is open,
// so this is the one place they can be searched at all: nothing on any account
// can answer a question about what is stored.
func (v *Vault) Search(opts SearchOptions) (*SearchResults, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, ErrEmptyQuery
	}
	kind := opts.Kind
	if kind == "" {
		kind = SearchAll
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("unknown search kind %q", opts.Kind)
	}

	scope := CleanDir(opts.Dir)
	if !v.manifest.FolderExists(scope) {
		return nil, fmt.Errorf("no such folder: %s", scope)
	}

	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}

	hits := v.manifest.search(match, scope, kind)
	results := &SearchResults{Query: query, Scope: scope, Matched: len(hits)}
	if len(hits) > limit {
		hits = hits[:limit]
		results.Truncated = true
	}
	results.Hits = hits

	matched := make([]*Entry, 0, len(hits))
	for _, hit := range hits {
		if hit.File != nil {
			matched = append(matched, hit.File)
		}
	}
	results.Thumbs = v.thumbIDsForLocked(matched)
	if results.Thumbs == nil {
		results.Thumbs = []string{}
	}
	results.Movies = v.movieBriefsForLocked(matched)

	folders := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.Type == "folder" {
			folders = append(folders, hit.Path)
		}
	}
	results.FolderArt = v.folderArtForLocked(folders)
	return results, nil
}

// search collects every hit under scope, best match first.
func (m *Manifest) search(match *matcher, scope string, kind SearchKind) []SearchHit {
	prefix := scope
	if prefix != "/" {
		prefix += "/"
	}
	inScope := func(dir string) bool { return dir == scope || strings.HasPrefix(dir, prefix) }

	hits := []SearchHit{}

	if kind != SearchFiles {
		for _, folder := range m.folderPaths() {
			// The scope itself is where the search starts, not a result of it.
			if folder == scope || !strings.HasPrefix(folder, prefix) {
				continue
			}
			dir, name := splitFolder(folder)
			if score, ok := match.on(name, folder); ok {
				hits = append(hits, SearchHit{
					Type: "folder", Path: folder, Dir: dir, Name: name, score: score,
				})
			}
		}
	}

	if kind != SearchFolders {
		for _, e := range m.Entries {
			if !inScope(e.Dir) {
				continue
			}
			if score, ok := match.on(e.Name, e.Path()); ok {
				hits = append(hits, SearchHit{
					Type: "file", Path: e.Path(), Dir: e.Dir, Name: e.Name, File: e, score: score,
				})
			}
		}
	}

	sortHits(hits)
	return hits
}

// folderPaths is every folder in the namespace: the ones created explicitly
// plus the ones implied by where files were stored, each with its ancestors.
func (m *Manifest) folderPaths() []string {
	seen := map[string]bool{}
	out := []string{}

	add := func(dir string) {
		for dir != "/" && dir != "." && dir != "" {
			if seen[dir] {
				return
			}
			seen[dir] = true
			out = append(out, dir)
			dir, _ = splitFolder(dir)
		}
	}

	for _, folder := range m.Folders {
		add(CleanDir(folder))
	}
	for _, e := range m.Entries {
		add(e.Dir)
	}
	return out
}

// splitFolder separates a normalized folder path into its parent and its own
// name: "/a/b" becomes "/a" and "b".
func splitFolder(dir string) (parent, name string) {
	idx := strings.LastIndex(dir, "/")
	if idx < 0 {
		return "/", dir
	}
	if idx == 0 {
		return "/", dir[1:]
	}
	return dir[:idx], dir[idx+1:]
}

// sortHits puts the closest matches first: a name that is exactly the query
// beats one that starts with it, which beats one that merely contains it.
// Beyond that, shallower paths come first — the file at the top of a tree is
// nearly always the one being looked for — then folders before files, then
// alphabetically so the order never depends on index order.
func sortHits(hits []SearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.score != b.score {
			return a.score < b.score
		}
		if da, db := depth(a.Path), depth(b.Path); da != db {
			return da < db
		}
		if a.Type != b.Type {
			return a.Type == "folder"
		}
		if la, lb := strings.ToLower(a.Path), strings.ToLower(b.Path); la != lb {
			return la < lb
		}
		return a.Path < b.Path
	})
}

// depth counts how many folders deep a path sits.
func depth(p string) int { return strings.Count(p, "/") }

// Match scores, best first.
const (
	scoreExact  = 0
	scorePrefix = 1
	scoreLoose  = 2
)

// matcher decides whether one name or path answers a query.
type matcher struct {
	query   string         // lowercased
	onPath  bool           // match against the full path rather than the name
	pattern *regexp.Regexp // set for a wildcard query
}

// newMatcher compiles a query.
//
// The rules are the ones a file browser's search box is expected to follow: a
// bare word is a case-insensitive substring, "*" and "?" turn the query into a
// wildcard pattern, and a slash anywhere in it means the whole path is being
// described rather than a name.
func newMatcher(query string) (*matcher, error) {
	m := &matcher{
		query:  strings.ToLower(query),
		onPath: strings.Contains(query, "/"),
	}
	if !strings.ContainsAny(query, "*?") {
		return m, nil
	}

	pattern := m.query
	if m.onPath && !strings.HasPrefix(pattern, "/") {
		// "photos/*.jpg" means a photos folder anywhere, not one at the root.
		pattern = "*/" + pattern
	}
	// Everything but the two wildcards is taken literally, so a name full of
	// regexp punctuation cannot turn into a pattern of its own.
	expr := regexp.QuoteMeta(pattern)
	expr = strings.ReplaceAll(expr, `\*`, `.*`)
	expr = strings.ReplaceAll(expr, `\?`, `.`)

	compiled, err := regexp.Compile(`\A` + expr + `\z`)
	if err != nil {
		return nil, fmt.Errorf("could not read the search pattern %q: %w", query, err)
	}
	m.pattern = compiled
	return m, nil
}

// on reports whether a hit's name and path match, and how closely.
func (m *matcher) on(name, path string) (int, bool) {
	subject := strings.ToLower(name)
	if m.onPath {
		subject = strings.ToLower(path)
	}

	if m.pattern != nil {
		if m.pattern.MatchString(subject) {
			return scoreLoose, true
		}
		return 0, false
	}

	idx := strings.Index(subject, m.query)
	switch {
	case idx < 0:
		return 0, false
	case subject == m.query:
		return scoreExact, true
	case idx == 0:
		return scorePrefix, true
	default:
		return scoreLoose, true
	}
}
