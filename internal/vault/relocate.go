package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Moving a file to different clouds is a different operation from every other
// one in this package, and the difference is that it never needs a key.
//
// A part is an opaque blob whose object key is derived from the file's random
// archive ID and the part number alone (§5.5) — nothing in it depends on which
// account it happens to sit on. So changing the accounts a file lives on is a
// copy of those blobs from one account to another under the same name, and a
// rewrite of the index rows that say where they went. The vault has to be
// unlocked to know what to copy and to read the accounts' credentials, but no
// plaintext is ever rebuilt, no data key is touched, and the archive ID, the
// hash and the chunk layout come out the other side unchanged.
//
// That is what makes the second half of this cheap: if a file is on A, B and C
// and it is asked for A, B and D, only part 3 moves. The parts already on an
// account that is staying stay exactly where they are, byte for byte, and the
// index rows for them are not touched at all. Re-encrypting the file to get the
// same result — which is what MigrateFiles does for a key change — would mean
// gathering two parts, decompressing, re-encoding and pushing three, for a file
// that did not change.
//
// The order is copy, commit, erase. Between the copy and the commit both
// accounts hold the part and the index still names the old one, so a reader
// during the move sees a file that works; after the commit the index names an
// account that definitely has the bytes. An interruption leaves a copy nobody
// references rather than a file nobody can rebuild.

// relocateWindow is how many part objects are copied between accounts at once,
// across every part of one file.
//
// A chunked file is one object per chunk per part — a 4 GB video is 768 of them
// — so copying strictly in sequence would make moving a film between clouds a
// long wait on round-trip latency. It is small because each in-flight copy holds
// a whole part object in memory while it is in the air, and because the accounts
// on both ends of it are consumer cloud APIs.
const relocateWindow = 4

// relocationPreviewLimit caps how many per-file rows a plan hands back. The
// totals always count every file; this only bounds the detail, so a folder of
// ten thousand files answers "what would this do?" with a summary rather than a
// megabyte of JSON.
const relocationPreviewLimit = 200

// PartMove is one copy of one part of one file changing accounts.
type PartMove struct {
	Part     int    `json:"part"`
	From     string `json:"from"`
	FromName string `json:"from_name"`
	To       string `json:"to"`
	ToName   string `json:"to_name"`

	// Bytes is what this part occupies, and Objects how many stored objects it
	// is spread over: one for a file stored whole, one per chunk otherwise.
	Bytes   int64 `json:"bytes"`
	Objects int   `json:"objects"`
}

