package server

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// Repro for: "Cannot move file across clouds when it is in a subvault."
func TestRelocateAFileInASubVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 4)
	id := c.createSub("Taxes", apiSubPassword)
	file := c.uploadInto(id, "deed.pdf", "/", []byte("the deed"))

	// Move it onto three named accounts, the way the browser's dialog does.
	ids := c.providerIDs()

	// With the vault named, as the API expects.
	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id":       file["id"].(string),
		"accounts": ids[:3],
		"vault":    id,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate with vault named: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "relocated") + number(t, body, "unchanged"); got != 1 {
		t.Errorf("relocated+unchanged = %d, want 1: %v", got, body)
	}

	// Without the vault named: an entry ID is unique across every vault, so
	// it resolves like every other ID-addressed request.
	w, _ = c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id":       file["id"].(string),
		"accounts": ids[1:4],
	})
	if w.Code != http.StatusOK {
		t.Errorf("relocating a sub vault file by ID without naming the vault: %d %s", w.Code, w.Body.String())
	}
}

// A folder can exist in two vaults under one name, so relocating one goes by
// the vault the request names.
func TestRelocateAFolderInASubVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 4)
	id := c.createSub("Taxes", apiSubPassword)

	// The same folder name in both vaults, holding different files.
	c.upload("public.txt", "/", []byte("stays put"))
	w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/papers"})
	if w.Code != http.StatusCreated {
		t.Fatalf("mkdir in the main vault: %d", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/papers", "vault": id}); w.Code != http.StatusCreated {
		t.Fatalf("mkdir in the sub vault: %d", w.Code)
	}
	c.uploadInto(id, "deed.pdf", "/papers", []byte("the deed"))

	ids := c.providerIDs()
	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"path":     "/papers",
		"accounts": ids[:3],
		"vault":    id,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate a sub vault folder: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "total"); got != 1 {
		t.Errorf("the plan covered %d file(s), want the sub vault folder's 1: %v", got, body)
	}
}

// Repro for: "Cannot directly download something into a subvault using sftp."
func TestRemoteImportIntoASubVault(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	sub := c.createSub("Private", apiSubPassword)

	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "secret.txt"), "for the sub vault")

	id := c.connectRemote(req)["id"].(string)

	w, body := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"vault": sub,
		"paths": []string{"secret.txt"},
		"dest":  "/",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("import into sub vault: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "imported"); got != 1 {
		t.Fatalf("imported %d, want 1: %v", got, body)
	}

	if names := c.listNames(t, sub, "/"); len(names) != 1 || names[0] != "secret.txt" {
		t.Errorf("sub vault listing = %v, want secret.txt", names)
	}
	if names := c.listNames(t, "", "/"); len(names) != 0 {
		t.Errorf("main listing = %v, want empty", names)
	}
}

// The detached variant, a nested destination, and a locked sub vault.
func TestRemoteImportIntoASubVaultVariants(t *testing.T) {
	c := newTestClient(t)
	c.setup("a vault password", 3)
	sub := c.createSub("Private", apiSubPassword)

	req, root := remoteFixture(t, "vps")
	seedRemote(t, filepath.Join(root, "docs", "secret.txt"), "for the sub vault")

	id := c.connectRemote(req)["id"].(string)

	// Nested destination that does not exist yet in the sub vault.
	w, body := c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"vault": sub,
		"paths": []string{"docs"},
		"dest":  "/inbox/deep",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("import into nested sub vault dest: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "imported"); got != 1 {
		t.Fatalf("imported %d, want 1: %v", got, body)
	}

	// Detached into the sub vault.
	seedRemote(t, filepath.Join(root, "more.txt"), "more")
	w, body = c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"vault":  sub,
		"paths":  []string{"more.txt"},
		"dest":   "/",
		"detach": true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("detached import into sub vault: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, status := c.json(http.MethodGet, "/api/remote/"+id+"/import", nil)
		runs, _ := status["imports"].([]any)
		done := false
		for _, r := range runs {
			m := r.(map[string]any)
			if d, _ := m["done"].(bool); d {
				done = true
				if e, _ := m["error"].(string); e != "" {
					t.Fatalf("detached import failed: %s", e)
				}
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached import never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if names := c.listNames(t, sub, "/"); len(names) != 1 || names[0] != "more.txt" {
		t.Errorf("sub vault root = %v, want more.txt", names)
	}

	// Locked sub vault: a clear refusal, not a landing in the main vault.
	if w, _ := c.json(http.MethodPost, "/api/subvaults/"+sub+"/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	seedRemote(t, filepath.Join(root, "late.txt"), "late")
	w, body = c.json(http.MethodPost, "/api/remote/"+id+"/import", map[string]any{
		"vault": sub,
		"paths": []string{"late.txt"},
		"dest":  "/",
	})
	if w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Fatalf("import into a locked sub vault was allowed: %d %v", w.Code, body)
	}
	if names := c.listNames(t, "", "/"); len(names) != 0 {
		t.Errorf("main vault got %v after an import aimed at a locked sub vault", names)
	}
}
