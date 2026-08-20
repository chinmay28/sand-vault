package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setAutomation puts a policy on a folder through the endpoint the browser uses.
func (c *testClient) setAutomation(policy map[string]any) map[string]any {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/automation", policy)
	if w.Code != http.StatusOK {
		c.t.Fatalf("POST /api/automation: %d %s", w.Code, w.Body.String())
	}
	auto, ok := body["automation"].(map[string]any)
	if !ok {
		c.t.Fatalf("no automation in %v", body)
	}
	return auto
}

// emptyAccount leaves a local-folder account answering and holding nothing,
// which is the state a file is in when the cloud that was meant to hold one of
// its parts was refusing at the moment it was uploaded.
func emptyAccount(t *testing.T, root string) {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatalf("listing %s: %v", root, err)
	}
	for _, name := range names {
		if err := os.RemoveAll(name); err != nil {
			t.Fatalf("removing %s: %v", name, err)
		}
	}
}

func TestAutomationEndpointStoresAndListsAPolicy(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	auto := c.setAutomation(map[string]any{
		"path":    "/",
		"enabled": true,
		"cadence": "daily",
		"at":      "10:00",
		"action":  "rebalance",
	})
	if auto["folder"] != "/" || auto["at"] != "10:00" || auto["action"] != "rebalance" {
		t.Fatalf("stored policy = %v", auto)
	}
	if auto["next_run_at"] == nil {
		t.Error("no next run given for a policy that is switched on")
	}

	w, body := c.json(http.MethodGet, "/api/automation", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/automation: %d %s", w.Code, w.Body.String())
	}
	list, ok := body["automations"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("listed %v, want the one policy", body["automations"])
	}
	if running, _ := body["running"].(bool); running {
		t.Error("a sweep is reported as running when none is")
	}

	// The per-folder form, which is what the browser asks on every folder it
	// opens.
	w, body = c.json(http.MethodGet, "/api/automation?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/automation?path=/: %d %s", w.Code, w.Body.String())
	}
	if body["automation"] == nil {
		t.Error("the folder's own policy came back empty")
	}
}

func TestAutomationEndpointSaysNoPolicyWithoutAnError(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, body := c.json(http.MethodGet, "/api/automation?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET on a folder with no policy: %d %s", w.Code, w.Body.String())
	}
	if body["automation"] != nil {
		t.Errorf("automation = %v, want null", body["automation"])
	}
}

func TestAutomationEndpointRefusesAScheduleItCannotKeep(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	for _, policy := range []map[string]any{
		{"path": "/", "cadence": "daily", "at": "half ten", "action": "check"},
		{"path": "/", "cadence": "fortnightly", "at": "10:00", "action": "check"},
		{"path": "/", "cadence": "daily", "at": "10:00", "action": "tidy"},
		{"path": "/nowhere", "cadence": "daily", "at": "10:00", "action": "check"},
		{"cadence": "daily", "at": "10:00", "action": "check"},
	} {
		w, _ := c.json(http.MethodPost, "/api/automation", policy)
		if w.Code == http.StatusOK {
			t.Errorf("accepted %v", policy)
		}
	}
}

func TestAutomationEndpointRemovesAPolicy(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.setAutomation(map[string]any{
		"path": "/", "enabled": true, "cadence": "hourly", "action": "check",
	})

	w, _ := c.json(http.MethodDelete, "/api/automation?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /api/automation: %d %s", w.Code, w.Body.String())
	}

	_, body := c.json(http.MethodGet, "/api/automation", nil)
	if list, _ := body["automations"].([]any); len(list) != 0 {
		t.Errorf("policies left = %v, want none", list)
	}

	// And saying so again is a 404 rather than a silent success.
	w, _ = c.json(http.MethodDelete, "/api/automation?path=/", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", w.Code)
	}
}

// shardsOf is the storage half of a run's report. The counts live under the
// task that produced them, because a repository count means nothing to the
// storage job and a file count means nothing to the mirror one.
func shardsOf(t *testing.T, run map[string]any) map[string]any {
	t.Helper()
	res, ok := run["shards"].(map[string]any)
	if !ok {
		t.Fatalf("run has no shards result: %v", run)
	}
	return res
}

