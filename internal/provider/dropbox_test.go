package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// dropboxStub is enough of Dropbox's API to drive the backend: path-addressed
// files on a case-insensitive filesystem, the paged folder listing, and the
// 409-with-a-body shape every endpoint-specific refusal arrives in.
type dropboxStub struct {
	t *testing.T

	// files by lower-cased path, the way Dropbox keys them.
	files map[string][]byte

	// folderDisplay is the casing the account shows the prefix folder with —
	// "Sand" on an account where somebody made the folder by hand before SAND
	// was pointed at it. Empty means the folder is shown as it was asked for.
	folderDisplay string

	// pageSize is how many entries a listing answers with before has_more.
	pageSize int

	// refuse, when set, is the body every files/* call answers with as a 409 —
	// what a rate limiter or a lock says, which is a 409 all the same.
	refuse string
}

func newDropboxStub(t *testing.T) *dropboxStub {
	return &dropboxStub{t: t, files: map[string][]byte{}, pageSize: 1000}
}

// install points the backend at the stub, both hosts, for the rest of the test.
func (d *dropboxStub) install(t *testing.T) {
	server := httptest.NewServer(d)
	restoreRPC, restoreContent := dropboxRPC, dropboxContent
	dropboxRPC, dropboxContent = server.URL+"/2", server.URL+"/2"
	t.Cleanup(func() {
		dropboxRPC, dropboxContent = restoreRPC, restoreContent
		server.Close()
	})
}

func (d *dropboxStub) conflict(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	w.Write([]byte(body))
}

const (
	dropboxPathNotFound   = `{"error_summary":"path/not_found/...","error":{".tag":"path","path":{".tag":"not_found"}}}`
	dropboxLookupNotFound = `{"error_summary":"path_lookup/not_found/...","error":{".tag":"path_lookup","path_lookup":{".tag":"not_found"}}}`
	dropboxTooManyWrites  = `{"error_summary":"too_many_write_operations/...","error":{".tag":"too_many_write_operations"}}`
)

// display is the path as the account shows it: the folder in its own casing,
// the file as it was written.
func (d *dropboxStub) display(lower string) string {
	if d.folderDisplay == "" {
		return lower
	}
	segments := strings.SplitN(strings.TrimPrefix(lower, "/"), "/", 2)
	if len(segments) == 2 && strings.EqualFold(segments[0], d.folderDisplay) {
		return "/" + d.folderDisplay + "/" + segments[1]
	}
	return lower
}

