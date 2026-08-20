package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// A sub vault is a vault inside the vault, with a password of its own.
//
// It exists for the things you would not want read by someone who has your main
// password — which, once a vault is mounted as a drive and left open on a
// laptop, is a broader set of people than it sounds. So a sub vault is sealed
// under Argon2id of its own password and nothing else: the main password
// unwraps every other section of the vault file and does not unwrap this one.
//
// Three consequences shape everything below.
//
// It has its own namespace. A sub vault's files are not hidden main-vault files
// at hidden main-vault paths; they live in a separate tree with its own root.
// Nothing can collide, and nothing has to be checked against paths that cannot
// be read while the sub vault is shut.
//
// It has its own data keys. A file records the key generation it was sealed
// under, and a sub vault's generations belong to the sub vault, so the existing
// machinery for "this file is on a key the vault holds somewhere" carries sub
// vaults with almost nothing added — including deferring the re-encryption that
// a password change or an assignment implies.
//
// It never appears on a WebDAV mount. Not while locked, and not while unlocked
// either: the share is pinned to the main vault, so the only way to see inside a
// sub vault is to be looking at the app.

// Scope names which vault inside the file an operation addresses. The zero
// value is the main vault, which is what every call site that predates sub
// vaults means and continues to mean.
type Scope string

// MainScope is the vault itself, as opposed to any vault inside it.
const MainScope Scope = ""

// Main reports whether this is the main vault rather than a sub vault.
func (s Scope) Main() bool { return s == MainScope }

// ErrSubVaultLocked is returned when an operation needs a sub vault that has
// not been opened. It is deliberately distinct from ErrLocked: the vault is
// open, and what is being asked for needs one more password.
var ErrSubVaultLocked = errors.New("sub vault is locked")

// ErrNoSubVault is returned when a scope names a sub vault this vault does not
// have.
var ErrNoSubVault = errors.New("no such sub vault")

// SubVaultInfo describes one sub vault to a caller outside this package.
type SubVaultInfo struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	Unlocked  bool      `json:"unlocked"`

	// Files and Bytes are what the sub vault holds. They come from the index
	// itself while it is open and from the inventory while it is shut, so the
	// figures do not collapse to zero the moment a sub vault is locked — an
	// account's usage bar would otherwise lose whatever the sub vault put
	// there.
	Files int   `json:"files"`
	Bytes int64 `json:"stored_bytes"`

	// Pending counts files still on an older key of this sub vault's own,
	// waiting for a re-encryption to finish. Only meaningful while unlocked.
	Pending int `json:"pending_migration,omitempty"`
}

// ---------------------------------------------------------------------------
// Resolving a scope
// ---------------------------------------------------------------------------

// manifestForLocked returns the index a scope addresses. The caller must hold
// at least the read lock.
func (v *Vault) manifestForLocked(scope Scope) (*Manifest, error) {
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	if scope.Main() {
		return v.manifest, nil
	}
	if sub, ok := v.subs[string(scope)]; ok {
		return sub.manifest, nil
	}
	if v.manifest.SubVaultByID(string(scope)) != nil {
		return nil, fmt.Errorf("%w: %s", ErrSubVaultLocked, v.subVaultLabelLocked(string(scope)))
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSubVault, scope)
}

// dataKeyIDForLocked is the key generation new files in a scope are sealed
// under.
func (v *Vault) dataKeyIDForLocked(scope Scope) (string, error) {
	if v.dataKey == nil {
		return "", ErrLocked
	}
	if scope.Main() {
		return v.dataKeyID, nil
	}
	sub, ok := v.subs[string(scope)]
	if !ok {
		if v.manifest.SubVaultByID(string(scope)) != nil {
			return "", fmt.Errorf("%w: %s", ErrSubVaultLocked, v.subVaultLabelLocked(string(scope)))
		}
		return "", fmt.Errorf("%w: %s", ErrNoSubVault, scope)
	}
	return sub.dataKeyID, nil
}

