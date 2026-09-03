package server

import (
	"net/http"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider/s3test"
)

// The scenario over HTTP: a vault on buckets that keep every version, a file
// deleted, the index rewritten a handful of times, and a browser asking what
// the buckets are storing beneath what they show — then agreeing to erase it.

// setupVersioned creates the vault and connects `buckets` versioned buckets on
// one stub server, returning the server and the accounts' IDs.
func (c *testClient) setupVersioned(password string, buckets int) (*s3test.Server, []string) {
	c.t.Helper()

	stub := s3test.New()
	c.t.Cleanup(stub.Close)

	w, _ := c.json(http.MethodPost, "/api/vault/init", map[string]any{"password": password})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("init vault: %d %s", w.Code, w.Body.String())
	}
	ids := make([]string, buckets)
	for i := range ids {
		name := "bucket-" + string(rune('a'+i))
		w, body := c.json(http.MethodPost, "/api/providers", map[string]any{
			"kind":    "s3",
			"name":    name,
			"options": stub.Options(name),
		})
		if w.Code != http.StatusCreated {
			c.t.Fatalf("connect %s: %d %v", name, w.Code, body)
		}
		ids[i] = body["provider"].(map[string]any)["id"].(string)
	}
	return stub, ids
}

func (c *testClient) versionScan() map[string]any {
	c.t.Helper()

	c.server.vault.AwaitBackupSync()
	w, body := c.json(http.MethodGet, "/api/vault/versions", nil)
	if w.Code != http.StatusOK {
		c.t.Fatalf("version scan: %d %s", w.Code, w.Body.String())
	}
	return body
}

func TestVersionScanAndSweepOverHTTP(t *testing.T) {
	c := newTestClient(t)
	stub, ids := c.setupVersioned("the vault password", 3)

	gone := c.upload("gone.txt", "/", []byte("deleted afterwards"))
	c.upload("kept.txt", "/", []byte("still here"))
	if w := c.do(http.MethodDelete, "/api/files/"+gone["id"].(string), nil, ""); w.Code != http.StatusOK {
		t.Fatalf("delete the file: %d %s", w.Code, w.Body.String())
	}

	scan := c.versionScan()
	if scan["found"] != true {
		t.Fatalf("nothing stale found on buckets that keep every version: %v", scan)
	}
	if got := scan["versioned"].(float64); got != 3 {
		t.Errorf("versioned accounts = %v, want 3", got)
	}
	stale := scan["stale"].(float64)
	if stale == 0 || scan["deletable"].(float64) != stale || scan["markers"].(float64) != 3 {
		t.Errorf("stale %v, deletable %v, markers %v", stale, scan["deletable"], scan["markers"])
	}
	accounts := scan["accounts"].([]any)
	if len(accounts) != 3 {
		t.Fatalf("%d account row(s), want 3", len(accounts))
	}
	for _, raw := range accounts {
		a := raw.(map[string]any)
		if a["versioned"] != true || a["error"] != nil || a["stale"].(float64) == 0 {
			t.Errorf("account row: %v", a)
		}
	}
	items := scan["items"].([]any)
	if len(items) == 0 {
		t.Fatal("no per-key rows")
	}
	if first := items[0].(map[string]any); first["key"] == nil || first["what"] == nil || first["deletable"] != true {
		t.Errorf("first row: %v", first)
	}

	// Nothing is running before anybody has asked.
	w, at := c.json(http.MethodGet, "/api/vault/versions/erasing", nil)
	if w.Code != http.StatusOK || at["running"] != false {
		t.Errorf("erasing before a sweep: %d %v", w.Code, at)
	}

	// A dry run promises what the scan showed, and changes nothing.
	before := len(stub.Versions("bucket-a")) + len(stub.Versions("bucket-b")) + len(stub.Versions("bucket-c"))
	w, preview := c.json(http.MethodPost, "/api/vault/versions", map[string]any{"dry_run": true})
	if w.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if preview["deleted"].(float64) != stale {
		t.Errorf("dry run promised %v, the scan showed %v", preview["deleted"], stale)
	}
	if after := len(stub.Versions("bucket-a")) + len(stub.Versions("bucket-b")) + len(stub.Versions("bucket-c")); after != before {
		t.Fatalf("a dry run erased something: %d before, %d after", before, after)
	}

	// Aimed at one account, only that bucket is cleaned.
	w, report := c.json(http.MethodPost, "/api/vault/versions", map[string]any{"accounts": []string{ids[0]}})
	if w.Code != http.StatusOK {
		t.Fatalf("sweep one account: %d %s", w.Code, w.Body.String())
	}
	if report["deleted"].(float64) == 0 || report["warnings"] != nil {
		t.Errorf("sweep of one account: %v", report)
	}
	if got := len(stub.Versions("bucket-a")); got != len(stub.Objects("bucket-a")) {
		t.Errorf("bucket-a still stores %d version(s) of %d object(s)", got, len(stub.Objects("bucket-a")))
	}
	if got := len(stub.Versions("bucket-b")); got == len(stub.Objects("bucket-b")) {
		t.Errorf("bucket-b was swept without being asked for")
	}

	// A bare POST cleans the rest.
	w, report = c.json(http.MethodPost, "/api/vault/versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	for _, bucket := range []string{"bucket-a", "bucket-b", "bucket-c"} {
		versions := stub.Versions(bucket)
		if len(versions) != len(stub.Objects(bucket)) {
			t.Errorf("%s still stores %d version(s) of %d object(s)", bucket, len(versions), len(stub.Objects(bucket)))
		}
	}
	if again := c.versionScan(); again["found"] != false {
		t.Errorf("straight after the sweep, the scan still finds %v stale", again["stale"])
	}
	if w, at := c.json(http.MethodGet, "/api/vault/versions/erasing", nil); w.Code != http.StatusOK || at["running"] != false {
		t.Errorf("erasing after the sweep: %d %v", w.Code, at)
	}

	// And the file that stayed still reads back.
	if w := c.do(http.MethodGet, "/api/files?path=/", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("list after sweep: %d %s", w.Code, w.Body.String())
	}
}

func TestVersionEndpointsNeedASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.cookies = nil

	for _, call := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPost, map[string]any{"dry_run": true}},
	} {
		w, _ := c.json(call.method, "/api/vault/versions", call.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/vault/versions without a session: %d, want 401", call.method, w.Code)
		}
	}
	w, _ := c.json(http.MethodGet, "/api/vault/versions/erasing", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/vault/versions/erasing without a session: %d, want 401", w.Code)
	}
}

func TestVersionScanOnFoldersReportsNothingToAsk(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.upload("a.txt", "/", []byte("stored"))

	scan := c.versionScan()
	if scan["found"] != false || scan["versioned"].(float64) != 0 {
		t.Errorf("local folders reported as keeping versions: %v", scan)
	}
	for _, raw := range scan["accounts"].([]any) {
		if a := raw.(map[string]any); a["versioned"] != false || a["error"] != nil {
			t.Errorf("account row: %v", a)
		}
	}
}
