package vault

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// Importing a vault found on one of the accounts, as a sub vault of this one.
//
// Noticing that there is something to import is recovery.go's job — ScanForRecovery
// already asks every account what it holds and says which ones carry an index
// this vault's key cannot open. What is here is the other half: bringing one in
// without replacing what is already stored.
//
// Recover refuses to run against a vault that already holds anything, because
// adopting a snapshot replaces the data key and would strand it. Importing as a
// sub vault dissolves that rather than working around it: a snapshot carries an
// index, a data key and a password that opens it, which is precisely what a sub
// vault is. So the found vault lands beside this one — its own tree, its own
// key, its own password — and the all-or-nothing problem does not arise.

// ImportReport says what an import brought in.
type ImportReport struct {
	// SubVault is the vault that was created, with its new name.
	SubVault SubVaultInfo `json:"sub_vault"`

	// Files and Folders are what came across.
	Files   int `json:"files"`
	Folders int `json:"folders"`

	// Relocated counts parts found on an account with a different ID than the
	// backup remembered, which is every part whose account has been
	// reconnected. Unreachable counts the parts whose account is not connected
	// here at all, and Recoverable the files that still have enough parts to be
	// rebuilt.
	Relocated   int `json:"relocated"`
	Unreachable int `json:"unreachable"`
	Recoverable int `json:"recoverable"`

	Warnings []string `json:"warnings,omitempty"`
}

// ImportOptions is everything about an import except which backup it is.
type ImportOptions struct {
	// Label is what the sub vault will be called. Empty falls back to the name
	// of the account the backup was found on.
	Label string

	// Password is what the imported sub vault will answer to from now on.
	//
	// It costs nothing to choose a new one. The snapshot's data key is adopted
	// as it stands, so not a byte is re-encrypted by this — only the section
	// wrapping is derived afresh. What it buys is not being stuck with a
	// password chosen years ago on a machine that is gone.
	Password string

	// AdoptBackup marks the account the backup came from as this vault's, so
	// the guard that refuses to overwrite another vault's recovery data stops
	// refusing and this vault's own backup replaces it.
	//
	// Left off, that account never receives this vault's backup — which quietly
	// reduces how many places the index is replicated to, and leaves the old
	// password able to recover the imported files standalone.
	AdoptBackup bool
}

// ImportAsSubVault brings a vault found on a connected account into this one as
// a sub vault.
//
// The snapshot's shard records name account IDs from the vault that wrote it,
// and reconnecting an account gives it a fresh one, so the mapping is rebuilt
// the way a recovery rebuilds it: by asking each account what object keys it
// actually holds. Part keys are unique and self-describing, so the account that
// answers with a given key is the account that has it.
//
// Files whose parts are on accounts that are not connected here come in anyway,
// counted as unreachable. They are not lost — connecting the account and
// running a health check finds them — and refusing the whole import over one
// missing account would be the wrong trade for someone who is trying to get
// their files back.
//
// Nothing is re-encrypted here. The snapshot's data keys are adopted as they
// are, which is what makes this instant; it also means the old password still
// opens those parts, and that is what MigrateFilesIn on the new sub vault is
// for.
func (v *Vault) ImportAsSubVault(ctx context.Context, providerID, backupPassword string, opts ImportOptions) (*ImportReport, error) {
	snapshot, err := v.FetchBackup(ctx, providerID, backupPassword)
	if err != nil {
		return nil, err
	}
	return v.importSnapshot(ctx, snapshot, providerID, opts)
}

