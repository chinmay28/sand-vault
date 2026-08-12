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

// sessionStore tracks browser sessions for an unlocked vault. When the last
// session expires the vault is locked again, so walking away from the machine
// eventually pulls the keys out of memory.
type sessionStore struct {
	idleTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

func newSessionStore(idleTimeout time.Duration) *sessionStore {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &sessionStore{idleTimeout: idleTimeout, sessions: map[string]time.Time{}}
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
	s.sessions[token] = time.Now().Add(s.idleTimeout)
	return token, nil
}

// validate checks a token and, when valid, extends its idle deadline.
func (s *sessionStore) validate(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for existing, expiry := range s.sessions {
		if now.After(expiry) {
			delete(s.sessions, existing)
			continue
		}
		// Constant-time compare so a token cannot be recovered by timing the
		// lookup, even though the window is tiny.
		if subtle.ConstantTimeCompare([]byte(existing), []byte(token)) == 1 {
			s.sessions[existing] = now.Add(s.idleTimeout)
			return true
		}
	}
	return false
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
	s.sessions = map[string]time.Time{}
}

// sweep removes expired sessions and reports how many are still live.
func (s *sessionStore) sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for token, expiry := range s.sessions {
		if now.After(expiry) {
			delete(s.sessions, token)
		}
	}
	return len(s.sessions)
}

// setCookie writes the session cookie. SameSite=Strict is what keeps another
// site in the same browser from driving this API through the user's cookie.
func setSessionCookie(w http.ResponseWriter, token string, secure bool, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(maxAge.Seconds()),
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
