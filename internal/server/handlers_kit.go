package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The recovery kit over HTTP.
//
// Two of these routes sit outside the session, and for the same reason
// /api/vault/init does: on the machine where they matter there is no vault to
// have a session with. What stands in front of them is possession of the file
// plus the secret that opens it, and — for the import — a refusal to run
// against a vault that already holds files.

// maxKitUpload bounds an uploaded kit. A kit is an index rather than content,
// and a gigabyte of JSON is a vault far larger than this format was drawn for.
const maxKitUpload = 1 << 30

// handleKitStatus reports on the last kit this vault exported: how stale it
// has become, and whether the code for it can still be shown.
func (s *Server) handleKitStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	status, err := v.KitStatus()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// kitExportRequest is what the export dialog sends.
//
// A POST rather than a GET because it carries a password on the opt-out path,
// and because the recovery code has to come back in a response body rather than
// in anything that could end up in a URL, a log or a browser history.
type kitExportRequest struct {
	UseVaultPassword bool   `json:"use_vault_password"`
	Password         string `json:"password"`
}

// handleKitExport builds a kit and streams it to the browser as a zip.
//
// The code rides in a header because the body is the archive and there is
// nowhere else for it to go. That is a narrower concession than it looks:
// anything positioned to read this response's headers is equally positioned to
// read its body, and the body is every credential this vault holds. The header
// is not a second channel — it is the same one.
//
// What the code must stay out of is everything with a longer life than this
// response: the archive, its filename, fingerprint.txt, and the process log.
// It is kept out of all four.
func (s *Server) handleKitExport(w http.ResponseWriter, r *http.Request) {
	var req kitExportRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.UseVaultPassword && req.Password == "" {
		writeError(w, http.StatusBadRequest,
			"sealing a kit under your vault password needs that password", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()

	// Built into memory first: a failure halfway through must produce an error
	// the browser can read, not a truncated zip with a 200 on it.
	var buf writeCounter
	fingerprint, err := v.ExportKit(vault.KitExportOptions{
		UseVaultPassword: req.UseVaultPassword,
		Password:         req.Password,
	}, &buf)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	name := fmt.Sprintf("sand-recovery-kit-%s.zip", fingerprint.CreatedAt.Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Length", fmt.Sprint(len(buf.data)))
	w.Header().Set("X-Sand-Kit-Id", fingerprint.KitID)
	w.Header().Set("X-Sand-Kit-Secret", fingerprint.Secret)
	w.Header().Set("X-Sand-Kit-Sha256", fingerprint.SHA256)
	if fingerprint.Code != "" {
		w.Header().Set("X-Sand-Kit-Code", fingerprint.Code)
	}
	// Never cached, never stored: the archive is every credential this vault
	// holds, and a copy of it in a proxy or a disk cache is a copy nobody
	// decided to make.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write(buf.data)
}

// writeCounter collects a kit in memory so its size is known before a byte of
// it is sent.
type writeCounter struct{ data []byte }

func (c *writeCounter) Write(p []byte) (int, error) {
	c.data = append(c.data, p...)
	return len(p), nil
}

// handleKitCode reads back the code this vault is holding for a kit it made.
func (s *Server) handleKitCode(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	code, err := v.KitCode(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if code == "" {
		writeError(w, http.StatusNotFound,
			"this vault is not holding a recovery code for that kit", "NO_KIT_CODE")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// handleKitForgetCode drops a retained code.
func (s *Server) handleKitForgetCode(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	if err := v.ForgetKitCode(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"forgotten": true})
}

// readUploadedKit pulls kit.sand out of a multipart request.
func readUploadedKit(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxKitUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("could not read the kit: %v", err), "PARSE_ERROR")
		return nil, false
	}

	file, _, err := r.FormFile("kit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing kit field", "MISSING_FILE")
		return nil, false
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxKitUpload))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "PARSE_ERROR")
		return nil, false
	}

	sealed, err := vault.ReadKitZip(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "NOT_A_KIT")
		return nil, false
	}
	return sealed, true
}

