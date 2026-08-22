package provider

// Proton Drive, reached through Proton's own command-line client.
//
// This is the second of two Proton backends and the reason the service has
// two. The one in syncfolder.go writes parts into the folder the desktop app
// keeps in step, which is the right answer on a laptop and no answer at all on
// a headless box: nothing there runs the desktop app, and a part written into
// a folder nothing syncs never leaves the machine. Worse, the folder backend
// cannot tell the difference — its Put returns the moment the file is on local
// disk, so an account whose client is signed out looks healthy while it quietly
// holds nothing. This backend puts a part on Proton's servers and only then
// says it did.
//
// It does that by driving `proton-drive`, the CLI Proton builds on its own
// Drive SDK (github.com/ProtonDriveApps/sdk). Shelling out rather than speaking
// the API is a deliberate choice and not a temporary one:
//
//   - Proton publishes no Go SDK. The native ones are TypeScript and C#; the
//     Kotlin and Swift bindings wrap the C# one. None of that links into a Go
//     binary.
//   - The SDK explicitly excludes authentication, session management and the
//     address provider, so speaking the API would mean carrying an
//     implementation of Proton's login as well as its API.
//   - Proton's cryptographic model changes at the end of 2026, and every client
//     that implements only the old one stops interoperating. A hand-written Go
//     implementation would have to be rewritten for that. The CLI is Proton's
//     own, so the migration lands on the binary rather than on this file.
//
// The cost is a process per operation, parts staged through temp files, and an
// account that only works where somebody has installed the client — which is
// why the connect dialog still offers the synced folder, and why a missing
// binary is a Ping failure that names the fix rather than a backend that
// silently disappears.
//
// The parts are encrypted long before any of this, so the CLI is in the same
// position every other account is in: holding one fragment, able to do nothing
// with it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func init() {
	Register(Spec{
		Kind:  KindProtonCLI,
		Label: "Proton Drive (app)",
		Description: "A Proton Drive account, reached through Proton's own command-line client. " +
			"Unlike the synced-folder route this confirms each part has reached Proton rather " +
			"than only the folder, and it works on a machine with no desktop app — but it needs " +
			"the `proton-drive` binary installed. The Linux quick-start installer builds it for you.",
		DocsURL: "https://github.com/ProtonDriveApps/sdk/tree/main/cli",
		SignInLink: &SignInLinkSpec{
			SignInLabel: "Sign in to Proton",
			StartPath:   "/api/providers/proton/signin",
			Note: "Proton's client prints a link and waits for you to follow it. " +
				"You can open it on any device — a phone, another computer — which is " +
				"how a machine with no browser of its own connects an account.",
		},
		// Immediately above the synced-folder Proton entry, so somebody
		// scanning for "Proton" meets the two together and can tell them apart.
		Order: 30,
		Fields: []FieldSpec{
			{
				Key:      "folder",
				Label:    "Folder",
				Default:  protonCLIDefaultFolder,
				Required: true,
				Help: "Where in Proton Drive the parts go. Created if it does not exist. " +
					"Your own files live under /my-files.",
			},
			{
				Key:      "binary",
				Label:    "proton-drive binary",
				Default:  protonCLIDefaultBinary,
				Advanced: true,
				Help: "Found on PATH by name, or give the full path to it. " +
					"Build it from github.com/ProtonDriveApps/sdk, or let scripts/quickstart.sh do it.",
			},
			{
				Key:       "state_dir",
				Label:     "Client state directory",
				Advanced:  true,
				Directory: true,
				Help: "Where this account's Proton client keeps its cache and its signed-in " +
					"session while a command runs. One per account, so two Proton accounts do " +
					"not overwrite each other. Left blank, SAND picks one.",
			},
			{
				Key:      "session",
				Label:    "Signed-in session",
				Secret:   true,
				Advanced: true,
				Help: "Filled in by signing in; there is nothing useful to type here. " +
					"It is kept in the vault rather than on disk — see the notes in the docs.",
			},
		},
	}, newProtonCLIProvider)
}

const (
	// protonCLIDefaultBinary is looked up on PATH rather than pinned to a
	// path, since where the client lands differs between a quick-start install,
	// a distribution package and a hand build.
	protonCLIDefaultBinary = "proton-drive"

	// protonCLIDefaultFolder sits under /my-files because that is what the
	// client calls the account's own files — not "My Files", not the account
	// root, both of which it rejects.
	protonCLIDefaultFolder = "/my-files/sand"

	// protonCLISessionFile is what the client calls the session it has been
	// told to keep in a file. The name is not ours to choose: it is where the
	// client looks.
	protonCLISessionFile = "auth-session.json"
)

