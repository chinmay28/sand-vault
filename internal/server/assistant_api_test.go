package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chinmay28/sand-vault/internal/movie"
)

// A stand-in for the model server, speaking the chat completions protocol
// the way Ollama and vLLM do, and playing a model that answers the Batman
// question the way a real one does: list the films, search the database,
// compare. The comparison is made from the tool results it is actually sent,
// so the test proves the tools over a real vault say the right things.
type fakeModelServer struct {
	*httptest.Server

	mu       sync.Mutex
	models   []string
	requests []map[string]any
}

func newFakeModelServer(t *testing.T) *fakeModelServer {
	t.Helper()
	f := &fakeModelServer{models: []string{"qwen3:14b"}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := []map[string]any{}
		for _, m := range f.models {
			data = append(data, map[string]any{"id": m})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()

		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": f.reply(req)}},
		})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// reply is the scripted model.
func (f *fakeModelServer) reply(req map[string]any) map[string]any {
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)

	call := func(id, name, args string) map[string]any {
		return map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}
	}

	switch {
	case last["role"] == "user":
		question := strings.ToLower(last["content"].(string))
		if strings.Contains(question, "missing") {
			return call("c1", "list_films", `{}`)
		}
		if strings.Contains(question, "called") {
			return call("c1", "search_vault", `{"query":"batman"}`)
		}
		return map[string]any{"role": "assistant", "content": "Ask me about your films."}
	case last["role"] == "tool" && last["name"] == "list_films":
		return call("c2", "search_film_database", `{"query":"Batman"}`)
	case last["role"] == "tool" && last["name"] == "search_vault":
		var res struct {
			Hits []struct {
				Path string `json:"path"`
				Film string `json:"film"`
			} `json:"hits"`
		}
		json.Unmarshal([]byte(last["content"].(string)), &res)
		lines := []string{}
		for _, h := range res.Hits {
			lines = append(lines, h.Path+" = "+h.Film)
		}
		return map[string]any{"role": "assistant", "content": strings.Join(lines, "\n")}
	}

	var have, known struct {
		Films []struct {
			Title string `json:"title"`
			Year  int    `json:"year"`
		} `json:"films"`
	}
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] != "tool" {
			continue
		}
		switch m["name"] {
		case "list_films":
			json.Unmarshal([]byte(m["content"].(string)), &have)
		case "search_film_database":
			json.Unmarshal([]byte(m["content"].(string)), &known)
		}
	}
	owned := map[string]bool{}
	for _, film := range have.Films {
		owned[strings.ToLower(film.Title)] = true
	}
	missing := []string{}
	for _, film := range known.Films {
		if !owned[strings.ToLower(film.Title)] {
			missing = append(missing, fmt.Sprintf("- %s (%d)", film.Title, film.Year))
		}
	}
	return map[string]any{
		"role":    "assistant",
		"content": "Missing from your collection:\n" + strings.Join(missing, "\n"),
	}
}

// batmanDB is a film database that knows four Batman films and nothing else.
func batmanDB(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/3/configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"images": map[string]string{}})
	})
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.URL.Query().Get("query"), "batman") {
			json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"id": 268, "title": "Batman", "release_date": "1989-06-23", "overview": "Gotham's caped crusader."},
			{"id": 364, "title": "Batman Returns", "release_date": "1992-06-19"},
			{"id": 155, "title": "The Dark Knight", "release_date": "2008-07-16"},
			{"id": 414906, "title": "The Batman", "release_date": "2022-03-01"},
		}})
	})
	db := httptest.NewServer(mux)
	t.Cleanup(db.Close)
	return db
}

