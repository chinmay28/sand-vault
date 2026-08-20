package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
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
	//
	// The main vault's alone. A sub vault's packs are sealed under a key this
	// change does not touch, and erasing them would cost the user their
	// pictures to no purpose.
	v.dropAllThumbs(ctx, MainScope)

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

	// The sub vaults come across untouched, and this line is the whole of what
	// "a password change does not touch a sub vault" means on disk. Their
	// sections are sealed under their own passwords, so there is nothing here
	// to re-wrap — but a fresh store file starts with none of them, and leaving
	// this out silently destroys every sub vault that was not open in memory at
	// the time. An open one would be written back by the next save and hide the
	// bug; a locked one is gone, and nothing is left to say what was in it.
	sf.SubVaults = append([]subVaultRecord(nil), v.store.SubVaults...)
	// Every copy on every account is now sealed under a password that no
	// longer opens this vault, and carries a data key that is being retired.
	// They have to be replaced, and the vault remembers that until they are.
	sf.BackupNeedsForce = !sf.ManifestBackupDisabled

	// What a recovery kit made before today can no longer do: open the copies
	// of manifest.sand on the accounts, which have just moved onto a vault key
	// derived from the new password. The kit still opens — its own secret is
	// not this password — and it still restores the credentials and the tree,
	// so this is a note for the settings panel rather than an invalidation.
	// See §6.5 of docs/recovery-kit.md.
	sf.LastKitExportAt = v.store.LastKitExportAt
	sf.LastKitID = v.store.LastKitID
	sf.LastKitSecret = v.store.LastKitSecret
	sf.LastKitFileCount = v.store.LastKitFileCount
	sf.LastKitAccounts = append([]string(nil), v.store.LastKitAccounts...)
	sf.LastPasswordChangeAt = time.Now().UTC()

	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		current.zero()
		return err
	}
	newVaultKey := crypto.DeriveKey(newPassword, salt, params)

	// Any thumbnail pack ChangePassword did not manage to erase — the vault was
	// locked, or an account was unreachable — is dropped from the main index
	// here.
	// Its parts are sealed under a key that is about to be retired with nothing
	// pointing at it, so keeping the pointer would only promise a picture that
	// can no longer be drawn.
	current.manifest.Thumbs = nil

	// The keys the stored files are on have to survive the change, or the
	// files become unreadable the moment the password is typed. Only the
	// generations something still points at are kept.
	retained, err := retainedKeys(current, current.manifest, v.store.SubVaults)
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

	// The read history was sealed under the key that has just been retired, so
	// it is written again here and not left to the next half-minute mark: a
	// process that stopped in between would leave a sidecar nothing could ever
	// open again.
	//
	// An open vault holds everything the file holds and more, so what is in
	// memory is what gets written. A locked one holds none of it — a password
	// can be changed at the lock screen — and the file is then the only copy
	// there is, so it is re-sealed from the old key to the new one instead.
	// Either way this is the last moment anything can open what is on disk.
	if v.reads.recorded() {
		v.saveReadHistoryLocked(true)
	} else {
		v.resealReadHistoryLocked(current.dataKey, dataKey)
	}

	// The copies on the accounts are sealed under the password, so they still
	// answer to the old one until they are replaced. Forced, because the guard
	// against clobbering a foreign backup cannot tell them apart from one.
	v.scheduleBackup(true)
	return nil
}

