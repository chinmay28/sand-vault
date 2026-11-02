package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A session's lifetime is the server's to decide, not the browser's. The
// cookie used to carry the idle timeout as a Max-Age counted from the unlock,
// so thirty minutes in — mid-upload, however busy — the browser stopped
// sending it and everything after was refused as locked.
func TestUnlockCookieCarriesNoLifetimeOfItsOwn(t *testing.T) {
	c := newTestClient(t)
	c.setup("a perfectly ordinary password", 0)

	w, _ := c.json(http.MethodPost, "/api/vault/lock", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}
	w, _ = c.json(http.MethodPost, "/api/vault/unlock", map[string]any{
		"password": "a perfectly ordinary password",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("unlock: %d %s", w.Code, w.Body.String())
	}

	var found bool
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name != sessionCookie {
			continue
		}
		found = true
		if cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
			t.Errorf("the session cookie expires on the browser's clock: Max-Age %d, Expires %v",
				cookie.MaxAge, cookie.Expires)
		}
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
			t.Errorf("the session cookie lost its guards: HttpOnly %v, SameSite %v", cookie.HttpOnly, cookie.SameSite)
		}
	}
	if !found {
		t.Fatal("unlock set no session cookie")
	}
}

// A request still running is use of the vault for as long as it runs. An
// upload batch over a slow link outlasts the idle timeout on its own; the
// session must neither lapse under it nor be swept while it runs, and the
// batch after it must be let in.
func TestRequestInFlightHoldsTheSessionPastTheIdleTimeout(t *testing.T) {
	c := newTestClient(t)
	c.server.IdleTimeout = 100 * time.Millisecond
	c.server.sessions = newSessionStore(c.server.IdleTimeout)
	c.setup("a perfectly ordinary password", 0)

	// The only cookie the client holds is the one setup's init issued.
	var token string
	for _, cookie := range c.cookies {
		if cookie.Name == sessionCookie {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatal("init issued no session")
	}

	entered := make(chan struct{})
	finish := make(chan struct{})
	slow := c.server.requireSession(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-finish
		w.WriteHeader(http.StatusNoContent)
	})

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		slow.ServeHTTP(w, req)
		done <- w.Code
	}()

	<-entered
	// Three timeouts later, the request is still running and the session
	// is still the one live session — the auto-lock has nothing to lock.
	time.Sleep(300 * time.Millisecond)
	if n := c.server.sessions.sweep(); n != 1 {
		t.Errorf("sessions live while a request runs: %d, want 1", n)
	}

	close(finish)
	if code := <-done; code != http.StatusNoContent {
		t.Fatalf("the slow request answered %d", code)
	}

	// The next batch, straight after.
	w := c.do(http.MethodGet, "/api/files", nil, "")
	if w.Code != http.StatusOK {
		t.Errorf("the request after a long one was refused: %d %s", w.Code, w.Body.String())
	}

	// And left alone, the session lapses as it always did.
	time.Sleep(250 * time.Millisecond)
	w = c.do(http.MethodGet, "/api/files", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an idle session was still let in: %d", w.Code)
	}
}

// The browser asks for the clouds' standing every minute for as long as a
// tab is open. That is the tab, not the person, so it must not keep the
// session alive — otherwise no machine with a tab left open ever auto-locks.
func TestHealthPollDoesNotKeepTheSessionAlive(t *testing.T) {
	c := newTestClient(t)
	c.server.IdleTimeout = 100 * time.Millisecond
	c.server.sessions = newSessionStore(c.server.IdleTimeout)
	c.setup("a perfectly ordinary password", 0)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		w := c.do(http.MethodGet, "/api/providers/health", nil, "")
		if w.Code != http.StatusOK && w.Code != http.StatusUnauthorized {
			t.Fatalf("health poll: %d %s", w.Code, w.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	w := c.do(http.MethodGet, "/api/providers/health", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("polling kept the session alive: %d %s", w.Code, w.Body.String())
	}
	w = c.do(http.MethodGet, "/api/files", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a session that was only polled still opens the vault: %d", w.Code)
	}
}
