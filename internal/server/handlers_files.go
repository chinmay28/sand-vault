package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/thumb"
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
	accounts := formAccounts(r)

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

	for i, fh := range uploads {
		result := uploadResult{Name: fh.Filename}

		f, err := fh.Open()
		if err != nil {
			result.Error = "could not read the uploaded file"
			results = append(results, result)
			continue
		}
		// Streamed rather than read into a slice first, and read where the
		// multipart parser already put it. Upload held the whole file in memory,
		// so a 4 GB film through the browser was 4 GB resident on top of
		// everything the scatter allocates; this is bounded by the chunk window.
		// UploadStreamAt rather than UploadStream because the parser has already
		// spilled anything large to disk, and spooling it again would write the
		// film twice.
		entry, warnings, err := v.UploadStreamAt(ctx, dir, fh.Filename, f, fh.Size, vault.UploadOptions{
			Overwrite: overwrite,
			Accounts:  accounts,
		})
		f.Close()
		result.Warnings = warnings
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}

		result.OK = true
		result.File = entry

		// The picture the browser made of this file, if it made one. A
		// thumbnail that will not store is a warning and nothing more: the
		// file itself is already scattered, and the list falls back to the
		// icon it has always shown.
		if thumb := formThumb(r, i); len(thumb) > 0 {
			if err := storeThumb(ctx, v, entry.ID, thumb); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("stored the file but not its preview image: %v", err))
			}
		}

		results = append(results, result)
		stored++
	}

	status := http.StatusCreated
	if stored == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"results": results, "stored": stored})
}

