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

// reattachRequest asks for the mislaid shards to be written back.
type reattachRequest struct {
	// DryRun asks how many would go back without the index being touched.
	DryRun bool `json:"dry_run"`
}

// handleOrphanReattach writes back the index records for shards sitting on a
// connected account with nothing pointing at them.
//
// The repair for a disconnect, which drops those records deliberately and
// leaves the objects where they are. It transfers nothing — a part's object key
// is derived from the archive ID and the shard number, so the bytes are already
// exactly where a record would say they are — and it is purely additive, which
// is why it carries none of the sweep's refusals. See ReattachShards.
func (s *Server) handleOrphanReattach(w http.ResponseWriter, r *http.Request) {
	var req reattachRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	// A listing of every account plus one index write. Long enough for the
	// first on a slow cloud; the write itself is local.
	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.ReattachShards(ctx, req.DryRun)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
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