// importSnapshot is ImportAsSubVault once the backup has been opened, which is
// also what importing a manifest file from disk would need.
func (v *Vault) importSnapshot(ctx context.Context, snapshot *Snapshot, providerID string, opts ImportOptions) (*ImportReport, error) {
	if strings.TrimSpace(opts.Password) == "" {
		return nil, fmt.Errorf("choose a password for the imported sub vault")
	}

	dataKey, err := base64.StdEncoding.DecodeString(snapshot.DataKey)
	if err != nil || len(dataKey) != DataKeySize {
		return nil, fmt.Errorf("the backup carries no usable data key")
	}
	retired, err := snapshotRetiredKeys(snapshot)
	if err != nil {
		return nil, err
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	configs := append([]provider.Config(nil), v.providers...)
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		if cfg, ok := v.configForLocked(providerID); ok {
			label = cfg.Name
		}
	}
	v.mu.RUnlock()

	if len(configs) == 0 {
		return nil, fmt.Errorf("connect the accounts holding the parts before importing")
	}
	if label == "" {
		label = "Imported vault"
	}

	// Ask the accounts what they hold, so parts are matched by the keys really
	// there rather than by IDs from a vault that is gone.
	holders, warnings := v.locateShards(ctx, configs)
	byOldID := map[string]provider.Config{}
	for _, account := range snapshot.Accounts {
		for _, cfg := range configs {
			if cfg.Kind == account.Kind && strings.EqualFold(cfg.Name, account.Name) {
				byOldID[account.ID] = cfg
				break
			}
		}
	}

	imported := newManifest()
	imported.Folders = append([]string(nil), snapshot.Manifest.Folders...)
	report := &ImportReport{Warnings: warnings}

	for _, entry := range snapshot.Manifest.Entries {
		clone := *entry
		clone.Shards = make([]Shard, 0, len(entry.Shards))

		reachable := 0
		for _, shard := range entry.Shards {
			switch cfg, ok := holders[shard.Key]; {
			case ok:
				if cfg.ID != shard.ProviderID {
					report.Relocated++
				}
				shard.ProviderID, shard.ProviderName, shard.ProviderKind = cfg.ID, cfg.Name, string(cfg.Kind)
				reachable++
			default:
				if cfg, ok := byOldID[shard.ProviderID]; ok {
					// Connected but silent when listed. Keeping the mapping
					// means a transient listing failure does not discard a part
					// that is really there; a health check settles it.
					shard.ProviderID, shard.ProviderName, shard.ProviderKind = cfg.ID, cfg.Name, string(cfg.Kind)
					reachable++
				} else {
					report.Unreachable++
				}
			}
			clone.Shards = append(clone.Shards, shard)
		}

		if reachable >= archive.MinPartsToRestore {
			report.Recoverable++
		}
		imported.add(&clone)
		report.Files++
	}

	// The thumbnail packs are sealed under the imported keys and their parts
	// are on the accounts, so they come too — the same shard remapping applies.
	if len(snapshot.Manifest.Thumbs) > 0 {
		imported.Thumbs = map[string]*ThumbPack{}
		for dir, pack := range snapshot.Manifest.Thumbs {
			if pack == nil {
				continue
			}
			clone := *pack
			clone.Shards = make([]Shard, 0, len(pack.Shards))
			for _, shard := range pack.Shards {
				if cfg, ok := holders[shard.Key]; ok {
					shard.ProviderID, shard.ProviderName, shard.ProviderKind = cfg.ID, cfg.Name, string(cfg.Kind)
				} else if cfg, ok := byOldID[shard.ProviderID]; ok {
					shard.ProviderID, shard.ProviderName, shard.ProviderKind = cfg.ID, cfg.Name, string(cfg.Kind)
				}
				clone.Shards = append(clone.Shards, shard)
			}
			imported.Thumbs[dir] = &clone
		}
	}

	folders := map[string]bool{}
	for _, f := range imported.Folders {
		folders[f] = true
	}
	for _, e := range imported.Entries {
		if e.Dir != "/" {
			folders[e.Dir] = true
		}
	}
	report.Folders = len(folders)

	// A brand new sub vault under the chosen password, then the snapshot's keys
	// and index adopted into it. Making it first rather than assembling the
	// record by hand means the KDF parameters, the salt and the verifier are
	// the ones every other sub vault gets.
	id := uuid.NewString()
	rec, sub, err := newSubVaultRecord(id, opts.Password)
	if err != nil {
		return nil, err
	}
	sub.dataKey, sub.dataKeyID = dataKey, snapshot.KeyID
	sub.retired = retired
	sub.manifest = imported

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		sub.zero()
		return nil, ErrLocked
	}
	for _, meta := range v.manifest.SubVaults {
		if strings.EqualFold(meta.Label, label) {
			v.mu.Unlock()
			sub.zero()
			return nil, fmt.Errorf("a sub vault named %q already exists — choose another name", label)
		}
	}

	meta := &SubVaultMeta{ID: id, Label: label, CreatedAt: time.Now().UTC()}
	v.store.SubVaults = append(v.store.SubVaults, rec)
	v.manifest.SubVaults = append(v.manifest.SubVaults, meta)
	v.subs[id] = sub

	err = v.persistLocked()
	if err != nil {
		v.store.SubVaults = v.store.SubVaults[:len(v.store.SubVaults)-1]
		v.manifest.SubVaults = v.manifest.SubVaults[:len(v.manifest.SubVaults)-1]
		delete(v.subs, id)
		sub.zero()
	}
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}

	report.SubVault = SubVaultInfo{ID: id, Label: label, CreatedAt: meta.CreatedAt, Unlocked: true}
	report.SubVault.Files = report.Files

	// A vault can itself have held sub vaults, and their sealed records travel
	// in the snapshot. They cannot be nested inside the one just made — nothing
	// here can open them, so there is nowhere to put them — so they land as
	// siblings, named after where they came from. Dropping them instead would
	// lose, silently, exactly the files the person had been most careful with.
	report.Warnings = append(report.Warnings, v.adoptNestedSubVaults(snapshot, label)...)

	if opts.AdoptBackup {
		// The account holds a backup this vault has now taken responsibility
		// for, so the guard against clobbering a stranger's recovery data has
		// to be told — otherwise the vault would silently stop backing up to
		// the account it just imported from.
		v.markBackupChecked(providerID)
		v.backupMu.Lock()
		delete(v.backupWarned, providerID)
		v.backupMu.Unlock()
		v.scheduleBackup(true)
	}
	return report, nil
}

