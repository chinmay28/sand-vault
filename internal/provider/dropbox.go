package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func init() {
	Register(Spec{
		Kind:  KindDropbox,
		Label: "Dropbox",
		Description: "A Dropbox account. Sign in and SAND stores its parts in one folder; " +
			"an app key, secret and refresh token can also be pasted in by hand.",
		DocsURL: "https://www.dropbox.com/developers/documentation/http/documentation",
		Order:   12,
		Fields: []FieldSpec{
			{Key: "app_key", Label: "App key", Help: "Required when using a refresh token.", Advanced: true},
			{Key: "app_secret", Label: "App secret", Secret: true, Advanced: true},
			{Key: "refresh_token", Label: "Refresh token", Secret: true, Advanced: true},
			{Key: "access_token", Label: "Access token", Secret: true, Advanced: true, Help: "Alternative to a refresh token; expires after a few hours."},
			{Key: "prefix", Label: "Folder", Default: "sand", Help: "Folder inside the app or account root."},
		},
		OAuth: &OAuthSpec{
			SignInLabel:       "Continue with Dropbox",
			AuthURL:           "https://www.dropbox.com/oauth2/authorize",
			TokenURL:          dropboxTokenURL,
			SecretRequired:    true,
			ClientIDField:     "app_key",
			ClientSecretField: "app_secret",
			RefreshTokenField: "refresh_token",
			ClientIDEnv:       "SAND_DROPBOX_APP_KEY",
			ClientSecretEnv:   "SAND_DROPBOX_APP_SECRET",
			ConsoleURL:        "https://www.dropbox.com/developers/apps",
			ConsoleSteps: []string{
				"Create an app with scoped access to its own app folder, tick " +
					"files.content.read and files.content.write, and add the redirect URI below.",
			},
			AuthParams: map[string]string{
				// Dropbox hands out a refresh token only when the request says
				// it wants offline access.
				"token_access_type": "offline",
			},
		},
	}, newDropboxProvider)
}

const (
	dropboxRPC      = "https://api.dropboxapi.com/2"
	dropboxContent  = "https://content.dropboxapi.com/2"
	dropboxTokenURL = "https://api.dropbox.com/oauth2/token"
)

// dropboxProvider stores each shard as a file under a folder in the account.
type dropboxProvider struct {
	oauthBase
	prefix string
}

func newDropboxProvider(cfg Config) (Provider, error) {
	if strings.TrimSpace(cfg.Option("access_token")) == "" &&
		strings.TrimSpace(cfg.Option("refresh_token")) == "" {
		return nil, fmt.Errorf("dropbox: provide either a refresh token or an access token")
	}

	return &dropboxProvider{
		oauthBase: oauthBase{
			base: base{cfg: cfg},
			tokens: &tokenSource{
				tokenURL:     dropboxTokenURL,
				clientID:     cfg.Option("app_key"),
				clientSecret: cfg.Option("app_secret"),
				refreshToken: strings.TrimSpace(cfg.Option("refresh_token")),
				staticToken:  strings.TrimSpace(cfg.Option("access_token")),
			},
			refreshField: "refresh_token",
		},
		prefix: strings.Trim(strings.TrimSpace(cfg.Option("prefix")), "/"),
	}, nil
}

// remotePath maps an object key to an absolute Dropbox path.
func (p *dropboxProvider) remotePath(key string) string {
	parts := []string{}
	if p.prefix != "" {
		parts = append(parts, p.prefix)
	}
	parts = append(parts, strings.Trim(key, "/"))
	return "/" + strings.Join(parts, "/")
}

