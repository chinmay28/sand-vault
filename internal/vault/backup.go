package vault

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// BackupKey is the object key the manifest backup is stored under on every
// connected account, alongside the parts themselves.
const BackupKey = "manifest.sand"

// backupMagic identifies the envelope, so a reader can tell a manifest backup
// from a part file before trying to open it.
const backupMagic = "SAND-MANIFEST"

// backupVersion is the envelope format version.
const backupVersion = 1

// backupCheckPlaintext is sealed under the derived key so a reader can tell a
// wrong password from a corrupt file.
const backupCheckPlaintext = "SAND-MANIFEST-OK"

// ErrNoBackup is returned when an account holds no manifest backup.
var ErrNoBackup = errors.New("this account holds no manifest backup")

// ErrBackupRefused is returned when the vault's configuration makes writing a
// backup unsafe. See Snapshot's documentation for why.
var ErrBackupRefused = errors.New("manifest backup refused")

// errBackupSkipped marks an account left alone for a reason already reported,
// so the same warning is not repeated on every index change.
var errBackupSkipped = errors.New("manifest backup skipped")

// Backup is the envelope written to each account as manifest.sand.
//
// Everything outside Payload is deliberately in the clear, because a reader
// who has lost their vault has nothing but a password: the KDF parameters have
// to travel with the ciphertext or there is no way to derive the key that
// opens it. A salt is not a secret, and Argon2id is what stands between this
// file and someone who has both stolen it and guessed the password.
type Backup struct {
	Magic   string    `json:"magic"`
	Version int       `json:"version"`
	KDF     kdfParams `json:"kdf"`
	Check   sealed    `json:"check"`
	Payload sealed    `json:"payload"`
}

// BackupAccount identifies a connected account without any means of reaching
// it. Credentials are never written to a backup: a copy of this file sits in
// every account, so including them would let one compromised account unlock
// all the others.
type BackupAccount struct {
	ID      string        `json:"id"`
	Kind    provider.Kind `json:"kind"`
	Name    string        `json:"name"`
	AddedAt time.Time     `json:"added_at"`
}

