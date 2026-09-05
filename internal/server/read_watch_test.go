package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The window onto a file being opened.

func TestReadTokenIsSomethingABrowserMinted(t *testing.T) {
	for _, ok := range []string{"abcdefgh", "a1B2c3D4e5F6", strings.Repeat("x", 64), "with-dash_and"} {
		if !validReadToken(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("x", 65), "has space", "semi;colon", "../etc"} {
		if validReadToken(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func shard(part int, id, name string) vault.Shard {
	return vault.Shard{Part: part, ProviderID: id, ProviderName: name, ProviderKind: "local"}
}

func TestReadWatchFollowsARead(t *testing.T) {
	rw := newReadWatch()
	ticket := rw.open("token-one")

	got, ok := rw.get("token-one")
	if !ok || got.Phase != readOpening {
		t.Fatalf("a freshly opened read is %+v, want opening", got)
	}
	if _, ok := rw.get("nobody"); ok {
		t.Error("an unknown token answered")
	}

	ticket.opened(5000)
	asked := []vault.Shard{shard(1, "p1", "Google Drive"), shard(2, "p2", "Dropbox"), shard(3, "p3", "Box")}
	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadChunkWaiting, Chunk: 0, Chunks: 2, Needed: 2})
	got, _ = rw.get("token-one")
	if got.Phase != readGathering || got.Chunks != 2 || got.Needed != 2 || got.Size != 5000 {
		t.Errorf("waiting on a chunk: %+v", got)
	}

	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadChunkStarted, Chunk: 0, Chunks: 2, Needed: 2, Asked: asked})
	got, _ = rw.get("token-one")
	if len(got.Accounts) != 3 {
		t.Fatalf("asked %d accounts, want 3: %+v", len(got.Accounts), got.Accounts)
	}
	for _, a := range got.Accounts {
		if a.State != "waiting" || a.Name == "" {
			t.Errorf("an account asked for a part is %+v, want waiting and named", a)
		}
	}

	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadShardArrived, Chunk: 0, Shard: asked[1], Took: 40 * time.Millisecond})
	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadShardFailed, Chunk: 0, Shard: asked[0], Took: time.Second, Err: errors.New("403 from drive")})
	got, _ = rw.get("token-one")
	if got.Have != 1 {
		t.Errorf("have = %d after one part arrived", got.Have)
	}
	byID := map[string]readAccount{}
	for _, a := range got.Accounts {
		byID[a.ProviderID] = a
	}
	if byID["p2"].State != "arrived" || byID["p2"].TookMS != 40 {
		t.Errorf("Dropbox answered and is %+v", byID["p2"])
	}
	if byID["p1"].State != "failed" || byID["p1"].Error != "403 from drive" {
		t.Errorf("Google Drive failed and is %+v", byID["p1"])
	}
	if byID["p3"].State != "waiting" {
		t.Errorf("Box has not answered and is %+v", byID["p3"])
	}

	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadChunkDecrypting, Chunk: 0})
	if got, _ = rw.get("token-one"); got.Phase != readDecrypting {
		t.Errorf("decrypting: phase %q", got.Phase)
	}
	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadChunkReady, Chunk: 0})
	ticket.sent(1024)
	ticket.sent(0)
	if got, _ = rw.get("token-one"); got.Phase != readSending || got.Sent != 1024 {
		t.Errorf("after the first write: %+v", got)
	}

	// The second chunk starts the count of parts again.
	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadChunkWaiting, Chunk: 1, Chunks: 2, Needed: 2})
	if got, _ = rw.get("token-one"); got.Chunk != 1 || got.Have != 0 || len(got.Accounts) != 0 {
		t.Errorf("the second chunk carried the first one's accounts: %+v", got)
	}

	ticket.finish(nil)
	got, _ = rw.get("token-one")
	if got.Phase != readDone {
		t.Errorf("finished: phase %q", got.Phase)
	}
	// Nothing said after the end changes it.
	ticket.sent(10)
	if got, _ = rw.get("token-one"); got.Sent != 1024 || got.Phase != readDone {
		t.Errorf("a trailing write reopened the read: %+v", got)
	}
	if rw.running() != 0 {
		t.Errorf("running = %d after the read finished", rw.running())
	}
}

func TestReadWatchKeepsAFailure(t *testing.T) {
	rw := newReadWatch()
	ticket := rw.open("token-two")
	ticket.ObserveRead(vault.ReadEvent{Kind: vault.ReadChunkFailed, Chunk: 0, Err: errors.New("could not gather 2 shards")})
	ticket.finish(nil)
	got, _ := rw.get("token-two")
	if got.Phase != readFailed || got.Error != "could not gather 2 shards" {
		t.Errorf("a chunk that failed ended as %+v", got)
	}

	ticket = rw.open("token-three")
	ticket.finish(errors.New("vault is locked"))
	if got, _ = rw.get("token-three"); got.Phase != readFailed || got.Error != "vault is locked" {
		t.Errorf("a read refused at the door ended as %+v", got)
	}
}

