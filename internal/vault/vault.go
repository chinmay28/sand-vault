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
	"log"
	"path"
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
	dataKey  []byte // the active data key; nil while locked

	// dataKeyID names the generation in dataKey, and retired holds the older
	// generations that files are still stored under while a password change
	// re-encrypts them. Both are empty on a vault whose key has never been
	// rotated.
	dataKeyID string
	retired   map[string][]byte

	providers []provider.Config
	manifest  *Manifest

	// chunks caches decrypted chunks for readers, and flight collapses
	// concurrent misses on the same chunk into one fetch. Both are leaf
	// structures with their own locks, never taken while mu is held.
	chunks *chunkCache
	flight *chunkFlight

	// chunkSize is the plaintext chunk length new uploads are cut into. It is
	// per-vault rather than a constant because every file records the size it
	// was written with, so changing it never invalidates what is already
	// stored — and because a test that wants several chunks should not have to
	// push tens of megabytes to get them.
	chunkSize uint32

	// liveMu guards the cache of constructed providers. It is a leaf lock,
	// always taken last, so cache warming can happen while mu is held.
	liveMu sync.Mutex
	live   map[string]provider.Provider

	// thumbMu guards the decrypted thumbnails held in memory, by folder and
	// then by file. Another leaf lock: nothing is acquired while it is held,
	// which is what lets Lock clear the cache while holding mu.
	thumbMu sync.Mutex
	thumbs  map[string]map[string][]byte

	// thumbLoad serializes gathering thumbnail packs from the accounts, so
	// that a folder whose every row wants a picture fetches its pack once
	// rather than once per row. It is taken before mu, never while holding it.
	thumbLoad sync.Mutex

	// backupMu guards the manifest backup syncer's state. Also a leaf lock:
	// scheduling a push happens while mu is held, and the push itself runs on
	// its own goroutine after mu is released.
	backupMu      sync.Mutex
	backupIdle    sync.Cond
	backupRunning bool
	backupPending bool
	backupForce   bool

	// backupChecked remembers the accounts already known to hold this vault's
	// own backup, so the guard against clobbering another vault's copy costs
	// one read per account per unlock rather than one per push.
	backupChecked map[string]bool

	// backupWarned remembers the accounts already reported as holding another
	// vault's backup, so the warning is said once rather than on every change.
	backupWarned map[string]bool
}

// Open returns a handle to the vault at path. The vault starts locked; if no
// file exists yet, Initialized reports false and Init can create one.
func Open(path string) (*Vault, error) {
	v := &Vault{
		path:      path,
		live:      map[string]provider.Provider{},
		chunkSize: archive.DefaultChunkSize,
		chunks:    newChunkCache(DefaultChunkCacheBytes),
		flight:    newChunkFlight(),
	}
	v.backupIdle.L = &v.backupMu

	// A conversion only ever holds its spool open, so anything matching that
	// name now is what a process killed mid-conversion left behind.
	sweepConversionSpools(path)

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

// DefaultAccounts returns the accounts uploads spread over unless they name
// their own. An empty result means no default is set, and every upload picks
// its own accounts at random.
func (v *Vault) DefaultAccounts() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.store == nil {
		return nil
	}
	return append([]string(nil), v.store.DefaultAccounts...)
}

// SetDefaultAccounts records which accounts future uploads should use. Passing
// nothing clears the default and hands the choice back to the per-file random
// pick.
//
// The selection is checked against what is actually connected, because a
// default naming an account that has gone away would quietly become a smaller
// spread than the user asked for on every upload after it.
func (v *Vault) SetDefaultAccounts(ids []string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	chosen := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		cfg, ok := v.configForLocked(id)
		if !ok {
			return fmt.Errorf("no connected account with id %s", id)
		}
		if seen[id] {
			return fmt.Errorf("%s is listed twice — a file's parts each go to a different account", cfg.Name)
		}
		seen[id] = true
		chosen = append(chosen, id)
	}
	if len(chosen) > AccountsPerFile {
		return fmt.Errorf("a file has only %d parts — choose at most %d accounts (got %d)",
			archive.PartCount, AccountsPerFile, len(chosen))
	}
	if len(chosen) > 0 && len(chosen) < archive.MinPartsToRestore {
		return fmt.Errorf(
			"choose at least %d accounts, so that a file still has a second place to be rebuilt from",
			archive.MinPartsToRestore)
	}

	if len(chosen) == 0 {
		chosen = nil
	}
	previous := v.store.DefaultAccounts
	v.store.DefaultAccounts = chosen
	if err := v.persistLocked(); err != nil {
		v.store.DefaultAccounts = previous
		return err
	}
	return nil
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
	v.dataKeyID = sf.DataKeyID
	v.retired = map[string][]byte{}
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

	u, err := unsealStore(v.store, password)
	if err != nil {
		return err
	}

	v.adoptLocked(u)
	v.resetLiveCache()

	// Anything that changed while an account was unreachable — or while the
	// vault was locked mid-push — is repaired here rather than staying stale
	// until the next upload.
	v.scheduleBackup(false)
	return nil
}

