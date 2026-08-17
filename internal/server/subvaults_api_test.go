package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

const apiSubPassword = "the sub vault's own password"

// createSub makes a sub vault over the API and returns its id.
func (c *testClient) createSub(label, password string) string {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/subvaults", map[string]any{
		"label":    label,
		"password": password,
	})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("create sub vault: %d %v", w.Code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		c.t.Fatalf("create sub vault returned no id: %v", body)
	}
	return id
}

// uploadInto posts one file into a folder of a named vault.
func (c *testClient) uploadInto(vaultID, name, dir string, content []byte) map[string]any {
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

	w := c.do(http.MethodPost, "/api/files?vault="+vaultID, &buf, mw.FormDataContentType())
	if w.Code != http.StatusCreated {
		c.t.Fatalf("upload %s into %s: %d %s", name, vaultID, w.Code, w.Body.String())
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

// listNames returns the file names a listing reports for a vault.
func (c *testClient) listNames(t *testing.T, vaultID, path string) []string {
	t.Helper()

	url := "/api/files?path=" + path
	if vaultID != "" {
		url += "&vault=" + vaultID
	}
	w, body := c.json(http.MethodGet, url, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list %s: %d %v", url, w.Code, body)
	}

	files, _ := body["files"].([]any)
	names := make([]string, 0, len(files))
	for _, f := range files {
		if m, ok := f.(map[string]any); ok {
			names = append(names, m["name"].(string))
		}
	}
	return names
}

func TestSubVaultListingIsSeparateFromTheMainVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	id := c.createSub("Taxes", apiSubPassword)

	c.upload("public.txt", "/", []byte("nothing secret"))
	c.uploadInto(id, "private.txt", "/", []byte("very secret"))

	if names := c.listNames(t, "", "/"); len(names) != 1 || names[0] != "public.txt" {
		t.Errorf("main listing = %v, want just public.txt", names)
	}
	if names := c.listNames(t, id, "/"); len(names) != 1 || names[0] != "private.txt" {
		t.Errorf("sub vault listing = %v, want just private.txt", names)
	}
}

func TestLockedSubVaultAnswersWithItsOwnCode(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	id := c.createSub("Taxes", apiSubPassword)
	c.uploadInto(id, "private.txt", "/", []byte("very secret"))

	if w, body := c.json(http.MethodPost, "/api/subvaults/"+id+"/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock sub vault: %d %v", w.Code, body)
	}

	// SUB_VAULT_LOCKED rather than LOCKED: the vault is open, and the browser
	// should ask for one more password rather than throw the session away.
	w, body := c.json(http.MethodGet, "/api/files?path=/&vault="+id, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("listing a locked sub vault = %d, want 401: %v", w.Code, body)
	}
	if code, _ := body["code"].(string); code != "SUB_VAULT_LOCKED" {
		t.Errorf("code = %q, want SUB_VAULT_LOCKED", code)
	}

	// The main vault is unaffected.
	if w, _ := c.json(http.MethodGet, "/api/files?path=/", nil); w.Code != http.StatusOK {
		t.Errorf("the main vault answered %d after a sub vault was locked", w.Code)
	}

	// The wrong password does not open it.
	w, body = c.json(http.MethodPost, "/api/subvaults/"+id+"/unlock", map[string]any{
		"password": "a vault password",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unlock with the main password = %d, want 401: %v", w.Code, body)
	}

	// Its own does.
	if w, body := c.json(http.MethodPost, "/api/subvaults/"+id+"/unlock", map[string]any{
		"password": apiSubPassword,
	}); w.Code != http.StatusOK {
		t.Fatalf("unlock sub vault: %d %v", w.Code, body)
	}
	if names := c.listNames(t, id, "/"); len(names) != 1 {
		t.Errorf("sub vault listing after unlocking = %v, want one file", names)
	}
}

func TestVaultStatusNamesTheSubVaults(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	c.createSub("Taxes", apiSubPassword)

	w, body := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("vault status: %d %v", w.Code, body)
	}
	subs, _ := body["sub_vaults"].([]any)
	if len(subs) != 1 {
		t.Fatalf("sub_vaults = %v, want one", body["sub_vaults"])
	}
	first := subs[0].(map[string]any)
	if first["label"] != "Taxes" {
		t.Errorf("label = %v, want Taxes", first["label"])
	}
	if unlocked, _ := first["unlocked"].(bool); !unlocked {
		t.Error("a freshly created sub vault should be reported as open")
	}
}

