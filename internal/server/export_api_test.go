package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exportFixture is a vault with three accounts, a connected machine, and two
// films stored under /media/films, ready to be sent back out.
func exportFixture(t *testing.T) (*testClient, string, string) {
	t.Helper()

	c := newTestClient(t)
	c.setup("pw", 3)
	req, root := remoteFixture(t, "vps")
	id := c.connectRemote(req)["id"].(string)

	c.mkdir("/media")
	c.mkdir("/media/films")
	c.upload("one.mp4", "/media/films", []byte("a film"))
	c.upload("two.mp4", "/media/films", []byte("another film"))
	return c, id, root
}

// mkdir makes one folder through the endpoint the browser uses.
func (c *testClient) mkdir(path string) {
	c.t.Helper()
	w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": path})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		c.t.Fatalf("mkdir %s: %d %s", path, w.Code, w.Body.String())
	}
}

func TestRemoteExport(t *testing.T) {
	c, id, root := exportFixture(t)

	w, body := c.json(http.MethodPost, "/api/remote/"+id+"/export", map[string]any{
		"paths": []string{"/media/films"},
		"dest":  "backup",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "exported"); got != 2 {
		t.Fatalf("exported %d, want 2: %v", got, body)
	}

	// On the machine, in the shape the folder had in the vault.
	got, err := os.ReadFile(filepath.Join(root, "backup", "films", "one.mp4"))
	if err != nil {
		t.Fatalf("the file did not land: %v", err)
	}
	if string(got) != "a film" {
		t.Errorf("the machine holds %q", got)
	}
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Errorf("answered with %d lines, want one per file", len(results))
	}
}

// Re-running an export is how you resume it, and the API says so in its own
// numbers: nothing moved is a 200, not a 201.
func TestRemoteExportSkipsWhatIsAlreadyThere(t *testing.T) {
	c, id, _ := exportFixture(t)
	body := map[string]any{"paths": []string{"/media/films"}, "dest": ""}

	w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/export", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("first export: %d %s", w.Code, w.Body.String())
	}

	w, again := c.json(http.MethodPost, "/api/remote/"+id+"/export", body)
	if w.Code != http.StatusOK {
		t.Errorf("second export: %d %s, want 200", w.Code, w.Body.String())
	}
	if got := number(t, again, "skipped"); got != 2 {
		t.Errorf("skipped %d, want 2: %v", got, again)
	}
	if got := number(t, again, "exported"); got != 0 {
		t.Errorf("exported %d on a repeat run, want 0", got)
	}
}

func TestRemoteExportRefusesToLeaveTheSourceFolder(t *testing.T) {
	c, id, root := exportFixture(t)

	for _, dest := range []string{"..", "../", "../escape", "x/../.."} {
		w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/export", map[string]any{
			"paths": []string{"/media/films"}, "dest": dest,
		})
		if w.Code == http.StatusCreated || w.Code == http.StatusOK {
			t.Errorf("exporting into %q was allowed", dest)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "films")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a folder was written above the source's folder")
	}
}

func TestRemoteExportRefusesWhatIsNotThere(t *testing.T) {
	c, id, _ := exportFixture(t)

	w, _ := c.json(http.MethodPost, "/api/remote/"+id+"/export", map[string]any{
		"paths": []string{"/nowhere"}, "dest": "",
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("exporting a path that is not there answered %d, want 404: %s", w.Code, w.Body.String())
	}
	w, _ = c.json(http.MethodPost, "/api/remote/nosuchsource/export", map[string]any{
		"paths": []string{"/media/films"}, "dest": "",
	})
	if w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Error("an export to a machine that is not there was accepted")
	}
}

// An export can be asked to keep going without a page in front of it, exactly
// as an import can. The request answers at once; the files follow.
func TestRemoteExportDetaches(t *testing.T) {
	c, id, root := exportFixture(t)

	w, body := c.json(http.MethodPost, "/api/remote/"+id+"/export", map[string]any{
		"paths": []string{"/media/films"}, "dest": "backup", "detach": true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached export: %d %s, want 202", w.Code, w.Body.String())
	}
	run, _ := body["run"].(map[string]any)
	if run == nil || run["id"] == "" {
		t.Fatalf("202 carried no run to watch: %v", body)
	}
	if run["kind"] != "export" {
		t.Errorf("the run does not say which way it is going: %v", run)
	}
	if detached, _ := run["detached"].(bool); !detached {
		t.Errorf("the run does not say it is detached: %v", run)
	}

	finished := waitForTransfer(t, c, id, "export", run["id"].(string))
	if errText, _ := finished["error"].(string); errText != "" {
		t.Fatalf("the detached export failed: %s", errText)
	}
	summary, _ := finished["summary"].(map[string]any)
	if summary == nil {
		t.Fatalf("a finished detached export kept no summary: %v", finished)
	}
	if got := number(t, summary, "exported"); got != 2 {
		t.Errorf("exported %v, want 2: %v", got, summary)
	}
	if _, err := os.Stat(filepath.Join(root, "backup", "films", "two.mp4")); err != nil {
		t.Errorf("the files did not land: %v", err)
	}

	// An export is not listed among the imports, and cannot be dismissed as
	// one.
	_, imports := c.json(http.MethodGet, "/api/remote/"+id+"/import", nil)
	if listed, _ := imports["imports"].([]any); len(listed) != 0 {
		t.Errorf("an export was listed as an import: %v", listed)
	}
	w, _ = c.json(http.MethodDelete, "/api/remote/"+id+"/import/"+run["id"].(string), nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("an export was dismissed by naming it as an import: %d", w.Code)
	}

	w, after := c.json(http.MethodDelete, "/api/remote/"+id+"/export/"+run["id"].(string), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dismissing the result: %d %s", w.Code, w.Body.String())
	}
	if exports, _ := after["exports"].([]any); len(exports) != 0 {
		t.Errorf("a dismissed result is still listed: %v", after)
	}
}

func TestRemoteExportProgressRefusesAnUnknownSource(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, _ := c.json(http.MethodGet, "/api/remote/nosuchsource/export", nil)
	if w.Code == http.StatusOK {
		t.Error("progress was answered for a machine that is not there")
	}
	w, _ = c.json(http.MethodDelete, "/api/remote/nosuchsource/export/nosuchrun", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("stopping an unknown run answered %d, want 404", w.Code)
	}
}

func TestRemoteExportNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	req, _ := remoteFixture(t, "vps")
	id := c.connectRemote(req)["id"].(string)

	c.cookies = nil
	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/api/remote/" + id + "/export"},
		{http.MethodGet, "/api/remote/" + id + "/export"},
		{http.MethodDelete, "/api/remote/" + id + "/export/run"},
	} {
		w, _ := c.json(call.method, call.path, map[string]any{"paths": []string{"/"}, "dest": ""})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session answered %d, want 401", call.method, call.path, w.Code)
		}
	}
}

// waitForTransfer polls until the named run says it is done, in the direction
// asked, and hands back what it said.
func waitForTransfer(t *testing.T, c *testClient, source, kind, run string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, body := c.json(http.MethodGet, "/api/remote/"+source+"/"+kind, nil)
		runs, _ := body[kind+"s"].([]any)
		for _, raw := range runs {
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
	t.Fatalf("%s %s never finished", kind, run)
	return nil
}
