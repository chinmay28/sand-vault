package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mintStream asks for a stream link and returns the path it plays from.
func (c *testClient) mintStream(id string) string {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/files/"+id+"/stream", nil)
	if w.Code != http.StatusCreated {
		c.t.Fatalf("mint stream link: %d %s", w.Code, w.Body.String())
	}
	url, _ := body["url"].(string)
	if url == "" {
		c.t.Fatalf("stream link came back without a url: %v", body)
	}
	return url
}

// The whole point of a ticket: a player that has none of what authenticates the
// app can still play the one file the link was minted for.
func TestStreamLinkPlaysWithoutASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	content := []byte("a small film, byte for byte")
	file := c.upload("film.mp4", "/", content)
	link := c.mintStream(file["id"].(string))

	// The name is the last segment because a player picks its demuxer off the
	// extension before a single byte arrives.
	if !strings.HasSuffix(link, "/film.mp4") {
		t.Errorf("stream link %q does not end in the file's own name", link)
	}

	// Deliberately not c.do: no cookie, which is the case being tested.
	req := httptest.NewRequest(http.MethodGet, link, nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", link, w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Errorf("stream returned %q, want %q", w.Body.String(), content)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q; rebuilt plaintext must never reach a shared cache", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "video/") {
		t.Errorf("Content-Type = %q, want the stored type so the player knows what it has", got)
	}
}

// Seeking is what makes a link worth handing to a player at all: a range
// request has to be answered as one, not with the whole file.
func TestStreamLinkServesRanges(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	file := c.upload("film.mp4", "/", content)
	link := c.mintStream(file["id"].(string))

	req := httptest.NewRequest(http.MethodGet, link, nil)
	req.Host = "example.test"
	req.Header.Set("Range", "bytes=10-19")
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", w.Code)
	}
	if got, want := w.Body.String(), string(content[10:20]); got != want {
		t.Errorf("range bytes=10-19 returned %q, want %q", got, want)
	}
	if got := w.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 10-19/%d", len(content)) {
		t.Errorf("Content-Range = %q", got)
	}
}

// A player asks what it is dealing with before it starts reading.
func TestStreamLinkAnswersHead(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	content := []byte("a small film, byte for byte")
	file := c.upload("film.mp4", "/", content)
	link := c.mintStream(file["id"].(string))

	req := httptest.NewRequest(http.MethodHead, link, nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD %s = %d, want 200", link, w.Code)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes — a player checks this before it offers a scrub bar", got)
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(content))
	}
}

// A ticket stands for one file, not for the vault.
func TestStreamLinkPlaysOnlyItsOwnFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	c.upload("secret.txt", "/", []byte("not this one"))
	film := c.upload("film.mp4", "/", []byte("this one"))
	link := c.mintStream(film["id"].(string))

	// The token is the whole credential; the name after it is decoration, and
	// changing it must not change which file plays.
	token := strings.Split(strings.TrimPrefix(link, "/stream/"), "/")[0]
	req := httptest.NewRequest(http.MethodGet, "/stream/"+token+"/secret.txt", nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if got := w.Body.String(); got != "this one" {
		t.Errorf("renaming the link's last segment played %q; the token decides, not the name", got)
	}
}

// Minting one is a privileged act even though following one is not.
func TestStreamLinkNeedsASessionToMint(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	file := c.upload("film.mp4", "/", []byte("a small film"))

	c.cookies = nil
	w, _ := c.json(http.MethodPost, "/api/files/"+file["id"].(string)+"/stream", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("minting a link without a session = %d, want 401", w.Code)
	}
}

// Locking takes the keys out of memory, so every link minted against them stops
// working then rather than one failed request at a time.
func TestStreamLinkDiesWithTheVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	file := c.upload("film.mp4", "/", []byte("a small film"))
	link := c.mintStream(file["id"].(string))

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, link, nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatal("a stream link outlived the vault it was minted from")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("GET a voided link = %d, want 404", w.Code)
	}
}

// A player has no browser session, so its requests are what has to keep the
// vault awake — the same hole a mounted share has.
func TestStreamHoldsOffTheAutoLock(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	file := c.upload("film.mp4", "/", []byte("a small film"))
	link := c.mintStream(file["id"].(string))

	c.server.externalMu.Lock()
	c.server.externalActivity = time.Time{}
	c.server.externalMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, link, nil)
	req.Host = "example.test"
	c.handler.ServeHTTP(httptest.NewRecorder(), req)

	if !c.server.externalActive() {
		t.Error("playing a stream link did not count as use of the vault")
	}
}

// An unknown or expired token is a 404, and says which of those it is.
func TestStreamLinkRejectsAnUnknownToken(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	c.upload("film.mp4", "/", []byte("a small film"))

	req := httptest.NewRequest(http.MethodGet, "/stream/not-a-real-token/film.mp4", nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET an unknown token = %d, want 404", w.Code)
	}
	var body APIError
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "NO_TICKET" {
		t.Errorf("error code = %q, want NO_TICKET", body.Code)
	}
}

// Minting for a file that is not there is refused in the app, rather than
// handed over as a link that fails in VLC.
func TestStreamLinkRefusesAnUnknownFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)

	w, _ := c.json(http.MethodPost, "/api/files/does-not-exist/stream", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("minting a link for a missing file = %d, want 404", w.Code)
	}
}

// A link in use never expires underneath the player holding it; one that is put
// down does.
func TestStreamTicketDeadlineSlidesOnUse(t *testing.T) {
	store := newStreamStore(time.Minute)

	token, expiry, err := store.issue("file-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	store.mu.Lock()
	store.tickets[token] = ticket[string]{subject: "file-1", expiry: time.Now().Add(2 * time.Second)}
	store.mu.Unlock()

	id, ok := store.lookup(token)
	if !ok || id != "file-1" {
		t.Fatalf("lookup = %q, %v; want the file it was minted for", id, ok)
	}

	store.mu.Lock()
	renewed := store.tickets[token].expiry
	store.mu.Unlock()
	if time.Until(renewed) < 50*time.Second {
		t.Errorf("a ticket used just now expires in %v; the deadline did not slide", time.Until(renewed))
	}
	if expiry.IsZero() {
		t.Error("issue returned no deadline for the caller to report")
	}

	store.mu.Lock()
	store.tickets[token] = ticket[string]{subject: "file-1", expiry: time.Now().Add(-time.Second)}
	store.mu.Unlock()
	if _, ok := store.lookup(token); ok {
		t.Error("an expired ticket still resolved")
	}
}

// The last segment is the stored name, whatever it happens to contain.
func TestStreamPathEscapesTheName(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{name: "film.mp4", want: "/stream/tok/film.mp4"},
		{name: "a film (2024).mkv", want: "/stream/tok/a%20film%20%282024%29.mkv"},
		{name: "why?.mov", want: "/stream/tok/why%3F.mov"},
		{name: "a/b.mov", want: "/stream/tok/a%2Fb.mov"},
	} {
		if got := streamPath("tok", tc.name); got != tc.want {
			t.Errorf("streamPath(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
