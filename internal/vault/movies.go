package vault

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/movie"
)

// Film details are stored in the index rather than beside the files, and turned
// on a folder at a time rather than everywhere.
//
// In the index, because that is where everything about a file that is not the
// file itself already lives: it is encrypted at rest, it is what a manifest
// backup carries, and a few hundred bytes of text per film next to the shard
// records for a 4 GB video is nothing. The poster is the one part that is not
// text, and it is not stored here at all — it becomes the file's thumbnail, so
// the list, the grid and the details view all draw it through the path every
// other picture in the vault already takes.
//
// A folder at a time, because a lookup is the only thing in SAND that talks to
// anyone but the user's own accounts. Nobody should discover after the fact
// that opening a folder sent the names of what is in it somewhere. So the
// setting is explicit, it names the folder it was made on, and it is inherited
// downwards — a films folder is a library, and libraries have folders inside
// them.

// MovieAPIKey returns the user's own key for the film database, or "" when
// none has been set — which is every vault until somebody sets one, and is why
// nothing can be looked up by accident.
func (v *Vault) MovieAPIKey() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil || v.settings == nil {
		return ""
	}
	return v.settings.MovieAPIKey
}

// SetMovieAPIKey stores the key, or clears it when given nothing.
//
// It is a credential, so it goes into the vault's sealed settings section
// rather than the manifest: the manifest is copied to every connected account
// as a recovery backup, and this has no business being on three clouds.
func (v *Vault) SetMovieAPIKey(key string) error {
	key = strings.TrimSpace(key)

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	if v.settings == nil {
		v.settings = &vaultSettings{}
	}
	if v.settings.MovieAPIKey == key {
		return nil
	}

	previous := v.settings.MovieAPIKey
	v.settings.MovieAPIKey = key
	if err := v.persistLocked(); err != nil {
		v.settings.MovieAPIKey = previous
		return err
	}
	return nil
}

// MovieFolder records that a folder's videos should be matched against the film
// database. It lives in the manifest, which is encrypted, because which folder
// somebody keeps their films in is as much a part of the index as the films.
type MovieFolder struct {
	// EnabledAt is when the setting was made, which is the only thing worth
	// recording beyond the fact of it.
	EnabledAt time.Time `json:"enabled_at"`
}

// MovieBrief is what a listing says about a matched file: enough to title a
// tile, and not the overview, the cast and the genres of every film in a folder
// of two hundred.
type MovieBrief struct {
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
}

// MovieLookup describes whether a folder's videos are matched, and where the
// setting that says so was made. A folder inside a matched folder is matched
// too, and the browser has to be able to say which folder to change it on.
type MovieLookup struct {
	Enabled bool `json:"enabled"`

	// Source is the folder carrying the setting. It is the folder itself when
	// the setting was made there, and an ancestor when it was inherited.
	Source string `json:"source,omitempty"`
}

// Inherited reports whether the setting came from a folder further up.
func (l MovieLookup) Inherited(dir string) bool {
	return l.Enabled && l.Source != CleanDir(dir)
}

// MovieLookupFor reports whether a folder's videos are matched against the film
// database, and which folder says so. It answers out of the index alone.
func (v *Vault) MovieLookupFor(scope Scope, dir string) MovieLookup {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return MovieLookup{}
	}
	return v.movieLookupLocked(m, CleanDir(dir))
}

// movieLookupLocked walks from the folder up to the root looking for the
// nearest setting, in the index the folder belongs to — a sub vault opts its
// own folders in, and never inherits a choice made on a main-vault folder
// that happens to share the path. The caller must hold at least the read
// lock.
func (v *Vault) movieLookupLocked(m *Manifest, dir string) MovieLookup {
	if len(m.MovieFolders) == 0 {
		return MovieLookup{}
	}
	for at := CleanDir(dir); ; {
		if _, ok := m.MovieFolders[at]; ok {
			return MovieLookup{Enabled: true, Source: at}
		}
		if at == "/" {
			return MovieLookup{}
		}
		if idx := strings.LastIndex(at, "/"); idx <= 0 {
			at = "/"
		} else {
			at = at[:idx]
		}
	}
}

