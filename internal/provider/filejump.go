package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
)

func init() {
	Register(Spec{
		Kind:  KindFileJump,
		Label: "FileJump",
		Description: "A FileJump account, reached through its API with an access token you create " +
			"in your account settings. SAND keeps its parts in one folder, and a part it deletes " +
			"is erased for good rather than left to fill the trash.",
		DocsURL: "https://filejump.com/kb",
		Order:   25,
		// FileJump runs three servers that an account belongs to one of, and
		// the two generations speak different upload protocols — so the
		// server is a setting, and these save looking it up.
		Presets: []Preset{
			{
				Key:    "eu",
				Label:  "eu.filejump.com",
				Help:   "The newest FileJump server, where accounts made recently live.",
				Values: map[string]string{"server": "eu.filejump.com"},
			},
			{
				Key:    "drive",
				Label:  "drive.filejump.com",
				Help:   "FileJump's first server.",
				Values: map[string]string{"server": "drive.filejump.com"},
			},
			{
				Key:    "app",
				Label:  "app.filejump.com",
				Help:   "The server between the two.",
				Values: map[string]string{"server": "app.filejump.com"},
			},
		},
		Fields: []FieldSpec{
			{
				Key:      "access_token",
				Label:    "Access token",
				Secret:   true,
				Required: true,
				Help: "In FileJump: account settings (top right) → Developers → API access tokens → " +
					"create one, and paste it here.",
			},
			{
				Key:      "server",
				Label:    "Server",
				Required: true,
				Default:  "eu.filejump.com",
				Help: "The address in your browser's bar when you are signed in: eu.filejump.com " +
					"for an account made recently, drive.filejump.com or app.filejump.com for an " +
					"older one.",
			},
			{
				Key:     "folder",
				Label:   "Folder",
				Default: "sand",
				Help:    "A folder at the top of the account. SAND creates it if it is missing; blank means the root.",
			},
			{
				Key:      "workspace_id",
				Label:    "Workspace ID",
				Advanced: true,
				Help:     "Only for parts kept in a shared workspace rather than your personal space.",
			},
		},
	}, newFileJumpProvider)
}

// filejumpAPI is where a FileJump server's REST API lives. A function, and a
// variable, so tests can point every server at one stub.
var filejumpAPI = func(server string) string { return "https://" + server + "/api/v1" }

const (
	// filejumpTUSServer is the deployment whose upload and download routes
	// changed: it takes uploads over tus, the resumable-upload protocol,
	// where the older servers hand out a signed URL on their object store
	// and take the file there. The two are not interchangeable, so the
	// server name decides which one a part goes up by.
	filejumpTUSServer = "eu.filejump.com"

	// filejumpChunkSize is how much of a part goes in one tus PATCH. It is
	// what FileJump's own web client sends; a SAND part is around half a
	// 16 MiB chunk, so most go up in two or three.
	filejumpChunkSize = 4 << 20

	filejumpPageSize = 1000
)

// filejumpProvider stores each shard as a file in a single FileJump folder.
// FileJump is a BeDrive deployment, as Drime is, so everything is addressed by
// ID rather than by path: the object key is the file's name, and the ID it
// maps to is cached from folder listings.
type filejumpProvider struct {
	base
	token      string
	server     string
	api        string
	folderName string
	// workspaceID is the shared workspace parts live in, or zero for the
	// account's own space, which is what the API calls "personal".
	workspaceID int
	// tus says this server takes uploads over tus and serves downloads by
	// hash; see filejumpTUSServer.
	tus bool

	mu       sync.Mutex
	folderID string
	entries  map[string]filejumpEntry // file name -> what the last listing said
}

// filejumpEntry is what SAND keeps about one file between listings.
type filejumpEntry struct {
	id   string
	size int64
	// hash is the handle the newer server's download route takes. Empty for
	// a file SAND has just uploaded, until the next listing fills it in.
	hash string
}

func newFileJumpProvider(cfg Config) (Provider, error) {
	server, err := filejumpServer(cfg.Option("server"))
	if err != nil {
		return nil, err
	}
	workspace := 0
	if raw := strings.TrimSpace(cfg.Option("workspace_id")); raw != "" {
		workspace, err = strconv.Atoi(raw)
		if err != nil || workspace < 0 {
			return nil, fmt.Errorf("filejump: the workspace ID is a number, not %q", raw)
		}
	}
	return &filejumpProvider{
		base:        base{cfg: cfg},
		token:       strings.TrimSpace(cfg.Option("access_token")),
		server:      server,
		api:         filejumpAPI(server),
		folderName:  strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
		workspaceID: workspace,
		tus:         server == filejumpTUSServer,
		entries:     map[string]filejumpEntry{},
	}, nil
}

