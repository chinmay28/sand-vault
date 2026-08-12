package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sand-project/sand/internal/version"
)

// testClient drives the full HTTP handler the same way a browser does,
// carrying the session cookie between calls.
type testClient struct {
	t       *testing.T
	handler http.Handler
	cookies []*http.Cookie
	origin  string
}

func newTestClient(t *testing.T) *testClient {
	t.Helper()

	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand")}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return &testClient{t: t, handler: handler, origin: "http://example.test"}
}

func (c *testClient) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	c.t.Helper()

	req := httptest.NewRequest(method, path, body)
	req.Host = "example.test"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", c.origin)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if fresh := w.Result().Cookies(); len(fresh) > 0 {
		c.cookies = fresh
	}
	return w
}

func (c *testClient) json(method, path string, payload any) (*httptest.ResponseRecorder, map[string]any) {
	c.t.Helper()

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(raw)
	}

	w := c.do(method, path, body, "application/json")
	out := map[string]any{}
	if w.Body.Len() > 0 {
		json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w, out
}

// setup creates the vault and connects `accounts` local-folder providers.
func (c *testClient) setup(password string, accounts int) []string {
	c.t.Helper()

	w, _ := c.json(http.MethodPost, "/api/vault/init", map[string]any{"password": password})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("init vault: %d %s", w.Code, w.Body.String())
	}

	roots := make([]string, accounts)
	base := c.t.TempDir()
	for i := 0; i < accounts; i++ {
		roots[i] = filepath.Join(base, fmt.Sprintf("cloud%d", i))
		w, body := c.json(http.MethodPost, "/api/providers", map[string]any{
			"kind":    "local",
			"name":    fmt.Sprintf("cloud%d", i),
			"options": map[string]string{"path": roots[i]},
		})
		if w.Code != http.StatusCreated {
			c.t.Fatalf("connect account %d: %d %v", i, w.Code, body)
		}
	}
	return roots
}

// upload posts one file into a folder and returns the parsed result.
func (c *testClient) upload(name, dir string, content []byte) map[string]any {
	c.t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("files[]", name)
	if err != nil {
		c.t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(content)
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

func TestVaultLifecycleOverHTTP(t *testing.T) {
	c := newTestClient(t)

	w, body := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if body["initialized"] != false {
		t.Errorf("a fresh install should report initialized=false, got %v", body["initialized"])
	}

	c.setup("s3cret-passphrase", 3)

	w, body = c.json(http.MethodGet, "/api/vault", nil)
	if body["unlocked"] != true {
		t.Errorf("vault should be unlocked after init, got %v", body)
	}

	// Lock, then confirm the browsing endpoints refuse to answer.
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	w, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	if w.Code != http.StatusUnauthorized || body["code"] != "LOCKED" {
		t.Errorf("listing a locked vault = %d %v, want 401 LOCKED", w.Code, body)
	}

	// Wrong password stays locked.
	w, body = c.json(http.MethodPost, "/api/vault/unlock", map[string]any{"password": "not-it"})
	if w.Code != http.StatusUnauthorized || body["code"] != "WRONG_PASSWORD" {
		t.Errorf("bad unlock = %d %v, want 401 WRONG_PASSWORD", w.Code, body)
	}

	if w, _ := c.json(http.MethodPost, "/api/vault/unlock", map[string]any{"password": "s3cret-passphrase"}); w.Code != http.StatusOK {
		t.Fatalf("unlock: %d", w.Code)
	}
	if w, _ := c.json(http.MethodGet, "/api/files?path=/", nil); w.Code != http.StatusOK {
		t.Errorf("listing after unlock: %d", w.Code)
	}
}

func TestUploadListDownloadRoundTrip(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	content := []byte("the contents of a file that was scattered and gathered again")
	file := c.upload("report.txt", "/", content)

	shards, _ := file["shards"].([]any)
	if len(shards) != 3 {
		t.Fatalf("expected 3 shards, got %d", len(shards))
	}

	// Every part must sit on a different account.
	seen := map[string]bool{}
	for _, raw := range shards {
		shard := raw.(map[string]any)
		id := shard["provider_id"].(string)
		if seen[id] {
			t.Error("two parts landed on the same account")
		}
		seen[id] = true
	}

	w, body := c.json(http.MethodGet, "/api/files?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	files := body["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != "report.txt" {
		t.Fatalf("unexpected listing: %v", body)
	}

	id := file["id"].(string)
	w = c.do(http.MethodGet, "/api/files/"+id+"/content?download=1", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Errorf("downloaded bytes differ from the original")
	}
	if disp := w.Header().Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", disp)
	}
	if store := w.Header().Get("Cache-Control"); !strings.Contains(store, "no-store") {
		t.Errorf("decrypted content must not be cacheable, got %q", store)
	}
}

func TestInlineContentForcesDownloadForRiskyTypes(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// A stored HTML file must never render in the app's own origin.
	file := c.upload("page.html", "/", []byte("<script>alert(1)</script>"))
	id := file["id"].(string)

	w := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("content: %d", w.Code)
	}
	if disp := w.Header().Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment") {
		t.Errorf("HTML served with Content-Disposition %q, want attachment", disp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on stored content")
	}

	// A plain image is fine to render inline.
	img := c.upload("photo.png", "/", []byte("\x89PNG\r\n\x1a\nnot-really-a-png"))
	w = c.do(http.MethodGet, "/api/files/"+img["id"].(string)+"/content", nil, "")
	if disp := w.Header().Get("Content-Disposition"); !strings.HasPrefix(disp, "inline") {
		t.Errorf("PNG served with Content-Disposition %q, want inline", disp)
	}
}

func TestFoldersOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos/2024"}); w.Code != http.StatusCreated {
		t.Fatalf("create folder: %d %v", w.Code, body)
	}
	c.upload("a.txt", "/photos/2024", []byte("a"))

	w, body := c.json(http.MethodGet, "/api/files?path=/photos", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	folders := body["folders"].([]any)
	if len(folders) != 1 || folders[0] != "2024" {
		t.Errorf("folders = %v, want [2024]", folders)
	}

	// Non-recursive delete must refuse to drop a folder with contents.
	if w, _ := c.json(http.MethodDelete, "/api/folders?path=/photos", nil); w.Code == http.StatusOK {
		t.Error("expected a non-recursive delete of a non-empty folder to fail")
	}
	if w, body := c.json(http.MethodDelete, "/api/folders?path=/photos&recursive=1", nil); w.Code != http.StatusOK {
		t.Fatalf("recursive delete: %d %v", w.Code, body)
	}

	w, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	if len(body["folders"].([]any)) != 0 {
		t.Errorf("root should be empty, got %v", body)
	}
}

func TestFileHealthAndDeleteOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	file := c.upload("watched.bin", "/", []byte("payload"))
	id := file["id"].(string)

	w, body := c.json(http.MethodGet, "/api/files/"+id+"/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d", w.Code)
	}
	if body["recoverable"] != true {
		t.Errorf("fresh upload reported unrecoverable: %v", body)
	}

	if w, _ := c.json(http.MethodDelete, "/api/files/"+id, nil); w.Code != http.StatusOK {
		t.Fatalf("delete: %d", w.Code)
	}
	if w, _ := c.json(http.MethodGet, "/api/files/"+id, nil); w.Code != http.StatusNotFound {
		t.Errorf("metadata after delete = %d, want 404", w.Code)
	}
}

func TestMoveOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	file := c.upload("draft.txt", "/", []byte("content"))
	id := file["id"].(string)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/final"})

	w, body := c.json(http.MethodPost, "/api/files/"+id+"/move",
		map[string]any{"dir": "/final", "name": "published.txt"})
	if w.Code != http.StatusOK {
		t.Fatalf("move: %d %v", w.Code, body)
	}
	if body["path"] != "/final/published.txt" {
		t.Errorf("path = %v, want /final/published.txt", body["path"])
	}

	// The content must still come back after a rename — the parts never moved.
	w = c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if w.Body.String() != "content" {
		t.Errorf("content after move = %q", w.Body.String())
	}
}

func TestProviderSecretsAreNeverReturned(t *testing.T) {
	c := newTestClient(t)
	c.json(http.MethodPost, "/api/vault/init", map[string]any{"password": "pw"})

	w, _ := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind": "s3",
		"name": "unreachable",
		"options": map[string]string{
			"bucket":            "b",
			"access_key_id":     "AKIA",
			"secret_access_key": "TOP-SECRET-KEY",
			"endpoint":          "http://127.0.0.1:1", // nothing listening
		},
	})
	// The account is rejected because it cannot be reached, and the error must
	// not echo the credentials back.
	if w.Code == http.StatusCreated {
		t.Fatal("expected an unreachable endpoint to be refused")
	}
	if strings.Contains(w.Body.String(), "TOP-SECRET-KEY") {
		t.Error("the secret key leaked into an error response")
	}

	w, body := c.json(http.MethodGet, "/api/providers", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("providers: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "TOP-SECRET-KEY") {
		t.Errorf("the provider listing leaked a secret: %v", body)
	}
}

func TestCrossOriginWritesAreRejected(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 2)

	req := httptest.NewRequest(http.MethodPost, "/api/folders", strings.NewReader(`{"path":"/evil"}`))
	req.Host = "example.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://attacker.example")
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("cross-origin write = %d, want 403", w.Code)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 2)

	// Drop the session cookie: the vault is still unlocked in-process, but
	// without a session there is no way in.
	c.cookies = nil

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/files?path=/"},
		{http.MethodGet, "/api/providers"},
		{http.MethodDelete, "/api/folders?path=/x"},
	} {
		w, body := c.json(probe.method, probe.path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (%v)", probe.method, probe.path, w.Code, body)
		}
	}
}

func TestStrictPolicyRefusesUploadWithOneAccount(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 1)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("files[]", "lonely.txt")
	part.Write([]byte("data"))
	mw.WriteField("path", "/")
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	if w.Code != http.StatusBadGateway {
		t.Fatalf("upload with one account = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "at least") {
		t.Errorf("error should explain the account requirement: %s", w.Body.String())
	}
}

func TestProviderSpecsAreAvailableWhileLocked(t *testing.T) {
	c := newTestClient(t)

	w, body := c.json(http.MethodGet, "/api/providers/specs", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("specs: %d", w.Code)
	}
	specs, _ := body["specs"].([]any)
	if len(specs) < 5 {
		t.Errorf("expected every backend to be described, got %d", len(specs))
	}
}

func TestHealthReportsTheBuildVersion(t *testing.T) {
	c := newTestClient(t)

	w, body := c.json(http.MethodGet, "/api/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d", w.Code)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}

	// One rendering of the version, from one place — the CLI, the health
	// endpoint and the web header all read this same string.
	if got := body["version"]; got != version.String() {
		t.Errorf("version = %v, want %s", got, version.String())
	}
	if !strings.HasPrefix(version.String(), "v") {
		t.Errorf("version %q should be v-prefixed to match the release tags", version.String())
	}
}
