package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Changing the vault password rotates the key that stored files are actually
// encrypted under, which means every file has to be re-encrypted before the old
// key can be thrown away.
//
// That cannot be one atomic act: it is a download and an upload per file across
// accounts that may be slow, rate-limited or offline, and a vault holding a
// terabyte cannot be unusable until it finishes. So the vault carries more than
// one data key. The password change itself is atomic and instant — it mints a
// fresh key, keeps the old one beside it, and rewrites the vault file under the
// new password in a single write. Every file then moves across one at a time,
// each move committed on its own, and each entry says which key its parts
// answer to. The old key is dropped the moment nothing names it.
//
// What that buys: the password is genuinely changed the second the command
// returns, every file stays readable throughout, an interrupted migration
// resumes rather than restarts, and a file whose account is offline holds up
// nothing but itself.

// MigrationReport says what a re-encryption pass did.
type MigrationReport struct {
	// Pending is how many files were still on an older key when the pass
	// started.
	Pending int `json:"pending"`

	// Migrated is how many of them were re-encrypted under the current key.
	Migrated int `json:"migrated"`

	// Remaining is how many are still on an older key now that it has
	// finished. Anything above zero is retryable: run the migration again.
	Remaining int `json:"remaining"`

	// Bytes is the total original size of the files that moved.
	Bytes int64 `json:"bytes"`

	Warnings []string `json:"warnings,omitempty"`
}

// Done reports whether every file is now on the vault's current data key.
func (r *MigrationReport) Done() bool { return r != nil && r.Remaining == 0 }

// ProgressFunc is called after each file is re-encrypted, so a caller running
// a long migration can say where it has got to. done counts the files
// attempted so far, including any that failed.
type ProgressFunc func(path string, done, total int)

// ChangePassword re-seals the vault under a new password and rotates the key
// its files are stored under.
//
// The rotation is the point. Re-wrapping alone would leave every part on the
// accounts encrypted exactly as before, so anyone holding the old password and
// a copy of the old vault file — or of the manifest backup it wrote to each
// account — could still read all of them. A new data key means the old password
// opens nothing, and it is why the files have to be migrated.
//
// With migrate set this returns once every file has moved. Otherwise it returns
// as soon as the password itself has changed, leaving the files on the old key
// until MigrateFiles is called; they stay readable either way. The vault is
// unlocked under the new password when this returns, whether or not it was
// unlocked before.
func (v *Vault) ChangePassword(ctx context.Context, oldPassword, newPassword string, migrate bool) (*MigrationReport, error) {
	// Thumbnails are sealed under the key being retired. They are derived from
	// files that are still stored, so they are erased here rather than
	// migrated: making one again costs a resize, and re-encrypting one costs
	// the same gather-and-scatter a real file does. Erasing them first means
	// it happens while the old key is still the vault's own.
	v.dropAllThumbs(ctx)

	if err := v.rotate(oldPassword, newPassword); err != nil {
		return nil, err
	}
	if !migrate {
		outstanding := v.PendingMigration()
		return &MigrationReport{Pending: outstanding, Remaining: outstanding}, nil
	}
	return v.MigrateFiles(ctx, nil)
}

