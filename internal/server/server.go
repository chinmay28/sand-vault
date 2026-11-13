// Package server exposes the SAND Vault index over HTTP and serves the embedded
// file-browser frontend.
//
// The server is the only component that ever holds plaintext: it decodes an
// upload before scattering it, and rebuilds a file in memory to answer a
// download. That is why it warns on every non-loopback bind (see DefaultBind),
// and why it belongs behind TLS when it is reachable from a network.
package server

import (
	"context"
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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chinmay28/sand-vault/internal/davfs"
	"github.com/chinmay28/sand-vault/internal/tvaccess"
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

	// Profile pins the machine profile — pi5, workstation, auto — over
	// whatever machine.json beside the vault says. Empty means the file
	// decides. See tuning.go.
	Profile string

	// machine is the tuning in effect and where it came from. Set when the
	// vault is opened, so it is never nil once Handler has run.
	machine *machineState

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

	// WebDAVSubPrefix is the path the sub vaults' shares sit under, one
	// segment each: a sub vault called Taxes is mounted at
	// WebDAVSubPrefix/Taxes/, with its own password. Served whenever WebDAV
	// is. Empty uses DefaultWebDAVSubPrefix. It must not lie inside
	// WebDAVPrefix or contain it, since a folder in one share would then
	// shadow the other.
	WebDAVSubPrefix string

	// MovieBaseURL and MovieImageBaseURL point the film lookup somewhere other
	// than the real database. Empty — which is everything but a test — means
	// the addresses in internal/movie, and a test that left them empty would be
	// a test that calls a stranger's API.
	MovieBaseURL      string
	MovieImageBaseURL string

	// TVDiscoveryPort is the UDP port a TV on the network broadcasts to when
	// looking for SAND. Zero turns discovery off; it is never served on a
	// loopback bind, where no TV could reach the web server anyway. See
	// internal/tvdiscover.
	TVDiscoveryPort int

	vault    *vault.Vault
	sessions *sessionStore
	streams  *streamStore

	// tv holds the four-character codes that open one folder to a TV, and
	// the TVs paired under them. See handlers_tv.go.
	tv         *tvaccess.Store
	oauthFlows *oauthFlowStore

	// generatedKeys holds SSH key pairs SAND made for a connect form that has
	// not been submitted yet. The private half never leaves this process — see
	// handlers_sshkeys.go.
	generatedKeys *generatedKeyStore

	// relocations is where the moves between clouds running right now say how
	// far they have got — the same window transfer_watch.go opens onto imports,
	// for the same two readers: a progress bar, and a move that was detached
	// from the page that started it.
	relocations *relocateWatch

	// transfers is where the imports and exports running right now say how
	// far they have got, so the dialog that started one can draw it. It is a
	// view of the requests in flight and dies with them — see
	// transfer_watch.go.
	transfers *transferWatch

	// zips holds the tickets for folders being handed back as archives: the
	// same bearer-link shape as streams, minted for a folder and dying with
	// the keys — see handlers_zip.go.
	zips *ticketStore[zipTicket]

	// erases is the same window onto recursive folder deletes, so the dialog
	// that is waiting on one can count files down instead of sitting on
	// "Deleting…" for minutes — see erase_watch.go. The orphan sweep opens
	// the same kind of window through it, under a key of its own — see
	// handlers_orphans.go.
	erases *eraseWatch

	// strayScans is the settings panel's scan of every account, run apart
	// from the request that asked for it so the page can be closed and come
	// back to the result — see orphan_scan.go.
	strayScans *scanWatch

	// reattaches is the repair that records mislaid parts back, run the same
	// way for the same reason: it is a listing of every account first, and a
	// page closed halfway must not take the repair with it. See
	// handleReattachStart.
	reattaches *reattachWatch

	// checks is the deep check of a folder's files and repairs the repair of
	// what it flagged, each run on its own like the scans above. See
	// handlers_check.go.
	checks  *checkWatch
	repairs *repairWatch

	// lostScans is the scan for parts the index records on an account that
	// the account is not holding, and lostRestores the run that puts them
	// back. See handlers_lostparts.go.
	lostScans    *lostScanWatch
	lostRestores *lostRestoreWatch

	// checksums is the verify or compute of a folder's checksums that is
	// running, with the control that paces it. See handlers_checksums.go.
	checksums *checksumJobs

	// snapshots is the snapshot of a folder being taken or restored, one at
	// a time between the two. See handlers_snapshots.go.
	snapshots *snapshotWatch

	// ChecksumsEvery is how often checksums.gz is brought up to date with
	// the index while the vault is unlocked. Zero leaves it to be written
	// only when asked for.
	ChecksumsEvery time.Duration

	// sweeps is the orphan sweep, run the same way: a listing of every
	// account and then a delete per object, which must not stop halfway
	// because the page that asked was closed. See sweep_run.go.
	sweeps *sweepWatch

	// movieScans is the sweep of a folder for film details, run the same
	// way: a search and a poster per film, which must not stop because the
	// dialog that asked was closed. See movie_scan_run.go.
	movieScans *movieScanWatch

	// erasures is where the deletes running right now say how far they have
	// got — the relocation watch's shape, for the same two readers: a progress
	// bar, and a delete that was detached from the page that started it. See
	// erase_runs.go.
	erasures *eraseRuns

	// readWatches is the window onto files being opened right now: which
	// account each is waiting on, and how much of it has been sent. Read by
	// the browser beside the content request it is waiting for. See
	// read_watch.go.
	readWatches *readWatch

	// externalActivity is when something outside the browser last read the
	// vault — a mounted share, or a player following a stream link. Neither has
	// a browser session to keep the vault alive, so without this it would lock
	// itself halfway through a film.
	externalMu       sync.Mutex
	externalActivity time.Time

	// rekeyWake has the re-encryption loop look at its plan now rather than
	// on its next tick; see handlers_rekey.go.
	rekeyWakeOnce sync.Once
	rekeyWake     chan struct{}

	// dav is the share, when one is served. It is asked whether a request is
	// still running before the vault is locked for being idle.
	dav *davfs.Endpoint

	// subdav is the sub vaults' shares, served beside dav, and asked the same
	// question.
	subdav *davfs.SubVaultShares
}

