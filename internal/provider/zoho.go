package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func init() {
	Register(Spec{
		Kind:  KindZoho,
		Label: "Zoho WorkDrive",
		Description: "A Zoho WorkDrive account, reached through its API. Sign in and SAND keeps " +
			"its parts in one folder under My Folders. Zoho runs a separate service in each " +
			"region, so the sign-in asks which one the account lives in.",
		DocsURL: "https://www.zoho.com/workdrive/developer/docs/api/v1/",
		Order:   14,
		Fields: []FieldSpec{
			{Key: "client_id", Label: "Client ID", Required: true, Advanced: true},
			{Key: "client_secret", Label: "Client secret", Secret: true, Required: true, Advanced: true},
			{Key: "refresh_token", Label: "Refresh token", Secret: true, Required: true, Advanced: true},
			{
				Key:      "region",
				Label:    "Region",
				Default:  "com",
				Required: true,
				Advanced: true,
				Help: "The Zoho region the account lives in, as the end of its web address: " +
					"com, eu, in, com.au, jp or com.cn.",
			},
			{
				Key:     "folder",
				Label:   "Folder",
				Default: "sand",
				Help:    "A folder under My Folders to keep parts in. Created if missing.",
			},
			{
				Key:      "parent_id",
				Label:    "Parent folder ID",
				Advanced: true,
				Help: "Only to keep the folder somewhere other than My Folders — a Team Folder, " +
					"say. The ID is the last part of the folder's address in WorkDrive.",
			},
		},
		OAuth: &OAuthSpec{
			SignInLabel: "Continue with Zoho",
			AuthURL:     "https://accounts.zoho." + regionPlaceholder + "/oauth/v2/auth",
			TokenURL:    zohoTokenURL,
			Scopes:      zohoScopes,
			// Without access_type=offline Zoho issues no refresh token, and
			// without prompt=consent it issues one only the first time.
			AuthParams:        map[string]string{"access_type": "offline", "prompt": "consent"},
			SecretRequired:    true,
			ClientIDField:     "client_id",
			ClientSecretField: "client_secret",
			RefreshTokenField: "refresh_token",
			RegionField:       "region",
			Regions: []Region{
				{Key: "com", Label: "United States and everywhere else (zoho.com)"},
				{Key: "eu", Label: "Europe (zoho.eu)"},
				{Key: "in", Label: "India (zoho.in)"},
				{Key: "com.au", Label: "Australia (zoho.com.au)"},
				{Key: "jp", Label: "Japan (zoho.jp)"},
				{Key: "com.cn", Label: "China (zoho.com.cn)"},
			},
			ClientIDEnv:     "SAND_ZOHO_CLIENT_ID",
			ClientSecretEnv: "SAND_ZOHO_CLIENT_SECRET",
			ConsoleURL:      "https://api-console.zoho.com/",
			ConsoleHelp: "Add a Server-based Application and give it the redirect URI below. Use " +
				"the console for your own region — api-console.zoho.eu in Europe, and so on — " +
				"since a client is registered in one region and signs in nowhere else.",
		},
	}, newZohoProvider)
}

// Zoho's hosts, one set per region. Each region is a separate deployment
// under its own domain — zoho.com, zoho.eu, zoho.in — with its own accounts
// server, its own API and its own download and upload hosts, and an account
// exists in exactly one of them. Variables so the tests can point the backend
// at a stub.
var (
	zohoAPIURL      = "https://workdrive.zoho." + regionPlaceholder + "/api/v1"
	zohoDownloadURL = "https://download.zoho." + regionPlaceholder + "/v1/workdrive"
	zohoUploadURL   = "https://upload.zoho." + regionPlaceholder + "/workdrive-api/v1"
)

const zohoTokenURL = "https://accounts.zoho." + regionPlaceholder + "/oauth/v2/token"

