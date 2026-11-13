package server

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// The reattach as something that runs on its own, rather than inside the
// request that asked for it.
//
// POST /api/vault/orphans/reattach is one request that answers at the end, and
// the end is a listing of every account away — minutes, on a slow cloud. A
// page closed or navigated away from in that time cancels the request, and
// with it the repair: nothing is recorded, and the files go on saying they are
// short of a part that is sitting on a connected cloud. So the browser starts
// it here instead, on a context of its own, and polls for where it has got to
// — the listing's progress, cloud by cloud, then the write — and what it
// recorded once it is done. Closing the page keeps it; a page that comes back
// finds it still going, or the report waiting.
//
// One at a time, scoped by the account it lists (empty for every account). A
// second ask for the same scope joins the run going; an ask for another scope
// while one runs is refused with 409 rather than queued, since the first will
// often have recorded what the second was after by the time it finishes.

// reattachWatch holds the reattach that is running, or the last one that ran.
type reattachWatch = jobWatch[vault.OrphanScanProgress, vault.ReattachReport]

// reattachStatus is what a poll is answered with — see jobStatus. The report
// is under `result`, and `scope` is the account it was pointed at.
type reattachStatus = jobStatus[vault.OrphanScanProgress, vault.ReattachReport]

func newReattachWatch() *reattachWatch {
	return newJobWatch[vault.OrphanScanProgress, vault.ReattachReport]()
}

// reattachStartRequest names the account to list, or none for every one.
type reattachStartRequest struct {
	ProviderID string `json:"provider_id"`
}

// handleReattachStart starts the reattach detached from this request, or joins
// the one already running for the same account.
//
// 202 with the run when it started one, 200 when it joined one, 409 when one
// is going for a different account (GET says which). A dry run is not
// offered here: it is the same listing with nothing to wait for at the end,
// and POST /api/vault/orphans/reattach answers it.
func (s *Server) handleReattachStart(w http.ResponseWriter, r *http.Request) {
	var req reattachStartRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	status, outcome := s.reattaches.start(req.ProviderID, func(ctx context.Context, onProgress func(vault.OrphanScanProgress)) (*vault.ReattachReport, error) {
		report, err := v.ReattachShards(ctx, vault.ReattachOptions{
			ProviderID: req.ProviderID,
			OnProgress: func(at vault.OrphanScanProgress) {
				onProgress(at)
				// Nobody may be watching, and the idle timer would otherwise
				// lock the vault out from under it.
				s.noteExternalActivity()
			},
		})
		if err == nil && report.Shards > 0 {
			// The last stray-parts scan lists these as mislaid, and they are
			// not any more. A scan still running is left to finish: it lists
			// the accounts after this, or races it, and either way the panel
			// that shows it re-scans once a repair has been made.
			s.strayScans.forgetFinished()
		}
		return report, err
	}, strayScanTimeout)

	switch outcome {
	case jobStarted:
		writeJSON(w, http.StatusAccepted, status)
	case jobJoined:
		writeJSON(w, http.StatusOK, status)
	default:
		// The run in the way is one GET away; the browser shows that one
		// rather than racing it, so this is for a second tab or a script.
		writeError(w, http.StatusConflict,
			"a reattach is already running for another cloud — wait for it to finish", "REATTACH_BUSY")
	}
}

// handleReattachStatus answers with the reattach that is running, and where it
// has got to, or the last one that ran, and what it recorded. No run at all is
// an ordinary answer, not an error.
func (s *Server) handleReattachStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reattaches.status())
}
