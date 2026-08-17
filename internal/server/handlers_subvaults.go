package server

import (
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The sub vault endpoints.
//
// Unlocking one is process-wide rather than per-session, which is the same rule
// the vault itself follows: the keys are in this process's memory, and a second
// browser talking to the same server is talking to the same open vault. What
// closes a sub vault is locking it, locking the vault, or the idle timeout
// pulling the keys out from under both.
//
// Everything here needs a session first — they are all behind requireSession —
// so "open the sub vault" is always a second password on top of the first,
// never a way around it.

// subVaultRequest is the body of the endpoints that name a sub vault by
// password.
type subVaultRequest struct {
	// Label names a sub vault being created or renamed.
	Label string `json:"label"`

	// Password is the sub vault's own, which is what opens it. It is never the
	// vault's — that one has already been used, to get a session at all.
	Password string `json:"password"`

	// NewPassword is only used by the password change.
	NewPassword string `json:"new_password"`

	// Migrate re-encrypts every file in the sub vault under the fresh key a
	// password change mints, before the call returns. A pointer so that
	// omitting it means yes: leaving files on the old key has to be asked for.
	Migrate *bool `json:"migrate"`
}

// subVaultsResponse is the list the settings panel draws.
type subVaultsResponse struct {
	SubVaults []vault.SubVaultInfo `json:"sub_vaults"`
}

func (s *Server) handleSubVaultsList(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	subs, err := v.SubVaults()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, subVaultsResponse{SubVaults: subs})
}

func (s *Server) handleSubVaultCreate(w http.ResponseWriter, r *http.Request) {
	var req subVaultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	info, err := v.CreateSubVault(req.Label, req.Password)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleSubVaultUnlock(w http.ResponseWriter, r *http.Request) {
	var req subVaultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.UnlockSubVault(r.PathValue("id"), req.Password); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "unlocked": true})
}

func (s *Server) handleSubVaultLock(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	if err := v.LockSubVault(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	// Locking one drops the cached chunks and the minted stream links, because
	// both are keys to plaintext and neither is filed by which vault it came
	// from. The links go here rather than in the vault, which knows nothing
	// about them.
	s.streams.clear()
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "unlocked": false})
}

func (s *Server) handleSubVaultRename(w http.ResponseWriter, r *http.Request) {
	var req subVaultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.RenameSubVault(r.PathValue("id"), req.Label); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "label": req.Label})
}

// handleSubVaultPassword changes one sub vault's password and rotates the key
// its files are stored under.
//
// Like the vault's own password change, the migration behind it is a download
// and an upload per file, so this request can run for a long time. The files
// stay readable throughout, and a client that gives up on the response has
// broken nothing: whatever moved stays moved, and the migrate endpoint finishes
// the rest.
func (s *Server) handleSubVaultPassword(w http.ResponseWriter, r *http.Request) {
	var req subVaultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	migrate := req.Migrate == nil || *req.Migrate

	v, _ := s.Vault()
	report, err := v.ChangeSubVaultPassword(r.Context(), r.PathValue("id"), req.Password, req.NewPassword, migrate)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleSubVaultMigrate(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	report, err := v.MigrateFilesIn(r.Context(), vault.Scope(r.PathValue("id")), nil, nil)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleSubVaultDelete erases a sub vault and everything in it.
//
// force is what a locked sub vault needs, because what is about to go cannot be
// listed. It is a query parameter rather than a body field so that the refusal
// and the confirmation are the same request with one flag different.
func (s *Server) handleSubVaultDelete(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	warnings, err := v.DeleteSubVault(ctx, r.PathValue("id"), force)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	s.streams.clear()
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "warnings": warnings})
}

// assignRequest moves a file or a folder from one vault inside the file to
// another.
type assignRequest struct {
	// From is the vault the target is in now, and To is where it should end up.
	// Either may be empty, meaning the main vault — assigning into a sub vault
	// and taking something back out are the same request with the two swapped.
	From string `json:"from"`
	To   string `json:"to"`

	// Target is a file ID, a file path, or a folder path.
	Target string `json:"target"`

	// Migrate re-encrypts the moved files under the destination's key before
	// the call returns. The browser leaves it off and starts the pass behind
	// the move, so that assigning a folder of films is instant rather than a
	// progress bar; the CLI waits by default.
	Migrate *bool `json:"migrate"`
}

func (s *Server) handleAssign(w http.ResponseWriter, r *http.Request) {
	var req assignRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "name the file or folder to assign", "BAD_REQUEST")
		return
	}
	migrate := req.Migrate != nil && *req.Migrate

	ctx, cancel := contextWithTimeout(r, relocateTimeout)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.Assign(ctx, vault.Scope(req.From), req.Target, vault.Scope(req.To), migrate)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// importRequest brings a vault found on an account in as a sub vault.
type importRequest struct {
	// Provider is the account holding the backup.
	Provider string `json:"provider"`

	// BackupPassword opens the found backup; Password is what the imported sub
	// vault will answer to from now on.
	BackupPassword string `json:"backup_password"`
	Password       string `json:"password"`

	// Label names it. Empty falls back to the account it came from.
	Label string `json:"label"`

	// AdoptBackup replaces the old recovery kit on that account with this
	// vault's own. A pointer so omitting it means yes: leaving a foreign backup
	// in place silently stops this vault replicating its index there.
	AdoptBackup *bool `json:"adopt_backup"`
}

func (s *Server) handleVaultImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "name the account holding the vault to import", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, relocateTimeout)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.ImportAsSubVault(ctx, req.Provider, req.BackupPassword, vault.ImportOptions{
		Label:       req.Label,
		Password:    req.Password,
		AdoptBackup: req.AdoptBackup == nil || *req.AdoptBackup,
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
