// Package vault is SAND's control plane: it owns the encrypted index of
// stored files, the set of connected cloud accounts, and the orchestration
// that scatters each file's encrypted parts across those accounts and gathers
// them back on demand.
//
// Nothing readable ever leaves this package. Files are encoded into parts by
// internal/archive before a single byte is handed to a provider, and the index
// that maps browser paths onto those parts is itself encrypted at rest.
package vault

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// Vault is the unlocked-or-locked handle to a SAND vault on disk. It is safe
// for concurrent use.
type Vault struct {
	path string

	mu       sync.RWMutex
	store    *storeFile
	vaultKey []byte // nil while locked
	dataKey  []byte // nil while locked

	providers []provider.Config
	manifest  *Manifest

	// liveMu guards the cache of constructed providers. It is a leaf lock,
	// always taken last, so cache warming can happen while mu is held.
	liveMu sync.Mutex
	live   map[string]provider.Provider
}

// Open returns a handle to the vault at path. The vault starts locked; if no
// file exists yet, Initialized reports false and Init can create one.
func Open(path string) (*Vault, error) {
	v := &Vault{path: path, live: map[string]provider.Provider{}}

	sf, err := readStore(path)
	if err != nil && !errors.Is(err, ErrNotInitialized) {
		return nil, err
	}
	v.store = sf
	return v, nil
}

// Path is the vault file's location on disk.
func (v *Vault) Path() string { return v.path }

// Initialized reports whether a vault file exists.
func (v *Vault) Initialized() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.store != nil
}

// Unlocked reports whether the vault is currently open.
func (v *Vault) Unlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dataKey != nil
}

// Policy returns the vault's shard placement policy.
func (v *Vault) Policy() Policy {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.store == nil {
		return PolicyStrict
	}
	return v.store.Policy
}

// SetPolicy changes the placement policy for future uploads.
func (v *Vault) SetPolicy(p Policy) error {
	if !p.Valid() {
		return fmt.Errorf("unknown placement policy %q", p)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	v.store.Policy = p
	return v.persistLocked()
}

// Init creates a new vault sealed under password and leaves it unlocked.
func (v *Vault) Init(password string, policy Policy) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("password must not be empty")
	}
	if !policy.Valid() {
		policy = PolicyStrict
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store != nil {
		return fmt.Errorf("a vault already exists at %s", v.path)
	}

	sf, dataKey, err := newStore(password, policy)
	if err != nil {
		return err
	}
	if err := writeStore(v.path, sf); err != nil {
		return err
	}

	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		return err
	}

	v.store = sf
	v.vaultKey = crypto.DeriveKey(password, salt, params)
	v.dataKey = dataKey
	v.providers = []provider.Config{}
	v.manifest = newManifest()
	v.resetLiveCache()
	return nil
}

// Unlock decrypts the vault with password.
func (v *Vault) Unlock(password string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store == nil {
		// Re-read in case another process created the vault since Open.
		sf, err := readStore(v.path)
		if err != nil {
			return err
		}
		v.store = sf
	}

	vaultKey, dataKey, providers, manifest, err := unsealStore(v.store, password)
	if err != nil {
		return err
	}

	v.vaultKey = vaultKey
	v.dataKey = dataKey
	v.providers = providers
	v.manifest = manifest
	v.resetLiveCache()
	return nil
}

// Lock discards the in-memory keys and decrypted index.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()

	crypto.ZeroBytes(v.vaultKey)
	crypto.ZeroBytes(v.dataKey)
	v.vaultKey = nil
	v.dataKey = nil
	v.providers = nil
	v.manifest = nil

	v.liveMu.Lock()
	v.live = map[string]provider.Provider{}
	v.liveMu.Unlock()
}

