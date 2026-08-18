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

	// settings is the sealed preferences section: the film database key, and
	// anything else that is a secret rather than a knob. nil while locked, like
	// everything else a password opens.
	settings *vaultSettings
	// subs holds the sub vaults that have been opened, by ID. A sub vault
	// absent from this map is locked: its section stays sealed in the store
	// file and is carried through every write untouched, so locking one is
	// simply dropping it from here.
	//
	// Each carries its own manifest rather than merging into the one above.
	// That is what makes "which vault is this file in" a question with a real
	// answer rather than a tag that has to be kept true — an entry is in the
	// vault whose index holds it, and it cannot be filed into the wrong section
	// by a bug because there is no filing step.
	subs map[string]*subVault

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

	// reads counts which accounts have been winning the race every read runs.
	// Its own lock, taken alone: recording is on the read path and must never
	// wait on the index. readsMu serializes the saves, and is taken after mu
	// and before the recorder's own. See readstats.go and readhistory.go.
	reads   *readStats
	readsMu sync.Mutex
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
		reads:     newReadStats(),
	}
	// How the recorder saves itself. It counts on the read path and knows
	// nothing about keys or files; this is the one line that hands it back to
	// the vault, which has both. See readhistory.go.
	v.reads.flush = v.flushReadHistory
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

// DefaultScheme is the erasure code future uploads are cut with when they do
// not choose their own. The zero value means no preference, and how many
// accounts a file lands on settles the code.
func (v *Vault) DefaultScheme() archive.Scheme {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.defaultSchemeLocked()
}

// defaultSchemeLocked reads the stored default, treating anything unparseable
// as no preference at all.
//
// A vault file is not a place a bad scheme can come from — SetDefaults writes
// only what it has checked — but it is a place one could be *edited* into, and
// a vault whose uploads fail because of a typo in a JSON field is worse than a
// vault that quietly goes back to its default family.
func (v *Vault) defaultSchemeLocked() archive.Scheme {
	if v.store == nil || v.store.DefaultScheme == "" {
		return archive.Scheme{}
	}
	scheme, err := archive.ParseScheme(v.store.DefaultScheme)
	if err != nil {
		return archive.Scheme{}
	}
	return scheme
}

// SetDefaultAccounts records which accounts future uploads should use, leaving
// any default scheme alone. It is SetDefaults for a caller that has nothing to
// say about the code.
func (v *Vault) SetDefaultAccounts(ids []string) error {
	v.mu.RLock()
	scheme := v.defaultSchemeLocked()
	v.mu.RUnlock()
	return v.SetDefaults(ids, scheme)
}

// SetDefaults records which accounts future uploads should use and which code
// they are cut with. Passing nothing clears the default and hands the choice
// back to the per-file random pick; passing a zero scheme clears that half and
// hands the code back to the count of accounts.
//
// The two are set together because neither is checkable alone. A default of
// 3-of-5 makes five accounts a spread that five accounts would not otherwise
// be, and dropping to four accounts would leave that scheme naming a width the
// vault no longer has — so the pair has to be accepted or refused as one.
//
// With no scheme named, how many accounts are named is what settles the code
// every upload is cut with: three is 2-of-3, six is 4-of-6, nine is 6-of-9.
//
// The selection is checked against what is actually connected, because a
// default naming an account that has gone away would quietly become a smaller
// spread than the user asked for on every upload after it.
func (v *Vault) SetDefaults(ids []string, scheme archive.Scheme) error {
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
			return fmt.Errorf("%s is listed twice — every shard of a file goes to a different account", cfg.Name)
		}
		seen[id] = true
		chosen = append(chosen, id)
	}
	named := scheme != archive.Scheme{}
	switch {
	case named && len(chosen) == 0:
		// A code with no accounts under it is half a preference. The width it
		// names is the number of clouds it wants, and there are none to check
		// it against.
		return fmt.Errorf(
			"%s names %d accounts to cut across — choose them, or clear the scheme to let each "+
				"upload's own count settle it", scheme, scheme.Total)
	case named:
		if err := scheme.Check(); err != nil {
			return err
		}
		if len(chosen) != scheme.Total {
			return fmt.Errorf(
				"%s is cut across %d accounts and %d were chosen — pick %d, or change the scheme",
				scheme, scheme.Total, len(chosen), scheme.Total)
		}
	default:
		if !ValidSpread(len(chosen)) {
			return ErrSpread(len(chosen))
		}
	}
	if len(chosen) > 0 && len(chosen) < archive.MinPartsToRestore {
		return fmt.Errorf(
			"choose at least %d accounts, so that a file still has a second place to be rebuilt from",
			archive.MinPartsToRestore)
	}

	if len(chosen) == 0 {
		chosen = nil
	}
	// Only a scheme the count would not have named by itself is worth storing:
	// recording 4-of-6 against six accounts would freeze a default that is
	// already what six accounts mean, and would then have to be cleared by hand
	// after every change to the list.
	stored := ""
	if named && scheme != mustSchemeFor(len(chosen)) {
		stored = scheme.String()
	}

	previousAccounts, previousScheme := v.store.DefaultAccounts, v.store.DefaultScheme
	v.store.DefaultAccounts, v.store.DefaultScheme = chosen, stored
	if err := v.persistLocked(); err != nil {
		v.store.DefaultAccounts, v.store.DefaultScheme = previousAccounts, previousScheme
		return err
	}
	return nil
}

