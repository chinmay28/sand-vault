package vault

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/chinmay28/sand-vault/internal/archive"
	"sort"
	"time"
)

// Thumbnails are stored a folder at a time rather than a file at a time, and
// the reason is the cost of a key derivation. archive.EncodeBytes runs one
// Argon2id pass — 64 MB, three iterations — per archive, whatever the payload
// weighs, so a 9 KB thumbnail would cost exactly as much as a 4 GB video. One
// archive per picture would spend that on every row of the list, twice: once
// writing and once reading. One archive per folder pays it once for the whole
// listing.
//
// A pack is otherwise an ordinary stored object: compressed, split into three
// encrypted parts under the vault's data key, and scattered across the same
// accounts by the same placement rules. Its parts are named like every other
// part, so nothing on an account marks them out as pictures.
//
// Nothing here is precious. A thumbnail is derived from a file that is still
// stored, so any pack that cannot be read is dropped and made again rather
// than repaired.

// MaxThumbBytes is the ceiling on one stored thumbnail. The server normalizes
// what it is given to a small JPEG well under this; the limit is what stops a
// caller storing something else entirely under the name of a thumbnail.
const MaxThumbBytes = 128 << 10

// maxPackEntries caps how many thumbnails one pack holds, so that uploading
// into a folder of thousands of pictures does not rewrite megabytes each time.
// Beyond it the oldest entries are dropped; they cost one regeneration each.
const maxPackEntries = 512

// thumbArchiveName is the filename recorded inside a pack's parts. Every part
// carries one, and "thumbs" says as little as the object keys do.
const thumbArchiveName = "thumbs"

// ErrNoThumb is returned when a file has no stored thumbnail.
var ErrNoThumb = errors.New("no thumbnail is stored for this file")

// ThumbPack points at one folder's stored thumbnails. It lives in the
// manifest, which is encrypted at rest, so the list of which files have a
// picture never leaves this machine in the clear.
type ThumbPack struct {
	// IDs is the files the pack holds, sorted. It is kept alongside the shards
	// so a listing can say which rows will have a picture without opening the
	// pack, which would mean a network round-trip per folder just to draw.
	IDs []string `json:"ids"`

	// KeyID names the data key generation the parts are sealed under, exactly
	// as an entry does.
	KeyID     string    `json:"key_id,omitempty"`
	Shards    []Shard   `json:"shards"`
	UpdatedAt time.Time `json:"updated_at"`
}

// holds reports whether the pack lists a file.
func (p *ThumbPack) holds(id string) bool {
	for _, held := range p.IDs {
		if held == id {
			return true
		}
	}
	return false
}

// ThumbIDs returns the files in a folder that have a stored thumbnail. It
// answers from the index alone — no account is contacted.
func (v *Vault) ThumbIDs(scope Scope, dir string) []string {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil
	}
	pack := m.Thumbs[CleanDir(dir)]
	if pack == nil {
		return nil
	}
	return append([]string(nil), pack.IDs...)
}

// thumbIDsForLocked collects the thumbnails stored for a set of entries that
// may span folders, which is what a search result is. The caller must hold at
// least the read lock, and the entries must all come from the manifest passed
// in — a pack belongs to the vault whose index names it.
func (v *Vault) thumbIDsForLocked(m *Manifest, entries []*Entry) []string {
	var out []string
	for _, e := range entries {
		pack := m.Thumbs[e.Dir]
		if pack != nil && pack.holds(e.ID) {
			out = append(out, e.ID)
		}
	}
	return out
}

// Thumb returns a file's stored thumbnail, gathering its folder's pack from
// the accounts if this is the first time it has been asked for.
func (v *Vault) Thumb(ctx context.Context, id string) ([]byte, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	scope, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	dir := entry.Dir
	pack := m.Thumbs[dir]
	v.mu.RUnlock()

	if pack == nil || !pack.holds(id) {
		return nil, ErrNoThumb
	}

	items, err := v.loadPack(ctx, scope, dir)
	if err != nil {
		return nil, err
	}
	thumb, ok := items[id]
	if !ok {
		// The index says there is one and the pack disagrees. The pack is the
		// copy that can actually be drawn, so it wins.
		return nil, ErrNoThumb
	}
	return thumb, nil
}

