package server

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// replicaObjectsIn counts the objects in a replica folder, which a local
// account removes once it is empty — so a folder that is gone holds none.
func replicaObjectsIn(t *testing.T, root string) int {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return 0
	}
	return sandFilesIn(t, root)
}

// replicationSetup is a vault with three local accounts and a 2-of-3
// replication policy over all three, each with a replica folder beside its
// parts folder.
func replicationSetup(t *testing.T) (*testClient, []string, []string) {
	t.Helper()
	c := newTestClient(t)
	roots := c.setup("pw", 3)
	ids := c.providerIDs()

	var accounts []map[string]string
	for i, id := range ids {
		accounts = append(accounts, map[string]string{"id": id, "replica_folder": roots[i] + "-replica"})
	}
	w, body := c.json(http.MethodPut, "/api/vault/replication", map[string]any{
		"accounts": accounts, "scheme": "2-of-3", "bytes_per_day": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("set replication: %d %v", w.Code, body)
	}
	return c, roots, ids
}

func TestReplicationPolicyOverHTTP(t *testing.T) {
	c, roots, ids := replicationSetup(t)

	w, status := c.json(http.MethodGet, "/api/vault/replication", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %v", w.Code, status)
	}
	policy := status["policy"].(map[string]any)
	if policy["data_shards"] != float64(2) || policy["total_shards"] != float64(3) {
		t.Errorf("policy = %v, want 2-of-3", policy)
	}
	folders := status["folders"].(map[string]any)
	if folders[ids[0]] != roots[0]+"-replica" {
		t.Errorf("folders = %v", folders)
	}

	// A folder inside the parts folder is refused, and nothing changes.
	w, body := c.json(http.MethodPut, "/api/vault/replication", map[string]any{
		"accounts": []map[string]string{{"id": ids[0], "replica_folder": roots[0] + "/inside"}},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "inside the parts") {
		t.Errorf("a folder inside the parts folder = %d %v, want 400", w.Code, body)
	}
	_, status = c.json(http.MethodGet, "/api/vault/replication", nil)
	if status["policy"].(map[string]any)["total_shards"] != float64(3) {
		t.Errorf("the refused policy was applied: %v", status["policy"])
	}

	// One cloud is a whole copy, said as such.
	w, body = c.json(http.MethodPut, "/api/vault/replication", map[string]any{
		"accounts": []map[string]string{{"id": ids[2], "replica_folder": roots[2] + "-replica"}},
		"scheme":   "whole",
	})
	if w.Code != http.StatusOK || body["policy"].(map[string]any)["data_shards"] != float64(1) {
		t.Errorf("whole copy = %d %v", w.Code, body)
	}

	// The vault's stats carry the line the settings list draws.
	_, vaultStatus := c.json(http.MethodGet, "/api/vault", nil)
	summary := vaultStatus["stats"].(map[string]any)["replication"].(map[string]any)
	if summary["accounts"] != float64(1) || summary["scheme"] != "whole" {
		t.Errorf("stats.replication = %v", summary)
	}

	// Cleared with no accounts.
	w, body = c.json(http.MethodPut, "/api/vault/replication", map[string]any{"accounts": []any{}})
	if w.Code != http.StatusOK || body["policy"] != nil {
		t.Errorf("clear = %d %v", w.Code, body)
	}
}

func TestReplicatedFolderIsCopiedAndReadBackOverHTTP(t *testing.T) {
	c, roots, _ := replicationSetup(t)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos"})
	payload := []byte(strings.Repeat("a replicated photo ", 300))
	uploaded := c.upload("hike.jpg", "/photos", payload)
	fileID := uploaded["id"].(string)

	// Before the folder is flagged, nothing is pending and the replica
	// cannot be asked for.
	_, status := c.json(http.MethodGet, "/api/vault/replication", nil)
	if status["pending"] != float64(0) {
		t.Errorf("pending before flagging = %v", status["pending"])
	}
	if w := c.do(http.MethodGet, "/api/files/"+fileID+"/content?from=replica", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("reading a replica that does not exist = %d, want 404", w.Code)
	}
	if w := c.do(http.MethodGet, "/api/files/"+fileID+"/content?from=sideways", nil, ""); w.Code != http.StatusBadRequest {
		t.Errorf("reading from nowhere = %d, want 400", w.Code)
	}

	w, status := c.json(http.MethodPost, "/api/replicate", map[string]any{"path": "/photos", "enabled": true})
	if w.Code != http.StatusOK || status["pending"] != float64(1) {
		t.Fatalf("flagging /photos = %d %v, want one pending", w.Code, status)
	}
	_, listing := c.json(http.MethodGet, "/api/files?path=/photos", nil)
	if flag := listing["replicate"].(map[string]any); flag["enabled"] != true || flag["source"] != "/photos" {
		t.Errorf("listing.replicate = %v", flag)
	}
	_, root := c.json(http.MethodGet, "/api/files?path=/", nil)
	if root["replicated_folders"].(map[string]any)["/photos"] != true {
		t.Errorf("root listing's replicated folders = %v", root["replicated_folders"])
	}

	// One step of the worker, as the loop would take it.
	v, _ := c.server.Vault()
	step, err := c.server.replicateStep(v, time.Now())
	if err != nil || !step.Done || step.Path != "/photos/hike.jpg" {
		t.Fatalf("step = %+v, %v; want the photo replicated", step, err)
	}
	_, status = c.json(http.MethodGet, "/api/vault/replication", nil)
	if status["replicated"] != float64(1) || status["pending"] != float64(0) {
		t.Errorf("after the step: %v", status)
	}

	// The file row carries the replica, by the accounts' real IDs.
	_, listing = c.json(http.MethodGet, "/api/files?path=/photos", nil)
	file := listing["files"].([]any)[0].(map[string]any)
	replica, ok := file["replica"].(map[string]any)
	if !ok || len(replica["shards"].([]any)) != 3 {
		t.Fatalf("file.replica = %v, want three shards", file["replica"])
	}
	for _, raw := range replica["shards"].([]any) {
		if id := raw.(map[string]any)["provider_id"].(string); strings.Contains(id, "#") {
			t.Errorf("a replica shard names a suffixed account id %q", id)
		}
	}

	// And reads from it, by every door.
	for _, from := range []string{"replica", "either", "parts"} {
		w := c.do(http.MethodGet, "/api/files/"+fileID+"/content?from="+from, nil, "")
		if w.Code != http.StatusOK || w.Body.String() != string(payload) {
			t.Errorf("content from=%s: %d, %d bytes", from, w.Code, w.Body.Len())
		}
	}
	w, link := c.json(http.MethodPost, "/api/files/"+fileID+"/stream?from=replica", nil)
	if w.Code != http.StatusCreated || !strings.HasSuffix(link["url"].(string), "?from=replica") {
		t.Fatalf("stream link from the replica = %d %v", w.Code, link)
	}
	if w := c.do(http.MethodGet, link["url"].(string), nil, ""); w.Code != http.StatusOK || w.Body.String() != string(payload) {
		t.Errorf("streaming the replica: %d, %d bytes", w.Code, w.Body.Len())
	}

	// The replica lives in the replica folders and nowhere else.
	for _, root := range roots {
		if n := replicaObjectsIn(t, root+"-replica"); n == 0 {
			t.Errorf("%s-replica holds no replica objects", root)
		}
	}

	// Dropping it over HTTP erases it.
	if w, _ := c.json(http.MethodDelete, "/api/files/"+fileID+"/replica", nil); w.Code != http.StatusOK {
		t.Fatalf("drop = %d", w.Code)
	}
	for _, root := range roots {
		if n := replicaObjectsIn(t, root+"-replica"); n != 0 {
			t.Errorf("%s-replica still holds %d objects after the drop", root, n)
		}
	}
	if w := c.do(http.MethodGet, "/api/files/"+fileID+"/content?from=replica", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("reading a dropped replica = %d, want 404", w.Code)
	}
}
