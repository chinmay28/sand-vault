package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/version"
)

// testClient drives the full HTTP handler the same way a browser does,
// carrying the session cookie between calls.
type testClient struct {
	t       *testing.T
	handler http.Handler
	cookies []*http.Cookie
	origin  string

	// server is the handler's own Server, for the few tests that have to look
	// at state the HTTP surface does not expose.
	server *Server
}

func newTestClient(t *testing.T) *testClient {
	t.Helper()

	dir := t.TempDir()
	s := &Server{VaultPath: filepath.Join(dir, "vault.sand")}
	handler, err := s.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	// Manifest backups are pushed in the background, so let any in-flight push
	// finish before the temporary account folders are cleaned up.
	t.Cleanup(func() {
		if s.vault != nil {
			s.vault.AwaitBackupSync()
		}
	})
	return &testClient{t: t, handler: handler, server: s, origin: "http://example.test"}
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

// uploadTo posts one file, naming the accounts its parts should go to.
func (c *testClient) uploadTo(name, dir string, content []byte, accounts []string) map[string]any {
	c.t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("files[]", name)
	if err != nil {
		c.t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(content)
	mw.WriteField("path", dir)
	for _, id := range accounts {
		mw.WriteField("accounts", id)
	}
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	var resp struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		c.t.Fatalf("expected 1 upload result, got %d: %s", len(resp.Results), w.Body.String())
	}
	return resp.Results[0]
}

// providerIDs lists the connected accounts in the order they were added.
func (c *testClient) providerIDs() []string {
	c.t.Helper()

	w, body := c.json(http.MethodGet, "/api/providers", nil)
	if w.Code != http.StatusOK {
		c.t.Fatalf("providers: %d %s", w.Code, w.Body.String())
	}
	var ids []string
	for _, raw := range body["providers"].([]any) {
		ids = append(ids, raw.(map[string]any)["id"].(string))
	}
	return ids
}

// shardAccounts is the set of accounts an upload result's parts landed on.
func shardAccounts(t *testing.T, file map[string]any) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, raw := range file["shards"].([]any) {
		out[raw.(map[string]any)["provider_id"].(string)] = true
	}
	return out
}

func TestUploadGoesToTheAccountsItNames(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()
	chosen := []string{ids[1], ids[3], ids[4]}

	result := c.uploadTo("picked.txt", "/", []byte("payload"), chosen)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}

	landed := shardAccounts(t, result["file"].(map[string]any))
	if len(landed) != 3 {
		t.Fatalf("parts landed on %d accounts, want 3", len(landed))
	}
	for _, id := range chosen {
		if !landed[id] {
			t.Errorf("chosen account %s holds no part", id)
		}
	}
}

func TestUploadRejectsAnAccountThatIsNotConnected(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	result := c.uploadTo("nowhere.txt", "/", []byte("payload"), []string{"not-an-account"})
	if ok, _ := result["ok"].(bool); ok {
		t.Fatal("expected an upload naming an unknown account to fail")
	}
	if msg, _ := result["error"].(string); !strings.Contains(msg, "no connected account") {
		t.Errorf("error should name the problem, got %q", msg)
	}
}

func TestDefaultAccountsApplyToLaterUploads(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()
	defaults := []string{ids[0], ids[2], ids[4]}

	w, body := c.json(http.MethodPost, "/api/vault/defaults", map[string]any{"accounts": defaults})
	if w.Code != http.StatusOK {
		t.Fatalf("set defaults: %d %s", w.Code, w.Body.String())
	}
	if got := body["default_accounts"].([]any); len(got) != 3 {
		t.Fatalf("default_accounts = %v, want the three that were set", got)
	}

	// And the status endpoint reports them, so the browser can preselect them.
	_, status := c.json(http.MethodGet, "/api/vault", nil)
	stats := status["stats"].(map[string]any)
	if got := stats["default_accounts"].([]any); len(got) != 3 {
		t.Errorf("stats.default_accounts = %v, want 3 accounts", got)
	}

	landed := shardAccounts(t, c.upload("defaulted.txt", "/", []byte("payload")))
	for _, id := range defaults {
		if !landed[id] {
			t.Errorf("default account %s holds no part", id)
		}
	}

	// Clearing hands the choice back to the per-file pick.
	if w, _ := c.json(http.MethodPost, "/api/vault/defaults", map[string]any{"accounts": []string{}}); w.Code != http.StatusOK {
		t.Fatalf("clear defaults: %d %s", w.Code, w.Body.String())
	}
	_, status = c.json(http.MethodGet, "/api/vault", nil)
	stats = status["stats"].(map[string]any)
	if got := stats["default_accounts"].([]any); len(got) != 0 {
		t.Errorf("stats.default_accounts = %v, want it cleared", got)
	}
}

