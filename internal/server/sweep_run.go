package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sort"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// The orphan sweep as something that runs on its own, rather than inside the
// request that asked for it — the reattach's shape (reattach_run.go), for the
// same reason and worse: the sweep lists every account again before its first
// delete and then erases object by object, which for a vault where somebody
// has been deleting films is minutes, and a page closed in that time used to
// cancel the request and leave the sweep half done.
//
// One at a time. A run is scoped by exactly what it was asked to erase, so a
// second ask for the same targets joins the run going, and any other ask while
// one runs is refused with 409: two sweeps listing the same accounts at once
// would race each other's deletes for nothing.

// sweepProgress is where a sweep has got to: objects erased of objects doomed.
// A total of zero while running means it is still listing every account to
// decide what goes.
type sweepProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// sweepWatch holds the sweep that is running, or the last one that ran.
type sweepWatch = jobWatch[sweepProgress, vault.OrphanSweepReport]

// sweepStatus is what a poll is answered with — see jobStatus. The report is
// under `result`.
type sweepStatus = jobStatus[sweepProgress, vault.OrphanSweepReport]

func newSweepWatch() *sweepWatch {
	return newJobWatch[sweepProgress, vault.OrphanSweepReport]()
}

// sweepScope names a set of targets, the same whatever order they come in.
// Empty for none, which is "everything the vault considers abandoned"; a
// short hash otherwise, since the list itself can run to hundreds of rows and
// the scope is sent back on every poll.
func sweepScope(targets []vault.OrphanTarget) string {
	if len(targets) == 0 {
		return ""
	}
	keys := make([]string, 0, len(targets))
	for _, t := range targets {
		keys = append(keys, t.ProviderID+"\x00"+t.ArchiveID)
	}
	sort.Strings(keys)
	h := sha256.New()
	prev := ""
	for i, k := range keys {
		// The same target named twice is the same ask.
		if i > 0 && k == prev {
			continue
		}
		prev = k
		h.Write([]byte(k))
		h.Write([]byte{0xff})
	}
	return "targets:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// sweepStartRequest names what to erase: the pairs from a scan, or none for
// everything the vault considers abandoned. No dry run — that is
// POST /api/vault/orphans, which has nothing to wait for.
type sweepStartRequest struct {
	Targets []vault.OrphanTarget `json:"targets"`
}

// handleSweepStart starts the sweep detached from this request, or joins the
// one already running for the same targets. 202 when it started one, 200 when
// it joined one, 409 when another is running (GET says which).
//
// The sweep re-scans before it deletes and refuses outright in the states
// where unaccounted for is not the same as abandoned, exactly as the
// synchronous POST does; a refusal arrives as the finished run's `error`.
func (s *Server) handleSweepStart(w http.ResponseWriter, r *http.Request) {
	var req sweepStartRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	targets := req.Targets
	status, outcome := s.sweeps.start(sweepScope(targets), func(ctx context.Context, onProgress func(sweepProgress)) (*vault.OrphanSweepReport, error) {
		// Opened at (0, 0), which reads as "still deciding what goes": the
		// listing before the first delete deserves to look like work.
		onProgress(sweepProgress{})
		report, err := v.SweepOrphans(ctx, targets, false, func(done, total int) {
			onProgress(sweepProgress{Done: done, Total: total})
			// Nobody may be watching, and the idle timer would otherwise
			// lock the vault out from under it.
			s.noteExternalActivity()
		})
		if err == nil && report.Deleted > 0 {
			// The last stray-parts scan offers what was just erased.
			s.strayScans.forgetFinished()
		}
		return report, err
	}, orphanSweepTimeout)

	switch outcome {
	case jobStarted:
		writeJSON(w, http.StatusAccepted, status)
	case jobJoined:
		writeJSON(w, http.StatusOK, status)
	default:
		writeError(w, http.StatusConflict,
			"a sweep is already running — wait for it to finish", "SWEEP_BUSY")
	}
}

// handleSweepStatus answers with the sweep that is running, and how far it has
// got, or the last one that ran, and what it erased. No run at all is an
// ordinary answer, not an error.
func (s *Server) handleSweepStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sweeps.status())
}
