package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/chinmay28/sand-vault/internal/movie"
	"github.com/chinmay28/sand-vault/internal/thumb"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// Matching a folder of videos against the film database is orchestration, and
// orchestration lives here rather than in the vault: the vault stores what a
// match found, the movie package knows how to ask, and neither of them has to
// know about the other.
//
// Three things shape the way the sweep is written.
//
// It never touches a folder that has not been opted in. The check is made here,
// against the index, rather than trusted from the request — a tab asking to
// scan somewhere else is asking to send filenames from a folder nobody agreed
// to.
//
// It writes each folder's posters once. A thumbnail pack is a single stored
// object per folder, so storing pictures one at a time would re-upload the
// whole growing pack per film. See vault.SetThumbs.
//
// And it can be run again safely. A film already matched is left alone unless
// its poster is missing, which is exactly the state a password change leaves
// behind: the details survive in the index, every thumbnail is dropped.
// Re-running then costs one image fetch per film and no searches at all.

// movieClient builds a database client from the key the vault is holding.
func (s *Server) movieClient() (*movie.Client, error) {
	v, err := s.Vault()
	if err != nil {
		return nil, err
	}
	key := v.MovieAPIKey()
	if strings.TrimSpace(key) == "" {
		return nil, movie.ErrNoKey
	}
	return &movie.Client{Key: key, BaseURL: s.MovieBaseURL, ImageBaseURL: s.MovieImageBaseURL}, nil
}

// isVideo reports whether a file is worth asking the film database about.
//
// Deliberately generous, and by extension rather than by type: a .mkv usually
// arrives with no recognised MIME type at all — Go's built-in table stops at
// the handful of web formats — and it is the single most likely thing in a
// films folder.
func isVideo(entry *vault.Entry) bool {
	if strings.HasPrefix(strings.ToLower(entry.MIME), "video/") {
		return true
	}
	switch strings.ToLower(path.Ext(entry.Name)) {
	case ".mkv", ".mp4", ".m4v", ".mov", ".avi", ".wmv", ".flv", ".mpg", ".mpeg",
		".m2ts", ".ts", ".webm", ".ogv", ".divx", ".vob", ".iso", ".3gp":
		return true
	}
	return false
}

// MatchResult is what happened to one file.
type MatchResult struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Query string `json:"query,omitempty"`

	// Title is the film it was matched to, empty when nothing was.
	Title string `json:"title,omitempty"`
	Year  int    `json:"year,omitempty"`
}

// ScanReport is the outcome of sweeping a folder.
type ScanReport struct {
	Path string `json:"path"`

	// Considered is how many videos were found, Matched how many came back with
	// a film, Skipped how many already had one.
	Considered int `json:"considered"`
	Matched    int `json:"matched"`
	Skipped    int `json:"skipped"`

	// Artwork counts the posters stored, which is not the same as Matched: a
	// film the database has no poster for still matches, and a re-run stores
	// artwork for films it did not have to look up again.
	Artwork int `json:"artwork"`

	// Unmatched lists the files the database had nothing for, so the answer can
	// say which ones want correcting by hand rather than only how many.
	Unmatched []MatchResult `json:"unmatched,omitempty"`

	// Warnings are the failures that did not stop the sweep.
	Warnings []string `json:"warnings,omitempty"`
}

// maxUnmatchedReported caps the list of misses one report carries back. A
// folder of holiday videos would otherwise answer with every one of them.
const maxUnmatchedReported = 40

// scanFolder matches every video at or below a folder that has not been
// matched already.
//
// It refuses unless the folder has been opted in, which is the whole consent
// model: nothing here reaches the network for a folder nobody asked about.
func (s *Server) scanFolder(ctx context.Context, scope vault.Scope, dir string, refresh bool) (*ScanReport, error) {
	v, err := s.Vault()
	if err != nil {
		return nil, err
	}
	dir = vault.CleanDir(dir)

	if !v.MovieLookupFor(scope, dir).Enabled {
		return nil, fmt.Errorf("film lookup is not turned on for %s", dir)
	}
	client, err := s.movieClient()
	if err != nil {
		return nil, err
	}

	entries, err := v.Descendants(dir)
	if err != nil {
		return nil, err
	}

	report := &ScanReport{Path: dir}
	// Posters are collected per folder and written per folder, so a sweep over a
	// tree of one-film folders costs one pack write each rather than one per
	// film per folder.
	posters := map[string]map[string][]byte{}
	drawn := thumbedIDs(v, entries)

	for _, entry := range entries {
		if !isVideo(entry) {
			continue
		}
		report.Considered++

		if known := v.Movie(entry.ID); known != nil {
			// A match somebody corrected by hand stays corrected, even when the
			// whole folder is asked for again.
			if !refresh || known.Manual {
				report.Skipped++
				if !drawn[entry.ID] {
					// Matched already, but the picture is gone. One image
					// fetch, no search.
					if poster := fetchPoster(ctx, client, known.PosterPath); poster != nil {
						collect(posters, entry.Dir, entry.ID, poster)
						report.Artwork++
					}
				}
				continue
			}
		}

		info, poster, err := s.match(ctx, client, entry, movie.ParseIn(entry.Dir, entry.Name), 0)
		if err != nil {
			// A rejected key, a rate limit or a dead connection will fail
			// identically for every remaining file, so stop rather than
			// spending two hundred requests to say so two hundred times.
			// Everything matched up to here is already stored, and asking
			// again picks up from there.
			if errors.Is(err, movie.ErrNoKey) || errors.Is(err, movie.ErrRejectedKey) ||
				errors.Is(err, movie.ErrRateLimited) || errors.Is(err, vault.ErrLocked) ||
				ctx.Err() != nil {
				// The posters gathered so far are worth keeping even though the
				// sweep is ending badly: they are already paid for.
				storePosters(ctx, v, posters, report)
				return nil, err
			}
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %v", entry.Name, err))
			continue
		}
		if info == nil {
			if len(report.Unmatched) < maxUnmatchedReported {
				report.Unmatched = append(report.Unmatched, MatchResult{
					ID:    entry.ID,
					Name:  entry.Name,
					Dir:   entry.Dir,
					Query: movie.ParseIn(entry.Dir, entry.Name).String(),
				})
			}
			continue
		}

		report.Matched++
		if poster != nil {
			collect(posters, entry.Dir, entry.ID, poster)
			report.Artwork++
		}
	}

	storePosters(ctx, v, posters, report)

	sort.Slice(report.Unmatched, func(i, j int) bool {
		return report.Unmatched[i].Name < report.Unmatched[j].Name
	})
	return report, nil
}

