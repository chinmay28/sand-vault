package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/movie"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// scanTimeout is the ceiling on one folder sweep.
//
// Long, because a folder of two hundred films is two hundred searches, two
// hundred detail fetches and two hundred posters over somebody's home
// connection — and because the request holds open for the duration, the way
// converting a file does. It is safe to be this long because a sweep commits as
// it goes: every film is stored the moment it is matched, so a timeout costs
// the artwork of the folder in flight and asking again picks up the rest.
const scanTimeout = 2 * time.Hour

// lookupTimeout is the ceiling on one file's worth of lookup — a search, a
// details fetch and a poster.
const lookupTimeout = 2 * time.Minute

// movieErrorResponse maps the film database's failures onto codes the browser
// switches on. They are worth telling apart: a missing key wants the settings
// dialog, a rejected one wants a different key typed in, and neither is a
// problem with the vault.
func movieErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, movie.ErrNoKey):
		writeError(w, http.StatusPreconditionFailed,
			"no film database key has been set — add one before looking anything up", "NO_MOVIE_KEY")
	case errors.Is(err, movie.ErrRejectedKey):
		writeError(w, http.StatusBadGateway, movie.ErrRejectedKey.Error(), "BAD_MOVIE_KEY")
	case errors.Is(err, movie.ErrNotFound):
		writeError(w, http.StatusNotFound, movie.ErrNotFound.Error(), "NOT_FOUND")
	default:
		vaultErrorResponse(w, err)
	}
}

// handleMovieSettings reports whether a film database key has been stored, and
// which folders are opted in. Never the key itself: it is a credential, and the
// browser has no use for one it already gave.
func (s *Server) handleMovieSettings(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, map[string]any{
		"has_key": v.MovieAPIKey() != "",
		"folders": v.MovieFolders(),
	})
}

type movieKeyRequest struct {
	// Key is the user's own film database key. An empty one clears it, which
	// stops every lookup this vault could make.
	Key string `json:"key"`
}

// handleMovieKey stores the film database key, after checking that it works.
//
// Checked because the alternative is finding out during a sweep of two hundred
// films, and because one request to the database's configuration endpoint is
// the cheapest possible way to ask.
func (s *Server) handleMovieKey(w http.ResponseWriter, r *http.Request) {
	var req movieKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	key := strings.TrimSpace(req.Key)

	if key != "" {
		ctx, cancel := contextWithTimeout(r, movie.RequestTimeout+5*time.Second)
		defer cancel()

		client := &movie.Client{Key: key, BaseURL: s.MovieBaseURL, ImageBaseURL: s.MovieImageBaseURL}
		if err := client.Ping(ctx); err != nil {
			movieErrorResponse(w, err)
			return
		}
	}

	if err := v.SetMovieAPIKey(key); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"has_key": key != ""})
}

type movieLookupRequest struct {
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

// handleMovieLookup turns film matching on or off for a folder and everything
// under it. It stores the setting and nothing else — sweeping is a second,
// explicit request, so that turning the switch on never silently sends a folder
// of filenames anywhere.
func (s *Server) handleMovieLookup(w http.ResponseWriter, r *http.Request) {
	var req movieLookupRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.SetMovieLookup(req.Path, req.Enabled); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":   vault.CleanDir(req.Path),
		"lookup": v.MovieLookupFor(req.Path),
	})
}

type movieScanRequest struct {
	Path string `json:"path"`

	// Refresh asks for films that already have details to be looked up again.
	// Off by default: a sweep is normally about what is new.
	Refresh bool `json:"refresh"`
}

// handleMovieScan matches every unmatched video under a folder.
func (s *Server) handleMovieScan(w http.ResponseWriter, r *http.Request) {
	var req movieScanRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, scanTimeout)
	defer cancel()

	report, err := s.scanFolder(ctx, req.Path, req.Refresh)
	if err != nil {
		movieErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleMovieGet returns what is stored about one file's film. A file that has
// not been matched is not an error — the details view opens on it and offers to
// look it up.
func (s *Server) handleMovieGet(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	entry, err := v.Entry(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"movie": v.Movie(entry.ID),
		// What the filename says, whether or not anything has been looked up.
		// It is what the search box opens on when a match has to be corrected,
		// and what the view shows against a film to say how it was found.
		"guess":  movie.ParseIn(entry.Dir, entry.Name),
		"lookup": v.MovieLookupFor(entry.Dir),
	})
}

