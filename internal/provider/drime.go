package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
)

func init() {
	Register(Spec{
		Kind:  KindDrime,
		Label: "Drime",
		Description: "A Drime account, reached through its API with an access token you create " +
			"in the web app. SAND keeps its parts in one folder, and a part it deletes is " +
			"erased for good rather than left to fill the trash.",
		DocsURL: "https://docs.drime.cloud/",
		Order:   22,
		Fields: []FieldSpec{
			{
				Key:      "access_token",
				Label:    "Access token",
				Secret:   true,
				Required: true,
				Help:     "In the Drime web app: Settings → Developer → create a token, and paste it here.",
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
				Help:     "Only for parts kept in a shared workspace rather than your own space.",
			},
		},
	}, newDrimeProvider)
}

// drimeAPI is where Drime's REST API lives. A variable so tests can point the
// backend at a stub.
var drimeAPI = "https://app.drime.cloud/api/v1"

const (
	// drimeSimpleUploadLimit is the largest body the one-request upload
	// endpoint takes. Anything bigger goes up in parts through the S3-style
	// multipart flow, which is a create, a signed URL per part, and a
	// complete — the same shape the S3 backend speaks, but with Drime
	// signing the part URLs.
	drimeSimpleUploadLimit = 5 << 20

	// drimeChunkSize is the size of each part in a multipart upload. S3
	// refuses parts under 5 MiB except the last, and a SAND part is around
	// half a 16 MiB chunk, so 8 MiB puts most of them up in one or two parts.
	drimeChunkSize = 8 << 20

	drimePageSize = 1000
)

// drimeProvider stores each shard as a file in a single Drime folder. Drime
// addresses everything by ID rather than by path, so the object key is the
// file's name and the ID it maps to is cached from folder listings, the same
// arrangement as Box.
type drimeProvider struct {
	base
	token       string
	folderName  string
	workspaceID string

	mu       sync.Mutex
	folderID string
	entries  map[string]drimeEntry // file name -> what the last listing said
}

// drimeEntry is what SAND keeps about one file between listings.
type drimeEntry struct {
	id   string
	size int64
	// url is where the file downloads from, relative to the API's origin.
	// Empty for a file SAND has just uploaded — the upload response does not
	// carry one — until the next listing fills it in.
	url string
}

func newDrimeProvider(cfg Config) (Provider, error) {
	return &drimeProvider{
		base:        base{cfg: cfg},
		token:       strings.TrimSpace(cfg.Option("access_token")),
		folderName:  strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
		workspaceID: strings.TrimSpace(cfg.Option("workspace_id")),
		entries:     map[string]drimeEntry{},
	}, nil
}

// drimeItem is the subset of a Drime file entry SAND reads.
type drimeItem struct {
	ID       json.Number `json:"id"`
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	FileSize int64       `json:"file_size"`
	URL      string      `json:"url"`
}

// endpoint builds an API URL with the workspace, when there is one, on the
// query string — which is where every Drime endpoint but the upload reads it.
func (p *drimeProvider) endpoint(route string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	if p.workspaceID != "" {
		params.Set("workspaceId", p.workspaceID)
	}
	full := drimeAPI + route
	if encoded := params.Encode(); encoded != "" {
		full += "?" + encoded
	}
	return full
}

// drimeDownloadURL resolves the download address a listing reports. Drime hands
// it out relative to the site's origin, not to the API root.
func drimeDownloadURL(reported string) (string, error) {
	if reported == "" {
		return "", fmt.Errorf("drime: the file has no download address")
	}
	if strings.Contains(reported, "://") {
		return reported, nil
	}
	api, err := url.Parse(drimeAPI)
	if err != nil {
		return "", err
	}
	return api.Scheme + "://" + api.Host + "/" + strings.TrimPrefix(reported, "/"), nil
}

// do sends a request with the account's token and answers the response,
// having read the body when the status says the request failed.
func (p *drimeProvider) do(ctx context.Context, op string, req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if !isSuccess(resp.StatusCode) {
		defer drainAndClose(resp)
		return nil, &drimeError{status: resp.StatusCode, err: httpError(op, resp)}
	}
	return resp, nil
}

// drimeError carries the status code of a failed request alongside the
// message, so a caller can tell "gone" from "broken" without parsing text.
type drimeError struct {
	status int
	err    error
}

func (e *drimeError) Error() string { return e.err.Error() }
func (e *drimeError) Unwrap() error { return e.err }