// FilePlan is what relocating one file comes to.
type FilePlan struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`

	// Moves are the shards that have to be copied to another account. Stay names
	// the ones already on an account that is being kept, which cost nothing at
	// all: their blobs are not read and their index rows are not rewritten.
	Moves []PartMove `json:"moves,omitempty"`
	Stay  []int      `json:"stay,omitempty"`

	// Drop names shards erased because the chosen accounts have no room for
	// them — three shards asked to live on two accounts under the strict policy,
	// where no account may hold two shards of the same file.
	Drop []int `json:"drop,omitempty"`

	// Stranded names shards that cannot be moved because the account holding
	// them is no longer connected, so there is nothing to copy from. They are
	// left recorded where they are rather than quietly dropped.
	Stranded []int `json:"stranded,omitempty"`

	// Recode is set when the chosen accounts imply a different erasure code
	// from the one the file is stored under — three clouds to six, say. A file
	// in that state cannot be moved shard by shard, because a 2-of-3 file and a
	// 4-of-6 file have no shards in common: it is gathered, cut again under
	// From → To, and written out whole. Moves and Drop are empty when this is
	// set, because neither describes what happens.
	Recode bool   `json:"recode,omitempty"`
	From   string `json:"from_scheme,omitempty"`
	To     string `json:"to_scheme,omitempty"`

	// Bytes is what a re-encode would move: the file, down and up again. It is
	// zero for a shard-by-shard move, where the per-move figures are the cost.
	Bytes int64 `json:"bytes,omitempty"`

	// archiveID is carried so the commit can check the file was not rewritten
	// underneath the move — by a password change's re-encryption, say.
	archiveID  string
	chunkCount int
}

// Changed reports whether this file would be touched at all.
func (p *FilePlan) Changed() bool { return p.Recode || len(p.Moves) > 0 || len(p.Drop) > 0 }

// RelocationPlan is what a relocation would do, worked out from the index alone
// — no account is contacted to produce it.
type RelocationPlan struct {
	// Path is what the relocation was pointed at, and Folder says whether that
	// turned out to be a folder rather than a single file.
	Path   string `json:"path"`
	Folder bool   `json:"folder"`

	// Accounts is the chosen destination set, in the order it was given.
	Accounts []string `json:"accounts"`

	Files     []FilePlan `json:"files"`
	Truncated bool       `json:"truncated,omitempty"`

	// Counted over every file, including any the Files list was truncated
	// before reaching.
	Total     int   `json:"total"`
	Unchanged int   `json:"unchanged"`
	Moves     int   `json:"moves"`
	Drops     int   `json:"drops"`
	Bytes     int64 `json:"bytes"`

	// Recoded is how many files have to be cut again because the chosen accounts
	// call for a different scheme, and RecodeBytes what that would move. They
	// are counted apart from Moves because they are a different operation and a
	// different order of cost: a moved shard is 1/k of a file copied between two
	// clouds, a re-encoded file is the whole of it down and back up again.
	Recoded     int   `json:"recoded"`
	RecodeBytes int64 `json:"recode_bytes"`

	// Outgoing and Incoming are the bytes leaving and arriving at each account,
	// by account ID — the answer to "how much is this going to cost me on
	// Dropbox?" before it starts.
	Outgoing map[string]int64 `json:"outgoing,omitempty"`
	Incoming map[string]int64 `json:"incoming,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// RelocationReport says what a relocation actually did.
type RelocationReport struct {
	Path     string   `json:"path"`
	Folder   bool     `json:"folder"`
	Accounts []string `json:"accounts"`

	// Total is how many files were in scope and Unchanged how many were already
	// on the chosen accounts. Relocated counts the ones that finished on them,
	// Partial the ones where some part could not be copied, and Failed the ones
	// nothing could be done with.
	Total      int `json:"total"`
	Unchanged  int `json:"unchanged"`
	Relocated  int `json:"relocated"`
	Partial    int `json:"partial"`
	Failed     int `json:"failed"`
	PartsMoved int `json:"parts_moved"`
	PartsDrop  int `json:"parts_dropped"`

	// Recoded is how many files were cut again under a different scheme rather
	// than having their shards carried across.
	Recoded int `json:"recoded"`

	// Bytes is how much was actually copied between accounts.
	Bytes int64 `json:"bytes"`

	Warnings []string `json:"warnings,omitempty"`
}

// Done reports whether every file in scope ended up on the chosen accounts.
func (r *RelocationReport) Done() bool {
	return r != nil && r.Partial == 0 && r.Failed == 0
}

// PlanRelocation answers what Relocate would do to a file or a folder, without
// contacting a single account.
//
// The whole answer comes out of the decrypted index: which parts are where,
// what each one weighs, and which of them the chosen accounts already hold. It
// is what lets the question "move this folder off Dropbox" be answered with
// "4 of 37 parts, 1.2 GB" before anything starts moving.
func (v *Vault) PlanRelocation(target string, accounts []string) (*RelocationPlan, error) {
	plan, err := v.planRelocation(target, accounts)
	if err != nil {
		return nil, err
	}
	if len(plan.Files) > relocationPreviewLimit {
		plan.Files = plan.Files[:relocationPreviewLimit]
		plan.Truncated = true
	}
	return plan, nil
}

