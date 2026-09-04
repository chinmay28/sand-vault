package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLToTextKeepsTheWordsAndDropsTheRest(t *testing.T) {
	page := `<!doctype html><html><head><title> IMDb Top 250 </title>
	<style>body{color:red}</style><script>window.x = "<li>not text</li>"</script></head>
	<body><nav><a href="/">Home</a></nav>
	<h1>Top   250 Movies</h1>
	<ul><li><h3>1. The Shawshank Redemption</h3><span>1994</span></li>
	<li><h3>2. The Godfather</h3><span>1972</span></li></ul>
	<p>Some <b>bold</b> and <i>italic</i> words.</p>
	<noscript>Enable JavaScript</noscript>
	<svg><text>icon</text></svg>
	</body></html>`

	title, text := HTMLToText([]byte(page))
	if title != "IMDb Top 250" {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{"Top 250 Movies", "1. The Shawshank Redemption", "1994", "2. The Godfather", "Some bold and italic words."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	for _, no := range []string{"color:red", "not text", "Enable JavaScript", "icon", "window.x"} {
		if strings.Contains(text, no) {
			t.Errorf("text kept %q:\n%s", no, text)
		}
	}
	if strings.Contains(text, "\n\n\n") {
		t.Error("blank lines were not collapsed")
	}
	if strings.Index(text, "1. The Shawshank") > strings.Index(text, "2. The Godfather") {
		t.Error("order was lost")
	}
}

func TestHTMLToTextOnSomethingThatIsNotHTML(t *testing.T) {
	_, text := HTMLToText([]byte("  just a line of text  "))
	if text != "just a line of text" {
		t.Errorf("text %q", text)
	}
}

// A search engine of each kind, on one fake server.
func newFakeEngines(t *testing.T) (*httptest.Server, *[]string, *string) {
	t.Helper()
	var queries []string
	var auth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			http.Error(w, "json format not enabled", http.StatusForbidden)
			return
		}
		queries = append(queries, r.URL.Query().Get("q"))
		results := []map[string]any{}
		for i := 0; i < 12; i++ {
			results = append(results, map[string]any{
				"title": "Result", "url": "https://example.org/" + string(rune('a'+i)),
				"content": strings.Repeat("snippet ", 100),
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"query": r.URL.Query().Get("q"), "results": results})
	})
	mux.HandleFunc("POST /api/web_search", func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		queries = append(queries, req["query"].(string))
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"title": "IMDb Top 250 Movies", "url": "https://www.imdb.com/chart/top/", "content": "As rated by regular IMDb voters."},
		}})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, &queries, &auth
}

func TestSearXNGSearchCapsAndClips(t *testing.T) {
	s, queries, _ := newFakeEngines(t)
	engine := &SearXNG{BaseURL: s.URL + "/"}

	got, err := engine.Search(context.Background(), "imdb top 250")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxSearchResults {
		t.Errorf("%d results, want the cap of %d", len(got), maxSearchResults)
	}
	if len(got[0].Snippet) > maxSnippet+4 {
		t.Errorf("snippet not clipped: %d chars", len(got[0].Snippet))
	}
	if (*queries)[0] != "imdb top 250" {
		t.Errorf("query sent as %q", (*queries)[0])
	}

	if _, err := (&SearXNG{}).Search(context.Background(), "x"); err == nil {
		t.Error("an engine with no URL searched anyway")
	}
}

func TestOllamaSearchSendsTheKeyAndReadsResults(t *testing.T) {
	s, queries, auth := newFakeEngines(t)
	engine := &OllamaSearch{Key: "ollama-key", BaseURL: s.URL}

	got, err := engine.Search(context.Background(), "imdb top 250")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://www.imdb.com/chart/top/" {
		t.Errorf("results %+v", got)
	}
	if *auth != "Bearer ollama-key" || (*queries)[0] != "imdb top 250" {
		t.Errorf("auth %q query %q", *auth, (*queries)[0])
	}

	if _, err := (&OllamaSearch{BaseURL: s.URL}).Search(context.Background(), "x"); err == nil {
		t.Error("a search with no key went out")
	}
}

