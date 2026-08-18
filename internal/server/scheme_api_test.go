package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// Naming the code a file is cut with, over the wire. The scheme travels as the
// string a person would write — "3-of-5" — on the upload form and in a
// relocation's JSON, and empty means the count of accounts settles it as it
// always did.

// uploadCut posts one file, naming both the accounts and the scheme.
func (c *testClient) uploadCut(name, dir string, content []byte, accounts []string, scheme string) map[string]any {
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
	if scheme != "" {
		mw.WriteField("scheme", scheme)
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

func TestUploadCutsToTheSchemeItNames(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()

	result := c.uploadCut("named.txt", "/", []byte("a payload"), ids, "3-of-5")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}

	file := result["file"].(map[string]any)
	if got := file["data_shards"].(float64); got != 3 {
		t.Errorf("data_shards = %v, want 3", got)
	}
	if got := file["total_shards"].(float64); got != 5 {
		t.Errorf("total_shards = %v, want 5", got)
	}
	if landed := shardAccounts(t, file); len(landed) != 5 {
		t.Errorf("parts landed on %d accounts, want 5", len(landed))
	}
}

func TestUploadWithNoSchemeStillTakesItFromTheCount(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 6)
	ids := c.providerIDs()

	result := c.uploadCut("derived.txt", "/", []byte("a payload"), ids, "")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	file := result["file"].(map[string]any)
	if got := file["data_shards"].(float64); got != 4 {
		t.Errorf("data_shards = %v, want 4 — six clouds is 4-of-6", got)
	}
}

func TestUploadRejectsASchemeItCannotWrite(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()

	for _, tc := range []struct{ scheme, want string }{
		{"1-of-5", "at least 2 shards"},
		{"6-of-5", "more shards to rebuild than it makes"},
		{"3-of-4", "would hold nothing"},
		{"three-of-five", "not a number of shards"},
		{"nonsense", "write one as k-of-n"},
	} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("files[]", "refused.txt")
		part.Write([]byte("x"))
		mw.WriteField("path", "/")
		for _, id := range ids {
			mw.WriteField("accounts", id)
		}
		mw.WriteField("scheme", tc.scheme)
		mw.Close()

		w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())

		// A scheme that cannot be parsed at all is refused for the whole
		// request; one that parses but cannot be written fails the file.
		body := w.Body.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("uploading as %q answered %q, want it to mention %q", tc.scheme, body, tc.want)
		}
	}
}

func TestRelocateRecodesToTheSchemeItNames(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()

	result := c.uploadTo("moving.txt", "/", []byte("a payload worth recoding"), ids[:3])
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	id := result["file"].(map[string]any)["id"].(string)

	// Preview first: five clouds names no scheme on its own, so the request
	// naming one is the only thing making this move possible.
	w, body := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id": id, "accounts": ids, "scheme": "3-of-5", "preview": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if got := body["recoded"].(float64); got != 1 {
		t.Fatalf("preview planned %v recodes, want 1", got)
	}

	w, body = c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id": id, "accounts": ids, "scheme": "3-of-5",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relocate: %d %s", w.Code, w.Body.String())
	}
	if got := body["recoded"].(float64); got != 1 {
		t.Fatalf("recoded %v files, want 1", got)
	}

	w, listed := c.json(http.MethodGet, "/api/files/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get file: %d %s", w.Code, w.Body.String())
	}
	file := listed["file"].(map[string]any)
	if got := file["data_shards"].(float64); got != 3 {
		t.Errorf("data_shards = %v, want 3", got)
	}
	if got := file["total_shards"].(float64); got != 5 {
		t.Errorf("total_shards = %v, want 5", got)
	}
}

func TestRelocateRejectsASchemeItCannotWrite(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()

	result := c.uploadTo("stuck.txt", "/", []byte("x"), ids[:3])
	id := result["file"].(map[string]any)["id"].(string)

	w, _ := c.json(http.MethodPost, "/api/relocate", map[string]any{
		"id": id, "accounts": ids, "scheme": "1-of-5", "preview": true,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("relocate to 1-of-5: %d %s, want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "at least 2 shards") {
		t.Errorf("error should say why, got %s", w.Body.String())
	}
}

// The default scheme, over the wire. Accounts and code go to /api/vault/defaults
// as one object, and come back on the status the frontend polls.

func TestVaultDefaultsCarryASchemeBothWays(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()

	w, body := c.json(http.MethodPost, "/api/vault/defaults", map[string]any{
		"accounts": ids, "scheme": "3-of-5",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("set defaults: %d %s", w.Code, w.Body.String())
	}
	if got := body["default_scheme"]; got != "3-of-5" {
		t.Fatalf("answered with default_scheme %v, want 3-of-5", got)
	}

	// And the status the browser polls says the same thing.
	w, status := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	stats := status["stats"].(map[string]any)
	if got := stats["default_scheme"]; got != "3-of-5" {
		t.Fatalf("status reports default_scheme %v, want 3-of-5", got)
	}

	// An upload that chooses nothing is cut by it.
	result := c.uploadCut("defaulted.txt", "/", []byte("a payload"), nil, "")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	file := result["file"].(map[string]any)
	if got := file["data_shards"].(float64); got != 3 {
		t.Errorf("data_shards = %v, want 3", got)
	}
	if got := file["total_shards"].(float64); got != 5 {
		t.Errorf("total_shards = %v, want 5", got)
	}
}

func TestVaultDefaultsRejectASchemeTheAccountsCannotHold(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 5)
	ids := c.providerIDs()

	w, _ := c.json(http.MethodPost, "/api/vault/defaults", map[string]any{
		"accounts": ids[:3], "scheme": "3-of-5",
	})
	if w.Code == http.StatusOK {
		t.Fatalf("3-of-5 over three accounts was accepted: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pick 5") {
		t.Errorf("error should say what to do, got %s", w.Body.String())
	}
}

func TestClearingTheDefaultSchemeGoesBackToTheCount(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 6)
	ids := c.providerIDs()

	if w, _ := c.json(http.MethodPost, "/api/vault/defaults", map[string]any{
		"accounts": ids[:5], "scheme": "2-of-5",
	}); w.Code != http.StatusOK {
		t.Fatalf("set defaults: %d %s", w.Code, w.Body.String())
	}

	// Omitting the scheme clears it, which is why the two travel as one object.
	w, body := c.json(http.MethodPost, "/api/vault/defaults", map[string]any{"accounts": ids})
	if w.Code != http.StatusOK {
		t.Fatalf("clear scheme: %d %s", w.Code, w.Body.String())
	}
	if got := body["default_scheme"]; got != "" {
		t.Fatalf("default_scheme = %v, want it cleared", got)
	}

	result := c.uploadCut("counted.txt", "/", []byte("a payload"), nil, "")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("upload failed: %v", result["error"])
	}
	if got := result["file"].(map[string]any)["data_shards"].(float64); got != 4 {
		t.Errorf("data_shards = %v, want 4 — six accounts is 4-of-6 again", got)
	}
}
