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
	"regexp"
	"strings"
	"sync"
	"testing"
)

// driveStub is enough of Drive's v3 files API to drive the backend: a flat
// store of files with parents and appProperties, a query matcher for the
// handful of clauses the backend writes, multipart upload, and folder creation.
type driveStub struct {
	t  *testing.T
	mu sync.Mutex
	// files by ID, in creation order so a query answers deterministically.
	order []string
	files map[string]*driveStubFile
	next  int

	// What the backend asked, for the assertions that are about the asking.
	queries        []string
	foldersCreated int

	// failUploads makes every upload answer with a 500 after reading the
	// body, the way Drive does when it is having a bad moment.
	failUploads bool
}

type driveStubFile struct {
	Name    string
	MIME    string
	Parents []string
	Props   map[string]string
	Data    []byte
}

func newDriveStub(t *testing.T) *driveStub {
	return &driveStub{t: t, files: map[string]*driveStubFile{}}
}

// install points the backend at the stub for the rest of the test.
func (d *driveStub) install(t *testing.T) *httptest.Server {
	server := httptest.NewServer(d)
	restoreAPI, restoreUpload := gdriveAPI, gdriveUploadAPI
	gdriveAPI = server.URL + "/drive/v3"
	gdriveUploadAPI = server.URL + "/upload/drive/v3"
	t.Cleanup(func() {
		gdriveAPI, gdriveUploadAPI = restoreAPI, restoreUpload
		server.Close()
	})
	return server
}

