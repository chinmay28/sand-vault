package server

import (
	"net/http"
	"sync"
	"testing"
)

// awaitMovieScan polls the detached sweep until it is no longer running and
// answers with its last status.
func (c *testClient) awaitMovieScan(id string) map[string]any {
	c.t.Helper()
	var last map[string]any
	awaitScan(c.t, "the film sweep", func() bool {
		_, last = c.json(http.MethodGet, "/api/movies/scanning", nil)
		if last["id"] != id {
			c.t.Fatalf("the poll answered for a different run: %v", last)
		}
		running, _ := last["running"].(bool)
		return !running
	})
	return last
}

// holdSearches makes the fake database's searches wait until the function
// returned is called, which is safe to call more than once and is called at
// the end of the test regardless.
func holdSearches(t *testing.T, db *fakeMovieDB) (release func()) {
	db.held = make(chan struct{})
	db.searching = make(chan string, 16)
	var once sync.Once
	release = func() { once.Do(func() { close(db.held) }) }
	t.Cleanup(release)
	return release
}

func TestAFilmSweepRunsApartFromTheRequest(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")
	thing := c.upload("The.Thing.1982.1080p.mkv", "/films", []byte("a film"))
	c.upload("Alien.1979.mkv", "/films", []byte("another film"))

	w, started := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/films"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %v", w.Code, started)
	}
	id, _ := started["id"].(string)
	if id == "" {
		t.Fatalf("the started run has no id: %v", started)
	}

	done := c.awaitMovieScan(id)
	if done["error"] != nil {
		t.Fatalf("the sweep failed: %v", done)
	}
	report, _ := done["result"].(map[string]any)
	if report["considered"] != 2.0 || report["matched"] != 2.0 || report["stopped"] != nil {
		t.Errorf("report = %v, want both films matched and not stopped", report)
	}
	_, got := c.json(http.MethodGet, "/api/files/"+thing["id"].(string)+"/movie", nil)
	if info, _ := got["movie"].(map[string]any); info == nil || info["title"] != "The Thing" {
		t.Errorf("stored = %v", got)
	}
}

func TestAFilmSweepCanBeWatchedJoinedAndStopped(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)
	for _, dir := range []string{"/films", "/other"} {
		c.json(http.MethodPost, "/api/folders", map[string]any{"path": dir})
		c.enableMovies(dir)
	}
	c.upload("The.Thing.1982.mkv", "/films", []byte("a film"))
	c.upload("Alien.1979.mkv", "/films", []byte("another film"))
	c.upload("Holiday.2019.mkv", "/films", []byte("not a film anyone made"))

	release := holdSearches(t, db)
	_, started := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/films"})
	id, _ := started["id"].(string)
	<-db.searching

	// Caught mid-film: the poll says where it is.
	_, status := c.json(http.MethodGet, "/api/movies/scanning", nil)
	progress, _ := status["progress"].(map[string]any)
	if status["running"] != true || progress["total"] != 3.0 || progress["path"] != "/films" || progress["current"] == "" {
		t.Errorf("status while running = %v", status)
	}

	// Asking again for the same folder is the same run.
	if w, joined := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/films"}); w.Code != http.StatusOK || joined["id"] != id {
		t.Errorf("joining: %d %v, want 200 and run %s", w.Code, joined, id)
	}
	// Anything else waits its turn.
	for _, req := range []map[string]any{{"path": "/other"}, {"path": "/films", "refresh": true}} {
		if w, body := c.json(http.MethodPost, "/api/movies/scanning", req); w.Code != http.StatusConflict {
			t.Errorf("%v while another runs: %d %v, want 409", req, w.Code, body)
		}
	}

	if w, _ := c.json(http.MethodDelete, "/api/movies/scanning", nil); w.Code != http.StatusOK {
		t.Errorf("stop: %d", w.Code)
	}
	release()

	done := c.awaitMovieScan(id)
	report, _ := done["result"].(map[string]any)
	if done["error"] != nil || report == nil {
		t.Fatalf("a stopped sweep should still report: %v", done)
	}
	if report["stopped"] != true {
		t.Errorf("report = %v, want stopped", report)
	}
	if considered, _ := report["considered"].(float64); considered >= 3 {
		t.Errorf("considered %v after a stop on the first film", considered)
	}
	if db.searches != 1 {
		t.Errorf("%d searches made, want the sweep to stop after the one in hand", db.searches)
	}

	// And a fresh one can start once it is over.
	if w, body := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/other"}); w.Code != http.StatusAccepted {
		t.Errorf("starting after a stop: %d %v", w.Code, body)
	}
}

func TestAFilmSweepIsRefusedAtOnceWhenItCouldNotRun(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})

	// Not opted in.
	if w, body := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/films"}); w.Code != http.StatusForbidden {
		t.Errorf("a folder never opted in: %d %v, want 403", w.Code, body)
	}
	if c.server.movieScans.status().ID != "" {
		t.Error("a refused sweep left a run behind")
	}

	// Opted in, but no key — which can only be cleared once it was set.
	db := newFakeMovieDB(t)
	c.withMovieDB(db)
	c.enableMovies("/films")
	c.json(http.MethodPost, "/api/movies/key", map[string]any{"key": ""})
	if w, body := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/films"}); w.Code != http.StatusPreconditionFailed {
		t.Errorf("no key: %d %v, want 412", w.Code, body)
	}
}

func TestLockingTheVaultForgetsTheFilmSweep(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")
	c.upload("Alien.1979.mkv", "/films", []byte("a film"))

	_, started := c.json(http.MethodPost, "/api/movies/scanning", map[string]any{"path": "/films"})
	c.awaitMovieScan(started["id"].(string))

	c.json(http.MethodPost, "/api/vault/lock", nil)
	if status := c.server.movieScans.status(); status.ID != "" {
		t.Errorf("a sweep read with the old keys is still held after locking: %+v", status)
	}
}
