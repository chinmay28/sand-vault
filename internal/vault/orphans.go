package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Parts on an account that the index has stopped pointing at.
//
// Every other operation in this package works outwards: the index says what
// exists and the accounts are told. This one works inwards. It asks each
// account what it is actually holding and subtracts what the index accounts
// for — and the remainder is not one thing but two.
//
// **Parts no file wants.** Erasing a file erases its parts from the accounts
// holding them, but only the ones connected at the time. Disconnect a cloud,
// spend a month deleting files, and none of the parts on it go anywhere. Nor
// can they later: reconnecting an account gives it a fresh ID (AddProvider
// mints one per connection, because a credential is all that is ever handed
// back and it says nothing about which account it was last time), so the vault
// has no way of knowing the returning account is the one those parts were
// erased from. They sit there — encrypted under a key the vault may not even
// hold any more, unreadable by their owner and by everybody else, and counting
// against a quota. Finding them is what the sweep half of this file is for.
//
// **Parts a file has lost track of.** The same disconnect does something else
// on the way past: RemoveProvider drops the shard records naming the account it
// is removing, because an index still claiming them would be lying about what
// can be retrieved. The objects stay, deliberately — deleting from an account
// SAND is being told to stop using is not SAND's call. Connect that storage
// back and the two facts never meet again, for a reason Reconcile cannot fix:
// it walks entry.Shards to re-point records rather than to invent them, and a
// record that was dropped has nothing left to re-point. So the file reports a
// missing spare part while the spare sits on a connected cloud. Putting it back
// is the reattach half, and it moves no bytes at all — a part's object key is
// derived from the archive ID and the shard number alone, so the object is
// already exactly where a record would say it is.
//
// Telling the two apart is the whole job, and it is why ownership is matched by
// archive and shard number rather than by exact key or by account. Getting it
// wrong in one direction wastes room; in the other it erases a file's last
// spare.
//
// Nothing here acts on its own initiative. Detection is safe to run unattended
// and writes nothing. The sweep is a separate call with its own
// re-verification, and it refuses in exactly the cases where "the index does
// not mention it" is not the same thing as "it is not needed" — see
// orphanGuard. The reattach is a separate call too, and carries none of those
// refusals, because it is purely additive: a file can only come out of it with
// more shards than it went in with.

// partKeyPattern matches the object keys SAND writes for parts, in both shapes:
// <archive>-p<n>.sand for a file stored whole, and <archive>-c<index>-p<n>.sand
// for one chunk of a chunked one (see ShardKey and ChunkShardKey).
//
// Strict on purpose. A key that does not match is not treated as SAND's at all,
// which costs an orphan going unreported and buys the guarantee that this never
// proposes deleting something it did not write.
var partKeyPattern = regexp.MustCompile(`^([0-9a-f]{8,64})(?:-c[0-9]{1,12})?-p([0-9]{1,4})\.sand$`)

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
	id, _, ours := partOfKey(key)
	return id, ours
}

