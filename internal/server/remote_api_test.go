package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