// MovieFolders lists every folder the setting was made on, nearest the root
// first. Nothing but the settings view asks for this.
func (v *Vault) MovieFolders() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil
	}
	out := make([]string, 0, len(v.manifest.MovieFolders))
	for dir := range v.manifest.MovieFolders {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// SetMovieLookup turns film matching on or off for a folder and everything
// under it.
//
// Turning it off leaves the details that have already been stored exactly where
// they are. They were fetched once and they describe files that have not
// changed, so deleting them would only mean fetching them again — and the
// switch is about whether this vault talks to the database, not about whether
// it is allowed to remember what it was told. Forgetting a film is its own
// action, per file, in the details view.
func (v *Vault) SetMovieLookup(scope Scope, dir string, enabled bool) error {
	dir = CleanDir(dir)

	v.mu.Lock()
	defer v.mu.Unlock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return err
	}
	if !m.FolderExists(dir) {
		return fmt.Errorf("no such folder: %s", dir)
	}

	_, had := m.MovieFolders[dir]
	if had == enabled {
		return nil
	}

	if enabled {
		if m.MovieFolders == nil {
			m.MovieFolders = map[string]*MovieFolder{}
		}
		m.MovieFolders[dir] = &MovieFolder{EnabledAt: time.Now().UTC()}
	} else {
		delete(m.MovieFolders, dir)
		if len(m.MovieFolders) == 0 {
			m.MovieFolders = nil
		}
	}

	if err := v.persistLocked(); err != nil {
		// Put the setting back the way the file on disk still has it.
		if enabled {
			delete(m.MovieFolders, dir)
			if len(m.MovieFolders) == 0 {
				m.MovieFolders = nil
			}
		} else {
			if m.MovieFolders == nil {
				m.MovieFolders = map[string]*MovieFolder{}
			}
			m.MovieFolders[dir] = &MovieFolder{EnabledAt: time.Now().UTC()}
		}
		return err
	}
	return nil
}

// Movie returns what is known about a file's film, or nil when it has not been
// matched. The details live in the index of whichever vault holds the file —
// a sub vault's film titles belong inside its sealed section, never in the
// main manifest that is replicated to every account — and the ID resolves
// that the way it does everywhere else.
func (v *Vault) Movie(id string) *movie.Info {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil
	}
	scope, _, ok := v.scopeOfEntryLocked(id)
	if !ok {
		return nil
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil
	}
	return m.Movies[id]
}

// SetMovie records a match against a file.
func (v *Vault) SetMovie(id string, info *movie.Info) error {
	if info == nil {
		return v.ForgetMovie(id)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	scope, _, ok := v.scopeOfEntryLocked(id)
	if !ok {
		return fmt.Errorf("no such file: %s", id)
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return err
	}

	if m.Movies == nil {
		m.Movies = map[string]*movie.Info{}
	}
	previous, had := m.Movies[id]
	m.Movies[id] = info

	if err := v.persistLocked(); err != nil {
		if had {
			m.Movies[id] = previous
		} else {
			delete(m.Movies, id)
		}
		return err
	}
	return nil
}

// ForgetMovie drops a file's stored details. The poster is not touched: it is
// the file's thumbnail now, and a thumbnail is dealt with as a thumbnail.
func (v *Vault) ForgetMovie(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	// Whichever readable index holds the record. The file itself may already
	// be gone — forgetting a film does not require the film's file.
	var m *Manifest
	var previous *movie.Info
	had := false
	for _, candidate := range v.manifestsLocked() {
		if info, ok := candidate.Movies[id]; ok {
			m, previous, had = candidate, info, true
			break
		}
	}
	if !had {
		return nil
	}

	delete(m.Movies, id)
	if len(m.Movies) == 0 {
		m.Movies = nil
	}
	if err := v.persistLocked(); err != nil {
		if m.Movies == nil {
			m.Movies = map[string]*movie.Info{}
		}
		m.Movies[id] = previous
		return err
	}
	return nil
}

// movieBriefsForLocked collects the titles of whichever entries have been
// matched. The caller must hold at least the read lock.
func (v *Vault) movieBriefsForLocked(m *Manifest, entries []*Entry) map[string]MovieBrief {
	if len(m.Movies) == 0 {
		return nil
	}
	var out map[string]MovieBrief
	for _, e := range entries {
		info := m.Movies[e.ID]
		if info == nil {
			continue
		}
		if out == nil {
			out = make(map[string]MovieBrief, len(entries))
		}
		out[e.ID] = MovieBrief{Title: info.Title, Year: info.Year}
	}
	return out
}

// forgetMoviesLocked drops the stored details of a set of files. It is what
// deleting one — or a folder of them — calls, and it does not persist: the
// caller is already writing the index in the same breath.
func (m *Manifest) forgetMovies(ids ...string) {
	if len(m.Movies) == 0 {
		return
	}
	for _, id := range ids {
		delete(m.Movies, id)
	}
	if len(m.Movies) == 0 {
		m.Movies = nil
	}
}

// dropMovieFolders removes the setting from a folder and everything under it,
// which is what deleting the folder means. It does not persist, for the same
// reason as above.
func (m *Manifest) dropMovieFolders(dir string) {
	dir = CleanDir(dir)
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	for stored := range m.MovieFolders {
		if stored == dir || strings.HasPrefix(stored, prefix) {
			delete(m.MovieFolders, stored)
		}
	}
	if len(m.MovieFolders) == 0 {
		m.MovieFolders = nil
	}
}
