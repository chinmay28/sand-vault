package vault

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The same file, twice, under one folder.
//
// A vault fills up the way a drawer does. The same photograph arrives from the
// phone and from the camera roll export; a folder is copied "just in case"
// before something is tried on it; a download that stalled is fetched again and
// the browser calls the second one "report (1).pdf". None of it is visible from
// a listing, because the copies are never side by side — that is precisely why
// they survived.
//
// So this asks the question three ways, from weakest evidence to strongest,
// because they are three different questions and a person clearing a drawer
// wants all three:
//
//   - By content — the same SHA-256, which is the same bytes. Not a guess: two
//     files with one hash are one file, whatever they are called and wherever
//     they sit.
//   - By size — the same number of bytes. It is the question somebody actually
//     asks out loud ("I have two of these, they're both 4.2 GB"), and it catches
//     the pair a hash cannot: a file stored before the vault recorded one. It is
//     also the weakest, and every group says whether the hashes back it up.
//   - By name — names alike enough that the difference is a copy marker, a
//     separator or a typo. This is the one that finds "IMG_0001.jpg" beside
//     "IMG_0001 (1).jpg", and the one that can be wrong, so it never stands
//     alone: each group carries the sizes and says whether the bytes agree.
//
// All three are read-only, and nothing here removes anything. Like the rest of
// the organizer (see organize.go) the answer is a plan the browser then runs
// over DELETE /api/files/{id} — or hands to the selection bar — one item at a
// time, so a run that stalls has erased exactly what it says it erased.
//
// Nothing is decrypted and no account is contacted: the index is in memory, and
// hashes and sizes have been in it since the files were stored.

// DuplicateFile is one copy in a group of them.
//
// It is a survey row with two things added. Hash is what separates a proof from
// a resemblance, and it is empty for a file stored before the vault recorded
// one — a group whose hashes are not all present and equal is a question rather
// than an answer, and says so. Keep marks the copy the group suggests surviving,
// so the dialog opens with everything but one copy of everything ticked.
type DuplicateFile struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Size  int64  `json:"size"`
	Ext   string `json:"ext"`
	Depth int    `json:"depth"`
	Hash  string `json:"hash"`
	Keep  bool   `json:"keep"`
}

// DuplicateGroup is one set of files that answer to the same thing.
type DuplicateGroup struct {
	// Key is what made them a group — a hash, a size, a reduced name. It is
	// what the group is addressed by rather than what it is described by: the
	// dialog names a group after the copy it suggests keeping.
	Key string `json:"key"`

	// Files are the copies, the suggested survivor first.
	Files []DuplicateFile `json:"files"`

	// Bytes is what the whole group occupies; Waste is what would come back if
	// every copy but the survivor went. They differ by exactly one file, and
	// Waste is the only one of the two worth putting on a button.
	Bytes int64 `json:"bytes"`
	Waste int64 `json:"waste"`

	// Certain reports that every file here carries the same non-empty hash —
	// that these are the same bytes and not merely the same length or a similar
	// name. It is the difference between "delete four of these" and "look at
	// these five".
	Certain bool `json:"certain"`
}

// DuplicateSet is one way of asking, with what it came to.
type DuplicateSet struct {
	Groups []DuplicateGroup `json:"groups"`

	// Files is how many files are in a group at all; Extra is how many would go
	// if each group kept one, and Waste is what those come to. Extra is the
	// number on the button and Files is the number in the list, and showing one
	// where the other belongs is how a tool like this frightens somebody.
	Files int   `json:"files"`
	Extra int   `json:"extra"`
	Waste int64 `json:"waste"`

	// Partial reports that the comparison ran out of budget before every pair
	// had been tried, so there may be more. Only name matching can set it —
	// hashes and sizes group in one pass — and a tool that quietly compared
	// some of a folder would be worse than one that compared none.
	Partial bool `json:"partial"`

	// Crowded counts the runs of names that were too alike in bulk to be copies
	// of anything — a naming scheme rather than a duplicate — and were broken
	// back apart into the exact matches inside them. Name matching only; see
	// maxNameKeys.
	Crowded int `json:"crowded"`
}