// filejumpServer reduces whatever was typed in the server field to a host
// name: the field asks for what the browser's address bar shows, and that is
// as likely to be pasted whole as typed.
func filejumpServer(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("filejump: invalid server %q: %w", raw, err)
		}
		raw = u.Host
	}
	raw = strings.Trim(strings.SplitN(raw, "/", 2)[0], ".")
	if raw == "" || strings.ContainsAny(raw, " \t") {
		return "", fmt.Errorf("filejump: the server is a host name such as eu.filejump.com")
	}
	return raw, nil
}

// filejumpItem is the subset of a FileJump file entry SAND reads.
type filejumpItem struct {
	ID       json.Number `json:"id"`
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	FileSize int64       `json:"file_size"`
	Hash     string      `json:"hash"`
}

// filejumpCreated is the answer to a request that makes a file. The newer
// server answers with the entry itself and the older ones wrap it, so both
// shapes are read and whichever carries an ID is the entry.
type filejumpCreated struct {
	filejumpItem
	FileEntry filejumpItem `json:"fileEntry"`
}

func (c filejumpCreated) entry() filejumpItem {
	if c.FileEntry.ID.String() != "" {
		return c.FileEntry
	}
	return c.filejumpItem
}

// endpoint builds an API URL with the workspace, when there is one, on the
// query string — where every FileJump endpoint but the uploads reads it.
func (p *filejumpProvider) endpoint(route string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	if p.workspaceID != 0 {
		params.Set("workspaceId", strconv.Itoa(p.workspaceID))
	}
	full := p.api + route
	if encoded := params.Encode(); encoded != "" {
		full += "?" + encoded
	}
	return full
}

// do sends a request with the account's token and answers the response,
// having read the body when the status says the request failed.
func (p *filejumpProvider) do(ctx context.Context, op string, req *http.Request) (*http.Response, error) {
	resp, _, err := p.doRetried(ctx, op, req)
	return resp, err
}

// doRetried is do, also reporting whether the request had to be sent more
// than once — which an upload needs to know, since a try that failed on the
// way back may still have landed.
func (p *filejumpProvider) doRetried(ctx context.Context, op string, req *http.Request) (*http.Response, bool, error) {
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/json")
	resp, retried, err := doWithRetry(ctx, req)
	if err != nil {
		return nil, retried, fmt.Errorf("%s: %w", op, err)
	}
	if !isSuccess(resp.StatusCode) {
		defer drainAndClose(resp)
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, retried, &filejumpError{
			status: resp.StatusCode,
			body:   string(snippet),
			err:    httpErrorWith(op, resp.Status, snippet),
		}
	}
	return resp, retried, nil
}

// filejumpError carries the status code of a failed request, and the start
// of its body, alongside the message — so a caller can tell "gone" from
// "broken" without parsing the message text.
type filejumpError struct {
	status int
	body   string
	err    error
}

func (e *filejumpError) Error() string { return e.err.Error() }
func (e *filejumpError) Unwrap() error { return e.err }

// filejumpStatus reports the HTTP status behind an error, or zero when the
// error was not a rejected request.
func filejumpStatus(err error) int {
	var fe *filejumpError
	if errors.As(err, &fe) {
		return fe.status
	}
	return 0
}

// filejumpEntryUnknown reports whether a delete failed because FileJump no
// longer has the entry — which is what a delete wanted anyway. The reading
// is Drime's, since the two run the same software; see drimeEntryUnknown.
func filejumpEntryUnknown(err error) bool {
	var fe *filejumpError
	if !errors.As(err, &fe) {
		return false
	}
	return laravelEntryMissing(fe.status, fe.body)
}

// postJSON sends a JSON body and decodes a JSON answer into out, when out is
// not nil.
func (p *filejumpProvider) postJSON(ctx context.Context, op, rawURL string, payload, out any) error {
	_, err := p.postJSONRetried(ctx, op, rawURL, payload, out)
	return err
}

// postJSONRetried is postJSON, also reporting whether the request was sent
// more than once.
func (p *filejumpProvider) postJSONRetried(ctx context.Context, op, rawURL string, payload, out any) (bool, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, retried, err := p.doRetried(ctx, op, req)
	if err != nil {
		return retried, err
	}
	defer drainAndClose(resp)
	return retried, decodeFileJump(op, resp, out)
}

