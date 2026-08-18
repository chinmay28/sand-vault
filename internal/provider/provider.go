// Package provider abstracts the cloud accounts SAND scatters shards across.
//
// Every backend exposes the same tiny object-store surface — put, get, delete,
// list — so the vault can treat a Google Drive account, an S3 bucket and a
// folder on local disk as interchangeable shard destinations. Nothing in this
// package knows about encryption: it only ever sees opaque, already-encrypted
// blobs.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind identifies a backend implementation.
type Kind string

const (
	KindLocal    Kind = "local"
	KindS3       Kind = "s3"
	KindWebDAV   Kind = "webdav"
	KindGDrive   Kind = "gdrive"
	KindDropbox  Kind = "dropbox"
	KindOneDrive Kind = "onedrive"
	KindBox      Kind = "box"
	KindProton   Kind = "proton"
	KindICloud   Kind = "icloud"

	// The rest of the synced-folder backends, registered from one table in
	// syncfolder.go.
	KindMega       Kind = "mega"
	KindJottacloud Kind = "jottacloud"
	KindSyncCom    Kind = "synccom"
	KindTresorit   Kind = "tresorit"
	KindIcedrive   Kind = "icedrive"
)

// ErrNotFound is returned by Get and Delete when an object does not exist.
var ErrNotFound = errors.New("object not found")

// Config is the persisted description of a connected account. Options holds
// backend-specific settings including credentials, so a Config is only ever
// written to disk inside the encrypted vault.
type Config struct {
	ID      string            `json:"id"`
	Kind    Kind              `json:"kind"`
	Name    string            `json:"name"`
	Options map[string]string `json:"options"`
	AddedAt time.Time         `json:"added_at"`

	// Color is the account's colour in the browser — the stripe down its card
	// and every part badge for a file it holds. Empty means nobody has chosen
	// one and the UI picks; a "#rrggbb" string is a choice, and it stays put as
	// other accounts come and go.
	Color string `json:"color,omitempty"`
}

// NormalizeColor validates a chosen account colour and returns it in the one
// form the vault stores: lower-case "#rrggbb". The shorthand "#abc" and a
// missing "#" are both accepted, since they are what someone types. An empty
// string is valid and means "no choice" — the browser goes back to picking.
func NormalizeColor(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	hex := strings.ToLower(strings.TrimPrefix(trimmed, "#"))
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%q is not a colour — use a hex value like #38bdf8", trimmed)
		}
	}
	switch len(hex) {
	case 6:
		return "#" + hex, nil
	case 3:
		// #abc is #aabbcc. Expanding it here means everything downstream — the
		// stored value, the swatch, a comparison between two accounts — only
		// ever deals with one length.
		return fmt.Sprintf("#%c%c%c%c%c%c", hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]), nil
	default:
		return "", fmt.Errorf("%q is not a colour — use a hex value like #38bdf8", trimmed)
	}
}

// Option returns the named option, or "" when unset.
func (c Config) Option(key string) string {
	if c.Options == nil {
		return ""
	}
	return c.Options[key]
}

// Redacted returns a copy of the config with every secret option replaced by a
// fixed placeholder, safe to hand to the API layer.
func (c Config) Redacted() Config {
	out := c
	out.Options = make(map[string]string, len(c.Options))
	secrets := secretKeys(c.Kind)
	for k, v := range c.Options {
		if v != "" && secrets[k] {
			out.Options[k] = "••••••••"
			continue
		}
		out.Options[k] = v
	}
	return out
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// Usage reports how full an account is. Total <= 0 means the backend does not
// report a quota.
//
// Free is what can still be written, which is not always Total - Used. A
// filesystem keeps a reserve only root may spend, so a local drive with 5 GB
// unused may have 4.7 GB a service running as an ordinary user can put a part
// in; a backend that reports only a quota leaves it zero and Remaining answers
// from the other two.
type Usage struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
	Free  int64 `json:"free,omitempty"`
}

// Remaining is what this account can still take, preferring a figure the
// backend measured over one subtracted from a quota. Zero when nothing is
// known, and never negative — an account over its quota has no room, not
// negative room.
func (u Usage) Remaining() int64 {
	if u.Free > 0 {
		return u.Free
	}
	if u.Total > u.Used {
		return u.Total - u.Used
	}
	return 0
}

// Provider is the object-store surface every backend implements.
type Provider interface {
	Config() Config

	// Put stores data under key, overwriting any existing object.
	Put(ctx context.Context, key string, data []byte) error

	// Get retrieves the object stored under key, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// Stat reports an object's presence and size without downloading it, or
	// ErrNotFound. Used for shard health checks.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Delete removes the object under key. Deleting a missing object is not
	// an error — the operation is idempotent.
	Delete(ctx context.Context, key string) error

	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Ping verifies the account is reachable and the credentials work.
	Ping(ctx context.Context) error
}

// UsageReporter is implemented by backends that can report quota.
type UsageReporter interface {
	Usage(ctx context.Context) (Usage, error)
}

// Identifier is implemented by backends that can say whose account they are
// pointed at — the signed-in email, usually. A freshly authorized account can
// then name itself instead of asking the user to invent a label.
type Identifier interface {
	Account(ctx context.Context) (string, error)
}

