package vault

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Old versions of SAND's objects on the buckets that keep them, and erasing
// them.
//
// orphans.go asks the accounts what they hold that no file wants. This asks a
// narrower question of a particular kind of account: what is a bucket storing
// *underneath* what it shows? An object store with versioning switched on —
// which Backblaze B2 is by default, under the name "keep all versions" — never
// overwrites and never deletes. A Put adds a version beneath the key and a
// Delete adds a delete marker on top, and everything underneath goes on being
// stored and billed until something erases it version by version. A plain
// listing shows none of it: it answers with the latest live version of each
// key, which is what Get would return, and that is all the rest of SAND ever
// asks for. So the usage bar says a gigabyte and the bill says ten, and both
// are telling the truth.
//
// SAND makes two kinds of it, and both are safe to lose. The index backup
// (manifest.sand) is rewritten on every change to the index — every upload,
// every rename, every move — so a bucket that keeps versions holds one copy of
// the index per change ever made, and only the newest is the index. And every
// part SAND has ever deleted is still there under a marker: the delete reached
// the bucket, the bucket wrote it down and kept the bytes. Neither is anything
// this vault would ever read back. A recovery opens the latest backup; a file
// is fetched from the latest version of its parts; nothing anywhere asks for a
// version by ID.
//
// **What is never touched.** The latest live version of every key — the object
// a listing shows — is not a version, it is the object, and this leaves it
// exactly where it is whether or not the index points at it: a part nothing
// wants is the orphan sweep's business, with its guards. Objects SAND did not
// write are not looked at, by the same strict rule partKeyPattern imposes on the
// orphan scan: only manifest.sand and the two part-key shapes are SAND's, and
// anything else in the bucket keeps every version it has, reported by count so
// that somebody sharing a bucket with something else can see where the rest of
// the room went. And one shape of stale version is held back even under
// SAND's own keys: a part the index still points at whose latest version is a
// delete marker. The index says the part exists and the bucket says it was
// deleted, which means something other than SAND deleted it — a console, a
// lifecycle rule — and the versions under the marker are the only copies left.
// Erasing them would turn a file that can be repaired into one that cannot.
//
// Nothing here acts on its own initiative. The scan writes nothing; the sweep
// is a separate call that scans again first, so what it erases is what is
// there now rather than what was there when somebody was shown a figure.

// versionKeyPreview caps how many per-key rows a scan hands back. The totals
// count every one; this only bounds the detail.
const versionKeyPreview = 200

// versionEraseWindow is how many versions are erased from one account at once.
// The same small number the orphan sweep uses, for the same reason: tidying up
// has no business being the heaviest thing the vault ever does to an account.
const versionEraseWindow = 8

// VersionKey is one object key on one account with versions underneath its
// current one, which is the unit somebody is being asked about: the index
// backup's four hundred old copies are one decision, not four hundred.
type VersionKey struct {
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderKind string `json:"provider_kind"`

	Key string `json:"key"`

	// What says which of SAND's objects this is, in words the browser can
	// show: the index backup, a part of a file that is still stored, a part of
	// a file that was deleted.
	What string `json:"what"`

	// Versions and Markers are the stale entries under this key — superseded
	// data and delete markers — and Bytes what the data among them weighs. A
	// marker weighs nothing, but a bucket that has one on top of a key is
	// storing the versions under it, which is where the bytes are.
	Versions int   `json:"versions"`
	Markers  int   `json:"markers"`
	Bytes    int64 `json:"bytes"`

	// Deletable is false when erasing these would lose something, and Reason
	// says what. A row like that is reported and never swept.
	Deletable bool   `json:"deletable"`
	Reason    string `json:"reason,omitempty"`
}