// planRelocation is PlanRelocation with every file's row kept, which is what
// the relocation itself walks.
func (v *Vault) planRelocation(target string, accounts []string) (*RelocationPlan, error) {
	entries, dir, folder, err := v.relocationScope(target)
	if err != nil {
		return nil, err
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	policy := v.store.Policy
	byID := make(map[string]provider.Config, len(v.providers))
	for _, cfg := range v.providers {
		byID[cfg.ID] = cfg
	}
	v.mu.RUnlock()

	targets, err := resolveAccounts(accounts, byID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("choose the accounts to move onto — naming none would leave nowhere to put the parts")
	}
	capacity, err := relocationCapacity(policy, len(targets))
	if err != nil {
		return nil, err
	}
	// How many accounts were chosen settles the code, and whether that matches
	// what each file is already cut with settles whether it moves or is rebuilt.
	want, err := SchemeFor(len(targets))
	if err != nil {
		return nil, err
	}

	plan := &RelocationPlan{
		Path:     dir,
		Folder:   folder,
		Accounts: targets,
		Files:    make([]FilePlan, 0, len(entries)),
		Total:    len(entries),
		Outgoing: map[string]int64{},
		Incoming: map[string]int64{},
	}

	for _, entry := range entries {
		fp := planFileRelocation(entry, targets, byID, capacity, want)
		if !fp.Changed() {
			plan.Unchanged++
		}
		if fp.Recode {
			plan.Recoded++
			plan.RecodeBytes += fp.Bytes
		}
		plan.Moves += len(fp.Moves)
		plan.Drops += len(fp.Drop)
		for _, m := range fp.Moves {
			plan.Bytes += m.Bytes
			plan.Outgoing[m.From] += m.Bytes
			plan.Incoming[m.To] += m.Bytes
		}
		for _, part := range fp.Stranded {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s: shard %d stays where it is — the account holding it is no longer connected, "+
					"so there is nothing to copy from", fp.Path, part))
		}
		if len(fp.Drop) > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s: shard %s will be erased — %d accounts have no room for %d shards under the %s policy",
				fp.Path, joinParts(fp.Drop), len(targets), len(entry.Shards), policy))
		}
		if fp.Recode {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s is stored as %s and %d clouds is %s, so it has to be rebuilt rather than moved — "+
					"the whole file comes down and goes back up",
				fp.Path, fp.From, len(targets), fp.To))
		}
		plan.Files = append(plan.Files, fp)
	}

	return plan, nil
}

// Relocate moves a file, or everything under a folder, onto a chosen set of
// cloud accounts.
//
// Only what has to move moves. A part already on one of the chosen accounts is
// left exactly where it is, and what does move is copied across as the encrypted
// blob it already is — see the note at the top of this file. Nothing is
// decrypted, and the file keeps its archive ID, its hash and its key generation.
//
// Each file is committed on its own, so this is safe to interrupt and safe to
// repeat: running it again moves whatever is still not where it was asked to be.
// A file whose account is offline is reported and left alone rather than holding
// up the rest. progress may be nil.
func (v *Vault) Relocate(ctx context.Context, target string, accounts []string, progress ProgressFunc) (*RelocationReport, error) {
	plan, err := v.planRelocation(target, accounts)
	if err != nil {
		return nil, err
	}

	report := &RelocationReport{
		Path:      plan.Path,
		Folder:    plan.Folder,
		Accounts:  plan.Accounts,
		Total:     plan.Total,
		Unchanged: plan.Unchanged,
		Warnings:  plan.Warnings,
	}

	for i := range plan.Files {
		fp := &plan.Files[i]
		if err := ctx.Err(); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"stopped after %d of %d file(s): %v", i, len(plan.Files), err))
			break
		}
		if !fp.Changed() {
			if progress != nil {
				progress(fp.Path, i+1, len(plan.Files))
			}
			continue
		}

		outcome, err := v.relocateEntry(ctx, fp, plan.Accounts)
		report.Warnings = append(report.Warnings, outcome.warnings...)
		report.PartsMoved += outcome.moved
		report.PartsDrop += outcome.dropped
		report.Bytes += outcome.bytes
		if err == nil && fp.Recode {
			report.Recoded++
		}

		switch {
		case err != nil:
			report.Warnings = append(report.Warnings, err.Error())
			report.Failed++
			if errors.Is(err, ErrLocked) {
				// Nothing further can be read or written, so stop rather than
				// pile up the same failure once per remaining file.
				report.Warnings = append(report.Warnings,
					"the vault was locked before the move finished")
				return report, ErrLocked
			}
		case !fp.Recode && outcome.moved < len(fp.Moves):
			report.Partial++
		default:
			report.Relocated++
		}

		if progress != nil {
			progress(fp.Path, i+1, len(plan.Files))
		}
	}

	// A folder's thumbnails are stored as their own scattered pack, filed under
	// the folder rather than under any of the files. Moving the files off an
	// account and leaving the pictures of them behind would not be the move
	// that was asked for.
	if plan.Folder {
		report.Warnings = append(report.Warnings, v.relocateThumbPacks(ctx, plan)...)
	}
	return report, nil
}

