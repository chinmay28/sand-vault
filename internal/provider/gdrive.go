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
				Key:   "folder_id",
				Label: "Folder ID",
				Help:  "Optional Drive folder to store shards in. Blank uses the account root.",
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
			ConsoleSteps: []string{
				"Enable the Google Drive API, then create an OAuth client ID of type " +
					"“Web application” and add the redirect URI below to it.",
				// The step everyone misses. A project starts in Testing, where
				// Google turns away any account not on the test-user list with
				// “Access blocked … has not completed the Google verification
				// process”, and expires refresh tokens after seven days.
				// drive.file is a non-sensitive scope, so publishing needs no
				// verification review.
				"Under APIs & Services → OAuth consent screen → Audience, press " +
					"Publish app. A project left in Testing turns away every account " +
					"that is not on its test-user list, and drops the ones it lets in " +
					"after seven days.",
			},
			AuthParams: map[string]string{
				// Google only parts with a refresh token when asked, and only
				// re-issues one on a fresh consent.
				"access_type": "offline",
				"prompt":      "consent",
			},
		},
	}, newGDriveProvider)
}

const (
	gdriveAPI       = "https://www.googleapis.com/drive/v3"
	gdriveUploadAPI = "https://www.googleapis.com/upload/drive/v3"
	googleTokenURL  = "https://oauth2.googleapis.com/token"
)

// gdriveProvider stores each shard as a Drive file. Drive has no real paths,
// so the SAND object key is recorded in the file's appProperties and used as
// the lookup index.
type gdriveProvider struct {
	oauthBase
	folderID string

	// idCache memoizes key -> Drive file ID so repeated reads of the same
	// shard skip the lookup query.
	mu      sync.Mutex
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
		folderID: strings.TrimSpace(cfg.Option("folder_id")),
		idCache:  map[string]string{},
	}, nil
}

func (p *gdriveProvider) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := p.tokens.authorize(ctx, req); err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

// gdriveFile is the subset of Drive's file resource SAND reads.
type gdriveFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size string `json:"size"`
}

// fileIDFor resolves an object key to a Drive file ID.
func (p *gdriveProvider) fileIDFor(ctx context.Context, key string) (string, error) {
	p.mu.Lock()
	cached, ok := p.idCache[key]
	p.mu.Unlock()
	if ok {
		return cached, nil
	}

	files, err := p.query(ctx, fmt.Sprintf("appProperties has {key='sandKey' and value='%s'} and trashed=false", escapeDriveQuery(key)))
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

func (p *gdriveProvider) Put(ctx context.Context, key string, data []byte) error {
	// Overwrite semantics: drop any existing object under this key first.
	if err := p.Delete(ctx, key); err != nil {
		return err
	}

	metadata := map[string]any{
		"name":          driveSafeName(key),
		"appProperties": map[string]string{"sandKey": key},
	}
	if p.folderID != "" {
		metadata["parents"] = []string{p.folderID}
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
	if err := json.Unmarshal(raw, &created); err == nil && created.ID != "" {
		p.mu.Lock()
		p.idCache[key] = created.ID
		p.mu.Unlock()
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
	p.forget(key)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if !isSuccess(resp.StatusCode) {
		return httpError("drive delete", resp)
	}
	return nil
}

func (p *gdriveProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	files, err := p.query(ctx, "appProperties has {key='sandKey' and value!=''} and trashed=false")
	if err != nil {
		// Drive rejects value!='' in some API revisions; fall back to listing
		// the folder and reading names.
		files, err = p.query(ctx, p.folderScopeQuery())
		if err != nil {
			return nil, err
		}
	}

	var out []ObjectInfo
	for _, f := range files {
		key := f.Name
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var size int64
		fmt.Sscanf(f.Size, "%d", &size)
		out = append(out, ObjectInfo{Key: key, Size: size})
	}
	return out, nil
}

func (p *gdriveProvider) folderScopeQuery() string {
	if p.folderID != "" {
		return fmt.Sprintf("'%s' in parents and trashed=false", escapeDriveQuery(p.folderID))
	}
	return "trashed=false"
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
// Drive has no directories in the path sense.
func driveSafeName(key string) string {
	return strings.ReplaceAll(strings.TrimPrefix(key, "/"), "/", "_")
}

// escapeDriveQuery escapes the characters that would break out of a quoted
// value in a Drive query expression.
func escapeDriveQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}
