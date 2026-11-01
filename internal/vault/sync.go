package vault

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Carrying the index between the machines that open this vault.
//
// There is no server. Each machine holds its own vault file, and the copies of
// the index every account already carries for recovery (manifest.sand, §3.6)
// are the channel: a machine reads the copies it can reach, folds each into
// its own index under the rules in merge.go, and pushes the result back. What
// a machine wrote is never lost in the process — it is in that machine's own
// vault file, and the next fold carries it to the copies again — so a push
// that lands over another machine's push costs one round, not a change.
//
// When a pull happens:
//
//   - On every unlock, and on demand (SyncIndex, `sand vault sync`, the
//     instances panel).
//   - On a timer while the vault is open: every few minutes once another
//     machine has been seen in the register, every hour until one has — a
//     vault that runs on one machine pays a read of one small object per
//     account per hour to find out that it still does. See SyncDue.
//   - Before every push, once another machine is known, so that what goes out
//     has what came in folded into it (SyncManifestBackup).
//
// A copy sealed under a password this vault does not have is left alone and
// counted; the push's own guard says what to do about it (guardForeignBackup).
// Sub vaults do not travel this way: their sections are sealed under their own
// passwords, so a sub vault made on another machine appears here shut, and
// what is done inside one stays on the machine it was done on.

// PeerSyncInterval is how often the index is pulled while another machine is
// known to open the vault; LoneSyncInterval is how often until one is. The
// first is a few minutes because that is how long a file uploaded on the desk
// should take to appear on the Pi; the second is an hour because that is what
// the pull costs a vault that runs on one machine for nothing.
const (
	PeerSyncInterval = 5 * time.Minute
	LoneSyncInterval = time.Hour
)

// syncFetchTimeout bounds the read of one account's copy.
const syncFetchTimeout = 60 * time.Second

// SyncReport is what one pull did.
type SyncReport struct {
	// Instance is this machine's name.
	Instance string `json:"instance"`

	// Asked counts the accounts whose copy was read for, Answered those that
	// held one this vault could open, and Foreign those that held one it
	// could not — sealed under another password, or another vault's.
	Asked    int `json:"asked"`
	Answered int `json:"answered"`
	Foreign  int `json:"foreign,omitempty"`

	// Outcome is the strongest outcome over every copy folded: merged if any
	// had to be combined, adopted if any was taken whole, otherwise current or
	// ahead.
	Outcome MergeOutcome `json:"outcome"`

	// What this machine's index gained, lost and saw changed, over every fold.
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
	Updated int      `json:"updated"`
	Renamed []string `json:"renamed,omitempty"`

	// Instances is the register as it stands after the pull, this machine
	// first.
	Instances []InstanceInfo `json:"instances"`

	Warnings []string  `json:"warnings,omitempty"`
	SyncedAt time.Time `json:"synced_at"`
}

// Changed reports whether the pull left this machine's index different.
func (r *SyncReport) Changed() bool {
	return r.Outcome == MergeAdopted || r.Outcome == MergeMerged
}

// SyncIndex pulls the copies of the index from every account this machine can
// reach, folds them into its own, writes the result, and — when the fold
// produced a version no machine had — schedules a push of it.
func (v *Vault) SyncIndex(ctx context.Context) (*SyncReport, error) {
	return v.pullIndex(ctx, true)
}

// LastSync is when this machine last pulled the index, zero if it has not.
func (v *Vault) LastSync() time.Time {
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	return v.lastSync
}

// SyncDue reports whether the scheduler should pull now: never on a vault
// whose index backup is off (there are no copies), every PeerSyncInterval
// once another machine is in the register, every LoneSyncInterval until then.
// A vault that has never pulled is due at once, which is what puts the first
// pull right after the unlock.
func (v *Vault) SyncDue(now time.Time) bool {
	v.mu.RLock()
	if v.dataKey == nil || v.store == nil || v.store.ManifestBackupDisabled {
		v.mu.RUnlock()
		return false
	}
	interval := LoneSyncInterval
	if v.hasPeersLocked() {
		interval = PeerSyncInterval
	}
	v.mu.RUnlock()

	last := v.LastSync()
	return last.IsZero() || now.Sub(last) >= interval
}

// cloudCopy is one account's copy of the index, opened.
type cloudCopy struct {
	account  provider.Config
	snapshot *Snapshot
}