// newProtonCLIProvider builds an account backed by the Proton client.
//
// Nothing here runs the client or checks it exists. A connect form that has
// not been signed in yet is a perfectly ordinary thing to hold — the sign-in
// is what fills the session in — and a missing binary is Ping's news to break,
// with a sentence about how to fix it, rather than a constructor error the
// dialog turns into "could not add account".
func newProtonCLIProvider(cfg Config) (Provider, error) {
	// A blank folder falls back rather than failing. The field is declared
	// required, so a connect form cannot leave it empty; what can is a sign-in,
	// which happens before anybody has chosen a folder and has no business
	// caring what it will be.
	folder := protonCLINormalizeFolder(cfg.Option("folder"))
	if folder == "" {
		folder = protonCLIDefaultFolder
	}

	binary := strings.TrimSpace(cfg.Option("binary"))
	if binary == "" {
		binary = protonCLIDefaultBinary
	}

	stateDir := ExpandHome(cfg.Option("state_dir"))
	if stateDir == "" {
		stateDir = protonCLIStateDir(cfg.ID)
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("proton drive: resolving %q: %w", stateDir, err)
	}

	return &protonCLIProvider{
		base:     base{cfg: cfg},
		binary:   binary,
		folder:   folder,
		stateDir: abs,
		session:  cfg.Option("session"),
	}, nil
}

// protonCLINormalizeFolder puts a folder into the one shape the client accepts:
// absolute, no trailing slash, no doubled separators. Somebody typing
// "my-files/sand" means the same thing as "/my-files/sand/", and the client
// only understands one of them.
func protonCLINormalizeFolder(folder string) string {
	trimmed := strings.TrimSpace(folder)
	if trimmed == "" {
		return ""
	}
	cleaned := path.Clean("/" + strings.Trim(trimmed, "/"))
	if cleaned == "/" {
		return ""
	}
	return cleaned
}

// protonCLIStateDir is where an account's client state goes when nobody has
// said. It is per account, because the session is the thing that lives there
// and two Proton accounts sharing one directory would each sign the other out.
//
// SAND_PROTON_STATE_DIR is how the systemd unit points this somewhere the
// service can actually write: ProtectSystem=strict leaves the cache directory
// of a user with no home read-only, and a client that cannot write its cache
// cannot run at all.
func protonCLIStateDir(id string) string {
	root := strings.TrimSpace(os.Getenv("SAND_PROTON_STATE_DIR"))
	if root == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			root = filepath.Join(cache, "sand", "proton")
		} else {
			root = filepath.Join(os.TempDir(), "sand-proton")
		}
	}
	if id == "" {
		// A config that has not been saved yet still has to run the client, so
		// that a sign-in can happen before the account exists.
		id = "pending"
	}
	return filepath.Join(root, id)
}

// protonCLIProvider stores each part as a file in one Proton Drive folder.
type protonCLIProvider struct {
	base

	binary   string
	folder   string
	stateDir string

	// mu serializes invocations of the client for this account.
	//
	// The client keeps its entity and crypto caches in SQLite files in the
	// state directory, and rewrites the session there whenever Proton rotates
	// it. Two copies running against the same directory race on both. SAND
	// happily puts parts to several accounts at once, which is where the
	// parallelism that matters comes from; within one account the parts go up
	// one at a time.
	mu sync.Mutex

	// session is the signed-in session as SAND last saw it, held here rather
	// than left on disk — see stage.
	session string

	// sink is where a rotated session goes to be written back to the vault.
	sink func(map[string]string)
}

