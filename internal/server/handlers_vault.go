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
}

func (s *Server) handleVaultPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.ChangePassword(req.OldPassword, req.NewPassword); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