// ChangePassword re-wraps the vault under a new password. Stored files are
// untouched: they are encrypted under the data key, which does not change.
func (v *Vault) ChangePassword(oldPassword, newPassword string) error {
	if strings.TrimSpace(newPassword) == "" {
		return fmt.Errorf("new password must not be empty")
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store == nil {
		return ErrNotInitialized
	}
	if _, _, _, _, err := unsealStore(v.store, oldPassword); err != nil {
		return err
	}

	// Everything the old key protected is already decrypted in memory when
	// unlocked; re-derive it here so the call also works from a locked vault.
	_, dataKey, providers, manifest, err := unsealStore(v.store, oldPassword)
	if err != nil {
		return err
	}

	sf, _, err := newStore(newPassword, v.store.Policy)
	if err != nil {
		return err
	}
	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		return err
	}
	newKey := crypto.DeriveKey(newPassword, salt, params)

	if sf.DataKey, err = seal(newKey, dataKey); err != nil {
		crypto.ZeroBytes(newKey)
		return err
	}
	if sf.Providers, err = sealJSON(newKey, providers); err != nil {
		crypto.ZeroBytes(newKey)
		return err
	}
	if sf.Manifest, err = sealJSON(newKey, manifest); err != nil {
		crypto.ZeroBytes(newKey)
		return err
	}
	if err := writeStore(v.path, sf); err != nil {
		crypto.ZeroBytes(newKey)
		return err
	}

	crypto.ZeroBytes(v.vaultKey)
	v.store = sf
	v.vaultKey = newKey
	v.dataKey = dataKey
	v.providers = providers
	v.manifest = manifest
	return nil
}

// shardPassword is the secret used to encrypt file parts. It is the hex form
// of the vault's random data key, so part encryption never depends on the
// strength of what the user typed.
func (v *Vault) shardPasswordLocked() string {
	return hex.EncodeToString(v.dataKey)
}

// persistLocked re-seals the mutable sections and writes the vault file. The
// caller must hold the write lock and the vault must be unlocked.
func (v *Vault) persistLocked() error {
	if v.vaultKey == nil {
		return ErrLocked
	}

	v.manifest.UpdatedAt = time.Now().UTC()

	providers, err := sealJSON(v.vaultKey, v.providers)
	if err != nil {
		return err
	}
	manifest, err := sealJSON(v.vaultKey, v.manifest)
	if err != nil {
		return err
	}

	v.store.Providers = providers
	v.store.Manifest = manifest
	return writeStore(v.path, v.store)
}

// ---------------------------------------------------------------------------
// Connected accounts
// ---------------------------------------------------------------------------

// Providers returns the connected account configs with secrets redacted.
func (v *Vault) Providers() ([]provider.Config, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	out := make([]provider.Config, 0, len(v.providers))
	for _, cfg := range v.providers {
		out = append(out, cfg.Redacted())
	}
	return out, nil
}

// configForLocked looks up a connected account's config. The caller must hold
// at least the read lock.
func (v *Vault) configForLocked(id string) (provider.Config, bool) {
	for _, cfg := range v.providers {
		if cfg.ID == id {
			return cfg, true
		}
	}
	return provider.Config{}, false
}

// configsForLocked collects the configs for every account referenced by a set
// of shards. Accounts that have since been disconnected are simply absent from
// the result. The caller must hold at least the read lock.
func (v *Vault) configsForLocked(shards []Shard) map[string]provider.Config {
	out := make(map[string]provider.Config, len(shards))
	for _, s := range shards {
		if _, done := out[s.ProviderID]; done {
			continue
		}
		if cfg, ok := v.configForLocked(s.ProviderID); ok {
			out[s.ProviderID] = cfg
		}
	}
	return out
}

// buildProvider returns a live provider for a config, constructing it on first
// use and caching it afterwards. Providers hold connection pools and OAuth
// tokens, so reusing them across transfers matters.
func (v *Vault) buildProvider(cfg provider.Config) (provider.Provider, error) {
	v.liveMu.Lock()
	defer v.liveMu.Unlock()

	if p, ok := v.live[cfg.ID]; ok {
		return p, nil
	}
	p, err := provider.New(cfg)
	if err != nil {
		return nil, err
	}
	v.live[cfg.ID] = p
	return p, nil
}

// resetLiveCache drops every constructed provider.
func (v *Vault) resetLiveCache() {
	v.liveMu.Lock()
	v.live = map[string]provider.Provider{}
	v.liveMu.Unlock()
}

// forgetProvider drops one account from the live cache.
func (v *Vault) forgetProvider(id string) {
	v.liveMu.Lock()
	delete(v.live, id)
	v.liveMu.Unlock()
}

