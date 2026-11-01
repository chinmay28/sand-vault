package vault

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// Two machines, one index: how their copies are told apart and put together.
//
// Each machine opening the vault holds its own vault file and makes its own
// changes to it; the copies of the index on the clouds (manifest.sand) are how
// the changes travel. A machine pulls the copies it can reach, folds each into
// its own index with mergeIndexes, and pushes the result — see sync.go. What
// this file settles is the folding, and it settles it without ever looking at
// a cloud, so that the rules can be read and tested on their own.
//
// Three facts the index carries make it possible (manifest.go):
//
//   - A vector clock, one counter per machine. Comparing two clocks says
//     whether one copy is simply a later version of the other — in which case
//     the later one is taken whole — or whether both machines have written
//     since they last agreed, in which case the rows have to be combined.
//   - A stamp on every row saying when it was last written differently
//     (Entry.IndexedAt). When both machines changed the same row, the later
//     write stands. Stamped by the index write itself (stampIndexLocked), so
//     no code path can forget.
//   - Tombstones: the IDs of rows taken out, and when. Without them a row on
//     one side only is ambiguous — added here, or deleted there? — and the
//     safe reading of ambiguity would bring every deleted file back on every
//     merge.
//
// The rules, for a row by ID when the clocks have diverged:
//
//   - On both sides: the later stamp wins, ties to this machine's copy.
//   - On one side only, and the other side has a tombstone for it: gone. The
//     machine that deleted it erased its parts; keeping the row would list a
//     file that opens to an error. A tombstone always wins, however the
//     stamps compare — the parts are not coming back.
//   - On one side only, and no tombstone: kept. It is a file the other side
//     has not heard of yet.
//
// Two files that end up with the same path — each machine uploaded a
// "notes.txt" to the same folder — both survive, and the later one is given
// the " (2)" name an upload beside an existing file would have been given.
// Deciding by stamp rather than by which side it came from is what makes both
// machines arrive at the same answer, since each sees the other's row as the
// remote one.
//
// Everything else in the index — thumbnails, film details, automations,
// folder art, the replication policy — merges by key, the way the rows do
// where it has a stamp and local-wins where it has not; none of it is worth
// a conflict, since each is recomputed or re-chosen cheaply.

// tombstoneLifetime is how long a deleted file's ID is remembered. A machine
// that has not merged for longer than this may bring a deleted file back as a
// row pointing at parts that are gone; the health check then calls it
// unrecoverable, and a delete clears it. Three months is far past how long a
// machine that is part of anybody's routine stays off.
const tombstoneLifetime = 90 * 24 * time.Hour

// MergeOutcome is how two copies of the index compared.
type MergeOutcome string

const (
	// MergeCurrent: the same version. Nothing to take, nothing to give.
	MergeCurrent MergeOutcome = "current"
	// MergeAhead: this machine's copy is a later version of the other.
	MergeAhead MergeOutcome = "ahead"
	// MergeAdopted: the other copy is a later version of this one, and was
	// taken whole.
	MergeAdopted MergeOutcome = "adopted"
	// MergeMerged: both machines wrote since they last agreed, and the rows
	// were combined.
	MergeMerged MergeOutcome = "merged"
)

// MergeReport is what one fold did to this machine's index.
type MergeReport struct {
	Outcome MergeOutcome `json:"outcome"`

	// Rows this machine's index gained, lost and saw changed by the fold.
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Updated int `json:"updated"`

	// Renamed says, one line each, which files were given a new name because
	// another file had arrived at their path from the other machine.
	Renamed []string `json:"renamed,omitempty"`

	// RegisterChanged says the register of machines changed, which it does
	// whatever the outcome — the first time a second machine's write is seen,
	// above all.
	RegisterChanged bool `json:"register_changed,omitempty"`
}

// Changed reports whether the fold left this machine's index different.
func (r MergeReport) Changed() bool {
	return r.Outcome == MergeAdopted || r.Outcome == MergeMerged || r.RegisterChanged
}

