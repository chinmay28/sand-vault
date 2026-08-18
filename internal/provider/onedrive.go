package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func init() {
	Register(Spec{
		Kind:  KindOneDrive,
		Label: "OneDrive",
		Description: "A Microsoft account's OneDrive, personal or work. Sign in and SAND keeps " +
			"its parts in one folder of the drive.",
		DocsURL: "https://learn.microsoft.com/en-us/onedrive/developer/rest-api/",
		Order:   11,
		Fields: []FieldSpec{
			{Key: "client_id", Label: "Application (client) ID", Required: true, Advanced: true},
			{
				Key:      "client_secret",
				Label:    "Client secret",
				Secret:   true,
				Advanced: true,
				Help:     "Only for confidential app registrations. Leave blank for a public client.",
			},
			{Key: "refresh_token", Label: "Refresh token", Secret: true, Required: true, Advanced: true},
			{
				Key:     "folder",
				Label:   "Folder",
				Default: "sand",
				Help:    "Folder in the drive root to store shards in.",
			},
		},
		OAuth: &OAuthSpec{
			SignInLabel: "Continue with Microsoft",
			AuthURL:     "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			TokenURL:    microsoftTokenURL,
			// offline_access is what turns the grant into a refresh token;
			// Files.ReadWrite is scoped to the signed-in user's own drive.
			Scopes:            []string{"offline_access", "Files.ReadWrite", "User.Read"},
			PKCE:              true,
			SecretRequired:    false,
			ClientIDField:     "client_id",
			ClientSecretField: "client_secret",
			RefreshTokenField: "refresh_token",
			ClientIDEnv:       "SAND_MICROSOFT_CLIENT_ID",
			ClientSecretEnv:   "SAND_MICROSOFT_CLIENT_SECRET",
			ConsoleURL:        "https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade",
			ConsoleHelp: "Register an application inside a directory — a personal Microsoft " +
				"account without one creates a free tenant from Manage tenants first — with " +
				"personal Microsoft accounts among the supported account types. Add the " +
				"redirect URI below under Mobile and desktop applications and allow public " +
				"client flows, or as a Web redirect if you also set a client secret.",
			AuthParams: map[string]string{
				"response_mode": "query",
				"prompt":        "select_account",
			},
		},
	}, newOneDriveProvider)
}

// graphAPI is where Microsoft Graph answers. A variable rather than a constant
// so the tests can point the backend at a stub.
var graphAPI = "https://graph.microsoft.com/v1.0"

const (
	microsoftTokenURL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"

	// graphSimpleUploadLimit is where Graph wants an upload session instead of
	// a plain PUT. Well under the documented ceiling, because a single PUT
	// cannot be resumed and shards are not small.
	graphSimpleUploadLimit = 4 << 20

	// graphChunkSize is how much of a shard each upload-session request
	// carries. Graph requires every chunk except the last to be a multiple of
	// 320 KiB.
	graphChunkSize = 10 * 320 << 10
)

// onedriveProvider stores each shard as a file under a folder in the drive,
// addressed by path rather than by item ID.
type onedriveProvider struct {
	oauthBase
	folder string
}

func newOneDriveProvider(cfg Config) (Provider, error) {
	return &onedriveProvider{
		oauthBase: oauthBase{
			base: base{cfg: cfg},
			tokens: &tokenSource{
				tokenURL:     microsoftTokenURL,
				clientID:     cfg.Option("client_id"),
				clientSecret: cfg.Option("client_secret"),
				refreshToken: strings.TrimSpace(cfg.Option("refresh_token")),
				staticToken:  strings.TrimSpace(cfg.Option("access_token")),
				// Microsoft refuses a refresh that does not restate the scopes
				// it was granted.
				scopes: []string{"offline_access", "Files.ReadWrite", "User.Read"},
			},
			refreshField: "refresh_token",
		},
		folder: strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
	}, nil
}

// itemPath maps an object key onto a drive-relative path.
func (p *onedriveProvider) itemPath(key string) string {
	parts := []string{}
	if p.folder != "" {
		parts = append(parts, p.folder)
	}
	if trimmed := strings.Trim(key, "/"); trimmed != "" {
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, "/")
}

// itemURL builds a Graph URL addressing an item by path, with an optional
// action such as :/content appended.
func (p *onedriveProvider) itemURL(path, action string) string {
	if path == "" {
		if action == "" {
			return graphAPI + "/me/drive/root"
		}
		return graphAPI + "/me/drive/root/" + action
	}

	escaped := make([]string, 0, 4)
	for _, segment := range strings.Split(path, "/") {
		escaped = append(escaped, url.PathEscape(segment))
	}
	addr := graphAPI + "/me/drive/root:/" + strings.Join(escaped, "/")
	if action == "" {
		return addr
	}
	return addr + ":/" + action
}

// do sends an authorized request.
func (p *onedriveProvider) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := p.tokens.authorize(ctx, req); err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

// getJSON fetches a Graph resource into out, reporting ErrNotFound for a 404.
func (p *onedriveProvider) getJSON(ctx context.Context, op, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return httpError(op, resp)
	}
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

func (p *onedriveProvider) Put(ctx context.Context, key string, data []byte) error {
	if len(data) > graphSimpleUploadLimit {
		return p.putSession(ctx, key, data)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		p.itemURL(p.itemPath(key), "content?@microsoft.graph.conflictBehavior=replace"),
		bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))

	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive upload: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("onedrive upload", resp)
	}
	return nil
}

