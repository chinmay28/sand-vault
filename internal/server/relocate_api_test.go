package server

import (
	"net/http"
	"testing"
)

// number pulls a JSON number out of a decoded response body.
func number(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		t.Fatalf("response has no %q: %v", key, body)
	}
	value, ok := raw.(float64)
	if !ok {
		t.Fatalf("%q is %T, want a number", key, raw)
	}
	return int(value)
}

func TestRelocatePreviewSaysWhatWouldMoveAndMovesNothing(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	result := c.uploadTo("notes.txt", "/", []byte("a payload worth moving"), ids[:3])
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	file := result["file"].(map[string]any)
	id := file["id"].(string)

	// Two of the three it is on, plus one it is not: exactly one part travels.
	targets := []string{ids[0], ids[1], ids[3]}

	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id": id, "accounts": targets, "preview": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "moves"); got != 1 {
		t.Errorf("preview says %d parts move, want 1: %v", got, body)
	}
	if got := number(t, body, "total"); got != 1 {
		t.Errorf("preview covers %d files, want 1", got)
	}

	// A preview must not have touched the index.
	_, meta := c.json(http.MethodGet, "/api/files/"+id, nil)
	if !shardAccounts(t, meta["file"].(map[string]any))[ids[2]] {
		t.Error("the preview moved a part off the account it was on")
	}
}

func TestRelocateMovesAFileAndKeepsItReadable(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 4)
	ids := c.providerIDs()

	payload := []byte("carried across, never rebuilt")
	result := c.uploadTo("notes.txt", "/", payload, ids[:3])
	file := result["file"].(map[string]any)
	id := file["id"].(string)
	targets := []string{ids[0], ids[1], ids[3]}

	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id": id, "accounts": targets,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate: %d %s", w.Code, w.Body.String())
	}
	if got := number(t, body, "parts_moved"); got != 1 {
		t.Errorf("moved %d parts, want 1: %v", got, body)
	}
	if got := number(t, body, "relocated"); got != 1 {
		t.Errorf("relocated %d files, want 1", got)
	}

	_, meta := c.json(http.MethodGet, "/api/files/"+id, nil)
	landed := shardAccounts(t, meta["file"].(map[string]any))
	if landed[ids[2]] || !landed[ids[3]] {
		t.Errorf("parts are on %v, want %v", landed, targets)
	}

	content := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if content.Code != http.StatusOK {
		t.Fatalf("content after relocating: %d", content.Code)
	}
	if content.Body.String() != string(payload) {
		t.Errorf("content = %q, want %q", content.Body.String(), payload)
	}
}

func TestRelocateFolderByPath(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 6)
	ids := c.providerIDs()

	if w, _ := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/photos"}); w.Code != http.StatusCreated {
		t.Fatalf("mkdir: %d", w.Code)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if r := c.uploadTo(name, "/photos", []byte(name), ids[:3]); r["ok"] != true {
			t.Fatalf("upload %s: %v", name, r["error"])
		}
	}

	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"path": "/photos", "accounts": ids[3:],
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate: %d %s", w.Code, w.Body.String())
	}
	if folder, _ := body["folder"].(bool); !folder {
		t.Error("the report should say it moved a folder")
	}
	if got := number(t, body, "parts_moved"); got != 6 {
		t.Errorf("moved %d parts, want 6: %v", got, body)
	}

	_, listing := c.json(http.MethodGet, "/api/files?path=/photos", nil)
	want := map[string]bool{ids[3]: true, ids[4]: true, ids[5]: true}
	for _, raw := range listing["files"].([]any) {
		for id := range shardAccounts(t, raw.(map[string]any)) {
			if !want[id] {
				t.Errorf("a part is still on %s", id)
			}
		}
	}
}

func TestRelocateRejectsBadRequests(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	ids := c.providerIDs()
	c.uploadTo("x.txt", "/", []byte("x"), ids)

	cases := []struct {
		name    string
		payload map[string]any
		status  int
	}{
		{"nothing named", map[string]any{"accounts": ids}, http.StatusBadRequest},
		{"no such path", map[string]any{"path": "/nope.txt", "accounts": ids}, http.StatusNotFound},
		{"unknown account", map[string]any{"path": "/x.txt", "accounts": []string{ids[0], "ghost"}}, http.StatusBadRequest},
		{"one account", map[string]any{"path": "/x.txt", "accounts": ids[:1]}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		w, _ := c.json(http.MethodPost, "/api/relocate", tc.payload)
		if w.Code != tc.status {
			t.Errorf("%s: got %d, want %d (%s)", tc.name, w.Code, tc.status, w.Body.String())
		}
	}
}

func TestRelocateNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	ids := c.providerIDs()
	c.cookies = nil

	w, _ := c.json(http.MethodPost, "/api/relocate", map[string]any{"path": "/", "accounts": ids})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated relocate returned %d, want 401", w.Code)
	}
}
