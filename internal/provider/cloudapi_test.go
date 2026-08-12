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
	"strings"
	"testing"
)

// --- OneDrive ---------------------------------------------------------------

// graphStub is enough of Microsoft Graph's drive API to drive the backend:
// path-addressed items, upload sessions, and paged children.
type graphStub struct {
	t      *testing.T
	files  map[string][]byte
	chunks map[string][]byte // upload session URL -> bytes received so far
	target map[string]string // upload session URL -> item path
}

func newGraphStub(t *testing.T) *graphStub {
	return &graphStub{
		t:      t,
		files:  map[string][]byte{},
		chunks: map[string][]byte{},
		target: map[string]string{},
	}
}

func (g *graphStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" && !strings.HasPrefix(r.URL.Path, "/upload/") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		g.serveUploadSession(w, r)
	case r.URL.Path == "/me/drive":
		w.Write([]byte(`{"id":"drive-1","quota":{"total":1000,"used":250}}`))
	case r.URL.Path == "/me":
		w.Write([]byte(`{"userPrincipalName":"alice@example.test","mail":"alice@example.test"}`))
	default:
		g.serveItem(w, r)
	}
}

// serveItem handles /me/drive/root:/{path} and its :/content, :/children and
// :/createUploadSession actions.
func (g *graphStub) serveItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/me/drive/root")
	path = strings.TrimPrefix(path, ":")
	action := ""
	if idx := strings.LastIndex(path, ":/"); idx >= 0 {
		action = path[idx+2:]
		path = path[:idx]
	} else if strings.HasPrefix(path, "/") && !strings.HasPrefix(r.URL.Path, "/me/drive/root:") {
		// The bare-root form: /me/drive/root/children
		action = strings.TrimPrefix(path, "/")
		path = ""
	}
	path = strings.Trim(path, "/")

	switch action {
	case "content":
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			g.files[path] = body
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"name":"shard","size":0}`))
		case http.MethodGet:
			body, ok := g.files[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)
		}
	case "createUploadSession":
		session := fmt.Sprintf("/upload/%d", len(g.target)+1)
		g.target[session] = path
		w.Write([]byte(fmt.Sprintf(`{"uploadUrl":%q}`, "http://"+r.Host+session)))
	case "children":
		g.serveChildren(w, path)
	case "":
		if r.Method == http.MethodDelete {
			delete(g.files, path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, ok := g.files[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(fmt.Sprintf(`{"name":%q,"size":%d}`, path, len(body))))
	default:
		g.t.Errorf("unexpected Graph action %q on %q", action, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *graphStub) serveChildren(w http.ResponseWriter, path string) {
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}

	var entries []string
	for name, body := range g.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if strings.Contains(rest, "/") {
			continue // a child of a subfolder, reported when that folder is walked
		}
		entries = append(entries, fmt.Sprintf(`{"name":%q,"size":%d}`, rest, len(body)))
	}

	// Any intermediate folder under this path shows up as a folder entry.
	folders := map[string]bool{}
	for name := range g.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if idx := strings.Index(rest, "/"); idx > 0 {
			folders[rest[:idx]] = true
		}
	}
	for folder := range folders {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"folder":{"childCount":1}}`, folder))
	}

	w.Write([]byte(`{"value":[` + strings.Join(entries, ",") + `]}`))
}

