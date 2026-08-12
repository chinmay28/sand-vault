package vault

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

// Shard records where one encrypted part of a file was placed.
type Shard struct {
	Part         int    `json:"part"` // 1, 2 or 3
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
}

// Path is the full browser path of the entry.
func (e *Entry) Path() string { return JoinPath(e.Dir, e.Name) }

// Redundancy reports how many parts were successfully stored. Two is the
// minimum needed to reconstruct; three means one account can go dark.
func (e *Entry) Redundancy() int { return len(e.Shards) }

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
	Entries   []*Entry  `json:"entries"`
	Folders   []string  `json:"folders"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newManifest() *Manifest {
	return &Manifest{Entries: []*Entry{}, Folders: []string{}}
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

	var built string
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
			m.Folders = append(m.Folders, built)
		}
	}
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