// relocationCapacity is how many parts of one file a single chosen account may
// end up holding. It is the placement policy (§5) applied to a set of accounts
// somebody named, and it deliberately answers exactly what an upload to the same
// accounts would: a relocation must not be able to produce a placement that
// could not have been uploaded.
//
// Widening the spread does not widen this. Six accounts under strict still hold
// one part each — the second group of three holds the second copy of each part,
// not a second part on the same account.
func relocationCapacity(policy Policy, targets int) (int, error) {
	switch policy {
	case PolicyStrict:
		if targets < archive.MinPartsToRestore {
			return 0, fmt.Errorf(
				"strict placement needs at least %d accounts so that no single account holds "+
					"enough parts to reconstruct a file (chose %d) — choose another account, or "+
					"switch this vault to the redundant policy",
				archive.MinPartsToRestore, targets)
		}
		return 1, nil
	case PolicyRedundant:
		return archive.PartCount, nil
	}
	return 0, fmt.Errorf("unknown placement policy %q", policy)
}

// planFileRelocation decides where each of one file's shards should end up.
//
// There are two shapes of answer, and which applies is settled by the accounts
// chosen rather than by anything about the file.
//
// If they call for the code the file already has, this is a *move*: the rule is
// "keep what is already right". A shard sitting on an account that is being kept
// stays there — it is not read, not rewritten, and not counted — and only what
// is left over is assigned to whatever room the chosen accounts still have. That
// is what makes changing one cloud out of three cost one shard rather than a
// whole file, and it is also why the answer does not depend on the order the
// shards happen to be recorded in.
//
// If they call for a different code — three clouds to six, or six back to three
// — no shard of the old file is a shard of the new one, so there is nothing to
// keep and nothing to move. The file is gathered, cut again and written out.
// That is expensive and is reported as its own thing rather than hidden among
// the moves, because a person deciding whether to widen a vault should see the
// bill before agreeing to it.
func planFileRelocation(entry *Entry, targets []string, byID map[string]provider.Config, capacity int, want archive.Scheme) FilePlan {
	plan := FilePlan{
		ID:         entry.ID,
		Path:       entry.Path(),
		Size:       entry.Size,
		archiveID:  entry.ArchiveID,
		chunkCount: entry.ChunkCount,
	}

	if have := entry.Scheme(); have != want {
		plan.Recode = true
		plan.From = have.String()
		plan.To = want.String()
		// Down once and up once: the shards that rebuild it, then the whole new
		// set. Both are about the file's own size, the second by the scheme's
		// ratio.
		plan.Bytes = entry.Size + entry.Size*int64(want.Total)/int64(want.Data)
		return plan
	}

	shards := append([]Shard(nil), entry.Shards...)
	sortShards(shards)

	free := make(map[string]int, len(targets))
	for _, id := range targets {
		free[id] = capacity
	}

	// First pass: anything already in the right place claims its account, so a
	// later shard cannot be handed the room it is standing in.
	settled := make(map[int]bool, len(shards))
	for _, s := range shards {
		if free[s.ProviderID] > 0 {
			free[s.ProviderID]--
			settled[s.Part] = true
			plan.Stay = append(plan.Stay, s.Part)
		}
	}

	// Second pass: everything else goes to whatever room is left, in the order
	// the accounts were named.
	for _, s := range shards {
		if settled[s.Part] {
			continue
		}
		if _, connected := byID[s.ProviderID]; !connected {
			plan.Stranded = append(plan.Stranded, s.Part)
			continue
		}

		dest := ""
		for _, id := range targets {
			if free[id] > 0 {
				dest = id
				break
			}
		}
		if dest == "" {
			plan.Drop = append(plan.Drop, s.Part)
			continue
		}
		free[dest]--

		objects := 1
		if entry.Chunked() {
			objects = entry.ChunkCount
		}
		plan.Moves = append(plan.Moves, PartMove{
			Part:     s.Part,
			From:     s.ProviderID,
			FromName: s.ProviderName,
			To:       dest,
			ToName:   byID[dest].Name,
			Bytes:    s.Size,
			Objects:  objects,
		})
	}

	return plan
}

