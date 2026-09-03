package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Old versions of SAND's objects on the buckets that keep them, over HTTP.
//
// A bucket with versioning switched on — Backblaze B2's default — stores every
// rewrite of the index backup and every part SAND ever deleted beneath what a
// listing shows, and bills for all of it. The browser is where somebody sees
// the usage bar say one thing and their provider say another, so it is where
// the answer belongs. See internal/vault/versions.go for what is stale, what
// is never touched, and why.
//
// Looking is a GET and erasing is a POST, and the POST re-scans before it
// deletes anything, for the reason the orphan sweep does.

// handleVersionScan reports what every versioned account is storing beneath
// the objects it shows. One listing of versions and one small download per
// account, the same cost as the orphan scan.
func (s *Server) handleVersionScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	scan, err := v.ScanForStaleVersions(ctx)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scan)
}

// versionSweepRequest names what to erase.
type versionSweepRequest struct {
	// Accounts are the connected accounts to sweep, by ID. Empty means every
	// one that keeps versions, which is what "clean them all up" means — and
	// what a bare POST from a script means too.
	Accounts []string `json:"accounts"`

	// DryRun asks what would go without anything going.
	DryRun bool `json:"dry_run"`
}

// versionSweepKey is the erase watch's slot for the version sweep. See
// orphanSweepKey for why the name cannot collide with a folder delete's.
const versionSweepKey = "version-sweep"

// handleVersionSweep erases the stale versions under SAND's own keys on the
// accounts named, leaving the current version of every object where it is.
func (s *Server) handleVersionSweep(w http.ResponseWriter, r *http.Request) {
	var req versionSweepRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	// A listing of versions per account plus a delete per version. A bucket
	// that has kept a year of index backups has thousands of them.
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	// Counted as it goes, for GET /api/vault/versions/erasing — the same
	// window the orphan sweep opens, for the same reason. A dry run opens no
	// window at all.
	var onProgress func(done, total int)
	if !req.DryRun {
		s.erases.set(versionSweepKey, folderErase{})
		defer s.erases.clear(versionSweepKey)
		onProgress = func(done, total int) {
			s.erases.set(versionSweepKey, folderErase{Done: done, Total: total})
		}
	}

	v, _ := s.Vault()
	report, err := v.SweepStaleVersions(ctx, req.Accounts, req.DryRun, onProgress)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleVersionErasing answers with where a running version sweep has got to.
// Not running is an ordinary answer rather than an error, because the poller
// and the sweep it watches race.
func (s *Server) handleVersionErasing(w http.ResponseWriter, r *http.Request) {
	at, ok := s.erases.get(versionSweepKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"running": ok,
		"done":    at.Done,
		"total":   at.Total,
	})
}

// pruneTick is how often the loop below asks whether a scheduled prune is due.
// The asking is arithmetic over one timestamp; no account is contacted unless
// something is actually due.
const pruneTick = time.Minute

// pruneTimeout is the ceiling on one scheduled prune: a listing of versions
// per account plus a delete per stale version, on the day the setting was
// first switched on for a bucket that has kept a year of history.
const pruneTimeout = 30 * time.Minute

// pruneLoop erases the old versions off the accounts that asked for it, once
// a day, while the vault is open. Nothing at all while it is locked: the
// setting is in the vault file, and a locked vault has no accounts.
//
// Said in the log only when something went, or something went wrong. This
// runs every day for as long as the machine is up, and a line saying the
// bucket was already tidy is not news.
func (s *Server) pruneLoop() {
	ticker := time.NewTicker(pruneTick)
	defer ticker.Stop()

	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() || !v.AutoPruneDue(time.Now()) {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), pruneTimeout)
		report, err := v.AutoPrune(ctx)
		cancel()
		switch {
		case err != nil:
			// A vault locked between the check above and the prune itself
			// is the ordinary case here, and is not news.
			if !errors.Is(err, vault.ErrLocked) {
				log.Printf("scheduled prune failed: %v", err)
			}
		case report.Deleted > 0 || len(report.Warnings) > 0:
			log.Printf("scheduled prune erased %d old version(s), %d of them delete markers, freeing %s",
				report.Deleted, report.Markers, formatBytes(report.Bytes))
			for _, warning := range report.Warnings {
				log.Printf("scheduled prune: %s", warning)
			}
		}
	}
}