// VersionAccount is one connected account's share of the answer.
type VersionAccount struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Versioned says the backend can be asked for versions at all. A folder
	// on a disk or a WebDAV share keeps nothing but the current file, and is
	// reported as such rather than as holding nothing stale.
	Versioned bool `json:"versioned"`

	// Current and CurrentBytes are the latest live versions of SAND's objects
	// — what a plain listing shows, and what the usage bar counts.
	Current      int   `json:"current"`
	CurrentBytes int64 `json:"current_bytes"`

	// Stale, StaleBytes and Markers are what the bucket is storing beneath
	// that under SAND's keys: superseded versions and delete markers, whether
	// or not they may be erased. Current plus Stale is what the bill sees.
	Stale      int   `json:"stale"`
	StaleBytes int64 `json:"stale_bytes"`
	Markers    int   `json:"markers"`

	// Deletable and DeletableBytes are the subset a sweep would erase.
	Deletable      int   `json:"deletable"`
	DeletableBytes int64 `json:"deletable_bytes"`

	// Other and OtherBytes are stale versions under keys SAND did not write:
	// something else's files in the same bucket, keeping their own history.
	// Counted so the room is accounted for; never touched.
	Other      int   `json:"other"`
	OtherBytes int64 `json:"other_bytes"`

	// Backup and Foreign are what they are on OrphanAccount: the account
	// carries an index backup, and this vault's key does not open it. A
	// foreign backup withholds the sweep on that account — another vault's
	// deleted parts are its own business, and a marker it did not write is
	// the case the hold above is for.
	Backup  bool `json:"backup"`
	Foreign bool `json:"foreign"`

	// AutoPrune says the account is pruned on a schedule, and LastPrune what
	// the last scheduled run did to it — see autoprune.go.
	AutoPrune bool         `json:"auto_prune,omitempty"`
	LastPrune *PruneRecord `json:"last_prune,omitempty"`

	// Error is why the account could not be asked.
	Error string `json:"error,omitempty"`
}

