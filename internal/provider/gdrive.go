package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
)

func init() {
	Register(Spec{
		Kind:  KindGDrive,
		Label: "Google Drive",
		Description: "A Google account's Drive storage. Sign in and SAND gets the drive.file " +
			"scope, which can only see the files it creates — nothing already in the account.",
		DocsURL: "https://developers.google.com/drive/api/quickstart/go",
		Order:   10,
		Fields: []FieldSpec{
			{Key: "client_id", Label: "OAuth client ID", Required: true, Advanced: true},
			{Key: "client_secret", Label: "OAuth client secret", Secret: true, Required: true, Advanced: true},
			{Key: "refresh_token", Label: "Refresh token", Secret: true, Required: true, Advanced: true},
			{
				Key:   "folder",
				Label: "Folder",
				Help: "A folder at the top of My Drive. SAND creates it if it is missing; " +
					"blank means the root.",
			},
			// The folder as an ID rather than a name, from before SAND could
			// make its own. Under the drive.file scope only a folder SAND
			// itself created is one it can see, so a pasted ID almost never
			// worked — but an account that has one and did get it working
			// keeps it, and it outranks the name.
			{
				Key:      "folder_id",
				Label:    "Folder ID",
				Advanced: true,
				Help:     "The ID of a folder SAND can already see. Outranks the folder name.",
			},
		},
		OAuth: &OAuthSpec{
			SignInLabel:       "Continue with Google",
			AuthURL:           "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:          googleTokenURL,
			Scopes:            []string{"https://www.googleapis.com/auth/drive.file"},
			PKCE:              true,
			SecretRequired:    true,
			ClientIDField:     "client_id",
			ClientSecretField: "client_secret",
			RefreshTokenField: "refresh_token",
			ClientIDEnv:       "SAND_GOOGLE_CLIENT_ID",
			ClientSecretEnv:   "SAND_GOOGLE_CLIENT_SECRET",
			ConsoleURL:        "https://console.cloud.google.com/apis/credentials",
			ConsoleHelp: "Enable the Google Drive API, then create an OAuth client ID of type " +
				"“Web application” and add the redirect URI below to it.",
			AuthParams: map[string]string{
				// Google only parts with a refresh token when asked, and only
				// re-issues one on a fresh consent.
				"access_type": "offline",
				"prompt":      "consent",
			},
		},
	}, newGDriveProvider)
}

// Variables rather than constants so a test can stand a stub in for Drive.
var (
	gdriveAPI       = "https://www.googleapis.com/drive/v3"
	gdriveUploadAPI = "https://www.googleapis.com/upload/drive/v3"
)

const googleTokenURL = "https://oauth2.googleapis.com/token"

const gdriveFolderMIME = "application/vnd.google-apps.folder"

// gdriveProvider stores each shard as a Drive file. Drive has no real paths,
// so the SAND object key is recorded in the file's appProperties and used as
// the lookup index.
//
// The folder is a boundary, not a decoration. Every query this backend makes
// is scoped to it — the key lookup, the listing, the upload's parent — so two
// providers on the same account with different folders never see each other's
// objects, even under the same keys. That is what lets one Drive account hold
// more than one role, and it only holds because the scoping is on every path:
// an unscoped lookup would find the other folder's object, and Put, which
// erases the copies it is replacing, would erase it. The root is a folder like
// any other for this purpose: a provider with no folder is scoped to the
// children of the root, not to everything the app can see, or it would list
// and delete the objects of every other provider on the account.
//
// The folder is one SAND makes. The account is connected with the drive.file
// scope, which sees only what the app created, so a folder made in Drive's
// own UI is one this backend cannot write into — a pasted ID of such a folder
// is a 404 on the first upload. Naming the folder instead, and creating it
// here when it is missing, is what makes the field work at all.
type gdriveProvider struct {
	oauthBase

	// folderName is the folder to make and use, at the top of My Drive. Empty
	// means the root — everything the app can see.
	folderName string
	// folderID, when set, is a folder somebody pasted the ID of, and is used
	// as it stands without any lookup.
	folderID string

	mu sync.Mutex
	// folder is the resolved folder's ID once a lookup or a create has
	// found it, cached for the life of the provider.
	folder string
	// idCache memoizes key -> Drive file ID so repeated reads of the same
	// shard skip the lookup query.
	idCache map[string]string
}

