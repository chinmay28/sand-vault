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
		Description: "Nextcloud, ownCloud, pCloud, Koofr, Fastmail Files, FileJump, FolderFort, " +
			"LifetimeCloud, Scramble Cloud, Mega via rclone, or any other WebDAV server. Use an " +
			"app password or access token where the provider offers one.",
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
			{Name: "FileJump", Hint: "https://uploads.filejump.com/dav — or the FileJump backend, which speaks its API"},
			{Name: "FolderFort", Hint: "https://connect.<region>.folderfort.com/ — na, na2, na3, eu or eu2"},
			{Name: "LifetimeCloud", Hint: "https://lifetimecloud.me/dav/<user>/"},
			{Name: "Scramble Cloud", Hint: "http://127.0.0.1:1900, served by Scramble's desktop app on this machine"},
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
				Key:   "filejump",
				Label: "FileJump",
				Help: "Username is your FileJump email. The password is a WebDAV token from the dashboard's " +
					"WebDAV menu — your account password is refused. FileJump also has a backend of its " +
					"own that speaks its API and reports quota.",
				Values: map[string]string{"url": "https://uploads.filejump.com/dav"},
			},
			{
				Key:   "folderfort",
				Label: "FolderFort",
				Help: "Swap in your region: na, na2, na3, eu or eu2, whichever FF Connect shows. Username is " +
					"your account email; the password is your API token. FolderFort allows two connections " +
					"per user, so expect uploads to pace themselves.",
				Values: map[string]string{"url": "https://connect.na.folderfort.com/"},
			},
			{
				Key:   "lifetimecloud",
				Label: "LifetimeCloud",
				Help: "Turn on WebDAV / rclone access in your profile settings, then swap in the username " +
					"it shows and paste the token it generates as the password.",
				Values: map[string]string{"url": "https://lifetimecloud.me/dav/alice/"},
			},
			{
				Key:   "scramble",
				Label: "Scramble Cloud",
				Help: "Scramble has no cloud API: its desktop app serves WebDAV on this machine. Turn WebDAV " +
					"on in the app and use the username and password it shows. The account answers only " +
					"while the app is running.",
				Values: map[string]string{"url": "http://127.0.0.1:1900"},
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
	if err := ValidateKey(key); err != nil {
		return err
	}
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
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
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
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
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
	// Without this, the empty key is the prefix collection, and a DELETE of
	// a collection takes everything in it.
	if err := ValidateKey(key); err != nil {
		return err
	}
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
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

// davPropstat is one group of properties with the status they were answered
// with. A server that could not supply a property SAND asked for answers it
// in a 404 group of its own, and RFC 4918 puts no order on the groups — so
// the properties that were found are in whichever group says 200, not in the
// first.
type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	ContentLength int64 `xml:"getcontentlength"`
	ResourceType  struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

// found returns the properties the server answered 200 for. A group with no
// status at all is taken as found, since a server that omits the status has
// nothing else to put there; a response whose every group is some other
// status has no properties to read.
func (r davResponse) found() (davProp, bool) {
	for _, ps := range r.Propstat {
		if strings.Contains(" "+ps.Status+" ", " 200 ") {
			return ps.Prop, true
		}
	}
	for _, ps := range r.Propstat {
		if strings.TrimSpace(ps.Status) == "" {
			return ps.Prop, true
		}
	}
	return davProp{}, false
}

// davPath reduces an href to the decoded path it names. Servers answer with
// an absolute path or a full URL, percent-encoded either way.
func davPath(href string) string {
	href = strings.TrimSpace(href)
	if strings.Contains(href, "://") {
		if u, err := url.Parse(href); err == nil {
			return u.Path
		}
	}
	if decoded, err := url.PathUnescape(href); err == nil {
		return decoded
	}
	return href
}

// hasPrefixFold is strings.HasPrefix without regard to case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// listedBase is the path the server spells the listed collection with.
//
// The hrefs in a listing are the server's, not the client's, and the two
// differ more often than they agree: Nextcloud answers with the account's
// canonical user id where the URL had a display name, a proxy in front of a
// server strips or adds a path segment, and a case-insensitive server shows
// a folder in whatever casing it was made with. Trimming the configured path
// off such an href trims nothing, and every key came back as a whole href
// that no prefix matched — an account listed as empty, with no error to say
// so, which the orphan scan reads as "nothing to keep".
//
// The listing itself says what the base is: a PROPFIND answers for the
// collection it was made on, and that response is the shortest collection in
// it. It is taken as the base when it is named what the configured path is
// named, case aside — a sub-collection cannot pass that test. Failing that
// the configured path is the base, matched without regard to case.
func listedBase(responses []davResponse, configured string) string {
	var best string
	for _, r := range responses {
		prop, ok := r.found()
		if !ok || prop.ResourceType.Collection == nil {
			continue
		}
		path := davPath(r.Href)
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		if best == "" || len(path) < len(best) {
			best = path
		}
	}
	if best == "" || !strings.EqualFold(lastSegment(best), lastSegment(configured)) {
		return configured
	}
	return best
}

// lastSegment is the name of the folder a collection path ends in.
func lastSegment(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	return trimmed[strings.LastIndex(trimmed, "/")+1:]
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

	configured := strings.TrimSuffix(p.root.Path, "/") + "/" + p.prefix
	base := listedBase(ms.Responses, configured)

	var out []ObjectInfo
	for _, r := range ms.Responses {
		prop, ok := r.found()
		if !ok || prop.ResourceType.Collection != nil {
			continue
		}
		path := davPath(r.Href)
		var key string
		switch {
		case hasPrefixFold(path, base):
			key = path[len(base):]
		case hasPrefixFold(path, configured):
			key = path[len(configured):]
		default:
			// A listing is complete or it is an error. An entry that cannot
			// be related to the folder is not one to drop quietly: dropped,
			// it is a part the orphan scan would take for gone.
			return nil, fmt.Errorf("webdav propfind: %s is not under %s", path, base)
		}
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
