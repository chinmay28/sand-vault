package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Uploading a directory rather than a file at a time.
//
// A browser cannot hand over a folder — it hands over the files inside it, each
// carrying the path it had within the folder that was chosen. Rebuilding the
// tree from those paths is the whole of the feature on this side, and the
// interesting part of it is that the paths come from the client and so cannot
// be trusted an inch.

// treeFile is one file of a directory upload: the path it had inside the folder
// that was chosen, and what is in it.
type treeFile struct {
	rel     string
	content []byte
}

// uploadTree posts a whole directory the way the browser does: the files under
// "files[]", each one's path inside the chosen folder under "rel-N", and any
// folder holding no file of its own under "dirs".
func (c *testClient) uploadTree(dir string, files []treeFile, dirs ...string) (int, []map[string]any) {
	c.t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, f := range files {
		// The browser sends the leaf as the file's name and the path beside it,
		// which is what makes the path worth checking rather than trusting.
		leaf := f.rel
		if idx := strings.LastIndex(leaf, "/"); idx >= 0 {
			leaf = leaf[idx+1:]
		}
		part, err := mw.CreateFormFile("files[]", leaf)
		if err != nil {
			c.t.Fatalf("CreateFormFile: %v", err)
		}
		part.Write(f.content)
		mw.WriteField("rel-"+strconv.Itoa(i), f.rel)
	}
	for _, d := range dirs {
		mw.WriteField("dirs", d)
	}
	mw.WriteField("path", dir)
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	var resp struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Results
}

func TestUploadRebuildsTheDirectoryItCameFrom(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	code, results := c.uploadTree("/", []treeFile{
		{"photos/hike.txt", []byte("a ridge")},
		{"photos/2024/summer/beach.txt", []byte("a beach")},
		{"photos/2024/summer/cover.txt", []byte("summer")},
		{"photos/2023/cover.txt", []byte("last year")},
	})
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	for _, r := range results {
		if ok, _ := r["ok"].(bool); !ok {
			t.Fatalf("%v failed: %v", r["name"], r["error"])
		}
	}

	// The folders the paths implied, made on the way rather than asked for
	// separately.
	got := strings.Join(c.folderPaths(), " ")
	want := "/ /photos /photos/2023 /photos/2024 /photos/2024/summer"
	if got != want {
		t.Errorf("folders = %q, want %q", got, want)
	}

	// And the files inside them, with the two "cover.txt" kept apart by the
	// folders they arrived in rather than collided into one.
	paths := []string{}
	for _, r := range results {
		paths = append(paths, r["file"].(map[string]any)["dir"].(string)+"/"+
			r["file"].(map[string]any)["name"].(string))
	}
	sort.Strings(paths)
	if joined := strings.Join(paths, " "); joined !=
		"/photos/2023/cover.txt /photos/2024/summer/beach.txt /photos/2024/summer/cover.txt /photos/hike.txt" {
		t.Errorf("paths = %q", joined)
	}
}

func TestUploadedDirectoryLandsInsideTheFolderItWasDroppedOn(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/archive"})

	code, results := c.uploadTree("/archive", []treeFile{
		{"notes/monday.txt", []byte("a note")},
	})
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	if dir := results[0]["file"].(map[string]any)["dir"].(string); dir != "/archive/notes" {
		t.Errorf("dir = %q, want /archive/notes — a dropped folder nests, it does not replace", dir)
	}
}

func TestUploadKeepsTheEmptyFoldersOfADirectory(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// A folder holding nothing has no file to carry it, so it is named in its
	// own right. Dropping a tree and getting back a tree with its empty corners
	// missing is not the tree that was dropped.
	code, _ := c.uploadTree("/", []treeFile{
		{"project/README.txt", []byte("read me")},
	}, "project", "project/drafts", "project/drafts/old")
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}

	got := strings.Join(c.folderPaths(), " ")
	if want := "/ /project /project/drafts /project/drafts/old"; got != want {
		t.Errorf("folders = %q, want %q", got, want)
	}
}

func TestUploadRefusesAPathThatClimbsOutOfItsFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/inbox"})

	for _, rel := range []string{"../escaped.txt", "a/../../escaped.txt", "/etc/escaped.txt"} {
		code, results := c.uploadTree("/inbox", []treeFile{{rel, []byte("x")}})
		if len(results) != 1 {
			t.Fatalf("%s: expected one result, got %d (status %d)", rel, len(results), code)
		}
		if ok, _ := results[0]["ok"].(bool); ok {
			t.Errorf("%s was stored — a relative path is the client's word, not the server's", rel)
		}
	}

	// An absolute path is refused rather than treated as one; nothing was made
	// outside the folder the upload was posted to either way.
	if got := strings.Join(c.folderPaths(), " "); got != "/ /inbox" {
		t.Errorf("folders = %q — the tree grew somewhere it should not have", got)
	}
}

func TestUploadRefusesADirectoryFieldThatClimbsOut(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	code, _ := c.uploadTree("/", []treeFile{{"ok.txt", []byte("x")}}, "../elsewhere")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a folder that climbs out is a malformed request", code)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/" {
		t.Errorf("folders = %q, want just the root", got)
	}
}

func TestUploadReportsFailuresByTheirPathInTheDirectory(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// A file already standing where a folder needs to be: everything under that
	// folder fails, and says which folder it was under rather than leaving four
	// identical "cover.txt" lines to guess between.
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos"})
	c.upload("2024", "/photos", []byte("not a folder"))

	code, results := c.uploadTree("/", []treeFile{
		{"photos/2024/cover.txt", []byte("x")},
		{"photos/2023/cover.txt", []byte("y")},
	})
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}

	byName := map[string]map[string]any{}
	for _, r := range results {
		byName[r["name"].(string)] = r
	}
	blocked, ok := byName["photos/2024/cover.txt"]
	if !ok {
		t.Fatalf("no result named for its path in the directory: %v", byName)
	}
	if okd, _ := blocked["ok"].(bool); okd {
		t.Error("stored a file under a folder that could not be made")
	}
	if fine, _ := byName["photos/2023/cover.txt"]["ok"].(bool); !fine {
		t.Errorf("the rest of the directory should still arrive: %v", byName["photos/2023/cover.txt"])
	}
}

func TestUploadWithoutAPathIsStillAPlainFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// The field is absent for files picked one by one, which is most uploads:
	// they land exactly where they always did.
	file := c.upload("loose.txt", "/", []byte("x"))
	if dir := file["dir"].(string); dir != "/" {
		t.Errorf("dir = %q, want /", dir)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/" {
		t.Errorf("folders = %q, want just the root", got)
	}
}