// DefaultWebDAVPrefix is where the share is mounted unless told otherwise.
const DefaultWebDAVPrefix = "/dav"

// DefaultWebDAVSubPrefix is where the sub vaults' shares sit unless told
// otherwise: one share per sub vault, at /sub/<name>/.
const DefaultWebDAVSubPrefix = "/sub"

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
	// Before anything can upload or read: the windows, cache and chunk size
	// this machine runs. See tuning.go.
	s.loadTuning(v)
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
	if s.tv == nil {
		s.tv = tvaccess.New(tvaccess.Options{})
	}
	if s.generatedKeys == nil {
		s.generatedKeys = newGeneratedKeyStore()
	}
	if s.transfers == nil {
		s.transfers = newTransferWatch()
	}
	if s.zips == nil {
		s.zips = newTicketStore[zipTicket](zipTicketTTL)
	}
	if s.relocations == nil {
		s.relocations = newRelocateWatch()
	}
	if s.erases == nil {
		s.erases = newEraseWatch()
	}
	if s.strayScans == nil {
		s.strayScans = newScanWatch()
	}
	if s.reattaches == nil {
		s.reattaches = newReattachWatch()
	}
	if s.sweeps == nil {
		s.sweeps = newSweepWatch()
	}
	if s.movieScans == nil {
		s.movieScans = newMovieScanWatch()
	}
	if s.checks == nil {
		s.checks = newJobWatch[vault.CheckProgress, vault.CheckReport]()
	}
	if s.checksums == nil {
		s.checksums = newChecksumJobs()
	}
	if s.snapshots == nil {
		s.snapshots = newJobWatch[vault.SnapshotProgress, vault.SnapshotReport]()
	}
	if s.lostScans == nil {
		s.lostScans = newJobWatch[vault.OrphanScanProgress, vault.LostPartsScan]()
	}
	if s.lostRestores == nil {
		s.lostRestores = newJobWatch[vault.LostRestoreProgress, vault.LostRestoreReport]()
	}
	if s.repairs == nil {
		s.repairs = newJobWatch[vault.RepairProgress, vault.RepairReport]()
	}
	if s.erasures == nil {
		s.erasures = newEraseRuns()
	}
	if s.readWatches == nil {
		s.readWatches = newReadWatch()
	}

	mux := http.NewServeMux()

	// --- WebDAV: the vault as a mountable share, off unless asked for -----
	if s.WebDAV {
		prefix, subPrefix := s.webdavPrefix(), s.webdavSubPrefix()
		if webdavPrefixesCollide(prefix, subPrefix) {
			return nil, fmt.Errorf("the WebDAV share at %s and the sub vault shares at %s would shadow each other; "+
				"give them paths neither of which lies inside the other", prefix, subPrefix)
		}
		dav, err := davfs.Handler(v, davfs.Options{
			Prefix:     prefix,
			Realm:      "SAND Vault",
			OnActivity: s.noteExternalActivity,
		})
		if err != nil {
			return nil, err
		}
		s.dav = dav
		// Both forms: a client asking about the share itself sends the bare
		// prefix, and everything inside it arrives with the trailing slash.
		mux.Handle(prefix, dav)
		mux.Handle(prefix+"/", dav)

		// And one share per sub vault, each behind its own password, beside
		// the vault's rather than under it: a sub vault's name under /dav/
		// would shadow a folder of the same name in the main vault. The
		// vault's share still never shows a sub vault; this is how a sub
		// vault is mounted on its own, with the rest of the vault out of
		// reach of that mount. See davfs.SubVaultShares.
		s.subdav = davfs.SubVaultHandler(v, davfs.Options{
			Prefix:     subPrefix,
			Realm:      "SAND Vault",
			OnActivity: s.noteExternalActivity,
		})
		mux.Handle(subPrefix, s.subdav)
		mux.Handle(subPrefix+"/", s.subdav)
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

	// A folder as one zip, by the same bargain: the ticket is the credential,
	// because the archive is too big to come back down a fetch the page
	// buffers and has to be an address the browser — or a download manager
	// on another machine — saves from directly. See handlers_zip.go.
	mux.HandleFunc("GET /zip/{token}/{name}", s.handleFolderZip)

	// The TV: a code traded for a device token, and then one folder's videos
	// to that token and nothing else. Outside the session because a TV has
	// none; the token is the credential. See handlers_tv.go.
	mux.HandleFunc("POST /api/tv/pair", s.handleTVPair)
	mux.HandleFunc("GET /api/tv/session", s.requireTV(s.handleTVSession))
	mux.HandleFunc("POST /api/tv/logout", s.requireTV(s.handleTVLogout))
	mux.HandleFunc("GET /api/tv/library", s.requireTV(s.handleTVLibrary))
	mux.HandleFunc("GET /api/tv/items/{id}", s.requireTV(s.handleTVItem))
	mux.HandleFunc("GET /api/tv/items/{id}/poster", s.requireTV(s.handleTVPoster))
	mux.HandleFunc("POST /api/tv/items/{id}/play", s.requireTV(s.handleTVPlay))
	mux.HandleFunc("GET /api/tv/folders/art", s.requireTV(s.handleTVFolderArt))
	// And the screen it is all drawn on, which the Fire TV app opens — or any
	// browser on a TV. See tvui.go.
	if err := s.mountTVApp(mux); err != nil {
		return nil, err
	}

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
		// Where the re-encryption behind a password change has got to, and
		// the plan it is following — so much a day, these folders first. The
		// migrate above is "finish now"; this is the slower way, and the one
		// the server runs on its own. See handlers_rekey.go.
		"GET /api/vault/rekey":       s.handleRekeyStatus,
		"PUT /api/vault/rekey":       s.handleRekeyPlan,
		"POST /api/vault/rekey/skip": s.handleRekeySkip,
		"GET /api/vault/rekey/files": s.handleRekeyFiles,
		// The other answer to a file that will not move: not left for last,
		// left out. Answers like DELETE /api/files/{id}.
		"DELETE /api/vault/rekey/files/{id}": s.handleRekeyDelete,
		"POST /api/vault/policy":             s.handleVaultPolicy,
		"POST /api/vault/defaults":           s.handleVaultDefaults,
		// Second copies: the policy, what replicates, and where the worker
		// has got to. See handlers_replication.go.
		"GET /api/vault/replication": s.handleReplicationStatus,
		"PUT /api/vault/replication": s.handleReplicationSet,
		"POST /api/replicate":        s.handleReplicateSet,
		"POST /api/vault/backup":     s.handleVaultBackup,
		// How far apart the pushes of the index are kept, so a burst of
		// changes is a few of them rather than one per change.
		"POST /api/vault/backup/delay": s.handleVaultBackupDelay,
		// The machines that open this vault, and a pull of the index they
		// share. See handlers_instances.go.
		"GET /api/vault/instances": s.handleInstances,
		"POST /api/vault/sync":     s.handleSync,

		// How this machine is sized: the profile, the machine it is taken
		// to be, and every knob derived from that or set by hand. Not under
		// /api/vault because it is not the vault's — it is kept beside it
		// and belongs to the box. See tuning.go.
		"GET /api/machine":    s.handleMachineGet,
		"PUT /api/machine":    s.handleMachinePut,
		"DELETE /api/machine": s.handleMachineReset,

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
		"GET /api/vault/orphans":  s.handleOrphanScan,
		"POST /api/vault/orphans": s.handleOrphanSweep,
		// Where a running sweep has got to — objects erased against objects
		// doomed — read beside the POST above, which for a vault where
		// somebody has been deleting films answers only after minutes.
		"GET /api/vault/orphans/erasing":   s.handleOrphanErasing,
		"POST /api/vault/orphans/reattach": s.handleOrphanReattach,
		"GET /api/vault/orphans/strays":    s.handleStrayScan,
		// The settings panel's scan of every account, run on its own so the
		// page that asked can be closed and come back to it. POST starts one
		// or joins the one running; GET says how far it is, then what it
		// found. See orphan_scan.go.
		"POST /api/vault/orphans/scan": s.handleStrayScanStart,
		"GET /api/vault/orphans/scan":  s.handleStrayScanStatus,
		// The reattach, run the same way: POST starts one (or joins the one
		// running for the same cloud), GET says how far it is, then what it
		// recorded. POST /api/vault/orphans/reattach above stays for scripts
		// that want the answer in one request.
		"POST /api/vault/orphans/reattaching": s.handleReattachStart,
		"GET /api/vault/orphans/reattaching":  s.handleReattachStatus,
		// And the sweep: POST starts one (or joins the one running for the
		// same targets), GET says how far it is, then what it erased. POST
		// /api/vault/orphans stays for scripts and dry runs.
		"POST /api/vault/orphans/sweeping": s.handleSweepStart,
		"GET /api/vault/orphans/sweeping":  s.handleSweepStatus,
		// The scan run the other way: parts the index records on an account
		// that the account is not holding, and the run that remakes each
		// from the file's other parts and writes it back where it was. See
		// handlers_lostparts.go.
		"POST /api/vault/lost-parts/scan":         s.handleLostScanStart,
		"GET /api/vault/lost-parts/scan":          s.handleLostScanStatus,
		"POST /api/vault/lost-parts/restore":      s.handleLostRestoreStart,
		"GET /api/vault/lost-parts/restore":       s.handleLostRestoreStatus,
		"POST /api/vault/lost-parts/restore/stop": s.handleLostRestoreStop,
		// The same question asked of the one disk SAND writes to itself. An
		// interrupted upload leaves its spool in the vault's own directory at
		// the full size of the file it was sending, and nothing goes back for
		// it either. The scan rides along with the GET above; erasing them is
		// its own POST, for the same reason the sweep is.
		"POST /api/vault/orphans/leftovers": s.handleLeftoverSweep,
		// What the buckets that keep every version are storing beneath the
		// objects they show: a copy of the index backup per change ever
		// made, and every part SAND ever deleted under a marker. Looking is
		// a GET and erasing is a POST that re-scans first, and the POST's
		// progress is read beside it. See handlers_versions.go.
		"GET /api/vault/versions":         s.handleVersionScan,
		"POST /api/vault/versions":        s.handleVersionSweep,
		"GET /api/vault/versions/erasing": s.handleVersionErasing,
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
		// Whether every account is still answering, asked on a schedule so that
		// a cloud going dark is noticed by the app rather than by the person who
		// happens to open the drawer next month. The check itself pings every
		// account; the schedule is how often that happens, or never. See
		// handlers_health.go. The GET — a read of what the last check found,
		// which contacts nobody and which the app polls every minute — is
		// registered below the loop, quietly, so the polling does not count as
		// somebody using the vault.
		"POST /api/providers/health/check":    s.handleCloudHealthCheck,
		"POST /api/providers/health/schedule": s.handleCloudHealthSchedule,
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
		"GET /api/transfers":     s.handleTransferStats,
		"GET /api/throughput":    s.handleThroughput,
		// Where one file being opened has got to — which account it is
		// waiting on, whether it is decrypting, how much has been sent —
		// keyed by the token the browser put on the content request. Read-only
		// and in memory; see read_watch.go.
		"GET /api/reads/watch/{token}": s.handleReadWatch,

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
		"GET /api/subvaults/{id}/defaults":  s.handleSubVaultDefaults,
		"POST /api/subvaults/{id}/defaults": s.handleSubVaultSetDefaults,
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
		"GET /api/degraded": s.handleDegradedList,
		"POST /api/files":   s.handleFilesUpload,
		// Which files of a choice are already stored at their destination with
		// the same name and size, asked before a byte of them is sent — the
		// browser drops those from the upload rather than storing copies.
		"POST /api/files/precheck": s.handleFilesPrecheck,
		"POST /api/files/retime":   s.handleFilesRetime,
		"GET /api/files/{id}":      s.handleFileMeta,
		"DELETE /api/files/{id}":   s.handleFileDelete,
		// A batch of files in one index write, and where it has got to while
		// it runs — the folder delete's pair, for a selection rather than a
		// folder.
		"POST /api/files/delete":    s.handleFilesDelete,
		"GET /api/files/erasing":    s.handleFilesErasing,
		"DELETE /api/files/erasing": s.handleFilesErasingStop,
		// A selection — files, folders, or both — deleted as one run, which
		// the browser hands to the machine rather than waiting on: `detach`
		// answers at once with the run, and the runs list is where it is
		// followed, stopped, and read when it is over. See handlers_erase.go.
		"POST /api/delete":              s.handleErase,
		"GET /api/delete/runs":          s.handleEraseRuns,
		"DELETE /api/delete/runs/{run}": s.handleEraseStop,
		"POST /api/files/{id}/move":     s.handleFileMove,
		// And a batch of them in one index write, for a plan of thousands.
		"POST /api/files/move":       s.handleFilesMove,
		"GET /api/files/{id}/health": s.handleFileHealth,
		// The deep check: every shard of every chunk fetched and opened, the
		// file held to its hash and its contents to their format. One file,
		// answered in the request; a folder, as a job. See handlers_check.go.
		"POST /api/files/{id}/check":  s.handleFileCheck,
		"GET /api/check/survey":       s.handleCheckSurvey,
		"POST /api/check":             s.handleCheckStart,
		"GET /api/check":              s.handleCheckStatus,
		"POST /api/check/stop":        s.handleCheckStop,
		"POST /api/check/repair":      s.handleRepairStart,
		"GET /api/check/repair":       s.handleRepairStatus,
		"POST /api/check/repair/stop": s.handleRepairStop,
		// Checksums: what a verify or a compute of a folder would read, the
		// job itself with its pace, pause and stop, and the ledger written
		// now. See handlers_checksums.go.
		"GET /api/checksums/survey":  s.handleChecksumSurvey,
		"POST /api/checksums":        s.handleChecksumStart,
		"GET /api/checksums":         s.handleChecksumStatus,
		"POST /api/checksums/pause":  s.handleChecksumControl(checksumPause),
		"POST /api/checksums/resume": s.handleChecksumControl(checksumResume),
		"POST /api/checksums/rate":   s.handleChecksumControl(checksumRate),
		"POST /api/checksums/stop":   s.handleChecksumStop,
		"POST /api/checksums/ledger": s.handleChecksumLedgerWrite,
		// Snapshots of a folder: what one would hold, which are kept at a
		// place, a password tried on one, one erased — and the job that takes
		// or restores one, with its status and its stop. See
		// handlers_snapshots.go.
		"GET /api/snapshots/plan":     s.handleSnapshotPlan,
		"GET /api/snapshots/stored":   s.handleSnapshotStored,
		"POST /api/snapshots/inspect": s.handleSnapshotInspect,
		"POST /api/snapshots/delete":  s.handleSnapshotDelete,
		"POST /api/snapshots":         s.handleSnapshotTake,
		"POST /api/snapshots/restore": s.handleSnapshotRestore,
		"GET /api/snapshots":          s.handleSnapshotStatus,
		"POST /api/snapshots/stop":    s.handleSnapshotStop,
		// Converting a pre-chunking file into the chunked format, which is what
		// the read path's refusal asks for. See handlers_convert.go.
		"GET /api/conversions":           s.handleConversionsPending,
		"POST /api/files/{id}/convert":   s.handleFileConvert,
		"GET /api/files/{id}/content":    s.handleFileContent,
		"POST /api/files/{id}/stream":    s.handleFileStreamLink,
		"DELETE /api/files/{id}/replica": s.handleReplicaDrop,
		"GET /api/files/{id}/thumb":      s.handleFileThumb,
		"PUT /api/files/{id}/thumb":      s.handleFileThumbSet,

		// The whole folder tree in one answer, which is what choosing a
		// destination needs — GET /api/files walks one level at a time.
		"GET /api/folders":    s.handleFoldersList,
		"POST /api/folders":   s.handleFolderCreate,
		"DELETE /api/folders": s.handleFolderDelete,
		// Where the DELETE above has got to, for whoever is waiting on it — a
		// recursive delete of a big folder runs for minutes and can only
		// answer at the end. Read-only and in-memory; see erase_watch.go.
		"GET /api/folders/erasing": s.handleFolderErasing,
		// A DELETE on the window stops the delete it watches.
		"DELETE /api/folders/erasing": s.handleFolderErasingStop,
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
		// Where everything under a folder would go if it were filed by the
		// date it was last modified — a flat folder of ten thousand files as
		// 2026/January and the rest. Read-only like the survey: the browser
		// makes the folders and moves the files over the endpoints above, one
		// at a time.
		"GET /api/folders/date-sort": s.handleFolderDateSort,
		// What a folder is holding, in the few figures its menu shows before
		// anything is done to it: the size of everything under it, how many
		// files and folders that is, and which accounts hold the parts. The
		// survey above could be counted for the same answer, and is a list of
		// every name under the folder to get it.
		"GET /api/folders/stats": s.handleFolderStats,
		// Everything under a folder as one zip. The POST plans it from the
		// index and mints the link to save it from; the GET /zip/ route
		// above streams it, gathering each file as the archive is written,
		// so a folder far bigger than this machine's memory still leaves it.
		"POST /api/folders/zip": s.handleFolderZipLink,

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
		"GET /api/movies":         s.handleMovieSettings,
		"POST /api/movies/key":    s.handleMovieKey,
		"POST /api/movies/lookup": s.handleMovieLookup,
		"POST /api/movies/scan":   s.handleMovieScan,
		// The same sweep run apart from the request, so the dialog can be
		// closed while it goes: POST starts one (or joins the one running
		// for the same folder), GET says how far it is, then what it found,
		// and DELETE stops it with what it has. POST /api/movies/scan above
		// stays for scripts that want the answer in one request. See
		// movie_scan_run.go.
		"POST /api/movies/scanning":            s.handleMovieScanStart,
		"GET /api/movies/scanning":             s.handleMovieScanStatus,
		"DELETE /api/movies/scanning":          s.handleMovieScanStop,
		"GET /api/files/{id}/movie":            s.handleMovieGet,
		"POST /api/files/{id}/movie":           s.handleMovieMatch,
		"DELETE /api/files/{id}/movie":         s.handleMovieForget,
		"GET /api/files/{id}/movie/candidates": s.handleMovieCandidates,

		// The chat assistant: a model the user runs on their own network,
		// reading the index through three tools. Where it runs is a setting;
		// a question is one POST that holds open while the model answers.
		// See assistant.go.
		"GET /api/assistant":           s.handleAssistantSettings,
		"POST /api/assistant/settings": s.handleAssistantSet,
		"POST /api/assistant/ask":      s.handleAssistantAsk,

		// Moving a file or a folder onto other clouds. One endpoint for both,
		// because it is one operation over a set of files and the only
		// difference is how the set was named.
		"POST /api/relocate":              s.handleRelocate,
		"GET /api/relocate/runs":          s.handleRelocateRuns,
		"DELETE /api/relocate/runs/{run}": s.handleRelocateStop,

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
		// Stored afresh from the upstream, on chosen clouds, whatever state
		// the bundle is in: the way back when its parts cannot be read.
		"POST /api/git/{id}/rebuild": s.handleGitRebuild,
		"DELETE /api/git/{id}":       s.handleGitUntrack,

		// A key pair for the two things here that sign in over SSH: a
		// connected account on a machine you have a login on, and a machine
		// files are imported from. It answers with the public half, which is a
		// line to install on the server, and keeps the private half here — see
		// handlers_sshkeys.go for why that direction is the whole point of it.
		"POST /api/ssh/keypair": s.handleGenerateSSHKey,

		// The machines a vault moves the user's own files to and from, which
		// is the opposite of a connected account and is deliberately not one:
		// an account holds opaque shards under keys SAND generates, a source
		// holds the user's own files under paths the user browses, and never
		// holds a shard. Listing is index work; the rest talk to somebody
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
		// The other direction: the vault's files written out onto the
		// machine, in the clear. The same three verbs, because it is the same
		// kind of request with the bytes going the other way — one long POST,
		// a GET beside it for progress, a DELETE to stop or dismiss.
		"POST /api/remote/{id}/export":         s.handleRemoteExport,
		"GET /api/remote/{id}/export":          s.handleRemoteExportProgress,
		"DELETE /api/remote/{id}/export/{run}": s.handleRemoteExportStop,
	}
	// Minting the codes is the owner's, behind the session like the rest.
	protected["GET /api/tv/codes"] = s.handleTVCodes
	protected["POST /api/tv/codes"] = s.handleTVCodeCreate
	protected["DELETE /api/tv/codes/{code}"] = s.handleTVCodeRevoke

	for pattern, handler := range protected {
		mux.HandleFunc(pattern, s.requireSession(handler))
	}
	// Behind the session like the rest, but not counted as use of the vault
	// — see requireSessionQuietly.
	mux.HandleFunc("GET /api/providers/health", s.requireSessionQuietly(s.handleCloudHealth))

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
	// And the question the folder policies do not answer for the vault as a
	// whole: are the clouds themselves still there. A ping each, on the vault's
	// own schedule, and nothing at all while it is locked — see healthLoop.
	go s.healthLoop()
	// The old versions on the buckets that asked to have them erased on a
	// schedule, once a day, and nothing while locked — see pruneLoop.
	go s.pruneLoop()
	// And the files a password change left on the previous key, moved one at
	// a time at the pace the change asked for, and nothing while locked — see
	// rekeyLoop.
	// And checksums.gz, kept in step with the index, and nothing while
	// locked or while the index has not changed — see checksumLedgerLoop.
	go s.checksumLedgerLoop()
	go s.rekeyLoop()
	// And the second copies the replication policy asks for, made one at a
	// time at its pace, and nothing while locked — see replicationLoop.
	go s.replicationLoop()
	// And what the other machines opening this vault wrote to the index,
	// pulled on its schedule, and nothing while locked — see syncLoop.
	go s.syncLoop()
	// And the answer to a TV on the network asking where SAND is — only on a
	// bind a TV could reach. See internal/tvdiscover.
	if port := s.discoveryPort(); port > 0 {
		go s.discoveryLoop(port)
	}

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
		log.Printf("sub vault shares at http://%s%s/<name>/ — mount one with any username and that sub vault's own password",
			addr, s.webdavSubPrefix())
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
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()

	// An orderly stop — Ctrl-C, or the service manager's SIGTERM on a restart
	// or an update — finishes what the vault owes the accounts before the
	// process goes. The copies of the index out there are rewritten a few
	// seconds after a change rather than on it (vault.BackupDelay), so a
	// restart that lands inside that gap would otherwise leave them a change
	// behind until the next unlock. Nothing on disk is at risk either way:
	// the vault file is written before any request is answered.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	stop()
	log.Print("shutting down")
	closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(closing)
	if !flushBackupsWithin(v, shutdownFlushBound) {
		log.Printf("the index backup did not finish writing within %s; the next unlock brings the copies up to date",
			shutdownFlushBound)
	}
	return nil
}

