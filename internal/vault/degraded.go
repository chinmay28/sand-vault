package vault

import (
	"sort"
	"time"
)

// The files that went out short.
//
// A scatter writes n parts and a file needs k of them back (§4), so a file
// missing one part is still readable and still one cloud closer to not being.
// It happens when an account is refusing at the moment a file is uploaded, and
// it is not an error — the upload succeeded, the file is readable, and nothing
// on any screen goes red. Stats counts them, the panel says "4 files missing a
// spare part", and until now that was the end of it: the number named a problem
// and gave no way to reach the files it was counting.
//
// This is the list behind that number. Same rule as Stats.Degraded — fewer
// distinct parts recorded than the file's own scheme calls for — and the same
// scope, the main vault alone, so the count somebody clicks and the list they
// land in are the same set of files rather than two figures that disagree.
//
// It reads the index and nothing else. Whether the accounts really still hold
// what the index says they hold is a different question, asked per file by
// Health; this one is about parts that were never written, which the index is
// the authority on. And putting a missing part back is not here either: it is
// Relocate, which the browser already has a dialog for — a degraded file
// re-spread over a set of clouds that are answering comes back whole.

// DegradedFile is one file with fewer parts stored than its scheme calls for.
type DegradedFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Dir  string `json:"dir"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	MIME string `json:"mime"`

	// Shards is where the parts that were written did land, so the browser can
	// draw the same badges the file list draws and open the relocation dialog
	// on the clouds the file is already on.
	Shards []Shard `json:"shards"`

	// DataShards and TotalShards are the file's own k and n, named the way the
	// file listing names them so one reader in the browser answers for both.
	DataShards  int `json:"data_shards"`
	TotalShards int `json:"total_shards"`

	// Stored is how many of the n were written, Missing the rest, and Spare how
	// many more could still go without the file becoming unreadable.
	Stored  int `json:"stored"`
	Missing int `json:"missing"`
	Spare   int `json:"spare"`

	// Readable says the index still records k parts. False is the serious case:
	// the file cannot be rebuilt from what is written down, and no amount of
	// moving it between clouds will change that.
	Readable bool `json:"readable"`

	ModifiedAt time.Time `json:"modified_at"`
}

// DegradedPage is one window onto that list.
//
// Paged because the failure that causes this is not one file at a time: an
// account that was down for an afternoon leaves every file uploaded that
// afternoon short a part, and a vault can come back from a bad day with
// thousands of them. Total, Bytes and Unreadable describe the whole list rather
// than the page, so the dialog can say what it is showing a slice of.
type DegradedPage struct {
	Files  []DegradedFile `json:"files"`
	Total  int            `json:"total"`
	Offset int            `json:"offset"`
	Limit  int            `json:"limit"`

	// Bytes is what every degraded file comes to, and Unreadable how many of
	// them are past the point of a spare part missing.
	Bytes      int64 `json:"bytes"`
	Unreadable int   `json:"unreadable"`
}

// DefaultDegradedPage is how many files a page holds when none is asked for,
// and MaxDegradedPage the ceiling on asking for more. Each row carries its
// file's whole placement, so a page is a good deal heavier than a row of the
// file listing and there is no reason to hand over ten thousand of them.
const (
	DefaultDegradedPage = 25
	MaxDegradedPage     = 200
)

// Degraded lists the files missing at least one part, worst first.
//
// The order is by how close to unreadable the file is rather than by name: the
// point of the list is to fix things, and a file down to its last usable part
// is the one to fix first. Files equally short of a full set are alphabetical
// within that, so the order is stable between calls and a page turned twice
// shows the same thing twice.
//
// offset and limit window it. A limit of zero takes the default; anything past
// MaxDegradedPage is trimmed to it, and an offset past the end is an empty page
// rather than an error — a file repaired while the dialog is open is allowed to
// shorten the list under it.
func (v *Vault) Degraded(offset, limit int) (*DegradedPage, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	if limit <= 0 {
		limit = DefaultDegradedPage
	}
	if limit > MaxDegradedPage {
		limit = MaxDegradedPage
	}
	if offset < 0 {
		offset = 0
	}

	page := &DegradedPage{Files: []DegradedFile{}, Offset: offset, Limit: limit}

	var all []DegradedFile
	for _, e := range v.manifest.Entries {
		scheme := e.Scheme()
		stored := e.Redundancy()
		if stored >= scheme.Total {
			continue
		}

		page.Bytes += e.Size
		if !e.Recoverable() {
			page.Unreadable++
		}

		all = append(all, DegradedFile{
			ID:          e.ID,
			Path:        e.Path(),
			Dir:         e.Dir,
			Name:        e.Name,
			Size:        e.Size,
			MIME:        e.MIME,
			Shards:      append([]Shard(nil), e.Shards...),
			DataShards:  scheme.Data,
			TotalShards: scheme.Total,
			Stored:      stored,
			Missing:     scheme.Total - stored,
			Spare:       e.Spare(),
			Readable:    e.Recoverable(),
			ModifiedAt:  e.ModifiedAt,
		})
	}
	page.Total = len(all)

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Spare != all[j].Spare {
			return all[i].Spare < all[j].Spare
		}
		if all[i].Missing != all[j].Missing {
			return all[i].Missing > all[j].Missing
		}
		return all[i].Path < all[j].Path
	})

	if offset >= len(all) {
		return page, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	page.Files = all[offset:end]
	return page, nil
}