func TestReadWatchForgetsWhatNobodyWillAskAbout(t *testing.T) {
	rw := newReadWatch()
	now := time.Date(2026, 9, 5, 9, 51, 0, 0, time.UTC)
	rw.now = func() time.Time { return now }

	rw.open("finished-x").finish(nil)
	rw.open("quiet-xxx")

	// Just inside both graces: both are still there.
	now = now.Add(readWatchTTL - time.Second)
	rw.open("another-1")
	if _, ok := rw.get("finished-x"); !ok {
		t.Error("a read finished a moment ago was forgotten")
	}

	// Past the finished one's grace, not yet past the quiet one's.
	now = now.Add(2 * time.Second)
	rw.open("another-2")
	if _, ok := rw.get("finished-x"); ok {
		t.Error("a finished read was kept past its grace")
	}
	if _, ok := rw.get("quiet-xxx"); !ok {
		t.Error("a read still running was forgotten early")
	}

	now = now.Add(readWatchStaleAfter)
	rw.open("another-3")
	if _, ok := rw.get("quiet-xxx"); ok {
		t.Error("a read that went quiet for ten minutes was kept")
	}

	// Too many at once: the oldest go first.
	for i := 0; i < readWatchLimit+5; i++ {
		now = now.Add(time.Millisecond)
		rw.open(fmt.Sprintf("flood-%08d", i))
	}
	if n := len(rw.byToken); n > readWatchLimit {
		t.Errorf("%d reads watched, cap is %d", n, readWatchLimit)
	}
	if _, ok := rw.get("flood-00000000"); ok {
		t.Error("the oldest read survived the cap")
	}
	if _, ok := rw.get(fmt.Sprintf("flood-%08d", readWatchLimit+4)); !ok {
		t.Error("the newest read was dropped by the cap")
	}
}

// Over the wire: a content request carrying a token, and the window beside it.

func readWindow(t *testing.T, c *testClient, token string) (int, map[string]any) {
	t.Helper()
	w, body := c.json(http.MethodGet, "/api/reads/watch/"+token, nil)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	read, ok := body["read"].(map[string]any)
	if !ok {
		t.Fatalf("no read in %v", body)
	}
	return w.Code, read
}

func TestReadWatchEndpointFollowsAContentRequest(t *testing.T) {
	c := newTestClient(t)
	c.setup("watch a read", 3)

	payload := []byte(strings.Repeat("watched all the way down ", 400))
	id := c.upload("watched.txt", "/", payload)["id"].(string)

	if code, _ := readWindow(t, c, "not-yet-asked"); code != http.StatusNotFound {
		t.Errorf("a token nothing has used: %d, want 404", code)
	}
	if code, _ := readWindow(t, c, "bad;token"); code != http.StatusBadRequest {
		t.Errorf("a token no browser minted: %d, want 400", code)
	}

	w := c.do(http.MethodGet, "/api/files/"+id+"/content?watch=browser-token-1", nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "watched all the way down") {
		t.Fatalf("content: %d %s", w.Code, w.Body.String()[:min(80, w.Body.Len())])
	}

	code, read := readWindow(t, c, "browser-token-1")
	if code != http.StatusOK {
		t.Fatalf("window after the read: %d", code)
	}
	if read["phase"] != "done" {
		t.Errorf("phase = %v after the response finished, want done", read["phase"])
	}
	if read["sent"].(float64) != float64(len(payload)) || read["size"].(float64) != float64(len(payload)) {
		t.Errorf("sent %v of %v, want all %d", read["sent"], read["size"], len(payload))
	}
	if read["chunks"].(float64) < 1 || read["needed"].(float64) != 2 {
		t.Errorf("chunks %v, needed %v", read["chunks"], read["needed"])
	}
	accounts := read["accounts"].([]any)
	if len(accounts) != 3 {
		t.Fatalf("asked %d accounts, want 3", len(accounts))
	}
	arrived := 0
	for _, raw := range accounts {
		a := raw.(map[string]any)
		if a["name"] == nil || a["kind"] == nil {
			t.Errorf("an account with no name in the window: %v", a)
		}
		if a["state"] == "arrived" {
			arrived++
		}
	}
	if arrived < 2 {
		t.Errorf("%d accounts arrived, need at least 2 to have rebuilt it", arrived)
	}

	// A token that could not have come from the browser is ignored on the
	// content request: the file is still served.
	w = c.do(http.MethodGet, "/api/files/"+id+"/content?watch=nope", nil, "")
	if w.Code != http.StatusOK {
		t.Errorf("content with a bad token: %d, want 200", w.Code)
	}
}

func TestReadWatchEndpointSaysWhichAccountFailed(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("watch a failing read", 3)

	id := c.upload("doomed.txt", "/", []byte("two of the three clouds went dark"))["id"].(string)
	for _, root := range roots[:2] {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("removing an account's folder: %v", err)
		}
	}

	c.do(http.MethodGet, "/api/files/"+id+"/content?watch=browser-token-2", nil, "")
	code, read := readWindow(t, c, "browser-token-2")
	if code != http.StatusOK {
		t.Fatalf("window after the read: %d", code)
	}
	if read["phase"] != "failed" || read["error"] == nil {
		t.Errorf("a read that could not rebuild its chunk ended as %v", read)
	}
	failed := 0
	for _, raw := range read["accounts"].([]any) {
		if raw.(map[string]any)["state"] == "failed" {
			failed++
		}
	}
	if failed != 2 {
		t.Errorf("%d accounts failed in the window, want the 2 that went dark", failed)
	}
}

func TestReadWatchEndpointNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("locked window", 1)
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}
	if w, _ := c.json(http.MethodGet, "/api/reads/watch/browser-token-3", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/reads/watch on a locked vault: %d, want 401", w.Code)
	}
}