func TestLockScreenSaysNothingAboutSubVaults(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	c.createSub("Taxes", apiSubPassword)

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	c.cookies = nil

	w, body := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("vault status: %d %v", w.Code, body)
	}
	if _, present := body["sub_vaults"]; present {
		t.Errorf("the lock screen was told about the sub vaults: %v", body["sub_vaults"])
	}
	if !strings.Contains(w.Body.String(), `"unlocked":false`) {
		t.Errorf("expected a locked status, got %s", w.Body.String())
	}
}

func TestAssignOverTheAPIMovesAFileBetweenVaults(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	id := c.createSub("Taxes", apiSubPassword)

	file := c.upload("deed.pdf", "/", []byte("the deed"))

	w, body := c.json(http.MethodPost, "/api/assign", map[string]any{
		"target":  "/deed.pdf",
		"to":      id,
		"migrate": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("assign: %d %v", w.Code, body)
	}
	if files, _ := body["files"].(float64); files != 1 {
		t.Errorf("files = %v, want 1", body["files"])
	}

	if names := c.listNames(t, "", "/"); len(names) != 0 {
		t.Errorf("main listing = %v, want empty", names)
	}
	if names := c.listNames(t, id, "/"); len(names) != 1 || names[0] != "deed.pdf" {
		t.Errorf("sub vault listing = %v, want deed.pdf", names)
	}

	// The file reads by ID without the caller ever naming a vault, which is the
	// point of leaving the ID-addressed endpoints alone.
	w = c.do(http.MethodGet, "/api/files/"+file["id"].(string)+"/content", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("content after assignment: %d %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "the deed" {
		t.Errorf("content = %q, want %q", w.Body.String(), "the deed")
	}
}

func TestDeletingALockedSubVaultNeedsForce(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	id := c.createSub("Taxes", apiSubPassword)
	c.uploadInto(id, "private.txt", "/", []byte("very secret"))

	if w, _ := c.json(http.MethodPost, "/api/subvaults/"+id+"/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock sub vault: %d", w.Code)
	}

	if w, _ := c.json(http.MethodDelete, "/api/subvaults/"+id, nil); w.Code == http.StatusOK {
		t.Fatal("deleting a locked sub vault should be refused without force")
	}
	if w, body := c.json(http.MethodDelete, "/api/subvaults/"+id+"?force=1", nil); w.Code != http.StatusOK {
		t.Fatalf("forced delete: %d %v", w.Code, body)
	}

	w, body := c.json(http.MethodGet, "/api/subvaults", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list sub vaults: %d %v", w.Code, body)
	}
	if subs, _ := body["sub_vaults"].([]any); len(subs) != 0 {
		t.Errorf("sub_vaults = %v, want none left", body["sub_vaults"])
	}
}

func TestSubVaultEndpointsNeedASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	id := c.createSub("Taxes", apiSubPassword)
	c.cookies = nil

	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/subvaults", nil},
		{http.MethodPost, "/api/subvaults", map[string]any{"label": "New", "password": "x"}},
		{http.MethodPost, "/api/subvaults/" + id + "/unlock", map[string]any{"password": apiSubPassword}},
		{http.MethodPost, "/api/subvaults/" + id + "/lock", nil},
		{http.MethodDelete, "/api/subvaults/" + id, nil},
		{http.MethodPost, "/api/assign", map[string]any{"target": "/x", "to": id}},
		{http.MethodPost, "/api/vaults/import", map[string]any{"provider": "x"}},
	} {
		w, _ := c.json(call.method, call.path, call.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", call.method, call.path, w.Code)
		}
	}
}

// Which accounts hold a vault that is not this one is the recovery scan's
// answer, and importing is what this branch adds on top of it.
func TestRecoveryScanReportsOurOwnBackupAsOurs(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	c.upload("public.txt", "/", []byte("nothing secret"))
	c.server.vault.AwaitBackupSync()

	w, body := c.json(http.MethodGet, "/api/vault/recovery", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("recovery scan: %d %v", w.Code, body)
	}
	sources, _ := body["sources"].([]any)
	if len(sources) == 0 {
		t.Fatal("expected this vault's own accounts to be listed")
	}
	for _, src := range sources {
		m := src.(map[string]any)
		if backup, _ := m["backup"].(bool); !backup {
			continue
		}
		if foreign, _ := m["foreign"].(bool); foreign {
			t.Errorf("this vault's own backup was reported as another vault's: %v", m)
		}
	}
}