// SetThumb stores a thumbnail for a file, replacing any it already had. The
// bytes are stored as given: normalizing them to a small image of a known
// format is the caller's job, and happens before this is reached.
func (v *Vault) SetThumb(ctx context.Context, id string, thumb []byte) error {
	if len(thumb) == 0 {
		return fmt.Errorf("thumbnail is empty")
	}
	if len(thumb) > MaxThumbBytes {
		return fmt.Errorf("thumbnail is %d bytes, over the %d-byte limit", len(thumb), MaxThumbBytes)
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return ErrLocked
	}
	scope, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.RUnlock()
		return fmt.Errorf("no such file: %s", id)
	}
	dir := entry.Dir
	v.mu.RUnlock()

	items, err := v.loadPack(ctx, scope, dir)
	if err != nil {
		// An unreadable pack is not a reason to refuse a new thumbnail: start
		// a fresh one and let the rest be made again as files are opened.
		items = map[string][]byte{}
	}

	next := make(map[string][]byte, len(items)+1)
	for k, val := range items {
		next[k] = val
	}
	next[id] = thumb
	v.trimPack(scope, dir, next, id)

	return v.savePack(ctx, scope, dir, next)
}

// SetThumbs stores a batch of thumbnails at once, all of them for files in the
// same folder, and writes that folder's pack a single time.
//
// SetThumb in a loop would be correct and ruinous. A pack is one stored object:
// each call gathers it, adds one picture, and scatters the whole thing again
// across three accounts — so matching a folder of two hundred films against the
// film database would upload the growing pack two hundred times, which is
// quadratic in the size of the folder and measured in tens of gigabytes. This
// pays that cost once.
//
// Files outside dir are skipped rather than refused: a caller sweeping a tree
// has already grouped them, and one stray entry should not throw away a folder
// of good pictures.
func (v *Vault) SetThumbs(ctx context.Context, scope Scope, dir string, thumbs map[string][]byte) error {
	if len(thumbs) == 0 {
		return nil
	}
	dir = CleanDir(dir)

	v.mu.RLock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return err
	}
	wanted := make(map[string][]byte, len(thumbs))
	for id, data := range thumbs {
		if len(data) == 0 || len(data) > MaxThumbBytes {
			continue
		}
		if entry := m.ByID(id); entry != nil && entry.Dir == dir {
			wanted[id] = data
		}
	}
	v.mu.RUnlock()

	if len(wanted) == 0 {
		return nil
	}

	items, err := v.loadPack(ctx, scope, dir)
	if err != nil {
		// An unreadable pack is not a reason to refuse new pictures; the rest
		// are made again as files are opened. Same rule as SetThumb.
		items = map[string][]byte{}
	}

	next := make(map[string][]byte, len(items)+len(wanted))
	for k, val := range items {
		next[k] = val
	}
	var last string
	for id, data := range wanted {
		next[id] = data
		last = id
	}
	v.trimPack(scope, dir, next, last)

	return v.savePack(ctx, scope, dir, next)
}

// trimPack keeps a pack under the entry ceiling, dropping the thumbnails of
// whichever files are no longer in the folder first and then the ones the
// folder lists last. keep is never dropped — it is the one just stored.
func (v *Vault) trimPack(scope Scope, dir string, items map[string][]byte, keep string) {
	if len(items) <= maxPackEntries {
		return
	}

	v.mu.RLock()
	var files []*Entry
	if m, err := v.manifestForLocked(scope); err == nil {
		_, files = m.Children(dir)
	}
	v.mu.RUnlock()

	order := make([]string, 0, len(items))
	present := map[string]bool{}
	for _, e := range files {
		present[e.ID] = true
	}
	// Files that have left the folder go first, then the folder's own in
	// listing order, so what survives is the start of what is on screen.
	for id := range items {
		if !present[id] {
			order = append(order, id)
		}
	}
	sort.Strings(order)
	for i := len(files) - 1; i >= 0; i-- {
		if _, ok := items[files[i].ID]; ok {
			order = append(order, files[i].ID)
		}
	}

	for _, id := range order {
		if len(items) <= maxPackEntries {
			return
		}
		if id == keep {
			continue
		}
		delete(items, id)
	}
}