// drimeStatus reports the HTTP status behind an error, or zero when the error
// was not a rejected request.
func drimeStatus(err error) int {
	var de *drimeError
	if errors.As(err, &de) {
		return de.status
	}
	return 0
}

// postJSON sends a JSON body and decodes a JSON answer into out, when out is
// not nil.
func (p *drimeProvider) postJSON(ctx context.Context, op, rawURL string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.do(ctx, op, req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	return decodeDrime(op, resp, out)
}

// getJSON fetches a resource into out.
func (p *drimeProvider) getJSON(ctx context.Context, op, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, op, req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	return decodeDrime(op, resp, out)
}

func decodeDrime(op string, resp *http.Response, out any) error {
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

// listFolder returns every entry directly inside a folder, following Drime's
// page numbering. An empty folderID lists the root of the account.
func (p *drimeProvider) listFolder(ctx context.Context, folderID string) ([]drimeItem, error) {
	var out []drimeItem
	for page := 1; ; page++ {
		params := url.Values{
			"perPage": {strconv.Itoa(drimePageSize)},
			"page":    {strconv.Itoa(page)},
		}
		if folderID != "" {
			params.Set("folderId", folderID)
		}

		var listing struct {
			Data        []drimeItem `json:"data"`
			CurrentPage int         `json:"current_page"`
			LastPage    int         `json:"last_page"`
		}
		if err := p.getJSON(ctx, "drime list", p.endpoint("/drive/file-entries", params), &listing); err != nil {
			return nil, err
		}
		out = append(out, listing.Data...)
		if len(listing.Data) == 0 || listing.CurrentPage >= listing.LastPage {
			return out, nil
		}
	}
}

// folder resolves — creating if necessary — the folder shards live in. The
// lookup and the creation happen under one lock, so three parts of one file
// arriving together create the folder once rather than racing to make three.
func (p *drimeProvider) folder(ctx context.Context) (string, error) {
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
		Folder drimeItem `json:"folder"`
	}
	err = p.postJSON(ctx, "drime create folder", p.endpoint("/folders", nil),
		map[string]any{"name": p.folderName}, &created)
	if err != nil {
		return "", err
	}
	if created.Folder.ID.String() == "" {
		return "", fmt.Errorf("drime create folder: no folder ID in the response")
	}
	p.folderID = created.Folder.ID.String()
	return p.folderID, nil
}

// refresh lists the shard folder and replaces what is known about the files
// in it, answering with the entry for key if there is one.
func (p *drimeProvider) refresh(ctx context.Context, key string) (drimeEntry, bool, error) {
	folderID, err := p.folder(ctx)
	if err != nil {
		return drimeEntry{}, false, err
	}
	items, err := p.listFolder(ctx, folderID)
	if err != nil {
		return drimeEntry{}, false, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = make(map[string]drimeEntry, len(items))
	for _, item := range items {
		if item.Type == "folder" {
			continue
		}
		p.entries[item.Name] = drimeEntry{id: item.ID.String(), size: item.FileSize, url: item.URL}
	}
	entry, ok := p.entries[key]
	return entry, ok, nil
}

// entryFor resolves an object key to the file holding it, filling the cache
// from a folder listing on a miss.
func (p *drimeProvider) entryFor(ctx context.Context, key string) (drimeEntry, error) {
	p.mu.Lock()
	entry, ok := p.entries[key]
	p.mu.Unlock()
	if ok {
		return entry, nil
	}
	entry, ok, err := p.refresh(ctx, key)
	if err != nil {
		return drimeEntry{}, err
	}
	if !ok {
		return drimeEntry{}, ErrNotFound
	}
	return entry, nil
}

func (p *drimeProvider) forget(key string) {
	p.mu.Lock()
	delete(p.entries, key)
	p.mu.Unlock()
}

func (p *drimeProvider) remember(key string, entry drimeEntry) {
	p.mu.Lock()
	p.entries[key] = entry
	p.mu.Unlock()
}

// Put uploads the shard, then removes any file already under that name.
// Drime has no overwrite: a second upload under a name is a second file. The
// new one goes up first so that a failed upload leaves the old part in place
// rather than leaving nothing.
func (p *drimeProvider) Put(ctx context.Context, key string, data []byte) error {
	folderID, err := p.folder(ctx)
	if err != nil {
		return err
	}
	previous, err := p.entryFor(ctx, key)
	if err != nil && err != ErrNotFound {
		return err
	}

	var uploaded drimeItem
	if len(data) <= drimeSimpleUploadLimit {
		uploaded, err = p.uploadSimple(ctx, folderID, key, data)
	} else {
		uploaded, err = p.uploadMultipart(ctx, folderID, key, data)
	}
	if err != nil {
		return err
	}
	if uploaded.ID.String() == "" {
		return fmt.Errorf("drime upload: no file ID in the response")
	}
	p.remember(key, drimeEntry{id: uploaded.ID.String(), size: int64(len(data)), url: uploaded.URL})

	if previous.id != "" && previous.id != uploaded.ID.String() {
		if err := p.deleteEntry(ctx, previous.id); err != nil {
			return fmt.Errorf("drime upload: replacing the old copy: %w", err)
		}
	}
	return nil
}

// uploadSimple posts a shard as one multipart form, which is how Drime takes
// anything up to a few megabytes.
func (p *drimeProvider) uploadSimple(ctx context.Context, folderID, key string, data []byte) (drimeItem, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fields := map[string]string{"relativePath": key}
	if folderID != "" {
		fields["parentId"] = folderID
	}
	if p.workspaceID != "" {
		fields["workspaceId"] = p.workspaceID
	}
	for name, value := range fields {
		if err := mw.WriteField(name, value); err != nil {
			return drimeItem{}, err
		}
	}
	part, err := mw.CreateFormFile("file", key)
	if err != nil {
		return drimeItem{}, err
	}
	if _, err := part.Write(data); err != nil {
		return drimeItem{}, err
	}
	if err := mw.Close(); err != nil {
		return drimeItem{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, drimeAPI+"/uploads", bytes.NewReader(body.Bytes()))
	if err != nil {
		return drimeItem{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(body.Len())

	resp, err := p.do(ctx, "drime upload", req)
	if err != nil {
		return drimeItem{}, err
	}
	defer drainAndClose(resp)

	var created struct {
		FileEntry drimeItem `json:"fileEntry"`
	}
	if err := decodeDrime("drime upload", resp, &created); err != nil {
		return drimeItem{}, err
	}
	return created.FileEntry, nil
}

// uploadMultipart sends a shard in parts: Drime opens an upload on its own
// object store, signs a URL for each part, and turns the completed upload
// into a file entry. A failure part-way aborts the upload so the pieces are
// not left behind, billed and invisible.
func (p *drimeProvider) uploadMultipart(ctx context.Context, folderID, key string, data []byte) (drimeItem, error) {
	extension := strings.TrimPrefix(path.Ext(key), ".")
	if extension == "" {
		extension = "bin"
	}
	parentID := json.Number(folderID)

	var opened struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
	}
	create := map[string]any{
		"filename":     key,
		"mime":         "application/octet-stream",
		"size":         len(data),
		"extension":    extension,
		"parentId":     parentID,
		"relativePath": key,
	}
	if p.workspaceID != "" {
		create["workspaceId"] = p.workspaceID
	}
	if err := p.postJSON(ctx, "drime upload", drimeAPI+"/s3/multipart/create", create, &opened); err != nil {
		return drimeItem{}, err
	}
	if opened.UploadID == "" || opened.Key == "" {
		return drimeItem{}, fmt.Errorf("drime upload: the multipart upload was not opened")
	}

	entry, err := p.sendParts(ctx, opened.UploadID, opened.Key, parentID, key, extension, data)
	if err != nil {
		// Best effort: the upload has already failed, and an abort that
		// fails too has nothing to add to the error the caller gets.
		_ = p.postJSON(ctx, "drime abort upload", drimeAPI+"/s3/multipart/abort",
			map[string]any{"uploadId": opened.UploadID, "key": opened.Key}, nil)
		return drimeItem{}, err
	}
	return entry, nil
}

func (p *drimeProvider) sendParts(ctx context.Context, uploadID, uploadKey string, parentID json.Number,
	key, extension string, data []byte) (drimeItem, error) {
	count := (len(data) + drimeChunkSize - 1) / drimeChunkSize
	numbers := make([]int, count)
	for i := range numbers {
		numbers[i] = i + 1
	}

	var signed struct {
		URLs []struct {
			URL        string `json:"url"`
			PartNumber int    `json:"partNumber"`
		} `json:"urls"`
	}
	err := p.postJSON(ctx, "drime upload", drimeAPI+"/s3/multipart/batch-sign-part-urls",
		map[string]any{"uploadId": uploadID, "key": uploadKey, "partNumbers": numbers}, &signed)
	if err != nil {
		return drimeItem{}, err
	}
	urls := make(map[int]string, len(signed.URLs))
	for _, u := range signed.URLs {
		urls[u.PartNumber] = u.URL
	}

	type completedPart struct {
		ETag       string `json:"ETag"`
		PartNumber int    `json:"PartNumber"`
	}
	parts := make([]completedPart, 0, count)
	for number := 1; number <= count; number++ {
		partURL, ok := urls[number]
		if !ok {
			return drimeItem{}, fmt.Errorf("drime upload: no signed URL for part %d of %d", number, count)
		}
		start := (number - 1) * drimeChunkSize
		end := min(start+drimeChunkSize, len(data))
		etag, err := p.putPart(ctx, partURL, data[start:end])
		if err != nil {
			return drimeItem{}, fmt.Errorf("drime upload: part %d of %d: %w", number, count, err)
		}
		parts = append(parts, completedPart{ETag: etag, PartNumber: number})
	}

	err = p.postJSON(ctx, "drime upload", drimeAPI+"/s3/multipart/complete",
		map[string]any{"uploadId": uploadID, "key": uploadKey, "parts": parts}, nil)
	if err != nil {
		return drimeItem{}, err
	}

	// The upload is on the object store; this is what makes it a file in the
	// account, under the name and folder SAND asked for.
	entry := map[string]any{
		"clientMime":      "application/octet-stream",
		"clientName":      key,
		"filename":        path.Base(uploadKey),
		"size":            len(data),
		"clientExtension": extension,
		"parentId":        parentID,
		"relativePath":    key,
	}
	if p.workspaceID != "" {
		entry["workspaceId"] = p.workspaceID
	}
	var created struct {
		FileEntry drimeItem `json:"fileEntry"`
	}
	if err := p.postJSON(ctx, "drime upload", drimeAPI+"/s3/entries", entry, &created); err != nil {
		return drimeItem{}, err
	}
	return created.FileEntry, nil
}

// putPart sends one part to the URL Drime signed for it. The URL carries its
// own signature, so the account token stays off the request: the object store
// behind it is not Drime and would refuse a bearer token it has never seen.
func (p *drimeProvider) putPart(ctx context.Context, partURL string, chunk []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, bytes.NewReader(chunk))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk))

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", httpError("upload", resp)
	}
	return resp.Header.Get("ETag"), nil
}