func (d *driveStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	switch {
	case r.URL.Path == "/drive/v3/about":
		w.Write([]byte(`{"user":{"emailAddress":"alice@example.test"},"storageQuota":{"limit":"1000","usage":"250"}}`))
	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet:
		d.list(w, r)
	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodPost:
		d.createFolder(w, r)
	case r.URL.Path == "/upload/drive/v3/files" && r.Method == http.MethodPost:
		d.upload(w, r)
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
		d.one(w, r, strings.TrimPrefix(r.URL.Path, "/drive/v3/files/"))
	default:
		d.t.Errorf("drive stub: unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

var (
	reProp    = regexp.MustCompile(`appProperties has \{key='sandKey' and value='([^']*)'\}`)
	reAnyProp = regexp.MustCompile(`appProperties has \{key='sandKey' and value!=''\}`)
	reParent  = regexp.MustCompile(`'([^']+)' in parents`)
	reName    = regexp.MustCompile(`name='([^']*)'`)
	reMIME    = regexp.MustCompile(`mimeType='([^']*)'`)
)

// matches answers a query the way Drive would, for the clauses the backend
// uses: a sandKey value, any sandKey, a parent, a name, a MIME type.
func (f *driveStubFile) matches(q string) bool {
	if m := reProp.FindStringSubmatch(q); m != nil && f.Props["sandKey"] != m[1] {
		return false
	}
	if reAnyProp.MatchString(q) && f.Props["sandKey"] == "" {
		return false
	}
	if m := reParent.FindStringSubmatch(q); m != nil {
		// As Drive has it: a file created with no parent is a child of the
		// root, and 'root' names it in a query.
		has := m[1] == "root" && len(f.Parents) == 0
		for _, p := range f.Parents {
			if p == m[1] {
				has = true
			}
		}
		if !has {
			return false
		}
	}
	if m := reName.FindStringSubmatch(q); m != nil && f.Name != m[1] {
		return false
	}
	if m := reMIME.FindStringSubmatch(q); m != nil && f.MIME != m[1] {
		return false
	}
	return true
}

func (d *driveStub) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	d.queries = append(d.queries, q)
	var out []map[string]any
	for _, id := range d.order {
		f := d.files[id]
		if !f.matches(q) {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "name": f.Name, "size": fmt.Sprint(len(f.Data)), "appProperties": f.Props,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"files": out})
}

func (d *driveStub) add(f *driveStubFile) string {
	d.next++
	id := fmt.Sprintf("id-%d", d.next)
	d.files[id] = f
	d.order = append(d.order, id)
	return id
}

func (d *driveStub) createFolder(w http.ResponseWriter, r *http.Request) {
	var meta struct {
		Name     string   `json:"name"`
		MIMEType string   `json:"mimeType"`
		Parents  []string `json:"parents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil || meta.MIMEType != gdriveFolderMIME {
		d.t.Errorf("drive stub: files.create without a folder body: %v %+v", err, meta)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	d.foldersCreated++
	id := d.add(&driveStubFile{Name: meta.Name, MIME: meta.MIMEType, Parents: meta.Parents})
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func (d *driveStub) upload(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		d.t.Errorf("drive stub: upload content type: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	metaPart, err := mr.NextPart()
	if err != nil {
		d.t.Errorf("drive stub: upload has no metadata part: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var meta struct {
		Name          string            `json:"name"`
		Parents       []string          `json:"parents"`
		AppProperties map[string]string `json:"appProperties"`
	}
	if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
		d.t.Errorf("drive stub: upload metadata: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	dataPart, err := mr.NextPart()
	if err != nil {
		d.t.Errorf("drive stub: upload has no data part: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	data, _ := io.ReadAll(dataPart)
	if d.failUploads {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"code":500,"message":"Internal Error"}}`))
		return
	}
	id := d.add(&driveStubFile{Name: meta.Name, Parents: meta.Parents, Props: meta.AppProperties, Data: data})
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func (d *driveStub) one(w http.ResponseWriter, r *http.Request, id string) {
	f, ok := d.files[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodDelete:
		delete(d.files, id)
		for i, o := range d.order {
			if o == id {
				d.order = append(d.order[:i], d.order[i+1:]...)
				break
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Query().Get("alt") == "media":
		w.Write(f.Data)
	default:
		json.NewEncoder(w).Encode(map[string]any{"id": id, "name": f.Name, "size": fmt.Sprint(len(f.Data))})
	}
}

// byKey is every stored file carrying this sandKey, in creation order.
func (d *driveStub) byKey(key string) []*driveStubFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*driveStubFile
	for _, id := range d.order {
		if f := d.files[id]; f.Props["sandKey"] == key {
			out = append(out, f)
		}
	}
	return out
}

func (d *driveStub) folders(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, f := range d.files {
		if f.MIME == gdriveFolderMIME && f.Name == name {
			n++
		}
	}
	return n
}

func driveProvider(t *testing.T, options map[string]string) Provider {
	opts := map[string]string{"access_token": "test-token"}
	for k, v := range options {
		opts[k] = v
	}
	p, err := newGDriveProvider(Config{ID: "drive", Kind: KindGDrive, Name: "drive", Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGDriveMakesItsFolderOnceAndWritesInsideIt(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	p := driveProvider(t, map[string]string{"folder": "SAND"})
	if err := p.Put(ctx, "a.1", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, "a.2", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if got := stub.folders("SAND"); got != 1 {
		t.Fatalf("folders named SAND = %d, want 1", got)
	}
	if stub.foldersCreated != 1 {
		t.Errorf("folder created %d times by one provider, want once", stub.foldersCreated)
	}
	stored := stub.byKey("a.1")
	if len(stored) != 1 || len(stored[0].Parents) != 1 || stub.files[stored[0].Parents[0]].Name != "SAND" {
		t.Fatalf("a.1 stored as %+v, want one file whose parent is the SAND folder", stored)
	}

	// A fresh provider — a restart — finds the folder rather than making a
	// second one beside it.
	again := driveProvider(t, map[string]string{"folder": "SAND"})
	if err := again.Put(ctx, "a.3", []byte("three")); err != nil {
		t.Fatal(err)
	}
	if got := stub.folders("SAND"); got != 1 {
		t.Errorf("after a restart, folders named SAND = %d, want still 1", got)
	}
	data, err := again.Get(ctx, "a.1")
	if err != nil || string(data) != "one" {
		t.Errorf("Get a.1 after a restart = %q, %v", data, err)
	}
}

func TestGDriveTwoFoldersOnOneAccountKeepTheirObjectsApart(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	parts := driveProvider(t, map[string]string{"folder": "SAND"})
	replicas := driveProvider(t, map[string]string{"folder": "Vault replicas"})

	if err := parts.Put(ctx, "x.1", []byte("part")); err != nil {
		t.Fatal(err)
	}
	// The same key in the other folder. Put erases the copies it replaces,
	// so an unscoped lookup here would have erased the part.
	if err := replicas.Put(ctx, "x.1", []byte("replica")); err != nil {
		t.Fatal(err)
	}
	if got := len(stub.byKey("x.1")); got != 2 {
		t.Fatalf("files under key x.1 = %d, want 2 — one per folder", got)
	}

	got, err := parts.Get(ctx, "x.1")
	if err != nil || string(got) != "part" {
		t.Errorf("parts.Get = %q, %v, want the part", got, err)
	}
	got, err = replicas.Get(ctx, "x.1")
	if err != nil || string(got) != "replica" {
		t.Errorf("replicas.Get = %q, %v, want the replica", got, err)
	}

	listed, err := parts.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Key != "x.1" || listed[0].Size != int64(len("part")) {
		t.Errorf("parts.List = %+v, want just the part", listed)
	}

	if err := replicas.Delete(ctx, "x.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := replicas.Get(ctx, "x.1"); err != ErrNotFound {
		t.Errorf("replicas.Get after delete = %v, want ErrNotFound", err)
	}
	got, err = parts.Get(ctx, "x.1")
	if err != nil || string(got) != "part" {
		t.Errorf("deleting the replica took the part with it: parts.Get = %q, %v", got, err)
	}
}

// A provider with no folder is scoped to the root, not to everything the app
// can see: another provider's folder on the same account holds objects under
// the same keys, and an unscoped root would list them, read them, and erase
// them on a Put. Objects written at the root before this was scoped are
// children of the root all the same, so they stay visible.
func TestGDriveRootIsScopedToRoot(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	// What another provider on the account holds, under a key this one
	// will use too — and an object written at the root before Put named
	// its parent, which Drive reports with no parent at all.
	stub.mu.Lock()
	stub.add(&driveStubFile{Name: "r.1", Parents: []string{"other-folder"},
		Props: map[string]string{"sandKey": "r.1"}, Data: []byte("theirs")})
	stub.add(&driveStubFile{Name: "r.0", Props: map[string]string{"sandKey": "r.0"}, Data: []byte("old root")})
	stub.mu.Unlock()

	p := driveProvider(t, nil)
	if err := p.Put(ctx, "r.1", []byte("root")); err != nil {
		t.Fatal(err)
	}
	if stub.foldersCreated != 0 {
		t.Errorf("a provider with no folder created %d folders", stub.foldersCreated)
	}
	for _, q := range stub.queries {
		if !strings.Contains(q, "'root' in parents") {
			t.Errorf("a root provider ran a query not scoped to the root: %q", q)
		}
	}
	if stored := stub.byKey("r.1"); len(stored) != 2 {
		t.Errorf("files under key r.1 = %d, want 2 — the other folder's copy was erased", len(stored))
	}

	listed, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, o := range listed {
		keys[o.Key] = fmt.Sprint(o.Size)
	}
	if len(keys) != 2 || keys["r.1"] != "4" || keys["r.0"] != "8" {
		t.Errorf("List = %+v, want r.1 and the old root object, and nothing from the other folder", listed)
	}
	got, err := p.Get(ctx, "r.0")
	if err != nil || string(got) != "old root" {
		t.Errorf("Get of an object written at the root before scoping = %q, %v", got, err)
	}

	if err := p.Delete(ctx, "r.1"); err != nil {
		t.Fatal(err)
	}
	if stored := stub.byKey("r.1"); len(stored) != 1 || string(stored[0].Data) != "theirs" {
		t.Errorf("after Delete, files under key r.1 = %+v, want only the other folder's copy", stored)
	}
}

// Put uploads first and erases the old copy after. The other order — delete,
// then upload — destroys the part when the upload fails, and the vault's
// health check then rebuilds and relocates a part this account never lost.
func TestGDriveFailedUploadLeavesTheOldCopy(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	p := driveProvider(t, map[string]string{"folder": "SAND"})
	if err := p.Put(ctx, "a.1", []byte("one")); err != nil {
		t.Fatal(err)
	}

	stub.mu.Lock()
	stub.failUploads = true
	stub.mu.Unlock()
	if err := p.Put(ctx, "a.1", []byte("two")); err == nil {
		t.Fatal("a Put whose upload failed reported success")
	}
	stub.mu.Lock()
	stub.failUploads = false
	stub.mu.Unlock()

	if stored := stub.byKey("a.1"); len(stored) != 1 || string(stored[0].Data) != "one" {
		t.Errorf("after a failed upload, files under a.1 = %+v, want the old copy untouched", stored)
	}
	got, err := p.Get(ctx, "a.1")
	if err != nil || string(got) != "one" {
		t.Errorf("Get after a failed overwrite = %q, %v, want the old copy", got, err)
	}

	// And a Put that does land replaces it: one file, the new bytes.
	if err := p.Put(ctx, "a.1", []byte("three")); err != nil {
		t.Fatal(err)
	}
	if stored := stub.byKey("a.1"); len(stored) != 1 || string(stored[0].Data) != "three" {
		t.Errorf("after an overwrite, files under a.1 = %+v, want one file with the new bytes", stored)
	}
	got, err = p.Get(ctx, "a.1")
	if err != nil || string(got) != "three" {
		t.Errorf("Get after an overwrite = %q, %v", got, err)
	}
}

// A duplicate an earlier failure left behind is cleared by the next Put,
// which keeps exactly the copy it wrote.
func TestGDrivePutClearsDuplicatesUnderTheKey(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	p := driveProvider(t, map[string]string{"folder": "SAND"})
	if err := p.Put(ctx, "d.1", []byte("first")); err != nil {
		t.Fatal(err)
	}
	folder := stub.byKey("d.1")[0].Parents[0]
	stub.mu.Lock()
	stub.add(&driveStubFile{Name: "d.1", Parents: []string{folder},
		Props: map[string]string{"sandKey": "d.1"}, Data: []byte("stray")})
	stub.mu.Unlock()

	if err := p.Put(ctx, "d.1", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if stored := stub.byKey("d.1"); len(stored) != 1 || string(stored[0].Data) != "second" {
		t.Errorf("files under d.1 = %+v, want only the copy the Put wrote", stored)
	}
}

// List answers with the key a file was stored under — its sandKey property,
// which is what Get and Delete look it up by — and not with the Drive
// filename, which is the key flattened for display and can be renamed in
// Drive's own UI without the property changing.
func TestGDriveListReadsTheKeyFromAppProperties(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	p := driveProvider(t, map[string]string{"folder": "SAND"})
	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	for _, f := range stub.files {
		if f.Props["sandKey"] == "abc-p1.sand" {
			f.Name = "renamed in the Drive UI.bin"
		}
	}
	stub.mu.Unlock()

	listed, err := p.List(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Key != "abc-p1.sand" || listed[0].Size != 4 {
		t.Errorf("List = %+v, want abc-p1.sand by its property", listed)
	}
	if got, err := p.Get(ctx, listed[0].Key); err != nil || string(got) != "part" {
		t.Errorf("Get of the listed key = %q, %v", got, err)
	}
}

func TestGDrivePastedFolderIDIsUsedAsItIsAndOutranksTheName(t *testing.T) {
	stub := newDriveStub(t)
	stub.install(t)
	ctx := context.Background()

	p := driveProvider(t, map[string]string{"folder_id": "pasted-id", "folder": "SAND"})
	if err := p.Put(ctx, "k.1", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if stub.foldersCreated != 0 || stub.folders("SAND") != 0 {
		t.Errorf("a pasted ID still had the named folder looked up or created")
	}
	if stored := stub.byKey("k.1"); len(stored) != 1 || strings.Join(stored[0].Parents, ",") != "pasted-id" {
		t.Errorf("k.1 stored as %+v, want a parent of pasted-id", stored)
	}
	for _, q := range stub.queries {
		if strings.Contains(q, "mimeType=") {
			t.Errorf("a pasted ID still ran a folder lookup: %q", q)
		}
	}
}
