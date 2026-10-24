package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// filejumpStub is enough of FileJump's API to drive the backend on either
// generation of server: a root holding folders, folders holding files
// addressed by ID, the older servers' signed-URL upload, and the newer one's
// tus sessions.
type filejumpStub struct {
	t  *testing.T
	mu sync.Mutex

	nextID  int
	folders map[string]string // name -> id
	files   map[string]filejumpStubFile

	// pending holds bytes PUT to a signed URL, by the object-store key the
	// presign handed out, until an entries call claims them.
	pending map[string][]byte
	// sessions holds open tus uploads by their key.
	sessions map[string]*filejumpStubSession

	hardDeletes  int
	trashed      int
	patches      int
	conflicts    int
	sessionsMade int

	// failures is how many times a path answers with nginx's 500 page
	// before working; ghost makes the request that creates a file, or a
	// tus PATCH, do its work and then answer with the 500 anyway, as a
	// proxy that lost the reply would.
	failures map[string]int
	ghost    bool
	requests map[string]int

	// bareEntries answers an entries call with the entry itself rather
	// than wrapped in fileEntry, as the newer server does.
	bareEntries bool
	// pagesByCount makes listings say how many pages there are instead of
	// where the next one is, as the older servers do.
	pagesByCount bool
	// refuseDelete, when set, is the 422 body every delete answers with
	// instead of deleting.
	refuseDelete string
	// stuck makes every tus PATCH a conflict, whatever its offset.
	stuck bool
}

type filejumpStubFile struct {
	name   string
	parent string
	body   []byte
}

type filejumpStubSession struct {
	size   int
	meta   map[string]string
	body   []byte
	cancel bool
}

func newFileJumpStub(t *testing.T) *filejumpStub {
	return &filejumpStub{
		t:        t,
		nextID:   100,
		folders:  map[string]string{},
		files:    map[string]filejumpStubFile{},
		pending:  map[string][]byte{},
		sessions: map[string]*filejumpStubSession{},
		failures: map[string]int{},
		requests: map[string]int{},
	}
}

func (d *filejumpStub) stumble(w http.ResponseWriter, path string) bool {
	d.requests[path]++
	if d.failures[path] <= 0 {
		return false
	}
	d.failures[path]--
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(nginx500))
	return true
}

func (d *filejumpStub) mint() string {
	d.nextID++
	return strconv.Itoa(d.nextID)
}