func (p *drimeProvider) Get(ctx context.Context, key string) ([]byte, error) {
	entry, err := p.entryFor(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry.url == "" {
		// A file SAND uploaded has an ID but no address yet; a listing has
		// both.
		var found bool
		entry, found, err = p.refresh(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	target, err := drimeDownloadURL(entry.url)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, "drime download", req)
	if drimeStatus(err) == http.StatusNotFound {
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
func (p *drimeProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	entry, found, err := p.refresh(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !found {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: entry.size}, nil
}

func (p *drimeProvider) Delete(ctx context.Context, key string) error {
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

// deleteEntry erases a file for good. Drime's delete moves a file to the
// trash unless told otherwise, and a trash full of encrypted parts nobody
// can recognise is quota spent on nothing.
func (p *drimeProvider) deleteEntry(ctx context.Context, id string) error {
	var result struct {
		Errors map[string]string `json:"errors"`
	}
	err := p.postJSON(ctx, "drime delete", p.endpoint("/file-entries/delete", nil),
		map[string]any{"entryIds": []string{id}, "deleteForever": true}, &result)
	switch drimeStatus(err) {
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		// Drime answers 422 for an ID it no longer has: the file is gone,
		// which is what a delete is for.
		return nil
	}
	if err != nil {
		return err
	}
	for name, message := range result.Errors {
		return fmt.Errorf("drime delete: %s: %s", name, message)
	}
	return nil
}

func (p *drimeProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
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
func (p *drimeProvider) Ping(ctx context.Context) error {
	if _, err := p.Usage(ctx); err != nil {
		return fmt.Errorf("cannot reach Drime: %w", err)
	}
	return nil
}

func (p *drimeProvider) Usage(ctx context.Context) (Usage, error) {
	var usage struct {
		Used      int64 `json:"used"`
		Available int64 `json:"available"`
	}
	if err := p.getJSON(ctx, "drime usage", p.endpoint("/user/space-usage", nil), &usage); err != nil {
		return Usage{}, err
	}
	// "available" is the size of the plan, not the room left in it.
	return Usage{Used: usage.Used, Total: usage.Available}, nil
}
