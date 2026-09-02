package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// zipFixture is a vault with a small tree under /photos.
func zipFixture(t *testing.T) *testClient {
	t.Helper()
	c := newTestClient(t)
	c.setup("pw", 3)
	c.mkdir("/photos")
	c.mkdir("/photos/2019")
	c.mkdir("/photos/empty")
	c.upload("a.jpg", "/photos", []byte("aaa"))
	c.upload("b.jpg", "/photos/2019", []byte("bbbb"))
	c.upload("elsewhere.txt", "/", []byte("not in the folder"))
	return c
}

// mintZip asks for a folder's download link through the endpoint the browser
// uses.
func (c *testClient) mintZip(path string) (*httptest.ResponseRecorder, map[string]any) {
	c.t.Helper()
	return c.json(http.MethodPost, "/api/folders/zip", map[string]any{"path": path})
}

// fetchZip follows a link with no session at all, the way a download does.
func fetchZip(t *testing.T, c *testClient, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	req.Host = "example.test"
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

// archiveNames reads the entry names out of a zip, sorted.
func archiveNames(t *testing.T, data []byte) ([]string, map[string]string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	names := []string{}
	contents := map[string]string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		contents[f.Name] = string(body)
	}
	sort.Strings(names)
	return names, contents
}

// The link says what the archive will hold, and following it — with no
// session, since the ticket is the credential — streams the folder as a zip.
func TestFolderZipLinkAndDownload(t *testing.T) {
	c := zipFixture(t)

	w, link := c.mintZip("/photos")
	if w.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	if link["name"] != "photos.zip" {
		t.Errorf("name = %v, want photos.zip", link["name"])
	}
	if number(t, link, "files") != 2 || number(t, link, "bytes") != 7 || number(t, link, "folders") != 2 {
		t.Errorf("the link describes %v", link)
	}
	url, _ := link["url"].(string)
	if !strings.HasPrefix(url, "/zip/") || !strings.HasSuffix(url, "/photos.zip") {
		t.Fatalf("url = %q", url)
	}

	resp := fetchZip(t, c, http.MethodGet, url)
	if resp.Code != http.StatusOK {
		t.Fatalf("download: %d %s", resp.Code, resp.Body.String())
	}
	if ct := resp.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "photos.zip") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cc := resp.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q: the plaintext must never be cached", cc)
	}

	names, contents := archiveNames(t, resp.Body.Bytes())
	want := []string{"photos/2019/", "photos/2019/b.jpg", "photos/a.jpg", "photos/empty/"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("archive holds %v, want %v", names, want)
	}
	if contents["photos/a.jpg"] != "aaa" || contents["photos/2019/b.jpg"] != "bbbb" {
		t.Errorf("contents came back as %v", contents)
	}

	// A HEAD says what a GET would, without the archive behind it.
	head := fetchZip(t, c, http.MethodHead, url)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD answered %d with %d bytes", head.Code, head.Body.Len())
	}
	if head.Header().Get("X-Sand-Files") != "2" {
		t.Errorf("HEAD did not say how many files: %v", head.Header())
	}
}

func TestFolderZipLinkNeedsASession(t *testing.T) {
	c := zipFixture(t)
	c.cookies = nil
	w, _ := c.mintZip("/photos")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("minting without a session answered %d, want 401", w.Code)
	}
}

// Everything that can be refused is refused on the link, where the page can
// read the answer, rather than on the download.
func TestFolderZipLinkRefusals(t *testing.T) {
	c := zipFixture(t)

	if w, _ := c.mintZip("/nowhere"); w.Code != http.StatusNotFound {
		t.Errorf("a folder that is not there answered %d, want 404", w.Code)
	}
	if w, body := c.mintZip("/photos/empty"); w.Code != http.StatusBadRequest {
		t.Errorf("an empty folder answered %d, want 400: %v", w.Code, body)
	}
}

