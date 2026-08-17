package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/chinmay28/sand-vault/internal/thumb"
)

// samplePNG is what the browser hands the server: a small picture it drew on a
// canvas from the file being uploaded.
func samplePNG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 120, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding sample: %v", err)
	}
	return buf.Bytes()
}

// uploadWithThumb posts one file alongside the preview image the browser made
// for it, the way the web client does.
func (c *testClient) uploadWithThumb(name, dir string, content, preview []byte) map[string]any {
	c.t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	part, err := mw.CreateFormFile("files[]", name)
	if err != nil {
		c.t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(content)

	if preview != nil {
		thumbPart, err := mw.CreateFormFile("thumb-0", "thumb.jpg")
		if err != nil {
			c.t.Fatalf("CreateFormFile thumb: %v", err)
		}
		thumbPart.Write(preview)
	}

	mw.WriteField("path", dir)
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	if w.Code != http.StatusCreated {
		c.t.Fatalf("upload %s: %d %s", name, w.Code, w.Body.String())
	}

	var resp struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		c.t.Fatalf("expected 1 upload result, got %d", len(resp.Results))
	}
	if ok, _ := resp.Results[0]["ok"].(bool); !ok {
		c.t.Fatalf("upload failed: %v", resp.Results[0]["error"])
	}
	return resp.Results[0]["file"].(map[string]any)
}

func TestUploadStoresAndServesThumbnail(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	file := c.uploadWithThumb("photo.jpg", "/", []byte("pretend this is a photo"), samplePNG(t, 900, 450))
	id := file["id"].(string)

	w := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET thumb: %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q — a picture of a stored file must not be cached publicly", got)
	}

	// Whatever was sent, what comes back is the server's own JPEG at its own
	// size: the PNG went in 900px wide and is re-encoded on the way through.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("the stored thumbnail is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want jpeg", format)
	}
	if cfg.Width != thumb.Size {
		t.Errorf("width = %d, want %d", cfg.Width, thumb.Size)
	}

	// And the listing says which rows have one, so no row asks in vain.
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	thumbs, _ := listing["thumbs"].([]any)
	if len(thumbs) != 1 || thumbs[0] != id {
		t.Errorf("listing thumbs = %v, want [%s]", thumbs, id)
	}
}

func TestThumbnailMissingIsNotFound(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	file := c.upload("notes.txt", "/", []byte("no picture here"))
	id := file["id"].(string)

	w := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET thumb: %d %s, want 404", w.Code, w.Body.String())
	}

	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	if thumbs, _ := listing["thumbs"].([]any); len(thumbs) != 0 {
		t.Errorf("listing thumbs = %v, want none", thumbs)
	}
}

// The backfill path: a file stored before thumbnails existed gets one the
// first time it is opened in the browser.
func TestPutThumbnail(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	file := c.upload("photo.jpg", "/", []byte("pretend this is a photo"))
	id := file["id"].(string)

	var body bytes.Buffer
	if err := jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil); err != nil {
		t.Fatalf("encoding: %v", err)
	}

	w := c.do(http.MethodPut, "/api/files/"+id+"/thumb", &body, "image/jpeg")
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT thumb: %d %s", w.Code, w.Body.String())
	}

	if got := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, ""); got.Code != http.StatusOK {
		t.Fatalf("GET thumb after PUT: %d %s", got.Code, got.Body.String())
	}
}

func TestPutThumbnailRejectsWhatIsNotAnImage(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	file := c.upload("photo.jpg", "/", []byte("pretend this is a photo"))
	id := file["id"].(string)

	w := c.do(http.MethodPut, "/api/files/"+id+"/thumb",
		bytes.NewReader([]byte("<html>not a picture</html>")), "image/jpeg")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT thumb: %d %s, want 400", w.Code, w.Body.String())
	}

	// Nothing was stored, so the row keeps its icon rather than a broken image.
	if got := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, ""); got.Code != http.StatusNotFound {
		t.Errorf("GET thumb: %d, want 404", got.Code)
	}
}

// A thumbnail that will not store must not fail the upload: the file is
// already scattered by then, and the list works without a picture.
func TestBadThumbnailStillStoresTheFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	file := c.uploadWithThumb("photo.jpg", "/", []byte("pretend this is a photo"),
		[]byte("this is not an image at all"))
	id := file["id"].(string)

	w := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("the file did not survive a bad thumbnail: %d %s", w.Code, w.Body.String())
	}
	if got := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, ""); got.Code != http.StatusNotFound {
		t.Errorf("GET thumb: %d, want 404", got.Code)
	}
}