// Snapshot is the plaintext inside a backup: everything needed to rebuild a
// vault except the credentials for the accounts it names.
//
// DataKey is what makes this a recovery kit rather than an inventory. Parts
// are encrypted under the vault's random data key, not under the user's
// password, so a manifest without it would name every file and say exactly
// where its parts live while leaving all of them permanently unreadable.
//
// Carrying it has a cost, which is what backupRefusalLocked guards. A single
// compromised account plus a cracked password yields the file tree, the map of
// which account holds which part, and — because each part is separately
// encrypted under the same key — whatever plaintext that account's own parts
// happen to contain. Reconstructing a whole file still needs parts from a
// second account, which is what keeps this from being a single point of
// failure. That last guarantee disappears under the redundant policy with
// fewer than three accounts, where one account can hold enough parts to
// reconstruct on its own, so the backup refuses to be written there at all.
type Snapshot struct {
	Version   int             `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	Policy    Policy          `json:"policy"`
	DataKey   string          `json:"data_key"` // base64, unwraps the stored parts
	Accounts  []BackupAccount `json:"accounts"`
	Manifest  *Manifest       `json:"manifest"`

	// KeyID names the generation in DataKey, and Keys carries every generation
	// the manifest still refers to, that one included. A password change
	// rotates the data key and re-encrypts the files onto it one at a time, so
	// a backup written while that is in flight describes files sitting on two
	// different keys and has to hand over both.
	KeyID string        `json:"key_id,omitempty"`
	Keys  []SnapshotKey `json:"keys,omitempty"`

	// SubVaults are the sealed records, carried exactly as they sit in the
	// vault file and openable by nothing this snapshot hands over.
	//
	// Carrying them is what makes a recovery whole. A backup that dropped them
	// would restore a vault missing everything its owner had most deliberately
	// protected, and would do it silently — the sub vaults would simply not be
	// there. Carrying them costs nothing in secrecy, because the password that
	// opens this snapshot does not open them: the recovered vault comes back
	// with its sub vaults present and shut, and each one's own password opens
	// it afterwards exactly as before.
	SubVaults []subVaultRecord `json:"sub_vaults,omitempty"`
}

// SnapshotKey is one data key generation carried by a backup.
type SnapshotKey struct {
	ID  string `json:"id"`
	Key string `json:"key"` // base64
}

// ShardPassword is the secret that opens parts written under the snapshot's
// current data key. It is the same value the vault feeds to the archive layer,
// so a caller holding a snapshot and enough part files can rebuild a file with
// no vault.
//
// Prefer ShardPasswordFor when the file is known: after a password change that
// has not finished migrating, some files answer to an older key instead.
func (s *Snapshot) ShardPassword() (string, error) {
	return s.ShardPasswordFor(s.KeyID)
}

// ShardPasswordFor is the secret that opens parts written under one key
// generation, named as a manifest entry names it.
func (s *Snapshot) ShardPasswordFor(keyID string) (string, error) {
	key, err := s.DataKeyFor(keyID)
	if err != nil {
		return "", err
	}
	return shardPasswordFor(key), nil
}

// DataKeyFor is the raw key material of one generation, which is what a chunked
// file's per-chunk keys are derived from. A file stored whole wants
// ShardPasswordFor instead — the same key, spelled the way that format expects.
func (s *Snapshot) DataKeyFor(keyID string) ([]byte, error) {
	encoded := ""
	switch {
	case keyID == s.KeyID:
		encoded = s.DataKey
	default:
		for _, k := range s.Keys {
			if k.ID == keyID {
				encoded = k.Key
				break
			}
		}
	}
	if encoded == "" {
		return nil, fmt.Errorf("this backup carries no data key for generation %q", keyID)
	}

	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding the recovered data key: %w", err)
	}
	if len(key) != DataKeySize {
		return nil, fmt.Errorf("recovered data key is %d bytes, expected %d", len(key), DataKeySize)
	}
	return key, nil
}

// ShardPasswordForEntry is the secret that opens one file's parts.
func (s *Snapshot) ShardPasswordForEntry(e *Entry) (string, error) {
	return s.ShardPasswordFor(e.KeyID)
}

// DataKeyForEntry is the key material one file's chunks are derived from.
func (s *Snapshot) DataKeyForEntry(e *Entry) ([]byte, error) {
	return s.DataKeyFor(e.KeyID)
}

// sealBackup wraps a snapshot in an envelope that a password alone can open.
func sealBackup(snapshot *Snapshot, kdf kdfParams, key []byte) ([]byte, error) {
	check, err := seal(key, []byte(backupCheckPlaintext))
	if err != nil {
		return nil, err
	}
	payload, err := sealJSON(key, snapshot)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(&Backup{
		Magic:   backupMagic,
		Version: backupVersion,
		KDF:     kdf,
		Check:   check,
		Payload: payload,
	}, "", "  ")
}

// OpenBackup decrypts a manifest backup with the password that wrote it. It
// needs nothing else: the envelope carries its own KDF parameters precisely so
// that a lost vault is not required to read it.
func OpenBackup(data []byte, password string) (*Snapshot, error) {
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("this does not look like a manifest backup: %w", err)
	}
	if b.Magic != backupMagic {
		return nil, fmt.Errorf("this does not look like a manifest backup (magic %q)", b.Magic)
	}
	if b.Version != backupVersion {
		return nil, fmt.Errorf("unsupported manifest backup version %d (this build understands %d)",
			b.Version, backupVersion)
	}

	params, salt, err := b.KDF.toArgon2()
	if err != nil {
		return nil, err
	}
	key := crypto.DeriveKey(password, salt, params)
	defer crypto.ZeroBytes(key)

	plain, err := open(key, b.Check)
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(backupCheckPlaintext)) != 1 {
		return nil, ErrWrongPassword
	}

	snapshot := &Snapshot{}
	if err := openJSON(key, b.Payload, snapshot); err != nil {
		return nil, fmt.Errorf("decrypting the manifest backup: %w", err)
	}
	if snapshot.Manifest == nil {
		snapshot.Manifest = newManifest()
	}
	if snapshot.Manifest.Entries == nil {
		snapshot.Manifest.Entries = []*Entry{}
	}
	if snapshot.Manifest.Folders == nil {
		snapshot.Manifest.Folders = []string{}
	}
	return snapshot, nil
}

// ---------------------------------------------------------------------------
// Writing the backup
// ---------------------------------------------------------------------------

// BackupEnabled reports whether this vault replicates its manifest to the
// connected accounts. It is on unless explicitly turned off.
func (v *Vault) BackupEnabled() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.store == nil || !v.store.ManifestBackupDisabled
}

// SetBackupEnabled turns manifest replication on or off. Turning it off erases
// the copies already sitting on the accounts: someone who switches this off
// wants the recovery data gone, not merely frozen.
func (v *Vault) SetBackupEnabled(ctx context.Context, enabled bool) ([]string, error) {
	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return nil, ErrLocked
	}
	v.store.ManifestBackupDisabled = !enabled
	err := v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}

	if !enabled {
		return v.ForgetBackups(ctx)
	}
	return v.SyncManifestBackup(ctx, false)
}

// backupRefusalLocked reports why a backup must not be written, or "" if it is
// safe to write one. The caller must hold at least the read lock.
//
// The one configuration this refuses is redundant placement with fewer than
// three accounts. There, a single account can already hold enough parts to
// rebuild a file on its own; adding the data key to that same account would
// leave the password as the only thing protecting the whole vault.
func (v *Vault) backupRefusalLocked() string {
	if v.store.Policy != PolicyRedundant {
		return ""
	}
	if len(v.providers) >= archive.PartCount {
		return ""
	}
	return fmt.Sprintf(
		"the redundant policy with %d connected account(s) puts enough parts of a file on one "+
			"account to rebuild it, so a backup there would leave your password as the only thing "+
			"protecting the whole vault — connect %d accounts, or switch this vault to the strict policy",
		len(v.providers), archive.PartCount)
}

// snapshotLocked builds the plaintext to be replicated. The caller must hold
// at least the read lock and the vault must be unlocked.
func (v *Vault) snapshotLocked() *Snapshot {
	accounts := make([]BackupAccount, 0, len(v.providers))
	for _, cfg := range v.providers {
		accounts = append(accounts, BackupAccount{
			ID:      cfg.ID,
			Kind:    cfg.Kind,
			Name:    cfg.Name,
			AddedAt: cfg.AddedAt,
		})
	}
	keys := []SnapshotKey{{
		ID:  v.dataKeyID,
		Key: base64.StdEncoding.EncodeToString(v.dataKey),
	}}
	for id, key := range v.retired {
		keys = append(keys, SnapshotKey{ID: id, Key: base64.StdEncoding.EncodeToString(key)})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })

	return &Snapshot{
		Version:   backupVersion,
		CreatedAt: time.Now().UTC(),
		Policy:    v.store.Policy,
		DataKey:   base64.StdEncoding.EncodeToString(v.dataKey),
		KeyID:     v.dataKeyID,
		Keys:      keys,
		Accounts:  accounts,
		Manifest:  v.manifest,
		SubVaults: append([]subVaultRecord(nil), v.store.SubVaults...),
	}
}

// SyncManifestBackup writes the current manifest to every connected account,
// replacing whatever copy is already there. Each account gets the same bytes,
// so any single one of them is enough to recover from.
//
// It returns a warning per account that could not be written. A backup that
// fails on one account is not an error: the copies on the others still do the
// job, which is the whole reason for replicating it.
// With force set it also overwrites a copy it cannot open. That is what a
// password change needs — after re-keying, the copies on the accounts still
// answer to the old password and have to be replaced rather than preserved —
// and it is how a user deliberately claims an account that another vault has
// already written to.
func (v *Vault) SyncManifestBackup(ctx context.Context, force bool) ([]string, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	if v.store.ManifestBackupDisabled {
		v.mu.RUnlock()
		return nil, nil
	}
	if reason := v.backupRefusalLocked(); reason != "" {
		v.mu.RUnlock()
		// A refusal has to clean up after itself. The configuration can turn
		// unsafe after copies are already out there — switching to redundant
		// placement, or disconnecting an account — and a stale copy carries
		// the same data key as a fresh one.
		erased, _ := v.ForgetBackups(ctx)
		return erased, fmt.Errorf("%w: %s", ErrBackupRefused, reason)
	}

	// A password change leaves copies behind that this vault can no longer
	// open, and that the foreign-backup guard would therefore protect. The
	// vault remembers that until a push has actually replaced them.
	force = force || v.store.BackupNeedsForce

	vaultKey := append([]byte(nil), v.vaultKey...)
	blob, err := sealBackup(v.snapshotLocked(), v.store.KDF, v.vaultKey)
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	defer crypto.ZeroBytes(vaultKey)
	if err != nil {
		return nil, fmt.Errorf("sealing the manifest backup: %w", err)
	}
	if len(configs) == 0 {
		return nil, nil
	}

	var mu sync.Mutex
	var warnings []string
	var wg sync.WaitGroup

	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err == nil {
				if err = v.guardForeignBackup(ctx, p, cfg, vaultKey, force); err != nil {
					if !errors.Is(err, errBackupSkipped) {
						mu.Lock()
						warnings = append(warnings, err.Error())
						mu.Unlock()
					}
					return
				}
				err = p.Put(ctx, BackupKey, blob)
			}
			if err != nil {
				mu.Lock()
				warnings = append(warnings, fmt.Sprintf("manifest backup on %s: %v", cfg.Name, err))
				mu.Unlock()
			}
		}(cfg)
	}
	wg.Wait()

	if len(warnings) == 0 {
		// Every account now holds a copy under the current password, so the
		// standing instruction to overwrite has done its job.
		v.clearBackupForce()
	}

	sort.Strings(warnings)
	return warnings, nil
}

// clearBackupForce records that the copies on the accounts are current again.
//
// It writes the vault file directly rather than going through persistLocked:
// nothing encrypted changed, and persisting would schedule the very push that
// just finished.
func (v *Vault) clearBackupForce() {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store == nil || !v.store.BackupNeedsForce {
		return
	}
	v.store.BackupNeedsForce = false
	if err := writeStore(v.path, v.store); err != nil {
		// Left set, so the next push forces again. The cost of being wrong in
		// this direction is one redundant overwrite of our own backup.
		v.store.BackupNeedsForce = true
		log.Printf("could not record that the manifest backups are current: %v", err)
	}
}

// guardForeignBackup refuses to overwrite a backup this vault cannot open.
//
// Connecting an account to a second vault must not destroy the recovery data
// the first one left there — that is exactly the account someone reaches for
// after losing a vault, and the new vault's own backup would be worthless to
// them. The check runs once per account per unlock: a copy this vault wrote is
// a copy it can overwrite freely from then on.
func (v *Vault) guardForeignBackup(ctx context.Context, p provider.Provider, cfg provider.Config, vaultKey []byte, force bool) error {
	if force {
		return nil
	}

	v.backupMu.Lock()
	checked := v.backupChecked[cfg.ID]
	v.backupMu.Unlock()
	if checked {
		return nil
	}

	existing, err := p.Get(ctx, BackupKey)
	if errors.Is(err, provider.ErrNotFound) {
		v.markBackupChecked(cfg.ID)
		return nil
	}
	if err != nil {
		// Could not read it, so cannot judge it. Writing is the safer failure:
		// a vault with no recoverable backup is the problem being solved.
		v.markBackupChecked(cfg.ID)
		return nil
	}

	var b Backup
	if json.Unmarshal(existing, &b) == nil && b.Magic == backupMagic && !b.opensWith(vaultKey) {
		v.backupMu.Lock()
		alreadyWarned := v.backupWarned[cfg.ID]
		if v.backupWarned == nil {
			v.backupWarned = map[string]bool{}
		}
		v.backupWarned[cfg.ID] = true
		v.backupMu.Unlock()

		if alreadyWarned {
			// The write is still refused; it is only the warning that is said
			// once per account per unlock. Every index change retries the
			// write, and a wall of identical warnings would bury whatever the
			// user was actually doing.
			return errBackupSkipped
		}
		return fmt.Errorf(
			"%s already holds another vault's recovery backup, so it was left alone — "+
				"recover from it with 'sand vault recover', or claim the account with "+
				"'sand vault backup --force'",
			cfg.Name)
	}

	v.markBackupChecked(cfg.ID)
	return nil
}

func (v *Vault) markBackupChecked(id string) {
	v.backupMu.Lock()
	if v.backupChecked == nil {
		v.backupChecked = map[string]bool{}
	}
	v.backupChecked[id] = true
	v.backupMu.Unlock()
}

// opensWith reports whether key decrypts this envelope's verifier.
func (b *Backup) opensWith(key []byte) bool {
	plain, err := open(key, b.Check)
	return err == nil && subtle.ConstantTimeCompare(plain, []byte(backupCheckPlaintext)) == 1
}

// openBackupWithKey opens a manifest backup with the vault key itself rather
// than with the password that derives it.
//
// A recovery kit carries that key (Kit.VaultKey), which is what lets an import
// read the copies of the index sitting on the accounts — copies newer than the
// kit, and the reason an old kit still comes back current — without asking for
// the old vault password. See §4.3 of docs/recovery-kit.md.
func openBackupWithKey(data, vaultKey []byte) (*Snapshot, error) {
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("this does not look like a manifest backup: %w", err)
	}
	if b.Magic != backupMagic {
		return nil, fmt.Errorf("this does not look like a manifest backup (magic %q)", b.Magic)
	}
	if b.Version != backupVersion {
		return nil, fmt.Errorf("unsupported manifest backup version %d (this build understands %d)",
			b.Version, backupVersion)
	}
	if !b.opensWith(vaultKey) {
		return nil, ErrWrongPassword
	}

	snapshot := &Snapshot{}
	if err := openJSON(vaultKey, b.Payload, snapshot); err != nil {
		return nil, fmt.Errorf("decrypting the manifest backup: %w", err)
	}
	if snapshot.Manifest == nil {
		snapshot.Manifest = newManifest()
	}
	snapshot.Manifest.normalize()
	return snapshot, nil
}

// ForgetBackups deletes the manifest backup from every connected account.
func (v *Vault) ForgetBackups(ctx context.Context) ([]string, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	var mu sync.Mutex
	var warnings []string
	var wg sync.WaitGroup

	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err == nil {
				err = p.Delete(ctx, BackupKey)
			}
			if err != nil {
				mu.Lock()
				warnings = append(warnings, fmt.Sprintf("removing the backup from %s: %v", cfg.Name, err))
				mu.Unlock()
			}
		}(cfg)
	}
	wg.Wait()

	sort.Strings(warnings)
	return warnings, nil
}

// ---------------------------------------------------------------------------
// Keeping the copies current
// ---------------------------------------------------------------------------

// scheduleBackup asks the background syncer to push the manifest out. It never
// blocks and never touches the network itself, so it is safe to call while
// holding the vault lock — which is what lets persistLocked be the single
// place that notices the index changed.
//
// Bursts collapse: a push already running simply gets marked as needing to run
// again when it finishes, so a hundred quick uploads cost one or two writes
// per account rather than a hundred.
func (v *Vault) scheduleBackup(force bool) {
	v.backupMu.Lock()
	defer v.backupMu.Unlock()

	v.backupForce = v.backupForce || force
	if v.backupHold {
		// An import is part-way through connecting the accounts. Pushing now
		// would write an index that is missing most of them, and would meet
		// the foreign-backup guard on every account not yet reached — which
		// says "another vault's backup is here" in the middle of the one
		// operation where that is both true and entirely expected. The release
		// runs one forced push over the finished state instead.
		v.backupPending = true
		return
	}
	if v.backupRunning {
		v.backupPending = true
		return
	}
	v.backupRunning = true
	go v.runBackupSync()
}

// holdBackups stops scheduled pushes until releaseBackups, collapsing whatever
// was asked for in between into one push over the finished state.
//
// For an import, which connects the accounts one at a time and would otherwise
// push a half-built index at each of them. Not a general-purpose lock: nothing
// waits on it, and a caller that forgets to release leaves the vault pushing
// only when something else asks.
func (v *Vault) holdBackups() {
	v.backupMu.Lock()
	v.backupHold = true
	v.backupMu.Unlock()
}

// releaseBackups lifts the hold and reports whether anything was asked for
// while it was on.
func (v *Vault) releaseBackups() bool {
	v.backupMu.Lock()
	defer v.backupMu.Unlock()
	v.backupHold = false
	pending := v.backupPending
	v.backupPending = false
	return pending
}

// runBackupSync pushes until no further change has been requested.
func (v *Vault) runBackupSync() {
	for {
		v.backupMu.Lock()
		force := v.backupForce
		v.backupForce = false
		v.backupMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		warnings, err := v.SyncManifestBackup(ctx, force)
		cancel()

		switch {
		case errors.Is(err, ErrLocked):
			// The vault was locked mid-flight. Whatever changed is already on
			// disk, and the next unlock schedules a fresh push.
		case errors.Is(err, ErrBackupRefused):
			log.Printf("manifest backup not written: %v", err)
		case err != nil:
			log.Printf("could not write the manifest backup: %v", err)
		default:
			for _, w := range warnings {
				log.Printf("%s", w)
			}
		}

		v.backupMu.Lock()
		if !v.backupPending {
			v.backupRunning = false
			v.backupIdle.Broadcast()
			v.backupMu.Unlock()
			return
		}
		v.backupPending = false
		v.backupMu.Unlock()
	}
}

// AwaitBackupSync blocks until no background push is running or queued. It
// exists for callers that need the copies to be settled before they look at
// them — tests, and the CLI when it reports what a command wrote.
func (v *Vault) AwaitBackupSync() {
	v.backupMu.Lock()
	defer v.backupMu.Unlock()
	for v.backupRunning {
		v.backupIdle.Wait()
	}
}