// mustSchemeFor is the code a count of accounts names, or the zero value where
// it names none — for a caller comparing against it rather than using it.
func mustSchemeFor(accounts int) archive.Scheme {
	scheme, err := archive.SchemeFor(accounts)
	if err != nil {
		return archive.Scheme{}
	}
	return scheme
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
	v.settings = &vaultSettings{}
	v.subs = map[string]*subVault{}
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

	// The read history is sealed under the key that has just arrived, so this
	// is the first moment it can be read — and the panel that draws it is one
	// of the first things somebody opens after signing in.
	v.loadReadHistoryLocked()

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

	// Last chance to save what has been counted: the key it is sealed under is
	// about to be wiped along with everything else.
	v.flushReadHistoryLocked()
	v.reads.reset()

	crypto.ZeroBytes(v.vaultKey)
	crypto.ZeroBytes(v.dataKey)
	for _, key := range v.retired {
		crypto.ZeroBytes(key)
	}
	// A sub vault cannot outlive the vault holding it: its keys came out of
	// this process's memory and go back the same way.
	for _, sub := range v.subs {
		sub.zero()
	}
	v.vaultKey = nil
	v.dataKey = nil
	v.dataKeyID = ""
	v.retired = nil
	v.providers = nil
	v.manifest = nil
	v.settings = nil
	v.subs = nil

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
	v.settings = u.settings
	if v.settings == nil {
		v.settings = &vaultSettings{}
	}
	// Sub vaults are not opened by the main password, so an unlock starts with
	// every one of them shut.
	v.subs = map[string]*subVault{}
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
	key, err := v.dataKeyForLocked(keyID)
	if err != nil {
		return "", err
	}
	defer crypto.ZeroBytes(key)
	return shardPasswordFor(key), nil
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
//
// Every open vault is searched, the main one and each unlocked sub vault, so a
// caller holding an entry never has to say which vault it came from. A key ID
// that belongs to a sub vault nobody has opened is the one case worth telling
// apart from a key that is simply gone, and it is: see subVaultForKeyLocked.
func (v *Vault) dataKeyForLocked(keyID string) ([]byte, error) {
	if keyID == v.dataKeyID {
		return append([]byte(nil), v.dataKey...), nil
	}
	if key, ok := v.retired[keyID]; ok {
		return append([]byte(nil), key...), nil
	}
	for _, sub := range v.subs {
		if keyID == sub.dataKeyID {
			return append([]byte(nil), sub.dataKey...), nil
		}
		if key, ok := sub.retired[keyID]; ok {
			return append([]byte(nil), key...), nil
		}
	}
	if id, ok := v.subVaultForKeyLocked(keyID); ok {
		return nil, fmt.Errorf("%w: %s", ErrSubVaultLocked, v.subVaultLabelLocked(id))
	}
	return nil, fmt.Errorf("this file is recorded under a data key the vault no longer holds")
}

// persistLocked re-seals the mutable sections and writes the vault file. The
// caller must hold the write lock and the vault must be unlocked.
func (v *Vault) persistLocked() error {
	if v.vaultKey == nil {
		return ErrLocked
	}

	now := time.Now().UTC()
	v.manifest.UpdatedAt = now

	// Every index change is a chance for the last file on a retired key to
	// have moved off it, and a key nothing needs must not linger in the file.
	v.pruneRetiredKeysLocked()

	// Re-seal every open sub vault's section, each under its own key, and
	// refresh what the main vault is allowed to know about it. A locked sub
	// vault's record is left exactly as it is — this is the write that carries
	// it through untouched.
	if err := v.resealSubVaultsLocked(now); err != nil {
		return err
	}

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

	// Written only once there is something in it, so a vault that has never set
	// a preference carries no settings section at all.
	if v.settings.empty() {
		v.store.Settings = nil
	} else {
		settings, err := sealJSON(v.vaultKey, v.settings)
		if err != nil {
			return err
		}
		v.store.Settings = &settings
	}

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

// ProviderEdit names what can be changed about an account after it is
// connected. A nil field is left exactly as it was, so recolouring an account
// cannot disturb its name and renaming one cannot disturb its colour.
type ProviderEdit struct {
	Name  *string
	Color *string

	// Capacity is how big the account holder says the account is, in bytes,
	// for backends with no quota call of their own. Zero clears it and the
	// account goes back to reporting no capacity at all.
	Capacity *int64
}

// UpdateProvider changes a connected account's label, its colour, or the
// capacity its holder declares for it. None of the three touches the
// credentials, the backend or the parts sitting on it: this is what the account
// is called, what colour it wears and how big its owner says it is, and nothing
// is uploaded, downloaded or re-encrypted by changing any of them.
//
// A rename is carried across the index as well. Every shard records the name of
// the account holding it — that is what the file list and the health read-out
// show, and what a recovery from a manifest backup matches accounts on — so
// leaving those behind would make the vault answer with a name that no longer
// exists.
func (v *Vault) UpdateProvider(id string, edit ProviderEdit) (provider.Config, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return provider.Config{}, ErrLocked
	}

	idx := -1
	for i := range v.providers {
		if v.providers[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return provider.Config{}, fmt.Errorf("no connected account with id %s", id)
	}

	before := v.providers[idx]
	after := before

	if edit.Name != nil {
		name := strings.TrimSpace(*edit.Name)
		if name == "" {
			return provider.Config{}, errors.New("an account needs a name")
		}
		for i, existing := range v.providers {
			if i != idx && strings.EqualFold(existing.Name, name) {
				return provider.Config{}, fmt.Errorf("an account named %q is already connected", name)
			}
		}
		after.Name = name
	}

	if edit.Color != nil {
		color, err := provider.NormalizeColor(*edit.Color)
		if err != nil {
			return provider.Config{}, err
		}
		after.Color = color
	}

	if edit.Capacity != nil {
		if *edit.Capacity < 0 {
			return provider.Config{}, errors.New("an account cannot hold a negative number of bytes")
		}
		after.Capacity = *edit.Capacity
	}

	if after.Name == before.Name && after.Color == before.Color && after.Capacity == before.Capacity {
		return after.Redacted(), nil
	}

	v.providers[idx] = after
	if after.Name != before.Name {
		v.renameShardsLocked(id, after.Name)
	}

	if err := v.persistLocked(); err != nil {
		// Put the in-memory state back the way the file on disk still has it,
		// index included, rather than leaving the two disagreeing.
		v.providers[idx] = before
		if after.Name != before.Name {
			v.renameShardsLocked(id, before.Name)
		}
		return provider.Config{}, err
	}
	return after.Redacted(), nil
}

// renameShardsLocked writes a new account name onto every shard held by that
// account. The caller must hold the write lock.
//
// Across every vault that is open. A sub vault that is shut keeps the old name
// on its shards until something rewrites them, which is a cosmetic staleness
// and nothing more: the name is what the file list and the health read-out
// print, and what a recovery matches accounts on by preference — the ID is what
// actually finds the parts.
func (v *Vault) renameShardsLocked(id, name string) {
	for _, m := range v.manifestsLocked() {
		for _, e := range m.Entries {
			for i := range e.Shards {
				if e.Shards[i].ProviderID == id {
					e.Shards[i].ProviderName = name
				}
			}
		}
	}
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
		for _, m := range v.manifestsLocked() {
			for _, e := range m.Entries {
				// Counted against the file's own code: a 4-of-6 file loses nothing
				// it needs when one of its six accounts goes, where a 2-of-3 one on
				// the same account might.
				reachable := map[int]bool{}
				for _, s := range e.Shards {
					if surviving[s.ProviderID] {
						reachable[s.Part] = true
					}
				}
				if len(reachable) < e.Scheme().Data {
					stranded = append(stranded, e.Path())
				}
			}
		}

		// A shut sub vault cannot be asked what it holds, so the inventory
		// answers for it. This is the guarantee the inventory was kept for:
		// without it, disconnecting an account would quietly strand files that
		// nothing in the process could see, and the refusal that exists to stop
		// exactly that would pass because it had nothing to count. The
		// inventory records each item's own code for the same reason the entry
		// carries it — the threshold is per file, not per vault.
		sealed := 0
		for _, meta := range v.manifest.SubVaults {
			if _, open := v.subs[meta.ID]; open {
				continue
			}
			for _, item := range meta.Inventory {
				reachable := map[int]bool{}
				for _, part := range item.Parts {
					if surviving[part.ProviderID] {
						reachable[part.Part] = true
					}
				}
				if len(reachable) < item.Scheme().Data {
					sealed++
				}
			}
		}

		switch {
		case len(stranded) > 0:
			return fmt.Errorf(
				"%d file(s) would become unrecoverable, starting with %s — download them first, or force the disconnect",
				len(stranded)+sealed, stranded[0])
		case sealed > 0:
			// Named by count alone, because naming them would mean reading an
			// index this vault cannot open.
			return fmt.Errorf(
				"%d file(s) inside a locked sub vault would become unrecoverable — open it to see which, "+
					"or force the disconnect", sealed)
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
		// A default scheme is a statement about a width, so losing an account
		// takes the width away with it: 3-of-5 has nothing to say about the four
		// that are left. The scheme goes rather than the accounts, because the
		// accounts are the half the user can see going.
		if scheme := v.defaultSchemeLocked(); scheme != (archive.Scheme{}) && scheme.Total != len(kept) {
			v.store.DefaultScheme = ""
		}
		// What is left has to still name a scheme. A default of six that loses
		// one account becomes a default of three rather than of five, which is
		// no scheme at all.
		for v.store.DefaultScheme == "" && !ValidSpread(len(kept)) && len(kept) > 0 {
			kept = kept[:len(kept)-1]
		}
		if len(kept) < archive.MinPartsToRestore {
			// Too little left to be a default at all; the per-file random pick
			// takes over rather than every upload landing on one account.
			kept = nil
			v.store.DefaultScheme = ""
		}
		v.store.DefaultAccounts = kept
	}

	// Drop shard records pointing at the disconnected account so the index
	// keeps telling the truth about what is actually retrievable. A shut sub
	// vault keeps its records until it is opened, where the part is skipped for
	// naming an account that is no longer connected — the same way a file
	// behaves between a forced disconnect and the next time it is read.
	for _, m := range v.manifestsLocked() {
		for _, e := range m.Entries {
			kept := e.Shards[:0]
			for _, s := range e.Shards {
				if surviving[s.ProviderID] {
					kept = append(kept, s)
				}
			}
			e.Shards = kept
		}
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

	// Measurable says this account can be counted on request — a bucket, which
	// has no quota call but can be listed (see provider.UsageMeasurer). It is
	// what puts the "count what is in it" button in front of somebody, and what
	// makes a declared capacity worth offering: without a used figure from
	// somewhere, a capacity is a denominator with no numerator.
	Measurable bool `json:"measurable,omitempty"`
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
		_, statuses[i].Measurable = p.(provider.UsageMeasurer)
		live[i] = p
	}
	byID := make(map[string]*ProviderStatus, len(statuses))
	for i := range statuses {
		byID[statuses[i].ID] = &statuses[i]
	}
	for _, m := range v.manifestsLocked() {
		for _, e := range m.Entries {
			for _, s := range e.Shards {
				if st, ok := byID[s.ProviderID]; ok {
					st.Shards++
					st.Stored += s.Size
				}
			}
		}
	}
	// What a shut sub vault put on each account counts too. Leaving it out
	// would draw an account as emptier than it is, and the amount missing would
	// change with which sub vault happened to be open — a usage bar that moves
	// when you type a password is worse than no usage bar.
	for _, meta := range v.manifest.SubVaults {
		if _, open := v.subs[meta.ID]; open {
			continue
		}
		for _, item := range meta.Inventory {
			for _, part := range item.Parts {
				if st, ok := byID[part.ProviderID]; ok {
					st.Shards++
					st.Stored += part.Size
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
			statuses[i].Usage = withDeclaredCapacity(statuses[i].Usage, statuses[i].Capacity)
		}(i)
	}
	wg.Wait()

	return statuses, nil
}

// withDeclaredCapacity puts the account holder's own figure in as the total
// where the backend has none of its own.
//
// Only against a used figure somebody has taken. A capacity with nothing
// measured against it draws a bar that says the account is empty, which is a
// worse answer than the honest blank a bucket has given until now — so the
// declared total waits for the count rather than the other way round.
func withDeclaredCapacity(usage provider.Usage, capacity int64) provider.Usage {
	if capacity <= 0 || usage.Total > 0 || !usage.UsedKnown() {
		return usage
	}
	usage.Total = capacity
	usage.Declared = true
	return usage
}

// MeasureProvider counts what is on one account, for the backends that can only
// answer that question by counting (see provider.UsageMeasurer).
//
// This is the expensive half of the usage figures and the reason it has a call
// of its own: a bucket is measured by listing it end to end, so it happens when
// somebody asks for the number and not on the ping that draws the sidebar. The
// backend keeps what it counted, so every card and panel drawn afterwards shows
// it without paying again.
func (v *Vault) MeasureProvider(ctx context.Context, id string) (provider.Usage, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return provider.Usage{}, ErrLocked
	}
	var (
		cfg   provider.Config
		found bool
	)
	for _, c := range v.providers {
		if c.ID == id {
			cfg, found = c, true
			break
		}
	}
	v.mu.RUnlock()
	if !found {
		return provider.Usage{}, fmt.Errorf("no connected account with id %s", id)
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		return provider.Usage{}, err
	}
	measurer, ok := p.(provider.UsageMeasurer)
	if !ok {
		return provider.Usage{}, fmt.Errorf("%s accounts cannot be counted", cfg.Kind)
	}

	usage, err := measurer.MeasureUsage(ctx)
	if err != nil {
		return provider.Usage{}, err
	}
	return withDeclaredCapacity(usage, cfg.Capacity), nil
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

	// Movies titles the files that have been matched against the film database,
	// so a grid of posters can say what each one is rather than what its file
	// is called. Titles only — the rest of a film's record is a request per
	// film, made when somebody opens one.
	Movies map[string]MovieBrief `json:"movies,omitempty"`

	// MovieLookup says whether this folder's videos are matched at all, and
	// which folder carries the setting. Off by default and everywhere: a lookup
	// is the only thing in SAND that talks to a third party.
	MovieLookup MovieLookup `json:"movie_lookup"`

	// FolderArt names, per subfolder path, the file whose thumbnail that folder
	// is drawn with — a poster from the films inside it, or whatever picture
	// somebody picked. A folder with nothing picturable under it is simply
	// absent and keeps its icon (see folderart.go).
	FolderArt map[string]FolderArt `json:"folder_art,omitempty"`
}

// List returns the contents of a folder in one of the vaults inside the file.
//
// The scope is the whole of what separates a sub vault from the main one at
// this level: each has its own root and its own tree beneath it, so there is no
// filtering to get wrong and no path in one that can shadow a path in the
// other.
func (v *Vault) List(scope Scope, dir string) (*Listing, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}

	dir = CleanDir(dir)
	if !m.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	folders, files := m.Children(dir)
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

	thumbs := v.thumbIDsForLocked(m, files)
	if thumbs == nil {
		thumbs = []string{}
	}

	// The subfolders' pictures, worked out in one walk of the index for all of
	// them. They are paths rather than names because a search result's folders
	// come from anywhere, and both draw the same row.
	paths := make([]string, 0, len(folders))
	for _, name := range folders {
		paths = append(paths, JoinPath(dir, name))
	}

	return &Listing{
		Path:        dir,
		Parent:      parent,
		Folders:     folders,
		Files:       append([]*Entry{}, files...),
		Thumbs:      thumbs,
		Movies:      v.movieBriefsForLocked(m, files),
		MovieLookup: v.movieLookupLocked(m, dir),
		FolderArt:   v.folderArtForLocked(m, paths),
	}, nil
}

// Entry returns a single file's metadata by ID.
//
// No scope is asked for and none is needed. An ID is unique across every vault
// in the file, so the search is over whatever is open — which means a caller
// holding an ID never has to remember where it came from, and a file inside a
// sub vault that has since been shut is simply not found.
func (v *Vault) Entry(id string) (*Entry, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	_, e, ok := v.scopeOfEntryLocked(id)
	if !ok {
		return nil, fmt.Errorf("no such file: %s", id)
	}
	return e, nil
}

// Descendants returns every file stored at or below a folder, in no particular
// order. It is what an operation over a subtree walks — matching a films folder
// against the film database, say — and it answers from the index alone.
func (v *Vault) Descendants(dir string) ([]*Entry, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	dir = CleanDir(dir)
	if !v.manifest.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}
	return append([]*Entry{}, v.manifest.Descendants(dir)...), nil
}

// EntryByPath looks a file up by its full browser path rather than its ID,
// which is what a caller working in paths — a filesystem view — has to hand.
func (v *Vault) EntryByPath(scope Scope, path string) (*Entry, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	e := m.ByPath(path)
	if e == nil {
		return nil, fmt.Errorf("no such file: %s", path)
	}
	return e, nil
}

// Folders lists every folder in the vault, root first, as normalized paths.
//
// It is the whole tree in one answer, which is what a "where should this go?"
// picker needs: the alternative is a request per level, and the folder a file
// is being moved into is rarely the one already open. It costs a walk of the
// index and contacts no account — the folder structure is in the manifest, and
// the manifest is already decrypted in memory.
func (v *Vault) Folders(scope Scope) ([]string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	return m.AllFolders(), nil
}

// FolderExists reports whether a folder is in the index. A locked vault knows
// nothing, so it answers false rather than guessing — and so does a scope
// naming a sub vault that has not been opened.
func (v *Vault) FolderExists(scope Scope, dir string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return false
	}
	return m.FolderExists(CleanDir(dir))
}

