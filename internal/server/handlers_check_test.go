package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// damageShard flips a byte in the first chunk's shard of the given part,
// wherever among the accounts it is.
func damageShard(t *testing.T, roots []string, part string) {
	t.Helper()
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*-c0000000-p"+part+".sand"))
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)-10] ^= 0x01
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no account holds part %s", part)
}

func (c *testClient) await(path string) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		w, body := c.json(http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			c.t.Fatalf("poll %s: %d %s", path, w.Code, w.Body.String())
		}
		if running, _ := body["running"].(bool); !running {
			return body
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s did not finish: %v", path, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The whole feature over HTTP: survey, check, find the damaged file, repair
// it, and check again to find nothing.
func TestCheckAndRepairOverHTTP(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	// The damaged file goes up first, while its shards are the only ones on
	// the accounts to find.
	broken := c.upload("broken.txt", "/", []byte(strings.Repeat("worth keeping ", 500)))
	c.server.vault.AwaitBackupSync()
	damageShard(t, roots, "2")
	c.upload("fine.txt", "/", []byte("nothing wrong here"))

	w, survey := c.json(http.MethodGet, "/api/check/survey?path=/", nil)
	if w.Code != http.StatusOK || survey["files"].(float64) != 2 {
		t.Fatalf("survey: %d %v", w.Code, survey)
	}

	if w, _ := c.json(http.MethodPost, "/api/check/repair", map[string]any{}); w.Code != http.StatusConflict {
		t.Fatalf("a repair before any check: %d", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/check", map[string]any{"path": "/", "kinds": []string{"films"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown kind: %d", w.Code)
	}

	w, _ = c.json(http.MethodPost, "/api/check", map[string]any{"path": "/"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	report, _ := c.await("/api/check")["result"].(map[string]any)
	if report == nil || report["checked"].(float64) != 2 || report["repairable"].(float64) != 1 {
		t.Fatalf("report = %v", report)
	}
	flagged := report["flagged"].([]any)
	if len(flagged) != 1 || flagged[0].(map[string]any)["id"] != broken["id"] {
		t.Fatalf("flagged = %v", flagged)
	}

	// The file still reads, on its spare — the read path sets the bad shard
	// aside rather than failing on it.
	if w := c.do(http.MethodGet, "/api/files/"+broken["id"].(string)+"/content", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("reading the damaged file: %d %s", w.Code, w.Body.String())
	}

	w, _ = c.json(http.MethodPost, "/api/check/repair", map[string]any{"ids": []string{broken["id"].(string)}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("repair: %d %s", w.Code, w.Body.String())
	}
	repaired, _ := c.await("/api/check/repair")["result"].(map[string]any)
	if repaired == nil || repaired["repaired"].(float64) != 1 {
		t.Fatalf("repair report = %v", repaired)
	}

	w, one := c.json(http.MethodPost, "/api/files/"+broken["id"].(string)+"/check", nil)
	if w.Code != http.StatusOK || one["state"] != "sound" {
		t.Fatalf("after the repair: %d %v", w.Code, one)
	}
}

// A file whose parts cannot give back its first chunk is an error with the
// reason in it, not a 200 with nothing in the body.
func TestUnreadableFileIsAnErrorNotAnEmptySuccess(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	file := c.upload("gone.txt", "/", []byte(strings.Repeat("unlucky ", 300)))
	c.server.vault.AwaitBackupSync()
	damageShard(t, roots, "1")
	damageShard(t, roots, "2")

	w := c.do(http.MethodGet, "/api/files/"+file["id"].(string)+"/content", nil, "")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "damaged") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