// A link dies with the keys it was minted against, and with its own clock.
func TestFolderZipLinkExpires(t *testing.T) {
	c := zipFixture(t)

	_, link := c.mintZip("/photos")
	url, _ := link["url"].(string)

	// Aged out: the ticket is still in the store, but past its time.
	token := strings.Split(strings.TrimPrefix(url, "/zip/"), "/")[0]
	c.server.zips.mu.Lock()
	stale := c.server.zips.tickets[token]
	stale.expiry = time.Now().Add(-time.Second)
	c.server.zips.tickets[token] = stale
	c.server.zips.mu.Unlock()
	resp := fetchZip(t, c, http.MethodGet, url)
	if resp.Code != http.StatusNotFound {
		t.Errorf("an expired link answered %d, want 404", resp.Code)
	}
	// Whoever followed the link is a browser, not the app: the answer is a
	// page that says what happened, not JSON with a code in it.
	if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("an expired link answered with %q, want a page a browser can show", ct)
	}
	if !strings.Contains(resp.Body.String(), "expired") || !strings.Contains(resp.Body.String(), "Download as zip") {
		t.Errorf("the refusal does not say what happened or what to do: %s", resp.Body.String())
	}

	// Locked: every link goes with the keys.
	_, link = c.mintZip("/photos")
	url, _ = link["url"].(string)
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if resp := fetchZip(t, c, http.MethodGet, url); resp.Code == http.StatusOK {
		t.Error("a link minted before the vault locked still answered")
	}
	// A link lasts three hours unless the vault says otherwise — long enough
	// to be carried to the machine with the disk for it.
	if c.server.zips.ttl != vault.DefaultLinkLifetime {
		t.Errorf("zip links last %v, want %v", c.server.zips.ttl, vault.DefaultLinkLifetime)
	}
	if resp := fetchZip(t, c, http.MethodGet, "/zip/nosuchtoken/x.zip"); resp.Code != http.StatusNotFound {
		t.Errorf("an unknown token answered %d, want 404", resp.Code)
	}
}

// Streaming a folder must not cost the folder.
//
// The assertion is the one the content endpoint makes for one file, made for
// a folder: a folder four times larger must not cost anything like four times
// as much to hand back, and the ceiling is deliberately below what buffering
// it would need. This is what lets a 200 GB folder leave a Raspberry Pi.
func TestFolderZipDoesNotScaleWithTheFolder(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates hundreds of megabytes")
	}

	measure := func(size int) uint64 {
		c := newTestClient(t)
		c.setup("zip-serving-passphrase", 3)
		c.mkdir("/big")

		// Incompressible, because a film is, and three of them so the archive
		// spans files rather than measuring one.
		for i := 0; i < 3; i++ {
			payload := make([]byte, size)
			rand.New(rand.NewSource(int64(i))).Read(payload)
			c.upload(fmt.Sprintf("film-%d.bin", i), "/big", payload)
		}

		_, link := c.mintZip("/big")
		url, _ := link["url"].(string)

		var peak uint64
		for attempt := 0; attempt < 2; attempt++ {
			peak = peakHeapDuring(t, func() {
				req := httptest.NewRequest(http.MethodGet, url, nil)
				req.Host = "example.test"
				w := httptest.NewRecorder()
				// The recorder would hold the whole archive; without a body
				// it throws the bytes away, so what is measured is what the
				// server spent producing them.
				w.Body = nil
				c.handler.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("zip GET = %d", w.Code)
				}
			})
		}
		return peak
	}

	// Both sizes are whole chunks, so the two folders differ in how much
	// they hold and not in how big a chunk is: a file smaller than one chunk
	// costs less to gather than a file of several, and that is the chunk
	// window's size showing, not the folder's.
	const small, large = 16 << 20, 64 << 20
	smallPeak, largePeak := measure(small), measure(large)

	fmt.Printf("  zipping three %d MB files peaked at %.1f MB live\n", small>>20, float64(smallPeak)/(1<<20))
	fmt.Printf("  zipping three %d MB files peaked at %.1f MB live\n", large>>20, float64(largePeak)/(1<<20))

	// Buffering would put the folder on the heap, so the larger one would
	// cost at least 144 MB more than the smaller. Streaming costs the chunk
	// window either way; the growth allowed here is a wide margin around that.
	growth := int64(largePeak) - int64(smallPeak)
	if growth > 3*(large-small)/4 {
		t.Errorf("a folder of three %d MB files cost %.1f MB more to zip than three %d MB files did; "+
			"the archive is being buffered rather than streamed",
			large>>20, float64(growth)/(1<<20), small>>20)
	}
	if largePeak > 3*large {
		t.Errorf("zipping three %d MB files held %.1f MB live — more than the folder",
			large>>20, float64(largePeak)/(1<<20))
	}
}