// OnCredentialChange registers a sink for a rotated session. It satisfies
// CredentialRotator.
//
// Proton rotates the session as it is used, exactly as Box and Microsoft
// rotate a refresh token, and with the same consequence: a rotation that is
// not written back to the vault leaves the account signed in until the token
// it still holds expires, and then signed out for good.
func (p *protonCLIProvider) OnCredentialChange(fn func(map[string]string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = fn
}

// --- running the client ----------------------------------------------------

// stage writes the session where the client expects to find it, immediately
// before running it, and harvest takes it away again afterwards.
//
// This is the part worth explaining. The client stores its session in the OS
// secret store by default, which is right on a desktop and unavailable to a
// system service with no session bus, no keyring and no home directory. Its
// other options are `pass`, which needs a GPG key that would have to sit
// unlocked next to the thing it protects, and a plaintext file, which Proton
// labels for testing only — fairly, because the session holds the key password
// that unlocks the account's key material, not merely an access token.
//
// So SAND keeps it where it keeps every other cloud credential: in the vault,
// encrypted under the vault password. The plaintext file is used as the
// handover between the two, for as long as one command takes to run, in a
// directory only the service user can read. It is not a place the session
// lives; it is the pipe it goes down.
func (p *protonCLIProvider) stage() error {
	if strings.TrimSpace(p.session) == "" {
		return fmt.Errorf("this Proton account is not signed in yet — sign in from the accounts " +
			"panel, or run `sand remote proton login`")
	}
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", p.stateDir, err)
	}
	// 0600 rather than the default, and written whole rather than appended to,
	// so a session is never readable by anyone else even for an instant.
	if err := os.WriteFile(p.sessionPath(), []byte(p.session), 0o600); err != nil {
		return fmt.Errorf("staging the Proton session: %w", err)
	}
	return nil
}

// harvest reads the session back and removes it, publishing it to the vault if
// the client rotated it. A failure to read it back is not an error worth
// failing the operation over — the operation already happened — but a rotation
// that is then lost would sign the account out later, so it is reported by the
// only means available here: leaving the stored session alone.
func (p *protonCLIProvider) harvest() {
	defer os.Remove(p.sessionPath())

	data, err := os.ReadFile(p.sessionPath())
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return
	}
	rotated := string(data)
	if rotated == p.session {
		return
	}
	p.session = rotated
	if p.sink != nil {
		p.sink(map[string]string{"session": rotated})
	}
}

func (p *protonCLIProvider) sessionPath() string {
	return filepath.Join(p.stateDir, protonCLISessionFile)
}

// scratchDir is where a part waits while it is handed to or taken from the
// client, which only moves files on disk and never bytes on a pipe.
//
// It is under the state directory rather than in the system temp directory,
// because the service runs with PrivateTmp and its /tmp is therefore memory:
// a chunk is sixteen megabytes, several accounts write at once, and a unit
// with a memory ceiling would be spending it on files it is only passing on.
// The state directory is on the disk the vault is on, which the unit already
// grants.
func (p *protonCLIProvider) scratchDir(what string) (string, error) {
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", p.stateDir, err)
	}
	dir, err := os.MkdirTemp(p.stateDir, "scratch-"+what+"-")
	if err != nil {
		return "", fmt.Errorf("staging under %s: %w", p.stateDir, err)
	}
	return dir, nil
}

// run invokes the client with the session staged around it, and returns what it
// wrote to stdout.
func (p *protonCLIProvider) run(ctx context.Context, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runLocked(ctx, args...)
}

// runLocked is run for callers that already hold the lock, which is every
// operation built out of more than one command — a Put that has to create the
// folder first must not let another goroutine sign the account out between the
// two.
func (p *protonCLIProvider) runLocked(ctx context.Context, args ...string) (string, error) {
	bin, err := p.resolveBinary()
	if err != nil {
		return "", err
	}
	if err := p.stage(); err != nil {
		return "", err
	}
	defer p.harvest()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = p.environ()

	// The client asks about a name that already exists unless it is told what
	// to do, and there is nobody here to ask. Every call site passes a conflict
	// strategy; closing stdin makes a missed one fail rather than hang.
	cmd.Stdin = nil

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), fmt.Errorf("proton-drive %s: %w", args[0], ctxErr)
		}
		return stdout.String(), protonCLIError(args, stderr.String(), err)
	}
	return stdout.String(), nil
}

// ProtonCLIClientPath reports where the client this account is configured for
// actually is, or why there is none.
//
// It exists so that a sign-in can fail before it starts on a machine with no
// client installed: the alternative is a flow that is begun, polled, and
// answers a moment later with the same news.
func ProtonCLIClientPath(cfg Config) (string, error) {
	p, err := newProtonCLIProvider(cfg)
	if err != nil {
		return "", err
	}
	client, ok := p.(*protonCLIProvider)
	if !ok {
		return "", fmt.Errorf("proton drive: unexpected backend %T", p)
	}
	return client.resolveBinary()
}