func newGDriveProvider(cfg Config) (Provider, error) {
	return &gdriveProvider{
		oauthBase: oauthBase{
			base: base{cfg: cfg},
			tokens: &tokenSource{
				tokenURL:     googleTokenURL,
				clientID:     cfg.Option("client_id"),
				clientSecret: cfg.Option("client_secret"),
				refreshToken: cfg.Option("refresh_token"),
				staticToken:  cfg.Option("access_token"),
			},
			refreshField: "refresh_token",
		},
		folderName: strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
		folderID:   strings.TrimSpace(cfg.Option("folder_id")),
		idCache:    map[string]string{},
	}, nil
}

// resolveFolder returns the ID of the folder every object lives in, or "" for
// the root. A pasted ID is taken as it is. A named folder is looked up among
// the folders at the top of My Drive that the app can see, and created there
// when none is found — once per provider, and then remembered.
func (p *gdriveProvider) resolveFolder(ctx context.Context) (string, error) {
	if p.folderID != "" {
		return p.folderID, nil
	}
	if p.folderName == "" {
		return "", nil
	}

	p.mu.Lock()
	cached := p.folder
	p.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	found, err := p.query(ctx, fmt.Sprintf(
		"name='%s' and mimeType='%s' and 'root' in parents and trashed=false",
		escapeDriveQuery(p.folderName), gdriveFolderMIME))
	if err != nil {
		return "", err
	}
	id := ""
	if len(found) > 0 {
		id = found[0].ID
	} else {
		id, err = p.createFolder(ctx)
		if err != nil {
			return "", err
		}
	}

	p.mu.Lock()
	p.folder = id
	p.mu.Unlock()
	return id, nil
}