// VerifyPassword reports whether a password opens this vault, without changing
// anything about it.
//
// Unlock is not a substitute. It adopts fresh keys, resets the provider cache
// and schedules a manifest backup push to every connected account — reasonable
// once, when someone signs in, and ruinous as the answer to "is this password
// right?" on a request that arrives hundreds of times while a film plays.
//
// It still costs a full Argon2id pass, which is what makes guessing expensive
// and is exactly why a caller checking repeatedly should remember the answer
// rather than asking again.
func (v *Vault) VerifyPassword(password string) error {
	v.mu.RLock()
	store := v.store
	v.mu.RUnlock()

	if store == nil {
		sf, err := readStore(v.path)
		if err != nil {
			return err
		}
		store = sf
	}

	u, err := unsealStore(store, password)
	if err != nil {
		return err
	}
	// Nothing here is adopted, so every key it produced is wiped.
	u.zero()
	return nil
}

// Lock discards the in-memory keys and decrypted index.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()

	crypto.ZeroBytes(v.vaultKey)
	crypto.ZeroBytes(v.dataKey)
	for _, key := range v.retired {
		crypto.ZeroBytes(key)
	}
	v.vaultKey = nil
	v.dataKey = nil
	v.dataKeyID = ""
	v.retired = nil
	v.providers = nil
	v.manifest = nil

	// Thumbnails are decrypted pictures of the user's files, so they go the
	// same way the keys do — and so do cached chunks, which are plaintext of
	// the files themselves.
	v.forgetAllThumbs()
	v.chunks.clear()

	v.liveMu.Lock()
	v.live = map[string]provider.Provider{}
	v.liveMu.Unlock()
}

// adoptLocked installs the keys and sections a password just opened. The
// caller must hold the write lock, and must not zero what it passes in.
func (v *Vault) adoptLocked(u *unsealed) {
	v.vaultKey = u.vaultKey
	v.dataKey = u.dataKey
	v.dataKeyID = u.dataKeyID
	v.retired = u.retired
	if v.retired == nil {
		v.retired = map[string][]byte{}
	}
	v.providers = u.providers
	v.manifest = u.manifest
}

// shardPasswordLocked is the secret used to encrypt the parts of new uploads.
// It is the hex form of the vault's active random data key, so part encryption
// never depends on the strength of what the user typed.
func (v *Vault) shardPasswordLocked() string {
	return shardPasswordFor(v.dataKey)
}

// shardPasswordForLocked returns the secret that opens the parts of a file
// sealed under a given key generation. Anything not yet re-encrypted after a
// password change is still on an older one, and asking for a generation the
// vault no longer holds is a corrupt index rather than a wrong password.
func (v *Vault) shardPasswordForLocked(keyID string) (string, error) {
	if keyID == v.dataKeyID {
		return shardPasswordFor(v.dataKey), nil
	}
	if key, ok := v.retired[keyID]; ok {
		return shardPasswordFor(key), nil
	}
	return "", fmt.Errorf("this file is recorded under a data key the vault no longer holds")
}

// shardPasswordFor derives the part secret from a data key. Recovery needs the
// same derivation from a key that came out of a backup rather than a vault.
func shardPasswordFor(dataKey []byte) string {
	return hex.EncodeToString(dataKey)
}

// dataKeyForLocked returns a copy of the raw data key that opens a given
// generation's chunks. It is shardPasswordForLocked's counterpart for the
// chunked format, which derives its keys from the key material itself rather
// than from a password spelled out in hex — see crypto.DeriveChunkKey.
//
// The copy is the caller's to zero.
func (v *Vault) dataKeyForLocked(keyID string) ([]byte, error) {
	if keyID == v.dataKeyID {
		return append([]byte(nil), v.dataKey...), nil
	}
	if key, ok := v.retired[keyID]; ok {
		return append([]byte(nil), key...), nil
	}
	return nil, fmt.Errorf("this file is recorded under a data key the vault no longer holds")
}

