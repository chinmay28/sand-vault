package server

import (
	"errors"
	"net"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/movie"
	"github.com/chinmay28/SandHarbor/internal/tvaccess"
	"github.com/chinmay28/SandHarbor/internal/vault"
)

// The TV: one folder of films, opened with a four-character code.
//
// Two audiences use these routes. The owner, signed in on a computer, mints a
// code for a folder and a length of time, sees which codes are out, and takes
// them back (/api/tv/codes, behind the session like everything else). The TV
// trades the code for a device token at /api/tv/pair and presents that token on
// everything after, as an Authorization: Bearer header — it has no cookie and
// no password, and is not meant to have either.
//
// What a token reaches is the grant's folder, its videos and the folders
// beneath it that hold videos, and nothing else: no other file in the folder,
// no folder above it, no settings, no writes. Every path the TV sends is
// relative to the folder, and every path it is sent back is too, so the TV is
// never told where in the vault its folder sits. A file ID from elsewhere in
// the vault is answered as not found, the same as an ID that names nothing.
//
// A grant outlives a lock of the vault, deliberately. Locking takes the keys
// out of memory, so nothing a grant reaches can be read until somebody unlocks
// it again — and the TV says so rather than throwing its pairing away, so
// unlocking on the laptop is all it takes for the evening to carry on. Revoking
// the code is how a grant is ended early; restarting the server ends them all.

// tvAuthScheme is how a device presents its token, in the Authorization header.
const tvAuthScheme = "Bearer "

// tvPosterTimeout is the deadline on a poster, which may have to gather its
// folder's thumbnail pack from the clouds the first time it is asked for.
const tvPosterTimeout = time.Minute

// --- Owner: minting and revoking codes ------------------------------------

type tvCodeRequest struct {
	Path  string `json:"path"`
	Vault string `json:"vault"`
	Label string `json:"label"`

	// Minutes is how long the code lasts; zero means tvaccess.DefaultTTL.
	Minutes int `json:"minutes"`

	// Forever asks for a code with no end, which lasts until it is revoked or
	// SAND restarts. Minutes must then be zero.
	Forever bool `json:"forever"`
}

// tvCodesResponse is the list the dialog draws, with the addresses a TV could
// be pointed at by hand when discovery cannot reach it.
type tvCodesResponse struct {
	Codes     []tvaccess.Grant `json:"codes"`
	Addresses []string         `json:"addresses"`
	Discovery int              `json:"discovery_port,omitempty"`

	// LAN is false when the server is bound to loopback, where no TV can
	// reach it whatever code it is given.
	LAN bool `json:"lan"`
}

// handleTVCodeCreate mints a code for a folder.
func (s *Server) handleTVCodeCreate(w http.ResponseWriter, r *http.Request) {
	var req tvCodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	scope := vault.Scope(req.Vault)
	dir := vault.CleanDir(req.Path)
	if !v.FolderExists(scope, dir) {
		writeError(w, http.StatusNotFound, "no such folder: "+dir, "NOT_FOUND")
		return
	}

	// A forever code passes its minutes through untouched, so that asking for
	// both is refused by Issue rather than quietly settled here.
	ttl := tvaccess.DefaultTTL
	if req.Minutes != 0 || req.Forever {
		ttl = time.Duration(req.Minutes) * time.Minute
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = folderLabel(dir)
	}
	if len(label) > 80 {
		label = label[:80]
	}

	grant, err := s.tv.Issue(tvaccess.Request{Scope: string(scope), Dir: dir, Label: label, TTL: ttl, Forever: req.Forever})
	switch {
	case errors.Is(err, tvaccess.ErrBadTTL), errors.Is(err, tvaccess.ErrForeverWithTTL):
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	case errors.Is(err, tvaccess.ErrTooManyGrants):
		writeError(w, http.StatusConflict, err.Error(), "TOO_MANY_CODES")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL_ERROR")
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

// handleTVCodes lists the codes that are live.
func (s *Server) handleTVCodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, tvCodesResponse{
		Codes:     s.tv.Grants(),
		Addresses: lanAddresses(s.Port),
		Discovery: s.discoveryPort(),
		LAN:       warnsOnBind(s.Bind),
	})
}

