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
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/davfs"
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

	// WebDAV mounts the vault as a WebDAV share at WebDAVPrefix.
	//
	// Off by default because it is a second way in, authenticated by the vault
	// password over HTTP Basic — which on a plain listener crosses the network
	// in the clear on every request, not only at sign-in.
	WebDAV bool

	// WebDAVPrefix is the path the share is mounted under. Empty uses
	// DefaultWebDAVPrefix.
	WebDAVPrefix string

	// MovieBaseURL and MovieImageBaseURL point the film lookup somewhere other
	// than the real database. Empty — which is everything but a test — means
	// the addresses in internal/movie, and a test that left them empty would be
	// a test that calls a stranger's API.
	MovieBaseURL      string
	MovieImageBaseURL string

	vault      *vault.Vault
	sessions   *sessionStore
	streams    *streamStore
	oauthFlows *oauthFlowStore

	// generatedKeys holds SSH key pairs SAND made for a connect form that has
	// not been submitted yet. The private half never leaves this process — see
	// handlers_sshkeys.go.
	generatedKeys *generatedKeyStore

	// imports is where the imports running right now say how far they have
	// got, so the dialog that started one can draw it. It is a view of the
	// requests in flight and dies with them — see import_watch.go.
	imports *importWatch

	// externalActivity is when something outside the browser last read the
	// vault — a mounted share, or a player following a stream link. Neither has
	// a browser session to keep the vault alive, so without this it would lock
	// itself halfway through a film.
	externalMu       sync.Mutex
	externalActivity time.Time
}