// relocationScope resolves what a relocation was pointed at — an entry ID, a
// file path, or a folder to walk — into the entries it covers.
//
// The entries are copies. Everything after this runs without the lock, and a
// pointer into the live index would be rewritten underneath it by any other
// upload.
func (v *Vault) relocationScope(target string) ([]*Entry, string, bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.dataKey == nil {
		return nil, "", false, ErrLocked
	}

	if e := v.manifest.ByID(target); e != nil {
		return []*Entry{copyEntry(e)}, e.Path(), false, nil
	}

	dir := CleanDir(target)
	if v.manifest.FolderExists(dir) {
		found := v.manifest.Descendants(dir)
		out := make([]*Entry, 0, len(found))
		for _, e := range found {
			out = append(out, copyEntry(e))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Path() < out[j].Path() })
		return out, dir, true, nil
	}

	if e := v.manifest.ByPath(dir); e != nil {
		return []*Entry{copyEntry(e)}, e.Path(), false, nil
	}
	return nil, "", false, fmt.Errorf("no such file or folder: %s", target)
}

// copyEntry takes a detached copy of an index row, shards and all.
func copyEntry(e *Entry) *Entry {
	out := *e
	out.Shards = append([]Shard(nil), e.Shards...)
	return &out
}

// relocateOutcome is what carrying out one file's plan came to.
type relocateOutcome struct {
	moved    int
	dropped  int
	bytes    int64
	warnings []string
}