func (c *testClient) withAssistant(f *fakeModelServer) {
	c.t.Helper()
	w, body := c.json(http.MethodPost, "/api/assistant/settings",
		map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b"})
	if w.Code != http.StatusOK {
		c.t.Fatalf("set up the assistant: %d %v", w.Code, body)
	}
}

// A collection with two of the four Batman films, and one film that is not
// a Batman film at all.
func (c *testClient) batmanCollection(db *httptest.Server) {
	c.t.Helper()
	c.server.MovieBaseURL = db.URL + "/3"
	c.server.MovieImageBaseURL = db.URL + "/t/p"
	w, body := c.json(http.MethodPost, "/api/movies/key", map[string]any{"key": "test-key"})
	if w.Code != http.StatusOK {
		c.t.Fatalf("store the film database key: %d %v", w.Code, body)
	}

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.enableMovies("/films")

	v, _ := c.server.Vault()
	for _, film := range []struct {
		name  string
		title string
		year  int
		tmdb  int
	}{
		{"Batman.1989.1080p.mkv", "Batman", 1989, 268},
		{"The.Dark.Knight.2008.mkv", "The Dark Knight", 2008, 155},
		{"Alien.1979.mkv", "Alien", 1979, 348},
	} {
		file := c.upload(film.name, "/films", []byte("pretend this is "+film.title))
		if err := v.SetMovie(file["id"].(string), &movie.Info{
			TMDBID: film.tmdb, Title: film.title, Year: film.year,
		}); err != nil {
			c.t.Fatalf("SetMovie %s: %v", film.title, err)
		}
	}
}

func TestAskingNeedsAnAssistantSetUpFirst(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 2)

	w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "what do I have?"}},
	})
	if w.Code != http.StatusPreconditionFailed || body["code"] != "NO_ASSISTANT" {
		t.Fatalf("asking with nothing set up: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodGet, "/api/assistant", nil)
	if w.Code != http.StatusOK || body["configured"] != false {
		t.Errorf("settings of a fresh vault: %d %v", w.Code, body)
	}
}

func TestAssistantSettingsAreCheckedBeforeTheyAreStored(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 2)
	f := newFakeModelServer(t)

	w, body := c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": "gaming-pc:11434", "model": "qwen3:14b"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a URL with no scheme: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": f.URL + "/v1", "model": ""})
	if w.Code != http.StatusBadRequest {
		t.Errorf("no model named: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": f.URL + "/v1", "model": "mistral"})
	if w.Code != http.StatusBadGateway || body["code"] != "NO_SUCH_MODEL" {
		t.Errorf("a model the server does not have: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": "http://127.0.0.1:1/v1", "model": "qwen3:14b"})
	if w.Code != http.StatusBadGateway || !strings.Contains(body["error"].(string), "could not reach") {
		t.Errorf("a server that is not there: %d %v", w.Code, body)
	}

	// Nothing above was stored.
	if _, body = c.json(http.MethodGet, "/api/assistant", nil); body["configured"] != false {
		t.Errorf("a rejected setting was stored: %v", body)
	}

	key := "secret-token"
	w, body = c.json(http.MethodPost, "/api/assistant/settings",
		map[string]any{"url": f.URL + "/v1/", "model": "qwen3:14b", "key": key})
	if w.Code != http.StatusOK || body["configured"] != true || body["has_key"] != true {
		t.Fatalf("a good setting: %d %v", w.Code, body)
	}
	if body["url"] != f.URL+"/v1" || body["model"] != "qwen3:14b" {
		t.Errorf("stored as %v", body)
	}
	if _, echoed := body["key"]; echoed {
		t.Error("the token was echoed back")
	}

	// Changing the model keeps the token; sending an empty one clears it.
	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b"})
	if w.Code != http.StatusOK || body["has_key"] != true {
		t.Errorf("an update without a key field dropped the token: %d %v", w.Code, body)
	}
	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b", "key": ""})
	if w.Code != http.StatusOK || body["has_key"] != false {
		t.Errorf("an empty key did not clear the token: %d %v", w.Code, body)
	}

	// An empty URL clears everything, and needs no server to answer.
	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": "", "model": "whatever"})
	if w.Code != http.StatusOK || body["configured"] != false || body["model"] != "" {
		t.Errorf("clearing: %d %v", w.Code, body)
	}
}