// retainedKeys picks out the data keys a manifest still depends on. A vault
// with no files keeps none: the old key has nothing left to protect and goes.
//
// It is asked about the main vault's index alone. A sub vault's entries name
// keys sealed under the sub vault's own password, which this change neither
// holds nor needs: their record is carried across untouched, keys and all.
func retainedKeys(u *unsealed, manifest *Manifest, subs []subVaultRecord) (map[string][]byte, error) {
	available := map[string][]byte{u.dataKeyID: u.dataKey}
	for id, key := range u.retired {
		available[id] = key
	}

	// A file assigned out of a sub vault keeps that sub vault's generation until
	// the re-encryption behind the move catches up, so the main index can name a
	// key the main vault has never held and never should. It is not missing —
	// the sub vault has it, sealed under its own password — so it is skipped
	// rather than demanded, and the file stays readable exactly as it was:
	// whenever that sub vault is open.
	elsewhere := map[string]bool{}
	for _, rec := range subs {
		for _, id := range rec.keyIDs() {
			elsewhere[id] = true
		}
	}

	retained := map[string][]byte{}
	for _, e := range manifest.Entries {
		if _, done := retained[e.KeyID]; done {
			continue
		}
		key, ok := available[e.KeyID]
		if !ok {
			if elsewhere[e.KeyID] {
				continue
			}
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
//
// The main vault's files. A sub vault has its own generations and its own
// deferred re-encryptions, which are its own business and are reported through
// SubVaultInfo.Pending — folding them in here would make the main vault look
// like it had work outstanding that its password change cannot do.
func (v *Vault) PendingMigration() int {
	return v.PendingMigrationIn(MainScope)
}

// PendingMigrationIn counts the files in one vault still stored under one of
// that vault's older data keys.
func (v *Vault) PendingMigrationIn(scope Scope) int {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return 0
	}
	active, err := v.dataKeyIDForLocked(scope)
	if err != nil {
		return 0
	}

	pending := 0
	for _, e := range m.Entries {
		if e.KeyID != active {
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
	return v.MigrateFilesIn(ctx, MainScope, accounts, progress)
}

// MigrateFilesIn is MigrateFilesTo for one vault inside the file. A sub vault
// defers re-encryption exactly as the main vault does — after its own password
// change, and after files are assigned into it — so it needs the same pass.
func (v *Vault) MigrateFilesIn(ctx context.Context, scope Scope, accounts []string, progress ProgressFunc) (*MigrationReport, error) {
	v.mu.RLock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	active, err := v.dataKeyIDForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	var pending []string
	for _, e := range m.Entries {
		if e.KeyID != active {
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

		// No scheme: re-encrypting keeps each file cut the way it already is,
		// whatever that is.
		path, size, warnings, err := v.migrateFile(ctx, scope, id, accounts, archive.Scheme{})
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

	report.Remaining = v.PendingMigrationIn(scope)
	return report, nil
}

// migrateFile rebuilds one file from its shards and scatters it again under the
// current data key. The index moves to the new shards in a single write, so the
// file is readable through the whole thing: on the old shards until that write,
// on the new ones after it.
//
// It is the expensive operation in this package — a whole download and a whole
// upload — and there are three reasons to pay it. A password change has to,
// because the shards are sealed under a key that is being retired. Reclaiming a
// recovered vault has to, onto the accounts somebody actually wants to keep. And
// a change of *scheme* has to, because a 2-of-3 file and a 4-of-6 file share no
// shards at all: the halves of one are not the quarters of the other, so there
// is nothing to copy across and the file has to be cut again. Changing which
// accounts hold a file at the same width is none of these; that is a copy of
// opaque blobs, and Relocate does it without a key.
//
// accounts, when given, is where the new shards go — followed exactly, since it
// is a choice somebody made rather than a default to top up.
//
// scheme, when given, is the code to cut the file with, and giving one is what
// makes this a recode. Left at its zero value the file keeps the code it is
// already cut with, which is what every reason other than a recode wants: a
// password change re-seals a 3-of-5 file as a 3-of-5 file. That has to be said
// rather than inferred, because inferring it from the account count would round
// five accounts up to the default family's six and change the file's scheme
// behind a rotation that was only supposed to change its key.
//
// scope is which of the vaults inside the file the entry belongs to, since an
// assignment can leave one naming a key generation that is not this vault's.
func (v *Vault) migrateFile(ctx context.Context, scope Scope, id string, accounts []string, scheme archive.Scheme) (path string, size int64, warnings []string, err error) {
	v.mu.RLock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return "", 0, nil, err
	}
	entry := m.ByID(id)
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
	// shard short for good. Given a selection, that selection is followed
	// exactly, which is what both reclaiming a recovered vault and changing a
	// file's scheme amount to.
	current, exact := accounts, len(accounts) > 0
	if !exact {
		current = make([]string, 0, len(stale.Shards))
		for _, s := range stale.Shards {
			current = append(current, s.ProviderID)
		}
	}
	// Only when nobody named accounts. A named set is a deliberate placement
	// choice and the count of it settles the code the way an upload's does —
	// which is what reclaiming a recovered vault onto three accounts means, and
	// it has to be free to narrow a 4-of-6 file to 2-of-3 in the process. Left
	// alone, the file goes back exactly as it was.
	if scheme == (archive.Scheme{}) && !exact {
		scheme = stale.Scheme()
	}

	// Re-encrypting writes the current format, so a file stored whole before
	// chunking existed comes back chunked. That is the cheapest moment to do it:
	// the file has already been gathered and is about to be scattered again, so
	// changing format costs nothing beyond what the re-encryption was paying.
	placed, err := v.scatterChunked(ctx, scope, name, data,
		spread{preferred: current, exact: exact, scheme: scheme}, v.uploadChunkSize())
	warnings = placed.warnings
	if err != nil {
		return path, 0, warnings, fmt.Errorf("re-encoding %s: %w", path, err)
	}
	fresh := &Entry{
		ArchiveID:   placed.archiveID,
		Shards:      placed.shards,
		ChunkSize:   placed.chunkSize,
		ChunkCount:  placed.chunkCount,
		DataShards:  placed.scheme.Data,
		TotalShards: placed.scheme.Total,
	}

	v.mu.Lock()
	if m, err = v.manifestForLocked(scope); err != nil {
		v.mu.Unlock()
		v.deleteEntryShards(context.WithoutCancel(ctx), fresh)
		return path, 0, warnings, err
	}
	e := m.ByID(id)
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
	e.DataShards = placed.scheme.Data
	e.TotalShards = placed.scheme.Total
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
	// A thumbnail pack names a generation too, and dropping the key it is
	// sealed under would leave the pack unreadable — the pictures would go
	// silently, one folder at a time.
	for _, pack := range v.manifest.Thumbs {
		if pack != nil {
			inUse[pack.KeyID] = true
		}
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
