package server

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// blockFolder puts a plain file where a connected local folder was, which is
// what a cloud that has stopped answering looks like from here: the account is
// still configured and there is nothing usable at the end of it.
func blockFolder(t *testing.T, v *vault.Vault, root string) {
	t.Helper()

	// Connecting the accounts queued a manifest push in the background. Let
	// it land before pulling the folder out from under it, or the wipe races
	// a part being written and fails on a folder that is not empty.
	v.AwaitBackupSync()
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("clearing %s: %v", root, err)
	}
	if err := os.WriteFile(root, []byte("not a folder"), 0600); err != nil {
		t.Fatalf("blocking %s: %v", root, err)
	}
}

// The line the accounts panel draws — "1 of 3 clouds unhealthy" — end to end.
func TestCloudHealthReportsTheOneThatIsDown(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("watch the clouds", 3)

	w, body := c.json(http.MethodPost, "/api/providers/health/check", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("check: %d %s", w.Code, w.Body.String())
	}
	health := body["health"].(map[string]any)
	if health["healthy"].(float64) != 3 || health["unhealthy"].(float64) != 0 {
		t.Fatalf("three working clouds: %v", health)
	}

	blockFolder(t, c.server.vault, roots[0])

	_, body = c.json(http.MethodPost, "/api/providers/health/check", nil)
	health = body["health"].(map[string]any)
	if health["unhealthy"].(float64) != 1 || health["accounts"].(float64) != 3 {
		t.Fatalf("one dead cloud of three: %v", health)
	}

	// And the read of what is already known says the same thing without
	// contacting anybody, which is the call the panel actually polls.
	w, body = c.json(http.MethodGet, "/api/providers/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET health: %d %s", w.Code, w.Body.String())
	}
	health = body["health"].(map[string]any)
	if health["unhealthy"].(float64) != 1 {
		t.Fatalf("the remembered standing: %v", health)
	}

	// Worst first, with the reason attached — an account that is red and an
	// account whose token has expired are the same colour and not the same
	// problem.
	clouds := health["clouds"].([]any)
	first := clouds[0].(map[string]any)
	if first["healthy"] == true || first["error"] == nil || first["error"] == "" {
		t.Errorf("the list leads with %v, want the unreachable one and its reason", first)
	}
	if first["failing_since"] == nil {
		t.Errorf("no failing-since on an unreachable cloud: %v", first)
	}
}

// The schedule is settable, bounded, and remembered.
func TestCloudHealthSchedule(t *testing.T) {
	c := newTestClient(t)
	c.setup("watch the clouds", 1)

	_, body := c.json(http.MethodGet, "/api/providers/health", nil)
	schedule := body["health"].(map[string]any)["schedule"].(map[string]any)
	if schedule["enabled"] != true || schedule["interval_minutes"].(float64) != 60 {
		t.Fatalf("a fresh vault is set to %v, want hourly and on", schedule)
	}

	w, body := c.json(http.MethodPost, "/api/providers/health/schedule",
		map[string]any{"interval_minutes": 360})
	if w.Code != http.StatusOK {
		t.Fatalf("set schedule: %d %s", w.Code, w.Body.String())
	}
	if got := body["schedule"].(map[string]any)["interval_minutes"].(float64); got != 360 {
		t.Errorf("interval = %v, want 360", got)
	}

	// Switching it off leaves the interval where it was, so switching it back
	// on returns to the schedule somebody chose.
	_, body = c.json(http.MethodPost, "/api/providers/health/schedule",
		map[string]any{"enabled": false})
	schedule = body["schedule"].(map[string]any)
	if schedule["enabled"] != false || schedule["interval_minutes"].(float64) != 360 {
		t.Errorf("switched off, the schedule is %v", schedule)
	}

	// A vault whose check is off names no next check, which is what stops the
	// panel promising one.
	_, body = c.json(http.MethodGet, "/api/providers/health", nil)
	if next := body["health"].(map[string]any)["next_check_at"]; next != nil {
		t.Errorf("a switched-off check is still due at %v", next)
	}

	w, _ = c.json(http.MethodPost, "/api/providers/health/schedule",
		map[string]any{"enabled": true, "interval_minutes": 1})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a one-minute interval was answered with %d, want 400", w.Code)
	}
}

