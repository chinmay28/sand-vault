package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
)

// remoteFixture starts a server over a tree and returns what the connect
// endpoint needs, plus the directory on disk the source is scoped to.
func remoteFixture(t *testing.T, name string) (map[string]any, string) {
	t.Helper()

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	host, port := server.HostPort(t)

	root := filepath.Join(disk, "share")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("making the root: %v", err)
	}

	return map[string]any{
		"name":        name,
		"host":        host,
		"port":        port,
		"user":        "sand",
		"private_key": key,
		"root":        filepath.ToSlash(root),
	}, root
}

func seedRemote(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// connectRemote adds a source through the endpoint the browser uses.
func (c *testClient) connectRemote(req map[string]any) map[string]any {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/remote", req)
	if w.Code != http.StatusCreated {
		c.t.Fatalf("POST /api/remote: %d %s", w.Code, w.Body.String())
	}
	source, ok := body["source"].(map[string]any)
	if !ok {
		c.t.Fatalf("no source in %v", body)
	}
	return source
}

func TestRemoteConnectAndList(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, _ := remoteFixture(t, "vps")

	source := c.connectRemote(req)
	if source["name"] != "vps" {
		t.Errorf("name = %v, want vps", source["name"])
	}
	// The connection that stored it is the one that learned the fingerprint.
	fingerprint, _ := source["host_key"].(string)
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Errorf("host_key = %q, want a learned fingerprint", fingerprint)
	}
	// A private key must never come back out of the vault.
	if key, _ := source["private_key"].(string); key == req["private_key"] {
		t.Error("the API handed the private key back")
	}

	_, body := c.json(http.MethodGet, "/api/remote", nil)
	sources, _ := body["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("listing has %d sources, want 1: %v", len(sources), body)
	}
	listed := sources[0].(map[string]any)
	if listed["private_key"] == req["private_key"] {
		t.Error("the listing hands out private keys")
	}
}

func TestRemoteConnectRefusesWhatItCannotReach(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	req, _ := remoteFixture(t, "vps")
	req["root"] = "/nowhere/at/all"
	w, _ := c.json(http.MethodPost, "/api/remote", req)
	if w.Code == http.StatusCreated {
		t.Error("stored a source whose folder does not exist")
	}
}

func TestRemoteBrowse(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "films", "one.mp4"), "a film")
	seedRemote(t, filepath.Join(root, "notes.txt"), "hello")

	id := c.connectRemote(req)["id"].(string)

	w, body := c.json(http.MethodGet, "/api/remote/"+id+"/files", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("browse: %d %s", w.Code, w.Body.String())
	}
	if atRoot, _ := body["at_root"].(bool); !atRoot {
		t.Error("the first listing does not say it is at the root")
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("root holds %d entries, want 2: %v", len(entries), body)
	}
	first := entries[0].(map[string]any)
	if first["name"] != "films" || first["dir"] != true {
		t.Errorf("folders do not come first: %v", entries)
	}

	// Descending, and the path that comes back is relative to the source's
	// folder rather than an absolute path on somebody's server.
	_, body = c.json(http.MethodGet, "/api/remote/"+id+"/files?path=films", nil)
	if body["path"] != "films" {
		t.Errorf("path = %v, want films", body["path"])
	}
	if strings.HasPrefix(body["path"].(string), "/") {
		t.Error("the API handed out an absolute server path")
	}
}

func TestRemoteBrowseCannotLeaveTheSourceFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(filepath.Dir(root), "secret.txt"), "not yours")

	id := c.connectRemote(req)["id"].(string)

	for _, path := range []string{"..", "../", "..%2F..", "a/../.."} {
		w, _ := c.json(http.MethodGet, "/api/remote/"+id+"/files?path="+path, nil)
		if w.Code == http.StatusOK {
			t.Errorf("browsing %q was allowed", path)
		}
	}
}

func TestRemoteImport(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "films", "one.mp4"), "a film")
	seedRemote(t, filepath.Join(root, "films", "two.mp4"), "another film")

	id := c.connectRemote(req)["id"].(string)

	w, body := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"paths": []string{"films"},
		"dest":  "/media",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "imported"); got != 2 {
		t.Fatalf("imported %d, want 2: %v", got, body)
	}

	// And they are in the vault, in the shape they had on the source.
	_, body = c.json(http.MethodGet, "/api/files?path=/media/films", nil)
	files, _ := body["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("vault holds %d files under /media/films: %v", len(files), body)
	}
}

