package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// The other half of the manifest backup: reading one back. Writing lives in
// backup.go, and everything from noticing that an account holds somebody else's
// vault through to rebuilding the index from it lives here.

// ---------------------------------------------------------------------------
// Noticing that there is something to recover
// ---------------------------------------------------------------------------

// shardSuffix is what every stored part's object key ends in. It is the one
// thing an account can be asked about without a vault: part keys are opaque
// hex, but they are opaque hex with a known ending.
const shardSuffix = ".sand"

// RecoverySource is what a single connected account is holding, judged only by
// what can be seen without a password.
type RecoverySource struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Backup is set when the account holds a readable manifest.sand envelope.
	Backup bool `json:"backup"`

	// Foreign says the envelope does not open under this vault's key, which is
	// what makes it worth offering to recover from: it was written by a vault
	// that is not this one — the vault that died.
	Foreign bool `json:"foreign"`

	// Parts counts the stored part files on the account, and Bytes is what they
	// weigh. Both are the account's own answer to "what do you hold", so they
	// describe everything sitting there, not only what a backup accounts for.
	Parts int   `json:"parts"`
	Bytes int64 `json:"bytes"`

	// Error is why this account could not be asked, if it could not be.
	Error string `json:"error,omitempty"`
}

// RecoveryScan is the answer to "is there anything here to recover?", which is
// the question a freshly reinstalled machine has to ask the moment its clouds
// are connected back.
type RecoveryScan struct {
	// VaultEmpty reports that this vault holds no files, which is the only
	// state a recovery can run into — adopting a snapshot replaces the data key
	// and would strand anything already encrypted under the current one.
	VaultEmpty bool `json:"vault_empty"`

	// Available is the prompt condition: an empty vault, and at least one
	// connected account holding another vault's index backup.
	Available bool `json:"available"`

	// Resumable is the other half of the same story, and the reason this is not
	// a single boolean. A recovery run before every account was back leaves an
	// index pointing at accounts that are not connected; connecting them later
	// makes those parts reachable again, and Reconcile is what notices. The
	// vault is not empty by then, so Available is false and there is still
	// something worth offering.
	Resumable bool `json:"resumable"`

	// Unresolved counts the shard records in this vault naming an account it is
	// not connected to, and Stranded the files that cannot be opened as a
	// result. Both are read out of the index, not off the network.
	Unresolved int `json:"unresolved"`
	Stranded   int `json:"stranded"`

	// Parts and Bytes total what the connected accounts are holding.
	Parts int   `json:"parts"`
	Bytes int64 `json:"bytes"`

	Sources  []RecoverySource `json:"sources"`
	Warnings []string         `json:"warnings,omitempty"`
}

// ScanForRecovery asks every connected account what it is holding and reports
// whether a lost vault could be rebuilt from it.
//
// This is deliberately cheap and password-free. It lists each account and reads
// the one small envelope, which is enough to tell three states apart: an
// account with nothing on it, an account holding this vault's own parts and
// backup, and an account still carrying the index of a vault that is gone. Only
// the third is worth interrupting somebody about.
func (v *Vault) ScanForRecovery(ctx context.Context) (*RecoveryScan, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	empty := len(v.manifest.Entries) == 0
	vaultKey := append([]byte(nil), v.vaultKey...)
	configs := append([]provider.Config(nil), v.providers...)
	unresolved, stranded := v.unreachableLocked()
	v.mu.RUnlock()
	defer crypto.ZeroBytes(vaultKey)

	scan := &RecoveryScan{
		VaultEmpty: empty,
		Unresolved: unresolved,
		Stranded:   stranded,
		Resumable:  unresolved > 0,
		Sources:    []RecoverySource{},
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()
			source := v.scanAccount(ctx, cfg, vaultKey)

			mu.Lock()
			defer mu.Unlock()
			scan.Sources = append(scan.Sources, source)
			scan.Parts += source.Parts
			scan.Bytes += source.Bytes
			if source.Error != "" {
				scan.Warnings = append(scan.Warnings,
					fmt.Sprintf("could not ask %s what it holds: %s", cfg.Name, source.Error))
			}
			if source.Backup && source.Foreign {
				scan.Available = empty
			}
		}(cfg)
	}
	wg.Wait()

	sort.Slice(scan.Sources, func(i, j int) bool { return scan.Sources[i].Name < scan.Sources[j].Name })
	sort.Strings(scan.Warnings)
	return scan, nil
}

