package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/provider"
)

// Parts the index says an account is holding, and the account is not.
//
// This is the stray-parts scan (orphans.go) run the other way. That one asks
// each account what it holds and subtracts what the index accounts for; this
// one takes what the index accounts for and subtracts what each account
// holds. The remainder is parts that were written, recorded, and have since
// gone from under SAND: a folder emptied by hand, a bucket restored from last
// month's backup, a sync client that decided the files were its to tidy, a
// provider that lost an object.
//
// Nothing else notices. The index is the only record of what should be there
// and it has no way to know, so the file goes on showing a full set of badges.
// It reads back perfectly too — on its spares, which is what they are for —
// right up until a second account is unavailable. A file's own health check
// would say so, one file at a time, to somebody who thought to ask; a folder's
// automation would, on its schedule, for the folders that have one. This is
// the same question asked of the whole vault at the price of one listing per
// account, and it downloads nothing.
//
// Putting a lost part back is what the second half of this file does, and it
// is neither of the repairs the vault already had. Those rebuild the file: read
// it, cut it again, write a full fresh set under a new archive, erase the old.
// That is the right answer when the place a part was on is gone, and a great
// deal more than is needed when the place is still there and merely empty. A
// code that rebuilds a file from any k of its shards rebuilds any one shard
// the same way (archive.RemakeShard), so the lost object alone is cut again
// from k of the survivors and written back under the name it had, on the
// account the index already says it is on:
//
//   - k objects come down and one goes up, per lost object, instead of the
//     whole file down and all n up;
//   - the index is not written at all — it was right all along about where
//     the part belongs, and now it is right about where the part is;
//   - nothing is erased and nothing is overwritten, so it is additive in the
//     way a reattach is: a file can only come out of it with more of its
//     objects on the clouds than it went in with.
//
// The one thing a listing cannot tell apart is an account that has been
// emptied and an account that is pointed at the wrong place — a folder that
// was moved, a drive that is not mounted. Both list as holding none of what
// the index records there. That is why nothing here runs on its own: the scan
// is safe unattended and says what it found, and the restore is a separate
// call somebody makes having read it (see LostAccount.Emptied).
//
// What this does not cover, on purpose: a file stored in the format from
// before chunking (it has no per-chunk shards to remake — converting it writes
// it out whole), a folder's thumbnail pack (drawn again rather than mended),
// a replica (made again by its own worker) and a locked sub vault's files
// (there is no key to cut a shard with until it is opened).

// lostPartPreview caps how many rows a scan hands back. The totals count
// every one.
const lostPartPreview = 200

// LostPart is one shard of one file that the index records on an account
// whose listing does not show all of it.
type LostPart struct {
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderKind string `json:"provider_kind"`

	// Scope is which vault inside the vault the file belongs to, and Path is
	// carried only for the main one — the rule StrayShard keeps.
	Scope  Scope  `json:"scope,omitempty"`
	FileID string `json:"file_id"`
	Path   string `json:"path,omitempty"`

	ArchiveID string `json:"archive_id"`
	Part      int    `json:"part"`

	// Objects is how many of this shard's stored objects are gone, out of Of:
	// one for a file stored whole, one per chunk otherwise. Bytes is about
	// what the missing ones weighed.
	Objects int   `json:"objects"`
	Of      int   `json:"of"`
	Bytes   int64 `json:"bytes"`

	// Restorable says the missing objects can be cut again from the file's
	// other parts as the listings show them, and Reason why not when they
	// cannot.
	Restorable bool   `json:"restorable"`
	Reason     string `json:"reason,omitempty"`

	// missing is which objects are gone: chunk indexes for a chunked file.
	missing []int
}

