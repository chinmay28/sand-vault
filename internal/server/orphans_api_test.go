package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scenario these are about, over HTTP: a cloud is disconnected, files are
// deleted while it is away, and it is connected back — as a new account, with a
// new ID, carrying parts that nothing will ever go looking for again.

// abandonPartsOn disconnects the account pointed at a folder, deletes a file
// without it, and connects the same folder back under a new name. It returns
// the new account's ID.
func (c *testClient) abandonPartsOn(root, fileID, name string) string {
	c.t.Helper()

	var away string
	_, body := c.json(http.MethodGet, "/api/providers", nil)
	for _, raw := range body["providers"].([]any) {
		p := raw.(map[string]any)
		options, _ := p["options"].(map[string]any)
		if options != nil && options["path"] == root {
			away = p["id"].(string)
		}
	}
	if away == "" {
		c.t.Fatalf("no connected account is pointed at %s", root)
	}

	w := c.do(http.MethodDelete, "/api/providers/"+away+"?force=true", nil, "")
	if w.Code != http.StatusOK {
		c.t.Fatalf("disconnect %s: %d %s", away, w.Code, w.Body.String())
	}
	if w := c.do(http.MethodDelete, "/api/files/"+fileID, nil, ""); w.Code != http.StatusOK {
		c.t.Fatalf("delete the file: %d %s", w.Code, w.Body.String())
	}

	w, added := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind":    "local",
		"name":    name,
		"options": map[string]string{"path": root},
	})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("reconnect %s: %d %v", root, w.Code, added)
	}
	return added["provider"].(map[string]any)["id"].(string)
}

// orphanScan asks what the accounts are holding that nothing points at.
func (c *testClient) orphanScan() map[string]any {
	c.t.Helper()

	w, body := c.json(http.MethodGet, "/api/vault/orphans", nil)
	if w.Code != http.StatusOK {
		c.t.Fatalf("orphan scan: %d %s", w.Code, w.Body.String())
	}
	return body
}

// sandFilesIn counts the part objects sitting in an account folder.
func sandFilesIn(t *testing.T, root string) int {
	t.Helper()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sand") && e.Name() != "manifest.sand" {
			count++
		}
	}
	return count
}

func TestOrphanScanReportsWhatAReconnectedAccountIsStillHolding(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)

	doomed := c.upload("gone.txt", "/", []byte("deleted while a cloud was away"))
	c.upload("kept.txt", "/", []byte("still very much stored"))

	// Nothing wrong yet, and the scan must not invent anything.
	if found, _ := c.orphanScan()["found"].(bool); found {
		t.Fatal("an undisturbed vault was told it has abandoned parts")
	}

	returned := c.abandonPartsOn(roots[0], doomed["id"].(string), "drive-personal-again")

	body := c.orphanScan()
	if found, _ := body["found"].(bool); !found {
		t.Fatalf("the parts left on the reconnected account were not noticed: %v", body)
	}
	if blocked, _ := body["blocked"].([]any); len(blocked) > 0 {
		t.Fatalf("the sweep was withheld from an ordinary vault: %v", blocked)
	}
	if archives, _ := body["archives"].(float64); archives != 1 {
		t.Fatalf("reported %v abandoned archive(s), want the one deleted file's: %v",
			body["archives"], body["items"])
	}
	if deletable, _ := body["deletable"].(float64); deletable == 0 {
		t.Error("nothing was offered for deletion")
	}
	if bytes, _ := body["deletable_bytes"].(float64); bytes == 0 {
		t.Error("the room it would give back was reported as nothing")
	}

	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d row(s), want 1: %v", len(items), items)
	}
	item := items[0].(map[string]any)
	if item["provider_id"] != returned {
		t.Errorf("blamed account %v, want the reconnected %s", item["provider_id"], returned)
	}
	if item["archive_id"] != doomed["archive_id"] {
		t.Errorf("blamed archive %v, want the deleted file's %v", item["archive_id"], doomed["archive_id"])
	}
	if ok, _ := item["deletable"].(bool); !ok {
		t.Errorf("the row is not offered for deletion: %v", item)
	}

	// The per-account breakdown is what the accounts drawer draws, so it has
	// to agree with the totals.
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 3 {
		t.Fatalf("reported %d account(s), want 3", len(accounts))
	}
	blamed := 0
	for _, raw := range accounts {
		account := raw.(map[string]any)
		orphans, _ := account["orphans"].(float64)
		if orphans == 0 {
			continue
		}
		blamed++
		if account["provider_id"] != returned {
			t.Errorf("%v is holding abandoned parts and should not be", account["name"])
		}
		if objects, _ := account["objects"].(float64); objects < orphans {
			t.Errorf("%v holds %v object(s) of which %v are abandoned", account["name"], objects, orphans)
		}
	}
	if blamed != 1 {
		t.Errorf("%d accounts were blamed, want 1", blamed)
	}
}

