package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The folder sweep for film details, run apart from the request that asked
// for it — the orphan sweep's shape (sweep_run.go), for the same reason. A
// folder of two hundred films is two hundred searches and two hundred posters
// over somebody's home connection, and while that was a request held open the
// dialog had to stay open with it, and a phone that slept or a tab that was
// closed cancelled the sweep. Nothing about it needs a page: the searching and
// the storing are the machine's.
//
// One at a time. The film database rate-limits by key, and two sweeps sharing
// one only make each other fail sooner. A second ask for the same folder joins
// the run going; any other is refused with 409 until it is done.
//
// Stopping keeps what was found. Every film is stored the moment it is
// matched, so a stopped sweep's report is as true as a finished one's, only
// shorter, and asking again picks up where it left off.

// movieScanProgress is where a sweep has got to: videos looked at of videos
// under the folder, how many of those were matched, and the one in hand.
type movieScanProgress struct {
	Path    string `json:"path"`
	Vault   string `json:"vault,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Matched int    `json:"matched"`
	Current string `json:"current,omitempty"`
}

// movieScanWatch holds the sweep that is running, or the last one that ran.
type movieScanWatch = jobWatch[movieScanProgress, ScanReport]

func newMovieScanWatch() *movieScanWatch {
	return newJobWatch[movieScanProgress, ScanReport]()
}

// movieScanScope names what a sweep was asked: the vault, the folder, and
// whether it looks everything up again. Two asks are the same ask only when
// all three agree.
func movieScanScope(scope vault.Scope, dir string, refresh bool) string {
	s := fmt.Sprintf("%s:%s", scope, dir)
	if refresh {
		s += " (again)"
	}
	return s
}

// handleMovieScanStart starts a sweep detached from this request, or joins
// the one already running for the same folder. 202 when it started one, 200
// when it joined one, 409 when another is running (GET says which).
//
// What can be refused at once is refused at once — a folder that is not
// opted in, a missing key — rather than as a run that fails a moment later,
// so the dialog can say so where the button is.
func (s *Server) handleMovieScanStart(w http.ResponseWriter, r *http.Request) {
	var req movieScanRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	scope := requestScope(r)
	dir := vault.CleanDir(req.Path)
	if !v.MovieLookupFor(scope, dir).Enabled {
		writeError(w, http.StatusForbidden, "film lookup is not turned on for "+dir, "MOVIE_LOOKUP_OFF")
		return
	}
	if _, err := s.movieClient(); err != nil {
		movieErrorResponse(w, err)
		return
	}

	refresh := req.Refresh
	status, outcome := s.movieScans.start(movieScanScope(scope, dir, refresh),
		func(ctx context.Context, onProgress func(movieScanProgress)) (*ScanReport, error) {
			return s.scanFolder(ctx, scope, dir, refresh, func(at movieScanProgress) {
				at.Vault = string(scope)
				onProgress(at)
				// Nobody may be watching, and the idle timer would otherwise
				// lock the vault out from under it.
				s.noteExternalActivity()
			})
		}, scanTimeout)

	switch outcome {
	case jobStarted:
		writeJSON(w, http.StatusAccepted, status)
	case jobJoined:
		writeJSON(w, http.StatusOK, status)
	default:
		writeError(w, http.StatusConflict,
			"films are already being looked up in another folder — wait for that to finish", "MOVIE_SCAN_BUSY")
	}
}

// handleMovieScanStatus answers with the sweep that is running, and how far it
// has got, or the last one that ran, and what it found. No run at all is an
// ordinary answer, not an error.
func (s *Server) handleMovieScanStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.movieScans.status())
}

// handleMovieScanStop stops the running sweep. It finishes with what it had
// matched so far, which stands as its report, marked as stopped.
func (s *Server) handleMovieScanStop(w http.ResponseWriter, r *http.Request) {
	s.movieScans.cancel()
	writeJSON(w, http.StatusOK, s.movieScans.status())
}