// handleTVCodeRevoke ends a code and unpairs every TV that used it.
func (s *Server) handleTVCodeRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.tv.Revoke(r.PathValue("code")) {
		writeError(w, http.StatusNotFound, "no such code", "NOT_FOUND")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// folderLabel is what a TV calls a folder when the owner did not name it: the
// folder's own name, which is what anybody would call it out loud.
func folderLabel(dir string) string {
	if dir == "/" {
		return "Everything"
	}
	return path.Base(dir)
}

// --- Device: pairing ------------------------------------------------------

type tvPairRequest struct {
	Code string `json:"code"`
}

type tvPairResponse struct {
	Token     string    `json:"token"`
	Label     string    `json:"label"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Forever   bool      `json:"forever,omitempty"`
}

// handleTVPair trades a code for a device token. Public, because a TV has
// nothing else to offer; the code and the throttle behind it are what stand in
// front of it.
func (s *Server) handleTVPair(w http.ResponseWriter, r *http.Request) {
	var req tvPairRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	dev, err := s.tv.Redeem(clientAddress(r), req.Code)
	var throttled *tvaccess.ThrottledError
	switch {
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(throttled.RetryAfter.Round(time.Second).Seconds())+1))
		writeError(w, http.StatusTooManyRequests, err.Error(), "THROTTLED")
		return
	case errors.Is(err, tvaccess.ErrBadCode):
		writeError(w, http.StatusUnauthorized, err.Error(), "BAD_CODE")
		return
	case errors.Is(err, tvaccess.ErrTooManyDevices):
		writeError(w, http.StatusConflict, err.Error(), "TOO_MANY_DEVICES")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL_ERROR")
		return
	}
	writeJSON(w, http.StatusCreated, tvPairResponse{
		Token:     dev.Token,
		Label:     dev.Grant.Label,
		ExpiresAt: dev.Grant.ExpiresAt,
		Forever:   dev.Grant.Forever,
	})
}

// clientAddress is who is asking, for the throttle: the connection's own
// address. Not X-Forwarded-For, which the asker writes themselves — behind a
// proxy every TV shares one address and the per-client limit becomes the
// global one, which errs the safe way.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Device: everything after pairing -------------------------------------

// tvHandler is a handler that has been given the grant its token is under.
type tvHandler func(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant)

// requireTV resolves the bearer token to its grant, or refuses with
// TV_UNPAIRED, which the TV reads as "show the pairing screen".
//
// It does not check that the vault is unlocked: the session endpoint has to
// answer while it is locked, so the TV can say so. Everything that reads the
// vault is refused by the vault itself, as LOCKED.
func (s *Server) requireTV(next tvHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, tvAuthScheme) {
			writeError(w, http.StatusUnauthorized, "this TV is not paired", "TV_UNPAIRED")
			return
		}
		grant, ok := s.tv.Authenticate(strings.TrimSpace(strings.TrimPrefix(auth, tvAuthScheme)))
		if !ok {
			writeError(w, http.StatusUnauthorized,
				"this TV's code has expired or been revoked — pair it again", "TV_UNPAIRED")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, r, grant)
	}
}

// tvActive counts a TV request as use of the vault, the way a stream does: a
// TV has no browser session to keep the vault from locking itself while
// somebody browses for something to watch.
func (s *Server) tvActive(v *vault.Vault) {
	if v.Unlocked() {
		s.noteExternalActivity()
	}
}

type tvSessionResponse struct {
	Label     string    `json:"label"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Forever   bool      `json:"forever,omitempty"`
	Locked    bool      `json:"locked"`
}

// handleTVSession says what the TV is paired to, and whether the vault is open.
// It is what the TV polls while the vault is locked, so it does not count as
// use: a TV left on its "unlock SAND" screen must not hold anything open.
func (s *Server) handleTVSession(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, tvSessionResponse{
		Label:     grant.Label,
		ExpiresAt: grant.ExpiresAt,
		Forever:   grant.Forever,
		Locked:    !v.Unlocked(),
	})
}

// handleTVLogout unpairs the TV asking.
func (s *Server) handleTVLogout(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	s.tv.Forget(strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), tvAuthScheme)))
	writeJSON(w, http.StatusOK, map[string]string{"status": "unpaired"})
}

// tvFolder is a folder the TV can open, by its path relative to the grant.
type tvFolder struct {
	Name string `json:"name"`
	Path string `json:"path"`

	// Count is how many videos are at or below it.
	Count int `json:"count"`

	// Art says the folder has a picture, fetched from /api/tv/folders/art.
	Art bool `json:"art,omitempty"`
}