// unreachableLocked counts the shard records naming an account this vault is
// not connected to, and the files left unopenable by them. The caller must hold
// at least the read lock.
func (v *Vault) unreachableLocked() (unresolved, stranded int) {
	for _, e := range v.manifest.Entries {
		reachable := 0
		for _, s := range e.Shards {
			if _, ok := v.configForLocked(s.ProviderID); ok {
				reachable++
			} else {
				unresolved++
			}
		}
		// Against the file's own code: a 4-of-6 file needs four shards where a
		// 2-of-3 one needs two, and a vault can hold both (§5.3).
		if reachable < e.Scheme().Data {
			stranded++
		}
	}
	return unresolved, stranded
}

// scanAccount is ScanForRecovery's work for one account.
func (v *Vault) scanAccount(ctx context.Context, cfg provider.Config, vaultKey []byte) RecoverySource {
	source := RecoverySource{
		ProviderID: cfg.ID,
		Name:       cfg.Name,
		Kind:       string(cfg.Kind),
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		source.Error = err.Error()
		return source
	}

	objects, err := p.List(ctx, "")
	if err != nil {
		source.Error = err.Error()
		// Still worth asking for the envelope by name: a backend that cannot
		// list can usually still fetch a key it is handed.
	}
	for _, obj := range objects {
		if obj.Key == BackupKey || !strings.HasSuffix(obj.Key, shardSuffix) {
			continue
		}
		source.Parts++
		source.Bytes += obj.Size
	}

	blob, err := p.Get(ctx, BackupKey)
	if err != nil {
		if !errors.Is(err, provider.ErrNotFound) && source.Error == "" {
			source.Error = err.Error()
		}
		return source
	}

	var b Backup
	if json.Unmarshal(blob, &b) != nil || b.Magic != backupMagic {
		// Something is sitting under that name that is not one of ours. Not an
		// error worth reporting — it is simply not a backup.
		return source
	}
	source.Backup = true
	source.Foreign = !b.opensWith(vaultKey)
	return source
}

// ---------------------------------------------------------------------------
// Reading a backup back
// ---------------------------------------------------------------------------

// FetchBackup downloads and opens the manifest backup held by one connected
// account. The password is the one the backup was written under, which is the
// password of the vault that is being recovered, not necessarily the password
// of the vault doing the recovering.
func (v *Vault) FetchBackup(ctx context.Context, providerID, password string) (*Snapshot, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	cfg, ok := v.configForLocked(providerID)
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no connected account with id %s", providerID)
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	blob, err := p.Get(ctx, BackupKey)
	if errors.Is(err, provider.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", cfg.Name, ErrNoBackup)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the manifest backup from %s: %w", cfg.Name, err)
	}
	return OpenBackup(blob, password)
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// maxMissingListed caps how many unrecovered files are named individually. The
// counts and the totals are always exact; it is only the list that is cut, and
// MissingTruncated says by how much. A vault whose accounts are all missing
// would otherwise put its entire file tree in one response.
const maxMissingListed = 200

// MissingFile is one file the recovery could not bring back, and why.
//
// PartsFound below PartsNeeded is the whole story: a file is rebuilt from any
// two of its three parts, so it survives one account going away and no more.
type MissingFile struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	PartsFound  int    `json:"parts_found"`
	PartsNeeded int    `json:"parts_needed"`

	// Accounts names the accounts — as the lost vault knew them — that hold the
	// parts which did not answer. Reconnecting these is what would finish the
	// job, so they are the actionable half of this record.
	Accounts []string `json:"accounts,omitempty"`
}

