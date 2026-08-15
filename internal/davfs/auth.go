package davfs

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"log"
	"net/http"
	"sync"
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

// verifier answers "is this the vault password?" without paying for it every
// time.
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
type verifier struct {
	vault *vault.Vault
	ttl   time.Duration
	key   []byte

	mu   sync.Mutex
	seen map[string]time.Time
}

func newVerifier(v *vault.Vault, ttl time.Duration) (*verifier, error) {
	if ttl <= 0 {
		ttl = DefaultVerifyTTL
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &verifier{vault: v, ttl: ttl, key: key, seen: map[string]time.Time{}}, nil
}

// fingerprint is the form a credential is remembered in.
func (a *verifier) fingerprint(password string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(password))
	return string(mac.Sum(nil))
}

// check verifies a password, unlocking the vault if it is locked.
//
// That last part is deliberate. A mounted share outlives the idle timeout, so
// the alternative is a mount that goes dead until someone opens a browser. The
// credential that would unlock the vault through the web UI unlocks it here
// too, and the surface is the same one /api/vault/unlock already presents to
// anyone who can reach the port.
func (a *verifier) check(password string) error {
	if password == "" {
		return errors.New("no password")
	}
	print := a.fingerprint(password)

	a.mu.Lock()
	expiry, remembered := a.seen[print]
	fresh := remembered && time.Now().Before(expiry)
	a.mu.Unlock()

	// A remembered credential still has to meet an unlocked vault: locking is
	// meant to take access away, and a cached "yes" must not survive it.
	if fresh && a.vault.Unlocked() {
		return nil
	}

	var err error
	if a.vault.Unlocked() {
		err = a.vault.VerifyPassword(password)
	} else {
		err = a.vault.Unlock(password)
	}
	if err != nil {
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

// Options configures the WebDAV endpoint.
type Options struct {
	// Prefix is the URL path the share is mounted under, e.g. "/dav".
	Prefix string

	// VerifyTTL is how long a verified password is remembered. Zero uses
	// DefaultVerifyTTL.
	VerifyTTL time.Duration

	// Realm is what a client shows when it asks for the password.
	Realm string

	// OnActivity, when set, is called after every authenticated request. The
	// server uses it to keep a share that is being read from counting as use,
	// so a film playing over it does not meet the idle auto-lock halfway.
	OnActivity func()
}

// Handler builds the WebDAV endpoint: Basic auth in front, the vault behind.
func Handler(v *vault.Vault, opts Options) (http.Handler, error) {
	auth, err := newVerifier(v, opts.VerifyTTL)
	if err != nil {
		return nil, err
	}

	realm := opts.Realm
	if realm == "" {
		realm = "SAND Vault"
	}

	dav := &webdav.Handler{
		Prefix:     opts.Prefix,
		FileSystem: New(v),
		// Locks are held in memory rather than in the vault: they are about one
		// server's clients not overwriting each other mid-edit, and mean nothing
		// once it restarts. macOS in particular refuses to write to a share that
		// cannot lock.
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("webdav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The username is not checked. A vault has one owner (§15), so there is
		// nothing for a name to distinguish — the password is the whole
		// credential, and pretending otherwise would only invite someone to
		// treat the name as a second secret.
		_, password, ok := r.BasicAuth()
		if !ok {
			unauthorized(w, realm)
			return
		}

		if err := auth.check(password); err != nil {
			if !errors.Is(err, vault.ErrWrongPassword) {
				log.Printf("webdav auth: %v", err)
			}
			unauthorized(w, realm)
			return
		}

		if opts.OnActivity != nil {
			opts.OnActivity()
		}
		dav.ServeHTTP(w, r)
	}), nil
}

// unauthorized asks for the password.
func unauthorized(w http.ResponseWriter, realm string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
	http.Error(w, "the vault password is required", http.StatusUnauthorized)
}