// getJSON fetches a resource into out.
func (p *filejumpProvider) getJSON(ctx context.Context, op, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, op, req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	return decodeFileJump(op, resp, out)
}

func decodeFileJump(op string, resp *http.Response, out any) error {
	raw, err := readAllBody(resp)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: parsing response: %w", op, err)
	}
	return nil
}

// listFolder returns every entry directly inside a folder, following the
// page numbers. An empty folderID lists the root of the account.
//
// The newer server says where the next page is; the older ones say how many
// there are. Either is followed, and a page with nothing on it is the end
// whatever the numbers say.
func (p *filejumpProvider) listFolder(ctx context.Context, folderID string) ([]filejumpItem, error) {
	var out []filejumpItem
	for page := 1; ; {
		params := url.Values{
			"perPage": {strconv.Itoa(filejumpPageSize)},
			"page":    {strconv.Itoa(page)},
		}
		if folderID != "" {
			params.Set("folderId", folderID)
		}

		var listing struct {
			Data        []filejumpItem `json:"data"`
			NextPage    *int           `json:"next_page"`
			CurrentPage int            `json:"current_page"`
			LastPage    int            `json:"last_page"`
		}
		if err := p.getJSON(ctx, "filejump list", p.endpoint("/drive/file-entries", params), &listing); err != nil {
			return nil, err
		}
		out = append(out, listing.Data...)
		if len(listing.Data) == 0 {
			return out, nil
		}
		switch {
		case listing.NextPage != nil && *listing.NextPage > page:
			page = *listing.NextPage
		case listing.NextPage == nil && listing.LastPage > listing.CurrentPage && listing.CurrentPage >= page:
			page = listing.CurrentPage + 1
		default:
			return out, nil
		}
	}
}

// folder resolves — creating if necessary — the folder shards live in. The
// lookup and the creation happen under one lock, so three parts of one file
// arriving together create the folder once rather than racing to make three.
func (p *filejumpProvider) folder(ctx context.Context) (string, error) {
	if p.folderName == "" {
		return "", nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.folderID != "" {
		return p.folderID, nil
	}

	items, err := p.listFolder(ctx, "")
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Type == "folder" && item.Name == p.folderName {
			p.folderID = item.ID.String()
			return p.folderID, nil
		}
	}

	var created struct {
		Folder filejumpItem `json:"folder"`
	}
	err = p.postJSON(ctx, "filejump create folder", p.endpoint("/folders", nil),
		map[string]any{"name": p.folderName}, &created)
	if err != nil {
		return "", err
	}
	if created.Folder.ID.String() == "" {
		return "", fmt.Errorf("filejump create folder: no folder ID in the response")
	}
	p.folderID = created.Folder.ID.String()
	return p.folderID, nil
}