// shutdownFlushBound is how long an orderly stop waits for the index to
// finish writing to the accounts. Long enough for a push to a slow provider;
// short enough that an account that is down does not hold the restart.
const shutdownFlushBound = 30 * time.Second

// flushBackupsWithin starts any push of the index that is waiting out its
// delay and waits for it, up to the bound. It reports whether the copies
// settled in time.
func flushBackupsWithin(v *vault.Vault, bound time.Duration) bool {
	done := make(chan struct{})
	go func() {
		v.AwaitBackupSync()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(bound):
		return false
	}
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
//
// A session with a request still running under it is not idle, however long
// ago it was renewed — sweep counts it as live — so an upload batch that takes
// longer than the timeout holds the lock off until it has landed.
func (s *Server) autoLockLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		if s.sessions.sweep() > 0 || s.externalActive() || s.transfers.running() > 0 || s.relocations.running() > 0 || s.erasures.running() > 0 {
			continue
		}
		// A stray-parts scan somebody started and walked away from is use
		// too: it says so as each cloud answers, but says nothing while it
		// waits on the slowest one, and that wait is the whole of it.
		if s.strayScans.running() || s.reattaches.running() || s.sweeps.running() || s.movieScans.running() ||
			s.checks.running() || s.repairs.running() || s.checksums.busy() ||
			s.lostScans.running() || s.lostRestores.running() || s.snapshots.running() {
			continue
		}
		// A replica half-made would be erased and started again on the next
		// unlock, which is the budget spent twice.
		if v.ReplicationKeepsBusy() {
			continue
		}
		v.Lock()
		// Every stream and zip link was minted against the keys that just
		// left memory, and so was what the last stray-parts scan found.
		s.streams.clear()
		s.zips.clear()
		s.strayScans.stopAll()
		s.reattaches.stopAll()
		s.sweeps.stopAll()
		s.movieScans.stopAll()
		s.checks.stopAll()
		s.repairs.stopAll()
		s.checksums.stopAll()
		s.lostScans.stopAll()
		s.lostRestores.stopAll()
		s.snapshots.stopAll()
		log.Print("vault auto-locked after idle timeout")
	}
}