// LostAccount is one connected account's share of the answer.
type LostAccount struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Recorded is how many objects the index says this account holds for the
	// files it could be asked about, and Lost how many of those its listing
	// did not show. Parts and Files group the lost ones; Restorable is how
	// many of the parts can be put back.
	Recorded   int   `json:"recorded"`
	Lost       int   `json:"lost"`
	LostBytes  int64 `json:"lost_bytes"`
	Parts      int   `json:"parts"`
	Files      int   `json:"files"`
	Restorable int   `json:"restorable"`

	// Emptied says the account lists none of what the index records on it.
	// That is what an emptied folder looks like, and it is also exactly what
	// an account pointed at the wrong folder, or a drive that is not mounted,
	// looks like — which is the thing to rule out before anything is written
	// back to it.
	Emptied bool `json:"emptied,omitempty"`

	// Error is why the account was not asked. An account that would not
	// answer contributes nothing rather than an empty listing: an empty
	// listing would make everything on it look lost.
	Error string `json:"error,omitempty"`
}

// LostSummary is the scan in figures, small enough to ride along on another
// answer — the stray-parts scan carries one, since it is the same listing.
type LostSummary struct {
	// Parts is how many shards have objects missing, across Files files;
	// Objects and Bytes are the objects themselves. Restorable is how many of
	// the parts can be put back.
	Parts      int   `json:"parts"`
	Files      int   `json:"files"`
	Objects    int   `json:"objects"`
	Bytes      int64 `json:"bytes"`
	Restorable int   `json:"restorable"`

	// On names the accounts the parts are missing from.
	On []string `json:"on,omitempty"`
}

// LostPartsScan is what the index records that the accounts do not hold.
type LostPartsScan struct {
	// Found is the prompt condition: at least one part is not where the index
	// says it is.
	Found bool `json:"found"`
	LostSummary

	// Items are the rows, by account and then by path, capped; ItemsTruncated
	// is how many did not fit.
	Items          []LostPart `json:"items"`
	ItemsTruncated int        `json:"items_truncated,omitempty"`

	Accounts []LostAccount `json:"accounts"`
	Warnings []string      `json:"warnings,omitempty"`

	// all is every row, for the restore, which works from the untrimmed scan.
	all []LostPart
}

// LostScanOptions is what a caller can ask of a scan beyond the answer.
type LostScanOptions struct {
	// OnProgress is called as the scan moves, exactly as the stray-parts
	// scan's is and in the same units: a stage, then a root per account.
	OnProgress func(OrphanScanProgress)
}

// ScanForLostParts lists every connected account and reports the parts the
// index records on it that the listing does not show.
//
// One listing per account. Nothing is downloaded, nothing is written and the
// vault is not modified. An account that will not answer, and a disk that
// lives on another machine, is named and not judged.
func (v *Vault) ScanForLostParts(ctx context.Context, opts LostScanOptions) (*LostPartsScan, error) {
	scan, err := v.scanForLostParts(ctx, opts.OnProgress)
	if err != nil {
		return nil, err
	}
	scan.trimPreview()
	return scan, nil
}

func (s *LostPartsScan) trimPreview() {
	if len(s.Items) > lostPartPreview {
		s.ItemsTruncated = len(s.Items) - lostPartPreview
		s.Items = s.Items[:lostPartPreview]
	}
}

// lostFile is one file as the index had it when the scan began: a copy, with
// the vault it is in.
type lostFile struct {
	scope Scope
	entry *Entry
}

// lostFilesLocked copies every file the open vaults record, with its
// placement. The caller must hold at least the read lock.
//
// Copied before the accounts are listed and never after: a file stored while
// the listings are in flight would otherwise be judged against a listing
// taken before its parts were written.
func (v *Vault) lostFilesLocked() []lostFile {
	var out []lostFile
	add := func(scope Scope, m *Manifest) {
		if m == nil {
			return
		}
		for _, e := range m.Entries {
			out = append(out, lostFile{scope: scope, entry: copyEntry(e)})
		}
	}
	add(MainScope, v.manifest)
	for id, sub := range v.subs {
		add(Scope(id), sub.manifest)
	}
	return out
}

// heldKeys is one account's listing reduced to the names of the part objects
// in it, which is all the question needs.
type heldKeys map[string]struct{}

func heldKeysOf(objects []provider.ObjectInfo) heldKeys {
	held := make(heldKeys, len(objects))
	for _, obj := range objects {
		if _, _, ours := partOfKey(obj.Key); ours {
			held[obj.Key] = struct{}{}
		}
	}
	return held
}