// rotate mints a fresh data key, keeps whatever older keys the stored files
// still need, and rewrites the vault file under the new password.
func (v *Vault) rotate(oldPassword, newPassword string) error {
	if strings.TrimSpace(newPassword) == "" {
		return fmt.Errorf("new password must not be empty")
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store == nil {
		return ErrNotInitialized
	}

	// Decrypted from the file rather than read out of memory, so this works on
	// a locked vault — and so the old password is verified before anything is
	// written.
	current, err := unsealStore(v.store, oldPassword)
	if err != nil {
		return err
	}

	// newStore generates the fresh data key: a password change gets the same
	// brand-new random key that creating a vault does.
	sf, dataKey, err := newStore(newPassword, v.store.Policy)
	if err != nil {
		current.zero()
		return err
	}
	sf.ManifestBackupDisabled = v.store.ManifestBackupDisabled
	// Every copy on every account is now sealed under a password that no
	// longer opens this vault, and carries a data key that is being retired.
	// They have to be replaced, and the vault remembers that until they are.
	sf.BackupNeedsForce = !sf.ManifestBackupDisabled

	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		current.zero()
		return err
	}
	newVaultKey := crypto.DeriveKey(newPassword, salt, params)

	// Any thumbnail pack ChangePassword did not manage to erase — the vault was
	// locked, or an account was unreachable — is dropped from the index here.
	// Its parts are sealed under a key that is about to be retired with nothing
	// pointing at it, so keeping the pointer would only promise a picture that
	// can no longer be drawn.
	current.manifest.Thumbs = nil

	// The keys the stored files are on have to survive the change, or the
	// files become unreadable the moment the password is typed. Only the
	// generations something still points at are kept.
	retained, err := retainedKeys(current, current.manifest)
	if err != nil {
		current.zero()
		crypto.ZeroBytes(newVaultKey)
		return err
	}
	for id, key := range retained {
		wrapped, err := seal(newVaultKey, key)
		if err != nil {
			current.zero()
			crypto.ZeroBytes(newVaultKey)
			return err
		}
		sf.RetiredKeys = append(sf.RetiredKeys, wrappedKey{ID: id, Key: wrapped})
	}

	if sf.Providers, err = sealJSON(newVaultKey, current.providers); err != nil {
		current.zero()
		crypto.ZeroBytes(newVaultKey)
		return err
	}
	if sf.Manifest, err = sealJSON(newVaultKey, current.manifest); err != nil {
		current.zero()
		crypto.ZeroBytes(newVaultKey)
		return err
	}
	// The settings section survives the change like the accounts do: a film
	// database key is a credential the user set, not something derived from the
	// old password.
	if !current.settings.empty() {
		settings, err := sealJSON(newVaultKey, current.settings)
		if err != nil {
			current.zero()
			crypto.ZeroBytes(newVaultKey)
			return err
		}
		sf.Settings = &settings
	}
	if err := writeStore(v.path, sf); err != nil {
		current.zero()
		crypto.ZeroBytes(newVaultKey)
		return err
	}

	// The file on disk is the new one now; take the in-memory state with it.
	// Anything this vault was already holding is a second copy of the same
	// secrets, so it is wiped rather than merely dropped.
	crypto.ZeroBytes(v.vaultKey)
	crypto.ZeroBytes(v.dataKey)
	for _, key := range v.retired {
		crypto.ZeroBytes(key)
	}

	v.store = sf
	v.vaultKey = newVaultKey
	v.dataKey = dataKey
	v.dataKeyID = sf.DataKeyID
	v.retired = retained
	v.providers = current.providers
	v.manifest = current.manifest
	v.settings = current.settings

	// The copies on the accounts are sealed under the password, so they still
	// answer to the old one until they are replaced. Forced, because the guard
	// against clobbering a foreign backup cannot tell them apart from one.
	v.scheduleBackup(true)
	return nil
}

// retainedKeys picks out the data keys a manifest still depends on. A vault
// with no files keeps none: the old key has nothing left to protect and goes.
func retainedKeys(u *unsealed, manifest *Manifest) (map[string][]byte, error) {
	available := map[string][]byte{u.dataKeyID: u.dataKey}
	for id, key := range u.retired {
		available[id] = key
	}

	retained := map[string][]byte{}
	for _, e := range manifest.Entries {
		if _, done := retained[e.KeyID]; done {
			continue
		}
		key, ok := available[e.KeyID]
		if !ok {
			// Nothing can open this file already — the key it names is not in
			// the vault — but carrying on would quietly bake that in, so say
			// so and let the user decide.
			return nil, fmt.Errorf(
				"%s is recorded under a data key this vault does not hold, so it cannot be "+
					"re-encrypted; recover it from a manifest backup, or remove it with "+
					"'sand rm %s', then change the password", e.Path(), e.Path())
		}
		retained[e.KeyID] = key
	}
	return retained, nil
}

