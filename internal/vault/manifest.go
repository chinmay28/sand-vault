package vault

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/movie"
)

// Shard records where one encrypted part of a file was placed.
//
// A chunked entry (see Entry.Chunked) places every chunk's matching part on the
// same account, so one Shard still describes one part of one file — Key names
// the object of chunk zero, the rest follow from ChunkShardKey, and Size is the
// total this part occupies across every chunk. Keeping placement per file
// rather than per chunk is what stops a large file adding thousands of records
// to the index, and it keeps the guarantees in §5 exactly as they were: the
// question of which accounts may hold a file is still answered once, for the
// file.
type Shard struct {
	Part         int    `json:"part"` // the shard number, 1..Entry.TotalShards
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"` // kept for display if the account is later removed
	ProviderKind string `json:"provider_kind"`
	Key          string `json:"key"`
	Size         int64  `json:"size"`
}

// Entry is one stored file in the browser's namespace.
type Entry struct {
	ID         string    `json:"id"`
	Dir        string    `json:"dir"`  // parent folder, always normalized ("/" or "/a/b")
	Name       string    `json:"name"` // display name within Dir
	Size       int64     `json:"size"` // original, pre-compression size
	Hash       string    `json:"hash"` // SHA-256 of the plaintext, hex
	MIME       string    `json:"mime"`
	ArchiveID  string    `json:"archive_id"`
	CreatedAt  time.Time `json:"created_at"`
	ModifiedAt time.Time `json:"modified_at"`
	Shards     []Shard   `json:"shards"`

	// KeyID names the data key generation this file's parts are sealed under.
	// It trails the vault's active generation only while a password change is
	// still re-encrypting; the empty string is the generation of a vault
	// written before keys could be rotated.
	KeyID string `json:"key_id,omitempty"`

	// ChunkSize and ChunkCount describe a file stored in the chunked format,
	// where each chunk is sealed on its own and can be opened without the rest.
	// Both are absent on a file stored whole, which is every file written before
	// the format existed — Chunked is the predicate to ask rather than either
	// field on its own.
	//
	// ChunkSize is the plaintext length of every chunk but the last, so the
	// chunk covering an offset is that offset divided by it. That is the whole
	// reason the size is fixed and recorded rather than inferred: a seek must
	// not have to consult a per-chunk index to find out where to look.
	ChunkSize  int64 `json:"chunk_size,omitempty"`
	ChunkCount int   `json:"chunk_count,omitempty"`

	// DataShards and TotalShards are the erasure code this file was cut with —
	// k and n, where any k of the n shards rebuild it (§4).
	//
	// They are per file rather than per vault because widening a vault does not
	// rewrite what is already in it: a vault that has grown from three clouds to
	// nine holds 2-of-3 files beside 6-of-9 ones, and each says which it is.
	// Absent on a file written before schemes existed, which is 2-of-3 — see
	// Scheme.
	DataShards  int `json:"data_shards,omitempty"`
	TotalShards int `json:"total_shards,omitempty"`
}

// Scheme is the erasure code this file's shards were cut with. A file recorded
// before the field existed is two of three, which is the only code the formats
// of the time could express.
func (e *Entry) Scheme() archive.Scheme {
	if e.DataShards <= 0 || e.TotalShards <= 0 {
		return archive.LegacyScheme()
	}
	return archive.Scheme{Data: e.DataShards, Total: e.TotalShards}
}

// Chunked reports whether the file is stored as independently readable chunks.
// A file stored whole has to be gathered in full before any of it can be read.
func (e *Entry) Chunked() bool { return e.ChunkCount > 0 && e.ChunkSize > 0 }

// ChunkIndexAt returns the chunk holding the given plaintext offset. A file
// stored whole is a single chunk covering all of it, which is the same way a
// whole-file part reports itself once read.
func (e *Entry) ChunkIndexAt(offset int64) int {
	if !e.Chunked() {
		return 0
	}
	return int(offset / e.ChunkSize)
}

// Path is the full browser path of the entry.
func (e *Entry) Path() string { return JoinPath(e.Dir, e.Name) }

