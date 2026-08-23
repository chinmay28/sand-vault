package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
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

	// SubVaults are the vaults inside this one, each with whether it is
	// currently open. Told to an authenticated session and no one else: that
	// there is a sub vault called Taxes is exactly the kind of thing the lock
	// screen should not be volunteering.
	SubVaults []vault.SubVaultInfo `json:"sub_vaults,omitempty"`

	// ManifestBackup says whether the index is being replicated to the
	// connected accounts. It has been settable from the CLI and invisible in
	// the app, which meant the one setting that decides whether a lost machine
	// is recoverable could only be seen by someone who went looking.
	ManifestBackup bool `json:"manifest_backup"`
}

// webdavStatus is what the browser needs to tell someone how to mount the
// share. The address is deliberately not included: the browser already knows
// which host it reached this server on, and that is the one that will work from
// the machine doing the mounting — a name the server guessed for itself might
// not resolve anywhere else.
type webdavStatus struct {
	Path string `json:"path"`
}

// unlockedStatus describes a vault that the caller has just been given access
// to, whether by unlocking it, creating it, or asking about one already open.
//
// It exists because those three answers used to be built separately and drifted:
// the WebDAV share was added to the one behind GET /api/vault and not to the one
// the unlock returns, so the browser — which takes its state straight from the
// unlock response — never heard about the share until something else happened
// to refetch. Anything the browser needs on sight of an open vault belongs here,
// once.
func (s *Server) unlockedStatus(v *vault.Vault) vaultStatus {
	status := vaultStatus{
		Initialized: true,
		Unlocked:    true,
		Path:        v.Path(),
		Policy:      v.Policy(),
	}
	if stats, err := v.Stats(); err == nil {
		status.Stats = &stats
	}
	if subs, err := v.SubVaults(); err == nil {
		status.SubVaults = subs
	}
	status.ManifestBackup = v.BackupEnabled()
	if s.WebDAV {
		status.WebDAV = &webdavStatus{Path: s.webdavPrefix() + "/"}
	}
	return status
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
		status = s.unlockedStatus(v)
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
	writeJSON(w, http.StatusCreated, s.unlockedStatus(v))
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
	writeJSON(w, http.StatusOK, s.unlockedStatus(v))
}

func (s *Server) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}

	// Locking is global: the keys leave memory, so every session goes with it —
	// and so does every stream link, which was minted against those keys, and
	// every import still running. An import that carried on would be sealing
	// chunks with a key that is about to be zeroed, and would fail further in
	// having spent the bandwidth first; stopping it costs only what it had not
	// fetched yet, since what it did fetch is already committed.
	s.sessions.clear()
	s.streams.clear()
	s.imports.stopAll()
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
	// Accounts is the vault-wide selection new uploads start from. How many are
	// named settles the erasure code unless Scheme names one — 3 is 2-of-3, 6 is
	// 4-of-6, 9 is 6-of-9. An empty list clears it, which puts every upload back
	// to picking its own accounts at random.
	Accounts []string `json:"accounts"`

	// Scheme is the code those uploads are cut with, written "k-of-n", and it
	// has to be as wide as the accounts are many. Empty clears it and hands the
	// code back to the count.
	//
	// The two are set together, as one object, because neither can be checked
	// without the other: 3-of-5 is only a default while five accounts are named
	// under it. A request that sends accounts and omits the scheme is therefore
	// clearing the scheme, not leaving it alone.
	Scheme string `json:"scheme,omitempty"`
}

// handleVaultDefaults records which accounts uploads should use when they do
// not name their own, and which code to cut them with.
func (s *Server) handleVaultDefaults(w http.ResponseWriter, r *http.Request) {
	var req defaultAccountsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	scheme, err := parseSchemeField(req.Scheme)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_SCHEME")
		return
	}

	v, _ := s.Vault()
	if err := v.SetDefaults(req.Accounts, scheme); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_accounts": v.DefaultAccounts(),
		"default_scheme":   schemeField(v.DefaultScheme()),
	})
}

// schemeField writes a scheme the way the API carries one, with the zero value
// as the empty string rather than as "0-of-0".
func schemeField(scheme archive.Scheme) string {
	if scheme == (archive.Scheme{}) {
		return ""
	}
	return scheme.String()
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

type backupRequest struct {
	Enabled bool `json:"enabled"`

	// Force claims the connected accounts for this vault, overwriting a copy
	// of the index it cannot open rather than leaving it alone.
	//
	// What needs it is an account repaired *after* a recovery. The push that
	// follows a recovery forces, and claims every account it can reach; one
	// that was unreachable then — or that has since been pointed at where its
	// parts really are — still holds the index of the vault that died, and the
	// guard protecting a foreign backup will refuse it forever. That guard is
	// right in general and wrong here, because this vault has already been
	// told, with the password and the kit, that these accounts are its own.
	Force bool `json:"force"`
}

// handleVaultBackup turns replication of the index to the connected accounts on
// or off — or, with force, claims those accounts for this vault.
//
// Turning it off erases the copies that are already out there, which is the
// point of asking for it: the setting exists for someone who does not want a
// file naming every one of their files sitting on a cloud account, and leaving
// the old copies behind would make it a setting that changed nothing.
//
// Forcing does the opposite of leaving things alone: it overwrites a copy this
// vault cannot open. See backupRequest.Force for the one situation that wants
// that. It is a no-op on a vault whose replication is switched off, since there
// is then nothing to claim an account with.
func (s *Server) handleVaultBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	v, _ := s.Vault()

	var warnings []string
	var err error
	if req.Force {
		warnings, err = v.SyncManifestBackup(ctx, true)
		// A configuration that refuses to hold a backup is the answer to a
		// claim rather than a failure of one: there is nothing to write, and
		// the caller wanted the accounts left consistent, which they are. Only
		// the forced path is forgiving of it — a refused *enable* has to keep
		// carrying its reason, since by then the copies that were there have
		// already been erased.
		if errors.Is(err, vault.ErrBackupRefused) {
			warnings = append(warnings, err.Error())
			err = nil
		}
	} else {
		warnings, err = v.SetBackupEnabled(ctx, req.Enabled)
	}
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"manifest_backup": v.BackupEnabled(),
		// Whether the claim did anything. Replication switched off means there
		// is no index to claim an account with, and a caller that reported
		// success on that would be reporting a push that never happened.
		"claimed":  req.Force && v.BackupEnabled(),
		"warnings": warnings,
	})
}
