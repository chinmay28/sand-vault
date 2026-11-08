package server

import (
	"net/http"
	"strings"
	"testing"
)

// How much has moved to and from each cloud, over the wire.

func transfersBoard(t *testing.T, c *testClient, window string) map[string]any {
	t.Helper()

	path := "/api/transfers"
	if window != "" {
		path += "?window=" + window
	}
	w, body := c.json(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("transfer stats: %d %s", w.Code, w.Body.String())
	}
	board, ok := body["transfers"].(map[string]any)
	if !ok {
		t.Fatalf("no transfers in %v", body)
	}
	return board
}

func TestTransferStatsReportWhatWentInAndCameOut(t *testing.T) {
	c := newTestClient(t)
	c.setup("what moved", 3)

	id := c.upload("notes.txt", "/", []byte(strings.Repeat("something to send and read back ", 200)))["id"].(string)
	afterUpload := transfersBoard(t, c, "")
	if afterUpload["bytes_in"].(float64) == 0 {
		t.Fatalf("nothing went in for an upload: %v", afterUpload)
	}
	if got := len(afterUpload["accounts"].([]any)); got != 3 {
		t.Errorf("accounts = %d, want the 3 connected", got)
	}
	for _, raw := range afterUpload["accounts"].([]any) {
		a := raw.(map[string]any)
		if a["name"] == nil || a["provider_id"] == nil {
			t.Errorf("an account with no name on the board: %v", a)
		}
		if a["bytes_in"].(float64) == 0 {
			t.Errorf("%v took a part and is credited with nothing in", a["name"])
		}
	}

	outBefore := afterUpload["bytes_out"].(float64)
	if w := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	if got := transfersBoard(t, c, "all")["bytes_out"].(float64); got <= outBefore {
		t.Errorf("bytes out = %v after a download, was %v", got, outBefore)
	}

	if w, _ := c.json(http.MethodGet, "/api/transfers?window=fortnight", nil); w.Code != http.StatusBadRequest {
		t.Errorf("GET /api/transfers?window=fortnight: %d, want 400", w.Code)
	}

	// One history, one button: forgetting the reads forgets the transfers.
	// Not necessarily to zero — the index backup the upload set off is pushed
	// in the background and may land after — but the upload itself is gone.
	inBefore := transfersBoard(t, c, "all")["bytes_in"].(float64)
	if w, _ := c.json(http.MethodPost, "/api/reads/forget?window=all", nil); w.Code != http.StatusOK {
		t.Fatalf("forget: %d", w.Code)
	}
	if got := transfersBoard(t, c, "all")["bytes_in"].(float64); got >= inBefore {
		t.Errorf("bytes in = %v after forgetting, was %v", got, inBefore)
	}
}

func TestTransferStatsNeedASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("locked out", 2)
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if w, _ := c.json(http.MethodGet, "/api/transfers", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/transfers on a locked vault: %d, want 401", w.Code)
	}
}

func TestThroughputReportsSpeedsInMbps(t *testing.T) {
	c := newTestClient(t)
	c.setup("how fast", 3)

	id := c.upload("notes.txt", "/", []byte(strings.Repeat("read me back quickly ", 500)))["id"].(string)
	if w := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}

	w, body := c.json(http.MethodGet, "/api/throughput?window=today", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("throughput: %d %s", w.Code, w.Body.String())
	}
	report := body["throughput"].(map[string]any)
	if open := report["open"].(map[string]any); open["mbps"].(float64) <= 0 {
		t.Errorf("no opening speed after a download: %v", open)
	}
	kinds := map[string]map[string]any{}
	for _, raw := range report["kinds"].([]any) {
		k := raw.(map[string]any)
		kinds[k["kind"].(string)] = k
	}
	for _, kind := range []string{"images", "video", "documents"} {
		if kinds[kind] == nil {
			t.Errorf("%s missing from the kinds", kind)
		}
	}
	// A .txt is a document.
	if docs := kinds["documents"]; docs == nil || docs["open"].(map[string]any)["mbps"].(float64) <= 0 {
		t.Errorf("a .txt opened and documents have no speed: %v", docs)
	}
	if got := len(report["accounts"].([]any)); got != 3 {
		t.Errorf("accounts = %d, want 3", got)
	}

	if w, _ := c.json(http.MethodGet, "/api/throughput?window=fortnight", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad window: %d, want 400", w.Code)
	}
}
