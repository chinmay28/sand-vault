package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Parts on an account that the index has stopped pointing at.
//
// Every other operation in this package works outwards: the index says what
// exists and the accounts are told. This one works inwards. It asks each
// account what it is actually holding and subtracts what the index accounts
// for, and whatever is left over is storage being paid for by nobody.
//
// The way that gap opens is almost always the same, and it is not a bug in
// anything here. Erasing a file erases its parts from the accounts holding
// them — but only the ones that are connected at the time. Disconnect a cloud,
// spend a month deleting files, and none of the parts on it go anywhere. Nor
// can they later: reconnecting an account gives it a fresh ID (AddProvider
// mints one per connection, because a credential is all that is ever handed
// back and it says nothing about which account it was last time), so the vault
// has no way of knowing the returning account is the one those parts were
// erased from. Reconcile re-points the records for parts that are still in use,
// which is what makes the files readable again. It has nothing to say about
// parts no record names, and by then nothing ever will.
//
// So they sit there. Encrypted under a key the vault may not even hold any
// more, unreadable by their owner and by everybody else, and counting against a
// quota. The point of this file is to find them and to say how much room they
// are taking, so that deleting them can be somebody's decision rather than
// nobody's.
//
// Nothing here deletes on its own initiative. Detection is safe to run
// unattended; the sweep is a separate call with its own re-verification, and it
// refuses in exactly the cases where "the index does not mention it" is not the
// same thing as "it is not needed". See orphanGuard.

// partKeyPattern matches the object keys SAND writes for parts, in both shapes:
// <archive>-p<n>.sand for a file stored whole, and <archive>-c<index>-p<n>.sand
// for one chunk of a chunked one (see ShardKey and ChunkShardKey).
//
// Strict on purpose. A key that does not match is not treated as SAND's at all,
// which costs an orphan going unreported and buys the guarantee that this never
// proposes deleting something it did not write.
var partKeyPattern = regexp.MustCompile(`^([0-9a-f]{8,64})(?:-c[0-9]{1,12})?-p[0-9]{1,4}\.sand$`)

// archiveOfKey returns the archive an object key belongs to, and whether the
// key is one of ours at all.
//
// Ownership is decided by archive rather than by exact key, and that is the
// whole trick. A chunked file occupies one object per chunk per part — a 4 GB
// film is 768 of them — and the index records only chunk zero's key per shard.
// Matching by archive means the other 767 are recognised without the index
// having to spell them out, and it means a stray chunk left by an interrupted
// upload of a file that is still stored is never mistaken for garbage.
func archiveOfKey(key string) (string, bool) {
	match := partKeyPattern.FindStringSubmatch(key)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// orphanArchivePreview caps how many per-archive rows a scan hands back. The
// totals always count every one; this only bounds the detail, so an account
// carrying ten thousand abandoned archives answers with a summary rather than a
// megabyte of JSON.
const orphanArchivePreview = 200

// OrphanArchive is one archive found on one account that no index names.
//
// Grouped by archive rather than listed object by object because that is the
// unit somebody is being asked about: a film's 768 leftover objects are one
// thing that was deleted, not 768 decisions. The keys are not carried — they
// are derivable from the archive ID, and the sweep re-lists anyway so that it
// erases what is there now rather than what was there when the scan ran.
type OrphanArchive struct {
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderKind string `json:"provider_kind"`

	// ArchiveID is the random ID the objects are named after. It is the only
	// thing that can be said about them: what the archive was, when it was
	// written and whether it was a file or a folder's thumbnails are all facts
	// that lived in the index that stopped mentioning it.
	ArchiveID string `json:"archive_id"`

	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// Deletable is false when something about this account makes "unaccounted
	// for" an unsafe reading, and Reason says which thing. A row like that is
	// reported and never swept.
	Deletable bool   `json:"deletable"`
	Reason    string `json:"reason,omitempty"`
}

// OrphanAccount is one connected account's share of the answer.
type OrphanAccount struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Objects and Bytes are every part object the account is holding, whether
	// or not the index accounts for it — the denominator the orphan figures
	// are worth reading against.
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// Orphans, OrphanBytes and Archives are what nothing points at.
	Orphans     int   `json:"orphans"`
	OrphanBytes int64 `json:"orphan_bytes"`
	Archives    int   `json:"archives"`

	// Backup says the account carries a manifest.sand, and Foreign that this
	// vault's key does not open it — which means another vault has been
	// writing here, and its parts would look exactly like orphans without
	// being any such thing. A backup this vault *can* open is the opposite
	// signal: proof that this vault owns the account, which is what makes an
	// emptied vault distinguishable from a vault waiting to be recovered. Both
	// are read by orphanGuard.
	Backup  bool `json:"backup"`
	Foreign bool `json:"foreign"`

	// Error is why the account could not be asked. An account that would not
	// answer contributes nothing at all rather than an empty listing, because
	// an empty listing would make everything it holds look abandoned.
	Error string `json:"error,omitempty"`
}

