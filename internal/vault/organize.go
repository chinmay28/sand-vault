package vault

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// What is under a folder, counted in one walk of the index.
//
// A file browser answers "what is in this folder"; tidying one up asks the
// other question — what is under it, how deep, how much of it is which kind of
// file, and which of the folders holding it are holding nothing at all. None of
// that can be assembled a level at a time without a request per level, and a
// folder somebody wants flattened is exactly the folder with the most levels.
// So it is one answer, and the browser plans every organizer tool from it:
// which files would come up, which folders would be left empty, how many .srt
// files are down there and what they come to.
//
// It reads and nothing else. Every change an organizer makes goes through the
// endpoints that already existed — move a file, delete a file, remove a folder —
// one item at a time, from the browser, so a run that stalls halfway has moved
// exactly what it says it moved and the rest is still where it was. That is the
// same bargain the bulk actions make (see web/src/components/BulkActions.jsx),
// and it is why there is no "flatten" endpoint here to half-succeed with no way
// to say which half.
//
// Nothing is contacted, nothing is decrypted beyond what is already open: the
// index is in memory, and this is a walk of it. It carries no shard placements
// and no thumbnails — a survey of ten thousand files is a list of names, and
// the listing endpoint the browser already calls per folder carries more per
// row than this does.

// SurveyFile is one file under the surveyed folder.
type SurveyFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Size int64  `json:"size"`

	// Ext is the filename's extension, lowercased and with its dot — ".jpg" —
	// and empty for a file that has none. It is worked out here rather than in
	// the browser so that grouping by kind, deleting by kind and counting by
	// kind are all the same answer to the same question.
	Ext string `json:"ext"`

	// Depth is how many folders down from the surveyed one this file sits: 0 is
	// a file already in it, which is the whole difference between "this folder"
	// and "everything under it" and between a file a flatten would move and one
	// it would leave alone.
	Depth int `json:"depth"`
}

// SurveyFolder is one folder under the surveyed one.
//
// Files and Total are both here because they answer different questions. A
// folder holding nothing directly is still not empty if something sits below
// it, and removing it would take that with it — Total is what says so, and a
// folder is safe to remove exactly when it is zero.
type SurveyFolder struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Depth int    `json:"depth"`

	// Files counts what is directly inside; Total counts everything at or
	// below, and Bytes is what that comes to.
	Files int   `json:"files"`
	Total int   `json:"total"`
	Bytes int64 `json:"bytes"`
}

// Survey is everything under one folder, as the index has it.
type Survey struct {
	Path string `json:"path"`

	// Files is every file at or below Path, shallowest first and alphabetical
	// within a folder — the order a flatten moves them in, so the progress a
	// run reports reads as a walk of the tree rather than as index order.
	Files []SurveyFile `json:"files"`

	// Folders is every folder strictly below Path, shallowest first. The
	// surveyed folder itself is not in it: it is the thing being tidied, never
	// one of the things a tidy may remove.
	Folders []SurveyFolder `json:"folders"`
}

// Survey walks a folder and everything beneath it.
func (v *Vault) Survey(scope Scope, dir string) (*Survey, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}

	dir = CleanDir(dir)
	if !m.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	out := &Survey{Path: dir, Files: []SurveyFile{}, Folders: []SurveyFolder{}}

	// The folders first, so every one of them is in the answer whether or not a
	// file ever lands in it — a folder created and never used is precisely what
	// the empty-folder tool exists to find, and counting only the folders files
	// are in would be counting the ones that are not empty.
	counts := map[string]*SurveyFolder{}
	for _, f := range m.AllFolders() {
		if !below(f, dir) {
			continue
		}
		entry := &SurveyFolder{Path: f, Name: path.Base(f), Depth: depthUnder(f, dir)}
		counts[f] = entry
		out.Folders = append(out.Folders, *entry)
	}

	for _, e := range m.Descendants(dir) {
		out.Files = append(out.Files, SurveyFile{
			ID:    e.ID,
			Name:  e.Name,
			Dir:   e.Dir,
			Size:  e.Size,
			Ext:   Extension(e.Name),
			Depth: depthUnder(e.Dir, dir),
		})

		// A file counts against the folder it is in and against every folder
		// between that one and the surveyed one, which is what makes Total the
		// question "would removing this take anything with it?".
		if c := counts[e.Dir]; c != nil {
			c.Files++
		}
		for at := e.Dir; below(at, dir); at = CleanDir(path.Dir(at)) {
			if c := counts[at]; c != nil {
				c.Total++
				c.Bytes += e.Size
			}
		}
	}

	// Written back over the copies handed out above, now that the walk has
	// counted them.
	for i := range out.Folders {
		out.Folders[i] = *counts[out.Folders[i].Path]
	}

	sort.SliceStable(out.Folders, func(i, j int) bool {
		if out.Folders[i].Depth != out.Folders[j].Depth {
			return out.Folders[i].Depth < out.Folders[j].Depth
		}
		return out.Folders[i].Path < out.Folders[j].Path
	})
	sort.SliceStable(out.Files, func(i, j int) bool {
		if out.Files[i].Depth != out.Files[j].Depth {
			return out.Files[i].Depth < out.Files[j].Depth
		}
		if out.Files[i].Dir != out.Files[j].Dir {
			return out.Files[i].Dir < out.Files[j].Dir
		}
		return strings.ToLower(out.Files[i].Name) < strings.ToLower(out.Files[j].Name)
	})
	return out, nil
}

// Extension is a filename's kind, lowercased and with its dot, or empty for a
// name that has none.
//
// A leading dot is a hidden file rather than an extension — ".gitignore" is not
// a ".gitignore file" — and neither is a name that ends in one.
func Extension(name string) string {
	ext := path.Ext(name)
	if ext == "" || ext == "." || ext == name {
		return ""
	}
	return strings.ToLower(ext)
}

// below reports whether a folder sits strictly beneath another.
func below(p, dir string) bool {
	if dir == "/" {
		return p != "/"
	}
	return strings.HasPrefix(p, dir+"/")
}

// depthUnder counts the folders between a path and the one being surveyed: 0 is
// the surveyed folder itself.
func depthUnder(p, dir string) int {
	if !below(p, dir) {
		return 0
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
	if rest == "" {
		return 0
	}
	return strings.Count(rest, "/") + 1
}