// zohoScopes is what the sign-in asks for: the files, and enough of the team
// and workspace surface to find the account's My Folders, which is where the
// SAND folder is made. The same set rclone's backend uses.
var zohoScopes = []string{
	"aaaserver.profile.read",
	"WorkDrive.team.READ",
	"WorkDrive.workspace.READ",
	"WorkDrive.files.ALL",
	"ZohoFiles.files.ALL",
}

// zohoSimpleUploadLimit is the largest body the one-request upload takes;
// Zoho refuses anything over 250 MB with a 413. Past it the part goes up
// through the streaming endpoint instead. A variable so the tests can drive
// the streaming path without a quarter-gigabyte body.
var zohoSimpleUploadLimit = 200 << 20

const zohoPageSize = 1000

// zohoProvider stores each shard as a file in a single Zoho WorkDrive folder.
// WorkDrive addresses everything by resource ID rather than by path, so the
// object key is the file's name and the ID it maps to is cached from folder
// listings, the same arrangement as Box and Drime.
type zohoProvider struct {
	oauthBase
	api      string
	download string
	upload   string

	folderName string
	// parentID is where the folder is made: a folder ID the account holder
	// named, or empty for My Folders, which is resolved through the account.
	parentID string

	// folderMu serialises resolving the folder, so the first few uploads of a
	// fresh account make one folder between them rather than racing to
	// create several of the same name.
	folderMu sync.Mutex

	mu       sync.Mutex
	folderID string
	entries  map[string]zohoEntry // file name -> what the last listing said

	// noPurge records that emptying the trash was refused — Zoho sells it
	// with the Team and Business plans — so a delete stops asking after the
	// first refusal rather than spending a request on every one.
	noPurge bool
}

// zohoEntry is what SAND keeps about one file between listings.
type zohoEntry struct {
	id   string
	size int64
}

func newZohoProvider(cfg Config) (Provider, error) {
	region := strings.TrimSpace(cfg.Option("region"))
	if err := validateZohoRegion(region); err != nil {
		return nil, err
	}
	return &zohoProvider{
		oauthBase: oauthBase{
			base: base{cfg: cfg},
			tokens: &tokenSource{
				tokenURL:     zohoURL(zohoTokenURL, region),
				clientID:     cfg.Option("client_id"),
				clientSecret: cfg.Option("client_secret"),
				refreshToken: strings.TrimSpace(cfg.Option("refresh_token")),
			},
			refreshField: "refresh_token",
		},
		api:        zohoURL(zohoAPIURL, region),
		download:   zohoURL(zohoDownloadURL, region),
		upload:     zohoURL(zohoUploadURL, region),
		folderName: strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
		parentID:   strings.TrimSpace(cfg.Option("parent_id")),
		entries:    map[string]zohoEntry{},
	}, nil
}

// validateZohoRegion refuses a region that could not be the tail of a Zoho
// domain. The value is spliced into every host the backend talks to, so
// anything but letters and dots would be a different host altogether.
func validateZohoRegion(region string) error {
	if region == "" {
		return fmt.Errorf("zoho: no region set")
	}
	for _, c := range region {
		if (c < 'a' || c > 'z') && c != '.' {
			return fmt.Errorf("zoho: %q is not a region — it is the end of the account's web address, like com or eu", region)
		}
	}
	if strings.HasPrefix(region, ".") || strings.HasSuffix(region, ".") || strings.Contains(region, "..") {
		return fmt.Errorf("zoho: %q is not a region — it is the end of the account's web address, like com or eu", region)
	}
	return nil
}

// zohoURL fills the region into one of the host templates.
func zohoURL(template, region string) string {
	return strings.ReplaceAll(template, regionPlaceholder, region)
}

