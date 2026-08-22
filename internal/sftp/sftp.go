// Package sftp is the SSH half of SAND's dealings with a machine you have a
// shell on — a VPS, a NAS, a storage box at a hosting company.
//
// It speaks the protocol rather than borrowing the ssh on the machine, which
// is the opposite of the choice internal/git made, and the difference is worth
// saying out loud. Git is a program that already knows how to reach every
// repository its user can reach: an agent holding a passphrase, a credential
// helper, a host alias in ~/.ssh/config. Borrowing it buys all of that for
// free. There is no equivalent program to borrow here — `sftp(1)` is an
// interactive client with no machine-readable output worth parsing — so the
// credentials have to be SAND's own, held where SAND holds credentials, which
// is inside the encrypted vault.
//
// # Host keys are not optional
//
// The consequence of speaking the protocol is that this package owns host key
// verification, and there is no ~/.ssh/known_hosts to fall back on: under the
// systemd unit both installers write, SAND runs as a user with no home of its
// own and ProtectHome=yes in force, so there is no such file and there never
// will be. Whatever this package does is the whole of the check.
//
// So it does trust on first use, and stores what it learned. The first
// connection to a host records its key fingerprint; every connection after
// that requires the same one, and a host that answers with a different key is
// refused with both fingerprints in the error rather than connected to. That
// is weaker than a fingerprint checked out of band — a first connection to an
// impostor pins the impostor — and it is the strongest thing available to a
// daemon nobody is sitting in front of. What it does buy is the property that
// matters most in practice: a host that was ever reached honestly cannot
// afterwards be impersonated.
//
// What this package will not do is connect without checking at all.
// ssh.InsecureIgnoreHostKey appears in every SFTP example on the internet and
// it quietly turns "encrypted to my server" into "encrypted to whoever
// answered", which is not a weaker version of the guarantee, it is the absence
// of it. There is no option to switch it on, because a setting that can be
// switched on is a setting somebody switches on to make an error go away.
package sftp

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// DefaultPort is where sshd listens unless somebody moved it.
const DefaultPort = 22

// DefaultTimeout bounds the TCP connect and the SSH handshake. It is not a
// bound on transfers, which run under the caller's context.
const DefaultTimeout = 20 * time.Second

// Config is everything needed to reach one host.
type Config struct {
	Host string
	Port int
	User string

	// PrivateKey is an OpenSSH or PEM private key, as pasted. Passphrase
	// decrypts it, and is held only for as long as the vault is open.
	PrivateKey string
	Passphrase string

	// Password authenticates by password where there is no key. Offered
	// because a NAS web UI will often not let you install one, not because it
	// is a good idea.
	Password string

	// HostKey is the fingerprint this host is pinned to, in the "SHA256:…"
	// form ssh-keygen prints. Empty means nothing has been learned yet and
	// this connection will learn it — see Client.HostKey.
	HostKey string

	// Timeout bounds connect and handshake. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Addr is the host:port to dial.
func (c Config) Addr() string {
	port := c.Port
	if port <= 0 {
		port = DefaultPort
	}
	return net.JoinHostPort(strings.TrimSpace(c.Host), strconv.Itoa(port))
}

// Client is a live SFTP session. Its methods are safe for concurrent use.
type Client struct {
	ssh  *ssh.Client
	sftp *pkgsftp.Client

	// hostKey is the fingerprint the server actually presented. On a
	// connection that learned it — Config.HostKey was empty — this is what the
	// caller must store, or the next connection learns again and the pin is
	// worth nothing.
	hostKey string

	// closed is a flag rather than the two pointers being nilled out, because
	// a closed Client is a thing other code still holds a reference to and
	// asks questions of — the pool asks whether it is still alive. Nilling
	// turns those questions into a panic; a flag turns them into a false.
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	// roots caches each configured folder's canonicalized form — see realRoot.
	rootsMu sync.Mutex
	roots   map[string]string
}

// HostKey returns the fingerprint the server presented, in "SHA256:…" form.
func (c *Client) HostKey() string { return c.hostKey }

// SFTP exposes the underlying client for the file operations callers need
// beyond what this package wraps.
func (c *Client) SFTP() *pkgsftp.Client { return c.sftp }

// Alive reports whether the session is still usable, at the cost of one cheap
// round trip.
//
// Worth paying before handing a pooled connection out: the alternative is
// discovering that the server restarted an hour ago inside a transfer, where
// it arrives as an EOF halfway through a shard rather than as a redial.
func (c *Client) Alive() bool {
	if c == nil || c.closed.Load() || c.sftp == nil {
		return false
	}
	_, err := c.sftp.Getwd()
	return err == nil
}

// Close shuts the session down. Safe to call more than once, and from more
// than one goroutine.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.sftp != nil {
			if err := c.sftp.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				c.closeErr = err
			}
		}
		if c.ssh != nil {
			if err := c.ssh.Close(); err != nil && c.closeErr == nil && !errors.Is(err, net.ErrClosed) {
				c.closeErr = err
			}
		}
	})
	return c.closeErr
}

