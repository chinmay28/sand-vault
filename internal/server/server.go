// Package server exposes the SAND vault over HTTP and serves the embedded
// file-browser frontend.
//
// The server is the only component that ever holds plaintext: it decodes an
// upload before scattering it, and rebuilds a file in memory to answer a
// download. It binds to loopback by default for exactly that reason.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sand-project/sand/internal/vault"
	"github.com/sand-project/sand/internal/version"
)

//go:embed all:dist
var webAssets embed.FS

// Server owns the vault handle and the HTTP surface around it.
type Server struct {
	Bind string
	Port int

	// VaultPath is the vault file to open. Empty means the default location.
	VaultPath string

	// IdleTimeout re-locks the vault after this much inactivity.
	IdleTimeout time.Duration

	// MaxUploadSize caps a single multipart upload request.
	MaxUploadSize int64

	vault    *vault.Vault
	sessions *sessionStore
}

// DefaultMaxUploadSize is the ceiling for one upload request.
const DefaultMaxUploadSize = 2 << 30 // 2 GiB

// APIError is a JSON error response.
type APIError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Error: msg, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// DefaultVaultPath is where the vault lives unless overridden: a per-user
// directory rather than the working directory, since it holds credentials.
func DefaultVaultPath() string {
	if custom := os.Getenv("SAND_VAULT"); custom != "" {
		return custom
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".sand", "vault.sand")
	}
	return filepath.Join(home, ".sand", "vault.sand")
}

// Vault returns the server's vault handle, opening it on first use.
func (s *Server) Vault() (*vault.Vault, error) {
	if s.vault != nil {
		return s.vault, nil
	}
	path := s.VaultPath
	if path == "" {
		path = DefaultVaultPath()
	}
	v, err := vault.Open(path)
	if err != nil {
		return nil, err
	}
	s.vault = v
	return v, nil
}

// Handler builds the full route table. It is exported so tests can drive the
// server without binding a port.
func (s *Server) Handler() (http.Handler, error) {
	if _, err := s.Vault(); err != nil {
		return nil, err
	}
	if s.sessions == nil {
		s.sessions = newSessionStore(s.IdleTimeout)
	}

	mux := http.NewServeMux()

	// --- Public: no session required -------------------------------------
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/vault", s.handleVaultStatus)
	mux.HandleFunc("POST /api/vault/init", s.handleVaultInit)
	mux.HandleFunc("POST /api/vault/unlock", s.handleVaultUnlock)
	mux.HandleFunc("GET /api/providers/specs", s.handleProviderSpecs)

	// --- Standalone mode: no vault, no accounts, just files in and out ----
	mux.HandleFunc("POST /api/archive", handleArchive)
	mux.HandleFunc("POST /api/restore", handleRestore)

	// --- Vault-backed: session required ----------------------------------
	protected := map[string]http.HandlerFunc{
		"POST /api/vault/lock":     s.handleVaultLock,
		"POST /api/vault/password": s.handleVaultPassword,
		"POST /api/vault/policy":   s.handleVaultPolicy,

		"GET /api/providers":            s.handleProvidersList,
		"POST /api/providers":           s.handleProviderAdd,
		"POST /api/providers/{id}/test": s.handleProviderTest,
		"DELETE /api/providers/{id}":    s.handleProviderRemove,

		"GET /api/files":              s.handleFilesList,
		"POST /api/files":             s.handleFilesUpload,
		"GET /api/files/{id}":         s.handleFileMeta,
		"DELETE /api/files/{id}":      s.handleFileDelete,
		"POST /api/files/{id}/move":   s.handleFileMove,
		"GET /api/files/{id}/health":  s.handleFileHealth,
		"GET /api/files/{id}/content": s.handleFileContent,

		"POST /api/folders":   s.handleFolderCreate,
		"DELETE /api/folders": s.handleFolderDelete,
	}
	for pattern, handler := range protected {
		mux.HandleFunc(pattern, s.requireSession(handler))
	}

	// --- Embedded single-page frontend -----------------------------------
	distFS, err := fs.Sub(webAssets, "dist")
	if err != nil {
		return nil, fmt.Errorf("failed to get embedded filesystem: %w", err)
	}
	fileServer := http.FileServer(http.FS(distFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "no such endpoint", "NOT_FOUND")
			return
		}
		// SPA routing: anything without a file extension renders the app.
		if r.URL.Path != "/" && !strings.Contains(filepath.Base(r.URL.Path), ".") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})

	return s.withSameOriginGuard(mux), nil
}

// Start initializes routes and starts the HTTP server.
func (s *Server) Start() error {
	handler, err := s.Handler()
	if err != nil {
		return err
	}

	go s.autoLockLoop()

	addr := net.JoinHostPort(s.Bind, fmt.Sprint(s.Port))
	v, _ := s.Vault()
	log.Printf("SAND %s starting on http://%s (vault: %s)", version.String(), addr, v.Path())
	if s.Bind != "127.0.0.1" && s.Bind != "localhost" && s.Bind != "::1" {
		log.Printf("WARNING: bound to %s — the vault password and decrypted files "+
			"will cross the network in the clear unless you put TLS in front of it", s.Bind)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 20 * time.Second,
	}
	return server.ListenAndServe()
}

// autoLockLoop locks the vault once every session has gone idle.
func (s *Server) autoLockLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		if s.sessions.sweep() == 0 {
			v.Lock()
			log.Print("vault auto-locked after idle timeout")
		}
	}
}

// requireSession rejects requests without a valid session, or when the vault
// has been locked out from under them.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.validate(sessionToken(r)) {
			writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
			return
		}
		v, err := s.Vault()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
			return
		}
		if !v.Unlocked() {
			writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
			return
		}
		next(w, r)
	}
}

// withSameOriginGuard blocks cross-origin state changes. Combined with the
// SameSite=Strict session cookie this stops a page on another site from
// driving the local API through the user's browser.
func (s *Server) withSameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin != "" && origin != "null" {
			u, err := url.Parse(origin)
			if err != nil || !sameHost(u.Host, r.Host) {
				writeError(w, http.StatusForbidden,
					"cross-origin request rejected", "CROSS_ORIGIN")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameHost compares two host:port values, tolerating a missing port.
func sameHost(a, b string) bool {
	if a == b {
		return true
	}
	hostA, _, err := net.SplitHostPort(a)
	if err != nil {
		hostA = a
	}
	hostB, _, err := net.SplitHostPort(b)
	if err != nil {
		hostB = b
	}
	return hostA == hostB
}

// isSecureRequest reports whether the connection is TLS-protected, so the
// session cookie only gets the Secure flag where it would actually work.
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// decodeJSON reads a JSON request body into out.
func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// vaultErrorResponse maps vault errors onto HTTP status codes and stable
// machine-readable codes the frontend switches on.
func vaultErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vault.ErrLocked):
		writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
	case errors.Is(err, vault.ErrWrongPassword):
		writeError(w, http.StatusUnauthorized, "wrong password", "WRONG_PASSWORD")
	case errors.Is(err, vault.ErrNotInitialized):
		writeError(w, http.StatusNotFound, "no vault has been created yet", "NO_VAULT")
	default:
		msg := err.Error()
		if strings.HasPrefix(msg, "no such file") || strings.HasPrefix(msg, "no such folder") {
			writeError(w, http.StatusNotFound, msg, "NOT_FOUND")
			return
		}
		writeError(w, http.StatusBadRequest, msg, "VAULT_ERROR")
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version.String()})
}