// subVaultForKeyLocked finds the locked sub vault that advertises a key
// generation. A record keeps its key IDs in the clear precisely so this can be
// answered without opening it — which is what turns "this file names a key the
// vault does not hold", a corruption, into "that file is in a sub vault you
// have not opened", a password prompt.
func (v *Vault) subVaultForKeyLocked(keyID string) (string, bool) {
	if v.store == nil {
		return "", false
	}
	for _, rec := range v.store.SubVaults {
		if _, open := v.subs[rec.ID]; open {
			continue
		}
		for _, id := range rec.keyIDs() {
			if id == keyID {
				return rec.ID, true
			}
		}
	}
	return "", false
}

// subVaultLabelLocked names a sub vault for a message. It falls back to the ID,
// which is all there is on a record whose metadata has somehow gone.
func (v *Vault) subVaultLabelLocked(id string) string {
	if v.manifest != nil {
		if meta := v.manifest.SubVaultByID(id); meta != nil && meta.Label != "" {
			return meta.Label
		}
	}
	return id
}

// scopeOfEntryLocked finds which open vault holds an entry, by ID. Endpoints
// that address a file by its ID — reading it, moving it, deleting it — never
// have to be told which vault it is in, because an ID is unique across all of
// them and this is how it is resolved.
func (v *Vault) scopeOfEntryLocked(id string) (Scope, *Entry, bool) {
	if v.manifest != nil {
		if e := v.manifest.ByID(id); e != nil {
			return MainScope, e, true
		}
	}
	for subID, sub := range v.subs {
		if e := sub.manifest.ByID(id); e != nil {
			return Scope(subID), e, true
		}
	}
	return MainScope, nil, false
}

// ScopeOf reports which of the vaults inside the file holds an entry.
//
// For callers that hold a file ID and need to write something filed by folder —
// a thumbnail pack, which belongs to a vault as much as the file does. Reading
// the file itself never needs this: an ID resolves against every open vault on
// its own.
func (v *Vault) ScopeOf(id string) (Scope, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	scope, _, ok := v.scopeOfEntryLocked(id)
	return scope, ok
}

// manifestsLocked returns every readable index, keyed by scope.
func (v *Vault) manifestsLocked() map[Scope]*Manifest {
	out := map[Scope]*Manifest{MainScope: v.manifest}
	for id, sub := range v.subs {
		out[Scope(id)] = sub.manifest
	}
	return out
}

// ---------------------------------------------------------------------------
// Persisting
// ---------------------------------------------------------------------------

// resealSubVaultsLocked writes every open sub vault back into the store file
// and refreshes the metadata the main vault keeps about it.
//
// Locked sub vaults are the interesting case: their records are simply left
// where they are. That is the whole mechanism by which a sub vault survives a
// main password change, a file being uploaded next to it, or a year of the main
// vault being used without it ever being opened.
func (v *Vault) resealSubVaultsLocked(now time.Time) error {
	if len(v.subs) == 0 {
		return nil
	}

	for id, sub := range v.subs {
		sub.manifest.UpdatedAt = now
		sub.pruneRetiredKeys()

		rec, err := sub.record()
		if err != nil {
			return fmt.Errorf("sealing the sub vault %s: %w", v.subVaultLabelLocked(id), err)
		}

		replaced := false
		for i := range v.store.SubVaults {
			if v.store.SubVaults[i].ID == id {
				v.store.SubVaults[i] = rec
				replaced = true
				break
			}
		}
		if !replaced {
			v.store.SubVaults = append(v.store.SubVaults, rec)
		}

		if meta := v.manifest.SubVaultByID(id); meta != nil {
			meta.Inventory = sub.manifest.inventory()
		}
	}
	return nil
}