// missingObjects is which of one shard record's objects a listing does not
// show: chunk indexes for a chunked file, and the single index 0 for a file
// stored whole. total is how many objects the record stands for.
func missingObjects(e *Entry, s Shard, held heldKeys) (missing []int, total int) {
	if !e.Chunked() {
		if _, ok := held[s.Key]; !ok {
			return []int{0}, 1
		}
		return nil, 1
	}
	for index := 0; index < e.ChunkCount; index++ {
		if _, ok := held[ChunkShardKey(e.ArchiveID, index, s.Part)]; !ok {
			missing = append(missing, index)
		}
	}
	return missing, e.ChunkCount
}

// judgeLostParts holds the index's records against the accounts' listings.
//
// held is by account ID and has an entry for every account that was listed —
// an account absent from it could not be asked, and a part recorded there is
// neither lost nor counted on. accounts is filled in place, by ID.
//
// It is a pure function of its arguments: no lock, no I/O.
func judgeLostParts(files []lostFile, held map[string]heldKeys, accounts map[string]*LostAccount) []LostPart {
	var lost []LostPart

	for _, f := range files {
		e := f.entry
		if len(e.Shards) == 0 {
			continue
		}
		// Which objects each record is missing, and whether its account was
		// listed at all.
		gaps := make([][]int, len(e.Shards))
		listed := make([]bool, len(e.Shards))
		short := false
		for i, s := range e.Shards {
			keys, ok := held[s.ProviderID]
			if !ok {
				continue
			}
			listed[i] = true
			missing, total := missingObjects(e, s, keys)
			gaps[i] = missing
			if acct := accounts[s.ProviderID]; acct != nil {
				acct.Recorded += total
			}
			if len(missing) > 0 {
				short = true
			}
		}
		if !short {
			continue
		}

		scheme := e.Scheme()
		absent := make([]map[int]bool, len(e.Shards))
		for i, missing := range gaps {
			if len(missing) == 0 {
				continue
			}
			absent[i] = make(map[int]bool, len(missing))
			for _, index := range missing {
				absent[i][index] = true
			}
		}

		for i, s := range e.Shards {
			if len(gaps[i]) == 0 {
				continue
			}
			total := 1
			if e.Chunked() {
				total = e.ChunkCount
			}
			row := LostPart{
				ProviderID:   s.ProviderID,
				ProviderName: s.ProviderName,
				ProviderKind: s.ProviderKind,
				Scope:        f.scope,
				FileID:       e.ID,
				ArchiveID:    e.ArchiveID,
				Part:         s.Part,
				Objects:      len(gaps[i]),
				Of:           total,
				Bytes:        s.Size * int64(len(gaps[i])) / int64(total),
				missing:      gaps[i],
			}
			if f.scope == MainScope {
				row.Path = e.Path()
			}
			if acct := accounts[s.ProviderID]; acct != nil {
				row.ProviderName, row.ProviderKind = acct.Name, acct.Kind
			}
			row.Restorable, row.Reason = restorable(e, scheme, i, gaps[i], absent, listed)
			lost = append(lost, row)
		}
	}
	return lost
}

// restorable judges whether the objects one record is missing can be cut
// again, from the listings alone.
//
// Each lost object is remade from k other shards of its own chunk, so the
// question is asked chunk by chunk: two parts that have each lost a different
// chunk can still mend one another. A part on an account that could not be
// listed is not counted on — it may be there, and the restore would find it,
// but a verdict that depends on it says so rather than promising.
func restorable(e *Entry, scheme archive.Scheme, record int, missing []int, absent []map[int]bool, listed []bool) (bool, string) {
	if !e.Chunked() {
		return false, "it is stored in the format SAND used before chunking, which has no single part to remake — " +
			"converting the file writes it out whole again"
	}
	if err := scheme.Check(); err != nil {
		return false, fmt.Sprintf("its code cannot be read: %v", err)
	}

	worst, unasked := -1, false
	for _, index := range missing {
		sure := map[int]bool{}
		maybe := map[int]bool{}
		for j, other := range e.Shards {
			if j == record {
				continue
			}
			switch {
			case !listed[j]:
				maybe[other.Part] = true
			case !absent[j][index]:
				sure[other.Part] = true
			}
		}
		if len(sure) >= scheme.Data {
			continue
		}
		if worst < 0 || len(sure) < worst {
			worst = len(sure)
		}
		for part := range maybe {
			if !sure[part] {
				unasked = true
			}
		}
	}
	switch {
	case worst < 0:
		return true, ""
	case unasked:
		return false, fmt.Sprintf(
			"remaking it takes %d of the file's other parts and only %d could be found — "+
				"the rest are on a cloud that could not be listed", scheme.Data, worst)
	default:
		return false, fmt.Sprintf(
			"remaking it takes %d of the file's other parts and only %d are still on the clouds",
			scheme.Data, worst)
	}
}

