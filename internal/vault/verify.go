package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/filecheck"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Finding the files that do not come back right, and putting right the ones
// that can be.
//
// Everything else that asks after a file's health asks whether its parts are
// *there* — Health and the folder automations stat a sample of objects and
// download nothing. That answers "has a cloud lost something" cheaply and says
// nothing about whether what is there is right: an object can be present,
// the right size, and not the bytes that were written. Nothing found that out
// until somebody opened the file.
//
// This is the expensive, complete answer. For each file it fetches every
// shard of every chunk from every account — all n, not the k a read stops at —
// and holds each to four things in turn:
//
//  1. Each shard, alone: is the object there, and does it open under its seal
//     as the part of the chunk of the archive the index sent for.
//  2. The shards, together: rebuilt from k of them, do the rest agree. A shard
//     that opens but was cut from different bytes is caught here and nowhere
//     else, because a read never looks at more than k.
//  3. The file: does it rebuild to the length and the SHA-256 recorded when it
//     was stored.
//  4. The contents: are those bytes a well-formed file of their kind
//     (internal/filecheck). This is the one check that is not about SAND at
//     all — a photograph that was already cut short when it was uploaded
//     passes the first three perfectly.
//
// Which of those fails decides what can be done, and that is the point of
// keeping them apart:
//
//   - Shards wrong but the file rebuilds and matches its hash: the code did its
//     job. The file is rebuilt onto its clouds, which writes a full fresh set
//     and erases the bad ones (CheckRebuild).
//   - Too few good shards, or a rebuild that does not match: the parts cannot
//     give the file back. If it has a replica, new parts are cut from that
//     (CheckFromReplica); if not, nothing here can, and the report says which
//     clouds are holding what.
//   - Hash right, contents wrong: SAND is returning exactly what it was given.
//     No repair exists inside the vault; the original has to be found and
//     uploaded again.
//
// A check reads and never writes. Repair is a second, separate call that
// names the files to act on.

// CheckState is what a deep check concluded about one file.
type CheckState string

const (
	// CheckSound: every shard is right, the file matches its hash, and its
	// contents are well-formed or of no kind that can be judged.
	CheckSound CheckState = "sound"

	// CheckRepairable: the file rebuilds and matches its hash, but one or
	// more of its shards are missing, damaged or disagree with the rest. It
	// reads today on its spares.
	CheckRepairable CheckState = "repairable"

	// CheckUnreadable: some chunk has fewer good shards than the code needs.
	CheckUnreadable CheckState = "unreadable"

	// CheckAltered: it rebuilds, but not to the bytes the index recorded.
	CheckAltered CheckState = "altered"

	// CheckDamagedAtSource: stored faithfully — the hash matches — and the
	// contents are a broken file of their kind. It arrived that way.
	CheckDamagedAtSource CheckState = "damaged_at_source"

	// CheckIncomplete: nothing wrong was found, but some of its shards could
	// not be looked at — an account that did not answer.
	CheckIncomplete CheckState = "incomplete"

	// CheckSkipped: not checked at all, and Summary says why.
	CheckSkipped CheckState = "skipped"
)

// CheckFix names the repair a checked file can be given.
type CheckFix string

const (
	// CheckRebuild gathers the file from its good shards and writes a full
	// fresh set onto its clouds.
	CheckRebuild CheckFix = "rebuild"
	// CheckFromReplica cuts new parts from the file's replica.
	CheckFromReplica CheckFix = "replica"
)

// Shard fault kinds.
const (
	FaultMissing     = "missing"     // the account says the object is not there
	FaultDamaged     = "damaged"     // there, and not what was written
	FaultMismatched  = "mismatched"  // opens, and disagrees with the rebuild
	FaultUnreachable = "unreachable" // could not be asked
)

// ShardFault is what is wrong with one shard of one file, across its chunks.
type ShardFault struct {
	Part         int    `json:"part"`
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderKind string `json:"provider_kind,omitempty"`

	// Kind is one of the Fault constants: the worst thing found on any chunk.
	Kind string `json:"kind"`

	// Chunks is how many of the file's chunks this shard is wrong on, and
	// FirstChunk the first of them. Detail is what the first one said.
	Chunks     int    `json:"chunks"`
	FirstChunk int    `json:"first_chunk"`
	Detail     string `json:"detail,omitempty"`
}

