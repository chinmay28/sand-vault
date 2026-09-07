package vault

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Filing a folder by when its files were last changed.
//
// A folder that has been collected into rather than curated — a camera roll, a
// scanner's output folder, ten years of statements — has one shape and it is
// flat. Ten thousand names in one listing is not a folder anybody navigates; it
// is a folder people search and otherwise avoid. The one division that always
// applies to such a folder, and the only one that needs nothing said about the
// files, is when each of them was last written: /2026, or /2026/January.
//
// Like the rest of the organizer this reads and nothing else (see organize.go).
// It answers where every file would go and what each would be called when it
// got there, and the browser then runs that plan over the endpoints that
// already existed — create a folder, move a file, remove a folder — one item at
// a time, so a run that stalls halfway has moved exactly what it says it moved.
// There is no "sort by date" endpoint to half-succeed with no way to say which
// half.
//
// It is planned here rather than in the browser, which is where flattening is
// planned, because the answer is not a rearrangement of the survey: it needs
// each file's own modified time, a calendar, and a simulation of what the tree
// looks like afterwards to say which folders the sort would leave empty. All
// three are worth being able to test, and none of them is worth a second
// walk of the index in the browser to reproduce.
//
// Two properties are what make it safe to press:
//
//   - It is idempotent. A file already in the folder its date names is settled,
//     not moved, so sorting twice does nothing the second time and sorting a
//     half-sorted folder finishes it.
//   - No name is ever landed on. Every name a move takes is checked against
//     what is in the destination now and against every other file arriving
//     there, and names are never freed by the moves that vacate them — so the
//     plan holds whatever order it is run in, including the order a stalled
//     run left behind.

// DateGrain is how finely the year is divided.
type DateGrain string

const (
	// ByYear files into /2026.
	ByYear DateGrain = "year"
	// ByMonth files into /2026/January, which is the default.
	ByMonth DateGrain = "month"
)

// DateSortOptions is what to file, and how finely.
type DateSortOptions struct {
	// Grain is ByYear or ByMonth; empty is ByMonth.
	Grain DateGrain

	// Deep files every file under the folder rather than only the files
	// directly in it. Off is the flat-folder case the tool exists for: ten
	// thousand loose files, and the folders somebody has already made left
	// alone.
	Deep bool

	// Offset is the viewer's distance from UTC in minutes, east positive — the
	// negation of JavaScript's Date.getTimezoneOffset(). Times are stored in
	// UTC, and a file the browser shows as "31 December 23:40" must not be
	// filed under January because the server reads it as the first. One fixed
	// offset is not a time zone: a summer file filed from a winter browser can
	// land an hour out, which shows only for a file modified within an hour of
	// a month's end.
	Offset int
}

// DateMove is one file and where filing it by its date would put it.
type DateMove struct {
	ID   string `json:"id"`
	Name string `json:"name"` // what it is called now
	Dir  string `json:"dir"`  // where it is now
	Size int64  `json:"size"`

	// To is the folder it lands in and As is what it is called when it gets
	// there — the same name unless something in that folder already has it.
	To string `json:"to"`
	As string `json:"as"`

	// Modified is the time the folder was chosen from, as stored: UTC, before
	// the viewer's offset is applied.
	Modified time.Time `json:"modified"`
}

// DateFolder is one of the folders the sort files into.
type DateFolder struct {
	Path string `json:"path"`

	// Label is the path as it reads from the folder being sorted — "2026" or
	// "2026/January" — which is the whole of what the plan is worth showing.
	Label string `json:"label"`

	// Year, and Month as 1..12 or zero when filing by year alone. Both are
	// here so a client can order or group the folders without parsing Label.
	Year  int `json:"year"`
	Month int `json:"month"`

	// Files is how many files this sort puts or keeps here — the ones moving in
	// plus the ones already here under the right date — and Bytes what they
	// come to. A file already here that the sort is not looking at (a folder
	// sorted shallowly, holding a file below) is in neither.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	// Exists reports whether the folder is already there, which is the client's
	// cue to create it or not.
	Exists bool `json:"exists"`
}

