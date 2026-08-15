package server

import (
	"net/http"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// vaultStatus is what the frontend polls to decide whether to show the lock
// screen, the setup screen, or the browser.
type vaultStatus struct {
	Initialized bool         `json:"initialized"`
	Unlocked    bool         `json:"unlocked"`
	Path        string       `json:"path"`
	Policy      vault.Policy `json:"policy"`
	Stats       *vault.Stats `json:"stats,omitempty"`

	// WebDAV describes the mountable share, and is absent when the server was
	// started without one. It is only told to an authenticated session: the
	// path is not a secret — the share answers 401 there whoever asks — but
	// there is no reason to help a stranger find a second door.
	WebDAV *webdavStatus `json:"webdav,omitempty"`
}

// webdavStatus is what the browser needs to tell someone how to mount the
// share. The address is deliberately not included: the browser already knows
// which host it reached this server on, and that is the one that will work from
// the machine doing the mounting — a name the server guessed for itself might
// not resolve anywhere else.
type webdavStatus struct {
	Path string `json:"path"`
}

func (s *Server) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}

	status := vaultStatus{
		Initialized: v.Initialized(),
		Path:        v.Path(),
		Policy:      v.Policy(),
	}
	// Only an authenticated session gets to see what is inside.
	if v.Unlocked() && s.sessions.validate(sessionToken(r)) {
		status.Unlocked = true
		if stats, err := v.Stats(); err == nil {
			status.Stats = &stats
		}
		if s.WebDAV {
			status.WebDAV = &webdavStatus{Path: s.webdavPrefix() + "/"}
		}
	}

	writeJSON(w, http.StatusOK, status)
}

type initRequest struct {
	Password string       `json:"password"`
	Policy   vault.Policy `json:"policy"`
}

func (s *Server) handleVaultInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Policy == "" {
		req.Policy = vault.PolicyStrict
	}

	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}
	if err := v.Init(req.Password, req.Policy); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	s.startSession(w, r)
	writeJSON(w, http.StatusCreated, vaultStatus{
		Initialized: true,
		Unlocked:    true,
		Path:        v.Path(),
		Policy:      v.Policy(),
	})
}

type unlockRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	var req unlockRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}
	if err := v.Unlock(req.Password); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	s.startSession(w, r)

	status := vaultStatus{Initialized: true, Unlocked: true, Path: v.Path(), Policy: v.Policy()}
	if stats, err := v.Stats(); err == nil {
		status.Stats = &stats
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}

	// Locking is global: the keys leave memory, so every session goes with it.
	s.sessions.clear()
	v.Lock()
	clearSessionCookie(w, isSecureRequest(r))

	writeJSON(w, http.StatusOK, vaultStatus{Initialized: true, Path: v.Path()})
}

type passwordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`

	// Migrate re-encrypts every stored file under the new data key before the
	// call returns. A pointer so that omitting it means yes: leaving files on
	// the old key has to be asked for, not fallen into.
	Migrate *bool `json:"migrate"`
}

// handleVaultPassword changes the password and, unless asked not to,
// re-encrypts every stored file under the fresh data key that comes with it.
//
// The migration is a download and an upload per file, so this request can run
// for a long time on a large vault. Files stay readable throughout, and a
// client that gives up on the response is not a client that broke anything:
// whatever moved stays moved, and POST /api/vault/migrate finishes the rest.
func (s *Server) handleVaultPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	migrate := req.Migrate == nil || *req.Migrate

	v, _ := s.Vault()
	report, err := v.ChangePassword(r.Context(), req.OldPassword, req.NewPassword, migrate)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleVaultMigrate finishes a re-encryption an earlier password change did
// not complete, because it was interrupted, deferred, or held up by an account
// that was offline at the time.
func (s *Server) handleVaultMigrate(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	report, err := v.MigrateFiles(r.Context(), nil)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

type defaultAccountsRequest struct {
	// Accounts is the vault-wide selection new uploads start from. An empty
	// list clears it, which puts every upload back to picking its own accounts
	// at random.
	Accounts []string `json:"accounts"`
}

// handleVaultDefaults records which accounts uploads should use when they do
// not name their own.
func (s *Server) handleVaultDefaults(w http.ResponseWriter, r *http.Request) {
	var req defaultAccountsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.SetDefaultAccounts(req.Accounts); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default_accounts": v.DefaultAccounts()})
}

type policyRequest struct {
	Policy vault.Policy `json:"policy"`
}

func (s *Server) handleVaultPolicy(w http.ResponseWriter, r *http.Request) {
	var req policyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.SetPolicy(req.Policy); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": v.Policy()})
}

// startSession issues a session cookie after a successful init or unlock.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	token, err := s.sessions.issue()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start a session", "INTERNAL_ERROR")
		return
	}
	setSessionCookie(w, token, isSecureRequest(r), s.sessions.idleTimeout)
}
