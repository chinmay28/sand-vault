package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Collection is what the tools read. The server implements it over the open
// vault; the tests implement it over a few slices.
//
// It answers three questions, which between them cover what somebody asks a
// file store in plain language: what films do I have, what is in here called
// X, and what does the film database say exists.
type Collection interface {
	// Films lists every video with stored film details at or below dir. An
	// empty dir means every folder that has film lookup turned on.
	Films(ctx context.Context, dir string) ([]Film, error)

	// Search finds files and folders whose name matches the query, the way the
	// search box does: a substring, or a pattern with * and ? in it.
	Search(ctx context.Context, query, dir string, limit int) ([]Hit, error)

	// FilmDatabase asks the film database what it has for a title, series or
	// subject. It is the one call that leaves the machine, and it sends the
	// query and nothing else.
	FilmDatabase(ctx context.Context, query string) ([]Title, error)
}

// Film is one matched video in the vault.
type Film struct {
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
	Path  string `json:"path"`

	// TMDBID lets two records of the same film be told apart from two films
	// with the same title, on both sides of a comparison.
	TMDBID int `json:"tmdb_id,omitempty"`
}

// Hit is one file or folder a search found.
type Hit struct {
	Type string `json:"type"` // "file" or "folder"
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`

	// Film is the stored film title, when the file has one.
	Film string `json:"film,omitempty"`
}

// Title is one film the database knows about.
type Title struct {
	Title    string `json:"title"`
	Year     int    `json:"year,omitempty"`
	TMDBID   int    `json:"tmdb_id,omitempty"`
	Overview string `json:"overview,omitempty"`
}

// maxOverview is how much of a database summary the model is shown. It is
// there to tell a remake from the original, not to be read out.
const maxOverview = 160

// maxSearchHits caps what one search hands the model. It is a context window
// being filled, not a screen, and two hundred paths is already more than any
// answer needs.
const maxSearchHits = 200

// Tools builds the three tools over a collection.
func Tools(c Collection) []Tool {
	return []Tool{
		{
			ToolSpec: ToolSpec{
				Name: "list_films",
				Description: "List the films in the vault: every video that has been matched " +
					"against the film database, with its title, year and path. Give a folder " +
					"path to list only what is under it; leave it empty for every film folder.",
				Parameters: schema(`{
					"type": "object",
					"properties": {
						"dir": {"type": "string", "description": "Folder path to list under, such as /films. Empty for all film folders."}
					}
				}`),
			},
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Dir string `json:"dir"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return "", err
				}
				films, err := c.Films(ctx, in.Dir)
				if err != nil {
					return "", err
				}
				return encode(map[string]any{"count": len(films), "films": films})
			},
		},
		{
			ToolSpec: ToolSpec{
				Name: "search_vault",
				Description: "Find files and folders in the vault by name. The query is a " +
					"case-insensitive substring, or a pattern using * and ? wildcards. " +
					"Returns paths, sizes and, for matched videos, the film title.",
				Parameters: schema(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "What to look for in file and folder names."},
						"dir": {"type": "string", "description": "Folder to search under. Empty for the whole vault."},
						"limit": {"type": "integer", "description": "Most hits to return. Default 50."}
					},
					"required": ["query"]
				}`),
			},
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Query string `json:"query"`
					Dir   string `json:"dir"`
					Limit int    `json:"limit"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.Query) == "" {
					return "", fmt.Errorf("search_vault needs a query")
				}
				limit := in.Limit
				if limit <= 0 {
					limit = 50
				}
				if limit > maxSearchHits {
					limit = maxSearchHits
				}
				hits, err := c.Search(ctx, in.Query, in.Dir, limit)
				if err != nil {
					return "", err
				}
				return encode(map[string]any{"count": len(hits), "hits": hits})
			},
		},
		{
			ToolSpec: ToolSpec{
				Name: "search_film_database",
				Description: "Search the film database for a title, a series or a character, " +
					"for example \"Batman\". Returns the films it knows with title, year and " +
					"a short summary. Use this to find out what exists, then compare with list_films.",
				Parameters: schema(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "Title, series or subject to search the database for."}
					},
					"required": ["query"]
				}`),
			},
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Query string `json:"query"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.Query) == "" {
					return "", fmt.Errorf("search_film_database needs a query")
				}
				titles, err := c.FilmDatabase(ctx, in.Query)
				if err != nil {
					return "", err
				}
				for i := range titles {
					titles[i].Overview = clip(titles[i].Overview, maxOverview)
				}
				return encode(map[string]any{"count": len(titles), "films": titles})
			},
		},
	}
}

// schema compacts a JSON schema written inline above, so the source can be
// indented and what goes over the wire is not.
func schema(s string) json.RawMessage {
	var out json.RawMessage
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		panic("assistant: bad tool schema: " + err.Error())
	}
	compact, err := json.Marshal(out)
	if err != nil {
		panic("assistant: bad tool schema: " + err.Error())
	}
	return compact
}

// decodeArgs reads what the model wrote. Unknown fields are ignored rather
// than rejected: a model that adds a field it dreamt up has still asked a
// sensible question.
func decodeArgs(args json.RawMessage, out any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, out); err != nil {
		return fmt.Errorf("could not read the tool arguments: %w", err)
	}
	return nil
}

func encode(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], ' ')
	if cut < n/2 {
		cut = n
	}
	return s[:cut] + "…"
}