// scanForLostParts is the scan with every row it found.
func (v *Vault) scanForLostParts(ctx context.Context, onProgress func(OrphanScanProgress)) (*LostPartsScan, error) {
	report := func(at OrphanScanProgress) {
		if onProgress != nil {
			onProgress(at)
		}
	}

	// A file another machine has moved or erased since the last pull is still
	// in this index where it used to be, and would read here as lost. Pulled
	// first, for the reason the stray-parts scan pulls.
	v.mu.RLock()
	peers := v.hasPeersLocked()
	v.mu.RUnlock()
	if peers {
		report(OrphanScanProgress{Stage: ScanStagePulling})
		v.pullBeforeScan(ctx)
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	files := v.lostFilesLocked()
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	accounts := make(map[string]*LostAccount, len(configs))
	held := make(map[string]heldKeys, len(configs))
	var listing []provider.Config
	progress := OrphanScanProgress{Stage: ScanStageListing}
	for _, cfg := range configs {
		acct := &LostAccount{ProviderID: cfg.ID, Name: cfg.Name, Kind: string(cfg.Kind)}
		accounts[cfg.ID] = acct
		if v.livesElsewhere(cfg) {
			// The path would be tried on the wrong machine, and whatever is
			// or is not at it there says nothing about this account.
			acct.Error = fmt.Sprintf("it is a disk on %s, and can only be listed from there", cfg.Host)
			continue
		}
		listing = append(listing, cfg)
		progress.Waiting = append(progress.Waiting, cfg.Name)
	}
	progress.Roots = len(listing)
	report(progress.copy())

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range listing {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()
			keys, err := v.listPartKeys(ctx, cfg)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				accounts[cfg.ID].Error = err.Error()
			} else {
				held[cfg.ID] = keys
			}
			progress.Listed++
			progress.Waiting = without(progress.Waiting, cfg.Name)
			report(progress.copy())
		}(cfg)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	progress.Stage = ScanStageJudging
	report(progress.copy())

	scan := v.lostPartsFrom(files, held, accounts)
	for _, cfg := range configs {
		if acct := accounts[cfg.ID]; acct.Error != "" {
			scan.Warnings = append(scan.Warnings, fmt.Sprintf(
				"could not ask %s what it holds, so nothing recorded there was checked: %s", acct.Name, acct.Error))
		}
	}
	sort.Strings(scan.Warnings)
	return scan, nil
}

// listPartKeys is one account's parts folder, as the names of the part
// objects in it.
func (v *Vault) listPartKeys(ctx context.Context, cfg provider.Config) (heldKeys, error) {
	p, err := v.buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	objects, err := p.List(ctx, "")
	if err != nil {
		return nil, err
	}
	return heldKeysOf(objects), nil
}

