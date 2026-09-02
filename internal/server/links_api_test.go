package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The lifetime of a download link is a setting: read with its bounds, set in
// hours, and put back to the default with a zero.
func TestLinkLifetimeIsASetting(t *testing.T) {
	c := zipFixture(t)

	w, body := c.json(http.MethodGet, "/api/vault/links", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if number(t, body, "hours") != 3 || number(t, body, "default_hours") != 3 {
		t.Errorf("a fresh vault's links last %v, want 3 hours", body)
	}
	if number(t, body, "min_hours") != 1 || number(t, body, "max_hours") != 168 {
		t.Errorf("bounds came back as %v", body)
	}

	w, body = c.json(http.MethodPost, "/api/vault/links", map[string]any{"hours": 6})
	if w.Code != http.StatusOK || number(t, body, "hours") != 6 {
		t.Fatalf("setting 6 hours: %d %s", w.Code, w.Body.String())
	}
	if c.server.zips.ttl != 6*time.Hour {
		t.Errorf("the ticket store lasts %v after the change, want 6h", c.server.zips.ttl)
	}

	for _, hours := range []int{-1, 169} {
		if w, _ := c.json(http.MethodPost, "/api/vault/links", map[string]any{"hours": hours}); w.Code != http.StatusBadRequest {
			t.Errorf("%d hours answered %d, want 400", hours, w.Code)
		}
	}

	w, body = c.json(http.MethodPost, "/api/vault/links", map[string]any{"hours": 0})
	if w.Code != http.StatusOK || number(t, body, "hours") != 3 {
		t.Errorf("zero did not put the default back: %d %v", w.Code, body)
	}
}

// A lifetime made shorter shortens the links already out there: one minted
// under a three-hour rule is not good for three hours once the rule says one.
func TestShorteningTheLifetimeShortensExistingLinks(t *testing.T) {
	c := zipFixture(t)

	_, link := c.mintZip("/photos")
	url, _ := link["url"].(string)
	token := strings.Split(strings.TrimPrefix(url, "/zip/"), "/")[0]

	if w, _ := c.json(http.MethodPost, "/api/vault/links", map[string]any{"hours": 1}); w.Code != http.StatusOK {
		t.Fatalf("setting 1 hour: %d", w.Code)
	}

	c.server.zips.mu.Lock()
	expiry := c.server.zips.tickets[token].expiry
	c.server.zips.mu.Unlock()
	if left := time.Until(expiry); left > time.Hour+time.Minute {
		t.Errorf("a link minted under the old rule is still good for %v", left.Round(time.Minute))
	}

	// And it still works within the shorter window.
	if resp := fetchZip(t, c, http.MethodHead, url); resp.Code != http.StatusOK {
		t.Errorf("the shortened link stopped answering: %d", resp.Code)
	}
}

func TestLinkSettingsNeedASession(t *testing.T) {
	c := zipFixture(t)
	c.cookies = nil
	if w, _ := c.json(http.MethodGet, "/api/vault/links", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("reading without a session answered %d", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/links", map[string]any{"hours": 2}); w.Code != http.StatusUnauthorized {
		t.Errorf("setting without a session answered %d", w.Code)
	}
}
