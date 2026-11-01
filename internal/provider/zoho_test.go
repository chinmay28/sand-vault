package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// zohoStub is enough of Zoho WorkDrive's API to drive the backend: the chain
// of lookups that finds My Folders, folders and files by ID, cursor-paged
// listings, both uploads, the download host, and a trash that a plan may or
// may not let the API empty.
type zohoStub struct {
	t *testing.T

	mu      sync.Mutex
	nextID  int
	folders map[string]zohoStubFolder
	files   map[string]*zohoStubFile

	// pageSize is how many items a listing answers with before handing out
	// a cursor. Small values exercise the paging in the backend.
	pageSize int

	// purgeRefused makes emptying the trash a feature the plan lacks.
	purgeRefused bool

	// What the backend did, for the tests to check.
	foldersMade int
	purges      int
	streamed    int
}

type zohoStubFolder struct {
	name   string
	parent string
}

type zohoStubFile struct {
	name    string
	parent  string
	content []byte
	trashed bool
}

// The identities the My Folders lookup walks through.
const (
	zohoStubUser   = "zuid-1"
	zohoStubTeam   = "team-1"
	zohoStubMember = "member-1"
	zohoStubSpace  = "space-1"
)

func newZohoStub(t *testing.T) *zohoStub {
	return &zohoStub{
		t:        t,
		nextID:   100,
		folders:  map[string]zohoStubFolder{zohoStubSpace: {name: "My Folders"}},
		files:    map[string]*zohoStubFile{},
		pageSize: 1000,
	}
}

func (z *zohoStub) mint() string {
	z.nextID++
	return fmt.Sprint(z.nextID)
}

