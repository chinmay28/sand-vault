package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			ConsoleHelp: "Create an app with scoped access to its own app folder, tick " +
				"files.content.read and files.content.write, and add the redirect URI below.",
			AuthParams: map[string]string{
				// Dropbox hands out a refresh token only when the request says
				// it wants offline access.
				"token_access_type": "offline",
			},
		},
	}, newDropboxProvider)
}

// Dropbox's two hosts — JSON calls on one, bytes on the other. Variables rather
// than constants so the tests can point the backend at a stub.
var (
	dropboxRPC     = "https://api.dropboxapi.com/2"
	dropboxContent = "https://content.dropboxapi.com/2"
)

const dropboxTokenURL = "https://api.dropbox.com/oauth2/token"

// dropboxErrorMaxBody bounds how much of an error answer is read to find out
// what it says. Dropbox's error bodies are a few hundred bytes of JSON.
const dropboxErrorMaxBody = 8 << 10

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

// Telling "not there" from "not now".
//
// Dropbox answers every endpoint-specific failure with a 409, and the status
// alone says nothing about which: a path that does not exist, a path that is
// a folder, a file somebody else has locked, a write refused for being one of
// too many at once, a restricted-content takedown — all 409. The JSON body is
// where the answer is: an "error" object whose ".tag" names the failure and
// nests the next tag down, and an "error_summary" that spells the same chain
// as "path/not_found/..". Only the chain that says the path is not there is a
// missing object. Everything else, read as missing, would have the health
// check rebuild parts that were never lost and Delete report success over a
// file still on the account.
type dropboxError struct {
	Summary string          `json:"error_summary"`
	Error   json.RawMessage `json:"error"`
}

// dropboxErrorTags returns the chain of tags an error body carries, outermost
// first — "path", "not_found" — reading the nested error object and falling
// back to the summary when the object cannot be followed.
func dropboxErrorTags(body []byte) []string {
	var e dropboxError
	if err := json.Unmarshal(body, &e); err != nil {
		return nil
	}

	var tags []string
	node := e.Error
	for len(node) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(node, &obj); err != nil {
			break
		}
		var tag string
		if err := json.Unmarshal(obj[".tag"], &tag); err != nil || tag == "" {
			break
		}
		tags = append(tags, tag)
		node = obj[tag]
	}
	if len(tags) > 0 {
		return tags
	}

	// The summary is the same chain with slashes, and a trailing one or two
	// — "path/not_found/.." — that are not tags.
	for _, part := range strings.Split(e.Summary, "/") {
		if part != "" && part != ".." && part != "." {
			tags = append(tags, part)
		}
	}
	return tags
}

// dropboxNotFound reports whether a failed call's body says the path is not
// there: path/not_found on a read, path_lookup/not_found on a delete. A path
// that is there but is a folder is not an object either, and a download of it
// says path/not_file.
func dropboxNotFound(body []byte) bool {
	tags := dropboxErrorTags(body)
	if len(tags) < 2 {
		return false
	}
	switch tags[0] {
	case "path", "path_lookup":
		return tags[1] == "not_found" || tags[1] == "not_file"
	}
	return false
}

// dropboxFailure builds the error for a call Dropbox refused, with the
// summary — the part of the body that says why — rather than the whole body.
func dropboxFailure(op string, resp *http.Response, body []byte) error {
	var e dropboxError
	if json.Unmarshal(body, &e) == nil && e.Summary != "" {
		return fmt.Errorf("%s: %s: %s", op, resp.Status, strings.TrimSuffix(e.Summary, "/.."))
	}
	if msg := terseBody(string(body)); msg != "" {
		return fmt.Errorf("%s: %s: %s", op, resp.Status, msg)
	}
	return fmt.Errorf("%s: %s", op, resp.Status)
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
		return data, resp.StatusCode, dropboxFailure("dropbox "+path, resp, data)
	}
	return data, resp.StatusCode, readErr
}

func (p *dropboxProvider) Put(ctx context.Context, key string, data []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
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
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
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

	if resp.StatusCode == http.StatusConflict {
		// A missing path is a 409, but so is every other refusal; the body
		// says which. See dropboxNotFound.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, dropboxErrorMaxBody))
		if dropboxNotFound(body) {
			return nil, ErrNotFound
		}
		return nil, dropboxFailure("dropbox download", resp, body)
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("dropbox download", resp)
	}
	return readAllBody(resp)
}

func (p *dropboxProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	body, status, err := p.rpc(ctx, "/files/get_metadata", map[string]any{"path": p.remotePath(key)})
	if status == http.StatusConflict && dropboxNotFound(body) {
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
	// Without this, the empty key is the prefix folder, and delete_v2 of it
	// takes every part with it.
	if err := ValidateKey(key); err != nil {
		return err
	}
	body, status, err := p.rpc(ctx, "/files/delete_v2", map[string]any{"path": p.remotePath(key)})
	if status == http.StatusConflict && dropboxNotFound(body) {
		// Already gone, which is what a delete is for. Any other 409 — too
		// many writes at once, a locked file, a restricted path — leaves the
		// file where it was, and saying nothing would have the orphan scan
		// count it as removed.
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
			if status == http.StatusConflict && dropboxNotFound(body) {
				// Folder does not exist yet — nothing stored. Any other
				// refusal is a failure to list, not an empty account: the
				// orphan scan and the recovery both act on what a listing
				// says, so it is complete or it is an error.
				return nil, nil
			}
			return nil, err
		}

		var page struct {
			Entries []struct {
				Tag         string `json:".tag"`
				PathDisplay string `json:"path_display"`
				PathLower   string `json:"path_lower"`
				Size        int64  `json:"size"`
			} `json:"entries"`
			Cursor  string `json:"cursor"`
			HasMore bool   `json:"has_more"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("dropbox list: parsing response: %w", err)
		}

		// Dropbox paths are case-insensitive and the account keeps whatever
		// casing the folder was first made with, so a prefix of "sand" lists
		// back as "/Sand/…" on an account where somebody created the folder
		// by hand. The base is matched without regard to case; the key keeps
		// the display casing, which is the casing it was written with, so a
		// key is listed as it was put.
		base := strings.ToLower(root) + "/"
		for _, e := range page.Entries {
			if e.Tag != "file" {
				continue
			}
			full := e.PathDisplay
			if full == "" {
				full = e.PathLower
			}
			if len(full) < len(base) || !strings.EqualFold(full[:len(base)], base) {
				return nil, fmt.Errorf("dropbox list: %s is not under %s", full, root+"/")
			}
			key := full[len(base):]
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
