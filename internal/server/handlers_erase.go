package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Deleting a selection — files, folders, or both — as one run, which the
// browser hands to the machine rather than waiting on.
//
// The three older endpoints (DELETE /api/files/{id}, POST /api/files/delete,
// DELETE /api/folders) each hold a request open for as long as the erasing
// takes, and answer at the end. This one takes the same selection the
// relocation endpoint does, runs it as one delete — the files in one index
// write, then each folder in its own — and, asked to detach, answers at once
// with the run and lets the machine finish. See erase_runs.go for what a run
// is and why deletes queue rather than refuse.

// eraseRequest names what to delete. The selection is written the way a
// relocation's is, so a dialog that already has one in hand sends it as is.
type eraseRequest struct {
	// ID names a single file; Path names a file or a folder, inside Vault.
	// One of them is required unless Targets carries the selection.
	ID    string `json:"id"`
	Path  string `json:"path"`
	Vault string `json:"vault,omitempty"`

	// Targets is a selection: several files and folders deleted as one act,
	// one run, one answer. When it is given, ID, Path and Vault are ignored.
	Targets []relocateTarget `json:"targets,omitempty"`

	// Detach asks for the delete to run with no request behind it: the answer
	// is the run, and how it comes out is read from GET /api/delete/runs.
	Detach bool `json:"detach"`
}

// targets normalizes the request into the selection it means.
func (req eraseRequest) targets() []relocateTarget {
	if len(req.Targets) > 0 {
		out := make([]relocateTarget, 0, len(req.Targets))
		for _, t := range req.Targets {
			if t.target() != "" {
				out = append(out, t)
			}
		}
		return out
	}
	single := relocateTarget{ID: req.ID, Path: req.Path, Vault: req.Vault}
	if single.target() == "" {
		return nil
	}
	return []relocateTarget{single}
}

// eraseTimeout is the most a foreground delete of a selection may run — the
// folder delete's own ceiling, since a selection is at most a few of those.
const eraseTimeout = 30 * time.Minute

// erasePlan is a selection sorted into what the vault is asked to do: which
// files to take in one write, which folders to take whole, and how many files
// the lot comes to, so the bar has its denominator before a part is touched.
type erasePlan struct {
	ids     []string
	folders []eraseFolder
	total   int

	// missing names the targets that pointed at nothing, which is an answer
	// rather than an error: a selection deleted twice names files the first
	// time took.
	missing []string
}

type eraseFolder struct {
	scope vault.Scope
	dir   string
	files int
}

// planErase reads the selection against the index and nothing else: which
// targets are files, which are folders, and what each folder holds. A path
// that names a file is deleted as a file. Nothing is contacted.
func planErase(v *vault.Vault, targets []relocateTarget) (*erasePlan, error) {
	plan := &erasePlan{missing: []string{}}
	seen := map[string]bool{}
	for _, t := range targets {
		scope := vault.Scope(t.Vault)
		if id := strings.TrimSpace(t.ID); id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
			if _, err := v.Entry(id); err != nil {
				if errors.Is(err, vault.ErrLocked) {
					return nil, err
				}
				plan.missing = append(plan.missing, id)
				continue
			}
			plan.ids = append(plan.ids, id)
			continue
		}

		path := strings.TrimSpace(t.Path)
		if entry, err := v.EntryByPath(scope, path); err == nil {
			if seen[entry.ID] {
				continue
			}
			seen[entry.ID] = true
			plan.ids = append(plan.ids, entry.ID)
			continue
		} else if errors.Is(err, vault.ErrLocked) {
			return nil, err
		}

		dir := vault.CleanDir(path)
		key := string(scope) + "\x00" + dir
		if seen[key] {
			continue
		}
		seen[key] = true
		stats, err := v.FolderStats(scope, dir)
		if err != nil {
			if errors.Is(err, vault.ErrLocked) {
				return nil, err
			}
			plan.missing = append(plan.missing, path)
			continue
		}
		plan.folders = append(plan.folders, eraseFolder{scope: scope, dir: dir, files: stats.Files})
		plan.total += stats.Files
	}
	plan.total += len(plan.ids)
	return plan, nil
}

// runErase carries out one plan: the files in a single index write, then
// each folder on its own, counted as one run. ctx done part-way is a stop,
// as it is for the vault: no further file is started, what was erased has
// left the index, the rest is where it was, and the error returned is
// ctx.Err() with the report saying how far it got.
func (s *Server) runErase(ctx context.Context, v *vault.Vault, plan *erasePlan, ticket *eraseTicket) (*eraseReport, error) {
	report := &eraseReport{Missing: append([]string{}, plan.missing...), Warnings: []string{}}
	ticket.update(0, plan.total)

	before := 0
	watch := func(done, _ int) {
		ticket.update(before+done, plan.total)
		// A delete with nobody watching is still use, and without saying so
		// the idle timer would lock the vault out from under it.
		s.noteExternalActivity()
	}

	if len(plan.ids) > 0 {
		got, err := v.DeleteMany(ctx, plan.ids, watch)
		if got != nil {
			report.Deleted += got.Deleted
			report.Missing = append(report.Missing, got.Missing...)
			report.Warnings = append(report.Warnings, got.Warnings...)
			before += got.Deleted
		}
		if err != nil {
			return report, err
		}
	}

	for _, f := range plan.folders {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		// Rmdir says how many it erased only through its progress window, so
		// the count is read off the last report it made.
		erased := 0
		warnings, err := v.Rmdir(ctx, f.scope, f.dir, true, func(done, total int) {
			erased = done
			watch(done, total)
		})
		for _, w := range warnings {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %s", f.dir, w))
		}
		report.Deleted += erased
		before += erased
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, vault.ErrLocked) {
				return report, err
			}
			// This folder's own trouble is one line; the rest of the
			// selection still goes.
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %v", f.dir, err))
			continue
		}
		report.Folders++
	}
	return report, nil
}