// CredentialRotator is implemented by backends whose stored credentials change
// as they are used. Box and Microsoft retire a refresh token the moment it is
// spent, so the replacement has to be written back to the vault or the account
// stops answering as soon as its access token expires.
type CredentialRotator interface {
	// OnCredentialChange registers a sink for option updates that must be
	// persisted. It is called from whichever goroutine was talking to the
	// backend at the time, and must return promptly: anything slow, or
	// anything that takes a lock the caller might already hold, belongs on a
	// goroutine of the sink's own.
	OnCredentialChange(func(map[string]string))
}

// FieldSpec describes one configuration input for a backend. The web UI
// renders its connect form straight from these, so adding a backend does not
// require touching the frontend.
type FieldSpec struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`

	// Directory marks a field that names a folder on the machine SAND is
	// running on, rather than a path inside somebody else's service. The
	// connect form puts a folder picker on those, so the path can be walked to
	// instead of transcribed from a terminal window.
	Directory bool `json:"directory,omitempty"`

	// Advanced fields are the ones a browser sign-in fills in for you. The
	// connect form keeps them behind a disclosure so the common path is a
	// button rather than a form.
	Advanced bool `json:"advanced,omitempty"`
}

// Preset pre-fills a backend's form for one well-known service, so pCloud is
// something you click rather than a WebDAV URL you have to know by heart.
type Preset struct {
	Key    string            `json:"key"`
	Label  string            `json:"label"`
	Help   string            `json:"help,omitempty"`
	Values map[string]string `json:"values"`
}

// Spec describes a backend and everything needed to connect one.
type Spec struct {
	Kind        Kind        `json:"kind"`
	Label       string      `json:"label"`
	Description string      `json:"description"`
	DocsURL     string      `json:"docs_url,omitempty"`
	Fields      []FieldSpec `json:"fields"`
	Presets     []Preset    `json:"presets,omitempty"`

	// OAuth, when set, means this backend can be connected by signing in from
	// the browser instead of pasting credentials.
	OAuth *OAuthSpec `json:"oauth,omitempty"`

	// Order sorts the backends in the connect dialog: the ones you sign in to
	// first, then the ones needing credentials, then the ones on disk.
	Order int `json:"-"`
}

// resolved returns a copy of the spec with everything that depends on how this
// deployment was started filled in, so no caller ever sees a half-answered
// spec.
func (s Spec) resolved() Spec {
	if s.OAuth == nil {
		return s
	}
	oauth := *s.OAuth
	oauth.Configured = oauth.configured()
	s.OAuth = &oauth
	return s
}

var (
	registryMu sync.RWMutex
	registry   = map[Kind]registration{}
)

type registration struct {
	spec    Spec
	factory func(Config) (Provider, error)
}

// Register adds a backend to the registry. It is called from each backend's
// init function.
func Register(spec Spec, factory func(Config) (Provider, error)) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[spec.Kind] = registration{spec: spec, factory: factory}
}

// Specs returns every registered backend spec, in the order the connect dialog
// should offer them.
func Specs() []Spec {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Spec, 0, len(registry))
	for _, reg := range registry {
		out = append(out, reg.spec.resolved())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// SpecFor returns the spec for a kind.
func SpecFor(kind Kind) (Spec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reg, ok := registry[kind]
	if !ok {
		return Spec{}, false
	}
	return reg.spec.resolved(), true
}

// WithDefaults returns a copy of cfg with every unset option filled in from
// the backend's declared defaults. The copy matters: callers hand the same
// options map to more than one thing, and a config that quietly grew fields on
// its way through a constructor is a bug waiting to happen.
func WithDefaults(cfg Config) Config {
	out := cfg
	out.Options = make(map[string]string, len(cfg.Options))
	for key, value := range cfg.Options {
		out.Options[key] = value
	}

	spec, ok := SpecFor(cfg.Kind)
	if !ok {
		return out
	}
	for _, f := range spec.Fields {
		if out.Options[f.Key] == "" && f.Default != "" {
			out.Options[f.Key] = f.Default
		}
	}
	return out
}

// New builds a live provider from a stored config, validating that every
// required option is present.
func New(cfg Config) (Provider, error) {
	registryMu.RLock()
	reg, ok := registry[cfg.Kind]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider kind %q", cfg.Kind)
	}

	cfg = WithDefaults(cfg)
	for _, f := range reg.spec.Fields {
		if f.Required && strings.TrimSpace(cfg.Options[f.Key]) == "" {
			return nil, fmt.Errorf("%s: missing required setting %q", reg.spec.Label, f.Label)
		}
	}

	return reg.factory(cfg)
}

// secretKeys returns the set of option keys marked secret for a kind.
func secretKeys(kind Kind) map[string]bool {
	spec, ok := SpecFor(kind)
	if !ok {
		return nil
	}
	out := make(map[string]bool, len(spec.Fields))
	for _, f := range spec.Fields {
		if f.Secret {
			out[f.Key] = true
		}
	}
	return out
}

// base carries the config for every implementation.
type base struct{ cfg Config }

func (b base) Config() Config { return b.cfg }