// match looks one file up and stores what came back, handing the poster to the
// caller rather than storing it: a sweep wants to write a folder's pictures in
// one go, and only the caller knows whether it is in the middle of one.
//
// A file the database has nothing for comes back as a nil Info and no error.
// Most folders hold something that is not a film, and that is not a failure.
//
// A tmdbID of zero means "search for the guess"; anything else is a film
// somebody picked by hand out of a list of candidates, and is recorded as such
// so a later sweep leaves it alone.
func (s *Server) match(
	ctx context.Context, client *movie.Client, entry *vault.Entry, guess movie.Guess, tmdbID int,
) (*movie.Info, []byte, error) {
	var (
		info *movie.Info
		err  error
	)

	if tmdbID > 0 {
		info, err = client.Details(ctx, tmdbID)
		if err != nil {
			return nil, nil, err
		}
		info.Manual = true
	} else {
		if guess.Empty() {
			return nil, nil, nil
		}
		info, err = client.Lookup(ctx, guess)
		if err != nil || info == nil {
			return nil, nil, err
		}
	}

	info.Query = guess.String()
	info.File = entry.Name

	v, err := s.Vault()
	if err != nil {
		return nil, nil, err
	}
	if err := v.SetMovie(entry.ID, info); err != nil {
		return nil, nil, err
	}
	return info, fetchPoster(ctx, client, info.PosterPath), nil
}

// thumbedIDs is the set of files in a sweep that already have a picture. It is
// answered out of the index — one lookup per distinct folder, no account
// contacted — because the only question being asked is whether a poster has to
// be fetched again.
func thumbedIDs(v *vault.Vault, entries []*vault.Entry) map[string]bool {
	out := map[string]bool{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.Dir] {
			continue
		}
		seen[entry.Dir] = true
		scope, _ := v.ScopeOf(entry.ID)
		for _, id := range v.ThumbIDs(scope, entry.Dir) {
			out[id] = true
		}
	}
	return out
}

// storePosters writes each folder's gathered artwork in one go, and forgets it
// so a second call cannot store the same pictures twice.
//
// A pack that will not store is a warning and nothing more: the details are in
// the index already, and those rows fall back to the icons they have always
// shown.
func storePosters(ctx context.Context, v *vault.Vault, posters map[string]map[string][]byte, report *ScanReport) {
	for folder, batch := range posters {
		// Every picture in a batch belongs to one folder, so any of its files
		// names the vault the pack goes in.
		scope := vault.MainScope
		for id := range batch {
			if found, ok := v.ScopeOf(id); ok {
				scope = found
				break
			}
		}
		if err := v.SetThumbs(ctx, scope, folder, batch); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("stored the details but not the artwork for %s: %v", folder, err))
		}
		delete(posters, folder)
	}
}

// collect files a poster under the folder whose pack will hold it.
func collect(posters map[string]map[string][]byte, dir, id string, data []byte) {
	batch := posters[dir]
	if batch == nil {
		batch = map[string][]byte{}
		posters[dir] = batch
	}
	batch[id] = data
}

// fetchPoster gets the artwork and normalizes it into the vault's own JPEG.
//
// Every failure is a nil rather than an error, deliberately: a film with no
// poster, an image host having a bad minute, bytes that will not decode — none
// of those is a reason to leave the details unstored. The list has always been
// readable without pictures.
func fetchPoster(ctx context.Context, client *movie.Client, posterPath string) []byte {
	if strings.TrimSpace(posterPath) == "" {
		return nil
	}
	raw, err := client.Poster(ctx, posterPath)
	if err != nil {
		return nil
	}
	// Re-encoded rather than stored as it arrived: what came back is a
	// stranger's file, and a thumbnail in this vault is always the vault's own
	// JPEG at the vault's own size.
	normalized, err := thumb.Normalize(raw)
	if err != nil {
		return nil
	}
	return normalized
}
