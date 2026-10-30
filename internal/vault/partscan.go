package vault

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// The scan of the local disks.
//
// Everything in lostparts.go waits for something to look at a part: a read,
// the inspector, a sweep over a folder somebody gave a schedule, the
// re-encryption reaching the file. The file nobody reads and no sweep
// covers is the one a disk can lose a part of in March and nobody finds out
// about until a second cloud is down in September.
//
// For a cloud, looking at every part is a request per part and real money
// at the providers that bill for listing, which is why the folder automation
// is opt-in and per folder. For a local folder — a disk on this machine, or
// a folder a desktop client keeps in step with a cloud — it is one directory
// listing: every object's name and size in one call, with nothing read. So
// the local disks get a scan of their own, vault-wide, on a schedule of its
// own: list each one, hold the listing against what the index says the disk
// holds, and write down every part that is not there or is too short to be
// a part (lostparts.go). Daily by default, because the disks this is for
// are the ones that get unplugged, and a day is about how long a lost part
// should go unnoticed on a machine that is on.
//
// A scan says what is missing; it moves nothing. What it finds goes where
// every other finding goes — the degraded list, with the reason on the row,
// and the repair that list offers — and a folder automation with the fix
// action rebuilds the file without anybody asking. A part the scan finds
// back where it should be, on a disk that was plugged in again, is taken
// off the record the same way.

// DefaultPartScanInterval is how often the local disks are scanned when
// nobody has chosen; MinPartScanInterval and MaxPartScanInterval bound the
// choice. A listing is cheap, but a vault of a hundred thousand parts is a
// hundred thousand lines held against the index, and an hour is as often as
// a disk's standing is worth asking.
const (
	DefaultPartScanInterval = 24 * time.Hour
	MinPartScanInterval     = time.Hour
	MaxPartScanInterval     = 30 * 24 * time.Hour
)

// ErrPartScanBusy is returned when a scan is asked for while one is running.
var ErrPartScanBusy = errors.New("a scan of the local disks is already running")

// PartScanSchedule is how often the local disks are scanned, and whether
// they are.
type PartScanSchedule struct {
	Enabled bool `json:"enabled"`

	// Minutes is the interval, in minutes for the same reason the health
	// schedule's is: it crosses to a browser.
	Minutes int `json:"interval_minutes"`
}

// Interval is the schedule as a duration, with the default filled in.
func (s PartScanSchedule) Interval() time.Duration {
	if s.Minutes <= 0 {
		return DefaultPartScanInterval
	}
	return time.Duration(s.Minutes) * time.Minute
}

// PartScanAccount is one disk's share of a scan.
type PartScanAccount struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	Kind provider.Kind `json:"kind"`

	// Parts is how many recorded parts the index expects on the disk, Lost
	// how many of them the listing did not bear out — not there, or too
	// short — and Recovered how many that an earlier look had found missing
	// are there after all.
	Parts     int `json:"parts"`
	Lost      int `json:"lost"`
	Recovered int `json:"recovered,omitempty"`

	// Error is why the disk could not be listed, in which case nothing
	// about its parts was learned and nothing was recorded against it.
	Error string `json:"error,omitempty"`
}

// PartScanReport is what one scan found.
type PartScanReport struct {
	RanAt time.Time `json:"ran_at"`
	// TookMs is how long the scan took, in milliseconds.
	TookMs int64 `json:"took_ms"`

	Accounts []PartScanAccount `json:"accounts"`

	// Parts is every recorded part the scan held against a listing, Lost
	// how many it found missing, Recovered how many it found back, and
	// Files how many files the losses are spread over — the figure the
	// degraded list will show.
	Parts     int `json:"parts"`
	Lost      int `json:"lost"`
	Recovered int `json:"recovered,omitempty"`
	Files     int `json:"files"`

	// Unlisted counts the disks that could not be listed; their rows say
	// why.
	Unlisted int `json:"unlisted,omitempty"`
}

// PartScanStatus is where the scan stands: its schedule, the last run, and
// when the next comes round.
type PartScanStatus struct {
	Schedule PartScanSchedule `json:"schedule"`

	// LocalAccounts is how many connected accounts are on a local disk,
	// which is how many the scan covers; zero means it has nothing to do.
	LocalAccounts int `json:"local_accounts"`

	// Last is the last scan this process ran, absent before the first.
	Last *PartScanReport `json:"last,omitempty"`

	// Running says a scan is going right now.
	Running bool `json:"running"`

	// NextAt is when the scheduler next comes round, absent when the
	// schedule is off or nothing is on a local disk.
	NextAt time.Time `json:"next_at,omitzero"`
}