// Redundancy reports how many of the file's shards were successfully stored.
// Scheme().Data of them rebuild it; a full set means the file can lose
// Scheme().Tolerance() accounts and still be read.
func (e *Entry) Redundancy() int { return distinctShards(e.Shards) }

// Recoverable reports whether enough shards are recorded to rebuild the file at
// all. It is a claim about the index rather than about the accounts — Health
// asks them.
func (e *Entry) Recoverable() bool { return e.Redundancy() >= e.Scheme().Data }

// Spare is how many shards the file could still lose and be readable.
func (e *Entry) Spare() int {
	spare := e.Redundancy() - e.Scheme().Data
	if spare < 0 {
		return 0
	}
	return spare
}

// distinctShards counts how many different shard numbers a set covers, which is
// the number that decides whether the thing they belong to can be rebuilt. A
// scatter never writes the same shard twice, so this is normally just the
// length; counting properly is what keeps a duplicated index row from reading
// as extra durability.
func distinctShards(shards []Shard) int {
	seen := map[int]bool{}
	for _, s := range shards {
		seen[s.Part] = true
	}
	return len(seen)
}

// Providers returns the distinct provider IDs holding parts of this file.
func (e *Entry) Providers() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range e.Shards {
		if !seen[s.ProviderID] {
			seen[s.ProviderID] = true
			out = append(out, s.ProviderID)
		}
	}
	return out
}

// Manifest is the vault's index: every stored file plus any folder the user
// created explicitly. It is serialized as JSON and only ever written to disk
// encrypted, because filenames and folder structure are themselves sensitive.
type Manifest struct {
	Entries []*Entry `json:"entries"`
	Folders []string `json:"folders"`

	// Thumbs points at the stored thumbnails, one pack per folder. It lives
	// here because it is exactly as sensitive as the rest of the index: which
	// files have a picture, and which folder they are in.
	Thumbs map[string]*ThumbPack `json:"thumbs,omitempty"`

	// MovieFolders names the folders whose videos are looked up against the
	// film database, and Movies is what those lookups found, by file ID. Both
	// are here for the same reason as Thumbs — which folder holds somebody's
	// films, and what those films are, is index rather than content. See
	// movies.go.
	MovieFolders map[string]*MovieFolder `json:"movie_folders,omitempty"`
	Movies       map[string]*movie.Info  `json:"movies,omitempty"`

	// Automations holds the standing instructions a folder was given, by folder
	// path: check what is under it on a schedule, and put back what is missing.
	// Here rather than beside the policy on disk because a policy names a
	// folder, and which folders somebody keeps and what they are called is the
	// index's business — the same reason the film-lookup switch is here. See
	// automation.go.
	Automations map[string]*Automation `json:"automations,omitempty"`

	// Repos records which stored files are mirrors of git repositories, by file
	// ID: the upstream they came from, and enough about what the bundle holds
	// to tell whether that upstream has moved. Here rather than on the Entry
	// for the same reason Movies is — most files are not this — and encrypted
	// for the same reason as everything else in the index: which repositories
	// somebody keeps a copy of is as revealing as which films they do. See
	// gitrepo.go.
	Repos map[string]*GitSource `json:"repos,omitempty"`

	// FolderArt records the picture a folder was told to wear, by folder path
	// and the ID of a file stored inside it. Only the choices made by hand are
	// here: a folder nobody has chosen for picks one of the films inside it and
	// stores nothing (see folderart.go).
	FolderArt map[string]string `json:"folder_art,omitempty"`
	// SubVaults is the main vault's record of the vaults inside it. It is only
	// ever populated on the main vault's own manifest — a sub vault does not
	// contain sub vaults — and it holds what the main password is allowed to
	// learn and no more. See SubVaultMeta.
	SubVaults []*SubVaultMeta `json:"sub_vaults,omitempty"`

	// AccountRemap is left behind by a recovery for the sub vaults it could not
	// open, mapping the account IDs of the vault that was lost onto the ones
	// reconnected here.
	//
	// A recovery rewrites every shard record to point at the account that
	// really holds the part, because reconnecting an account gives it a fresh
	// ID. It cannot do that inside a sub vault: the section is sealed, and the
	// password that opens it is not the one doing the recovering. So the
	// translation is written down instead, and applied the first time each sub
	// vault is opened — at which point its own index can be rewritten and its
	// files found. Entries are dropped once no shut sub vault could still need
	// them.
	AccountRemap map[string]string `json:"account_remap,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

func newManifest() *Manifest {
	return &Manifest{Entries: []*Entry{}, Folders: []string{}}
}

// normalize fills in the empty slices a decoded manifest may be missing, so
// that JSON consumers never have to tell "empty" from "null" and neither does
// any code here.
func (m *Manifest) normalize() {
	if m.Entries == nil {
		m.Entries = []*Entry{}
	}
	if m.Folders == nil {
		m.Folders = []string{}
	}
}

// SubVaultMeta is a sub vault as the main vault sees it: a name, when it was
// made, and an inventory of the objects it owns out on the accounts.
//
// Everything here is sealed under the main vault key, which makes it the
// deliberate boundary of what a main password reveals about a sub vault. It
// reveals the name, the file count and where the parts sit. It reveals nothing
// about what any of those files are — no path, no filename, no size of any one
// file, no type.
//
// The inventory is the price of being able to clean up. Without it, deleting a
// sub vault whose password has been forgotten could only forget the record and
// leave its parts on the accounts for good, unattributable and undeletable,
// because the only list of them was inside the section nothing can open. It
// also lets the per-account usage figures stay honest while a sub vault is
// locked, rather than under-reporting by however much it holds.
type SubVaultMeta struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`

	// Inventory is every object this sub vault owns. It is derived from the
	// sub vault's own index on each write rather than maintained alongside it,
	// so it cannot drift: an open sub vault's inventory is rebuilt from what
	// its manifest actually says, and a locked one's is left exactly as the
	// last open moment left it.
	Inventory []InventoryItem `json:"inventory,omitempty"`
}