// pullIndex is SyncIndex, with a say over whether a fold that produced a new
// version schedules a push. The push path itself pulls first and then pushes
// what it has, so it asks for no second push.
func (v *Vault) pullIndex(ctx context.Context, push bool) (*SyncReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	report := &SyncReport{Instance: v.instance, Outcome: MergeCurrent}
	if v.store.ManifestBackupDisabled {
		v.mu.RUnlock()
		report.Warnings = append(report.Warnings,
			"the index backup is off, so there are no copies of the index on the accounts for "+
				"another machine to read or write — switch it on to open this vault from more than one machine")
		report.Instances, _ = v.Instances()
		report.SyncedAt = time.Now()
		return report, nil
	}
	var configs []provider.Config
	for _, cfg := range v.providers {
		if !v.livesElsewhere(cfg) {
			configs = append(configs, cfg)
		}
	}
	vaultKey := append([]byte(nil), v.vaultKey...)
	v.mu.RUnlock()
	defer crypto.ZeroBytes(vaultKey)

	copies, foreign, warnings := v.fetchCopies(ctx, configs, vaultKey)
	report.Asked = len(configs)
	report.Answered = len(copies)
	report.Foreign = foreign
	report.Warnings = warnings
	if foreign > 0 && len(copies) == 0 && len(configs) > 0 {
		report.Warnings = append(report.Warnings,
			"every copy of the index on the accounts is sealed under a password this vault does not "+
				"have — the vault password was changed on another machine, or these accounts belong to "+
				"another vault; another machine joins this vault by copying its vault file or importing "+
				"its recovery kit as a second instance")
	}

	if err := v.foldCopies(copies, report, push); err != nil {
		return report, err
	}

	v.syncMu.Lock()
	v.lastSync = time.Now()
	v.syncMu.Unlock()
	report.SyncedAt = time.Now()
	report.Instances, _ = v.Instances()
	return report, nil
}

// fetchCopies reads each account's copy of the index at once and opens the
// ones this vault's key opens. An account with no copy is not news; one that
// could not be asked is a warning.
func (v *Vault) fetchCopies(ctx context.Context, configs []provider.Config, vaultKey []byte) (copies []cloudCopy, foreign int, warnings []string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()
			p, err := v.buildProvider(cfg)
			if err != nil {
				mu.Lock()
				warnings = append(warnings, fmt.Sprintf("%s: %v", cfg.Name, err))
				mu.Unlock()
				return
			}
			fetchCtx, cancel := context.WithTimeout(ctx, syncFetchTimeout)
			defer cancel()
			data, err := p.Get(fetchCtx, BackupKey)
			if err != nil {
				if !isNotFound(err) {
					mu.Lock()
					warnings = append(warnings, fmt.Sprintf("could not read the index copy on %s: %v", cfg.Name, err))
					mu.Unlock()
				}
				return
			}
			snapshot, err := openBackupWithKey(data, vaultKey)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				foreign++
				return
			}
			copies = append(copies, cloudCopy{account: cfg, snapshot: snapshot})
		}(cfg)
	}
	wg.Wait()

	// Oldest copy first, so the fold's last word is the freshest copy's, and
	// the same order on every machine.
	sort.Slice(copies, func(i, j int) bool {
		a, b := copies[i].snapshot.CreatedAt, copies[j].snapshot.CreatedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return copies[i].account.ID < copies[j].account.ID
	})
	sort.Strings(warnings)
	return copies, foreign, warnings
}

func isNotFound(err error) bool { return errors.Is(err, provider.ErrNotFound) }

