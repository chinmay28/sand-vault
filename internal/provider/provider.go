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
	"math"
	"sort"
	"strconv"
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
	KindSFTP     Kind = "sftp"
	KindGDrive   Kind = "gdrive"
	KindDropbox  Kind = "dropbox"
	KindOneDrive Kind = "onedrive"
	KindBox      Kind = "box"
	KindProton   Kind = "proton"
	KindICloud   Kind = "icloud"

	// Proton Drive again, reached through Proton's own command-line client
	// rather than through the folder its desktop app syncs. See protoncli.go
	// for why one service has two backends.
	KindProtonCLI Kind = "protoncli"

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

	// Capacity is how big the account holder says this account is, in bytes,
	// for the backends that cannot say it themselves. A bucket has no quota
	// call — S3 has never had one and B2's own API does not add one — so the
	// figure a bar needs has to come from the person who set the cap in the
	// provider's console, or who decided how much of an unlimited bucket this
	// vault is allowed to fill. Zero means nobody has said, and the account
	// goes on reporting no capacity rather than a guessed one.
	//
	// It is not a limit SAND enforces. Nothing here refuses an upload for
	// crossing it: it is the denominator the usage bar is drawn against, and
	// an account over the figure reads as full rather than as an error.
	Capacity int64 `json:"capacity,omitempty"`
}

// ParseCapacity reads a declared capacity as somebody would write it — "10 GB",
// "1.5t", "500 MiB", or a bare byte count — and returns it in bytes. An empty
// string, a zero, or a dash is "nobody has said" rather than "an account with
// no room", and returns zero.
//
// The units are the ones the rest of SAND prints: a GB here is 1024³, the same
// figure the file list and the account cards mean by GB, so a capacity typed to
// match what the browser shows does not come back a few percent different. GiB
// is accepted and means the same thing.
func ParseCapacity(value string) (int64, error) {
	text := strings.ToLower(strings.TrimSpace(value))
	if text == "" || text == "-" || text == "—" {
		return 0, nil
	}

	digits := strings.TrimRight(text, "abcdefghijklmnopqrstuvwxyz ")
	unit := strings.TrimSpace(strings.TrimPrefix(text, digits))
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "b"), "i")

	size, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size — write it like 10 GB", value)
	}
	if size < 0 {
		return 0, fmt.Errorf("%q is not a size — a capacity cannot be negative", value)
	}

	scale := float64(1)
	switch unit {
	case "":
		// A bare number is bytes, which is what an API client sends and what
		// the field round-trips as.
	case "k":
		scale = 1 << 10
	case "m":
		scale = 1 << 20
	case "g":
		scale = 1 << 30
	case "t":
		scale = 1 << 40
	case "p":
		scale = 1 << 50
	default:
		return 0, fmt.Errorf("%q is not a size — %q is not a unit SAND knows", value, unit)
	}

	bytes := size * scale
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("%q is not a size any account has", value)
	}
	return int64(bytes), nil
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

// RedactedSecret is what a secret option looks like once it has left the
// vault. It is a value in its own right rather than a blank, because the
// browser has to be able to tell "this account has a client secret" from "this
// account has none" — and because an edit that sends it back means "leave the
// one you have alone" (see MergeOptions).
const RedactedSecret = "••••••••"

// Redacted returns a copy of the config with every secret option replaced by a
// fixed placeholder, safe to hand to the API layer.
func (c Config) Redacted() Config {
	out := c
	out.Options = make(map[string]string, len(c.Options))
	secrets := secretKeys(c.Kind)
	for k, v := range c.Options {
		if v != "" && secrets[k] {
			out.Options[k] = RedactedSecret
			continue
		}
		out.Options[k] = v
	}
	return out
}

// MergeOptions folds a set of edited settings into the ones an account is
// already connected with, and reports what the account would then be
// configured as.
//
// Editing a live account's settings is not the same shape as filling in a
// connect form, and the difference is entirely about secrets. The browser is
// never sent a stored one — it gets RedactedSecret — so a form generated from
// the account's current settings can only ever hand a secret back as either
// something newly typed or the placeholder it was given. The placeholder means
// "unchanged", and is the one value a real secret can never be.
//
// Everything else is taken at face value, including the empty string: clearing
// an optional field is how somebody says "no folder, use the root", and it has
// to be distinguishable from not mentioning the field at all. A key the backend
// does not declare is refused rather than stored, so a typo cannot quietly
// become a setting nothing reads.
func MergeOptions(kind Kind, stored, edits map[string]string) (map[string]string, error) {
	spec, ok := SpecFor(kind)
	if !ok {
		return nil, fmt.Errorf("unknown provider kind %q", kind)
	}
	fields := make(map[string]FieldSpec, len(spec.Fields))
	for _, f := range spec.Fields {
		fields[f.Key] = f
	}

	out := make(map[string]string, len(stored)+len(edits))
	for k, v := range stored {
		out[k] = v
	}
	for key, value := range edits {
		field, known := fields[key]
		if !known {
			return nil, fmt.Errorf("%s has no setting called %q", spec.Label, key)
		}
		value = strings.TrimSpace(value)
		if field.Secret && value == RedactedSecret {
			continue
		}
		out[key] = value
	}

	// Defaults fill the blanks the same way they do on a fresh connection, so
	// a field cleared back to empty comes back as whatever the backend would
	// have started it at rather than as nothing.
	for _, f := range spec.Fields {
		if out[f.Key] == "" && f.Default != "" {
			out[f.Key] = f.Default
		}
	}
	for _, f := range spec.Fields {
		if f.Required && strings.TrimSpace(out[f.Key]) == "" {
			return nil, fmt.Errorf("%s cannot be left blank", f.Label)
		}
	}
	return out, nil
}

