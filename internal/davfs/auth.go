package davfs

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"golang.org/x/net/webdav"
)

// DefaultVerifyTTL is how long a verified password is remembered.
//
// Short enough that a password changed elsewhere stops working within a minute,
// long enough that a film playing over a mounted share pays the Argon2id cost
// once rather than on every range request.
const DefaultVerifyTTL = time.Minute

// verifier answers "is this the password?" without paying for it every time.
//
// WebDAV authenticates with HTTP Basic, which is stateless: the password
// arrives on every single request, and a player streaming a film sends hundreds
// of them. Checking each one properly means a 64 MB, three-iteration Argon2id
// pass per request, which is a denial of service aimed squarely at oneself.
//
// So a verified credential is remembered, keyed by an HMAC of it under a key
// minted for this process. The HMAC is what stops the map being a list of
// passwords in memory, and being per-process means the remembered form is
// useless anywhere else.
//
// What the password guards is behind two functions rather than the vault
// itself, because there are two kinds of share: the vault's, which the vault
// password opens, and a sub vault's, which only that sub vault's own password
// opens. The remembering is the same for both.
type verifier struct {
	// open reports whether what the password guards is readable right now.
	// A remembered credential is only honoured while it is: locking is meant
	// to take access away, and a cached "yes" must not survive it.
	open func() bool

	// verify checks a password against what it guards, opening that if it is
	// shut. It is the expensive call the cache exists to avoid.
	verify func(password string) error

	ttl time.Duration
	key []byte

	mu   sync.Mutex
	seen map[string]time.Time
}

func newVerifier(open func() bool, verify func(string) error, ttl time.Duration) (*verifier, error) {
	if ttl <= 0 {
		ttl = DefaultVerifyTTL
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &verifier{open: open, verify: verify, ttl: ttl, key: key, seen: map[string]time.Time{}}, nil
}

// mainVaultVerifier guards the vault's own share with the vault password.
//
// A correct password against a locked vault unlocks it, deliberately. A
// mounted share outlives the idle timeout, so the alternative is a mount that
// goes dead until someone opens a browser. The credential that would unlock the
// vault through the web UI unlocks it here too, and the surface is the same
// one /api/vault/unlock already presents to anyone who can reach the port.
func mainVaultVerifier(v *vault.Vault, ttl time.Duration) (*verifier, error) {
	return newVerifier(v.Unlocked, func(password string) error {
		if v.Unlocked() {
			return v.VerifyPassword(password)
		}
		return v.Unlock(password)
	}, ttl)
}

// subVaultVerifier guards one sub vault's share with that sub vault's password
// and nothing else — the vault password opens every other section of the file
// and does not open this one, and so does not open this share either.
//
// A correct password against a shut sub vault opens it, for the reason the
// vault's own share unlocks the vault: the mount outlives whoever opened the
// sub vault in the app. What it cannot do is open the vault around it. A sub
// vault's files are on accounts whose credentials the vault password seals,
// so while the vault is locked this verifier reports ErrLocked and the share
// says so, rather than asking for a password that would change nothing.
func subVaultVerifier(v *vault.Vault, id string, ttl time.Duration) (*verifier, error) {
	return newVerifier(func() bool { return v.SubVaultUnlocked(id) }, func(password string) error {
		// Opens a shut sub vault, and checks the password against an open
		// one — never a bare "already open, so yes". A credential arriving
		// from outside the app is checked every time it is not remembered.
		return v.UnlockSubVault(id, password)
	}, ttl)
}

// fingerprint is the form a credential is remembered in.
func (a *verifier) fingerprint(password string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(password))
	return string(mac.Sum(nil))
}

// check verifies a password, opening what it guards if that is shut.
func (a *verifier) check(password string) error {
	if password == "" {
		return errors.New("no password")
	}
	print := a.fingerprint(password)

	a.mu.Lock()
	expiry, remembered := a.seen[print]
	fresh := remembered && time.Now().Before(expiry)
	a.mu.Unlock()

	// A remembered credential still has to meet an open vault: locking is
	// meant to take access away, and a cached "yes" must not survive it.
	if fresh && a.open() {
		return nil
	}

	if err := a.verify(password); err != nil {
		a.forget(print)
		return err
	}

	a.mu.Lock()
	a.seen[print] = time.Now().Add(a.ttl)
	// Sweeping here keeps the map to the handful of credentials actually in
	// use rather than every wrong guess ever made at it.
	for key, expiry := range a.seen {
		if time.Now().After(expiry) {
			delete(a.seen, key)
		}
	}
	a.mu.Unlock()
	return nil
}

func (a *verifier) forget(print string) {
	a.mu.Lock()
	delete(a.seen, print)
	a.mu.Unlock()
}

// Options configures a WebDAV endpoint.
type Options struct {
	// Prefix is the URL path the share is mounted under, e.g. "/dav". For
	// SubVaultHandler it is the path the sub vaults' shares sit under, e.g.
	// "/sub", and each share is one segment below it.
	Prefix string

	// Scope is which vault inside the file the share presents: the main vault
	// (the zero value), or one sub vault by ID. A share presents its scope and
	// nothing else — the main vault's share never shows a sub vault, and a sub
	// vault's share never shows the main vault or another sub vault. Which
	// password opens the share follows from it.
	Scope vault.Scope

	// VerifyTTL is how long a verified password is remembered. Zero uses
	// DefaultVerifyTTL.
	VerifyTTL time.Duration

	// Realm is what a client shows when it asks for the password.
	Realm string

	// OnActivity, when set, is called as every authenticated request begins.
	// The server uses it to keep a share that is being read from counting as
	// use, so a film playing over it does not meet the idle auto-lock halfway.
	// A request still running counts on its own, through Endpoint.InFlight.
	OnActivity func()
}