// relocateEntry copies one file's parts to their new accounts, commits the new
// placement, and erases what it left behind.
//
// A part that will not copy is reported and left where it was: the file is
// still whole, just not yet all in the right place, and running the relocation
// again picks up exactly that part. Only parts that really landed are recorded.
func (v *Vault) relocateEntry(ctx context.Context, plan *FilePlan, accounts []string) (relocateOutcome, error) {
	var out relocateOutcome

	if plan.Recode {
		return v.recodeEntry(ctx, plan, accounts)
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return out, ErrLocked
	}
	entry := v.manifest.ByID(plan.ID)
	if entry == nil {
		// Deleted since the plan was drawn; nothing to move.
		v.mu.RUnlock()
		return out, nil
	}
	if entry.ArchiveID != plan.archiveID {
		v.mu.RUnlock()
		out.warnings = append(out.warnings, fmt.Sprintf(
			"%s was rewritten while it was being moved, so it was left alone — move it again",
			plan.Path))
		return out, nil
	}
	archiveID, chunkCount := entry.ArchiveID, entry.ChunkCount
	configs := v.configsForLocked(entry.Shards)
	for _, m := range plan.Moves {
		if cfg, ok := v.configForLocked(m.To); ok {
			configs[m.To] = cfg
		}
	}
	v.mu.RUnlock()

	landed := v.copyParts(ctx, plan, archiveID, chunkCount, configs, &out)
	if len(landed) == 0 && len(plan.Drop) == 0 {
		return out, nil
	}

	stale, err := v.commitRelocation(ctx, plan, landed, &out)
	if err != nil {
		// The index still points at the parts where they were, so the copies
		// just made are referenced by nothing.
		for _, move := range landed {
			if cfg, ok := configs[move.To]; ok {
				v.erasePartCopy(context.WithoutCancel(ctx), cfg, archiveID, chunkCount, move.Part)
			}
		}
		out.moved -= len(landed)
		return out, err
	}

	// The index names the new accounts now, so what is left behind is
	// unreferenced. Failing to erase it is litter, not breakage.
	for _, w := range v.deleteStoredShards(context.WithoutCancel(ctx), archiveID, chunkCount, stale) {
		out.warnings = append(out.warnings, fmt.Sprintf("%s: %s", plan.Path, w))
	}
	return out, nil
}

// recodeEntry rebuilds one file under the scheme the chosen accounts call for.
//
// It is the expensive branch, and the one that cannot be avoided: a 2-of-3
// file's shards are halves of the compressed stream and a 4-of-6 file's are
// quarters, so there is no blob to carry across and no index rewrite that would
// do. The file comes down, is cut again, and goes back up — which is exactly
// what migrateFile does for a password change, pointed at different accounts.
//
// The ordering guarantees are migrateFile's, and they are the same ones the
// copy path gives: the index moves to the new shards in one write, the old ones
// are erased only after it, and an interruption leaves unreferenced objects
// rather than an unreadable file.
func (v *Vault) recodeEntry(ctx context.Context, plan *FilePlan, accounts []string) (relocateOutcome, error) {
	var out relocateOutcome

	_, _, warnings, err := v.migrateFile(ctx, plan.ID, accounts)
	out.warnings = append(out.warnings, warnings...)
	if err != nil {
		return out, err
	}

	// Every shard of the file was written, so the whole of it counts as moved
	// and the bytes are what the plan estimated rather than what any single
	// copy reported.
	v.mu.RLock()
	if entry := v.manifest.ByID(plan.ID); entry != nil {
		out.moved = len(entry.Shards)
	}
	v.mu.RUnlock()
	out.bytes = plan.Bytes
	return out, nil
}

// copyParts copies each moving part's objects to its new account, and returns
// the moves that really landed.
//
// Parts are copied concurrently but share one window, because the window is
// there to bound how many part objects are held in memory at once rather than
// to fan out as widely as the accounts will tolerate.
func (v *Vault) copyParts(
	ctx context.Context,
	plan *FilePlan,
	archiveID string,
	chunkCount int,
	configs map[string]provider.Config,
	out *relocateOutcome,
) []PartMove {
	window := make(chan struct{}, relocateWindow)

	var mu sync.Mutex
	var wg sync.WaitGroup
	var warnings []string
	landed := make([]PartMove, 0, len(plan.Moves))

	for _, move := range plan.Moves {
		src, haveSrc := configs[move.From]
		dst, haveDst := configs[move.To]
		if !haveSrc || !haveDst {
			name := move.FromName
			if haveSrc {
				name = move.ToName
			}
			warnings = append(warnings, fmt.Sprintf(
				"%s: shard %d was not moved — %s is no longer connected", plan.Path, move.Part, name))
			continue
		}

		wg.Add(1)
		go func(move PartMove, src, dst provider.Config) {
			defer wg.Done()

			bytes, err := v.copyPart(ctx, src, dst, archiveID, chunkCount, move.Part, window)
			if err != nil {
				// Half a part on the destination is worse than none: it would
				// answer a read with a hole. The record still points at the
				// source, which still has all of it.
				v.erasePartCopy(context.WithoutCancel(ctx), dst, archiveID, chunkCount, move.Part)
			}

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				warnings = append(warnings, fmt.Sprintf(
					"%s: shard %d could not be moved from %s to %s: %v",
					plan.Path, move.Part, src.Name, dst.Name, err))
				return
			}
			move.Bytes = bytes
			landed = append(landed, move)
			out.bytes += bytes
		}(move, src, dst)
	}
	wg.Wait()

	sort.Slice(landed, func(i, j int) bool { return landed[i].Part < landed[j].Part })
	sort.Strings(warnings)
	out.warnings = append(out.warnings, warnings...)
	out.moved += len(landed)
	return landed
}