// lostPartsFrom turns a snapshot of the index and the listings taken after it
// into a scan: judged, held against the index as it is now, and counted.
//
// The second look at the index is what makes a row true. A file moved to
// other clouds, re-encrypted or erased while the accounts were being listed
// was recorded where the snapshot says and is rightly not there any more; only
// a record that is still exactly what it was — same archive, same part, same
// account — is one whose objects ought to have been in the listing.
func (v *Vault) lostPartsFrom(files []lostFile, held map[string]heldKeys, accounts map[string]*LostAccount) *LostPartsScan {
	lost := judgeLostParts(files, held, accounts)

	if len(lost) > 0 {
		still := v.recordedNow()
		kept := lost[:0]
		for _, row := range lost {
			if still[recordKey{row.FileID, row.ArchiveID, row.Part, row.ProviderID}] {
				kept = append(kept, row)
			}
		}
		lost = kept
	}

	sort.SliceStable(lost, func(i, j int) bool {
		a, b := lost[i], lost[j]
		if a.ProviderName != b.ProviderName {
			return a.ProviderName < b.ProviderName
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.FileID != b.FileID {
			return a.FileID < b.FileID
		}
		return a.Part < b.Part
	})

	scan := &LostPartsScan{Items: []LostPart{}, Accounts: []LostAccount{}}
	fileSet := map[string]bool{}
	perAccount := map[string]map[string]bool{}
	for _, row := range lost {
		scan.Parts++
		scan.Objects += row.Objects
		scan.Bytes += row.Bytes
		if row.Restorable {
			scan.Restorable++
		}
		fileSet[row.FileID] = true

		acct := accounts[row.ProviderID]
		if acct == nil {
			continue
		}
		acct.Parts++
		acct.Lost += row.Objects
		acct.LostBytes += row.Bytes
		if row.Restorable {
			acct.Restorable++
		}
		if perAccount[row.ProviderID] == nil {
			perAccount[row.ProviderID] = map[string]bool{}
		}
		perAccount[row.ProviderID][row.FileID] = true
	}
	scan.Files = len(fileSet)
	scan.Found = scan.Parts > 0

	for id, acct := range accounts {
		acct.Files = len(perAccount[id])
		acct.Emptied = acct.Recorded > 0 && acct.Lost >= acct.Recorded
		if acct.Parts > 0 {
			scan.On = append(scan.On, acct.Name)
		}
		scan.Accounts = append(scan.Accounts, *acct)
	}
	sort.Strings(scan.On)
	sort.Slice(scan.Accounts, func(i, j int) bool {
		if scan.Accounts[i].Name != scan.Accounts[j].Name {
			return scan.Accounts[i].Name < scan.Accounts[j].Name
		}
		return scan.Accounts[i].ProviderID < scan.Accounts[j].ProviderID
	})

	scan.all = lost
	scan.Items = append(scan.Items, lost...)
	return scan
}

// recordKey names one shard record exactly: this part of this archive of this
// file, on this account.
type recordKey struct {
	fileID, archiveID string
	part              int
	providerID        string
}

// recordedNow is every shard record the open vaults hold at this moment.
func (v *Vault) recordedNow() map[recordKey]bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	out := map[recordKey]bool{}
	if v.dataKey == nil {
		return out
	}
	for _, f := range v.lostFilesLocked() {
		for _, s := range f.entry.Shards {
			out[recordKey{f.entry.ID, f.entry.ArchiveID, s.Part, s.ProviderID}] = true
		}
	}
	return out
}

// stillRecorded finds one row's file as the index has it now, provided the
// record the row is about is still exactly what it was. The entry is a copy.
func (v *Vault) stillRecorded(row *LostPart) (*Entry, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, false
	}
	_, found, ok := v.scopeOfEntryLocked(row.FileID)
	if !ok || found.ArchiveID != row.ArchiveID {
		return nil, false
	}
	for _, s := range found.Shards {
		if s.Part == row.Part && s.ProviderID == row.ProviderID {
			return copyEntry(found), true
		}
	}
	return nil, false
}

// The stages of a restore, in order. See LostRestoreProgress.
const (
	RestoreStageScanning  = "scanning"
	RestoreStageRestoring = "restoring"
)

// LostRestoreProgress is where a running restore has got to.
type LostRestoreProgress struct {
	// Stage is RestoreStageScanning while the accounts are listed again —
	// Scan then says how far that is — and RestoreStageRestoring after.
	Stage string              `json:"stage"`
	Scan  *OrphanScanProgress `json:"scan,omitempty"`

	// Objects is how many lost objects the run set out to put back, and Done
	// how many it has dealt with, written or not. Bytes is what has gone up.
	// Parts and PartsDone count shards the same way; Path and Cloud are the
	// one being worked on.
	Objects   int    `json:"objects"`
	Done      int    `json:"done"`
	Bytes     int64  `json:"bytes"`
	Parts     int    `json:"parts"`
	PartsDone int    `json:"parts_done"`
	Path      string `json:"path,omitempty"`
	Cloud     string `json:"cloud,omitempty"`
}