// refresh lists the shard folder and replaces what is known about the files
// in it, answering with the entry for key if there is one.
func (p *filejumpProvider) refresh(ctx context.Context, key string) (filejumpEntry, bool, error) {
	folderID, err := p.folder(ctx)
	if err != nil {
		return filejumpEntry{}, false, err
	}
	items, err := p.listFolder(ctx, folderID)
	if err != nil {
		return filejumpEntry{}, false, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = make(map[string]filejumpEntry, len(items))
	for _, item := range items {
		if item.Type == "folder" {
			continue
		}
		p.entries[item.Name] = filejumpEntry{id: item.ID.String(), size: item.FileSize, hash: item.Hash}
	}
	entry, ok := p.entries[key]
	return entry, ok, nil
}

// entryFor resolves an object key to the file holding it, filling the cache
// from a folder listing on a miss.
func (p *filejumpProvider) entryFor(ctx context.Context, key string) (filejumpEntry, error) {
	p.mu.Lock()
	entry, ok := p.entries[key]
	p.mu.Unlock()
	if ok {
		return entry, nil
	}
	entry, ok, err := p.refresh(ctx, key)
	if err != nil {
		return filejumpEntry{}, err
	}
	if !ok {
		return filejumpEntry{}, ErrNotFound
	}
	return entry, nil
}

func (p *filejumpProvider) forget(key string) {
	p.mu.Lock()
	delete(p.entries, key)
	p.mu.Unlock()
}

func (p *filejumpProvider) remember(key string, entry filejumpEntry) {
	p.mu.Lock()
	p.entries[key] = entry
	p.mu.Unlock()
}

// parentField is the folder as an upload names it: the ID as a number, or
// nothing at all for the root, which the API spells as an empty string.
func (p *filejumpProvider) parentField(folderID string) any {
	if folderID == "" {
		return ""
	}
	return json.Number(folderID)
}

// Put uploads the shard, then removes any file already under that name.
// FileJump has no overwrite: a second upload under a name is a second file.
// The new one goes up first so that a failed upload leaves the old part in
// place rather than leaving nothing.
//
// FileJump refuses a name under three characters. A SAND key is never that
// short, so no padding is done here.
func (p *filejumpProvider) Put(ctx context.Context, key string, data []byte) error {
	folderID, err := p.folder(ctx)
	if err != nil {
		return err
	}
	previous, err := p.entryFor(ctx, key)
	if err != nil && err != ErrNotFound {
		return err
	}

	var uploaded filejumpItem
	var retried bool
	if p.tus {
		uploaded, retried, err = p.uploadTUS(ctx, folderID, key, data)
	} else {
		uploaded, retried, err = p.uploadSigned(ctx, folderID, key, data)
	}
	if err != nil {
		return err
	}
	if uploaded.ID.String() == "" {
		return fmt.Errorf("filejump upload: no file ID in the response")
	}
	p.remember(key, filejumpEntry{id: uploaded.ID.String(), size: int64(len(data)), hash: uploaded.Hash})

	if previous.id != "" && previous.id != uploaded.ID.String() {
		if err := p.deleteEntry(ctx, previous.id); err != nil {
			return fmt.Errorf("filejump upload: replacing the old copy: %w", err)
		}
	}
	if retried {
		// A try that came back as a 5xx may still have made a file before
		// the answer was lost, and FileJump has no overwrite to fold it into
		// the one that succeeded. A listing says whether it did.
		if err := p.dedupe(ctx, key, uploaded.ID.String()); err != nil {
			return fmt.Errorf("filejump upload: clearing a duplicate left by a retry: %w", err)
		}
	}
	return nil
}

// dedupe erases every file under key other than the one to keep, then puts
// the kept one back in the cache, since the listing that found the others
// may have cached one of them instead.
func (p *filejumpProvider) dedupe(ctx context.Context, key, keep string) error {
	folderID, err := p.folder(ctx)
	if err != nil {
		return err
	}
	items, err := p.listFolder(ctx, folderID)
	if err != nil {
		return err
	}
	var kept filejumpEntry
	for _, item := range items {
		if item.Type == "folder" || item.Name != key {
			continue
		}
		if item.ID.String() == keep {
			kept = filejumpEntry{id: keep, size: item.FileSize, hash: item.Hash}
			continue
		}
		if err := p.deleteEntry(ctx, item.ID.String()); err != nil {
			return err
		}
	}
	if kept.id != "" {
		p.remember(key, kept)
	}
	return nil
}

// filejumpExtension is what an upload declares as the file's extension.
// FileJump wants one; a SAND key carries its own.
func filejumpExtension(key string) string {
	if ext := strings.TrimPrefix(path.Ext(key), "."); ext != "" {
		return ext
	}
	return "bin"
}

// uploadSigned is the older servers' upload: FileJump signs a URL on its
// object store, the bytes go there in one PUT, and an entries call turns
// what landed into a file in the account.
func (p *filejumpProvider) uploadSigned(ctx context.Context, folderID, key string, data []byte) (filejumpItem, bool, error) {
	extension := filejumpExtension(key)

	var signed struct {
		URL    string `json:"url"`
		Key    string `json:"key"`
		ACL    string `json:"acl"`
		Status string `json:"status"`
	}
	presign := map[string]any{
		"filename":     key,
		"mime":         "application/octet-stream",
		"disk":         "uploads",
		"size":         len(data),
		"extension":    extension,
		"workspaceId":  p.workspaceID,
		"relativePath": "",
	}
	if folderID != "" {
		presign["parentId"] = json.Number(folderID)
	}
	if err := p.postJSON(ctx, "filejump upload", p.endpoint("/s3/simple/presign", nil), presign, &signed); err != nil {
		return filejumpItem{}, false, err
	}
	if signed.URL == "" || signed.Key == "" {
		return filejumpItem{}, false, fmt.Errorf("filejump upload: no signed URL in the response")
	}

	// The URL carries its own signature, so the account token stays off the
	// request: the object store behind it is not FileJump and would refuse
	// a bearer token it has never seen. The PUT is addressed by key, so
	// sending it twice is harmless.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signed.URL, bytes.NewReader(data))
	if err != nil {
		return filejumpItem{}, false, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if signed.ACL != "" {
		req.Header.Set("x-amz-acl", signed.ACL)
	}
	req.ContentLength = int64(len(data))
	resp, _, err := doWithRetry(ctx, req)
	if err != nil {
		return filejumpItem{}, false, fmt.Errorf("filejump upload: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return filejumpItem{}, false, httpError("filejump upload", resp)
	}

	// The bytes are on the object store; this is what makes them a file in
	// the account, under the name and folder SAND asked for.
	entry := map[string]any{
		"workspaceId":     p.workspaceID,
		"parentId":        p.parentField(folderID),
		"relativePath":    "",
		"disk":            "uploads",
		"clientMime":      "application/octet-stream",
		"clientName":      key,
		"filename":        path.Base(signed.Key),
		"size":            len(data),
		"clientExtension": extension,
	}
	var created filejumpCreated
	retried, err := p.postJSONRetried(ctx, "filejump upload", p.endpoint("/s3/entries", nil), entry, &created)
	if err != nil {
		return filejumpItem{}, retried, err
	}
	return created.entry(), retried, nil
}

// uploadTUS is the newer server's upload: a tus session is opened with the
// file's metadata, the bytes go up in PATCHes at their offsets, and an
// entries call turns the finished upload into a file. A failure part-way
// asks the server to drop the session rather than leave it half full.
func (p *filejumpProvider) uploadTUS(ctx context.Context, folderID, key string, data []byte) (filejumpItem, bool, error) {
	session, err := p.openTUS(ctx, folderID, key, len(data))
	if err != nil {
		return filejumpItem{}, false, err
	}
	if err := p.sendTUS(ctx, session, data); err != nil {
		// Best effort: the upload has already failed, and a cancel that
		// fails too has nothing to add to the error the caller gets.
		p.cancelTUS(ctx, session)
		return filejumpItem{}, false, err
	}

	var created filejumpCreated
	retried, err := p.postJSONRetried(ctx, "filejump upload", p.endpoint("/tus/entries", nil),
		map[string]any{"uploadKey": path.Base(session.Path)}, &created)
	if err != nil {
		return filejumpItem{}, retried, err
	}
	return created.entry(), retried, nil
}

// openTUS creates the upload session and answers with where it lives.
func (p *filejumpProvider) openTUS(ctx context.Context, folderID, key string, size int) (*url.URL, error) {
	metadata := [][2]string{
		{"name", key},
		{"clientName", key},
		{"clientExtension", filejumpExtension(key)},
		{"clientMime", "application/octet-stream"},
		{"clientSize", strconv.Itoa(size)},
		{"workspaceId", strconv.Itoa(p.workspaceID)},
		{"parentId", folderID},
	}
	pairs := make([]string, 0, len(metadata))
	for _, kv := range metadata {
		pairs = append(pairs, kv[0]+" "+base64.StdEncoding.EncodeToString([]byte(kv[1])))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.api+"/tus/upload", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(size))
	req.Header.Set("Upload-Metadata", strings.Join(pairs, ","))
	resp, err := p.do(ctx, "filejump upload", req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)

	location := resp.Header.Get("Location")
	if location == "" {
		return nil, fmt.Errorf("filejump upload: the upload session has no address")
	}
	base, err := url.Parse(p.api)
	if err != nil {
		return nil, err
	}
	session, err := base.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("filejump upload: the upload session's address %q: %w", location, err)
	}
	return session, nil
}

// sendTUS sends the bytes in chunks at their offsets. A chunk whose answer
// was lost may well have landed, and the retry that follows is then refused
// as out of step; the server is asked where it got to, and sending goes on
// from there rather than failing a file over bytes it already has.
func (p *filejumpProvider) sendTUS(ctx context.Context, session *url.URL, data []byte) error {
	refusedAt := -1
	for offset := 0; offset < len(data); {
		end := min(offset+filejumpChunkSize, len(data))
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, session.String(), bytes.NewReader(data[offset:end]))
		if err != nil {
			return err
		}
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", strconv.Itoa(offset))
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.ContentLength = int64(end - offset)

		resp, err := p.do(ctx, "filejump upload", req)
		if filejumpStatus(err) == http.StatusConflict {
			if offset == refusedAt {
				// Refused twice at the offset the server itself reported:
				// going round again would not end.
				return fmt.Errorf("filejump upload: the server refuses the upload at offset %d", offset)
			}
			refusedAt = offset
			offset, err = p.tusOffset(ctx, session)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		drainAndClose(resp)
		offset = end
	}
	return nil
}

// tusOffset asks the server how much of the upload it holds.
func (p *filejumpProvider) tusOffset(ctx context.Context, session *url.URL) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, session.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	resp, err := p.do(ctx, "filejump upload", req)
	if err != nil {
		return 0, err
	}
	drainAndClose(resp)
	offset, err := strconv.Atoi(resp.Header.Get("Upload-Offset"))
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("filejump upload: the server did not say how much it holds")
	}
	return offset, nil
}