// removeThumbs drops a set of files from their folders' packs. It is what
// deleting or moving a file calls, and it never fails the operation that asked
// for it: a stale thumbnail is a cosmetic problem, and the next write of the
// pack clears it anyway.
func (v *Vault) removeThumbs(ctx context.Context, scope Scope, dir string, ids ...string) {
	if len(ids) == 0 {
		return
	}

	v.mu.RLock()
	pack := (*ThumbPack)(nil)
	if m, err := v.manifestForLocked(scope); err == nil {
		pack = m.Thumbs[CleanDir(dir)]
	}
	v.mu.RUnlock()
	if pack == nil {
		return
	}

	wanted := false
	for _, id := range ids {
		if pack.holds(id) {
			wanted = true
			break
		}
	}
	if !wanted {
		return
	}

	items, err := v.loadPack(ctx, scope, dir)
	if err != nil {
		v.dropPack(ctx, scope, dir)
		return
	}
	next := make(map[string][]byte, len(items))
	for k, val := range items {
		next[k] = val
	}
	for _, id := range ids {
		delete(next, id)
	}
	if err := v.savePack(ctx, scope, dir, next); err != nil {
		v.dropPack(ctx, scope, dir)
	}
}

// moveThumb carries a file's thumbnail from one folder's pack to another's,
// so renaming a file does not cost it its picture.
func (v *Vault) moveThumb(ctx context.Context, scope Scope, id, from, to string) {
	from, to = CleanDir(from), CleanDir(to)
	if from == to {
		return
	}

	v.mu.RLock()
	pack := (*ThumbPack)(nil)
	if m, err := v.manifestForLocked(scope); err == nil {
		pack = m.Thumbs[from]
	}
	v.mu.RUnlock()
	if pack == nil || !pack.holds(id) {
		return
	}

	items, err := v.loadPack(ctx, scope, from)
	if err != nil {
		return
	}
	thumb, ok := items[id]
	if !ok {
		return
	}

	// Stored in its new home first: a thumbnail that exists in both places for
	// a moment is invisible, and one that exists in neither is a lost picture.
	if err := v.SetThumb(ctx, id, thumb); err != nil {
		return
	}
	v.removeThumbs(ctx, scope, from, id)
}

// dropThumbFolders erases the packs of a folder and everything under it.
func (v *Vault) dropThumbFolders(ctx context.Context, scope Scope, dir string) {
	dir = CleanDir(dir)
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}

	v.mu.RLock()
	var doomed []string
	if m, err := v.manifestForLocked(scope); err == nil {
		for stored := range m.Thumbs {
			if stored == dir || len(stored) > len(prefix) && stored[:len(prefix)] == prefix {
				doomed = append(doomed, stored)
			}
		}
	}
	v.mu.RUnlock()

	for _, stored := range doomed {
		v.dropPack(ctx, scope, stored)
	}
}

// dropAllThumbs erases every pack. A password change calls it: the packs are
// sealed under the key being retired, and re-encrypting derived data is work
// that regenerating it does for free.
func (v *Vault) dropAllThumbs(ctx context.Context, scope Scope) {
	v.mu.RLock()
	var dirs []string
	if m, err := v.manifestForLocked(scope); err == nil {
		for dir := range m.Thumbs {
			dirs = append(dirs, dir)
		}
	}
	v.mu.RUnlock()

	for _, dir := range dirs {
		v.dropPack(ctx, scope, dir)
	}
}

