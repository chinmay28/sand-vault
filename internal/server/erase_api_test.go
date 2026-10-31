package server

import (
	"net/http"
	"testing"
	"time"
)

// pollEraseRuns asks the delete runs endpoint until every run is done, and
// returns the final listing.
func (c *testClient) pollEraseRuns(t *testing.T) []map[string]any {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for {
		w, body := c.json(http.MethodGet, "/api/delete/runs", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("runs: %d %s", w.Code, w.Body.String())
		}
		raw, _ := body["runs"].([]any)
		runs := make([]map[string]any, 0, len(raw))
		waiting := false
		for _, r := range raw {
			run := r.(map[string]any)
			runs = append(runs, run)
			if done, _ := run["done"].(bool); !done {
				waiting = true
			}
		}
		if !waiting {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("a delete never finished: %v", runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A selection of files and folders goes as one request and comes back
// counted: files and folders taken, what was not there to take, and the parts
// left behind. A path that names a file is a file.
func TestEraseSelectionOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	one := c.upload("one.bin", "/", []byte("one"))["id"].(string)
	c.upload("two.bin", "/", []byte("two"))
	c.mkdir("/films/2024")
	c.upload("inside.bin", "/films", []byte("inside"))
	c.upload("deeper.bin", "/films/2024", []byte("deeper"))
	kept := c.upload("kept.bin", "/", []byte("kept"))["id"].(string)

	w, body := c.json(http.MethodPost, "/api/delete", map[string]any{
		"targets": []map[string]any{
			{"id": one},
			{"path": "/two.bin"},
			{"path": "/films"},
			{"id": "never-was"},
			{"path": "/nowhere"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %v", w.Code, body)
	}
	if body["status"] != "deleted" {
		t.Errorf("status = %v, want deleted", body["status"])
	}
	// Two files named, two inside the folder.
	if body["deleted"] != float64(4) {
		t.Errorf("deleted = %v, want 4", body["deleted"])
	}
	if body["folders"] != float64(1) {
		t.Errorf("folders = %v, want 1", body["folders"])
	}
	missing, _ := body["missing"].([]any)
	if len(missing) != 2 || missing[0] != "never-was" || missing[1] != "/nowhere" {
		t.Errorf("missing = %v, want [never-was /nowhere]", body["missing"])
	}
	if warnings, _ := body["warnings"].([]any); len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", body["warnings"])
	}

	w, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	if files, _ := body["files"].([]any); len(files) != 1 {
		t.Errorf("files left at the root = %v, want only kept.bin", body["files"])
	}
	if folders, _ := body["folders"].([]any); len(folders) != 0 {
		t.Errorf("folders left at the root = %v, want none", body["folders"])
	}
	if w, _ := c.json(http.MethodGet, "/api/files/"+kept, nil); w.Code != http.StatusOK {
		t.Errorf("a file outside the selection is gone: %d", w.Code)
	}

	// Nothing is left running once the request has answered.
	if runs := c.pollEraseRuns(t); len(runs) != 0 {
		t.Errorf("a foreground delete is still listed after answering: %v", runs)
	}
}

// Asked to detach, the delete answers at once with the run, reports where it
// has got to where anyone can poll it, and leaves its outcome there for
// whoever comes back — until they dismiss it.
func TestDetachedEraseOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.mkdir("/doomed")
	c.upload("a.bin", "/doomed", []byte("a"))
	c.upload("b.bin", "/doomed", []byte("b"))
	c.upload("c.bin", "/doomed", []byte("c"))
	kept := c.upload("kept.bin", "/", []byte("kept"))["id"].(string)

	w, body := c.json(http.MethodPost, "/api/delete", map[string]any{
		"path":   "/doomed",
		"detach": true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached delete: %d %s", w.Code, w.Body.String())
	}
	run, _ := body["run"].(map[string]any)
	if run == nil || run["id"] == "" {
		t.Fatalf("no run in the answer: %v", body)
	}
	if detached, _ := run["detached"].(bool); !detached {
		t.Errorf("the run does not say it is detached: %v", run)
	}
	if run["label"] != "/doomed" {
		t.Errorf("label = %v, want /doomed", run["label"])
	}
	// The denominator is known from the answer on: three files to go.
	if at, _ := run["at"].(map[string]any); at == nil || at["total"] != float64(3) {
		t.Errorf("at = %v, want total 3", run["at"])
	}

	runs := c.pollEraseRuns(t)
	if len(runs) != 1 || runs[0]["id"] != run["id"] {
		t.Fatalf("runs after the delete = %v, want the one started", runs)
	}
	finished := runs[0]
	if cancelled, _ := finished["cancelled"].(bool); cancelled || finished["error"] != nil {
		t.Errorf("the run did not finish cleanly: %v", finished)
	}
	report, _ := finished["report"].(map[string]any)
	if report == nil || report["deleted"] != float64(3) || report["folders"] != float64(1) {
		t.Errorf("report = %v, want 3 files and 1 folder", finished["report"])
	}
	if at, _ := finished["at"].(map[string]any); at == nil || at["done"] != float64(3) {
		t.Errorf("at = %v, want done 3", finished["at"])
	}
	if waiting, _ := finished["waiting"].(bool); waiting {
		t.Errorf("a finished run still says it is waiting: %v", finished)
	}

	w, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	if folders, _ := body["folders"].([]any); len(folders) != 0 {
		t.Errorf("the folder is still listed: %v", body["folders"])
	}
	if w, _ := c.json(http.MethodGet, "/api/files/"+kept, nil); w.Code != http.StatusOK {
		t.Errorf("a file outside the selection is gone: %d", w.Code)
	}

	// Dismissing the result is a DELETE on the run; a second one finds
	// nothing by that name.
	w, body = c.json(http.MethodDelete, "/api/delete/runs/"+run["id"].(string), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %v", w.Code, body)
	}
	if left, _ := body["runs"].([]any); len(left) != 0 {
		t.Errorf("runs after dismissing = %v, want none", body["runs"])
	}
	if w, _ := c.json(http.MethodDelete, "/api/delete/runs/"+run["id"].(string), nil); w.Code != http.StatusNotFound {
		t.Errorf("dismissing twice = %d, want 404", w.Code)
	}
}

// A selection of files is one index write, detached or not, and a file given
// twice is one file.
func TestDetachedEraseOfFiles(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	one := c.upload("one.bin", "/", []byte("one"))["id"].(string)
	two := c.upload("two.bin", "/", []byte("two"))["id"].(string)

	w, body := c.json(http.MethodPost, "/api/delete", map[string]any{
		"targets": []map[string]any{{"id": one}, {"id": two}, {"id": one}},
		"detach":  true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached delete: %d %v", w.Code, body)
	}
	run, _ := body["run"].(map[string]any)
	if run["label"] != "3 items" {
		t.Errorf("label = %v, want 3 items", run["label"])
	}

	runs := c.pollEraseRuns(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want one", runs)
	}
	report, _ := runs[0]["report"].(map[string]any)
	if report == nil || report["deleted"] != float64(2) || report["folders"] != float64(0) {
		t.Errorf("report = %v, want 2 files", runs[0]["report"])
	}
	for _, id := range []string{one, two} {
		if w, _ := c.json(http.MethodGet, "/api/files/"+id, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s is still there after the delete: %d", id, w.Code)
		}
	}
}

// A single file named by ID is labelled by its path, so the card can say
// what is going rather than quote an identifier.
func TestEraseLabelsASingleFileByPath(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.mkdir("/films")
	id := c.upload("film.mkv", "/films", []byte("film"))["id"].(string)

	w, body := c.json(http.MethodPost, "/api/delete", map[string]any{"id": id, "detach": true})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached delete: %d %v", w.Code, body)
	}
	run, _ := body["run"].(map[string]any)
	if run["label"] != "/films/film.mkv" {
		t.Errorf("label = %v, want /films/film.mkv", run["label"])
	}
	c.pollEraseRuns(t)
}

// Nothing named is a bad request; one thing named that is not there is not
// found; a selection none of which is there is an ordinary answer, as a
// selection deleted twice — and a detached one is not a run.
func TestEraseRefusesWhatItCannotName(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, _ := c.json(http.MethodPost, "/api/delete", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Errorf("nothing named = %d, want 400", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/delete", map[string]any{"id": "never-was", "detach": true}); w.Code != http.StatusNotFound {
		t.Errorf("one missing target = %d, want 404", w.Code)
	}

	w, body := c.json(http.MethodPost, "/api/delete", map[string]any{
		"targets": []map[string]any{{"id": "never-was"}, {"path": "/nowhere"}},
		"detach":  true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("a selection of nothing = %d %v, want 200", w.Code, body)
	}
	if body["deleted"] != float64(0) {
		t.Errorf("deleted = %v, want 0", body["deleted"])
	}
	if missing, _ := body["missing"].([]any); len(missing) != 2 {
		t.Errorf("missing = %v, want both", body["missing"])
	}
	if runs := c.pollEraseRuns(t); len(runs) != 0 {
		t.Errorf("a delete of nothing left a run behind: %v", runs)
	}

	if w, _ := c.json(http.MethodDelete, "/api/delete/runs/nobody", nil); w.Code != http.StatusNotFound {
		t.Errorf("stopping a run that never was = %d, want 404", w.Code)
	}
}

// Locking the vault while a delete is listed takes the run with it — stopped,
// not failed — and the files it had already erased stay gone.
func TestLockStopsDetachedErase(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.mkdir("/doomed")
	c.upload("a.bin", "/doomed", []byte("a"))

	w, body := c.json(http.MethodPost, "/api/delete", map[string]any{"path": "/doomed", "detach": true})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached delete: %d %v", w.Code, body)
	}
	runs := c.pollEraseRuns(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want one", runs)
	}

	// The results survive a lock — they are memory, not keys — but nothing
	// is running under them to stop.
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if c.server.erasures.running() != 0 {
		t.Errorf("%d deletes still running after the lock", c.server.erasures.running())
	}
}