// Re-running an import is how you resume it, and the API says so in its own
// numbers rather than leaving the browser to work it out.
func TestRemoteImportSkipsWhatIsAlreadyThere(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "a.txt"), "aaa")

	id := c.connectRemote(req)["id"].(string)
	body := map[string]any{"paths": []string{"a.txt"}, "dest": "/"}

	w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/import", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("first import: %d", w.Code)
	}

	w, again := c.json(http.MethodPost, "/api/remote/"+id+"/import", body)
	// Nothing arrived because nothing needed to, which is a success rather
	// than a creation.
	if w.Code != http.StatusOK {
		t.Errorf("second import: %d %s, want 200", w.Code, w.Body.String())
	}
	if got := number(t, again, "skipped"); got != 1 {
		t.Errorf("skipped %d, want 1: %v", got, again)
	}
	if got := number(t, again, "imported"); got != 0 {
		t.Errorf("imported %d on a repeat run, want 0", got)
	}
}

func TestRemoteImportRefusesToLeaveTheSourceFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(filepath.Dir(root), "secret.txt"), "not yours")

	id := c.connectRemote(req)["id"].(string)

	w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"paths": []string{"../secret.txt"},
		"dest":  "/",
	})
	if w.Code == http.StatusCreated {
		t.Error("importing a file outside the source's folder was allowed")
	}
}

func TestRemoteUpdateAndRemove(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, _ := remoteFixture(t, "vps")

	source := c.connectRemote(req)
	id := source["id"].(string)

	// What an edit form sends back: the redacted secrets as they were given,
	// and no fingerprint at all.
	edit := map[string]any{
		"name":        "the vps",
		"host":        source["host"],
		"port":        source["port"],
		"user":        source["user"],
		"root":        source["root"],
		"private_key": source["private_key"],
	}
	w, body := c.json(http.MethodPatch, "/api/remote/"+id, edit)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
	updated := body["source"].(map[string]any)
	if updated["name"] != "the vps" {
		t.Errorf("name = %v, want %q", updated["name"], "the vps")
	}
	// Renaming must not quietly stop pinning the host key.
	if updated["host_key"] != source["host_key"] {
		t.Errorf("renaming changed the pin from %v to %v", source["host_key"], updated["host_key"])
	}

	w, _ = c.json(http.MethodDelete, "/api/remote/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	_, body = c.json(http.MethodGet, "/api/remote", nil)
	if sources, _ := body["sources"].([]any); len(sources) != 0 {
		t.Errorf("%d sources left after removing the only one", len(sources))
	}
}

// Everything here is behind the session, like the rest of the API — and it
// matters more here than most, since these endpoints hold an SSH key.
func TestRemoteEndpointsNeedASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, _ := remoteFixture(t, "vps")
	id := c.connectRemote(req)["id"].(string)

	c.cookies = nil

	for _, call := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/remote"},
		{http.MethodPost, "/api/remote"},
		{http.MethodPatch, "/api/remote/" + id},
		{http.MethodDelete, "/api/remote/" + id},
		{http.MethodGet, "/api/remote/" + id + "/files"},
		{http.MethodPost, "/api/remote/" + id + "/import"},
	} {
		w, _ := c.json(call.method, call.path, map[string]any{})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: %d, want 401", call.method, call.path, w.Code)
		}
	}
}

// What an import is doing while it does it. Nothing running is the ordinary
// answer rather than an error: finished, failed and cancelled are all the same
// thing to a bar that should stop being drawn.
func TestRemoteImportProgressIsEmptyWhenNothingRuns(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "a.txt"), "aaa")

	id := c.connectRemote(req)["id"].(string)

	w, body := c.json(http.MethodGet, "/api/remote/"+id+"/import", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("progress before any import: %d %s", w.Code, w.Body.String())
	}
	if imports, _ := body["imports"].([]any); len(imports) != 0 {
		t.Errorf("listed %d imports with none running: %v", len(imports), body)
	}

	// And after one has run and returned, which is the case a poll one tick
	// behind the request will hit.
	if w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"paths": []string{"a.txt"}, "dest": "/",
	}); w.Code != http.StatusCreated {
		t.Fatalf("import: %d", w.Code)
	}
	_, body = c.json(http.MethodGet, "/api/remote/"+id+"/import", nil)
	if imports, _ := body["imports"].([]any); len(imports) != 0 {
		t.Errorf("a finished import is still listed: %v", body)
	}
}

