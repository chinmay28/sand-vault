package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAPlannedPasswordChangeIsPacedAndReported(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/Taxes"})
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/Photos"})
	c.upload("p60.pdf", "/Taxes", []byte(strings.Repeat("t", 500)))
	c.upload("holiday.jpg", "/Photos", []byte(strings.Repeat("p", 2000)))

	w, body := c.json(http.MethodPost, "/api/vault/password", map[string]any{
		"old_password": "pw",
		"new_password": "slowly does it",
		"plan": map[string]any{
			"bytes_per_day": 1 << 40,
			"folders":       []string{"/Taxes"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	if body["remaining"] != float64(2) {
		t.Fatalf("report = %v, want both files left for the plan", body)
	}

	// The status names the key in force, so a file list can mark the rest.
	w, status := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	stats := status["stats"].(map[string]any)
	if stats["pending_migration"] != float64(2) || stats["key_id"] == "" {
		t.Errorf("stats = %v, want two pending and a key id", stats)
	}
	w, listing := c.json(http.MethodGet, "/api/files?path=/Taxes", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	file := listing["files"].([]any)[0].(map[string]any)
	if file["key_id"] == stats["key_id"] {
		t.Error("a file still on the previous key carries the current key id")
	}

	w, rekey := c.json(http.MethodGet, "/api/vault/rekey", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rekey status: %d %s", w.Code, w.Body.String())
	}
	if rekey["pending"] != float64(2) || rekey["total"] != float64(2) || rekey["total_bytes"] != float64(2500) {
		t.Errorf("rekey = %v, want 2 of 2 pending, 2500 bytes", rekey)
	}
	plan := rekey["plan"].(map[string]any)
	if plan["bytes_per_day"] != float64(1<<40) || plan["folders"].([]any)[0] != "/Taxes" {
		t.Errorf("plan = %v", plan)
	}
	folders := rekey["folders"].([]any)
	if len(folders) != 2 || folders[0].(map[string]any)["path"] != "/Taxes" || folders[0].(map[string]any)["queued"] != true {
		t.Errorf("folders = %v, want Taxes queued first", folders)
	}

	// A second change is refused while the first is still moving files.
	w, refused := c.json(http.MethodPost, "/api/vault/password", map[string]any{
		"old_password": "slowly does it",
		"new_password": "too soon",
	})
	if w.Code != http.StatusConflict || refused["code"] != "REKEY_PENDING" {
		t.Fatalf("second change = %d %v, want 409 REKEY_PENDING", w.Code, refused)
	}
	if msg, _ := refused["error"].(string); !strings.Contains(msg, "2 file(s)") {
		t.Errorf("refusal = %q, want the count", msg)
	}

	// The plan can be adjusted in flight: paused, and widened.
	w, adjusted := c.json(http.MethodPut, "/api/vault/rekey", map[string]any{
		"bytes_per_day": 1 << 30,
		"paused":        true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("set plan: %d %s", w.Code, w.Body.String())
	}
	if p := adjusted["plan"].(map[string]any); p["paused"] != true || p["bytes_per_day"] != float64(1<<30) {
		t.Errorf("adjusted plan = %v", p)
	}
	// A folder that is not there is a refusal, not a plan with a hole in it.
	if w, _ := c.json(http.MethodPut, "/api/vault/rekey", map[string]any{
		"bytes_per_day": 1 << 30, "folders": []string{"/Nowhere"},
	}); w.Code != http.StatusNotFound {
		t.Errorf("plan over a missing folder: %d, want 404", w.Code)
	}

	// One paced step, as the loop would take it, moves the queued folder's
	// file first — once the plan is running again.
	c.json(http.MethodPut, "/api/vault/rekey", map[string]any{"bytes_per_day": 1 << 40, "folders": []string{"/Taxes"}})
	v, _ := c.server.Vault()
	step, err := c.server.rekeyStep(v, time.Now())
	if err != nil || !step.Moved || step.Path != "/Taxes/p60.pdf" {
		t.Fatalf("step = %+v, %v; want the tax return moved", step, err)
	}

	// Finish now takes the rest, and the plan goes with the last file.
	w, report := c.json(http.MethodPost, "/api/vault/migrate", nil)
	if w.Code != http.StatusOK || report["remaining"] != float64(0) {
		t.Fatalf("migrate: %d %v", w.Code, report)
	}
	w, rekey = c.json(http.MethodGet, "/api/vault/rekey", nil)
	if rekey["pending"] != float64(0) || rekey["plan"] != nil {
		t.Errorf("rekey after finishing = %v, want nothing pending and no plan", rekey)
	}
	// And the second change goes through.
	if w, _ := c.json(http.MethodPost, "/api/vault/password", map[string]any{
		"old_password": "slowly does it", "new_password": "in good time",
	}); w.Code != http.StatusOK {
		t.Errorf("change after finishing: %d %s", w.Code, w.Body.String())
	}
}

func TestAPlanNeedsSomethingOnThePreviousKey(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.upload("a.txt", "/", []byte("current"))

	w, body := c.json(http.MethodPut, "/api/vault/rekey", map[string]any{"bytes_per_day": 1 << 30})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("plan with nothing pending = %d %v, want 400", w.Code, body)
	}
	w, rekey := c.json(http.MethodGet, "/api/vault/rekey", nil)
	if w.Code != http.StatusOK || rekey["pending"] != float64(0) {
		t.Errorf("rekey = %d %v, want nothing pending", w.Code, rekey)
	}
}

func TestAFileCanBeLeftForLastOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	small := c.upload("small.txt", "/", []byte("tiny"))
	c.upload("large.txt", "/", []byte(strings.Repeat("l", 4000)))

	if w, _ := c.json(http.MethodPost, "/api/vault/password", map[string]any{
		"old_password": "pw", "new_password": "later",
		"plan": map[string]any{"bytes_per_day": 1 << 40},
	}); w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}

	// Nothing is moving, so skipping "the current file" has nothing to skip.
	if w, _ := c.json(http.MethodPost, "/api/vault/rekey/skip", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Errorf("skip with nothing moving: %d, want 400", w.Code)
	}

	w, status := c.json(http.MethodPost, "/api/vault/rekey/skip", map[string]any{"id": small["id"]})
	if w.Code != http.StatusOK {
		t.Fatalf("skip: %d %s", w.Code, w.Body.String())
	}
	deferred := status["deferred"].([]any)
	if len(deferred) != 1 || deferred[0].(map[string]any)["path"] != "/small.txt" {
		t.Fatalf("deferred = %v, want small.txt", deferred)
	}

	// Smallest first would have taken small.txt; deferred, it goes last.
	v, _ := c.server.Vault()
	step, err := c.server.rekeyStep(v, time.Now())
	if err != nil || !step.Moved || step.Path != "/large.txt" {
		t.Fatalf("first step = %+v, %v; want large.txt moved first", step, err)
	}

	w, status = c.json(http.MethodPost, "/api/vault/rekey/skip", map[string]any{"id": small["id"], "undo": true})
	if w.Code != http.StatusOK || status["deferred"] != nil {
		t.Fatalf("undo = %d %v, want nothing deferred", w.Code, status["deferred"])
	}
	if step, err := c.server.rekeyStep(v, time.Now()); err != nil || !step.Moved || step.Path != "/small.txt" {
		t.Fatalf("second step = %+v, %v; want small.txt moved", step, err)
	}
}