// SameOptions reports whether two option sets say the same thing, treating a
// missing key and an empty one as the same absence.
func SameOptions(a, b map[string]string) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
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

	// Measured says Used was arrived at by counting what is on the account
	// rather than by asking it. A bucket answers "how full are you" with a
	// listing and nothing else, so the figure costs a request per thousand
	// objects and is taken when somebody asks for it rather than on every ping
	// — and it is worth labelling as counted, since it is a moment's snapshot
	// of somebody else's storage and not a live reading.
	Measured   bool      `json:"measured,omitempty"`
	MeasuredAt time.Time `json:"measured_at,omitzero"`

	// Declared says Total is the figure the account holder typed rather than
	// one the backend reports. The bar is drawn the same either way; the panel
	// says whose number it is, because a wrong quota read off an API is a bug
	// and a wrong one typed into a form is a typo, and they are not fixed in
	// the same place.
	Declared bool `json:"declared,omitempty"`
}

// UsedKnown reports whether Used means anything. A backend that reports
// nothing at all leaves it zero, which is indistinguishable from an account
// with nothing on it until something has actually looked: a quota call sets a
// total, and a measurement says so.
func (u Usage) UsedKnown() bool {
	return (u.Total > 0 && !u.Declared) || u.Measured
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
//
// Called on every ping, so it must be cheap: one request that the service
// answers from its own bookkeeping, or nothing at all. A backend with no such
// call implements UsageMeasurer instead, or neither.
type UsageReporter interface {
	Usage(ctx context.Context) (Usage, error)
}

// UsageMeasurer is implemented by backends whose usage can only be counted.
//
// S3 has no quota call and never has, and Backblaze's own API does not add one
// — the only honest answer to "what is in this bucket" is the sum of a full
// listing, which costs a request per thousand objects and real money at some
// providers. So it is never taken on the sidebar's ping: this is called when
// somebody opens the panel that shows the figure, and the result is what the
// backend's cheap Usage hands back afterwards.
type UsageMeasurer interface {
	MeasureUsage(ctx context.Context) (Usage, error)
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

	// Multiline marks a field whose value has newlines in it, which so far
	// means a private key. Not a cosmetic preference: an <input> silently
	// drops the line breaks out of a pasted PEM block, so a key pasted into
	// one arrives as a single line and does not parse. The form has to render
	// a textarea or the field cannot be filled in at all.
	Multiline bool `json:"multiline,omitempty"`
}

// Preset pre-fills a backend's form for one well-known service, so pCloud is
// something you click rather than a WebDAV URL you have to know by heart.
type Preset struct {
	Key    string            `json:"key"`
	Label  string            `json:"label"`
	Help   string            `json:"help,omitempty"`
	Values map[string]string `json:"values"`
}

// Service names one storage service a backend reaches, for the catalogue the
// browser shows behind the connect dialog. The generic backends are where this
// earns its keep: "S3-compatible storage" is a true description and a useless
// answer to "can it hold my Google Cloud Storage bucket?", which it can.
//
// Hint is what to put in the endpoint or URL field, and is deliberately a
// shape rather than a hostname wherever the real one carries a region or an
// account in it — a pattern stays true when a provider adds a region, and a
// copied-out hostname does not.
type Service struct {
	Name string `json:"name"`
	Hint string `json:"hint,omitempty"`
}

// Spec describes a backend and everything needed to connect one.
type Spec struct {
	Kind        Kind        `json:"kind"`
	Label       string      `json:"label"`
	Description string      `json:"description"`
	DocsURL     string      `json:"docs_url,omitempty"`
	Fields      []FieldSpec `json:"fields"`
	Presets     []Preset    `json:"presets,omitempty"`

	// Covers lists the services this backend reaches beyond the one its label
	// names. Empty where the label already says it — Dropbox covers Dropbox.
	Covers []Service `json:"covers,omitempty"`

	// OAuth, when set, means this backend can be connected by signing in from
	// the browser instead of pasting credentials.
	OAuth *OAuthSpec `json:"oauth,omitempty"`

	// SignInLink, when set, means the same thing by a different route: this
	// backend is connected by signing in, but it cannot send the browser
	// anywhere. What it produces is a link to show — which can be followed on
	// a different device from the one SAND is being driven from, and that is
	// the whole reason a machine with no browser on it can connect an account
	// at all. Proton's client is the one that works this way; see
	// protoncli.go.
	SignInLink *SignInLinkSpec `json:"sign_in_link,omitempty"`

	// Order sorts the backends in the connect dialog: the ones you sign in to
	// first, then the ones needing credentials, then the ones on disk.
	Order int `json:"-"`
}

// SignInLinkSpec describes a sign-in the browser watches rather than performs.
type SignInLinkSpec struct {
	// SignInLabel is the button, e.g. "Sign in to Proton".
	SignInLabel string `json:"sign_in_label"`

	// StartPath is the endpoint that begins it. Unlike an OAuth flow there is
	// no authorize URL to build here: the link does not exist until the client
	// has been run, so it arrives on a later poll rather than in the answer to
	// starting.
	StartPath string `json:"start_path"`

	// Note is the one thing about this shape somebody has to be told, since it
	// is not how any other account connects.
	Note string `json:"note,omitempty"`
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
