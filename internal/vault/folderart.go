package vault

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// A folder can be given a picture of something inside it.
//
// A folder of films is otherwise a row of identical 📁 icons — the same problem
// the files inside it had before they got posters, since the name of a folder
// holding The Dark Knight Trilogy tells you rather less than the poster of any
// film in it. So a folder can borrow one, and borrowing is exactly what it does.
// Nothing new is stored: what is recorded is the ID of a file that already has a
// thumbnail, and the browser draws it through the same endpoint it draws that
// file's row with. A folder's picture therefore costs no upload, no extra object
// on any account, and nothing at all to change or to take away again.
//
// Nothing is picked automatically. A folder keeps its icon until somebody says
// otherwise, and saying so is one click on a list of what is actually in there.
// Guessing was the alternative and it is the wrong trade: which film stands for
// a trilogy is a matter of taste, a guess would have to be explained and undone
// rather than simply made, and a picture arriving by itself on a folder somebody
// never asked about is a surprise — the wrong kind, in an app whose whole
// posture is that nothing happens to your files unless you ask for it.
//
// What this file still works out for every folder is whether there is anything
// to offer at all, which is what decides whether the control to choose one is on
// screen. That is one walk of the index per listing and contacts no account.
//
// The picture itself costs the thumbnail pack of the folder holding it, gathered
// the first time it is drawn (§4.3's packs are one per folder). A parent whose
// folders have all been given pictures can therefore gather one pack per folder
// — but only for the tiles actually on screen, since the browser loads them
// lazily, and only once, since a gathered pack is held in memory until the vault
// locks. It is the same cost as opening each of those folders in turn, paid
// where the folders are listed instead.

// FolderArt is what a folder is drawn with.
type FolderArt struct {
	// ID is the file whose stored thumbnail stands for the folder, and is empty
	// when nobody has chosen one — which is every folder until somebody does.
	// An entry exists either way: a folder with nothing picturable inside it is
	// absent from the map entirely, and that is the difference between "no
	// picture yet" and "no picture possible".
	ID string `json:"id,omitempty"`

	// Film says the picture is a matched film's poster, which is two-by-three
	// rather than square — the grid has to know before it lays anything out.
	Film bool `json:"film,omitempty"`
}

// folderArtChoiceLimit caps how many files the picker is offered. A folder of
// ten thousand photographs is not a list anybody scrolls to the end of, and the
// films — which is what this is for — are sorted to the front of it.
const folderArtChoiceLimit = 300

// ArtChoice is one file a folder could be drawn with.
type ArtChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir"`

	// Title and Year are the film's, where the file has been matched to one.
	// A poster is picked out by the film's name, not the file's.
	Title string `json:"title,omitempty"`
	Year  int    `json:"year,omitempty"`
	Film  bool   `json:"film,omitempty"`
}

// FolderArtFor answers what one folder is drawn with, and whether it is drawn
// with anything at all.
func (v *Vault) FolderArtFor(scope Scope, dir string) (FolderArt, bool) {
	dir = CleanDir(dir)

	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return FolderArt{}, false
	}
	art := v.folderArtForLocked(m, []string{dir})[dir]
	return art, art.ID != ""
}

// FolderArtChoices lists the pictures a folder could be drawn with: every file
// at or below it that has a stored thumbnail, films first.
//
// It reports whether the list was cut short, because a picker that silently
// showed the first three hundred of a thousand would be lying about what the
// folder holds.
func (v *Vault) FolderArtChoices(scope Scope, dir string) ([]ArtChoice, bool, error) {
	dir = CleanDir(dir)

	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, false, err
	}
	if !m.FolderExists(dir) {
		return nil, false, fmt.Errorf("no such folder: %s", dir)
	}

	thumbed := v.thumbIndexLocked(m)

	var out []ArtChoice
	for _, e := range m.Entries {
		if !underDir(e.Dir, dir) || !thumbed[e.Dir][e.ID] {
			continue
		}
		choice := ArtChoice{ID: e.ID, Name: e.Name, Dir: e.Dir}
		if info := m.Movies[e.ID]; info != nil {
			choice.Title, choice.Year, choice.Film = info.Title, info.Year, true
		}
		out = append(out, choice)
	}

	// Films first and by title, because in the folder this is for they are the
	// answer; everything else falls in behind by path, which is the order the
	// browser would have shown it in anyway.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Film != b.Film {
			return a.Film
		}
		if a.Film {
			if !strings.EqualFold(a.Title, b.Title) {
				return strings.ToLower(a.Title) < strings.ToLower(b.Title)
			}
			return a.Year < b.Year
		}
		if a.Dir != b.Dir {
			return a.Dir < b.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	if len(out) > folderArtChoiceLimit {
		return out[:folderArtChoiceLimit], true, nil
	}
	return out, false, nil
}