func TestFetcherReadsAPageAsText(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chart":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<html><head><title>Top 250</title></head><body><h3>1. Alien</h3><h3>2. The Thing</h3></body></html>`))
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"a":1}`))
		case "/photo":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("\xff\xd8"))
		case "/gone":
			http.NotFound(w, r)
		case "/hop":
			http.Redirect(w, r, "/chart", http.StatusFound)
		}
	}))
	t.Cleanup(site.Close)
	f := &Fetcher{AllowPrivate: true}

	page, err := f.Fetch(context.Background(), site.URL+"/chart")
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Top 250" || !strings.Contains(page.Text, "1. Alien") || page.Total != len(page.Text) {
		t.Errorf("page %+v", page)
	}
	if page.URL != site.URL+"/chart" {
		t.Errorf("url %q", page.URL)
	}

	page, err = f.Fetch(context.Background(), site.URL+"/hop")
	if err != nil || page.URL != site.URL+"/chart" {
		t.Errorf("after a redirect: %+v %v", page, err)
	}

	page, err = f.Fetch(context.Background(), site.URL+"/data.json")
	if err != nil || page.Text != `{"a":1}` {
		t.Errorf("json: %+v %v", page, err)
	}

	if _, err := f.Fetch(context.Background(), site.URL+"/photo"); err == nil || !strings.Contains(err.Error(), "image/jpeg") {
		t.Errorf("a picture was read as text: %v", err)
	}
	if _, err := f.Fetch(context.Background(), site.URL+"/gone"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing page: %v", err)
	}
	if _, err := f.Fetch(context.Background(), "ftp://example.org/x"); err == nil {
		t.Error("an ftp address was fetched")
	}
}

func TestFetcherRefusesTheOwnersOwnNetwork(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	t.Cleanup(site.Close)
	f := &Fetcher{}

	for _, address := range []string{
		site.URL,
		"http://localhost:8123/api/vault",
		"http://127.0.0.1:11434/api/tags",
		"http://192.168.1.1/",
		"http://10.0.0.5/",
		"http://[::1]/",
		"http://gaming-pc.local/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		_, err := f.Fetch(context.Background(), address)
		if !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("%s: err %v, want ErrPrivateAddress", address, err)
		}
	}

	// A redirect into the private network is caught on the hop.
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, site.URL, http.StatusFound)
	}))
	t.Cleanup(hop.Close)
	open := &Fetcher{AllowPrivate: true}
	if page, err := open.Fetch(context.Background(), hop.URL); err != nil || page.Text != "secret" {
		t.Fatalf("the test's own hop does not work: %v", err)
	}
}

// fakeWeb is a web of two pages and one search.
type fakeWeb struct {
	searches []string
	fetched  []string
	pages    map[string]*Page
}

func (w *fakeWeb) Search(_ context.Context, query string) ([]WebResult, error) {
	w.searches = append(w.searches, query)
	return []WebResult{{Title: "IMDb Top 250", URL: "https://www.imdb.com/chart/top/", Snippet: "the chart"}}, nil
}

func (w *fakeWeb) Fetch(_ context.Context, address string) (*Page, error) {
	w.fetched = append(w.fetched, address)
	p, ok := w.pages[address]
	if !ok {
		return nil, errors.New("the site answered 404 Not Found")
	}
	return p, nil
}

func TestWebToolsSearchAndPage(t *testing.T) {
	long := strings.Repeat("0123456789", 2000) // 20,000 chars
	w := &fakeWeb{pages: map[string]*Page{
		"https://www.imdb.com/chart/top/": {URL: "https://www.imdb.com/chart/top/", Title: "Top 250", Text: long, Total: len(long)},
	}}
	tools := WebTools(w)

	got := runTool(t, tools, "web_search", `{"query":" imdb top 250 "}`)
	if got["count"] != float64(1) || w.searches[0] != "imdb top 250" {
		t.Errorf("search %+v, sent %q", got, w.searches)
	}

	first := runTool(t, tools, "fetch_page", `{"url":"https://www.imdb.com/chart/top/"}`)
	if first["end"] != float64(defaultWindow) || first["total_chars"] != float64(20000) || first["more"] == nil {
		t.Errorf("first window %+v", first)
	}
	if len(first["text"].(string)) != defaultWindow {
		t.Errorf("first window is %d chars", len(first["text"].(string)))
	}
	rest := runTool(t, tools, "fetch_page", `{"url":"https://www.imdb.com/chart/top/","start":12000}`)
	if rest["start"] != float64(12000) || rest["end"] != float64(20000) || rest["more"] != nil {
		t.Errorf("second window %+v", rest)
	}

	for _, tool := range tools {
		if _, err := tool.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s ran with nothing to do", tool.Name)
		}
	}
	if _, err := tools[1].Run(context.Background(), json.RawMessage(`{"url":"https://nowhere.example/"}`)); err == nil {
		t.Error("a missing page was not an error")
	}
}

func TestSiteWithNoEngineSaysTheWebIsOff(t *testing.T) {
	if _, err := (Site{}).Search(context.Background(), "x"); !errors.Is(err, ErrWebOff) {
		t.Errorf("err %v", err)
	}
}

// The IMDb question, end to end: search, read the chart, list the films,
// compare. The model is scripted but the comparison is made from what the
// tools returned.
func TestIMDbTop250EndToEnd(t *testing.T) {
	chart := "IMDb Top 250\n1. The Shawshank Redemption 1994\n2. The Godfather 1972\n3. The Dark Knight 2008\n4. Alien 1979\n"
	w := &fakeWeb{pages: map[string]*Page{
		"https://www.imdb.com/chart/top/": {URL: "https://www.imdb.com/chart/top/", Title: "Top 250", Text: chart, Total: len(chart)},
	}}
	c := batmanCollection()
	tools := append(Tools(c), WebTools(w)...)

	a := &Assistant{Model: &charting{}, Tools: tools}
	got, err := a.Ask(context.Background(), nil, "which of the IMDb top 250 am I missing?")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The Shawshank Redemption", "The Godfather"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("answer lacks %s: %q", want, got.Text)
		}
	}
	for _, no := range []string{"Dark Knight", "Alien"} {
		if strings.Contains(got.Text, no) {
			t.Errorf("answer names a film the vault has: %q", got.Text)
		}
	}
	names := []string{}
	for _, s := range got.Steps {
		names = append(names, s.Tool)
	}
	if strings.Join(names, " ") != "web_search fetch_page list_films" {
		t.Errorf("steps %v", names)
	}
	// Nothing from the index went to the web.
	for _, q := range w.searches {
		if strings.Contains(q, "/films") || strings.Contains(q, ".mkv") {
			t.Errorf("a vault name went into a web query: %q", q)
		}
	}
}

// charting plays a model on the IMDb question.
type charting struct{}

func (charting) Chat(_ context.Context, msgs []Message, _ []ToolSpec) (Message, error) {
	last := msgs[len(msgs)-1]
	switch {
	case last.Role == RoleUser:
		return Message{ToolCalls: []ToolCall{{ID: "1", Name: "web_search", Arguments: json.RawMessage(`{"query":"IMDb top 250"}`)}}}, nil
	case last.Role == RoleTool && last.Name == "web_search":
		var res struct {
			Results []WebResult `json:"results"`
		}
		json.Unmarshal([]byte(last.Content), &res)
		return Message{ToolCalls: []ToolCall{{ID: "2", Name: "fetch_page", Arguments: json.RawMessage(`{"url":"` + res.Results[0].URL + `"}`)}}}, nil
	case last.Role == RoleTool && last.Name == "fetch_page":
		return Message{ToolCalls: []ToolCall{{ID: "3", Name: "list_films", Arguments: json.RawMessage(`{}`)}}}, nil
	}

	var page struct {
		Text string `json:"text"`
	}
	var have struct {
		Films []Film `json:"films"`
	}
	for _, m := range msgs {
		if m.Role != RoleTool {
			continue
		}
		switch m.Name {
		case "fetch_page":
			json.Unmarshal([]byte(m.Content), &page)
		case "list_films":
			json.Unmarshal([]byte(m.Content), &have)
		}
	}
	owned := map[string]bool{}
	for _, f := range have.Films {
		owned[strings.ToLower(f.Title)] = true
	}
	var missing []string
	for _, line := range strings.Split(page.Text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasSuffix(fields[0], ".") {
			continue
		}
		title := strings.Join(fields[1:len(fields)-1], " ")
		if !owned[strings.ToLower(title)] {
			missing = append(missing, title+" ("+fields[len(fields)-1]+")")
		}
	}
	return Message{Content: "Missing from the top 250:\n- " + strings.Join(missing, "\n- ")}, nil
}
