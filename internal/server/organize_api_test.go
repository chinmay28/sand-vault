package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
)

// The survey, decoded into what the browser reads off it.
type surveyBody struct {
	Path    string          `json:"path"`
	Files   []surveyFileRow `json:"files"`
	Folders []surveyDirRow  `json:"folders"`
}

type surveyFileRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Size  int64  `json:"size"`
	Ext   string `json:"ext"`
	Depth int    `json:"depth"`
}

type surveyDirRow struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Depth int    `json:"depth"`
	Files int    `json:"files"`
	Total int    `json:"total"`
	Bytes int64  `json:"bytes"`
}

func (c *testClient) survey(t *testing.T, dir string) surveyBody {
	t.Helper()

	w := c.do(http.MethodGet, "/api/folders/survey?path="+url.QueryEscape(dir), nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("survey %s: %d %s", dir, w.Code, w.Body.String())
	}
	var out surveyBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("survey %s: %v", dir, err)
	}
	return out
}

// grow stores one file at each full path, creating the folders above it.
func (c *testClient) grow(t *testing.T, paths ...string) {
	t.Helper()

	for _, full := range paths {
		cut := 0
		for i := len(full) - 1; i >= 0; i-- {
			if full[i] == '/' {
				cut = i
				break
			}
		}
		dir, name := full[:cut], full[cut+1:]
		if dir == "" {
			dir = "/"
		} else if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": dir}); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", dir, w.Code, body)
		}
		c.upload(name, dir, []byte("contents of "+full))
	}
}

func TestSurveyAnswersWhatIsUnderAFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t,
		"/films/loose.mkv",
		"/films/2023/corfu/one.mkv",
		"/films/2023/corfu/one.srt",
	)
	if w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films/nothing"}); w.Code != http.StatusCreated {
		t.Fatal("could not create an empty folder to find")
	}

	s := c.survey(t, "/films")
	if s.Path != "/films" {
		t.Errorf("survey answers for %s, want /films", s.Path)
	}
	if len(s.Files) != 3 {
		t.Fatalf("survey found %d files, want 3", len(s.Files))
	}

	depths := map[string]int{}
	kinds := map[string]string{}
	for _, f := range s.Files {
		depths[f.Name] = f.Depth
		kinds[f.Name] = f.Ext
	}
	if depths["loose.mkv"] != 0 || depths["one.srt"] != 2 {
		t.Errorf("depths are %v, want loose.mkv at 0 and one.srt at 2", depths)
	}
	if kinds["one.srt"] != ".srt" {
		t.Errorf("one.srt is a %q file, want .srt", kinds["one.srt"])
	}

	holding := map[string]int{}
	for _, f := range s.Folders {
		holding[f.Path] = f.Total
	}
	want := map[string]int{"/films/2023": 2, "/films/2023/corfu": 2, "/films/nothing": 0}
	if fmt.Sprint(holding) != fmt.Sprint(want) {
		t.Errorf("folders hold %v, want %v", holding, want)
	}
}

// The browser's flatten, run over the API exactly as the organizer runs it:
// read the folder once, move every file below up into it under a name nothing
// else has claimed, then remove the folders the moves emptied — deepest first
// and never recursively, so a folder that still holds something is refused
// rather than taken with its contents.
func TestFlatteningAFolderMovesEveryFileUpAndClearsTheFoldersBehindIt(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t,
		"/trip/one.jpg",
		"/trip/corfu/one.jpg",
		"/trip/corfu/two.jpg",
		"/trip/rome/day one/one.jpg",
	)

	s := c.survey(t, "/trip")
	taken := map[string]bool{}
	for _, f := range s.Files {
		if f.Depth == 0 {
			taken[f.Name] = true
		}
	}

	for _, f := range s.Files {
		if f.Depth == 0 {
			continue
		}
		name := f.Name
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("one (%d).jpg", n)
		}
		taken[name] = true
		if w, body := c.json(http.MethodPost, "/api/files/"+f.ID+"/move",
			map[string]any{"dir": "/trip", "name": name}); w.Code != http.StatusOK {
			t.Fatalf("move %s: %d %v", f.Name, w.Code, body)
		}
	}

	// Deepest first: /trip/rome can only go once /trip/rome/day one has.
	folders := append([]surveyDirRow{}, s.Folders...)
	sort.Slice(folders, func(i, j int) bool { return folders[i].Depth > folders[j].Depth })
	for _, f := range folders {
		if w, body := c.json(http.MethodDelete, "/api/folders?path="+url.QueryEscape(f.Path), nil); w.Code != http.StatusOK {
			t.Fatalf("remove %s: %d %v", f.Path, w.Code, body)
		}
	}

	after := c.survey(t, "/trip")
	if len(after.Folders) != 0 {
		t.Errorf("%d folders survived the flatten: %v", len(after.Folders), after.Folders)
	}
	if len(after.Files) != 4 {
		t.Fatalf("the flattened folder holds %d files, want 4", len(after.Files))
	}

	names := []string{}
	for _, f := range after.Files {
		if f.Depth != 0 {
			t.Errorf("%s is still %d folders down", f.Name, f.Depth)
		}
		names = append(names, f.Name)
	}
	sort.Strings(names)
	// Four files that were all called one.jpg somewhere, and four distinct
	// names — nothing was overwritten on the way up.
	if fmt.Sprint(names) != "[one (2).jpg one (3).jpg one.jpg two.jpg]" {
		t.Errorf("the flattened folder holds %v, want four distinct names", names)
	}
}