// eraseLabel names a run for its card: the one thing picked, or how many.
func eraseLabel(v *vault.Vault, targets []relocateTarget) string {
	if len(targets) == 1 {
		t := targets[0]
		if path := strings.TrimSpace(t.Path); path != "" {
			return path
		}
		if entry, err := v.Entry(strings.TrimSpace(t.ID)); err == nil {
			return entry.Path()
		}
		return "1 item"
	}
	return fmt.Sprintf("%d items", len(targets))
}

// handleErase deletes a selection. Detached, it answers 202 with the run the
// moment the selection has been read against the index; otherwise it holds
// the request open and answers with what the delete came to, as the older
// endpoints do.
func (s *Server) handleErase(w http.ResponseWriter, r *http.Request) {
	var req eraseRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	targets := req.targets()
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "name the file or folder to delete", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	plan, err := planErase(v, targets)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	// A selection nothing in which exists is answered here, as a selection
	// deleted twice: nothing to do, and nothing wrong. Detached it would
	// otherwise be a run that appears and at once reports nothing.
	if len(plan.ids) == 0 && len(plan.folders) == 0 {
		if len(targets) == 1 {
			writeError(w, http.StatusNotFound, "no such file or folder: "+targets[0].target(), "NOT_FOUND")
			return
		}
		writeJSON(w, http.StatusOK, eraseAnswer("deleted", &eraseReport{
			Missing: plan.missing, Warnings: []string{},
		}))
		return
	}

	label := eraseLabel(v, targets)
	if req.Detach {
		s.startDetachedErase(w, v, label, plan)
		return
	}

	ctx, cancel := contextWithTimeout(r, eraseTimeout)
	defer cancel()

	// Registered before the first part goes and forgotten however this ends,
	// so what GET /api/delete/runs answers with is what is actually running —
	// including this, held open in the foreground.
	ticket, err := s.erasures.start(label, false, cancel)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error(), "DELETE_BUSY")
		return
	}
	defer ticket.done()

	report, err := s.runErase(ctx, v, plan, ticket)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		vaultErrorResponse(w, err)
		return
	}
	status := "deleted"
	if err != nil {
		status = "stopped"
	}
	writeJSON(w, http.StatusOK, eraseAnswer(status, report))
}

// eraseAnswer is a foreground delete's response: the report, flat, under a
// status — the shape POST /api/files/delete answers in, with folders added.
func eraseAnswer(status string, report *eraseReport) map[string]any {
	return map[string]any{
		"status":   status,
		"deleted":  report.Deleted,
		"folders":  report.Folders,
		"missing":  report.Missing,
		"warnings": report.Warnings,
	}
}

// startDetachedErase answers 202 and lets the delete run with no request
// behind it. The context is derived from Background rather than from r — that
// is the whole difference, and what makes closing the page harmless.
func (s *Server) startDetachedErase(w http.ResponseWriter, v *vault.Vault, label string, plan *erasePlan) {
	ctx, cancel := context.WithTimeout(context.Background(), eraseRunTimeout)
	ticket, err := s.erasures.start(label, true, cancel)
	if err != nil {
		cancel()
		writeError(w, http.StatusConflict, err.Error(), "DELETE_BUSY")
		return
	}
	// The denominator is known before the run has a turn, so a waiting run
	// can already say what it is waiting to do.
	ticket.update(0, plan.total)

	go func() {
		defer cancel()
		// Its turn, or a stop while waiting for one — which is a run that
		// touched nothing, and says so.
		if err := ticket.acquire(ctx); err != nil {
			ticket.finish(&eraseReport{Missing: plan.missing, Warnings: []string{}}, nil, true)
			return
		}
		report, err := s.runErase(ctx, v, plan, ticket)
		// A cancelled run is not a failed one: it stopped because somebody
		// stopped it, every file it finished has left the index, and running
		// the same delete again picks up the rest.
		cancelled := ctx.Err() != nil
		if err != nil && cancelled {
			err = nil
		}
		ticket.finish(report, err, cancelled)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"run": s.erasures.forRun(ticket.id)})
}

// handleEraseRuns answers with every delete waiting or running right now, and
// what a detached one lately finished with. An empty list is the ordinary
// answer.
func (s *Server) handleEraseRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.erasures.all()})
}

// handleEraseStop cancels a waiting or running delete, or forgets a finished
// one's result. Both are the same gesture: stop showing me this.
func (s *Server) handleEraseStop(w http.ResponseWriter, r *http.Request) {
	if !s.erasures.stop(r.PathValue("run")) {
		writeError(w, http.StatusNotFound, "no delete by that name is running", "NOT_FOUND")
		return
	}
	// 200 with what is left rather than 204, so the page redraws from the
	// answer instead of from a guess about what it did.
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.erasures.all()})
}
