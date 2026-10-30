package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAZeroLengthPartIsReportedMissingOverHTTP(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("pw", 4)
	result := c.uploadCut("photo.jpg", "/", []byte(strings.Repeat("photo ", 400)), c.providerIDs(), "3-of-4")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	id := result["file"].(map[string]any)["id"].(string)

	// Leave a zero-length file where part 4 is, whichever account has it.
	truncated := ""
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*-p4.sand"))
		for _, m := range matches {
			if err := os.WriteFile(m, nil, 0o600); err != nil {
				t.Fatalf("truncating %s: %v", m, err)
			}
			truncated = m
		}
	}
	if truncated == "" {
		t.Fatal("no part 4 on disk")
	}

	// The inspector says the part is not there, and why.
	w, health := c.json(http.MethodGet, "/api/files/"+id+"/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d %s", w.Code, w.Body.String())
	}
	if health["recoverable"] != true {
		t.Errorf("health = %v, want recoverable from the three good parts", health)
	}
	for _, s := range health["shards"].([]any) {
		shard := s.(map[string]any)
		if shard["part"] == float64(4) {
			if shard["present"] != false || !strings.Contains(shard["error"].(string), "truncated") {
				t.Errorf("part 4 = %v, want absent and called truncated", shard)
			}
		} else if shard["present"] != true {
			t.Errorf("part %v = %v, want present", shard["part"], shard)
		}
	}

	// And the degraded list carries the file, with the reason on the row.
	w, page := c.json(http.MethodGet, "/api/degraded", nil)
	if w.Code != http.StatusOK || page["total"] != float64(1) {
		t.Fatalf("degraded = %d %v, want the one file", w.Code, page)
	}
	row := page["files"].([]any)[0].(map[string]any)
	if row["id"] != id || row["stored"] != float64(3) || row["missing"] != float64(1) || row["readable"] != true {
		t.Errorf("row = %v, want the photo with three of four parts", row)
	}
	lost := row["lost"].([]any)
	if len(lost) != 1 || lost[0].(map[string]any)["part"] != float64(4) || !strings.Contains(lost[0].(map[string]any)["reason"].(string), "0 B") {
		t.Errorf("row.lost = %v, want part 4, 0 B", lost)
	}
}