// The guarantee the empty-folder tool leans on: removing a folder without
// asking for it recursively cannot take a file with it.
func TestRemovingAFolderNonRecursivelyRefusesToTakeAFileWithIt(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t, "/keep/one.txt")

	if w, _ := c.json(http.MethodDelete, "/api/folders?path=/keep", nil); w.Code == http.StatusOK {
		t.Fatal("a folder holding a file was removed without recursive=1")
	}
	if s := c.survey(t, "/keep"); len(s.Files) != 1 {
		t.Errorf("the refused removal left %d files, want 1", len(s.Files))
	}
}

func TestSurveyRefusesAFolderThatIsNotThere(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w := c.do(http.MethodGet, "/api/folders/survey?path=/nowhere", nil, ""); w.Code == http.StatusOK {
		t.Fatalf("surveying a folder that does not exist answered %d", w.Code)
	}
}

// It reads the index, so it is behind the session like everything else that
// does.
func TestSurveyNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t, "/private/one.txt")
	c.cookies = nil

	if w := c.do(http.MethodGet, "/api/folders/survey?path=/private", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("survey without a session answered %d, want 401", w.Code)
	}
}

/* --- Duplicates ------------------------------------------------------- */

// The copies under a folder, decoded into what the dialog reads off them.
type duplicatesBody struct {
	Path    string       `json:"path"`
	Scanned int          `json:"scanned"`
	Content duplicateSet `json:"content"`
	Size    duplicateSet `json:"size"`
	Name    duplicateSet `json:"name"`
}

type duplicateSet struct {
	Groups  []duplicateGroup `json:"groups"`
	Files   int              `json:"files"`
	Extra   int              `json:"extra"`
	Waste   int64            `json:"waste"`
	Partial bool             `json:"partial"`
	Crowded int              `json:"crowded"`
}

type duplicateGroup struct {
	Key     string         `json:"key"`
	Files   []duplicateRow `json:"files"`
	Bytes   int64          `json:"bytes"`
	Waste   int64          `json:"waste"`
	Certain bool           `json:"certain"`
}

type duplicateRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Size  int64  `json:"size"`
	Ext   string `json:"ext"`
	Depth int    `json:"depth"`
	Hash  string `json:"hash"`
	Keep  bool   `json:"keep"`
}

func (c *testClient) duplicates(t *testing.T, dir string) duplicatesBody {
	t.Helper()

	w := c.do(http.MethodGet, "/api/folders/duplicates?path="+url.QueryEscape(dir), nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("duplicates %s: %d %s", dir, w.Code, w.Body.String())
	}
	var out duplicatesBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("duplicates %s: %v", dir, err)
	}
	return out
}

// One walk, three answers — and each of the three has to reach the browser
// whole, because switching between them is the whole of using the dialog.
func TestDuplicatesAnswersAllThreeWaysAtOnce(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos/2023"}); w.Code != http.StatusCreated {
		t.Fatalf("create /photos/2023: %d %v", w.Code, body)
	}

	same := []byte("one photograph, stored twice")
	c.upload("corfu.jpg", "/photos", same)
	c.upload("DSC_9912.jpg", "/photos/2023", same)
	// The same length as each other but not as the pair above, and named the
	// way a second download is named.
	c.upload("notes.txt", "/photos", []byte("aaaaaaaa"))
	c.upload("notes (1).txt", "/photos", []byte("bbbbbbbb"))

	d := c.duplicates(t, "/photos")
	if d.Path != "/photos" || d.Scanned != 4 {
		t.Fatalf("duplicates of /photos scanned %d files at %q, want 4 at /photos", d.Scanned, d.Path)
	}

	// By content: the pair that really is one file, and it says so.
	if len(d.Content.Groups) != 1 || len(d.Content.Groups[0].Files) != 2 {
		t.Fatalf("content found %+v, want the one identical pair", d.Content.Groups)
	}
	if !d.Content.Groups[0].Certain {
		t.Error("a pair sharing one hash is not reported as certain")
	}
	if d.Content.Groups[0].Files[0].Hash == "" {
		t.Error("a duplicate row carries no hash, so the dialog cannot say what it knows")
	}
	if !d.Content.Groups[0].Files[0].Keep || d.Content.Groups[0].Files[1].Keep {
		t.Error("the group does not mark exactly one copy to keep")
	}

	// By size: both pairs, since each pair weighs the same within itself.
	if len(d.Size.Groups) != 2 {
		t.Fatalf("size found %d groups, want 2", len(d.Size.Groups))
	}
	// By name: only the pair a copy marker separates.
	if len(d.Name.Groups) != 1 || len(d.Name.Groups[0].Files) != 2 {
		t.Fatalf("name found %+v, want just notes.txt and notes (1).txt", d.Name.Groups)
	}
	if d.Name.Groups[0].Certain {
		t.Error("two different files with alike names are reported as certainly identical")
	}
	if d.Name.Extra != 1 || d.Name.Files != 2 {
		t.Errorf("name counts %d files of which %d spare, want 2 and 1", d.Name.Files, d.Name.Extra)
	}
}

