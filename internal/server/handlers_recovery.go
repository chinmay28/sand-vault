package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Disaster recovery over HTTP: the browser's half of what `sand vault recover`
// does at the command line.
//
// The shape of the thing being solved is the reason this is three endpoints
// rather than one. Somebody whose machine died reinstalls SAND, makes a fresh
// vault, and connects their clouds back — and at that moment the app knows
// something they may not: those accounts are still carrying the index of the
// vault that is gone. So the browser asks after every connection (GET), offers
// what it found, tries it without committing (preview), and only then rebuilds.

// handleRecoveryScan reports whether any connected account is holding a vault
// that could be recovered.
//
// It costs a listing and one small download per account, which is why the
// frontend only asks when the vault is empty — the state a reinstalled machine
// is in, and the only state a recovery can run into anyway.
func (s *Server) handleRecoveryScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	v, _ := s.Vault()
	scan, err := v.ScanForRecovery(ctx)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scan)
}

// recoveryRequest names the account to read the backup from and the password
// that opens it.
//
// The password is the lost vault's, which need not be the password of the vault
// doing the recovering: someone reinstalling picks a new one, and the copy on
// the account is still sealed under the old.
type recoveryRequest struct {
	ProviderID string `json:"provider_id"`
	Password   string `json:"password"`

	// DryRun asks what would come back without changing anything. The browser
	// runs one of these first, so the confirmation says what will actually
	// happen rather than what usually happens.
	DryRun bool `json:"dry_run"`
}

func (s *Server) handleRecoveryRun(w http.ResponseWriter, r *http.Request) {
	var req recoveryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest,
			"the password of the vault being recovered is required", "BAD_REQUEST")
		return
	}

	// Long enough for a listing of every account plus the rebuild itself, which
	// on a large vault is a lot of object keys to page through.
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()

	providerID := req.ProviderID
	if providerID == "" {
		id, err := firstRecoverableAccount(ctx, v)
		if err != nil {
			vaultErrorResponse(w, err)
			return
		}
		providerID = id
	}

	snapshot, err := v.FetchBackup(ctx, providerID, req.Password)
	switch {
	case errors.Is(err, vault.ErrNoBackup):
		writeError(w, http.StatusNotFound, err.Error(), "NO_BACKUP")
		return
	case err != nil:
		vaultErrorResponse(w, err)
		return
	}

	report, err := v.Recover(ctx, snapshot, req.DryRun)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	if !req.DryRun {
		// This vault is now the legitimate owner of these accounts, so it
		// replaces the copies the lost vault left behind — they are still
		// sealed under the old password, and the guard that protects a foreign
		// backup would otherwise refuse to overwrite them forever.
		v.AwaitBackupSync()
		warnings, err := v.SyncManifestBackup(ctx, true)
		report.Warnings = append(report.Warnings, warnings...)
		if err != nil {
			report.Warnings = append(report.Warnings,
				"the recovered index could not be copied back to the accounts: "+err.Error())
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"report":    report,
		"source":    providerID,
		"backup_at": snapshot.CreatedAt,
	})
}

// resumeRequest asks to finish a recovery that ran before every account was
// reconnected. There is no password: the data key was adopted when the first
// recovery ran, and what was missing is a reachable copy of the parts.
type resumeRequest struct {
	DryRun bool `json:"dry_run"`
}

// handleRecoveryResume re-points the index at the accounts that are back.
//
// This is the other half of what the report says to do. Telling somebody which
// accounts to connect is only useful if connecting them finishes the job, and
// a second POST to /api/vault/recovery cannot: by then the vault holds the
// files the first pass brought back, and adopting a snapshot again would
// replace the very data key they depend on.
func (s *Server) handleRecoveryResume(w http.ResponseWriter, r *http.Request) {
	// Nothing in the body is required, so no body at all is a valid request for
	// the whole job — which is what a plain `POST /api/vault/recovery/resume`
	// from a script or a curl means.
	var req resumeRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.Reconcile(ctx, req.DryRun)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report})
}

// reclaimRequest names the accounts the re-encrypted files should end up on.
type reclaimRequest struct {
	// Accounts is a set of connected account IDs. Empty leaves each file on the
	// accounts it is already on — which after a recovery are the ones the vault
	// that died chose.
	Accounts []string `json:"accounts"`
}

// handleVaultReclaim takes recovered files off the dead vault's key.
//
// Recovery adopts that key because it is the only thing that opens the parts
// already on the accounts, and that leaves the old password able to open them
// too — through any copy of the old manifest.sand, including ones this vault
// never got to overwrite. Reclaiming ends it: a fresh data key under this
// vault's own password, every file rebuilt onto it, the old parts erased.
//
// A download and an upload per file, so this request can run for a very long
// time on a full vault. Files stay readable throughout, and a client that gives
// up on the response has broken nothing: whatever moved stays moved, and
// POST /api/vault/migrate finishes the rest.
func (s *Server) handleVaultReclaim(w http.ResponseWriter, r *http.Request) {
	var req reclaimRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	report, err := v.Reclaim(r.Context(), req.Accounts, nil)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// firstRecoverableAccount picks the account to read the backup from when the
// caller did not name one: the first holding a backup this vault cannot open,
// which is by definition one written by the vault being recovered.
//
// Every account carries the same copy, so any of them will do — but preferring
// a foreign one matters when the vault has already written its own backup to an
// account it connected first.
func firstRecoverableAccount(ctx context.Context, v *vault.Vault) (string, error) {
	scan, err := v.ScanForRecovery(ctx)
	if err != nil {
		return "", err
	}
	for _, source := range scan.Sources {
		if source.Backup && source.Foreign {
			return source.ProviderID, nil
		}
	}
	for _, source := range scan.Sources {
		if source.Backup {
			return source.ProviderID, nil
		}
	}
	return "", fmt.Errorf("none of the connected accounts is holding a %s to recover from", vault.BackupKey)
}