// tvItem is one video as a row of the TV's grid.
type tvItem struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Folder   string    `json:"folder"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified_at"`

	// Title and Year are the film's when it has been matched, so the grid can
	// show "Alien (1979)" rather than Alien.1979.REMASTERED.1080p.mkv.
	Title  string  `json:"title,omitempty"`
	Year   int     `json:"year,omitempty"`
	Rating float64 `json:"rating,omitempty"`

	Poster bool `json:"poster"`
}

type tvLibraryResponse struct {
	Label   string     `json:"label"`
	Path    string     `json:"path"`
	Parent  string     `json:"parent,omitempty"`
	Folders []tvFolder `json:"folders"`
	Items   []tvItem   `json:"items"`
}

// handleTVLibrary lists one folder under the grant: the folders beneath it
// that hold a video somewhere, and the videos in it. With all=1 it is every
// video at or below the folder instead, in one flat list — the "all films"
// shelf, for a library sorted into a folder per film.
func (s *Server) handleTVLibrary(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	v, _ := s.Vault()
	scope := vault.Scope(grant.Scope)
	rel := vault.CleanDir(r.URL.Query().Get("path"))
	abs := confine(grant.Dir, rel)

	below, err := v.Descendants(scope, abs)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	s.tvActive(v)

	all := r.URL.Query().Get("all") == "1"
	resp := tvLibraryResponse{Label: grant.Label, Path: rel, Folders: []tvFolder{}, Items: []tvItem{}}
	if rel != "/" {
		resp.Parent = path.Dir(rel)
	}

	// Which videos have a poster, asked once per folder that holds one.
	posters := postersOf(v, scope, below)

	counts := map[string]int{}
	var videos, deeper []*vault.Entry
	for _, e := range below {
		if !vault.IsVideo(e) {
			continue
		}
		if all || e.Dir == abs {
			videos = append(videos, e)
			if all {
				continue
			}
		}
		if e.Dir == abs {
			continue
		}
		deeper = append(deeper, e)
		// A file deeper down counts toward the subfolder of abs it is under.
		rest := strings.TrimPrefix(e.Dir, strings.TrimSuffix(abs, "/")+"/")
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		counts[rest]++
	}

	if !all {
		dirs := make([]string, 0, len(counts))
		for name := range counts {
			dirs = append(dirs, vault.JoinPath(abs, name))
		}
		art := folderArt(v, scope, dirs, deeper, posters)
		for name, n := range counts {
			resp.Folders = append(resp.Folders, tvFolder{
				Name:  name,
				Path:  vault.JoinPath(rel, name),
				Count: n,
				Art:   art[vault.JoinPath(abs, name)] != "",
			})
		}
		sort.Slice(resp.Folders, func(i, j int) bool {
			return strings.ToLower(resp.Folders[i].Name) < strings.ToLower(resp.Folders[j].Name)
		})
	}

	for _, e := range videos {
		resp.Items = append(resp.Items, s.tvItemFor(v, grant, e, posters[e.ID]))
	}
	sort.Slice(resp.Items, func(i, j int) bool {
		a, b := resp.Items[i].sortKey(), resp.Items[j].sortKey()
		if a != b {
			return a < b
		}
		return resp.Items[i].ID < resp.Items[j].ID
	})

	writeJSON(w, http.StatusOK, resp)
}

// postersOf reports which of a set of files have a stored picture, asking the
// index once per folder rather than once per file.
func postersOf(v *vault.Vault, scope vault.Scope, entries []*vault.Entry) map[string]bool {
	out := map[string]bool{}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Dir] {
			continue
		}
		seen[e.Dir] = true
		for _, id := range v.ThumbIDs(scope, e.Dir) {
			out[id] = true
		}
	}
	return out
}

// folderArt picks the picture each folder is drawn with on the TV, by the ID
// of the file whose thumbnail it is; a folder with none is absent.
//
// The picture chosen for it in the browser comes first — that is the owner
// saying which film stands for the folder. Failing that, a folder holding
// exactly one video wears that video's poster: a folder per film is how most
// libraries are sorted, and for those there is nothing to guess. A folder of
// several films with nothing chosen keeps its icon, as it does in the browser.
//
// videos are the videos somewhere beneath the folders, and posters which of
// them have a picture.
func folderArt(v *vault.Vault, scope vault.Scope, dirs []string, videos []*vault.Entry, posters map[string]bool) map[string]string {
	out := map[string]string{}
	chosen := v.FolderArts(scope, dirs)
	for _, dir := range dirs {
		if art, ok := chosen[dir]; ok {
			out[dir] = art.ID
			continue
		}
		var only *vault.Entry
		n := 0
		for _, e := range videos {
			if within(e.Dir, dir) {
				only = e
				n++
			}
		}
		if n == 1 && posters[only.ID] {
			out[dir] = only.ID
		}
	}
	return out
}

