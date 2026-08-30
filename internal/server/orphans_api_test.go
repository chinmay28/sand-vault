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

	// The window the dialog polls while a sweep runs. Nothing is running, so
	// it must say so — and a dry run must not open it either: a browser asking
	// what a sweep would do must not make one that is running look otherwise.
	if w, body := c.json(http.MethodGet, "/api/vault/orphans/erasing", nil); w.Code != http.StatusOK || body["running"] != false {
		t.Errorf("erasing before any sweep: %d %v, want running=false", w.Code, body)
	}

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
	// And the window comes down with the request it was counting beside.
	if w, body := c.json(http.MethodGet, "/api/vault/orphans/erasing", nil); w.Code != http.StatusOK || body["running"] != false {
		t.Errorf("erasing after the sweep finished: %d %v, want running=false", w.Code, body)
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

	w, _ := c.json(http.MethodGet, "/api/vault/orphans/erasing", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/vault/orphans/erasing without a session: %d, want 401", w.Code)
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

// ---------------------------------------------------------------------------
// Shards a disconnect mislaid
// ---------------------------------------------------------------------------

// degradeByDisconnect disconnects the account holding one of a file's shards
// and connects the same folder back under a new name, which is what leaves the
// index a record short while the object stays put.
func (c *testClient) degradeByDisconnect(file map[string]any, roots []string, name string) string {
	c.t.Helper()

	shards, _ := file["shards"].([]any)
	if len(shards) == 0 {
		c.t.Fatalf("the file records no shards: %v", file)
	}
	away := shards[0].(map[string]any)["provider_id"].(string)

	root := ""
	_, body := c.json(http.MethodGet, "/api/providers", nil)
	for _, raw := range body["providers"].([]any) {
		p := raw.(map[string]any)
		if p["id"] != away {
			continue
		}
		if options, _ := p["options"].(map[string]any); options != nil {
			root, _ = options["path"].(string)
		}
	}
	if root == "" {
		c.t.Fatalf("could not find the folder behind account %s", away)
	}

	w := c.do(http.MethodDelete, "/api/providers/"+away+"?force=true", nil, "")
	if w.Code != http.StatusOK {
		c.t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}
	w, added := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind":    "local",
		"name":    name,
		"options": map[string]string{"path": root},
	})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("reconnect: %d %v", w.Code, added)
	}
	return added["provider"].(map[string]any)["id"].(string)
}

// degraded is how many files the vault reports as short of a shard.
func (c *testClient) degraded() int {
	c.t.Helper()

	_, body := c.json(http.MethodGet, "/api/vault", nil)
	stats, _ := body["stats"].(map[string]any)
	if stats == nil {
		c.t.Fatalf("no stats in the vault status: %v", body)
	}
	count, _ := stats["degraded"].(float64)
	return int(count)
}

func TestOrphanScanReportsAShardTheDisconnectMislaid(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)

	file := c.upload("important.txt", "/", []byte("the file that lost a spare"))
	back := c.degradeByDisconnect(file, roots, "drive-personal-again")

	if c.degraded() != 1 {
		t.Fatalf("the vault reports %d degraded file(s), want 1", c.degraded())
	}

	body := c.orphanScan()
	// Emphatically not an orphan: the file is still in the tree, so the sweep
	// must never see it.
	if found, _ := body["found"].(bool); found {
		t.Errorf("a mislaid shard was reported as abandoned: %v", body["items"])
	}
	if deletable, _ := body["deletable"].(float64); deletable != 0 {
		t.Errorf("%v object(s) of a stored file were offered for deletion", deletable)
	}

	strays, _ := body["strays"].([]any)
	if len(strays) != 1 {
		t.Fatalf("found %d mislaid shard(s), want 1: %v", len(strays), strays)
	}
	stray := strays[0].(map[string]any)
	if stray["provider_id"] != back {
		t.Errorf("blamed account %v, want the reconnected %s", stray["provider_id"], back)
	}
	if stray["file_id"] != file["id"] {
		t.Errorf("pointed at file %v, want %v", stray["file_id"], file["id"])
	}
	if stray["path"] != "/important.txt" {
		t.Errorf("named the file %v", stray["path"])
	}
	if ok, _ := stray["reattachable"].(bool); !ok {
		t.Errorf("the shard is not offered for reattachment: %v", stray["reason"])
	}
	if have, _ := stray["have"].(float64); have != 2 {
		t.Errorf("said the file has %v shards, want 2", stray["have"])
	}
	if n, _ := body["reattachable"].(float64); n != 1 {
		t.Errorf("the totals say %v reattachable, want 1", body["reattachable"])
	}
}

func TestOrphanReattachRestoresTheSpareWithoutMovingAByte(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("the vault password", 3)

	file := c.upload("important.txt", "/", []byte("the file that lost a spare"))
	c.degradeByDisconnect(file, roots, "drive-personal-again")

	before := 0
	for _, root := range roots {
		before += sandFilesIn(t, root)
	}

	w, preview := c.json(http.MethodPost, "/api/vault/orphans/reattach", map[string]any{"dry_run": true})
	if w.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if shards, _ := preview["shards"].(float64); shards != 1 {
		t.Fatalf("the dry run promised %v shard(s), want 1", preview["shards"])
	}
	if c.degraded() != 1 {
		t.Fatal("a dry run changed the index")
	}

	w, report := c.json(http.MethodPost, "/api/vault/orphans/reattach", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reattach: %d %s", w.Code, w.Body.String())
	}
	if report["shards"] != preview["shards"] || report["bytes"] != preview["bytes"] {
		t.Fatalf("the repair did not do what the dry run said: %v vs %v", report, preview)
	}
	restored, _ := report["restored"].([]any)
	if len(restored) != 1 || restored[0] != "/important.txt" {
		t.Errorf("the file was not reported as restored: %v", report["restored"])
	}

	// The claim that matters: the index changed and the clouds did not.
	if c.degraded() != 0 {
		t.Errorf("the vault still reports %d degraded file(s)", c.degraded())
	}
	after := 0
	for _, root := range roots {
		after += sandFilesIn(t, root)
	}
	if after != before {
		t.Errorf("the repair wrote to the accounts: %d object(s), was %d", after, before)
	}

	// Three shards recorded again, and the file still reads.
	_, meta := c.json(http.MethodGet, "/api/files/"+file["id"].(string), nil)
	stored, _ := meta["file"].(map[string]any)
	shards, _ := stored["shards"].([]any)
	if len(shards) != 3 {
		t.Errorf("the file records %d shard(s), want 3", len(shards))
	}
	w = c.do(http.MethodGet, "/api/files/"+file["id"].(string)+"/content", nil, "")
	if w.Code != http.StatusOK || w.Body.String() != "the file that lost a spare" {
		t.Fatalf("reading it back: %d %s", w.Code, w.Body.String())
	}

	// Nothing left mislaid, and a second run is a no-op.
	if strays, _ := c.orphanScan()["strays"].([]any); len(strays) != 0 {
		t.Errorf("%d shard(s) still reported as mislaid", len(strays))
	}
	_, again := c.json(http.MethodPost, "/api/vault/orphans/reattach", nil)
	if shards, _ := again["shards"].(float64); shards != 0 {
		t.Errorf("the repair ran twice over the same shard: %v", again)
	}
}

func TestOrphanReattachNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.cookies = nil

	w, _ := c.json(http.MethodPost, "/api/vault/orphans/reattach", map[string]any{"dry_run": true})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/vault/orphans/reattach without a session: %d, want 401", w.Code)
	}
}
