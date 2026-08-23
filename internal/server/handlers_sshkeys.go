package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
)

// SSH keys SAND makes for itself, and the one thing that makes them different
// from a pasted one.
//
// Everywhere else in this server a credential arrives from the browser, is
// checked, and is stored. A generated key runs the other way: it is made here,
// and the browser is never given it. What the browser gets back is the public
// half — a line for authorized_keys, which is not a secret — and a handle
// standing in for the private half in the form it is filling in. When the
// connect request comes back carrying that handle, the private key is swapped
// in on the way past and goes straight into the encrypted vault.
//
// The private key therefore never crosses the wire in either direction, which
// is worth the small amount of state below: the connect form is served over
// plain HTTP at a LAN address on most installs, and a private key is the one
// credential here that opens a shell rather than a bucket.
//
// The state is small on purpose. A handle is not a session, not a login, and
// not anything a stored source refers to afterwards — it names a key for the
// few minutes between "make me one" and "connect", and then it ages out. A key
// nobody ever connects with is a key nobody ever installed anywhere, so
// forgetting it costs nothing.

// generatedKeyTTL is how long a key waits to be used. Long enough to open a
// terminal, ssh to the server, paste the public half into authorized_keys and
// come back; short enough that walking away from the form does not leave a key
// sitting in memory for the rest of the day.
const generatedKeyTTL = 30 * time.Minute

// maxGeneratedKeys caps how many wait at once, so a client stuck in a loop
// cannot grow the map without bound.
const maxGeneratedKeys = 16

// generatedKeyPrefix marks a form value as a handle rather than a credential.
//
// It is a prefix rather than a separate field because the handle has to travel
// through two different shapes of request — a source's private_key and a
// connected account's free-form options map — and a prefix is the one thing
// both can carry without either learning what an SSH key is. Nothing that is
// actually a private key or a password can collide with it: a PEM block starts
// with its own header, and this is not a string anybody types.
const generatedKeyPrefix = "sand-generated-key:"

// generatedKey is one private key waiting to be connected with.
//
// Only the private half is kept. The public half was handed to the browser the
// moment it was made and is the browser's to hold onto; keeping a second copy
// here would be state nothing reads.
type generatedKey struct {
	privateKey string
	madeAt     time.Time
}

// generatedKeyStore holds the keys made but not yet stored.
type generatedKeyStore struct {
	mu   sync.Mutex
	keys map[string]generatedKey
}

func newGeneratedKeyStore() *generatedKeyStore {
	return &generatedKeyStore{keys: map[string]generatedKey{}}
}

// put files a pair away and returns the handle standing in for its private
// half.
func (s *generatedKeyStore) put(pair sandsftp.KeyPair) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	// Over the cap the oldest loses, on the same reasoning as an OAuth flow:
	// whoever is at the keyboard is the one who just asked for this.
	for len(s.keys) >= maxGeneratedKeys {
		var oldestHandle string
		var oldest time.Time
		for handle, existing := range s.keys {
			if oldestHandle == "" || existing.madeAt.Before(oldest) {
				oldestHandle, oldest = handle, existing.madeAt
			}
		}
		delete(s.keys, oldestHandle)
	}

	handle := generatedKeyPrefix + token
	s.keys[handle] = generatedKey{privateKey: pair.PrivateKey, madeAt: time.Now()}
	return handle, nil
}

// privateKey returns the key a handle stands for.
//
// It does not remove it, which is the whole difference between this and a
// nonce. Connecting is the step that fails — a folder that is not there, a
// public half not installed yet, a username typed wrong — and every one of
// those is fixed by editing the form and pressing the button again. A handle
// consumed on first use would turn each of those retries into "generate a new
// key and install it again", which is the opposite of the point.
func (s *generatedKeyStore) privateKey(handle string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	key, ok := s.keys[handle]
	return key.privateKey, ok
}

func (s *generatedKeyStore) sweepLocked() {
	now := time.Now()
	for handle, key := range s.keys {
		if now.Sub(key.madeAt) > generatedKeyTTL {
			delete(s.keys, handle)
		}
	}
}

// errGeneratedKeyGone is what a handle that has aged out — or that outlived a
// restart — turns into.
//
// Said as a thing to do rather than as a failure, because there is nothing
// wrong: the form is simply older than the key it refers to, and the fix is
// one button. It matters that this is not silently treated as a pasted key,
// which is what would happen if the prefix were not recognised: the handle
// would be handed to the SSH client as a PEM block and come back as "this does
// not look like a private key", which is true and explains nothing.
var errGeneratedKeyGone = errors.New(
	"the key SAND generated for this form has expired — generate a new one, " +
		"and put its public half on the server in place of the old one")

// resolveGeneratedKey swaps a handle for the private key it stands for, and
// leaves anything else exactly as it is.
func (s *Server) resolveGeneratedKey(value string) (string, error) {
	if !strings.HasPrefix(value, generatedKeyPrefix) {
		return value, nil
	}
	if s.generatedKeys == nil {
		return "", errGeneratedKeyGone
	}
	key, ok := s.generatedKeys.privateKey(value)
	if !ok {
		return "", errGeneratedKeyGone
	}
	return key, nil
}

// resolveGeneratedKeys does the same across a settings map, so a backend whose
// form has a key field in it gets this without the map having to know which
// field that is.
func (s *Server) resolveGeneratedKeys(options map[string]string) error {
	for name, value := range options {
		resolved, err := s.resolveGeneratedKey(value)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		options[name] = resolved
	}
	return nil
}

// generateKeyRequest asks for a key pair. The comment is what ends up on the
// authorized_keys line, so it is worth letting the form say which machine this
// one is for when it knows.
type generateKeyRequest struct {
	Comment string `json:"comment,omitempty"`
}

// handleGenerateSSHKey makes a key pair and answers with the half that is safe
// to show.
//
// Behind the session check like every other endpoint that touches the vault's
// world, even though the key it makes is not yet anybody's credential. It is
// about to become one, and an endpoint that mints keys for whoever asks is an
// endpoint worth pointing at eventually.
func (s *Server) handleGenerateSSHKey(w http.ResponseWriter, r *http.Request) {
	var req generateKeyRequest
	// A body is optional here: "make me a key" is a complete request.
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
			return
		}
	}

	pair, err := sandsftp.GenerateKeyPair(req.Comment)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	if s.generatedKeys == nil {
		s.generatedKeys = newGeneratedKeyStore()
	}
	handle, err := s.generatedKeys.put(pair)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "KEYGEN_FAILED")
		return
	}

	// Note what is not in this response: the private half. The handle is what
	// the form carries in its place, and it is worth nothing to anybody who
	// cannot already reach this server with a session.
	writeJSON(w, http.StatusCreated, map[string]any{
		"handle":      handle,
		"public_key":  pair.PublicKey,
		"fingerprint": pair.Fingerprint,
	})
}
