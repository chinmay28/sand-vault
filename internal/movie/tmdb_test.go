package movie

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDatabase stands in for the real one. Every test here drives the client
// against it, because a test that reached the real database would need somebody
// else's key and their servers to be up.
type fakeDatabase struct {
	*httptest.Server

	// keys seen on the way in, so a test can check how the key travelled.
	queryKeys  []string
	bearerKeys []string

	// searches is every query string asked for, in order.
	searches []string
}

func newFakeDatabase(t *testing.T) *fakeDatabase {
	t.Helper()
	db := &fakeDatabase{}

	mux := http.NewServeMux()
	mux.HandleFunc("/3/configuration", func(w http.ResponseWriter, r *http.Request) {
		if !db.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"images": map[string]string{"secure_base_url": "https://example.invalid/t/p/"},
		})
	})

	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		if !db.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		query := r.URL.Query()
		db.searches = append(db.searches, query.Get("query")+"|"+query.Get("primary_release_year"))

		// The year in the fixture is 1982, so a search pinned to any other year
		// finds nothing — which is what makes the retry visible.
		if year := query.Get("primary_release_year"); year != "" && year != "1982" {
			json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
			return
		}
		if !strings.Contains(strings.ToLower(query.Get("query")), "thing") {
			json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{
				"id": 1091, "title": "The Thing", "original_title": "The Thing",
				"release_date": "1982-06-25", "overview": "A research team in Antarctica.",
				"poster_path": "/poster.jpg", "vote_average": 8.2, "vote_count": 9000,
			},
		}})
	})

	mux.HandleFunc("/3/movie/1091", func(w http.ResponseWriter, r *http.Request) {
		if !db.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1091, "imdb_id": "tt0084787", "title": "The Thing",
			"original_title": "The Thing", "release_date": "1982-06-25",
			"tagline": "Man is the warmest place to hide.", "runtime": 109,
			"overview": "A research team in Antarctica.", "poster_path": "/poster.jpg",
			"vote_average": 8.2, "vote_count": 9000,
			"genres": []map[string]any{{"name": "Horror"}, {"name": "Science Fiction"}},
			"credits": map[string]any{
				"cast": []map[string]any{
					{"name": "Kurt Russell", "character": "R.J. MacReady", "order": 0},
					{"name": "Wilford Brimley", "character": "Blair", "order": 1},
				},
				"crew": []map[string]any{
					{"name": "John Carpenter", "job": "Director"},
					{"name": "Bill Lancaster", "job": "Screenplay"},
				},
			},
		})
	})

	mux.HandleFunc("/3/movie/404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	// The image host, served from the same test server under its own prefix.
	mux.HandleFunc("/t/p/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/"+PosterSize+"/poster.jpg") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("pretend this is a poster"))
	})

	db.Server = httptest.NewServer(mux)
	t.Cleanup(db.Close)
	return db
}

func (db *fakeDatabase) authorized(r *http.Request) bool {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		token := strings.TrimPrefix(header, "Bearer ")
		db.bearerKeys = append(db.bearerKeys, token)
		return token != "wrong"
	}
	key := r.URL.Query().Get("api_key")
	db.queryKeys = append(db.queryKeys, key)
	return key != "" && key != "wrong"
}

func (db *fakeDatabase) client(key string) *Client {
	return &Client{
		Key:          key,
		BaseURL:      db.URL + "/3",
		ImageBaseURL: db.URL + "/t/p",
	}
}

func TestLookupReadsAFilm(t *testing.T) {
	db := newFakeDatabase(t)

	info, err := db.client("abc123").Lookup(context.Background(),
		Guess{Title: "The Thing", Year: 1982})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info == nil {
		t.Fatal("no film came back")
	}

	if info.TMDBID != 1091 || info.Title != "The Thing" || info.Year != 1982 {
		t.Errorf("got %d/%q/%d", info.TMDBID, info.Title, info.Year)
	}
	if info.Runtime != 109 || info.IMDBID != "tt0084787" {
		t.Errorf("runtime = %d, imdb = %q", info.Runtime, info.IMDBID)
	}
	if len(info.Genres) != 2 || info.Genres[0] != "Horror" {
		t.Errorf("genres = %v", info.Genres)
	}
	// The director comes out of the crew and only the director does.
	if len(info.Directors) != 1 || info.Directors[0] != "John Carpenter" {
		t.Errorf("directors = %v", info.Directors)
	}
	if len(info.Cast) != 2 || info.Cast[0].Role != "R.J. MacReady" {
		t.Errorf("cast = %v", info.Cast)
	}
	// The original title is dropped when it says nothing the title did not.
	if info.Original != "" {
		t.Errorf("original title = %q, want it dropped as a repeat", info.Original)
	}
	// What was searched for is kept, because the details view has to be able to
	// say how the match was made.
	if info.Query != "The Thing (1982)" {
		t.Errorf("query = %q", info.Query)
	}
}

