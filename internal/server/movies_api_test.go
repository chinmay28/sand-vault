package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A stand-in for the film database, so these tests never leave the machine.
// The real one wants somebody's key and their servers to be up, neither of
// which belongs in a unit test.
type fakeMovieDB struct {
	*httptest.Server

	searches int // how many times a title was looked up
	details  int // how many times a film's record was fetched
	posters  int
}

func newFakeMovieDB(t *testing.T) *fakeMovieDB {
	t.Helper()
	db := &fakeMovieDB{}

	// A real JPEG, because the poster is normalized before it is stored and
	// bytes that will not decode are silently dropped.
	var poster bytes.Buffer
	if err := jpeg.Encode(&poster, image.NewRGBA(image.Rect(0, 0, 342, 513)), nil); err != nil {
		t.Fatalf("encoding the fixture poster: %v", err)
	}

	films := map[string]map[string]any{
		"the thing": {
			"id": 1091, "title": "The Thing", "release_date": "1982-06-25",
			"poster_path": "/thing.jpg", "vote_average": 8.2, "vote_count": 9000,
			"overview": "A research team in Antarctica.",
			"runtime":  109,
			"genres":   []map[string]any{{"name": "Horror"}},
			"credits": map[string]any{
				"crew": []map[string]any{{"name": "John Carpenter", "job": "Director"}},
				"cast": []map[string]any{{"name": "Kurt Russell", "character": "R.J. MacReady"}},
			},
		},
		"alien": {
			"id": 348, "title": "Alien", "release_date": "1979-05-25",
			"poster_path": "/alien.jpg", "vote_average": 8.4, "vote_count": 13000,
			"overview": "A crew answers a distress call.",
			"runtime":  117,
		},
	}
	byID := map[string]map[string]any{"1091": films["the thing"], "348": films["alien"]}

	mux := http.NewServeMux()
	mux.HandleFunc("/3/configuration", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") == "wrong-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"images": map[string]string{}})
	})
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		db.searches++
		query := strings.ToLower(r.URL.Query().Get("query"))
		results := []map[string]any{}
		for title, film := range films {
			if strings.Contains(query, title) {
				results = append(results, film)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/3/movie/", func(w http.ResponseWriter, r *http.Request) {
		db.details++
		film, ok := byID[strings.TrimPrefix(r.URL.Path, "/3/movie/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(film)
	})
	mux.HandleFunc("/t/p/", func(w http.ResponseWriter, r *http.Request) {
		db.posters++
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(poster.Bytes())
	})

	db.Server = httptest.NewServer(mux)
	t.Cleanup(db.Close)
	return db
}

// withMovieDB points a test client's film lookup at the fake and stores a key,
// which is what every test here needs before anything can be looked up.
func (c *testClient) withMovieDB(db *fakeMovieDB) {
	c.t.Helper()
	c.server.MovieBaseURL = db.URL + "/3"
	c.server.MovieImageBaseURL = db.URL + "/t/p"

	w, body := c.json(http.MethodPost, "/api/movies/key", map[string]any{"key": "test-key"})
	if w.Code != http.StatusOK {
		c.t.Fatalf("store the film database key: %d %v", w.Code, body)
	}
}

func (c *testClient) enableMovies(path string) {
	c.t.Helper()
	w, body := c.json(http.MethodPost, "/api/movies/lookup",
		map[string]any{"path": path, "enabled": true})
	if w.Code != http.StatusOK {
		c.t.Fatalf("turn film lookup on for %s: %d %v", path, w.Code, body)
	}
}

// Nothing is looked up in a folder nobody opted in — which is the whole
// consent model, so it is the first thing worth testing.
func TestFilmLookupIsOffUntilAFolderAsksForIt(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	file := c.upload("The.Thing.1982.1080p.BluRay.mkv", "/films", []byte("pretend this is a film"))
	id := file["id"].(string)

	w, _ := c.json(http.MethodPost, "/api/files/"+id+"/movie", map[string]any{})
	if w.Code != http.StatusForbidden {
		t.Errorf("matching a file in a folder that never opted in: %d, want 403", w.Code)
	}
	w, _ = c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})
	if w.Code == http.StatusOK {
		t.Error("a folder that never opted in was swept anyway")
	}
	if db.searches != 0 {
		t.Errorf("%d searches were made for a folder nobody opted in", db.searches)
	}

	// And the listing says so, so the browser never offers what would be refused.
	_, listing := c.json(http.MethodGet, "/api/files?path=/films", nil)
	lookup, _ := listing["movie_lookup"].(map[string]any)
	if enabled, _ := lookup["enabled"].(bool); enabled {
		t.Errorf("movie_lookup = %v, want off", lookup)
	}
}