func (g *graphStub) serveUploadSession(w http.ResponseWriter, r *http.Request) {
	path, ok := g.target[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodDelete {
		delete(g.chunks, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Header.Get("Content-Range") == "" {
		g.t.Error("an upload session chunk arrived without a Content-Range")
	}

	body, _ := io.ReadAll(r.Body)
	g.chunks[r.URL.Path] = append(g.chunks[r.URL.Path], body...)

	// Graph answers 202 until the last chunk, then 201 with the finished item.
	total := parseRangeTotal(g.t, r.Header.Get("Content-Range"))
	if len(g.chunks[r.URL.Path]) < total {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"nextExpectedRanges":["0-"]}`))
		return
	}
	g.files[path] = g.chunks[r.URL.Path]
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"name":"shard","size":0}`))
}

func parseRangeTotal(t *testing.T, header string) int {
	t.Helper()
	idx := strings.LastIndex(header, "/")
	if idx < 0 {
		t.Fatalf("malformed Content-Range %q", header)
	}
	var total int
	fmt.Sscanf(header[idx+1:], "%d", &total)
	return total
}

// newTestOneDrive builds the backend against a stub, skipping New so the test
// does not have to invent OAuth credentials.
func newTestOneDrive() *onedriveProvider {
	return &onedriveProvider{
		oauthBase: oauthBase{
			base:   base{cfg: Config{Kind: KindOneDrive, Name: "onedrive"}},
			tokens: &tokenSource{staticToken: "test-token"},
		},
		folder: "sand",
	}
}

func TestOneDriveRoundTrip(t *testing.T) {
	stub := httptest.NewServer(newGraphStub(t))
	defer stub.Close()

	restore := graphAPI
	graphAPI = stub.URL
	defer func() { graphAPI = restore }()

	p := newTestOneDrive()
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if account, err := p.Account(ctx); err != nil || account != "alice@example.test" {
		t.Errorf("Account = %q, %v", account, err)
	}

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "sand/abc/p1.media", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := p.Get(ctx, "sand/abc/p1.media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	info, err := p.Stat(ctx, "sand/abc/p1.media")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Stat size = %d, want %d", info.Size, len(payload))
	}

	objects, err := p.List(ctx, "sand/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "sand/abc/p1.media" {
		t.Errorf("List = %+v, want the one shard", objects)
	}

	usage, err := p.Usage(ctx)
	if err != nil || usage.Total != 1000 || usage.Used != 250 {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	if err := p.Delete(ctx, "sand/abc/p1.media"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "sand/abc/p1.media"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	// Deleting what is already gone is not an error.
	if err := p.Delete(ctx, "sand/abc/p1.media"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// TestOneDriveLargeShardUsesAnUploadSession covers the path a real shard takes:
// anything past a few megabytes has to go up in chunks, and arrive whole.
func TestOneDriveLargeShardUsesAnUploadSession(t *testing.T) {
	stub := httptest.NewServer(newGraphStub(t))
	defer stub.Close()

	restore := graphAPI
	graphAPI = stub.URL
	defer func() { graphAPI = restore }()

	p := newTestOneDrive()
	ctx := context.Background()

	payload := make([]byte, graphSimpleUploadLimit+graphChunkSize+1234)
	for i := range payload {
		payload[i] = byte(i)
	}

	if err := p.Put(ctx, "sand/big/p2.media", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := p.Get(ctx, "sand/big/p2.media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("a chunked upload came back as %d bytes, want %d", len(got), len(payload))
	}
}

func TestOneDriveItemPathsAreEscaped(t *testing.T) {
	p := newTestOneDrive()
	if got := p.itemPath("sand/abc/p1.media"); got != "sand/sand/abc/p1.media" {
		t.Errorf("itemPath = %q", got)
	}

	restore := graphAPI
	graphAPI = "https://graph.test/v1.0"
	defer func() { graphAPI = restore }()

	got := p.itemURL("folder name/p1.media", "content")
	want := "https://graph.test/v1.0/me/drive/root:/folder%20name/p1.media:/content"
	if got != want {
		t.Errorf("itemURL = %q, want %q", got, want)
	}
	if got := p.itemURL("", "children"); got != "https://graph.test/v1.0/me/drive/root/children" {
		t.Errorf("itemURL at the root = %q", got)
	}
}

// --- Box --------------------------------------------------------------------

// boxStub is enough of Box's API to drive the backend: a single folder of
// files addressed by ID, with uploads on a second host.
type boxStub struct {
	t       *testing.T
	nextID  int
	folders map[string]string // name -> id
	names   map[string]string // file id -> name
	files   map[string][]byte // file id -> content
}

func newBoxStub(t *testing.T) *boxStub {
	return &boxStub{
		t:       t,
		nextID:  100,
		folders: map[string]string{},
		names:   map[string]string{},
		files:   map[string][]byte{},
	}
}

func (b *boxStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/2.0/users/me":
		w.Write([]byte(`{"login":"alice@example.test","space_amount":1000,"space_used":250}`))

	case r.URL.Path == "/2.0/folders" && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if id, ok := b.folders[body.Name]; ok {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(fmt.Sprintf(`{"context_info":{"conflicts":[{"type":"folder","id":%q}]}}`, id)))
			return
		}
		id := b.mint()
		b.folders[body.Name] = id
		w.Write([]byte(fmt.Sprintf(`{"type":"folder","id":%q,"name":%q}`, id, body.Name)))

	case strings.HasSuffix(r.URL.Path, "/items"):
		b.serveItems(w, r)

	case strings.HasPrefix(r.URL.Path, "/api/2.0/files/") || r.URL.Path == "/api/2.0/files/content":
		b.serveUpload(w, r)

	case strings.HasPrefix(r.URL.Path, "/2.0/files/"):
		b.serveFile(w, r)

	default:
		b.t.Errorf("unexpected Box request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *boxStub) mint() string {
	b.nextID++
	return fmt.Sprint(b.nextID)
}

func (b *boxStub) serveItems(w http.ResponseWriter, r *http.Request) {
	folderID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/2.0/folders/"), "/items")

	var entries []string
	if folderID == boxRoot {
		for name, id := range b.folders {
			entries = append(entries, fmt.Sprintf(`{"type":"folder","id":%q,"name":%q}`, id, name))
		}
	} else {
		for id, name := range b.names {
			entries = append(entries, fmt.Sprintf(`{"type":"file","id":%q,"name":%q,"size":%d}`,
				id, name, len(b.files[id])))
		}
	}
	w.Write([]byte(fmt.Sprintf(`{"total_count":%d,"entries":[%s]}`,
		len(entries), strings.Join(entries, ","))))
}

func (b *boxStub) serveUpload(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		b.t.Fatalf("upload content type: %v", err)
	}
	reader := multipart.NewReader(r.Body, params["boundary"])

	var attributes struct {
		Name string `json:"name"`
	}
	var content []byte
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(part)
		switch part.FormName() {
		case "attributes":
			json.Unmarshal(body, &attributes)
		case "file":
			content = body
		}
	}

	// A new version of an existing file posts to /files/{id}/content.
	id := ""
	if trimmed := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/2.0/files/"), "/content"); trimmed != "content" {
		id = trimmed
	}
	if id == "" {
		id = b.mint()
	}
	b.names[id] = attributes.Name
	b.files[id] = content
	w.Write([]byte(fmt.Sprintf(`{"entries":[{"type":"file","id":%q,"name":%q}]}`, id, attributes.Name)))
}

func (b *boxStub) serveFile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/2.0/files/")
	id := strings.TrimSuffix(rest, "/content")

	if _, ok := b.files[id]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodDelete:
		delete(b.files, id)
		delete(b.names, id)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(rest, "/content"):
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b.files[id])
	default:
		w.Write([]byte(fmt.Sprintf(`{"type":"file","id":%q,"name":%q,"size":%d}`,
			id, b.names[id], len(b.files[id]))))
	}
}