// DefaultWebDAVPrefix is where the share is mounted unless told otherwise.
const DefaultWebDAVPrefix = "/dav"

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
	v, err := s.Vault()
	if err != nil {
		return nil, err
	}
	if s.sessions == nil {
		s.sessions = newSessionStore(s.IdleTimeout)
	}
	if s.streams == nil {
		s.streams = newStreamStore(0)
	}
	if s.oauthFlows == nil {
		s.oauthFlows = newOAuthFlowStore()
	}
	if s.generatedKeys == nil {
		s.generatedKeys = newGeneratedKeyStore()
	}
	if s.imports == nil {
		s.imports = newImportWatch()
	}

	mux := http.NewServeMux()

	// --- WebDAV: the vault as a mountable share, off unless asked for -----
	if s.WebDAV {
		prefix := s.webdavPrefix()
		dav, err := davfs.Handler(v, davfs.Options{
			Prefix:     prefix,
			Realm:      "SAND Vault",
			OnActivity: s.noteExternalActivity,
		})
		if err != nil {
			return nil, err
		}
		// Both forms: a client asking about the share itself sends the bare
		// prefix, and everything inside it arrives with the trailing slash.
		mux.Handle(prefix, dav)
		mux.Handle(prefix+"/", dav)
	}

	// --- Public: no session required -------------------------------------
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/vault", s.handleVaultStatus)
	mux.HandleFunc("POST /api/vault/init", s.handleVaultInit)
	mux.HandleFunc("POST /api/vault/unlock", s.handleVaultUnlock)
	mux.HandleFunc("GET /api/providers/specs", s.handleProviderSpecs)

	// Reading a recovery kit, and importing one. Outside the session in the
	// same way and for the same reason /api/vault/init is: on the machine
	// where they matter there is no vault to have a session with. What stands
	// in front of them is possession of the file plus the secret that opens
	// it, and the import's own refusal to run against a vault holding files.
	// See handlers_kit.go.
	mux.HandleFunc("POST /api/vault/kit/inspect", s.handleKitInspect)
	mux.HandleFunc("POST /api/vault/kit/import", s.handleKitImport)

	// A stream link carries its own credential in the path, which is the point
	// of it: the player following one is not the browser and has no session to
	// offer. Registered for GET, which the router answers HEAD on too — a
	// player asks for the length and the range support before the first byte.
	mux.HandleFunc("GET /stream/{token}", s.handleStream)
	mux.HandleFunc("GET /stream/{token}/{name}", s.handleStream)

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
		"POST /api/vault/backup":   s.handleVaultBackup,

		// Disaster recovery: what the accounts are still holding after the
		// machine that held the vault is gone, and rebuilding the index from
		// it. See handlers_recovery.go.
		"GET /api/vault/recovery":         s.handleRecoveryScan,
		"POST /api/vault/recovery":        s.handleRecoveryRun,
		"POST /api/vault/recovery/resume": s.handleRecoveryResume,
		// Taking recovered files off the dead vault's key and onto this one's,
		// on whichever accounts are named.
		"POST /api/vault/reclaim": s.handleVaultReclaim,

		// Parts on the accounts that no index points at any more, which is what
		// deleting a file while one of its clouds was disconnected leaves
		// behind — the account comes back with a new ID and nothing ever goes
		// looking for them again. Looking is a GET and erasing is a POST, and
		// the POST re-scans before it deletes anything. See handlers_orphans.go.
		// The same listing answers two questions, and only one of them is
		// about deleting: a part with no record can belong to a file that is
		// still in the tree — a disconnect drops the records naming the
		// account it removes — in which case the repair is to write the record
		// back, which moves nothing.
		"GET /api/vault/orphans":           s.handleOrphanScan,
		"POST /api/vault/orphans":          s.handleOrphanSweep,
		"POST /api/vault/orphans/reattach": s.handleOrphanReattach,
		// The recovery kit: one sealed file that reconnects every cloud on a
		// fresh install, rather than only rebuilding the index. Exporting one
		// and testing one both need the vault open; reading and importing one
		// do not, and are above. See handlers_kit.go.
		"GET /api/vault/kit":         s.handleKitStatus,
		"POST /api/vault/kit":        s.handleKitExport,
		"POST /api/vault/kit/verify": s.handleKitVerify,
		// The code a kit was sealed under, for somebody who still has their
		// working vault and has mislaid the slip of paper. Worthless to
		// anybody who could not already export a fresh kit.
		"GET /api/vault/kit/code/{id}":    s.handleKitCode,
		"DELETE /api/vault/kit/code/{id}": s.handleKitForgetCode,

		"GET /api/providers":            s.handleProvidersList,
		"POST /api/providers":           s.handleProviderAdd,
		"POST /api/providers/{id}/test": s.handleProviderTest,
		// One account taken apart: its quota against what SAND actually put
		// there, and what the rest of its load is made of.
		//
		// The id trails the verb rather than leading it, which is the wrong way
		// round for a REST path and the only way round the router will take:
		// "GET /api/providers/{id}/stats" and the sign-in status route below
		// both match "/api/providers/oauth/stats", neither is more specific
		// than the other, and ServeMux refuses the pair outright. Moving the
		// older route would be a break for the sake of a nicer new path.
		"GET /api/providers/stats/{id}": s.handleProviderStats,
		// The expensive half of that panel, kept apart from it: what is
		// actually in a bucket, counted by listing it. Only the backends with
		// no quota call have one, and nothing calls it on a schedule.
		"POST /api/providers/{id}/measure": s.handleProviderMeasure,
		// What an account is called, what colour it wears, and how big its
		// holder says it is. None of the three is a credential and none of them
		// moves a byte, so this is the one write against an account that never
		// touches the backend holding it.
		"PATCH /api/providers/{id}":  s.handleProviderUpdate,
		"DELETE /api/providers/{id}": s.handleProviderRemove,

		// Which accounts have been winning the race every read runs, over
		// today, this month, this year or all of it. Counters the read path
		// already keeps, so this costs nothing and touches nobody's storage.
		// See handlers_reads.go.
		"GET /api/reads":         s.handleReadStats,
		"POST /api/reads/forget": s.handleReadStatsForget,

		// Folders on this machine, for the backends configured with a path.
		"GET /api/system/folders": s.handleSystemFolders,

		"POST /api/providers/proton/signin":  s.handleProtonSignIn,
		"POST /api/providers/oauth/start":    s.handleOAuthStart,
		"POST /api/providers/oauth/exchange": s.handleOAuthExchange,
		"POST /api/providers/oauth/complete": s.handleOAuthComplete,
		// The same finished sign-in, spent on an account that is already
		// connected: new credentials for the same ID, the same name and the
		// same parts. What answers a revoked consent or a deleted OAuth client
		// without disconnecting the account.
		"POST /api/providers/oauth/reauthorize": s.handleOAuthReauthorize,
		"GET /api/providers/oauth/{id}":         s.handleOAuthStatus,

		// The vaults inside the vault. Creating, opening and closing one all
		// sit behind the session, so a sub vault's password is always a second
		// password on top of the first rather than a way around it.
		"GET /api/subvaults":                s.handleSubVaultsList,
		"POST /api/subvaults":               s.handleSubVaultCreate,
		"POST /api/subvaults/{id}/unlock":   s.handleSubVaultUnlock,
		"POST /api/subvaults/{id}/lock":     s.handleSubVaultLock,
		"PATCH /api/subvaults/{id}":         s.handleSubVaultRename,
		"POST /api/subvaults/{id}/password": s.handleSubVaultPassword,
		"POST /api/subvaults/{id}/migrate":  s.handleSubVaultMigrate,
		"DELETE /api/subvaults/{id}":        s.handleSubVaultDelete,

		// Moving a file or a folder from one vault inside the file to another.
		// One endpoint for both directions, because assigning into a sub vault
		// and taking something back out are the same operation with the two
		// scopes swapped.
		"POST /api/assign": s.handleAssign,

		// Bringing a vault found on an account in as a sub vault, rather than
		// recovering over this one. Which accounts hold one is already the
		// recovery scan's answer, above. The DELETE is the other answer to the
		// same finding: an old install nobody wants back, whose index would
		// otherwise be offered on that account forever.
		"POST /api/vaults/import":             s.handleVaultImport,
		"DELETE /api/vaults/found/{provider}": s.handleFoundVaultDiscard,

		"GET /api/search": s.handleSearch,
		"GET /api/files":  s.handleFilesList,
		// The files behind the "missing a spare part" figure the accounts panel
		// shows. Paged, and a read of the index alone — putting a part back is
		// POST /api/relocate, the same call the file list's own move uses.
		"GET /api/degraded":          s.handleDegradedList,
		"POST /api/files":            s.handleFilesUpload,
		"GET /api/files/{id}":        s.handleFileMeta,
		"DELETE /api/files/{id}":     s.handleFileDelete,
		"POST /api/files/{id}/move":  s.handleFileMove,
		"GET /api/files/{id}/health": s.handleFileHealth,
		// Converting a pre-chunking file into the chunked format, which is what
		// the read path's refusal asks for. See handlers_convert.go.
		"GET /api/conversions":         s.handleConversionsPending,
		"POST /api/files/{id}/convert": s.handleFileConvert,
		"GET /api/files/{id}/content":  s.handleFileContent,
		"POST /api/files/{id}/stream":  s.handleFileStreamLink,
		"GET /api/files/{id}/thumb":    s.handleFileThumb,
		"PUT /api/files/{id}/thumb":    s.handleFileThumbSet,

		// The whole folder tree in one answer, which is what choosing a
		// destination needs — GET /api/files walks one level at a time.
		"GET /api/folders":    s.handleFoldersList,
		"POST /api/folders":   s.handleFolderCreate,
		"DELETE /api/folders": s.handleFolderDelete,
		// The picture a folder is drawn with: which file's thumbnail stands for
		// it, and which others could. Nothing is stored by choosing — the
		// answer is a file ID, drawn through that file's own thumbnail.
		"GET /api/folders/art":  s.handleFolderArt,
		"POST /api/folders/art": s.handleFolderArtSet,
		// Everything under a folder in one walk of the index, which is what
		// tidying one up has to see before it changes anything. Read-only: the
		// organizer's four tools plan from this answer and then run over the
		// move, delete and remove-folder endpoints above, one item at a time.
		"GET /api/folders/survey": s.handleFolderSurvey,
		// The copies under a folder, asked three ways in one walk: the same
		// bytes, the same length, or names alike enough to be copies of each
		// other. Read-only like the survey, and for the same reason — what is
		// removed goes through DELETE /api/files/{id}, one file at a time.
		"GET /api/folders/duplicates": s.handleFolderDuplicates,
		// Moving a folder within the vault, which moves everything under it.
		// No part leaves the account it is on: a folder is a path in the index,
		// so this is a rewrite of that index and nothing more — the same as
		// POST /api/files/{id}/move is for one file.
		"POST /api/folders/move": s.handleFolderMove,

		// Film details for the folders that ask for them. The only part of SAND
		// that talks to anything but the user's own accounts, which is why the
		// switch is per folder and why the sweep is a request of its own rather
		// than something turning the switch on sets going. See movies.go.
		"GET /api/movies":                      s.handleMovieSettings,
		"POST /api/movies/key":                 s.handleMovieKey,
		"POST /api/movies/lookup":              s.handleMovieLookup,
		"POST /api/movies/scan":                s.handleMovieScan,
		"GET /api/files/{id}/movie":            s.handleMovieGet,
		"POST /api/files/{id}/movie":           s.handleMovieMatch,
		"DELETE /api/files/{id}/movie":         s.handleMovieForget,
		"GET /api/files/{id}/movie/candidates": s.handleMovieCandidates,

		// Moving a file or a folder onto other clouds. One endpoint for both,
		// because it is one operation over a set of files and the only
		// difference is how the set was named.
		"POST /api/relocate": s.handleRelocate,

		// The standing instructions a folder has been given: check what is
		// under it on a schedule, and put back what is missing. Reading and
		// writing one is index work; running one contacts every account and can
		// rebuild files, which is why it is a route of its own with a deadline
		// of its own. See handlers_automation.go.
		"GET /api/automation":      s.handleAutomationList,
		"POST /api/automation":     s.handleAutomationSet,
		"DELETE /api/automation":   s.handleAutomationRemove,
		"POST /api/automation/run": s.handleAutomationRun,

		// The repositories a vault is keeping a copy of, each stored as one
		// git bundle. Listing is index work; tracking and refreshing borrow the
		// machine's git and talk to somebody else's server, so they have
		// deadlines of their own. See handlers_git.go.
		"GET /api/git":               s.handleGitList,
		"POST /api/git/track":        s.handleGitTrack,
		"POST /api/git/{id}/refresh": s.handleGitRefresh,
		"DELETE /api/git/{id}":       s.handleGitUntrack,

		// A key pair for the two things here that sign in over SSH: a
		// connected account on a machine you have a login on, and a machine
		// files are imported from. It answers with the public half, which is a
		// line to install on the server, and keeps the private half here — see
		// handlers_sshkeys.go for why that direction is the whole point of it.
		"POST /api/ssh/keypair": s.handleGenerateSSHKey,

		// The machines a vault imports files *from*, which is the opposite
		// direction from a connected account and is deliberately not one: an
		// account holds opaque shards under keys SAND generates, a source holds
		// the user's own files under paths the user browses, and it is never
		// written to. Listing is index work; the other four talk to somebody
		// else's machine and carry deadlines of their own. See
		// handlers_remote.go and internal/vault/source.go.
		"GET /api/remote":              s.handleRemoteList,
		"POST /api/remote":             s.handleRemoteAdd,
		"PATCH /api/remote/{id}":       s.handleRemoteUpdate,
		"DELETE /api/remote/{id}":      s.handleRemoteRemove,
		"GET /api/remote/{id}/files":   s.handleRemoteBrowse,
		"POST /api/remote/{id}/import": s.handleRemoteImport,
		// What that import is doing while it does it. A GET beside the POST
		// rather than a route of its own, because it is the same noun: one
		// asks a machine for files, the other asks how far that has got. It
		// reads memory only and answers immediately, which is what lets a
		// dialog poll it every second without slowing the transfer down.
		//
		// The DELETE is what a detached import needs and a foreground one
		// never did: an import with no request behind it has to be stopped on
		// purpose, and its result dismissed on purpose too.
		"GET /api/remote/{id}/import":          s.handleRemoteImportProgress,
		"DELETE /api/remote/{id}/import/{run}": s.handleRemoteImportStop,
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
	// Before anything can allocate: tell the collector what it has to work
	// with, so an unexpectedly large read becomes a slow process rather than a
	// machine that stops answering.
	applyMemoryLimit()

	handler, err := s.Handler()
	if err != nil {
		return err
	}

	go s.autoLockLoop()
	// The folder policies. Nothing happens on a tick with nothing due, and
	// nothing happens at all while the vault is locked — see automationLoop.
	go s.automationLoop()

	addr := net.JoinHostPort(s.Bind, fmt.Sprint(s.Port))
	v, _ := s.Vault()
	log.Printf("SAND Vault %s starting on http://%s (vault: %s)", version.String(), addr, v.Path())
	if warnsOnBind(s.Bind) {
		log.Printf("WARNING: bound to %s — the vault password and decrypted files "+
			"will cross the network in the clear unless you put TLS in front of it", s.Bind)
	}
	if s.WebDAV {
		log.Printf("WebDAV share at http://%s%s/ — mount it with any username and the vault password",
			addr, s.webdavPrefix())
		if warnsOnBind(s.Bind) {
			log.Print("WARNING: WebDAV sends the vault password on every single request, " +
				"not once at sign-in, so serving it without TLS exposes the password far " +
				"more than the browser does — see scripts/nginx-sand.conf, or Tailscale Serve")
		}
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
		if s.sessions.sweep() > 0 || s.externalActive() || s.imports.running() > 0 {
			continue
		}
		v.Lock()
		// Every stream link was minted against the keys that just left memory.
		s.streams.clear()
		log.Print("vault auto-locked after idle timeout")
	}
}

