package davfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// serve runs one request straight through the endpoint, without a network in
// between, so the body and the context can be shaped exactly: a body shorter
// than its Content-Length, or a context already done.
func serve(t *testing.T, v *vault.Vault, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	h, err := Handler(v, Options{Prefix: "/dav"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	req.SetBasicAuth("anyone", testPassword)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// stored reads a file back out of the vault by path.
func stored(t *testing.T, v *vault.Vault, path string) []byte {
	t.Helper()
	entry, err := v.EntryByPath(vault.MainScope, path)
	if err != nil {
		t.Fatalf("EntryByPath(%s): %v", path, err)
	}
	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", path, err)
	}
	return data
}

// names lists what a folder holds, files and folders together.
func names(t *testing.T, v *vault.Vault, dir string) []string {
	t.Helper()
	listing, err := v.List(vault.MainScope, dir)
	if err != nil {
		t.Fatalf("List(%s): %v", dir, err)
	}
	var out []string
	out = append(out, listing.Folders...)
	for _, f := range listing.Files {
		out = append(out, f.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// A PUT that does not deliver what it promised
// ---------------------------------------------------------------------------

// The library closes the file whether or not the body arrived in full, and a
// close is what commits the upload. A body cut off partway must therefore be
// told apart from a whole one, or it is stored in place of the file it was
// meant to replace.
func TestShortPutLeavesTheExistingFileAlone(t *testing.T) {
	v := newTestVault(t)
	original := payload(5000)
	if _, _, err := v.Upload(context.Background(), vault.MainScope, "/", "keep.bin", original, vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Promises 9000 bytes, delivers 4000, and then ends cleanly — which is
	// what a cut-off body looks like from inside the filesystem.
	req := httptest.NewRequest("PUT", "/dav/keep.bin", bytes.NewReader(payload(4000)))
	req.ContentLength = 9000
	w := serve(t, v, req)
	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("a PUT whose body ended early was answered %d, want a failure", w.Code)
	}

	if got := stored(t, v, "/keep.bin"); !bytes.Equal(got, original) {
		t.Errorf("the file was replaced by the partial body: %d bytes, want the original %d", len(got), len(original))
	}
	if got := names(t, v, "/"); len(got) != 1 {
		t.Errorf("the folder holds %v, want just the original", got)
	}
}

// A body that fails partway through rather than ending early.
func TestPutWhoseBodyFailsIsNotCommitted(t *testing.T) {
	v := newTestVault(t)
	original := payload(5000)
	if _, _, err := v.Upload(context.Background(), vault.MainScope, "/", "keep.bin", original, vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	body := io.MultiReader(bytes.NewReader(payload(4000)), failingReader{})
	req := httptest.NewRequest("PUT", "/dav/keep.bin", io.NopCloser(body))
	req.ContentLength = 9000
	w := serve(t, v, req)
	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("a PUT whose body failed was answered %d, want a failure", w.Code)
	}
	if got := stored(t, v, "/keep.bin"); !bytes.Equal(got, original) {
		t.Error("the file was replaced by what arrived before the body failed")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the connection went away") }

// A chunked body says nothing about its length, so the only sign of a client
// that hung up is its context being done — which must be enough on its own.
func TestPutWithACancelledContextIsNotCommitted(t *testing.T) {
	v := newTestVault(t)
	original := payload(5000)
	if _, _, err := v.Upload(context.Background(), vault.MainScope, "/", "keep.bin", original, vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("PUT", "/dav/keep.bin", bytes.NewReader(payload(4000))).WithContext(ctx)
	req.ContentLength = -1
	w := serve(t, v, req)
	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("a PUT from a client that had gone was answered %d, want a failure", w.Code)
	}
	if got := stored(t, v, "/keep.bin"); !bytes.Equal(got, original) {
		t.Error("the file was replaced although the client had gone")
	}
}

// And the ordinary case still commits: a declared length that is met, and a
// chunked body that ends cleanly with the context alive.
func TestWholePutStillCommits(t *testing.T) {
	v := newTestVault(t)

	body := payload(6000)
	req := httptest.NewRequest("PUT", "/dav/whole.bin", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if w := serve(t, v, req); w.Code != http.StatusCreated {
		t.Fatalf("PUT with a met Content-Length = %d, want 201", w.Code)
	}

	req = httptest.NewRequest("PUT", "/dav/chunked.bin", bytes.NewReader(body))
	req.ContentLength = -1
	if w := serve(t, v, req); w.Code != http.StatusCreated {
		t.Fatalf("chunked PUT = %d, want 201", w.Code)
	}

	for _, name := range []string{"/whole.bin", "/chunked.bin"} {
		if got := stored(t, v, name); !bytes.Equal(got, body) {
			t.Errorf("%s did not come back as it went in", name)
		}
	}
}

// ---------------------------------------------------------------------------
// A ranged PUT
// ---------------------------------------------------------------------------

// The library does not read Content-Range, so a ranged PUT would store the
// range's bytes as the whole file. Refused, as RFC 7231 says it must be.
func TestRangedPutIsRefused(t *testing.T) {
	v := newTestVault(t)
	original := payload(5000)
	if _, _, err := v.Upload(context.Background(), vault.MainScope, "/", "keep.bin", original, vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	req := httptest.NewRequest("PUT", "/dav/keep.bin", bytes.NewReader(payload(100)))
	req.Header.Set("Content-Range", "bytes 1000-1099/5000")
	w := serve(t, v, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ranged PUT = %d, want 400", w.Code)
	}
	if got := stored(t, v, "/keep.bin"); !bytes.Equal(got, original) {
		t.Error("the ranged PUT changed the file")
	}
}

// ---------------------------------------------------------------------------
// COPY
// ---------------------------------------------------------------------------

// removeParts deletes a file's parts from the named cloud folders, which is
// what an account gone dark looks like to a read.
func removeParts(t *testing.T, entry *vault.Entry, roots ...string) {
	t.Helper()
	removed := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if strings.Contains(d.Name(), entry.ArchiveID) {
				removed++
				return os.Remove(p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if removed == 0 {
		t.Fatalf("no parts of %s found under %v", entry.Name, roots)
	}
}

// A COPY over an existing file reads the source and writes it to the
// destination. When the read fails partway, the destination must be what it
// was — not deleted ahead of a replacement that never came, and not replaced
// by the part that did.
func TestCopyWhoseSourceFailsKeepsTheDestination(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	source := payload(9000)
	if resp := do(t, srv, "PUT", "/dav/source.bin", bytes.NewReader(source), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT source = %d", resp.StatusCode)
	}
	target := payload(5000)
	if resp := do(t, srv, "PUT", "/dav/target.bin", bytes.NewReader(target), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT target = %d", resp.StatusCode)
	}

	// Two of the three accounts lose the source's parts: a 2-of-3 file with
	// one shard left cannot be read.
	src, err := v.EntryByPath(vault.MainScope, "/source.bin")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	removeParts(t, src, cloudRoots(t, v)[1:]...)

	resp := do(t, srv, "COPY", "/dav/source.bin", nil, map[string]string{
		"Destination": srv.URL + "/dav/target.bin",
		"Overwrite":   "T",
	})
	if resp.StatusCode < 300 {
		t.Fatalf("COPY from an unreadable source = %d, want a failure", resp.StatusCode)
	}

	if got := stored(t, v, "/target.bin"); !bytes.Equal(got, target) {
		t.Errorf("the destination was changed by a COPY that failed: %d bytes, want the original %d", len(got), len(target))
	}
	if got := names(t, v, "/"); len(got) != 2 {
		t.Errorf("the folder holds %v, want the source and the untouched destination", got)
	}
}

// cloudRoots is where each of the test vault's accounts keeps its objects.
func cloudRoots(t *testing.T, v *vault.Vault) []string {
	t.Helper()
	configs, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	roots := make([]string, 0, len(configs))
	for _, cfg := range configs {
		roots = append(roots, cfg.Options["path"])
	}
	return roots
}

// A COPY that succeeds over an existing file leaves exactly one file there,
// with the source's contents, and no trace of the one it replaced.
func TestCopyOverAFileReplacesIt(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	source := payload(9000)
	if resp := do(t, srv, "PUT", "/dav/source.bin", bytes.NewReader(source), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT source = %d", resp.StatusCode)
	}
	if resp := do(t, srv, "PUT", "/dav/target.bin", bytes.NewReader(payload(5000)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT target = %d", resp.StatusCode)
	}
	old, err := v.EntryByPath(vault.MainScope, "/target.bin")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}

	resp := do(t, srv, "COPY", "/dav/source.bin", nil, map[string]string{
		"Destination": srv.URL + "/dav/target.bin",
		"Overwrite":   "T",
	})
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		t.Fatalf("COPY = %d", resp.StatusCode)
	}

	if got := stored(t, v, "/target.bin"); !bytes.Equal(got, source) {
		t.Error("the destination does not hold the source's contents")
	}
	if got := names(t, v, "/"); len(got) != 2 {
		t.Errorf("the folder holds %v, want the source and the destination only", got)
	}
	if _, err := v.Entry(old.ID); err == nil {
		t.Error("the replaced file is still in the index under another name")
	}
}

// ---------------------------------------------------------------------------
// MOVE with Overwrite: T
// ---------------------------------------------------------------------------

// The library deletes the destination before renaming the source onto it. A
// rename that then fails used to leave the destination erased and the source
// where it was; now the destination is put aside and comes back.
func TestFailedMoveWithOverwriteKeepsTheDestination(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	target := payload(5000)
	if resp := do(t, srv, "PUT", "/dav/target.bin", bytes.NewReader(target), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT target = %d", resp.StatusCode)
	}

	// A source that is not there: the library clears the destination first
	// and only then finds out.
	resp := do(t, srv, "MOVE", "/dav/nothing-here.bin", nil, map[string]string{
		"Destination": srv.URL + "/dav/target.bin",
		"Overwrite":   "T",
	})
	if resp.StatusCode < 300 {
		t.Fatalf("MOVE of a missing file = %d, want a failure", resp.StatusCode)
	}

	if got := stored(t, v, "/target.bin"); !bytes.Equal(got, target) {
		t.Error("the destination did not survive a MOVE that failed")
	}
	if got := names(t, v, "/"); len(got) != 1 || got[0] != "target.bin" {
		t.Errorf("the folder holds %v, want just target.bin", got)
	}
}

// The same for a folder being moved over another folder.
func TestFailedMoveOverAFolderKeepsTheFolder(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "MKCOL", "/dav/kept", nil, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL = %d", resp.StatusCode)
	}
	inside := payload(3000)
	if resp := do(t, srv, "PUT", "/dav/kept/inside.bin", bytes.NewReader(inside), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	resp := do(t, srv, "MOVE", "/dav/nothing-here", nil, map[string]string{
		"Destination": srv.URL + "/dav/kept",
		"Overwrite":   "T",
	})
	if resp.StatusCode < 300 {
		t.Fatalf("MOVE of a missing folder = %d, want a failure", resp.StatusCode)
	}

	if !v.FolderExists(vault.MainScope, "/kept") {
		t.Fatal("the folder did not survive a MOVE that failed")
	}
	if got := stored(t, v, "/kept/inside.bin"); !bytes.Equal(got, inside) {
		t.Error("the folder's file did not come back with it")
	}
	if got := names(t, v, "/"); len(got) != 1 {
		t.Errorf("the root holds %v, want just the folder", got)
	}
}

// And a MOVE that succeeds replaces the destination outright: one file at the
// path, the source gone, the replaced one erased rather than left aside.
func TestMoveWithOverwriteReplacesTheDestination(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	source := payload(9000)
	if resp := do(t, srv, "PUT", "/dav/source.bin", bytes.NewReader(source), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT source = %d", resp.StatusCode)
	}
	if resp := do(t, srv, "PUT", "/dav/target.bin", bytes.NewReader(payload(5000)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT target = %d", resp.StatusCode)
	}
	old, err := v.EntryByPath(vault.MainScope, "/target.bin")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}

	resp := do(t, srv, "MOVE", "/dav/source.bin", nil, map[string]string{
		"Destination": srv.URL + "/dav/target.bin",
		"Overwrite":   "T",
	})
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		t.Fatalf("MOVE = %d", resp.StatusCode)
	}

	if got := stored(t, v, "/target.bin"); !bytes.Equal(got, source) {
		t.Error("the destination does not hold the moved file")
	}
	if got := names(t, v, "/"); len(got) != 1 || got[0] != "target.bin" {
		t.Errorf("the folder holds %v, want just target.bin", got)
	}
	if _, err := v.Entry(old.ID); err == nil {
		t.Error("the replaced file is still in the index")
	}

	// Without Overwrite the library refuses before touching anything, which
	// is the other half of the contract.
	if resp := do(t, srv, "PUT", "/dav/another.bin", bytes.NewReader(payload(100)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	resp = do(t, srv, "MOVE", "/dav/another.bin", nil, map[string]string{
		"Destination": srv.URL + "/dav/target.bin",
		"Overwrite":   "F",
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("MOVE with Overwrite: F onto an existing file = %d, want 412", resp.StatusCode)
	}
	if got := stored(t, v, "/target.bin"); !bytes.Equal(got, source) {
		t.Error("a refused MOVE changed the destination")
	}
}

// A DELETE still deletes: only the library's clearing of a COPY or MOVE
// destination is turned into a set-aside.
func TestDeleteIsStillADelete(t *testing.T) {
	v := newTestVault(t)
	srv := newTestServer(t, v)

	if resp := do(t, srv, "PUT", "/dav/gone.bin", bytes.NewReader(payload(1000)), nil); resp.StatusCode > 299 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	if resp := do(t, srv, "DELETE", "/dav/gone.bin", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	if got := names(t, v, "/"); len(got) != 0 {
		t.Errorf("the folder holds %v after the delete, want nothing", got)
	}
}

// ---------------------------------------------------------------------------
// In-flight requests
// ---------------------------------------------------------------------------

// A request still running counts for as long as it runs, not only for the
// moment it began — that is what keeps a long upload from meeting the idle
// auto-lock partway.
func TestInFlightCountsARunningRequest(t *testing.T) {
	v := newTestVault(t)
	h, err := Handler(v, Options{Prefix: "/dav"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/dav/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if h.InFlight() != 0 {
		t.Fatalf("InFlight = %d before any request", h.InFlight())
	}

	// A PUT whose body is held open: the request is running for as long as
	// the writer is.
	pr, pw := io.Pipe()
	req, err := http.NewRequest("PUT", srv.URL+"/dav/slow.bin", pr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.SetBasicAuth("anyone", testPassword)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Errorf("PUT: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	if _, err := pw.Write(payload(1000)); err != nil {
		t.Fatalf("writing the body: %v", err)
	}
	waitFor(t, "the request to be counted", func() bool { return h.InFlight() == 1 })

	pw.Close()
	resp := <-done
	if resp == nil {
		t.FailNow()
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("PUT = %d, want 201", resp.StatusCode)
	}
	waitFor(t, "the request to be uncounted", func() bool { return h.InFlight() == 0 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