// r008 is how Zoho answers for a resource it no longer has.
func (z *zohoStub) r008(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"errors":[{"id":"R008","title":"Unauthorized access"}]}`))
}

func (z *zohoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Zoho-oauthtoken test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"id":"F0000","title":"Invalid OAuth token"}]}`))
		return
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/dl/v1/workdrive/download/"):
		z.serveDownload(w, strings.TrimPrefix(path, "/dl/v1/workdrive/download/"))
	case path == "/up/workdrive-api/v1/stream/upload":
		z.serveStreamUpload(w, r)
	case strings.HasPrefix(path, "/api/v1/"):
		if r.Header.Get("Accept") != "application/vnd.api+json" {
			z.t.Errorf("zoho stub: %s %s without the JSON:API Accept header", r.Method, path)
		}
		z.serveAPI(w, r, strings.TrimPrefix(path, "/api/v1/"))
	default:
		z.t.Errorf("unexpected Zoho request %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (z *zohoStub) serveAPI(w http.ResponseWriter, r *http.Request, route string) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	parts := strings.Split(route, "/")

	switch {
	case route == "users/me":
		fmt.Fprintf(w, `{"data":{"id":%q,"type":"users","attributes":{"email_id":"alice@example.test","display_name":"Alice"}}}`, zohoStubUser)
	case route == "users/"+zohoStubUser+"/teams":
		fmt.Fprintf(w, `{"data":[{"id":%q,"type":"teams","attributes":{"name":"Alice's Team"}}]}`, zohoStubTeam)
	case route == "teams/"+zohoStubTeam+"/currentuser":
		fmt.Fprintf(w, `{"data":{"id":%q,"type":"users"}}`, zohoStubMember)
	case route == "users/"+zohoStubMember+"/privatespace":
		fmt.Fprintf(w, `{"data":[{"id":%q,"type":"privatespace"}]}`, zohoStubSpace)

	case route == "upload" && r.Method == http.MethodPost:
		z.serveUpload(w, r)

	case route == "files" && r.Method == http.MethodPost:
		z.serveCreateFolder(w, r)
	case route == "files" && r.Method == http.MethodPatch:
		z.serveTrash(w, r)

	case len(parts) == 3 && parts[0] == "files" && parts[2] == "files" && r.Method == http.MethodGet:
		z.serveListing(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "files" && r.Method == http.MethodGet:
		z.serveItem(w, parts[1])
	case len(parts) == 2 && parts[0] == "files" && r.Method == http.MethodPatch:
		z.servePurge(w, r, parts[1])

	default:
		z.t.Errorf("unexpected Zoho API request %s %s", r.Method, route)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (z *zohoStub) fileJSON(id string, f *zohoStubFile) string {
	status := "1"
	if f.trashed {
		status = "51"
	}
	return fmt.Sprintf(`{"id":%q,"type":"files","attributes":{"name":%q,"is_folder":false,"status":%q,"storage_info":{"size_in_bytes":%d}}}`,
		id, f.name, status, len(f.content))
}

func (z *zohoStub) folderJSON(id string, f zohoStubFolder) string {
	size := 0
	for _, file := range z.files {
		if file.parent == id && !file.trashed {
			size += len(file.content)
		}
	}
	return fmt.Sprintf(`{"id":%q,"type":"files","attributes":{"name":%q,"is_folder":true,"status":"1","storage_info":{"size_in_bytes":%d,"files_count":0,"folders_count":0}}}`,
		id, f.name, size)
}

// serveListing pages a folder's children by cursor, as Zoho does: the
// response carries the URL of the next page, and the backend has to pull
// page[next] back out of it.
func (z *zohoStub) serveListing(w http.ResponseWriter, r *http.Request, folderID string) {
	if _, ok := z.folders[folderID]; !ok {
		z.r008(w)
		return
	}
	q := r.URL.Query()
	if q.Get("page[limit]") == "" {
		z.t.Errorf("zoho stub: a listing without page[limit]: %s", r.URL.RawQuery)
	}

	var entries []string
	ids := make([]string, 0, len(z.folders)+len(z.files))
	for id, f := range z.folders {
		if f.parent == folderID {
			ids = append(ids, id)
		}
	}
	for id, f := range z.files {
		if f.parent == folderID && !f.trashed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if f, ok := z.folders[id]; ok {
			entries = append(entries, z.folderJSON(id, f))
		} else {
			entries = append(entries, z.fileJSON(id, z.files[id]))
		}
	}

	from := 0
	if next := q.Get("page[next]"); next != "" && next != "0" {
		fmt.Sscanf(next, "c%d", &from)
	}
	end := from + z.pageSize
	if end > len(entries) {
		end = len(entries)
	}
	hasNext := end < len(entries)
	nextLink := ""
	if hasNext {
		nextLink = fmt.Sprintf("http://%s%s?page%%5Blimit%%5D=%d&page%%5Bnext%%5D=c%d", r.Host, r.URL.Path, z.pageSize, end)
	}
	fmt.Fprintf(w, `{"data":[%s],"links":{"cursor":{"has_next":%v,"next":%q}}}`,
		strings.Join(entries[from:end], ","), hasNext, nextLink)
}

func (z *zohoStub) serveItem(w http.ResponseWriter, id string) {
	if f, ok := z.files[id]; ok {
		fmt.Fprintf(w, `{"data":%s}`, z.fileJSON(id, f))
		return
	}
	if f, ok := z.folders[id]; ok {
		fmt.Fprintf(w, `{"data":%s}`, z.folderJSON(id, f))
		return
	}
	z.r008(w)
}

func (z *zohoStub) serveCreateFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data struct {
			Attributes struct {
				Name     string `json:"name"`
				ParentID string `json:"parent_id"`
			} `json:"attributes"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Data.Type != "files" {
		z.t.Errorf("zoho stub: malformed create folder: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, ok := z.folders[body.Data.Attributes.ParentID]; !ok {
		z.r008(w)
		return
	}
	id := z.mint()
	z.folders[id] = zohoStubFolder{name: body.Data.Attributes.Name, parent: body.Data.Attributes.ParentID}
	z.foldersMade++
	fmt.Fprintf(w, `{"data":%s}`, z.folderJSON(id, z.folders[id]))
}

// place stores uploaded bytes under a name in a folder, replacing a file of
// that name when asked to, as override-name-exist does.
func (z *zohoStub) place(parent, name string, content []byte, override bool) string {
	if override {
		for id, f := range z.files {
			if f.parent == parent && f.name == name && !f.trashed {
				f.content = content
				return id
			}
		}
	}
	id := z.mint()
	z.files[id] = &zohoStubFile{name: name, parent: parent, content: content}
	return id
}

func (z *zohoStub) serveUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parent := q.Get("parent_id")
	if _, ok := z.folders[parent]; !ok {
		z.r008(w)
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		z.t.Fatalf("upload content type: %v", err)
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var content []byte
	name := q.Get("filename")
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "content" {
			content, _ = io.ReadAll(part)
			if name == "" {
				name = part.FileName()
			}
		}
	}
	id := z.place(parent, name, content, q.Get("override-name-exist") == "true")
	info, _ := json.Marshal(map[string]any{"RESOURCE_ID": id, "size": len(content)})
	fmt.Fprintf(w, `{"data":[{"attributes":{"parent_id":%q,%q:%q,"resource_id":%q,"File INFO":%q},"type":"files"}]}`,
		parent, name, name, id, string(info))
}

func (z *zohoStub) serveStreamUpload(w http.ResponseWriter, r *http.Request) {
	parent := r.Header.Get("x-parent_id")
	if _, ok := z.folders[parent]; !ok {
		z.r008(w)
		return
	}
	if r.Header.Get("x-streammode") != "1" || r.Header.Get("upload-id") == "" {
		z.t.Errorf("zoho stub: a stream upload without its headers: %v", r.Header)
	}
	content, _ := io.ReadAll(r.Body)
	id := z.place(parent, r.Header.Get("x-filename"), content, r.Header.Get("x-override-name-exist") == "true")
	z.streamed++
	fmt.Fprintf(w, `{"data":[{"attributes":{"parent_id":%q,"file_name":%q,"resource_id":%q},"type":"files"}],"status":"success"}`,
		parent, r.Header.Get("x-filename"), id)
}

func (z *zohoStub) serveTrash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Status string `json:"status"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Data) == 0 {
		z.t.Errorf("zoho stub: malformed trash request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, record := range body.Data {
		f, ok := z.files[record.ID]
		if !ok || f.trashed {
			z.r008(w)
			return
		}
		if record.Attributes.Status != "51" {
			z.t.Errorf("zoho stub: PATCH /files with status %q, want 51", record.Attributes.Status)
		}
		f.trashed = true
	}
	w.Write([]byte(`{"data":[]}`))
}

func (z *zohoStub) servePurge(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Data struct {
			Attributes struct {
				Status string `json:"status"`
			} `json:"attributes"`
		} `json:"data"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Data.Attributes.Status != "61" {
		z.t.Errorf("zoho stub: PATCH /files/%s with status %q, want 61", id, body.Data.Attributes.Status)
	}
	z.purges++
	if z.purgeRefused {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":[{"id":"F6016","title":"Permanent delete is not available in this plan"}]}`))
		return
	}
	f, ok := z.files[id]
	if !ok || !f.trashed {
		z.r008(w)
		return
	}
	delete(z.files, id)
	w.Write([]byte(`{"data":{}}`))
}