// A vault with nothing doubled answers with three empty sets rather than with
// nulls the dialog would have to guard against.
func TestDuplicatesAnswersEmptySetsWhenThereAreNone(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/tidy"}); w.Code != http.StatusCreated {
		t.Fatalf("create /tidy: %d %v", w.Code, body)
	}
	c.upload("one.txt", "/tidy", []byte("a"))
	c.upload("two.txt", "/tidy", []byte("bb"))

	d := c.duplicates(t, "/tidy")
	for name, set := range map[string]duplicateSet{"content": d.Content, "size": d.Size, "name": d.Name} {
		if set.Groups == nil {
			t.Errorf("%s came back as null rather than as an empty list", name)
		}
		if len(set.Groups) != 0 || set.Extra != 0 || set.Waste != 0 {
			t.Errorf("%s found %+v in a folder with no duplicates", name, set)
		}
	}
}

func TestDuplicatesRefusesAFolderThatIsNotThere(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w := c.do(http.MethodGet, "/api/folders/duplicates?path=/nowhere", nil, ""); w.Code == http.StatusOK {
		t.Fatalf("looking under a folder that does not exist answered %d", w.Code)
	}
}

// It reads the index, so it is behind the session like everything else that
// does.
func TestDuplicatesNeedASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t, "/private/one.txt")
	c.cookies = nil

	if w := c.do(http.MethodGet, "/api/folders/duplicates?path=/private", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("duplicates without a session answered %d, want 401", w.Code)
	}
}

// The folder menu's figures, decoded the way the browser reads them.
type folderStatsBody struct {
	Path     string   `json:"path"`
	Files    int      `json:"files"`
	Folders  int      `json:"folders"`
	Bytes    int64    `json:"bytes"`
	Stored   int64    `json:"stored_bytes"`
	Clouds   []string `json:"clouds"`
	Degraded int      `json:"degraded"`
	Newest   string   `json:"newest"`
}

func (c *testClient) folderStats(t *testing.T, dir string) folderStatsBody {
	t.Helper()

	w := c.do(http.MethodGet, "/api/folders/stats?path="+url.QueryEscape(dir), nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("stats %s: %d %s", dir, w.Code, w.Body.String())
	}
	var out folderStatsBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("stats %s: %v", dir, err)
	}
	return out
}

// What the menu header shows: everything under the folder rather than what is
// directly in it, weighed twice — once as the files and once as the parts.
func TestFolderStatsAnswersWhatAFolderHolds(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t,
		"/films/loose.mkv",
		"/films/2023/corfu/one.mkv",
		"/photos/elsewhere.jpg",
	)

	s := c.folderStats(t, "/films")
	if s.Path != "/films" {
		t.Errorf("stats answer for %s, want /films", s.Path)
	}
	if s.Files != 2 {
		t.Errorf("/films holds %d files, want 2 — the one in /photos is not under it", s.Files)
	}
	if s.Folders != 2 {
		t.Errorf("/films holds %d folders, want 2", s.Folders)
	}
	if s.Bytes == 0 || s.Stored <= s.Bytes {
		t.Errorf("/films is %d bytes stored as %d, want the parts to weigh more", s.Bytes, s.Stored)
	}
	if len(s.Clouds) != 3 {
		t.Errorf("/films lives on %v, want all three accounts", s.Clouds)
	}
	if s.Newest == "" {
		t.Error("/films holds files and reports no newest one")
	}
	if s.Degraded != 0 {
		t.Errorf("%d files under /films are short a part, want none", s.Degraded)
	}
}

// A folder that is not there is a refusal rather than a folder holding nothing.
func TestFolderStatsRefusesAFolderThatIsNotThere(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w := c.do(http.MethodGet, "/api/folders/stats?path=/nowhere", nil, "")
	if w.Code == http.StatusOK {
		t.Errorf("stats for a folder that is not there: %d %s", w.Code, w.Body.String())
	}
}
