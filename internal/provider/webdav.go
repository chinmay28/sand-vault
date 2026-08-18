package provider

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func init() {
	Register(Spec{
		Kind:  KindWebDAV,
		Label: "WebDAV / Nextcloud",
		Description: "Nextcloud, ownCloud, pCloud, Koofr, Fastmail Files, Mega via rclone, or " +
			"any other WebDAV server. Use an app password where the provider offers one.",
		DocsURL: "https://docs.nextcloud.com/server/latest/user_manual/en/files/access_webdav.html",
		Order:   21,
		// A WebDAV server is a WebDAV server; these are the ones people
		// actually have. The hint is the shape of the URL field, since that is
		// the only part that differs between them.
		Covers: []Service{
			{Name: "Nextcloud, ownCloud", Hint: "https://<server>/remote.php/dav/files/<user>"},
			{Name: "pCloud", Hint: "https://webdav.pcloud.com, or eapi… in the EU region"},
			{Name: "Koofr", Hint: "https://app.koofr.net/dav/Koofr"},
			{Name: "Fastmail Files", Hint: "https://webdav.fastmail.com/"},
			{Name: "Seafile", Hint: "https://<server>/seafdav"},
			{Name: "Yandex Disk", Hint: "https://webdav.yandex.com"},
			{Name: "Hetzner Storage Box", Hint: "https://<box>.your-storagebox.de"},
			{Name: "Synology, QNAP and other NAS boxes", Hint: "the WebDAV address the box serves"},
			{Name: "Infomaniak kDrive, OpenDrive, 4shared", Hint: "the WebDAV address the service shows"},
			{Name: "MEGA, Jottacloud, anything rclone speaks", Hint: "`rclone serve webdav` on your own machine"},
		},
		Presets: []Preset{
			{
				Key:    "nextcloud",
				Label:  "Nextcloud / ownCloud",
				Help:   "Swap in your server and username. Settings → Security → Create app password.",
				Values: map[string]string{"url": "https://cloud.example.com/remote.php/dav/files/alice"},
			},
			{
				Key:    "pcloud",
				Label:  "pCloud",
				Help:   "Use eapi.pcloud.com instead if your account lives in the EU region.",
				Values: map[string]string{"url": "https://webdav.pcloud.com"},
			},
			{
				Key:    "koofr",
				Label:  "Koofr",
				Help:   "Generate an app password under Preferences → Password.",
				Values: map[string]string{"url": "https://app.koofr.net/dav/Koofr"},
			},
			{
				Key:    "fastmail",
				Label:  "Fastmail Files",
				Help:   "Username is your Fastmail address; create an app password with Files access.",
				Values: map[string]string{"url": "https://webdav.fastmail.com/"},
			},
			{
				Key:    "rclone",
				Label:  "rclone serve webdav",
				Help:   "Anything rclone speaks — Mega, Proton Drive, Jottacloud — fronted on your own machine.",
				Values: map[string]string{"url": "http://127.0.0.1:8080"},
			},
		},
		Fields: []FieldSpec{
			{
				Key:         "url",
				Label:       "WebDAV URL",
				Placeholder: "https://cloud.example.com/remote.php/dav/files/alice",
				Required:    true,
			},
			{Key: "username", Label: "Username", Required: true},
			{Key: "password", Label: "Password or app password", Secret: true, Required: true},
			{Key: "prefix", Label: "Folder", Help: "Optional subfolder, e.g. sand/.", Default: "sand"},
		},
	}, newWebDAVProvider)
}

// webdavProvider stores each shard as a file on a WebDAV server.
type webdavProvider struct {
	base
	root     *url.URL
	username string
	password string
	prefix   string
}

func newWebDAVProvider(cfg Config) (Provider, error) {
	raw := strings.TrimSpace(cfg.Option("url"))
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid WebDAV URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid WebDAV URL %q", raw)
	}
	u.Path = "/" + strings.Trim(u.Path, "/")

	return &webdavProvider{
		base:     base{cfg: cfg},
		root:     u,
		username: cfg.Option("username"),
		password: cfg.Option("password"),
		prefix:   normalizePrefix(cfg.Option("prefix")),
	}, nil
}

// objectURL maps an object key to its absolute URL on the server.
func (p *webdavProvider) objectURL(key string) string {
	full := strings.TrimSuffix(p.root.Path, "/") + "/" + p.prefix + key
	u := *p.root
	u.Path = full
	return u.String()
}

