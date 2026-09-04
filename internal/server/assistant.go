package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/chinmay28/sand-vault/internal/assistant"
	"github.com/chinmay28/sand-vault/internal/movie"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// Sandy, the chat assistant, is a model the user runs on a machine of their
// own, given three tools that read the open index. What it can see is exactly
// what the tools hand it, and the tools are written here, over the vault, so
// that the answer to "what does the assistant send to the model server" is
// three functions long.
//
// It sees names, paths, sizes and stored film titles. It never sees a file's
// contents, an account, a credential, or a folder in a sub vault that is not
// open — the vault's own scoping rules apply, because every tool goes
// through the same calls the browser does.

// vaultCollection is the assistant's view of one vault: the main one, or a
// sub vault somebody has open. It is built per request, so it is as current
// as the request.
type vaultCollection struct {
	server *Server
	scope  vault.Scope
}

var _ assistant.Collection = vaultCollection{}

// Films lists every matched video under dir, or under every folder that has
// film lookup turned on when dir is empty.
//
// It lists films the way the search box would list "*" under a folder and
// keeps the hits that carry film details. That reuses the one walk the vault
// already has rather than adding a second one for the assistant, and it
// means the assistant can only ever list what a search could find.
func (c vaultCollection) Films(ctx context.Context, dir string) ([]assistant.Film, error) {
	v, err := c.server.Vault()
	if err != nil {
		return nil, err
	}

	var dirs []string
	if strings.TrimSpace(dir) != "" {
		dirs = []string{vault.CleanDir(dir)}
	} else {
		dirs = v.MovieFolders()
		// A vault with film details but no folder switched on — the switch
		// was turned off after a sweep, say — still has films worth listing.
		if len(dirs) == 0 {
			dirs = []string{"/"}
		}
	}

	seen := map[string]bool{}
	out := []assistant.Film{}
	for _, d := range dirs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		results, err := v.Search(vault.SearchOptions{
			Vault: c.scope, Query: "*", Dir: d, Kind: vault.SearchFiles, Limit: maxFilmsListed,
		})
		if err != nil {
			return nil, err
		}
		for _, hit := range results.Hits {
			if hit.File == nil || seen[hit.File.ID] {
				continue
			}
			info := v.Movie(hit.File.ID)
			if info == nil {
				continue
			}
			seen[hit.File.ID] = true
			out = append(out, assistant.Film{
				Title: info.Title, Year: info.Year, Path: hit.Path, TMDBID: info.TMDBID,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].Year < out[j].Year
	})
	return out, nil
}

// maxFilmsListed bounds one folder's walk. It is the search's own cap turned
// up: a listing is for comparing against a database search, and a film
// library of ten thousand is not one this is going to compare in a prompt.
const maxFilmsListed = 5000

// Search finds files and folders by name, exactly the way the search box
// does, and captions the matched videos with their film title.
func (c vaultCollection) Search(ctx context.Context, query, dir string, limit int) ([]assistant.Hit, error) {
	v, err := c.server.Vault()
	if err != nil {
		return nil, err
	}
	results, err := v.Search(vault.SearchOptions{
		Vault: c.scope, Query: query, Dir: dir, Limit: limit,
	})
	if err != nil {
		if errors.Is(err, vault.ErrEmptyQuery) {
			return nil, errors.New("search needs something to look for")
		}
		return nil, err
	}

	out := make([]assistant.Hit, 0, len(results.Hits))
	for _, hit := range results.Hits {
		h := assistant.Hit{Type: hit.Type, Path: hit.Path}
		if hit.File != nil {
			h.Size = hit.File.Size
			if !hit.File.ModifiedAt.IsZero() {
				h.Modified = hit.File.ModifiedAt.UTC().Format("2006-01-02")
			}
			if brief, ok := results.Movies[hit.File.ID]; ok {
				h.Film = brief.Title
				if brief.Year > 0 {
					h.Film = fmt.Sprintf("%s (%d)", brief.Title, brief.Year)
				}
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// FilmDatabase asks the film database about a title or a series. It uses
// the same client and the same key film matching does, so it is not a new
// place SAND talks to — and it is the assistant's one call that leaves the
// machine, carrying the query and nothing else.
func (c vaultCollection) FilmDatabase(ctx context.Context, query string) ([]assistant.Title, error) {
	client, err := c.server.movieClient()
	if err != nil {
		if errors.Is(err, movie.ErrNoKey) {
			return nil, errors.New("no film database key has been set in Settings, so the database cannot be searched")
		}
		return nil, err
	}
	candidates, err := client.Search(ctx, movie.Guess{Title: query})
	if err != nil {
		return nil, err
	}
	out := make([]assistant.Title, 0, len(candidates))
	for _, cand := range candidates {
		out = append(out, assistant.Title{
			Title: cand.Title, Year: cand.Year, TMDBID: cand.TMDBID, Overview: cand.Overview,
		})
	}
	return out, nil
}

// assistantFor builds the assistant for one request, from what the vault
// says about where the model runs.
func (s *Server) assistantFor(scope vault.Scope) (*assistant.Assistant, error) {
	v, err := s.Vault()
	if err != nil {
		return nil, err
	}
	settings := v.Assistant()
	if !settings.Configured() {
		return nil, assistant.ErrNotConfigured
	}
	tools := assistant.Tools(vaultCollection{server: s, scope: scope})
	if web := s.webFor(settings.Web); web != nil {
		tools = append(tools, assistant.WebTools(web)...)
	}
	return &assistant.Assistant{
		Model: &assistant.ChatCompletions{
			BaseURL: settings.URL, Model: settings.Model, APIKey: settings.APIKey,
		},
		Tools:         tools,
		ContextTokens: settings.ContextWindow(),
	}, nil
}

// webFor builds Sandy's way onto the web from the settings, or nil when the
// owner has not turned it on — in which case the web tools are not offered
// at all, and the prompt tells him to say so.
func (s *Server) webFor(settings vault.WebSettings) assistant.Web {
	var engine assistant.Searcher
	switch settings.Engine {
	case vault.WebEngineSearXNG:
		engine = &assistant.SearXNG{BaseURL: settings.URL}
	case vault.WebEngineOllama:
		engine = &assistant.OllamaSearch{Key: settings.Key, BaseURL: s.OllamaSearchURL}
	default:
		return nil
	}
	return assistant.Site{
		Searcher: engine,
		Fetcher:  &assistant.Fetcher{AllowPrivate: s.WebAllowPrivate},
	}
}