func (d *filejumpStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Signed-URL uploads go to the object store, with no token.
	if strings.HasPrefix(r.URL.Path, "/store/") {
		if r.Header.Get("Authorization") != "" {
			d.t.Errorf("a bearer token was sent to the object store")
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("x-amz-acl") != "private" {
			d.t.Errorf("the signed PUT carried ACL %q, want the one the presign named", r.Header.Get("x-amz-acl"))
		}
		body, _ := io.ReadAll(r.Body)
		d.pending[strings.TrimPrefix(r.URL.Path, "/store/")] = body
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	ghosted := d.ghost && d.failures[r.URL.Path] > 0
	if ghosted && r.URL.Path == "/api/v1/s3/entries" {
		d.serveEntries(httptest.NewRecorder(), r)
		d.stumble(w, r.URL.Path)
		return
	}
	if ghosted && r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/tus/upload/") {
		d.serveSession(httptest.NewRecorder(), r)
		d.stumble(w, r.URL.Path)
		return
	}
	if d.stumble(w, r.URL.Path) {
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/api/v1/user/space-usage":
		w.Write([]byte(`{"used":250,"available":1000,"status":"success"}`))
	case r.URL.Path == "/api/v1/drive/file-entries":
		d.serveListing(w, r)
	case r.URL.Path == "/api/v1/folders" && r.Method == http.MethodPost:
		d.serveCreateFolder(w, r)
	case r.URL.Path == "/api/v1/s3/simple/presign" && r.Method == http.MethodPost:
		d.servePresign(w, r)
	case r.URL.Path == "/api/v1/s3/entries" && r.Method == http.MethodPost:
		d.serveEntries(w, r)
	case r.URL.Path == "/api/v1/tus/upload" && r.Method == http.MethodPost:
		d.serveOpenSession(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/tus/upload/"):
		d.serveSession(w, r)
	case r.URL.Path == "/api/v1/tus/entries" && r.Method == http.MethodPost:
		d.serveTUSEntries(w, r)
	case r.URL.Path == "/api/v1/file-entries/delete" && r.Method == http.MethodPost:
		d.serveDelete(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/file-entries/download/"):
		d.serveDownloadByHash(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/file-entries/") && strings.HasSuffix(r.URL.Path, "/download"):
		d.serveDownload(w, r)
	default:
		d.t.Errorf("unexpected FileJump request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *filejumpStub) item(id string, f filejumpStubFile) string {
	return fmt.Sprintf(`{"id":%s,"name":%q,"type":"text","file_size":%d,"hash":%q}`,
		id, f.name, len(f.body), filejumpHash(id))
}

func (d *filejumpStub) serveListing(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage, _ := strconv.Atoi(q.Get("perPage"))
	page, _ := strconv.Atoi(q.Get("page"))
	if perPage <= 0 || page <= 0 {
		d.t.Errorf("listing without paging: %s", r.URL.RawQuery)
	}
	folderID := q.Get("folderId")

	var entries []string
	if folderID == "" {
		for name, id := range d.folders {
			entries = append(entries, fmt.Sprintf(`{"id":%s,"name":%q,"type":"folder","hash":%q}`, id, name, filejumpHash(id)))
		}
	}
	for id, f := range d.files {
		if f.parent == folderID {
			entries = append(entries, d.item(id, f))
		}
	}
	sort.Strings(entries)

	// Tiny pages, so the backend has to follow them to see everything.
	const pageSize = 2
	lastPage := max((len(entries)+pageSize-1)/pageSize, 1)
	start := min((page-1)*pageSize, len(entries))
	end := min(start+pageSize, len(entries))
	data := strings.Join(entries[start:end], ",")
	if d.pagesByCount {
		w.Write([]byte(fmt.Sprintf(`{"current_page":%d,"last_page":%d,"data":[%s]}`, page, lastPage, data)))
		return
	}
	next := "null"
	if page < lastPage {
		next = strconv.Itoa(page + 1)
	}
	w.Write([]byte(fmt.Sprintf(`{"current_page":%d,"next_page":%s,"data":[%s]}`, page, next, data)))
}

func (d *filejumpStub) serveCreateFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if len(body.Name) < 3 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"The name must be at least 3 characters."}`))
		return
	}
	if _, exists := d.folders[body.Name]; exists {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"The name has already been taken."}`))
		return
	}
	id := d.mint()
	d.folders[body.Name] = id
	w.Write([]byte(fmt.Sprintf(`{"status":"success","folder":{"id":%s,"name":%q,"type":"folder"}}`, id, body.Name)))
}

func (d *filejumpStub) servePresign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename    string      `json:"filename"`
		Size        int         `json:"size"`
		Disk        string      `json:"disk"`
		WorkspaceID *int        `json:"workspaceId"`
		ParentID    json.Number `json:"parentId"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Disk != "uploads" || body.WorkspaceID == nil || body.Filename == "" {
		d.t.Errorf("presign body is missing what the API requires: %+v", body)
	}
	key := "uploads/" + d.mint() + ".part"
	w.Write([]byte(fmt.Sprintf(`{"url":"http://%s/store/%s","key":%q,"acl":"private","status":"success"}`,
		r.Host, key, key)))
}

func (d *filejumpStub) fileJSON(id string, f filejumpStubFile) string {
	if d.bareEntries {
		return d.item(id, f)
	}
	return fmt.Sprintf(`{"status":"success","fileEntry":%s}`, d.item(id, f))
}

func (d *filejumpStub) serveEntries(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename   string `json:"filename"`
		ClientName string `json:"clientName"`
		ParentID   any    `json:"parentId"`
		Size       int    `json:"size"`
		Disk       string `json:"disk"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		d.t.Errorf("entries body: %v: %s", err, raw)
	}
	// parentId is "" for the root and a number otherwise.
	parent := ""
	if n, ok := body.ParentID.(float64); ok {
		parent = strconv.Itoa(int(n))
	} else if str, ok := body.ParentID.(string); ok {
		parent = str
	}
	// The object store keeps what was PUT whatever happens here, so a
	// second entries call for the same key makes a second file — which is
	// what a retry after a lost answer does on the real service.
	var found []byte
	for key, pending := range d.pending {
		if path.Base(key) == body.Filename {
			found = pending
			break
		}
	}
	if found == nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"nothing was uploaded under that key"}`))
		return
	}
	if len(found) != body.Size {
		d.t.Errorf("entry declared %d bytes, %d landed", body.Size, len(found))
	}
	id := d.mint()
	f := filejumpStubFile{name: body.ClientName, parent: parent, body: found}
	d.files[id] = f
	w.Write([]byte(d.fileJSON(id, f)))
}

func (d *filejumpStub) serveOpenSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Tus-Resumable") != "1.0.0" {
		d.t.Errorf("tus create without the protocol version")
	}
	size, err := strconv.Atoi(r.Header.Get("Upload-Length"))
	if err != nil {
		d.t.Errorf("tus create without a length: %q", r.Header.Get("Upload-Length"))
	}
	meta := map[string]string{}
	for _, pair := range strings.Split(r.Header.Get("Upload-Metadata"), ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(pair), " ")
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			d.t.Errorf("tus metadata %q is not base64: %v", pair, err)
		}
		meta[key] = string(decoded)
	}
	for _, want := range []string{"name", "clientName", "clientSize", "workspaceId"} {
		if _, ok := meta[want]; !ok {
			d.t.Errorf("tus metadata lacks %s: %v", want, meta)
		}
	}
	// Sessions are numbered on their own, so a test can name the path of
	// the one an upload is about to open.
	d.sessionsMade++
	key := "sess-" + strconv.Itoa(d.sessionsMade)
	d.sessions[key] = &filejumpStubSession{size: size, meta: meta}
	// Relative, as a server behind a proxy may well answer.
	w.Header().Set("Location", "/api/v1/tus/upload/"+key)
	w.WriteHeader(http.StatusCreated)
}

func (d *filejumpStub) serveSession(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/v1/tus/upload/")
	session, ok := d.sessions[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodHead:
		w.Header().Set("Upload-Offset", strconv.Itoa(len(session.body)))
		w.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		d.patches++
		if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
			d.t.Errorf("tus PATCH with content type %q", r.Header.Get("Content-Type"))
		}
		offset, _ := strconv.Atoi(r.Header.Get("Upload-Offset"))
		if d.stuck || offset != len(session.body) {
			d.conflicts++
			w.WriteHeader(http.StatusConflict)
			return
		}
		chunk, _ := io.ReadAll(r.Body)
		if len(chunk) > filejumpChunkSize {
			d.t.Errorf("a %d-byte PATCH, over the %d-byte chunk", len(chunk), filejumpChunkSize)
		}
		session.body = append(session.body, chunk...)
		w.Header().Set("Upload-Offset", strconv.Itoa(len(session.body)))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		session.cancel = true
		delete(d.sessions, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (d *filejumpStub) serveTUSEntries(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UploadKey string `json:"uploadKey"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	session, ok := d.sessions[body.UploadKey]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(session.body) != session.size {
		d.t.Errorf("session finished with %d of %d bytes", len(session.body), session.size)
	}
	delete(d.sessions, body.UploadKey)
	id := d.mint()
	f := filejumpStubFile{name: session.meta["name"], parent: session.meta["parentId"], body: session.body}
	d.files[id] = f
	w.Write([]byte(d.fileJSON(id, f)))
}

