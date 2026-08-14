package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// contextWithTimeout derives a request-scoped context so a hung provider
// cannot pin a handler open forever.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}

	v, _ := s.Vault()
	listing, err := v.List(path)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

// handleSearch answers a query against the file index. Only the vault can:
// names and folder structure are encrypted everywhere else, so no account can
// be asked what it is holding.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a whole number", "BAD_REQUEST")
			return
		}
		limit = parsed
	}

	v, _ := s.Vault()
	results, err := v.Search(vault.SearchOptions{
		Query: query.Get("q"),
		Dir:   query.Get("path"),
		Kind:  vault.SearchKind(query.Get("type")),
		Limit: limit,
	})
	if err != nil {
		if errors.Is(err, vault.ErrEmptyQuery) {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
			return
		}
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handleFileMeta(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	entry, err := v.Entry(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": entry, "path": entry.Path()})
}

// uploadResult reports the outcome of one file in a multi-file upload. A
// partial failure is normal when several files are dropped at once, so each
// one carries its own status rather than failing the whole request.
type uploadResult struct {
	Name     string       `json:"name"`
	OK       bool         `json:"ok"`
	Error    string       `json:"error,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
	File     *vault.Entry `json:"file,omitempty"`
}

func (s *Server) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	maxUpload := s.MaxUploadSize
	if maxUpload <= 0 {
		maxUpload = DefaultMaxUploadSize
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	// Keep only a modest amount in RAM; the rest spills to a temp file that
	// ParseMultipartForm cleans up when the request ends.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("could not read the upload: %v", err), "PARSE_ERROR")
		return
	}
	defer r.MultipartForm.RemoveAll()

	dir := r.FormValue("path")
	if dir == "" {
		dir = "/"
	}
	overwrite := r.FormValue("overwrite") == "true" || r.FormValue("overwrite") == "1"

	uploads := r.MultipartForm.File["files[]"]
	if len(uploads) == 0 {
		writeError(w, http.StatusBadRequest, "missing files[] field", "MISSING_FILE")
		return
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	results := make([]uploadResult, 0, len(uploads))
	stored := 0

	for _, fh := range uploads {
		result := uploadResult{Name: fh.Filename}

		f, err := fh.Open()
		if err != nil {
			result.Error = "could not read the uploaded file"
			results = append(results, result)
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			result.Error = "could not read the uploaded file"
			results = append(results, result)
			continue
		}

		entry, warnings, err := v.Upload(ctx, dir, fh.Filename, data, overwrite)
		result.Warnings = warnings
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}

		result.OK = true
		result.File = entry
		results = append(results, result)
		stored++
	}

	status := http.StatusCreated
	if stored == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"results": results, "stored": stored})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	warnings, err := v.Delete(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "warnings": warnings})
}

type moveRequest struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

func (s *Server) handleFileMove(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	entry, err := v.Move(r.PathValue("id"), req.Dir, req.Name)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": entry, "path": entry.Path()})
}

func (s *Server) handleFileHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	health, err := v.Health(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// handleFileContent gathers the file's parts from the connected accounts,
// rebuilds the plaintext, and serves it. With ?download=1 the browser saves it;
// otherwise it renders inline, which is what makes previewing possible.
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	data, entry, err := v.Fetch(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	download := r.URL.Query().Get("download") == "1" || r.URL.Query().Get("download") == "true"
	disposition := "inline"
	if download {
		disposition = "attachment"
	}

	contentType := entry.MIME
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// Anything that could execute in the page's origin is served as a
	// download instead: a stored HTML or SVG file must never run as if it
	// were part of the SAND app.
	if !download && isRiskyInline(contentType) {
		disposition = "attachment"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType(disposition, map[string]string{"filename": entry.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// The content is reconstructed from encrypted parts; never let a shared
	// cache hold on to the plaintext.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", `"`+entry.Hash+`"`)

	// ServeContent handles range requests, which is what lets audio and video
	// seek instead of buffering the whole file first.
	http.ServeContent(w, r, entry.Name, entry.ModifiedAt, bytes.NewReader(data))
}

// isRiskyInline reports content types that must not be rendered inline in the
// app's own origin.
func isRiskyInline(contentType string) bool {
	base, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	}
	switch base {
	case "text/html", "application/xhtml+xml", "image/svg+xml",
		"application/xml", "text/xml", "application/javascript", "text/javascript":
		return true
	}
	return false
}

type folderRequest struct {
	Path string `json:"path"`
}

func (s *Server) handleFolderCreate(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.Mkdir(req.Path); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": vault.CleanDir(req.Path)})
}

func (s *Server) handleFolderDelete(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	recursive := r.URL.Query().Get("recursive") == "1" || r.URL.Query().Get("recursive") == "true"

	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	warnings, err := v.Rmdir(ctx, path, recursive)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "warnings": warnings})
}
