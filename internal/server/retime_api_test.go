package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Times, through the API: an upload keeps the age the file has on the machine
// it came from, and re-uploading a folder puts right the times of the files
// that are already here rather than sending them again.

// uploadAt posts one file with the modification time a browser would report for
// it, and returns the stored entry.
func (c *testClient) uploadAt(name, dir string, content []byte, mod time.Time) map[string]any {
	c.t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("files[]", name)
	if err != nil {
		c.t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(content)
	mw.WriteField("path", dir)
	if !mod.IsZero() {
		mw.WriteField("mod-0", strconv.FormatInt(mod.UnixMilli(), 10))
	}
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	if w.Code != http.StatusCreated {
		c.t.Fatalf("upload %s: %d %s", name, w.Code, w.Body.String())
	}
	var resp struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		c.t.Fatalf("expected 1 upload result, got %d: %s", len(resp.Results), w.Body.String())
	}
	if ok, _ := resp.Results[0]["ok"].(bool); !ok {
		c.t.Fatalf("upload failed: %v", resp.Results[0]["error"])
	}
	return resp.Results[0]["file"].(map[string]any)
}

// precheckBoth posts one precheck request and returns both halves of the
// answer: which files are already here, and which of those are stored under a
// different time from the one they carry.
func (c *testClient) precheckBoth(dir string, files []map[string]any) (existing, retime []int) {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/files/precheck",
		map[string]any{"path": dir, "files": files})
	if w.Code != http.StatusOK {
		c.t.Fatalf("precheck: %d %s", w.Code, w.Body.String())
	}
	return positions(body["existing"]), positions(body["retime"])
}

// retime posts one retime request and returns how many entries it changed.
func (c *testClient) retime(dir string, files []map[string]any) int {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/files/retime",
		map[string]any{"path": dir, "files": files})
	if w.Code != http.StatusOK {
		c.t.Fatalf("retime: %d %s", w.Code, w.Body.String())
	}
	n, _ := body["retimed"].(float64)
	return int(n)
}

func positions(raw any) []int {
	list, _ := raw.([]any)
	out := make([]int, len(list))
	for i, v := range list {
		out[i] = int(v.(float64))
	}
	return out
}

// modified reads back the time a stored file is filed under.
func (c *testClient) modified(dir, name string) time.Time {
	c.t.Helper()

	w, body := c.json(http.MethodGet, "/api/files?path="+dir, nil)
	if w.Code != http.StatusOK {
		c.t.Fatalf("listing %s: %d %s", dir, w.Code, w.Body.String())
	}
	files, _ := body["files"].([]any)
	for _, raw := range files {
		f := raw.(map[string]any)
		if f["name"] != name {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, f["modified_at"].(string))
		if err != nil {
			c.t.Fatalf("parsing the time of %s: %v", name, err)
		}
		return at
	}
	c.t.Fatalf("nothing named %s in %s", name, dir)
	return time.Time{}
}

func TestUploadKeepsTheTimeTheBrowserReported(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	c.uploadAt("hike.jpg", "/", []byte("a photograph"), taken)

	if got := c.modified("/", "hike.jpg"); !got.Equal(taken) {
		t.Errorf("stored under %s, want the photograph's own time %s", got, taken)
	}
}

// A client that says nothing about times gets what every upload used to get.
func TestUploadWithoutATimeIsStampedNow(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.upload("notes.txt", "/", []byte("ten bytes!"))

	if got := c.modified("/", "notes.txt"); time.Since(got) > time.Minute {
		t.Errorf("stored under %s, want the moment it landed", got)
	}
}

// The precheck answers both halves: already here, and of those, wrongly dated.
func TestPrecheckNamesTheFilesStoredUnderAnotherTime(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	c.uploadAt("kept.jpg", "/", []byte("ten bytes!"), taken)
	c.upload("stale.jpg", "/", []byte("ten bytes!")) // stamped now, as uploads used to be

	existing, retime := c.precheckBoth("/", []map[string]any{
		{"name": "kept.jpg", "size": 10, "mod": taken.UnixMilli()},
		{"name": "stale.jpg", "size": 10, "mod": taken.UnixMilli()},
		// Here, but the client knows nothing about its age: not something to
		// correct, since there is nothing to correct it to.
		{"name": "kept.jpg", "size": 10},
		// Not here at all.
		{"name": "new.jpg", "size": 10, "mod": taken.UnixMilli()},
	})
	if len(existing) != 3 {
		t.Errorf("existing = %v, want the three that are stored", existing)
	}
	if len(retime) != 1 || retime[0] != 1 {
		t.Errorf("retime = %v, want [1] — only the one stored under another time", retime)
	}
}

