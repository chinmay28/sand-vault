package vault

import (
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strings"
)

// A folder is drawn with a picture of something inside it.
//
// A folder of films was a row of identical 📁 icons, which is the same problem
// the files inside it had before they got posters: the name of a folder holding
// The Dark Knight Trilogy tells you rather less than the poster of any film in
// it. So a folder borrows one — and borrowing is exactly what it does. Nothing
// new is stored: the answer here is the ID of a file that already has a
// thumbnail, and the browser draws it through the same endpoint it draws that
// file's row with. A folder's picture therefore costs no upload, no extra
// object on any account, and nothing at all to change.
//
// What it does cost is the thumbnail pack of the folder the picture lives in,
// gathered the first time it is drawn (§4.3's packs are one per folder). Opening
// a parent of twenty folders can therefore gather twenty packs — but only for
// the tiles actually on screen, since the browser loads them lazily, and only
// once, since a gathered pack is held in memory until the vault locks. It is the
// same cost as opening each of those folders in turn, paid where the folders are
// listed instead.
//
// The choice is the vault's until somebody makes it. Left alone it picks one of
// the films inside — stably, so a folder does not change its face on every
// refresh, and unpredictably enough that twenty folders do not all show whatever
// happens to sort first. Picked by hand, it is recorded in the manifest by file
// ID, which is why renaming the file, moving it deeper, or moving the whole
// folder somewhere else all leave the choice standing.

// FolderArt names the picture a folder is drawn with.
type FolderArt struct {
	// ID is the file whose stored thumbnail stands for the folder.
	ID string `json:"id"`

	// Film says the picture is a matched film's poster, which is two-by-three
	// rather than square — the grid has to know before it lays anything out.
	Film bool `json:"film,omitempty"`

	// Chosen says somebody picked this one. Otherwise the vault did, and it
	// will pick again if the file it chose goes away.
	Chosen bool `json:"chosen,omitempty"`
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

// FolderArtFor answers what one folder is drawn with, and whether it has
// anything to be drawn with at all.
func (v *Vault) FolderArtFor(dir string) (FolderArt, bool) {
	dir = CleanDir(dir)

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return FolderArt{}, false
	}
	art, ok := v.folderArtForLocked([]string{dir})[dir]
	return art, ok
}

// FolderArtChoices lists the pictures a folder could be drawn with: every file
// at or below it that has a stored thumbnail, films first.
//
// It reports whether the list was cut short, because a picker that silently
// showed the first three hundred of a thousand would be lying about what the
// folder holds.
func (v *Vault) FolderArtChoices(dir string) ([]ArtChoice, bool, error) {
	dir = CleanDir(dir)

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, false, ErrLocked
	}
	if !v.manifest.FolderExists(dir) {
		return nil, false, fmt.Errorf("no such folder: %s", dir)
	}

	thumbed := v.thumbIndexLocked()

	var out []ArtChoice
	for _, e := range v.manifest.Entries {
		if !underDir(e.Dir, dir) || !thumbed[e.Dir][e.ID] {
			continue
		}
		choice := ArtChoice{ID: e.ID, Name: e.Name, Dir: e.Dir}
		if info := v.manifest.Movies[e.ID]; info != nil {
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

// SetFolderArt fixes the picture a folder is drawn with. An empty id hands the
// choice back to the vault.
//
// The file has to be one inside the folder and it has to have a thumbnail:
// a folder's picture is a picture of what is in it, and one that pointed
// somewhere else would be a way to make a folder claim to hold something it
// does not.
func (v *Vault) SetFolderArt(dir, id string) (FolderArt, error) {
	dir, id = CleanDir(dir), strings.TrimSpace(id)

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return FolderArt{}, ErrLocked
	}
	if !v.manifest.FolderExists(dir) {
		return FolderArt{}, fmt.Errorf("no such folder: %s", dir)
	}

	previous, had := v.manifest.FolderArt[dir]
	if id == "" {
		if !had {
			return v.folderArtForLocked([]string{dir})[dir], nil
		}
		v.manifest.forgetFolderArtAt(dir)
	} else {
		if _, ok := v.chosenArtLocked(dir, id); !ok {
			return FolderArt{}, fmt.Errorf(
				"that file cannot stand for %s — a folder's picture has to be something stored inside it, with a picture of its own", dir)
		}
		if v.manifest.FolderArt == nil {
			v.manifest.FolderArt = map[string]string{}
		}
		v.manifest.FolderArt[dir] = id
	}

	if err := v.persistLocked(); err != nil {
		// Put the map back the way the file on disk still has it.
		if had {
			if v.manifest.FolderArt == nil {
				v.manifest.FolderArt = map[string]string{}
			}
			v.manifest.FolderArt[dir] = previous
		} else {
			v.manifest.forgetFolderArtAt(dir)
		}
		return FolderArt{}, err
	}
	return v.folderArtForLocked([]string{dir})[dir], nil
}

// artPick is the best candidate found for one folder so far.
type artPick struct {
	id    string
	film  bool
	score uint64
	found bool
}

// offer keeps the better of what is held and what is handed in.
//
// A film beats anything else, because a folder of films is what this exists
// for. Between two of the same kind the higher score wins, and the score is a
// hash of the folder and the file together — so the choice is fixed for as long
// as the folder holds that file, and two folders holding the same films do not
// both show the first one.
func (p *artPick) offer(dir, id string, film bool) {
	score := artScore(dir, id)
	if p.found && !betterArt(film, score, p.film, p.score) {
		return
	}
	p.id, p.film, p.score, p.found = id, film, score, true
}

// betterArt compares a candidate against what is already held.
func betterArt(film bool, score uint64, heldFilm bool, heldScore uint64) bool {
	if film != heldFilm {
		return film
	}
	return score > heldScore
}

func artScore(dir, id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(dir))
	h.Write([]byte{0})
	h.Write([]byte(id))
	return h.Sum64()
}

