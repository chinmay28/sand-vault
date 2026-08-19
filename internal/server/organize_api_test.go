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
