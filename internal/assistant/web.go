package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Sandy on the web, when he is allowed there.
//
// Two tools: search, and read a page. Search goes through whichever engine
// the owner chose — a SearXNG they run themselves, or Ollama's search
// service with their own key — and reading a page is done from here, by
// fetching it and boiling the HTML down to text. Neither is on by default:
// a query is the one thing about a question that leaves the owner's
// network, and turning that on is theirs to do.
//
// What goes over: the query Sandy wrote, and the address of a page he
// chose. The prompt tells him never to put a file name or a folder from the
// index into either, and every search and every page is shown under the
// answer so that rule can be checked by eye.

// Web is a search engine and a page fetcher, which is all the two tools
// need. The server composes one from the settings; the tests use a fake.
type Web interface {
	Search(ctx context.Context, query string) ([]WebResult, error)
	Fetch(ctx context.Context, address string) (*Page, error)
}

// WebResult is one search hit.
type WebResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// Page is a fetched page, as text.
type Page struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`

	// Total is how long the whole text is; Text may be a window into it.
	Total int `json:"total_chars"`
}

// ErrWebOff is returned when a web tool is called but the owner has not
// turned the web on.
var ErrWebOff = errors.New("web access is turned off in Sandy's settings")

// ErrPrivateAddress is returned by the fetcher for an address on the owner's
// own network. The tool reads the public web; the vault, the model server
// and the router are not on it.
var ErrPrivateAddress = errors.New("that address is on a private network, and Sandy only reads the public web")

// Searcher is the search half of Web, on its own so the two engines can be
// small and the fetcher shared.
type Searcher interface {
	Search(ctx context.Context, query string) ([]WebResult, error)
}

// maxSearchResults caps what a search hands the model.
const maxSearchResults = 8

// maxSnippet caps a result's snippet. It is for choosing which page to read,
// not for reading.
const maxSnippet = 300

// SearXNG searches through a SearXNG instance — a metasearch engine the
// owner can run on their own machine, which keeps the queries on their own
// network and needs no key. Its JSON API has to be enabled in its settings.
type SearXNG struct {
	// BaseURL is the instance's root, such as http://gaming-pc:8080.
	BaseURL string

	HTTPClient *http.Client
}

// Search asks the instance for the first page of web results.
func (s *SearXNG) Search(ctx context.Context, query string) ([]WebResult, error) {
	base, err := ValidateBaseURL(s.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("search engine: %w", err)
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("categories", "general")

	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := getJSON(ctx, httpClient(s.HTTPClient), base+"/search?"+q.Encode(), "", &resp); err != nil {
		return nil, fmt.Errorf("search engine: %w", err)
	}
	out := make([]WebResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		if len(out) == maxSearchResults {
			break
		}
		out = append(out, WebResult{Title: r.Title, URL: r.URL, Snippet: clip(r.Content, maxSnippet)})
	}
	return out, nil
}

// OllamaSearch searches through Ollama's web search service, with the
// owner's own ollama.com key. Queries go to Ollama, which is a third party
// the owner chose — the same arrangement as the film database.
type OllamaSearch struct {
	Key string

	// BaseURL is where the service lives. Empty means ollama.com; a test
	// points it somewhere else.
	BaseURL string

	HTTPClient *http.Client
}

// DefaultOllamaSearchURL is the service's address.
const DefaultOllamaSearchURL = "https://ollama.com"

// Search asks the service for results.
func (o *OllamaSearch) Search(ctx context.Context, query string) ([]WebResult, error) {
	if strings.TrimSpace(o.Key) == "" {
		return nil, errors.New("search engine: Ollama web search needs an ollama.com key")
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = DefaultOllamaSearchURL
	}
	body := map[string]any{"query": query, "max_results": maxSearchResults}
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := postJSON(ctx, httpClient(o.HTTPClient), base+"/api/web_search", o.Key, body, &resp); err != nil {
		return nil, fmt.Errorf("search engine: %w", err)
	}
	out := make([]WebResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		if len(out) == maxSearchResults {
			break
		}
		out = append(out, WebResult{Title: r.Title, URL: r.URL, Snippet: clip(r.Content, maxSnippet)})
	}
	return out, nil
}

// Fetcher reads a public web page and returns it as text.
//
// It is deliberately plain: one request, no scripts run, so a page that
// draws itself with JavaScript comes back as whatever its HTML carried.
// Most pages worth reading — charts, lists, articles, documentation —
// carry their text in the HTML, and a page that does not is reported as
// short rather than pretended at.
type Fetcher struct {
	HTTPClient *http.Client

	// AllowPrivate lets the fetcher reach loopback and private addresses.
	// Only a test sets it: the tool is for the public web, and the owner's
	// own network — the vault itself, the model server, the router — is
	// exactly what a model must not be talked into reading.
	AllowPrivate bool
}

// Limits on one fetch. A page is read for its text, not archived.
const (
	fetchTimeout  = 25 * time.Second
	maxPageBytes  = 4 << 20
	maxPageChars  = 40_000
	defaultWindow = 12_000
	maxRedirects  = 5
)

// Fetch reads one page.
func (f *Fetcher) Fetch(ctx context.Context, address string) (*Page, error) {
	target, err := url.Parse(strings.TrimSpace(address))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, errors.New("that is not a web address")
	}

	client := f.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	// A copy, so the redirect check does not leak into a shared client.
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		return f.check(req.URL)
	}
	if err := f.check(target); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	// Named for what it is. A page that refuses a named reader is a page
	// Sandy does not read.
	req.Header.Set("User-Agent", "SAND-Vault-Sandy/1 (+https://github.com/chinmay28/sand-vault)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch the page: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("the site answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the page: %w", err)
	}

	page := &Page{URL: resp.Request.URL.String()}
	kind := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case strings.Contains(kind, "text/html"), strings.Contains(kind, "xhtml"), kind == "":
		page.Title, page.Text = HTMLToText(raw)
	case strings.HasPrefix(kind, "text/"), strings.Contains(kind, "json"), strings.Contains(kind, "xml"):
		page.Text = strings.TrimSpace(string(raw))
	default:
		return nil, fmt.Errorf("the page is %s, which is not something to read", strings.SplitN(kind, ";", 2)[0])
	}
	if len(page.Text) > maxPageChars {
		page.Text = page.Text[:maxPageChars]
	}
	page.Total = len(page.Text)
	return page, nil
}

// check refuses an address that points into the owner's own network,
// resolving the name first so a public hostname that answers with a private
// address is caught as well.
func (f *Fetcher) check(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("that is not a web address")
	}
	if f.AllowPrivate {
		return nil
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return ErrPrivateAddress
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("could not resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if isPrivate(a.IP) {
			return ErrPrivateAddress
		}
	}
	return nil
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// HTMLToText boils a page down to its title and its readable text: scripts,
// styles and markup gone, one line per block, blank lines collapsed. It is
// what the model reads, so it errs towards keeping text rather than
// guessing which parts are navigation.
func HTMLToText(raw []byte) (title, text string) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", strings.TrimSpace(string(raw))
	}

	var out strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.ElementNode:
			switch n.Data {
			case "script", "style", "noscript", "template", "svg", "iframe", "head":
				// The title is the one thing in the head worth having.
				if n.Data == "head" {
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.ElementNode && c.Data == "title" && c.FirstChild != nil {
							title = strings.TrimSpace(c.FirstChild.Data)
						}
					}
				}
				return
			}
			if isBlock(n.Data) {
				out.WriteByte('\n')
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if isBlock(n.Data) {
				out.WriteByte('\n')
			}
		case html.TextNode:
			out.WriteString(strings.Join(strings.Fields(n.Data), " "))
			out.WriteByte(' ')
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(doc)

	lines := make([]string, 0, 256)
	blank := true
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		lines = append(lines, line)
		blank = false
	}
	return title, strings.TrimSpace(strings.Join(lines, "\n"))
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "br", "li", "ul", "ol", "tr", "td", "th", "table", "section", "article",
		"header", "footer", "main", "aside", "nav", "h1", "h2", "h3", "h4", "h5", "h6",
		"blockquote", "pre", "dd", "dt", "dl", "figcaption", "hr", "title":
		return true
	}
	return false
}

// Site composes a search engine and the fetcher into one Web.
type Site struct {
	Searcher Searcher
	Fetcher  *Fetcher
}

var _ Web = Site{}

func (s Site) Search(ctx context.Context, query string) ([]WebResult, error) {
	if s.Searcher == nil {
		return nil, ErrWebOff
	}
	return s.Searcher.Search(ctx, query)
}

func (s Site) Fetch(ctx context.Context, address string) (*Page, error) {
	f := s.Fetcher
	if f == nil {
		f = &Fetcher{}
	}
	return f.Fetch(ctx, address)
}

// WebTools builds the two web tools over a Web.
func WebTools(w Web) []Tool {
	return []Tool{
		{
			ToolSpec: ToolSpec{
				Name: "web_search",
				Description: "Search the public web. Returns up to eight results with title, " +
					"address and a snippet; read one with fetch_page. Never put the name of a " +
					"file or folder from the vault into a query — search for the public thing " +
					"(a list, a series, a year) and compare with the vault yourself.",
				Parameters: schema(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "What to search the web for."}
					},
					"required": ["query"]
				}`),
			},
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Query string `json:"query"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.Query) == "" {
					return "", fmt.Errorf("web_search needs a query")
				}
				results, err := w.Search(ctx, strings.TrimSpace(in.Query))
				if err != nil {
					return "", err
				}
				return encode(map[string]any{"count": len(results), "results": results})
			},
		},
		{
			ToolSpec: ToolSpec{
				Name: "fetch_page",
				Description: "Read a public web page as plain text, up to 12,000 characters at a " +
					"time. For a longer page, call again with `start` set to where the last " +
					"call stopped. Only public addresses; nothing on the owner's own network.",
				Parameters: schema(`{
					"type": "object",
					"properties": {
						"url": {"type": "string", "description": "The page's address, from a search result or a known site."},
						"start": {"type": "integer", "description": "Character offset to continue reading from. Default 0."}
					},
					"required": ["url"]
				}`),
			},
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					URL   string `json:"url"`
					Start int    `json:"start"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.URL) == "" {
					return "", fmt.Errorf("fetch_page needs a url")
				}
				page, err := w.Fetch(ctx, in.URL)
				if err != nil {
					return "", err
				}
				start := in.Start
				if start < 0 || start > len(page.Text) {
					start = 0
				}
				end := start + defaultWindow
				if end > len(page.Text) {
					end = len(page.Text)
				}
				out := map[string]any{
					"url": page.URL, "title": page.Title,
					"start": start, "end": end, "total_chars": page.Total,
					"text": page.Text[start:end],
				}
				if end < page.Total {
					out["more"] = fmt.Sprintf("call again with start=%d for the rest", end)
				}
				return encode(out)
			},
		},
	}
}

// Small HTTP helpers for the engines. Kept apart from ChatCompletions.do
// because a search engine's errors read differently from a model server's.

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: fetchTimeout}
}

func getJSON(ctx context.Context, c *http.Client, target, key string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	return doJSON(c, req, key, out)
}

func postJSON(ctx context.Context, c *http.Client, target, key string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(c, req, key, out)
}

func doJSON(c *http.Client, req *http.Request, key string, out any) error {
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach it: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		line := strings.TrimSpace(string(raw))
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		if line == "" {
			line = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("it answered %d: %s", resp.StatusCode, line)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("its answer was not JSON: %w", err)
	}
	return nil
}