func (p *filejumpProvider) cancelTUS(ctx context.Context, session *url.URL) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, session.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	if resp, err := p.do(ctx, "filejump cancel upload", req); err == nil {
		drainAndClose(resp)
	}
}

// filejumpHash is the handle the newer server's download route takes for a
// file: its ID and a fixed suffix, base64 without the padding. Listings
// carry it, and this is what to use for a file a listing has not shown yet.
func filejumpHash(id string) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(id+"|pad")), "=")
}

// downloadURL is where a file's bytes come from, which differs between the
// server generations as the uploads do.
func (p *filejumpProvider) downloadURL(entry filejumpEntry) string {
	if !p.tus {
		return p.endpoint("/file-entries/"+entry.id+"/download", nil)
	}
	hash := entry.hash
	if hash == "" {
		hash = filejumpHash(entry.id)
	}
	// This route wants the workspace spelled out even for the personal one.
	// The hash goes on the path as the server handed it out.
	return p.api + "/file-entries/download/" + hash + "?workspaceId=" + strconv.Itoa(p.workspaceID)
}

func (p *filejumpProvider) Get(ctx context.Context, key string) ([]byte, error) {
	entry, err := p.entryFor(ctx, key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.downloadURL(entry), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, "filejump download", req)
	if filejumpStatus(err) == http.StatusNotFound {
		p.forget(key)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	return readAllBody(resp)
}

// Stat answers from a fresh listing rather than the cache, since it is what
// the health check asks and a stale size would tell it a part is fine when
// it is not.
func (p *filejumpProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	entry, found, err := p.refresh(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !found {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: entry.size}, nil
}

func (p *filejumpProvider) Delete(ctx context.Context, key string) error {
	entry, err := p.entryFor(ctx, key)
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	p.forget(key)
	return p.deleteEntry(ctx, entry.id)
}

// deleteEntry erases a file for good. FileJump's delete moves a file to the
// trash unless told otherwise, and a trash full of encrypted parts nobody
// can recognise is quota spent on nothing.
func (p *filejumpProvider) deleteEntry(ctx context.Context, id string) error {
	var result struct {
		Status string            `json:"status"`
		Errors map[string]string `json:"errors"`
	}
	err := p.postJSON(ctx, "filejump delete", p.endpoint("/file-entries/delete", nil),
		map[string]any{"entryIds": []json.Number{json.Number(id)}, "deleteForever": true}, &result)
	if filejumpEntryUnknown(err) {
		// The file is gone, which is what a delete is for.
		return nil
	}
	if err != nil {
		return err
	}
	for name, message := range result.Errors {
		return fmt.Errorf("filejump delete: %s: %s", name, message)
	}
	if result.Status != "" && result.Status != "success" {
		return fmt.Errorf("filejump delete: %s", result.Status)
	}
	return nil
}

func (p *filejumpProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if _, _, err := p.refresh(ctx, ""); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ObjectInfo
	for name, entry := range p.entries {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, ObjectInfo{Key: name, Size: entry.size})
	}
	return out, nil
}

// Ping asks for the account's usage, which is the cheapest request that
// proves the token is good.
func (p *filejumpProvider) Ping(ctx context.Context) error {
	if _, err := p.Usage(ctx); err != nil {
		return fmt.Errorf("cannot reach FileJump: %w", err)
	}
	return nil
}

func (p *filejumpProvider) Usage(ctx context.Context) (Usage, error) {
	var usage struct {
		Used      int64 `json:"used"`
		Available int64 `json:"available"`
	}
	if err := p.getJSON(ctx, "filejump usage", p.endpoint("/user/space-usage", nil), &usage); err != nil {
		return Usage{}, err
	}
	// "available" is the size of the plan, not the room left in it.
	return Usage{Used: usage.Used, Total: usage.Available}, nil
}