// VersionScan is what the versioned accounts are storing beneath what they
// show.
type VersionScan struct {
	// Found is the prompt condition: at least one stale version somewhere.
	Found bool `json:"found"`

	// Versioned is how many accounts could be asked at all.
	Versioned int `json:"versioned"`

	// Stale, StaleBytes and Markers count every stale version under SAND's
	// keys, deletable or not; Deletable and DeletableBytes the subset a sweep
	// would erase.
	Stale          int   `json:"stale"`
	StaleBytes     int64 `json:"stale_bytes"`
	Markers        int   `json:"markers"`
	Deletable      int   `json:"deletable"`
	DeletableBytes int64 `json:"deletable_bytes"`

	Accounts []VersionAccount `json:"accounts"`

	// Items are the per-key rows, heaviest first and capped; ItemsTruncated is
	// how many did not fit.
	Items          []VersionKey `json:"items"`
	ItemsTruncated int          `json:"items_truncated,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// doomedVersions is what a sweep erases from one account, in the order it has
// to: data first and markers last. A marker over an unwanted key is what keeps
// the key out of listings, so erasing it before the versions beneath it — and
// then being interrupted — would bring an object back to life as an orphan.
// The other way round, an interruption leaves a marker over nothing, which is
// exactly what a finished delete looks like.
type doomedVersions struct {
	data    []provider.ObjectVersion
	markers []provider.ObjectVersion
}

func (d doomedVersions) count() int { return len(d.data) + len(d.markers) }

// ScanForStaleVersions asks every account that keeps versions what it is
// storing beneath the objects it shows, and reports which of it SAND wrote and
// would never read back.
//
// One listing of versions per account plus one small download per account,
// the same cost as the orphan scan. Nothing is deleted, nothing is written and
// the vault is not modified.
func (v *Vault) ScanForStaleVersions(ctx context.Context) (*VersionScan, error) {
	scan, _, err := v.scanForStaleVersions(ctx)
	if err != nil {
		return nil, err
	}
	if len(scan.Items) > versionKeyPreview {
		scan.ItemsTruncated = len(scan.Items) - versionKeyPreview
		scan.Items = scan.Items[:versionKeyPreview]
	}
	return scan, nil
}

// scanForStaleVersions is ScanForStaleVersions with every row, and with the
// versions themselves keyed by account, which is what the sweep works from.
func (v *Vault) scanForStaleVersions(ctx context.Context) (*VersionScan, map[string]doomedVersions, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, nil, ErrLocked
	}
	owned := v.ownedArchivesLocked()
	configs := append([]provider.Config(nil), v.providers...)
	vaultKey := append([]byte(nil), v.vaultKey...)
	v.mu.RUnlock()
	defer crypto.ZeroBytes(vaultKey)

	owns := func(archiveID string) bool {
		_, ok := owned[archiveID]
		return ok
	}

	scan := &VersionScan{Accounts: []VersionAccount{}, Items: []VersionKey{}}
	doomed := map[string]doomedVersions{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()
			account, rows, d := v.scanAccountForStaleVersions(ctx, cfg, owns, vaultKey)

			mu.Lock()
			defer mu.Unlock()
			scan.Accounts = append(scan.Accounts, account)
			scan.Items = append(scan.Items, rows...)
			if d.count() > 0 {
				doomed[cfg.ID] = d
			}
			if account.Error != "" {
				scan.Warnings = append(scan.Warnings, fmt.Sprintf(
					"could not list the versions on %s, so nothing stale there is counted: %s",
					cfg.Name, account.Error))
			}
		}(cfg)
	}
	wg.Wait()

	for _, account := range scan.Accounts {
		if account.Versioned {
			scan.Versioned++
		}
		scan.Stale += account.Stale
		scan.StaleBytes += account.StaleBytes
		scan.Markers += account.Markers
		scan.Deletable += account.Deletable
		scan.DeletableBytes += account.DeletableBytes
	}
	scan.Found = scan.Stale > 0

	// Heaviest first: the question is how much room this gives back, so the
	// rows that answer it should not be the ones cut.
	sort.Slice(scan.Items, func(i, j int) bool {
		a, b := scan.Items[i], scan.Items[j]
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		if a.Versions+a.Markers != b.Versions+b.Markers {
			return a.Versions+a.Markers > b.Versions+b.Markers
		}
		if a.ProviderName != b.ProviderName {
			return a.ProviderName < b.ProviderName
		}
		return a.Key < b.Key
	})
	sort.Slice(scan.Accounts, func(i, j int) bool { return scan.Accounts[i].Name < scan.Accounts[j].Name })
	sort.Strings(scan.Warnings)
	return scan, doomed, nil
}

// scanAccountForStaleVersions is the scan's work for a single account.
func (v *Vault) scanAccountForStaleVersions(
	ctx context.Context,
	cfg provider.Config,
	owns func(archiveID string) bool,
	vaultKey []byte,
) (VersionAccount, []VersionKey, doomedVersions) {
	account := VersionAccount{
		ProviderID: cfg.ID,
		Name:       cfg.Name,
		Kind:       string(cfg.Kind),
		AutoPrune:  cfg.AutoPrune,
		LastPrune:  v.lastPrune(cfg.ID),
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		account.Error = err.Error()
		return account, nil, doomedVersions{}
	}
	versioner, ok := p.(provider.Versioner)
	if !ok {
		return account, nil, doomedVersions{}
	}
	account.Versioned = true

	versions, err := versioner.ListVersions(ctx, "")
	if err != nil {
		account.Error = err.Error()
		return account, nil, doomedVersions{}
	}
	account.Backup, account.Foreign = v.backupState(ctx, p, vaultKey)

	hold := ""
	if account.Foreign {
		hold = fmt.Sprintf("%s carries a vault index this one did not write, "+
			"so what was deleted there is that vault's business rather than this one's", cfg.Name)
	}
	rows, doomed := classifyVersions(&account, versions, owns, hold)
	return account, rows, doomed
}

// classifyVersions sorts one account's versions into current, stale and
// somebody else's, fills in the account's counts, and says which of the stale
// ones may go. It is the whole of the decision, kept free of I/O so it can be
// tested against every shape of bucket.
//
// hold, when set, is why nothing on this account may be erased; every row is
// still reported, with that reason attached.
func classifyVersions(
	account *VersionAccount,
	versions []provider.ObjectVersion,
	owns func(archiveID string) bool,
	hold string,
) ([]VersionKey, doomedVersions) {
	byKey := map[string][]provider.ObjectVersion{}
	var order []string
	for _, ver := range versions {
		if _, seen := byKey[ver.Key]; !seen {
			order = append(order, ver.Key)
		}
		byKey[ver.Key] = append(byKey[ver.Key], ver)
	}
	sort.Strings(order)

	var rows []VersionKey
	var doomed doomedVersions
	for _, key := range order {
		entries := byKey[key]

		archiveID, _, part := partOfKey(key)
		ours := key == BackupKey || part
		if !ours {
			// Something else's file, sharing the bucket. Its history is
			// counted so the room is accounted for, and left alone.
			for _, ver := range entries {
				if !ver.Latest || ver.DeleteMarker {
					account.Other++
					account.OtherBytes += ver.Size
				}
			}
			continue
		}

		var latest *provider.ObjectVersion
		for i := range entries {
			if entries[i].Latest {
				latest = &entries[i]
				break
			}
		}
		if latest != nil && !latest.DeleteMarker {
			account.Current++
			account.CurrentBytes += latest.Size
		}

		row := VersionKey{
			ProviderID:   account.ProviderID,
			ProviderName: account.Name,
			ProviderKind: account.Kind,
			Key:          key,
			Deletable:    true,
		}
		var data, markers []provider.ObjectVersion
		for _, ver := range entries {
			switch {
			case ver.DeleteMarker:
				markers = append(markers, ver)
				row.Markers++
			case !ver.Latest:
				data = append(data, ver)
				row.Versions++
				row.Bytes += ver.Size
			}
		}
		if row.Versions+row.Markers == 0 {
			continue
		}

		deleted := latest != nil && latest.DeleteMarker
		switch {
		case key == BackupKey:
			row.What = "the index backup"
		case latest == nil:
			row.What = "a part"
		case deleted && owns(archiveID):
			row.What = "a part of a stored file, deleted by something other than SAND"
		case deleted:
			row.What = "a part of a deleted file"
		case owns(archiveID):
			row.What = "a part of a stored file"
		default:
			row.What = "a part no file points at"
		}

		switch {
		case hold != "":
			row.Deletable, row.Reason = false, hold
		case latest == nil:
			// A backend that marked nothing as current. Erasing anything
			// under a key whose current version cannot be told is a guess,
			// and a guess is not what this is for.
			row.Deletable, row.Reason = false,
				fmt.Sprintf("%s did not say which version of it is current", account.Name)
		case deleted && part && owns(archiveID):
			row.Deletable, row.Reason = false,
				"the index still points at this part and its current version is a delete marker, "+
					"so the versions beneath it are the only copies left"
		}

		account.Stale += row.Versions + row.Markers
		account.StaleBytes += row.Bytes
		account.Markers += row.Markers
		if row.Deletable {
			account.Deletable += row.Versions + row.Markers
			account.DeletableBytes += row.Bytes
			doomed.data = append(doomed.data, data...)
			doomed.markers = append(doomed.markers, markers...)
		}
		rows = append(rows, row)
	}
	return rows, doomed
}

// VersionSweepReport says what a sweep did.
type VersionSweepReport struct {
	// Deleted is how many stale versions went, Markers how many of those were
	// delete markers, and Bytes what the data among them weighed.
	Deleted int   `json:"deleted"`
	Markers int   `json:"markers"`
	Bytes   int64 `json:"bytes"`

	// Accounts is the same, per account swept, keyed by account ID — what a
	// scheduled prune writes down for each account it was aimed at.
	Accounts map[string]SweepOutcome `json:"accounts,omitempty"`

	// Skipped names the accounts that were asked for and refused, and why.
	Skipped []string `json:"skipped,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// SweepOutcome is what a sweep did to one account.
type SweepOutcome struct {
	Deleted int   `json:"deleted"`
	Markers int   `json:"markers"`
	Bytes   int64 `json:"bytes"`

	// Error is the first thing that went wrong on this account, if anything
	// did; the rest are in the report's warnings.
	Error string `json:"error,omitempty"`
}

// SweepStaleVersions erases the stale versions a scan reported: every
// superseded version and delete marker under SAND's own keys, on the accounts
// named, leaving the current version of every object exactly as it is.
//
// accounts names which connected accounts to sweep, by ID. Empty means every
// one, which is what a command line asking to tidy up means.
//
// The scan is run again from scratch here rather than trusted from the caller,
// for the reason SweepOrphans does the same: it closes the window between
// being shown a figure and agreeing to it. A version that was stale a minute
// ago is still stale — nothing un-supersedes — but the guard on a part deleted
// by something other than SAND is a fact about the index, and the index can
// have changed.
//
// onProgress, when given, is told how many versions have been dealt with so
// far — once with (0, total) the moment the re-scan has decided what goes,
// then once per version, erased or failed alike. Calls arrive in order.
func (v *Vault) SweepStaleVersions(ctx context.Context, accounts []string, dryRun bool, onProgress func(done, total int)) (*VersionSweepReport, error) {
	scan, doomed, err := v.scanForStaleVersions(ctx)
	if err != nil {
		return nil, err
	}
	report := &VersionSweepReport{Warnings: scan.Warnings, Accounts: map[string]SweepOutcome{}}

	known := map[string]VersionAccount{}
	for _, account := range scan.Accounts {
		known[account.ProviderID] = account
	}
	wanted := map[string]doomedVersions{}
	if len(accounts) == 0 {
		wanted = doomed
	} else {
		for _, id := range accounts {
			account, ok := known[id]
			switch {
			case !ok:
				report.Skipped = append(report.Skipped, fmt.Sprintf("no connected account has the id %s", id))
			case !account.Versioned:
				report.Skipped = append(report.Skipped, fmt.Sprintf("%s does not keep old versions", account.Name))
			case account.Error != "":
				// Already a warning, from the scan.
			default:
				wanted[id] = doomed[id]
			}
		}
	}

	if dryRun {
		for id, d := range wanted {
			outcome := SweepOutcome{Deleted: d.count(), Markers: len(d.markers)}
			for _, ver := range d.data {
				outcome.Bytes += ver.Size
			}
			report.Accounts[id] = outcome
			report.Deleted += outcome.Deleted
			report.Markers += outcome.Markers
			report.Bytes += outcome.Bytes
		}
		sort.Strings(report.Skipped)
		return report, nil
	}

	v.mu.RLock()
	configs := make(map[string]provider.Config, len(v.providers))
	for _, cfg := range v.providers {
		configs[cfg.ID] = cfg
	}
	v.mu.RUnlock()

	// The denominator, announced before the first delete.
	var advance func()
	if onProgress != nil {
		total := 0
		for _, d := range wanted {
			total += d.count()
		}
		onProgress(0, total)

		var progressMu sync.Mutex
		done := 0
		advance = func() {
			progressMu.Lock()
			done++
			onProgress(done, total)
			progressMu.Unlock()
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, d := range wanted {
		cfg, ok := configs[id]
		if !ok {
			mu.Lock()
			report.Skipped = append(report.Skipped,
				fmt.Sprintf("the account holding %d stale version(s) was disconnected mid-sweep", d.count()))
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(cfg provider.Config, d doomedVersions) {
			defer wg.Done()
			deleted, markers, bytes, warnings := v.eraseVersions(ctx, cfg, d, advance)

			mu.Lock()
			defer mu.Unlock()
			outcome := SweepOutcome{Deleted: deleted, Markers: markers, Bytes: bytes}
			if len(warnings) > 0 {
				outcome.Error = warnings[0]
			}
			report.Accounts[cfg.ID] = outcome
			report.Deleted += deleted
			report.Markers += markers
			report.Bytes += bytes
			report.Warnings = append(report.Warnings, warnings...)
		}(cfg, d)
	}
	wg.Wait()

	sort.Strings(report.Skipped)
	sort.Strings(report.Warnings)
	return report, nil
}

// eraseVersions erases one account's doomed versions, data first and markers
// last (see doomedVersions), a few at a time.
func (v *Vault) eraseVersions(
	ctx context.Context,
	cfg provider.Config,
	doomed doomedVersions,
	advance func(),
) (deleted, markers int, bytes int64, warnings []string) {
	p, err := v.buildProvider(cfg)
	if err != nil {
		return 0, 0, 0, []string{fmt.Sprintf("could not reach %s: %v", cfg.Name, err)}
	}
	versioner, ok := p.(provider.Versioner)
	if !ok {
		return 0, 0, 0, []string{fmt.Sprintf("%s stopped keeping versions mid-sweep", cfg.Name)}
	}

	var mu sync.Mutex
	erase := func(batch []provider.ObjectVersion) {
		var wg sync.WaitGroup
		window := make(chan struct{}, versionEraseWindow)
		for _, ver := range batch {
			wg.Add(1)
			window <- struct{}{}
			go func(ver provider.ObjectVersion) {
				defer wg.Done()
				defer func() { <-window }()

				err := versioner.DeleteVersion(ctx, ver.Key, ver.VersionID)

				mu.Lock()
				defer mu.Unlock()
				if advance != nil {
					advance()
				}
				if err != nil {
					warnings = append(warnings,
						fmt.Sprintf("could not erase version %s of %s from %s: %v", ver.VersionID, ver.Key, cfg.Name, err))
					return
				}
				deleted++
				if ver.DeleteMarker {
					markers++
				}
				bytes += ver.Size
			}(ver)
		}
		wg.Wait()
	}

	erase(doomed.data)
	erase(doomed.markers)
	return deleted, markers, bytes, warnings
}
