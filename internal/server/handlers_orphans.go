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
// One GET and three POSTs, for the same reason recovery is three: looking is
// safe and acting is not, so looking is a GET that anything may call on a
// schedule and each of the three things that can be done about what it found —
// erase the abandoned parts, record the mislaid ones back, erase the working
// files left in the vault's own directory — is a POST naming exactly what it
// agreed to.

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

// leftoverSweepRequest names the working files to erase from the vault's own
// directory.
type leftoverSweepRequest struct {
	// Names are the file names from a scan's leftovers, which is what the
	// browser sends back from the rows somebody ticked. Empty means every one
	// the vault currently considers finished with — what "clean them all up"
	// means, and what a bare POST from a script means too.
	//
	// Only a name the fresh scan makes again is ever acted on, so a request
	// naming something outside the vault's directory reaches nothing.
	Names []string `json:"names"`

	// DryRun asks what would go without anything going.
	DryRun bool `json:"dry_run"`
}

// handleLeftoverSweep erases the working files SAND left in its own directory.
//
// The local half of the same housekeeping: a spool an interrupted upload left
// behind is the whole file it was sending, in plaintext, and there is one per
// upload that did not finish. Reading them is part of the scan above; erasing
// them is here, and re-scans first so that a spool something has started
// writing to since is skipped rather than pulled out from under it. See
// internal/vault/leftovers.go.
func (s *Server) handleLeftoverSweep(w http.ResponseWriter, r *http.Request) {
	var req leftoverSweepRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, v.SweepLeftovers(req.Names, req.DryRun))
}

// orphanSweepKey is the erase watch's slot for the orphan sweep. Folder
// deletes key their windows by scope and path, and every one of those keys
// carries a NUL between the two (see eraseKey), so this name cannot collide
// with any of them.
const orphanSweepKey = "orphan-sweep"

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

	// Counted as it goes, so GET /api/vault/orphans/erasing can answer whoever
	// is waiting on this request — which for that same vault runs for minutes
	// and says nothing until the end. The window opens at (0, 0), which reads
	// as "running, still deciding what goes": the sweep lists every account
	// again before its first delete, and that stretch deserves to look like
	// work rather than like a hang. Cleared however the sweep comes out. A dry
	// run opens no window at all — it erases nothing, and a browser asking
	// what a sweep would do must not make one that is running look finished.
	var onProgress func(done, total int)
	if !req.DryRun {
		s.erases.set(orphanSweepKey, folderErase{})
		defer s.erases.clear(orphanSweepKey)
		onProgress = func(done, total int) {
			s.erases.set(orphanSweepKey, folderErase{Done: done, Total: total})
		}
	}

	v, _ := s.Vault()
	report, err := v.SweepOrphans(ctx, req.Targets, req.DryRun, onProgress)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleOrphanErasing answers with where a running sweep has got to — the same
// window handleFolderErasing opens onto a recursive folder delete, for the
// same reason: the POST it watches can only answer at the end. Not running is
// an ordinary answer rather than an error, because the poller and the sweep it
// watches race. While running, a total of zero means the sweep is still
// listing the accounts to decide what goes.
func (s *Server) handleOrphanErasing(w http.ResponseWriter, r *http.Request) {
	at, ok := s.erases.get(orphanSweepKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"running": ok,
		"done":    at.Done,
		"total":   at.Total,
	})
}