func TestDeletedFileLosesItsThumbnail(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)

	file := c.uploadWithThumb("photo.jpg", "/", []byte("pretend this is a photo"), samplePNG(t, 300, 300))
	id := file["id"].(string)

	if w := c.do(http.MethodDelete, "/api/files/"+id, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, ""); w.Code == http.StatusOK {
		t.Error("the deleted file still serves a thumbnail")
	}
}

// --- A folder's own picture ------------------------------------------------
//
// A folder wears a picture of something inside it. Nothing new is stored for
// it: the answer is the ID of a file that already has a thumbnail, drawn
// through the same endpoint that file's own row draws through.

func TestAFolderIsDrawnWithAPictureOnlyOnceOneIsPicked(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films/batman"})
	first := c.uploadWithThumb("Batman Begins 2005.mkv", "/films/batman",
		[]byte("pretend this is a film"), samplePNG(t, 400, 600))
	second := c.uploadWithThumb("The Dark Knight 2008.mkv", "/films/batman",
		[]byte("pretend this is a film"), samplePNG(t, 400, 600))

	// The listing of the folder above carries an entry for the subfolder, so
	// the browser knows there is something to choose — and no picture, because
	// nothing chooses one on anybody's behalf.
	_, listing := c.json(http.MethodGet, "/api/files?path=/films", nil)
	art, _ := listing["folder_art"].(map[string]any)
	batman, listed := art["/films/batman"].(map[string]any)
	if !listed {
		t.Fatalf("no entry for /films/batman: %v", listing["folder_art"])
	}
	if batman["id"] != nil {
		t.Fatalf("the folder picked a picture for itself: %v", batman)
	}

	// The picker offers both, and choosing one sticks.
	w, body := c.json(http.MethodGet, "/api/folders/art?path=/films/batman", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("folder art: %d %v", w.Code, body)
	}
	if body["art"] != nil {
		t.Errorf("art = %v, want nothing chosen", body["art"])
	}
	if candidates, _ := body["candidates"].([]any); len(candidates) != 2 {
		t.Fatalf("candidates = %v, want both films", body["candidates"])
	}

	wanted := second["id"].(string)
	if w, body := c.json(http.MethodPost, "/api/folders/art",
		map[string]any{"path": "/films/batman", "id": wanted}); w.Code != http.StatusOK {
		t.Fatalf("choosing: %d %v", w.Code, body)
	}

	_, listing = c.json(http.MethodGet, "/api/files?path=/films", nil)
	chosen := listing["folder_art"].(map[string]any)["/films/batman"].(map[string]any)
	if chosen["id"] != wanted {
		t.Errorf("after choosing = %v, want %s", chosen, wanted)
	}

	// Another choice replaces it; an empty one takes the picture away and
	// leaves the folder as it started.
	if w, _ := c.json(http.MethodPost, "/api/folders/art",
		map[string]any{"path": "/films/batman", "id": first["id"]}); w.Code != http.StatusOK {
		t.Fatalf("choosing again: %d", w.Code)
	}
	if w, body := c.json(http.MethodPost, "/api/folders/art",
		map[string]any{"path": "/films/batman", "id": ""}); w.Code != http.StatusOK {
		t.Fatalf("clearing: %d %v", w.Code, body)
	}
	_, listing = c.json(http.MethodGet, "/api/files?path=/films", nil)
	back := listing["folder_art"].(map[string]any)["/films/batman"].(map[string]any)
	if back["id"] != nil {
		t.Errorf("after clearing = %v, want no picture", back)
	}
}

func TestAFolderPictureMustBeSomethingInsideTheFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/elsewhere"})
	outsider := c.uploadWithThumb("stray.jpg", "/elsewhere",
		[]byte("pretend this is a photo"), samplePNG(t, 300, 300))

	if w, _ := c.json(http.MethodPost, "/api/folders/art",
		map[string]any{"path": "/films", "id": outsider["id"]}); w.Code == http.StatusOK {
		t.Error("a folder was allowed to wear a picture of something it does not hold")
	}

	// A folder with nothing picturable in it is left out of the map entirely,
	// which is what keeps the control to choose one off the screen.
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	if art, _ := listing["folder_art"].(map[string]any); art["/films"] != nil {
		t.Errorf("a folder with nothing picturable inside it offers something: %v", art["/films"])
	}
}