// partScanMemory is what this process remembers about the scan.
type partScanMemory struct {
	slot    sync.Mutex // the one slot a scan runs in, taken with TryLock
	mu      sync.Mutex // guards the rest
	ranAt   time.Time
	last    *PartScanReport
	running bool
}

// PartScanSchedule returns how often the local disks are scanned. Readable
// while the vault is locked, like the health schedule and for the same
// reason: it is a setting kept in the clear.
func (v *Vault) PartScanSchedule() PartScanSchedule {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.partScanScheduleLocked()
}

func (v *Vault) partScanScheduleLocked() PartScanSchedule {
	if v.store == nil {
		return PartScanSchedule{Enabled: true, Minutes: int(DefaultPartScanInterval / time.Minute)}
	}
	minutes := v.store.PartScanMinutes
	if minutes <= 0 {
		minutes = int(DefaultPartScanInterval / time.Minute)
	}
	// Stored as the negative — see storeFile.PartScanOff — so a vault
	// written before this existed reads as scanned.
	return PartScanSchedule{Enabled: !v.store.PartScanOff, Minutes: minutes}
}

// SetPartScanSchedule changes how often the local disks are scanned, or
// switches the scan off. Switching it off keeps the interval, so switching
// it back on returns to the schedule somebody chose.
func (v *Vault) SetPartScanSchedule(s PartScanSchedule) (PartScanSchedule, error) {
	if s.Minutes != 0 {
		interval := time.Duration(s.Minutes) * time.Minute
		if interval < MinPartScanInterval || interval > MaxPartScanInterval {
			return PartScanSchedule{}, fmt.Errorf(
				"scan the local disks at most every %s and at least every %s",
				MinPartScanInterval, MaxPartScanInterval)
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return PartScanSchedule{}, ErrLocked
	}
	v.store.PartScanOff = !s.Enabled
	if s.Minutes != 0 {
		v.store.PartScanMinutes = s.Minutes
	}
	if err := v.persistLocked(); err != nil {
		return PartScanSchedule{}, err
	}
	return v.partScanScheduleLocked(), nil
}

// PartScanDue reports whether the scheduler should run a scan now: the
// schedule is on, something is on a local disk, and the last scan this
// process ran is older than the interval. A process that has never scanned
// is due at once, as the health check is, so a vault opened after a night
// shut is looked at on opening.
func (v *Vault) PartScanDue(now time.Time) bool {
	schedule := v.PartScanSchedule()
	if !schedule.Enabled {
		return false
	}
	v.mu.RLock()
	local := len(v.localAccountsLocked())
	v.mu.RUnlock()
	if local == 0 {
		return false
	}
	v.partScan.mu.Lock()
	last := v.partScan.ranAt
	v.partScan.mu.Unlock()
	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(schedule.Interval()))
}

// PartScanStatus is where the scan stands. Contacts nobody.
func (v *Vault) PartScanStatus() (PartScanStatus, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return PartScanStatus{}, ErrLocked
	}
	status := PartScanStatus{
		Schedule:      v.partScanScheduleLocked(),
		LocalAccounts: len(v.localAccountsLocked()),
	}
	v.mu.RUnlock()

	v.partScan.mu.Lock()
	status.Last = v.partScan.last
	status.Running = v.partScan.running
	ranAt := v.partScan.ranAt
	v.partScan.mu.Unlock()

	if status.Schedule.Enabled && status.LocalAccounts > 0 {
		if ranAt.IsZero() {
			status.NextAt = time.Now().UTC()
		} else {
			status.NextAt = ranAt.Add(status.Schedule.Interval())
		}
	}
	return status, nil
}

// localAccountsLocked is the connected accounts that are folders on this
// machine's disks, in the vault's order. The caller holds at least the read
// lock.
func (v *Vault) localAccountsLocked() []provider.Config {
	var out []provider.Config
	for _, cfg := range v.providers {
		if cfg.Kind.OnLocalDisk() {
			out = append(out, cfg)
		}
	}
	return out
}

// expectedPart is one part the index says a disk holds.
type expectedPart struct {
	entryID   string
	archiveID string
	shard     Shard
	key       string
}

