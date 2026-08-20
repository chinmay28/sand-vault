package server

import (
	"fmt"
	"net/http"
	"testing"
)

// degradedFiles reads one page of the endpoint and hands back the rows.
func degradedFiles(t *testing.T, c *testClient, query string) (map[string]any, []map[string]any) {
	t.Helper()

	w, body := c.json(http.MethodGet, "/api/degraded"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/degraded%s: %d %s", query, w.Code, w.Body.String())
	}
	raw, ok := body["files"].([]any)
	if !ok {
		t.Fatalf("no files in %v", body)
	}
	files := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		files = append(files, entry.(map[string]any))
	}
	return body, files
}

// A forced disconnect drops the shard records pointing at the account, which is
// the same state an upload lands in when one cloud was not answering: the file
// is still there, still readable, and short of the spread it asked for.
func TestDegradedEndpointListsTheFilesShortAPart(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	short := c.uploadTo("short.txt", "/", []byte("one part will go"), ids[:3])["file"].(map[string]any)
	whole := c.uploadTo("whole.txt", "/", []byte("all three stay"), ids[1:4])["file"].(map[string]any)

	w, _ := c.json(http.MethodDelete, "/api/providers/"+ids[0]+"?force=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}

	body, files := degradedFiles(t, c, "")
	if got := number(t, body, "total"); got != 1 {
		t.Fatalf("total = %d, want 1: %v", got, body)
	}
	if len(files) != 1 {
		t.Fatalf("listed %d files, want 1", len(files))
	}
	if files[0]["id"] != short["id"] {
		t.Errorf("listed %v, want the short file %v", files[0]["path"], short["name"])
	}
	if files[0]["path"] != "/short.txt" {
		t.Errorf("path = %v, want /short.txt", files[0]["path"])
	}
	if got := number(t, files[0], "stored"); got != 2 {
		t.Errorf("stored = %d, want 2", got)
	}
	if got := number(t, files[0], "missing"); got != 1 {
		t.Errorf("missing = %d, want 1", got)
	}
	if readable, _ := files[0]["readable"].(bool); !readable {
		t.Error("a file with 2 of its 3 parts reports itself unreadable")
	}
	if shards, ok := files[0]["shards"].([]any); !ok || len(shards) != 2 {
		t.Errorf("carries %v placements, want the 2 still standing", files[0]["shards"])
	}
	if whole["id"] == files[0]["id"] {
		t.Error("the untouched file is in the list")
	}

	// The figure the panel draws and the list behind it name the same files.
	_, status := c.json(http.MethodGet, "/api/vault", nil)
	stats := status["stats"].(map[string]any)
	if got := number(t, stats, "degraded"); got != number(t, body, "total") {
		t.Errorf("stats say %d degraded, the list has %d", got, number(t, body, "total"))
	}
}

func TestDegradedEndpointPagesAndCountsTheWholeList(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	const files = 5
	for i := 0; i < files; i++ {
		c.uploadTo(fmt.Sprintf("file%d.txt", i), "/", []byte("short one"), ids[:3])
	}
	if w, _ := c.json(http.MethodDelete, "/api/providers/"+ids[0]+"?force=1", nil); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}

	seen := map[string]bool{}
	for offset := 0; offset < files; offset += 2 {
		body, page := degradedFiles(t, c, fmt.Sprintf("?offset=%d&limit=2", offset))
		if got := number(t, body, "total"); got != files {
			t.Errorf("page at %d says %d in total, want %d", offset, got, files)
		}
		if number(t, body, "offset") != offset || number(t, body, "limit") != 2 {
			t.Errorf("page reports offset %d limit %d, want %d and 2",
				number(t, body, "offset"), number(t, body, "limit"), offset)
		}
		for _, file := range page {
			id := file["id"].(string)
			if seen[id] {
				t.Errorf("%v appears on two pages", file["path"])
			}
			seen[id] = true
		}
	}
	if len(seen) != files {
		t.Errorf("paging showed %d files, want all %d", len(seen), files)
	}
}

func TestDegradedEndpointRefusesANonsensePage(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	for _, query := range []string{"?offset=-1", "?limit=nope", "?offset=two"} {
		w, _ := c.json(http.MethodGet, "/api/degraded"+query, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET /api/degraded%s: %d, want 400", query, w.Code)
		}
	}
}

func TestDegradedEndpointNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.cookies = nil

	w, _ := c.json(http.MethodGet, "/api/degraded", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", w.Code)
	}
}