// InventoryItem is one stored archive — a file, or a folder's thumbnail pack —
// reduced to what is needed to erase it and to count it.
//
// The object keys are not recorded because they do not need to be: a key is
// derived from the archive ID, the chunk index and the part number, so those
// three facts regenerate every name this archive occupies. See ShardKey.
type InventoryItem struct {
	ArchiveID  string          `json:"archive_id"`
	ChunkCount int             `json:"chunk_count,omitempty"`
	Parts      []InventoryPart `json:"parts"`

	// DataShards and TotalShards are the erasure code this archive was cut
	// with, carried for the same reason an entry carries it: how many parts
	// have to survive is a fact about the file, not about the vault. Without it
	// the guard that refuses to disconnect an account holding the last copy of
	// something could only guess, and a 4-of-6 file and a 2-of-3 file guess
	// differently.
	DataShards  int `json:"data_shards,omitempty"`
	TotalShards int `json:"total_shards,omitempty"`
}

// Scheme is the erasure code this archive was cut with, falling back to the
// fixed one every file used before the code was recorded.
func (i InventoryItem) Scheme() archive.Scheme {
	if i.DataShards <= 0 || i.TotalShards <= 0 {
		return archive.LegacyScheme()
	}
	return archive.Scheme{Data: i.DataShards, Total: i.TotalShards}
}

// InventoryPart is where one part of an archive sits, and how much room it
// takes there.
type InventoryPart struct {
	Part       int    `json:"part"`
	ProviderID string `json:"provider_id"`
	Size       int64  `json:"size"`
}