// foldCopies folds every copy into this machine's index under the lock and
// writes what came of it, in the way that fits what came of it (indexWrite).
func (v *Vault) foldCopies(copies []cloudCopy, report *SyncReport, push bool) error {
	if len(copies) == 0 {
		return nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	before := v.manifest
	current := v.manifest
	adopted, merged, register := false, false, false
	for _, c := range copies {
		next, rep := mergeIndexes(current, c.snapshot.Manifest)
		switch rep.Outcome {
		case MergeAdopted:
			adopted = true
		case MergeMerged:
			merged = true
		}
		register = register || rep.RegisterChanged
		if rep.Outcome == MergeAdopted || rep.Outcome == MergeMerged {
			// The copy may name key generations and sub vaults this machine
			// has not got; what its rows need comes with them.
			v.adoptKeysLocked(c.snapshot)
			v.adoptSubVaultsLocked(c.snapshot, next)
			// The register's own view of this machine is the register's; the
			// metas this machine holds for its sub vaults are not lost to a
			// copy that does not list them.
			next.SubVaults = mergeSubVaultMetas(next.SubVaults, current.SubVaults)
		}
		report.Renamed = append(report.Renamed, rep.Renamed...)
		current = next
	}

	switch {
	case merged:
		report.Outcome = MergeMerged
	case adopted:
		report.Outcome = MergeAdopted
	case compareClocks(before.Clock, current.Clock) == clockAfter:
		report.Outcome = MergeAhead
	default:
		report.Outcome = MergeCurrent
	}
	report.Added, report.Removed, report.Updated = diffRows(before, current)

	// A machine the register has never heard of announces itself: one write
	// of its own, so the other machines learn of it — and of which accounts
	// it reaches — from its first pull rather than from its first change. A
	// machine that only ever reads would otherwise stay invisible.
	join := current.Instances[v.instance] == nil

	if !merged && !adopted && !register && !join {
		return nil
	}

	v.manifest = current
	v.indexFP = fingerprints(current)
	if v.manifest.Instances != nil && !merged && !join {
		// Whatever the copies said about this machine, this machine knows
		// better; its entry is rewritten by the write below when it stamps,
		// and left to the copy's word otherwise.
		if own := before.Instances[v.instance]; own != nil {
			v.manifest.Instances[v.instance] = own
		}
	}

	// A combined index is a version no machine has, and is this machine's
	// write: stamped, counted, and pushed if the caller is not about to. So
	// is the write that announces this machine. An adopted copy, or a
	// register that merely learned of a machine, is written down and
	// nothing more.
	stamp := merged || join
	how := indexWrite{stamp: stamp, push: stamp && push}
	if err := v.persistIndexLocked(how); err != nil {
		v.manifest = before
		v.indexFP = fingerprints(before)
		return fmt.Errorf("writing the merged index: %w", err)
	}
	return nil
}

// adoptKeysLocked takes the key generations a copy carries that this vault
// does not hold, as retired keys: rows the copy brought may be sealed under
// them. The active generation stays this machine's own. The caller holds the
// write lock.
func (v *Vault) adoptKeysLocked(snapshot *Snapshot) {
	keys := append([]SnapshotKey(nil), snapshot.Keys...)
	if snapshot.KeyID != "" && snapshot.DataKey != "" {
		keys = append(keys, SnapshotKey{ID: snapshot.KeyID, Key: snapshot.DataKey})
	}
	for _, k := range keys {
		if k.ID == v.dataKeyID {
			continue
		}
		if _, have := v.retired[k.ID]; have {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(k.Key)
		if err != nil || len(raw) != DataKeySize {
			continue
		}
		wrapped, err := seal(v.vaultKey, raw)
		if err != nil {
			continue
		}
		v.retired[k.ID] = raw
		v.store.RetiredKeys = append(v.store.RetiredKeys, wrappedKey{ID: k.ID, Key: wrapped})
	}
	sort.Slice(v.store.RetiredKeys, func(i, j int) bool {
		return v.store.RetiredKeys[i].ID < v.store.RetiredKeys[j].ID
	})
}

// adoptSubVaultsLocked takes the sealed records of sub vaults the merged index
// lists and this vault file does not hold — sub vaults made on another
// machine, which appear here shut. A record this machine already holds is
// never replaced: it may be open here, and its section is this machine's to
// write. The caller holds the write lock.
func (v *Vault) adoptSubVaultsLocked(snapshot *Snapshot, merged *Manifest) {
	have := map[string]bool{}
	for _, rec := range v.store.SubVaults {
		have[rec.ID] = true
	}
	listed := map[string]bool{}
	for _, meta := range merged.SubVaults {
		if meta != nil {
			listed[meta.ID] = true
		}
	}
	for _, rec := range snapshot.SubVaults {
		if !have[rec.ID] && listed[rec.ID] {
			v.store.SubVaults = append(v.store.SubVaults, rec)
			have[rec.ID] = true
		}
	}
}

// pullBeforeScan pulls the shared index ahead of a scan that reasons from it
// about what the accounts hold, when another machine is known to write it, and
// returns the one caveat such a scan has to carry. Nothing when this vault
// runs on one machine.
//
// The caveat is real: a file the other machine is uploading right now has
// parts on the accounts and no row in any copy of the index, exactly like a
// part no file wants. The in-flight register (inflight.go) covers this
// process's own uploads and cannot see another machine's, so the sweep is
// left to the person, with the machines named.
func (v *Vault) pullBeforeScan(ctx context.Context) string {
	v.mu.RLock()
	peers := v.peersLocked()
	v.mu.RUnlock()
	if len(peers) == 0 {
		return ""
	}
	if _, err := v.pullIndex(ctx, true); err != nil && !errors.Is(err, ErrLocked) {
		log.Printf("could not pull the shared index before the scan: %v", err)
	}
	names := make([]string, 0, len(peers))
	for _, peer := range peers {
		names = append(names, peer.Name)
	}
	return fmt.Sprintf("this vault is also open on %s: a file being uploaded there right now has "+
		"parts on the accounts and no row in the index yet, and would show here as parts no file "+
		"wants — sweep only while the other machine is idle", strings.Join(names, ", "))
}