// webdavPrefix is the path the share is mounted under, normalized to have a
// leading slash and no trailing one.
func (s *Server) webdavPrefix() string {
	return normalizeWebDAVPrefix(s.WebDAVPrefix, DefaultWebDAVPrefix)
}

// webdavSubPrefix is the path the sub vaults' shares sit under, normalized
// the same way.
func (s *Server) webdavSubPrefix() string {
	return normalizeWebDAVPrefix(s.WebDAVSubPrefix, DefaultWebDAVSubPrefix)
}

func normalizeWebDAVPrefix(prefix, fallback string) string {
	if prefix == "" {
		prefix = fallback
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return strings.TrimSuffix(prefix, "/")
}

// webdavPrefixesCollide reports whether two normalized mount paths are the
// same or one lies inside the other — in which case the routes for one would
// take requests meant for a folder in the other.
func webdavPrefixesCollide(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// noteExternalActivity records that something without a browser session — the
// share, or a stream link — read the vault just now.
func (s *Server) noteExternalActivity() {
	s.externalMu.Lock()
	s.externalActivity = time.Now()
	s.externalMu.Unlock()
}

// externalActive reports whether that happened inside the idle timeout, which
// counts as use in exactly the way a live browser session does — or whether
// the share is in the middle of a request right now.
//
// The second half matters for anything long. A request is noted as use once,
// when it begins, so a PUT of a film or a DELETE of a folder that outlasts the
// idle timeout would otherwise look idle while still running, and the lock
// would take the keys out from under it.
func (s *Server) externalActive() bool {
	if s.dav != nil && s.dav.InFlight() > 0 {
		return true
	}
	if s.subdav != nil && s.subdav.InFlight() > 0 {
		return true
	}

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
//
// A request that gets through counts as use of the vault for as long as it
// runs, not only for the moment it began: the session is held for the length
// of the handler and its idle deadline moves on again when the handler
// returns. An upload of a folder is one request per batch, and a batch over a
// slow link can take longer than the idle timeout on its own; held only at the
// start, the session would lapse under it, the auto-lock would take the keys
// part way through, and every batch after it would be turned away as locked.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := s.sessions.acquire(sessionToken(r))
		if !ok {
			writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
			return
		}
		defer release()
		s.serveUnlocked(w, r, next)
	}
}

// requireSessionQuietly is requireSession for a request that is not anybody
// doing anything: the browser asks for the standing of the clouds every
// minute for as long as a tab is open, and a poll that counted as use would
// mean a tab left open kept the vault unlocked forever. The session has to
// be valid, but asking leaves its idle deadline where it was.
func (s *Server) requireSessionQuietly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.peek(sessionToken(r)) {
			writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
			return
		}
		s.serveUnlocked(w, r, next)
	}
}

// serveUnlocked runs a handler whose session has been checked, provided the
// vault itself is still open.
func (s *Server) serveUnlocked(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
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
	case errors.Is(err, vault.ErrNotTheVaultsPassword):
		// 400 with its own code: the request is for a second instance and
		// the password given is not the vault's, which the kit dialog turns
		// into a hint on the password field rather than a general failure.
		writeError(w, http.StatusBadRequest, err.Error(), "NOT_THE_VAULTS_PASSWORD")
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
	case errors.Is(err, vault.ErrRekeyPending):
		// 409: the request is well-formed and the vault is open; it is the
		// vault's state — files still on the previous key — that has to
		// change first. The message carries the counts.
		writeError(w, http.StatusConflict, err.Error(), "REKEY_PENDING")
	case errors.Is(err, vault.ErrMigrationBusy):
		writeError(w, http.StatusConflict, err.Error(), "MIGRATION_BUSY")
	case errors.Is(err, vault.ErrNoReplica):
		writeError(w, http.StatusNotFound, err.Error(), "NO_REPLICA")
	case errors.Is(err, vault.ErrNoAccount):
		writeError(w, http.StatusNotFound, err.Error(), "NOT_FOUND")
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
