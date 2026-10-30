package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheLocalDiskScanRunsAndIsScheduledOverHTTP(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("pw", 4)
	result := c.uploadCut("photo.jpg", "/", []byte(strings.Repeat("photo ", 400)), c.providerIDs(), "3-of-4")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	id := result["file"].(map[string]any)["id"].(string)

	// Daily and on, with nothing scanned yet.
	w, status := c.json(http.MethodGet, "/api/vault/partscan", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	schedule := status["schedule"].(map[string]any)
	if schedule["enabled"] != true || schedule["interval_minutes"] != float64(1440) || status["local_accounts"] != float64(4) || status["last"] != nil {
		t.Errorf("fresh status = %v, want daily, four disks, no scan yet", status)
	}

	// A truncated part, and a scan run by hand finds it: the status carries
	// the report, and the degraded list the file.
	for _, root := range roots {
		for _, m := range mustGlob(t, filepath.Join(root, "*-p2.sand")) {
			if err := os.WriteFile(m, nil, 0o600); err != nil {
				t.Fatalf("truncating: %v", err)
			}
		}
	}
	w, status = c.json(http.MethodPost, "/api/vault/partscan", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %s", w.Code, w.Body.String())
	}
	last := status["last"].(map[string]any)
	if last["parts"] != float64(4) || last["lost"] != float64(1) || last["files"] != float64(1) {
		t.Errorf("report = %v, want one of four parts lost in one file", last)
	}
	if status["next_at"] == nil {
		t.Error("a scan that ran names no next time")
	}
	_, page := c.json(http.MethodGet, "/api/degraded", nil)
	if page["total"] != float64(1) || page["files"].([]any)[0].(map[string]any)["id"] != id {
		t.Errorf("degraded after the scan = %v, want the photo", page)
	}
	_, stats := c.json(http.MethodGet, "/api/vault", nil)
	if stats["stats"].(map[string]any)["degraded"] != float64(1) {
		t.Errorf("stats after the scan = %v, want one degraded", stats["stats"])
	}

	// The schedule: changed, bounded, and off keeps the interval.
	w, status = c.json(http.MethodPost, "/api/vault/partscan/schedule", map[string]any{"interval_minutes": 360})
	if w.Code != http.StatusOK || status["schedule"].(map[string]any)["interval_minutes"] != float64(360) {
		t.Errorf("set schedule = %d %v", w.Code, status)
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/partscan/schedule", map[string]any{"interval_minutes": 5}); w.Code != http.StatusBadRequest {
		t.Errorf("a five-minute scan: %d, want 400", w.Code)
	}
	_, status = c.json(http.MethodPost, "/api/vault/partscan/schedule", map[string]any{"enabled": false})
	schedule = status["schedule"].(map[string]any)
	if schedule["enabled"] != false || schedule["interval_minutes"] != float64(360) || status["next_at"] != nil {
		t.Errorf("switched off = %v, want off at 360 with no next time", status)
	}
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	return matches
}