func (z *zohoStub) serveDownload(w http.ResponseWriter, id string) {
	f, ok := z.files[id]
	if !ok || f.trashed {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(f.content)
}

// counts reports what the backend has done so far, read under the lock the
// handlers write it under.
func (z *zohoStub) counts() (foldersMade, purges, streamed int) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.foldersMade, z.purges, z.streamed
}

// liveFiles is what the account holds outside the trash, by name.
func (z *zohoStub) liveFiles() map[string][]byte {
	z.mu.Lock()
	defer z.mu.Unlock()
	out := map[string][]byte{}
	for _, f := range z.files {
		if !f.trashed {
			out[f.name] = f.content
		}
	}
	return out
}

// pointAtStub aims the backend's hosts at the stub for the rest of the test.
func pointAtStub(t *testing.T, stub *httptest.Server) {
	restoreAPI, restoreDownload, restoreUpload := zohoAPIURL, zohoDownloadURL, zohoUploadURL
	zohoAPIURL = stub.URL + "/api/v1"
	zohoDownloadURL = stub.URL + "/dl/v1/workdrive"
	zohoUploadURL = stub.URL + "/up/workdrive-api/v1"
	t.Cleanup(func() { zohoAPIURL, zohoDownloadURL, zohoUploadURL = restoreAPI, restoreDownload, restoreUpload })
}