func TestWhichBatmanFilmsAreMissing(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	f := newFakeModelServer(t)
	c.withAssistant(f)
	c.batmanCollection(batmanDB(t))

	w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{
			{"role": "user", "content": "what Batman movies are missing from my collection?"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ask: %d %v", w.Code, body)
	}

	text := body["text"].(string)
	if !strings.Contains(text, "Batman Returns (1992)") || !strings.Contains(text, "The Batman (2022)") {
		t.Errorf("the two missing films were not named: %q", text)
	}
	if strings.Contains(text, "Dark Knight") || strings.Contains(text, "Alien") {
		t.Errorf("a film the vault holds was called missing: %q", text)
	}

	steps := body["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps %v, want list_films then search_film_database", steps)
	}
	if steps[0].(map[string]any)["tool"] != "list_films" || steps[1].(map[string]any)["tool"] != "search_film_database" {
		t.Errorf("steps %v", steps)
	}

	// What the model server was sent: the film list carried titles and
	// paths, and the system prompt led; nothing that looks like a file's
	// contents or an account went over.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 3 {
		t.Fatalf("model was asked %d times, want 3", len(f.requests))
	}
	first := f.requests[0]["messages"].([]any)[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "Answer only from what the tools return") {
		t.Errorf("the transcript did not open with the standing instructions: %v", first)
	}
	raw, _ := json.Marshal(f.requests[2])
	sent := string(raw)
	var listed string
	for _, m := range f.requests[2]["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" && msg["name"] == "list_films" {
			listed = msg["content"].(string)
		}
	}
	if !strings.Contains(listed, "/films/Batman.1989.1080p.mkv") || !strings.Contains(listed, `"title":"The Dark Knight"`) {
		t.Errorf("the film list did not reach the model: %s", listed)
	}
	if strings.Contains(sent, "pretend this is") {
		t.Error("a file's contents reached the model")
	}
	if strings.Contains(sent, "cloud0") || strings.Contains(sent, "test-key") {
		t.Error("an account or a credential reached the model")
	}
}

func TestSearchVaultAnswersTheWayTheSearchBoxDoes(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	f := newFakeModelServer(t)
	c.withAssistant(f)
	c.batmanCollection(batmanDB(t))

	w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "what have I got called batman?"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ask: %d %v", w.Code, body)
	}
	text := body["text"].(string)
	if text != "/films/Batman.1989.1080p.mkv = Batman (1989)" {
		t.Errorf("answer %q", text)
	}
}

func TestTheFilmDatabaseBeingUnsetIsToldNotFatal(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	f := newFakeModelServer(t)
	c.withAssistant(f)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})

	w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "what is missing?"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ask: %d %v", w.Code, body)
	}
	steps := body["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps %v", steps)
	}
	dbStep := steps[1].(map[string]any)
	if !strings.Contains(dbStep["error"].(string), "no film database key") {
		t.Errorf("the failed lookup was not reported: %v", dbStep)
	}
}

func TestAQuestionMustEndWithTheUser(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 2)
	f := newFakeModelServer(t)
	c.withAssistant(f)

	for _, messages := range [][]map[string]string{
		{},
		{{"role": "assistant", "content": "hello"}},
		{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}},
	} {
		w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{"messages": messages})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%v: %d %v", messages, w.Code, body)
		}
	}

	w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "   "}},
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("an empty question: %d %v", w.Code, body)
	}
}

func TestTheAssistantIsLockedWithTheVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 2)
	f := newFakeModelServer(t)
	c.withAssistant(f)

	c.json(http.MethodPost, "/api/vault/lock", nil)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/assistant"},
		{http.MethodPost, "/api/assistant/settings"},
		{http.MethodPost, "/api/assistant/ask"},
	} {
		w, _ := c.json(route.method, route.path, map[string]any{})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s while locked: %d", route.method, route.path, w.Code)
		}
	}
}
