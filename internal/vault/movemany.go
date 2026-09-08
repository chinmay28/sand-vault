package vault

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Moving a batch of files in one write.
//
// Move, one file at a time, pays the full cost of a change on every call: the
// whole index re-sealed and written to disk, and then — for a file that
// changed folder — its thumbnail lifted out of one folder's pack and put into
// another's, which is two more index writes and two scatters across the
// accounts, because a pack is one stored object per folder rather than one per
// picture.
//
// That is affordable for a rename and ruinous for a plan. Filing ten thousand
// photographs by date (see datesort.go) is ten thousand moves, and one at a
// time that is ten thousand index writes plus twenty thousand pack scatters
// carrying a growing pack every time — quadratic in the size of the folder,
// for a change that touches nothing but the folder field of ten thousand
// records. It is the same trap SetThumbs was written to get out of, in the
// same place.
//
// So a batch is applied here instead: every move against one manifest under
// one lock, the index written once for the lot, and the thumbnails carried a
// folder at a time — one read of each folder losing files, one write of each
// folder gaining them, however many files that is. Ten thousand moves become
// one index write and a pack per folder.
//
// What it is not is a plan endpoint. It has no idea what the moves mean, and
// it reports each order's own outcome, so a caller running a plan in pieces
// still knows exactly which pieces landed — see MoveReport.

// MoveOrder is one file and where it should end up: the same three fields
// Move takes, so a batch of one and a call to Move are the same act.
type MoveOrder struct {
	ID string `json:"id"`

	// Dir is the folder it should end up in, and Name what it should be called
	// there. Empty leaves that half alone, so a batch can rename without
	// moving and move without renaming.
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

// MoveReport is what MoveMany came to.
type MoveReport struct {
	// Moved counts the orders that were satisfied. A file already where its
	// order puts it counts as satisfied without anything being touched, which
	// is what makes running the same batch twice a no-op rather than a page of
	// collisions.
	Moved int `json:"moved"`

	// Missing lists the IDs that named no file — most often a batch tried
	// again after one that stopped part-way, where the files it already moved
	// are still there and only the missing would be an error.
	Missing []string `json:"missing"`

	// Refused is one line per order that could not be applied: a destination
	// folder that is not there, a name already taken, a name the vault will
	// not store. The orders around it are applied regardless, and every line
	// names the file it is about, so a caller knows which half of its plan to
	// run again.
	Refused []string `json:"refused"`
}

// MoveMany applies a batch of moves in one index write.
//
// The orders are applied in the order given, each against the folder as the
// ones before it left it — so a batch means exactly what the same orders
// would have meant run one at a time through Move, including a move that
// takes a name another move in the same batch has just vacated.
//
// An order naming no file is reported in Missing, and one that cannot be
// applied in Refused; neither fails the batch, and neither stops the orders
// after it. What fails the batch is a fault of the vault as a whole — locked,
// or an index that could not be written — and then nothing has moved at all:
// the entries are put back as they were before the write was attempted.
//
// Thumbnails are carried afterwards, a folder at a time, and never fail the
// move: a picture in the wrong pack is cosmetic, and the file is where it was
// asked to be either way.
func (v *Vault) MoveMany(ctx context.Context, orders []MoveOrder) (*MoveReport, error) {
	report := &MoveReport{Missing: []string{}, Refused: []string{}}
	if len(orders) == 0 {
		return report, nil
	}

	// Where each moved file's picture has to follow it to, gathered under the
	// lock and acted on outside it.
	var carried []thumbCarry

	// What was changed, well enough to put back: a persist that fails must not
	// leave a batch applied in memory and unwritten on disk. Same discipline
	// as Retime.
	type change struct {
		entry *Entry
		dir   string
		name  string
	}
	var changed []change

	v.mu.Lock()

	if v.dataKey == nil {
		v.mu.Unlock()
		return nil, ErrLocked
	}

	// The manifest is a slice, and ByID and ByPath walk it. A batch of ten
	// thousand orders resolving that way is a hundred million comparisons for
	// an answer a map gives in one, so the whole index is turned into maps
	// once here and kept true as the orders are applied.
	index := v.entryIndexLocked()

	applied := 0
	for _, order := range orders {
		found, ok := index.byID[order.ID]
		if !ok {
			report.Missing = append(report.Missing, order.ID)
			continue
		}
		e := found.entry

		dir := e.Dir
		if order.Dir != "" {
			dir = CleanDir(order.Dir)
			if !index.exists(found.scope, dir) {
				report.Refused = append(report.Refused, fmt.Sprintf("%s: no such folder: %s", e.Path(), dir))
				continue
			}
		}
		name := e.Name
		if order.Name != "" {
			clean, err := SanitizeName(order.Name)
			if err != nil {
				report.Refused = append(report.Refused, fmt.Sprintf("%s: %s", e.Path(), err))
				continue
			}
			name = clean
		}

		// Already where it was asked to be. Settled, not moved, and not a
		// collision with itself.
		if dir == e.Dir && name == e.Name {
			report.Moved++
			continue
		}
		if index.at(found.scope, JoinPath(dir, name)) != nil {
			report.Refused = append(report.Refused,
				fmt.Sprintf("%s: %s already exists", e.Path(), JoinPath(dir, name)))
			continue
		}

		changed = append(changed, change{entry: e, dir: e.Dir, name: e.Name})
		if dir != e.Dir {
			carried = append(carried, thumbCarry{scope: found.scope, id: e.ID, from: e.Dir, to: dir})
		}
		index.rename(found.scope, e, dir, name)
		// ModifiedAt is left alone, for the reason Move gives: a file does not
		// become new by being called something else or by sitting somewhere
		// else — and stamping it here would make filing a folder by date
		// destroy the dates it had just read.
		e.Dir, e.Name = dir, name
		applied++
		report.Moved++
	}

	var err error
	if applied > 0 {
		if err = v.persistLocked(); err != nil {
			for i := len(changed) - 1; i >= 0; i-- {
				changed[i].entry.Dir, changed[i].entry.Name = changed[i].dir, changed[i].name
			}
		}
	}
	v.mu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("could not record the moves: %w", err)
	}

	// After the index write, so a pack that will not save cannot keep a file
	// from having moved — and a folder at a time, not a file at a time.
	v.carryThumbs(ctx, carried)
	return report, nil
}