// rpc calls a JSON-in/JSON-out endpoint on api.dropboxapi.com.
func (p *dropboxProvider) rpc(ctx context.Context, path string, payload any) ([]byte, int, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dropboxRPC+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := p.tokens.authorize(ctx, req); err != nil {
		return nil, 0, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer drainAndClose(resp)

	data, readErr := readAllBody(resp)
	if !isSuccess(resp.StatusCode) {
		return data, resp.StatusCode, fmt.Errorf("dropbox %s: %s: %s",
			path, resp.Status, strings.TrimSpace(string(data)))
	}
	return data, resp.StatusCode, readErr
}

func (p *dropboxProvider) Put(ctx context.Context, key string, data []byte) error {
	arg, err := json.Marshal(map[string]any{
		"path":       p.remotePath(key),
		"mode":       "overwrite",
		"autorename": false,
		"mute":       true,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dropboxContent+"/files/upload", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Dropbox-API-Arg", string(arg))
	req.ContentLength = int64(len(data))
	if err := p.tokens.authorize(ctx, req); err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("dropbox upload: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("dropbox upload", resp)
	}
	return nil
}

func (p *dropboxProvider) Get(ctx context.Context, key string) ([]byte, error) {
	arg, err := json.Marshal(map[string]any{"path": p.remotePath(key)})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dropboxContent+"/files/download", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Dropbox-API-Arg", string(arg))
	if err := p.tokens.authorize(ctx, req); err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dropbox download: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound {
		// Dropbox reports a missing path as 409 with a path/not_found tag.
		return nil, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("dropbox download", resp)
	}
	return readAllBody(resp)
}

func (p *dropboxProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	body, status, err := p.rpc(ctx, "/files/get_metadata", map[string]any{"path": p.remotePath(key)})
	if status == http.StatusConflict || status == http.StatusNotFound {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}

	var meta struct {
		Tag  string `json:".tag"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return ObjectInfo{}, fmt.Errorf("dropbox stat: parsing response: %w", err)
	}
	if meta.Tag != "file" {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: meta.Size}, nil
}

func (p *dropboxProvider) Delete(ctx context.Context, key string) error {
	_, status, err := p.rpc(ctx, "/files/delete_v2", map[string]any{"path": p.remotePath(key)})
	if status == http.StatusConflict || status == http.StatusNotFound {
		return nil
	}
	return err
}

func (p *dropboxProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	root := p.remotePath("")
	if root == "/" {
		root = ""
	}
	root = strings.TrimSuffix(root, "/")

	payload := map[string]any{"path": root, "recursive": true}
	endpoint := "/files/list_folder"

	var out []ObjectInfo
	for {
		body, status, err := p.rpc(ctx, endpoint, payload)
		if err != nil {
			if status == http.StatusConflict {
				// Folder does not exist yet — nothing stored.
				return nil, nil
			}
			return nil, err
		}

		var page struct {
			Entries []struct {
				Tag         string `json:".tag"`
				PathDisplay string `json:"path_display"`
				Size        int64  `json:"size"`
			} `json:"entries"`
			Cursor  string `json:"cursor"`
			HasMore bool   `json:"has_more"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("dropbox list: parsing response: %w", err)
		}

		base := root + "/"
		for _, e := range page.Entries {
			if e.Tag != "file" {
				continue
			}
			key := strings.TrimPrefix(e.PathDisplay, base)
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			out = append(out, ObjectInfo{Key: key, Size: e.Size})
		}

		if !page.HasMore {
			return out, nil
		}
		endpoint = "/files/list_folder/continue"
		payload = map[string]any{"cursor": page.Cursor}
	}
}

func (p *dropboxProvider) Ping(ctx context.Context) error {
	// current_account takes a null body rather than an empty object.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dropboxRPC+"/users/get_current_account", nil)
	if err != nil {
		return err
	}
	if err := p.tokens.authorize(ctx, req); err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Dropbox: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("dropbox check", resp)
	}
	return nil
}

// Account reports the signed-in Dropbox account, used to label the connection.
func (p *dropboxProvider) Account(ctx context.Context) (string, error) {
	body, _, err := p.rpc(ctx, "/users/get_current_account", nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		Email string `json:"email"`
		Name  struct {
			DisplayName string `json:"display_name"`
		} `json:"name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.Email != "" {
		return payload.Email, nil
	}
	return payload.Name.DisplayName, nil
}

func (p *dropboxProvider) Usage(ctx context.Context) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dropboxRPC+"/users/get_space_usage", nil)
	if err != nil {
		return Usage{}, err
	}
	if err := p.tokens.authorize(ctx, req); err != nil {
		return Usage{}, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return Usage{}, err
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return Usage{}, httpError("dropbox usage", resp)
	}

	body, err := readAllBody(resp)
	if err != nil {
		return Usage{}, err
	}
	var payload struct {
		Used       int64 `json:"used"`
		Allocation struct {
			Allocated int64 `json:"allocated"`
		} `json:"allocation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Usage{}, err
	}
	return Usage{Used: payload.Used, Total: payload.Allocation.Allocated}, nil
}
