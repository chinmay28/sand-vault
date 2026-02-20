package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sand-project/sand/internal/archive"
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

	fw, err := writer.CreateFormFile("file", filename)
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

	// Verify ZIP contains 3 media files
	zipReader, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}

	if len(zipReader.File) != 3 {
		t.Fatalf("expected 3 files in zip, got %d", len(zipReader.File))
	}
}

func TestArchiveEndpoint_MissingPassword(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fw, _ := writer.CreateFormFile("file", "test.txt")
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

// Suppress unused import warnings
var _ = io.Discard