func TestOrphanSweepErasesWhatTheDryRunPromised(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)

	doomed := c.upload("gone.txt", "/", []byte("deleted while a cloud was away"))
	keeper := c.upload("kept.txt", "/", []byte("still very much stored"))
	c.abandonPartsOn(roots[0], doomed["id"].(string), "drive-personal-again")

	before := sandFilesIn(t, roots[0])

	w, preview := c.json(http.MethodPost, "/api/vault/orphans", map[string]any{"dry_run": true})
	if w.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if deleted, _ := preview["deleted"].(float64); deleted == 0 {
		t.Fatalf("the dry run promised nothing: %v", preview)
	}
	if after := sandFilesIn(t, roots[0]); after != before {
		t.Fatalf("the dry run erased %d object(s)", before-after)
	}

	w, report := c.json(http.MethodPost, "/api/vault/orphans", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	if report["deleted"] != preview["deleted"] || report["bytes"] != preview["bytes"] {
		t.Fatalf("the sweep did not do what the dry run said: %v vs %v", report, preview)
	}
	if archives, _ := report["archives"].(float64); archives != 1 {
		t.Errorf("swept %v archive(s), want 1", report["archives"])
	}
	if after := sandFilesIn(t, roots[0]); after != before-1 {
		t.Errorf("%d part object(s) left on the account, want %d", after, before-1)
	}

	// Nothing is abandoned any more, and the file that was never in question
	// still reads back.
	if found, _ := c.orphanScan()["found"].(bool); found {
		t.Error("something is still reported as abandoned after the sweep")
	}
	w = c.do(http.MethodGet, "/api/files/"+keeper["id"].(string)+"/content", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("reading the surviving file: %d %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "still very much stored" {
		t.Error("the surviving file came back changed")
	}
}

func TestOrphanSweepNamingOneArchiveLeavesTheRest(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)

	first := c.upload("one.txt", "/", []byte("the first one deleted"))
	second := c.upload("two.txt", "/", []byte("the second one deleted"))

	// Both deleted while the same cloud was away, so both leave a part behind
	// on it — and somebody may well want only one of them gone.
	var away string
	_, providers := c.json(http.MethodGet, "/api/providers", nil)
	for _, raw := range providers["providers"].([]any) {
		p := raw.(map[string]any)
		if options, _ := p["options"].(map[string]any); options != nil && options["path"] == roots[0] {
			away = p["id"].(string)
		}
	}
	if w := c.do(http.MethodDelete, "/api/providers/"+away+"?force=true", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}
	for _, file := range []map[string]any{first, second} {
		if w := c.do(http.MethodDelete, "/api/files/"+file["id"].(string), nil, ""); w.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
	}
	w, added := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind":    "local",
		"name":    "back-again",
		"options": map[string]string{"path": roots[0]},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("reconnect: %d %v", w.Code, added)
	}
	returned := added["provider"].(map[string]any)["id"].(string)

	if archives, _ := c.orphanScan()["archives"].(float64); archives != 2 {
		t.Fatalf("found %v abandoned archive(s), want 2", archives)
	}

	w, report := c.json(http.MethodPost, "/api/vault/orphans", map[string]any{
		"targets": []map[string]any{
			{"provider_id": returned, "archive_id": first["archive_id"]},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	if archives, _ := report["archives"].(float64); archives != 1 {
		t.Fatalf("swept %v archive(s), want the one named", archives)
	}

	body := c.orphanScan()
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("%d archive(s) left, want the one that was not named: %v", len(items), items)
	}
	if got := items[0].(map[string]any)["archive_id"]; got != second["archive_id"] {
		t.Errorf("the wrong archive survived: %v", got)
	}
}

func TestOrphanSweepIsRefusedOnAVaultWaitingToBeRecovered(t *testing.T) {
	roots := lostVault(t, "the password that is gone")
	c := reconnected(t, "a brand new password", roots)

	body := c.orphanScan()
	if found, _ := body["found"].(bool); !found {
		t.Fatal("the parts on the reconnected accounts were not noticed at all")
	}
	blocked, _ := body["blocked"].([]any)
	if len(blocked) == 0 {
		t.Fatal("an empty vault was offered a sweep of the parts a recovery would use")
	}
	if deletable, _ := body["deletable"].(float64); deletable != 0 {
		t.Errorf("%v object(s) marked deletable on a vault waiting to be recovered", deletable)
	}

	w, _ := c.json(http.MethodPost, "/api/vault/orphans", nil)
	if w.Code == http.StatusOK {
		t.Fatal("the sweep ran on a vault waiting to be recovered")
	}
	for i, root := range roots {
		if sandFilesIn(t, root) == 0 {
			t.Fatalf("account %d was emptied despite the refusal", i)
		}
	}
}

func TestOrphanEndpointsNeedASession(t *testing.T) {
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
		w, _ := c.json(call.method, "/api/vault/orphans", call.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/vault/orphans without a session: %d, want 401", call.method, w.Code)
		}
	}
}

func TestOrphanScanIgnoresWhatSandDidNotWrite(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)
	c.upload("ours.txt", "/", []byte("stored"))

	// Somebody else's folder, shared with SAND.
	strangers := []string{"holiday.jpg", "notes.sand", "invoice-2026-p1.sand"}
	for _, name := range strangers {
		path := filepath.Join(roots[0], name)
		if err := os.WriteFile(path, []byte("not ours"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	if found, _ := c.orphanScan()["found"].(bool); found {
		t.Fatal("files SAND never wrote were counted as abandoned")
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/orphans", nil); w.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", w.Code, w.Body.String())
	}
	for _, name := range strangers {
		if _, err := os.Stat(filepath.Join(roots[0], name)); err != nil {
			t.Errorf("the sweep erased %s, which is not ours: %v", name, err)
		}
	}
}