// A dialog polling a machine that has since been forgotten is told so, rather
// than left watching an empty list forever.
func TestRemoteImportProgressRefusesAnUnknownSource(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, _ := c.json(http.MethodGet, "/api/remote/nosuchsource/import", nil)
	if w.Code == http.StatusOK {
		t.Errorf("progress for an unknown source answered %d", w.Code)
	}
}

// The other half of the same question: an import that *is* running says what it
// is doing, on the endpoint the dialog polls.
//
// A file big enough to still be moving while the poll goes out — the wiring
// under test is that the handler hands the vault somewhere to report to, and a
// file that arrives instantly reports nothing to catch.
func TestRemoteImportProgressWhileItRuns(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "big.bin"), strings.Repeat("sand", 8<<20))

	id := c.connectRemote(req)["id"].(string)

	done := make(chan int)
	go func() {
		w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
			"paths": []string{"big.bin"}, "dest": "/",
		})
		done <- w.Code
	}()

	var seen map[string]any
	for seen == nil {
		select {
		case code := <-done:
			if code != http.StatusCreated {
				t.Fatalf("import: %d", code)
			}
			// It beat every poll. Nothing is wrong, and there is nothing left
			// to look at — the import is over and correctly no longer listed.
			t.Skip("the import finished before a poll caught it")
		default:
		}
		_, body := c.json(http.MethodGet, "/api/remote/"+id+"/import", nil)
		if imports, _ := body["imports"].([]any); len(imports) > 0 {
			seen, _ = imports[0].(map[string]any)
		}
	}

	if seen["dest"] != "/" {
		t.Errorf("running import landing at %v, want /", seen["dest"])
	}
	at, _ := seen["at"].(map[string]any)
	if at == nil {
		t.Fatalf("a running import reported nothing about the file: %v", seen)
	}
	// The plan is walked before a byte moves, so a file may be listed with no
	// progress against it yet — but it is this file, and it is one of one.
	if name, _ := at["name"].(string); name != "" && name != "big.bin" {
		t.Errorf("working on %q, want big.bin", name)
	}
	if code := <-done; code != http.StatusCreated {
		t.Errorf("import finished with %d", code)
	}
}

// An import can be asked to keep going without a page in front of it. The
// request answers at once; the transfer carries on behind it.
func TestRemoteImportDetaches(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "films", "one.mp4"), "a film")
	seedRemote(t, filepath.Join(root, "films", "two.mp4"), "another film")

	id := c.connectRemote(req)["id"].(string)

	w, body := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"paths": []string{"films"}, "dest": "/media", "detach": true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached import: %d %s, want 202", w.Code, w.Body.String())
	}
	run, _ := body["run"].(map[string]any)
	if run == nil || run["id"] == "" {
		t.Fatalf("202 carried no run to watch: %v", body)
	}
	if detached, _ := run["detached"].(bool); !detached {
		t.Errorf("the run does not say it is detached: %v", run)
	}

	// The answer came back before the files did, so the result is not in it —
	// it is on the endpoint the dialog polls, once the import gets there.
	finished := waitForImport(t, c, id, run["id"].(string))
	if errText, _ := finished["error"].(string); errText != "" {
		t.Fatalf("the detached import failed: %s", errText)
	}
	summary, _ := finished["summary"].(map[string]any)
	if summary == nil {
		t.Fatalf("a finished detached import kept no summary: %v", finished)
	}
	if got := number(t, summary, "imported"); got != 2 {
		t.Errorf("imported %v, want 2: %v", got, summary)
	}

	// And the files really are in the vault, in the shape they had.
	_, listing := c.json(http.MethodGet, "/api/files?path=/media/films", nil)
	if files, _ := listing["files"].([]any); len(files) != 2 {
		t.Errorf("vault holds %d files under /media/films: %v", len(files), listing)
	}

	// The result waits to be read and is dismissed on purpose, since nothing
	// else is going to carry it away.
	w, after := c.json(http.MethodDelete, "/api/remote/"+id+"/import/"+run["id"].(string), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dismissing the result: %d %s", w.Code, w.Body.String())
	}
	if imports, _ := after["imports"].([]any); len(imports) != 0 {
		t.Errorf("a dismissed result is still listed: %v", after)
	}
}

