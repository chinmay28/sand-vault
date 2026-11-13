package server

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http"
	"net/url"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// giveThumb stores a picture for a file the way the browser does.
func (c *testClient) giveThumb(id string) {
	c.t.Helper()
	var body bytes.Buffer
	if err := jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 342, 513)), nil); err != nil {
		c.t.Fatalf("encoding: %v", err)
	}
	if w := c.do(http.MethodPut, "/api/files/"+id+"/thumb", &body, "image/jpeg"); w.Code != http.StatusCreated {
		c.t.Fatalf("PUT thumb for %s: %d %s", id, w.Code, w.Body.String())
	}
}

func folderNamed(t *testing.T, lib map[string]any, name string) map[string]any {
	t.Helper()
	for _, raw := range lib["folders"].([]any) {
		if f := raw.(map[string]any); f["name"] == name {
			return f
		}
	}
	t.Fatalf("no folder %q in %v", name, lib["folders"])
	return nil
}

// A folder's picture follows it onto the TV: the one picked in the browser,
// or — for a folder holding a single film — that film's poster.
func TestATVSeesFolderPictures(t *testing.T) {
	c, films := tvLibrary(t)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films/Pair"})
	first := c.upload("First.mkv", "/films/Pair", []byte("first"))["id"].(string)
	second := c.upload("Second.mkv", "/films/Pair", []byte("second"))["id"].(string)
	c.giveThumb(films["alien"])
	c.giveThumb(first)
	c.giveThumb(second)

	token := c.pairTV(c.mintTVCode("/films", 60))
	_, lib := c.tvDo(http.MethodGet, "/api/tv/library", token, nil)

	// One film, with a poster, and nothing chosen: it wears the film's poster.
	if f := folderNamed(t, lib, "Alien (1979)"); f["art"] != true {
		t.Errorf("a folder of one film with a poster has no picture: %v", f)
	}
	// Two films and nothing chosen: no guess, as in the browser.
	if f := folderNamed(t, lib, "Pair"); f["art"] == true {
		t.Errorf("a folder of two films guessed a picture: %v", f)
	}

	w, _ := c.tvDo(http.MethodGet, "/api/tv/folders/art?path="+url.QueryEscape("/Alien (1979)"), token, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("art for the one-film folder: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w, _ := c.tvDo(http.MethodGet, "/api/tv/folders/art?path=/Pair", token, nil); w.Code != http.StatusNotFound {
		t.Errorf("art for a folder with none: %d, want 404", w.Code)
	}

	// The owner picks one for the two-film folder, and the TV shows it.
	if w, body := c.json(http.MethodPost, "/api/folders/art", map[string]any{"path": "/films/Pair", "id": second}); w.Code != http.StatusOK {
		t.Fatalf("set folder art: %d %v", w.Code, body)
	}
	_, lib = c.tvDo(http.MethodGet, "/api/tv/library", token, nil)
	if f := folderNamed(t, lib, "Pair"); f["art"] != true {
		t.Errorf("the picture chosen in the browser did not reach the TV: %v", f)
	}
	if w, _ := c.tvDo(http.MethodGet, "/api/tv/folders/art?path=/Pair", token, nil); w.Code != http.StatusOK {
		t.Errorf("art for the chosen folder: %d", w.Code)
	}

	// And the path is confined like every other: no reaching above the grant.
	if w, _ := c.tvDo(http.MethodGet, "/api/tv/folders/art?path=/../private", token, nil); w.Code == http.StatusOK {
		t.Error("folder art reached outside the grant")
	}
}

// The .srt beside a film is handed to the player with the film, behind a link
// of its own that dies with the code.
func TestPlayingAFilmBringsItsSidecarSubtitles(t *testing.T) {
	c, films := tvLibrary(t)
	c.upload("Alien.1979.en.srt", "/films/Alien (1979)", []byte("1\n00:00:01,000 --> 00:00:02,000\nIn space\n"))
	c.upload("Alien.1979.srt", "/films/Alien (1979)", []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"))
	c.upload("Other.film.srt", "/films/Alien (1979)", []byte("not this film's"))
	code := c.mintTVCode("/films", 60)
	token := c.pairTV(code)

	w, play := c.tvDo(http.MethodPost, "/api/tv/items/"+films["alien"]+"/play", token, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("play: %d %v", w.Code, play)
	}
	subs := play["subtitles"].([]any)
	if len(subs) != 2 {
		t.Fatalf("subtitles = %v, want the two named after the film", subs)
	}
	en := subs[0].(map[string]any)
	if en["language"] != "en" || en["mime"] != "application/x-subrip" || en["label"] != "en (SRT)" {
		t.Errorf("first subtitle = %v", en)
	}

	sw, _ := c.tvDo(http.MethodGet, en["url"].(string), "", nil)
	if sw.Code != http.StatusOK || sw.Body.String() != "1\n00:00:01,000 --> 00:00:02,000\nIn space\n" {
		t.Errorf("subtitle stream: %d %q", sw.Code, sw.Body.String())
	}

	// A film with none says so with an empty list, not a missing one.
	_, plain := c.tvDo(http.MethodPost, "/api/tv/items/"+films["thing"]+"/play", token, nil)
	if subs, ok := plain["subtitles"].([]any); !ok || len(subs) != 0 {
		t.Errorf("subtitles for a film without any = %v", plain["subtitles"])
	}

	// The subtitle file is not itself something the TV can open.
	_, lib := c.tvDo(http.MethodGet, "/api/tv/library?all=1", token, nil)
	for _, raw := range lib["items"].([]any) {
		if name := raw.(map[string]any)["name"].(string); name != "Alien.1979.mp4" && name != "The.Thing.1982.1080p.mkv" {
			t.Errorf("library lists %q", name)
		}
	}

	c.json(http.MethodDelete, "/api/tv/codes/"+code, nil)
	if sw, _ := c.tvDo(http.MethodGet, en["url"].(string), "", nil); sw.Code != http.StatusNotFound {
		t.Errorf("subtitle link after revoke = %d, want 404", sw.Code)
	}
}

func TestSidecarSubtitlesAreTheFilmsOwn(t *testing.T) {
	video := &vault.Entry{ID: "v", Dir: "/films", Name: "Film.2010.1080p.mkv"}
	sibling := func(id, name string) *vault.Entry { return &vault.Entry{ID: id, Dir: "/films", Name: name} }
	got := sidecarSubtitles(video, []*vault.Entry{
		video,
		sibling("a", "Film.2010.1080p.srt"),
		sibling("b", "film.2010.1080p.FRA.ass"),
		sibling("c", "Film.2010.1080p.English.forced.vtt"),
		sibling("d", "Film.2010.1080p.nfo"),
		sibling("e", "Film.2010.srt"),        // a shorter name: some other cut
		sibling("f", "Film.2010.1080pX.srt"), // not a dot after the name
		{ID: "g", Dir: "/elsewhere", Name: "Film.2010.1080p.srt"},
	})

	want := map[string][2]string{ // id -> language, label
		"a": {"", "External (SRT)"},
		"b": {"fra", "FRA (ASS)"},
		"c": {"", "English forced (VTT)"},
	}
	if len(got) != len(want) {
		t.Fatalf("found %d sidecars, want %d: %+v", len(got), len(want), got)
	}
	for _, s := range got {
		w, ok := want[s.Entry.ID]
		if !ok {
			t.Errorf("claimed %s, which is not this film's", s.Entry.Name)
			continue
		}
		if s.Language != w[0] || s.Label != w[1] {
			t.Errorf("%s: language %q label %q, want %q %q", s.Entry.Name, s.Language, s.Label, w[0], w[1])
		}
	}
	if sidecarSubtitles(&vault.Entry{ID: "x", Dir: "/", Name: ".mkv"}, nil) != nil {
		t.Error("a nameless video claimed subtitles")
	}
}