func (d *filejumpStub) serveDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EntryIDs      []json.Number `json:"entryIds"`
		DeleteForever bool          `json:"deleteForever"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if d.refuseDelete != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(d.refuseDelete))
		return
	}
	for _, id := range body.EntryIDs {
		if _, ok := d.files[id.String()]; !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"The selected entryIds.0 is invalid.","errors":{"entryIds.0":["The selected entryIds.0 is invalid."]}}`))
			return
		}
		delete(d.files, id.String())
		if body.DeleteForever {
			d.hardDeletes++
		} else {
			d.trashed++
		}
	}
	w.Write([]byte(`{"status":"success"}`))
}

func (d *filejumpStub) serveDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/file-entries/"), "/download")
	f, ok := d.files[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(f.body)
}

func (d *filejumpStub) serveDownloadByHash(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("workspaceId") == "" {
		d.t.Errorf("a download by hash without the workspace: %s", r.URL.String())
	}
	hash := strings.TrimPrefix(r.URL.Path, "/api/v1/file-entries/download/")
	for id, f := range d.files {
		if filejumpHash(id) == hash {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(f.body)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

// newTestFileJump builds a backend against the stub, as an account on the
// given server — which decides the upload protocol, since every server
// name is pointed at the same stub.
func newTestFileJump(t *testing.T, server string) (*filejumpProvider, *filejumpStub) {
	stub := newFileJumpStub(t)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	restoreDelay := retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = restoreDelay })

	restore := filejumpAPI
	filejumpAPI = func(string) string { return srv.URL + "/api/v1" }
	t.Cleanup(func() { filejumpAPI = restore })

	p, err := New(Config{
		Kind: KindFileJump,
		Name: "filejump",
		Options: map[string]string{
			"access_token": "test-token",
			"server":       server,
			"folder":       "sand",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*filejumpProvider), stub
}

// roundTrip is the whole contract against one server generation.
func filejumpRoundTrip(t *testing.T, p *filejumpProvider, stub *filejumpStub) {
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "abc-c0000000-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id, ok := stub.folders["sand"]; !ok || id == "" {
		t.Fatal("the shard folder was not created at the root")
	}

	got, err := p.Get(ctx, "abc-c0000000-p1.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	// Overwriting has to leave one file under the name, not two.
	if err := p.Put(ctx, "abc-c0000000-p1.sand", []byte("second")); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if got, _ := p.Get(ctx, "abc-c0000000-p1.sand"); string(got) != "second" {
		t.Errorf("Get after overwrite = %q, want the new bytes", got)
	}
	if len(stub.files) != 1 {
		t.Errorf("an overwrite left %d files in the folder, want 1", len(stub.files))
	}

	info, err := p.Stat(ctx, "abc-c0000000-p1.sand")
	if err != nil || info.Size != int64(len("second")) {
		t.Errorf("Stat = %+v, %v", info, err)
	}

	// Enough files that the listing spans several pages.
	for _, key := range []string{"abc-c0000000-p2.sand", "abc-c0000000-p3.sand", "def-c0000000-p1.sand", "manifest.sand"} {
		if err := p.Put(ctx, key, []byte(key)); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	objects, err := p.List(ctx, "abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	sort.Strings(keys)
	want := []string{"abc-c0000000-p1.sand", "abc-c0000000-p2.sand", "abc-c0000000-p3.sand"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("List(abc) = %v, want %v", keys, want)
	}
	if all, _ := p.List(ctx, ""); len(all) != 5 {
		t.Errorf("List of everything found %d objects, want 5", len(all))
	}

	usage, err := p.Usage(ctx)
	if err != nil || usage.Total != 1000 || usage.Used != 250 {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	if err := p.Delete(ctx, "abc-c0000000-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "abc-c0000000-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if _, err := p.Stat(ctx, "abc-c0000000-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if err := p.Delete(ctx, "abc-c0000000-p1.sand"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if stub.trashed != 0 {
		t.Errorf("%d deletes went to the trash; every one should be permanent", stub.trashed)
	}
	if stub.hardDeletes == 0 {
		t.Error("nothing was erased for good")
	}
	if len(stub.sessions) != 0 {
		t.Errorf("%d tus sessions left open", len(stub.sessions))
	}
}

func TestFileJumpRoundTripOnTheOlderServers(t *testing.T) {
	p, stub := newTestFileJump(t, "drive.filejump.com")
	stub.pagesByCount = true
	if p.tus {
		t.Fatal("drive.filejump.com takes signed uploads, not tus")
	}
	filejumpRoundTrip(t, p, stub)
	if stub.patches != 0 {
		t.Errorf("%d tus PATCHes against a server that takes signed uploads", stub.patches)
	}
}

func TestFileJumpRoundTripOnTheNewerServer(t *testing.T) {
	p, stub := newTestFileJump(t, "eu.filejump.com")
	stub.bareEntries = true
	if !p.tus {
		t.Fatal("eu.filejump.com takes tus uploads")
	}
	filejumpRoundTrip(t, p, stub)
	if stub.patches == 0 {
		t.Error("nothing went up over tus")
	}
	if stub.requests["/api/v1/s3/simple/presign"] != 0 {
		t.Error("a signed upload was asked of the server that does not do them")
	}
}

// A real part spans several tus chunks, and arrives whole.
func TestFileJumpLargeShardGoesUpInChunks(t *testing.T) {
	p, stub := newTestFileJump(t, "eu.filejump.com")
	ctx := context.Background()

	payload := make([]byte, 2*filejumpChunkSize+4321)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	if err := p.Put(ctx, "big-c0000001-p2.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stub.patches != 3 {
		t.Errorf("%d PATCHes for a part of %d bytes, want 3", stub.patches, len(payload))
	}
	if len(stub.sessions) != 0 {
		t.Errorf("%d tus sessions left open after a successful Put", len(stub.sessions))
	}
	got, err := p.Get(ctx, "big-c0000001-p2.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("a chunked upload came back as %d bytes, want %d", len(got), len(payload))
	}
}

// A PATCH whose answer was lost has landed all the same, so the retry is out
// of step with the server. The upload asks where the server got to and goes
// on from there, and the file still arrives whole and once.
func TestFileJumpResumesATUSUploadAfterALostAnswer(t *testing.T) {
	p, stub := newTestFileJump(t, "eu.filejump.com")
	ctx := context.Background()
	stub.ghost = true

	payload := make([]byte, filejumpChunkSize+100)
	for i := range payload {
		payload[i] = byte(i)
	}
	stub.failures["/api/v1/tus/upload/sess-1"] = 1
	if err := p.Put(ctx, "res-c0000000-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stub.conflicts != 1 {
		t.Errorf("the retry was refused %d times, want once", stub.conflicts)
	}
	if got, err := p.Get(ctx, "res-c0000000-p1.sand"); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("Get after a resumed upload: %d bytes, %v", len(got), err)
	}
	if len(stub.files) != 1 {
		t.Errorf("%d files after a resumed upload, want 1", len(stub.files))
	}
}

// A server that refuses the offset it reported itself is not resumed from
// forever: the upload fails, saying where, and the session is cancelled.
func TestFileJumpTUSUploadStuckAtAnOffsetFails(t *testing.T) {
	p, stub := newTestFileJump(t, "eu.filejump.com")
	stub.stuck = true
	err := p.Put(context.Background(), "stuck-c0000000-p1.sand", make([]byte, 10))
	if err == nil || !strings.Contains(err.Error(), "offset 0") {
		t.Fatalf("Put against a stuck server = %v, want a failure naming the offset", err)
	}
	if stub.conflicts != 2 {
		t.Errorf("the offset was tried %d times, want twice", stub.conflicts)
	}
	if len(stub.sessions) != 0 {
		t.Errorf("%d sessions left open after a stuck upload", len(stub.sessions))
	}
}

// A failure part-way through a tus upload cancels the session rather than
// leaving it half full on the server.
func TestFileJumpFailedTUSUploadIsCancelled(t *testing.T) {
	p, stub := newTestFileJump(t, "eu.filejump.com")
	// Every try the PATCH gets fails; the cancel that follows gets through.
	stub.failures["/api/v1/tus/upload/sess-1"] = retryAttempts
	err := p.Put(context.Background(), "bad-c0000000-p1.sand", make([]byte, 10))
	if err == nil {
		t.Fatal("Put succeeded against a session that never takes a byte")
	}
	if len(stub.sessions) != 0 {
		t.Errorf("%d sessions left open after a failed upload", len(stub.sessions))
	}
	if len(stub.files) != 0 {
		t.Errorf("%d files made by a failed upload", len(stub.files))
	}
}

// A 500 can arrive after the entries call actually made the file, and
// FileJump keeps both copies under one name. The retry must not leave two.
func TestFileJumpRetriedEntriesCallLeavesOneFile(t *testing.T) {
	p, stub := newTestFileJump(t, "drive.filejump.com")
	ctx := context.Background()
	stub.ghost = true
	stub.failures["/api/v1/s3/entries"] = 1
	if err := p.Put(ctx, "abc-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(stub.files) != 1 {
		t.Errorf("%d files under one name after a ghosted entries call, want 1", len(stub.files))
	}
	if got, err := p.Get(ctx, "abc-c0000000-p1.sand"); err != nil || string(got) != "x" {
		t.Errorf("Get = %q, %v", got, err)
	}
}

func TestFileJumpRetriesATransientServerError(t *testing.T) {
	p, stub := newTestFileJump(t, "drive.filejump.com")
	ctx := context.Background()
	stub.failures["/api/v1/s3/simple/presign"] = 2
	if err := p.Put(ctx, "abc-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put across two 500s: %v", err)
	}
	if stub.requests["/api/v1/s3/simple/presign"] != 3 {
		t.Errorf("presign was asked %d times, want 3", stub.requests["/api/v1/s3/simple/presign"])
	}

	stub.failures["/api/v1/user/space-usage"] = 100
	err := p.Ping(ctx)
	if err == nil {
		t.Fatal("Ping succeeded against a proxy that never recovers")
	}
	if strings.Contains(err.Error(), "<html>") || !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Errorf("the error should carry the status line and no markup: %v", err)
	}
}

func TestFileJumpDeleteOfAVanishedFileIsQuiet(t *testing.T) {
	p, stub := newTestFileJump(t, "eu.filejump.com")
	ctx := context.Background()

	if err := p.Put(ctx, "gone-c0000000-p3.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for id := range stub.files {
		delete(stub.files, id)
	}
	if err := p.Delete(ctx, "gone-c0000000-p3.sand"); err != nil {
		t.Errorf("Delete of a file removed elsewhere = %v, want nil", err)
	}
	if _, err := p.Get(ctx, "gone-c0000000-p3.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}

func TestFileJumpDeleteRefusedForAnotherReasonIsAnError(t *testing.T) {
	p, stub := newTestFileJump(t, "drive.filejump.com")
	ctx := context.Background()

	if err := p.Put(ctx, "kept-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stub.refuseDelete = `{"message":"The selected workspace id is invalid.","errors":{"workspaceId":["The selected workspace id is invalid."]}}`
	err := p.Delete(ctx, "kept-c0000000-p1.sand")
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Errorf("Delete refused by validation = %v, want an error that says why", err)
	}
	if len(stub.files) != 1 {
		t.Errorf("the stub holds %d files, want the one it refused to delete", len(stub.files))
	}
}

func TestFileJumpWorkspaceRidesOnEveryRequest(t *testing.T) {
	seen := map[string]bool{}
	var mu sync.Mutex
	inner := newFileJumpStub(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") && !strings.HasPrefix(r.URL.Path, "/api/v1/tus/upload") {
			mu.Lock()
			seen[r.URL.Path] = r.URL.Query().Get("workspaceId") == "7"
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	restore := filejumpAPI
	filejumpAPI = func(string) string { return server.URL + "/api/v1" }
	t.Cleanup(func() { filejumpAPI = restore })

	p, err := New(Config{Kind: KindFileJump, Options: map[string]string{
		"access_token": "test-token", "server": "eu.filejump.com", "workspace_id": "7",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.Put(ctx, "ws-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := p.Get(ctx, "ws-c0000000-p1.sand"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := p.Delete(ctx, "ws-c0000000-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.(UsageReporter).Usage(ctx); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for path, carried := range seen {
		if !carried {
			t.Errorf("%s was asked without the workspace", path)
		}
	}
	if len(seen) < 4 {
		t.Errorf("only %d endpoints were exercised: %v", len(seen), seen)
	}
}

func TestFileJumpBadTokenIsAPingFailure(t *testing.T) {
	p, _ := newTestFileJump(t, "eu.filejump.com")
	p.token = "wrong"
	if err := p.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Ping with a bad token = %v, want a 401", err)
	}
}

func TestFileJumpRootFolderNeedsNoFolder(t *testing.T) {
	for _, server := range []string{"drive.filejump.com", "eu.filejump.com"} {
		p, stub := newTestFileJump(t, server)
		p.folderName = ""
		ctx := context.Background()
		if err := p.Put(ctx, "root-c0000000-p1.sand", []byte("at the root")); err != nil {
			t.Fatalf("%s: Put: %v", server, err)
		}
		if len(stub.folders) != 0 {
			t.Errorf("%s: a folder was created for an account configured to use the root: %v", server, stub.folders)
		}
		for _, f := range stub.files {
			if f.parent != "" {
				t.Errorf("%s: the file landed under parent %q, want the root", server, f.parent)
			}
		}
		if got, err := p.Get(ctx, "root-c0000000-p1.sand"); err != nil || string(got) != "at the root" {
			t.Errorf("%s: Get = %q, %v", server, got, err)
		}
	}
}

// The server field asks for what the address bar shows, and takes it
// however it was copied.
func TestFileJumpServerIsReadFromWhateverWasPasted(t *testing.T) {
	cases := []struct {
		in, want string
		tus      bool
	}{
		{"eu.filejump.com", "eu.filejump.com", true},
		{"https://eu.filejump.com/drive", "eu.filejump.com", true},
		{"  EU.FileJump.com/ ", "eu.filejump.com", true},
		{"drive.filejump.com", "drive.filejump.com", false},
		{"https://app.filejump.com", "app.filejump.com", false},
	}
	for _, c := range cases {
		p, err := New(Config{Kind: KindFileJump, Options: map[string]string{"access_token": "t", "server": c.in}})
		if err != nil {
			t.Errorf("server %q refused: %v", c.in, err)
			continue
		}
		fj := p.(*filejumpProvider)
		if fj.server != c.want || fj.tus != c.tus {
			t.Errorf("server %q = %q tus=%v, want %q tus=%v", c.in, fj.server, fj.tus, c.want, c.tus)
		}
	}
	for _, bad := range []string{"https://", "not a host"} {
		if _, err := New(Config{Kind: KindFileJump, Options: map[string]string{"access_token": "t", "server": bad}}); err == nil {
			t.Errorf("server %q was accepted", bad)
		}
	}
	if _, err := New(Config{Kind: KindFileJump, Options: map[string]string{"access_token": "t", "workspace_id": "team"}}); err == nil {
		t.Error("a workspace ID that is not a number was accepted")
	}
	// Leaving the server blank means the newest one.
	p, err := New(Config{Kind: KindFileJump, Options: map[string]string{"access_token": "t"}})
	if err != nil || p.(*filejumpProvider).server != "eu.filejump.com" {
		t.Errorf("default server = %+v, %v", p, err)
	}
}

func TestFileJumpHashIsTheIDWithItsSuffix(t *testing.T) {
	// base64("123|pad") is MTIzfHBhZA==, and the route takes it unpadded.
	if got := filejumpHash("123"); got != "MTIzfHBhZA" {
		t.Errorf("filejumpHash(123) = %q", got)
	}
}

func TestFileJumpCreatedEntryIsReadWrappedOrBare(t *testing.T) {
	var wrapped, bare filejumpCreated
	json.Unmarshal([]byte(`{"status":"success","fileEntry":{"id":5,"name":"a.sand","file_size":3}}`), &wrapped)
	json.Unmarshal([]byte(`{"id":9,"name":"b.sand","file_size":4,"hash":"h"}`), &bare)
	if e := wrapped.entry(); e.ID.String() != "5" || e.Name != "a.sand" {
		t.Errorf("wrapped entry = %+v", e)
	}
	if e := bare.entry(); e.ID.String() != "9" || e.Hash != "h" {
		t.Errorf("bare entry = %+v", e)
	}
}

func TestFileJumpSpecIsAPastedToken(t *testing.T) {
	spec, ok := SpecFor(KindFileJump)
	if !ok {
		t.Fatal("filejump is not registered")
	}
	if spec.OAuth != nil || spec.SignInLink != nil {
		t.Error("FileJump is connected with a token, not a sign-in")
	}
	var token *FieldSpec
	for i := range spec.Fields {
		if spec.Fields[i].Key == "access_token" {
			token = &spec.Fields[i]
		}
	}
	if token == nil || !token.Secret || !token.Required {
		t.Errorf("access_token should be a required secret, got %+v", token)
	}
	if len(spec.Presets) != 3 {
		t.Errorf("the three servers should each be a preset, got %d", len(spec.Presets))
	}
	for _, preset := range spec.Presets {
		if preset.Values["server"] == "" {
			t.Errorf("preset %s names no server", preset.Key)
		}
	}
	if _, err := New(Config{Kind: KindFileJump, Options: map[string]string{}}); err == nil {
		t.Error("an account with no token should be refused")
	}
	redacted := Config{Kind: KindFileJump, Options: map[string]string{"access_token": "abc"}}.Redacted()
	if redacted.Options["access_token"] != RedactedSecret {
		t.Error("the token reached the API layer unredacted")
	}
}

// The four services the WebDAV backend grew presets for are reachable by
// nothing else SAND speaks, which is what a preset is for.
func TestWebDAVPresetsCoverTheLifetimeClouds(t *testing.T) {
	spec, _ := SpecFor(KindWebDAV)
	presets := map[string]Preset{}
	for _, preset := range spec.Presets {
		presets[preset.Key] = preset
	}
	covered := map[string]bool{}
	for _, service := range spec.Covers {
		covered[service.Name] = true
	}
	for key, name := range map[string]string{
		"filejump": "FileJump", "folderfort": "FolderFort", "lifetimecloud": "LifetimeCloud", "scramble": "Scramble Cloud",
	} {
		preset, ok := presets[key]
		if !ok {
			t.Errorf("no %s preset", key)
			continue
		}
		if !strings.Contains(preset.Values["url"], "://") {
			t.Errorf("%s preset fills in %q, not a URL", key, preset.Values["url"])
		}
		if preset.Help == "" {
			t.Errorf("%s preset does not say where the credentials come from", key)
		}
		if !covered[name] {
			t.Errorf("the catalogue does not name %s", name)
		}
	}
	// Scramble's WebDAV is the desktop app on this machine, not a cloud.
	if !strings.HasPrefix(presets["scramble"].Values["url"], "http://127.0.0.1:") {
		t.Errorf("Scramble preset points at %q; its server is local", presets["scramble"].Values["url"])
	}
}