// formAccounts reads the accounts an upload chose to spread over. The field
// may be repeated once per account or given as one comma-separated list,
// because a form built by hand and a form built by the browser tend to differ
// on that, and neither is worth refusing over. Absent, the vault's default
// applies.
func formAccounts(r *http.Request) []string {
	var out []string
	for _, key := range []string{"accounts", "accounts[]"} {
		for _, raw := range r.MultipartForm.Value[key] {
			for _, id := range strings.Split(raw, ",") {
				if id = strings.TrimSpace(id); id != "" {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// formThumb reads the preview image the browser generated for the nth file of
// an upload. The field is named for the file's position because that is the
// only thing in a multi-file upload that is unique — two files dropped
// together can share a name, and only some of them will have a picture.
func formThumb(r *http.Request, index int) []byte {
	files := r.MultipartForm.File[fmt.Sprintf("thumb-%d", index)]
	if len(files) == 0 {
		return nil
	}
	if files[0].Size > thumb.MaxInputBytes {
		return nil
	}

	f, err := files[0].Open()
	if err != nil {
		return nil
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, thumb.MaxInputBytes))
	if err != nil {
		return nil
	}
	return data
}

// storeThumb normalizes an incoming preview image and stores it against a
// file. Everything that reaches the vault goes through here, so a thumbnail is
// always the vault's own JPEG at a known size rather than whatever bytes a
// caller supplied.
func storeThumb(ctx context.Context, v *vault.Vault, id string, data []byte) error {
	normalized, err := thumb.Normalize(data)
	if err != nil {
		return err
	}
	return v.SetThumb(ctx, id, normalized)
}

// handleFileThumb serves a file's stored thumbnail. The first row of a folder
// to ask for one gathers that folder's whole pack; every other row is answered
// from memory.
func (s *Server) handleFileThumb(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	data, err := v.Thumb(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, vault.ErrNoThumb) {
			writeError(w, http.StatusNotFound, err.Error(), "NO_THUMBNAIL")
			return
		}
		vaultErrorResponse(w, err)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// A thumbnail is a picture of a stored file, so it is treated as the file
	// content is: never held by a shared cache, and gone when the vault locks.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handleFileThumbSet stores a thumbnail for a file that has none — a file
// uploaded before thumbnails existed, or from the command line, whose picture
// the browser made the first time someone opened it.
func (s *Server) handleFileThumbSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, thumb.MaxInputBytes)

	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the image", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	if err := storeThumb(ctx, v, r.PathValue("id"), data); err != nil {
		if errors.Is(err, vault.ErrLocked) {
			vaultErrorResponse(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_THUMBNAIL")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "stored"})
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
	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	entry, err := v.Move(ctx, r.PathValue("id"), req.Dir, req.Name)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": entry, "path": entry.Path()})
}

// relocateRequest asks for a file, or a folder and everything under it, to be
// moved onto a different set of cloud accounts.
type relocateRequest struct {
	// ID names a single file; Path names either a file or a folder. One of
	// them is required, and ID wins if both are given.
	ID   string `json:"id"`
	Path string `json:"path"`

	// Accounts is where the shards should end up, and how many are named settles
	// the scheme. Naming a different *count* from the one the file is on now
	// changes its code, which rebuilds the file rather than moving it.
	Accounts []string `json:"accounts"`

	// Preview asks what the move would do without doing any of it. The answer
	// comes out of the index alone, so it costs nothing and no account is
	// contacted.
	Preview bool `json:"preview"`
}

// relocateTimeout is the ceiling on one relocation request.
//
// It is hours rather than minutes because moving a folder is a copy between two
// clouds for every part of every chunk of every file in it, and a request that
// gave up halfway would be indistinguishable from one that never started. It can
// afford to be this long precisely because a relocation is resumable: each file
// commits on its own, so a timeout costs the file in flight and nothing else,
// and asking again picks up whatever is still in the wrong place.
const relocateTimeout = 6 * time.Hour

// handleRelocate moves a file or a folder onto other clouds. Only the parts
// that are not already on one of the chosen accounts are copied — see
// vault.Relocate.
func (s *Server) handleRelocate(w http.ResponseWriter, r *http.Request) {
	var req relocateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	target := strings.TrimSpace(req.ID)
	if target == "" {
		target = strings.TrimSpace(req.Path)
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, "name the file or folder to move", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()

	if req.Preview {
		plan, err := v.PlanRelocation(target, req.Accounts)
		if err != nil {
			vaultErrorResponse(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
		return
	}

	ctx, cancel := contextWithTimeout(r, relocateTimeout)
	defer cancel()

	report, err := v.Relocate(ctx, target, req.Accounts, nil)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
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

// handleFileContent serves a stored file, rebuilding only the part of it the
// request actually asks for. With ?download=1 the browser saves it; otherwise it
// renders inline, which is what makes previewing possible.
//
// It reads at an offset rather than rebuilding the file first, and the
// difference is the whole point. A <video> element asks for a few hundred
// kilobytes at a time and seeks around; answering each of those from a buffer
// holding the entire film cost memory proportional to the film — measured at
// 337 MB to serve one range of a 256 MB file, and so about 5 GB for a 4 GB one,
// per request in flight. Through the chunked reader the same request costs the
// chunks that range covers, which does not grow with the file at all.
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	body, entry, err := v.OpenReadSeeker(ctx, r.PathValue("id"))
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

	// ServeContent turns a Range header into seeks and reads on the body, which
	// is why the body has to be the vault's own random-access reader rather
	// than a buffer holding the file.
	http.ServeContent(w, r, entry.Name, entry.ModifiedAt, body)
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

// handleFoldersList hands back every folder in the vault at once.
//
// The listing endpoint answers one level at a time, which is right for a file
// browser and wrong for a picker: choosing where to move something means seeing
// the tree, and walking it a request per level to draw one dialog is a lot of
// round-trips for an answer that is a walk of the index either way. It carries
// no file names and no placements — folder paths and nothing else.
func (s *Server) handleFoldersList(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	folders, err := v.Folders()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
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

// folderArtRequest gives a folder a picture, or takes it away again with an
// empty ID.
type folderArtRequest struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}

// handleFolderArt answers what a folder is drawn with — nothing, until somebody
// picks — and what it could be drawn with: every file under it that has a
// thumbnail, films first.
//
// The picture itself is not here and never was: the answer is a file ID, and the
// browser draws it through the thumbnail endpoint that file's own row uses. A
// folder's picture is borrowed, not stored.
func (s *Server) handleFolderArt(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}

	v, _ := s.Vault()
	choices, truncated, err := v.FolderArtChoices(dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if choices == nil {
		choices = []vault.ArtChoice{}
	}

	body := map[string]any{
		"path":       vault.CleanDir(dir),
		"candidates": choices,
		"truncated":  truncated,
	}
	if art, ok := v.FolderArtFor(dir); ok {
		body["art"] = art
	}
	writeJSON(w, http.StatusOK, body)
}

// handleFolderArtSet records the picture somebody picked for a folder, or drops
// it so the folder goes back to its icon.
func (s *Server) handleFolderArtSet(w http.ResponseWriter, r *http.Request) {
	var req folderArtRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "name the folder", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	art, err := v.SetFolderArt(req.Path, req.ID)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	body := map[string]any{"path": vault.CleanDir(req.Path)}
	if art.ID != "" {
		body["art"] = art
	}
	writeJSON(w, http.StatusOK, body)
}

// folderMoveRequest renames a folder, or moves it under another one — which are
// the same thing, since a folder is a path in the index and nothing else.
type folderMoveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleFolderMove moves a folder and everything beneath it to another path.
//
// Nothing is transferred: the parts of every file inside it stay exactly where
// they are on the accounts holding them, and the thumbnails and film settings
// filed under the folder travel with it for the price of rewriting a map. The
// whole subtree changes in a single write, so there is no moment where half of
// it answers to the old name — see vault.MoveFolder.
func (s *Server) handleFolderMove(w http.ResponseWriter, r *http.Request) {
	var req folderMoveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.From) == "" || strings.TrimSpace(req.To) == "" {
		writeError(w, http.StatusBadRequest,
			"name the folder to move and where it should go", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, 2*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	if err := v.MoveFolder(ctx, req.From, req.To); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"from": vault.CleanDir(req.From),
		"path": vault.CleanDir(req.To),
	})
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
