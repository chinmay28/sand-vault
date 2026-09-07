package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The date plan, decoded into what the browser reads off it.
type datePlanBody struct {
	Path    string        `json:"path"`
	Grain   string        `json:"grain"`
	Deep    bool          `json:"deep"`
	Offset  int           `json:"offset"`
	Folders []dateDirRow  `json:"folders"`
	Moves   []dateMoveRow `json:"moves"`
	Bytes   int64         `json:"bytes"`
	Settled int           `json:"settled"`
	Undated int           `json:"undated"`
	Emptied []string      `json:"emptied"`
}

type dateDirRow struct {
	Path   string `json:"path"`
	Label  string `json:"label"`
	Year   int    `json:"year"`
	Month  int    `json:"month"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	Exists bool   `json:"exists"`
}

type dateMoveRow struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Dir      string    `json:"dir"`
	Size     int64     `json:"size"`
	To       string    `json:"to"`
	As       string    `json:"as"`
	Modified time.Time `json:"modified"`
}

func (c *testClient) datePlan(t *testing.T, dir, query string) datePlanBody {
	t.Helper()

	w := c.do(http.MethodGet, "/api/folders/date-sort?path="+url.QueryEscape(dir)+query, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("date plan %s: %d %s", dir, w.Code, w.Body.String())
	}
	var out datePlanBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("date plan %s: %v", dir, err)
	}
	return out
}

// The whole round trip: a flat folder is planned, the plan is run over the
// endpoints that already existed, and asking again finds nothing left to do.
func TestDateSortPlanCanBeRunOverTheOrdinaryEndpoints(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/roll"}); w.Code != http.StatusCreated {
		t.Fatalf("create /roll: %d %v", w.Code, body)
	}
	c.uploadAt("one.jpg", "/roll", []byte("one"), time.Date(2026, time.January, 14, 9, 0, 0, 0, time.UTC))
	c.uploadAt("two.jpg", "/roll", []byte("two"), time.Date(2026, time.January, 30, 9, 0, 0, 0, time.UTC))
	c.uploadAt("old.jpg", "/roll", []byte("old"), time.Date(2025, time.July, 4, 9, 0, 0, 0, time.UTC))

	plan := c.datePlan(t, "/roll", "")
	if len(plan.Moves) != 3 {
		t.Fatalf("plan moves %d files, want 3", len(plan.Moves))
	}
	if len(plan.Folders) != 2 {
		t.Fatalf("plan makes %d folders, want 2", len(plan.Folders))
	}
	// Oldest first.
	if plan.Folders[0].Label != "2025/July" {
		t.Errorf("plan leads with %q, want 2025/July", plan.Folders[0].Label)
	}

	for _, folder := range plan.Folders {
		if folder.Exists {
			t.Fatalf("%s is reported as already there", folder.Path)
		}
		if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": folder.Path}); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", folder.Path, w.Code, body)
		}
	}
	for _, move := range plan.Moves {
		w, body := c.json(http.MethodPost, fmt.Sprintf("/api/files/%s/move", move.ID),
			map[string]any{"dir": move.To, "name": move.As})
		if w.Code != http.StatusOK {
			t.Fatalf("move %s: %d %v", move.Name, w.Code, body)
		}
	}

	// The folders are there, holding what the plan said they would.
	survey := c.survey(t, "/roll")
	filed := map[string]bool{}
	for _, f := range survey.Files {
		filed[f.Dir+"/"+f.Name] = true
	}
	for _, want := range []string{
		"/roll/2026/January/one.jpg",
		"/roll/2026/January/two.jpg",
		"/roll/2025/July/old.jpg",
	} {
		if !filed[want] {
			t.Errorf("%s is not there after the sort ran", want)
		}
	}

	// And running it again would do nothing, which is what makes it safe to
	// press twice — and what makes a run that stalled halfway resumable.
	again := c.datePlan(t, "/roll", "&deep=1")
	if len(again.Moves) != 0 {
		t.Errorf("sorting the same folder again moves %+v, want nothing", again.Moves)
	}
	if again.Settled != 3 {
		t.Errorf("second plan counts %d files settled, want 3", again.Settled)
	}
}

// The knobs the browser turns, each answered without anything being changed.
func TestDateSortPlanAnswersTheQuestionItWasAsked(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/roll/Corfu"}); w.Code != http.StatusCreated {
		t.Fatal("create /roll/Corfu")
	}
	c.uploadAt("loose.jpg", "/roll", []byte("loose"), time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC))
	c.uploadAt("deep.jpg", "/roll/Corfu", []byte("deep"), time.Date(2026, time.March, 3, 9, 0, 0, 0, time.UTC))

	// Shallow by default: the folder somebody made is left alone.
	shallow := c.datePlan(t, "/roll", "")
	if len(shallow.Moves) != 1 || shallow.Moves[0].Name != "loose.jpg" {
		t.Errorf("a shallow plan moves %+v, want only loose.jpg", shallow.Moves)
	}
	if len(shallow.Emptied) != 0 {
		t.Errorf("a shallow plan would empty %v, want nothing", shallow.Emptied)
	}

	// Deep takes the folder's contents too, and says the folder is then empty.
	deep := c.datePlan(t, "/roll", "&deep=1")
	if len(deep.Moves) != 2 {
		t.Errorf("a deep plan moves %d files, want 2", len(deep.Moves))
	}
	if len(deep.Emptied) != 1 || deep.Emptied[0] != "/roll/Corfu" {
		t.Errorf("a deep plan would empty %v, want /roll/Corfu", deep.Emptied)
	}

	// By year alone is one level rather than two.
	yearly := c.datePlan(t, "/roll", "&grain=year")
	if len(yearly.Folders) != 1 || yearly.Folders[0].Path != "/roll/2026" {
		t.Errorf("filing by year made %+v, want one /roll/2026", yearly.Folders)
	}

	// And the viewer's clock decides the month, not the server's.
	c.uploadAt("newyear.jpg", "/roll", []byte("ny"), time.Date(2026, time.January, 1, 6, 40, 0, 0, time.UTC))
	west := c.datePlan(t, "/roll", "&offset=-420")
	for _, move := range west.Moves {
		if move.Name == "newyear.jpg" && move.To != "/roll/2025/December" {
			t.Errorf("seven hours west, newyear.jpg files under %s, want /roll/2025/December", move.To)
		}
	}

	// Nothing above changed anything.
	if survey := c.survey(t, "/roll"); len(survey.Folders) != 1 || survey.Folders[0].Path != "/roll/Corfu" {
		t.Errorf("planning changed the tree: %+v", survey.Folders)
	}
}

func TestDateSortPlanRefusesWhatItCannotAnswer(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	for _, query := range []string{"&grain=week", "&offset=5000", "&offset=soon"} {
		w := c.do(http.MethodGet, "/api/folders/date-sort?path=/"+query, nil, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("date-sort%s answered %d, want 400", query, w.Code)
		}
	}
	if w := c.do(http.MethodGet, "/api/folders/date-sort?path=/nowhere", nil, ""); w.Code == http.StatusOK {
		t.Error("planning a folder that is not there succeeded")
	}
}