// copyPart copies one part of one file from one account to another.
//
// The object keys are identical on both ends — they are derived from the
// archive ID and the part number and nothing else (§5.5) — so this is a read and
// a write of the same encrypted bytes under the same name, with no decryption
// and no re-encoding anywhere in it.
func (v *Vault) copyPart(
	ctx context.Context,
	src, dst provider.Config,
	archiveID string,
	chunkCount, part int,
	window chan struct{},
) (int64, error) {
	from, err := v.buildProvider(src)
	if err != nil {
		return 0, err
	}
	to, err := v.buildProvider(dst)
	if err != nil {
		return 0, err
	}

	copyOne := func(ctx context.Context, key string) (int64, error) {
		blob, err := from.Get(ctx, key)
		if err != nil {
			return 0, err
		}
		if err := to.Put(ctx, key, blob); err != nil {
			return 0, err
		}
		return int64(len(blob)), nil
	}

	if chunkCount <= 0 {
		window <- struct{}{}
		defer func() { <-window }()
		return copyOne(ctx, ShardKey(archiveID, part))
	}

	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	var total int64
	var firstErr error
	var wg sync.WaitGroup

	for index := 0; index < chunkCount; index++ {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}

		wg.Add(1)
		window <- struct{}{}
		go func(index int) {
			defer wg.Done()
			defer func() { <-window }()

			n, err := copyOne(copyCtx, ChunkShardKey(archiveID, index, part))

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("chunk %d: %w", index, err)
					// The rest of this part is pointless now: it is erased
					// either way, and the caller only records a part that
					// copied whole.
					cancel()
				}
				return
			}
			total += n
		}(index)
	}
	wg.Wait()

	if firstErr != nil {
		return 0, firstErr
	}
	return total, nil
}

// commitRelocation writes the new placement into the index in one atomic write,
// and returns the shards that are now unreferenced and can be erased.
func (v *Vault) commitRelocation(ctx context.Context, plan *FilePlan, landed []PartMove, out *relocateOutcome) ([]Shard, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return nil, ErrLocked
	}
	entry := v.manifest.ByID(plan.ID)
	if entry == nil {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"%s was deleted while it was being moved", plan.Path))
		return nil, nil
	}
	if entry.ArchiveID != plan.archiveID {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"%s was rewritten while it was being moved, so the copies made for it were discarded",
			plan.Path))
		return nil, nil
	}

	byPart := make(map[int]PartMove, len(landed))
	for _, m := range landed {
		byPart[m.Part] = m
	}
	drop := make(map[int]bool, len(plan.Drop))
	for _, part := range plan.Drop {
		drop[part] = true
	}

	// Erasing a shard must never take a file below what it takes to rebuild it.
	// The plan cannot ask for that on its own, but a move that failed can leave
	// the file with fewer shards in play than the plan assumed.
	remaining := 0
	for _, s := range entry.Shards {
		if !drop[s.Part] {
			remaining++
		}
	}
	if remaining < entry.Scheme().Data {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"%s: kept shard %s rather than erasing it — dropping it would have left too few to rebuild the file",
			plan.Path, joinParts(plan.Drop)))
		drop = nil
	}

	previous := append([]Shard(nil), entry.Shards...)
	var stale []Shard
	dropped := 0
	kept := make([]Shard, 0, len(previous))
	for _, s := range previous {
		if drop[s.Part] {
			stale = append(stale, s)
			dropped++
			continue
		}
		if move, moved := byPart[s.Part]; moved {
			stale = append(stale, s)
			// The object key does not change: it never named the account, only
			// the file and the shard.
			s.ProviderID = move.To
			s.ProviderName = move.ToName
			if cfg, ok := v.configForLocked(move.To); ok {
				s.ProviderKind = string(cfg.Kind)
			}
		}
		kept = append(kept, s)
	}
	sortShards(kept)
	entry.Shards = kept

	// The file did not change, only where its parts sit, so ModifiedAt is left
	// alone the same way a re-encryption leaves it.
	if err := v.persistLocked(); err != nil {
		entry.Shards = previous
		return nil, fmt.Errorf("recording where %s moved to: %w", plan.Path, err)
	}

	out.dropped += dropped
	return stale, nil
}