func (p *webdavProvider) request(ctx context.Context, method, rawURL string, body []byte) (*http.Response, error) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(reader.Len())
	req.SetBasicAuth(p.username, p.password)
	return httpClient.Do(req)
}

// mkcolAll creates every missing parent collection for a key. WebDAV servers
// reject a PUT into a directory that does not exist yet, and MKCOL is not
// recursive, so the path is walked one level at a time.
func (p *webdavProvider) mkcolAll(ctx context.Context, key string) error {
	dir := p.prefix + key
	idx := strings.LastIndex(dir, "/")
	if idx < 0 {
		return nil
	}
	dir = dir[:idx]

	built := strings.TrimSuffix(p.root.Path, "/")
	for _, segment := range strings.Split(dir, "/") {
		if segment == "" {
			continue
		}
		built += "/" + segment
		u := *p.root
		u.Path = built

		resp, err := p.request(ctx, "MKCOL", u.String(), nil)
		if err != nil {
			return fmt.Errorf("webdav mkcol: %w", err)
		}
		// 405 Method Not Allowed means the collection already exists, which
		// is exactly what we want.
		status := resp.StatusCode
		drainAndClose(resp)
		if !isSuccess(status) && status != http.StatusMethodNotAllowed && status != http.StatusConflict {
			return fmt.Errorf("webdav mkcol %s: %s", built, http.StatusText(status))
		}
	}
	return nil
}

func (p *webdavProvider) Put(ctx context.Context, key string, data []byte) error {
	if err := p.mkcolAll(ctx, key); err != nil {
		return err
	}

	resp, err := p.request(ctx, http.MethodPut, p.objectURL(key), data)
	if err != nil {
		return fmt.Errorf("webdav put: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("webdav put", resp)
	}
	return nil
}

func (p *webdavProvider) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := p.request(ctx, http.MethodGet, p.objectURL(key), nil)
	if err != nil {
		return nil, fmt.Errorf("webdav get: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("webdav get", resp)
	}
	return readAllBody(resp)
}

func (p *webdavProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	resp, err := p.request(ctx, http.MethodHead, p.objectURL(key), nil)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("webdav stat: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return ObjectInfo{}, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return ObjectInfo{}, httpError("webdav stat", resp)
	}
	return ObjectInfo{Key: key, Size: resp.ContentLength}, nil
}

func (p *webdavProvider) Delete(ctx context.Context, key string) error {
	resp, err := p.request(ctx, http.MethodDelete, p.objectURL(key), nil)
	if err != nil {
		return fmt.Errorf("webdav delete: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if !isSuccess(resp.StatusCode) {
		return httpError("webdav delete", resp)
	}
	return nil
}

// davMultistatus is the subset of a PROPFIND response SAND reads.
type davMultistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Prop struct {
				ContentLength int64 `xml:"getcontentlength"`
				ResourceType  struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func (p *webdavProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	body := []byte(`<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/><d:resourcetype/></d:prop></d:propfind>`)

	u := *p.root
	u.Path = strings.TrimSuffix(p.root.Path, "/") + "/" + p.prefix

	req, err := http.NewRequestWithContext(ctx, "PROPFIND", u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Depth", "infinity")
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth(p.username, p.password)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav propfind: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("webdav propfind", resp)
	}

	raw, err := readAllBody(resp)
	if err != nil {
		return nil, err
	}
	var ms davMultistatus
	if err := xml.Unmarshal(raw, &ms); err != nil {
		return nil, fmt.Errorf("webdav propfind: parsing response: %w", err)
	}

	basePath := strings.TrimSuffix(p.root.Path, "/") + "/" + p.prefix
	var out []ObjectInfo
	for _, r := range ms.Responses {
		if len(r.Propstat) == 0 {
			continue
		}
		prop := r.Propstat[0].Prop
		if prop.ResourceType.Collection != nil {
			continue
		}
		href, err := url.PathUnescape(r.Href)
		if err != nil {
			href = r.Href
		}
		// Servers return either an absolute path or a full URL.
		if idx := strings.Index(href, "://"); idx >= 0 {
			if hu, err := url.Parse(href); err == nil {
				href = hu.Path
			}
		}
		key := strings.TrimPrefix(href, basePath)
		if key == "" || !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, ObjectInfo{Key: key, Size: prop.ContentLength})
	}
	return out, nil
}

func (p *webdavProvider) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", p.root.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Depth", "0")
	req.SetBasicAuth(p.username, p.password)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach WebDAV server: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("webdav: credentials rejected")
	}
	if !isSuccess(resp.StatusCode) {
		return httpError("webdav check", resp)
	}
	return nil
}