func newTestBox() *boxProvider {
	return &boxProvider{
		oauthBase: oauthBase{
			base:   base{cfg: Config{Kind: KindBox, Name: "box"}},
			tokens: &tokenSource{staticToken: "test-token"},
		},
		folderName: "sand",
		idCache:    map[string]string{},
	}
}

func TestBoxRoundTrip(t *testing.T) {
	stub := httptest.NewServer(newBoxStub(t))
	defer stub.Close()

	restoreAPI, restoreUpload := boxAPI, boxUpload
	boxAPI, boxUpload = stub.URL+"/2.0", stub.URL+"/api/2.0"
	defer func() { boxAPI, boxUpload = restoreAPI, restoreUpload }()

	p := newTestBox()
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if account, err := p.Account(ctx); err != nil || account != "alice@example.test" {
		t.Errorf("Account = %q, %v", account, err)
	}

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "sand/abc/p3.media", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := p.Get(ctx, "sand/abc/p3.media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	// Overwriting has to land as a new version of the same file rather than a
	// second item under the same name.
	if err := p.Put(ctx, "sand/abc/p3.media", []byte("second")); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	objects, err := p.List(ctx, "sand/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "sand/abc/p3.media" {
		t.Errorf("List = %+v, want one shard keyed by its object key", objects)
	}

	info, err := p.Stat(ctx, "sand/abc/p3.media")
	if err != nil || info.Size != int64(len("second")) {
		t.Errorf("Stat = %+v, %v", info, err)
	}

	usage, err := p.Usage(ctx)
	if err != nil || usage.Total != 1000 || usage.Used != 250 {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	if err := p.Delete(ctx, "sand/abc/p3.media"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "sand/abc/p3.media"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := p.Delete(ctx, "sand/abc/p3.media"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestBoxNamesRoundTripThroughAFlatFolder(t *testing.T) {
	key := "sand/6f1b8c2a/p2.media"
	if got := keyFromBoxName(boxName(key)); got != key {
		t.Errorf("%q survived the flattening as %q", key, got)
	}
}

// --- Registry ---------------------------------------------------------------

func TestNewProviderKindsAreRegistered(t *testing.T) {
	for _, kind := range []Kind{KindOneDrive, KindBox, KindProton} {
		spec, ok := SpecFor(kind)
		if !ok {
			t.Fatalf("no spec registered for %s", kind)
		}
		if spec.Label == "" || spec.Description == "" {
			t.Errorf("%s is missing a label or description", kind)
		}
	}

	// Proton Drive is a folder the desktop client syncs, so it must arrive
	// with a path already suggested.
	proton, _ := SpecFor(KindProton)
	if proton.Fields[0].Default == "" {
		t.Error("the Proton Drive folder field should come pre-filled with a guess")
	}
	if proton.OAuth != nil {
		t.Error("Proton publishes no API to sign in to")
	}
}

func TestSignInBackendsDeclareWhatTheFlowNeeds(t *testing.T) {
	for _, kind := range []Kind{KindGDrive, KindDropbox, KindOneDrive, KindBox} {
		spec, ok := SpecFor(kind)
		if !ok || spec.OAuth == nil {
			t.Fatalf("%s should be connectable by signing in", kind)
		}
		o := spec.OAuth
		if o.AuthURL == "" || o.TokenURL == "" || o.SignInLabel == "" {
			t.Errorf("%s: incomplete OAuth spec %+v", kind, o)
		}
		if o.ClientIDField == "" || o.RefreshTokenField == "" {
			t.Errorf("%s: the exchanged tokens have nowhere to go", kind)
		}
		if o.ClientIDEnv == "" {
			t.Errorf("%s: no environment variable to preconfigure an app with", kind)
		}

		// Every option the flow writes must be a field the backend declares,
		// or it would never be redacted on its way to the browser.
		declared := map[string]bool{}
		for _, f := range spec.Fields {
			declared[f.Key] = true
		}
		for _, key := range []string{o.ClientIDField, o.ClientSecretField, o.RefreshTokenField} {
			if key != "" && !declared[key] {
				t.Errorf("%s: sign-in writes %q, which is not a declared field", kind, key)
			}
		}
	}
}