// erasePartCopy removes every object one part of a file occupies on one
// account. An object that was never written answers not-found, which is the
// outcome wanted anyway.
func (v *Vault) erasePartCopy(ctx context.Context, cfg provider.Config, archiveID string, chunkCount, part int) {
	p, err := v.buildProvider(cfg)
	if err != nil {
		return
	}
	_ = deleteShardObjects(ctx, p, archiveID, chunkCount, Shard{
		Part: part,
		Key:  ShardKey(archiveID, part),
	})
}

// relocateThumbPacks moves the thumbnail packs of the folders a relocation
// covered.
//
// A pack belongs to a folder rather than to any of the files in it, so it does
// not follow their placement and would otherwise be left sitting on the accounts
// the folder was just moved off. It is not moved the way a file is: a pack is
// small, already gathered on any folder that has been opened, and re-scattering
// it is the operation that already exists (savePackOn). Paying a rebuild for the
// pictures is worth not having a second copy of the placement machinery.
//
// It runs after the files, and every failure is a warning: a thumbnail that will
// not move is a picture the browser can draw again, not a file at risk.
func (v *Vault) relocateThumbPacks(ctx context.Context, plan *RelocationPlan) []string {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return []string{"the vault was locked before the folder's thumbnails could be moved"}
	}
	wanted := make(map[string]bool, len(plan.Accounts))
	for _, id := range plan.Accounts {
		wanted[id] = true
	}
	prefix := plan.Path
	if prefix != "/" {
		prefix += "/"
	}

	var dirs []string
	for dir, pack := range v.manifest.Thumbs {
		if dir != plan.Path && !strings.HasPrefix(dir, prefix) {
			continue
		}
		// Already entirely on the chosen accounts: nothing to do, and asking
		// would cost a gather and a scatter to find that out. A pack is two of
		// three wherever it lives, so its width never has to change — only
		// which accounts it sits on.
		settled := true
		for _, s := range pack.Shards {
			if !wanted[s.ProviderID] {
				settled = false
				break
			}
		}
		if !settled {
			dirs = append(dirs, dir)
		}
	}
	v.mu.RUnlock()

	sort.Strings(dirs)

	var warnings []string
	for _, dir := range dirs {
		items, err := v.loadPack(ctx, dir)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"the thumbnails for %s stayed where they were: %v", dir, err))
			continue
		}
		if len(items) == 0 {
			continue
		}
		if err := v.savePackOn(ctx, dir, items, plan.Accounts); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"the thumbnails for %s stayed where they were: %v", dir, err))
		}
	}
	return warnings
}

// joinParts renders a set of shard numbers for a sentence: "3", or "2 and 3".
func joinParts(parts []int) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return fmt.Sprint(parts[0])
	}
	labels := make([]string, 0, len(parts))
	for _, p := range parts {
		labels = append(labels, fmt.Sprint(p))
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}

// packCopies is gone with the copies it counted; a pack is two of three
// wherever it lives.