// persistLocked re-seals the mutable sections and writes the vault file. The
// caller must hold the write lock and the vault must be unlocked.
func (v *Vault) persistLocked() error {
	if v.vaultKey == nil {
		return ErrLocked
	}

	v.manifest.UpdatedAt = time.Now().UTC()

	// Every index change is a chance for the last file on a retired key to
	// have moved off it, and a key nothing needs must not linger in the file.
	v.pruneRetiredKeysLocked()

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
	if err := writeStore(v.path, v.store); err != nil {
		return err
	}

	// The index on disk just changed, so the copies on the accounts are now
	// stale. Pushing happens on its own goroutine: this runs under the write
	// lock, and network round-trips must not block browsing.
	v.scheduleBackup(false)
	return nil
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
	p, err := v.newLiveProvider(cfg)
	if err != nil {
		return nil, err
	}
	v.live[cfg.ID] = p
	return p, nil
}

// newLiveProvider constructs a backend and connects it to the vault, so
// credentials it rotates while running are written back rather than lost.
func (v *Vault) newLiveProvider(cfg provider.Config) (provider.Provider, error) {
	p, err := provider.New(cfg)
	if err != nil {
		return nil, err
	}
	if rotator, ok := p.(provider.CredentialRotator); ok {
		id, name := cfg.ID, cfg.Name
		rotator.OnCredentialChange(func(updates map[string]string) {
			// On a goroutine of our own: the sink runs inline inside whatever
			// call was talking to the backend, and that call may already be
			// holding the lock this write needs.
			go func() {
				if err := v.updateProviderOptions(id, updates); err != nil {
					log.Printf("could not store %s's renewed credentials: %v", name, err)
				}
			}()
		})
	}
	return p, nil
}

// updateProviderOptions merges option changes into a connected account and
// writes the vault back out.
//
// This is how a rotated refresh token survives a restart. It is called from
// whatever goroutine happened to be refreshing, so it takes the lock itself
// and shrugs at an account that has been disconnected in the meantime.
func (v *Vault) updateProviderOptions(id string, updates map[string]string) error {
	if len(updates) == 0 {
		return nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return ErrLocked
	}
	for i := range v.providers {
		if v.providers[i].ID != id {
			continue
		}
		if v.providers[i].Options == nil {
			v.providers[i].Options = map[string]string{}
		}
		changed := false
		for key, value := range updates {
			if v.providers[i].Options[key] == value {
				continue
			}
			v.providers[i].Options[key] = value
			changed = true
		}
		if !changed {
			return nil
		}
		return v.persistLocked()
	}
	return nil
}