// handleKitInspect says what a kit is without opening it: which secret it
// wants, when it was made, and which kit it is.
//
// Outside the session because the import screen it feeds runs on a machine
// with no vault, and it reveals nothing — every field it returns is in the
// clear inside the envelope already.
func (s *Server) handleKitInspect(w http.ResponseWriter, r *http.Request) {
	sealed, ok := readUploadedKit(w, r)
	if !ok {
		return
	}
	env, err := vault.InspectKit(sealed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "NOT_A_KIT")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kit_id":     env.KitID,
		"created_at": env.CreatedAt,
		"secret":     env.Secret,
	})
}

// handleKitImport runs the import and starts a session on success.
func (s *Server) handleKitImport(w http.ResponseWriter, r *http.Request) {
	sealed, ok := readUploadedKit(w, r)
	if !ok {
		return
	}

	secret := r.FormValue("secret")
	if secret == "" {
		writeError(w, http.StatusBadRequest,
			"the secret that opens this kit is required", "BAD_REQUEST")
		return
	}
	password := r.FormValue("password")
	if password == "" {
		writeError(w, http.StatusBadRequest,
			"choose a password for the recovered vault", "BAD_REQUEST")
		return
	}

	kit, err := vault.OpenKit(sealed, secret)
	if err != nil {
		kitOpenErrorResponse(w, err)
		return
	}

	// Long enough for a listing of every account plus the rebuild itself,
	// which on a large vault is a lot of object keys to page through.
	ctx, cancel := contextWithTimeout(r, 15*time.Minute)
	defer cancel()

	// This route is outside the session by design — the machine it runs on has
	// no vault to have a session with — which means that for the whole of the
	// import the idle sweep counts zero sessions and would lock the vault the
	// import has just created, one tick in. The vault refuses to lose its keys
	// mid-import either way (vault.holdKeys), but the sweep should not be
	// trying: this counts as use for exactly as long as the import runs, the
	// same way a sweep or a mounted share does.
	s.noteExternalActivity()
	defer s.noteExternalActivity()

	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}

	report, err := v.ImportKit(ctx, kit, vault.KitImportOptions{
		Password:       password,
		Replace:        r.FormValue("replace") == "true" || r.FormValue("replace") == "1",
		OldPassword:    r.FormValue("old_password"),
		SkipCloudIndex: r.FormValue("skip_cloud_index") == "true" || r.FormValue("skip_cloud_index") == "1",
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	s.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"report": report,
		"status": s.unlockedStatus(v),
	})
}

// handleKitVerify runs the fire drill against the live accounts.
func (s *Server) handleKitVerify(w http.ResponseWriter, r *http.Request) {
	sealed, ok := readUploadedKit(w, r)
	if !ok {
		return
	}

	secret := r.FormValue("secret")
	if secret == "" {
		writeError(w, http.StatusBadRequest,
			"the secret that opens this kit is required — proving you still have it is half of "+
				"what this test is for", "BAD_REQUEST")
		return
	}

	kit, err := vault.OpenKit(sealed, secret)
	if err != nil {
		kitOpenErrorResponse(w, err)
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.VerifyKit(ctx, kit)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// kitOpenErrorResponse gives each way of failing to open a kit its own code,
// because they need different words in front of a frightened user: a typo is
// the typist's and fixable, a wrong code means they are holding two kits, and a
// damaged file means the secret was right and the bytes are not.
func kitOpenErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vault.ErrKitCodeTypo):
		writeError(w, http.StatusBadRequest, err.Error(), "KIT_CODE_TYPO")
	case errors.Is(err, vault.ErrKitCodeLength):
		writeError(w, http.StatusBadRequest, err.Error(), "KIT_CODE_LENGTH")
	case errors.Is(err, vault.ErrKitDamaged):
		writeError(w, http.StatusBadRequest, err.Error(), "KIT_DAMAGED")
	case errors.Is(err, vault.ErrNotAKit):
		writeError(w, http.StatusBadRequest, err.Error(), "NOT_A_KIT")
	case errors.Is(err, vault.ErrWrongPassword):
		writeError(w, http.StatusUnauthorized, err.Error(), "WRONG_PASSWORD")
	default:
		writeError(w, http.StatusBadRequest, err.Error(), "KIT_SECRET_WRONG")
	}
}
