package server

import (
	"net/http"
	"strings"
	"testing"
)

// The setting, over HTTP: the Edit dialog's checkbox is one PATCH field, and
// the bucket it is set on is the only kind of account that may carry it.

func TestAutoPruneIsPatchedLikeAnyOtherAccountSetting(t *testing.T) {
	c := newTestClient(t)
	_, ids := c.setupVersioned("the vault password", 3)

	w, body := c.json(http.MethodPatch, "/api/providers/"+ids[0], map[string]any{"auto_prune": true})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH auto_prune: %d %s", w.Code, w.Body.String())
	}
	if body["provider"].(map[string]any)["auto_prune"] != true {
		t.Errorf("the answer does not carry the setting: %v", body)
	}

	// It shows on the account from then on, and on the version scan's row.
	_, listed := c.json(http.MethodGet, "/api/providers", nil)
	for _, raw := range listed["providers"].([]any) {
		p := raw.(map[string]any)
		if p["id"] == ids[0] && p["auto_prune"] != true {
			t.Errorf("the listed account does not carry the setting: %v", p)
		}
		if p["id"] != ids[0] && p["auto_prune"] != nil {
			t.Errorf("an account never asked carries the setting: %v", p)
		}
	}
	scan := c.versionScan()
	for _, raw := range scan["accounts"].([]any) {
		if a := raw.(map[string]any); a["provider_id"] == ids[0] && a["auto_prune"] != true {
			t.Errorf("the scan row does not say the account is scheduled: %v", a)
		}
	}

	// A PATCH that leaves it out leaves it alone; one that says false clears it.
	_, body = c.json(http.MethodPatch, "/api/providers/"+ids[0], map[string]any{"name": "renamed"})
	if body["provider"].(map[string]any)["auto_prune"] != true {
		t.Errorf("a rename cleared the setting: %v", body)
	}
	_, body = c.json(http.MethodPatch, "/api/providers/"+ids[0], map[string]any{"auto_prune": false})
	if body["provider"].(map[string]any)["auto_prune"] != nil {
		t.Errorf("false did not clear the setting: %v", body)
	}
}

func TestAutoPruneIsRefusedOnAFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 1)
	_, listed := c.json(http.MethodGet, "/api/providers", nil)
	id := listed["providers"].([]any)[0].(map[string]any)["id"].(string)

	w, body := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"auto_prune": true})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("auto_prune on a folder: %d %s, want 400", w.Code, w.Body.String())
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "keeps no old versions") {
		t.Errorf("the refusal does not say why: %v", body)
	}
}