// AddProvider connects a new cloud account after verifying it is reachable.
func (v *Vault) AddProvider(ctx context.Context, cfg provider.Config) (provider.Config, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return provider.Config{}, ErrLocked
	}
	if strings.TrimSpace(cfg.Name) == "" {
		if spec, ok := provider.SpecFor(cfg.Kind); ok {
			cfg.Name = spec.Label
		} else {
			cfg.Name = string(cfg.Kind)
		}
	}
	for _, existing := range v.providers {
		if strings.EqualFold(existing.Name, cfg.Name) {
			return provider.Config{}, fmt.Errorf("an account named %q is already connected", cfg.Name)
		}
	}

	cfg.ID = uuid.NewString()
	cfg.AddedAt = time.Now().UTC()

	live, err := provider.New(cfg)
	if err != nil {
		return provider.Config{}, err
	}
	if err := live.Ping(ctx); err != nil {
		return provider.Config{}, fmt.Errorf("could not connect to %s: %w", cfg.Name, err)
	}

	v.providers = append(v.providers, cfg)

	if err := v.persistLocked(); err != nil {
		// Roll back the in-memory state so it matches what is on disk.
		v.providers = v.providers[:len(v.providers)-1]
		return provider.Config{}, err
	}

	v.liveMu.Lock()
	v.live[cfg.ID] = live
	v.liveMu.Unlock()
	return cfg.Redacted(), nil
}

// RemoveProvider disconnects an account. Unless force is set, it refuses when
// files would be left with too few reachable parts to reconstruct.
func (v *Vault) RemoveProvider(id string, force bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return ErrLocked
	}

	idx := -1
	for i, cfg := range v.providers {
		if cfg.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no connected account with id %s", id)
	}

	// Count against the accounts that will still be connected afterwards, not
	// just against this one: a shard whose account was disconnected earlier is
	// already out of reach and must not prop up the count.
	surviving := map[string]bool{}
	for _, cfg := range v.providers {
		if cfg.ID != id {
			surviving[cfg.ID] = true
		}
	}

	if !force {
		var stranded []string
		for _, e := range v.manifest.Entries {
			reachable := 0
			for _, s := range e.Shards {
				if surviving[s.ProviderID] {
					reachable++
				}
			}
			if reachable < archive.MinPartsToRestore {
				stranded = append(stranded, e.Path())
			}
		}
		if len(stranded) > 0 {
			return fmt.Errorf(
				"%d file(s) would become unrecoverable, starting with %s — download them first, or force the disconnect",
				len(stranded), stranded[0])
		}
	}

	v.providers = append(v.providers[:idx], v.providers[idx+1:]...)

	// Drop shard records pointing at the disconnected account so the index
	// keeps telling the truth about what is actually retrievable.
	for _, e := range v.manifest.Entries {
		kept := e.Shards[:0]
		for _, s := range e.Shards {
			if surviving[s.ProviderID] {
				kept = append(kept, s)
			}
		}
		e.Shards = kept
	}

	v.forgetProvider(id)
	return v.persistLocked()
}

// TestProvider pings a connected account.
func (v *Vault) TestProvider(ctx context.Context, id string) error {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return ErrLocked
	}
	cfg, ok := v.configForLocked(id)
	v.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no connected account with id %s", id)
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		return err
	}
	return p.Ping(ctx)
}

// ProviderStatus is the health and load of one connected account.
type ProviderStatus struct {
	provider.Config
	Online bool           `json:"online"`
	Error  string         `json:"error,omitempty"`
	Shards int            `json:"shards"`
	Stored int64          `json:"stored"`
	Usage  provider.Usage `json:"usage"`
}

// ProviderStatuses pings every connected account in parallel and reports how
// much of the vault each one is holding.
func (v *Vault) ProviderStatuses(ctx context.Context) ([]ProviderStatus, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}

	statuses := make([]ProviderStatus, len(v.providers))
	live := make([]provider.Provider, len(v.providers))
	for i, cfg := range v.providers {
		statuses[i] = ProviderStatus{Config: cfg.Redacted()}
		p, err := v.buildProvider(cfg)
		if err != nil {
			statuses[i].Error = err.Error()
			continue
		}
		live[i] = p
	}
	for _, e := range v.manifest.Entries {
		for _, s := range e.Shards {
			for i := range statuses {
				if statuses[i].ID == s.ProviderID {
					statuses[i].Shards++
					statuses[i].Stored += s.Size
				}
			}
		}
	}
	v.mu.RUnlock()

	var wg sync.WaitGroup
	for i := range statuses {
		if live[i] == nil {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()

			if err := live[i].Ping(pingCtx); err != nil {
				statuses[i].Error = err.Error()
				return
			}
			statuses[i].Online = true
			if reporter, ok := live[i].(provider.UsageReporter); ok {
				if usage, err := reporter.Usage(pingCtx); err == nil {
					statuses[i].Usage = usage
				}
			}
		}(i)
	}
	wg.Wait()

	return statuses, nil
}