// Duplicates is all three questions about one folder, answered from one walk.
//
// All three rather than the one that was asked for, because they are cheap next
// to the walk itself and because switching between them is the whole of using
// this: a group that is only a size match is worth a second look, and finding
// that out should not cost another request.
type Duplicates struct {
	Path string `json:"path"`

	// Scanned is how many files were considered — every file at or below Path,
	// at any depth. Copies scattered over forty folders are the ones nobody
	// finds, so scattered is the only way this looks.
	Scanned int `json:"scanned"`

	Content DuplicateSet `json:"content"`
	Size    DuplicateSet `json:"size"`
	Name    DuplicateSet `json:"name"`
}

// Duplicates finds the copies under a folder, three ways.
func (v *Vault) Duplicates(scope Scope, dir string) (*Duplicates, error) {
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

	files := make([]DuplicateFile, 0)
	for _, e := range m.Descendants(dir) {
		files = append(files, DuplicateFile{
			ID:    e.ID,
			Name:  e.Name,
			Dir:   e.Dir,
			Size:  e.Size,
			Ext:   Extension(e.Name),
			Depth: depthUnder(e.Dir, dir),
			Hash:  e.Hash,
		})
	}

	return &Duplicates{
		Path:    dir,
		Scanned: len(files),
		Content: byContent(files),
		Size:    bySize(files),
		Name:    byName(files),
	}, nil
}

// byContent groups files that are the same bytes.
//
// A file with no recorded hash is not in the answer at all rather than being
// grouped with the other files that have none: "we do not know what either of
// these is" is not a reason to put them together. Those are exactly what the
// size question is for.
func byContent(files []DuplicateFile) DuplicateSet {
	by := map[string][]int{}
	order := []string{}
	for i, f := range files {
		if f.Hash == "" {
			continue
		}
		if _, seen := by[f.Hash]; !seen {
			order = append(order, f.Hash)
		}
		by[f.Hash] = append(by[f.Hash], i)
	}
	return collect(files, by, order, func(h string) string { return h })
}

// bySize groups files of the same length.
//
// Empty files are left out. Two files of nought bytes are the same bytes, and
// the hash question already says so with certainty; putting them here as well
// would fill the loosest of the three answers with the one match that tells
// nobody anything about what they are holding.
func bySize(files []DuplicateFile) DuplicateSet {
	by := map[string][]int{}
	order := []string{}
	for i, f := range files {
		if f.Size <= 0 {
			continue
		}
		key := strconv.FormatInt(f.Size, 10)
		if _, seen := by[key]; !seen {
			order = append(order, key)
		}
		by[key] = append(by[key], i)
	}
	return collect(files, by, order, func(k string) string { return k })
}

// collect turns buckets into groups, dropping every bucket holding one file —
// which is most of them, and none of which is a duplicate of anything.
func collect(files []DuplicateFile, by map[string][]int, order []string, key func(string) string) DuplicateSet {
	out := DuplicateSet{Groups: []DuplicateGroup{}}
	for _, k := range order {
		idx := by[k]
		if len(idx) < 2 {
			continue
		}
		group := DuplicateGroup{Key: key(k), Files: make([]DuplicateFile, 0, len(idx))}
		for _, i := range idx {
			group.Files = append(group.Files, files[i])
		}
		out.Groups = append(out.Groups, finish(group))
	}
	rank(&out)
	return out
}