// do sends a request with the account's token on it, retrying the way the
// other HTTP backends do. Zoho reads its token from a scheme of its own
// rather than Bearer, which is why this does not go through tokenSource's
// authorize. Every response is wrapped in op for the error message.
//
// A 401 that is not Zoho's not-found is tried once more on a fresh token.
// The access token is cached until the expiry the token endpoint stated, and
// a provider that cuts one short — or has had it revoked — would otherwise
// fail every request until the hour was up.
func (p *zohoProvider) do(ctx context.Context, op string, req *http.Request) (*http.Response, error) {
	resp, err := p.send(ctx, op, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || req.GetBody == nil && req.Body != nil && req.Body != http.NoBody {
		return resp, nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	drainAndClose(resp)
	if zohoGone(resp, snippet) {
		// Not the token: the thing asked about is gone. Hand the answer on
		// with its body in place for zohoFail to read.
		resp.Body = io.NopCloser(bytes.NewReader(snippet))
		return resp, nil
	}

	p.tokens.invalidate()
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			resp.Body = io.NopCloser(bytes.NewReader(snippet))
			return resp, nil
		}
		req.Body = body
	}
	return p.send(ctx, op, req)
}

// send puts the account's token on a request and sends it, retrying the way
// the other HTTP backends do.
func (p *zohoProvider) send(ctx context.Context, op string, req *http.Request) (*http.Response, error) {
	token, err := p.tokens.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	req.Header.Set("Authorization", "Zoho-oauthtoken "+token)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/vnd.api+json")
	}
	resp, _, err := doWithRetry(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return resp, nil
}

// zohoGone reports whether a failed response says the thing asked about is
// not there. A 404 is one answer. The other is Zoho's own: a 401 whose body
// names error R008, "unauthorized access", which is what it says of a file
// or folder it no longer has. A 401 saying anything else is a real refusal
// — an expired token, a missing scope — and is not a not-found.
func zohoGone(resp *http.Response, body []byte) bool {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return true
	case http.StatusUnauthorized:
		return bytes.Contains(body, []byte(`"R008"`))
	}
	return false
}

// zohoFail turns a failed response into an error: ErrNotFound where the body
// says the thing is gone, otherwise the status and the start of the body,
// which is where Zoho puts the reason, with the status code kept on the
// error for the callers that act on it.
func zohoFail(op string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if zohoGone(resp, snippet) {
		return ErrNotFound
	}
	return &zohoError{status: resp.StatusCode, err: httpErrorWith(op, resp.Status, snippet)}
}

// zohoError is a refused request, with the status it was refused with.
type zohoError struct {
	status int
	err    error
}

func (e *zohoError) Error() string { return e.err.Error() }
func (e *zohoError) Unwrap() error { return e.err }

// zohoStatus reports the HTTP status behind an error, or 0 for an error that
// is not a refused request — a network failure, a parse error.
func zohoStatus(err error) int {
	var refused *zohoError
	if errors.As(err, &refused) {
		return refused.status
	}
	return 0
}

// decodeZoho reads a successful response into out, when there is one.
func decodeZoho(op string, resp *http.Response, out any) error {
	body, err := readAllBody(resp)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: parsing response: %w", op, err)
	}
	return nil
}

// getJSON fetches a resource into out, reporting ErrNotFound for one that is
// gone.
func (p *zohoProvider) getJSON(ctx context.Context, op, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, op, req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return zohoFail(op, resp)
	}
	return decodeZoho(op, resp, out)
}

// sendJSON posts or patches a JSON body and reads the answer into out.
func (p *zohoProvider) sendJSON(ctx context.Context, op, method, rawURL string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.do(ctx, op, req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return zohoFail(op, resp)
	}
	return decodeZoho(op, resp, out)
}

// zohoItem is the subset of a WorkDrive file resource SAND reads.
type zohoItem struct {
	ID         string `json:"id"`
	Attributes struct {
		Name     string `json:"name"`
		IsFolder bool   `json:"is_folder"`
		// Status is "1" for a live file, "51" for one in the trash and "61"
		// for one erased from it. Zoho writes it as a string in some
		// answers and a number in others, so it is kept raw and compared
		// by its digits.
		Status      json.RawMessage `json:"status"`
		StorageInfo struct {
			Size int64 `json:"size_in_bytes"`
		} `json:"storage_info"`
	} `json:"attributes"`
}