// OrphanScan is what the accounts are holding that the vault has forgotten.
type OrphanScan struct {
	// Found is the prompt condition: at least one archive nothing points at.
	Found bool `json:"found"`

	// Objects, Bytes and Archives count every orphan, deletable or not.
	Objects  int   `json:"objects"`
	Bytes    int64 `json:"bytes"`
	Archives int   `json:"archives"`

	// Deletable and DeletableBytes are the subset a sweep would actually erase.
	Deletable      int   `json:"deletable"`
	DeletableBytes int64 `json:"deletable_bytes"`

	Accounts []OrphanAccount `json:"accounts"`

	// Items are the per-archive rows, largest first and capped;
	// ItemsTruncated is how many did not fit.
	Items          []OrphanArchive `json:"items"`
	ItemsTruncated int             `json:"items_truncated,omitempty"`

	// Blocked says why a sweep is being withheld across the board, and is what
	// the browser shows instead of a delete button. Empty means the sweep is
	// offered.
	Blocked []string `json:"blocked,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// ScanForOrphans asks every connected account what it holds and reports the
// part objects no index accounts for.
//
// One listing per account and one small download per account, which is what
// ScanForRecovery already costs — the browser runs it when the set of connected
// accounts changes, which is when the gap this finds actually opens.
//
// Nothing is deleted, nothing is written, and the vault is not modified.
func (v *Vault) ScanForOrphans(ctx context.Context) (*OrphanScan, error) {
	scan, err := v.scanForOrphans(ctx)
	if err != nil {
		return nil, err
	}
	scan.trimPreview()
	return scan, nil
}

// trimPreview cuts the per-archive rows down to what is worth sending, leaving
// every total counting what was actually found. The sweep works from the
// untrimmed answer, which is why this is a separate step rather than something
// the scan does on its way out.
func (s *OrphanScan) trimPreview() {
	if len(s.Items) <= orphanArchivePreview {
		return
	}
	s.ItemsTruncated = len(s.Items) - orphanArchivePreview
	s.Items = s.Items[:orphanArchivePreview]
}

// scanForOrphans is ScanForOrphans with every row it found, however many that
// is.
func (v *Vault) scanForOrphans(ctx context.Context) (*OrphanScan, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	owned := v.ownedArchivesLocked()
	configs := append([]provider.Config(nil), v.providers...)
	vaultKey := append([]byte(nil), v.vaultKey...)
	v.mu.RUnlock()
	defer crypto.ZeroBytes(vaultKey)

	scan := &OrphanScan{Accounts: []OrphanAccount{}, Items: []OrphanArchive{}}
	if len(configs) == 0 {
		return scan, nil
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var found []OrphanArchive

	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()
			account, archives := v.scanAccountForOrphans(ctx, cfg, owned, vaultKey)

			mu.Lock()
			defer mu.Unlock()
			scan.Accounts = append(scan.Accounts, account)
			found = append(found, archives...)
			if account.Error != "" {
				scan.Warnings = append(scan.Warnings, fmt.Sprintf(
					"could not ask %s what it holds, so anything abandoned there is not counted: %s",
					cfg.Name, account.Error))
			}
		}(cfg)
	}
	wg.Wait()

	// Applied after the listings rather than during them, because one of the
	// reasons is a fact about the whole vault rather than about one account.
	blocked := orphanGuard(owned, scan.Accounts)
	scan.Blocked = blocked
	for i := range found {
		if len(blocked) > 0 {
			found[i].Deletable = false
			if found[i].Reason == "" {
				found[i].Reason = blocked[0]
			}
		}
		scan.Objects += found[i].Objects
		scan.Bytes += found[i].Bytes
		scan.Archives++
		if found[i].Deletable {
			scan.Deletable += found[i].Objects
			scan.DeletableBytes += found[i].Bytes
		}
	}
	scan.Found = scan.Archives > 0

	// Heaviest first: the question somebody is answering is how much room this
	// would give back, so the rows that answer it should not be the ones cut.
	sort.Slice(found, func(i, j int) bool {
		if found[i].Bytes != found[j].Bytes {
			return found[i].Bytes > found[j].Bytes
		}
		if found[i].ProviderName != found[j].ProviderName {
			return found[i].ProviderName < found[j].ProviderName
		}
		return found[i].ArchiveID < found[j].ArchiveID
	})
	scan.Items = found

	sort.Slice(scan.Accounts, func(i, j int) bool { return scan.Accounts[i].Name < scan.Accounts[j].Name })
	sort.Strings(scan.Warnings)
	return scan, nil
}

// scanAccountForOrphans is ScanForOrphans' work for a single account.
func (v *Vault) scanAccountForOrphans(
	ctx context.Context,
	cfg provider.Config,
	owned map[string]struct{},
	vaultKey []byte,
) (OrphanAccount, []OrphanArchive) {
	account := OrphanAccount{
		ProviderID: cfg.ID,
		Name:       cfg.Name,
		Kind:       string(cfg.Kind),
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		account.Error = err.Error()
		return account, nil
	}
	objects, err := p.List(ctx, "")
	if err != nil {
		// No listing, no answer. Reporting nothing here is the conservative
		// outcome: the alternative is an account whose every part reads as
		// abandoned because it never got to say otherwise.
		account.Error = err.Error()
		return account, nil
	}

	account.Backup, account.Foreign = v.backupState(ctx, p, vaultKey)

	reason := ""
	if account.Foreign {
		reason = fmt.Sprintf("%s carries a vault index this one did not write, "+
			"so parts here may belong to that vault rather than to nobody", cfg.Name)
	}

	sizes := map[string]int64{}
	counts := map[string]int{}
	for _, obj := range objects {
		if obj.Key == BackupKey {
			continue
		}
		id, ours := archiveOfKey(obj.Key)
		if !ours {
			// Something else's file, sharing the folder or the bucket. Not
			// counted, not reported and certainly not deleted.
			continue
		}
		account.Objects++
		account.Bytes += obj.Size
		if _, known := owned[id]; known {
			continue
		}
		account.Orphans++
		account.OrphanBytes += obj.Size
		counts[id]++
		sizes[id] += obj.Size
	}

	account.Archives = len(counts)
	archives := make([]OrphanArchive, 0, len(counts))
	for id, count := range counts {
		archives = append(archives, OrphanArchive{
			ProviderID:   cfg.ID,
			ProviderName: cfg.Name,
			ProviderKind: string(cfg.Kind),
			ArchiveID:    id,
			Objects:      count,
			Bytes:        sizes[id],
			Deletable:    reason == "",
			Reason:       reason,
		})
	}
	return account, archives
}

// backupState reports whether an account carries a manifest backup, and whether
// it is one this vault's key cannot open — the mark of a second vault having
// written here. It is the same test guardForeignBackup and the recovery scan
// apply.
//
// An account that will not say is treated as carrying a foreign one, which is
// the answer that withholds the delete button rather than the one that offers
// it.
func (v *Vault) backupState(ctx context.Context, p provider.Provider, vaultKey []byte) (backup, foreign bool) {
	blob, err := p.Get(ctx, BackupKey)
	if err != nil {
		// Not-found is the ordinary case for an account backup is switched off
		// for, and is not suspicious. Any other failure is.
		return false, !errors.Is(err, provider.ErrNotFound)
	}
	var b Backup
	if json.Unmarshal(blob, &b) != nil || b.Magic != backupMagic {
		// Something under that name that is not one of ours. It says nothing
		// about another vault, so it is not held against the account.
		return false, false
	}
	return true, !b.opensWith(vaultKey)
}

// orphanGuard lists the reasons a sweep must not be offered at all.
//
// Each of them is a state in which "no index mentions these parts" stops
// meaning "these parts are abandoned", and every one of them is a state a
// perfectly ordinary user can be in.
func orphanGuard(owned map[string]struct{}, accounts []OrphanAccount) []string {
	var blocked []string

	// A vault that holds nothing, with parts sitting on its accounts, is
	// ambiguous in a way no other state is: it is either somebody who has
	// deleted their last file, or a reinstalled machine that has not yet been
	// told the password of the vault that died. Sweeping the second would erase
	// precisely what the recovery needs.
	//
	// What tells them apart is whose index is on the accounts. This vault's own
	// backup sitting there is proof it has been writing to them, whatever it
	// happens to hold today; a backup it cannot open is proof somebody else
	// has. So an emptied vault may tidy up after itself, and a fresh one on
	// borrowed accounts may not.
	if len(owned) == 0 {
		holding, ours, foreign := false, false, false
		for _, a := range accounts {
			holding = holding || a.Orphans > 0
			ours = ours || (a.Backup && !a.Foreign)
			foreign = foreign || a.Foreign
		}
		if holding && (foreign || !ours) {
			blocked = append(blocked,
				"this vault holds no files of its own and none of these accounts carries an index "+
					"it wrote, so the parts on them are far more likely to belong to a vault "+
					"waiting to be recovered than to nobody")
		}
	}

	// An account that would not answer leaves a hole in the arithmetic, and
	// the arithmetic is the only evidence there is.
	var silent []string
	for _, a := range accounts {
		if a.Error != "" {
			silent = append(silent, a.Name)
		}
	}
	if len(silent) > 0 {
		sort.Strings(silent)
		blocked = append(blocked, fmt.Sprintf(
			"%s could not be listed, and a part is only abandoned if every account agrees it is — "+
				"reconnect and scan again", strings.Join(silent, ", ")))
	}

	return blocked
}

// ownedArchivesLocked collects every archive ID this vault still points at,
// across the main index, every sub vault that is open, and every sub vault that
// is not.
//
// The last of those is what makes this safe to act on. A locked sub vault's
// index is unreadable — that is the point of it — but the main vault keeps an
// inventory of the archives it owns for exactly this kind of question, refreshed
// from its index on every write it makes. Without it, opening the browser
// without typing a sub vault's password would make every file inside it look
// like garbage.
//
// The caller must hold at least the read lock.
func (v *Vault) ownedArchivesLocked() map[string]struct{} {
	owned := map[string]struct{}{}

	add := func(id string) {
		if id != "" {
			owned[id] = struct{}{}
		}
	}
	fromManifest := func(m *Manifest) {
		if m == nil {
			return
		}
		for _, item := range m.inventory() {
			add(item.ArchiveID)
		}
		// The inventory derives an archive ID per row; a row whose shards
		// carry a key it could not derive one from still names that key, and
		// its archive is read straight off it.
		for _, e := range m.Entries {
			add(e.ArchiveID)
			for _, s := range e.Shards {
				if id, ours := archiveOfKey(s.Key); ours {
					add(id)
				}
			}
		}
		for _, pack := range m.Thumbs {
			if pack == nil {
				continue
			}
			for _, s := range pack.Shards {
				if id, ours := archiveOfKey(s.Key); ours {
					add(id)
				}
			}
		}
	}

	fromManifest(v.manifest)
	for _, sub := range v.subs {
		fromManifest(sub.manifest)
	}
	for _, meta := range v.manifest.SubVaults {
		if meta == nil {
			continue
		}
		for _, item := range meta.Inventory {
			add(item.ArchiveID)
		}
	}
	return owned
}

// OrphanTarget names one archive on one account to erase.
type OrphanTarget struct {
	ProviderID string `json:"provider_id"`
	ArchiveID  string `json:"archive_id"`
}

// OrphanSweepReport says what a sweep did.
type OrphanSweepReport struct {
	// Archives, Deleted and Bytes are what went.
	Archives int   `json:"archives"`
	Deleted  int   `json:"deleted"`
	Bytes    int64 `json:"bytes"`

	// Skipped names the targets that were refused, and why. A target the vault
	// has started pointing at again since the scan ran is the case this exists
	// for.
	Skipped []string `json:"skipped,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// SweepOrphans erases the part objects a scan reported as abandoned.
//
// targets names archive-and-account pairs, which is what the browser sends back
// from the rows somebody ticked. Empty means everything the vault currently
// considers deletable, which is what a command line asking to tidy up means.
//
// The scan is run again from scratch here rather than trusted from the caller.
// It costs a second listing and it closes the window between being shown a
// figure and agreeing to it: a file uploaded in between, a sub vault opened, an
// account reconnected — each of those changes the answer, and each of them
// would otherwise be a deletion of something live.
func (v *Vault) SweepOrphans(ctx context.Context, targets []OrphanTarget, dryRun bool) (*OrphanSweepReport, error) {
	// The untrimmed scan: the preview cap bounds what a client is shown, and
	// sweeping only what fitted on the screen would leave the rest behind
	// silently.
	scan, err := v.scanForOrphans(ctx)
	if err != nil {
		return nil, err
	}
	report := &OrphanSweepReport{Warnings: scan.Warnings}
	if len(scan.Blocked) > 0 {
		return nil, fmt.Errorf("%s", scan.Blocked[0])
	}

	// Everything the fresh scan is willing to erase, keyed the way a target
	// names it.
	deletable := map[OrphanTarget]OrphanArchive{}
	for _, item := range scan.Items {
		if item.Deletable {
			deletable[OrphanTarget{ProviderID: item.ProviderID, ArchiveID: item.ArchiveID}] = item
		}
	}

	wanted := make([]OrphanArchive, 0, len(deletable))
	if len(targets) == 0 {
		for _, item := range deletable {
			wanted = append(wanted, item)
		}
	} else {
		for _, t := range targets {
			item, ok := deletable[t]
			if !ok {
				report.Skipped = append(report.Skipped, fmt.Sprintf(
					"%s is no longer an abandoned archive on that account, so it was left alone", t.ArchiveID))
				continue
			}
			wanted = append(wanted, item)
		}
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].ArchiveID < wanted[j].ArchiveID })

	if dryRun {
		for _, item := range wanted {
			report.Archives++
			report.Deleted += item.Objects
			report.Bytes += item.Bytes
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

	byAccount := map[string][]OrphanArchive{}
	for _, item := range wanted {
		byAccount[item.ProviderID] = append(byAccount[item.ProviderID], item)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, items := range byAccount {
		cfg, ok := configs[id]
		if !ok {
			mu.Lock()
			report.Skipped = append(report.Skipped,
				fmt.Sprintf("the account holding %d archive(s) was disconnected mid-sweep", len(items)))
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(cfg provider.Config, items []OrphanArchive) {
			defer wg.Done()
			erased, objects, bytes, warnings := v.eraseOrphans(ctx, cfg, items)

			mu.Lock()
			defer mu.Unlock()
			report.Archives += erased
			report.Deleted += objects
			report.Bytes += bytes
			report.Warnings = append(report.Warnings, warnings...)
		}(cfg, items)
	}
	wg.Wait()

	sort.Strings(report.Skipped)
	sort.Strings(report.Warnings)
	return report, nil
}

// orphanEraseWindow is how many objects are deleted from one account at once.
// Small, because the accounts on the other end are consumer cloud APIs and
// tidying up has no business being the heaviest thing the vault ever does to
// one.
const orphanEraseWindow = 8

// eraseOrphans deletes every object belonging to the named archives from one
// account, listing it once more so that what is erased is what is there.
func (v *Vault) eraseOrphans(
	ctx context.Context,
	cfg provider.Config,
	items []OrphanArchive,
) (archives, objects int, bytes int64, warnings []string) {
	p, err := v.buildProvider(cfg)
	if err != nil {
		return 0, 0, 0, []string{fmt.Sprintf("could not reach %s: %v", cfg.Name, err)}
	}
	listing, err := p.List(ctx, "")
	if err != nil {
		return 0, 0, 0, []string{fmt.Sprintf("could not list %s: %v", cfg.Name, err)}
	}

	doomed := map[string]struct{}{}
	for _, item := range items {
		doomed[item.ArchiveID] = struct{}{}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	window := make(chan struct{}, orphanEraseWindow)
	hit := map[string]bool{}

	for _, obj := range listing {
		if obj.Key == BackupKey {
			continue
		}
		id, ours := archiveOfKey(obj.Key)
		if !ours {
			continue
		}
		if _, wanted := doomed[id]; !wanted {
			continue
		}

		wg.Add(1)
		window <- struct{}{}
		go func(obj provider.ObjectInfo, id string) {
			defer wg.Done()
			defer func() { <-window }()

			err := p.Delete(ctx, obj.Key)

			mu.Lock()
			defer mu.Unlock()
			if err != nil && !errors.Is(err, provider.ErrNotFound) {
				warnings = append(warnings,
					fmt.Sprintf("could not erase %s from %s: %v", obj.Key, cfg.Name, err))
				return
			}
			objects++
			bytes += obj.Size
			hit[id] = true
		}(obj, id)
	}
	wg.Wait()

	return len(hit), objects, bytes, warnings
}