// The retroactive half: the folder is re-chosen, nothing is sent, and the times
// of what is already there are put right.
func TestRetimeCorrectsWhatIsAlreadyStored(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	entry := c.upload("hike.jpg", "/", []byte("ten bytes!"))

	n := c.retime("/", []map[string]any{
		{"name": "hike.jpg", "size": 10, "mod": taken.UnixMilli()},
	})
	if n != 1 {
		t.Fatalf("retimed %d files, want 1", n)
	}
	if got := c.modified("/", "hike.jpg"); !got.Equal(taken) {
		t.Errorf("the file says %s, want %s", got, taken)
	}

	// The file itself was never in question: it is the same entry, still whole.
	w, body := c.json(http.MethodGet, "/api/files/"+entry["id"].(string), nil)
	after, _ := body["file"].(map[string]any)
	if w.Code != http.StatusOK || after == nil || after["id"] != entry["id"] {
		t.Errorf("retiming replaced the entry: %d %v", w.Code, body)
	}

	// Asking again changes nothing, which is what makes re-choosing a folder
	// safe to do as often as you like.
	if again := c.retime("/", []map[string]any{
		{"name": "hike.jpg", "size": 10, "mod": taken.UnixMilli()},
	}); again != 0 {
		t.Errorf("the second run reported %d changes, want none", again)
	}
}

func TestRetimeLeavesADifferentFileAlone(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	c.upload("notes.txt", "/", []byte("ten bytes!"))
	was := c.modified("/", "notes.txt")

	n := c.retime("/", []map[string]any{
		// The same name at another size is a different file: correcting its
		// time would be describing the stored one with somebody else's date.
		{"name": "notes.txt", "size": 11, "mod": taken.UnixMilli()},
		// Nothing is stored under this name.
		{"name": "gone.txt", "size": 10, "mod": taken.UnixMilli()},
		// A path the upload itself would refuse.
		{"name": "escaped.txt", "rel": "../escaped.txt", "size": 10, "mod": taken.UnixMilli()},
	})
	if n != 0 {
		t.Errorf("retimed %d files, want none", n)
	}
	if got := c.modified("/", "notes.txt"); !got.Equal(was) {
		t.Errorf("the stored file was restamped: %s, was %s", got, was)
	}
}

// The guard the import has too: a file whose time is newer than the copy stored
// here changed on the machine since, and the copy here is still the old
// contents — an upload will not replace it, so its date must not claim to be
// the new file's either.
func TestRetimeLeavesAFileChangedSinceItWasUploadedAlone(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.upload("notes.txt", "/", []byte("ten bytes!"))
	was := c.modified("/", "notes.txt")
	edited := time.Now().Add(time.Hour)

	_, retime := c.precheckBoth("/", []map[string]any{
		{"name": "notes.txt", "size": 10, "mod": edited.UnixMilli()},
	})
	if len(retime) != 0 {
		t.Errorf("retime = %v, want none for a file touched since it was uploaded", retime)
	}
	if n := c.retime("/", []map[string]any{
		{"name": "notes.txt", "size": 10, "mod": edited.UnixMilli()},
	}); n != 0 {
		t.Errorf("retimed %d files, want none", n)
	}
	if got := c.modified("/", "notes.txt"); !got.Equal(was) {
		t.Errorf("the stored file took a newer file's date: %s, was %s", got, was)
	}
}

// The path a dropped folder's file lands at is worked out the same way the
// upload works it out, so what is corrected is the entry that file is stored as.
func TestRetimeResolvesAPathTheWayTheUploadWould(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	code, _ := c.uploadTree("/", []treeFile{{"photos/2024/hike.txt", []byte("a ridge")}})
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}

	n := c.retime("/", []map[string]any{
		{"name": "hike.txt", "rel": "photos/2024/hike.txt", "size": 7, "mod": taken.UnixMilli()},
		// The leaf name alone would land in the root, where nothing is.
		{"name": "hike.txt", "size": 7, "mod": taken.UnixMilli()},
	})
	if n != 1 {
		t.Fatalf("retimed %d files, want only the one inside the folder", n)
	}
	if got := c.modified("/photos/2024", "hike.txt"); !got.Equal(taken) {
		t.Errorf("the file says %s, want %s", got, taken)
	}
}