func newTestZoho(t *testing.T, stub *httptest.Server) *zohoProvider {
	pointAtStub(t, stub)
	p, err := newZohoProvider(Config{Kind: KindZoho, Name: "zoho", Options: map[string]string{
		"region": "com", "folder": "sand",
	}})
	if err != nil {
		t.Fatalf("newZohoProvider: %v", err)
	}
	z := p.(*zohoProvider)
	z.tokens = &tokenSource{staticToken: "test-token"}
	return z
}

func TestZohoRoundTrip(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if account, err := p.Account(ctx); err != nil || account != "alice@example.test" {
		t.Errorf("Account = %q, %v", account, err)
	}

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "abc123-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The folder was made under My Folders, once, and the part is in it.
	if made, _, _ := stub.counts(); made != 1 {
		t.Errorf("folders made = %d, want 1", made)
	}
	if got := stub.liveFiles(); string(got["abc123-p1.sand"]) != string(payload) {
		t.Errorf("stored files = %v", got)
	}

	got, err := p.Get(ctx, "abc123-p1.sand")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	info, err := p.Stat(ctx, "abc123-p1.sand")
	if err != nil || info.Size != int64(len(payload)) {
		t.Errorf("Stat = %+v, %v", info, err)
	}
	if _, err := p.Stat(ctx, "abc123-p9.sand"); err != ErrNotFound {
		t.Errorf("Stat of a missing part = %v, want ErrNotFound", err)
	}
	if _, err := p.Get(ctx, "abc123-p9.sand"); err != ErrNotFound {
		t.Errorf("Get of a missing part = %v, want ErrNotFound", err)
	}

	if err := p.Put(ctx, "def456-p2.sand", []byte("another")); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	list, err := p.List(ctx, "abc123-")
	if err != nil || len(list) != 1 || list[0].Key != "abc123-p1.sand" || list[0].Size != int64(len(payload)) {
		t.Errorf("List(abc123/) = %+v, %v", list, err)
	}
	all, err := p.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Errorf("List() = %+v, %v", all, err)
	}

	usage, err := p.MeasureUsage(ctx)
	if err != nil || !usage.Measured || usage.Used != int64(len(payload)+len("another")) {
		t.Errorf("MeasureUsage = %+v, %v", usage, err)
	}

	if err := p.Delete(ctx, "abc123-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Trashed and then erased, since this plan allows it.
	if _, purges, _ := stub.counts(); purges != 1 {
		t.Errorf("purges = %d, want 1", purges)
	}
	stub.mu.Lock()
	for id, f := range stub.files {
		if f.name == "abc123-p1.sand" {
			t.Errorf("the deleted part is still in the account as %s (trashed=%v)", id, f.trashed)
		}
	}
	stub.mu.Unlock()
	if files := stub.liveFiles(); len(files) != 1 {
		t.Errorf("live files after the delete = %v", files)
	}
	if _, err := p.Get(ctx, "abc123-p1.sand"); err != ErrNotFound {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := p.Delete(ctx, "abc123-p1.sand"); err != nil {
		t.Errorf("Delete of a deleted part = %v, want nil", err)
	}
}

func TestZohoListingWalksEveryPageByCursor(t *testing.T) {
	stub := newZohoStub(t)
	stub.pageSize = 2
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := p.Put(ctx, fmt.Sprintf("file%d-p1.sand", i), []byte("x")); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	list, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 5 {
		t.Errorf("List returned %d parts, want 5: %+v", len(list), list)
	}

	// A fresh backend has no cache, so a lookup has to page through the
	// folder to find a part on the last page.
	fresh := newTestZoho(t, server)
	if _, err := fresh.Stat(ctx, "file4-p1.sand"); err != nil {
		t.Errorf("Stat through a paged listing: %v", err)
	}
}

func TestZohoWritingAPartAgainKeepsOneFile(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	if err := p.Put(ctx, "abc-p1.sand", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, "abc-p1.sand", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if files := stub.liveFiles(); len(files) != 1 || string(files["abc-p1.sand"]) != "second" {
		t.Errorf("stored files = %v, want one holding the second write", files)
	}
	got, err := p.Get(ctx, "abc-p1.sand")
	if err != nil || string(got) != "second" {
		t.Errorf("Get = %q, %v", got, err)
	}
}

func TestZohoLargePartGoesThroughTheStreamUpload(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	restore := zohoSimpleUploadLimit
	zohoSimpleUploadLimit = 16
	defer func() { zohoSimpleUploadLimit = restore }()

	big := []byte(strings.Repeat("Z", 100))
	if err := p.Put(ctx, "big-p1.sand", big); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, _, streamed := stub.counts(); streamed != 1 {
		t.Errorf("stream uploads = %d, want 1", streamed)
	}
	if err := p.Put(ctx, "small-p1.sand", []byte("tiny")); err != nil {
		t.Fatalf("Put small: %v", err)
	}
	if _, _, streamed := stub.counts(); streamed != 1 {
		t.Errorf("a part under the limit went through the stream upload")
	}
	got, err := p.Get(ctx, "big-p1.sand")
	if err != nil || string(got) != string(big) {
		t.Errorf("Get = %d bytes, %v", len(got), err)
	}
	// Writing it again replaces, through the stream upload too.
	if err := p.Put(ctx, "big-p1.sand", big); err != nil {
		t.Fatal(err)
	}
	if files := stub.liveFiles(); len(files) != 2 {
		t.Errorf("stored files = %d, want 2", len(files))
	}
}

func TestZohoDeleteStillSucceedsWhereThePlanKeepsTheTrash(t *testing.T) {
	stub := newZohoStub(t)
	stub.purgeRefused = true
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	for _, key := range []string{"a-p1.sand", "b-p1.sand", "c-p1.sand"} {
		if err := p.Put(ctx, key, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"a-p1.sand", "b-p1.sand"} {
		if err := p.Delete(ctx, key); err != nil {
			t.Errorf("Delete(%s) = %v, want nil: the part is out of the folder", key, err)
		}
	}
	// The refusal was taken as an answer the first time.
	if _, purges, _ := stub.counts(); purges != 1 {
		t.Errorf("purge attempts = %d, want 1", purges)
	}
	// The parts are in the trash, and the trash is not the folder.
	if files := stub.liveFiles(); len(files) != 1 {
		t.Errorf("live files = %v, want only c/p1", files)
	}
	if _, err := p.Stat(ctx, "a-p1.sand"); err != ErrNotFound {
		t.Errorf("Stat of a trashed part = %v, want ErrNotFound", err)
	}
	list, err := p.List(ctx, "")
	if err != nil || len(list) != 1 || list[0].Key != "c-p1.sand" {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestZohoStatNoticesAPartTrashedElsewhere(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	if err := p.Put(ctx, "abc-p1.sand", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Another client trashes it; the cache still has the ID.
	stub.mu.Lock()
	for _, f := range stub.files {
		f.trashed = true
	}
	stub.mu.Unlock()

	if _, err := p.Stat(ctx, "abc-p1.sand"); err != ErrNotFound {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
	if _, err := p.Get(ctx, "abc-p1.sand"); err != ErrNotFound {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}

func TestZohoFolderGoneIsAnErrorNotAnEmptyAccount(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	if err := p.Put(ctx, "abc-p1.sand", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Someone removes the SAND folder itself.
	stub.mu.Lock()
	for id, f := range stub.folders {
		if f.name == "sand" {
			delete(stub.folders, id)
		}
	}
	stub.files = map[string]*zohoStubFile{}
	stub.mu.Unlock()

	// A listing of a folder that is not there must not read as a folder
	// with nothing in it: the orphan scan and recovery both act on an
	// empty listing.
	if _, err := p.List(ctx, ""); err == nil {
		t.Fatal("List of a vanished folder succeeded")
	}
	// But the next write makes the folder again.
	if err := p.Put(ctx, "def-p1.sand", []byte("y")); err != nil {
		t.Fatalf("Put after the folder vanished: %v", err)
	}
	if made, _, _ := stub.counts(); made != 2 {
		t.Errorf("folders made = %d, want 2", made)
	}
}

func TestZohoParallelFirstWritesMakeOneFolder(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- p.Put(ctx, fmt.Sprintf("f%d-p1.sand", i), []byte("x"))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Put: %v", err)
		}
	}
	if made, _, _ := stub.counts(); made != 1 {
		t.Errorf("folders made = %d, want 1", made)
	}
}

func TestZohoRefusesKeysThatNameTheFolder(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	for _, key := range []string{"", ".", "..", "a/b/../c"} {
		if err := p.Put(ctx, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) succeeded", key)
		}
		if _, err := p.Get(ctx, key); err == nil || err == ErrNotFound {
			t.Errorf("Get(%q) = %v, want a refusal", key, err)
		}
		if err := p.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) succeeded", key)
		}
	}
	if len(stub.liveFiles()) != 0 {
		t.Errorf("a refused key reached the account: %v", stub.liveFiles())
	}
}

func TestZohoParentFolderCanBeNamed(t *testing.T) {
	stub := newZohoStub(t)
	stub.folders["team-folder-1"] = zohoStubFolder{name: "Shared"}
	server := httptest.NewServer(stub)
	defer server.Close()
	pointAtStub(t, server)

	built, err := newZohoProvider(Config{Kind: KindZoho, Options: map[string]string{
		"region": "com", "folder": "sand", "parent_id": "team-folder-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := built.(*zohoProvider)
	p.tokens = &tokenSource{staticToken: "test-token"}

	if err := p.Put(context.Background(), "abc-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for id, f := range stub.folders {
		if f.name == "sand" && f.parent != "team-folder-1" {
			t.Errorf("the sand folder %s was made under %s, not the named parent", id, f.parent)
		}
	}
}

func TestZohoRegionNamesEveryHost(t *testing.T) {
	built, err := newZohoProvider(Config{Kind: KindZoho, Options: map[string]string{"region": "eu"}})
	if err != nil {
		t.Fatal(err)
	}
	p := built.(*zohoProvider)
	for name, got := range map[string]string{
		"api":      p.api,
		"download": p.download,
		"upload":   p.upload,
		"token":    p.tokens.tokenURL,
	} {
		if !strings.Contains(got, ".zoho.eu/") || strings.Contains(got, "{region}") {
			t.Errorf("%s host for region eu = %q", name, got)
		}
	}

	// The region is spliced into hostnames, so it is only ever the tail of
	// a domain.
	for _, bad := range []string{"", "evil.example/", "com:8080", "eu..", ".eu", "EU"} {
		if _, err := newZohoProvider(Config{Kind: KindZoho, Options: map[string]string{"region": bad}}); err == nil {
			t.Errorf("region %q was accepted", bad)
		}
	}
	// New fills the default in for an account connected without one.
	if _, err := New(Config{Kind: KindZoho, Options: map[string]string{
		"client_id": "c", "client_secret": "s", "refresh_token": "r",
	}}); err != nil {
		t.Errorf("a config without a region should take the default: %v", err)
	}
}

func TestZohoSpecSignsInByRegion(t *testing.T) {
	spec, ok := SpecFor(KindZoho)
	if !ok || spec.OAuth == nil {
		t.Fatal("Zoho WorkDrive should be connectable by signing in")
	}
	o := spec.OAuth
	if o.RegionField != "region" || len(o.Regions) == 0 || o.DefaultRegion() != "com" {
		t.Errorf("regions = %+v, default %q", o.Regions, o.DefaultRegion())
	}
	if !strings.Contains(o.AuthURL, regionPlaceholder) || !strings.Contains(o.TokenURL, regionPlaceholder) {
		t.Errorf("the sign-in URLs do not carry the region: %s %s", o.AuthURL, o.TokenURL)
	}
	eu, err := o.ForRegion("eu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(eu.AuthURL, "https://accounts.zoho.eu/") || !strings.HasPrefix(eu.TokenURL, "https://accounts.zoho.eu/") {
		t.Errorf("ForRegion(eu) = %s %s", eu.AuthURL, eu.TokenURL)
	}
	options := eu.Options("id", "secret", Tokens{RefreshToken: "rt"})
	if options["region"] != "eu" {
		t.Errorf("the sign-in does not store the region it was made in: %v", options)
	}
	// Every field the sign-in offers has to exist, because the region is a
	// hostname.
	for _, r := range o.Regions {
		if err := validateZohoRegion(r.Key); err != nil {
			t.Errorf("offered region %q: %v", r.Key, err)
		}
	}
	if o.AuthParams["access_type"] != "offline" {
		t.Error("without access_type=offline Zoho issues no refresh token")
	}
}

// Zoho states an hour on its access tokens. A token it stops honouring before
// then — revoked, or shorter-lived than stated — must cost one refresh, not
// every request until the hour is up; and its not-found, which is also a 401,
// must not be mistaken for a bad token.
func TestZohoRefreshesATokenTheServiceStopsHonouring(t *testing.T) {
	stub := newZohoStub(t)
	server := httptest.NewServer(stub)
	defer server.Close()
	p := newTestZoho(t, server)
	ctx := context.Background()

	var (
		mu     sync.Mutex
		issued int
	)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "rt" {
			t.Errorf("token request = %v", r.PostForm)
		}
		mu.Lock()
		issued++
		n := issued
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"access_token":"stale","expires_in":3600,"token_type":"Bearer"}`)
		} else {
			fmt.Fprint(w, `{"access_token":"test-token","expires_in":3600,"token_type":"Bearer"}`)
		}
	}))
	defer tokens.Close()
	p.tokens = &tokenSource{tokenURL: tokens.URL, clientID: "c", clientSecret: "s", refreshToken: "rt"}

	// The stub only ever honours "test-token", so the first token is dead on
	// arrival — as one Zoho has revoked would be.
	if err := p.Put(ctx, "abc-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put on a token the service refuses: %v", err)
	}
	mu.Lock()
	n := issued
	mu.Unlock()
	if n != 2 {
		t.Errorf("tokens issued = %d, want the stale one and its replacement", n)
	}
	// The replacement is kept.
	if _, err := p.Get(ctx, "abc-p1.sand"); err != nil {
		t.Errorf("Get: %v", err)
	}
	mu.Lock()
	n = issued
	mu.Unlock()
	if n != 2 {
		t.Errorf("tokens issued after the retry = %d, want still 2", n)
	}

	// A 401 that is Zoho's not-found costs no refresh at all.
	if _, err := p.Stat(ctx, "never-p1.sand"); err != ErrNotFound {
		t.Errorf("Stat of a missing part = %v", err)
	}
	mu.Lock()
	n = issued
	mu.Unlock()
	if n != 2 {
		t.Errorf("a not-found was taken for a bad token: %d tokens issued", n)
	}
}