// finish orders a group around the copy worth keeping and counts what the rest
// come to.
//
// The survivor is the shallowest copy, then the one with the plainest name, and
// then whichever sorts first — which is to say "report.pdf" in the folder you
// are standing in beats "report (2).pdf" three folders down, and the answer
// never depends on the order the index happened to be walked in. It is a
// suggestion and nothing more: every row can be ticked or unticked, and the
// keeper can be moved to any file in the group.
func finish(group DuplicateGroup) DuplicateGroup {
	sort.SliceStable(group.Files, func(i, j int) bool {
		a, b := group.Files[i], group.Files[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if len([]rune(a.Name)) != len([]rune(b.Name)) {
			return len([]rune(a.Name)) < len([]rune(b.Name))
		}
		if !strings.EqualFold(a.Name, b.Name) {
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		if a.Dir != b.Dir {
			return a.Dir < b.Dir
		}
		return a.ID < b.ID
	})

	group.Files[0].Keep = true
	group.Certain = group.Files[0].Hash != ""
	for _, f := range group.Files {
		group.Bytes += f.Size
		if f.Hash == "" || f.Hash != group.Files[0].Hash {
			group.Certain = false
		}
	}
	group.Waste = group.Bytes - group.Files[0].Size
	return group
}

// rank puts the groups in the order somebody clearing space reads them in —
// most reclaimable first, then most copies — and totals the set.
func rank(set *DuplicateSet) {
	sort.SliceStable(set.Groups, func(i, j int) bool {
		a, b := set.Groups[i], set.Groups[j]
		if a.Waste != b.Waste {
			return a.Waste > b.Waste
		}
		if len(a.Files) != len(b.Files) {
			return len(a.Files) > len(b.Files)
		}
		return a.Key < b.Key
	})
	for _, g := range set.Groups {
		set.Files += len(g.Files)
		set.Extra += len(g.Files) - 1
		set.Waste += g.Waste
	}
}

/* --- Names alike enough to be copies ---------------------------------- */

// How many pairs of distinct names may be compared before the search gives up
// and says so. Exact matching is one pass and never touches this; only the
// fuzzy pass over distinct reduced names can, and a folder large enough to hit
// it is a folder where the exact matches were the answer anyway.
const maxNameComparisons = 2_000_000

// How many distinct reduced names one run of near-matches may join before it
// stops being a set of copies.
//
// Similarity chains: a is one edit from b, b from c, c from d, and nothing
// stops the walk at d. In a folder of machine-made names — eight letters, no
// numbers, every one of them a letter away from a dozen others — that chain
// swallows the whole folder and offers it as one group of nine thousand
// duplicates, which is both wrong and the most alarming thing this dialog could
// possibly say. A run that wide is a naming scheme rather than a set of copies,
// so it is broken back into the names that matched exactly inside it — those
// are still real, and they are the ones somebody was looking for — and the fact
// that it happened is counted rather than swallowed.
//
// Only chains are capped. A folder holding "report (1).pdf" through
// "report (40).pdf" is one reduced name forty times over, not forty names in a
// chain, and it comes back whole.
const maxNameKeys = 8

// How far apart two reduced names may drift and still be called the same one.
// Nothing at all for a short name, because at four letters every word is one
// edit from another word.
func allowedEdits(n int) int {
	switch {
	case n >= 16:
		return 2
	case n >= 5:
		return 1
	default:
		return 0
	}
}

// The marks a copy is made with, stripped off the end of a name: the counter a
// browser or a file manager appends, and the word for what it did.
//
// The word has to be a word — preceded by a space, a dash or an underscore —
// so that a photocopy stays a photocopy rather than becoming a photo.
var copyMarks = []*regexp.Regexp{
	regexp.MustCompile(`\s*\((?:\d+|copy(?:\s*\d+)?)\)$`),
	regexp.MustCompile(`\s*\[(?:\d+|copy(?:\s*\d+)?)\]$`),
	regexp.MustCompile(`[\s\-_]+copy(?:\s*\d+)?$`),
}

// byName groups files whose names differ by no more than a copy is allowed to.
//
// Two passes. The first reduces every name to what a copy of it would share and
// groups the ones that reduce to the same thing, which is most of what there is
// to find: a counter in brackets, a different separator, a capital letter. The
// second walks the reduced names that remain and joins the ones a couple of
// edits apart, so a typo counts as a copy too.
//
// The reduction keeps the digits in a name apart from the letters and insists
// they match exactly, and that is the load-bearing part of the whole thing.
// IMG_0001.jpg and IMG_0002.jpg are one edit apart and are not copies of
// anything — they are the two photographs a folder of photographs is made of.
// Holiday 2023 and Holiday 2024 are the same trap. So the letters may drift and
// the numbers may not, and a name whose counter was a copy marker had that
// stripped before the numbers were read.
//
// The extension is part of the key, so film.mkv and film.srt are never a group:
// they are a film and its subtitles, which is the opposite of a duplicate.
func byName(files []DuplicateFile) DuplicateSet {
	// Every distinct reduced name, and which files reduced to it.
	by := map[string][]int{}
	order := []string{}
	for i, f := range files {
		key := nameKey(f.Name)
		if _, seen := by[key]; !seen {
			order = append(order, key)
		}
		by[key] = append(by[key], i)
	}

	joined, partial := joinNearNames(order)

	// Runs of near-matches that grew past what a set of copies can be, undone
	// back to the names that matched exactly.
	width := map[string]int{}
	for _, key := range order {
		width[joined[key]]++
	}
	crowded := 0
	for _, keys := range width {
		if keys > maxNameKeys {
			crowded++
		}
	}
	for _, key := range order {
		if width[joined[key]] > maxNameKeys {
			joined[key] = key
		}
	}

	// The keys, merged into whichever key leads their cluster, in the order
	// they were first met — so the answer does not depend on map iteration.
	merged := map[string][]int{}
	mergedOrder := []string{}
	for _, key := range order {
		lead := joined[key]
		if _, seen := merged[lead]; !seen {
			mergedOrder = append(mergedOrder, lead)
		}
		merged[lead] = append(merged[lead], by[key]...)
	}

	// The reduced name, written out so it reads — and so that no two groups
	// share a key, which two extensions of one stem otherwise would.
	set := collect(files, merged, mergedOrder, func(k string) string {
		ext, digits, skeleton := splitKey(k)
		if digits == "" {
			return skeleton + ext
		}
		return skeleton + " " + digits + ext
	})
	set.Partial = partial
	set.Crowded = crowded
	return set
}

// joinNearNames says, for every reduced name, which cluster it belongs to.
//
// Only names sharing an extension and a digit signature are ever compared, and
// within that only names whose lengths are close enough that the allowed number
// of edits could bridge them — sorted by length, so the moment a candidate is
// too long every candidate after it is too. That is what keeps a folder of ten
// thousand files from becoming fifty million comparisons; the budget is what
// catches the folder that manages it anyway.
func joinNearNames(keys []string) (map[string]string, bool) {
	lead := make(map[string]string, len(keys))
	for _, k := range keys {
		lead[k] = k
	}

	// Union-find over the keys, by way of the lead map: joining two clusters
	// rewrites every member of one to the other's lead. Clusters here are
	// small — a name with a dozen near-copies is already remarkable — so the
	// rewrite is cheaper than the bookkeeping that would avoid it.
	members := map[string][]string{}
	for _, k := range keys {
		members[k] = []string{k}
	}
	join := func(a, b string) {
		ra, rb := lead[a], lead[b]
		if ra == rb {
			return
		}
		// The smaller cluster is rewritten into the larger, which is what keeps
		// a chain of near-copies from costing a rewrite per link.
		if len(members[ra]) < len(members[rb]) {
			ra, rb = rb, ra
		}
		for _, m := range members[rb] {
			lead[m] = ra
		}
		members[ra] = append(members[ra], members[rb]...)
		delete(members, rb)
	}

	// Bucketed by everything that has to match exactly, then by length within
	// the bucket.
	buckets := map[string][]string{}
	for _, k := range keys {
		ext, digits, _ := splitKey(k)
		buckets[ext+"\x00"+digits] = append(buckets[ext+"\x00"+digits], k)
	}

	budget := maxNameComparisons
	partial := false
	for _, bucket := range buckets {
		sort.Slice(bucket, func(i, j int) bool {
			a, b := skeletonOf(bucket[i]), skeletonOf(bucket[j])
			if len(a) != len(b) {
				return len(a) < len(b)
			}
			return a < b
		})
		for i := 0; i < len(bucket); i++ {
			a := []rune(skeletonOf(bucket[i]))
			for j := i + 1; j < len(bucket); j++ {
				b := []rune(skeletonOf(bucket[j]))
				allowed := allowedEdits(min(len(a), len(b)))
				if len(b)-len(a) > allowed {
					// Sorted by length, so nothing further along can be nearer.
					break
				}
				if budget <= 0 {
					partial = true
					break
				}
				budget--
				if withinEdits(a, b, allowed) {
					join(bucket[i], bucket[j])
				}
			}
			if partial {
				break
			}
		}
	}
	return lead, partial
}

// nameKey reduces a filename to what a copy of it would share: its extension,
// the numbers in it, and the letters in it — in that order, so a key sorts and
// splits without being parsed.
func nameKey(name string) string {
	ext := Extension(name)
	stem := strings.ToLower(strings.TrimSuffix(name, ext))

	// Repeatedly, because a file copied twice is "report (1) (2)". Never down
	// to nothing: a file actually called "copy.txt" keeps its name.
	for {
		cut := stem
		for _, mark := range copyMarks {
			cut = mark.ReplaceAllString(cut, "")
		}
		cut = strings.TrimSpace(cut)
		if cut == stem || cut == "" {
			break
		}
		stem = cut
	}

	/* The numbers, as the runs of digits they appear in, and the letters, with
	   everything else thrown away.

	   Separators go because a separator is how a name was typed rather than
	   what it says: "vacation photo", "vacation-photo" and "VacationPhoto" are
	   one name and not three. The digit runs are read out in order and joined
	   by a mark of their own, so where the separators fell cannot change the
	   signature — "holiday 2023 12" and "holiday2023-12" are the same two
	   numbers — while "a1b2" and "a12b" are not. */
	var runs []string
	var run, letters strings.Builder
	flush := func() {
		if run.Len() > 0 {
			runs = append(runs, run.String())
			run.Reset()
		}
	}
	for _, r := range stem {
		switch {
		case r >= '0' && r <= '9':
			run.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '\'' || r == ',' || r == '(' || r == ')' || r == '[' || r == ']':
			flush()
		default:
			flush()
			letters.WriteRune(r)
		}
	}
	flush()

	return ext + "\x00" + strings.Join(runs, ".") + "\x00" + letters.String()
}

func splitKey(key string) (ext, digits, skeleton string) {
	parts := strings.SplitN(key, "\x00", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

func skeletonOf(key string) string {
	_, _, skeleton := splitKey(key)
	return skeleton
}

// withinEdits reports whether two reduced names are at most `allowed`
// insertions, deletions or substitutions apart.
//
// Levenshtein over two rows, stopped the moment a whole row is further out than
// the allowance — which is what makes running it over a folder affordable: the
// answer is almost always "no", and almost always after a few columns.
func withinEdits(a, b []rune, allowed int) bool {
	if allowed <= 0 {
		return string(a) == string(b)
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > allowed {
		return false
	}

	prev := make([]int, len(a)+1)
	curr := make([]int, len(a)+1)
	for i := range prev {
		prev[i] = i
	}
	for j := 1; j <= len(b); j++ {
		curr[0] = j
		best := curr[0]
		for i := 1; i <= len(a); i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[i] = min(min(curr[i-1]+1, prev[i]+1), prev[i-1]+cost)
			if curr[i] < best {
				best = curr[i]
			}
		}
		if best > allowed {
			return false
		}
		prev, curr = curr, prev
	}
	return prev[len(a)] <= allowed
}