func TestScanMatchesAFolderOfFilms(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")

	film := c.upload("The.Thing.1982.REMASTERED.1080p.BluRay.x265.mkv", "/films", []byte("a film"))
	// Not a video, so it is never considered — and never sent anywhere.
	c.upload("notes.txt", "/films", []byte("what to watch next"))

	w, report := c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %v", w.Code, report)
	}
	if considered, matched := report["considered"], report["matched"]; considered != 1.0 || matched != 1.0 {
		t.Errorf("considered = %v, matched = %v, want 1 and 1", considered, matched)
	}
	if report["artwork"] != 1.0 {
		t.Errorf("artwork = %v, want the poster stored", report["artwork"])
	}

	// The details are readable per file, with what was searched for alongside
	// them: a match is only judgeable against the query that produced it.
	id := film["id"].(string)
	_, got := c.json(http.MethodGet, "/api/files/"+id+"/movie", nil)
	info, _ := got["movie"].(map[string]any)
	if info == nil {
		t.Fatalf("no film stored: %v", got)
	}
	if info["title"] != "The Thing" || info["year"] != 1982.0 {
		t.Errorf("stored %v/%v", info["title"], info["year"])
	}
	if info["query"] != "The Thing (1982)" {
		t.Errorf("query = %v", info["query"])
	}
	if directors, _ := info["directors"].([]any); len(directors) != 1 || directors[0] != "John Carpenter" {
		t.Errorf("directors = %v", directors)
	}

	// The listing titles the file, so a grid of posters says what each one is.
	_, listing := c.json(http.MethodGet, "/api/files?path=/films", nil)
	movies, _ := listing["movies"].(map[string]any)
	brief, _ := movies[id].(map[string]any)
	if brief == nil || brief["title"] != "The Thing" {
		t.Errorf("listing movies = %v", movies)
	}
	if lookup, _ := listing["movie_lookup"].(map[string]any); lookup["enabled"] != true {
		t.Errorf("movie_lookup = %v", lookup)
	}

	// And the poster is the file's thumbnail, served the way every other
	// picture in the vault is.
	if w := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, ""); w.Code != http.StatusOK {
		t.Errorf("GET thumb: %d %s", w.Code, w.Body.String())
	}
	if thumbs, _ := listing["thumbs"].([]any); len(thumbs) != 1 {
		t.Errorf("listing thumbs = %v, want the poster", thumbs)
	}
}

// A folder inside a matched folder is matched too: a films library has folders
// in it, and the setting is a library rather than a single directory.
func TestFilmLookupIsInheritedBySubfolders(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films/Alien (1979)"})
	c.enableMovies("/films")
	c.upload("title00.mkv", "/films/Alien (1979)", []byte("a film"))

	w, report := c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %v", w.Code, report)
	}
	// The filename says nothing at all, so the match came off the folder name.
	if report["matched"] != 1.0 {
		t.Fatalf("matched = %v, want the film named by its folder: %v", report["matched"], report)
	}

	_, listing := c.json(http.MethodGet, "/api/files?path="+url.QueryEscape("/films/Alien (1979)"), nil)
	lookup, _ := listing["movie_lookup"].(map[string]any)
	if lookup["enabled"] != true || lookup["source"] != "/films" {
		t.Errorf("movie_lookup = %v, want inherited from /films", lookup)
	}
}

// Sweeping again must not re-search what is already matched: the database is
// somebody else's service, and a folder of two hundred films should cost
// nothing to look at twice.
func TestScanningAgainAsksTheDatabaseNothing(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")
	c.upload("Alien.1979.1080p.mkv", "/films", []byte("a film"))

	c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})
	searches, details, posters := db.searches, db.details, db.posters

	w, report := c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})
	if w.Code != http.StatusOK {
		t.Fatalf("second scan: %d %v", w.Code, report)
	}
	if report["skipped"] != 1.0 || report["matched"] != 0.0 {
		t.Errorf("skipped = %v, matched = %v, want the film left alone", report["skipped"], report["matched"])
	}
	if db.searches != searches || db.details != details || db.posters != posters {
		t.Errorf("the second sweep made %d searches, %d detail fetches and %d poster fetches",
			db.searches-searches, db.details-details, db.posters-posters)
	}
}