// The scheduler's own arithmetic: what is due, when, and the one line it logs
// when something changes.
func TestHealthLoopLogsOnlyWhatChanged(t *testing.T) {
	report := vault.HealthReport{
		Accounts:  2,
		Healthy:   1,
		Unhealthy: 1,
		Clouds: []vault.CloudHealth{
			{ID: "a", Name: "backblaze", Checked: true, Error: "token expired"},
			{ID: "b", Name: "box", Checked: true, Healthy: true},
		},
	}

	failing := logHealthChanges(report, map[string]bool{})
	if !failing["a"] || failing["b"] {
		t.Fatalf("what is failing now = %v", failing)
	}

	// The same state next hour is not news, and the account coming back is.
	same := logHealthChanges(report, failing)
	if !same["a"] {
		t.Errorf("a cloud that is still down stopped being counted: %v", same)
	}

	report.Clouds[0].Healthy, report.Clouds[0].Error = true, ""
	report.Unhealthy, report.Healthy = 0, 2
	if mended := logHealthChanges(report, same); len(mended) != 0 {
		t.Errorf("everything is answering but %v is still counted as failing", mended)
	}
}

// A check must not keep the vault open. An hourly ping that renewed the idle
// timer would mean no machine running SAND ever auto-locked again.
func TestHealthCheckDoesNotHoldTheVaultOpen(t *testing.T) {
	c := newTestClient(t)
	c.setup("watch the clouds", 1)

	c.server.externalMu.Lock()
	before := c.server.externalActivity
	c.server.externalMu.Unlock()

	if _, err := c.server.vault.CheckClouds(t.Context()); err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}

	c.server.externalMu.Lock()
	after := c.server.externalActivity
	c.server.externalMu.Unlock()

	if !after.Equal(before) {
		t.Errorf("a health check counted as use: %v became %v", before, after)
	}
	if c.server.externalActive() {
		t.Error("a health check left something looking like an active reader")
	}
}

// A file's health carries its replica, checked in the replica folders, so
// the inspector can draw the second placement beside the first.
func TestFileHealthCarriesTheReplicaOverHTTP(t *testing.T) {
	c, _, _ := replicationSetup(t)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos"})
	uploaded := c.upload("hike.jpg", "/photos", []byte(strings.Repeat("a replicated photo ", 300)))
	fileID := uploaded["id"].(string)

	w, body := c.json(http.MethodGet, "/api/files/"+fileID+"/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d %v", w.Code, body)
	}
	if _, ok := body["replica"]; ok {
		t.Errorf("a file with no replica carries one in its health: %v", body["replica"])
	}

	if w, status := c.json(http.MethodPost, "/api/replicate", map[string]any{"path": "/photos", "enabled": true}); w.Code != http.StatusOK {
		t.Fatalf("flagging /photos = %d %v", w.Code, status)
	}
	v, _ := c.server.Vault()
	if step, err := c.server.replicateStep(v, time.Now()); err != nil || !step.Done {
		t.Fatalf("step = %+v, %v; want the photo replicated", step, err)
	}

	w, body = c.json(http.MethodGet, "/api/files/"+fileID+"/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d %v", w.Code, body)
	}
	if body["recoverable"] != true {
		t.Errorf("the parts: %v", body)
	}
	replica, ok := body["replica"].(map[string]any)
	if !ok {
		t.Fatalf("health carries no replica: %v", body)
	}
	if replica["recoverable"] != true || replica["scheme"] != "2-of-3" || replica["whole"] != false {
		t.Errorf("replica = %v, want a readable 2-of-3", replica)
	}
	shards, _ := replica["shards"].([]any)
	if len(shards) != 3 {
		t.Fatalf("replica shards = %v, want 3", replica["shards"])
	}
	for _, raw := range shards {
		s := raw.(map[string]any)
		if s["present"] != true || s["observed_size"].(float64) <= 0 {
			t.Errorf("replica shard %v", s)
		}
		if strings.Contains(s["provider_id"].(string), "#") {
			t.Errorf("a replica shard names a suffixed account id %q", s["provider_id"])
		}
	}
}