// FileCheck is one file as a deep check found it.
type FileCheck struct {
	ID    string `json:"id"`
	Vault Scope  `json:"vault,omitempty"`
	Path  string `json:"path"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Kind  string `json:"kind"`

	State CheckState `json:"state"`

	// Summary is the finding in one sentence, and Advice what to do about it.
	Summary string `json:"summary"`
	Advice  string `json:"advice,omitempty"`

	// Scheme is the code the file is cut with; Good how many of its shards
	// were right on every chunk.
	Scheme string `json:"scheme"`
	Needed int    `json:"needed"`
	Total  int    `json:"total"`
	Good   int    `json:"good"`

	Faults []ShardFault `json:"faults,omitempty"`

	// Rebuilt says the file came back from its parts at all, and HashOK that
	// what came back is what was stored.
	Rebuilt bool `json:"rebuilt"`
	HashOK  bool `json:"hash_ok"`

	// Content is what the bytes look like as a file of their kind. Nil when
	// the file did not rebuild.
	Content *filecheck.Verdict `json:"content,omitempty"`

	// HasReplica says a readable replica is on record; Fix what repair is on
	// offer, empty for none.
	HasReplica bool     `json:"has_replica,omitempty"`
	Fix        CheckFix `json:"fix,omitempty"`

	archiveID string
}

// absentParts is the parts a rebuild must not count on.
func (c *FileCheck) absentParts() map[int]bool {
	out := map[int]bool{}
	for _, f := range c.Faults {
		if f.Kind != FaultUnreachable {
			out[f.Part] = true
		}
	}
	return out
}

// CheckKinds are the kinds a check can be narrowed to — kindOf's words.
var CheckKinds = []string{"images", "documents", "video", "audio", "archives", "other"}

// CheckOptions narrows and observes a check.
type CheckOptions struct {
	// Kinds limits the check to files of these kinds. Empty is every file.
	Kinds []string

	// OnProgress hears where the check has got to. May be nil.
	OnProgress func(CheckProgress)
}

// CheckProgress is where a running check has got to.
type CheckProgress struct {
	Path  string `json:"path"`
	File  int    `json:"file"`
	Files int    `json:"files"`

	// Bytes is how much plaintext has been rebuilt and checked, of Total.
	Bytes int64 `json:"bytes"`
	Total int64 `json:"total"`

	// Flagged is how many files so far are anything but sound.
	Flagged int `json:"flagged"`
}

// checkReportLimit caps how many flagged files a report lists. The counts
// always cover every file.
const checkReportLimit = 1000

// CheckReport is what a deep check of a folder found.
type CheckReport struct {
	Vault Scope    `json:"vault,omitempty"`
	Path  string   `json:"path"`
	Kinds []string `json:"kinds,omitempty"`

	// Files is how many were in scope and Checked how many were got to.
	Files   int   `json:"files"`
	Checked int   `json:"checked"`
	Bytes   int64 `json:"bytes"`

	Sound           int `json:"sound"`
	Repairable      int `json:"repairable"`
	Unreadable      int `json:"unreadable"`
	Altered         int `json:"altered"`
	DamagedAtSource int `json:"damaged_at_source"`
	Incomplete      int `json:"incomplete"`
	Skipped         int `json:"skipped"`

	// Fixable is how many flagged files have a repair on offer.
	Fixable int `json:"fixable"`

	// Flagged lists every file that is not sound, worst first. Truncated says
	// the list was cut at checkReportLimit.
	Flagged   []FileCheck `json:"flagged"`
	Truncated bool        `json:"truncated,omitempty"`

	// Stopped says the check was cut short, and everything above describes
	// only the files it reached. StoppedBy says what cut it short when that
	// was not somebody asking — see scopeShut.
	Stopped   bool   `json:"stopped,omitempty"`
	StoppedBy string `json:"stopped_by,omitempty"`

	// Offline names accounts that did not answer when the check began. Their
	// shards were not asked for.
	Offline []string `json:"offline,omitempty"`
}

// CheckSurvey counts what a check of a folder would read, by kind, from the
// index alone.
type CheckSurvey struct {
	Path  string            `json:"path"`
	Kinds []CheckSurveyKind `json:"kinds"`
	Files int               `json:"files"`
	Bytes int64             `json:"bytes"`
}

// CheckSurveyKind is one kind's share of a survey.
type CheckSurveyKind struct {
	Kind  string `json:"kind"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// SurveyCheck says how many files of each kind are at or below dir, and how
// much they weigh — the bill for a deep check, before it is run.
func (v *Vault) SurveyCheck(scope Scope, dir string) (*CheckSurvey, error) {
	entries, err := v.Descendants(scope, dir)
	if err != nil {
		return nil, err
	}
	by := map[string]*CheckSurveyKind{}
	out := &CheckSurvey{Path: CleanDir(dir)}
	for _, e := range entries {
		kind := kindOf(e)
		if by[kind] == nil {
			by[kind] = &CheckSurveyKind{Kind: kind}
		}
		by[kind].Files++
		by[kind].Bytes += e.Size
		out.Files++
		out.Bytes += e.Size
	}
	for _, kind := range CheckKinds {
		if k := by[kind]; k != nil {
			out.Kinds = append(out.Kinds, *k)
		}
	}
	return out, nil
}

// CheckFolder deep-checks every file at or below dir. It reads and never
// writes.
//
// A cancelled context ends it early with what it had found, marked Stopped,
// rather than with an error: a check of a large folder is hours of downloads
// and the half that was done is still true.
func (v *Vault) CheckFolder(ctx context.Context, scope Scope, dir string, opts CheckOptions) (*CheckReport, error) {
	found, err := v.Descendants(scope, dir)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, kind := range opts.Kinds {
		want[kind] = true
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	entries := make([]*Entry, 0, len(found))
	var total int64
	for _, e := range found {
		if len(want) > 0 && !want[kindOf(e)] {
			continue
		}
		entries = append(entries, copyEntry(e))
		total += e.Size
	}
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path() < entries[j].Path() })

	report := &CheckReport{
		Vault: scope, Path: CleanDir(dir), Kinds: opts.Kinds,
		Files: len(entries), Flagged: []FileCheck{},
	}

	// Asked once for the folder, as a sweep does: a shard on an account that
	// is down is known to be out of reach without waiting out a timeout per
	// object to learn it again.
	online, offline := v.probeAccounts(ctx, configs)
	for _, cfg := range configs {
		if _, dark := offline[cfg.ID]; dark {
			report.Offline = append(report.Offline, cfg.Name)
		}
	}

	var done int64
	for i, entry := range entries {
		if ctx.Err() != nil {
			report.Stopped = true
			break
		}
		say := func(within int64) {
			if opts.OnProgress != nil {
				opts.OnProgress(CheckProgress{
					Path: entry.Path(), File: i + 1, Files: len(entries),
					Bytes: done + within, Total: total, Flagged: report.flagged(),
				})
			}
		}
		if why := v.scopeShut(scope); why != "" {
			report.Stopped, report.StoppedBy = true, why
			break
		}
		say(0)
		check, err := v.checkEntry(ctx, scope, entry, online, say)
		// A file "skipped" because its vault shut as it was reached is not a
		// finding about the file.
		if why := v.scopeShut(scope); why != "" && (err != nil || check.State == CheckSkipped) {
			report.Stopped, report.StoppedBy = true, why
			break
		}
		if err != nil {
			if errors.Is(err, ErrLocked) {
				return nil, err
			}
			// The context went mid-file: what was learned about this one is
			// half an answer, and is not reported as one.
			report.Stopped = true
			break
		}
		done += entry.Size
		report.add(check)
	}

	sort.SliceStable(report.Flagged, func(i, j int) bool {
		if a, b := checkRank(report.Flagged[i].State), checkRank(report.Flagged[j].State); a != b {
			return a < b
		}
		return report.Flagged[i].Path < report.Flagged[j].Path
	})
	if len(report.Flagged) > checkReportLimit {
		report.Flagged = report.Flagged[:checkReportLimit]
		report.Truncated = true
	}
	return report, nil
}

