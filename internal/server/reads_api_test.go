package server

import (
	"net/http"
	"strings"
	"testing"
)

// The race board, over the wire.
//
// Reading a file is a race between the accounts holding its shards, and the
// app has never had anything to say about who keeps winning it. These are
// about the endpoint that now does.

func readsBoard(t *testing.T, c *testClient) map[string]any {
	t.Helper()

	w, body := c.json(http.MethodGet, "/api/reads", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("read stats: %d %s", w.Code, w.Body.String())
	}
	reads, ok := body["reads"].(map[string]any)
	if !ok {
		t.Fatalf("no reads in %v", body)
	}
	return reads
}

func TestReadStatsReportsWhoAnsweredTheReads(t *testing.T) {
	c := newTestClient(t)
	c.setup("who answers the reads", 3)

	// Nothing has been read yet: every connected account is on the board with
	// nothing against it, which is what an honest empty state looks like.
	empty := readsBoard(t, c)
	if empty["races"].(float64) != 0 {
		t.Errorf("races = %v before anything was read", empty["races"])
	}
	if got := len(empty["accounts"].([]any)); got != 3 {
		t.Errorf("accounts = %d on a board nobody has raced on, want 3", got)
	}

	id := c.upload("notes.txt", "/", []byte(strings.Repeat("something to read back ", 200)))["id"].(string)
	if w := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}

	board := readsBoard(t, c)
	if board["races"].(float64) == 0 {
		t.Fatalf("no races recorded after a download")
	}
	if board["shortfalls"].(float64) != 0 {
		t.Errorf("shortfalls = %v on a download that worked", board["shortfalls"])
	}
	if board["since"] == nil {
		t.Errorf("the board does not say when it started counting")
	}

	wins := 0.0
	for _, raw := range board["accounts"].([]any) {
		a := raw.(map[string]any)
		if a["name"] == nil || a["provider_id"] == nil {
			t.Errorf("an account with no name on the board: %v", a)
		}
		if a["failures"].(float64) != 0 {
			t.Errorf("%v failed a fetch with every account up: %v", a["name"], a["last_error"])
		}
		wins += a["wins"].(float64)
	}
	if wins == 0 {
		t.Errorf("a file was rebuilt and no account is credited with a shard of it")
	}

	// Ranked, so the account carrying the reads is the first thing read and
	// the one carrying none is the last.
	accounts := board["accounts"].([]any)
	for i := 1; i < len(accounts); i++ {
		prev := accounts[i-1].(map[string]any)["wins"].(float64)
		if cur := accounts[i].(map[string]any)["wins"].(float64); cur > prev {
			t.Errorf("account %d won %v races, more than the one above it (%v)", i, cur, prev)
		}
	}
}

func TestReadStatsResetStartsTheCountingAgain(t *testing.T) {
	c := newTestClient(t)
	c.setup("start again", 3)

	id := c.upload("notes.txt", "/", []byte("read once"))["id"].(string)
	if w := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	if readsBoard(t, c)["races"].(float64) == 0 {
		t.Fatalf("nothing recorded to reset")
	}

	w, body := c.json(http.MethodPost, "/api/reads/reset", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}
	// The reset answers with the board it just cleared, so the panel redraws
	// from the response rather than asking again.
	if got := body["reads"].(map[string]any)["races"].(float64); got != 0 {
		t.Errorf("races = %v in the reset's own answer", got)
	}

	after := readsBoard(t, c)
	if after["races"].(float64) != 0 {
		t.Errorf("races = %v after a reset", after["races"])
	}
	if got := len(after["accounts"].([]any)); got != 3 {
		t.Errorf("accounts = %d after a reset, want the 3 still connected", got)
	}
}

// The board is behind the session like everything else the vault knows: which
// clouds a stranger's vault is on, and which of them is limping, is not a
// question the lock screen answers.
func TestReadStatsNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("locked out", 2)
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}

	if w, _ := c.json(http.MethodGet, "/api/reads", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/reads on a locked vault: %d, want 401", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/reads/reset", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/reads/reset on a locked vault: %d, want 401", w.Code)
	}
}
