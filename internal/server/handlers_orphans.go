package server

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Parts on the accounts that the index has stopped pointing at, over HTTP.
//
// The browser is the right place for this because the browser is where the
// gap opens: somebody reconnects a cloud, the app notices that the returning
// account is carrying archives no index names, and it can say so at the moment
// it is relevant rather than leaving the room to be paid for indefinitely. See
// internal/vault/orphans.go for how the gap opens and why the scan refuses to
// act on its own in the cases it refuses.
//
// Two endpoints and not one, for the same reason recovery is three: looking is
// safe and erasing is not, so looking is a GET that anything may call on a
// schedule and erasing is a POST that names exactly what it agreed to.

// handleOrphanScan reports the abandoned parts on every connected account.
//
// One listing and one small download per account — the same cost as the
// recovery scan, which is why the frontend asks when the set of connected
// accounts changes rather than on every render.
func (s *Server) handleOrphanScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	scan, err := v.ScanForOrphans(ctx)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scan)
}

// orphanSweepRequest names what to erase.
type orphanSweepRequest struct {
	// Targets are archive-and-account pairs from a scan. Empty means every
	// abandoned archive the vault currently considers safe to erase, which is
	// what "clean them all up" means — and what a bare POST from a script
	// means too.
	Targets []vault.OrphanTarget `json:"targets"`

	// DryRun asks what would go without anything going. The browser runs one
	// before the confirmation, so what it promises is measured against the
	// accounts as they are now rather than as the scan left them.
	DryRun bool `json:"dry_run"`
}

// handleOrphanSweep erases the parts nothing points at.
//
// The sweep re-scans before it deletes, so a target that has stopped being
// abandoned between the scan and the confirmation is skipped and said so rather
// than erased. A vault in one of the states where "unaccounted for" is not the
// same as "abandoned" refuses outright — see orphanGuard.
func (s *Server) handleOrphanSweep(w http.ResponseWriter, r *http.Request) {
	var req orphanSweepRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	// Long enough for a listing of every account plus a delete per object, on
	// a vault where somebody has been deleting films.
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.SweepOrphans(ctx, req.Targets, req.DryRun)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
