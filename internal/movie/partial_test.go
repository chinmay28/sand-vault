package movie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestVariants(t *testing.T) {
	cases := []struct {
		guess Guess
		want  []string
	}{
		// A franchise name in front: dropping words from the front comes first.
		{Guess{Title: "james bond a view to a kill", Year: 1985}, []string{
			"bond a view to a kill (1985)",
			"james bond a view to a (1985)",
			"a view to a kill (1985)",
			"james bond a view to (1985)",
			"view to a kill (1985)",
			"james bond a view (1985)",
		}},
		// Numbers off both ends; a year among them becomes the year.
		{Guess{Title: "1985 casino royale 007"}, []string{
			"casino royale (1985)",
			"royale (1985)",
			"casino (1985)",
		}},
		// A year on the guess is not replaced by a number off the end.
		{Guess{Title: "casino royale 1999", Year: 2006}, []string{
			"casino royale (2006)",
			"royale (2006)",
			"casino (2006)",
		}},
		// Single short or generic words are never searched on their own.
		{Guess{Title: "the man"}, []string{}},
		{Guess{Title: "dr no 1962"}, []string{"dr no (1962)"}},
		// Nothing left once the numbers are gone.
		{Guess{Title: "0001 0002"}, []string{}},
		{Guess{}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.guess.String(), func(t *testing.T) {
			got := []string{}
			for _, v := range tc.guess.Variants() {
				got = append(got, v.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Variants(%q) =\n  %q\nwant\n  %q", tc.guess, got, tc.want)
			}
		})
	}
}

func TestVariantsAreBounded(t *testing.T) {
	g := Guess{Title: "one two three four five six seven eight nine ten eleven"}
	if n := len(g.Variants()); n != maxVariants {
		t.Errorf("%d variants, want the cap of %d", n, maxVariants)
	}
}

func TestFits(t *testing.T) {
	name := Guess{Title: "james bond a view to a kill", Year: 1985}
	cases := []struct {
		c    Candidate
		want bool
	}{
		{Candidate{Title: "A View to a Kill", Year: 1985}, true},
		{Candidate{Title: "A View to a Kill", Year: 1986}, true}, // a year either side
		{Candidate{Title: "A View to a Kill"}, true},             // no year to contradict
		{Candidate{Title: "A View to a Kill", Year: 2015}, false},
		{Candidate{Title: "Everything or Nothing", Original: "A View to a Kill", Year: 1985}, true},
		// The title has to be whole words of the name, not part of one.
		{Candidate{Title: "A View to a Kil", Year: 1985}, false},
		{Candidate{Title: "Kill Bill", Year: 1985}, false},
		{Candidate{Title: "", Year: 1985}, false},
	}
	for _, tc := range cases {
		if got := name.Fits(tc.c); got != tc.want {
			t.Errorf("Fits(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}

	// Apostrophes compare the way the parser leaves them.
	if !(Guess{Title: "Charlottes Web"}).Fits(Candidate{Title: "Charlotte's Web"}) {
		t.Error("Charlotte's Web does not fit Charlottes Web")
	}
}

// partialDatabase knows one film and finds it only when the query is its title
// and nothing more — a strict stand-in for a search that cannot see past a
// franchise name. Anything else comes back as a documentary that does not fit.
func partialDatabase(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var searches []string
	mux := http.NewServeMux()
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		searches = append(searches, q.Get("query")+"|"+q.Get("primary_release_year"))
		results := []map[string]any{}
		switch strings.ToLower(q.Get("query")) {
		case "a view to a kill":
			results = append(results, map[string]any{
				"id": 707, "title": "A View to a Kill", "release_date": "1985-05-24",
			})
		case "licence to kill", "license to kill":
			results = append(results, map[string]any{
				"id": 709, "title": "Licence to Kill", "release_date": "1989-06-13",
			})
		default:
			results = append(results, map[string]any{
				"id": 1, "title": "Becoming Bond", "release_date": "2017-05-20",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/3/movie/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/3/movie/")
		titles := map[string]string{"707": "A View to a Kill", "709": "Licence to Kill", "1": "Becoming Bond"}
		json.NewEncoder(w).Encode(map[string]any{"id": json.Number(id), "title": titles[id]})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{Key: "abc123", BaseURL: srv.URL + "/3", ImageBaseURL: srv.URL + "/t/p"}, &searches
}

func TestLookupFallsBackToPartOfTheName(t *testing.T) {
	client, searches := partialDatabase(t)

	guess := Parse("[1985]-james-bond-a-view-to-a-kill.mp4")
	info, err := client.Lookup(context.Background(), guess)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info == nil || info.TMDBID != 707 {
		t.Fatalf("got %+v, want A View to a Kill", info)
	}
	if info.Query != "a view to a kill (1985)" {
		t.Errorf("query = %q, want the search that found it", info.Query)
	}
	want := []string{
		"james bond a view to a kill|1985",
		"bond a view to a kill|1985",
		"james bond a view to a|1985",
		"a view to a kill|1985",
	}
	if !reflect.DeepEqual(*searches, want) {
		t.Errorf("searches = %q, want %q", *searches, want)
	}
}

// A fitting answer to the name as given costs no further searches.
func TestLookupStopsAtAFittingAnswer(t *testing.T) {
	client, searches := partialDatabase(t)

	info, err := client.Lookup(context.Background(), Guess{Title: "A View to a Kill", Year: 1985})
	if err != nil || info == nil || info.TMDBID != 707 {
		t.Fatalf("Lookup = %+v, %v", info, err)
	}
	if len(*searches) != 1 {
		t.Errorf("searches = %q, want one", *searches)
	}
}

// When nothing fits anywhere, the first search's own answer stands, which is
// what keeps a misspelt name matching the film the database corrected it to.
func TestLookupKeepsTheDatabasesAnswerWhenNothingFits(t *testing.T) {
	client, _ := partialDatabase(t)

	info, err := client.Lookup(context.Background(), Guess{Title: "license to kill", Year: 1989})
	if err != nil || info == nil || info.TMDBID != 709 {
		t.Fatalf("Lookup = %+v, %v, want Licence to Kill", info, err)
	}
	if info.Query != "license to kill (1989)" {
		t.Errorf("query = %q", info.Query)
	}
}