// CheckFile deep-checks one file by ID.
func (v *Vault) CheckFile(ctx context.Context, id string) (*FileCheck, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	scope, found, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	entry := copyEntry(found)
	configs := v.configsAtLocked(provider.Parts, entry.Shards)
	v.mu.RUnlock()

	// One file's accounts are simply asked: a probe of every cloud in the
	// vault first would cost more than the answer.
	online := map[string]bool{}
	for id := range configs {
		online[id] = true
	}
	check, err := v.checkEntry(ctx, scope, entry, online, nil)
	if err != nil {
		return nil, err
	}
	return &check, nil
}

func checkRank(s CheckState) int {
	switch s {
	case CheckUnreadable:
		return 0
	case CheckAltered:
		return 1
	case CheckRepairable:
		return 2
	case CheckDamagedAtSource:
		return 3
	case CheckIncomplete:
		return 4
	case CheckSkipped:
		return 5
	}
	return 6
}

func (r *CheckReport) flagged() int { return r.Checked - r.Sound }

func (r *CheckReport) add(c FileCheck) {
	r.Checked++
	r.Bytes += c.Size
	switch c.State {
	case CheckSound:
		r.Sound++
		return
	case CheckRepairable:
		r.Repairable++
	case CheckUnreadable:
		r.Unreadable++
	case CheckAltered:
		r.Altered++
	case CheckDamagedAtSource:
		r.DamagedAtSource++
	case CheckIncomplete:
		r.Incomplete++
	case CheckSkipped:
		r.Skipped++
	}
	if c.Fix != "" {
		r.Fixable++
	}
	r.Flagged = append(r.Flagged, c)
}

