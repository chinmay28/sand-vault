package server

import (
	"archive/zip"
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
	// ParseMultipartForm also calls ParseForm internally, so FormValue works for
	// both multipart and url-encoded requests.  Ignore parse errors here and let
	// FormFile return MISSING_FILE if no file part was provided.
	r.ParseMultipartForm(100 << 20) //nolint:errcheck

	file, handler, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field", "MISSING_FILE")
		return
	}
	defer file.Close()

	password := r.FormValue("password")
	if password == "" {
		writeError(w, http.StatusBadRequest, "missing password", "MISSING_PASSWORD")
		return
	}

	// Create temp dirs
	tmpInput := filepath.Join(os.TempDir(), "sand-archive-input-*")
	inputDir, err := os.MkdirTemp("", "sand-archive-input-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir", "INTERNAL_ERROR")
		return
	}
	defer os.RemoveAll(inputDir)
	_ = tmpInput

	outputDir, err := os.MkdirTemp("", "sand-archive-output-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir", "INTERNAL_ERROR")
		return
	}
	defer os.RemoveAll(outputDir)

	// Write uploaded file to temp
	inputPath := filepath.Join(inputDir, handler.Filename)
	outFile, err := os.Create(inputPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp file", "INTERNAL_ERROR")
		return
	}
	if _, err := io.Copy(outFile, file); err != nil {
		outFile.Close()
		writeError(w, http.StatusInternalServerError, "failed to write temp file", "INTERNAL_ERROR")
		return
	}
	outFile.Close()

	// Run archive
	paths, err := archive.Archive(inputPath, password, outputDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("archive failed: %v", err), "ARCHIVE_ERROR")
		return
	}

	// Create ZIP response
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.sand.zip"`, handler.Filename))

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("failed to read media file: %v", err)
			return
		}

		zf, err := zipWriter.Create(filepath.Base(path))
		if err != nil {
			log.Printf("failed to create zip entry: %v", err)
			return
		}
		if _, err := zf.Write(data); err != nil {
			log.Printf("failed to write zip entry: %v", err)
			return
		}
	}
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