// trashed reports whether the item's status says it is in the trash or past
// it. An item with no status at all is live.
func (it zohoItem) trashed() bool {
	status := strings.Trim(string(it.Attributes.Status), `"`)
	return status == "51" || status == "61"
}

// zohoRecord is the shape every write to the files API takes: one resource,
// its attributes, and the literal type "files".
type zohoRecord struct {
	ID         string         `json:"id,omitempty"`
	Type       string         `json:"type"`
	Attributes map[string]any `json:"attributes"`
}

// listFolder returns every item in a folder, following Zoho's cursor paging.
// The cursor comes back as the URL of the next page; the part of it that
// matters is the page[next] value, which is sent back as a parameter.
func (p *zohoProvider) listFolder(ctx context.Context, folderID string) ([]zohoItem, error) {
	var out []zohoItem
	next := "0"
	seen := map[string]bool{next: true}

	for {
		var page struct {
			Data  []zohoItem `json:"data"`
			Links struct {
				Cursor struct {
					HasNext bool   `json:"has_next"`
					Next    string `json:"next"`
				} `json:"cursor"`
			} `json:"links"`
		}
		params := url.Values{}
		params.Set("page[limit]", fmt.Sprint(zohoPageSize))
		params.Set("page[next]", next)
		addr := p.api + "/files/" + url.PathEscape(folderID) + "/files?" + params.Encode()
		if err := p.getJSON(ctx, "zoho list", addr, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)

		if !page.Links.Cursor.HasNext || len(page.Data) == 0 {
			return out, nil
		}
		cursor, err := zohoNextCursor(page.Links.Cursor.Next)
		if err != nil {
			return nil, err
		}
		if seen[cursor] {
			// A listing that is never complete is one the orphan scan must
			// not act on, so a cursor that goes round is an error rather
			// than a page served twice.
			return nil, fmt.Errorf("zoho list: the next page is %q, which has been read already", cursor)
		}
		seen[cursor] = true
		next = cursor
	}
}

// zohoNextCursor digs the page[next] value out of a next-page link.
func zohoNextCursor(link string) (string, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("zoho list: parsing the next page link %q: %w", link, err)
	}
	cursor := parsed.Query().Get("page[next]")
	if cursor == "" {
		return "", fmt.Errorf("zoho list: the next page link %q names no page", link)
	}
	return cursor, nil
}

// myFolders finds the account's own space — "My Folders" in the app — which
// is the folder parts go in unless the account holder named another. It is
// reached through the team the account belongs to: the user, their first
// team, their membership of it, and that membership's private space.
func (p *zohoProvider) myFolders(ctx context.Context) (string, error) {
	var me struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, "zoho account", p.api+"/users/me", &me); err != nil {
		return "", err
	}
	if me.Data.ID == "" {
		return "", fmt.Errorf("zoho account: the account reported no user ID")
	}

	var teams struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, "zoho teams", p.api+"/users/"+url.PathEscape(me.Data.ID)+"/teams", &teams); err != nil {
		return "", err
	}
	if len(teams.Data) == 0 {
		return "", fmt.Errorf("zoho teams: the account belongs to no WorkDrive team, so it has no My Folders " +
			"— set a parent folder ID instead")
	}

	var member struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, "zoho team member",
		p.api+"/teams/"+url.PathEscape(teams.Data[0].ID)+"/currentuser", &member); err != nil {
		return "", err
	}
	if member.Data.ID == "" {
		return "", fmt.Errorf("zoho team member: the team reported no membership for this account")
	}

	var spaces struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, "zoho my folders",
		p.api+"/users/"+url.PathEscape(member.Data.ID)+"/privatespace", &spaces); err != nil {
		return "", err
	}
	if len(spaces.Data) == 0 || spaces.Data[0].ID == "" {
		return "", fmt.Errorf("zoho my folders: the account has no private space to keep parts in " +
			"— set a parent folder ID instead")
	}
	return spaces.Data[0].ID, nil
}

