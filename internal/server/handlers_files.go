package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/thumb"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// contextWithTimeout derives a request-scoped context so a hung provider
// cannot pin a handler open forever.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// requestScope reads which of the vaults inside the file a request is aimed at.
//
// Only the endpoints that address something by path need it — a listing, a
// search, an upload, a folder. Everything addressed by file ID does not, and
// deliberately: an ID is unique across every vault, so the vault resolves it
// against whatever is open and a file inside a shut sub vault is simply not
// found. That is what keeps reading, moving, deleting, streaming and
// thumbnailing a file exactly the requests they were before sub vaults existed.
func requestScope(r *http.Request) vault.Scope {
	return vault.Scope(r.URL.Query().Get("vault"))
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}

	v, _ := s.Vault()
	listing, err := v.List(requestScope(r), path)
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
		Vault: requestScope(r),
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
//
// Name is the path the file had inside the directory it came from, not just its
// leaf: a dropped folder can hold four "cover.jpg", and a failure has to say
// which one.
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

	scope := requestScope(r)
	dir := r.FormValue("path")
	if dir == "" {
		dir = "/"
	}
	overwrite := r.FormValue("overwrite") == "true" || r.FormValue("overwrite") == "1"
	accounts := formAccounts(r)
	scheme, err := formScheme(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_SCHEME")
		return
	}

	uploads := r.MultipartForm.File["files[]"]
	if len(uploads) == 0 {
		writeError(w, http.StatusBadRequest, "missing files[] field", "MISSING_FILE")
		return
	}

	// Where every file of this upload lands, worked out before anything is
	// sent: a file chosen on its own keeps its name in the folder being looked
	// at, and a file that came in as part of a directory keeps the path it had
	// inside it. Done up front so a path the client should not have asked for
	// is refused before a byte of it is scattered.
	places := make([]uploadPlace, len(uploads))
	for i, fh := range uploads {
		places[i] = placeUpload(dir, formRelPath(r, i), fh.Filename)
	}

	folders, err := uploadFolders(r, dir, places)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_PATH")
		return
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	results := make([]uploadResult, 0, len(uploads)+len(folders))
	stored := 0

	// The tree the files hang off, made once each. A folder that will not be
	// made is reported like a file that will not store — everything under it
	// then fails on its own line, which is what says how much of the directory
	// actually arrived.
	made := map[string]bool{vault.CleanDir(dir): true}
	for _, folder := range folders {
		if made[folder] || v.FolderExists(scope, folder) {
			made[folder] = true
			continue
		}
		if err := v.Mkdir(scope, folder); err != nil {
			results = append(results, uploadResult{Name: strings.TrimPrefix(folder, "/"), Error: err.Error()})
			continue
		}
		made[folder] = true
	}

	for i, fh := range uploads {
		place := places[i]
		result := uploadResult{Name: place.Label}
		if place.Err != nil {
			result.Error = place.Err.Error()
			results = append(results, result)
			continue
		}
		if !made[place.Dir] {
			result.Error = fmt.Sprintf("could not make the folder %s", place.Dir)
			results = append(results, result)
			continue
		}

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
		entry, warnings, err := v.UploadStreamAt(ctx, scope, place.Dir, place.Name, f, fh.Size, vault.UploadOptions{
			Overwrite: overwrite,
			Accounts:  accounts,
			Scheme:    scheme,
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

// parseSchemeField reads a "k-of-n" scheme out of a JSON field, treating empty
// as "no choice made" rather than as a malformed one.
func parseSchemeField(raw string) (archive.Scheme, error) {
	if strings.TrimSpace(raw) == "" {
		return archive.Scheme{}, nil
	}
	return archive.ParseScheme(strings.TrimSpace(raw))
}

// formScheme reads the erasure code an upload chose to be cut with, written the
// way a person writes one: "2-of-3", "3-of-5", "6-of-10". Absent or empty, how
// many accounts were chosen settles it, which is what almost every upload
// wants.
func formScheme(r *http.Request) (archive.Scheme, error) {
	raw := strings.TrimSpace(r.FormValue("scheme"))
	if raw == "" {
		return archive.Scheme{}, nil
	}
	return archive.ParseScheme(raw)
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

// uploadPlace is where one file of an upload belongs: the folder it goes in,
// the name it takes there, and the path to call it by when reporting back. Err
// is set instead of the rest when the client asked for somewhere it may not go.
type uploadPlace struct {
	Dir   string
	Name  string
	Label string
	Err   error
}

// placeUpload works out where one file of an upload belongs.
//
// A file chosen on its own is a name, and lands in the folder the upload was
// posted to. A file that arrived as part of a directory carries the path it had
// inside that directory — "2024/summer/hike.jpg" — and that path is rebuilt
// under the destination rather than flattened into it, because a folder that
// arrives as four hundred loose files is not the folder that was dropped.
//
// The relative path is whatever the client wrote in the form, so it is treated
// as such: every segment goes through the check a typed name goes through, and
// a ".." among them is refused rather than cleaned away — a form asking to
// climb out of the folder it was posted to is not one to guess the intent of.
func placeUpload(dir, rel, filename string) uploadPlace {
	segments, err := pathSegments(rel)
	if err != nil {
		return uploadPlace{Label: rel, Err: err}
	}

	// No path, or one that was nothing but separators: an ordinary single file,
	// placed exactly where it was before directories could be uploaded at all.
	if len(segments) == 0 {
		name, err := vault.SanitizeName(filename)
		if err != nil {
			return uploadPlace{Label: filename, Err: err}
		}
		return uploadPlace{Dir: vault.CleanDir(dir), Name: name, Label: name}
	}

	place := uploadPlace{
		Dir:   vault.CleanDir(dir),
		Name:  segments[len(segments)-1],
		Label: strings.Join(segments, "/"),
	}
	for _, segment := range segments[:len(segments)-1] {
		place.Dir = vault.JoinPath(place.Dir, segment)
	}
	return place
}

// pathSegments splits a client-supplied relative path into names that are safe
// to build a folder out of. Separators are normalized because a Windows browser
// and a Unix one do not agree on them, "." segments are dropped as the nothing
// they are, and anything else that is not a plain name is an error rather than
// something to be quietly repaired.
func pathSegments(rel string) ([]string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" {
		return nil, nil
	}
	// A path inside a folder is relative by definition. One that is not is
	// refused rather than quietly read as though it were: no browser writes
	// this field with a root on it, so whatever did is not describing a
	// directory somebody chose.
	if strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("invalid path %q", rel)
	}

	var segments []string
	for _, segment := range strings.Split(rel, "/") {
		switch strings.TrimSpace(segment) {
		case "", ".":
			continue
		case "..":
			return nil, fmt.Errorf("invalid path %q", rel)
		}
		name, err := vault.SanitizeName(segment)
		if err != nil {
			return nil, err
		}
		segments = append(segments, name)
	}
	return segments, nil
}

// uploadFolders lists every folder this upload needs, parents first.
//
// Two things ask for one: a file that came in with a path under it, and the
// "dirs" field, which names folders in their own right. The second exists for
// the folders a directory holds that hold no file — a browser that walks a
// dropped tree can see those, and a directory that arrives with its empty
// corners missing is not the directory that was dropped.
func uploadFolders(r *http.Request, dir string, places []uploadPlace) ([]string, error) {
	base := vault.CleanDir(dir)
	seen := map[string]bool{base: true}
	var wanted []string

	add := func(folder string) {
		if seen[folder] {
			return
		}
		seen[folder] = true
		wanted = append(wanted, folder)
	}

	for _, raw := range r.MultipartForm.Value["dirs"] {
		segments, err := pathSegments(raw)
		if err != nil {
			return nil, err
		}
		folder := base
		for _, segment := range segments {
			folder = vault.JoinPath(folder, segment)
			add(folder)
		}
	}
	for _, place := range places {
		if place.Err == nil {
			add(place.Dir)
		}
	}

	// Shortest first, so a folder is never asked for before the folder holding
	// it — which is also the order the failures read in when a tree cannot be
	// built.
	sort.Strings(wanted)
	return wanted, nil
}

// formRelPath reads the path a file had inside the directory it came from.
// Named for the file's position for the same reason the thumbnail field is: two
// files in one upload can share a name, and only some of them came out of a
// directory at all. Absent, the file was chosen on its own.
func formRelPath(r *http.Request, index int) string {
	values := r.MultipartForm.Value[fmt.Sprintf("rel-%d", index)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
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
	// the scheme unless Scheme names one. Ending up under a different code from
	// the one a file is on now rebuilds it rather than moving it.
	Accounts []string `json:"accounts"`

	// Scheme is the code the files should end up cut with, written "k-of-n".
	// Empty leaves it to the count of accounts, which names one only for the
	// default family — so moving onto five clouds has to say what five clouds
	// means, and moving onto six need not.
	Scheme string `json:"scheme,omitempty"`

	// Preview asks what the move would do without doing any of it. The answer
	// comes out of the index alone, so it costs nothing and no account is
	// contacted.
	Preview bool `json:"preview"`

	// Vault names which of the vaults inside the file the target is in. Empty
	// is the main vault. It is needed because a relocation can be aimed at a
	// path, and two vaults can each have one of the same name.
	Vault string `json:"vault,omitempty"`
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

	scheme, err := parseSchemeField(req.Scheme)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_SCHEME")
		return
	}

	v, _ := s.Vault()

	if req.Preview {
		plan, err := v.PlanRelocation(vault.Scope(req.Vault), target, req.Accounts, scheme)
		if err != nil {
			vaultErrorResponse(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
		return
	}

	ctx, cancel := contextWithTimeout(r, relocateTimeout)
	defer cancel()

	report, err := v.Relocate(ctx, vault.Scope(req.Vault), target, req.Accounts, scheme, nil)
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

	// Vault names which of the vaults inside the file the folder belongs to.
	// Empty is the main vault.
	Vault string `json:"vault,omitempty"`
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
	folders, err := v.Folders(requestScope(r))
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
	if err := v.Mkdir(vault.Scope(req.Vault), req.Path); err != nil {
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

	// Vault names which of the vaults inside the file the folder is in. Empty
	// is the main vault.
	Vault string `json:"vault,omitempty"`
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
	choices, truncated, err := v.FolderArtChoices(requestScope(r), dir)
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
	if art, ok := v.FolderArtFor(requestScope(r), dir); ok {
		body["art"] = art
	}
	writeJSON(w, http.StatusOK, body)
}

// handleFolderSurvey answers what is under a folder: every file at or below it
// with its kind and how deep it sits, and every folder below it with what it is
// holding.
//
// It is what the organizer plans from — flattening a tree, clearing out the
// folders left empty, finding every file of a kind — and it is deliberately the
// only thing added for them. Each of those tools then runs over the endpoints
// that already existed, one item at a time from the browser, so a run that
// stalls has done exactly what its progress said it had. See vault.Survey.
func (s *Server) handleFolderSurvey(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}

	v, _ := s.Vault()
	survey, err := v.Survey(requestScope(r), dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, survey)
}

// handleFolderStats answers what one folder is holding: how much is under it,
// in how many files and folders, what those files weigh once split, and which
// accounts their parts went to.
//
// Separate from the survey next door because the two are asked at different
// moments for different reasons. The survey is what the organizer plans a run
// from and names every file to do it; this is what the folder menu puts in its
// header the moment somebody opens it, and a header is not worth ten thousand
// names. Both are one walk of an index already in memory. See vault.FolderStats.
func (s *Server) handleFolderStats(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}

	v, _ := s.Vault()
	stats, err := v.FolderStats(requestScope(r), dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleFolderDuplicates answers which files under a folder are copies of each
// other — by content, by size, and by name, all three from one walk.
//
// Three answers to one request because they are three readings of the same
// question and switching between them is the whole of using it: a pair that is
// only a size match is worth a second look, and finding that out should not
// cost another walk of the index. It reads and nothing else; erasing a copy is
// the DELETE endpoint every other delete goes through. See vault.Duplicates.
func (s *Server) handleFolderDuplicates(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}

	v, _ := s.Vault()
	dupes, err := v.Duplicates(requestScope(r), dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dupes)
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
	art, err := v.SetFolderArt(vault.Scope(req.Vault), req.Path, req.ID)
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

	// Vault names which of the vaults inside the file the folder is in. A move
	// stays inside one tree — crossing between them is an assignment, which
	// re-encrypts. Empty is the main vault.
	Vault string `json:"vault,omitempty"`
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
	if err := v.MoveFolder(ctx, vault.Scope(req.Vault), req.From, req.To); err != nil {
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
	warnings, err := v.Rmdir(ctx, requestScope(r), path, recursive)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "warnings": warnings})
}

// handleDegradedList answers with the files missing at least one part.
//
// The count in the accounts panel is Stats.Degraded; this is what that number
// is counting, so that a figure somebody reads there can be clicked rather than
// only worried about. Paged, because the thing that leaves files short — an
// account refusing for an afternoon — leaves as many of them as were uploaded
// in that afternoon.
//
// A read of the index and nothing more: no account is contacted, which is what
// makes it safe for a dialog to ask again after every repair.
func (s *Server) handleDegradedList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	offset, err := intParam(query.Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "offset must be a whole number", "BAD_REQUEST")
		return
	}
	limit, err := intParam(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit must be a whole number", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	page, err := v.Degraded(offset, limit)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// intParam reads an optional non-negative whole number from the query string.
// An absent one is zero, which every caller here reads as "no preference".
func intParam(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("not a whole number: %q", raw)
	}
	return parsed, nil
}
