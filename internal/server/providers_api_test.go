package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// webdavStub is a WebDAV server that answers to exactly one password, so an
// edit that changes the credentials can be told apart from one that does not.
func webdavStub(t *testing.T, password string) string {
	t.Helper()

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, given, ok := r.BasicAuth(); !ok || given != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"></d:multistatus>`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(stub.Close)
	return stub.URL
}

// The edit menu's endpoint. Renaming a cloud or recolouring it is the one write
// against an account that never reaches the account: no credentials change, no
// part moves, and the file index keeps telling the truth about both.
func TestProviderUpdateRenamesAndRecolours(t *testing.T) {
	c := newTestClient(t)
	c.setup("edit the accounts", 3)
	ids := c.providerIDs()

	file := c.upload("badge.txt", "/", []byte("which cloud am I on"))

	w, body := c.json(http.MethodPatch, "/api/providers/"+ids[0], map[string]any{
		"name":  "the blue one",
		"color": "#38BDF8",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH provider: %d %s", w.Code, w.Body.String())
	}
	updated := body["provider"].(map[string]any)
	if updated["name"] != "the blue one" {
		t.Errorf("name = %v", updated["name"])
	}
	if updated["color"] != "#38bdf8" {
		t.Errorf("color = %v, want #38bdf8", updated["color"])
	}

	// The listing the sidebar draws from carries the colour, which is the whole
	// point: the browser reads it back rather than picking one.
	_, listing := c.json(http.MethodGet, "/api/providers", nil)
	for _, raw := range listing["providers"].([]any) {
		account := raw.(map[string]any)
		if account["id"] != ids[0] {
			continue
		}
		if account["color"] != "#38bdf8" || account["name"] != "the blue one" {
			t.Errorf("listed as %v/%v", account["name"], account["color"])
		}
	}

	// And the part badges in the file list name the account by the name it now
	// has, not the one it had when the file was uploaded.
	_, health := c.json(http.MethodGet, "/api/files/"+file["id"].(string)+"/health", nil)
	named := false
	for _, raw := range health["shards"].([]any) {
		shard := raw.(map[string]any)
		if shard["provider_id"] != ids[0] {
			continue
		}
		named = true
		if shard["provider_name"] != "the blue one" {
			t.Errorf("part still held by %v", shard["provider_name"])
		}
	}
	if !named {
		t.Error("no part reported on the renamed account")
	}
}

func TestProviderUpdateRefusesABadEdit(t *testing.T) {
	c := newTestClient(t)
	c.setup("edit the accounts", 2)
	ids := c.providerIDs()

	for _, bad := range []map[string]any{
		{"name": "   "},
		{"name": "CLOUD1"}, // the account next door, in another case
		{"color": "chartreuse"},
	} {
		w, body := c.json(http.MethodPatch, "/api/providers/"+ids[0], bad)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PATCH %v: %d %s", bad, w.Code, w.Body.String())
		}
		if body["error"] == nil {
			t.Errorf("PATCH %v answered without saying why", bad)
		}
	}

	// An account that is not connected is a 400 with a message, not a panic on
	// a missing id.
	w, _ := c.json(http.MethodPatch, "/api/providers/not-an-account", map[string]any{"name": "x"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH unknown account: %d", w.Code)
	}
}

// Only what the request names changes. The dialog sends both fields, but the
// CLI and anything else sends one, and a colour must not be cleared by a
// rename that never mentioned it.
func TestProviderUpdateLeavesUnnamedFieldsAlone(t *testing.T) {
	c := newTestClient(t)
	c.setup("edit the accounts", 1)
	id := c.providerIDs()[0]

	c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"color": "#a3e635"})
	_, body := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"name": "renamed"})

	updated := body["provider"].(map[string]any)
	if updated["color"] != "#a3e635" {
		t.Errorf("color = %v after a rename, want #a3e635", updated["color"])
	}

	// And "" is a value rather than an absence: it is how the Automatic swatch
	// hands the choice back to the browser.
	_, cleared := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"color": ""})
	if colour, ok := cleared["provider"].(map[string]any)["color"]; ok && colour != "" {
		t.Errorf("color = %v after clearing it", colour)
	}
}

// The vault is the only thing that may answer this. A PATCH without a session
// is rejected exactly like every other write, so an account cannot be renamed
// through a locked vault.
func TestProviderUpdateNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("edit the accounts", 1)
	id := c.providerIDs()[0]

	c.cookies = nil
	w, _ := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"name": "sneaky"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("PATCH without a session: %d, want 401", w.Code)
	}
}