func TestAutomationRunReportsAMissingPart(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("pw", 3)

	c.upload("notes.txt", "/", []byte("one of these parts is about to go missing"))
	emptyAccount(t, roots[0])

	c.setAutomation(map[string]any{
		"path": "/", "enabled": true, "cadence": "daily", "at": "10:00", "action": "check",
	})

	w, body := c.json(http.MethodPost, "/api/automation/run", map[string]any{"path": "/"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/automation/run: %d %s", w.Code, w.Body.String())
	}
	run, ok := body["run"].(map[string]any)
	if !ok {
		t.Fatalf("no run in %v", body)
	}
	if got := number(t, shardsOf(t, run), "checked"); got != 1 {
		t.Errorf("checked = %d, want 1", got)
	}
	if got := number(t, shardsOf(t, run), "short"); got != 1 {
		t.Errorf("short = %d, want the one file missing a part", got)
	}
	if got := number(t, shardsOf(t, run), "repaired"); got != 0 {
		t.Errorf("repaired = %d, want none from a check-only policy", got)
	}

	// And the run is on the record afterwards.
	_, body = c.json(http.MethodGet, "/api/automation?path=/", nil)
	auto := body["automation"].(map[string]any)
	if history, _ := auto["history"].([]any); len(history) != 1 {
		t.Errorf("history = %v, want the one run", auto["history"])
	}
	if auto["last_run_at"] == nil {
		t.Error("the run did not stamp the policy")
	}
}

func TestAutomationRunRebuildsOntoAnotherCloud(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("pw", 4)
	ids := c.providerIDs()

	// Three of the four hold it; the fourth is the spare a repair can reach for.
	payload := []byte("this file will lose a cloud and get it back")
	result := c.uploadTo("notes.txt", "/", payload, ids[:3])
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload: %v", result["error"])
	}
	id := result["file"].(map[string]any)["id"].(string)
	emptyAccount(t, roots[0])

	c.setAutomation(map[string]any{
		"path": "/", "enabled": true, "cadence": "daily", "at": "10:00", "action": "rebalance",
	})

	w, body := c.json(http.MethodPost, "/api/automation/run", map[string]any{"path": "/"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/automation/run: %d %s", w.Code, w.Body.String())
	}
	run := body["run"].(map[string]any)
	if got := number(t, shardsOf(t, run), "repaired"); got != 1 {
		t.Fatalf("repaired = %d, want the one file put back: %v", got, run)
	}
	if got := number(t, shardsOf(t, run), "failed"); got != 0 {
		t.Errorf("failed = %d, want none: %v", got, run)
	}

	// A second sweep finds nothing left to do, which is what "put back" means.
	_, body = c.json(http.MethodPost, "/api/automation/run", map[string]any{"path": "/"})
	run = body["run"].(map[string]any)
	if got := number(t, shardsOf(t, run), "whole"); got != 1 {
		t.Errorf("whole = %d after the repair, want 1: %v", got, run)
	}
	if got := number(t, shardsOf(t, run), "short"); got != 0 {
		t.Errorf("short = %d after the repair, want 0: %v", got, run)
	}

	// And the whole point of it: the file still reads back, byte for byte.
	w = c.do(http.MethodGet, "/api/files/"+id+"/content?download=1", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("reading the rebuilt file: %d %s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(payload) {
		t.Error("the rebuilt file does not match what was stored")
	}
}

func TestAutomationRunNeedsAPolicy(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, _ := c.json(http.MethodPost, "/api/automation/run", map[string]any{"path": "/"})
	if w.Code != http.StatusNotFound {
		t.Errorf("run without a policy = %d, want 404", w.Code)
	}

	w, _ = c.json(http.MethodPost, "/api/automation/run", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("run with no folder named = %d, want 400", w.Code)
	}
}

func TestAutomationEndpointsNeedAnUnlockedVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.setAutomation(map[string]any{
		"path": "/", "enabled": true, "cadence": "hourly", "action": "check",
	})

	w, _ := c.json(http.MethodPost, "/api/vault/lock", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}

	for _, call := range []struct {
		method, path string
		payload      any
	}{
		{http.MethodGet, "/api/automation", nil},
		{http.MethodPost, "/api/automation", map[string]any{
			"path": "/", "cadence": "hourly", "action": "check"}},
		{http.MethodDelete, "/api/automation?path=/", nil},
		{http.MethodPost, "/api/automation/run", map[string]any{"path": "/"}},
	} {
		w, _ := c.json(call.method, call.path, call.payload)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s while locked = %d, want 401", call.method, call.path, w.Code)
		}
	}
}

func TestAutomationSurvivesARename(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/archive"})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body.String())
	}
	c.setAutomation(map[string]any{
		"path": "/archive", "enabled": true, "cadence": "weekly",
		"at": "03:00", "weekday": int(time.Sunday), "action": "check",
	})

	w, _ = c.json(http.MethodPost, "/api/folders/move", map[string]any{
		"from": "/archive", "to": "/keep",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("move folder: %d %s", w.Code, w.Body.String())
	}

	_, body := c.json(http.MethodGet, "/api/automation?path=/keep", nil)
	if body["automation"] == nil {
		t.Fatal("the renamed folder stopped being looked after")
	}
	_, body = c.json(http.MethodGet, "/api/automation?path=/archive", nil)
	if body["automation"] != nil {
		t.Error("a policy is still filed under the old name")
	}
}