// PendingMigration counts the files still stored under an older data key.
func (v *Vault) PendingMigration() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return 0
	}

	pending := 0
	for _, e := range v.manifest.Entries {
		if e.KeyID != v.dataKeyID {
			pending++
		}
	}
	return pending
}

// MigrateFiles re-encrypts every file that is still on an older data key,
// under the current one, and erases the parts left behind.
//
// It is safe to interrupt and safe to repeat: each file is committed on its
// own, and a file that could not be read — an account offline, too few parts
// left — is reported as a warning and left where it is. Calling it again picks
// up whatever is still outstanding. progress may be nil.
func (v *Vault) MigrateFiles(ctx context.Context, progress ProgressFunc) (*MigrationReport, error) {
	return v.MigrateFilesTo(ctx, nil, progress)
}

// MigrateFilesTo is MigrateFiles with somewhere to put the result.
//
// A password change leaves every file where it is, because moving them is not
// what was asked for. Reclaiming a recovered vault is the case where it is: the
// files are being gathered and scattered anyway, and the accounts they are on
// are the ones a vault that no longer exists chose. accounts nil keeps each
// file where it is.
func (v *Vault) MigrateFilesTo(ctx context.Context, accounts []string, progress ProgressFunc) (*MigrationReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	var pending []string
	for _, e := range v.manifest.Entries {
		if e.KeyID != v.dataKeyID {
			pending = append(pending, e.ID)
		}
	}
	v.mu.RUnlock()

	report := &MigrationReport{Pending: len(pending)}
	for i, id := range pending {
		if err := ctx.Err(); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("stopped after %d of %d file(s): %v", i, len(pending), err))
			break
		}

		path, size, warnings, err := v.migrateFile(ctx, id, accounts)
		report.Warnings = append(report.Warnings, warnings...)
		switch {
		case errors.Is(err, ErrLocked):
			// Nothing further can be read, so stop rather than pile up one
			// failure per remaining file.
			report.Warnings = append(report.Warnings,
				"the vault was locked before the migration finished")
			report.Remaining = report.Pending - report.Migrated
			return report, ErrLocked
		case err != nil:
			report.Warnings = append(report.Warnings, err.Error())
		default:
			report.Migrated++
			report.Bytes += size
		}
		if progress != nil {
			progress(path, i+1, len(pending))
		}
	}

	report.Remaining = v.PendingMigration()
	return report, nil
}

