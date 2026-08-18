package davfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/vault"
)

const testPassword = "a perfectly ordinary password"

// newTestVault builds an unlocked vault over three local folders standing in
// for cloud accounts, cut into small chunks so a modest payload still spans
// several of them.
func newTestVault(t *testing.T) *vault.Vault {
	t.Helper()

	dir := t.TempDir()
	v, err := vault.Open(filepath.Join(dir, "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	// The read history saves itself on a goroutine, into the directory the
	// vault file is in — let it land before that directory is removed.
	t.Cleanup(v.AwaitReadHistory)

	for i := 0; i < 3; i++ {
		if _, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "cloud-" + string(rune('a'+i)),
			Options: map[string]string{"path": filepath.Join(dir, "cloud", string(rune('a'+i)))},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	return v
}

func payload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*11 + i/97)
	}
	return out
}

// newTestServer returns the WebDAV handler and a helper that makes an
// authenticated request against it.
func newTestServer(t *testing.T, v *vault.Vault) *httptest.Server {
	t.Helper()
	h, err := Handler(v, Options{Prefix: "/dav"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/dav", h)
	mux.Handle("/dav/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.SetBasicAuth("anyone", testPassword)
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPutAndGetRoundTrip(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	body := payload(9000)
	resp := do(t, srv, "PUT", "/dav/film.mkv", bytes.NewReader(body), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 201 or 204", resp.StatusCode)
	}

	// It really went into the vault, chunked, not into some side channel.
	entry, err := v.EntryByPath(vault.MainScope, "/film.mkv")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if !entry.Chunked() {
		t.Error("a file stored over WebDAV was not chunked")
	}
	if entry.Size != int64(len(body)) {
		t.Errorf("stored size = %d, want %d", entry.Size, len(body))
	}

	resp = do(t, srv, "GET", "/dav/film.mkv", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("the file did not come back as it went in")
	}
}

// A range request against a small file still has to come back correct, and has
// to come back as a 206 with the range advertised — that combination is what
// makes a player seek instead of downloading from the start.
func TestRangeRequestServesJustThatRange(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	body := payload(40000)
	if resp := do(t, srv, "PUT", "/dav/movie.mkv", bytes.NewReader(body), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	resp := do(t, srv, "GET", "/dav/movie.mkv", nil, map[string]string{
		"Range": "bytes=20000-20099",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !bytes.Equal(got, body[20000:20100]) {
		t.Error("the range returned the wrong bytes")
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 20000-20099/") {
		t.Errorf("Content-Range = %q", cr)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("the server did not advertise range support, which is what makes a player seek")
	}
}

// The same, over a file genuinely large enough to span several chunks, so the
// range really is served by seeking into the middle of a chunked file rather
// than by slicing one that happened to fit in a single chunk.
func TestRangeRequestAcrossManyChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("pushes tens of megabytes through the whole pipeline")
	}

	v := newTestVault(t)
	srv := newTestServer(t, v)

	// Just over two default chunks.
	body := payload(2*archive.DefaultChunkSize + 4096)
	if resp := do(t, srv, "PUT", "/dav/film.mkv", bytes.NewReader(body), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	entry, err := v.EntryByPath(vault.MainScope, "/film.mkv")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if entry.ChunkCount != 3 {
		t.Fatalf("chunk count = %d, want 3", entry.ChunkCount)
	}

	// A range wholly inside the last chunk, which is only reachable by seeking
	// past the two before it.
	start := 2*archive.DefaultChunkSize + 100
	resp := do(t, srv, "GET", "/dav/film.mkv", nil, map[string]string{
		"Range": fmt.Sprintf("bytes=%d-%d", start, start+255),
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !bytes.Equal(got, body[start:start+256]) {
		t.Error("the range into the last chunk returned the wrong bytes")
	}
}

func TestPropfindListsTheTree(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if err := v.Mkdir(vault.MainScope, "/films"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if resp := do(t, srv, "PUT", "/dav/films/one.mkv", bytes.NewReader(payload(2000)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	resp := do(t, srv, "PROPFIND", "/dav/films/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d, want 207", resp.StatusCode)
	}
	doc, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !strings.Contains(string(doc), "one.mkv") {
		t.Errorf("the listing does not mention the file: %s", doc)
	}
	if !strings.Contains(string(doc), "2000") {
		t.Errorf("the listing does not carry the file's size: %s", doc)
	}
}

func TestMkcolDeleteAndMove(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "MKCOL", "/dav/shows", nil, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL = %d, want 201", resp.StatusCode)
	}
	if !v.FolderExists(vault.MainScope, "/shows") {
		t.Fatal("MKCOL did not create the folder in the vault")
	}

	if resp := do(t, srv, "PUT", "/dav/shows/ep1.mkv", bytes.NewReader(payload(1500)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	resp := do(t, srv, "MOVE", "/dav/shows/ep1.mkv", nil, map[string]string{
		"Destination": srv.URL + "/dav/shows/episode-one.mkv",
	})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE = %d", resp.StatusCode)
	}
	if _, err := v.EntryByPath(vault.MainScope, "/shows/episode-one.mkv"); err != nil {
		t.Errorf("the moved file is not at its new path: %v", err)
	}
	if _, err := v.EntryByPath(vault.MainScope, "/shows/ep1.mkv"); err == nil {
		t.Error("the file is still at its old path too")
	}

	if resp := do(t, srv, "DELETE", "/dav/shows/episode-one.mkv", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", resp.StatusCode)
	}
	if _, err := v.EntryByPath(vault.MainScope, "/shows/episode-one.mkv"); err == nil {
		t.Error("the deleted file is still in the index")
	}
}

func TestOverwritingAFileReplacesIt(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "PUT", "/dav/notes.bin", bytes.NewReader(payload(5000)), nil); resp.StatusCode > 299 {
		t.Fatalf("first PUT = %d", resp.StatusCode)
	}
	second := payload(700)
	if resp := do(t, srv, "PUT", "/dav/notes.bin", bytes.NewReader(second), nil); resp.StatusCode > 299 {
		t.Fatalf("second PUT = %d", resp.StatusCode)
	}

	listing, err := v.List(vault.MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 {
		names := []string{}
		for _, f := range listing.Files {
			names = append(names, f.Name)
		}
		t.Fatalf("the folder holds %d files (%v), want the one that replaced the other",
			len(listing.Files), names)
	}
	if listing.Files[0].Size != int64(len(second)) {
		t.Errorf("size = %d, want the replacement's %d", listing.Files[0].Size, len(second))
	}
}

func TestMissingFileIsNotFound(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "GET", "/dav/nothing-here.mkv", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET of a missing file = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestAuthRejectsAWrongPassword(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	req, _ := http.NewRequest("GET", srv.URL+"/dav/", nil)
	req.SetBasicAuth("anyone", "not the password")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Error("a 401 without a Basic challenge gives the client nothing to prompt with")
	}
}

func TestAuthRejectsNoCredentials(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	resp, err := srv.Client().Get(srv.URL + "/dav/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credentials = %d, want 401", resp.StatusCode)
	}
}

// A mounted share outlives the idle timeout, so the credential it keeps sending
// is what brings a locked vault back rather than leaving the mount dead.
func TestCorrectPasswordUnlocksALockedVault(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "PUT", "/dav/kept.bin", bytes.NewReader(payload(1200)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	v.Lock()
	if v.Unlocked() {
		t.Fatal("the vault did not lock")
	}

	resp := do(t, srv, "GET", "/dav/kept.bin", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET against a locked vault = %d, want 200 after the credential unlocks it", resp.StatusCode)
	}
	if !v.Unlocked() {
		t.Error("the vault is still locked")
	}
}

// The cache must not outlive the lock: a credential verified before locking
// cannot be what lets a request straight back in afterwards without the
// password being checked again.
func TestVerifierDoesNotCacheThroughALock(t *testing.T) {
	v := newTestVault(t)
	auth, err := newVerifier(v, DefaultVerifyTTL)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}

	if err := auth.check(testPassword); err != nil {
		t.Fatalf("check: %v", err)
	}
	v.Lock()

	// Still allowed — but only by unlocking again, which is the point.
	if err := auth.check(testPassword); err != nil {
		t.Fatalf("check after locking: %v", err)
	}
	if !v.Unlocked() {
		t.Error("the cached answer was used instead of re-verifying against a locked vault")
	}

	v.Lock()
	if err := auth.check("still not the password"); err == nil {
		t.Error("a wrong password was accepted")
	}
	if v.Unlocked() {
		t.Error("a wrong password unlocked the vault")
	}
}

func TestVerifierRemembersAcrossCalls(t *testing.T) {
	v := newTestVault(t)
	auth, err := newVerifier(v, DefaultVerifyTTL)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}

	if err := auth.check(testPassword); err != nil {
		t.Fatalf("check: %v", err)
	}
	auth.mu.Lock()
	remembered := len(auth.seen)
	auth.mu.Unlock()
	if remembered != 1 {
		t.Fatalf("the verifier remembered %d credentials, want 1", remembered)
	}

	// A wrong password is never remembered, so it cannot fill the map.
	for i := 0; i < 5; i++ {
		if err := auth.check(fmt.Sprintf("wrong %d", i)); err == nil {
			t.Fatal("a wrong password was accepted")
		}
	}
	auth.mu.Lock()
	after := len(auth.seen)
	auth.mu.Unlock()
	if after != 1 {
		t.Errorf("the verifier is holding %d credentials after five wrong guesses, want 1", after)
	}
}

// The map holds an HMAC of the password under a per-process key, not the
// password.
func TestVerifierDoesNotHoldThePassword(t *testing.T) {
	v := newTestVault(t)
	auth, err := newVerifier(v, DefaultVerifyTTL)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	if err := auth.check(testPassword); err != nil {
		t.Fatalf("check: %v", err)
	}

	auth.mu.Lock()
	defer auth.mu.Unlock()
	for key := range auth.seen {
		if strings.Contains(key, testPassword) {
			t.Error("the verifier is holding the password itself")
		}
	}
}

// ---------------------------------------------------------------------------
// Path handling
// ---------------------------------------------------------------------------

func TestCleanNameRefusesEscapingTheRoot(t *testing.T) {
	for _, name := range []string{"/../etc/passwd", "../secrets", "/a/../../b"} {
		if _, err := cleanName(name); err == nil {
			t.Errorf("cleanName(%q) was allowed, want a refusal", name)
		}
	}

	for name, want := range map[string]string{
		"":             "/",
		"/":            "/",
		"/a/b":         "/a/b",
		"/a/b/":        "/a/b",
		"a/b":          "/a/b",
		"/a//b":        "/a/b",
		"/a/./b":       "/a/b",
		"/a/c/../b":    "/a/b",
		"/films/x.mkv": "/films/x.mkv",
	} {
		got, err := cleanName(name)
		if err != nil {
			t.Errorf("cleanName(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("cleanName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestStatAndOpenOnFoldersAndFiles(t *testing.T) {
	v := newTestVault(t)
	fs := New(v)
	ctx := context.Background()

	if err := v.Mkdir(vault.MainScope, "/films"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, vault.MainScope, "/films", "a.mkv", payload(1000), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	info, err := fs.Stat(ctx, "/films")
	if err != nil {
		t.Fatalf("Stat folder: %v", err)
	}
	if !info.IsDir() || info.Name() != "films" {
		t.Errorf("folder stat = %q dir=%v", info.Name(), info.IsDir())
	}

	info, err = fs.Stat(ctx, "/films/a.mkv")
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if info.IsDir() || info.Size() != 1000 {
		t.Errorf("file stat = size %d dir=%v", info.Size(), info.IsDir())
	}

	if _, err := fs.Stat(ctx, "/films/missing.mkv"); !os.IsNotExist(err) {
		t.Errorf("Stat of a missing file = %v, want a not-exist error", err)
	}

	// Readdir pages the way os.File does, ending in io.EOF.
	dir, err := fs.OpenFile(ctx, "/films", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile folder: %v", err)
	}
	entries, err := dir.Readdir(1)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Readdir(1) returned %d entries", len(entries))
	}
	if _, err := dir.Readdir(1); err != io.EOF {
		t.Errorf("Readdir past the end = %v, want io.EOF", err)
	}
}

func TestMkdirNeedsItsParent(t *testing.T) {
	v := newTestVault(t)
	fs := New(v)
	ctx := context.Background()

	if err := fs.Mkdir(ctx, "/a/b/c", 0); !os.IsNotExist(err) {
		t.Errorf("Mkdir with no parent = %v, want a not-exist error", err)
	}
	if err := fs.Mkdir(ctx, "/a", 0); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := fs.Mkdir(ctx, "/a", 0); !os.IsExist(err) {
		t.Errorf("Mkdir over an existing folder = %v, want an exists error", err)
	}
}

// Renaming a folder over the share moves everything beneath it, in one write
// and without transferring a byte.
func TestMoveOnACollection(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "MKCOL", "/dav/media", nil, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL = %d", resp.StatusCode)
	}
	if resp := do(t, srv, "MKCOL", "/dav/media/films", nil, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL = %d", resp.StatusCode)
	}
	body := payload(4000)
	if resp := do(t, srv, "PUT", "/dav/media/films/one.mkv", bytes.NewReader(body), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	resp := do(t, srv, "MOVE", "/dav/media", nil, map[string]string{
		"Destination": srv.URL + "/dav/library",
	})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE of a collection = %d", resp.StatusCode)
	}

	if !v.FolderExists(vault.MainScope, "/library/films") {
		t.Error("the subfolder did not move with its parent")
	}
	if v.FolderExists(vault.MainScope, "/media") {
		t.Error("the old folder is still there")
	}

	// And the file inside it still reads, at its new path.
	got := do(t, srv, "GET", "/dav/library/films/one.mkv", nil, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET after the move = %d", got.StatusCode)
	}
	data, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Error("the moved file does not read back the same")
	}
}

// Appending is exercised through the FileSystem rather than over HTTP, because
// WebDAV has no append verb — PUT always opens with O_TRUNC. It is the
// filesystem contract that has to hold, for whatever calls it.
func TestOpenFileAppends(t *testing.T) {
	v := newTestVault(t)
	fs := New(v)
	ctx := context.Background()

	first := payload(3000)
	if _, _, err := v.Upload(ctx, vault.MainScope, "/", "log.bin", first, vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	second := []byte("...and this came later")
	f, err := fs.OpenFile(ctx, "/log.bin", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("OpenFile for append: %v", err)
	}
	if _, err := f.Write(second); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entry, err := v.EntryByPath(vault.MainScope, "/log.bin")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if entry.Size != int64(len(first)+len(second)) {
		t.Errorf("size = %d, want %d", entry.Size, len(first)+len(second))
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, append(append([]byte{}, first...), second...)) {
		t.Error("the appended file is not the original followed by the new bytes")
	}
}

// O_TRUNC beside O_APPEND truncates, which is what os.OpenFile does — and is
// what keeps a PUT from accidentally appending to what it means to replace.
func TestOpenFileTruncateWinsOverAppend(t *testing.T) {
	v := newTestVault(t)
	fs := New(v)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, vault.MainScope, "/", "notes.bin", payload(5000), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	replacement := []byte("all of it, replaced")
	f, err := fs.OpenFile(ctx, "/notes.bin", os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write(replacement); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entry, err := v.EntryByPath(vault.MainScope, "/notes.bin")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if entry.Size != int64(len(replacement)) {
		t.Errorf("size = %d, want the replacement's %d", entry.Size, len(replacement))
	}
}

// Appending to a path that holds nothing yet simply creates it.
func TestOpenFileAppendsToANewFile(t *testing.T) {
	v := newTestVault(t)
	fs := New(v)
	ctx := context.Background()

	body := []byte("the first thing written")
	f, err := fs.OpenFile(ctx, "/fresh.bin", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entry, err := v.EntryByPath(vault.MainScope, "/fresh.bin")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if entry.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", entry.Size, len(body))
	}
}

// A sub vault is never on the share. Not while locked, and not while unlocked
// either — the adapter is pinned to the main vault by a constant, so there is
// no request that can ask for a different scope and no setting that changes it.
//
// This is the guarantee somebody puts a file in a sub vault for. A mounted
// drive is a folder every process running as that user can read, that a backup
// agent will copy elsewhere, and that stays mounted long after they have walked
// away from the machine.
func TestSubVaultIsNeverVisibleOverWebDAV(t *testing.T) {
	v := newTestVault(t)
	ctx := context.Background()

	sub, err := v.CreateSubVault("Taxes", "the sub vault's own password")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	scope := vault.Scope(sub.ID)

	if err := v.Mkdir(vault.MainScope, "/Public"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, vault.MainScope, "/Public", "open.txt", []byte("nothing secret"), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload to main: %v", err)
	}
	if err := v.Mkdir(scope, "/Papers"); err != nil {
		t.Fatalf("Mkdir in the sub vault: %v", err)
	}
	if _, _, err := v.Upload(ctx, scope, "/Papers", "p60.pdf", []byte("a payslip"), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload to the sub vault: %v", err)
	}

	// Unlocked in the app, which is the case that matters: the share still
	// cannot see it.
	if !v.SubVaultUnlocked(sub.ID) {
		t.Fatal("the sub vault should be open at this point")
	}

	fs := New(v)

	if _, err := fs.Stat(ctx, "/Papers"); err == nil {
		t.Error("a sub vault's folder is reachable over WebDAV")
	}
	if _, err := fs.Stat(ctx, "/Papers/p60.pdf"); err == nil {
		t.Error("a sub vault's file is reachable over WebDAV")
	}

	root, err := fs.OpenFile(ctx, "/", 0, 0)
	if err != nil {
		t.Fatalf("OpenFile(/): %v", err)
	}
	defer root.Close()
	infos, err := root.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	for _, info := range infos {
		if info.Name() == "Papers" {
			t.Error("a sub vault's folder is listed at the root of the share")
		}
	}
	if len(infos) != 1 || infos[0].Name() != "Public" {
		names := make([]string, 0, len(infos))
		for _, info := range infos {
			names = append(names, info.Name())
		}
		t.Errorf("the share lists %v, want just the main vault's Public", names)
	}

	// And writing at a sub vault's path lands in the main vault rather than
	// reaching into it — the two trees are separate, so this is a new file at
	// a path that happens to look the same.
	if err := fs.Mkdir(ctx, "/Papers", 0o755); err != nil {
		t.Fatalf("Mkdir over WebDAV at a path a sub vault also uses: %v", err)
	}
	if !v.FolderExists(vault.MainScope, "/Papers") {
		t.Error("the folder should have been made in the main vault")
	}
	listing, err := v.List(scope, "/Papers")
	if err != nil {
		t.Fatalf("List inside the sub vault: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "p60.pdf" {
		t.Error("the sub vault's own /Papers was disturbed by a write over the share")
	}
}