type movieMatchRequest struct {
	// Query is what to search for instead of the guess off the filename. It is
	// how a wrong match gets corrected when the right film is simply spelled
	// differently.
	Query string `json:"query"`
	Year  int    `json:"year"`

	// TMDBID names a film outright, which is what choosing one from the
	// candidate list does. It wins over Query and is recorded as a match made
	// by hand, so a later sweep of the folder leaves it alone.
	TMDBID int `json:"tmdb_id"`
}

// handleMovieMatch looks one file up and stores what comes back, on demand:
// the file a sweep missed, or the one it got wrong.
func (s *Server) handleMovieMatch(w http.ResponseWriter, r *http.Request) {
	var req movieMatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	entry, err := v.Entry(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	// The same consent check the sweep makes, for the same reason: this is a
	// request that leaves the machine.
	if !v.MovieLookupFor(entry.Dir).Enabled {
		writeError(w, http.StatusForbidden,
			"film lookup is not turned on for "+entry.Dir, "MOVIE_LOOKUP_OFF")
		return
	}

	client, err := s.movieClient()
	if err != nil {
		movieErrorResponse(w, err)
		return
	}

	guess := movie.ParseIn(entry.Dir, entry.Name)
	if query := strings.TrimSpace(req.Query); query != "" {
		guess = movie.Guess{Title: query, Year: req.Year}
	}

	ctx, cancel := contextWithTimeout(r, lookupTimeout)
	defer cancel()

	info, poster, err := s.match(ctx, client, entry, guess, req.TMDBID)
	if err != nil {
		movieErrorResponse(w, err)
		return
	}
	if info == nil {
		writeJSON(w, http.StatusOK, map[string]any{"movie": nil, "guess": guess})
		return
	}

	// One file, so the folder's pack is written once either way. A poster that
	// will not store is a warning and nothing more: the details are already in
	// the index, and the row falls back to the icon it has always shown.
	var warnings []string
	if poster != nil {
		if err := v.SetThumbs(ctx, entry.Dir, map[string][]byte{entry.ID: poster}); err != nil {
			warnings = append(warnings, "stored the details but not the artwork: "+err.Error())
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"movie":    info,
		"guess":    guess,
		"artwork":  poster != nil && len(warnings) == 0,
		"warnings": warnings,
	})
}

// handleMovieCandidates searches the database without storing anything, so a
// wrong match can be corrected against a list of real films rather than by
// retyping a query and hoping.
func (s *Server) handleMovieCandidates(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	entry, err := v.Entry(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if !v.MovieLookupFor(entry.Dir).Enabled {
		writeError(w, http.StatusForbidden,
			"film lookup is not turned on for "+entry.Dir, "MOVIE_LOOKUP_OFF")
		return
	}

	client, err := s.movieClient()
	if err != nil {
		movieErrorResponse(w, err)
		return
	}

	guess := movie.ParseIn(entry.Dir, entry.Name)
	if query := strings.TrimSpace(r.URL.Query().Get("q")); query != "" {
		guess = movie.Guess{Title: query}
	}
	if guess.Empty() {
		writeError(w, http.StatusBadRequest, "nothing to search for", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, lookupTimeout)
	defer cancel()

	candidates, err := client.Search(ctx, guess)
	if err != nil {
		movieErrorResponse(w, err)
		return
	}
	if candidates == nil {
		candidates = []movie.Candidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"guess": guess, "candidates": candidates})
}

// handleMovieForget drops a file's stored details.
//
// The poster is left alone, and that is not an oversight: by the time it is
// stored it is the file's thumbnail, indistinguishable from a picture the
// browser drew of any other file, and thumbnails are dealt with as thumbnails.
func (s *Server) handleMovieForget(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	if _, err := v.Entry(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if err := v.ForgetMovie(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "forgotten"})
}