// Mkdirs records a set of folders in one index write.
//
// Mkdir in a loop is the same trap as Move in a loop, in miniature: a plan
// that files a decade of photographs into months makes a hundred and twenty
// folders before it moves anything, and a hundred and twenty re-sealings of
// the whole index to do it. Missing ancestors are still created along the way,
// a folder already there is not an error, and a path the vault will not accept
// fails the call before anything is written — so either every folder named is
// there afterwards or none of them is.
func (v *Vault) Mkdirs(scope Scope, dirs []string) error {
	if len(dirs) == 0 {
		return nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return err
	}

	// The folders as they were, so a refusal half way along the list — or a
	// write that fails after all of them — leaves the manifest exactly as it
	// was found rather than partly built.
	was := append([]string(nil), m.Folders...)
	for _, dir := range dirs {
		if err := m.Mkdir(dir); err != nil {
			m.Folders = was
			return err
		}
	}
	if len(m.Folders) == len(was) {
		return nil
	}
	if err := v.persistLocked(); err != nil {
		m.Folders = was
		return err
	}
	return nil
}

// entryIndex is every file in every open vault — by ID, by path, and counted
// by the folder it sits in — so a batch can resolve and check an order without
// walking the manifest for each one.
//
// It points at the manifest's own entries and is true only for as long as the
// lock it was built under is held. Applying a move to an entry means telling
// the index about it (see rename), which is what keeps an order's answer the
// same as it would have been running one at a time.
type entryIndex struct {
	byID  map[string]locatedEntry
	scope map[Scope]*scopeIndex
}

type locatedEntry struct {
	scope Scope
	entry *Entry
}

// scopeIndex is one vault's files, by path and by folder.
type scopeIndex struct {
	byPath map[string]*Entry

	// explicit is the folders the manifest records, and population how many
	// files sit directly in each folder. Between them they answer
	// Manifest.FolderExists, which walks every entry in the vault to answer it
	// and is asked once per order.
	explicit   map[string]bool
	population map[string]int

	// known is what exists() has already worked out. Only a folder emptying
	// can make an answer in here wrong, and that clears the lot: a file
	// arriving somewhere cannot bring a folder into existence, because it can
	// only arrive where one is already.
	known map[string]bool
}

// at returns the entry at a path within one vault, or nil.
func (ix entryIndex) at(scope Scope, full string) *Entry {
	return ix.scope[scope].byPath[full]
}

// exists answers Manifest.FolderExists: a folder is there if it was recorded
// or if a file sits anywhere beneath it.
func (ix entryIndex) exists(scope Scope, dir string) bool {
	si := ix.scope[scope]
	if si == nil {
		return false
	}
	if dir == "/" || si.explicit[dir] {
		return true
	}
	if found, asked := si.known[dir]; asked {
		return found
	}
	found := si.population[dir] > 0
	if !found {
		prefix := dir + "/"
		for held, n := range si.population {
			if n > 0 && strings.HasPrefix(held, prefix) {
				found = true
				break
			}
		}
	}
	si.known[dir] = found
	return found
}

// rename moves an entry within the index, leaving the entry itself alone —
// the caller writes the new folder and name onto it. Called before the entry
// is changed, so the path and folder it is filed under now are still the ones
// to drop it from.
func (ix entryIndex) rename(scope Scope, e *Entry, dir, name string) {
	si := ix.scope[scope]
	delete(si.byPath, e.Path())
	si.byPath[JoinPath(dir, name)] = e
	if dir == e.Dir {
		return
	}
	si.population[e.Dir]--
	si.population[dir]++
	if si.population[e.Dir] == 0 {
		// The folder it left may have been the only reason something above it
		// existed at all, so every worked-out answer goes.
		si.known = map[string]bool{}
	}
}

