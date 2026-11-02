package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// sessionCookie is the name of the cookie carrying the browser's session token.
const sessionCookie = "sand_session"

// DefaultIdleTimeout is how long a session survives without activity before
// the vault re-locks itself.
const DefaultIdleTimeout = 30 * time.Minute

// session is one browser's claim on the unlocked vault: when that claim
// lapses unless renewed, and how many of its requests are running right now.
type session struct {
	expiry time.Time

	// inFlight counts the requests under this token that have begun and not
	// yet returned. A session with one is in use whatever its expiry says: a
	// folder of films takes longer to arrive than the idle timeout, and a
	// request is renewed when it begins, so without this the token would
	// lapse — and the auto-lock take the keys — part way through the very
	// request that is the activity.
	inFlight int
}

// sessionStore tracks browser sessions for an unlocked vault. When the last
// session expires the vault is locked again, so walking away from the machine
// eventually pulls the keys out of memory.
//
// A session is live while its idle deadline has not passed or a request under
// it is still running. The deadline moves on every request that counts as
// use — when it begins, and again when it ends, so a request that outlasts
// the timeout is followed by a full timeout's grace rather than none.
type sessionStore struct {
	idleTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*session // token -> session
}

func newSessionStore(idleTimeout time.Duration) *sessionStore {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &sessionStore{idleTimeout: idleTimeout, sessions: map[string]*session{}}
}

// issue mints a new session token.
func (s *sessionStore) issue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = &session{expiry: time.Now().Add(s.idleTimeout)}
	return token, nil
}

// lapsed reports whether a session has run out — past its deadline with
// nothing running under it. Called with the lock held.
func (sess *session) lapsed(now time.Time) bool {
	return sess.inFlight == 0 && now.After(sess.expiry)
}

// find looks a token up, dropping every session that has lapsed on the way.
// Called with the lock held.
func (s *sessionStore) find(token string, now time.Time) *session {
	var found *session
	for existing, sess := range s.sessions {
		if sess.lapsed(now) {
			delete(s.sessions, existing)
			continue
		}
		// Constant-time compare so a token cannot be recovered by timing the
		// lookup, even though the window is tiny.
		if subtle.ConstantTimeCompare([]byte(existing), []byte(token)) == 1 {
			found = sess
		}
	}
	return found
}

// validate checks a token and, when valid, extends its idle deadline.
func (s *sessionStore) validate(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	sess := s.find(token, now)
	if sess == nil {
		return false
	}
	sess.expiry = now.Add(s.idleTimeout)
	return true
}

// peek checks a token without counting the check as use. For a request the
// browser makes on a timer rather than because anybody did anything: a poll
// that renewed the deadline would keep the vault open for as long as a tab
// was left open, and the idle lock would never fire on a machine somebody
// walked away from.
func (s *sessionStore) peek(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(token, time.Now()) != nil
}

// acquire validates a token and marks a request as running under it. The
// session cannot lapse until release is called, which also moves the deadline
// on from that moment. release is safe to call once, and is a no-op for a
// session that was revoked or cleared while the request ran — an explicit lock
// is not undone by the request it interrupted finishing.
func (s *sessionStore) acquire(token string) (release func(), ok bool) {
	if token == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	sess := s.find(token, now)
	if sess == nil {
		return nil, false
	}
	sess.expiry = now.Add(s.idleTimeout)
	sess.inFlight++

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			// The same session, not just the same token: a session revoked
			// meanwhile must stay revoked.
			if current, held := s.sessions[token]; held && current == sess {
				sess.inFlight--
				sess.expiry = time.Now().Add(s.idleTimeout)
			}
		})
	}, true
}

// revoke drops a single session and reports whether any remain.
func (s *sessionStore) revoke(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
	return len(s.sessions) > 0
}

// clear drops every session.
func (s *sessionStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*session{}
}

// sweep removes lapsed sessions and reports how many are still live — past
// their deadline or not, a session with a request running is live.
func (s *sessionStore) sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for token, sess := range s.sessions {
		if sess.lapsed(now) {
			delete(s.sessions, token)
		}
	}
	return len(s.sessions)
}

// setCookie writes the session cookie. SameSite=Strict is what keeps another
// site in the same browser from driving this API through the user's cookie.
//
// No Max-Age, on purpose. The cookie used to be given the idle timeout as its
// lifetime, which the browser counts from the moment it was set and nothing
// ever reset: thirty minutes after unlocking, however busy, the browser
// stopped sending it, the next request was refused as locked, and an upload
// that was going to take all evening stopped at the half-hour with the vault
// still open behind it. The store is the one clock on how long a session
// lasts — it is the only thing that knows when the session was last used —
// so the cookie lives until the browser is closed and the store decides
// whether it still means anything.
func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie expires the session cookie.
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// sessionToken reads the token out of a request.
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}