// A year the database disagrees with is a filename that got the year wrong, not
// a film that does not exist — so the search is made again without it.
func TestSearchRetriesWithoutTheYear(t *testing.T) {
	db := newFakeDatabase(t)

	candidates, err := db.client("abc123").Search(context.Background(),
		Guess{Title: "The Thing", Year: 2011})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 1 || candidates[0].TMDBID != 1091 {
		t.Fatalf("candidates = %+v", candidates)
	}
	if len(db.searches) != 2 {
		t.Fatalf("searches = %v, want the pinned one and the retry", db.searches)
	}
	if db.searches[0] != "The Thing|2011" || db.searches[1] != "The Thing|" {
		t.Errorf("searches = %v", db.searches)
	}
}

func TestLookupOfSomethingThatIsNotAFilm(t *testing.T) {
	db := newFakeDatabase(t)

	info, err := db.client("abc123").Lookup(context.Background(), Guess{Title: "kitchen renovation"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info != nil {
		t.Errorf("got %+v, want nothing — an unmatched file is not an error", info)
	}
}

func TestKeyTravelsByShape(t *testing.T) {
	db := newFakeDatabase(t)

	// A v3 key: a bare string, sent as a query parameter.
	if err := db.client("abc123").Ping(context.Background()); err != nil {
		t.Fatalf("Ping with a v3 key: %v", err)
	}
	if len(db.queryKeys) != 1 || db.queryKeys[0] != "abc123" {
		t.Errorf("query keys = %v", db.queryKeys)
	}

	// A v4 read token: a JWT, sent as a bearer header and never in the URL.
	token := "eyJhbGciOi.eyJhdWQiOi.signature"
	if err := db.client(token).Ping(context.Background()); err != nil {
		t.Fatalf("Ping with a v4 token: %v", err)
	}
	if len(db.bearerKeys) != 1 || db.bearerKeys[0] != token {
		t.Errorf("bearer keys = %v", db.bearerKeys)
	}
	if len(db.queryKeys) != 1 {
		t.Errorf("the read token was also put in the query: %v", db.queryKeys)
	}
}

func TestKeyFailuresAreToldApart(t *testing.T) {
	db := newFakeDatabase(t)

	if err := (&Client{BaseURL: db.URL + "/3"}).Ping(context.Background()); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: %v, want ErrNoKey", err)
	}
	if err := db.client("wrong").Ping(context.Background()); !errors.Is(err, ErrRejectedKey) {
		t.Errorf("wrong key: %v, want ErrRejectedKey", err)
	}
	if _, err := db.client("abc123").Details(context.Background(), 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing film: %v, want ErrNotFound", err)
	}
}

func TestPosterFetch(t *testing.T) {
	db := newFakeDatabase(t)

	data, err := db.client("abc123").Poster(context.Background(), "/poster.jpg")
	if err != nil {
		t.Fatalf("Poster: %v", err)
	}
	if string(data) != "pretend this is a poster" {
		t.Errorf("poster = %q", data)
	}

	// A film with no artwork, and a path that tries to be more than a filename.
	if _, err := db.client("abc123").Poster(context.Background(), ""); err == nil {
		t.Error("an empty poster path was accepted")
	}
	for _, bad := range []string{"/../../etc/passwd", "/a/b.jpg", "//evil.example/x.jpg"} {
		if _, err := db.client("abc123").Poster(context.Background(), bad); err == nil {
			t.Errorf("Poster(%q) was accepted", bad)
		}
	}
}

// The key travels in the URL for a v3 key, and net/http puts the URL in the
// errors it builds. Nothing that comes back out of this package may carry it.
func TestTransportErrorsDoNotLeakTheKey(t *testing.T) {
	client := &Client{Key: "s3cret-key", BaseURL: "http://127.0.0.1:1/3"}

	err := client.Ping(context.Background())
	if err == nil {
		t.Fatal("expected the connection to fail")
	}
	if strings.Contains(err.Error(), "s3cret-key") {
		t.Errorf("the key is in the error: %v", err)
	}
}