// createFolder makes the named folder at the top of My Drive and returns its
// ID. Creating a folder is an ordinary files.create with the folder MIME type,
// which the drive.file scope allows — and the folder, being the app's own, is
// then one the app can see.
func (p *gdriveProvider) createFolder(ctx context.Context) (string, error) {
	body, err := json.Marshal(map[string]any{
		"name":     p.folderName,
		"mimeType": gdriveFolderMIME,
		"parents":  []string{"root"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gdriveAPI+"/files?fields=id", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")

	resp, err := p.do(ctx, req)
	if err != nil {
		return "", fmt.Errorf("drive create folder: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", httpError("drive create folder", resp)
	}
	raw, err := readAllBody(resp)
	if err != nil {
		return "", err
	}
	var created gdriveFile
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("drive create folder: %s was created but its ID was not reported", p.folderName)
	}
	return created.ID, nil
}

// scoped narrows a query to the folder. An empty folder is the root of My
// Drive, which Drive's query language names 'root' — and it is a boundary
// like any other folder: a file created without a parent is a child of the
// root, so an account that has been storing at the root since before this
// was scoped still sees everything it wrote, and nothing another folder
// holds.
func scoped(q, folder string) string {
	if folder == "" {
		folder = "root"
	}
	return fmt.Sprintf("%s and '%s' in parents", q, escapeDriveQuery(folder))
}

func (p *gdriveProvider) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := p.tokens.authorize(ctx, req); err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

// gdriveFile is the subset of Drive's file resource SAND reads.
type gdriveFile struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Size          string            `json:"size"`
	AppProperties map[string]string `json:"appProperties"`
}

// key is the object key the file was stored under: the sandKey property,
// which is what every lookup goes by, and the name only for a file that has
// none — which is not one this backend wrote, but is what the fallback
// listing in List can answer with.
func (f gdriveFile) key() string {
	if key := f.AppProperties["sandKey"]; key != "" {
		return key
	}
	return f.Name
}

// fileIDFor resolves an object key to a Drive file ID.
func (p *gdriveProvider) fileIDFor(ctx context.Context, key string) (string, error) {
	p.mu.Lock()
	cached, ok := p.idCache[key]
	p.mu.Unlock()
	if ok {
		return cached, nil
	}

	folder, err := p.resolveFolder(ctx)
	if err != nil {
		return "", err
	}
	files, err := p.query(ctx, scoped(fmt.Sprintf(
		"appProperties has {key='sandKey' and value='%s'} and trashed=false",
		escapeDriveQuery(key)), folder))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", ErrNotFound
	}

	p.mu.Lock()
	p.idCache[key] = files[0].ID
	p.mu.Unlock()
	return files[0].ID, nil
}

// query runs a files.list request and returns every page of results.
func (p *gdriveProvider) query(ctx context.Context, q string) ([]gdriveFile, error) {
	var out []gdriveFile
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("q", q)
		params.Set("fields", "nextPageToken,files(id,name,size,appProperties)")
		params.Set("pageSize", "1000")
		params.Set("spaces", "drive")
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			gdriveAPI+"/files?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := p.do(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("drive list: %w", err)
		}
		if !isSuccess(resp.StatusCode) {
			err := httpError("drive list", resp)
			drainAndClose(resp)
			return nil, err
		}
		body, err := readAllBody(resp)
		drainAndClose(resp)
		if err != nil {
			return nil, err
		}

		var page struct {
			NextPageToken string       `json:"nextPageToken"`
			Files         []gdriveFile `json:"files"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("drive list: parsing response: %w", err)
		}
		out = append(out, page.Files...)

		if page.NextPageToken == "" {
			return out, nil
		}
		pageToken = page.NextPageToken
	}
}

// Put stores data under key: it uploads a new file, and only then erases the
// other files carrying the key.
//
// In that order, because Drive has no overwrite and the alternative — delete
// the old copy, then upload — destroys the part when the upload fails. A
// part being rewritten is a part the vault already has somewhere, but the
// window between the delete and a failed upload is one where this account
// holds nothing under the key, and a health check in that window rebuilds
// and relocates. Uploading first means the worst a failure leaves is two
// copies, and the next Put — or the next cleanup here — reduces them to one.
func (p *gdriveProvider) Put(ctx context.Context, key string, data []byte) error {
	folder, err := p.resolveFolder(ctx)
	if err != nil {
		return err
	}
	parent := folder
	if parent == "" {
		parent = "root"
	}
	metadata := map[string]any{
		"name":          driveSafeName(key),
		"appProperties": map[string]string{"sandKey": key},
		"parents":       []string{parent},
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	// Drive's multipart upload wants the metadata part first, then the bytes.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := mw.CreatePart(metaHeader)
	if err != nil {
		return err
	}
	if _, err := metaPart.Write(metaJSON); err != nil {
		return err
	}

	dataHeader := textproto.MIMEHeader{}
	dataHeader.Set("Content-Type", "application/octet-stream")
	dataPart, err := mw.CreatePart(dataHeader)
	if err != nil {
		return err
	}
	if _, err := dataPart.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gdriveUploadAPI+"/files?uploadType=multipart&fields=id", bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	req.ContentLength = int64(body.Len())

	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("drive upload: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("drive upload", resp)
	}

	raw, err := readAllBody(resp)
	if err != nil {
		return err
	}
	var created gdriveFile
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		// Without the ID there is no telling the new copy from the ones it
		// replaces, so nothing is erased. The next Put sorts it out.
		return fmt.Errorf("drive upload: %s was stored but its ID was not reported", key)
	}
	p.mu.Lock()
	p.idCache[key] = created.ID
	p.mu.Unlock()

	if err := p.deleteOthers(ctx, key, created.ID, folder); err != nil {
		return fmt.Errorf("drive upload: replacing the old copy of %s: %w", key, err)
	}
	return nil
}

// deleteOthers erases every file in the folder carrying key, other than the
// one to keep: the copy a Put replaced, and any duplicate an earlier failure
// left behind.
func (p *gdriveProvider) deleteOthers(ctx context.Context, key, keep, folder string) error {
	files, err := p.query(ctx, scoped(fmt.Sprintf(
		"appProperties has {key='sandKey' and value='%s'} and trashed=false",
		escapeDriveQuery(key)), folder))
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.ID == keep {
			continue
		}
		if err := p.deleteFile(ctx, f.ID); err != nil {
			return err
		}
	}
	return nil
}

// deleteFile erases one file by ID. A file that is already gone is not an
// error — it is what a delete is for.
func (p *gdriveProvider) deleteFile(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		gdriveAPI+"/files/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("drive delete: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if !isSuccess(resp.StatusCode) {
		return httpError("drive delete", resp)
	}
	return nil
}

func (p *gdriveProvider) Get(ctx context.Context, key string) ([]byte, error) {
	id, err := p.fileIDFor(ctx, key)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gdriveAPI+"/files/"+url.PathEscape(id)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("drive download: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		p.forget(key)
		return nil, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("drive download", resp)
	}
	return readAllBody(resp)
}

func (p *gdriveProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	id, err := p.fileIDFor(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gdriveAPI+"/files/"+url.PathEscape(id)+"?fields=id,name,size", nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("drive stat: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		p.forget(key)
		return ObjectInfo{}, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return ObjectInfo{}, httpError("drive stat", resp)
	}

	body, err := readAllBody(resp)
	if err != nil {
		return ObjectInfo{}, err
	}
	var f gdriveFile
	if err := json.Unmarshal(body, &f); err != nil {
		return ObjectInfo{}, err
	}
	var size int64
	fmt.Sscanf(f.Size, "%d", &size)
	return ObjectInfo{Key: key, Size: size}, nil
}

func (p *gdriveProvider) Delete(ctx context.Context, key string) error {
	id, err := p.fileIDFor(ctx, key)
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	p.forget(key)
	return p.deleteFile(ctx, id)
}

func (p *gdriveProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	folder, err := p.resolveFolder(ctx)
	if err != nil {
		return nil, err
	}
	files, err := p.query(ctx, scoped("appProperties has {key='sandKey' and value!=''} and trashed=false", folder))
	if err != nil {
		// Drive rejects value!='' in some API revisions; fall back to listing
		// the folder and reading names.
		files, err = p.query(ctx, scoped("trashed=false", folder))
		if err != nil {
			return nil, err
		}
	}

	var out []ObjectInfo
	for _, f := range files {
		// The key is the sandKey property, which is what Get and Delete
		// look a file up by; the name is the key flattened into something
		// Drive will show, and reading it back would list a key nothing
		// else can find.
		key := f.key()
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var size int64
		fmt.Sscanf(f.Size, "%d", &size)
		out = append(out, ObjectInfo{Key: key, Size: size})
	}
	return out, nil
}

func (p *gdriveProvider) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gdriveAPI+"/about?fields=user(emailAddress)", nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("cannot reach Google Drive: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("google drive check", resp)
	}
	return nil
}

// Account reports the signed-in Google account, used to label the connection.
func (p *gdriveProvider) Account(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gdriveAPI+"/about?fields=user(emailAddress)", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", httpError("google drive account", resp)
	}
	body, err := readAllBody(resp)
	if err != nil {
		return "", err
	}
	var payload struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	return payload.User.EmailAddress, nil
}

func (p *gdriveProvider) Usage(ctx context.Context) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gdriveAPI+"/about?fields=storageQuota", nil)
	if err != nil {
		return Usage{}, err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return Usage{}, err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return Usage{}, httpError("google drive quota", resp)
	}
	body, err := readAllBody(resp)
	if err != nil {
		return Usage{}, err
	}

	var payload struct {
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
		} `json:"storageQuota"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Usage{}, err
	}

	var u Usage
	fmt.Sscanf(payload.StorageQuota.Usage, "%d", &u.Used)
	fmt.Sscanf(payload.StorageQuota.Limit, "%d", &u.Total)
	return u, nil
}

func (p *gdriveProvider) forget(key string) {
	p.mu.Lock()
	delete(p.idCache, key)
	p.mu.Unlock()
}

// driveSafeName flattens an object key into a single Drive filename, since
// Drive has no directories in the path sense. Shard keys are already flat, so
// this only bites for a key that came from somewhere else.
func driveSafeName(key string) string {
	return strings.ReplaceAll(strings.TrimPrefix(key, "/"), "/", "_")
}

// escapeDriveQuery escapes the characters that would break out of a quoted
// value in a Drive query expression.
func escapeDriveQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}
