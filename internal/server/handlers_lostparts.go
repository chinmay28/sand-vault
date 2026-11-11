package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Parts the index records on an account that the account is not holding,
// over HTTP. See internal/vault/lostparts.go for what they are and how one is
// put back.
//
// Two jobs, each run apart from the request that asked for it, for the reason
// the stray-parts scan is (orphan_scan.go): the scan is a listing of every
// account, and the restore is that listing and then a transfer per lost
// object, so neither fits in a request a closed page would cancel.
//
//	POST /api/vault/lost-parts/scan      start a scan, or join the one running
//	GET  /api/vault/lost-parts/scan      where it has got to, then what it found
//	POST /api/vault/lost-parts/restore   start putting them back
//	GET  /api/vault/lost-parts/restore   where that has got to, then what it did
//	POST /api/vault/lost-parts/restore/stop
//
// Looking is safe and is offered freely. Putting back writes to an account,
// and is its own POST for the reason a sweep is — with one more: an account
// that lists as holding none of its parts may be an emptied folder or may be
// a folder that is not the one the parts are in, and a listing cannot say
// which. The scan flags it (`emptied`); the restore is what somebody asks for
// after reading that.

// lostScanWatch holds the lost-parts scan that is running, or the last one.
type lostScanWatch = jobWatch[vault.OrphanScanProgress, vault.LostPartsScan]

// lostRestoreWatch holds the restore that is running, or the last one.
type lostRestoreWatch = jobWatch[vault.LostRestoreProgress, vault.LostRestoreReport]

// handleLostScanStart starts a scan of every account for parts that are not
// where the index records them, or joins the one already running. 202 when it
// started one, 200 when it joined.
func (s *Server) handleLostScanStart(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	status, outcome := s.lostScans.start("", func(ctx context.Context, onProgress func(vault.OrphanScanProgress)) (*vault.LostPartsScan, error) {
		return v.ScanForLostParts(ctx, vault.LostScanOptions{
			OnProgress: func(at vault.OrphanScanProgress) {
				onProgress(at)
				// Nobody may be watching, and the idle timer would
				// otherwise lock the vault out from under it.
				s.noteExternalActivity()
			},
		})
	}, strayScanTimeout)
	writeJSON(w, startCode(outcome), status)
}

// handleLostScanStatus answers with the scan that is running, or the last one
// that ran and what it found. No run at all is an ordinary answer.
func (s *Server) handleLostScanStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lostScans.status())
}

// lostRestoreRequest names the accounts to put parts back on, or none for
// every account that has lost any.
type lostRestoreRequest struct {
	ProviderIDs []string `json:"provider_ids"`
}

// handleLostRestoreStart starts putting lost parts back, detached from this
// request, or joins the restore already running for the same accounts.
//
// 202 with the run when it started one, 200 when it joined one, 409 when one
// is going for different accounts. The restore lists the accounts itself and
// acts on what is missing then, so it needs no scan to have been run first —
// and it leaves the last scan's answer stale, which is dropped when it lands.
func (s *Server) handleLostRestoreStart(w http.ResponseWriter, r *http.Request) {
	var req lostRestoreRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	accounts := append([]string(nil), req.ProviderIDs...)
	sort.Strings(accounts)
	scope := strings.Join(accounts, ",")

	v, _ := s.Vault()
	status, outcome := s.lostRestores.start(scope, func(ctx context.Context, onProgress func(vault.LostRestoreProgress)) (*vault.LostRestoreReport, error) {
		report, err := v.RestoreLostParts(ctx, vault.LostRestoreOptions{
			Accounts: accounts,
			OnProgress: func(at vault.LostRestoreProgress) {
				onProgress(at)
				s.noteExternalActivity()
			},
		})
		if err == nil && report.Objects > 0 {
			// The last scan lists these as lost, and they are not any more.
			s.lostScans.forgetFinished()
		}
		return report, err
	}, checkTimeout)

	if outcome == jobBusy {
		writeError(w, http.StatusConflict,
			"lost parts are already being put back on other clouds — wait for that to finish", "RESTORE_BUSY")
		return
	}
	writeJSON(w, startCode(outcome), status)
}

// handleLostRestoreStatus answers with the restore that is running, and where
// it has got to, or the last one that ran, and what it put back.
func (s *Server) handleLostRestoreStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lostRestores.status())
}

// handleLostRestoreStop ends a running restore. It finishes with what it had
// put back, marked stopped, so the answer is the status as it now stands.
func (s *Server) handleLostRestoreStop(w http.ResponseWriter, r *http.Request) {
	s.lostRestores.cancel()
	s.handleLostRestoreStatus(w, r)
}