// Dial connects, authenticates, verifies the host key and opens the SFTP
// subsystem.
//
// The context bounds getting there. It does not bound the session afterwards:
// an ssh.Client has no notion of a context, so a caller that wants to abandon
// a transfer in flight closes the Client rather than cancelling anything.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	auths, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	if len(auths) == 0 {
		return nil, errors.New("no way to sign in: give a private key or a password")
	}

	user := strings.TrimSpace(cfg.User)
	if user == "" {
		return nil, errors.New("no username given")
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("no host given")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	checker := &hostKeyChecker{expected: strings.TrimSpace(cfg.HostKey)}
	client := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: checker.check,
		Timeout:         timeout,
		// The server picks from these in its own order of preference; naming
		// them keeps a server that still offers ssh-rsa with SHA-1 from
		// choosing it when it has something better.
		HostKeyAlgorithms: []string{
			ssh.KeyAlgoED25519,
			ssh.CertAlgoED25519v01,
			ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512,
			ssh.KeyAlgoRSA,
		},
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "tcp", cfg.Addr())
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", cfg.Addr(), err)
	}

	// The handshake gets the same deadline as the dial did, because an
	// ssh.NewClientConn against a host that accepts TCP and then says nothing
	// would otherwise hang for as long as the caller's context allows.
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, cfg.Addr(), client)
	if err != nil {
		conn.Close()
		var mismatch *HostKeyMismatchError
		if errors.As(err, &mismatch) {
			return nil, mismatch
		}
		return nil, fmt.Errorf("ssh handshake with %s: %w", cfg.Addr(), err)
	}
	// Cleared, or every read on the session inherits the handshake's deadline
	// and a transfer longer than the timeout dies mid-file.
	_ = conn.SetDeadline(time.Time{})

	sshClient := ssh.NewClient(sshConn, chans, reqs)
	session, err := pkgsftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("opening the sftp subsystem on %s: %w"+subsystemHint(err), cfg.Addr(), err)
	}

	return &Client{ssh: sshClient, sftp: session, hostKey: checker.seen}, nil
}

// subsystemHint explains the one failure whose message says nothing useful. A
// server with SFTP switched off accepts the connection, accepts the session,
// and then refuses the subsystem — which reads as an unexplained EOF unless
// somebody says what it means.
func subsystemHint(err error) string {
	if err == nil {
		return ""
	}
	return " — the account signed in but the server would not start SFTP." +
		" Check that sshd has an sftp subsystem enabled and that this user is" +
		" not restricted to a shell-only or command-forced login"
}

// authMethods turns the credentials in a Config into ssh auth methods, in the
// order they should be tried.
func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	var out []ssh.AuthMethod

	if key := strings.TrimSpace(cfg.PrivateKey); key != "" {
		signer, err := parseKey(key, cfg.Passphrase)
		if err != nil {
			return nil, err
		}
		out = append(out, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		out = append(out, ssh.Password(cfg.Password))
	}
	return out, nil
}

// ErrPassphraseRequired says the key is encrypted and no passphrase was given,
// which is a thing to ask the user for rather than an error to report.
var ErrPassphraseRequired = errors.New("this private key is encrypted: a passphrase is needed to use it")

// ErrWrongPassphrase says a passphrase was given and did not open the key.
var ErrWrongPassphrase = errors.New("the passphrase does not open this private key")