// migrateFile rebuilds one file from its parts and scatters it again under the
// current data key. The index moves to the new parts in a single write, so the
// file is readable through the whole thing: on the old parts until that write,
// on the new ones after it.
//
// accounts, when given, is where the new parts go — followed exactly, since it
// is a choice somebody made rather than a default to top up.
func (v *Vault) migrateFile(ctx context.Context, id string, accounts []string) (path string, size int64, warnings []string, err error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return "", 0, nil, ErrLocked
	}
	entry := v.manifest.ByID(id)
	if entry == nil {
		// Deleted since the list was taken; nothing to move.
		v.mu.RUnlock()
		return "", 0, nil, nil
	}
	path, name := entry.Path(), entry.Name
	// A copy of the entry as it stands, so the parts it is on now can be erased
	// after the new ones commit — including, for a file already chunked, every
	// chunk rather than only the first.
	stale := *entry
	stale.Shards = append([]Shard(nil), entry.Shards...)
	v.mu.RUnlock()

	data, _, err := v.Fetch(ctx, id)
	if err != nil {
		return path, 0, nil, fmt.Errorf("%s could not be re-encrypted: %w", path, err)
	}

	// Re-encrypting is not a move: unless it was asked to be. Left alone, the
	// file goes back to the accounts it was already on, whether they were
	// chosen for it or picked at random — any that have been disconnected since
	// are topped up from what is connected now, rather than leaving the file a
	// part short for good. Given a selection, that selection is followed
	// exactly, which is what reclaiming a recovered vault onto the accounts
	// somebody actually wants to keep amounts to.
	current, exact := accounts, len(accounts) > 0
	if !exact {
		current = make([]string, 0, len(stale.Shards))
		for _, s := range stale.Shards {
			current = append(current, s.ProviderID)
		}
	}

	// Re-encrypting writes the current format, so a file stored whole before
	// chunking existed comes back chunked. That is the cheapest moment to do it:
	// the file has already been gathered and is about to be scattered again, so
	// changing format costs nothing beyond what the re-encryption was paying.
	placed, err := v.scatterChunked(ctx, name, data, current, exact, v.uploadChunkSize())
	warnings = placed.warnings
	if err != nil {
		return path, 0, warnings, fmt.Errorf("re-encrypting %s: %w", path, err)
	}
	fresh := &Entry{
		ArchiveID:  placed.archiveID,
		Shards:     placed.shards,
		ChunkSize:  placed.chunkSize,
		ChunkCount: placed.chunkCount,
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		v.deleteEntryShards(context.WithoutCancel(ctx), fresh)
		return path, 0, warnings, ErrLocked
	}
	e := v.manifest.ByID(id)
	if e == nil {
		// Deleted while it was being re-encrypted. The parts just written are
		// referenced by nothing, so they go the same way the old ones did.
		v.mu.Unlock()
		warnings = append(warnings,
			v.deleteEntryShards(context.WithoutCancel(ctx), fresh)...)
		return path, 0, warnings, nil
	}

	previous := *e
	e.ArchiveID = placed.archiveID
	e.KeyID = placed.keyID
	e.Shards = placed.shards
	e.ChunkSize = placed.chunkSize
	e.ChunkCount = placed.chunkCount
	// ModifiedAt is left alone: the file did not change, only the key that
	// hides it did.
	err = v.persistLocked()
	if err != nil {
		*e = previous
	}
	size = e.Size
	v.mu.Unlock()

	if err != nil {
		v.deleteEntryShards(context.WithoutCancel(ctx), fresh)
		return path, 0, warnings, fmt.Errorf("recording the re-encrypted %s: %w", path, err)
	}

	// The old parts are unreferenced now, and they are the ones the old
	// password could open, so getting rid of them is the point rather than
	// housekeeping. A failure here is reported, not fatal: the index already
	// points at the new parts.
	for _, w := range v.deleteEntryShards(context.WithoutCancel(ctx), &stale) {
		warnings = append(warnings, fmt.Sprintf("%s: a part under the old key is still there — %s", path, w))
	}
	return path, size, warnings, nil
}

// pruneRetiredKeysLocked drops every data key no entry names any more, wiping
// it from memory and from the vault file. Called from persistLocked, so a key
// stops being held the moment the last file leaves it — whether that file was
// migrated or simply deleted.
//
// The caller must hold the write lock.
func (v *Vault) pruneRetiredKeysLocked() {
	if len(v.retired) == 0 {
		return
	}

	inUse := make(map[string]bool, len(v.manifest.Entries))
	for _, e := range v.manifest.Entries {
		inUse[e.KeyID] = true
	}

	dropped := false
	for id, key := range v.retired {
		if inUse[id] {
			continue
		}
		crypto.ZeroBytes(key)
		delete(v.retired, id)
		dropped = true
	}
	if !dropped {
		return
	}

	kept := v.store.RetiredKeys[:0]
	for _, wrapped := range v.store.RetiredKeys {
		if _, ok := v.retired[wrapped.ID]; ok {
			kept = append(kept, wrapped)
		}
	}
	v.store.RetiredKeys = kept
}