// inventory reduces a manifest to the objects it owns on the accounts: every
// file, and every folder's thumbnail pack.
func (m *Manifest) inventory() []InventoryItem {
	items := make([]InventoryItem, 0, len(m.Entries)+len(m.Thumbs))

	add := func(archiveID string, chunkCount int, scheme archive.Scheme, shards []Shard) {
		if archiveID == "" {
			return
		}
		parts := make([]InventoryPart, 0, len(shards))
		for _, s := range shards {
			parts = append(parts, InventoryPart{Part: s.Part, ProviderID: s.ProviderID, Size: s.Size})
		}
		items = append(items, InventoryItem{
			ArchiveID:   archiveID,
			ChunkCount:  chunkCount,
			Parts:       parts,
			DataShards:  scheme.Data,
			TotalShards: scheme.Total,
		})
	}

	for _, e := range m.Entries {
		add(e.ArchiveID, e.ChunkCount, e.Scheme(), e.Shards)
	}
	// Sorted by folder, so that two runs over the same index produce the same
	// inventory and an unchanged sub vault does not churn the vault file.
	dirs := make([]string, 0, len(m.Thumbs))
	for dir := range m.Thumbs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if pack := m.Thumbs[dir]; pack != nil {
			// A pack is always cut with the fixed code, the way gather reads
			// it back — see loadPack.
			add(archiveIDOf(pack.Shards), 0, archive.LegacyScheme(), pack.Shards)
		}
	}
	return items
}

// archiveIDOf recovers the archive ID a set of shards belongs to. A thumbnail
// pack records its parts but not the archive they came from, and the key each
// part is stored under begins with it.
func archiveIDOf(shards []Shard) string {
	for _, s := range shards {
		if idx := strings.Index(s.Key, "-p"); idx > 0 {
			return s.Key[:idx]
		}
	}
	return ""
}

// SubVaultByID returns the metadata for one sub vault.
func (m *Manifest) SubVaultByID(id string) *SubVaultMeta {
	for _, meta := range m.SubVaults {
		if meta.ID == id {
			return meta
		}
	}
	return nil
}

// CleanDir normalizes a folder path to a rooted, slash-separated form with no
// trailing slash: "" and "/" both become "/", "a/b/" becomes "/a/b".
func CleanDir(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == "/" {
		return "/"
	}
	return cleaned
}

// JoinPath joins a normalized directory and a name.
func JoinPath(dir, name string) string {
	dir = CleanDir(dir)
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// SanitizeName rejects names that would break the flat path model or let a
// caller climb out of their folder.
func SanitizeName(name string) (string, error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	// Uploads from a browser can carry a relative path; keep only the leaf.
	name = path.Base(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "", fmt.Errorf("invalid name %q", name)
	}
	if strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("invalid name: contains a null byte")
	}
	return name, nil
}

// ByID returns the entry with the given ID.
func (m *Manifest) ByID(id string) *Entry {
	for _, e := range m.Entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// ByPath returns the entry at a full path.
func (m *Manifest) ByPath(full string) *Entry {
	full = CleanDir(full)
	dir, name := path.Split(full)
	for _, e := range m.Entries {
		if e.Dir == CleanDir(dir) && e.Name == name {
			return e
		}
	}
	return nil
}

// FolderExists reports whether a folder is present, either explicitly created
// or implied by a file stored beneath it.
func (m *Manifest) FolderExists(dir string) bool {
	dir = CleanDir(dir)
	if dir == "/" {
		return true
	}
	for _, f := range m.Folders {
		if f == dir {
			return true
		}
	}
	prefix := dir + "/"
	for _, e := range m.Entries {
		if e.Dir == dir || strings.HasPrefix(e.Dir, prefix) {
			return true
		}
	}
	return false
}

// Mkdir records a folder, creating every missing ancestor along the way.
func (m *Manifest) Mkdir(dir string) error {
	dir = CleanDir(dir)
	if dir == "/" {
		return nil
	}

	// Every segment is checked before any of them is kept: a path that fails
	// half way through would otherwise leave the folders above the failure in
	// the manifest, to be written out by whatever persists next.
	var built string
	var missing []string
	for _, segment := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if segment == "" {
			return fmt.Errorf("invalid folder path %q", dir)
		}
		if _, err := SanitizeName(segment); err != nil {
			return err
		}
		built += "/" + segment
		if m.ByPath(built) != nil {
			return fmt.Errorf("a file already exists at %s", built)
		}
		if !m.hasExplicitFolder(built) {
			missing = append(missing, built)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	m.Folders = append(m.Folders, missing...)
	sort.Strings(m.Folders)
	return nil
}

func (m *Manifest) hasExplicitFolder(dir string) bool {
	for _, f := range m.Folders {
		if f == dir {
			return true
		}
	}
	return false
}

// Children lists the immediate subfolders and files of a directory.
func (m *Manifest) Children(dir string) (folders []string, files []*Entry) {
	dir = CleanDir(dir)
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}

	seen := map[string]bool{}
	addChild := func(candidate string) {
		if candidate == dir || !strings.HasPrefix(candidate, prefix) {
			return
		}
		rest := strings.TrimPrefix(candidate, prefix)
		name := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			name = rest[:idx]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			folders = append(folders, name)
		}
	}

	for _, f := range m.Folders {
		addChild(f)
	}
	for _, e := range m.Entries {
		addChild(e.Dir)
		if e.Dir == dir {
			files = append(files, e)
		}
	}

	sort.Strings(folders)
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	return folders, files
}

