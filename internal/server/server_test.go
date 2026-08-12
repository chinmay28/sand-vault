package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// ---------------------------------------------------------------------------
// Health Check
// ---------------------------------------------------------------------------

func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected ok status, got %q", resp["status"])
	}
}

// ---------------------------------------------------------------------------
// Archive Endpoint
// ---------------------------------------------------------------------------

func createArchiveRequest(t *testing.T, filename string, content []byte, password string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	fw, err := writer.CreateFormFile("files[]", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)

	writer.WriteField("password", password)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/archive", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestArchiveEndpoint_Success(t *testing.T) {
	req := createArchiveRequest(t, "test.txt", []byte("hello world"), "password123")
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/zip" {
		t.Fatalf("expected application/zip, got %s", ct)
	}

	// Outer zip must contain exactly 3 inner zip files (media1.zip, media2.zip, media3.zip)
	outerZip, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read outer zip: %v", err)
	}
	if len(outerZip.File) != 3 {
		t.Fatalf("expected 3 entries in outer zip, got %d", len(outerZip.File))
	}

	// Each inner zip must contain exactly 1 media file
	for _, zf := range outerZip.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatalf("failed to open inner zip %s: %v", zf.Name, err)
		}
		innerData, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("failed to read inner zip %s: %v", zf.Name, err)
		}
		innerZip, err := zip.NewReader(bytes.NewReader(innerData), int64(len(innerData)))
		if err != nil {
			t.Fatalf("failed to parse inner zip %s: %v", zf.Name, err)
		}
		if len(innerZip.File) != 1 {
			t.Fatalf("inner zip %s: expected 1 file, got %d", zf.Name, len(innerZip.File))
		}
	}
}

func TestArchiveEndpoint_MultipleFiles(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for i, content := range [][]byte{[]byte("file one"), []byte("file two")} {
		fw, _ := writer.CreateFormFile("files[]", "test"+string(rune('1'+i))+".txt")
		fw.Write(content)
	}
	writer.WriteField("password", "pw")
	writer.Close()

	req := httptest.NewRequest("POST", "/api/archive", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outerZip, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read outer zip: %v", err)
	}
	if len(outerZip.File) != 3 {
		t.Fatalf("expected 3 inner zips, got %d", len(outerZip.File))
	}

	// Each inner zip should have 2 media files (one per input file)
	for _, zf := range outerZip.File {
		rc, _ := zf.Open()
		innerData, _ := io.ReadAll(rc)
		rc.Close()
		innerZip, err := zip.NewReader(bytes.NewReader(innerData), int64(len(innerData)))
		if err != nil {
			t.Fatalf("failed to parse inner zip %s: %v", zf.Name, err)
		}
		if len(innerZip.File) != 2 {
			t.Fatalf("inner zip %s: expected 2 files, got %d", zf.Name, len(innerZip.File))
		}
	}
}