// webdavPrefix is the path the share is mounted under, normalized to have a
// leading slash and no trailing one.
func (s *Server) webdavPrefix() string {
	prefix := s.WebDAVPrefix
	if prefix == "" {
		prefix = DefaultWebDAVPrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return strings.TrimSuffix(prefix, "/")
}

// noteExternalActivity records that something without a browser session — the
// share, or a stream link — read the vault just now.
func (s *Server) noteExternalActivity() {
	s.externalMu.Lock()
	s.externalActivity = time.Now()
	s.externalMu.Unlock()
}

// externalActive reports whether that happened inside the idle timeout, which
// counts as use in exactly the way a live browser session does.
func (s *Server) externalActive() bool {
	idle := s.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}

	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	return !s.externalActivity.IsZero() && time.Since(s.externalActivity) < idle
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
	case errors.Is(err, vault.ErrSubVaultLocked):
		// Distinct from LOCKED so the browser knows to ask for one more
		// password rather than throwing the session away and going back to the
		// lock screen — the vault itself is open.
		writeError(w, http.StatusUnauthorized, err.Error(), "SUB_VAULT_LOCKED")
	case errors.Is(err, vault.ErrNoSubVault):
		writeError(w, http.StatusNotFound, err.Error(), "NOT_FOUND")
	case errors.Is(err, vault.ErrNoAutomation):
		writeError(w, http.StatusNotFound, err.Error(), "NO_AUTOMATION")
	case errors.Is(err, vault.ErrAutomationBusy):
		// 409 rather than 429: nothing is rate limiting this, there is simply
		// one sweep at a time and one is already going.
		writeError(w, http.StatusConflict, err.Error(), "AUTOMATION_BUSY")
	case errors.Is(err, vault.ErrNeedsConversion):
		// 409 rather than 400: nothing about the request is wrong, the file is
		// simply in a state that has to change before it can be answered for.
		// The browser reads this code and offers to convert.
		writeError(w, http.StatusConflict, err.Error(), "NEEDS_CONVERSION")
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
