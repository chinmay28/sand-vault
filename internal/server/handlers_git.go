package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/git"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// The repositories a vault is keeping a copy of, over HTTP.
//
// Four endpoints, and the split between them is the same one the design turns
// on: listing is index work and answers in microseconds, while tracking and
// refreshing talk to somebody else's git server and can move a gigabyte, so
// they get deadlines of their own.
//
// See internal/vault/gitrepo.go for what a repository in a vault actually is —
// one bundle file — and internal/git for why SAND borrows the system git rather
// than carrying its own.

// gitTrackTimeout is the ceiling on cloning a repository for the first time.
//
// Generous, because the first one is the expensive one: it is the entire
// history of a project coming down over somebody's home connection and going
// back up to three clouds, and a Linux kernel is not a five-minute job. Every
// refresh after it costs the difference instead.
const gitTrackTimeout = 6 * time.Hour

// gitRefreshTimeout is the ceiling on bringing one repository up to date by
// hand from the browser.
const gitRefreshTimeout = 2 * time.Hour

// gitTrackRequest is a repository somebody wants kept.
type gitTrackRequest struct {
	Vault string `json:"vault,omitempty"`

	// Path is the folder the bundle is stored in, URL the repository to mirror.
	Path string `json:"path"`
	URL  string `json:"url"`

	// Accounts and Scheme are the ordinary upload choices, because a stored
	// repository is an ordinary file and there is no reason it should not get
	// the same say over where it lands as a photograph does.
	Accounts []string `json:"accounts,omitempty"`
	Scheme   string   `json:"scheme,omitempty"`
}

// handleGitList answers with the repositories stored under a folder, or under
// the whole vault when no folder is named.
//
// It also says whether this machine has a git at all, because that is what the
// browser needs to decide between offering to track a repository and explaining
// why it cannot.
func (s *Server) handleGitList(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()

	dir := strings.TrimSpace(r.URL.Query().Get("path"))
	if dir == "" {
		dir = "/"
	}
	repos, err := v.TrackedRepos(requestScope(r), dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if repos == nil {
		repos = []vault.TrackedRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repos":     repos,
		"folder":    dir,
		"available": git.Available(),
	})
}

// handleGitTrack mirrors a repository and stores it as a bundle.
func (s *Server) handleGitTrack(w http.ResponseWriter, r *http.Request) {
	var req gitTrackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "give the repository's URL", "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		req.Path = "/"
	}

	scheme, err := parseSchemeField(req.Scheme)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, gitTrackTimeout)
	defer cancel()

	v, _ := s.Vault()
	repo, warnings, err := v.TrackRepo(ctx, vault.Scope(req.Vault), req.Path, req.URL,
		vault.UploadOptions{Accounts: req.Accounts, Scheme: scheme})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo":     repo,
		"warnings": warnings,
	})
}

// handleGitRefresh brings one stored repository up to date now.
//
// The answer says whether anything actually moved, because on most days nothing
// has and "already up to date" is the useful thing to be told — it means the
// check cost one ref advertisement and nothing else.
func (s *Server) handleGitRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "name the repository", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, gitRefreshTimeout)
	defer cancel()

	v, _ := s.Vault()
	repo, moved, err := v.RefreshRepo(ctx, requestScope(r), id)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo":    repo,
		"updated": moved,
	})
}

// handleGitUntrack stops SAND asking a repository's upstream anything, leaving
// the stored bundle exactly where it is.
func (s *Server) handleGitUntrack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "name the repository", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.UntrackRepo(requestScope(r), id); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"untracked": id})
}