func (d *dropboxStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/2/files/") && d.refuse != "" {
		d.conflict(w, d.refuse)
		return
	}

	var arg struct {
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
	}
	if header := r.Header.Get("Dropbox-API-Arg"); header != "" {
		json.Unmarshal([]byte(header), &arg)
	} else {
		json.NewDecoder(r.Body).Decode(&arg)
	}
	lower := strings.ToLower(arg.Path)

	switch r.URL.Path {
	case "/2/users/get_current_account":
		w.Write([]byte(`{"email":"alice@example.test","name":{"display_name":"Alice"}}`))
	case "/2/users/get_space_usage":
		w.Write([]byte(`{"used":250,"allocation":{"allocated":1000}}`))
	case "/2/files/upload":
		body, _ := io.ReadAll(r.Body)
		d.files[lower] = body
		w.Write([]byte(fmt.Sprintf(`{"name":%q,"size":%d}`, arg.Path, len(body))))
	case "/2/files/download":
		body, ok := d.files[lower]
		if !ok {
			d.conflict(w, dropboxPathNotFound)
			return
		}
		w.Write(body)
	case "/2/files/get_metadata":
		body, ok := d.files[lower]
		if !ok {
			d.conflict(w, dropboxPathNotFound)
			return
		}
		w.Write([]byte(fmt.Sprintf(`{".tag":"file","size":%d,"path_display":%q,"path_lower":%q}`,
			len(body), d.display(lower), lower)))
	case "/2/files/delete_v2":
		if _, ok := d.files[lower]; !ok {
			d.conflict(w, dropboxLookupNotFound)
			return
		}
		delete(d.files, lower)
		w.Write([]byte(`{"metadata":{".tag":"file"}}`))
	case "/2/files/list_folder":
		d.serveList(w, lower, 0)
	case "/2/files/list_folder/continue":
		var folder string
		var from int
		fmt.Sscanf(arg.Cursor, "%d %s", &from, &folder)
		d.serveList(w, folder, from)
	default:
		d.t.Errorf("dropbox stub: unexpected %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveList answers one page of a recursive listing of folder, pageSize
// entries from offset from, in the order Dropbox has them.
func (d *dropboxStub) serveList(w http.ResponseWriter, folder string, from int) {
	base := folder + "/"
	var paths []string
	for lower := range d.files {
		if strings.HasPrefix(lower, base) {
			paths = append(paths, lower)
		}
	}
	if folder != "" && len(paths) == 0 {
		d.conflict(w, dropboxPathNotFound)
		return
	}
	sort.Strings(paths)

	end := from + d.pageSize
	if end > len(paths) {
		end = len(paths)
	}
	var entries []string
	for _, lower := range paths[from:end] {
		entries = append(entries, fmt.Sprintf(`{".tag":"file","path_display":%q,"path_lower":%q,"size":%d}`,
			d.display(lower), lower, len(d.files[lower])))
	}
	w.Write([]byte(fmt.Sprintf(`{"entries":[%s],"cursor":"%d %s","has_more":%v}`,
		strings.Join(entries, ","), end, folder, end < len(paths))))
}

func newTestDropbox(t *testing.T, prefix string) *dropboxProvider {
	t.Helper()
	p, err := New(Config{Kind: KindDropbox, Name: "dropbox", Options: map[string]string{
		"access_token": "test-token",
		"prefix":       prefix,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*dropboxProvider)
}

func TestDropboxRoundTrip(t *testing.T) {
	stub := newDropboxStub(t)
	stub.install(t)
	stub.pageSize = 2
	p := newTestDropbox(t, "sand")
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if account, err := p.Account(ctx); err != nil || account != "alice@example.test" {
		t.Errorf("Account = %q, %v", account, err)
	}

	// Nothing written yet: the folder is not there, which is an empty
	// account rather than a failure.
	if listed, err := p.List(ctx, ""); err != nil || len(listed) != 0 {
		t.Errorf("List before anything is written = %+v, %v", listed, err)
	}

	payload := []byte("encrypted shard bytes")
	keys := []string{"abc-p1.sand", "abc-p2.sand", "abc-p3.sand", "def-p1.sand", "def-p2.sand"}
	for _, key := range keys {
		if err := p.Put(ctx, key, payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	if _, ok := stub.files["/sand/abc-p1.sand"]; !ok {
		t.Errorf("stored under unexpected paths: %v", keysOf(stub.files))
	}

	got, err := p.Get(ctx, "abc-p1.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}
	info, err := p.Stat(ctx, "abc-p1.sand")
	if err != nil || info.Size != int64(len(payload)) {
		t.Errorf("Stat = %+v, %v", info, err)
	}

	// A listing that takes more than one page comes back whole.
	listed, err := p.List(ctx, "abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listedKeys []string
	for _, o := range listed {
		listedKeys = append(listedKeys, o.Key)
	}
	if strings.Join(listedKeys, ",") != "abc-p1.sand,abc-p2.sand,abc-p3.sand" {
		t.Errorf("List(abc) = %v", listedKeys)
	}

	usage, err := p.Usage(ctx)
	if err != nil || usage.Used != 250 || usage.Total != 1000 {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "abc-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if _, err := p.Stat(ctx, "abc-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat after Delete = %v, want ErrNotFound", err)
	}
	// Deleting what is already gone is not an error.
	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Dropbox answers every endpoint-specific refusal as a 409, and only the one
// whose body says path/not_found is a missing object. A rate limiter's 409
// read as "gone" would have Delete report success over a file still there,
// and the health check rebuild parts that were never lost.
func TestDropboxOnlyAMissingPathIsNotFound(t *testing.T) {
	stub := newDropboxStub(t)
	stub.install(t)
	p := newTestDropbox(t, "sand")
	ctx := context.Background()

	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stub.refuse = dropboxTooManyWrites

	if err := p.Delete(ctx, "abc-p1.sand"); err == nil {
		t.Error("Delete refused by the rate limiter reported success")
	} else if !strings.Contains(err.Error(), "too_many_write_operations") {
		t.Errorf("Delete error does not say why: %v", err)
	}
	if _, ok := stub.files["/sand/abc-p1.sand"]; !ok {
		t.Fatal("the stub lost the file it was refusing to delete")
	}

	if _, err := p.Stat(ctx, "abc-p1.sand"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Stat refused by the rate limiter = %v, want an error that is not ErrNotFound", err)
	}
	if _, err := p.Get(ctx, "abc-p1.sand"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get refused by the rate limiter = %v, want an error that is not ErrNotFound", err)
	}
	// A listing is complete or it is an error — never silently empty.
	if listed, err := p.List(ctx, ""); err == nil {
		t.Errorf("List refused by the rate limiter = %+v, nil; want an error", listed)
	}
}

func TestDropboxErrorTags(t *testing.T) {
	cases := []struct {
		body     string
		tags     string
		notFound bool
	}{
		{dropboxPathNotFound, "path/not_found", true},
		{dropboxLookupNotFound, "path_lookup/not_found", true},
		{dropboxTooManyWrites, "too_many_write_operations", false},
		{`{"error_summary":"path/restricted_content/..","error":{".tag":"path","path":{".tag":"restricted_content"}}}`, "path/restricted_content", false},
		{`{"error_summary":"path/not_file/..","error":{".tag":"path","path":{".tag":"not_file"}}}`, "path/not_file", true},
		// The summary alone, for an error object the parser cannot follow.
		{`{"error_summary":"path/not_found/..","error":"?"}`, "path/not_found", true},
		{`not json at all`, "", false},
		{``, "", false},
	}
	for _, c := range cases {
		if got := strings.Join(dropboxErrorTags([]byte(c.body)), "/"); got != c.tags {
			t.Errorf("dropboxErrorTags(%s) = %q, want %q", c.body, got, c.tags)
		}
		if got := dropboxNotFound([]byte(c.body)); got != c.notFound {
			t.Errorf("dropboxNotFound(%s) = %v, want %v", c.body, got, c.notFound)
		}
	}
}

// Dropbox keeps the casing a folder was made with, so an account where the
// folder was created as "Sand" lists every path as "/Sand/…" against a prefix
// of "sand". Trimming the configured spelling off would leave every key whole
// and the account would list as empty, with no error to say so.
func TestDropboxListsAFolderMadeWithOtherCasing(t *testing.T) {
	stub := newDropboxStub(t)
	stub.install(t)
	stub.folderDisplay = "Sand"
	p := newTestDropbox(t, "sand")
	ctx := context.Background()

	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	listed, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "abc-p1.sand" || listed[0].Size != 4 {
		t.Errorf("List = %+v, want the one part under its key", listed)
	}
}

// The empty key is the prefix folder, and delete_v2 of the folder takes every
// part with it. It is refused before any request is made.
func TestDropboxRefusesKeysThatNameTheFolder(t *testing.T) {
	stub := newDropboxStub(t)
	stub.install(t)
	p := newTestDropbox(t, "sand")
	ctx := context.Background()

	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, key := range []string{"", ".", "..", "../abc-p1.sand", "sub/abc-p1.sand"} {
		if err := p.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) was allowed", key)
		}
		if _, err := p.Get(ctx, key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want a refusal that is not ErrNotFound", key, err)
		}
		if _, err := p.Stat(ctx, key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want a refusal that is not ErrNotFound", key, err)
		}
		if err := p.Put(ctx, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) was allowed", key)
		}
	}
	if _, ok := stub.files["/sand/abc-p1.sand"]; !ok {
		t.Error("a refused key still reached Dropbox and took the folder with it")
	}
}