// faultTally gathers per-chunk findings into one fault per shard record.
type faultTally struct {
	order  []string
	faults map[string]*ShardFault
}

func faultSeverity(kind string) int {
	switch kind {
	case FaultDamaged:
		return 3
	case FaultMismatched:
		return 2
	case FaultMissing:
		return 1
	}
	return 0
}

func (t *faultTally) note(s Shard, chunk int, kind, detail string) {
	key := fmt.Sprintf("%d/%s", s.Part, s.ProviderID)
	f := t.faults[key]
	if f == nil {
		if t.faults == nil {
			t.faults = map[string]*ShardFault{}
		}
		f = &ShardFault{
			Part: s.Part, ProviderID: accountIDOf(s.ProviderID), ProviderName: s.ProviderName,
			ProviderKind: s.ProviderKind, Kind: kind, FirstChunk: chunk, Detail: detail,
		}
		t.faults[key] = f
		t.order = append(t.order, key)
	}
	f.Chunks++
	if faultSeverity(kind) > faultSeverity(f.Kind) {
		f.Kind, f.Detail = kind, detail
	}
}

func (t *faultTally) list() []ShardFault {
	out := make([]ShardFault, 0, len(t.order))
	for _, key := range t.order {
		out = append(out, *t.faults[key])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Part < out[j].Part })
	return out
}

// checkedShard is one shard of one chunk, fetched and judged.
type checkedShard struct {
	shard  Shard
	opened *archive.OpenedShard
	kind   string // a Fault constant, or empty for good
	detail string
}