// loadPack returns a folder's thumbnails, from memory if they have been read
// already and from the accounts otherwise.
//
// The cached copy is decrypted, so it is held in memory only and cleared when
// the vault locks — the same rule the rebuilt file content follows.
func (v *Vault) loadPack(ctx context.Context, scope Scope, dir string) (map[string][]byte, error) {
	dir = CleanDir(dir)
	if items, ok := v.cachedPack(scope, dir); ok {
		return items, nil
	}

	v.mu.RLock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	pack := m.Thumbs[dir]
	v.mu.RUnlock()

	if pack == nil {
		return map[string][]byte{}, nil
	}

	// One gather at a time across the whole vault. Opening a folder asks for
	// every row's thumbnail at once, and they all want this same pack: without
	// this they would each fetch it.
	v.thumbLoad.Lock()
	defer v.thumbLoad.Unlock()

	if items, ok := v.cachedPack(scope, dir); ok {
		return items, nil
	}

	blob, err := v.gather(ctx, pack.Shards, archive.LegacyScheme(), pack.KeyID, "the thumbnails for "+dir)
	if err != nil {
		return nil, err
	}
	items, err := decodePack(blob)
	if err != nil {
		return nil, fmt.Errorf("reading the thumbnails for %s: %w", dir, err)
	}

	v.cacheThumbs(scope, dir, items)
	return items, nil
}

// savePack scatters a folder's thumbnails as one archive and points the index
// at it, erasing whatever the folder's previous pack was stored as.
func (v *Vault) savePack(ctx context.Context, scope Scope, dir string, items map[string][]byte) error {
	return v.savePackOn(ctx, scope, dir, items, nil)
}

// savePackOn is savePack onto a named set of accounts, which is what moving a
// folder to different clouds needs: a pack belongs to a folder rather than to
// any one file, so it does not follow a file's placement and has to be told.
//
// Empty accounts is the ordinary case — the vault's default, and failing that a
// pick of its own, exactly as an upload does.
//
// A pack is always two of three, whatever scheme the folder's files use, and is
// narrowed to three accounts here to keep it that way. It rides the whole-file
// format, which predates schemes and cannot express a wider code — and there is
// nothing to gain by teaching it one, because a thumbnail is a picture the
// browser can draw again from a file that is itself stored properly. What the
// narrowing does buy is that a folder on nine clouds does not fail to save its
// pictures.
func (v *Vault) savePackOn(ctx context.Context, scope Scope, dir string, items map[string][]byte, accounts []string) error {
	dir = CleanDir(dir)
	if len(items) == 0 {
		v.dropPack(ctx, scope, dir)
		return nil
	}
	if len(accounts) > archive.SchemeDefault.Total {
		accounts = accounts[:archive.SchemeDefault.Total]
	}

	placed, err := v.scatter(ctx, scope, thumbArchiveName, encodePack(items), accounts, len(accounts) > 0)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	pack := &ThumbPack{
		IDs:       ids,
		KeyID:     placed.keyID,
		Shards:    placed.shards,
		UpdatedAt: time.Now().UTC(),
	}

	v.mu.Lock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		v.deleteShards(context.WithoutCancel(ctx), placed.shards)
		return err
	}
	if m.Thumbs == nil {
		m.Thumbs = map[string]*ThumbPack{}
	}
	previous, had := m.Thumbs[dir]
	m.Thumbs[dir] = pack
	err = v.persistLocked()
	if err != nil {
		if had {
			m.Thumbs[dir] = previous
		} else {
			delete(m.Thumbs, dir)
		}
	}
	v.mu.Unlock()

	if err != nil {
		v.deleteShards(context.WithoutCancel(ctx), placed.shards)
		return err
	}

	// The old parts are unreferenced now. Erasing them is best-effort: the
	// index already points at the new ones, so a failure here leaves litter
	// rather than a broken pack.
	if had && previous != nil {
		v.deleteShards(context.WithoutCancel(ctx), previous.Shards)
	}

	v.cacheThumbs(scope, dir, items)
	return nil
}