// Stopping one is the same request as dismissing one, and what it had already
// brought in stays — which is what the next run will skip.
func TestRemoteImportStopKeepsWhatArrived(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "big.bin"), strings.Repeat("sand", 8<<20))

	id := c.connectRemote(req)["id"].(string)

	_, body := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"paths": []string{"big.bin"}, "dest": "/", "detach": true,
	})
	run, _ := body["run"].(map[string]any)
	if run == nil {
		t.Fatalf("detached import carried no run: %v", body)
	}

	w, _ := c.json(http.MethodDelete, "/api/remote/"+id+"/import/"+run["id"].(string), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("stopping the import: %d %s", w.Code, w.Body.String())
	}

	stopped := waitForImport(t, c, id, run["id"].(string))
	// It may have finished before the stop landed — the file is not large
	// enough to guarantee otherwise on a fast machine — but it must not be
	// reported as a failure either way.
	if errText, _ := stopped["error"].(string); errText != "" {
		t.Errorf("a stopped import was recorded as a failure: %s", errText)
	}
}

// Stopping something that is not running says so rather than pretending.
func TestRemoteImportStopUnknownRun(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, _ := remoteFixture(t, "vps")
	id := c.connectRemote(req)["id"].(string)

	w, _ := c.json(http.MethodDelete, "/api/remote/"+id+"/import/nosuchrun", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("stopping an unknown run answered %d, want 404", w.Code)
	}
}

// A detached import against a machine that is not there is refused on the
// request that asked for it, rather than becoming a run that appears and
// immediately fails.
func TestRemoteImportDetachRefusesAnUnknownSource(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, _ := c.json(http.MethodPost, "/api/remote/nosuchsource/import", map[string]any{
		"paths": []string{"a.txt"}, "dest": "/", "detach": true,
	})
	if w.Code == http.StatusAccepted {
		t.Error("a detached import was accepted for a source that does not exist")
	}
}

// waitForImport polls until the named run says it is done, and hands back what
// it said. The endpoint is the same one the dialog watches.
func waitForImport(t *testing.T, c *testClient, source, run string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, body := c.json(http.MethodGet, "/api/remote/"+source+"/import", nil)
		imports, _ := body["imports"].([]any)
		for _, raw := range imports {
			entry, _ := raw.(map[string]any)
			if entry == nil || entry["id"] != run {
				continue
			}
			if done, _ := entry["done"].(bool); done {
				return entry
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("import %s never finished", run)
	return nil
}

// The whole point of detaching: the request that started it goes away — the tab
// is closed, the laptop lid comes down — and the transfer carries on.
//
// The request is made with a context of its own and that context is cancelled
// the moment the 202 comes back, which is what a closed page does to a request
// in flight. A foreground import dies there; a detached one has to not.
func TestDetachedImportSurvivesTheRequestGoingAway(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "one.mp4"), strings.Repeat("film", 1<<20))

	id := c.connectRemote(req)["id"].(string)

	body, err := json.Marshal(map[string]any{
		"paths": []string{"one.mp4"}, "dest": "/", "detach": true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/remote/"+id+"/import", bytes.NewReader(body)).WithContext(ctx)
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.origin)
	for _, cookie := range c.cookies {
		request.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, request)
	// The page is gone from here on.
	cancel()

	if w.Code != http.StatusAccepted {
		t.Fatalf("detached import: %d %s", w.Code, w.Body.String())
	}
	var answer struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}

	finished := waitForImport(t, c, id, answer.Run.ID)
	if errText, _ := finished["error"].(string); errText != "" {
		t.Fatalf("the import died with its request: %s", errText)
	}
	if cancelled, _ := finished["cancelled"].(bool); cancelled {
		t.Error("the import was recorded as cancelled when only its request went away")
	}

	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	files, _ := listing["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("the vault holds %d files, want the one that was imported: %v", len(files), listing)
	}
}