// pruneRetiredKeys drops the sub vault's own key generations that nothing in
// its index names any more. It mirrors the main vault's pruning and is scoped
// the same way: only an open sub vault's keys are considered, because only an
// open sub vault can be asked what it still refers to.
func (s *subVault) pruneRetiredKeys() {
	if len(s.retired) == 0 {
		return
	}
	inUse := make(map[string]bool, len(s.manifest.Entries))
	for _, e := range s.manifest.Entries {
		inUse[e.KeyID] = true
	}
	for _, pack := range s.manifest.Thumbs {
		if pack != nil {
			inUse[pack.KeyID] = true
		}
	}
	for id, key := range s.retired {
		if !inUse[id] {
			crypto.ZeroBytes(key)
			delete(s.retired, id)
		}
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// SubVaults lists the vaults inside this one, whether or not they are open.
func (v *Vault) SubVaults() ([]SubVaultInfo, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	out := make([]SubVaultInfo, 0, len(v.manifest.SubVaults))
	for _, meta := range v.manifest.SubVaults {
		info := SubVaultInfo{
			ID:        meta.ID,
			Label:     meta.Label,
			CreatedAt: meta.CreatedAt,
		}
		if sub, ok := v.subs[meta.ID]; ok {
			info.Unlocked = true
			info.Files = len(sub.manifest.Entries)
			for _, e := range sub.manifest.Entries {
				for _, sh := range e.Shards {
					info.Bytes += sh.Size
				}
				if e.KeyID != sub.dataKeyID {
					info.Pending++
				}
			}
		} else {
			// Shut, so the inventory answers instead. It counts archives rather
			// than files, which for everything but a thumbnail pack is the same
			// number.
			for _, item := range meta.Inventory {
				info.Files++
				for _, p := range item.Parts {
					info.Bytes += p.Size
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label)
	})
	return out, nil
}

// CreateSubVault makes a new sub vault sealed under its own password and leaves
// it open.
func (v *Vault) CreateSubVault(label, password string) (SubVaultInfo, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return SubVaultInfo{}, fmt.Errorf("a sub vault needs a name")
	}
	if strings.TrimSpace(password) == "" {
		return SubVaultInfo{}, fmt.Errorf("password must not be empty")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return SubVaultInfo{}, ErrLocked
	}
	for _, meta := range v.manifest.SubVaults {
		if strings.EqualFold(meta.Label, label) {
			return SubVaultInfo{}, fmt.Errorf("a sub vault named %q already exists", label)
		}
	}

	id := uuid.NewString()
	rec, sub, err := newSubVaultRecord(id, password)
	if err != nil {
		return SubVaultInfo{}, err
	}

	meta := &SubVaultMeta{ID: id, Label: label, CreatedAt: time.Now().UTC()}
	v.store.SubVaults = append(v.store.SubVaults, rec)
	v.manifest.SubVaults = append(v.manifest.SubVaults, meta)
	v.subs[id] = sub

	if err := v.persistLocked(); err != nil {
		v.store.SubVaults = v.store.SubVaults[:len(v.store.SubVaults)-1]
		v.manifest.SubVaults = v.manifest.SubVaults[:len(v.manifest.SubVaults)-1]
		delete(v.subs, id)
		sub.zero()
		return SubVaultInfo{}, err
	}

	return SubVaultInfo{ID: id, Label: label, CreatedAt: meta.CreatedAt, Unlocked: true}, nil
}

// UnlockSubVault opens one sub vault with its password.
//
// It costs a full Argon2id pass, exactly as the main vault's does, which is
// what makes guessing expensive — and exactly why nothing on a hot path should
// be calling it to test whether a password is right.
func (v *Vault) UnlockSubVault(id, password string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	if _, already := v.subs[id]; already {
		return nil
	}

	rec, ok := v.subVaultRecordLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSubVault, id)
	}
	sub, err := unsealSubVault(rec, password)
	if err != nil {
		return err
	}
	v.subs[id] = sub

	// A recovery may have left a translation for this sub vault's shard
	// records, because it could not open the section to rewrite them itself.
	// This is the first moment anything can, so it happens here — before the
	// caller gets a chance to read a file and be told its parts are on an
	// account that no longer exists.
	if v.applyAccountRemapLocked(sub) {
		if err := v.persistLocked(); err != nil {
			return err
		}
	}
	return nil
}