// folder resolves — creating if necessary — the folder shards live in.
func (p *zohoProvider) folder(ctx context.Context) (string, error) {
	p.mu.Lock()
	cached := p.folderID
	p.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	p.folderMu.Lock()
	defer p.folderMu.Unlock()

	// Whoever held the lock first may have done the work.
	p.mu.Lock()
	cached = p.folderID
	p.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	parent := p.parentID
	if parent == "" {
		var err error
		if parent, err = p.myFolders(ctx); err != nil {
			return "", err
		}
	}
	if p.folderName == "" {
		p.setFolder(parent)
		return parent, nil
	}

	items, err := p.listFolder(ctx, parent)
	if err == ErrNotFound {
		return "", fmt.Errorf("zoho: the parent folder %s is not in the account", parent)
	}
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Attributes.IsFolder && item.Attributes.Name == p.folderName && !item.trashed() {
			p.setFolder(item.ID)
			return item.ID, nil
		}
	}

	var created struct {
		Data zohoItem `json:"data"`
	}
	err = p.sendJSON(ctx, "zoho create folder", http.MethodPost, p.api+"/files", map[string]any{
		"data": zohoRecord{
			Type:       "files",
			Attributes: map[string]any{"name": p.folderName, "parent_id": parent},
		},
	}, &created)
	if err == ErrNotFound {
		return "", fmt.Errorf("zoho create folder: the parent folder %s is not in the account", parent)
	}
	if err != nil {
		return "", err
	}
	if created.Data.ID == "" {
		return "", fmt.Errorf("zoho create folder: %s was made but its ID was not reported", p.folderName)
	}
	p.setFolder(created.Data.ID)
	return created.Data.ID, nil
}

func (p *zohoProvider) setFolder(id string) {
	p.mu.Lock()
	p.folderID = id
	p.mu.Unlock()
}

// refresh lists the folder and makes the listing the whole of what is known
// about it, so a file removed by another client leaves the cache as well.
func (p *zohoProvider) refresh(ctx context.Context) error {
	folderID, err := p.folder(ctx)
	if err != nil {
		return err
	}
	items, err := p.listFolder(ctx, folderID)
	if err == ErrNotFound {
		// The folder is gone from under the cached ID. Forget it, so the
		// next upload makes it again, and say so rather than report every
		// part missing: a folder someone moved is not the same as one
		// someone emptied, and only a listing of the real one can tell.
		p.mu.Lock()
		if p.folderID == folderID {
			p.folderID = ""
		}
		p.mu.Unlock()
		return fmt.Errorf("zoho list: the %s folder is no longer in the account", p.folderName)
	}
	if err != nil {
		return err
	}

	entries := make(map[string]zohoEntry, len(items))
	for _, item := range items {
		if item.Attributes.IsFolder || item.trashed() {
			continue
		}
		entries[item.Attributes.Name] = zohoEntry{id: item.ID, size: item.Attributes.StorageInfo.Size}
	}
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	return nil
}

// entryFor resolves an object key to the file holding it, listing the folder
// on a miss.
func (p *zohoProvider) entryFor(ctx context.Context, key string) (zohoEntry, error) {
	p.mu.Lock()
	entry, ok := p.entries[key]
	p.mu.Unlock()
	if ok {
		return entry, nil
	}
	if err := p.refresh(ctx); err != nil {
		return zohoEntry{}, err
	}
	p.mu.Lock()
	entry, ok = p.entries[key]
	p.mu.Unlock()
	if !ok {
		return zohoEntry{}, ErrNotFound
	}
	return entry, nil
}

func (p *zohoProvider) forget(key string) {
	p.mu.Lock()
	delete(p.entries, key)
	p.mu.Unlock()
}

func (p *zohoProvider) remember(key string, entry zohoEntry) {
	p.mu.Lock()
	p.entries[key] = entry
	p.mu.Unlock()
}