// LostRestoreFailure is one part a restore did not put back, and why.
type LostRestoreFailure struct {
	Path         string `json:"path,omitempty"`
	FileID       string `json:"file_id"`
	Part         int    `json:"part"`
	ProviderName string `json:"provider_name"`
	Reason       string `json:"reason"`
}

// LostRestoreReport is what a restore did.
type LostRestoreReport struct {
	// Parts is how many shards are whole again, across Files files; Objects
	// and Bytes are what was written to do it.
	Parts   int   `json:"parts"`
	Files   int   `json:"files"`
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// AlreadyThere counts objects the scan had down as lost that the account
	// said it was holding when asked for each by name — a listing that was
	// behind, or cut short. Nothing was written over them.
	AlreadyThere int `json:"already_there,omitempty"`

	// Withheld is how many parts the scan found that cannot be remade, and
	// Failed the ones that could have been and were not; Failures lists both,
	// capped, with FailuresTruncated the rest.
	Withheld          int                  `json:"withheld"`
	Failed            int                  `json:"failed"`
	Failures          []LostRestoreFailure `json:"failures,omitempty"`
	FailuresTruncated int                  `json:"failures_truncated,omitempty"`

	// On names the accounts parts were put back on.
	On []string `json:"on,omitempty"`

	// Stopped says the run ended before it had been through every part, and
	// StoppedBy why. What it had put back stays put back.
	Stopped   bool   `json:"stopped,omitempty"`
	StoppedBy string `json:"stopped_by,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

func (r *LostRestoreReport) fail(row *LostPart, reason string) {
	if len(r.Failures) >= lostPartPreview {
		r.FailuresTruncated++
		return
	}
	r.Failures = append(r.Failures, LostRestoreFailure{
		Path: row.Path, FileID: row.FileID, Part: row.Part,
		ProviderName: row.ProviderName, Reason: reason,
	})
}

// LostRestoreOptions says which lost parts to put back.
type LostRestoreOptions struct {
	// Accounts limits the restore to parts missing from these accounts, by
	// ID. Empty is every account. Every account is listed either way: the
	// other parts a shard is remade from have to be found too.
	Accounts []string

	// OnProgress is called as the restore moves, one call at a time, with a
	// value the receiver may keep. Nil is fine.
	OnProgress func(LostRestoreProgress)
}

// RestoreLostParts lists the accounts again and puts back every lost part
// that can be remade: each missing object is cut again from the file's other
// parts and written to the account the index records it on, under the name it
// had.
//
// It works from a listing of its own rather than from a scan somebody is
// holding, so that it acts on what is missing now. Before each object is
// written the account is asked for it by name, and one that answers is left
// alone — nothing is ever overwritten — and the file's record is read again,
// so a file moved or erased in the meantime is skipped. The index is not
// written. A stopped run (a cancelled context, the vault locking) keeps what
// it had put back and says so.
func (v *Vault) RestoreLostParts(ctx context.Context, opts LostRestoreOptions) (*LostRestoreReport, error) {
	var mu sync.Mutex
	at := LostRestoreProgress{Stage: RestoreStageScanning}
	say := func(change func(*LostRestoreProgress)) {
		mu.Lock()
		defer mu.Unlock()
		if change != nil {
			change(&at)
		}
		if opts.OnProgress != nil {
			out := at
			if at.Scan != nil {
				scan := at.Scan.copy()
				out.Scan = &scan
			}
			opts.OnProgress(out)
		}
	}
	say(nil)

	scan, err := v.scanForLostParts(ctx, func(p OrphanScanProgress) {
		say(func(at *LostRestoreProgress) { at.Scan = &p })
	})
	if err != nil {
		return nil, err
	}

	only := map[string]bool{}
	for _, id := range opts.Accounts {
		only[id] = true
	}
	report := &LostRestoreReport{Warnings: scan.Warnings}
	var todo []*LostPart
	objects := 0
	for i := range scan.all {
		row := &scan.all[i]
		if len(only) > 0 && !only[row.ProviderID] {
			continue
		}
		if !row.Restorable {
			report.Withheld++
			report.fail(row, row.Reason)
			continue
		}
		todo = append(todo, row)
		objects += row.Objects
	}
	// By file, so that a file with several parts lost is finished before the
	// next is begun and a stopped run leaves whole files rather than a
	// scattering of half-mended ones.
	sort.SliceStable(todo, func(i, j int) bool {
		if todo[i].FileID != todo[j].FileID {
			if todo[i].Path != todo[j].Path {
				return todo[i].Path < todo[j].Path
			}
			return todo[i].FileID < todo[j].FileID
		}
		return todo[i].Part < todo[j].Part
	})

	say(func(at *LostRestoreProgress) {
		at.Stage, at.Scan = RestoreStageRestoring, nil
		at.Objects, at.Parts = objects, len(todo)
	})

	files := map[string]bool{}
	on := map[string]bool{}
	for _, row := range todo {
		if err := ctx.Err(); err != nil {
			report.Stopped, report.StoppedBy = true, "it was stopped"
			break
		}
		say(func(at *LostRestoreProgress) { at.Path, at.Cloud = row.Path, row.ProviderName })

		dealt := 0
		outcome, err := v.restoreLostPart(ctx, row, func(n int64) {
			say(func(at *LostRestoreProgress) { at.Done++; at.Bytes += n; dealt++ })
		})
		// Whatever the part's objects came to, the bar has been through them.
		say(func(at *LostRestoreProgress) { at.Done += row.Objects - dealt; at.PartsDone++ })

		report.Objects += outcome.written
		report.Bytes += outcome.bytes
		report.AlreadyThere += outcome.present
		switch {
		case errors.Is(err, ErrLocked):
			report.Stopped, report.StoppedBy = true, "the vault was locked"
		case err != nil && ctx.Err() != nil:
			report.Stopped, report.StoppedBy = true, "it was stopped"
		case err != nil:
			report.Failed++
			report.fail(row, err.Error())
		default:
			if outcome.written > 0 {
				report.Parts++
				files[row.FileID] = true
				on[row.ProviderName] = true
			}
		}
		if report.Stopped {
			break
		}
	}
	report.Files = len(files)
	for name := range on {
		report.On = append(report.On, name)
	}
	sort.Strings(report.On)
	return report, nil
}

// lostPartOutcome is what putting one part back came to.
type lostPartOutcome struct {
	written int
	bytes   int64
	present int
}

// errNoLongerRecorded is a part whose file changed under the restore.
var errNoLongerRecorded = errors.New(
	"the file was moved, rewritten or erased while its part was being put back, so the part was left alone")

// restoreLostPart remakes the missing objects of one shard and writes them to
// the account the index records the shard on.
//
// onObject is called once per object dealt with — written (with its size) or
// found already there (with zero) — from the goroutines doing them.
func (v *Vault) restoreLostPart(ctx context.Context, row *LostPart, onObject func(int64)) (lostPartOutcome, error) {
	var out lostPartOutcome

	entry, ok := v.stillRecorded(row)
	if !ok {
		if v.locked() {
			return out, ErrLocked
		}
		return out, errNoLongerRecorded
	}
	if !entry.Chunked() {
		return out, errors.New("it is stored in the format from before chunking")
	}

	snap, err := v.snapshotRead(entry, provider.Parts)
	if err != nil {
		return out, err
	}
	defer crypto.ZeroBytes(snap.dataKey)

	target, connected := snap.configs[row.ProviderID]
	if !connected {
		return out, fmt.Errorf("%s is no longer connected", row.ProviderName)
	}
	p, err := v.buildProvider(target)
	if err != nil {
		return out, err
	}

	// Everything but the record being mended, on accounts this machine can
	// reach: the shards the lost one is cut again from.
	sources := *entry
	sources.Shards = sources.Shards[:0:0]
	for _, s := range entry.Shards {
		if s.Part == row.Part && s.ProviderID == row.ProviderID {
			continue
		}
		if cfg, ok := snap.configs[s.ProviderID]; !ok || v.livesElsewhere(cfg) {
			continue
		}
		sources.Shards = append(sources.Shards, s)
	}

	var mu sync.Mutex
	var first error
	var wrote []string
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return first != nil
	}

	// A handful at a time. Each in flight holds k shards of one chunk and the
	// chunk itself, which at the default chunk size is tens of megabytes.
	window := make(chan struct{}, min(max(v.Tuning().TransferWindow, 1), 4))
	var wg sync.WaitGroup
	for _, index := range row.missing {
		if failed() || ctx.Err() != nil {
			break
		}
		window <- struct{}{}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer func() { <-window }()
			if failed() {
				return
			}

			key := ChunkShardKey(entry.ArchiveID, index, row.Part)
			// Only "not found" is the account saying the object is gone. One
			// that is there is left exactly as it is; any other answer is no
			// answer, and nothing is written on the strength of it.
			switch _, err := p.Stat(ctx, key); {
			case err == nil:
				mu.Lock()
				out.present++
				mu.Unlock()
				onObject(0)
				return
			case !errors.Is(err, provider.ErrNotFound):
				fail(fmt.Errorf("%s would not say whether it holds chunk %d: %w", row.ProviderName, index+1, err))
				return
			}

			shards, err := v.gatherChunkShards(ctx, &sources, index, snap.configs, snap.dataKey, nil)
			if err != nil {
				fail(err)
				return
			}
			blob, err := archive.RemakeShard(shards, row.Part, snap.dataKey)
			if err != nil {
				fail(fmt.Errorf("remaking part %d of chunk %d: %w", row.Part, index+1, err))
				return
			}
			// The last moment before the write. A file that has moved on is
			// one whose object here would be a part no record points at.
			if _, ok := v.stillRecorded(row); !ok {
				fail(errNoLongerRecorded)
				return
			}
			if err := v.meteredPut(ctx, p, target, key, blob); err != nil {
				fail(fmt.Errorf("writing chunk %d to %s: %w", index+1, row.ProviderName, err))
				return
			}
			mu.Lock()
			out.written++
			out.bytes += int64(len(blob))
			wrote = append(wrote, key)
			mu.Unlock()
			onObject(int64(len(blob)))
		}(index)
	}
	wg.Wait()

	// And once more after the last write: a move or a re-encryption that
	// committed while these were in the air has already erased the objects
	// it knew about, and would not have known about these.
	if _, ok := v.stillRecorded(row); !ok && len(wrote) > 0 {
		if v.locked() {
			// Locked, not moved: the index cannot be read to know, and an
			// object the record still points at must not be taken away on a
			// guess. What was written is where the index said it belongs.
			return out, ErrLocked
		}
		for _, key := range wrote {
			_ = p.Delete(context.WithoutCancel(ctx), key)
		}
		out.written, out.bytes = 0, 0
		return out, errNoLongerRecorded
	}

	if first != nil {
		if errors.Is(first, ErrLocked) {
			return out, ErrLocked
		}
		if out.written > 0 {
			return out, fmt.Errorf("%d of its %d missing objects were put back before it stopped: %w",
				out.written, len(row.missing), first)
		}
		return out, first
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// locked reports whether the vault is locked right now.
func (v *Vault) locked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dataKey == nil
}

// Sentence is the summary as one line, for a log or a terminal.
func (s LostSummary) Sentence() string {
	if s.Parts == 0 {
		return "every part the index records is on the cloud it is recorded on"
	}
	return fmt.Sprintf("%d %s of %d %s %s not on %s, where the index records %s (%d can be put back)",
		s.Parts, pluralWord(s.Parts, "part", "parts"),
		s.Files, pluralWord(s.Files, "file", "files"),
		pluralWord(s.Parts, "is", "are"),
		strings.Join(s.On, ", "),
		pluralWord(s.Parts, "it", "them"), s.Restorable)
}