// applyAccountRemapLocked rewrites a freshly opened sub vault's shard records
// onto the accounts a recovery reconnected, and reports whether anything moved.
//
// The remap is kept until no shut sub vault could still be waiting for it, so
// opening one does not strand the others. It is safe to apply twice: after the
// first pass nothing names an old ID, so the second finds nothing to do.
func (v *Vault) applyAccountRemapLocked(sub *subVault) bool {
	if len(v.manifest.AccountRemap) == 0 {
		return false
	}

	byID := make(map[string]provider.Config, len(v.providers))
	for _, cfg := range v.providers {
		byID[cfg.ID] = cfg
	}

	changed := false
	rewrite := func(shards []Shard) {
		for i := range shards {
			to, ok := v.manifest.AccountRemap[shards[i].ProviderID]
			if !ok {
				continue
			}
			shards[i].ProviderID = to
			if cfg, ok := byID[to]; ok {
				shards[i].ProviderName = cfg.Name
				shards[i].ProviderKind = string(cfg.Kind)
			}
			changed = true
		}
	}
	for _, e := range sub.manifest.Entries {
		rewrite(e.Shards)
	}
	for _, pack := range sub.manifest.Thumbs {
		if pack != nil {
			rewrite(pack.Shards)
		}
	}

	// Once every sub vault has been through this, the translation has nothing
	// left to translate.
	if len(v.subs) == len(v.manifest.SubVaults) {
		v.manifest.AccountRemap = nil
		changed = true
	}
	return changed
}

// LockSubVault shuts one sub vault, wiping its keys and its index from memory.
//
// The caches go with them. A decrypted chunk is the file itself and a stream
// link is a key to one, so both are dropped rather than filtered: keeping only
// the main vault's would mean deciding, per cached chunk, which vault it came
// from, and the cost of being wrong is serving a sub vault's plaintext after it
// has been shut.
func (v *Vault) LockSubVault(id string) error {
	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return ErrLocked
	}
	sub, ok := v.subs[id]
	if !ok {
		if v.manifest.SubVaultByID(id) == nil {
			v.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrNoSubVault, id)
		}
		v.mu.Unlock()
		return nil
	}

	// Sealed one last time before the keys go, so anything changed since the
	// last write is on disk rather than lost with the section key.
	if err := v.persistLocked(); err != nil {
		v.mu.Unlock()
		return err
	}

	delete(v.subs, id)
	sub.zero()
	v.mu.Unlock()

	v.chunks.clear()
	v.forgetAllThumbs()
	return nil
}

// subVaultRecordLocked returns the on-disk record for a sub vault.
func (v *Vault) subVaultRecordLocked(id string) (subVaultRecord, bool) {
	if v.store == nil {
		return subVaultRecord{}, false
	}
	for _, rec := range v.store.SubVaults {
		if rec.ID == id {
			return rec, true
		}
	}
	return subVaultRecord{}, false
}

// SubVaultUnlocked reports whether a sub vault is currently open.
func (v *Vault) SubVaultUnlocked(id string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.subs[id]
	return ok
}