// Endpoint is one WebDAV share: Basic auth in front, one vault behind.
type Endpoint struct {
	fs    *FileSystem
	dav   *webdav.Handler
	auth  *verifier
	realm string
	opts  Options

	// inFlight counts the requests being served right now. A PUT of a film or
	// a DELETE of a folder runs for as long as the clouds take, and only the
	// moment it began is otherwise recorded as use (OnActivity) — so a
	// transfer longer than the idle timeout used to meet the auto-lock
	// partway through.
	inFlight atomic.Int64
}

// Handler builds the WebDAV endpoint for the scope opts names: the vault's own
// share by default, or one sub vault's.
func Handler(v *vault.Vault, opts Options) (*Endpoint, error) {
	// Locks are held in memory rather than in the vault: they are about one
	// server's clients not overwriting each other mid-edit, and mean nothing
	// once it restarts. macOS in particular refuses to write to a share that
	// cannot lock.
	return newEndpoint(v, opts, webdav.NewMemLS())
}

// newEndpoint is Handler with the lock system supplied, so that a sub vault
// reachable under two names shares one set of locks between them.
func newEndpoint(v *vault.Vault, opts Options, locks webdav.LockSystem) (*Endpoint, error) {
	var auth *verifier
	var err error
	if opts.Scope.Main() {
		auth, err = mainVaultVerifier(v, opts.VerifyTTL)
	} else {
		auth, err = subVaultVerifier(v, string(opts.Scope), opts.VerifyTTL)
	}
	if err != nil {
		return nil, err
	}

	realm := opts.Realm
	if realm == "" {
		realm = "SAND Vault"
	}

	fs := NewScoped(v, opts.Scope)
	dav := &webdav.Handler{
		Prefix:     opts.Prefix,
		FileSystem: fs,
		LockSystem: locks,
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("webdav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}

	return &Endpoint{fs: fs, dav: dav, auth: auth, realm: realm, opts: opts}, nil
}

// InFlight is how many requests the endpoint is serving at this moment. The
// server reads it to keep the vault open while one of them is still running.
func (e *Endpoint) InFlight() int64 { return e.inFlight.Load() }

func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The username is not checked. A vault has one owner (§15), so there is
	// nothing for a name to distinguish — the password is the whole
	// credential, and pretending otherwise would only invite someone to
	// treat the name as a second secret. The same holds of a sub vault,
	// whose one password is the one that seals it.
	_, password, ok := r.BasicAuth()
	if !ok {
		unauthorized(w, e.realm, e.opts.Scope)
		return
	}

	if err := e.auth.check(password); err != nil {
		if errors.Is(err, vault.ErrLocked) {
			// Only a sub vault's share lands here: its password cannot open
			// the vault around it, so asking for the password again would be
			// asking for one that changes nothing. Said as unavailable, not
			// as unauthorized, so a client does not sit in a password loop.
			http.Error(w, "the vault is locked; unlock it in the app, or mount the vault's own share, "+
				"and this sub vault's share will answer", http.StatusServiceUnavailable)
			return
		}
		if !errors.Is(err, vault.ErrWrongPassword) {
			log.Printf("webdav auth: %v", err)
		}
		unauthorized(w, e.realm, e.opts.Scope)
		return
	}

	e.inFlight.Add(1)
	defer e.inFlight.Add(-1)
	if e.opts.OnActivity != nil {
		e.opts.OnActivity()
	}

	// A PUT with Content-Range is a request to write part of a file. The
	// library does not read the header, so it would store the range's bytes
	// as the whole file — in place of the file, since a PUT replaces. RFC 7231
	// §4.3.4 says a server that does not understand the header on a PUT must
	// refuse the request, and 400 is the refusal it names.
	if r.Method == http.MethodPut && r.Header.Get("Content-Range") != "" {
		http.Error(w, "a partial PUT is not supported; send the whole file", http.StatusBadRequest)
		return
	}

	info := describe(e.opts.Prefix, r)
	if info == nil {
		e.dav.ServeHTTP(w, r)
		return
	}
	ctx := withRequestInfo(r.Context(), info)
	r = r.WithContext(ctx)

	if info.method != "COPY" && info.method != "MOVE" {
		e.dav.ServeHTTP(w, r)
		return
	}
	// A COPY or MOVE may have put the file it replaces aside; whether that
	// file goes or comes back depends on how the request came out, which
	// only the status says.
	rec := &statusRecorder{ResponseWriter: w}
	e.dav.ServeHTTP(rec, r)
	e.fs.finishReplacement(info, rec.landed())
}

// unauthorized asks for the password — the vault's, or the sub vault's own.
func unauthorized(w http.ResponseWriter, realm string, scope vault.Scope) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
	if scope.Main() {
		http.Error(w, "the vault password is required", http.StatusUnauthorized)
		return
	}
	http.Error(w, "the sub vault's password is required", http.StatusUnauthorized)
}