func TestDefaultAccountsRejectAnUnknownAccount(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	ids := c.providerIDs()

	w, _ := c.json(http.MethodPost, "/api/vault/defaults",
		map[string]any{"accounts": []string{ids[0], "not-an-account"}})
	if w.Code == http.StatusOK {
		t.Fatal("expected a default naming an unknown account to be refused")
	}
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

func TestChangingThePasswordReEncryptsWhatIsStored(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	content := []byte("scattered under the key that is about to be replaced")
	file := c.upload("secret.txt", "/", content)
	id := file["id"].(string)
	shardsBefore := file["shards"].([]any)
	keyBefore := shardsBefore[0].(map[string]any)["key"].(string)

	w, body := c.json(http.MethodPost, "/api/vault/password", map[string]any{
		"old_password": "pw",
		"new_password": "a different passphrase entirely",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	if body["migrated"] != float64(1) || body["remaining"] != float64(0) {
		t.Fatalf("report = %v, want the one file re-encrypted", body)
	}

	// Same file, same bytes, different parts: the migration rewrote them.
	w, meta := c.json(http.MethodGet, "/api/files/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("file meta: %d %s", w.Code, w.Body.String())
	}
	shardsAfter := meta["file"].(map[string]any)["shards"].([]any)
	if keyAfter := shardsAfter[0].(map[string]any)["key"].(string); keyAfter == keyBefore {
		t.Error("the parts kept their keys, so nothing was re-encrypted")
	}

	w = c.do(http.MethodGet, "/api/files/"+id+"/content?download=1", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("download after the change: %d %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Error("the re-encrypted file does not read back as it was stored")
	}

	// And the vault answers to the new password alone.
	c.json(http.MethodPost, "/api/vault/lock", nil)
	if w, _ := c.json(http.MethodPost, "/api/vault/unlock", map[string]any{"password": "pw"}); w.Code != http.StatusUnauthorized {
		t.Errorf("the old password still unlocks the vault: %d", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/unlock",
		map[string]any{"password": "a different passphrase entirely"}); w.Code != http.StatusOK {
		t.Fatalf("unlock with the new password: %d", w.Code)
	}
}

func TestDeferredMigrationIsFinishedByTheMigrateEndpoint(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.upload("waiting.txt", "/", []byte("still on the old key"))

	w, body := c.json(http.MethodPost, "/api/vault/password", map[string]any{
		"old_password": "pw",
		"new_password": "changed in a hurry",
		"migrate":      false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	if body["remaining"] != float64(1) {
		t.Fatalf("report = %v, want one file outstanding", body)
	}

	// The status endpoint is how the app knows to offer finishing it.
	w, status := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	stats := status["stats"].(map[string]any)
	if stats["pending_migration"] != float64(1) {
		t.Errorf("stats = %v, want one file pending migration", stats)
	}

	w, body = c.json(http.MethodPost, "/api/vault/migrate", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("migrate: %d %s", w.Code, w.Body.String())
	}
	if body["migrated"] != float64(1) || body["remaining"] != float64(0) {
		t.Fatalf("report = %v, want the outstanding file migrated", body)
	}

	w, status = c.json(http.MethodGet, "/api/vault", nil)
	if pending := status["stats"].(map[string]any)["pending_migration"]; pending != float64(0) {
		t.Errorf("pending_migration = %v after migrating, want 0", pending)
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

// searchPaths runs a search and returns the paths it found, in order.
func (c *testClient) searchPaths(query string) []string {
	c.t.Helper()

	w, body := c.json(http.MethodGet, "/api/search?"+query, nil)
	if w.Code != http.StatusOK {
		c.t.Fatalf("search %q: %d %v", query, w.Code, body)
	}
	hits, _ := body["hits"].([]any)
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.(map[string]any)["path"].(string))
	}
	return out
}

func TestSearchOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos/2024"})
	c.upload("beach.jpg", "/photos/2024", []byte("jpeg-ish"))
	c.upload("beach-notes.txt", "/", []byte("where the photo was taken"))
	c.upload("unrelated.bin", "/", []byte("nothing to find here"))

	if got := c.searchPaths("q=beach"); len(got) != 2 ||
		got[0] != "/beach-notes.txt" || got[1] != "/photos/2024/beach.jpg" {
		t.Errorf("search for beach = %v, want the two beach files, shallowest first", got)
	}

	// A folder is a result in its own right.
	if got := c.searchPaths("q=photos"); len(got) != 1 || got[0] != "/photos" {
		t.Errorf("search for photos = %v, want [/photos]", got)
	}

	// Scope, type and wildcards all reach the vault.
	if got := c.searchPaths("q=beach&path=/photos"); len(got) != 1 || got[0] != "/photos/2024/beach.jpg" {
		t.Errorf("scoped search = %v, want only the file under /photos", got)
	}
	if got := c.searchPaths("q=photos&type=file"); len(got) != 0 {
		t.Errorf("file-only search for photos = %v, want nothing", got)
	}
	if got := c.searchPaths("q=" + url.QueryEscape("*.jpg")); len(got) != 1 || got[0] != "/photos/2024/beach.jpg" {
		t.Errorf("wildcard search = %v, want the jpg", got)
	}

	// A file hit carries its index entry, so a result row needs no second call.
	w, body := c.json(http.MethodGet, "/api/search?q=beach.jpg", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %v", w.Code, body)
	}
	hit := body["hits"].([]any)[0].(map[string]any)
	if hit["type"] != "file" {
		t.Errorf("type = %v, want file", hit["type"])
	}
	file, _ := hit["file"].(map[string]any)
	if file == nil || len(file["shards"].([]any)) != 3 {
		t.Errorf("hit = %v, want the entry and its three shards", hit)
	}
}

func TestSearchRejectsAnEmptyQuery(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	w, body := c.json(http.MethodGet, "/api/search?q=%20", nil)
	if w.Code != http.StatusBadRequest || body["code"] != "BAD_REQUEST" {
		t.Errorf("empty search = %d %v, want 400 BAD_REQUEST", w.Code, body)
	}

	w, body = c.json(http.MethodGet, "/api/search?q=x&path=/nowhere", nil)
	if w.Code != http.StatusNotFound || body["code"] != "NOT_FOUND" {
		t.Errorf("search in a missing folder = %d %v, want 404 NOT_FOUND", w.Code, body)
	}
}

func TestSearchNeedsAnOpenVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.upload("secret-name.txt", "/", []byte("x"))

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}

	w, body := c.json(http.MethodGet, "/api/search?q=secret", nil)
	if w.Code != http.StatusUnauthorized || body["code"] != "LOCKED" {
		t.Errorf("searching a locked vault = %d %v, want 401 LOCKED", w.Code, body)
	}
	if strings.Contains(w.Body.String(), "secret-name") {
		t.Error("a locked vault must not leak a filename")
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

func TestDefaultPort(t *testing.T) {
	// The default is duplicated into the Makefile, the Vite dev proxy, the
	// install scripts and the nginx template, so a silent change here desyncs
	// all of them — most visibly `npm run dev`, whose proxy would then point at
	// nothing.
	if DefaultPort != 8123 {
		t.Errorf("DefaultPort = %d, want 8123 — update the Makefile, web/vite.config.js "+
			"and scripts/ alongside any deliberate change", DefaultPort)
	}
}

func TestDefaultBind(t *testing.T) {
	// All interfaces by default, matching the installers. This is a security
	// relevant default — the server holds plaintext and /api/vault/unlock is
	// unauthenticated — so it should not drift silently in either direction,
	// and the copies in scripts/ have to move with it.
	if DefaultBind != "0.0.0.0" {
		t.Errorf("DefaultBind = %q, want 0.0.0.0 — update scripts/quickstart.sh, "+
			"scripts/deploy-linux.sh and scripts/deploy-windows.ps1 alongside any "+
			"deliberate change", DefaultBind)
	}
}

func TestNonLoopbackBindIsWarnedAbout(t *testing.T) {
	// The warning is the only thing standing between the new default and a
	// user who has not thought about TLS, so assert the condition that fires it
	// rather than trusting it stays wired up.
	for _, bind := range []string{"127.0.0.1", "localhost", "::1"} {
		if warnsOnBind(bind) {
			t.Errorf("bind %q is loopback and should not warn", bind)
		}
	}
	for _, bind := range []string{"0.0.0.0", "192.168.1.10", "::"} {
		if !warnsOnBind(bind) {
			t.Errorf("bind %q is reachable off-host and must warn", bind)
		}
	}
}

// ---------------------------------------------------------------------------
// Frontend caching
// ---------------------------------------------------------------------------

// A browser handed no expiry information at all is free to invent one, and
// mobile browsers invent generously. index.html names the current bundle, so a
// stale copy pins the whole app to the build it was cached from — deploys stop
// reaching the device entirely. These assert the headers that prevent it.

func TestIndexRevalidatesOnEveryLoad(t *testing.T) {
	c := newTestClient(t)

	for _, path := range []string{"/", "/some/app/route"} {
		w := c.do("GET", path, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, "no-cache")
		}
		if w.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag, so revalidating costs a full re-download", path)
		}
	}
}

func TestFingerprintedAssetsAreCachedForever(t *testing.T) {
	c := newTestClient(t)

	// Whatever this build emitted — the name carries a content hash, so the
	// test cannot spell it out.
	entries, err := fs.Glob(webAssets, "dist/assets/*.js")
	if err != nil || len(entries) == 0 {
		t.Skipf("no built frontend to serve (%v)", err)
	}
	path := strings.TrimPrefix(entries[0], "dist")

	w := c.do("GET", path, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for %s, got %d", path, w.Code)
	}
	got := w.Header().Get("Cache-Control")
	if !strings.Contains(got, "immutable") || !strings.Contains(got, "max-age=") {
		t.Errorf("Cache-Control = %q, want a long-lived immutable directive", got)
	}
}

func TestUnchangedIndexAnswers304(t *testing.T) {
	c := newTestClient(t)

	first := c.do("GET", "/", nil, "")
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag on the first response")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.test"
	req.Header.Set("If-None-Match", tag)
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 for a matching ETag, got %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body", w.Body.Len())
	}
}

func TestETagChangesWithContent(t *testing.T) {
	// Two different files must not share a validator, or a browser will hold
	// one while believing it has the other.
	tags, err := buildETags(mustDistFS(t))
	if err != nil {
		t.Fatalf("buildETags: %v", err)
	}
	if len(tags) < 2 {
		t.Skip("frontend not built")
	}

	seen := make(map[string]string)
	for path, tag := range tags {
		if path == "/" {
			continue // deliberately shares index.html's tag
		}
		if other, dup := seen[tag]; dup {
			t.Errorf("%s and %s share the ETag %s", path, other, tag)
		}
		seen[tag] = path
	}
}

func mustDistFS(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(webAssets, "dist")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	return sub
}

// ---------------------------------------------------------------------------
// Home-screen install
// ---------------------------------------------------------------------------

// Adding the vault to a phone's home screen is served entirely out of the
// binary: the manifest, the icons it names, and the tags in index.html that
// point at them. A phone asks for these once, when the shortcut is created,
// and silently substitutes a screenshot of the page if anything is missing —
// so nothing here fails loudly in a browser, and these have to.

// webManifest is the shape the app declares to a phone. Only the fields the
// install prompt actually reads are modelled.
type webManifest struct {
	Name            string `json:"name"`
	ShortName       string `json:"short_name"`
	StartURL        string `json:"start_url"`
	Display         string `json:"display"`
	ThemeColor      string `json:"theme_color"`
	BackgroundColor string `json:"background_color"`
	Icons           []struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	} `json:"icons"`
}

func fetchManifest(t *testing.T, c *testClient) webManifest {
	t.Helper()

	w := c.do("GET", "/manifest.json", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /manifest.json: %d", w.Code)
	}
	// A manifest served as anything but JSON is a manifest a browser may
	// refuse to parse, which is the whole install gone.
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want a JSON type", ct)
	}

	var m webManifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	return m
}

func TestManifestDescribesTheInstalledApp(t *testing.T) {
	c := newTestClient(t)
	m := fetchManifest(t, c)

	if m.Name == "" || m.ShortName == "" {
		t.Errorf("name/short_name = %q/%q, both are shown under the icon", m.Name, m.ShortName)
	}
	if m.StartURL != "/" {
		t.Errorf("start_url = %q, want / — the shortcut must open the app root", m.StartURL)
	}
	if m.Display != "standalone" {
		t.Errorf("display = %q, want standalone", m.Display)
	}
	// The splash screen is drawn from these before a single byte of the app
	// has run; left unset it is white, which flashes against a dark app.
	if m.BackgroundColor == "" || m.ThemeColor == "" {
		t.Errorf("background_color/theme_color = %q/%q, both must be set",
			m.BackgroundColor, m.ThemeColor)
	}
}

// TestManifestIconsAreServedAtTheirDeclaredSize resolves every icon the
// manifest names. A manifest pointing at an icon that has moved, or that
// claims a size the file does not have, is dropped by the installer without
// complaint — so both are checked against the bytes actually served.
func TestManifestIconsAreServedAtTheirDeclaredSize(t *testing.T) {
	c := newTestClient(t)
	m := fetchManifest(t, c)

	if len(m.Icons) == 0 {
		t.Fatal("manifest declares no icons")
	}

	var maskable, largest int
	for _, icon := range m.Icons {
		w := c.do("GET", icon.Src, nil, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: %d — the manifest names an icon that is not served",
				icon.Src, w.Code)
			continue
		}

		cfg, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Errorf("%s: not a decodable PNG: %v", icon.Src, err)
			continue
		}
		if want := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height); want != icon.Sizes {
			t.Errorf("%s: declared %q but is %s", icon.Src, icon.Sizes, want)
		}
		if cfg.Width > largest {
			largest = cfg.Width
		}
		if strings.Contains(icon.Purpose, "maskable") {
			maskable++
		}
	}

	// Android's install prompt wants a 512px icon, and an adaptive launcher
	// crops any icon not marked maskable to fit its own shape.
	if largest < 512 {
		t.Errorf("largest icon is %dpx, want at least 512", largest)
	}
	if maskable == 0 {
		t.Error("no maskable icon — Android launchers will crop the mark")
	}
}

func TestIndexPointsPhonesAtTheHomeScreenIcon(t *testing.T) {
	c := newTestClient(t)

	w := c.do("GET", "/", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: %d", w.Code)
	}
	head := w.Body.String()

	// iOS reads none of the manifest: without these tags it puts a screenshot
	// of the page on the home screen instead of the mark.
	for _, want := range []string{
		`rel="manifest"`,
		`rel="apple-touch-icon"`,
		`name="apple-mobile-web-app-title"`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}

	touch := c.do("GET", "/apple-touch-icon.png", nil, "")
	if touch.Code != http.StatusOK {
		t.Fatalf("GET /apple-touch-icon.png: %d", touch.Code)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(touch.Body.Bytes()))
	if err != nil {
		t.Fatalf("apple-touch-icon is not a decodable PNG: %v", err)
	}
	// 180px is what iOS asks for at 3x; anything smaller is upscaled on the
	// home screen.
	if cfg.Width != 180 || cfg.Height != 180 {
		t.Errorf("apple-touch-icon is %dx%d, want 180x180", cfg.Width, cfg.Height)
	}
}