// AllFolders lists every folder in the vault as a normalized path, root first.
//
// A folder counts whether it was created outright or exists only because a file
// sits somewhere beneath it — both are folders the browser can walk into, and so
// both are folders something can be moved into. Which is what this is for: the
// destination picker draws the whole tree from one answer rather than asking
// per level, and a tree is only usable if the branch a file made is on it.
func (m *Manifest) AllFolders() []string {
	seen := map[string]bool{"/": true}
	out := []string{"/"}

	add := func(dir string) {
		// Ancestors first would need a second pass; walking up and stopping at
		// the first folder already seen is the same set in one, because a
		// folder is only ever recorded together with everything above it.
		for d := CleanDir(dir); d != "/" && !seen[d]; d = CleanDir(path.Dir(d)) {
			seen[d] = true
			out = append(out, d)
		}
	}

	for _, f := range m.Folders {
		add(f)
	}
	for _, e := range m.Entries {
		add(e.Dir)
	}

	sort.Strings(out)
	return out
}

// Descendants returns every entry stored at or below a directory.
func (m *Manifest) Descendants(dir string) []*Entry {
	dir = CleanDir(dir)
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}

	var out []*Entry
	for _, e := range m.Entries {
		if e.Dir == dir || strings.HasPrefix(e.Dir, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// underFolder reports whether a path is the folder itself or sits beneath it,
// and rewrites it to sit under a new one.
func underFolder(p, oldDir, newDir string) (string, bool) {
	if p == oldDir {
		return newDir, true
	}
	if strings.HasPrefix(p, oldDir+"/") {
		return newDir + strings.TrimPrefix(p, oldDir), true
	}
	return p, false
}

// moveFolder rewrites every path at or under oldDir to sit under newDir, and
// returns a function that puts everything back.
//
// The undo exists because the caller has to persist afterwards and that write
// can fail. Doing the rewrite in memory first and rolling it back on failure is
// what keeps the two halves of the tree from ever disagreeing about their own
// name — the whole reason this is one operation rather than a loop over Move.
func (m *Manifest) moveFolder(oldDir, newDir string) func() {
	type moved struct {
		entry *Entry
		from  string
	}

	var changed []moved
	for _, e := range m.Entries {
		if to, ok := underFolder(e.Dir, oldDir, newDir); ok {
			changed = append(changed, moved{entry: e, from: e.Dir})
			e.Dir = to
		}
	}

	previousFolders := append([]string(nil), m.Folders...)
	folders := make([]string, 0, len(m.Folders)+1)
	for _, f := range m.Folders {
		to, _ := underFolder(f, oldDir, newDir)
		folders = append(folders, to)
	}
	// The destination is a folder now whether or not the source was ever
	// recorded as one — it may have existed only because files sat under it.
	folders = append(folders, newDir)
	m.Folders = dedupeFolders(folders)

	// A thumbnail pack records nothing about which folder it belongs to; the
	// folder is the key it is filed under. So the pictures travel with the
	// folder for the price of rewriting a map, with no network work at all.
	previousThumbs := m.Thumbs
	if len(m.Thumbs) > 0 {
		rekeyed := make(map[string]*ThumbPack, len(m.Thumbs))
		for dir, pack := range m.Thumbs {
			to, _ := underFolder(dir, oldDir, newDir)
			rekeyed[to] = pack
		}
		m.Thumbs = rekeyed
	}

	// The film-lookup setting is filed by folder in exactly the same way, and
	// moving a films folder must not quietly turn the lookup off for it. What
	// each film *is* needs no rewriting at all: those are filed by file ID,
	// which a move never changes.
	previousMovieFolders := m.MovieFolders
	if len(m.MovieFolders) > 0 {
		rekeyed := make(map[string]*MovieFolder, len(m.MovieFolders))
		for dir, folder := range m.MovieFolders {
			to, _ := underFolder(dir, oldDir, newDir)
			rekeyed[to] = folder
		}
		m.MovieFolders = rekeyed
	}

	// A folder's standing instructions are keyed by folder in the same way, and
	// renaming a folder must not quietly stop it being looked after.
	previousAutomations := m.Automations
	if len(m.Automations) > 0 {
		rekeyed := make(map[string]*Automation, len(m.Automations))
		for dir, auto := range m.Automations {
			to, _ := underFolder(dir, oldDir, newDir)
			rekeyed[to] = auto
		}
		m.Automations = rekeyed
	}

	// And so is the picture a folder was told to wear — the fourth map keyed by
	// folder rather than by file. Its values are file IDs, which a move never
	// changes, so only the keys need rewriting.
	previousFolderArt := m.FolderArt
	if len(m.FolderArt) > 0 {
		rekeyed := make(map[string]string, len(m.FolderArt))
		for dir, id := range m.FolderArt {
			to, _ := underFolder(dir, oldDir, newDir)
			rekeyed[to] = id
		}
		m.FolderArt = rekeyed
	}

	return func() {
		for _, m := range changed {
			m.entry.Dir = m.from
		}
		m.Folders = previousFolders
		m.Thumbs = previousThumbs
		m.MovieFolders = previousMovieFolders
		m.Automations = previousAutomations
		m.FolderArt = previousFolderArt
	}
}

// dedupeFolders sorts a folder list and drops repeats.
func dedupeFolders(folders []string) []string {
	sort.Strings(folders)
	out := folders[:0]
	for i, f := range folders {
		if i == 0 || f != folders[i-1] {
			out = append(out, f)
		}
	}
	return out
}

// add appends an entry, replacing any existing entry at the same path.
func (m *Manifest) add(entry *Entry) {
	for i, e := range m.Entries {
		if e.Dir == entry.Dir && e.Name == entry.Name {
			m.Entries[i] = entry
			return
		}
	}
	m.Entries = append(m.Entries, entry)
}

// remove drops an entry by ID and reports whether it was present.
func (m *Manifest) remove(id string) bool {
	for i, e := range m.Entries {
		if e.ID == id {
			m.Entries = append(m.Entries[:i], m.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// removeFolders drops a folder and all of its explicit subfolders.
func (m *Manifest) removeFolders(dir string) {
	dir = CleanDir(dir)
	prefix := dir + "/"
	kept := m.Folders[:0]
	for _, f := range m.Folders {
		if f == dir || strings.HasPrefix(f, prefix) {
			continue
		}
		kept = append(kept, f)
	}
	m.Folders = kept
}

// uniqueName returns name, or name with a " (2)"-style suffix if that path is
// already taken, matching how a desktop file manager handles a collision.
func (m *Manifest) uniqueName(dir, name string) string {
	if m.ByPath(JoinPath(dir, name)) == nil {
		return name
	}

	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if m.ByPath(JoinPath(dir, candidate)) == nil {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext)
}

// DetectMIME guesses a content type from the filename, falling back to
// sniffing the leading bytes of the plaintext.
func DetectMIME(name string, data []byte) string {
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(name))); ct != "" {
		return ct
	}
	if len(data) > 0 {
		limit := 512
		if len(data) < limit {
			limit = len(data)
		}
		return http.DetectContentType(data[:limit])
	}
	return "application/octet-stream"
}
