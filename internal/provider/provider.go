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
	KindLocal   Kind = "local"
	KindS3      Kind = "s3"
	KindWebDAV  Kind = "webdav"
	KindGDrive  Kind = "gdrive"
	KindDropbox Kind = "dropbox"
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
type Usage struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
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
}

// Spec describes a backend and everything needed to connect one.
type Spec struct {
	Kind        Kind        `json:"kind"`
	Label       string      `json:"label"`
	Description string      `json:"description"`
	DocsURL     string      `json:"docs_url,omitempty"`
	Fields      []FieldSpec `json:"fields"`
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

// Specs returns every registered backend spec, ordered by label.
func Specs() []Spec {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Spec, 0, len(registry))
	for _, reg := range registry {
		out = append(out, reg.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// SpecFor returns the spec for a kind.
func SpecFor(kind Kind) (Spec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reg, ok := registry[kind]
	return reg.spec, ok
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

	if cfg.Options == nil {
		cfg.Options = map[string]string{}
	}
	for _, f := range reg.spec.Fields {
		if cfg.Options[f.Key] == "" && f.Default != "" {
			cfg.Options[f.Key] = f.Default
		}
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