// destinationLocked resolves a folder a write is aimed at, checking that the
// scope is open and the folder is really in it. It is the preamble every
// upload shares.
func (v *Vault) destinationLocked(scope Scope, dir string) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return "", err
	}
	dir = CleanDir(dir)
	if !m.FolderExists(dir) {
		return "", fmt.Errorf("no such folder: %s", dir)
	}
	return dir, nil
}

// MoveFolder renames a folder, carrying everything beneath it along.
//
// Nothing is transferred. A file records which folder it is in, so moving a
// folder is a rewrite of the index and the stored parts never move — which is
// also what makes it safe to offer: every entry beneath the folder changes in
// the same write, so there is no moment where half a tree answers to its old
// name and half to its new one. Thumbnails come too, since a pack is filed
// under its folder rather than carrying the folder's name inside it.
func (v *Vault) MoveFolder(ctx context.Context, scope Scope, oldDir, newDir string) error {
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
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		return err
	}
	if !m.FolderExists(oldDir) {
		v.mu.Unlock()
		return fmt.Errorf("no such folder: %s", oldDir)
	}
	if m.FolderExists(newDir) {
		v.mu.Unlock()
		return fmt.Errorf("%s already exists", newDir)
	}
	if m.ByPath(newDir) != nil {
		v.mu.Unlock()
		return fmt.Errorf("a file already exists at %s", newDir)
	}
	if parent := CleanDir(path.Dir(newDir)); !m.FolderExists(parent) {
		v.mu.Unlock()
		return fmt.Errorf("no such folder: %s", parent)
	}

	undo := m.moveFolder(oldDir, newDir)
	err = v.persistLocked()
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
func (v *Vault) Mkdir(scope Scope, dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return err
	}
	if err := m.Mkdir(dir); err != nil {
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

	scope, e, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.Unlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	// A move stays inside the vault the file is already in. Crossing from one
	// to the other is Assign, which is a different act: it re-encrypts.
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}
	from := e.Dir

	dir := e.Dir
	if newDir != "" {
		dir = CleanDir(newDir)
		if !m.FolderExists(dir) {
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
	if existing := m.ByPath(JoinPath(dir, name)); existing != nil {
		v.mu.Unlock()
		return nil, fmt.Errorf("%s already exists", JoinPath(dir, name))
	}

	e.Dir = dir
	e.Name = name
	e.ModifiedAt = time.Now().UTC()

	err = v.persistLocked()
	v.mu.Unlock()

	if err != nil {
		return nil, err
	}

	v.moveThumb(ctx, scope, id, from, dir)
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

	// DefaultScheme is the code those uploads are cut with, written "k-of-n".
	// Empty means how many accounts a file lands on settles it, which is what
	// a vault that has never chosen otherwise reports.
	DefaultScheme string `json:"default_scheme,omitempty"`

	// Pending counts the files still stored under a retired data key after a
	// password change, waiting to be re-encrypted under the new one.
	Pending int `json:"pending_migration"`

	// Unresolved counts shard records naming an account this vault is not
	// connected to, and Stranded the files that cannot be opened as a result.
	//
	// Both are normally zero. They are what a recovery run before every account
	// was reconnected leaves behind: the index knows the file exists and where
	// its parts went, and cannot reach enough of them. Connecting the rest and
	// resuming the recovery is what clears them — see Reconcile.
	Unresolved int `json:"unresolved"`
	Stranded   int `json:"stranded"`

	// InheritedKey says the key these files are stored under came from a
	// recovery rather than from this vault, so the password of the vault that
	// died still opens their parts. Reclaim is what clears it.
	InheritedKey bool `json:"inherited_key"`
}

// Stats returns aggregate counters for the main vault.
//
// Deliberately the main vault alone, not a total across everything open. These
// figures are drawn in the app beside the accounts and the placement policy,
// and a number that grew when a sub vault was opened and shrank when it was
// shut would be reporting the state of the session rather than the state of the
// vault. What each sub vault holds is asked for by name — see SubVaults, which
// answers from the inventory while one is shut so the storage figures stay
// whole even when the index behind them cannot be read.
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
		DefaultScheme:   v.store.DefaultScheme,
	}
	for _, f := range v.manifest.Folders {
		folders[f] = true
	}
	s.Unresolved, s.Stranded = v.unreachableLocked()
	s.InheritedKey = v.store.InheritedKeyID != "" && v.store.InheritedKeyID == v.dataKeyID
	for _, e := range v.manifest.Entries {
		s.Files++
		s.Bytes += e.Size
		for _, sh := range e.Shards {
			s.StoredBytes += sh.Size
		}
		if e.Redundancy() < e.Scheme().Total {
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