// putSession uploads a shard too large for a single request, in the chunks
// Graph's upload sessions expect.
func (p *onedriveProvider) putSession(ctx context.Context, key string, data []byte) error {
	body, err := json.Marshal(map[string]any{
		"item": map[string]string{"@microsoft.graph.conflictBehavior": "replace"},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.itemURL(p.itemPath(key), "createUploadSession"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive upload session: %w", err)
	}
	if !isSuccess(resp.StatusCode) {
		err := httpError("onedrive upload session", resp)
		drainAndClose(resp)
		return err
	}
	raw, err := readAllBody(resp)
	drainAndClose(resp)
	if err != nil {
		return err
	}

	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return fmt.Errorf("onedrive upload session: parsing response: %w", err)
	}
	if session.UploadURL == "" {
		return fmt.Errorf("onedrive upload session: no upload URL returned")
	}

	total := len(data)
	for start := 0; start < total; start += graphChunkSize {
		end := start + graphChunkSize
		if end > total {
			end = total
		}

		// The upload URL carries its own credentials, so these requests are
		// deliberately unauthenticated.
		chunk, err := http.NewRequestWithContext(ctx, http.MethodPut,
			session.UploadURL, bytes.NewReader(data[start:end]))
		if err != nil {
			return err
		}
		chunk.Header.Set("Content-Type", "application/octet-stream")
		chunk.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, total))
		chunk.ContentLength = int64(end - start)

		chunkResp, err := httpClient.Do(chunk)
		if err != nil {
			p.cancelSession(ctx, session.UploadURL)
			return fmt.Errorf("onedrive upload chunk: %w", err)
		}
		ok := isSuccess(chunkResp.StatusCode)
		if !ok {
			err := httpError("onedrive upload chunk", chunkResp)
			drainAndClose(chunkResp)
			p.cancelSession(ctx, session.UploadURL)
			return err
		}
		drainAndClose(chunkResp)
	}
	return nil
}

// cancelSession abandons a half-finished upload so the drive is not left
// holding an incomplete item. Failures here are not worth reporting: the
// session expires on its own.
func (p *onedriveProvider) cancelSession(ctx context.Context, uploadURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, uploadURL, nil)
	if err != nil {
		return
	}
	if resp, err := httpClient.Do(req); err == nil {
		drainAndClose(resp)
	}
}

func (p *onedriveProvider) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.itemURL(p.itemPath(key), "content"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("onedrive download: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("onedrive download", resp)
	}
	return readAllBody(resp)
}

// driveItem is the subset of Graph's driveItem resource SAND reads.
type driveItem struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder,omitempty"`
}

func (p *onedriveProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	var item driveItem
	err := p.getJSON(ctx, "onedrive stat",
		p.itemURL(p.itemPath(key), "")+"?$select=name,size,folder", &item)
	if err != nil {
		return ObjectInfo{}, err
	}
	if item.Folder != nil {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: item.Size}, nil
}

func (p *onedriveProvider) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		p.itemURL(p.itemPath(key), ""), nil)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive delete: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if !isSuccess(resp.StatusCode) {
		return httpError("onedrive delete", resp)
	}
	return nil
}

func (p *onedriveProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	if err := p.walk(ctx, "", prefix, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walk lists one folder and recurses into its children, collecting the keys
// that start with prefix. Graph has no recursive listing that also survives a
// drive with other things in it, so this walks only SAND's own folder.
func (p *onedriveProvider) walk(ctx context.Context, rel, prefix string, out *[]ObjectInfo) error {
	next := p.itemURL(p.itemPath(rel), "children") + "?$select=name,size,folder&$top=200"

	for next != "" {
		var page struct {
			Value    []driveItem `json:"value"`
			NextLink string      `json:"@odata.nextLink"`
		}
		if err := p.getJSON(ctx, "onedrive list", next, &page); err != nil {
			if err == ErrNotFound {
				// The folder has not been created yet: nothing stored.
				return nil
			}
			return err
		}

		for _, item := range page.Value {
			key := strings.TrimPrefix(rel+"/"+item.Name, "/")
			if item.Folder != nil {
				if err := p.walk(ctx, key, prefix, out); err != nil {
					return err
				}
				continue
			}
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			*out = append(*out, ObjectInfo{Key: key, Size: item.Size})
		}
		next = page.NextLink
	}
	return nil
}

func (p *onedriveProvider) Ping(ctx context.Context) error {
	if err := p.getJSON(ctx, "onedrive check", graphAPI+"/me/drive?$select=id", nil); err != nil {
		return fmt.Errorf("cannot reach OneDrive: %w", err)
	}
	return nil
}

// Account reports the signed-in Microsoft account, used to label the
// connection.
func (p *onedriveProvider) Account(ctx context.Context) (string, error) {
	var me struct {
		UserPrincipalName string `json:"userPrincipalName"`
		Mail              string `json:"mail"`
		DisplayName       string `json:"displayName"`
	}
	if err := p.getJSON(ctx, "onedrive account",
		graphAPI+"/me?$select=userPrincipalName,mail,displayName", &me); err != nil {
		return "", err
	}
	for _, candidate := range []string{me.Mail, me.UserPrincipalName, me.DisplayName} {
		if candidate != "" {
			return candidate, nil
		}
	}
	return "", nil
}

func (p *onedriveProvider) Usage(ctx context.Context) (Usage, error) {
	var payload struct {
		Quota struct {
			Total int64 `json:"total"`
			Used  int64 `json:"used"`
		} `json:"quota"`
	}
	if err := p.getJSON(ctx, "onedrive quota", graphAPI+"/me/drive?$select=quota", &payload); err != nil {
		return Usage{}, err
	}
	return Usage{Used: payload.Quota.Used, Total: payload.Quota.Total}, nil
}