// entryIndexLocked indexes every open vault's entries. The caller must hold at
// least the read lock.
func (v *Vault) entryIndexLocked() entryIndex {
	ix := entryIndex{byID: map[string]locatedEntry{}, scope: map[Scope]*scopeIndex{}}

	add := func(scope Scope, m *Manifest) {
		if m == nil {
			return
		}
		si := &scopeIndex{
			byPath:     make(map[string]*Entry, len(m.Entries)),
			explicit:   make(map[string]bool, len(m.Folders)),
			population: map[string]int{},
			known:      map[string]bool{},
		}
		for _, e := range m.Entries {
			if _, seen := ix.byID[e.ID]; !seen {
				ix.byID[e.ID] = locatedEntry{scope: scope, entry: e}
			}
			si.byPath[e.Path()] = e
			si.population[e.Dir]++
		}
		for _, f := range m.Folders {
			si.explicit[f] = true
		}
		ix.scope[scope] = si
	}

	// The main vault first, so an ID somehow in two open vaults at once
	// resolves the way scopeOfEntryLocked resolves it.
	add(MainScope, v.manifest)
	for id, sub := range v.subs {
		add(Scope(id), sub.manifest)
	}
	return ix
}

// thumbCarry is one file's picture and the two folders' packs it sits between.
type thumbCarry struct {
	scope Scope
	id    string
	from  string
	to    string
}

// packWindow is how many folders' packs are written at once.
//
// A pack is a scatter across three accounts, and a folder's turn is over only
// when the slowest of them has answered. Filing a decade into months writes a
// hundred and twenty of them, and one after another that is a hundred and
// twenty worst-of-three waits in a row. A few abreast overlaps the waits
// without turning it into the burst per account that gets rate-limited — the
// same figure, for the same reason, as eraseWindow.
const packWindow = 4

// carryThumbs moves a batch of thumbnails between folders' packs, reading each
// losing folder once and writing each gaining folder once.
//
// moveThumb per file would read and scatter both packs for every picture,
// which for a folder being filed by date is the whole pack uploaded twice per
// photograph. Here a folder of ten thousand costs one read and one write of
// the folder they came from, and one write of each folder they went to.
//
// Like every other thumbnail path this never reports a failure: a picture that
// does not make it is drawn again from the file the next time the folder is
// opened. The order within a folder is the one moveThumb uses and matters for
// the same reason — stored in the new pack first, so a picture that exists in
// both places for a moment is invisible where one that exists in neither is
// lost.
func (v *Vault) carryThumbs(ctx context.Context, carried []thumbCarry) {
	if len(carried) == 0 {
		return
	}

	type folder struct {
		scope Scope
		dir   string
	}

	// Every folder losing pictures, and to where. The pack is read once per
	// losing folder, whatever the fan-out.
	leaving := map[folder][]thumbCarry{}
	order := []folder{}
	for _, c := range carried {
		key := folder{c.scope, CleanDir(c.from)}
		if _, seen := leaving[key]; !seen {
			order = append(order, key)
		}
		leaving[key] = append(leaving[key], c)
	}

	arriving := map[folder]map[string][]byte{}
	for _, key := range order {
		v.mu.RLock()
		pack := (*ThumbPack)(nil)
		if m, err := v.manifestForLocked(key.scope); err == nil {
			pack = m.Thumbs[key.dir]
		}
		v.mu.RUnlock()
		if pack == nil {
			continue
		}
		held := false
		for _, c := range leaving[key] {
			if pack.holds(c.id) {
				held = true
				break
			}
		}
		if !held {
			continue
		}

		items, err := v.loadPack(ctx, key.scope, key.dir)
		if err != nil {
			continue
		}
		for _, c := range leaving[key] {
			thumb, ok := items[c.id]
			if !ok {
				continue
			}
			dest := folder{c.scope, CleanDir(c.to)}
			if arriving[dest] == nil {
				arriving[dest] = map[string][]byte{}
			}
			arriving[dest][c.id] = thumb
		}
	}
	if len(arriving) == 0 {
		return
	}

	// The gaining folders first, a few abreast. Which pictures actually landed
	// is what the losing folders are then allowed to forget.
	var mu sync.Mutex
	stored := map[string]bool{}
	each(packWindow, keysOf(arriving), func(dest folder) {
		if err := v.SetThumbs(ctx, dest.scope, dest.dir, arriving[dest]); err != nil {
			return
		}
		mu.Lock()
		for id := range arriving[dest] {
			stored[id] = true
		}
		mu.Unlock()
	})

	each(packWindow, order, func(key folder) {
		var gone []string
		for _, c := range leaving[key] {
			if stored[c.id] {
				gone = append(gone, c.id)
			}
		}
		v.removeThumbs(ctx, key.scope, key.dir, gone...)
	})
}

// keysOf is a map's keys, for iterating them a few abreast.
func keysOf[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// each runs work over items with at most window of them in flight, and returns
// once every one has finished.
func each[T any](window int, items []T, work func(T)) {
	var wg sync.WaitGroup
	gate := make(chan struct{}, window)
	for _, item := range items {
		gate <- struct{}{}
		wg.Add(1)
		go func(item T) {
			defer wg.Done()
			defer func() { <-gate }()
			work(item)
		}(item)
	}
	wg.Wait()
}