func TestArchiveEndpoint_MissingPassword(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fw, _ := writer.CreateFormFile("files[]", "test.txt")
	fw.Write([]byte("data"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/archive", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestArchiveEndpoint_MissingFile(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writer.WriteField("password", "test")
	writer.Close()

	req := httptest.NewRequest("POST", "/api/archive", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Restore Endpoint
// ---------------------------------------------------------------------------

func TestRestoreEndpoint_Success(t *testing.T) {
	// First, create an archive to get media files
	original := []byte("content to archive and restore via API")

	inputDir := t.TempDir()
	outputDir := t.TempDir()
	inputPath := filepath.Join(inputDir, "api_test.txt")
	os.WriteFile(inputPath, original, 0644)

	paths, err := archive.Archive(inputPath, "apipass", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	// Build restore request with parts 1 and 2
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for _, path := range paths[:2] {
		data, _ := os.ReadFile(path)
		fw, _ := writer.CreateFormFile("parts[]", filepath.Base(path))
		fw.Write(data)
	}
	writer.WriteField("password", "apipass")
	writer.Close()

	req := httptest.NewRequest("POST", "/api/restore", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	handleRestore(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if !bytes.Equal(w.Body.Bytes(), original) {
		t.Fatal("restored content mismatch")
	}

	cd := w.Header().Get("Content-Disposition")
	if cd == "" {
		t.Fatal("missing Content-Disposition header")
	}
}

func TestRestoreEndpoint_WrongPassword(t *testing.T) {
	original := []byte("wrong password test")

	inputDir := t.TempDir()
	outputDir := t.TempDir()
	inputPath := filepath.Join(inputDir, "test.txt")
	os.WriteFile(inputPath, original, 0644)

	paths, _ := archive.Archive(inputPath, "correct", outputDir)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, path := range paths[:2] {
		data, _ := os.ReadFile(path)
		fw, _ := writer.CreateFormFile("parts[]", filepath.Base(path))
		fw.Write(data)
	}
	writer.WriteField("password", "wrong")
	writer.Close()

	req := httptest.NewRequest("POST", "/api/restore", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	handleRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var apiErr APIError
	json.NewDecoder(w.Body).Decode(&apiErr)
	if apiErr.Code != "WRONG_PASSWORD" {
		t.Fatalf("expected WRONG_PASSWORD code, got %q", apiErr.Code)
	}
}

func TestRestoreEndpoint_TooFewParts(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fw, _ := writer.CreateFormFile("parts[]", "single.media1")
	fw.Write([]byte("data"))
	writer.WriteField("password", "test")
	writer.Close()

	req := httptest.NewRequest("POST", "/api/restore", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	handleRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// Ensure restore works with parts from any 2-of-3 combination via API
func TestRestoreEndpoint_AllCombinations(t *testing.T) {
	original := []byte("testing all 2-of-3 combinations via API")

	inputDir := t.TempDir()
	outputDir := t.TempDir()
	inputPath := filepath.Join(inputDir, "combo.bin")
	os.WriteFile(inputPath, original, 0644)

	paths, _ := archive.Archive(inputPath, "pw", outputDir)

	combos := [][]int{{0, 1}, {0, 2}, {1, 2}}
	for _, combo := range combos {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		for _, idx := range combo {
			data, _ := os.ReadFile(paths[idx])
			fw, _ := writer.CreateFormFile("parts[]", filepath.Base(paths[idx]))
			fw.Write(data)
		}
		writer.WriteField("password", "pw")
		writer.Close()

		req := httptest.NewRequest("POST", "/api/restore", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		handleRestore(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("combo %v: expected 200, got %d: %s", combo, w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), original) {
			t.Fatalf("combo %v: content mismatch", combo)
		}
	}
}

func TestArchiveEndpoint_InnerZipNames(t *testing.T) {
	// Outer zip entries must be named exactly media1.zip, media2.zip, media3.zip.
	req := createArchiveRequest(t, "named.txt", []byte("naming test"), "pw")
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outer, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read outer zip: %v", err)
	}

	for i, zf := range outer.File {
		want := fmt.Sprintf("media%d.zip", i+1)
		if zf.Name != want {
			t.Errorf("entry %d: got name %q, want %q", i, zf.Name, want)
		}
	}
}

func TestArchiveEndpoint_OuterZipContentDisposition(t *testing.T) {
	req := createArchiveRequest(t, "any.bin", []byte("data"), "pw")
	w := httptest.NewRecorder()
	handleArchive(w, req)

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "sand-archives.zip") {
		t.Errorf("Content-Disposition should mention sand-archives.zip, got: %q", cd)
	}
}

func TestArchiveEndpoint_InnerZipContainsMediaFiles(t *testing.T) {
	// The media file inside each inner zip must end in .media1 / .media2 / .media3.
	req := createArchiveRequest(t, "check.txt", []byte("checking extensions"), "pw")
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	outer, _ := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	for i, zf := range outer.File {
		rc, _ := zf.Open()
		innerData, _ := io.ReadAll(rc)
		rc.Close()

		inner, err := zip.NewReader(bytes.NewReader(innerData), int64(len(innerData)))
		if err != nil {
			t.Fatalf("inner zip %s is not a valid zip: %v", zf.Name, err)
		}
		for _, entry := range inner.File {
			want := fmt.Sprintf(".media%d", i+1)
			if !strings.HasSuffix(entry.Name, want) {
				t.Errorf("inner zip %s: entry %q should end in %s", zf.Name, entry.Name, want)
			}
		}
	}
}

// Suppress unused import warnings
var _ = io.Discard