// clockOrder is how two vector clocks compare.
type clockOrder int

const (
	clocksEqual      clockOrder = iota
	clockBefore                 // a is an earlier version of b
	clockAfter                  // a is a later version of b
	clocksConcurrent            // each has writes the other has not seen
)

// compareClocks orders two vector clocks. A missing counter is zero.
func compareClocks(a, b map[string]uint64) clockOrder {
	aAhead, bAhead := false, false
	for name, n := range a {
		if n > b[name] {
			aAhead = true
		}
	}
	for name, n := range b {
		if n > a[name] {
			bAhead = true
		}
	}
	switch {
	case aAhead && bAhead:
		return clocksConcurrent
	case aAhead:
		return clockAfter
	case bAhead:
		return clockBefore
	default:
		return clocksEqual
	}
}

// entryFingerprint is a row's contents in one number, its stamp left out: two
// rows with the same fingerprint say the same thing about the file.
//
// Over the row's JSON rather than a hand-picked list of fields, so a field
// added to Entry is in the fingerprint without anybody remembering to put it
// there — a field left out would be a change the stamp never noticed and the
// merge never carried. It costs the index write a second encoding of every
// row, on top of the one that seals it, which is a fraction of the write.
func entryFingerprint(e *Entry) uint64 {
	plain := *e
	plain.IndexedAt = time.Time{}
	data, err := json.Marshal(&plain)
	if err != nil {
		// Marshal of a struct of plain fields cannot fail; if it somehow
		// did, a fingerprint nothing else has makes the row read as changed,
		// which is the safe mistake.
		return uint64(time.Now().UnixNano())
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64()
}

// fingerprints is every row's fingerprint, by ID.
func fingerprints(m *Manifest) map[string]uint64 {
	out := make(map[string]uint64, len(m.Entries))
	for _, e := range m.Entries {
		out[e.ID] = entryFingerprint(e)
	}
	return out
}

// stampIndexLocked is the main index's mark for this write: every row written
// differently since the last write is stamped now, this machine's counter in
// the clock goes up by one, and tombstones past their lifetime are dropped.
// The caller holds the write lock and is about to seal the manifest.
//
// The last write's fingerprints are kept in memory (indexFP), rebuilt from
// the index whenever one is adopted (adoptLocked, and sync.go). With none to
// compare against — the first write after an upgrade — a row without a stamp
// is stamped now and a row with one is believed.
func (v *Vault) stampIndexLocked(now time.Time) {
	m := v.manifest
	if m == nil {
		return
	}
	next := make(map[string]uint64, len(m.Entries))
	for _, e := range m.Entries {
		fp := entryFingerprint(e)
		next[e.ID] = fp
		switch was, known := v.indexFP[e.ID]; {
		case v.indexFP == nil:
			if e.IndexedAt.IsZero() {
				e.IndexedAt = now
			}
		case !known || was != fp:
			e.IndexedAt = now
		}
	}
	v.indexFP = next

	if m.Clock == nil {
		m.Clock = map[string]uint64{}
	}
	if v.instance != "" {
		m.Clock[v.instance]++
	}
	pruneTombstones(m.Tombstones, now)
	pruneTombstones(m.FolderTombstones, now)
}

// pruneTombstones drops the tombstones older than tombstoneLifetime.
func pruneTombstones(stones map[string]time.Time, now time.Time) {
	for key, at := range stones {
		if now.Sub(at) > tombstoneLifetime {
			delete(stones, key)
		}
	}
}

// mergeIndexes folds a copy of the index from another machine into this one's
// and says what it did. local is this machine's index and is not changed;
// remote is the copy read off a cloud, which the result may share rows with.
//
// The outcome follows the clocks: a copy that is a later version is taken
// whole, an earlier one is left, and two that have diverged are combined row
// by row under the rules at the top of this file. The register of machines is
// unioned whatever the outcome — it is the one thing worth taking from a copy
// that is otherwise behind.
func mergeIndexes(local, remote *Manifest) (*Manifest, MergeReport) {
	order := compareClocks(local.Clock, remote.Clock)

	var merged *Manifest
	var report MergeReport
	switch order {
	case clocksEqual:
		report.Outcome = MergeCurrent
		merged = local
	case clockAfter:
		report.Outcome = MergeAhead
		merged = local
	case clockBefore:
		report.Outcome = MergeAdopted
		merged = remote
		merged.normalize()
	case clocksConcurrent:
		report.Outcome = MergeMerged
		merged = combine(local, remote, &report)
	}

	// The register is unioned into the result whatever the clocks said. When
	// the result is one side or the other it is a fresh map on a copy of the
	// manifest, so neither input is written to.
	instances, changed := unionInstances(local.Instances, remote.Instances)
	if report.Outcome != MergeMerged && changed {
		copied := *merged
		merged = &copied
	}
	merged.Instances = instances
	report.RegisterChanged = changed

	report.Added, report.Removed, report.Updated = diffRows(local, merged)
	return merged, report
}

// combine is the row-by-row merge of two copies that have diverged.
func combine(local, remote *Manifest, report *MergeReport) *Manifest {
	out := *local
	out.Entries = nil
	out.Folders = nil

	// Rows, by the rules at the top of the file. Walked in ID order so the
	// result is the same whichever side runs it.
	ids := map[string]bool{}
	byLocal := map[string]*Entry{}
	byRemote := map[string]*Entry{}
	for _, e := range local.Entries {
		ids[e.ID] = true
		byLocal[e.ID] = e
	}
	for _, e := range remote.Entries {
		ids[e.ID] = true
		byRemote[e.ID] = e
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	for _, id := range sorted {
		l, r := byLocal[id], byRemote[id]
		switch {
		case l != nil && r != nil:
			if r.stamp().After(l.stamp()) {
				out.Entries = append(out.Entries, r)
			} else {
				out.Entries = append(out.Entries, l)
			}
		case l != nil:
			if _, deleted := remote.Tombstones[id]; !deleted {
				out.Entries = append(out.Entries, l)
			}
		default:
			if _, deleted := local.Tombstones[id]; !deleted {
				out.Entries = append(out.Entries, r)
			}
		}
	}
	report.Renamed = resolveCollisions(&out)

	// Folders: the union of what either side made by hand, less what the
	// other side deleted and this side did not re-make. A folder with a file
	// under it is a folder whatever this says (FolderExists), so the worst a
	// wrong call here can do is show an empty folder once more.
	seen := map[string]bool{}
	for _, f := range local.Folders {
		if _, deleted := remote.FolderTombstones[f]; deleted && !hasFolder(remote, f) {
			continue
		}
		seen[f] = true
	}
	for _, f := range remote.Folders {
		if _, deleted := local.FolderTombstones[f]; deleted && !hasFolder(local, f) {
			continue
		}
		seen[f] = true
	}
	for f := range seen {
		out.Folders = append(out.Folders, f)
	}
	sort.Strings(out.Folders)

	out.Tombstones = laterOf(local.Tombstones, remote.Tombstones)
	out.FolderTombstones = laterOf(local.FolderTombstones, remote.FolderTombstones)
	// A row that survived is not deleted, whatever a stale tombstone says: it
	// was re-added (a move back out of a sub vault, say) after the delete.
	for _, e := range out.Entries {
		delete(out.Tombstones, e.ID)
	}
	for _, f := range out.Folders {
		delete(out.FolderTombstones, f)
	}

	out.Clock = maxClock(local.Clock, remote.Clock)
	if remote.UpdatedAt.After(local.UpdatedAt) {
		out.UpdatedAt = remote.UpdatedAt
	}

	// Per-file and per-folder records, by key. Where a record has a time it
	// decides; where it has none this machine's stands. A record for a file
	// that did not survive goes with the file.
	alive := map[string]bool{}
	for _, e := range out.Entries {
		alive[e.ID] = true
	}
	out.Thumbs = mergeThumbs(local.Thumbs, remote.Thumbs)
	out.Movies = unionKeep(local.Movies, remote.Movies, alive)
	out.Repos = unionKeep(local.Repos, remote.Repos, alive)
	out.MovieFolders = unionKeep(local.MovieFolders, remote.MovieFolders, nil)
	out.Automations = unionKeep(local.Automations, remote.Automations, nil)
	out.ReplicatedFolders = unionKeep(local.ReplicatedFolders, remote.ReplicatedFolders, nil)
	out.FolderArt = unionKeep(local.FolderArt, remote.FolderArt, nil)
	for dir, id := range out.FolderArt {
		if !alive[id] {
			delete(out.FolderArt, dir)
		}
	}
	if out.Replication == nil {
		out.Replication = remote.Replication
	}
	if out.Rekey == nil {
		out.Rekey = remote.Rekey
	}
	out.SubVaults = mergeSubVaultMetas(local.SubVaults, remote.SubVaults)
	out.StaleParts = mergeStaleParts(local.StaleParts, remote.StaleParts)
	return &out
}

// resolveCollisions gives the later of two rows at one path the name an upload
// beside an existing file gets, and says which files were renamed. Rows are
// visited earliest stamp first, ties by ID, so both machines rename the same
// row.
func resolveCollisions(m *Manifest) []string {
	order := make([]*Entry, len(m.Entries))
	copy(order, m.Entries)
	sort.SliceStable(order, func(i, j int) bool {
		if !order[i].stamp().Equal(order[j].stamp()) {
			return order[i].stamp().Before(order[j].stamp())
		}
		return order[i].ID < order[j].ID
	})

	taken := map[string]bool{}
	var renamed []string
	for _, e := range order {
		path := e.Path()
		if !taken[path] {
			taken[path] = true
			continue
		}
		// A copy, so the row the other side still holds is not written to.
		fresh := *e
		fresh.Name = uniqueAmong(taken, e.Dir, e.Name)
		taken[fresh.Path()] = true
		renamed = append(renamed, fmt.Sprintf("%s → %s", path, fresh.Path()))
		for i := range m.Entries {
			if m.Entries[i].ID == e.ID {
				m.Entries[i] = &fresh
			}
		}
	}
	return renamed
}

// uniqueAmong is uniqueName's rule over a set of taken paths rather than an
// index: name, or name with a " (2)"-style suffix.
func uniqueAmong(taken map[string]bool, dir, name string) string {
	if !taken[JoinPath(dir, name)] {
		return name
	}
	ext := ""
	stem := name
	if i := strings.LastIndex(name, "."); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !taken[JoinPath(dir, candidate)] {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext)
}

// hasFolder reports whether a manifest lists a folder by hand.
func hasFolder(m *Manifest, dir string) bool {
	for _, f := range m.Folders {
		if f == dir {
			return true
		}
	}
	return false
}

// laterOf unions two maps of times, keeping the later time where both have
// the key. nil when both are empty.
func laterOf(a, b map[string]time.Time) map[string]time.Time {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(a)+len(b))
	for k, at := range a {
		out[k] = at
	}
	for k, at := range b {
		if was, ok := out[k]; !ok || at.After(was) {
			out[k] = at
		}
	}
	return out
}

// maxClock is the elementwise maximum of two clocks.
func maxClock(a, b map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(a)+len(b))
	for k, n := range a {
		out[k] = n
	}
	for k, n := range b {
		if n > out[k] {
			out[k] = n
		}
	}
	return out
}