func (p *zohoProvider) Put(ctx context.Context, key string, data []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	folderID, err := p.folder(ctx)
	if err != nil {
		return err
	}

	// Both uploads are asked to replace a file of the same name, so a part
	// written again lands as a new version of the one file rather than a
	// second file the listing cannot tell from the first.
	var id string
	if len(data) <= zohoSimpleUploadLimit {
		id, err = p.uploadSimple(ctx, folderID, key, data)
	} else {
		id, err = p.uploadStream(ctx, folderID, key, data)
	}
	if err == ErrNotFound {
		// The only thing an upload can fail to find is the folder it was
		// aimed at. Forget it, so the next write makes it again, and say
		// so: ErrNotFound from a Put would read as the part being missing.
		p.mu.Lock()
		if p.folderID == folderID {
			p.folderID = ""
		}
		p.mu.Unlock()
		return fmt.Errorf("zoho upload: the %s folder is no longer in the account", p.folderName)
	}
	if err != nil {
		return err
	}
	p.remember(key, zohoEntry{id: id, size: int64(len(data))})
	return nil
}

// zohoUploadResponse is what both upload endpoints answer with: a list of one
// record whose attributes name the resource the bytes became.
type zohoUploadResponse struct {
	Data []struct {
		Attributes struct {
			ResourceID string `json:"resource_id"`
		} `json:"attributes"`
	} `json:"data"`
}

// id returns the uploaded file's ID, or an error if the answer named none.
func (r zohoUploadResponse) id(op string) (string, error) {
	if len(r.Data) == 0 || r.Data[0].Attributes.ResourceID == "" {
		return "", fmt.Errorf("%s: the upload was accepted but no file ID came back", op)
	}
	return r.Data[0].Attributes.ResourceID, nil
}

// uploadSimple posts a part as the multipart form the one-request upload
// wants, and returns the ID it landed under.
func (p *zohoProvider) uploadSimple(ctx context.Context, folderID, key string, data []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("content", key)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	params := url.Values{}
	params.Set("parent_id", folderID)
	params.Set("filename", key)
	params.Set("override-name-exist", "true")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.api+"/upload?"+params.Encode(), bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(body.Len())

	resp, err := p.do(ctx, "zoho upload", req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", zohoFail("zoho upload", resp)
	}
	var uploaded zohoUploadResponse
	if err := decodeZoho("zoho upload", resp, &uploaded); err != nil {
		return "", err
	}
	return uploaded.id("zoho upload")
}

// uploadStream sends a part as a raw body to the streaming upload, which is
// the endpoint for anything the one-request upload is too small for. The
// file's name and destination travel as headers.
func (p *zohoProvider) uploadStream(ctx context.Context, folderID, key string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.upload+"/stream/upload", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-filename", url.QueryEscape(key))
	req.Header.Set("x-parent_id", folderID)
	req.Header.Set("x-override-name-exist", "true")
	req.Header.Set("x-streammode", "1")
	req.Header.Set("upload-id", uuid.NewString())
	req.ContentLength = int64(len(data))

	resp, err := p.do(ctx, "zoho stream upload", req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", zohoFail("zoho stream upload", resp)
	}
	var uploaded zohoUploadResponse
	if err := decodeZoho("zoho stream upload", resp, &uploaded); err != nil {
		return "", err
	}
	return uploaded.id("zoho stream upload")
}

func (p *zohoProvider) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	entry, err := p.entryFor(ctx, key)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.download+"/download/"+url.PathEscape(entry.id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := p.do(ctx, "zoho download", req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		err := zohoFail("zoho download", resp)
		if err == ErrNotFound {
			p.forget(key)
		}
		return nil, err
	}
	return readAllBody(resp)
}