func TestCorrectingAMatchSticks(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")
	file := c.upload("The.Thing.1982.mkv", "/films", []byte("a film"))
	id := file["id"].(string)
	c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})

	// The candidate list is what the correction dialog picks from, and it
	// stores nothing.
	w, found := c.json(http.MethodGet, "/api/files/"+id+"/movie/candidates?q=alien", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("candidates: %d %v", w.Code, found)
	}
	if candidates, _ := found["candidates"].([]any); len(candidates) != 1 {
		t.Fatalf("candidates = %v", found["candidates"])
	}

	w, corrected := c.json(http.MethodPost, "/api/files/"+id+"/movie", map[string]any{"tmdb_id": 348})
	if w.Code != http.StatusOK {
		t.Fatalf("correct the match: %d %v", w.Code, corrected)
	}
	info, _ := corrected["movie"].(map[string]any)
	if info["title"] != "Alien" || info["manual"] != true {
		t.Fatalf("corrected to %v", info)
	}

	// Even a sweep asked to look everything up again leaves a hand-picked match
	// alone. That is the whole point of correcting one.
	c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films", "refresh": true})
	_, got := c.json(http.MethodGet, "/api/files/"+id+"/movie", nil)
	if info, _ := got["movie"].(map[string]any); info["title"] != "Alien" {
		t.Errorf("after a refresh the match became %v", info["title"])
	}
}

func TestForgettingAFilmAndDeletingItsFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")
	first := c.upload("The.Thing.1982.mkv", "/films", []byte("a film"))["id"].(string)
	second := c.upload("Alien.1979.mkv", "/films", []byte("another film"))["id"].(string)
	c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})

	if w := c.do(http.MethodDelete, "/api/files/"+first+"/movie", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", w.Code, w.Body.String())
	}
	_, got := c.json(http.MethodGet, "/api/files/"+first+"/movie", nil)
	if got["movie"] != nil {
		t.Errorf("the film is still stored: %v", got["movie"])
	}
	// Forgetting says what the file is, not what it was guessed to be, so the
	// guess is still there to look it up again with.
	if guess, _ := got["guess"].(map[string]any); guess["title"] != "The Thing" {
		t.Errorf("guess = %v", got["guess"])
	}

	// Deleting a file takes its details with it rather than leaving a title in
	// the index forever.
	if w := c.do(http.MethodDelete, "/api/files/"+second, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	_, listing := c.json(http.MethodGet, "/api/files?path=/films", nil)
	if movies, _ := listing["movies"].(map[string]any); len(movies) != 0 {
		t.Errorf("listing movies = %v, want none left", movies)
	}
}

func TestFilmDatabaseKeyIsCheckedAndNeverHandedBack(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	db := newFakeMovieDB(t)
	c.server.MovieBaseURL = db.URL + "/3"

	// A key the database refuses is refused here, rather than being stored and
	// failing halfway through a sweep of two hundred films.
	w, body := c.json(http.MethodPost, "/api/movies/key", map[string]any{"key": "wrong-key"})
	if w.Code != http.StatusBadGateway || body["code"] != "BAD_MOVIE_KEY" {
		t.Fatalf("wrong key: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodPost, "/api/movies/key", map[string]any{"key": "test-key"})
	if w.Code != http.StatusOK || body["has_key"] != true {
		t.Fatalf("good key: %d %v", w.Code, body)
	}

	// It is a credential, so the settings say that there is one and never what
	// it is.
	w, settings := c.json(http.MethodGet, "/api/movies", nil)
	if w.Code != http.StatusOK || settings["has_key"] != true {
		t.Fatalf("settings: %d %v", w.Code, settings)
	}
	if strings.Contains(w.Body.String(), "test-key") {
		t.Errorf("the key came back to the browser: %s", w.Body.String())
	}
}

// Without a key nothing can be looked up, whatever a folder says — and the
// browser is told which of the two is missing.
func TestScanWithoutAKey(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")
	c.upload("The.Thing.1982.mkv", "/films", []byte("a film"))

	w, body := c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"})
	if w.Code != http.StatusPreconditionFailed || body["code"] != "NO_MOVIE_KEY" {
		t.Fatalf("scan without a key: %d %v", w.Code, body)
	}
}