// DatePlan is everything filing a folder by date would do, before any of it is
// done.
type DatePlan struct {
	Path   string `json:"path"`
	Grain  string `json:"grain"`
	Deep   bool   `json:"deep"`
	Offset int    `json:"offset"`

	// Folders is where the files go, oldest first, and Moves is which file goes
	// to which, in that same order and alphabetical within a folder — the order
	// a run should make them in, so progress reads as a calendar filling up.
	Folders []DateFolder `json:"folders"`
	Moves   []DateMove   `json:"moves"`

	// Bytes is what the moving files come to. Nothing travels: a file records
	// the folder it is in, and its parts stay where they are.
	Bytes int64 `json:"bytes"`

	// Settled counts the files already in the folder their date names, which is
	// what makes running this twice a no-op, and Undated the files stored with
	// no modified time at all. Neither is moved.
	Settled int `json:"settled"`
	Undated int `json:"undated"`

	// Emptied is every folder under the sorted one that would hold nothing at
	// all once the moves are done, deepest first — a folder whose only contents
	// were empty folders included. It is what an offer to clear up afterwards
	// removes, and it is never a folder the sort files into.
	Emptied []string `json:"emptied"`
}

// DateSort plans where every file under a folder would go, filed by the date it
// was last modified. It changes nothing.
func (v *Vault) DateSort(scope Scope, dir string, opts DateSortOptions) (*DatePlan, error) {
	grain, err := opts.grain()
	if err != nil {
		return nil, err
	}
	if opts.Offset < -1440 || opts.Offset > 1440 {
		return nil, fmt.Errorf("offset from UTC out of range: %d minutes", opts.Offset)
	}

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

	plan := &DatePlan{
		Path:    dir,
		Grain:   string(grain),
		Deep:    opts.Deep,
		Offset:  opts.Offset,
		Folders: []DateFolder{},
		Moves:   []DateMove{},
		Emptied: []string{},
	}

	// Every file under the folder, whether or not the sort is looking at it:
	// the ones out of scope still hold the names they hold, and a file landing
	// beside one must not be given its name.
	under := m.Descendants(dir)
	sort.SliceStable(under, func(i, j int) bool {
		if under[i].Dir != under[j].Dir {
			return under[i].Dir < under[j].Dir
		}
		return strings.ToLower(under[i].Name) < strings.ToLower(under[j].Name)
	})

	taken := map[string]map[string]bool{}
	claim := func(folder, name string) {
		if taken[folder] == nil {
			taken[folder] = map[string]bool{}
		}
		taken[folder][strings.ToLower(name)] = true
	}
	for _, e := range under {
		claim(e.Dir, e.Name)
	}

	// Where each file ends up, so the folders the sort empties can be counted
	// without walking the index again. A file that does not move is its own
	// answer.
	lands := map[string]string{}
	folders := map[string]*DateFolder{}

	for _, e := range under {
		lands[e.ID] = e.Dir
		if !opts.Deep && e.Dir != dir {
			continue
		}
		// A file stored with no time at all cannot be filed by one. It is left
		// exactly where it is and counted, rather than swept into a folder
		// named for a date nobody claimed.
		if e.ModifiedAt.IsZero() {
			plan.Undated++
			continue
		}

		when := e.ModifiedAt.UTC().Add(time.Duration(opts.Offset) * time.Minute)
		label := dateLabel(when, grain)
		target := CleanDir(path.Join(dir, label))

		entry := folders[target]
		if entry == nil {
			entry = &DateFolder{
				Path:   target,
				Label:  label,
				Year:   when.Year(),
				Exists: m.FolderExists(target),
			}
			if grain == ByMonth {
				entry.Month = int(when.Month())
			}
			folders[target] = entry
		}
		entry.Files++
		entry.Bytes += e.Size

		if e.Dir == target {
			plan.Settled++
			continue
		}

		// The name it lands under, against what is in the destination now and
		// against everything else arriving. Names are never released by the
		// moves that vacate them, so the plan is good in any order — including
		// the order a run that stopped halfway left the folder in.
		as := freeName(e.Name, taken[target])
		claim(target, as)

		lands[e.ID] = target
		plan.Bytes += e.Size
		plan.Moves = append(plan.Moves, DateMove{
			ID:       e.ID,
			Name:     e.Name,
			Dir:      e.Dir,
			Size:     e.Size,
			To:       target,
			As:       as,
			Modified: e.ModifiedAt.UTC(),
		})
	}

	for _, f := range folders {
		plan.Folders = append(plan.Folders, *f)
	}
	sort.Slice(plan.Folders, func(i, j int) bool { return earlier(plan.Folders[i], plan.Folders[j]) })

	order := map[string]int{}
	for i, f := range plan.Folders {
		order[f.Path] = i
	}
	sort.SliceStable(plan.Moves, func(i, j int) bool {
		if a, b := order[plan.Moves[i].To], order[plan.Moves[j].To]; a != b {
			return a < b
		}
		return strings.ToLower(plan.Moves[i].As) < strings.ToLower(plan.Moves[j].As)
	})

	plan.Emptied = emptiedBy(m, dir, under, lands)
	return plan, nil
}