// checkEntry is the check itself, for one file. An error means the check
// could not be carried out — the vault locked, the context went — and never
// that the file is bad; that is what the FileCheck says.
func (v *Vault) checkEntry(ctx context.Context, scope Scope, entry *Entry, online map[string]bool, say func(within int64)) (FileCheck, error) {
	scheme := entry.Scheme()
	out := FileCheck{
		ID: entry.ID, Vault: scope, Path: entry.Path(), Name: entry.Name, Size: entry.Size,
		Kind: kindOf(entry), Scheme: scheme.String(), Needed: scheme.Data, Total: scheme.Total,
		HasReplica: entry.Replica != nil && entry.Replica.Recoverable(),
		archiveID:  entry.ArchiveID,
	}
	if !entry.Chunked() {
		out.State = CheckSkipped
		out.Summary = "It is stored in the format SAND used before chunking, which this check does not read."
		out.Advice = "Convert it first (sand vault convert), then check it again."
		return out, nil
	}

	snap, err := v.snapshotRead(entry, provider.Parts)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return out, err
		}
		out.State = CheckSkipped
		out.Summary = fmt.Sprintf("Its key is not available: %v.", err)
		return out, nil
	}
	defer crypto.ZeroBytes(snap.dataKey)

	// A disk that lives on another machine is not asked from this one.
	for id, cfg := range snap.configs {
		if v.livesElsewhere(cfg) {
			delete(snap.configs, id)
		}
	}

	var tally faultTally
	digest := sha256.New()
	var rebuilt int64
	var head, tail []byte
	tailAt := int64(0)
	firstBad, badChunks, badDetail := -1, 0, ""

	for index := 0; index < entry.ChunkCount; index++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		shards := v.fetchChunkShards(ctx, entry, index, snap, online)
		if err := ctx.Err(); err != nil {
			return out, err
		}

		good := make([]*archive.OpenedShard, 0, len(shards))
		seen := map[int]bool{}
		for _, s := range shards {
			if s.kind != "" {
				tally.note(s.shard, index, s.kind, s.detail)
				continue
			}
			if !seen[s.shard.Part] {
				seen[s.shard.Part] = true
				good = append(good, s.opened)
			}
		}

		var chunk *archive.DecodedChunk
		var chunkErr error
		if len(good) < scheme.Data {
			chunkErr = fmt.Errorf("only %d of the %d parts it needs are readable", len(good), scheme.Data)
		} else {
			chunk, chunkErr = archive.AssembleChunk(good)
		}
		if chunkErr != nil {
			if firstBad < 0 {
				firstBad, badDetail = index, chunkErr.Error()
			}
			badChunks++
			continue
		}

		// The rebuild used k of them. Recompute all n from it and hold every
		// shard that opened to its own.
		legacy := good[0].Legacy()
		if expect, err := archive.ShardData(chunk.Compressed, good[0].Scheme(), legacy); err == nil {
			for _, s := range shards {
				if s.kind != "" {
					continue
				}
				n := int(s.opened.Header.PartNumber)
				if n < 1 || n > len(expect) || !sameShard(s.opened.Data, expect[n-1], legacy) {
					tally.note(s.shard, index, FaultMismatched,
						"it opens under its seal but does not agree with the shards the file rebuilds from")
				}
			}
		}

		if badChunks == 0 {
			digest.Write(chunk.Data)
		}
		if index == 0 {
			head = chunk.Data
		}
		if index == entry.ChunkCount-1 {
			tail, tailAt = chunk.Data, rebuilt
		}
		rebuilt += int64(len(chunk.Data))
		if say != nil {
			say(rebuilt)
		}
	}

	out.Faults = tally.list()
	bad := map[int]bool{}
	unreachable := 0
	for _, f := range out.Faults {
		bad[f.Part] = true
		if f.Kind == FaultUnreachable {
			unreachable++
		}
	}
	out.Good = max(distinctShards(entry.Shards)-len(bad), 0)
	// A shard the upload never stored is a gap too, though no account is
	// holding anything wrong: the index simply has no record of it.
	short := scheme.Total - distinctShards(entry.Shards)
	wrong := len(out.Faults) - unreachable

	replicaAdvice := func() {
		if out.HasReplica {
			out.Fix = CheckFromReplica
			out.Advice = "It has a replica: new parts can be cut from that."
			return
		}
		if unreachable > 0 {
			out.Advice = "Some of its accounts did not answer — check it again once they do. It has no replica to rebuild from."
			return
		}
		out.Advice = "It has no replica, so nothing in the vault can put it back. Upload the original again if you still have it."
	}

	switch {
	case badChunks > 0:
		out.State = CheckUnreadable
		out.Summary = fmt.Sprintf("It cannot be rebuilt from its parts: %s.", badDetail)
		if entry.ChunkCount > 1 {
			out.Summary = fmt.Sprintf("It cannot be rebuilt from its parts: %d of its %d chunks are lost, the first being chunk %d — %s.",
				badChunks, entry.ChunkCount, firstBad+1, badDetail)
		}
		replicaAdvice()
		return out, nil
	}
	out.Rebuilt = true

	sum := hex.EncodeToString(digest.Sum(nil))
	switch {
	case rebuilt != entry.Size:
		out.State = CheckAltered
		out.Summary = fmt.Sprintf("It rebuilds to %d bytes and the index recorded %d.", rebuilt, entry.Size)
		replicaAdvice()
		return out, nil
	case entry.Hash != "" && sum != entry.Hash:
		out.State = CheckAltered
		out.Summary = "It rebuilds, but not to the bytes that were stored: its SHA-256 does not match the one recorded at upload."
		replicaAdvice()
		return out, nil
	}
	out.HashOK = true

	verdict := filecheck.Check(&checkedBytes{
		v: v, id: entry.ID, size: entry.Size, head: head, tail: tail, tailAt: tailAt,
	}, entry.Size, entry.Name)
	out.Content = &verdict

	switch {
	case wrong > 0 || short > 0:
		out.State = CheckRepairable
		out.Fix = CheckRebuild
		out.Summary = fmt.Sprintf("It reads correctly, on %d of its %d shards: %s.",
			out.Good, scheme.Total, describeFaults(out.Faults, short))
		out.Advice = fmt.Sprintf("It can lose %d more before it cannot be read. Rebuilding writes a full fresh set.",
			max(out.Good-scheme.Data, 0))
		if verdict.Status == filecheck.Damaged {
			out.Advice += " Separately, its contents were already damaged when it was stored: " +
				strings.Join(verdict.Problems, "; ") + "."
		}
	case verdict.Status == filecheck.Damaged:
		out.State = CheckDamagedAtSource
		out.Summary = fmt.Sprintf("SAND is returning exactly what was uploaded — every shard is right and the SHA-256 matches — "+
			"but as a %s file it is broken: %s.", verdict.Format, strings.Join(verdict.Problems, "; "))
		out.Advice = "It was like this when it arrived, so nothing in the vault can mend it. Upload a good copy of the original over it."
	case unreachable > 0:
		out.State = CheckIncomplete
		out.Summary = fmt.Sprintf("It reads correctly, but %d of its shards could not be looked at: %s.",
			unreachable, describeFaults(out.Faults, 0))
		out.Advice = "Check it again once those accounts answer."
	default:
		out.State = CheckSound
		out.Summary = "Every shard is right and it matches the SHA-256 recorded when it was stored."
	}
	return out, nil
}

