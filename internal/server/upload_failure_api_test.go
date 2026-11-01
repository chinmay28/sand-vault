package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// A failed upload answers with where it failed, which cloud refused it and
// what to do next, so the browser has more to say than "upload failed".

// uploadResultFor posts one file and returns its result whatever the status,
// along with the status, for the uploads that are expected to fail.
func (c *testClient) uploadResultFor(name, dir string, content []byte) (int, map[string]any) {
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
	var resp struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		c.t.Fatalf("expected 1 upload result, got %d: %s", len(resp.Results), w.Body.String())
	}
	return w.Code, resp.Results[0]
}

func TestUploadFailureNamesTheCloudsAndSaysWhatNext(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("pw", 3)
	blockFolder(t, c.server.vault, roots[1])
	blockFolder(t, c.server.vault, roots[2])

	status, result := c.uploadResultFor("photo.jpg", "/", bytes.Repeat([]byte("x"), 4096))
	if status != http.StatusBadGateway {
		t.Errorf("status %d, want 502 when nothing was stored", status)
	}
	if ok, _ := result["ok"].(bool); ok {
		t.Fatal("an upload with one cloud of three answering should fail")
	}

	if got, _ := result["stage"].(string); got != "scattering" {
		t.Errorf("stage %q, want scattering", got)
	}
	parts, _ := result["parts"].(map[string]any)
	if parts["stored"] != 1.0 || parts["needed"] != 2.0 || parts["total"] != 3.0 {
		t.Errorf("parts %v, want 1 stored of 3, 2 needed", parts)
	}

	failed, _ := result["failed_parts"].([]any)
	if len(failed) != 2 {
		t.Fatalf("failed_parts %v, want one per blocked cloud", failed)
	}
	names := map[string]bool{}
	for _, raw := range failed {
		f := raw.(map[string]any)
		names[f["account"].(string)] = true
		if f["kind"] != "local" || f["account_id"] == "" || f["reason"] == "" || f["part"] == nil {
			t.Errorf("failed part is missing its account: %v", f)
		}
	}
	if !names["cloud1"] || !names["cloud2"] || names["cloud0"] {
		t.Errorf("failed parts name %v, want cloud1 and cloud2 only", names)
	}

	advice, _ := result["advice"].(string)
	for _, want := range []string{"Nothing of the file was kept", "cloud1", "cloud2", "upload the file again"} {
		if !strings.Contains(advice, want) {
			t.Errorf("advice %q does not say %q", advice, want)
		}
	}
	if msg, _ := result["error"].(string); !strings.Contains(msg, "stored only 1 of 3 shards") {
		t.Errorf("the one-line error %q changed", msg)
	}
}

func TestUploadRefusedBeforeSendingSaysSo(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	result := c.uploadTo("nowhere.txt", "/", []byte("payload"), []string{"not-an-account"})
	if ok, _ := result["ok"].(bool); ok {
		t.Fatal("expected an upload naming an unknown account to fail")
	}
	if got, _ := result["stage"].(string); got != "placing" {
		t.Errorf("stage %q, want placing", got)
	}
	if _, has := result["failed_parts"]; has {
		t.Errorf("nothing was sent, so no part can have failed: %v", result["failed_parts"])
	}
	if _, has := result["parts"]; has {
		t.Errorf("nothing was sent, so there is no count of parts: %v", result["parts"])
	}
	if advice, _ := result["advice"].(string); !strings.Contains(advice, "Nothing was sent") {
		t.Errorf("advice %q should say nothing went", advice)
	}
}

func TestUploadThatSucceedsCarriesNoFailureFields(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	status, result := c.uploadResultFor("fine.txt", "/", []byte("payload"))
	if status != http.StatusCreated {
		t.Fatalf("status %d, want 201", status)
	}
	for _, key := range []string{"stage", "parts", "failed_parts", "advice", "error"} {
		if _, has := result[key]; has {
			t.Errorf("a stored file carries %q: %v", key, result[key])
		}
	}
}
