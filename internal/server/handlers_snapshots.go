package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/snapshot"
	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Snapshots of a folder over HTTP. The work is internal/vault/snapshot.go and
// the format internal/snapshot; this is what lets a browser take one, see
// which are kept where, try a password on one, bring one back, and walk away
// from either long job.
//
// Taking and restoring are one job at a time between them, run as the other
// long jobs here are — on a context of its own, polled, its report held until
// the next replaces it. Listing, inspecting and deleting are answered in the
// request: each is a listing of one place and, at most, the opening megabyte
// of one part.
//
// The snapshot's password crosses in the request body and is handed straight
// to the job. It is never part of the key a run is joined by, never in a
// status, and never written down.

// snapshotPlaceTimeout bounds a listing, an inspect and a delete. An inspect
// on a cloud is the longest of them: it fetches the whole first part, since
// that is the only way a backend hands back any of an object.
const snapshotPlaceTimeout = 10 * time.Minute

type snapshotWatch = jobWatch[vault.SnapshotProgress, vault.SnapshotReport]

// snapshotTarget is where a snapshot is kept, as a request names it.
type snapshotTarget struct {
	Account string `json:"account,omitempty"`
	Machine string `json:"machine,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

func (t snapshotTarget) toVault() vault.SnapshotTarget {
	return vault.SnapshotTarget{
		Account: strings.TrimSpace(t.Account),
		Machine: strings.TrimSpace(t.Machine),
		Dir:     strings.TrimSpace(t.Dir),
	}
}

// key is the target as part of what a run is joined by.
func (t snapshotTarget) key() string {
	return t.Account + "\x00" + t.Machine + "\x00" + t.Dir
}

type snapshotTakeRequest struct {
	Path  string `json:"path"`
	Vault string `json:"vault,omitempty"`

	Target   snapshotTarget `json:"target"`
	Password string         `json:"password"`

	// PartSize is the most one part file may be, in bytes. Zero is no limit
	// on a machine and the default on a cloud.
	PartSize int64 `json:"part_size,omitempty"`
}

type snapshotRestoreRequest struct {
	// Path is the folder the snapshot is restored into.
	Path  string `json:"path"`
	Vault string `json:"vault,omitempty"`

	Target   snapshotTarget `json:"target"`
	Name     string         `json:"name"`
	Password string         `json:"password"`
	Replace  bool           `json:"replace,omitempty"`
}

type snapshotNameRequest struct {
	Target   snapshotTarget `json:"target"`
	Name     string         `json:"name"`
	Password string         `json:"password,omitempty"`
}

// snapshotErrorResponse answers for the failures a snapshot has of its own,
// and hands the rest to the vault's.
func snapshotErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, snapshot.ErrPassword):
		// 400 with a code of its own: the dialog puts this beside the
		// password field rather than across the top.
		writeError(w, http.StatusBadRequest, err.Error(), "SNAPSHOT_PASSWORD")
	case errors.Is(err, snapshot.ErrIncomplete):
		writeError(w, http.StatusConflict, err.Error(), "SNAPSHOT_INCOMPLETE")
	case errors.Is(err, snapshot.ErrDamaged), errors.Is(err, snapshot.ErrNotSnapshot):
		writeError(w, http.StatusConflict, err.Error(), "SNAPSHOT_DAMAGED")
	default:
		vaultErrorResponse(w, err)
	}
}

// handleSnapshotPlan says what a snapshot of a folder would hold, and the
// limits a part's size is held to.
func (s *Server) handleSnapshotPlan(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	plan, err := v.PlanSnapshot(requestScope(r), r.URL.Query().Get("path"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleSnapshotStored lists the snapshots one place is keeping.
func (s *Server) handleSnapshotStored(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target := snapshotTarget{Account: q.Get("account"), Machine: q.Get("machine"), Dir: q.Get("dir")}

	ctx, cancel := contextWithTimeout(r, snapshotPlaceTimeout)
	defer cancel()

	v, _ := s.Vault()
	sets, err := v.ListSnapshots(ctx, target.toVault())
	if err != nil {
		snapshotErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": sets})
}

// handleSnapshotInspect opens one snapshot under a password and says what it
// is a snapshot of. It is how the dialog tries a password before starting a
// restore that would otherwise fail a poll later.
func (s *Server) handleSnapshotInspect(w http.ResponseWriter, r *http.Request) {
	var req snapshotNameRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	ctx, cancel := contextWithTimeout(r, snapshotPlaceTimeout)
	defer cancel()

	v, _ := s.Vault()
	info, err := v.InspectSnapshot(ctx, req.Target.toVault(), req.Name, req.Password)
	if err != nil {
		snapshotErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleSnapshotDelete erases one snapshot from where it is kept.
func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	var req snapshotNameRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	// Detached from the request: a delete of forty parts that stops at the
	// twentieth because a tab closed leaves an incomplete snapshot nobody
	// asked for.
	ctx, cancel := detachedContextWithTimeout(r, snapshotPlaceTimeout)
	defer cancel()

	v, _ := s.Vault()
	if err := v.DeleteSnapshot(ctx, req.Target.toVault(), req.Name); err != nil {
		snapshotErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": req.Name})
}

// handleSnapshotTake starts a snapshot of a folder, or joins the same one
// already running. A different snapshot job running is 409 with that run.
func (s *Server) handleSnapshotTake(w http.ResponseWriter, r *http.Request) {
	var req snapshotTakeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "a snapshot needs a password", "BAD_REQUEST")
		return
	}
	if req.PartSize < 0 {
		writeError(w, http.StatusBadRequest, "part_size is bytes, zero for no limit", "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	scope := vault.Scope(req.Vault)
	dir := vault.CleanDir(req.Path)

	// Asked here rather than left to the job, so that what can be refused
	// from the index alone is refused in the answer to the request that
	// asked — the same order the folder zip works in.
	plan, err := v.PlanSnapshot(scope, dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if err := plan.Ready(); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	key := "take\x00" + req.Vault + "\x00" + dir + "\x00" + req.Target.key()
	status, outcome := s.snapshots.start(key, func(ctx context.Context, onProgress func(vault.SnapshotProgress)) (*vault.SnapshotReport, error) {
		return v.TakeSnapshot(ctx, scope, dir, vault.SnapshotRequest{
			Target:   req.Target.toVault(),
			Password: req.Password,
			PartSize: req.PartSize,
			OnProgress: func(at vault.SnapshotProgress) {
				onProgress(at)
				// A snapshot is hours of reading with nobody at the page;
				// without saying so the idle timer locks the vault under it.
				s.noteExternalActivity()
			},
		})
	}, checkTimeout)
	writeJSON(w, startCode(outcome), snapshotStatus(status))
}

// handleSnapshotRestore starts a restore of one snapshot into a folder.
func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	var req snapshotRestoreRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name the snapshot to restore", "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	scope := vault.Scope(req.Vault)
	dir := vault.CleanDir(req.Path)

	key := "restore\x00" + req.Vault + "\x00" + dir + "\x00" + req.Target.key() + "\x00" + req.Name
	status, outcome := s.snapshots.start(key, func(ctx context.Context, onProgress func(vault.SnapshotProgress)) (*vault.SnapshotReport, error) {
		return v.RestoreSnapshot(ctx, scope, dir, vault.RestoreRequest{
			Target:   req.Target.toVault(),
			Name:     req.Name,
			Password: req.Password,
			Replace:  req.Replace,
			OnProgress: func(at vault.SnapshotProgress) {
				onProgress(at)
				s.noteExternalActivity()
			},
		})
	}, checkTimeout)
	writeJSON(w, startCode(outcome), snapshotStatus(status))
}

// handleSnapshotStatus answers with the snapshot job that is running or the
// last one that ran. No run at all is an ordinary answer.
func (s *Server) handleSnapshotStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, snapshotStatus(s.snapshots.status()))
}

// handleSnapshotStop ends a running job. A snapshot being taken is thrown
// away, parts and all; a restore keeps the files that had arrived.
func (s *Server) handleSnapshotStop(w http.ResponseWriter, r *http.Request) {
	s.snapshots.cancel()
	writeJSON(w, http.StatusOK, snapshotStatus(s.snapshots.status()))
}

// snapshotStatus hides the scope key, which is plumbing: the progress and the
// report carry what a page wants.
func snapshotStatus(status jobStatus[vault.SnapshotProgress, vault.SnapshotReport]) jobStatus[vault.SnapshotProgress, vault.SnapshotReport] {
	status.Scope = ""
	return status
}