func (p *zohoProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	entry, err := p.entryFor(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}

	// The cache says what the last listing saw; the file itself says
	// whether it is still there, which is the question a health check asks.
	var item struct {
		Data zohoItem `json:"data"`
	}
	err = p.getJSON(ctx, "zoho stat", p.api+"/files/"+url.PathEscape(entry.id), &item)
	if err == ErrNotFound || (err == nil && item.Data.trashed()) {
		p.forget(key)
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	size := item.Data.Attributes.StorageInfo.Size
	if size == 0 {
		size = entry.size
	}
	return ObjectInfo{Key: key, Size: size}, nil
}

func (p *zohoProvider) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	entry, err := p.entryFor(ctx, key)
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	p.forget(key)

	// Zoho's delete is a move to the trash, where the part stays — and
	// stays counted against the quota — until the trash is emptied.
	err = p.sendJSON(ctx, "zoho delete", http.MethodPatch, p.api+"/files", map[string]any{
		"data": []zohoRecord{{
			ID:         entry.id,
			Type:       "files",
			Attributes: map[string]any{"status": "51"},
		}},
	}, nil)
	if err == ErrNotFound {
		// Already gone, which is what a delete is for.
		return nil
	}
	if err != nil {
		return err
	}
	p.purge(ctx, entry.id)
	return nil
}

// purge erases a trashed file for good, where the plan allows it. Emptying
// the trash is a feature of Zoho's Team and Business plans, so a refusal is
// remembered and the trash is left to the account holder from then on. It
// is the best-effort half of a delete: by the time it runs the part is out
// of the folder, so nothing here can fail the delete.
func (p *zohoProvider) purge(ctx context.Context, id string) {
	p.mu.Lock()
	skip := p.noPurge
	p.mu.Unlock()
	if skip {
		return
	}

	err := p.sendJSON(ctx, "zoho purge", http.MethodPatch, p.api+"/files/"+url.PathEscape(id), map[string]any{
		"data": zohoRecord{
			Type:       "files",
			Attributes: map[string]any{"status": "61"},
		},
	}, nil)
	if err == nil || err == ErrNotFound {
		return
	}
	if zohoRefused(err) {
		p.mu.Lock()
		p.noPurge = true
		p.mu.Unlock()
	}
}

// zohoRefused reports whether an error was a 4xx: the service understood
// the request and said no, as opposed to not answering.
func zohoRefused(err error) bool {
	status := zohoStatus(err)
	return status >= 400 && status < 500
}

func (p *zohoProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := p.refresh(ctx); err != nil {
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

// zohoUser is the subset of the signed-in user SAND reads.
type zohoUser struct {
	Data struct {
		ID         string `json:"id"`
		Attributes struct {
			Email       string `json:"email_id"`
			DisplayName string `json:"display_name"`
		} `json:"attributes"`
	} `json:"data"`
}

// Ping asks who is signed in, which is the cheapest request that proves the
// tokens are good.
func (p *zohoProvider) Ping(ctx context.Context) error {
	if err := p.getJSON(ctx, "zoho check", p.api+"/users/me", nil); err != nil {
		return fmt.Errorf("cannot reach Zoho WorkDrive: %w", err)
	}
	return nil
}

// Account reports the signed-in Zoho account, used to label the connection.
func (p *zohoProvider) Account(ctx context.Context) (string, error) {
	var me zohoUser
	if err := p.getJSON(ctx, "zoho account", p.api+"/users/me", &me); err != nil {
		return "", err
	}
	if me.Data.Attributes.Email != "" {
		return me.Data.Attributes.Email, nil
	}
	return me.Data.Attributes.DisplayName, nil
}

// MeasureUsage reports how much the SAND folder holds, which WorkDrive keeps
// on the folder itself. The account's quota is not on any endpoint this
// backend has scopes for, so the figure is measured rather than reported,
// and the account's size is the one its holder declares.
func (p *zohoProvider) MeasureUsage(ctx context.Context) (Usage, error) {
	folderID, err := p.folder(ctx)
	if err != nil {
		return Usage{}, err
	}
	var item struct {
		Data zohoItem `json:"data"`
	}
	if err := p.getJSON(ctx, "zoho usage", p.api+"/files/"+url.PathEscape(folderID), &item); err != nil {
		return Usage{}, err
	}
	return Usage{Used: item.Data.Attributes.StorageInfo.Size, Measured: true, MeasuredAt: time.Now().UTC()}, nil
}