// partOfKey reads an object key back into the archive and the shard number it
// belongs to.
//
// The shard number is what tells a part that is merely unrecorded apart from
// one that is genuinely abandoned. Both are objects the index does not name;
// only the first has a file in the tree waiting for it.
func partOfKey(key string) (archiveID string, part int, ours bool) {
	match := partKeyPattern.FindStringSubmatch(key)
	if match == nil {
		return "", 0, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return match[1], number, true
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

	// PreviousKey says the index knows exactly what this is: a part a
	// re-encryption left here, sealed under the previous key, because the
	// account could not be reached at the time (staleparts.go). The one
	// kind of abandoned archive something *can* be said about — Path is the
	// file it belonged to, for the main vault's files — and the one kind
	// worth erasing for more than the room: the previous password opens it.
	PreviousKey bool   `json:"previous_key,omitempty"`
	Path        string `json:"path,omitempty"`
}

// StrayShard is one shard of one file, sitting on an account with no record in
// the index pointing at it.
//
// This is the other thing a listing turns up, and it is the opposite of an
// orphan: the file is right there in the tree, missing a spare part, while the
// part itself sits on a connected account unreferenced. Disconnecting an
// account is what makes them — RemoveProvider drops the shard records naming it
// so that the index goes on telling the truth about what is retrievable — and
// reconnecting cannot undo it, because Reconcile re-points records and there is
// no record left to re-point.
//
// Writing one back moves no bytes at all. The object key is derived from the
// archive ID and the shard number (see ShardKey), so a shard is recoverable the
// moment its account can be listed.
type StrayShard struct {
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderKind string `json:"provider_kind"`

	// Scope is which vault inside the vault the file belongs to, and Path is
	// carried only for the main one — a sub vault tells the main password its
	// name and its weight and nothing else, which is the rule ProviderStats
	// keeps too.
	Scope  Scope  `json:"scope,omitempty"`
	FileID string `json:"file_id"`
	Path   string `json:"path,omitempty"`

	ArchiveID string `json:"archive_id"`
	Part      int    `json:"part"`

	// Objects is how many stored objects this shard occupies — one for a file
	// stored whole, one per chunk otherwise — and Bytes what they weigh.
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// Have and Want are the shards the index records for this file against the
	// shards its code allows, so a row can say what recording this buys.
	Have int `json:"have"`
	Want int `json:"want"`

	Reattachable bool   `json:"reattachable"`
	Reason       string `json:"reason,omitempty"`
}

// OrphanAccount is one connected account's share of the answer — or one
// root's: an account that keeps a replica folder answers twice, once for its
// parts folder and once, in a row of its own, for its replicas.
type OrphanAccount struct {
	// ProviderID is the account's ID for its parts folder, and the account's
	// ID with replicaSuffix for its replica folder, so that a sweep target
	// names one root and never the other. Name says which in words.
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Replicas says this row is the account's replica folder. What it counts
	// is replica objects against the replicas the index names; a part never
	// lives there, and a replica object never counts in the parts row.
	Replicas bool `json:"replicas,omitempty"`

	// Objects and Bytes are every part object the account is holding, whether
	// or not the index accounts for it — the denominator the orphan figures
	// are worth reading against.
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// Orphans, OrphanBytes and Archives are what nothing points at.
	Orphans     int   `json:"orphans"`
	OrphanBytes int64 `json:"orphan_bytes"`
	Archives    int   `json:"archives"`

	// PreviousKey and PreviousKeyBytes are the share of those that a
	// re-encryption left here under the previous key, on record in the
	// index. Counted inside Orphans, not beside it.
	PreviousKey      int   `json:"previous_key,omitempty"`
	PreviousKeyBytes int64 `json:"previous_key_bytes,omitempty"`

	// Strays and StrayBytes are the other half: objects belonging to a file
	// that is still in the tree but has no record of them. Not abandoned —
	// mislaid, and free to put back.
	Strays     int   `json:"strays"`
	StrayBytes int64 `json:"stray_bytes"`

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

	// PreviousKey, PreviousKeyBytes and PreviousKeyArchives count the orphans
	// the index put there itself: parts a re-encryption left under the
	// previous key on an account it could not reach, now found where it left
	// them. PreviousKeyOn names the accounts. Counted inside the totals
	// above, and called out because they are the reason to run the sweep
	// today rather than someday — the previous password still opens them.
	PreviousKey         int      `json:"previous_key,omitempty"`
	PreviousKeyBytes    int64    `json:"previous_key_bytes,omitempty"`
	PreviousKeyArchives int      `json:"previous_key_archives,omitempty"`
	PreviousKeyOn       []string `json:"previous_key_on,omitempty"`

	// Strays are shards of files still in the tree that the index has no
	// record of, and Reattachable how many of them can be written back. They
	// have nothing to do with the sweep — nothing here is ever erased — and
	// are reported by the same pass because it is the same listing.
	Strays          []StrayShard `json:"strays"`
	StraysTruncated int          `json:"strays_truncated,omitempty"`
	Reattachable    int          `json:"reattachable"`
	StrayFiles      int          `json:"stray_files"`
	StrayBytes      int64        `json:"stray_bytes"`

	Accounts []OrphanAccount `json:"accounts"`

	// Items are the per-archive rows, largest first and capped;
	// ItemsTruncated is how many did not fit.
	Items          []OrphanArchive `json:"items"`
	ItemsTruncated int             `json:"items_truncated,omitempty"`

	// Leftovers is the same question asked of the one disk SAND writes to
	// itself: the working files in the vault's own directory that no operation
	// is using any more, chiefly the spool an interrupted upload left at the
	// full size of the file it was sending. It rides along with this scan
	// because it is the same question and it is asked at the same moment, and
	// it is swept by a call of its own — see leftovers.go.
	Leftovers *LeftoverScan `json:"leftovers,omitempty"`

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
	if len(s.Items) > orphanArchivePreview {
		s.ItemsTruncated = len(s.Items) - orphanArchivePreview
		s.Items = s.Items[:orphanArchivePreview]
	}
	if len(s.Strays) > orphanArchivePreview {
		s.StraysTruncated = len(s.Strays) - orphanArchivePreview
		s.Strays = s.Strays[:orphanArchivePreview]
	}
}

// StrayScan is the reattach half of a listing on its own: the shards of files
// still in the tree that an account holds with no record pointing at them.
//
// It exists so that the question can be asked of one cloud. The full scan
// lists every account at once, which is the right cost when the set of
// connected accounts has just changed and the wrong one when somebody is
// looking at a list of thirty thousand short files and has a fair idea which
// cloud the parts are on. Nothing about the sweep is in here — no orphan
// figures, no Blocked — because a scan of one account cannot say anything safe
// about what is abandoned, and must not look as though it could.
type StrayScan struct {
	// Accounts are the roots that were listed: the one asked for, or every
	// connected account's parts folder. An account that would not answer has
	// its Error set, and is the only way "no strays" can be told apart from
	// "could not look".
	Accounts []OrphanAccount `json:"accounts"`

	// Strays are the rows, capped the way the full scan caps them, with the
	// same per-row verdicts; Reattachable, StrayFiles and StrayBytes describe
	// every row found rather than the ones that fit.
	Strays          []StrayShard `json:"strays"`
	StraysTruncated int          `json:"strays_truncated,omitempty"`
	Reattachable    int          `json:"reattachable"`
	StrayFiles      int          `json:"stray_files"`
	StrayBytes      int64        `json:"stray_bytes"`

	Warnings []string `json:"warnings,omitempty"`
}

// ScanForStrays lists one connected account — or every one, when providerID
// is empty — for shards of files still in the tree that the index has no
// record of, and says which of them ReattachShards would write back.
//
// One listing for the account named, and nothing is written. The verdict per
// row is exactly the one the full scan gives, from the same index; see
// scanForOrphansOn for what a scan of one account can and cannot see. An ID no
// connected account carries is ErrNoAccount.
func (v *Vault) ScanForStrays(ctx context.Context, providerID string) (*StrayScan, error) {
	scan, err := v.scanForOrphansOn(ctx, providerID)
	if err != nil {
		return nil, err
	}
	scan.trimPreview()
	return &StrayScan{
		Accounts:        scan.Accounts,
		Strays:          scan.Strays,
		StraysTruncated: scan.StraysTruncated,
		Reattachable:    scan.Reattachable,
		StrayFiles:      scan.StrayFiles,
		StrayBytes:      scan.StrayBytes,
		Warnings:        scan.Warnings,
	}, nil
}

// scanForOrphans is ScanForOrphans with every row it found, however many that
// is.
func (v *Vault) scanForOrphans(ctx context.Context) (*OrphanScan, error) {
	return v.scanForOrphansOn(ctx, "")
}

// scanForOrphansOn is the scan over one connected account's parts folder when
// providerID names one, and over every root when it is empty.
//
// The owned set is the same either way — it comes from the index, not from
// the other accounts — so a shard judged mislaid on one account is judged
// exactly as a full scan would judge it. What a scoped scan cannot see is a
// second copy of the same shard on another account, and it does not need to:
// a copy the index already has a record for is "accounted for" whichever
// account it sits on, and one it does not is found the next time that account
// is listed. A replica folder is not listed in a scoped scan, since nothing in
// one is ever reattached (see ownedReplicaArchivesLocked).
//
// An ID no connected account carries is ErrNoAccount rather than an empty
// answer, because an empty answer here reads as "that cloud holds nothing of
// yours", which is not what happened.
func (v *Vault) scanForOrphansOn(ctx context.Context, providerID string) (*OrphanScan, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	owned := v.ownedArchivesLocked()
	// Taken under the same lock as the index, so that an upload whose commit
	// lands between the two reads is seen by at least one of them: either the
	// index already names it, or the registry still does.
	v.noteInFlight(owned)
	listings := v.orphanListingsLocked(owned)
	if providerID != "" {
		cfg, ok := v.configForLocked(providerID)
		if !ok {
			v.mu.RUnlock()
			return nil, fmt.Errorf("%w: %s", ErrNoAccount, providerID)
		}
		listings = []orphanListing{{cfg: cfg, owned: owned}}
	}
	policy := v.store.Policy
	vaultKey := append([]byte(nil), v.vaultKey...)
	stale := v.manifest.stalePartsByKey()
	// The salt of this vault's own copies from before a password change, for
	// as long as one may still be sitting on an account the pushes since
	// could not reach. A copy sealed under it is stale, not foreign.
	var previousSalts []string
	if v.store.BackupNeedsForce {
		previousSalts = append([]string(nil), v.store.PreviousBackupSalts...)
	}
	v.mu.RUnlock()
	defer crypto.ZeroBytes(vaultKey)

	scan := &OrphanScan{
		Accounts: []OrphanAccount{},
		Items:    []OrphanArchive{},
		Strays:   []StrayShard{},

		// A local directory read, before any account is asked anything. It is
		// answered even on a vault with no clouds connected at all, which is
		// the state a machine that has just been reinstalled is in — and the
		// state a half-finished import leaves the most spools behind in.
		Leftovers: v.ScanForLeftovers(),
	}
	if len(listings) == 0 {
		return scan, nil
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var found []OrphanArchive
	var loose []StrayShard
	// The parts on record as left under the previous key that a listing
	// actually turned up, and the accounts whose listing was complete — the
	// two facts that together say which of the record is out of date.
	seen := map[stalePartKey]bool{}
	listed := map[string]bool{}

	for _, listing := range listings {
		wg.Add(1)
		go func(listing orphanListing) {
			defer wg.Done()
			account, archives, strays, present := v.scanAccountForOrphans(ctx, listing, policy, vaultKey, previousSalts, stale)

			mu.Lock()
			defer mu.Unlock()
			scan.Accounts = append(scan.Accounts, account)
			found = append(found, archives...)
			loose = append(loose, strays...)
			if account.Error != "" {
				scan.Warnings = append(scan.Warnings, fmt.Sprintf(
					"could not ask %s what it holds, so anything abandoned there is not counted: %s",
					account.Name, account.Error))
			} else if !listing.replicas {
				listed[listing.cfg.ID] = true
				for key := range present {
					seen[key] = true
				}
			}
		}(listing)
	}
	wg.Wait()

	// A part on record that a complete listing of its account did not show
	// is gone — erased by hand, by the account's own tools, or by a sweep
	// this process did not see — and the record is corrected here rather
	// than left to promise a cleanup forever. The write is housekeeping on
	// the index's own notes, nothing a file depends on, so a failure is a
	// warning and the next scan tries again.
	if len(stale) > 0 {
		if err := v.forgetStaleParts(func(p *StalePart) bool {
			return listed[p.ProviderID] && !seen[p.key()]
		}); err != nil && !errors.Is(err, ErrLocked) {
			scan.Warnings = append(scan.Warnings, fmt.Sprintf(
				"the record of parts left under the previous key could not be brought up to date: %v", err))
		}
	}

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
		if found[i].PreviousKey {
			scan.PreviousKey += found[i].Objects
			scan.PreviousKeyBytes += found[i].Bytes
			scan.PreviousKeyArchives++
		}
	}
	scan.Found = scan.Archives > 0
	for _, a := range scan.Accounts {
		if a.PreviousKey > 0 {
			scan.PreviousKeyOn = append(scan.PreviousKeyOn, a.Name)
		}
	}
	sort.Strings(scan.PreviousKeyOn)

	// Only one row per shard survives, however many accounts turn out to be
	// holding a copy of it: recording the same shard twice is not redundancy,
	// it is one file claiming a spare it does not have. The rest are left where
	// they are and said so.
	sort.Slice(loose, func(i, j int) bool {
		if loose[i].ArchiveID != loose[j].ArchiveID {
			return loose[i].ArchiveID < loose[j].ArchiveID
		}
		if loose[i].Part != loose[j].Part {
			return loose[i].Part < loose[j].Part
		}
		return loose[i].ProviderName < loose[j].ProviderName
	})
	claimed := map[strayKey]string{}
	files := map[string]bool{}
	for i := range loose {
		key := strayKey{archiveID: loose[i].ArchiveID, part: loose[i].Part}
		switch holder, taken := claimed[key]; {
		case !loose[i].Reattachable:
		case taken:
			loose[i].Reattachable = false
			loose[i].Reason = fmt.Sprintf("%s holds a copy of that same shard and is taking it", holder)
		default:
			claimed[key] = loose[i].ProviderName
		}
		scan.StrayBytes += loose[i].Bytes
		files[string(loose[i].Scope)+"\x00"+loose[i].FileID] = true
		if loose[i].Reattachable {
			scan.Reattachable++
		}
	}
	scan.StrayFiles = len(files)
	scan.Strays = loose

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

// orphanListing is one root to be listed: an account at its parts folder, or
// the same account at its replica folder, with the archives the index owns
// in that root.
//
// The two roots are listed separately and judged against separate owned
// sets, because the folder is the only thing that tells a replica object from
// a part (see replica.go) and an object is abandoned only relative to the
// root it is in: a replica archive the index names is owned in the replica
// folder and would be an orphan anywhere else.
type orphanListing struct {
	// cfg is what the root is opened with: the account's own config, or its
	// replicaConfig, whose ID carries replicaSuffix.
	cfg      provider.Config
	owned    map[string]*ownedArchive
	replicas bool
}

// orphanListingsLocked is every root the scan lists: each connected account
// at its parts folder, judged against owned, and each account that keeps a
// replica folder at that folder too, judged against the replica archives the
// index names. The caller must hold at least the read lock.
func (v *Vault) orphanListingsLocked(owned map[string]*ownedArchive) []orphanListing {
	var replicas map[string]*ownedArchive
	out := make([]orphanListing, 0, len(v.providers))
	for _, cfg := range v.providers {
		out = append(out, orphanListing{cfg: cfg, owned: owned})
		if cfg.ReplicaFolder == "" {
			continue
		}
		at, err := replicaConfig(cfg)
		if err != nil {
			// A folder that no longer validates against its parts folder
			// cannot be opened, and a root that cannot be opened is one the
			// scan says nothing about rather than one it empties.
			continue
		}
		if replicas == nil {
			replicas = v.ownedReplicaArchivesLocked()
			// A replica being written is registered in the same in-flight
			// registry as a part (the scatter does not know which root it is
			// at), so the replica roots honour it the way the parts roots do.
			v.noteInFlight(replicas)
		}
		out = append(out, orphanListing{cfg: at, owned: replicas, replicas: true})
	}
	return out
}

// scanAccountForOrphans is ScanForOrphans' work for a single root.
//
// previousSalts are the KDF salts of this vault's own manifest copies from
// before a password change that has not reached every account yet; a copy
// sealed under one of them is this vault's, stale, and not a sign of another
// vault. stale is the record of parts re-encryptions left under the
// previous key, by account, archive and shard; the keys of it this listing
// turned up come back as present, so the caller can tell which of the record
// has been overtaken by events.
func (v *Vault) scanAccountForOrphans(
	ctx context.Context,
	listing orphanListing,
	policy Policy,
	vaultKey []byte,
	previousSalts []string,
	stale map[stalePartKey]*StalePart,
) (account OrphanAccount, archives []OrphanArchive, loose []StrayShard, present map[stalePartKey]bool) {
	cfg, owned := listing.cfg, listing.owned
	account = OrphanAccount{
		ProviderID: cfg.ID,
		Name:       cfg.Name,
		Kind:       string(cfg.Kind),
		Replicas:   listing.replicas,
	}
	if listing.replicas {
		// Said in the name as well as the flag, so a row is never read as
		// the parts folder by anything that shows only names.
		account.Name = cfg.Name + " (replicas)"
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		account.Error = err.Error()
		return account, nil, nil, nil
	}
	objects, err := p.List(ctx, "")
	if err != nil {
		// No listing, no answer. Reporting nothing here is the conservative
		// outcome: the alternative is an account whose every part reads as
		// abandoned because it never got to say otherwise.
		account.Error = err.Error()
		return account, nil, nil, nil
	}

	// The manifest backup lives in the parts folder, so only the parts row
	// can say whose account this is; the replica row has nothing to read.
	if !listing.replicas {
		account.Backup, account.Foreign = v.backupState(ctx, p, vaultKey, previousSalts)
	}

	reason := ""
	if account.Foreign {
		reason = fmt.Sprintf("%s carries a vault index this one did not write, "+
			"so parts here may belong to that vault rather than to nobody", cfg.Name)
	}

	// Three buckets, not two. An object the index does not name is not one
	// thing but two: a part of a file that is still in the tree, whose record
	// was dropped and can be written back — and an object belonging to no file
	// at all, which is the only kind worth erasing. Everything else is already
	// accounted for.
	sizes := map[string]int64{}
	counts := map[string]int{}
	strays := map[strayKey]*StrayShard{}
	// The archives among the orphans that the index itself left here under
	// the previous key, with the file each belonged to.
	previous := map[string]string{}
	present = map[stalePartKey]bool{}

	for _, obj := range objects {
		if obj.Key == BackupKey {
			continue
		}
		id, part, ours := partOfKey(obj.Key)
		if !ours {
			// Something else's file, sharing the folder or the bucket. Not
			// counted, not reported and certainly not deleted.
			continue
		}
		account.Objects++
		account.Bytes += obj.Size

		owner, known := owned[id]
		switch {
		case !known:
			account.Orphans++
			account.OrphanBytes += obj.Size
			counts[id]++
			sizes[id] += obj.Size
			// An orphan the index can account for: a part a re-encryption
			// left here when the account could not be reached. It is
			// matched by account as well as by object, because the record
			// names the account the part was left on, and an account
			// connected back under a fresh ID is a different account.
			if left, ok := stale[stalePartKey{providerID: cfg.ID, archiveID: id, part: part}]; ok {
				present[left.key()] = true
				account.PreviousKey++
				account.PreviousKeyBytes += obj.Size
				if _, noted := previous[id]; !noted {
					previous[id] = left.Path
				}
			}

		case owner.parts[part]:
			// The index already has a record for this shard of this file. The
			// object is that record's, or a second copy of it left by an
			// interrupted move; either way a file in the tree answers for it.

		case !owner.reattachable:
			// A thumbnail pack's shard, or one belonging to a locked sub
			// vault. Not abandoned, and not something this can mend.

		default:
			row := strays[strayKey{archiveID: id, part: part}]
			if row == nil {
				row = &StrayShard{
					ProviderID:   cfg.ID,
					ProviderName: cfg.Name,
					ProviderKind: string(cfg.Kind),
					Scope:        owner.scope,
					FileID:       owner.entryID,
					ArchiveID:    id,
					Part:         part,
					Have:         len(owner.parts),
					Want:         owner.scheme.Total,
				}
				if owner.scope == MainScope {
					// A sub vault tells the main password its name and its
					// weight and nothing about what is inside it, which is the
					// rule ProviderStats keeps too — so a path is carried only
					// for the vault the panel showing this is already looking
					// at.
					row.Path = owner.path
				}
				strays[strayKey{archiveID: id, part: part}] = row
			}
			row.Objects++
			row.Bytes += obj.Size
			account.Strays++
			account.StrayBytes += obj.Size
		}
	}

	account.Archives = len(counts)
	archives = make([]OrphanArchive, 0, len(counts))
	for id, count := range counts {
		path, fromRekey := previous[id]
		archives = append(archives, OrphanArchive{
			ProviderID:   cfg.ID,
			ProviderName: cfg.Name,
			ProviderKind: string(cfg.Kind),
			ArchiveID:    id,
			Objects:      count,
			Bytes:        sizes[id],
			Deletable:    reason == "",
			Reason:       reason,
			PreviousKey:  fromRekey,
			Path:         path,
		})
	}

	loose = make([]StrayShard, 0, len(strays))
	for key, row := range strays {
		owner := owned[key.archiveID]
		row.Reattachable, row.Reason = reattachable(owner, cfg, policy, key.part, row.Objects)
		loose = append(loose, *row)
	}
	return account, archives, loose, present
}

// strayKey identifies one shard of one archive, which is the unit a reattach
// works in: a chunked file's shard is many objects and one decision.
type strayKey struct {
	archiveID string
	part      int
}

// reattachable judges whether one loose shard can be written back into the
// index, and says why not when it cannot.
//
// Recording a shard moves no bytes and re-encrypts nothing — the object is
// already sitting there under a key derived from the archive ID, which is what
// makes this cheap. What it changes is what the vault believes it can reach,
// so the checks are all about not believing something false.
func reattachable(owner *ownedArchive, cfg provider.Config, policy Policy, part, objects int) (bool, string) {
	if owner == nil {
		return false, "the file it belongs to is no longer in the index"
	}
	// A shard number the file's own code has no room for is not this file's
	// shard, whatever its name says — a 2-of-3 file has no part 4. Checked
	// against the scheme rather than against the records, because the records
	// are precisely what is missing here.
	if total := owner.scheme.Total; total > 0 && (part < 1 || part > total) {
		return false, fmt.Sprintf("shard %d is not one a %s file has", part, owner.scheme)
	}
	if owner.chunkCount > 0 && objects != owner.chunkCount {
		// A chunked file is one object per chunk per shard, and a shard that
		// is only partly here is not a shard. It would still win the odd race
		// per chunk, but recording it would say the file has a spare it does
		// not have.
		return false, fmt.Sprintf("only %d of its %d chunks are on %s", objects, owner.chunkCount, cfg.Name)
	}
	if policy == PolicyStrict && owner.accounts[cfg.ID] {
		// Strict placement gives each account one shard of a file. The bytes
		// are on the account either way; recording them would make the vault
		// act on a placement it promises not to make.
		return false, fmt.Sprintf("%s already holds a shard of that file, and strict placement gives each account one", cfg.Name)
	}
	return true, ""
}

// backupState reports whether an account carries a manifest backup, and whether
// it is one this vault's key cannot open — the mark of a second vault having
// written here. It is the same test guardForeignBackup and the recovery scan
// apply.
//
// An account that will not say is treated as carrying a foreign one, which is
// the answer that withholds the delete button rather than the one that offers
// it.
//
// One copy this vault cannot open is still its own: the one sealed before a
// password change, on an account the forced pushes since have not reached —
// away through the change, back now, and holding exactly the stale copy the
// vault remembers owing it (store.PreviousBackupSalts). The envelope carries
// the salt it was sealed with, which is how it is recognised. It is reported
// as a backup and not as a foreign one, so the parts beside it — which are
// where a re-encryption past that account left them — are offered rather
// than withheld as another vault's.
func (v *Vault) backupState(ctx context.Context, p provider.Provider, vaultKey []byte, previousSalts []string) (backup, foreign bool) {
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
	if b.opensWith(vaultKey) {
		return true, false
	}
	return true, !slices.Contains(previousSalts, b.KDF.Salt)
}

// orphanGuard lists the reasons a sweep must not be offered at all.
//
// Each of them is a state in which "no index mentions these parts" stops
// meaning "these parts are abandoned", and every one of them is a state a
// perfectly ordinary user can be in.
func orphanGuard(owned map[string]*ownedArchive, accounts []OrphanAccount) []string {
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
	//
	// An archive that is only in flight does not count as the vault holding
	// something: it is not in any index yet, and a fresh vault's first upload
	// must not be what lets a sweep run on borrowed accounts.
	indexed := 0
	for _, row := range owned {
		if !row.inFlight {
			indexed++
		}
	}
	if indexed == 0 {
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

// ownedArchive is what the index still says about one archive it points at.
//
// The orphan half of this file only ever asks whether an archive is in here at
// all. The reattach half needs the rest of it: which shard numbers already have
// a record, which accounts they sit on, and whether the index this came from is
// one that can be written to.
type ownedArchive struct {
	scope   Scope
	entryID string
	path    string

	scheme     archive.Scheme
	chunkCount int

	// parts are the shard numbers the index has a record for, and accounts the
	// accounts it records any shard of this archive on.
	parts    map[int]bool
	accounts map[string]bool

	// reattachable is false where a missing shard cannot be recorded back even
	// if its object turns up: a thumbnail pack, which is remade rather than
	// repaired, and a locked sub vault's inventory, which names archives
	// without there being a readable index to write a record into.
	reattachable bool

	// inFlight marks a row that came from the registry in inflight.go rather
	// than from any index: an archive a scatter is writing, or has just
	// written, whose entry has not been recorded yet. Owned, so its parts are
	// never offered; not a file, so it does not count as the vault holding
	// anything.
	inFlight bool
}

// ownedArchivesLocked collects every archive this vault still points at, across
// the main index, every sub vault that is open, and every sub vault that is not.
//
// The last of those is what makes the sweep safe to act on. A locked sub
// vault's index is unreadable — that is the point of it — but the main vault
// keeps an inventory of the archives it owns for exactly this kind of question,
// refreshed from its index on every write it makes. Without it, opening the
// browser without typing a sub vault's password would make every file inside it
// look like garbage.
//
// The caller must hold at least the read lock.
func (v *Vault) ownedArchivesLocked() map[string]*ownedArchive {
	owned := map[string]*ownedArchive{}

	note := func(id string) *ownedArchive {
		if id == "" {
			return nil
		}
		if existing, ok := owned[id]; ok {
			return existing
		}
		fresh := &ownedArchive{parts: map[int]bool{}, accounts: map[string]bool{}}
		owned[id] = fresh
		return fresh
	}
	// A shard's own key names its archive, which is how a record whose entry
	// has no ArchiveID of its own is still accounted for.
	fromShards := func(shards []Shard, fill func(*ownedArchive)) {
		for _, sh := range shards {
			id, part, ours := partOfKey(sh.Key)
			if !ours {
				continue
			}
			row := note(id)
			if row == nil {
				continue
			}
			if fill != nil {
				fill(row)
			}
			row.parts[part] = true
			row.accounts[sh.ProviderID] = true
		}
	}

	fromManifest := func(scope Scope, m *Manifest) {
		if m == nil {
			return
		}
		for _, e := range m.Entries {
			describe := func(row *ownedArchive) {
				row.scope = scope
				row.entryID = e.ID
				row.path = e.Path()
				row.scheme = e.Scheme()
				row.chunkCount = e.ChunkCount
				row.reattachable = true
			}
			if row := note(e.ArchiveID); row != nil {
				describe(row)
				// The records themselves may name a different account for a
				// part than the entry's ArchiveID suggests; they are read
				// below. What matters here is that the archive is described
				// even when it holds no records at all, which is a file that
				// has lost every shard.
			}
			fromShards(e.Shards, describe)
		}
		// Packs are noted so their parts are never mistaken for abandoned, and
		// left unreattachable: a thumbnail is derived from a file that is still
		// stored, so a pack missing a shard is drawn again rather than mended.
		for _, pack := range m.Thumbs {
			if pack != nil {
				fromShards(pack.Shards, nil)
			}
		}
	}

	fromManifest(MainScope, v.manifest)
	for id, sub := range v.subs {
		fromManifest(Scope(id), sub.manifest)
	}
	for _, meta := range v.manifest.SubVaults {
		if meta == nil {
			continue
		}
		if _, open := v.subs[meta.ID]; open {
			// Already read from its own index above, in more detail than an
			// inventory carries.
			continue
		}
		for _, item := range meta.Inventory {
			row := note(item.ArchiveID)
			if row == nil {
				continue
			}
			for _, part := range item.Parts {
				row.parts[part.Part] = true
				row.accounts[part.ProviderID] = true
			}
		}
	}
	return owned
}

// ownedReplicaArchivesLocked collects every replica archive the index still
// points at, which is the owned set for a replica folder: anything else in
// one is abandoned. Main vault only, since a replica is a main-vault
// placement.
//
// Replicas are forgotten without being erased more often than parts are —
// a recovery drops them all, a disconnect drops the ones that lost too many
// shards, a retired key being dropped takes its replicas with it, and an
// erase that fails leaves objects behind — so this is the only way the space
// ever comes back. Left unreattachable: a replica short of a shard is made
// again by the worker, not mended.
//
// A replica being made right now is not in the index yet; it is in the
// index, or erased, by the time replicateFile returns, and the worker holds
// one slot. When a registry of archives in flight exists, the IDs it holds
// at the Replicas role belong in this set too, so a scan that overlaps a
// long copy does not offer to erase it.
//
// The caller must hold at least the read lock.
func (v *Vault) ownedReplicaArchivesLocked() map[string]*ownedArchive {
	owned := map[string]*ownedArchive{}
	for _, e := range v.manifest.Entries {
		r := e.Replica
		if r == nil || r.ArchiveID == "" {
			continue
		}
		row := &ownedArchive{
			scope:      MainScope,
			entryID:    e.ID,
			path:       e.Path(),
			scheme:     r.Scheme(),
			chunkCount: r.ChunkCount,
			parts:      map[int]bool{},
			accounts:   map[string]bool{},
		}
		for _, s := range r.Shards {
			row.parts[s.Part] = true
			row.accounts[s.ProviderID] = true
		}
		owned[r.ArchiveID] = row
	}
	return owned
}

// OrphanTarget names one archive on one account to erase. ProviderID is the
// account's ID for its parts folder, or the ID the scan reported for its
// replica folder (see OrphanAccount.ProviderID).
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
//
// onProgress, when given, is told how many of the doomed objects have been
// dealt with so far — once with (0, total) the moment the re-scan has decided
// what goes, then once per object, erased or failed alike, since a failure is
// dealt with too: it is reported at the end, not waited on. The total is the
// fresh scan's count and the erase lists each account once more on its way in,
// so the last call can land slightly off it — the report is the exact answer.
// It is a window for whoever is waiting on the request, nothing more: no job
// state, nothing written down. Calls arrive in order.
func (v *Vault) SweepOrphans(ctx context.Context, targets []OrphanTarget, dryRun bool, onProgress func(done, total int)) (*OrphanSweepReport, error) {
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

	// Every root a target can name: the account at its parts folder under
	// its own ID, and at its replica folder under the suffixed one the scan
	// reported — so an erase lands in the folder the archive was found in.
	v.mu.RLock()
	configs := make(map[string]provider.Config, len(v.providers))
	for _, listing := range v.orphanListingsLocked(nil) {
		configs[listing.cfg.ID] = listing.cfg
	}
	v.mu.RUnlock()

	byAccount := map[string][]OrphanArchive{}
	for _, item := range wanted {
		byAccount[item.ProviderID] = append(byAccount[item.ProviderID], item)
	}

	// The denominator, announced before the first delete: the re-scan above is
	// a listing of every account, and whoever is watching has been looking at a
	// bare "running" for the length of the slowest of them.
	var advance func()
	if onProgress != nil {
		total := 0
		for _, item := range wanted {
			total += item.Objects
		}
		onProgress(0, total)

		var progressMu sync.Mutex
		done := 0
		advance = func() {
			progressMu.Lock()
			done++
			// Under the lock, so the counts leave in the order they were
			// taken — the accounts erase in parallel, and a bar that steps
			// backwards reads as a bug in whatever is drawing it.
			onProgress(done, total)
			progressMu.Unlock()
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	// The archives each account is now rid of, for the record of parts left
	// under the previous key: a sweep is one of the two ways such a part
	// stops being owed.
	gone := map[string]map[string]bool{}
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
			erased, objects, bytes, warnings := v.eraseOrphans(ctx, cfg, items, advance)

			mu.Lock()
			defer mu.Unlock()
			report.Archives += len(erased)
			report.Deleted += objects
			report.Bytes += bytes
			report.Warnings = append(report.Warnings, warnings...)
			if len(erased) > 0 {
				gone[cfg.ID] = erased
			}
		}(cfg, items)
	}
	wg.Wait()

	if len(gone) > 0 {
		if err := v.forgetStaleParts(func(p *StalePart) bool {
			return gone[p.ProviderID][p.ArchiveID]
		}); err != nil && !errors.Is(err, ErrLocked) {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"the record of parts left under the previous key could not be brought up to date: %v", err))
		}
	}

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
//
// advance, when given, is called once per object dealt with — erased, already
// gone, or failed and reported — which is what keeps a watcher's count moving
// against the total SweepOrphans announced.
//
// archives is the set of archive IDs at least one object was erased from —
// what the caller needs to bring the record of parts under the previous key
// up to date, as well as a count.
func (v *Vault) eraseOrphans(
	ctx context.Context,
	cfg provider.Config,
	items []OrphanArchive,
	advance func(),
) (archives map[string]bool, objects int, bytes int64, warnings []string) {
	p, err := v.buildProvider(cfg)
	if err != nil {
		return nil, 0, 0, []string{fmt.Sprintf("could not reach %s: %v", cfg.Name, err)}
	}
	listing, err := p.List(ctx, "")
	if err != nil {
		return nil, 0, 0, []string{fmt.Sprintf("could not list %s: %v", cfg.Name, err)}
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
			if advance != nil {
				advance()
			}
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

	return hit, objects, bytes, warnings
}

// ---------------------------------------------------------------------------
// Putting a mislaid shard back
// ---------------------------------------------------------------------------

// ReattachReport says what a reattach did.
type ReattachReport struct {
	// Shards is how many records were written back, Files how many files got
	// at least one, and Bytes what those shards weigh on the accounts.
	Shards int   `json:"shards"`
	Files  int   `json:"files"`
	Bytes  int64 `json:"bytes"`

	// Restored names the files that are no longer short of a shard, and
	// Improved the ones that gained one without reaching full spread.
	Restored []string `json:"restored,omitempty"`

	// Skipped names the shards that were refused between the scan and the
	// write, and why.
	Skipped []string `json:"skipped,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// ReattachOptions says how far a reattach looks and whether it writes.
type ReattachOptions struct {
	// ProviderID names one connected account to list. Empty lists every one,
	// which is what a reconnect calls for; one is what somebody with a long
	// list of short files and a good idea of where the parts are calls for.
	// An ID no connected account carries is ErrNoAccount.
	ProviderID string

	// DryRun asks what would be written back without the index being touched.
	DryRun bool
}

// ReattachShards writes back the index records for shards that are sitting on a
// connected account with nothing pointing at them.
//
// This is the repair for a disconnect. RemoveProvider drops the shard records
// naming the account it is removing, deliberately — an index that still claimed
// them would be lying about what is retrievable — and the objects themselves
// stay where they are, because SAND has no business deleting from an account it
// is being told to stop using. Connect that storage back and the two facts do
// not meet again on their own: the account arrives with a new ID, Reconcile
// re-points records rather than inventing them, and there is no record left to
// re-point. The file goes on reporting a missing spare part while the part sits
// on a connected cloud.
//
// So this is the pass that reunites them. It costs a listing per account and a
// single index write; not one byte is transferred, because a part's object key
// is derived from the archive ID and the shard number alone and the object is
// already exactly where the key says it should be.
//
// Purely additive: nothing is erased, no key is touched, and a file can only
// come out of it with more shards than it went in with. That is why it carries
// none of the sweep's refusals — orphanGuard is about not deleting something
// that turns out to be wanted, and there is no such risk in writing a record
// for bytes that are demonstrably there.
//
// What it does not do is verify the contents. Neither does Reconcile, and for
// the same reason: an object key is derived from a random 128-bit archive ID,
// so a name that matches is that archive's. A shard that is present but corrupt
// is what the health check is for, and it was equally corrupt before this ran.
//
// Scoped to one account by opts.ProviderID, it lists that account alone and
// records what it finds there; the other accounts' mislaid shards stay mislaid
// until they are asked. Nothing else changes: the verdict per shard comes from
// the index, and the write is the same write.
func (v *Vault) ReattachShards(ctx context.Context, opts ReattachOptions) (*ReattachReport, error) {
	scan, err := v.scanForOrphansOn(ctx, opts.ProviderID)
	if err != nil {
		return nil, err
	}

	report := &ReattachReport{Warnings: scan.Warnings}
	for _, stray := range scan.Strays {
		if !stray.Reattachable {
			report.Skipped = append(report.Skipped, fmt.Sprintf(
				"shard %d of %s: %s", stray.Part, strayName(stray), stray.Reason))
		}
	}

	wanted := make([]StrayShard, 0, scan.Reattachable)
	for _, stray := range scan.Strays {
		if stray.Reattachable {
			wanted = append(wanted, stray)
		}
	}
	if opts.DryRun || len(wanted) == 0 {
		files := map[string]bool{}
		for _, stray := range wanted {
			report.Shards++
			report.Bytes += stray.Bytes
			files[string(stray.Scope)+"\x00"+stray.FileID] = true
		}
		report.Files = len(files)
		sort.Strings(report.Skipped)
		return report, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	files := map[string]bool{}
	for _, stray := range wanted {
		entry, err := v.entryForReattachLocked(stray)
		if err != nil {
			report.Skipped = append(report.Skipped, fmt.Sprintf(
				"shard %d of %s: %s", stray.Part, strayName(stray), err))
			continue
		}
		cfg, connected := v.configForLocked(stray.ProviderID)
		if !connected {
			report.Skipped = append(report.Skipped, fmt.Sprintf(
				"shard %d of %s: its account was disconnected mid-repair", stray.Part, strayName(stray)))
			continue
		}

		key := ShardKey(stray.ArchiveID, stray.Part)
		if entry.ChunkCount > 0 {
			// One record still describes one shard of the whole file; the rest
			// of the chunks follow from ChunkShardKey. See putChunkParts.
			key = ChunkShardKey(stray.ArchiveID, 0, stray.Part)
		}
		entry.Shards = append(entry.Shards, Shard{
			Part:         stray.Part,
			ProviderID:   cfg.ID,
			ProviderName: cfg.Name,
			ProviderKind: string(cfg.Kind),
			Key:          key,
			Size:         stray.Bytes,
		})

		report.Shards++
		report.Bytes += stray.Bytes
		files[string(stray.Scope)+"\x00"+stray.FileID] = true
		if entry.Redundancy() >= entry.Scheme().Total && stray.Scope == MainScope {
			report.Restored = append(report.Restored, entry.Path())
		}
	}
	report.Files = len(files)

	if report.Shards > 0 {
		if err := v.persistLocked(); err != nil {
			return nil, err
		}
	}

	sort.Strings(report.Restored)
	sort.Strings(report.Skipped)
	sort.Strings(report.Warnings)
	return report, nil
}

// entryForReattachLocked finds the file a stray shard belongs to and re-checks,
// under the write lock this time, that it is still short of that shard.
//
// The scan ran without the lock and the vault does not stand still: an upload
// commits, a relocation moves a shard onto the very account this is about, a
// sub vault is shut. Each of those makes a row stale, and a stale row here
// would mean a duplicate record or a write into a vault that is no longer open.
//
// The caller must hold the write lock.
func (v *Vault) entryForReattachLocked(stray StrayShard) (*Entry, error) {
	m, err := v.manifestForLocked(stray.Scope)
	if err != nil {
		// A sub vault locked between the scan and the write. Its records are
		// its own again, and this cannot reach them.
		return nil, fmt.Errorf("its vault is no longer open")
	}
	for _, e := range m.Entries {
		if e.ID != stray.FileID {
			continue
		}
		if e.ArchiveID != stray.ArchiveID {
			return nil, fmt.Errorf("the file has been rewritten since the scan")
		}
		for _, sh := range e.Shards {
			if sh.Part == stray.Part {
				return nil, fmt.Errorf("the index has a record for it again")
			}
		}
		if total := e.Scheme().Total; stray.Part < 1 || stray.Part > total {
			return nil, fmt.Errorf("shard %d is not one a %s file has", stray.Part, e.Scheme())
		}
		return e, nil
	}
	return nil, fmt.Errorf("the file is no longer in the index")
}

// strayName is what to call a stray shard's file in a report. A sub vault's
// paths are not the main password's to print, so one is named by its archive
// instead — enough to tell two rows apart, and no more.
func strayName(stray StrayShard) string {
	if stray.Path != "" {
		return stray.Path
	}
	return "archive " + stray.ArchiveID
}
