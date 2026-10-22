package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// Replication over HTTP: the policy, what replicates, and the worker that
// makes the copies while the vault is open. See vault/replica.go for what a
// replica is; this file is only the door to it.

// replicationTick is how often the loop asks whether a replica is wanted.
const replicationTick = 20 * time.Second

// replicationStepTimeout bounds one file, as generously as a re-encryption's.
const replicationStepTimeout = 12 * time.Hour

// replicationMaxSleep caps how long the loop trusts a wait the meter gave it.
const replicationMaxSleep = 5 * time.Minute

// replicationLoop makes replicas one at a time, at the policy's pace, while
// the vault is unlocked, and does nothing at all while it is locked.
func (s *Server) replicationLoop() {
	ticker := time.NewTicker(replicationTick)
	defer ticker.Stop()

	var notBefore time.Time
	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		now := time.Now()
		if now.Before(notBefore) {
			continue
		}

		step, err := s.replicateStep(v, now)
		switch {
		case errors.Is(err, vault.ErrLocked):
			// Locked between the check and the step, which is ordinary.
		case err != nil:
			log.Printf("replication: %v", err)
		case step.Done:
			log.Printf("replicated %s (%s)", step.Path, formatBytes(step.Bytes))
			if step.Warning != "" {
				log.Printf("replication: %s", step.Warning)
			}
		case step.Warning != "":
			log.Printf("replication: %s", step.Warning)
		}

		switch {
		case step.Idle:
			notBefore = now.Add(time.Minute)
		case step.Wait > replicationMaxSleep:
			notBefore = now.Add(replicationMaxSleep)
		case step.Wait > 0:
			notBefore = now.Add(step.Wait)
		}
	}
}

// replicateStep runs one step with the idle timer held off for as long as
// the step takes, and no longer.
func (s *Server) replicateStep(v *vault.Vault, now time.Time) (vault.ReplicateStep, error) {
	ctx, cancel := context.WithTimeout(context.Background(), replicationStepTimeout)
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		keep := time.NewTicker(time.Minute)
		defer keep.Stop()
		for {
			select {
			case <-done:
				return
			case <-keep.C:
				s.noteExternalActivity()
			}
		}
	}()

	return v.ReplicateStep(ctx, now)
}

// handleReplicationStatus says what the policy is and how far it has got.
func (s *Server) handleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	status, err := v.ReplicationStatus(time.Now())
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// replicationRequest is the policy as the browser sends it.
type replicationRequest struct {
	Accounts []vault.ReplicaAccount `json:"accounts"`
	// Scheme is "k-of-n", "whole", or empty for the default the count names.
	Scheme      string `json:"scheme"`
	BytesPerDay int64  `json:"bytes_per_day"`
	Paused      bool   `json:"paused"`
}

// handleReplicationSet sets the policy, or clears it with no accounts.
func (s *Server) handleReplicationSet(w http.ResponseWriter, r *http.Request) {
	var req replicationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	scheme, err := parseReplicaScheme(req.Scheme)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.SetReplication(vault.ReplicationSettings{
		Accounts:    req.Accounts,
		Scheme:      scheme,
		BytesPerDay: req.BytesPerDay,
		Paused:      req.Paused,
	}); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	status, err := v.ReplicationStatus(time.Now())
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// parseReplicaScheme reads a replica's code: "k-of-n" as an upload's is
// written, "whole" for the single-cloud copy, or nothing.
func parseReplicaScheme(raw string) (archive.Scheme, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "whole") {
		return archive.WholeCopy, nil
	}
	return parseSchemeField(raw)
}

// replicateRequest says whether a folder or a file is replicated. Enabled is
// a pointer so that null means "back to inheriting".
type replicateRequest struct {
	Path    string `json:"path"`
	Enabled *bool  `json:"enabled"`
}

// handleReplicateSet flags a folder or a file.
func (s *Server) handleReplicateSet(w http.ResponseWriter, r *http.Request) {
	var req replicateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "path is required", "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	if err := v.SetReplicate(req.Path, req.Enabled); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	status, err := v.ReplicationStatus(time.Now())
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleReplicaDrop erases one file's replica now, rather than leaving it
// for a sweep. The file's own parts are untouched.
func (s *Server) handleReplicaDrop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	warnings, err := v.DropReplica(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"warnings": warnings})
}

// readSourceParam reads where a content request wants its bytes from.
func readSourceParam(r *http.Request) (vault.ReadSource, error) {
	return vault.ParseReadSource(r.URL.Query().Get("from"))
}
