package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// pollRelocations asks the runs endpoint until every run is done, and returns
// the final listing.
func (c *testClient) pollRelocations(t *testing.T) []map[string]any {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for {
		w, body := c.json(http.MethodGet, "/api/relocate/runs", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("runs: %d %s", w.Code, w.Body.String())
		}
		raw, _ := body["runs"].([]any)
		runs := make([]map[string]any, 0, len(raw))
		waiting := false
		for _, r := range raw {
			run := r.(map[string]any)
			runs = append(runs, run)
			if done, _ := run["done"].(bool); !done {
				waiting = true
			}
		}
		if !waiting {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("a relocation never finished: %v", runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A move handed to the machine answers at once, reports its progress where
// anyone can poll it, and leaves its outcome there for whoever comes back.
func TestDetachedRelocation(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	c.upload("film.bin", "/", make([]byte, 4096))

	// An upload with no accounts named picks three of the four at random, so
	// the move is aimed away from one the file actually landed on — otherwise
	// one run in four would find every part already in place and move nothing.
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	files, _ := listing["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("listing: %v", listing)
	}
	shards, _ := files[0].(map[string]any)["shards"].([]any)
	leaving, _ := shards[0].(map[string]any)["provider_id"].(string)
	targets := make([]string, 0, 3)
	for _, id := range ids {
		if id != leaving {
			targets = append(targets, id)
		}
	}

	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"path":     "/film.bin",
		"accounts": targets,
		"detach":   true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached relocate: %d %s", w.Code, w.Body.String())
	}
	run, _ := body["run"].(map[string]any)
	if run == nil || run["id"] == "" {
		t.Fatalf("no run in the answer: %v", body)
	}
	if detached, _ := run["detached"].(bool); !detached {
		t.Errorf("the run does not say it is detached: %v", run)
	}

	runs := c.pollRelocations(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want the one that was started", runs)
	}
	finished := runs[0]
	if e, _ := finished["error"].(string); e != "" {
		t.Fatalf("the run failed: %s", e)
	}
	report, _ := finished["report"].(map[string]any)
	if report == nil {
		t.Fatalf("a finished run carries no report: %v", finished)
	}
	if got := number(t, report, "relocated"); got != 1 {
		t.Errorf("relocated %d files, want 1: %v", got, report)
	}
	// The last progress report closed the bar out.
	if at, _ := finished["at"].(map[string]any); at != nil {
		if done := number(t, at, "done"); done != 1 {
			t.Errorf("at.done = %d, want 1: %v", done, at)
		}
		if number(t, at, "bytes") <= 0 || number(t, at, "total") <= 0 {
			t.Errorf("the bar never had bytes on it: %v", at)
		}
	}

	// Dismissing the result is the same request as stopping a run.
	w, body = c.json(http.MethodDelete, "/api/relocate/runs/"+finished["id"].(string), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body.String())
	}
	if left, _ := body["runs"].([]any); len(left) != 0 {
		t.Errorf("the dismissed run is still listed: %v", left)
	}

	// And the file really moved.
	_, listing = c.json(http.MethodGet, "/api/files?path=/", nil)
	files, _ = listing["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("listing: %v", listing)
	}
	shards, _ = files[0].(map[string]any)["shards"].([]any)
	for _, s := range shards {
		if s.(map[string]any)["provider_id"] == leaving {
			t.Errorf("a shard is still on the account the move left: %v", shards)
		}
	}
}

// A selection goes as one request and one run, answered as one report.
func TestRelocateASelectionAsOneRun(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	one := c.upload("one.txt", "/", []byte("the first file"))
	c.upload("two.txt", "/", []byte("the second file"))

	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"targets": []map[string]any{
			{"id": one["id"].(string)},
			{"path": "/two.txt"},
		},
		"accounts": ids[:3],
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate selection: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "total"); got != 2 {
		t.Errorf("the report covers %d files, want 2: %v", got, body)
	}
	if got := number(t, body, "relocated") + number(t, body, "unchanged"); got != 2 {
		t.Errorf("relocated+unchanged = %d, want 2: %v", got, body)
	}

	// A foreground run is forgotten the moment its request answers.
	_, runs := c.json(http.MethodGet, "/api/relocate/runs", nil)
	if left, _ := runs["runs"].([]any); len(left) != 0 {
		t.Errorf("a finished foreground run is still listed: %v", left)
	}
}

// A selection where one target is nonsense still moves the rest, and says
// which line failed.
func TestRelocateSelectionReportsTheTargetThatFailed(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	c.upload("real.txt", "/", []byte("here"))

	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"targets": []map[string]any{
			{"path": "/real.txt"},
			{"path": "/not-there.txt"},
		},
		"accounts": ids[:3],
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "total"); got != 1 {
		t.Errorf("total = %d, want the one real file: %v", got, body)
	}
	warnings, _ := body["warnings"].([]any)
	found := false
	for _, warning := range warnings {
		if s, _ := warning.(string); s != "" && strings.Contains(s, "not-there.txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning names the target that failed: %v", warnings)
	}

	// A detached run refuses only when nothing in the selection can be
	// planned at all.
	w, _ = c.json(http.MethodPost, "/api/relocate", map[string]any{
		"targets":  []map[string]any{{"path": "/also-not-there.txt"}},
		"accounts": ids[:3],
		"detach":   true,
	})
	if w.Code == http.StatusAccepted {
		t.Errorf("a detached move of nothing was accepted: %d", w.Code)
	}
}