// MissingAccount is an account the backup names that nothing answered for.
type MissingAccount struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Parts int    `json:"parts"` // parts of any file that were stored here
	Files int    `json:"files"` // files with at least one part here

	// Blocking marks an account without which some file cannot be opened at
	// all, as opposed to one that only held spare copies.
	Blocking bool `json:"blocking"`
}

// RecoveryReport describes what a recovery did, or would do.
//
// The counts come in pairs on purpose: Files against Recoverable, and Bytes
// against RecoverableBytes. What someone wants to know after a recovery is not
// how much came back but how much did not, and that is a subtraction they
// should not have to guess at from a file count when the sizes are what differ.
type RecoveryReport struct {
	Files   int   `json:"files"`
	Folders int   `json:"folders"`
	Bytes   int64 `json:"bytes"`

	Relocated   int `json:"relocated"`   // shards found on a reconnected account
	Unreachable int `json:"unreachable"` // shards whose account is not connected

	// Recoverable counts the files with enough reachable parts to open, and
	// RecoverableBytes what they weigh.
	Recoverable      int   `json:"recoverable"`
	RecoverableBytes int64 `json:"recoverable_bytes"`

	// Degraded counts files that came back openable but short of a full set of
	// parts, so they have no spare left.
	Degraded int `json:"degraded"`

	// Lost is the portion that did not come back: the complement of
	// Recoverable, spelled out rather than left as a subtraction.
	Lost      int   `json:"lost"`
	LostBytes int64 `json:"lost_bytes"`

	// Missing names the lost files, capped at maxMissingListed, and
	// MissingAccounts names the accounts whose absence caused it.
	Missing          []MissingFile    `json:"missing,omitempty"`
	MissingTruncated int              `json:"missing_truncated,omitempty"`
	MissingAccounts  []MissingAccount `json:"missing_accounts,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// Complete reports whether every file the backup described came back.
func (r *RecoveryReport) Complete() bool { return r.Lost == 0 }

// ---------------------------------------------------------------------------
// Rebuilding the index
// ---------------------------------------------------------------------------

// Recover rebuilds this vault's index from a snapshot taken elsewhere.
//
// The snapshot's shard records point at account IDs from the vault that is
// gone, and reconnecting an account gives it a fresh ID, so the mapping has to
// be rebuilt. Rather than trusting names, this asks each connected account
// what it actually holds: part keys are unique and self-describing, so the
// account that answers with a given key is the account that has it.
//
// It refuses to run against a vault that already holds files, because adopting
// a snapshot replaces the data key and would strand anything encrypted under
// the old one.
func (v *Vault) Recover(ctx context.Context, snapshot *Snapshot, dryRun bool) (*RecoveryReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	if len(v.manifest.Entries) > 0 {
		v.mu.RUnlock()
		return nil, fmt.Errorf(
			"this vault already holds %d file(s) — recovering would replace its data key and "+
				"strand them; recover into a fresh vault instead, or import the backup as a "+
				"sub vault of this one", len(v.manifest.Entries))
	}
	if len(v.store.SubVaults) > 0 {
		v.mu.RUnlock()
		return nil, fmt.Errorf(
			"this vault already holds %d sub vault(s) — recovering would replace them with the "+
				"backup's, and nothing here can open them to say what would be lost; recover into "+
				"a fresh vault instead", len(v.store.SubVaults))
	}
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	if len(configs) == 0 {
		return nil, fmt.Errorf("connect the accounts holding the parts before recovering")
	}

	dataKey, err := base64.StdEncoding.DecodeString(snapshot.DataKey)
	if err != nil || len(dataKey) != DataKeySize {
		return nil, fmt.Errorf("the backup carries no usable data key")
	}
	// A backup taken while a password change was still re-encrypting names
	// more than one generation, and the files left on the older one need it.
	retired, err := snapshotRetiredKeys(snapshot)
	if err != nil {
		return nil, err
	}

	// Ask every account what it holds, so shards can be matched to accounts by
	// the keys that are actually there rather than by remembered IDs.
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

	recovered := newManifest()
	recovered.Folders = append([]string(nil), snapshot.Manifest.Folders...)
	// The names and inventories of the sub vaults come back with the sealed
	// records they describe. Without them the recovered vault would hold the
	// ciphertext and have nothing to call it.
	recovered.SubVaults = append([]*SubVaultMeta(nil), snapshot.Manifest.SubVaults...)
	report := &RecoveryReport{Warnings: warnings}
	absent := newAbsentAccounts(snapshot)

	for _, entry := range snapshot.Manifest.Entries {
		clone := *entry
		clone.Shards = make([]Shard, 0, len(entry.Shards))

		reachable := 0
		var lost lostShards
		for _, shard := range entry.Shards {
			if cfg, ok := holders[shard.Key]; ok {
				if cfg.ID != shard.ProviderID {
					report.Relocated++
				}
				shard.ProviderID = cfg.ID
				shard.ProviderName = cfg.Name
				shard.ProviderKind = string(cfg.Kind)
				reachable++
			} else if cfg, ok := byOldID[shard.ProviderID]; ok {
				// The account is connected but did not list the key. Keep the
				// mapping so a transient listing failure does not discard the
				// shard; a health check will show whether it is really there.
				shard.ProviderID = cfg.ID
				shard.ProviderName = cfg.Name
				shard.ProviderKind = string(cfg.Kind)
				reachable++
			} else {
				report.Unreachable++
				absent.note(shard, &lost)
			}
			clone.Shards = append(clone.Shards, shard)
		}

		report.Files++
		report.Bytes += entry.Size
		switch {
		case reachable >= entry.Scheme().Data:
			report.Recoverable++
			report.RecoverableBytes += entry.Size
			if reachable < len(entry.Shards) {
				report.Degraded++
			}
		default:
			report.Lost++
			report.LostBytes += entry.Size
			absent.blame(&lost)
			if len(report.Missing) < maxMissingListed {
				report.Missing = append(report.Missing, MissingFile{
					Path:        entry.Path(),
					Size:        entry.Size,
					PartsFound:  reachable,
					PartsNeeded: entry.Scheme().Data,
					Accounts:    lost.names,
				})
			} else {
				report.MissingTruncated++
			}
		}

		// A dry run answers out of the counts alone and throws the rebuilt
		// index away, so it does not pay for building one: add is a scan of
		// what is already there, which over a whole manifest is quadratic.
		if !dryRun {
			recovered.add(&clone)
		}
	}

	// The sub vaults cannot be opened here, so their shard records cannot be
	// rewritten the way the main vault's just were. What can be done is to work
	// out the translation from the inventory — which names every object they
	// own and the account it was on — and leave it for the moment each one is
	// opened. See Manifest.AccountRemap.
	recovered.AccountRemap = remapForSealedSubVaults(recovered.SubVaults, holders, byOldID)

	folders := map[string]bool{}
	for _, f := range recovered.Folders {
		folders[f] = true
	}
	for _, e := range snapshot.Manifest.Entries {
		if e.Dir != "/" {
			folders[e.Dir] = true
		}
	}
	report.Folders = len(folders)
	report.MissingAccounts = absent.list()

	if dryRun {
		return report, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	if len(v.manifest.Entries) > 0 {
		return nil, fmt.Errorf("this vault gained files while the recovery was running; start again")
	}

	// Adopt the recovered data keys: they are what the stored parts were
	// encrypted under, and new uploads should join them rather than start a
	// key of their own.
	wrapped, err := seal(v.vaultKey, dataKey)
	if err != nil {
		return nil, err
	}
	var wrappedRetired []wrappedKey
	for id, key := range retired {
		sealedKey, err := seal(v.vaultKey, key)
		if err != nil {
			return nil, err
		}
		wrappedRetired = append(wrappedRetired, wrappedKey{ID: id, Key: sealedKey})
	}

	previous := struct {
		dataKey   []byte
		dataKeyID string
		retired   map[string][]byte
		wrapped   sealed
		retiredOn []wrappedKey
		subVaults []subVaultRecord
		policy    Policy
		manifest  *Manifest
		inherited string
	}{v.dataKey, v.dataKeyID, v.retired, v.store.DataKey, v.store.RetiredKeys, v.store.SubVaults,
		v.store.Policy, v.manifest, v.store.InheritedKeyID}

	v.store.DataKey = wrapped
	v.store.DataKeyID = snapshot.KeyID
	v.store.RetiredKeys = wrappedRetired
	// The sealed records come back as they were. Nothing here can open them,
	// and nothing needs to: they are ciphertext to this vault until someone
	// types the password that made them.
	v.store.SubVaults = append([]subVaultRecord(nil), snapshot.SubVaults...)
	// Adopted, not minted. Until these files are re-encrypted, the password of
	// the vault that died still opens their parts — see Reclaim.
	v.store.InheritedKeyID = snapshot.KeyID
	v.dataKey = dataKey
	v.dataKeyID = snapshot.KeyID
	v.retired = retired
	if snapshot.Policy.Valid() {
		v.store.Policy = snapshot.Policy
	}
	v.manifest = recovered

	if err := v.persistLocked(); err != nil {
		// Put back what was there. Leaving the recovered index in memory after
		// a failed write would make a second attempt refuse to run — it would
		// see a vault that already holds files — and would strand the caller
		// with an index that is not on disk.
		v.dataKey = previous.dataKey
		v.dataKeyID = previous.dataKeyID
		v.retired = previous.retired
		v.store.DataKey = previous.wrapped
		v.store.DataKeyID = previous.dataKeyID
		v.store.RetiredKeys = previous.retiredOn
		v.store.SubVaults = previous.subVaults
		v.store.Policy = previous.policy
		v.manifest = previous.manifest
		v.store.InheritedKeyID = previous.inherited
		return nil, err
	}

	crypto.ZeroBytes(previous.dataKey)
	for _, key := range previous.retired {
		crypto.ZeroBytes(key)
	}
	return report, nil
}

// Reconcile finishes a recovery that ran before every account was back.
//
// A partial recovery leaves the index complete and some of it unreachable: the
// vault knows the file exists, knows which account holds each part, and cannot
// reach enough of them to open it. Recover cannot be run a second time — it
// replaces the data key, and by then there are files depending on the one it
// adopted — so resuming is its own operation, and a much smaller one. It asks
// the accounts what they hold, exactly as a recovery does, and re-points the
// records that now have somewhere to point.
//
// No password is involved. The data key was adopted by the recovery that ran
// first; what is missing is not a secret but a reachable copy of the parts.
func (v *Vault) Reconcile(ctx context.Context, dryRun bool) (*RecoveryReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	entries := append([]*Entry(nil), v.manifest.Entries...)
	configs := append([]provider.Config(nil), v.providers...)
	folders := map[string]bool{}
	for _, f := range v.manifest.Folders {
		folders[f] = true
	}
	v.mu.RUnlock()

	if len(configs) == 0 {
		return nil, fmt.Errorf("connect the accounts holding the parts first")
	}

	holders, warnings := v.locateShards(ctx, configs)
	connected := make(map[string]provider.Config, len(configs))
	for _, cfg := range configs {
		connected[cfg.ID] = cfg
	}

	report := &RecoveryReport{Warnings: warnings}
	absent := newReconcileAccounts()
	// Shard by shard rather than entry by entry, so that a re-point applies to
	// the live index rather than to a copy that a concurrent upload would then
	// overwrite. Collected first, applied under the write lock below.
	repointed := map[string]Shard{}

	for _, entry := range entries {
		reachable := 0
		var lost lostShards
		for _, shard := range entry.Shards {
			switch cfg, found := holders[shard.Key]; {
			case found:
				if cfg.ID != shard.ProviderID {
					report.Relocated++
					shard.ProviderID = cfg.ID
					shard.ProviderName = cfg.Name
					shard.ProviderKind = string(cfg.Kind)
					repointed[entry.ID+"\x00"+shard.Key] = shard
				}
				reachable++
			default:
				if _, ok := connected[shard.ProviderID]; ok {
					// Connected but silent: a listing that failed, or an object
					// that has gone. Keeping the record is the conservative
					// answer — a health check is what settles whether it is
					// really there, and it is not this operation's business to
					// throw placement away over one bad listing.
					reachable++
					continue
				}
				report.Unreachable++
				absent.note(shard, &lost)
			}
		}

		report.Files++
		report.Bytes += entry.Size
		if entry.Dir != "/" {
			folders[entry.Dir] = true
		}

		if reachable >= entry.Scheme().Data {
			report.Recoverable++
			report.RecoverableBytes += entry.Size
			if reachable < len(entry.Shards) {
				report.Degraded++
			}
			continue
		}

		report.Lost++
		report.LostBytes += entry.Size
		absent.blame(&lost)
		if len(report.Missing) < maxMissingListed {
			report.Missing = append(report.Missing, MissingFile{
				Path:        entry.Path(),
				Size:        entry.Size,
				PartsFound:  reachable,
				PartsNeeded: entry.Scheme().Data,
				Accounts:    lost.names,
			})
		} else {
			report.MissingTruncated++
		}
	}
	report.MissingAccounts = absent.list()
	report.Folders = len(folders)

	if dryRun || len(repointed) == 0 {
		return report, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	for _, entry := range v.manifest.Entries {
		for i, shard := range entry.Shards {
			if fixed, ok := repointed[entry.ID+"\x00"+shard.Key]; ok {
				entry.Shards[i] = fixed
			}
		}
	}
	if err := v.persistLocked(); err != nil {
		return nil, err
	}
	return report, nil
}

// newReconcileAccounts is absentAccounts for a vault that is already carrying
// the index, where the shard records are the only description of an account
// that was never reconnected — there is no backup being read to name it better.
func newReconcileAccounts() *absentAccounts {
	return &absentAccounts{byID: map[string]*MissingAccount{}, named: map[string]BackupAccount{}}
}

// absentAccounts tallies the accounts a recovery could not reach, so the report
// can end on the one instruction that would change the outcome: connect these.
type absentAccounts struct {
	byID  map[string]*MissingAccount
	named map[string]BackupAccount
}

func newAbsentAccounts(snapshot *Snapshot) *absentAccounts {
	named := make(map[string]BackupAccount, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		named[account.ID] = account
	}
	return &absentAccounts{byID: map[string]*MissingAccount{}, named: named}
}

// lostShards is the per-file running tally of where a recovery came up short:
// the accounts it could not reach, by id for bookkeeping and by name for the
// report. Kept as two slices rather than a map so the names stay in the order
// the parts were stored.
type lostShards struct {
	ids   []string
	names []string
}

// note records one unreachable shard against the account that holds it, and
// against the file being examined.
func (a *absentAccounts) note(shard Shard, lost *lostShards) {
	name, kind := shard.ProviderName, shard.ProviderKind
	if account, ok := a.named[shard.ProviderID]; ok {
		// The backup's own account list is the better source: a shard record
		// keeps the name the account had when the part was written, which a
		// later rename would have left behind.
		name, kind = account.Name, string(account.Kind)
	}
	if name == "" {
		name = "an account this backup does not name"
	}

	missing, ok := a.byID[shard.ProviderID]
	if !ok {
		missing = &MissingAccount{ID: shard.ProviderID, Name: name, Kind: kind}
		a.byID[shard.ProviderID] = missing
	}
	missing.Parts++

	// Deduplicated by id rather than by name: two accounts can wear the same
	// name — a cloud reconnected twice, or two folders both called "backup" —
	// and counting them as one would undercount the files each is holding up.
	for _, already := range lost.ids {
		if already == shard.ProviderID {
			return
		}
	}
	// First part of this file to go missing here, so it is also the first time
	// this file counts towards the account's tally.
	missing.Files++
	lost.ids = append(lost.ids, shard.ProviderID)
	lost.names = append(lost.names, name)
}

// blame marks the accounts a file could not be opened without, as opposed to
// ones that only held a spare copy of something still within reach.
func (a *absentAccounts) blame(lost *lostShards) {
	for _, id := range lost.ids {
		if missing, ok := a.byID[id]; ok {
			missing.Blocking = true
		}
	}
}

// list returns the tally, the accounts holding up the most files first — which
// is the order somebody reconnecting them one at a time wants.
func (a *absentAccounts) list() []MissingAccount {
	if len(a.byID) == 0 {
		return nil
	}
	out := make([]MissingAccount, 0, len(a.byID))
	for _, missing := range a.byID {
		out = append(out, *missing)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Blocking != out[j].Blocking {
			return out[i].Blocking
		}
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// snapshotRetiredKeys decodes the generations a snapshot carries beyond its
// current one, keeping only those its own manifest still points at.
func snapshotRetiredKeys(snapshot *Snapshot) (map[string][]byte, error) {
	needed := map[string]bool{}
	for _, e := range snapshot.Manifest.Entries {
		if e.KeyID != snapshot.KeyID {
			needed[e.KeyID] = true
		}
	}

	retired := map[string][]byte{}
	for _, k := range snapshot.Keys {
		if !needed[k.ID] {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(k.Key)
		if err != nil || len(key) != DataKeySize {
			return nil, fmt.Errorf("the backup carries an unusable data key for generation %q", k.ID)
		}
		retired[k.ID] = key
	}

	for id := range needed {
		if _, ok := retired[id]; !ok {
			return nil, fmt.Errorf(
				"the backup describes files stored under a data key it does not carry (%q) — "+
					"it was written by a build that could not record a key change in progress", id)
		}
	}
	return retired, nil
}

// locateShards asks every connected account for a listing and returns a map of
// object key to the account holding it.
func (v *Vault) locateShards(ctx context.Context, configs []provider.Config) (map[string]provider.Config, []string) {
	var mu sync.Mutex
	holders := map[string]provider.Config{}
	var warnings []string
	var wg sync.WaitGroup

	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			var objects []provider.ObjectInfo
			if err == nil {
				objects, err = p.List(ctx, "")
			}
			if err != nil {
				mu.Lock()
				warnings = append(warnings, fmt.Sprintf(
					"could not list %s, so its parts had to be matched by account name: %v", cfg.Name, err))
				mu.Unlock()
				return
			}

			mu.Lock()
			for _, obj := range objects {
				if obj.Key != BackupKey {
					holders[obj.Key] = cfg
				}
			}
			mu.Unlock()
		}(cfg)
	}
	wg.Wait()

	sort.Strings(warnings)
	return holders, warnings
}

// remapForSealedSubVaults works out which account each sealed sub vault's parts
// are really on now, and rewrites the inventory to match.
//
// The object keys are derivable from the archive ID, so the accounts can be
// asked what they hold and matched exactly, the same way the main vault's own
// shards are matched. Where a part cannot be found — its account offline during
// the listing, or not reconnected at all — the account is matched by name and
// kind instead, which is the same fallback the main path uses.
func remapForSealedSubVaults(
	metas []*SubVaultMeta,
	holders map[string]provider.Config,
	byOldID map[string]provider.Config,
) map[string]string {
	remap := map[string]string{}

	for _, meta := range metas {
		if meta == nil {
			continue
		}
		for i := range meta.Inventory {
			item := &meta.Inventory[i]
			for j := range item.Parts {
				part := &item.Parts[j]

				key := ShardKey(item.ArchiveID, part.Part)
				if item.ChunkCount > 0 {
					key = ChunkShardKey(item.ArchiveID, 0, part.Part)
				}
				cfg, ok := holders[key]
				if !ok {
					if cfg, ok = byOldID[part.ProviderID]; !ok {
						continue
					}
				}
				if cfg.ID != part.ProviderID {
					remap[part.ProviderID] = cfg.ID
					part.ProviderID = cfg.ID
				}
			}
		}
	}

	if len(remap) == 0 {
		return nil
	}
	return remap
}
