package server

import (
	"context"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Finding corrupt files and fixing them, over HTTP. The work is
// vault.CheckFolder and vault.RepairChecked (internal/vault/verify.go); this
// is the plumbing that lets a browser start one, walk away, and come back.
//
// A deep check downloads every shard of every file under a folder, which for
// a folder of films is hours. So it is a job in the sense jobWatch means — on
// a context of its own, polled for where it has got to, its report held until
// the next check replaces it or the vault locks — and it can be stopped, at
// which point it answers with what it had reached rather than with nothing.
//
// The repair is a second job, and deliberately takes file IDs rather than
// findings: what is wrong with each file is read from the report the server
// is holding, so a request cannot ask for a rebuild that treats a healthy
// part as absent. A check is a GET's worth of safety — it reads and never
// writes — and still a POST, because it costs bandwidth on every account.

// checkTimeout bounds one deep check, and one repair run. Generous: the work
// is proportional to the folder, and a check cut off is a check to run again.
const checkTimeout = 48 * time.Hour

type checkWatch = jobWatch[vault.CheckProgress, vault.CheckReport]
type repairWatch = jobWatch[vault.RepairProgress, vault.RepairReport]

type checkRequest struct {
	Path  string   `json:"path"`
	Vault string   `json:"vault,omitempty"`
	Kinds []string `json:"kinds,omitempty"`
}

// handleCheckSurvey says what a check of a folder would read, by kind, from
// the index alone.
func (s *Server) handleCheckSurvey(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	survey, err := v.SurveyCheck(requestScope(r), r.URL.Query().Get("path"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, survey)
}

// handleCheckStart starts a deep check of a folder, or joins the one running
// over the same folder and kinds. A check of something else already running
// is 409 with that run.
func (s *Server) handleCheckStart(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	known := map[string]bool{}
	for _, kind := range vault.CheckKinds {
		known[kind] = true
	}
	for _, kind := range req.Kinds {
		if !known[kind] {
			writeError(w, http.StatusBadRequest, "unknown kind of file: "+kind, "BAD_REQUEST")
			return
		}
	}

	v, _ := s.Vault()
	scope := vault.Scope(req.Vault)
	dir := vault.CleanDir(req.Path)
	if !v.FolderExists(scope, dir) {
		writeError(w, http.StatusNotFound, "no such folder: "+dir, "NOT_FOUND")
		return
	}

	key := req.Vault + "\x00" + dir
	for _, kind := range req.Kinds {
		key += "\x00" + kind
	}
	status, outcome := s.checks.start(key, func(ctx context.Context, onProgress func(vault.CheckProgress)) (*vault.CheckReport, error) {
		return v.CheckFolder(ctx, scope, dir, vault.CheckOptions{
			Kinds: req.Kinds,
			OnProgress: func(at vault.CheckProgress) {
				onProgress(at)
				// Hours of reading with nobody watching is still use; without
				// saying so the idle timer locks the vault out from under it.
				s.noteExternalActivity()
			},
		})
	}, checkTimeout)
	writeJSON(w, startCode(outcome), checkStatus(status))
}

// handleCheckStatus answers with the check that is running or the last one
// that ran. No run at all is an ordinary answer.
func (s *Server) handleCheckStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, checkStatus(s.checks.status()))
}

// handleCheckStop ends a running check. It finishes with what it had reached,
// marked stopped, so the answer is the status as it now stands.
func (s *Server) handleCheckStop(w http.ResponseWriter, r *http.Request) {
	s.checks.cancel()
	writeJSON(w, http.StatusOK, checkStatus(s.checks.status()))
}

// checkStatus hides the scope key, which is plumbing: the report carries the
// path and kinds a page wants.
func checkStatus(status jobStatus[vault.CheckProgress, vault.CheckReport]) jobStatus[vault.CheckProgress, vault.CheckReport] {
	status.Scope = ""
	return status
}

func startCode(outcome startOutcome) int {
	switch outcome {
	case jobStarted:
		return http.StatusAccepted
	case jobBusy:
		return http.StatusConflict
	}
	return http.StatusOK
}

type repairRequest struct {
	// IDs names the files to repair, from the last check's flagged list.
	// Empty is every file that check offered a repair for.
	IDs []string `json:"ids,omitempty"`
}

// handleRepairStart repairs files the last finished check flagged.
func (s *Server) handleRepairStart(w http.ResponseWriter, r *http.Request) {
	var req repairRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	last := s.checks.status()
	if last.Running || last.Result == nil {
		writeError(w, http.StatusConflict,
			"there is no finished check to repair from — run a check first", "NO_CHECK")
		return
	}

	want := map[string]bool{}
	for _, id := range req.IDs {
		want[id] = true
	}
	var chosen []vault.FileCheck
	for _, c := range last.Result.Flagged {
		if c.Fix != "" && (len(want) == 0 || want[c.ID]) {
			chosen = append(chosen, c)
		}
	}
	if len(chosen) == 0 {
		writeError(w, http.StatusBadRequest,
			"none of those files has a repair on offer in the last check", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	status, outcome := s.repairs.start(last.ID, func(ctx context.Context, onProgress func(vault.RepairProgress)) (*vault.RepairReport, error) {
		report, err := v.RepairChecked(ctx, chosen, func(at vault.RepairProgress) {
			onProgress(at)
			s.noteExternalActivity()
		})
		if err == nil {
			// What the check found is no longer what is there.
			s.checks.forgetFinished()
		}
		return report, err
	}, checkTimeout)
	status.Scope = ""
	writeJSON(w, startCode(outcome), status)
}

func (s *Server) handleRepairStatus(w http.ResponseWriter, r *http.Request) {
	status := s.repairs.status()
	status.Scope = ""
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleRepairStop(w http.ResponseWriter, r *http.Request) {
	s.repairs.cancel()
	s.handleRepairStatus(w, r)
}

// handleFileCheck deep-checks one file and answers when it is done: what the
// preview asks when a picture will not draw, to say whether the file is
// damaged or merely something this browser cannot display.
func (s *Server) handleFileCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	check, err := v.CheckFile(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, check)
}