// adoptNestedSubVaults brings the sub vaults of an imported vault in as sub
// vaults of this one, since they cannot be nested inside the one just created.
//
// Their records are adopted exactly as they sit — this vault cannot open them
// and does not need to. Each keeps its own password, which is the one it always
// had, and each is named for the vault it arrived with so a list of six does not
// read as six strangers.
func (v *Vault) adoptNestedSubVaults(snapshot *Snapshot, parentLabel string) []string {
	if len(snapshot.SubVaults) == 0 {
		return nil
	}

	labels := map[string]string{}
	if snapshot.Manifest != nil {
		for _, meta := range snapshot.Manifest.SubVaults {
			if meta != nil {
				labels[meta.ID] = meta.Label
			}
		}
	}

	var warnings []string

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return []string{"the vault was locked before the imported vault's own sub vaults could be brought in"}
	}

	taken := map[string]bool{}
	for _, meta := range v.manifest.SubVaults {
		taken[strings.ToLower(meta.Label)] = true
	}
	byID := map[string]bool{}
	for _, rec := range v.store.SubVaults {
		byID[rec.ID] = true
	}

	added := 0
	for _, rec := range snapshot.SubVaults {
		if byID[rec.ID] {
			warnings = append(warnings, fmt.Sprintf(
				"a sub vault inside %s was already here, so it was not brought in twice", parentLabel))
			continue
		}

		inner := labels[rec.ID]
		if inner == "" {
			inner = "sub vault"
		}
		label := fmt.Sprintf("%s / %s", parentLabel, inner)
		for i := 2; taken[strings.ToLower(label)]; i++ {
			label = fmt.Sprintf("%s / %s (%d)", parentLabel, inner, i)
		}
		taken[strings.ToLower(label)] = true

		meta := &SubVaultMeta{ID: rec.ID, Label: label, CreatedAt: time.Now().UTC()}
		if snapshot.Manifest != nil {
			if original := snapshot.Manifest.SubVaultByID(rec.ID); original != nil {
				meta.CreatedAt = original.CreatedAt
				meta.Inventory = original.Inventory
			}
		}
		v.store.SubVaults = append(v.store.SubVaults, rec)
		v.manifest.SubVaults = append(v.manifest.SubVaults, meta)
		added++
	}

	if added == 0 {
		return warnings
	}
	if err := v.persistLocked(); err != nil {
		v.store.SubVaults = v.store.SubVaults[:len(v.store.SubVaults)-added]
		v.manifest.SubVaults = v.manifest.SubVaults[:len(v.manifest.SubVaults)-added]
		return append(warnings, fmt.Sprintf(
			"the sub vaults inside %s could not be brought in: %v", parentLabel, err))
	}
	return append(warnings, fmt.Sprintf(
		"%s held %d sub vault(s) of its own; they were brought in beside it and still answer to "+
			"their own passwords", parentLabel, added))
}