// unionInstances unions two registers, the later LastSeen winning, and says
// whether the result differs from the first. nil when both are empty.
func unionInstances(a, b map[string]*Instance) (map[string]*Instance, bool) {
	if len(a) == 0 && len(b) == 0 {
		return nil, false
	}
	out := make(map[string]*Instance, len(a)+len(b))
	for k, inst := range a {
		out[k] = inst
	}
	changed := false
	for k, inst := range b {
		if inst == nil {
			continue
		}
		was, ok := out[k]
		if !ok || was == nil || inst.LastSeen.After(was.LastSeen) {
			out[k] = inst
			changed = changed || !ok || was == nil || !sameInstance(was, inst)
		}
	}
	return out, changed
}

func sameInstance(a, b *Instance) bool {
	if a.Name != b.Name || !a.LastSeen.Equal(b.LastSeen) || len(a.Accounts) != len(b.Accounts) {
		return false
	}
	for i := range a.Accounts {
		if a.Accounts[i] != b.Accounts[i] {
			return false
		}
	}
	return true
}

// unionKeep unions two maps, this machine's value standing where both have the
// key, and drops keys whose value names a file that did not survive when alive
// is given. nil when the result is empty.
func unionKeep[V any](local, remote map[string]V, alive map[string]bool) map[string]V {
	if len(local) == 0 && len(remote) == 0 {
		return nil
	}
	out := make(map[string]V, len(local)+len(remote))
	for k, val := range remote {
		out[k] = val
	}
	for k, val := range local {
		out[k] = val
	}
	if alive != nil {
		for k := range out {
			if !alive[k] {
				delete(out, k)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeThumbs unions two sets of thumbnail packs, by folder, the later-written
// pack winning: a pack is rewritten whole whenever a file in the folder gains
// or loses a picture, so the later one describes the folder better.
func mergeThumbs(local, remote map[string]*ThumbPack) map[string]*ThumbPack {
	if len(local) == 0 && len(remote) == 0 {
		return nil
	}
	out := make(map[string]*ThumbPack, len(local)+len(remote))
	for dir, pack := range local {
		out[dir] = pack
	}
	for dir, pack := range remote {
		if pack == nil {
			continue
		}
		if was, ok := out[dir]; !ok || was == nil || pack.UpdatedAt.After(was.UpdatedAt) {
			out[dir] = pack
		}
	}
	return out
}

// mergeSubVaultMetas unions the main vault's records of its sub vaults by ID,
// this machine's standing where both have one.
func mergeSubVaultMetas(local, remote []*SubVaultMeta) []*SubVaultMeta {
	if len(remote) == 0 {
		return local
	}
	have := map[string]bool{}
	out := append([]*SubVaultMeta(nil), local...)
	for _, meta := range local {
		have[meta.ID] = true
	}
	for _, meta := range remote {
		if meta != nil && !have[meta.ID] {
			out = append(out, meta)
		}
	}
	return out
}

// mergeStaleParts unions two lists of stray parts, one record per part.
func mergeStaleParts(local, remote []*StalePart) []*StalePart {
	if len(remote) == 0 {
		return local
	}
	key := func(p *StalePart) string {
		return p.ProviderID + "/" + p.ArchiveID + "/" + fmt.Sprint(p.Part)
	}
	have := map[string]bool{}
	out := append([]*StalePart(nil), local...)
	for _, p := range local {
		have[key(p)] = true
	}
	for _, p := range remote {
		if p != nil && !have[key(p)] {
			out = append(out, p)
		}
	}
	return out
}

// diffRows counts the rows gained, lost and changed between two indexes.
func diffRows(before, after *Manifest) (added, removed, updated int) {
	was := fingerprints(before)
	now := map[string]bool{}
	for _, e := range after.Entries {
		now[e.ID] = true
		fp, known := was[e.ID]
		switch {
		case !known:
			added++
		case fp != entryFingerprint(e):
			updated++
		}
	}
	for id := range was {
		if !now[id] {
			removed++
		}
	}
	return added, removed, updated
}
