package server

import (
	"context"
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

		// A token count the way a real server gives one: the whole
		// transcript in, the reply out. Four characters a token is close
		// enough for a fake.
		raw, _ := json.Marshal(req["messages"])
		reply := f.reply(req)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": reply}},
			"usage": map[string]any{
				"prompt_tokens":     len(raw) / 4,
				"completion_tokens": len(fmt.Sprint(reply["content"])) / 4,
			},
		})
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"model_info":   map[string]any{"qwen3.context_length": 40960},
			"capabilities": []string{"completion", "tools"},
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

	if reply, ok := f.imdb(req); ok {
		return reply
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
	if body["context_reported"] != float64(40960) || body["context_window"] != float64(40960) || body["context_tokens"] != float64(0) {
		t.Errorf("the window the server reported was not kept: %v", body)
	}

	// Setting the window by hand outranks what the server said, and clearing
	// it goes back to the server's figure.
	w, body = c.json(http.MethodPost, "/api/assistant/settings",
		map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b", "context_tokens": 8192})
	if w.Code != http.StatusOK || body["context_window"] != float64(8192) || body["context_reported"] != float64(40960) {
		t.Errorf("an override: %d %v", w.Code, body)
	}
	w, body = c.json(http.MethodPost, "/api/assistant/settings",
		map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b", "context_tokens": -1})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a negative window: %d %v", w.Code, body)
	}
	w, body = c.json(http.MethodPost, "/api/assistant/settings",
		map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b"})
	if w.Code != http.StatusOK || body["context_window"] != float64(40960) {
		t.Errorf("back to the server's figure: %d %v", w.Code, body)
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

	// How full the window was: the last request's count, against the window
	// the server reported when Sandy was set up.
	usage, _ := body["context"].(map[string]any)
	if usage == nil || usage["tokens"].(float64) < 500 || usage["window"] != float64(40960) {
		t.Errorf("context usage %v, want a real count of 40960", usage)
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

// A search engine and one page, for Sandy's web tools. The engine answers
// any search with the chart; the chart lists four films.
func newFakeWeb(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	mux := http.NewServeMux()
	var site *httptest.Server
	mux.HandleFunc("POST /api/web_search", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ollama-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		queries = append(queries, req["query"].(string))
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"title": "IMDb Top 250", "url": site.URL + "/chart/top/", "content": "As rated by voters."},
		}})
	})
	mux.HandleFunc("GET /chart/top/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>IMDb Top 250</title></head><body><ul>
			<li>1. The Shawshank Redemption 1994</li>
			<li>2. The Godfather 1972</li>
			<li>3. The Dark Knight 2008</li>
			<li>4. Alien 1979</li></ul></body></html>`))
	})
	site = httptest.NewServer(mux)
	t.Cleanup(site.Close)
	return site, &queries
}

// The fake model's IMDb script, added beside the Batman one: search, read
// the page, list the films, compare titles by line.
func (f *fakeModelServer) imdb(req map[string]any) (map[string]any, bool) {
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	call := func(id, name, args string) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
			"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args},
		}}}
	}
	asked := false
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] == "user" && strings.Contains(strings.ToLower(fmt.Sprint(m["content"])), "imdb") {
			asked = true
		}
	}
	if !asked {
		return nil, false
	}
	hasTool := func(name string) bool {
		for _, raw := range req["tools"].([]any) {
			if raw.(map[string]any)["function"].(map[string]any)["name"] == name {
				return true
			}
		}
		return false
	}
	switch {
	case last["role"] == "user":
		if !hasTool("web_search") {
			return map[string]any{"role": "assistant", "content": "I have no web access. It can be turned on in my settings."}, true
		}
		return call("w1", "web_search", `{"query":"IMDb top 250"}`), true
	case last["role"] == "tool" && last["name"] == "web_search":
		var res struct {
			Results []struct {
				URL string `json:"url"`
			} `json:"results"`
		}
		json.Unmarshal([]byte(last["content"].(string)), &res)
		return call("w2", "fetch_page", `{"url":"`+res.Results[0].URL+`"}`), true
	case last["role"] == "tool" && last["name"] == "fetch_page":
		return call("w3", "list_films", `{}`), true
	}
	var page struct {
		Text string `json:"text"`
	}
	var have struct {
		Films []struct {
			Title string `json:"title"`
		} `json:"films"`
	}
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] != "tool" {
			continue
		}
		switch m["name"] {
		case "fetch_page":
			json.Unmarshal([]byte(m["content"].(string)), &page)
		case "list_films":
			json.Unmarshal([]byte(m["content"].(string)), &have)
		}
	}
	owned := map[string]bool{}
	for _, film := range have.Films {
		owned[strings.ToLower(film.Title)] = true
	}
	missing := []string{}
	for _, line := range strings.Split(page.Text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasSuffix(fields[0], ".") {
			continue
		}
		title := strings.Join(fields[1:len(fields)-1], " ")
		if !owned[strings.ToLower(title)] {
			missing = append(missing, "- "+title+" ("+fields[len(fields)-1]+")")
		}
	}
	return map[string]any{"role": "assistant", "content": "Missing from the top 250:\n" + strings.Join(missing, "\n")}, true
}

func TestTheWebIsOffUntilTheOwnerTurnsItOn(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	f := newFakeModelServer(t)
	c.withAssistant(f)
	c.batmanCollection(batmanDB(t))

	w, body := c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "which of the IMDb top 250 am I missing?"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ask: %d %v", w.Code, body)
	}
	if !strings.Contains(body["text"].(string), "no web access") {
		t.Errorf("answer %q", body["text"])
	}
	f.mu.Lock()
	tools := f.requests[0]["tools"].([]any)
	f.mu.Unlock()
	for _, raw := range tools {
		if name := raw.(map[string]any)["function"].(map[string]any)["name"]; name == "web_search" || name == "fetch_page" {
			t.Errorf("%s was offered with the web off", name)
		}
	}
	if _, body = c.json(http.MethodGet, "/api/assistant", nil); body["web"].(map[string]any)["engine"] != "" {
		t.Errorf("web settings of a fresh vault: %v", body["web"])
	}
}

func TestWebSettingsAreCheckedAndTheKeyNeverEchoed(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 2)
	f := newFakeModelServer(t)
	c.withAssistant(f)

	set := func(web map[string]any) (*httptest.ResponseRecorder, map[string]any) {
		return c.json(http.MethodPost, "/api/assistant/settings", map[string]any{
			"url": f.URL + "/v1", "model": "qwen3:14b", "web": web,
		})
	}

	if w, body := set(map[string]any{"engine": "searxng", "url": "searx:8080"}); w.Code != http.StatusBadRequest {
		t.Errorf("a SearXNG address with no scheme: %d %v", w.Code, body)
	}
	if w, body := set(map[string]any{"engine": "ollama"}); w.Code != http.StatusBadRequest {
		t.Errorf("Ollama with no key: %d %v", w.Code, body)
	}
	if w, body := set(map[string]any{"engine": "bing", "key": "k"}); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown engine: %d %v", w.Code, body)
	}

	w, body := set(map[string]any{"engine": "ollama", "key": "ollama-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("ollama: %d %v", w.Code, body)
	}
	web := body["web"].(map[string]any)
	if web["engine"] != "ollama" || web["has_key"] != true {
		t.Errorf("stored as %v", web)
	}
	if _, echoed := web["key"]; echoed {
		t.Error("the key was echoed back")
	}

	// Saving the model settings without a web field keeps the web as it was.
	w, body = c.json(http.MethodPost, "/api/assistant/settings", map[string]any{"url": f.URL + "/v1", "model": "qwen3:14b"})
	if w.Code != http.StatusOK || body["web"].(map[string]any)["engine"] != "ollama" {
		t.Errorf("an update without a web field dropped it: %d %v", w.Code, body)
	}

	// Switching engines drops the other engine's credential.
	w, body = set(map[string]any{"engine": "searxng", "url": "http://searx:8080/"})
	web = body["web"].(map[string]any)
	if w.Code != http.StatusOK || web["url"] != "http://searx:8080" || web["has_key"] != false {
		t.Errorf("searxng: %d %v", w.Code, body)
	}

	w, body = set(map[string]any{"engine": ""})
	if w.Code != http.StatusOK || body["web"].(map[string]any)["engine"] != "" {
		t.Errorf("turning the web off: %d %v", w.Code, body)
	}
}

func TestWhichOfTheIMDbTop250AreMissing(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	f := newFakeModelServer(t)
	c.withAssistant(f)
	c.batmanCollection(batmanDB(t))

	site, queries := newFakeWeb(t)
	c.server.OllamaSearchURL = site.URL
	c.server.WebAllowPrivate = true
	w, body := c.json(http.MethodPost, "/api/assistant/settings", map[string]any{
		"url": f.URL + "/v1", "model": "qwen3:14b",
		"web": map[string]any{"engine": "ollama", "key": "ollama-key"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("turn the web on: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodPost, "/api/assistant/ask", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "Look up the IMDb top 250. Which of those am I missing?"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ask: %d %v", w.Code, body)
	}
	text := body["text"].(string)
	for _, want := range []string{"The Shawshank Redemption (1994)", "The Godfather (1972)"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer lacks %s: %q", want, text)
		}
	}
	if strings.Contains(text, "Dark Knight") || strings.Contains(text, "Alien") {
		t.Errorf("a film the vault holds was called missing: %q", text)
	}
	names := []string{}
	for _, s := range body["steps"].([]any) {
		names = append(names, s.(map[string]any)["tool"].(string))
	}
	if strings.Join(names, " ") != "web_search fetch_page list_films" {
		t.Errorf("steps %v", names)
	}
	if len(*queries) != 1 || (*queries)[0] != "IMDb top 250" {
		t.Errorf("the engine was sent %v", *queries)
	}
}

func TestSandyCannotBeTalkedIntoReadingTheVaultsOwnAPI(t *testing.T) {
	site, _ := newFakeWeb(t)
	c := newTestClient(t)
	c.setup("correct horse battery staple", 2)
	f := newFakeModelServer(t)
	c.withAssistant(f)
	c.server.OllamaSearchURL = site.URL
	// WebAllowPrivate deliberately left off: this is the production guard.
	c.json(http.MethodPost, "/api/assistant/settings", map[string]any{
		"url": f.URL + "/v1", "model": "qwen3:14b",
		"web": map[string]any{"engine": "ollama", "key": "ollama-key"},
	})

	a, err := c.server.assistantFor("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range a.Tools {
		if tool.Name != "fetch_page" {
			continue
		}
		for _, address := range []string{"http://127.0.0.1:8123/api/vault", "http://localhost:11434/api/tags", site.URL} {
			_, err := tool.Run(context.Background(), json.RawMessage(`{"url":"`+address+`"}`))
			if err == nil || !strings.Contains(err.Error(), "private network") {
				t.Errorf("%s: %v", address, err)
			}
		}
	}
}