// folderArtForLocked resolves the picture for each of the folders named, in one
// walk of the index rather than one per folder.
//
// Every file is offered to each of the wanted folders that contains it, which
// is why the walk goes up from the file rather than down from the folder: a
// listing's folders are siblings and a search's are not, and a file deep under
// two of them should count for both. The caller must hold at least the read
// lock.
func (v *Vault) folderArtForLocked(dirs []string) map[string]FolderArt {
	if len(dirs) == 0 {
		return nil
	}

	wanted := make(map[string]*artPick, len(dirs))
	for _, dir := range dirs {
		wanted[CleanDir(dir)] = &artPick{}
	}

	thumbed := v.thumbIndexLocked()

	for _, e := range v.manifest.Entries {
		if !thumbed[e.Dir][e.ID] {
			continue
		}
		film := v.manifest.Movies[e.ID] != nil
		for at := CleanDir(e.Dir); ; {
			if pick, ok := wanted[at]; ok {
				pick.offer(at, e.ID, film)
			}
			if at == "/" {
				break
			}
			at = CleanDir(path.Dir(at))
		}
	}

	out := make(map[string]FolderArt, len(wanted))
	for dir, pick := range wanted {
		// A choice made by hand outranks the one made for it, as long as it
		// still points at something that is there.
		if chosen, ok := v.chosenArtLocked(dir, v.manifest.FolderArt[dir]); ok {
			out[dir] = chosen
			continue
		}
		if pick.found {
			out[dir] = FolderArt{ID: pick.id, Film: pick.film}
		}
	}
	return out
}

// chosenArtLocked reports whether a hand-picked file can still stand for a
// folder: it has to exist, sit inside it, and have a thumbnail to draw.
func (v *Vault) chosenArtLocked(dir, id string) (FolderArt, bool) {
	if id == "" {
		return FolderArt{}, false
	}
	e := v.manifest.ByID(id)
	if e == nil || !underDir(e.Dir, dir) || !v.hasThumbLocked(e) {
		return FolderArt{}, false
	}
	return FolderArt{ID: id, Film: v.manifest.Movies[id] != nil, Chosen: true}, true
}

// hasThumbLocked reports whether a file's folder pack holds a picture of it.
func (v *Vault) hasThumbLocked(e *Entry) bool {
	pack := v.manifest.Thumbs[e.Dir]
	return pack != nil && pack.holds(e.ID)
}

// thumbIndexLocked turns the packs into "does this file have a picture?" in one
// lookup, by folder and then by file.
//
// A pack keeps its IDs as a sorted list, which is the right shape for the index
// on disk and the wrong one for asking about every file in the vault: the walk
// below asks once per entry, and a linear scan of a 512-entry pack per question
// is the difference between a listing costing microseconds and milliseconds.
func (v *Vault) thumbIndexLocked() map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(v.manifest.Thumbs))
	for dir, pack := range v.manifest.Thumbs {
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
