package server

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sand-project/sand/internal/archive"
)

//go:embed all:dist
var webAssets embed.FS

// Server holds the HTTP server configuration.
type Server struct {
	Bind string
	Port int
}

// APIError is a JSON error response.
type APIError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Error: msg, Code: code})
}

// Start initializes routes and starts the HTTP server.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("POST /api/archive", handleArchive)
	mux.HandleFunc("POST /api/restore", handleRestore)
	mux.HandleFunc("GET /api/health", handleHealth)

	// Serve embedded React frontend
	distFS, err := fs.Sub(webAssets, "dist")
	if err != nil {
		return fmt.Errorf("failed to get embedded filesystem: %w", err)
	}
	fileServer := http.FileServer(http.FS(distFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// For SPA: serve index.html for non-file routes
		path := r.URL.Path
		if path != "/" && !strings.Contains(path, ".") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})

	addr := fmt.Sprintf("%s:%d", s.Bind, s.Port)
	log.Printf("SAND server starting on http://%s", addr)
	return http.ListenAndServe(addr, mux)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "1.0.0"})
}

func handleArchive(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(500 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse form", "PARSE_ERROR")
		return
	}

	password := r.FormValue("password")
	if password == "" {
		writeError(w, http.StatusBadRequest, "missing password", "MISSING_PASSWORD")
		return
	}

	// Accept files[] (one or more files)
	uploadedFiles := r.MultipartForm.File["files[]"]
	if len(uploadedFiles) == 0 {
		writeError(w, http.StatusBadRequest, "missing files[] field", "MISSING_FILE")
		return
	}

	inputDir, err := os.MkdirTemp("", "sand-archive-input-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir", "INTERNAL_ERROR")
		return
	}
	defer os.RemoveAll(inputDir)

	outputDir, err := os.MkdirTemp("", "sand-archive-output-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir", "INTERNAL_ERROR")
		return
	}
	defer os.RemoveAll(outputDir)

	// Save each uploaded file to the temp input dir
	var inputPaths []string
	for _, fh := range uploadedFiles {
		f, err := fh.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read uploaded file", "READ_ERROR")
			return
		}
		inputPath := filepath.Join(inputDir, fh.Filename)
		out, err := os.Create(inputPath)
		if err != nil {
			f.Close()
			writeError(w, http.StatusInternalServerError, "failed to create temp file", "INTERNAL_ERROR")
			return
		}
		_, copyErr := io.Copy(out, f)
		f.Close()
		out.Close()
		if copyErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to write temp file", "INTERNAL_ERROR")
			return
		}
		inputPaths = append(inputPaths, inputPath)
	}

	// Archive all files; partPaths[i] holds the paths for part i+1 across all files.
	partPaths, err := archive.ArchiveMultiple(inputPaths, password, outputDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("archive failed: %v", err), "ARCHIVE_ERROR")
		return
	}

	// Build media1.zip, media2.zip, media3.zip in memory.
	innerZips := make([][]byte, 3)
	for i := 0; i < 3; i++ {
		data, err := buildZipBytes(partPaths[i])
		if err != nil {
			log.Printf("failed to build media%d.zip: %v", i+1, err)
			writeError(w, http.StatusInternalServerError, "failed to build zip", "INTERNAL_ERROR")
			return
		}
		innerZips[i] = data
	}

	// Stream an outer zip to the client containing the 3 inner zips.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="sand-archives.zip"`)

	outer := zip.NewWriter(w)
	defer outer.Close()

	for i, data := range innerZips {
		name := fmt.Sprintf("media%d.zip", i+1)
		entry, err := outer.Create(name)
		if err != nil {
			log.Printf("failed to create outer zip entry %s: %v", name, err)
			return
		}
		if _, err := entry.Write(data); err != nil {
			log.Printf("failed to write outer zip entry %s: %v", name, err)
			return
		}
	}
}

// buildZipBytes creates an in-memory zip archive containing the given files.
func buildZipBytes(filePaths []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, fp := range filePaths {
		data, err := os.ReadFile(fp)
		if err != nil {
			zw.Close()
			return nil, err
		}
		entry, err := zw.Create(filepath.Base(fp))
		if err != nil {
			zw.Close()
			return nil, err
		}
		if _, err := entry.Write(data); err != nil {
			zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func handleRestore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse form", "PARSE_ERROR")
		return
	}

	password := r.FormValue("password")
	if password == "" {
		writeError(w, http.StatusBadRequest, "missing password", "MISSING_PASSWORD")
		return
	}

	partsDir, err := os.MkdirTemp("", "sand-restore-parts-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir", "INTERNAL_ERROR")
		return
	}
	defer os.RemoveAll(partsDir)

	outputDir, err := os.MkdirTemp("", "sand-restore-output-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir", "INTERNAL_ERROR")
		return
	}
	defer os.RemoveAll(outputDir)

	// Read uploaded part files
	files := r.MultipartForm.File["parts[]"]
	if len(files) < 2 || len(files) > 3 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("need 2 or 3 part files, got %d", len(files)), "INVALID_PARTS")
		return
	}

	var partPaths []string
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read part file", "READ_ERROR")
			return
		}

		partPath := filepath.Join(partsDir, fh.Filename)
		out, err := os.Create(partPath)
		if err != nil {
			f.Close()
			writeError(w, http.StatusInternalServerError, "failed to create temp file", "INTERNAL_ERROR")
			return
		}
		io.Copy(out, f)
		f.Close()
		out.Close()
		partPaths = append(partPaths, partPath)
	}

	// Run restore
	outputPath, err := archive.Restore(partPaths, password, outputDir)
	if err != nil {
		code := "RESTORE_ERROR"
		if strings.Contains(err.Error(), "wrong password") || strings.Contains(err.Error(), "decryption failed") {
			code = "WRONG_PASSWORD"
		} else if strings.Contains(err.Error(), "mismatch") {
			code = "MISMATCHED_PARTS"
		} else if strings.Contains(err.Error(), "integrity") {
			code = "CORRUPT_FILE"
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("restore failed: %v", err), code)
		return
	}

	// Stream the restored file back
	data, err := os.ReadFile(outputPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read restored file", "INTERNAL_ERROR")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(outputPath)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}
