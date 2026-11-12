package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// awaitSnapshot waits for the snapshot job to finish. It is given longer than
// the other jobs' polls: each one opens with a key that costs a second or two
// and half a gigabyte to make, on purpose.
func (c *testClient) awaitSnapshot() map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		w, body := c.json(http.MethodGet, "/api/snapshots", nil)
		if w.Code != http.StatusOK {
			c.t.Fatalf("poll: %d %s", w.Code, w.Body.String())
		}
		if running, _ := body["running"].(bool); !running {
			return body
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the snapshot job did not finish: %v", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fileAt is the listing row of one file, or nil.
func (c *testClient) fileAt(dir, name string) map[string]any {
	c.t.Helper()
	_, body := c.json(http.MethodGet, "/api/files?path="+dir, nil)
	files, _ := body["files"].([]any)
	for _, f := range files {
		if row, _ := f.(map[string]any); row["name"] == name {
			return row
		}
	}
	return nil
}

// The whole feature over HTTP, on a cloud: plan, take, list, try a password,
// lose the folder, restore it, and erase the snapshot.
func TestSnapshotTakeAndRestoreOverHTTP(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/album"})
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/album/empty"})
	content := bytes.Repeat([]byte("holiday "), 20_000)
	c.upload("a.bin", "/album", content)
	account := c.providerIDs()[0]
	target := map[string]any{"account": account}

	w, plan := c.json(http.MethodGet, "/api/snapshots/plan?path=/album", nil)
	if w.Code != http.StatusOK || plan["files"].(float64) != 1 || plan["folders"].(float64) != 1 ||
		plan["cloud_max_part_size"].(float64) <= 0 {
		t.Fatalf("plan: %d %v", w.Code, plan)
	}

	w, started := c.json(http.MethodPost, "/api/snapshots", map[string]any{
		"path": "/album", "target": target, "password": "snap-password",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %v", w.Code, started)
	}
	if strings.Contains(w.Body.String(), "snap-password") {
		t.Error("the status hands the snapshot's password back")
	}
	report, _ := c.awaitSnapshot()["result"].(map[string]any)
	if report == nil || report["kind"] != "take" || report["files"].(float64) != 1 {
		t.Fatalf("report = %v", report)
	}
	name := report["name"].(string)
	if report["stored"].(float64) >= float64(len(content))/10 {
		t.Errorf("160 KB of repetition was stored in %v bytes", report["stored"])
	}
	if _, err := os.Stat(filepath.Join(roots[0], name+".sandsnap")); err != nil {
		t.Errorf("the snapshot is not on the account: %v", err)
	}

	w, stored := c.json(http.MethodGet, "/api/snapshots/stored?account="+account, nil)
	sets, _ := stored["snapshots"].([]any)
	if w.Code != http.StatusOK || len(sets) != 1 {
		t.Fatalf("stored: %d %v", w.Code, stored)
	}
	if set := sets[0].(map[string]any); set["name"] != name || set["complete"] != true {
		t.Errorf("the listing says %v", set)
	}

	w, wrong := c.json(http.MethodPost, "/api/snapshots/inspect", map[string]any{
		"target": target, "name": name, "password": "not it",
	})
	if w.Code != http.StatusBadRequest || wrong["code"] != "SNAPSHOT_PASSWORD" {
		t.Fatalf("a wrong password: %d %v", w.Code, wrong)
	}
	w, info := c.json(http.MethodPost, "/api/snapshots/inspect", map[string]any{
		"target": target, "name": name, "password": "snap-password",
	})
	if w.Code != http.StatusOK || info["path"] != "/album" || info["files"].(float64) != 1 {
		t.Fatalf("inspect: %d %v", w.Code, info)
	}

	if w := c.do(http.MethodDelete, "/api/folders?path=/album&recursive=true", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("deleting the folder: %d %s", w.Code, w.Body.String())
	}
	if c.fileAt("/album", "a.bin") != nil {
		t.Fatal("the folder was not deleted")
	}

	w, started = c.json(http.MethodPost, "/api/snapshots/restore", map[string]any{
		"path": "/album", "target": target, "name": name, "password": "snap-password",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("restore: %d %v", w.Code, started)
	}
	back, _ := c.awaitSnapshot()["result"].(map[string]any)
	if back == nil || back["kind"] != "restore" || back["restored"].(float64) != 1 {
		t.Fatalf("restore report = %v", back)
	}
	row := c.fileAt("/album", "a.bin")
	if row == nil {
		t.Fatal("the file did not come back")
	}
	if got := c.do(http.MethodGet, "/api/files/"+row["id"].(string)+"/content", nil, ""); !bytes.Equal(got.Body.Bytes(), content) {
		t.Error("the file came back different")
	}
	if !slices.Contains(c.folderPaths(), "/album/empty") {
		t.Error("the empty folder did not come back")
	}

	if w, _ := c.json(http.MethodPost, "/api/snapshots/delete", map[string]any{"target": target, "name": name}); w.Code != http.StatusOK {
		t.Fatalf("delete: %d", w.Code)
	}
	_, stored = c.json(http.MethodGet, "/api/snapshots/stored?account="+account, nil)
	if sets, _ := stored["snapshots"].([]any); len(sets) != 0 {
		t.Errorf("after the delete the account still lists %v", sets)
	}
}

// A machine keeps a snapshot as part files in a folder of its own.
func TestSnapshotToAMachineOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.upload("a.txt", "/", []byte("hello"))
	req, root := remoteFixture(t, "vps")
	machine := c.connectRemote(req)["id"].(string)

	w, _ := c.json(http.MethodPost, "/api/snapshots", map[string]any{
		"path": "/", "target": map[string]any{"machine": machine, "dir": "snaps"}, "password": "pw",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	status := c.awaitSnapshot()
	report, _ := status["result"].(map[string]any)
	if report == nil {
		t.Fatalf("status = %v", status)
	}
	if _, err := os.Stat(filepath.Join(root, "snaps", report["name"].(string)+".sandsnap")); err != nil {
		t.Errorf("the snapshot is not on the machine: %v", err)
	}

	w, stored := c.json(http.MethodGet, "/api/snapshots/stored?machine="+machine+"&dir=snaps", nil)
	if sets, _ := stored["snapshots"].([]any); w.Code != http.StatusOK || len(sets) != 1 {
		t.Fatalf("stored: %d %v", w.Code, stored)
	}
}

// What can be refused from the request and the index is refused in the
// answer, before any job starts.
func TestSnapshotRefusalsOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/hollow"})
	c.upload("a.txt", "/", []byte("hello"))
	target := map[string]any{"account": c.providerIDs()[0]}

	for name, probe := range map[string]struct {
		body map[string]any
		want int
	}{
		"no password":    {map[string]any{"path": "/", "target": target}, http.StatusBadRequest},
		"no such folder": {map[string]any{"path": "/missing", "target": target, "password": "pw"}, http.StatusNotFound},
		"an empty one":   {map[string]any{"path": "/hollow", "target": target, "password": "pw"}, http.StatusBadRequest},
		"a negative size": {map[string]any{"path": "/", "target": target, "password": "pw", "part_size": -5},
			http.StatusBadRequest},
	} {
		if w, body := c.json(http.MethodPost, "/api/snapshots", probe.body); w.Code != probe.want {
			t.Errorf("%s: %d %v, want %d", name, w.Code, body, probe.want)
		}
	}
	if status := c.awaitSnapshot(); status["id"] != nil {
		t.Errorf("a refused request started a job: %v", status)
	}

	if w, _ := c.json(http.MethodPost, "/api/snapshots/restore", map[string]any{"path": "/", "target": target}); w.Code != http.StatusBadRequest {
		t.Errorf("a restore naming no snapshot: %d", w.Code)
	}
	if w, _ := c.json(http.MethodGet, "/api/snapshots/stored", nil); w.Code != http.StatusBadRequest {
		t.Errorf("a listing of nowhere: %d", w.Code)
	}
	if w, _ := c.json(http.MethodGet, "/api/snapshots/stored?account=nope", nil); w.Code == http.StatusOK {
		t.Errorf("a listing of an account that is not connected: %d", w.Code)
	}

	// A job that fails says so in its status rather than vanishing: a part
	// size no cloud takes is only known once the place has been opened.
	w, _ := c.json(http.MethodPost, "/api/snapshots", map[string]any{
		"path": "/", "target": target, "password": "pw", "part_size": 1 << 40,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d", w.Code)
	}
	if status := c.awaitSnapshot(); status["error"] == nil || status["result"] != nil {
		t.Errorf("status = %v", status)
	}
}

// Without a session there is no way to a snapshot or to what is stored.
func TestSnapshotEndpointsNeedASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.cookies = nil

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/snapshots"},
		{http.MethodGet, "/api/snapshots/plan?path=/"},
		{http.MethodGet, "/api/snapshots/stored?account=x"},
		{http.MethodPost, "/api/snapshots"},
		{http.MethodPost, "/api/snapshots/restore"},
		{http.MethodPost, "/api/snapshots/inspect"},
		{http.MethodPost, "/api/snapshots/delete"},
		{http.MethodPost, "/api/snapshots/stop"},
	} {
		if w, body := c.json(probe.method, probe.path, map[string]any{}); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (%v)", probe.method, probe.path, w.Code, body)
		}
	}
}