// handleTVFolderArt serves a folder's picture, by the folder's path relative
// to the grant. The picture is worked out again here rather than taken from an
// ID the TV sends, so the TV can only ever ask for a folder's picture — never
// for an arbitrary file's thumbnail.
func (s *Server) handleTVFolderArt(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	v, _ := s.Vault()
	scope := vault.Scope(grant.Scope)
	abs := confine(grant.Dir, vault.CleanDir(r.URL.Query().Get("path")))

	below, err := v.Descendants(scope, abs)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	s.tvActive(v)

	var videos []*vault.Entry
	for _, e := range below {
		if vault.IsVideo(e) {
			videos = append(videos, e)
		}
	}
	id := folderArt(v, scope, []string{abs}, videos, postersOf(v, scope, videos))[abs]
	if id == "" {
		writeError(w, http.StatusNotFound, "this folder has no picture", "NO_THUMBNAIL")
		return
	}
	s.writeThumb(w, r, v, id)
}

func (s *Server) tvItemFor(v *vault.Vault, grant tvaccess.Grant, e *vault.Entry, poster bool) tvItem {
	folder, _ := relativeTo(grant.Dir, e.Dir)
	item := tvItem{
		ID: e.ID, Name: e.Name, Folder: folder, Size: e.Size, Modified: e.ModifiedAt, Poster: poster,
	}
	if info := v.Movie(e.ID); info != nil {
		item.Title, item.Year, item.Rating = info.Title, info.Year, info.Rating
	}
	return item
}

// sortKey orders the grid by what it shows: the film's title when there is
// one, the filename when not, and a leading "The" ignored the way every film
// shelf ignores it.
func (i tvItem) sortKey() string {
	key := i.Title
	if key == "" {
		key = strings.TrimSuffix(i.Name, path.Ext(i.Name))
	}
	key = strings.ToLower(key)
	return strings.TrimPrefix(key, "the ")
}

type tvItemResponse struct {
	tvItem
	MIME  string      `json:"mime,omitempty"`
	Movie *movie.Info `json:"movie,omitempty"`
}

// handleTVItem is the details page: everything the film database said, which
// is the whole of what makes the TV feel like a film library rather than a
// file list.
func (s *Server) handleTVItem(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	v, _ := s.Vault()
	entry, ok := s.tvEntry(w, v, grant, r.PathValue("id"))
	if !ok {
		return
	}
	s.tvActive(v)

	poster := false
	for _, id := range v.ThumbIDs(vault.Scope(grant.Scope), entry.Dir) {
		if id == entry.ID {
			poster = true
			break
		}
	}
	info := v.Movie(entry.ID)
	if info != nil {
		// What it was matched on is for the owner correcting a match, not for
		// a TV; and the file's own name is already on the item.
		copied := *info
		copied.Query, copied.File = "", ""
		info = &copied
	}
	writeJSON(w, http.StatusOK, tvItemResponse{
		tvItem: s.tvItemFor(v, grant, entry, poster),
		MIME:   entry.MIME,
		Movie:  info,
	})
}

// handleTVPoster serves a video's thumbnail — its poster, when the film
// database found one.
func (s *Server) handleTVPoster(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	v, _ := s.Vault()
	entry, ok := s.tvEntry(w, v, grant, r.PathValue("id"))
	if !ok {
		return
	}
	s.tvActive(v)

	s.writeThumb(w, r, v, entry.ID)
}