// dropPack erases a folder's pack from the accounts and from the index.
func (v *Vault) dropPack(ctx context.Context, scope Scope, dir string) {
	dir = CleanDir(dir)

	v.mu.Lock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		return
	}
	pack, ok := m.Thumbs[dir]
	if !ok {
		v.mu.Unlock()
		v.forgetThumbs(scope, dir)
		return
	}
	delete(m.Thumbs, dir)
	if len(m.Thumbs) == 0 {
		m.Thumbs = nil
	}
	err = v.persistLocked()
	v.mu.Unlock()

	v.forgetThumbs(scope, dir)
	if err == nil && pack != nil {
		v.deleteShards(context.WithoutCancel(ctx), pack.Shards)
	}
}

// packKey names one pack in the memory cache. Two vaults inside the same file
// can each have a folder called /Photos holding different pictures, so the
// folder alone is not the identity of a pack — which vault it belongs to is
// half of it.
func packKey(scope Scope, dir string) string {
	return string(scope) + "\x00" + CleanDir(dir)
}

// cachedPack returns a folder's thumbnails if they are already in memory.
func (v *Vault) cachedPack(scope Scope, dir string) (map[string][]byte, bool) {
	v.thumbMu.Lock()
	defer v.thumbMu.Unlock()
	items, ok := v.thumbs[packKey(scope, dir)]
	return items, ok
}

// cacheThumbs remembers a folder's thumbnails for the rest of the session.
func (v *Vault) cacheThumbs(scope Scope, dir string, items map[string][]byte) {
	v.thumbMu.Lock()
	defer v.thumbMu.Unlock()
	if v.thumbs == nil {
		v.thumbs = map[string]map[string][]byte{}
	}
	v.thumbs[packKey(scope, dir)] = items
}

// forgetThumbs drops one folder from the memory cache.
func (v *Vault) forgetThumbs(scope Scope, dir string) {
	v.thumbMu.Lock()
	defer v.thumbMu.Unlock()
	delete(v.thumbs, packKey(scope, dir))
}

// forgetAllThumbs empties the memory cache. Locking the vault calls it: the
// cache holds decrypted pictures of the user's files.
func (v *Vault) forgetAllThumbs() {
	v.thumbMu.Lock()
	defer v.thumbMu.Unlock()
	v.thumbs = nil
}

// A pack is a flat sequence of records so that JPEG bytes travel as bytes.
// Encoding the map as JSON would base64 every picture on the way in, which
// costs a third more of everything that then has to be compressed, split,
// encrypted and sent.
//
//	record := idLen uint16 | id | dataLen uint32 | data
const (
	packIDLenSize   = 2
	packDataLenSize = 4
)

func encodePack(items map[string][]byte) []byte {
	ids := make([]string, 0, len(items))
	size := 0
	for id, data := range items {
		ids = append(ids, id)
		size += packIDLenSize + len(id) + packDataLenSize + len(data)
	}
	// Sorted, so that storing the same thumbnails twice produces the same
	// bytes and a re-upload is not mistaken for a change.
	sort.Strings(ids)

	out := make([]byte, 0, size)
	for _, id := range ids {
		out = binary.BigEndian.AppendUint16(out, uint16(len(id)))
		out = append(out, id...)
		out = binary.BigEndian.AppendUint32(out, uint32(len(items[id])))
		out = append(out, items[id]...)
	}
	return out
}

func decodePack(blob []byte) (map[string][]byte, error) {
	items := map[string][]byte{}
	for len(blob) > 0 {
		if len(blob) < packIDLenSize {
			return nil, fmt.Errorf("truncated thumbnail pack")
		}
		idLen := int(binary.BigEndian.Uint16(blob))
		blob = blob[packIDLenSize:]
		if len(blob) < idLen+packDataLenSize {
			return nil, fmt.Errorf("truncated thumbnail pack")
		}
		id := string(blob[:idLen])
		blob = blob[idLen:]

		dataLen := int(binary.BigEndian.Uint32(blob))
		blob = blob[packDataLenSize:]
		if len(blob) < dataLen {
			return nil, fmt.Errorf("truncated thumbnail pack")
		}
		items[id] = append([]byte(nil), blob[:dataLen]...)
		blob = blob[dataLen:]
	}
	return items, nil
}
