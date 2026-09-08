package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What POST /api/files/move answers with.
type moveBatchBody struct {
	Moved   int      `json:"moved"`
	Missing []string `json:"missing"`
	Refused []string `json:"refused"`
}

func (c *testClient) moveBatch(t *testing.T, moves []map[string]any) moveBatchBody {
	t.Helper()

	w, _ := c.json(http.MethodPost, "/api/files/move", map[string]any{"moves": moves})
	if w.Code != http.StatusOK {
		t.Fatalf("batch move: %d %s", w.Code, w.Body.String())
	}
	var out moveBatchBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("batch move: %v", err)
	}
	return out
}

// The date plan run the way the browser runs it: one request for the folders,
// one for the moves, rather than one request per row.
func TestDateSortPlanRunsAsTwoBatches(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/roll"}); w.Code != http.StatusCreated {
		t.Fatalf("create /roll: %d %v", w.Code, body)
	}
	c.uploadAt("one.jpg", "/roll", []byte("one"), time.Date(2026, time.January, 14, 9, 0, 0, 0, time.UTC))
	c.uploadAt("two.jpg", "/roll", []byte("two"), time.Date(2026, time.January, 30, 9, 0, 0, 0, time.UTC))
	c.uploadAt("old.jpg", "/roll", []byte("old"), time.Date(2025, time.July, 4, 9, 0, 0, 0, time.UTC))

	plan := c.datePlan(t, "/roll", "")

	folders := []string{}
	for _, folder := range plan.Folders {
		folders = append(folders, folder.Path)
	}
	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"paths": folders}); w.Code != http.StatusCreated {
		t.Fatalf("create %v: %d %v", folders, w.Code, body)
	}

	moves := []map[string]any{}
	for _, move := range plan.Moves {
		moves = append(moves, map[string]any{"id": move.ID, "dir": move.To, "name": move.As})
	}
	report := c.moveBatch(t, moves)
	if report.Moved != 3 || len(report.Missing) > 0 || len(report.Refused) > 0 {
		t.Fatalf("batch move report = %+v, want 3 moved and nothing else", report)
	}

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
			t.Errorf("%s is not there after the batch ran", want)
		}
	}

	// And it is still idempotent run this way, which is what makes a batch
	// that stopped part-way safe to send again.
	if again := c.datePlan(t, "/roll", "&deep=1"); len(again.Moves) != 0 {
		t.Errorf("sorting the same folder again moves %+v, want nothing", again.Moves)
	}
	if second := c.moveBatch(t, moves); second.Moved != 3 || len(second.Refused) > 0 {
		t.Errorf("sending the same batch again = %+v, want it settled rather than refused", second)
	}
}

// A batch says which of its rows landed. Anything less and a caller running a
// plan in pieces would have no way to tell which half it had to run again.
func TestBatchMoveReportsTheRowsItCouldNotApply(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	for _, dir := range []string{"/in", "/out"} {
		if w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": dir}); w.Code != http.StatusCreated {
			t.Fatalf("create %s", dir)
		}
	}
	one := c.upload("one.jpg", "/in", []byte("one"))["id"]
	two := c.upload("two.jpg", "/in", []byte("two"))["id"]
	c.upload("two.jpg", "/out", []byte("already here"))

	report := c.moveBatch(t, []map[string]any{
		{"id": one, "dir": "/out"},
		{"id": two, "dir": "/out"},
		{"id": "not-a-file", "dir": "/out"},
	})

	if report.Moved != 1 {
		t.Errorf("moved %d, want 1", report.Moved)
	}
	if len(report.Refused) != 1 {
		t.Errorf("refused %v, want the one collision", report.Refused)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "not-a-file" {
		t.Errorf("missing = %v, want [not-a-file]", report.Missing)
	}
}

func TestBatchMoveRefusesWhatItCannotAnswer(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, _ := c.json(http.MethodPost, "/api/files/move", map[string]any{"moves": []any{}}); w.Code != http.StatusBadRequest {
		t.Errorf("an empty batch answered %d, want 400", w.Code)
	}
	if w := c.do(http.MethodPost, "/api/files/move", strings.NewReader("not json"), "application/json"); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed batch answered %d, want 400", w.Code)
	}
}

// Several folders in one request, and either all of them or none.
func TestBatchFolderCreate(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	wanted := []string{"/roll/2026/January", "/roll/2026/March", "/roll/2025/July"}
	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"paths": wanted}); w.Code != http.StatusCreated {
		t.Fatalf("create %v: %d %v", wanted, w.Code, body)
	}

	survey := c.survey(t, "/roll")
	there := map[string]bool{}
	for _, f := range survey.Folders {
		there[f.Path] = true
	}
	for _, want := range append(wanted, "/roll/2026", "/roll/2025") {
		if !there[want] {
			t.Errorf("%s was not created", want)
		}
	}

	// Asking again is not an error: a plan does not know which of its folders
	// are already there, and half of them usually are.
	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"paths": wanted}); w.Code != http.StatusCreated {
		t.Fatalf("creating them again: %d %v", w.Code, body)
	}

	// And a list with an impossible path in it leaves none of them behind.
	if w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/held"}); w.Code != http.StatusCreated {
		t.Fatal("create /held")
	}
	c.upload("taken", "/held", []byte("a file, not a folder"))
	w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"paths": []string{"/wanted", "/held/taken"}})
	if w.Code < 400 {
		t.Fatalf("creating a folder where a file is answered %d, want a refusal", w.Code)
	}
	for _, f := range c.survey(t, "/").Folders {
		if f.Path == "/wanted" {
			t.Error("/wanted was left behind by a refused batch")
		}
	}
}
