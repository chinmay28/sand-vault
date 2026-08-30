package server

import (
	"net/http"
	"testing"
)

// The upload precheck: which files of a choice the vault already holds at the
// destination, with the same name and the same size. Those are the ones not
// worth sending — without an overwrite the upload would store a second copy
// beside the first under a made-up name — so the browser asks first and drops
// them from the choice.

// precheck posts one precheck request and returns the positions the server
// says already exist.
func (c *testClient) precheck(dir string, files []map[string]any, vaultID ...string) []int {
	c.t.Helper()

	payload := map[string]any{"path": dir, "files": files}
	if len(vaultID) > 0 {
		payload["vault"] = vaultID[0]
	}
	w, body := c.json(http.MethodPost, "/api/files/precheck", payload)
	if w.Code != http.StatusOK {
		c.t.Fatalf("precheck: %d %s", w.Code, w.Body.String())
	}

	raw, _ := body["existing"].([]any)
	out := make([]int, len(raw))
	for i, v := range raw {
		out[i] = int(v.(float64))
	}
	return out
}

func TestPrecheckNamesTheFilesAlreadyStoredAtTheSameSize(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.upload("notes.txt", "/", []byte("ten bytes!"))

	existing := c.precheck("/", []map[string]any{
		// The same file again: same name, same size — the duplicate the check
		// exists to catch, whatever the bytes turn out to be.
		{"name": "notes.txt", "size": 10},
		// The same name at another size is a changed file, not a duplicate.
		{"name": "notes.txt", "size": 11},
		// A name the vault has never seen.
		{"name": "new.txt", "size": 10},
	})
	if len(existing) != 1 || existing[0] != 0 {
		t.Errorf("existing = %v, want [0]", existing)
	}
}

func TestPrecheckResolvesAPathTheWayTheUploadWould(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	code, _ := c.uploadTree("/", []treeFile{
		{"photos/2024/hike.txt", []byte("a ridge")},
	})
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}

	existing := c.precheck("/", []map[string]any{
		// The same drop again: the file arrives as its leaf name plus the path
		// it had inside the folder, and lands where it landed last time.
		{"name": "hike.txt", "rel": "photos/2024/hike.txt", "size": 7},
		// The leaf name alone would land in the root, where nothing is.
		{"name": "hike.txt", "size": 7},
		// Separators the upload would normalize away resolve to the same place.
		{"name": "hike.txt", "rel": "photos//2024/./hike.txt", "size": 7},
	})
	if len(existing) != 2 || existing[0] != 0 || existing[1] != 2 {
		t.Errorf("existing = %v, want [0 2]", existing)
	}
}

func TestPrecheckAsksInsideTheFolderTheUploadIsAimedAt(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/archive"})
	c.upload("report.txt", "/archive", []byte("q3"))

	if got := c.precheck("/archive", []map[string]any{
		{"name": "report.txt", "size": 2},
	}); len(got) != 1 {
		t.Errorf("existing = %v, want the file found in its own folder", got)
	}
	if got := c.precheck("/", []map[string]any{
		{"name": "report.txt", "size": 2},
	}); len(got) != 0 {
		t.Errorf("existing = %v — the root holds no report.txt", got)
	}
}

func TestPrecheckLeavesRefusablePathsToTheUploadItself(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.upload("escaped.txt", "/", []byte("x"))

	// A path the upload would refuse is not "already stored" — it is the
	// upload's job to refuse it, per file, where the refusal is reported.
	existing := c.precheck("/inbox", []map[string]any{
		{"name": "escaped.txt", "rel": "../escaped.txt", "size": 1},
	})
	if len(existing) != 0 {
		t.Errorf("existing = %v, want none for a path that climbs out", existing)
	}
}

func TestPrecheckStaysInsideTheVaultItWasAskedAbout(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	sub := c.createSub("Private", "sub-pw")

	c.upload("shared.txt", "/", []byte("main"))
	c.uploadInto(sub, "hidden.txt", "/", []byte("sub!"))

	// The main vault's files are not duplicates of a sub vault's upload, and
	// the other way round — each check is scoped to the tree it would land in.
	if got := c.precheck("/", []map[string]any{
		{"name": "shared.txt", "size": 4},
		{"name": "hidden.txt", "size": 4},
	}, sub); len(got) != 1 || got[0] != 1 {
		t.Errorf("existing = %v, want [1] — only the sub vault's own file", got)
	}
	if got := c.precheck("/", []map[string]any{
		{"name": "hidden.txt", "size": 4},
	}); len(got) != 0 {
		t.Errorf("existing = %v — the main vault holds no hidden.txt", got)
	}
}
