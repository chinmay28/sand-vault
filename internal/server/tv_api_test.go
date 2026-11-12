package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tvDo makes a request the way the TV does: a bearer token, never the
// browser's cookie, and no Origin header.
func (c *testClient) tvDo(method, path, token string, payload any) (*httptest.ResponseRecorder, map[string]any) {
	c.t.Helper()
	var body io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = "example.test"
	req.RemoteAddr = "192.168.1.50:41000"
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	out := map[string]any{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// mintTVCode asks for a code the way the owner's dialog does.
func (c *testClient) mintTVCode(path string, minutes int) string {
	c.t.Helper()
	w, body := c.json(http.MethodPost, "/api/tv/codes", map[string]any{"path": path, "minutes": minutes})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("mint TV code for %s: %d %v", path, w.Code, body)
	}
	code, _ := body["code"].(string)
	if len(code) != 4 {
		c.t.Fatalf("code = %q, want four characters", code)
	}
	return code
}

// pairTV redeems a code and returns the device token.
func (c *testClient) pairTV(code string) string {
	c.t.Helper()
	w, body := c.tvDo(http.MethodPost, "/api/tv/pair", "", map[string]any{"code": code})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("pair with %s: %d %v", code, w.Code, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		c.t.Fatalf("pairing returned no token: %v", body)
	}
	return token
}

// tvLibrary sets up a vault with films inside /films and a private folder
// beside it, the shape every test below is about.
func tvLibrary(t *testing.T) (c *testClient, films map[string]string) {
	t.Helper()
	c = newTestClient(t)
	c.setup("pw", 3)
	for _, dir := range []string{"/films", "/films/Alien (1979)", "/films/empty", "/private"} {
		c.json(http.MethodPost, "/api/folders", map[string]any{"path": dir})
	}
	films = map[string]string{
		"thing":   c.upload("The.Thing.1982.1080p.mkv", "/films", []byte("the thing"))["id"].(string),
		"alien":   c.upload("Alien.1979.mp4", "/films/Alien (1979)", []byte("alien, byte for byte"))["id"].(string),
		"notes":   c.upload("notes.txt", "/films", []byte("not a film"))["id"].(string),
		"private": c.upload("home-video.mp4", "/private", []byte("nobody else's"))["id"].(string),
	}
	return c, films
}

func TestATVCodeOpensOneFolderOfVideos(t *testing.T) {
	c, films := tvLibrary(t)
	token := c.pairTV(c.mintTVCode("/films", 60))

	w, lib := c.tvDo(http.MethodGet, "/api/tv/library", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("library: %d %v", w.Code, lib)
	}
	if lib["label"] != "films" || lib["path"] != "/" {
		t.Errorf("label %v, path %v — the TV should see its folder as the root", lib["label"], lib["path"])
	}

	items := lib["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != films["thing"] {
		t.Errorf("items = %v, want only the film in /films — not the notes", items)
	}
	// Only the subfolder that holds a video is offered.
	folders := lib["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("folders = %v, want just Alien (1979)", folders)
	}
	f := folders[0].(map[string]any)
	if f["name"] != "Alien (1979)" || f["path"] != "/Alien (1979)" || f["count"] != 1.0 {
		t.Errorf("folder = %v", f)
	}

	// Into the subfolder, by its path relative to the grant.
	_, sub := c.tvDo(http.MethodGet, "/api/tv/library?path=/Alien%20(1979)", token, nil)
	subItems := sub["items"].([]any)
	if len(subItems) != 1 || subItems[0].(map[string]any)["id"] != films["alien"] {
		t.Errorf("subfolder items = %v", subItems)
	}
	if sub["parent"] != "/" {
		t.Errorf("parent = %v, want /", sub["parent"])
	}

	// And every film at once.
	_, all := c.tvDo(http.MethodGet, "/api/tv/library?all=1", token, nil)
	if n := len(all["items"].([]any)); n != 2 {
		t.Errorf("all films: %d items, want 2", n)
	}
	if n := len(all["folders"].([]any)); n != 0 {
		t.Errorf("all films should be a flat list, got %d folders", n)
	}
}

// The whole point of the grant: nothing outside the folder is reachable, by
// path or by ID.
func TestATVCannotLeaveItsFolder(t *testing.T) {
	c, films := tvLibrary(t)
	token := c.pairTV(c.mintTVCode("/films", 60))

	for _, path := range []string{"/..", "/../private", "/Alien (1979)/../../private"} {
		// Resolved beneath the folder — /films/private, which does not exist —
		// so a 404 or a listing of /films, never /private.
		_, lib := c.tvDo(http.MethodGet, "/api/tv/library?path="+strings.ReplaceAll(path, " ", "%20"), token, nil)
		items, _ := lib["items"].([]any)
		for _, raw := range items {
			if raw.(map[string]any)["id"] == films["private"] {
				t.Errorf("library?path=%s reached /private", path)
			}
		}
	}

	for _, name := range []string{"private", "notes"} {
		id := films[name]
		for _, route := range []string{"/api/tv/items/" + id, "/api/tv/items/" + id + "/poster"} {
			if w, _ := c.tvDo(http.MethodGet, route, token, nil); w.Code != http.StatusNotFound {
				t.Errorf("GET %s (%s) = %d, want 404", route, name, w.Code)
			}
		}
		if w, _ := c.tvDo(http.MethodPost, "/api/tv/items/"+id+"/play", token, nil); w.Code != http.StatusNotFound {
			t.Errorf("play %s = %d, want 404", name, w.Code)
		}
	}

	// And the owner's API is not reachable with a TV token.
	for _, route := range []string{"/api/files?path=/private", "/api/tv/codes", "/api/providers"} {
		if w, _ := c.tvDo(http.MethodGet, route, token, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a TV token = %d, want 401", route, w.Code)
		}
	}
}

func TestATVPlaysAFilmThroughAStreamLink(t *testing.T) {
	c, films := tvLibrary(t)
	token := c.pairTV(c.mintTVCode("/films", 60))

	w, item := c.tvDo(http.MethodGet, "/api/tv/items/"+films["alien"], token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("item: %d %v", w.Code, item)
	}
	if item["folder"] != "/Alien (1979)" || item["name"] != "Alien.1979.mp4" {
		t.Errorf("item = %v", item)
	}

	w, link := c.tvDo(http.MethodPost, "/api/tv/items/"+films["alien"]+"/play", token, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("play: %d %v", w.Code, link)
	}
	url := link["url"].(string)

	// The player follows the link with nothing else: no token, no cookie.
	sw, _ := c.tvDo(http.MethodGet, url, "", nil)
	if sw.Code != http.StatusOK || sw.Body.String() != "alien, byte for byte" {
		t.Fatalf("stream: %d %q", sw.Code, sw.Body.String())
	}

	// Ranges, because that is how a player seeks.
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Host = "example.test"
	req.Header.Set("Range", "bytes=0-4")
	rw := httptest.NewRecorder()
	c.handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusPartialContent || rw.Body.String() != "alien" {
		t.Errorf("range: %d %q", rw.Code, rw.Body.String())
	}
}

// Revoking the code ends everything it opened: the pairing, and the film that
// is playing on it.
func TestRevokingACodeUnpairsTheTVAndStopsItsStreams(t *testing.T) {
	c, films := tvLibrary(t)
	code := c.mintTVCode("/films", 60)
	token := c.pairTV(code)
	_, link := c.tvDo(http.MethodPost, "/api/tv/items/"+films["thing"]+"/play", token, nil)
	url := link["url"].(string)

	w, _ := c.json(http.MethodDelete, "/api/tv/codes/"+strings.ToLower(code), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}

	if w, body := c.tvDo(http.MethodGet, "/api/tv/library", token, nil); w.Code != http.StatusUnauthorized || body["code"] != "TV_UNPAIRED" {
		t.Errorf("library after revoke = %d %v, want 401 TV_UNPAIRED", w.Code, body)
	}
	if w, _ := c.tvDo(http.MethodGet, url, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("stream after revoke = %d, want 404", w.Code)
	}
	if w, _ := c.tvDo(http.MethodPost, "/api/tv/pair", "", map[string]any{"code": code}); w.Code != http.StatusUnauthorized {
		t.Errorf("pairing with a revoked code = %d, want 401", w.Code)
	}
	if w, _ := c.json(http.MethodDelete, "/api/tv/codes/"+code, nil); w.Code != http.StatusNotFound {
		t.Errorf("revoking twice = %d, want 404", w.Code)
	}
}

func TestTheOwnerListsLiveCodes(t *testing.T) {
	c, _ := tvLibrary(t)
	code := c.mintTVCode("/films", 90)
	c.pairTV(code)

	w, body := c.json(http.MethodGet, "/api/tv/codes", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %v", w.Code, body)
	}
	codes := body["codes"].([]any)
	if len(codes) != 1 {
		t.Fatalf("codes = %v", codes)
	}
	got := codes[0].(map[string]any)
	if got["code"] != code || got["path"] != "/films" || got["devices"] != 1.0 || got["label"] != "films" {
		t.Errorf("code = %v", got)
	}
	expires, _ := time.Parse(time.RFC3339Nano, got["expires_at"].(string))
	if d := time.Until(expires); d < 89*time.Minute || d > 91*time.Minute {
		t.Errorf("expires in %s, want about 90 minutes", d)
	}
	if _, ok := body["addresses"].([]any); !ok {
		t.Errorf("addresses missing: %v", body)
	}
}

func TestMintingACodeNeedsTheSessionAndARealFolder(t *testing.T) {
	c, _ := tvLibrary(t)

	if w, _ := c.json(http.MethodPost, "/api/tv/codes", map[string]any{"path": "/nowhere"}); w.Code != http.StatusNotFound {
		t.Errorf("code for a missing folder = %d, want 404", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/tv/codes", map[string]any{"path": "/films", "minutes": 1}); w.Code != http.StatusBadRequest {
		t.Errorf("a one-minute code = %d, want 400", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/tv/codes", map[string]any{"path": "/films", "minutes": 60 * 24 * 365}); w.Code != http.StatusBadRequest {
		t.Errorf("a year-long code = %d, want 400", w.Code)
	}

	// Without the browser's cookie, nobody mints anything.
	if w, _ := c.tvDo(http.MethodPost, "/api/tv/codes", "", map[string]any{"path": "/films"}); w.Code != http.StatusUnauthorized {
		t.Errorf("minting without a session = %d, want 401", w.Code)
	}
}

func TestPairingIsThrottledAfterWrongCodes(t *testing.T) {
	c, _ := tvLibrary(t)
	code := c.mintTVCode("/films", 60)

	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last, _ = c.tvDo(http.MethodPost, "/api/tv/pair", "", map[string]any{"code": "ZZZZ"})
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("sixth wrong code = %d (Retry-After %q), want 429 with a wait", last.Code, last.Header().Get("Retry-After"))
	}
	// The right code is refused too while the throttle holds.
	if w, body := c.tvDo(http.MethodPost, "/api/tv/pair", "", map[string]any{"code": code}); w.Code != http.StatusTooManyRequests {
		t.Errorf("right code while throttled = %d %v, want 429", w.Code, body)
	}
}

// A locked vault holds the TV at "unlock it on your computer" without
// throwing its pairing away, and unlocking is all it takes to carry on.
func TestATVWaitsOutALockedVault(t *testing.T) {
	c, _ := tvLibrary(t)
	token := c.pairTV(c.mintTVCode("/films", 60))

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	w, s := c.tvDo(http.MethodGet, "/api/tv/session", token, nil)
	if w.Code != http.StatusOK || s["locked"] != true {
		t.Errorf("session while locked = %d %v, want locked", w.Code, s)
	}
	if w, body := c.tvDo(http.MethodGet, "/api/tv/library", token, nil); w.Code != http.StatusUnauthorized || body["code"] != "LOCKED" {
		t.Errorf("library while locked = %d %v, want 401 LOCKED", w.Code, body)
	}
	// Polling the session while locked must not hold the vault open.
	if c.server.externalActive() {
		t.Error("asking whether the vault is locked counted as using it")
	}

	if w, _ := c.json(http.MethodPost, "/api/vault/unlock", map[string]any{"password": "pw"}); w.Code != http.StatusOK {
		t.Fatalf("unlock: %d", w.Code)
	}
	if w, _ := c.tvDo(http.MethodGet, "/api/tv/library", token, nil); w.Code != http.StatusOK {
		t.Errorf("library after unlock = %d, want the same pairing to carry on", w.Code)
	}
	// And browsing is use, so the vault does not lock under somebody choosing
	// a film.
	if !c.server.externalActive() {
		t.Error("browsing on the TV did not count as using the vault")
	}
}

func TestSigningOutUnpairsOnlyThisTV(t *testing.T) {
	c, _ := tvLibrary(t)
	code := c.mintTVCode("/films", 60)
	lounge, bedroom := c.pairTV(code), c.pairTV(code)

	if w, _ := c.tvDo(http.MethodPost, "/api/tv/logout", lounge, nil); w.Code != http.StatusOK {
		t.Fatalf("logout: %d", w.Code)
	}
	if w, _ := c.tvDo(http.MethodGet, "/api/tv/session", lounge, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("signed-out TV = %d, want 401", w.Code)
	}
	if w, _ := c.tvDo(http.MethodGet, "/api/tv/session", bedroom, nil); w.Code != http.StatusOK {
		t.Errorf("the other TV = %d, want still paired", w.Code)
	}
}

func TestTVRoutesRefuseAMissingOrForeignToken(t *testing.T) {
	c, _ := tvLibrary(t)
	for _, tok := range []string{"", "not-a-token"} {
		w, body := c.tvDo(http.MethodGet, "/api/tv/library", tok, nil)
		if w.Code != http.StatusUnauthorized || body["code"] != "TV_UNPAIRED" {
			t.Errorf("token %q: %d %v, want 401 TV_UNPAIRED", tok, w.Code, body)
		}
	}
}

// Film details are what make the TV a library rather than a file list.
func TestATVSeesFilmDetailsAndPosters(t *testing.T) {
	c, films := tvLibrary(t)
	db := newFakeMovieDB(t)
	c.withMovieDB(db)
	c.enableMovies("/films")
	if w, report := c.json(http.MethodPost, "/api/movies/scan", map[string]any{"path": "/films"}); w.Code != http.StatusOK {
		t.Fatalf("scan: %d %v", w.Code, report)
	}

	token := c.pairTV(c.mintTVCode("/films", 60))
	_, lib := c.tvDo(http.MethodGet, "/api/tv/library", token, nil)
	row := lib["items"].([]any)[0].(map[string]any)
	if row["title"] != "The Thing" || row["year"] != 1982.0 || row["poster"] != true {
		t.Errorf("library row = %v", row)
	}

	_, item := c.tvDo(http.MethodGet, "/api/tv/items/"+films["thing"], token, nil)
	info, _ := item["movie"].(map[string]any)
	if info == nil || info["overview"] != "A research team in Antarctica." || info["runtime"] != 109.0 {
		t.Fatalf("item movie = %v", item["movie"])
	}
	if _, leaked := info["query"]; leaked {
		t.Errorf("the match query is the owner's business, not the TV's: %v", info)
	}

	w, _ := c.tvDo(http.MethodGet, "/api/tv/items/"+films["thing"]+"/poster", token, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("poster: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestTheTVPageIsServed(t *testing.T) {
	c := newTestClient(t)
	w := c.do(http.MethodGet, "/tv", nil, "")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/tv/" {
		t.Errorf("/tv = %d → %q", w.Code, w.Header().Get("Location"))
	}
	for path, want := range map[string]string{"/tv/": "SAND TV", "/tv/tv.js": "SandTVNative", "/tv/tv.css": "--accent"} {
		w := c.do(http.MethodGet, path, nil, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("GET %s = %d, missing %q", path, w.Code, want)
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("GET %s CSP = %q", path, csp)
		}
	}
}

func TestPathConfinement(t *testing.T) {
	for _, tc := range []struct{ root, rel, want string }{
		{"/films", "/", "/films"},
		{"/films", "/a/b", "/films/a/b"},
		{"/films", "/../private", "/films/private"},
		{"/films", "../../x", "/films/x"},
		{"/", "/a", "/a"},
		{"/", "/", "/"},
	} {
		if got := confine(tc.root, tc.rel); got != tc.want {
			t.Errorf("confine(%q, %q) = %q, want %q", tc.root, tc.rel, got, tc.want)
		}
	}

	for _, tc := range []struct {
		dir, root string
		want      bool
	}{
		{"/films", "/films", true},
		{"/films/a", "/films", true},
		{"/filmsextra", "/films", false},
		{"/private", "/films", false},
		{"/anything", "/", true},
	} {
		if got := within(tc.dir, tc.root); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.dir, tc.root, got, tc.want)
		}
	}

	for _, tc := range []struct {
		root, dir, want string
		ok              bool
	}{
		{"/films", "/films", "/", true},
		{"/films", "/films/a/b", "/a/b", true},
		{"/films", "/private", "", false},
		{"/", "/x", "/x", true},
	} {
		got, ok := relativeTo(tc.root, tc.dir)
		if got != tc.want || ok != tc.ok {
			t.Errorf("relativeTo(%q, %q) = %q, %v", tc.root, tc.dir, got, ok)
		}
	}
}

func TestDiscoveryIsOffOnALoopbackBind(t *testing.T) {
	for _, tc := range []struct {
		bind string
		port int
		want int
	}{
		{"0.0.0.0", 8124, 8124},
		{"127.0.0.1", 8124, 0},
		{"0.0.0.0", 0, 0},
	} {
		s := &Server{Bind: tc.bind, TVDiscoveryPort: tc.port}
		if got := s.discoveryPort(); got != tc.want {
			t.Errorf("bind %s port %d: discoveryPort() = %d, want %d", tc.bind, tc.port, got, tc.want)
		}
	}
}