// ---------------------------------------------------------------------------
// Browsing
// ---------------------------------------------------------------------------

// Listing is one directory's worth of the browser namespace.
type Listing struct {
	Path    string   `json:"path"`
	Parent  string   `json:"parent"`
	Folders []string `json:"folders"`
	Files   []*Entry `json:"files"`
}

// List returns the contents of a folder.
func (v *Vault) List(dir string) (*Listing, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	dir = CleanDir(dir)
	if !v.manifest.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	folders, files := v.manifest.Children(dir)
	// Always hand back arrays rather than nil, so JSON consumers never have to
	// distinguish "empty folder" from "null".
	if folders == nil {
		folders = []string{}
	}
	parent := "/"
	if dir != "/" {
		if idx := strings.LastIndex(dir, "/"); idx > 0 {
			parent = dir[:idx]
		}
	}

	return &Listing{
		Path:    dir,
		Parent:  parent,
		Folders: folders,
		Files:   append([]*Entry{}, files...),
	}, nil
}

// Entry returns a single file's metadata by ID.
func (v *Vault) Entry(id string) (*Entry, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	e := v.manifest.ByID(id)
	if e == nil {
		return nil, fmt.Errorf("no such file: %s", id)
	}
	return e, nil
}

// Mkdir creates a folder.
func (v *Vault) Mkdir(dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	if err := v.manifest.Mkdir(dir); err != nil {
		return err
	}
	return v.persistLocked()
}

// Move renames a file and/or moves it to another folder. Only the index
// changes: the encrypted parts stay exactly where they are.
func (v *Vault) Move(id, newDir, newName string) (*Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	e := v.manifest.ByID(id)
	if e == nil {
		return nil, fmt.Errorf("no such file: %s", id)
	}

	dir := e.Dir
	if newDir != "" {
		dir = CleanDir(newDir)
		if !v.manifest.FolderExists(dir) {
			return nil, fmt.Errorf("no such folder: %s", dir)
		}
	}
	name := e.Name
	if newName != "" {
		clean, err := SanitizeName(newName)
		if err != nil {
			return nil, err
		}
		name = clean
	}
	if dir == e.Dir && name == e.Name {
		return e, nil
	}
	if existing := v.manifest.ByPath(JoinPath(dir, name)); existing != nil {
		return nil, fmt.Errorf("%s already exists", JoinPath(dir, name))
	}

	e.Dir = dir
	e.Name = name
	e.ModifiedAt = time.Now().UTC()

	if err := v.persistLocked(); err != nil {
		return nil, err
	}
	return e, nil
}

// Stats summarizes what the vault holds.
type Stats struct {
	Files       int    `json:"files"`
	Folders     int    `json:"folders"`
	Bytes       int64  `json:"bytes"`
	StoredBytes int64  `json:"stored_bytes"`
	Degraded    int    `json:"degraded"`
	Accounts    int    `json:"accounts"`
	Policy      Policy `json:"policy"`
}

// Stats returns aggregate counters for the whole vault.
func (v *Vault) Stats() (Stats, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return Stats{}, ErrLocked
	}

	folders := map[string]bool{}
	s := Stats{Accounts: len(v.providers), Policy: v.store.Policy}
	for _, f := range v.manifest.Folders {
		folders[f] = true
	}
	for _, e := range v.manifest.Entries {
		s.Files++
		s.Bytes += e.Size
		for _, sh := range e.Shards {
			s.StoredBytes += sh.Size
		}
		if len(e.Shards) < archive.PartCount {
			s.Degraded++
		}
		if e.Dir != "/" {
			folders[e.Dir] = true
		}
	}
	s.Folders = len(folders)
	return s, nil
}