// SetFolderArt gives a folder a picture, or takes it away again with an empty
// id — which is also the state every folder starts in.
//
// The file has to be one inside the folder and it has to have a thumbnail:
// a folder's picture is a picture of what is in it, and one that pointed
// somewhere else would be a way to make a folder claim to hold something it
// does not.
func (v *Vault) SetFolderArt(scope Scope, dir, id string) (FolderArt, error) {
	dir, id = CleanDir(dir), strings.TrimSpace(id)

	v.mu.Lock()
	defer v.mu.Unlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return FolderArt{}, err
	}
	if !m.FolderExists(dir) {
		return FolderArt{}, fmt.Errorf("no such folder: %s", dir)
	}

	previous, had := m.FolderArt[dir]
	if id == "" {
		if !had {
			return v.folderArtForLocked(m, []string{dir})[dir], nil
		}
		m.forgetFolderArtAt(dir)
	} else {
		if _, ok := v.chosenArtLocked(m, dir, id); !ok {
			return FolderArt{}, fmt.Errorf(
				"that file cannot stand for %s — a folder's picture has to be something stored inside it, with a picture of its own", dir)
		}
		if m.FolderArt == nil {
			m.FolderArt = map[string]string{}
		}
		m.FolderArt[dir] = id
	}

	if err := v.persistLocked(); err != nil {
		// Put the map back the way the file on disk still has it.
		if had {
			if m.FolderArt == nil {
				m.FolderArt = map[string]string{}
			}
			m.FolderArt[dir] = previous
		} else {
			m.forgetFolderArtAt(dir)
		}
		return FolderArt{}, err
	}
	return v.folderArtForLocked(m, []string{dir})[dir], nil
}

// folderArtForLocked answers, for each of the folders named, what it is drawn
// with — and, for the ones drawn with nothing, whether there is anything inside
// them to choose from. A folder with nothing picturable under it is left out of
// the map altogether, so a browser can tell "not chosen yet" from "nothing to
// choose".
//
// It is one walk of the index rather than one per folder, and every file is
// counted towards each of the wanted folders that contains it — which is why the
// walk goes up from the file rather than down from the folder: a listing's
// folders are siblings and a search's are not, and a file deep under two of them
// counts for both. The caller must hold at least the read lock.
func (v *Vault) folderArtForLocked(m *Manifest, dirs []string) map[string]FolderArt {
	if len(dirs) == 0 {
		return nil
	}

	wanted := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		wanted[CleanDir(dir)] = false
	}

	thumbed := v.thumbIndexLocked(m)

	for _, e := range m.Entries {
		if !thumbed[e.Dir][e.ID] {
			continue
		}
		for at := CleanDir(e.Dir); ; {
			if offered, ok := wanted[at]; ok && !offered {
				wanted[at] = true
			}
			if at == "/" {
				break
			}
			at = CleanDir(path.Dir(at))
		}
	}

	out := make(map[string]FolderArt, len(wanted))
	for dir, offered := range wanted {
		chosen, ok := v.chosenArtLocked(m, dir, m.FolderArt[dir])
		switch {
		case ok:
			out[dir] = chosen
		case offered:
			// Nothing chosen, but something to choose: the folder keeps its
			// icon and offers the control that changes that.
			out[dir] = FolderArt{}
		}
	}
	return out
}

// chosenArtLocked reports whether a hand-picked file can still stand for a
// folder: it has to exist, sit inside it, and have a thumbnail to draw.
func (v *Vault) chosenArtLocked(m *Manifest, dir, id string) (FolderArt, bool) {
	if id == "" {
		return FolderArt{}, false
	}
	e := m.ByID(id)
	if e == nil || !underDir(e.Dir, dir) || !v.hasThumbLocked(m, e) {
		return FolderArt{}, false
	}
	return FolderArt{ID: id, Film: m.Movies[id] != nil}, true
}

// hasThumbLocked reports whether a file's folder pack holds a picture of it.
func (v *Vault) hasThumbLocked(m *Manifest, e *Entry) bool {
	pack := m.Thumbs[e.Dir]
	return pack != nil && pack.holds(e.ID)
}

// thumbIndexLocked turns the packs into "does this file have a picture?" in one
// lookup, by folder and then by file.
//
// A pack keeps its IDs as a sorted list, which is the right shape for the index
// on disk and the wrong one for asking about every file in the vault: the walk
// below asks once per entry, and a linear scan of a 512-entry pack per question
// is the difference between a listing costing microseconds and milliseconds.
func (v *Vault) thumbIndexLocked(m *Manifest) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(m.Thumbs))
	for dir, pack := range m.Thumbs {
		ids := make(map[string]bool, len(pack.IDs))
		for _, id := range pack.IDs {
			ids[id] = true
		}
		out[dir] = ids
	}
	return out
}

// underDir reports whether a folder is the given one or sits beneath it.
func underDir(dir, root string) bool {
	if dir == root {
		return true
	}
	if root == "/" {
		return true
	}
	return strings.HasPrefix(dir, root+"/")
}

// forgetFolderArtAt drops one folder's choice. It does not persist: every
// caller is already writing the index in the same breath.
func (m *Manifest) forgetFolderArtAt(dir string) {
	delete(m.FolderArt, dir)
	if len(m.FolderArt) == 0 {
		m.FolderArt = nil
	}
}

// forgetFolderArt drops any choice that named one of these files, which is what
// deleting them means: the folder goes back to picking for itself.
func (m *Manifest) forgetFolderArt(ids ...string) {
	if len(m.FolderArt) == 0 {
		return
	}
	doomed := make(map[string]bool, len(ids))
	for _, id := range ids {
		doomed[id] = true
	}
	for dir, id := range m.FolderArt {
		if doomed[id] {
			delete(m.FolderArt, dir)
		}
	}
	if len(m.FolderArt) == 0 {
		m.FolderArt = nil
	}
}

// dropFolderArt drops the choices made on a folder and everything under it.
func (m *Manifest) dropFolderArt(dir string) {
	if len(m.FolderArt) == 0 {
		return
	}
	dir = CleanDir(dir)
	for stored := range m.FolderArt {
		if underDir(stored, dir) {
			delete(m.FolderArt, stored)
		}
	}
	if len(m.FolderArt) == 0 {
		m.FolderArt = nil
	}
}
