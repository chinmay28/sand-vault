// Package server exposes the SAND Vault index over HTTP and serves the embedded
// file-browser frontend.
//
// The server is the only component that ever holds plaintext: it decodes an
// upload before scattering it, and rebuilds a file in memory to answer a
// download. It binds to loopback by default for exactly that reason.
package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/chinmay28/sand-vault/internal/version"
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

	vault      *vault.Vault
	sessions   *sessionStore
	oauthFlows *oauthFlowStore
}

// DefaultPort is the port `sand serve` binds unless told otherwise.
const DefaultPort = 8123

// DefaultBind is the address `sand serve` binds unless told otherwise.
//
// All interfaces, so a freshly installed service is reachable from the other
// machines on your network without a second step. Note what that means: this
// server takes the vault password over the wire and sends rebuilt, decrypted
// files back, so on a bare HTTP listener both are in the clear to anyone on
// the network, and /api/vault/unlock is reachable unauthenticated. Start()
// warns on every non-loopback bind for that reason. Put TLS in front of it.
const DefaultBind = "0.0.0.0"

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
	if s.oauthFlows == nil {
		s.oauthFlows = newOAuthFlowStore()
	}

	mux := http.NewServeMux()

	// --- Public: no session required -------------------------------------
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/vault", s.handleVaultStatus)
	mux.HandleFunc("POST /api/vault/init", s.handleVaultInit)
	mux.HandleFunc("POST /api/vault/unlock", s.handleVaultUnlock)
	mux.HandleFunc("GET /api/providers/specs", s.handleProviderSpecs)

	// The provider sends the browser back here after the account holder has
	// signed in. It arrives as a cross-site navigation, which the session
	// cookie deliberately does not survive, so the flow's state parameter is
	// what authenticates it.
	mux.HandleFunc("GET "+oauthCallbackPath, s.handleOAuthCallback)

	// --- Standalone mode: no vault, no accounts, just files in and out ----
	mux.HandleFunc("POST /api/archive", handleArchive)
	mux.HandleFunc("POST /api/restore", handleRestore)

	// --- Vault-backed: session required ----------------------------------
	protected := map[string]http.HandlerFunc{
		"POST /api/vault/lock":     s.handleVaultLock,
		"POST /api/vault/password": s.handleVaultPassword,
		"POST /api/vault/migrate":  s.handleVaultMigrate,
		"POST /api/vault/policy":   s.handleVaultPolicy,
		"POST /api/vault/defaults": s.handleVaultDefaults,

		"GET /api/providers":            s.handleProvidersList,
		"POST /api/providers":           s.handleProviderAdd,
		"POST /api/providers/{id}/test": s.handleProviderTest,
		"DELETE /api/providers/{id}":    s.handleProviderRemove,

		"POST /api/providers/oauth/start":    s.handleOAuthStart,
		"POST /api/providers/oauth/exchange": s.handleOAuthExchange,
		"POST /api/providers/oauth/complete": s.handleOAuthComplete,
		"GET /api/providers/oauth/{id}":      s.handleOAuthStatus,

		"GET /api/search":             s.handleSearch,
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
	etags, err := buildETags(distFS)
	if err != nil {
		return nil, fmt.Errorf("failed to fingerprint embedded assets: %w", err)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "no such endpoint", "NOT_FOUND")
			return
		}
		// SPA routing: anything without a file extension renders the app.
		if r.URL.Path != "/" && !strings.Contains(filepath.Base(r.URL.Path), ".") {
			r.URL.Path = "/"
		}
		setAssetCaching(w, r.URL.Path, etags)
		fileServer.ServeHTTP(w, r)
	})

	return s.withSameOriginGuard(mux), nil
}

// buildETags fingerprints every embedded frontend file by content.
//
// The assets are baked into the binary, so a hash of the bytes is fixed for the
// life of the process and changes only when the build does — which is exactly
// what a validator is supposed to mean. Computed once here rather than per
// request; the whole frontend is a few hundred kilobytes.
func buildETags(distFS fs.FS) (map[string]string, error) {
	tags := make(map[string]string)

	err := fs.WalkDir(distFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, readErr := fs.ReadFile(distFS, p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		tag := `"` + hex.EncodeToString(sum[:8]) + `"`
		tags["/"+p] = tag
		// The SPA rewrite turns every app route into a request for "/".
		if p == "index.html" {
			tags["/"] = tag
		}
		return nil
	})

	return tags, err
}

// setAssetCaching tells the browser how long it may keep each part of the
// frontend.
//
// Without this the response carries no Cache-Control, no ETag and no
// Last-Modified — the embed filesystem reports a zero modification time, so
// net/http omits that too. A cache handed no expiry information at all is
// allowed to invent one (RFC 9111 §4.2.2), and mobile browsers invent
// generously. index.html is the file that names the current bundle, so a
// browser holding a stale copy pins the entire app to the build it first
// loaded: a phone that has opened the vault once keeps serving itself that
// frontend through any number of deploys, which is exactly what happened.
//
// Vite fingerprints everything under /assets/, so those may be kept forever —
// a rebuild changes the filename rather than the bytes behind one. Everything
// else revalidates on every load, which the ETag settles in a 304.
func setAssetCaching(w http.ResponseWriter, path string, etags map[string]string) {
	if tag, ok := etags[path]; ok {
		// Set before serving: net/http answers If-None-Match off this header.
		w.Header().Set("ETag", tag)
	}

	if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
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
	log.Printf("SAND Vault %s starting on http://%s (vault: %s)", version.String(), addr, v.Path())
	if warnsOnBind(s.Bind) {
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

// warnsOnBind reports whether an address is reachable from off this machine,
// and so deserves the plaintext warning.
func warnsOnBind(bind string) bool {
	switch bind {
	case "127.0.0.1", "localhost", "::1":
		return false
	}
	return true
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
