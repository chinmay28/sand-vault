package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// drimeStub is enough of Drime's API to drive the backend: a root holding
// folders, folders holding files addressed by ID, one-request uploads, and the
// S3-style multipart flow for anything bigger.
type drimeStub struct {
	t  *testing.T
	mu sync.Mutex

	nextID  int
	folders map[string]string // name -> id
	files   map[string]drimeStubFile

	// Multipart uploads in flight: upload id -> parts received so far.
	uploads map[string]map[int][]byte

	hardDeletes int
	trashed     int
}

type drimeStubFile struct {
	name   string
	parent string
	body   []byte
}

func newDrimeStub(t *testing.T) *drimeStub {
	return &drimeStub{
		t:       t,
		nextID:  100,
		folders: map[string]string{},
		files:   map[string]drimeStubFile{},
		uploads: map[string]map[int][]byte{},
	}
}

func (d *drimeStub) mint() string {
	d.nextID++
	return strconv.Itoa(d.nextID)
}

func (d *drimeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Part uploads go to a signed URL on the object store, with no token.
	if strings.HasPrefix(r.URL.Path, "/store/") {
		if r.Header.Get("Authorization") != "" {
			d.t.Errorf("a bearer token was sent to the object store")
		}
		d.servePart(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
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
	case r.URL.Path == "/api/v1/uploads" && r.Method == http.MethodPost:
		d.serveUpload(w, r)
	case r.URL.Path == "/api/v1/file-entries/delete" && r.Method == http.MethodPost:
		d.serveDelete(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/file-entries/") && strings.HasSuffix(r.URL.Path, "/download"):
		d.serveDownload(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/s3/"):
		d.serveMultipart(w, r)
	default:
		d.t.Errorf("unexpected Drime request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *drimeStub) item(id string, f drimeStubFile) string {
	parent := "null"
	if f.parent != "" {
		parent = f.parent
	}
	return fmt.Sprintf(`{"id":%s,"name":%q,"type":"text","file_size":%d,"parent_id":%s,"url":"api/v1/file-entries/%s/download"}`,
		id, f.name, len(f.body), parent, id)
}

func (d *drimeStub) serveListing(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage, _ := strconv.Atoi(q.Get("perPage"))
	page, _ := strconv.Atoi(q.Get("page"))
	if perPage <= 0 || page <= 0 {
		d.t.Errorf("listing without paging: %s", r.URL.RawQuery)
	}
	folderID := q.Get("folderId")

	// The root holds the folders and any file put there directly; a folder
	// holds its files.
	var entries []string
	if folderID == "" {
		for name, id := range d.folders {
			entries = append(entries, fmt.Sprintf(`{"id":%s,"name":%q,"type":"folder"}`, id, name))
		}
	}
	for id, f := range d.files {
		if f.parent == folderID {
			entries = append(entries, d.item(id, f))
		}
	}
	sort.Strings(entries)

	// Tiny pages, so the backend has to follow the page numbers to see
	// everything.
	const pageSize = 2
	lastPage := (len(entries) + pageSize - 1) / pageSize
	if lastPage == 0 {
		lastPage = 1
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > len(entries) {
		start = len(entries)
	}
	if end > len(entries) {
		end = len(entries)
	}
	w.Write([]byte(fmt.Sprintf(`{"current_page":%d,"last_page":%d,"data":[%s]}`,
		page, lastPage, strings.Join(entries[start:end], ","))))
}

func (d *drimeStub) serveCreateFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if _, exists := d.folders[body.Name]; exists {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"The name has already been taken."}`))
		return
	}
	id := d.mint()
	d.folders[body.Name] = id
	w.Write([]byte(fmt.Sprintf(`{"status":"success","folder":{"id":%s,"name":%q,"type":"folder"}}`, id, body.Name)))
}

func (d *drimeStub) serveUpload(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		d.t.Fatalf("upload content type: %v", err)
	}
	reader := multipart.NewReader(r.Body, params["boundary"])

	f := drimeStubFile{}
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(part)
		switch part.FormName() {
		case "parentId":
			f.parent = string(body)
		case "relativePath":
			f.name = string(body)
		case "file":
			f.body = body
		}
	}
	if len(f.body) > drimeSimpleUploadLimit {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte(`{"message":"File is too large."}`))
		return
	}
	id := d.mint()
	d.files[id] = f
	// The upload answer has no download URL, as the real one does not.
	w.Write([]byte(fmt.Sprintf(`{"status":"success","fileEntry":{"id":%s,"name":%q,"type":"text","file_size":%d}}`,
		id, f.name, len(f.body))))
}

func (d *drimeStub) serveDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EntryIDs      []string `json:"entryIds"`
		DeleteForever bool     `json:"deleteForever"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	for _, id := range body.EntryIDs {
		if _, ok := d.files[id]; !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"The selected entry ids is invalid."}`))
			return
		}
		delete(d.files, id)
		if body.DeleteForever {
			d.hardDeletes++
		} else {
			d.trashed++
		}
	}
	w.Write([]byte(`{"status":"success"}`))
}

func (d *drimeStub) serveDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/file-entries/"), "/download")
	f, ok := d.files[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(f.body)
}

func (d *drimeStub) serveMultipart(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string { return fmt.Sprint(body[k]) }

	switch strings.TrimPrefix(r.URL.Path, "/api/v1/s3/") {
	case "multipart/create":
		id := "upload-" + d.mint()
		d.uploads[id] = map[int][]byte{}
		// The key's last segment names the upload, which is what the entries
		// request hands back as "filename".
		w.Write([]byte(fmt.Sprintf(`{"uploadId":%q,"key":"tmp/%s"}`, id, id)))

	case "multipart/batch-sign-part-urls":
		if _, ok := d.uploads[str("uploadId")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var urls []string
		for _, n := range body["partNumbers"].([]any) {
			urls = append(urls, fmt.Sprintf(`{"url":"http://%s/store/%s/%v","partNumber":%v}`,
				r.Host, str("uploadId"), n, n))
		}
		w.Write([]byte(`{"urls":[` + strings.Join(urls, ",") + `]}`))

	case "multipart/complete":
		parts, ok := d.uploads[str("uploadId")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for _, p := range body["parts"].([]any) {
			part := p.(map[string]any)
			n := int(part["PartNumber"].(float64))
			if want := fmt.Sprintf(`"etag-%d"`, n); part["ETag"] != want {
				d.t.Errorf("part %d completed with ETag %v, want %s", n, part["ETag"], want)
			}
			if _, received := parts[n]; !received {
				d.t.Errorf("part %d completed but never uploaded", n)
			}
		}
		w.Write([]byte(`{"location":"somewhere"}`))

	case "entries":
		uploadID := str("filename")
		parts := d.uploads[uploadID]
		if parts == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var numbers []int
		for n := range parts {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		var assembled []byte
		for _, n := range numbers {
			assembled = append(assembled, parts[n]...)
		}
		if int64(len(assembled)) != int64(body["size"].(float64)) {
			d.t.Errorf("entry declared %v bytes, parts add up to %d", body["size"], len(assembled))
		}
		delete(d.uploads, uploadID)
		id := d.mint()
		f := drimeStubFile{name: str("clientName"), parent: str("parentId"), body: assembled}
		d.files[id] = f
		w.Write([]byte(fmt.Sprintf(`{"fileEntry":{"id":%s,"name":%q,"type":"text","file_size":%d}}`,
			id, f.name, len(f.body))))

	case "multipart/abort":
		delete(d.uploads, str("uploadId"))
		w.Write([]byte(`{}`))

	default:
		d.t.Errorf("unexpected multipart request %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *drimeStub) servePart(w http.ResponseWriter, r *http.Request) {
	rest := strings.Split(strings.TrimPrefix(r.URL.Path, "/store/"), "/")
	if len(rest) != 2 || r.Method != http.MethodPut {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	parts, ok := d.uploads[rest[0]]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	n, _ := strconv.Atoi(rest[1])
	body, _ := io.ReadAll(r.Body)
	parts[n] = body
	w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	w.WriteHeader(http.StatusOK)
}

func newTestDrime(t *testing.T) (*drimeProvider, *drimeStub) {
	stub := newDrimeStub(t)
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	restore := drimeAPI
	drimeAPI = server.URL + "/api/v1"
	t.Cleanup(func() { drimeAPI = restore })

	p, err := New(Config{
		Kind: KindDrime,
		Name: "drime",
		Options: map[string]string{
			"access_token": "test-token",
			"folder":       "sand",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*drimeProvider), stub
}

func TestDrimeRoundTrip(t *testing.T) {
	p, stub := newTestDrime(t)
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
	// Deleting what is already gone is not an error.
	if err := p.Delete(ctx, "abc-c0000000-p1.sand"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if stub.trashed != 0 {
		t.Errorf("%d deletes went to the trash; every one should be permanent", stub.trashed)
	}
	if stub.hardDeletes == 0 {
		t.Error("nothing was erased for good")
	}
}

// TestDrimeLargeShardGoesUpInParts covers the path a real shard takes: past
// the one-request limit it has to go up through the multipart flow, and
// arrive whole.
func TestDrimeLargeShardGoesUpInParts(t *testing.T) {
	p, stub := newTestDrime(t)
	ctx := context.Background()

	payload := make([]byte, drimeSimpleUploadLimit+drimeChunkSize+4321)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	if err := p.Put(ctx, "big-c0000001-p2.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(stub.uploads) != 0 {
		t.Errorf("%d multipart uploads left open after a successful Put", len(stub.uploads))
	}

	got, err := p.Get(ctx, "big-c0000001-p2.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("a multipart upload came back as %d bytes, want %d", len(got), len(payload))
	}
	info, err := p.Stat(ctx, "big-c0000001-p2.sand")
	if err != nil || info.Size != int64(len(payload)) {
		t.Errorf("Stat = %+v, %v", info, err)
	}
}

// An ID Drime no longer knows comes back as a 422, and that is the answer a
// delete wants.
func TestDrimeDeleteOfAVanishedFileIsQuiet(t *testing.T) {
	p, stub := newTestDrime(t)
	ctx := context.Background()

	if err := p.Put(ctx, "gone-c0000000-p3.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Something else removes it behind SAND's back, so the cached ID is stale.
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

func TestDrimeBadTokenIsAPingFailure(t *testing.T) {
	p, _ := newTestDrime(t)
	p.token = "wrong"
	if err := p.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Ping with a bad token = %v, want a 401", err)
	}
}

func TestDrimeRootFolderNeedsNoFolder(t *testing.T) {
	p, stub := newTestDrime(t)
	p.folderName = ""
	ctx := context.Background()

	if err := p.Put(ctx, "root-c0000000-p1.sand", []byte("at the root")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(stub.folders) != 0 {
		t.Errorf("a folder was created for an account configured to use the root: %v", stub.folders)
	}
	for _, f := range stub.files {
		if f.parent != "" {
			t.Errorf("the file landed under parent %q, want the root", f.parent)
		}
	}
	if got, err := p.Get(ctx, "root-c0000000-p1.sand"); err != nil || string(got) != "at the root" {
		t.Errorf("Get = %q, %v", got, err)
	}
}

func TestDrimeWorkspaceRidesOnEveryRequest(t *testing.T) {
	seen := map[string]bool{}
	var mu sync.Mutex
	inner := newDrimeStub(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") && r.URL.Path != "/api/v1/uploads" &&
			!strings.HasPrefix(r.URL.Path, "/api/v1/s3/") && !strings.HasSuffix(r.URL.Path, "/download") {
			mu.Lock()
			seen[r.URL.Path] = r.URL.Query().Get("workspaceId") == "ws-7"
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	restore := drimeAPI
	drimeAPI = server.URL + "/api/v1"
	t.Cleanup(func() { drimeAPI = restore })

	p, err := New(Config{Kind: KindDrime, Options: map[string]string{
		"access_token": "test-token", "workspace_id": "ws-7",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.Put(ctx, "ws-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
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
	if len(seen) < 3 {
		t.Errorf("only %d endpoints were exercised: %v", len(seen), seen)
	}
}

func TestDrimeDownloadURLIsRelativeToTheSite(t *testing.T) {
	restore := drimeAPI
	drimeAPI = "https://app.drime.test/api/v1"
	defer func() { drimeAPI = restore }()

	got, err := drimeDownloadURL("api/v1/file-entries/abc/download")
	if err != nil || got != "https://app.drime.test/api/v1/file-entries/abc/download" {
		t.Errorf("relative = %q, %v", got, err)
	}
	got, err = drimeDownloadURL("https://cdn.drime.test/x")
	if err != nil || got != "https://cdn.drime.test/x" {
		t.Errorf("absolute = %q, %v", got, err)
	}
	if _, err := drimeDownloadURL(""); err == nil {
		t.Error("an empty address should be an error, not a request to the site root")
	}
}

func TestDrimeSpecIsAPastedToken(t *testing.T) {
	spec, ok := SpecFor(KindDrime)
	if !ok {
		t.Fatal("drime is not registered")
	}
	if spec.OAuth != nil || spec.SignInLink != nil {
		t.Error("Drime is connected with a token, not a sign-in")
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
	if _, err := New(Config{Kind: KindDrime, Options: map[string]string{}}); err == nil {
		t.Error("an account with no token should be refused")
	}
	redacted := Config{Kind: KindDrime, Options: map[string]string{"access_token": "abc"}}.Redacted()
	if redacted.Options["access_token"] != RedactedSecret {
		t.Error("the token reached the API layer unredacted")
	}
}
