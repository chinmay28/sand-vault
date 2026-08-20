package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The standing instructions a folder has been given, over HTTP.
//
// Four endpoints and one of them does the work. Listing, setting and removing a
// policy are all reads and writes of the encrypted index and answer in
// microseconds; running one contacts every connected account and can rebuild
// files, which is why it gets a deadline of its own and why only one runs at a
// time. See internal/vault/automation.go for what a sweep actually does.

// automationRunTimeout is the ceiling on one sweep started from the browser.
//
// Hours, for the same reason a relocation gets hours: a sweep over a folder
// whose cloud has died rebuilds every file under it, and a request that gave up
// after five minutes would leave the work half done with no way to say which
// half. It can afford to be this long because a sweep is resumable in the only
// sense that matters — each file is committed on its own, and running it again
// picks up whatever is still short.
const automationRunTimeout = 6 * time.Hour

// automationRequest is one policy as the browser sends it. The fields mirror
// vault.Automation, minus the ones only the vault writes: when it was made,
// when it last ran, and what it found.
type automationRequest struct {
	// Vault names which of the vaults inside the file the folder is in, empty
	// for the main one.
	Vault string `json:"vault,omitempty"`
	Path  string `json:"path"`

	Enabled bool   `json:"enabled"`
	Cadence string `json:"cadence"`
	At      string `json:"at,omitempty"`
	Weekday int    `json:"weekday,omitempty"`
	Action  string `json:"action"`

	Narrow       bool  `json:"narrow,omitempty"`
	MaxRepairs   int   `json:"max_repairs,omitempty"`
	RebuildLimit int64 `json:"rebuild_limit,omitempty"`
}

// handleAutomationList answers with every policy the vault can currently see,
// or with the one on a named folder when ?path= is given.
//
// The named form answers with a null policy rather than a 404 for a folder that
// has none: "does this folder have one?" is a question the browser asks every
// time a folder is opened, and "no" is an answer rather than an error.
func (s *Server) handleAutomationList(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()

	if path := strings.TrimSpace(r.URL.Query().Get("path")); path != "" {
		auto, err := v.AutomationFor(requestScope(r), path)
		if err != nil {
			vaultErrorResponse(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"automation": auto})
		return
	}

	all, err := v.AllAutomations()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	running, busy := v.AutomationRunning()
	writeJSON(w, http.StatusOK, map[string]any{
		"automations": all,
		"running":     busy,
		"folder":      running,
	})
}

// handleAutomationSet puts a policy on a folder, replacing whatever was there.
// One endpoint for creating and editing, because a folder has at most one
// policy and there is nothing a create could do that an overwrite could not.
func (s *Server) handleAutomationSet(w http.ResponseWriter, r *http.Request) {
	var req automationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "name the folder the policy is for", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	auto, err := v.SetAutomation(vault.Scope(req.Vault), req.Path, vault.Automation{
		Enabled:      req.Enabled,
		Cadence:      vault.Cadence(req.Cadence),
		At:           req.At,
		Weekday:      time.Weekday(req.Weekday),
		Action:       vault.AutomationAction(req.Action),
		Narrow:       req.Narrow,
		MaxRepairs:   req.MaxRepairs,
		RebuildLimit: req.RebuildLimit,
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"automation": auto})
}

// handleAutomationRemove takes the policy off a folder, history and all.
func (s *Server) handleAutomationRemove(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, "name the folder the policy is on", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.RemoveAutomation(requestScope(r), path); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": path})
}

// runAutomationRequest names the folder to sweep now.
type runAutomationRequest struct {
	Vault string `json:"vault,omitempty"`
	Path  string `json:"path"`
}

// handleAutomationRun carries out a folder's policy now, whether or not it is
// due and whether or not it is switched on — which is what somebody who has
// just connected a replacement cloud wants, rather than waiting until ten
// tomorrow to find out whether it worked.
func (s *Server) handleAutomationRun(w http.ResponseWriter, r *http.Request) {
	var req runAutomationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "name the folder to sweep", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, automationRunTimeout)
	defer cancel()

	// A sweep can outlast the idle timer, and being locked out halfway through
	// rebuilding a file is not a thing to let happen quietly. This counts as
	// use for exactly as long as the sweep runs, the same way a mounted share
	// or a stream in flight does.
	s.noteExternalActivity()
	defer s.noteExternalActivity()

	v, _ := s.Vault()
	run, err := v.RunAutomation(ctx, vault.Scope(req.Vault), req.Path)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// automationTick is how often the scheduler wakes to see whether anything is
// due. A minute, because that is the resolution a wall-clock schedule is
// written at and because the check itself is arithmetic over a handful of
// records — no account is contacted unless something is actually due.
const automationTick = time.Minute

// automationLoop runs the folder policies whose time has come.
//
// It runs only while the vault is unlocked, and that is the honest shape of the
// thing rather than an oversight: the schedules live in the encrypted index and
// the files can only be checked with the keys that unlock it, so a locked vault
// has nothing to read and nothing to read it with. A policy whose slot passes
// while the vault is shut is not lost — it comes up due the moment the vault is
// next opened, because "due" is a comparison against the last run rather than a
// timer that has to be running. A machine meant to keep these schedules should
// be started with a long --idle-timeout so that the vault, once opened, stays
// open.
func (s *Server) automationLoop() {
	ticker := time.NewTicker(automationTick)
	defer ticker.Stop()

	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		due, err := v.DueAutomations(time.Now())
		if err != nil || len(due) == 0 {
			continue
		}

		// Only now that there is work: a sweep contacts every account and can
		// rebuild files, so it must not be cut short by the idle timer landing
		// in the middle of it.
		s.noteExternalActivity()
		ctx, cancel := context.WithTimeout(context.Background(), automationRunTimeout)
		runs, err := v.RunDueAutomations(ctx, time.Now())
		cancel()
		s.noteExternalActivity()

		if err != nil {
			log.Printf("automation: %v", err)
		}
		for _, run := range runs {
			logAutomationRun(run)
		}
	}
}

// logAutomationRun says what a scheduled sweep came to, in one line.
//
// One line and always one line: this is the only place an unattended run is
// visible, and a log that says nothing on the nights everything was fine is a
// log nobody can use to tell "it is working" from "it never ran".
func logAutomationRun(run *vault.AutomationRun) {
	where := run.Folder
	if run.Vault != "" {
		where = string(run.Vault) + ":" + where
	}
	switch {
	case run.Error != "":
		log.Printf("automation %s: %s", where, run.Error)
	case run.Clean():
		log.Printf("automation %s: %d file(s) checked, every part where it should be",
			where, run.Checked)
	default:
		log.Printf("automation %s: %d checked, %d short, %d past repairing, "+
			"%d rebuilt, %d left for later%s",
			where, run.Checked, run.Short, run.AtRisk, run.Repaired, run.Deferred,
			offlineSuffix(run.Offline))
	}
}

// offlineSuffix names the accounts that did not answer, when any did not.
func offlineSuffix(offline []string) string {
	if len(offline) == 0 {
		return ""
	}
	return " (no answer from " + strings.Join(offline, ", ") + ")"
}