// resolveBinary finds the client, and says something useful when it is not
// there. This is the message somebody sees on a machine where SAND was
// installed without it, so it names the fix rather than the failure.
func (p *protonCLIProvider) resolveBinary() (string, error) {
	if strings.ContainsAny(p.binary, `/\`) {
		expanded := ExpandHome(p.binary)
		if info, err := os.Stat(expanded); err != nil || info.IsDir() {
			return "", fmt.Errorf("no Proton Drive client at %s — install it (the Linux "+
				"quick-start installer builds one), or connect this account as a synced "+
				"folder instead", expanded)
		}
		return expanded, nil
	}
	found, err := exec.LookPath(p.binary)
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH — install Proton's Drive client from "+
			"github.com/ProtonDriveApps/sdk (or re-run scripts/quickstart.sh, which builds "+
			"it), or connect this account as a synced folder instead", p.binary)
	}
	return found, nil
}

// environ is the environment the client runs in.
//
// It is built rather than inherited. The client reads a handful of variables
// that decide where it puts its cache and where it looks for its session, and
// inheriting a stray one from whoever started SAND would point an account at
// another account's state. HOME goes to the state directory for the same
// reason: the service user has no home, and a client that falls back to one
// would write outside everything the unit grants it.
func (p *protonCLIProvider) environ() []string {
	env := []string{
		"HOME=" + p.stateDir,
		"PROTON_DRIVE_CACHE_DIR=" + p.stateDir,
		"PROTON_DRIVE_CREDENTIALS_STORE=unsafe_file",

		// The client's own logs are not SAND's to keep. DEBUG is its default
		// and writes the lot to a file in the state directory.
		"PROTON_DRIVE_LOG_LEVEL=ERROR",
	}
	// PATH so the client can find whatever it shells out to, and the proxy and
	// CA variables because without them it cannot reach Proton at all on a host
	// behind a corporate proxy — the same set the build passes through in
	// scripts/quickstart.sh.
	for _, key := range []string{
		"PATH", "TMPDIR", "LANG", "LC_ALL",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// protonCLIError turns a failed invocation into something a person can read.
//
// The client traces the whole error object to stderr on a fatal, which is
// useful in a terminal and unreadable in an account's status line, so this
// keeps the first line that says anything and drops the trace.
func protonCLIError(args []string, stderrText string, runErr error) error {
	msg := protonCLIFirstMeaningfulLine(stderrText)
	if msg == "" {
		msg = runErr.Error()
	}
	if protonCLIIsAuthFailure(stderrText) {
		return fmt.Errorf("this Proton account is signed out — sign in again from the "+
			"accounts panel (%s)", msg)
	}
	return fmt.Errorf("proton-drive %s: %s", strings.Join(args[:min(2, len(args))], " "), msg)
}

// protonCLIFirstMeaningfulLine picks the line worth showing out of the client's
// stderr, skipping the rule it prints above a trace and the trace's own frames.
func protonCLIFirstMeaningfulLine(stderrText string) string {
	for _, line := range strings.Split(stderrText, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "="):
		case strings.HasPrefix(trimmed, "at "):
		default:
			return strings.TrimPrefix(trimmed, "Trace: ")
		}
	}
	return ""
}

// protonCLIIsNotFound reports whether a failure was the client saying the path
// is not there, which every object store has to tell apart from a real error.
func protonCLIIsNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "does not exist")
}

// protonCLIIsAuthFailure reports whether the client refused for want of a
// session, which is a different thing from a network failure and gets a
// different sentence.
func protonCLIIsAuthFailure(stderrText string) bool {
	text := strings.ToLower(stderrText)
	return strings.Contains(text, "you need to login first") ||
		strings.Contains(text, "authrequirederror")
}

// --- paths -----------------------------------------------------------------

// remotePath maps a part's key onto a path in Proton Drive.
//
// Keys are flat filenames — the vault names a part and never nests one — so a
// key with a separator in it is not a key this backend has ever been handed.
// Refusing it keeps a malformed one from reaching into another folder.
func (p *protonCLIProvider) remotePath(key string) (string, error) {
	clean := strings.TrimSpace(key)
	if clean == "" || strings.ContainsAny(clean, `/\`) || clean == "." || clean == ".." {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return p.folder + "/" + clean, nil
}

// --- the object store surface ----------------------------------------------

func (p *protonCLIProvider) Put(ctx context.Context, key string, data []byte) error {
	remote, err := p.remotePath(key)
	if err != nil {
		return err
	}

	// The client uploads a file from disk and names it after the file, so the
	// part has to become a file called exactly the key first. It goes in a
	// directory of its own so that two concurrent Puts of the same key — which
	// the lock below makes impossible for one account, but not across a
	// retry — cannot see each other's half-written staging file.
	dir, err := p.scratchDir("put")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	staged := filepath.Join(dir, key)
	if err := os.WriteFile(staged, data, 0o600); err != nil {
		return fmt.Errorf("staging the part: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureFolderLocked(ctx); err != nil {
		return err
	}

	// create-new-revision is what "overwriting any existing object" means to
	// Proton: the name keeps its history rather than the old part being
	// trashed, which is both what the interface promises and the only strategy
	// here that never leaves a part in the trash still spending quota.
	//
	// --skip-thumbnails because a part is encrypted bytes. Asked to think about
	// it the client would try to decode an image out of one, find none, and
	// have spent the time for nothing.
	_, err = p.runLocked(ctx, "filesystem", "upload",
		"--file-conflict-strategy", "create-new-revision",
		"--folder-conflict-strategy", "merge",
		"--skip-thumbnails",
		"--json",
		staged, p.folder)
	if err != nil {
		return fmt.Errorf("uploading %s: %w", remote, err)
	}
	return nil
}

func (p *protonCLIProvider) Get(ctx context.Context, key string) ([]byte, error) {
	remote, err := p.remotePath(key)
	if err != nil {
		return nil, err
	}

	dir, err := p.scratchDir("get")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// The directory is empty and freshly made, so no conflict strategy can come
	// into play; one is passed anyway, because the alternative to a strategy is
	// the client stopping to ask a question nobody is there to answer.
	if _, err := p.run(ctx, "filesystem", "download",
		"--file-conflict-strategy", "skip",
		"--folder-conflict-strategy", "skip",
		"--json",
		remote, dir); err != nil {
		if protonCLIIsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("downloading %s: %w", remote, err)
	}

	data, err := os.ReadFile(filepath.Join(dir, key))
	if errors.Is(err, os.ErrNotExist) {
		// The client reports a skipped or missing transfer by not writing the
		// file rather than by failing, so an absent file here is the same news
		// as a not-found error and has to be turned into the same one.
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading the downloaded part: %w", err)
	}
	return data, nil
}

func (p *protonCLIProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	remote, err := p.remotePath(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	out, err := p.run(ctx, "filesystem", "info", "--json", remote)
	if err != nil {
		if protonCLIIsNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("checking %s: %w", remote, err)
	}

	var node protonCLINode
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &node); err != nil {
		return ObjectInfo{}, fmt.Errorf("checking %s: %s said something unexpected: %w",
			remote, p.binary, err)
	}
	return ObjectInfo{Key: key, Size: node.size()}, nil
}

func (p *protonCLIProvider) Delete(ctx context.Context, key string) error {
	remote, err := p.remotePath(key)
	if err != nil {
		return err
	}

	// `delete` rather than `trash`: a part SAND has finished with should stop
	// costing the account quota, and a trashed one goes on costing it until
	// somebody empties the trash by hand.
	if _, err := p.run(ctx, "filesystem", "delete", "--json", remote); err != nil {
		if protonCLIIsNotFound(err) {
			// Deleting a missing object is not an error — the interface says
			// the operation is idempotent.
			return nil
		}
		return fmt.Errorf("deleting %s: %w", remote, err)
	}
	return nil
}

func (p *protonCLIProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	nodes, err := p.listFolder(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]ObjectInfo, 0, len(nodes))
	for _, node := range nodes {
		name := node.name()
		if name == "" || node.Type != "file" {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, ObjectInfo{Key: name, Size: node.size()})
	}
	return out, nil
}

// listFolder reads the folder's children, treating a folder that is not there
// as an empty one: an account connected but not yet written to has no folder,
// and "nothing stored here" is the true answer rather than an error.
func (p *protonCLIProvider) listFolder(ctx context.Context) ([]protonCLINode, error) {
	out, err := p.run(ctx, "filesystem", "list", "--json", p.folder)
	if err != nil {
		if protonCLIIsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s: %w", p.folder, err)
	}

	var nodes []protonCLINode
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &nodes); err != nil {
		return nil, fmt.Errorf("listing %s: %s said something unexpected: %w",
			p.folder, p.binary, err)
	}
	return nodes, nil
}

// Ping verifies the client is installed, the account is signed in, and the
// folder is there — creating it if it is not, which is how an account connects
// for the first time.
func (p *protonCLIProvider) Ping(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureFolderLocked(ctx)
}

// ensureFolderLocked makes sure the parts have somewhere to go.
//
// Every parent in the path is created in turn, so a folder two levels below
// /my-files does not have to be made by hand before an account will connect.
func (p *protonCLIProvider) ensureFolderLocked(ctx context.Context) error {
	if _, err := p.runLocked(ctx, "filesystem", "info", "--json", p.folder); err == nil {
		return nil
	} else if !protonCLIIsNotFound(err) {
		return err
	}

	// Walk down from the account root creating what is missing. The first
	// segment is /my-files or another section the client provides and can
	// never be created, so it is only ever descended into.
	segments := strings.Split(strings.Trim(p.folder, "/"), "/")
	built := "/" + segments[0]
	for _, segment := range segments[1:] {
		next := built + "/" + segment
		if _, err := p.runLocked(ctx, "filesystem", "info", "--json", next); err == nil {
			built = next
			continue
		} else if !protonCLIIsNotFound(err) {
			return err
		}
		if _, err := p.runLocked(ctx, "filesystem", "create-folder", "--json", built, segment); err != nil {
			return fmt.Errorf("creating %s in Proton Drive: %w", next, err)
		}
		built = next
	}
	return nil
}

// MeasureUsage counts what this account holds, and is deliberately not Usage.
//
// The client has no quota command — there is no "how full is this account"
// to ask — so the only honest figure is the sum of a listing, exactly as it is
// for a bucket. That makes this a UsageMeasurer: the figure is taken when
// somebody opens the panel that shows it, never on the sidebar's ping.
//
// The total is left at zero rather than guessed. Somebody who wants a bar
// drawn against their plan's size types it into the account's capacity, and it
// is then labelled as their figure rather than Proton's.
func (p *protonCLIProvider) MeasureUsage(ctx context.Context) (Usage, error) {
	nodes, err := p.listFolder(ctx)
	if err != nil {
		return Usage{}, err
	}
	var used int64
	for _, node := range nodes {
		if node.Type == "file" {
			used += node.size()
		}
	}
	return Usage{Used: used, Measured: true, MeasuredAt: time.Now().UTC()}, nil
}

// Account names the signed-in address, so a freshly connected account can call
// itself something better than "Proton Drive 2".
func (p *protonCLIProvider) Account(ctx context.Context) (string, error) {
	// The section the account's own files live in is owned by the account, and
	// is there before anything has been stored — which the parts folder is not.
	out, err := p.run(ctx, "filesystem", "info", "--json", protonCLIRootSection(p.folder))
	if err != nil {
		return "", err
	}
	var node protonCLINode
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &node); err != nil {
		return "", fmt.Errorf("%s said something unexpected: %w", p.binary, err)
	}
	if node.OwnedBy.Email == "" {
		return "", fmt.Errorf("the Proton client did not say which account this is")
	}
	return node.OwnedBy.Email, nil
}

// protonCLIRootSection is the section a folder is in — /my-files for a folder
// under it — which is the shallowest path that exists before SAND has stored
// anything.
func protonCLIRootSection(folder string) string {
	segments := strings.Split(strings.Trim(folder, "/"), "/")
	return "/" + segments[0]
}

// --- what the client says --------------------------------------------------

// protonCLINode is the part of the client's JSON this backend reads.
//
// It prints the SDK's node straight out, which carries far more than an object
// store needs — sharing, authorship, revisions, event scopes. Naming only the
// four fields that matter means a node growing a fifth cannot break parsing.
type protonCLINode struct {
	// Name is a result rather than a string: a node whose name will not
	// decrypt still lists, carrying the error in place of the name. Such a node
	// is not a part SAND wrote, so it is skipped rather than guessed at.
	Name struct {
		OK    bool   `json:"ok"`
		Value string `json:"value"`
	} `json:"name"`

	Type string `json:"type"`

	// ActiveRevision.ClaimedSize is the size of the part as it was uploaded.
	// TotalStorageSize is the encrypted size of every revision, which is the
	// account's problem and not the vault's — a shard health check comparing it
	// against the part it wrote would find a mismatch every time.
	ActiveRevision struct {
		ClaimedSize int64 `json:"claimedSize"`
	} `json:"activeRevision"`

	OwnedBy struct {
		Email string `json:"email"`
	} `json:"ownedBy"`
}

func (n protonCLINode) name() string {
	if !n.Name.OK {
		return ""
	}
	return n.Name.Value
}

func (n protonCLINode) size() int64 { return n.ActiveRevision.ClaimedSize }