// sameShard compares a stored shard's bytes with what the rebuild says they
// should be. A version 3 chunk's second half and parity may carry one byte of
// padding the recomputation does not, so those are compared over the shorter.
func sameShard(got, want []byte, legacy bool) bool {
	if legacy {
		n := min(len(got), len(want))
		return n > 0 && len(got)-n <= 1 && len(want)-n <= 1 && bytes.Equal(got[:n], want[:n])
	}
	return bytes.Equal(got, want)
}

// describeFaults is the faults as a clause: "part 3 on Dropbox is damaged;
// part 7 on Box is missing".
func describeFaults(faults []ShardFault, short int) string {
	words := map[string]string{
		FaultMissing:     "is missing",
		FaultDamaged:     "is damaged",
		FaultMismatched:  "does not match the rest",
		FaultUnreachable: "could not be reached",
	}
	var parts []string
	for _, f := range faults {
		parts = append(parts, fmt.Sprintf("part %d on %s %s", f.Part, f.ProviderName, words[f.Kind]))
	}
	if short > 0 {
		parts = append(parts, fmt.Sprintf("%d %s never stored", short, pluralWord(short, "part was", "parts were")))
	}
	return strings.Join(parts, "; ")
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// fetchChunkShards fetches every recorded shard of one chunk, all at once,
// and judges each on its own evidence.
func (v *Vault) fetchChunkShards(ctx context.Context, entry *Entry, index int, snap *readSnapshot, online map[string]bool) []checkedShard {
	out := make([]checkedShard, len(entry.Shards))
	var wg sync.WaitGroup
	for i, shard := range entry.Shards {
		out[i].shard = shard
		cfg, connected := snap.configs[shard.ProviderID]
		switch {
		case !connected:
			out[i].kind, out[i].detail = FaultUnreachable, "its account is not connected, or its disk is on another machine"
			continue
		case !online[accountIDOf(shard.ProviderID)] && !online[shard.ProviderID]:
			out[i].kind, out[i].detail = FaultUnreachable, "its account did not answer when the check began"
			continue
		}
		wg.Add(1)
		go func(i int, shard Shard, cfg provider.Config) {
			defer wg.Done()
			p, err := v.buildProvider(cfg)
			var blob []byte
			if err == nil {
				blob, err = v.meteredGet(ctx, p, cfg, ChunkShardKey(entry.ArchiveID, index, shard.Part))
			}
			switch {
			case errors.Is(err, provider.ErrNotFound):
				out[i].kind, out[i].detail = FaultMissing, "the account has no such object"
			case err != nil:
				// Anything else is no answer, and must not be read as one: a
				// rate limit is not a missing part, and a repair on the
				// strength of it would erase a part that is still there.
				out[i].kind, out[i].detail = FaultUnreachable, err.Error()
			default:
				opened, err := openChunkShard(entry, index, shard, blob, snap.dataKey)
				if err != nil {
					out[i].kind, out[i].detail = FaultDamaged, err.Error()
					return
				}
				out[i].opened = opened
			}
		}(i, shard, cfg)
	}
	wg.Wait()
	return out
}

// checkedBytes is a file's rebuilt plaintext as the content check reads it:
// the first and last chunks, which the check has just verified and still
// holds, and the vault's own reader for anything between. Nearly every format
// is judged from its two ends, so the reader is opened only for the one that
// is not — a film whose boxes have to be walked.
type checkedBytes struct {
	v      *Vault
	id     string
	size   int64
	head   []byte
	tail   []byte
	tailAt int64

	once   sync.Once
	reader *ChunkedReader
	err    error
}

func (c *checkedBytes) ReadAt(p []byte, off int64) (int, error) {
	if off >= 0 && off+int64(len(p)) <= int64(len(c.head)) {
		return copy(p, c.head[off:]), nil
	}
	if off >= c.tailAt && off+int64(len(p)) <= c.tailAt+int64(len(c.tail)) {
		return copy(p, c.tail[off-c.tailAt:]), nil
	}
	c.once.Do(func() { c.reader, c.err = c.v.OpenReader(c.id) })
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.reader.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

// RepairProgress is where a running repair has got to.
type RepairProgress struct {
	Path  string `json:"path"`
	File  int    `json:"file"`
	Files int    `json:"files"`
	Done  int    `json:"done"`
}

// RepairOutcome is what became of one file a repair was asked for.
type RepairOutcome struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Fix      CheckFix `json:"fix"`
	Repaired bool     `json:"repaired"`

	// Deferred says the file was left exactly as it was for a reason that is
	// not its fault, and Reason — set whenever Repaired is not — says why.
	Deferred bool     `json:"deferred,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// RepairReport is what a repair did.
type RepairReport struct {
	Repaired  int             `json:"repaired"`
	Failed    int             `json:"failed"`
	Deferred  int             `json:"deferred"`
	Files     []RepairOutcome `json:"files"`
	Stopped   bool            `json:"stopped,omitempty"`
	StoppedBy string          `json:"stopped_by,omitempty"`
}

// RepairChecked repairs files a check found wanting, each by the fix the
// check offered it: a rebuild from its good shards, or new parts cut from its
// replica.
//
// Both are the rebuild every other repair in the vault is (see automation.go,
// "a repair is always a rebuild"): the file is read, cut again under the code
// it has, and written whole onto the clouds it is already on wherever they
// answer; the index moves to the fresh parts in one write and the old ones
// are erased after it. The read is authenticated shard by shard and hashed
// end to end, so a repair cannot write out a file that differs from the one
// that was stored — it fails instead, and the file is left as it was.
//
// A file that has been rewritten since it was checked is left alone: the
// check's findings are about parts that no longer exist.
func (v *Vault) RepairChecked(ctx context.Context, checks []FileCheck, onProgress func(RepairProgress)) (*RepairReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	online, offline := v.probeAccounts(ctx, configs)
	dark := map[string]bool{}
	for id := range offline {
		dark[id] = true
	}
	answering := v.answeringAccounts(online)

	report := &RepairReport{Files: []RepairOutcome{}}
	for i := range checks {
		c := &checks[i]
		if c.Fix == "" {
			continue
		}
		if ctx.Err() != nil {
			report.Stopped = true
			break
		}
		// A repair in a sub vault that has shut would fail once per file,
		// each a "failed" that is not a failure of the file.
		if why := v.scopeShut(c.Vault); why != "" {
			report.Stopped, report.StoppedBy = true, why
			break
		}
		if onProgress != nil {
			onProgress(RepairProgress{Path: c.Path, File: i + 1, Files: len(checks), Done: len(report.Files)})
		}
		outcome := v.repairChecked(ctx, c, answering, dark)
		switch {
		case outcome.Repaired:
			report.Repaired++
		case outcome.Deferred:
			report.Deferred++
		default:
			report.Failed++
		}
		report.Files = append(report.Files, outcome)
		if onProgress != nil {
			onProgress(RepairProgress{Path: c.Path, File: i + 1, Files: len(checks), Done: len(report.Files)})
		}
	}
	return report, nil
}

func (v *Vault) repairChecked(ctx context.Context, c *FileCheck, answering []string, dark map[string]bool) RepairOutcome {
	out := RepairOutcome{ID: c.ID, Path: c.Path, Fix: c.Fix}

	v.mu.RLock()
	_, found, ok := v.scopeOfEntryLocked(c.ID)
	var entry *Entry
	if ok {
		entry = copyEntry(found)
	}
	v.mu.RUnlock()
	switch {
	case entry == nil:
		out.Deferred, out.Reason = true, "it has been deleted since it was checked"
		return out
	case entry.ArchiveID != c.archiveID:
		out.Deferred, out.Reason = true, "it has been rewritten since it was checked — check it again"
		return out
	}

	accounts, scheme, err := rebalanceTarget(entry, answering, false)
	if err != nil {
		out.Deferred, out.Reason = true, err.Error()
		return out
	}

	opts := relocationOptions{unreachable: dark}
	switch c.Fix {
	case CheckRebuild:
		absent := c.absentParts()
		if len(absent) == 0 && distinctShards(entry.Shards) >= scheme.Total {
			out.Deferred, out.Reason = true, "nothing is wrong with its parts"
			return out
		}
		opts.absent = map[string]map[int]bool{entry.ID: absent}
	case CheckFromReplica:
		opts.source = ReadReplica
	default:
		out.Reason = fmt.Sprintf("%q is not a repair", c.Fix)
		return out
	}

	report, err := v.relocate(ctx, c.Vault, entry.ID, accounts, scheme, opts, nil, nil)
	if report != nil {
		out.Warnings = report.Warnings
	}
	switch {
	case err != nil:
		out.Reason = err.Error()
	case report.Deferred > 0:
		out.Deferred = true
		out.Reason = "a re-encryption is using the slot a rebuild runs in — try again once it has finished"
	case report.NoReplica > 0:
		out.Reason = "it has no replica to rebuild from"
	case report.Relocated == 0:
		out.Reason = "the rebuild did not complete"
		if len(report.Warnings) > 0 {
			out.Reason = report.Warnings[len(report.Warnings)-1]
		}
	default:
		out.Repaired = true
	}
	return out
}