// writeThumb serves a file's stored thumbnail to a TV.
func (s *Server) writeThumb(w http.ResponseWriter, r *http.Request, v *vault.Vault, id string) {
	ctx, cancel := contextWithTimeout(r, tvPosterTimeout)
	defer cancel()
	data, err := v.Thumb(ctx, id)
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
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handleTVPlay mints a stream link for a video, which is what the player is
// handed: a player follows an address and sends no headers of its own, so the
// bearer token cannot come with it. The link is bound to the grant as well as
// the file, so revoking the code stops a film that is already playing.
func (s *Server) handleTVPlay(w http.ResponseWriter, r *http.Request, grant tvaccess.Grant) {
	v, _ := s.Vault()
	entry, ok := s.tvEntry(w, v, grant, r.PathValue("id"))
	if !ok {
		return
	}
	s.tvActive(v)

	token, expiry, err := s.streams.issue(streamTicket{fileID: entry.ID, source: vault.ReadParts, grant: grant.Code})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not mint a stream link", "INTERNAL_ERROR")
		return
	}
	resp := tvPlayResponse{
		streamLinkResponse: streamLinkResponse{
			URL:       streamPath(token, entry.Name),
			Name:      entry.Name,
			MIME:      entry.MIME,
			ExpiresAt: expiry,
			ExpiresIn: int(time.Until(expiry).Round(time.Second).Seconds()),
		},
		Subtitles: []tvSubtitleLink{},
	}

	// The subtitle files beside it, each behind a link of its own bound to
	// the same code. A folder that will not list is a film without them, not
	// a film that will not play.
	if listing, err := v.List(vault.Scope(grant.Scope), entry.Dir); err == nil {
		for _, sub := range sidecarSubtitles(entry, listing.Files) {
			subToken, _, err := s.streams.issue(streamTicket{fileID: sub.Entry.ID, source: vault.ReadParts, grant: grant.Code})
			if err != nil {
				continue
			}
			resp.Subtitles = append(resp.Subtitles, tvSubtitleLink{
				URL:      streamPath(subToken, sub.Entry.Name),
				Label:    sub.Label,
				Language: sub.Language,
				MIME:     sub.MIME,
			})
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// tvPlayResponse is a stream link for the film and one for each subtitle file
// found beside it.
type tvPlayResponse struct {
	streamLinkResponse
	Subtitles []tvSubtitleLink `json:"subtitles"`
}

// tvSubtitleLink is one sidecar subtitle, as the player is handed it.
type tvSubtitleLink struct {
	URL      string `json:"url"`
	Label    string `json:"label"`
	Language string `json:"language,omitempty"`
	MIME     string `json:"mime"`
}

// tvEntry resolves a file ID for a TV, answering not found for anything the
// grant does not reach: a file in another vault, outside the folder, or one
// that is not a video. Not found rather than forbidden, so a TV cannot learn
// which IDs exist elsewhere by asking.
func (s *Server) tvEntry(w http.ResponseWriter, v *vault.Vault, grant tvaccess.Grant, id string) (*vault.Entry, bool) {
	entry, err := v.Entry(id)
	if err != nil {
		if errors.Is(err, vault.ErrLocked) {
			vaultErrorResponse(w, err)
			return nil, false
		}
		writeError(w, http.StatusNotFound, "no such video", "NOT_FOUND")
		return nil, false
	}
	scope, _ := v.ScopeOf(entry.ID)
	if string(scope) != grant.Scope || !within(entry.Dir, grant.Dir) || !vault.IsVideo(entry) {
		writeError(w, http.StatusNotFound, "no such video", "NOT_FOUND")
		return nil, false
	}
	return entry, true
}

// --- Paths ---------------------------------------------------------------

// confine joins a path the TV sent onto the grant's folder. rel has been
// through CleanDir, which resolves every "..", so the result cannot climb
// above root however rel was spelled.
func confine(root, rel string) string {
	root, rel = vault.CleanDir(root), vault.CleanDir(rel)
	if rel == "/" {
		return root
	}
	if root == "/" {
		return rel
	}
	return root + rel
}

// within reports whether dir is root or a folder beneath it.
func within(dir, root string) bool {
	dir, root = vault.CleanDir(dir), vault.CleanDir(root)
	return root == "/" || dir == root || strings.HasPrefix(dir, root+"/")
}

// relativeTo is confine undone: dir as the TV knows it, or false when dir is
// not under root at all.
func relativeTo(root, dir string) (string, bool) {
	root, dir = vault.CleanDir(root), vault.CleanDir(dir)
	if !within(dir, root) {
		return "", false
	}
	if root == "/" {
		return dir, true
	}
	if dir == root {
		return "/", true
	}
	return strings.TrimPrefix(dir, root), true
}

// --- Addresses -----------------------------------------------------------

// lanAddresses lists the addresses a TV on this network could reach the web
// server at, for the dialog to show beside the code when the TV cannot find
// the server on its own. IPv4 only: nobody types an IPv6 address with a remote.
func lanAddresses(port int) []string {
	if port == 0 {
		port = DefaultPort
	}
	out := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, "http://"+net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		}
	}
	sort.Strings(out)
	return out
}