// parseKey reads a private key, with or without a passphrase, and says which
// of the two ways it failed.
func parseKey(pem, passphrase string) (ssh.Signer, error) {
	if passphrase == "" {
		signer, err := ssh.ParsePrivateKey([]byte(pem))
		if err == nil {
			return signer, nil
		}
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, ErrPassphraseRequired
		}
		return nil, fmt.Errorf("this does not look like a private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(pem), []byte(passphrase))
	if err != nil {
		// Both key formats funnel a bad passphrase to this one sentinel, and
		// it is the only parse failure that is the user's typo rather than a
		// bad key, so it is the only one worth its own sentence.
		if errors.Is(err, x509.IncorrectPasswordError) {
			return nil, ErrWrongPassphrase
		}
		return nil, fmt.Errorf("this does not look like a private key: %w", err)
	}
	return signer, nil
}

// CleanPath normalizes a remote path the way the SFTP protocol wants it:
// forward slashes, no trailing slash, no empty segments. Remote paths are
// always slash-separated regardless of what the server runs on, so
// path.Clean is right here and filepath.Clean would be wrong on Windows.
func CleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// Under joins a relative path onto a root and refuses anything that climbs out
// of it — or that tries to.
//
// It refuses rather than clamps. Anchoring the path to the root first, which
// is the usual trick, turns "../../etc/passwd" into a perfectly good path
// inside the root and hands it back without a word; the caller asked for one
// file and quietly gets another. That is fine for a backend whose keys SAND
// generates itself and wrong for a path a person typed, and this is the one
// function both go through.
//
// The check is made twice over. First any ".." segment at all is refused,
// before anything is cleaned — cleaning first would make the refusal
// impossible to state, because path.Clean collapses "/../../etc/passwd" to
// "/etc/passwd" and the hops above the root are gone before they can be
// objected to. Nothing SAND generates needs a parent hop and nothing picked
// out of a file browser does either, so nothing legitimate is lost.
//
// Then the joined result is checked against the root, which is not a
// duplicate of the first check: it is what still holds if somebody later
// relaxes the segment scan, and it is what catches a sibling — /srv/sandbox
// is not under /srv/sand, however much it looks like it.
func Under(root, rel string) (string, error) {
	root = CleanPath(root)
	if root == "" {
		root = "/"
	}

	rel = strings.ReplaceAll(strings.TrimSpace(rel), `\`, "/")
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".." {
			return "", fmt.Errorf("path %q climbs above %s", rel, root)
		}
	}

	joined := path.Join(root, CleanPath(rel))
	if joined != root && !strings.HasPrefix(joined, strings.TrimSuffix(root, "/")+"/") {
		return "", fmt.Errorf("path %q is outside %s", rel, root)
	}
	return joined, nil
}

// Pool keeps one connection to a host and hands it out, redialling when the
// one it has has died.
//
// A scatter writes every shard of a chunk at once, so a provider that dialled
// per operation would open a connection per shard and pay an SSH handshake —
// two round trips and a key exchange — for a 5 MiB write. One session
// multiplexes them all: pkgsftp.Client is safe for concurrent use and keeps
// several requests in flight.
type Pool struct {
	cfg Config

	// learned is called when a connection discovers a host key that was not
	// pinned, so the caller can write it back. Optional.
	learned func(fingerprint string)

	mu     sync.Mutex
	client *Client
}

// NewPool returns a pool for one host. Nothing is dialled until Get.
func NewPool(cfg Config) *Pool { return &Pool{cfg: cfg} }

// OnHostKeyLearned registers a sink for a fingerprint discovered on first use.
// It is called with the pool's lock held, so it must return promptly.
func (p *Pool) OnHostKeyLearned(fn func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.learned = fn
}

// Get returns a live client, dialling if there is not one already.
func (p *Pool) Get(ctx context.Context) (*Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil {
		if p.client.Alive() {
			return p.client, nil
		}
		p.client.Close()
		p.client = nil
	}

	client, err := Dial(ctx, p.cfg)
	if err != nil {
		return nil, err
	}
	if p.cfg.HostKey == "" && client.hostKey != "" {
		p.cfg.HostKey = client.hostKey
		if p.learned != nil {
			p.learned(client.hostKey)
		}
	}
	p.client = client
	return client, nil
}

// Close drops the pooled connection.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		return nil
	}
	err := p.client.Close()
	p.client = nil
	return err
}