// RenameSubVault changes what a sub vault is called. The name is metadata the
// main vault keeps, so this needs the main password rather than the sub
// vault's — you can tidy up the list without opening what is in it.
func (v *Vault) RenameSubVault(id, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("a sub vault needs a name")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	meta := v.manifest.SubVaultByID(id)
	if meta == nil {
		return fmt.Errorf("%w: %s", ErrNoSubVault, id)
	}
	for _, other := range v.manifest.SubVaults {
		if other.ID != id && strings.EqualFold(other.Label, label) {
			return fmt.Errorf("a sub vault named %q already exists", label)
		}
	}

	previous := meta.Label
	meta.Label = label
	if err := v.persistLocked(); err != nil {
		meta.Label = previous
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Moving files between vaults
// ---------------------------------------------------------------------------

// AssignReport says what an assignment did.
type AssignReport struct {
	// Files is how many files changed vaults, and Folders how many folder
	// records travelled with them.
	Files   int `json:"files"`
	Folders int `json:"folders"`

	// Renamed counts the files that landed under a different name because the
	// destination already had one at that path.
	Renamed int `json:"renamed"`

	// Migration is the re-encryption onto the destination's key. It is absent
	// when the assignment was told not to wait for it, in which case the files
	// are still on the key of the vault they came from and MigrateFilesIn
	// finishes the job.
	Migration *MigrationReport `json:"migration,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// Assign moves a file or a folder from one vault inside the file into another.
//
// The path is kept. A folder assigned from the main vault at /Photos/2019
// arrives in the sub vault at /Photos/2019, with the folders above it created
// as it lands, so sending it back puts it exactly where it was rather than
// somewhere that has to be guessed at.
//
// The index move is one write and it is instant: nothing is uploaded, nothing
// is downloaded, and the entries keep the key generation they were sealed
// under until the re-encryption behind the move catches up.
//
// No key crosses the boundary, and that is the point. An earlier version copied
// the generation into the destination's key set so the moved file would read
// immediately — which handed the destination the key sealing everything *else*
// in the source, because a vault's files share one active generation. Assigning
// a single receipt out of a sub vault therefore gave the main password the whole
// sub vault, and the manifest backup replicated that key to every connected
// account. The inventory the main vault keeps names the objects, so the two
// together were a complete break of what a sub vault is for.
//
// So nothing is adopted. A moved file is readable while both vaults are open,
// because dataKeyForLocked searches every open vault — and once the source is
// shut it reports ErrSubVaultLocked until the re-encryption has moved it. That
// is the honest state, and it is why the pass matters: with migrate set this
// returns only once every assigned file is on the destination's own key and the
// parts on the old one are erased. Without it, MigrateFilesIn finishes the job,
// and until it does the file needs both vaults open.
func (v *Vault) Assign(ctx context.Context, from Scope, target string, to Scope, migrate bool) (*AssignReport, error) {
	if from == to {
		return nil, fmt.Errorf("that is already where it is")
	}

	entries, dir, folder, err := v.relocationScope(from, target)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 && !folder {
		return nil, fmt.Errorf("no such file or folder: %s", target)
	}

	report := &AssignReport{}

	// Which pictures have to follow, worked out before the index moves — after
	// it, the source no longer says which folder any of them was in.
	type picture struct {
		id  string
		dir string
	}
	var pictures []picture

	v.mu.Lock()
	src, err := v.manifestForLocked(from)
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}
	dst, err := v.manifestForLocked(to)
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}

	for _, e := range entries {
		if pack := src.Thumbs[e.Dir]; pack != nil && pack.holds(e.ID) {
			pictures = append(pictures, picture{id: e.ID, dir: e.Dir})
		}
	}

	// The folders first, so that a folder assigned while empty still arrives.
	folders := []string{}
	if folder {
		folders = append(folders, dir)
		for _, f := range src.Folders {
			if f == dir || strings.HasPrefix(f, dir+"/") {
				folders = append(folders, f)
			}
		}
	}
	for _, e := range entries {
		folders = append(folders, e.Dir)
	}
	created := map[string]bool{}
	for _, f := range dedupeFolders(folders) {
		if f == "/" || created[f] {
			continue
		}
		if err := dst.Mkdir(f); err != nil {
			v.mu.Unlock()
			return nil, fmt.Errorf("making room for %s in the destination: %w", f, err)
		}
		created[f] = true
		report.Folders++
	}

	// Then the files. Each keeps its key generation, so the destination has to
	// be given that key before anything points at it from over there.
	for _, e := range entries {
		live := src.ByID(e.ID)
		if live == nil {
			// Deleted since the scope was taken.
			continue
		}
		src.remove(live.ID)
		if name := dst.uniqueName(live.Dir, live.Name); name != live.Name {
			live.Name = name
			report.Renamed++
		}
		dst.add(live)
		report.Files++
	}
	if folder {
		src.removeFolders(dir)
		// The folder is not in this vault any more, so a standing instruction
		// naming it would be a schedule sweeping a tree that is not there.
		src.dropAutomations(dir)
	}

	err = v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		// Nothing was written, but the in-memory indexes have already been
		// rearranged, so the vault has to be put back the way it was. Re-reading
		// from disk is the one way to be sure of that.
		return nil, fmt.Errorf("recording the assignment: %w (nothing was moved on the accounts; "+
			"lock and unlock the vault to reload the index)", err)
	}

	// The pictures follow, once the index says where the files live. Failing to
	// carry one costs a thumbnail that is drawn again on the next preview, so
	// every failure here is a warning.
	for _, p := range pictures {
		if err := v.movePictureBetweenVaults(ctx, p.id, from, p.dir, to); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"the thumbnail for %s did not travel: %v", p.id, err))
		}
	}

	if !migrate {
		return report, nil
	}
	migration, err := v.MigrateFilesIn(ctx, to, nil, nil)
	report.Migration = migration
	if migration != nil {
		report.Warnings = append(report.Warnings, migration.Warnings...)
	}
	return report, err
}

// movePictureBetweenVaults carries one thumbnail from a pack in one vault to a
// pack in another, which re-seals it under the destination's key.
//
// Stored in its new home before it is dropped from the old one: a picture that
// exists in both places for a moment is invisible, and one that exists in
// neither is gone.
func (v *Vault) movePictureBetweenVaults(ctx context.Context, id string, from Scope, dir string, to Scope) error {
	items, err := v.loadPack(ctx, from, dir)
	if err != nil {
		return err
	}
	thumb, ok := items[id]
	if !ok {
		return nil
	}
	if err := v.SetThumb(ctx, id, thumb); err != nil {
		return err
	}
	v.removeThumbs(ctx, from, dir, id)
	return nil
}

// ---------------------------------------------------------------------------
// Changing a sub vault's password, and getting rid of one
// ---------------------------------------------------------------------------

// ChangeSubVaultPassword re-seals one sub vault under a new password and
// rotates the key its files are stored under.
//
// It is the main vault's ChangePassword, scoped — and it rotates for the same
// reason. Re-wrapping the section alone would leave every part on the accounts
// encrypted exactly as before, so anyone holding the old password and a copy of
// the old vault file could still read all of them. A fresh data key is what
// makes the old password open nothing, and it is why the files then have to be
// migrated onto it.
//
// The change itself is atomic and instant; the files move across one at a time
// afterwards and stay readable throughout. The sub vault must be open, because
// its own password is what opens it and the old one has to be verified against
// something.
func (v *Vault) ChangeSubVaultPassword(ctx context.Context, id, oldPassword, newPassword string, migrate bool) (*MigrationReport, error) {
	if strings.TrimSpace(newPassword) == "" {
		return nil, fmt.Errorf("new password must not be empty")
	}

	// The thumbnails go first, while the key that seals them is still this sub
	// vault's own. They are derived from files that are still stored, so making
	// them again costs a resize where re-encrypting them costs a gather and a
	// scatter apiece.
	v.dropAllThumbs(ctx, Scope(id))

	v.mu.Lock()

	rec, ok := v.subVaultRecordLocked(id)
	if !ok {
		v.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNoSubVault, id)
	}
	// Opened from the record rather than read out of memory, so the old
	// password is verified before anything is written — the same discipline the
	// main vault's rotation follows.
	current, err := unsealSubVault(rec, oldPassword)
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}

	_, next, err := newSubVaultRecord(id, newPassword)
	if err != nil {
		current.zero()
		v.mu.Unlock()
		return nil, err
	}

	// The index comes across as it stands, and so do the key generations its
	// files are still on — dropping those would make every file in the sub
	// vault unreadable the moment the password was typed.
	next.manifest = current.manifest
	next.manifest.Thumbs = nil
	for _, e := range next.manifest.Entries {
		if e.KeyID == current.dataKeyID {
			next.retired[e.KeyID] = append([]byte(nil), current.dataKey...)
			continue
		}
		if key, held := current.retired[e.KeyID]; held {
			next.retired[e.KeyID] = append([]byte(nil), key...)
			continue
		}
		current.zero()
		next.zero()
		v.mu.Unlock()
		return nil, fmt.Errorf(
			"%s is recorded under a data key this sub vault does not hold, so it cannot be "+
				"re-encrypted; remove it, then change the password", e.Path())
	}

	// The record on disk is rewritten from this by the write below, which is
	// where a sub vault's section is always sealed from.
	previous := v.subs[id]
	v.subs[id] = next

	if err := v.persistLocked(); err != nil {
		v.subs[id] = previous
		current.zero()
		next.zero()
		v.mu.Unlock()
		return nil, err
	}
	// On disk under the new password now, so the keys this process was holding
	// for the old one are a second copy of the same secrets and are wiped
	// rather than merely dropped.
	current.zero()
	if previous != nil {
		previous.zero()
	}
	v.mu.Unlock()

	if !migrate {
		outstanding := v.PendingMigrationIn(Scope(id))
		return &MigrationReport{Pending: outstanding, Remaining: outstanding}, nil
	}
	return v.MigrateFilesIn(ctx, Scope(id), nil, nil)
}

// DeleteSubVault erases a sub vault and everything it holds.
//
// Erasing means the objects on the accounts, not merely the record. Which list
// is used depends on whether it is open: its own index while it is, and the
// inventory the main vault keeps while it is not. The second case is the one
// that matters — a sub vault whose password is gone can never be opened again,
// and without the inventory its parts would sit on the accounts for good, taking
// up room and belonging to nothing.
//
// A shut sub vault has to say force, because nobody can be shown what is about
// to go.
func (v *Vault) DeleteSubVault(ctx context.Context, id string, force bool) ([]string, error) {
	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return nil, ErrLocked
	}
	meta := v.manifest.SubVaultByID(id)
	if meta == nil {
		v.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNoSubVault, id)
	}

	sub, open := v.subs[id]
	if !open && !force {
		count := len(meta.Inventory)
		v.mu.Unlock()
		return nil, fmt.Errorf(
			"%s is locked, so what would be erased cannot be listed — it holds %d stored item(s); "+
				"unlock it first, or delete it anyway", meta.Label, count)
	}

	// The objects to erase, gathered before the record goes.
	var doomed []InventoryItem
	if open {
		doomed = sub.manifest.inventory()
	} else {
		doomed = append([]InventoryItem(nil), meta.Inventory...)
	}
	configs := make(map[string]provider.Config, len(v.providers))
	for _, cfg := range v.providers {
		configs[cfg.ID] = cfg
	}

	// The record and the metadata leave together. Everything after this is
	// erasure on the accounts, which is best-effort: what it cannot reach is a
	// warning, and the sub vault is gone from the vault either way.
	kept := v.store.SubVaults[:0]
	for _, rec := range v.store.SubVaults {
		if rec.ID != id {
			kept = append(kept, rec)
		}
	}
	v.store.SubVaults = kept

	metas := v.manifest.SubVaults[:0]
	for _, m := range v.manifest.SubVaults {
		if m.ID != id {
			metas = append(metas, m)
		}
	}
	v.manifest.SubVaults = metas

	delete(v.subs, id)
	err := v.persistLocked()
	v.mu.Unlock()

	if sub != nil {
		sub.zero()
	}
	if err != nil {
		// The record is still on disk — the write that would have removed it
		// failed — but this process has already dropped it and wiped its keys,
		// so what is in memory and what is in the file no longer agree. Nothing
		// has been erased from the accounts yet, which is what makes reloading
		// the honest instruction rather than a lost cause.
		return nil, fmt.Errorf("%s was not deleted: %w (nothing was erased from your "+
			"accounts; lock and unlock the vault to reload the index)", meta.Label, err)
	}

	v.chunks.clear()
	v.forgetAllThumbs()
	return v.eraseInventory(ctx, doomed, configs), nil
}

// eraseInventory deletes every object an inventory names, returning a warning
// per failure.
//
// The keys are regenerated from the archive ID rather than recorded, which is
// the whole reason the inventory can be this small: one line per archive
// describes however many objects a chunked file occupies.
func (v *Vault) eraseInventory(ctx context.Context, items []InventoryItem, configs map[string]provider.Config) []string {
	var warnings []string

	for _, item := range items {
		for _, part := range item.Parts {
			cfg, ok := configs[part.ProviderID]
			if !ok {
				warnings = append(warnings, fmt.Sprintf(
					"part %d of %s was left behind: its account is no longer connected",
					part.Part, item.ArchiveID))
				continue
			}
			p, err := v.buildProvider(cfg)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf(
					"part %d of %s was left behind: %v", part.Part, item.ArchiveID, err))
				continue
			}

			keys := []string{ShardKey(item.ArchiveID, part.Part)}
			for chunk := 0; chunk < item.ChunkCount; chunk++ {
				keys = append(keys, ChunkShardKey(item.ArchiveID, chunk, part.Part))
			}
			for _, key := range keys {
				if err := p.Delete(ctx, key); err != nil && !errors.Is(err, provider.ErrNotFound) {
					warnings = append(warnings, fmt.Sprintf(
						"%s on %s was left behind: %v", key, cfg.Name, err))
				}
			}
		}
	}
	return warnings
}