// resetLiveCache drops every constructed provider, and with it the record of
// which accounts have already been checked for a foreign backup: the set of
// connected accounts may be entirely different after an unlock.
func (v *Vault) resetLiveCache() {
	v.liveMu.Lock()
	v.live = map[string]provider.Provider{}
	v.liveMu.Unlock()

	v.backupMu.Lock()
	v.backupChecked = map[string]bool{}
	v.backupWarned = map[string]bool{}
	v.backupMu.Unlock()
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

	// Store the backend's defaults alongside what the user supplied, so an
	// account keeps the folder it was connected with even if a later release
	// changes what a fresh connection would pick.
	cfg = provider.WithDefaults(cfg)
	cfg.ID = uuid.NewString()
	cfg.AddedAt = time.Now().UTC()

	live, err := v.newLiveProvider(cfg)
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

	// A default naming an account that is no longer there would silently
	// shrink the spread of every upload after this one.
	if len(v.store.DefaultAccounts) > 0 {
		kept := make([]string, 0, len(v.store.DefaultAccounts))
		for _, def := range v.store.DefaultAccounts {
			if surviving[def] {
				kept = append(kept, def)
			}
		}
		if len(kept) < archive.MinPartsToRestore {
			// Too little left to be a default at all; the per-file random pick
			// takes over rather than every upload landing on one account.
			kept = nil
		}
		v.store.DefaultAccounts = kept
	}

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

	// Thumbs names the files in this listing that have a stored thumbnail, so
	// the browser knows which rows to draw a picture for without asking for
	// one it will not get.
	Thumbs []string `json:"thumbs"`
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

	thumbs := v.thumbIDsForLocked(files)
	if thumbs == nil {
		thumbs = []string{}
	}

	return &Listing{
		Path:    dir,
		Parent:  parent,
		Folders: folders,
		Files:   append([]*Entry{}, files...),
		Thumbs:  thumbs,
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

// EntryByPath looks a file up by its full browser path rather than its ID,
// which is what a caller working in paths — a filesystem view — has to hand.
func (v *Vault) EntryByPath(path string) (*Entry, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	e := v.manifest.ByPath(path)
	if e == nil {
		return nil, fmt.Errorf("no such file: %s", path)
	}
	return e, nil
}

// FolderExists reports whether a folder is in the index. A locked vault knows
// nothing, so it answers false rather than guessing.
func (v *Vault) FolderExists(dir string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return false
	}
	return v.manifest.FolderExists(CleanDir(dir))
}

// MoveFolder renames a folder, carrying everything beneath it along.
//
// Nothing is transferred. A file records which folder it is in, so moving a
// folder is a rewrite of the index and the stored parts never move — which is
// also what makes it safe to offer: every entry beneath the folder changes in
// the same write, so there is no moment where half a tree answers to its old
// name and half to its new one. Thumbnails come too, since a pack is filed
// under its folder rather than carrying the folder's name inside it.
func (v *Vault) MoveFolder(ctx context.Context, oldDir, newDir string) error {
	oldDir, newDir = CleanDir(oldDir), CleanDir(newDir)

	if oldDir == "/" || newDir == "/" {
		return fmt.Errorf("the root folder cannot be moved")
	}
	if oldDir == newDir {
		return nil
	}
	// Moving a folder inside itself would rewrite the destination's own path as
	// the rewrite walked it, leaving a tree that contains itself.
	if strings.HasPrefix(newDir, oldDir+"/") {
		return fmt.Errorf("cannot move %s inside itself", oldDir)
	}
	if _, err := SanitizeName(path.Base(newDir)); err != nil {
		return err
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return ErrLocked
	}
	if !v.manifest.FolderExists(oldDir) {
		v.mu.Unlock()
		return fmt.Errorf("no such folder: %s", oldDir)
	}
	if v.manifest.FolderExists(newDir) {
		v.mu.Unlock()
		return fmt.Errorf("%s already exists", newDir)
	}
	if v.manifest.ByPath(newDir) != nil {
		v.mu.Unlock()
		return fmt.Errorf("a file already exists at %s", newDir)
	}
	if parent := CleanDir(path.Dir(newDir)); !v.manifest.FolderExists(parent) {
		v.mu.Unlock()
		return fmt.Errorf("no such folder: %s", parent)
	}

	undo := v.manifest.moveFolder(oldDir, newDir)
	err := v.persistLocked()
	if err != nil {
		undo()
	}
	v.mu.Unlock()

	if err != nil {
		return err
	}

	// The decrypted thumbnails in memory are filed by folder too, and the
	// cheapest correct thing is to let them be read again from the packs that
	// just moved with the tree.
	v.forgetAllThumbs()
	return nil
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
//
// The thumbnail is the exception, because thumbnails are stored a folder at a
// time: moving a file across folders moves its picture between two packs,
// which is network work and so happens after the move itself has landed.
func (v *Vault) Move(ctx context.Context, id, newDir, newName string) (*Entry, error) {
	v.mu.Lock()

	if v.dataKey == nil {
		v.mu.Unlock()
		return nil, ErrLocked
	}

	e := v.manifest.ByID(id)
	if e == nil {
		v.mu.Unlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	from := e.Dir

	dir := e.Dir
	if newDir != "" {
		dir = CleanDir(newDir)
		if !v.manifest.FolderExists(dir) {
			v.mu.Unlock()
			return nil, fmt.Errorf("no such folder: %s", dir)
		}
	}
	name := e.Name
	if newName != "" {
		clean, err := SanitizeName(newName)
		if err != nil {
			v.mu.Unlock()
			return nil, err
		}
		name = clean
	}
	if dir == e.Dir && name == e.Name {
		v.mu.Unlock()
		return e, nil
	}
	if existing := v.manifest.ByPath(JoinPath(dir, name)); existing != nil {
		v.mu.Unlock()
		return nil, fmt.Errorf("%s already exists", JoinPath(dir, name))
	}

	e.Dir = dir
	e.Name = name
	e.ModifiedAt = time.Now().UTC()

	err := v.persistLocked()
	v.mu.Unlock()

	if err != nil {
		return nil, err
	}

	v.moveThumb(ctx, id, from, dir)
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

	// DefaultAccounts is the vault-wide account selection new uploads start
	// from. Empty means each upload picks its own at random.
	DefaultAccounts []string `json:"default_accounts"`

	// Pending counts the files still stored under a retired data key after a
	// password change, waiting to be re-encrypted under the new one.
	Pending int `json:"pending_migration"`
}

// Stats returns aggregate counters for the whole vault.
func (v *Vault) Stats() (Stats, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return Stats{}, ErrLocked
	}

	folders := map[string]bool{}
	s := Stats{
		Accounts:        len(v.providers),
		Policy:          v.store.Policy,
		DefaultAccounts: append([]string{}, v.store.DefaultAccounts...),
	}
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
		if e.KeyID != v.dataKeyID {
			s.Pending++
		}
		if e.Dir != "/" {
			folders[e.Dir] = true
		}
	}
	s.Folders = len(folders)
	return s, nil
}
