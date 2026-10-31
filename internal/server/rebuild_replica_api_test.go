package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// emptyFolder removes everything under one account's folder, which is a
// cloud that has lost what it held.
func emptyFolder(t *testing.T, root string) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.RemoveAll(name); err != nil {
			t.Fatal(err)
		}
	}
}

// relock locks the vault and unlocks it again, which is the one door to
// dropping the decrypted chunks the vault keeps in memory: after it, a read
// is answered by the clouds and nothing else.
func (c *testClient) relock() {
	c.t.Helper()
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		c.t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/unlock", map[string]any{"password": "pw"}); w.Code != http.StatusOK {
		c.t.Fatalf("unlock: %d %s", w.Code, w.Body.String())
	}
}

// A file whose parts are gone past reading is put back from its replica onto
// the clouds somebody picks — priced first, then run on the machine like any
// other move, and read back from its parts afterwards.
func TestRebuildFromReplicaOverHTTP(t *testing.T) {
	c, roots, ids := replicationSetup(t)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos"})
	payload := []byte(strings.Repeat("a photo kept twice ", 400))
	result := c.uploadTo("hike.jpg", "/photos", payload, ids)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	uploaded := result["file"].(map[string]any)
	fileID := uploaded["id"].(string)

	if w, status := c.json(http.MethodPost, "/api/replicate", map[string]any{"path": "/photos", "enabled": true}); w.Code != http.StatusOK {
		t.Fatalf("flagging /photos = %d %v", w.Code, status)
	}
	v, _ := c.server.Vault()
	if step, err := c.server.replicateStep(v, time.Now()); err != nil || !step.Done {
		t.Fatalf("step = %+v, %v; want the photo replicated", step, err)
	}

	// Two of the three parts folders go dark. The parts cannot answer; the
	// replica, in folders of its own, can.
	emptyFolder(t, roots[0])
	emptyFolder(t, roots[1])
	// Locking drops the chunk cache, so what the clouds hold is what answers.
	c.relock()
	// A read that fails partway has sent its headers, so the body is what
	// says whether the parts answered.
	if w := c.do(http.MethodGet, "/api/files/"+fileID+"/content", nil, ""); w.Code == http.StatusOK && w.Body.String() == string(payload) {
		t.Fatal("the parts still read with two of three accounts emptied")
	}

	// "either" is a way to read, not a copy to trust.
	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"path": "/photos", "accounts": ids, "source": "either", "preview": true,
	})
	if w.Code != http.StatusBadRequest || body["code"] != "BAD_SOURCE" {
		t.Errorf("source=either = %d %v, want 400 BAD_SOURCE", w.Code, body)
	}

	// The price, out of the index alone.
	w, plan := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"path": "/photos", "accounts": ids, "source": "replica", "preview": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %v", w.Code, plan)
	}
	if plan["source"] != "replica" || number(t, plan, "recoded") != 1 || number(t, plan, "recode_bytes") <= len(payload) {
		t.Errorf("plan = %v, want one file rebuilt from the replica at more than its size", plan)
	}
	if plan["folder"] != true {
		t.Errorf("plan.folder = %v, want a folder", plan["folder"])
	}

	// The rebuild itself, handed to the machine.
	w, body = c.json(http.MethodPost, "/api/relocate", map[string]any{
		"path": "/photos", "accounts": ids, "source": "replica", "detach": true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached rebuild: %d %s", w.Code, w.Body.String())
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
	if report == nil || report["source"] != "replica" || number(t, report, "recoded") != 1 || number(t, report, "failed") != 0 {
		t.Fatalf("report = %v, want one file rebuilt from the replica", report)
	}

	// The file reads from its parts again, and the replica is still there.
	c.relock()
	if w := c.do(http.MethodGet, "/api/files/"+fileID+"/content", nil, ""); w.Code != http.StatusOK || w.Body.String() != string(payload) {
		t.Errorf("content from the parts after the rebuild: %d, %d bytes", w.Code, w.Body.Len())
	}
	_, listing := c.json(http.MethodGet, "/api/files?path=/photos", nil)
	file := listing["files"].([]any)[0].(map[string]any)
	if file["archive_id"] == uploaded["archive_id"] {
		t.Error("the parts keep the archive ID they had; a rebuild writes a fresh set")
	}
	if replica, ok := file["replica"].(map[string]any); !ok || len(replica["shards"].([]any)) != 3 {
		t.Errorf("file.replica = %v after the rebuild, want it untouched", file["replica"])
	}
	for _, root := range roots {
		if n := replicaObjectsIn(t, root+"-replica"); n == 0 {
			t.Errorf("%s-replica holds no replica objects after the rebuild", root)
		}
	}
}
