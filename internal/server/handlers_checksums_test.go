package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func (c *testClient) checksumsStatus() map[string]any {
	c.t.Helper()
	w, body := c.json(http.MethodGet, "/api/checksums", nil)
	if w.Code != http.StatusOK {
		c.t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	return body
}

// A verify over HTTP, driven the way the dialog drives it: started slow, held,
// found held by a poll, sped up, let go, and read at the end.
func TestChecksumVerifyIsPacedPausedAndFinished(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/album"})
	for _, name := range []string{"a.bin", "b.bin"} {
		c.upload(name, "/album", bytes.Repeat([]byte(name), 40_000))
	}

	w, survey := c.json(http.MethodGet, "/api/checksums/survey?path=/album", nil)
	if w.Code != http.StatusOK || survey["with"].(float64) != 2 || survey["without"].(float64) != 0 {
		t.Fatalf("survey: %d %v", w.Code, survey)
	}
	if w, _ := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "audit", "path": "/album"}); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown kind: %d", w.Code)
	}
	if w, _ := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "verify", "path": "/album", "rate": -1}); w.Code != http.StatusBadRequest {
		t.Fatalf("a negative rate: %d", w.Code)
	}

	// 1 KB/s over 400 KB: it will not finish on its own inside this test.
	w, started := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "verify", "path": "/album", "rate": 1024})
	if w.Code != http.StatusAccepted || started["rate"].(float64) != 1024 {
		t.Fatalf("start: %d %v", w.Code, started)
	}
	// Asking again joins it; asking for something else is refused.
	if w, joined := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "verify", "path": "/album"}); w.Code != http.StatusOK || joined["id"] != started["id"] {
		t.Fatalf("join: %d %v", w.Code, joined)
	}
	if w, _ := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "compute", "path": "/album"}); w.Code != http.StatusConflict {
		t.Fatalf("a second job: %d", w.Code)
	}

	if _, paused := c.json(http.MethodPost, "/api/checksums/pause", nil); paused["paused"] != true || paused["running"] != true {
		t.Fatalf("pause: %v", paused)
	}
	if c.server.checksums.busy() {
		t.Error("a paused job counts as keeping the vault busy")
	}
	if _, faster := c.json(http.MethodPost, "/api/checksums/rate", map[string]any{"rate": 0}); faster["rate"].(float64) != 0 || faster["paused"] != true {
		t.Fatalf("rate: %v", faster)
	}
	time.Sleep(300 * time.Millisecond)
	if status := c.checksumsStatus(); status["running"] != true {
		t.Fatalf("a paused job finished: %v", status)
	}

	c.json(http.MethodPost, "/api/checksums/resume", nil)
	report, _ := c.await("/api/checksums")["result"].(map[string]any)
	if report == nil || report["kind"] != "verify" || report["matched"].(float64) != 2 || report["checked"].(float64) != 2 {
		t.Fatalf("report = %v", report)
	}
}

func TestChecksumStopKeepsWhatWasDoneEvenWhenPaused(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.upload("a.bin", "/", bytes.Repeat([]byte("x"), 200_000))

	c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "verify", "path": "/", "rate": 1024})
	c.json(http.MethodPost, "/api/checksums/pause", nil)
	c.json(http.MethodPost, "/api/checksums/stop", nil)
	report, _ := c.await("/api/checksums")["result"].(map[string]any)
	if report == nil || report["stopped"] != true || report["checked"].(float64) != 0 {
		t.Fatalf("report = %v", report)
	}
}

// A file with no checksum gets one from a compute, and the ledger says so.
func TestChecksumComputeAndTheLedgerOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	content := []byte(strings.Repeat("a file that lost its checksum ", 300))
	file := c.upload("lost.txt", "/", content)
	id := file["id"].(string)
	if err := c.server.vault.DropChecksum(id); err != nil {
		t.Fatal(err)
	}

	w, _ := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "compute", "path": "/"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	report, _ := c.await("/api/checksums")["result"].(map[string]any)
	if report == nil || report["computed"].(float64) != 1 {
		t.Fatalf("report = %v", report)
	}
	ledger, _ := report["ledger"].(map[string]any)
	if ledger == nil || ledger["written"] != true {
		t.Fatalf("ledger = %v", report)
	}

	// The ledger is a file in the vault like any other: fetched, gunzipped,
	// and it is a path and a checksum.
	got := c.do(http.MethodGet, "/api/files/"+ledger["id"].(string)+"/content", nil, "")
	zr, err := gzip.NewReader(got.Body)
	if err != nil {
		t.Fatalf("the ledger is not gzip: %v", err)
	}
	text, _ := io.ReadAll(zr)
	sum := sha256.Sum256(content)
	if want := "/lost.txt, " + hex.EncodeToString(sum[:]) + "\n"; string(text) != want {
		t.Fatalf("ledger = %q, want %q", text, want)
	}

	// Written again with nothing changed, it is the same file.
	w, again := c.json(http.MethodPost, "/api/checksums/ledger", nil)
	if w.Code != http.StatusOK || again["written"] == true || again["id"] != ledger["id"] {
		t.Fatalf("second write: %d %v", w.Code, again)
	}
}

