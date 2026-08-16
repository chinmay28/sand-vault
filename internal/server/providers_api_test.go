package server

import (
	"net/http"
	"testing"
)

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