// expectedPartsLocked is every part the index records on each of the given
// accounts, by account ID, every chunk of a chunked file counted on its own.
// The caller holds at least the read lock.
func (v *Vault) expectedPartsLocked(accounts []provider.Config) map[string][]expectedPart {
	wanted := map[string]bool{}
	for _, cfg := range accounts {
		wanted[cfg.ID] = true
	}
	out := map[string][]expectedPart{}
	for _, e := range v.manifest.Entries {
		for _, s := range e.Shards {
			if !wanted[s.ProviderID] {
				continue
			}
			if !e.Chunked() {
				out[s.ProviderID] = append(out[s.ProviderID], expectedPart{e.ID, e.ArchiveID, s, s.Key})
				continue
			}
			for index := 0; index < e.ChunkCount; index++ {
				out[s.ProviderID] = append(out[s.ProviderID],
					expectedPart{e.ID, e.ArchiveID, s, ChunkShardKey(e.ArchiveID, index, s.Part)})
			}
		}
	}
	return out
}

// ScanLocalParts lists every local-disk account and holds the listing
// against the index: a recorded part that is not in the listing, or is
// there but too short to be a part, is written down as lost (lostparts.go);
// one that an earlier look had found missing and the listing bears out is
// taken off the record. Answers with what it found. One at a time:
// ErrPartScanBusy while another is running.
//
// The index is read under the lock and the disks are listed without it, so
// a slow disk holds up nothing but the scan. A file deleted or rewritten
// meanwhile is recorded against an archive the index no longer names, and
// so reads as nothing.
func (v *Vault) ScanLocalParts(ctx context.Context) (*PartScanReport, error) {
	if !v.partScan.slot.TryLock() {
		return nil, ErrPartScanBusy
	}
	defer v.partScan.slot.Unlock()

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	accounts := v.localAccountsLocked()
	expected := v.expectedPartsLocked(accounts)
	v.mu.RUnlock()

	v.partScan.mu.Lock()
	v.partScan.running = true
	v.partScan.mu.Unlock()
	defer func() {
		v.partScan.mu.Lock()
		v.partScan.running = false
		v.partScan.mu.Unlock()
	}()

	started := time.Now()
	report := &PartScanReport{RanAt: started.UTC(), Accounts: []PartScanAccount{}}
	touched := map[string]bool{}
	for _, cfg := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := PartScanAccount{ID: cfg.ID, Name: cfg.Name, Kind: cfg.Kind, Parts: len(expected[cfg.ID])}
		report.Parts += row.Parts

		sizes, err := v.listSizes(ctx, cfg)
		if err != nil {
			// No listing is no answer: nothing is recorded against the
			// disk, and the row says why.
			row.Error = err.Error()
			report.Unlisted++
			report.Accounts = append(report.Accounts, row)
			continue
		}
		for _, p := range expected[cfg.ID] {
			size, there := sizes[p.key]
			var reason string
			switch {
			case !there:
				reason = "not found on the disk — the part was deleted, or the folder is on a different disk than it was"
			case partTooShort(size):
				reason = truncatedReason(size)
			}
			if reason == "" {
				if v.forgetLostPartOn(p.entryID, p.shard.Part, cfg.ID) {
					row.Recovered++
				}
				continue
			}
			row.Lost++
			touched[p.entryID] = true
			v.noteLostPart(p.entryID, p.archiveID, p.shard, reason)
		}
		report.Lost += row.Lost
		report.Recovered += row.Recovered
		report.Accounts = append(report.Accounts, row)
	}
	report.Files = len(touched)
	report.TookMs = time.Since(started).Milliseconds()

	v.partScan.mu.Lock()
	v.partScan.ranAt = started
	v.partScan.last = report
	v.partScan.mu.Unlock()
	return report, nil
}

// listSizes is one account's listing as a map from key to size.
func (v *Vault) listSizes(ctx context.Context, cfg provider.Config) (map[string]int64, error) {
	p, err := v.buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	objects, err := p.List(ctx, "")
	if err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(objects))
	for _, o := range objects {
		sizes[o.Key] = o.Size
	}
	return sizes, nil
}

// forgetLostPartOn drops the record of one part being lost, if it was
// recorded against this account, and reports whether it was.
func (v *Vault) forgetLostPartOn(id string, part int, providerID string) bool {
	v.lostMu.Lock()
	defer v.lostMu.Unlock()
	byPart := v.lost[id]
	lost, ok := byPart[part]
	if !ok || lost.ProviderID != providerID {
		return false
	}
	delete(byPart, part)
	if len(byPart) == 0 {
		delete(v.lost, id)
	}
	return true
}