// grain is the granularity asked for, defaulting to the month.
func (o DateSortOptions) grain() (DateGrain, error) {
	switch o.Grain {
	case "", ByMonth:
		return ByMonth, nil
	case ByYear:
		return ByYear, nil
	}
	return "", fmt.Errorf("unknown grain %q: want %q or %q", o.Grain, ByYear, ByMonth)
}

// dateLabel is the folder a time is filed under, relative to the folder being
// sorted: "2026" or "2026/January".
//
// The month is its English name rather than a number because the folder is for
// a person to read in a listing, and "2026/January" is the thing somebody asked
// for when they asked for this. It is deliberately not the viewer's language:
// the name is written into the vault and has to keep meaning the same folder
// when the vault is opened from somewhere else.
func dateLabel(when time.Time, grain DateGrain) string {
	year := strconv.Itoa(when.Year())
	if grain == ByYear {
		return year
	}
	return year + "/" + when.Month().String()
}

// earlier orders two date folders oldest first.
func earlier(a, b DateFolder) bool {
	if a.Year != b.Year {
		return a.Year < b.Year
	}
	return a.Month < b.Month
}

// freeName is the name a file lands under: its own, or the way a desktop file
// manager numbers one that is already spoken for. Compared without case, which
// is stricter than the index is — a folder holding both a.jpg and A.jpg is not
// a folder the sort has made easier to read.
func freeName(name string, taken map[string]bool) string {
	if !taken[strings.ToLower(name)] {
		return name
	}

	stem, ext := name, ""
	if dot := strings.LastIndex(name, "."); dot > 0 {
		stem, ext = name[:dot], name[dot:]
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !taken[strings.ToLower(candidate)] {
			return candidate
		}
	}
	return fmt.Sprintf("%s (%d)%s", stem, time.Now().UnixNano(), ext)
}

// emptiedBy is every folder under the sorted one left holding nothing at all
// once the moves are made, deepest first.
//
// Deepest first is not presentation: each folder is removed on its own and
// non-recursively, so a parent can only go after its children have, and a
// folder whose only contents were empty folders is empty too.
func emptiedBy(m *Manifest, dir string, under []*Entry, lands map[string]string) []string {
	holds := map[string]bool{}
	for _, e := range under {
		for at := lands[e.ID]; below(at, dir); at = CleanDir(path.Dir(at)) {
			holds[at] = true
		}
	}

	var out []string
	for _, f := range m.AllFolders() {
		if below(f, dir) && !holds[f] {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := depthUnder(out[i], dir), depthUnder(out[j], dir); a != b {
			return a > b
		}
		return out[i] < out[j]
	})
	if out == nil {
		return []string{}
	}
	return out
}
