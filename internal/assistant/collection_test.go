package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeCollection is a vault made of slices.
type fakeCollection struct {
	films    []Film
	hits     []Hit
	database map[string][]Title
	dbErr    error

	// What was asked, so a test can check the arguments got through.
	filmsDir string
	searched []string
	queried  []string
}

func (f *fakeCollection) Films(_ context.Context, dir string) ([]Film, error) {
	f.filmsDir = dir
	if dir == "" {
		return f.films, nil
	}
	out := []Film{}
	for _, film := range f.films {
		if strings.HasPrefix(film.Path, dir+"/") {
			out = append(out, film)
		}
	}
	return out, nil
}

func (f *fakeCollection) Search(_ context.Context, query, dir string, limit int) ([]Hit, error) {
	f.searched = append(f.searched, query)
	out := []Hit{}
	for _, h := range f.hits {
		if strings.Contains(strings.ToLower(h.Path), strings.ToLower(query)) {
			out = append(out, h)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeCollection) FilmDatabase(_ context.Context, query string) ([]Title, error) {
	f.queried = append(f.queried, query)
	if f.dbErr != nil {
		return nil, f.dbErr
	}
	return f.database[strings.ToLower(query)], nil
}

func batmanCollection() *fakeCollection {
	return &fakeCollection{
		films: []Film{
			{Title: "Batman", Year: 1989, Path: "/films/Batman.1989.mkv", TMDBID: 268},
			{Title: "The Dark Knight", Year: 2008, Path: "/films/The.Dark.Knight.2008.mkv", TMDBID: 155},
			{Title: "Alien", Year: 1979, Path: "/films/Alien.1979.mkv", TMDBID: 348},
		},
		hits: []Hit{
			{Type: "file", Path: "/films/Batman.1989.mkv", Size: 4_200_000_000, Film: "Batman"},
			{Type: "file", Path: "/films/The.Dark.Knight.2008.mkv", Size: 9_000_000_000, Film: "The Dark Knight"},
			{Type: "folder", Path: "/photos/batman-day"},
		},
		database: map[string][]Title{
			"batman": {
				{Title: "Batman", Year: 1989, TMDBID: 268, Overview: "The Dark Knight of Gotham City begins his war on crime."},
				{Title: "Batman Returns", Year: 1992, TMDBID: 364, Overview: strings.Repeat("Penguin. ", 60)},
				{Title: "The Dark Knight", Year: 2008, TMDBID: 155},
				{Title: "The Batman", Year: 2022, TMDBID: 414906},
			},
		},
	}
}

func runTool(t *testing.T, tools []Tool, name, args string) map[string]any {
	t.Helper()
	for _, tool := range tools {
		if tool.Name != name {
			continue
		}
		out, err := tool.Run(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s(%s): %v", name, args, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("%s answered with something that is not JSON: %q", name, out)
		}
		return parsed
	}
	t.Fatalf("no tool named %s", name)
	return nil
}

func TestTheThreeToolsAreDescribedForTheModel(t *testing.T) {
	tools := Tools(batmanCollection())
	names := []string{}
	for _, tool := range tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		var s map[string]any
		if err := json.Unmarshal(tool.Parameters, &s); err != nil || s["type"] != "object" {
			t.Errorf("%s schema %s", tool.Name, tool.Parameters)
		}
		if strings.Contains(string(tool.Parameters), "\n") {
			t.Errorf("%s schema is not compact", tool.Name)
		}
	}
	if strings.Join(names, " ") != "list_films search_vault search_film_database" {
		t.Errorf("tools %v", names)
	}
}

func TestListFilmsHandsOverTitleYearAndPath(t *testing.T) {
	c := batmanCollection()
	tools := Tools(c)

	all := runTool(t, tools, "list_films", `{}`)
	if all["count"] != float64(3) {
		t.Errorf("count %v", all["count"])
	}
	first := all["films"].([]any)[0].(map[string]any)
	if first["title"] != "Batman" || first["year"] != float64(1989) || first["path"] != "/films/Batman.1989.mkv" || first["tmdb_id"] != float64(268) {
		t.Errorf("first film %+v", first)
	}

	under := runTool(t, tools, "list_films", `{"dir":"/nowhere"}`)
	if c.filmsDir != "/nowhere" || under["count"] != float64(0) {
		t.Errorf("dir %q count %v", c.filmsDir, under["count"])
	}
	if _, isNull := under["films"].([]any); !isNull {
		t.Errorf("an empty list came back as %v, want []", under["films"])
	}
}

func TestSearchVaultCapsAndRequiresAQuery(t *testing.T) {
	c := batmanCollection()
	tools := Tools(c)

	got := runTool(t, tools, "search_vault", `{"query":"batman","limit":1}`)
	if got["count"] != float64(1) {
		t.Errorf("count %v with limit 1", got["count"])
	}
	hit := got["hits"].([]any)[0].(map[string]any)
	if hit["film"] != "Batman" || hit["size"] != float64(4_200_000_000) {
		t.Errorf("hit %+v", hit)
	}

	for _, tool := range tools {
		if tool.Name == "search_vault" {
			if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"  "}`)); err == nil {
				t.Error("an empty query was searched")
			}
		}
	}
}

func TestSearchFilmDatabaseClipsSummaries(t *testing.T) {
	c := batmanCollection()
	tools := Tools(c)

	got := runTool(t, tools, "search_film_database", `{"query":"Batman"}`)
	if c.queried[0] != "Batman" || got["count"] != float64(4) {
		t.Errorf("queried %v count %v", c.queried, got["count"])
	}
	films := got["films"].([]any)
	returns := films[1].(map[string]any)
	if overview := returns["overview"].(string); len(overview) > maxOverview+4 || !strings.HasSuffix(overview, "…") {
		t.Errorf("overview was not clipped: %q", overview)
	}
	if films[0].(map[string]any)["overview"] != "The Dark Knight of Gotham City begins his war on crime." {
		t.Error("a short overview was altered")
	}
}

func TestSearchFilmDatabasePassesTheCollectionsErrorThrough(t *testing.T) {
	c := batmanCollection()
	c.dbErr = errors.New("no film database key has been set")
	for _, tool := range Tools(c) {
		if tool.Name != "search_film_database" {
			continue
		}
		_, err := tool.Run(context.Background(), json.RawMessage(`{"query":"Batman"}`))
		if err == nil || err.Error() != "no film database key has been set" {
			t.Fatalf("err %v", err)
		}
	}
}

func TestArgumentsThatAreNotJSONAreAnError(t *testing.T) {
	for _, tool := range Tools(batmanCollection()) {
		if _, err := tool.Run(context.Background(), json.RawMessage(`nonsense`)); err == nil {
			t.Errorf("%s accepted arguments that are not JSON", tool.Name)
		}
	}
}

// The whole thing, end to end: a model that does what a real one does with
// the Batman question, over a collection that is missing two of the films.
// The model is scripted, but the comparison in its last reply is made from
// what the tools actually returned, so a tool that lied would fail this.
func TestBatmanEndToEnd(t *testing.T) {
	c := batmanCollection()
	tools := Tools(c)

	m := &comparing{}
	a := &Assistant{Model: m, Tools: tools}
	got, err := a.Ask(context.Background(), nil, "what Batman movies are missing from my collection?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "Batman Returns (1992)") || !strings.Contains(got.Text, "The Batman (2022)") {
		t.Errorf("answer does not name the two missing films: %q", got.Text)
	}
	if strings.Contains(got.Text, "Dark Knight") || strings.Contains(got.Text, "Batman (1989)") {
		t.Errorf("answer names a film the vault has: %q", got.Text)
	}
	if len(got.Steps) != 2 || got.Steps[0].Tool != "list_films" || got.Steps[1].Tool != "search_film_database" {
		t.Errorf("steps %+v", got.Steps)
	}
}

// comparing plays a model answering the Batman question: it lists the films,
// searches the database, and reports the database titles the list lacks.
type comparing struct{}

func (comparing) Chat(_ context.Context, msgs []Message, _ []ToolSpec) (Message, error) {
	last := msgs[len(msgs)-1]
	switch {
	case last.Role == RoleUser:
		return Message{ToolCalls: []ToolCall{{ID: "1", Name: "list_films", Arguments: json.RawMessage(`{}`)}}}, nil
	case last.Role == RoleTool && last.Name == "list_films":
		return Message{ToolCalls: []ToolCall{{ID: "2", Name: "search_film_database", Arguments: json.RawMessage(`{"query":"Batman"}`)}}}, nil
	}

	var have, known struct {
		Films []struct {
			Title string `json:"title"`
			Year  int    `json:"year"`
		} `json:"films"`
	}
	for _, m := range msgs {
		if m.Role != RoleTool {
			continue
		}
		switch m.Name {
		case "list_films":
			json.Unmarshal([]byte(m.Content), &have)
		case "search_film_database":
			json.Unmarshal([]byte(m.Content), &known)
		}
	}
	owned := map[string]bool{}
	for _, f := range have.Films {
		owned[strings.ToLower(f.Title)] = true
	}
	var missing []string
	for _, f := range known.Films {
		if !owned[strings.ToLower(f.Title)] {
			missing = append(missing, f.Title+" ("+itoa(f.Year)+")")
		}
	}
	return Message{Content: "Missing from your collection:\n- " + strings.Join(missing, "\n- ")}, nil
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