// The other half of the edit dialog: the settings the account is actually
// connected with. This one does reach the backend — an account whose keys have
// been rotated is repaired by retyping them here rather than by disconnecting
// it and forgetting every part it holds.
func TestProviderUpdateReconnectsWithNewSettings(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("edit the accounts", 3)
	id := c.providerIDs()[0]

	// A folder backend's setting is its folder, which is the same kind of edit
	// as a rotated key: what the account reaches, changed underneath it.
	moved := filepath.Join(filepath.Dir(roots[0]), "cloud0-moved")
	w, body := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{
		"options": map[string]string{"path": moved},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH settings: %d %s", w.Code, w.Body.String())
	}
	options := body["provider"].(map[string]any)["options"].(map[string]any)
	if options["path"] != moved {
		t.Errorf("path = %v, want %v", options["path"], moved)
	}

	// Verified before it was stored, which for a folder means it now exists.
	if info, err := os.Stat(moved); err != nil || !info.IsDir() {
		t.Errorf("the new folder was stored without being connected to: %v", err)
	}

	// And it is what the account is really using now, not just what it says:
	// a file uploaded afterwards lands in the new folder rather than the old
	// one, which is the difference between a stored setting and a live one.
	c.upload("moved.txt", "/", []byte("after the move"))
	entries, err := os.ReadDir(moved)
	if err != nil || len(entries) == 0 {
		t.Errorf("nothing was written to the new folder: %v (%v)", entries, err)
	}
}

// Settings SAND cannot connect with are settings it refuses to store. The
// account is left on the ones that still work rather than edited into one that
// answers nothing.
func TestProviderUpdateRefusesSettingsThatDoNotConnect(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("edit the accounts", 2)
	id := c.providerIDs()[0]

	// A file where a folder should be: the backend cannot be built on it.
	blocked := filepath.Join(c.t.TempDir(), "not-a-folder")
	if err := os.WriteFile(blocked, []byte("in the way"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w, body := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{
		"options": map[string]string{"path": blocked},
	})
	if w.Code == http.StatusOK {
		t.Fatalf("PATCH stored settings that do not connect: %s", w.Body.String())
	}
	if body["error"] == nil {
		t.Error("refused without saying why")
	}

	// Still on the folder it was connected with.
	_, listing := c.json(http.MethodGet, "/api/providers", nil)
	for _, raw := range listing["providers"].([]any) {
		account := raw.(map[string]any)
		if account["id"] != id {
			continue
		}
		if account["options"].(map[string]any)["path"] != roots[0] {
			t.Errorf("the account was moved by a failed edit: %v", account["options"])
		}
	}

	// A required setting cleared, and a setting the backend has never heard
	// of, are both refused before anything is built.
	for _, bad := range []map[string]string{{"path": ""}, {"paht": "/tmp"}} {
		w, _ := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"options": bad})
		if w.Code != http.StatusBadRequest {
			t.Errorf("PATCH %v: %d %s", bad, w.Code, w.Body.String())
		}
	}
}

// A stored secret never reaches the browser, so the dialog can only ever hand
// one back as the placeholder it was shown. That means "unchanged", and must
// not be stored as though it were the credential itself.
func TestProviderUpdateKeepsASecretHandedBackRedacted(t *testing.T) {
	c := newTestClient(t)
	c.setup("edit the accounts", 1)

	w, created := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind": "webdav",
		"name": "shelf",
		"options": map[string]string{
			"url":      webdavStub(t, "hunter2") + "/dav",
			"username": "sand",
			"password": "hunter2",
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("connect webdav: %d %s", w.Code, w.Body.String())
	}
	id := created["provider"].(map[string]any)["id"].(string)

	// Exactly what the dialog sends after somebody edits the username and
	// leaves the password field as it found it.
	w, _ = c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{
		"options": map[string]string{
			"username": "sand",
			"password": provider.RedactedSecret,
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH with a redacted secret: %d %s", w.Code, w.Body.String())
	}

	// The account still answers, which it would not if the placeholder had been
	// stored as the password.
	_, tested := c.json(http.MethodPost, "/api/providers/"+id+"/test", nil)
	if online, _ := tested["online"].(bool); !online {
		t.Errorf("the account stopped answering after an edit that changed nothing: %v", tested["error"])
	}

	// The stub is torn down at the end of this test, so let the background
	// manifest backup finish talking to it first.
	c.server.vault.AwaitBackupSync()
}