// Opening a file that has no checksum, and reading all of it, records one.
// Reading part of it does not.
func TestOpeningAFileRecordsItsMissingChecksum(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	content := bytes.Repeat([]byte("opened and read to the end "), 2000)
	file := c.upload("opened.bin", "/", content)
	id := file["id"].(string)
	if err := c.server.vault.DropChecksum(id); err != nil {
		t.Fatal(err)
	}
	hash := func() string {
		e, err := c.server.vault.Entry(id)
		if err != nil {
			t.Fatal(err)
		}
		return e.Hash
	}

	req := func(rng string) int {
		r := httptest.NewRequest(http.MethodGet, "/api/files/"+id+"/content", nil)
		r.Host = "example.test"
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		for _, cookie := range c.cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		c.handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := req("bytes=100-5000"); code != http.StatusPartialContent {
		t.Fatalf("range read: %d", code)
	}
	if hash() != "" {
		t.Fatal("a partial read recorded a checksum")
	}
	if code := req(""); code != http.StatusOK {
		t.Fatalf("whole read: %d", code)
	}
	sum := sha256.Sum256(content)
	if got := hash(); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("recorded %q, want the file's SHA-256", got)
	}
}

// A job in a sub vault, over HTTP: it walks that vault and writes that vault's
// ledger, and locking the sub vault under it ends it as stopped with the
// reason rather than as a list of unreadable files.
func TestChecksumJobInASubVaultAndLockingItMidway(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	sub := c.createSub("Taxes", apiSubPassword)
	c.upload("open.bin", "/", bytes.Repeat([]byte("o"), 5_000))
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		c.uploadInto(sub, name, "/", bytes.Repeat([]byte(name), 30_000))
	}

	w, survey := c.json(http.MethodGet, "/api/checksums/survey?path=/&vault="+sub, nil)
	if w.Code != http.StatusOK || survey["files"].(float64) != 3 {
		t.Fatalf("survey of the sub vault: %d %v", w.Code, survey)
	}

	w, ledger := c.json(http.MethodPost, "/api/checksums/ledger?vault="+sub, nil)
	if w.Code != http.StatusOK || ledger["written"] != true || ledger["entries"].(float64) != 3 {
		t.Fatalf("the sub vault's ledger: %d %v", w.Code, ledger)
	}
	if _, main := c.json(http.MethodGet, "/api/checksums/survey?path=/", nil); main["ledger"].(map[string]any)["stored"] == true {
		t.Error("writing a sub vault's ledger wrote the main vault's")
	}

	// Slow enough to still be on its first file when the sub vault shuts.
	w, _ = c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "verify", "path": "/", "vault": sub, "rate": 4096})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	// Under way before the lock: a job that has not begun when its vault
	// shuts is refused outright, which is the next test's business.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		at, _ := c.checksumsStatus()["progress"].(map[string]any)
		if at != nil && at["bytes"].(float64) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never got going")
		}
	}
	if w, _ := c.json(http.MethodPost, "/api/subvaults/"+sub+"/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	c.json(http.MethodPost, "/api/checksums/rate", map[string]any{"rate": 0})

	status := c.await("/api/checksums")
	report, _ := status["result"].(map[string]any)
	if report == nil || report["stopped"] != true || report["stopped_by"] != "the sub vault was locked" {
		t.Fatalf("status = %v", status)
	}
	if report["unreadable"].(float64) != 0 || len(report["findings"].([]any)) != 0 {
		t.Fatalf("a shut sub vault was reported as damage: %v", report)
	}
}

// A job asked of a sub vault that is shut is refused, and the refusal is what
// the status carries — not an empty report.
func TestChecksumJobInAShutSubVaultSaysSo(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	sub := c.createSub("Taxes", apiSubPassword)
	c.uploadInto(sub, "a.bin", "/", []byte("in the sub vault"))
	c.json(http.MethodPost, "/api/subvaults/"+sub+"/lock", nil)

	// The folder cannot be looked up in a shut vault, so the start is refused.
	w, _ := c.json(http.MethodPost, "/api/checksums", map[string]any{"kind": "verify", "path": "/", "vault": sub})
	if w.Code == http.StatusAccepted {
		status := c.await("/api/checksums")
		if msg, _ := status["error"].(string); !strings.Contains(msg, "locked") || status["result"] != nil {
			t.Fatalf("status = %v", status)
		}
		return
	}
	if w.Code != http.StatusNotFound && w.Code != http.StatusUnauthorized {
		t.Fatalf("start in a shut sub vault: %d %s", w.Code, w.Body.String())
	}
}
