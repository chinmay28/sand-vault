package vault

import (
	"context"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Pruning on a schedule, for the accounts that asked for it.
//
// versions.go is the answer to "why does the bucket say ten gigabytes"; this is
// the answer to "and I would rather not have to remember to ask". An account
// with AutoPrune set is swept once a day while the vault is open under the
// server, with exactly the sweep a person would run — the same scan, the same
// holds, the same order of erasing — so that switching it on changes when the
// question is asked and nothing about what is done with the answer.
//
// Once a day rather than after every change, because the question is a listing
// of every version on the bucket, and at Backblaze that is a billed call per
// thousand entries. A bucket that has been kept tidy answers in one call, but
// a bucket answers in one call per day at most however busy the vault is.
//
// The setting lives on the account (provider.Config.AutoPrune) and so travels
// with it in the vault file; the clock lives in memory, so a vault unlocked
// after a night shut is pruned shortly after unlocking rather than at the hour
// it would have been.

// AutoPruneInterval is how often the accounts that asked for it are pruned.
const AutoPruneInterval = 24 * time.Hour

// PruneRecord is what the last scheduled prune did to one account.
type PruneRecord struct {
	At      time.Time `json:"at"`
	Deleted int       `json:"deleted"`
	Bytes   int64     `json:"bytes"`
	Error   string    `json:"error,omitempty"`
}

// keepsVersions reports whether an account's backend can be asked for old
// versions at all, which is the only thing AutoPrune can mean. Decided from the
// backend's type rather than by asking it, so it costs no network call and can
// be answered under the lock.
func keepsVersions(cfg provider.Config) bool {
	p, err := provider.New(cfg)
	if err != nil {
		return false
	}
	_, ok := p.(provider.Versioner)
	return ok
}

// autoPruneTargets lists the connected accounts with AutoPrune set.
func (v *Vault) autoPruneTargets() ([]string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	var ids []string
	for _, cfg := range v.providers {
		if cfg.AutoPrune {
			ids = append(ids, cfg.ID)
		}
	}
	return ids, nil
}

// AutoPruneDue reports whether a scheduled prune should run now: some account
// has asked for it, and none has run within AutoPruneInterval. Measured from
// the last run rather than from a timer, the way HealthCheckDue is, so a
// missed slot is caught up on the next tick rather than skipped.
func (v *Vault) AutoPruneDue(now time.Time) bool {
	ids, err := v.autoPruneTargets()
	if err != nil || len(ids) == 0 {
		return false
	}
	v.pruneMu.Lock()
	last := v.prunedAt
	v.pruneMu.Unlock()
	return last.IsZero() || !now.Before(last.Add(AutoPruneInterval))
}

// AutoPrune sweeps the stale versions off every account that asked for it, and
// records what it did to each so the panels can say so. It is SweepStaleVersions
// aimed at those accounts and nothing more: the holds, the re-scan and the
// order of erasing are all the sweep's own.
//
// The clock is set whether or not the sweep succeeded on every account: an
// account that would not answer is recorded as such and tried again tomorrow,
// not every minute until it does.
func (v *Vault) AutoPrune(ctx context.Context) (*VersionSweepReport, error) {
	ids, err := v.autoPruneTargets()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return &VersionSweepReport{}, nil
	}

	report, err := v.SweepStaleVersions(ctx, ids, false, nil)
	now := time.Now()

	v.pruneMu.Lock()
	defer v.pruneMu.Unlock()
	v.prunedAt = now
	if v.pruneSeen == nil {
		v.pruneSeen = map[string]PruneRecord{}
	}
	if err != nil {
		for _, id := range ids {
			v.pruneSeen[id] = PruneRecord{At: now, Error: err.Error()}
		}
		return nil, err
	}
	for _, id := range ids {
		record := PruneRecord{At: now}
		if per, ok := report.Accounts[id]; ok {
			record.Deleted, record.Bytes, record.Error = per.Deleted, per.Bytes, per.Error
		}
		v.pruneSeen[id] = record
	}
	return report, nil
}

// lastPrune reports what the last scheduled prune did to one account, or nil
// when none has run since the vault was opened.
func (v *Vault) lastPrune(id string) *PruneRecord {
	v.pruneMu.Lock()
	defer v.pruneMu.Unlock()
	record, ok := v.pruneSeen[id]
	if !ok {
		return nil
	}
	return &record
}

// forgetPrune drops what is remembered about an account that has been
// disconnected, so a reconnected one starts with no history.
func (v *Vault) forgetPrune(id string) {
	v.pruneMu.Lock()
	delete(v.pruneSeen, id)
	v.pruneMu.Unlock()
}
