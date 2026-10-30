package vault

import (
	"fmt"
	"sort"
	"time"
)

// The files still on the previous key, one by one.
//
// RekeyStatus counts them — so many left, so many under each folder — and
// for a plan that is moving, counts are what a person watches. A plan that
// has stopped moving is different: seven files left, a status that says
// "taking the next file", and nothing happening, is a question the counts
// cannot answer. This lists the files the counts stand for, in the order the
// plan takes them, with where each one stands — moving, waiting its turn,
// left for last, held for a cloud that is not answering, or failed lately and
// waiting to be tried again, with why. Paged, because a vault that has just
// changed its password has every file on the previous key.

// Where a pending file stands in the plan: RekeyPendingFile.State.
const (
	// RekeyFileQueued is a file waiting its turn; the list's order is the
	// order the turns come in.
	RekeyFileQueued = "queued"
	// RekeyFileMoving is the file being re-encrypted right now.
	RekeyFileMoving = "moving"
	// RekeyFileDeferred is a file left for last by request (DeferRekeyFile).
	RekeyFileDeferred = "deferred"
	// RekeyFileHeld is a file the plan is leaving where it is because too
	// few of its parts are on accounts that answered the last health check;
	// HeldOn names the account it waits for.
	RekeyFileHeld = "held"
	// RekeyFileRetrying is a file the plan tried lately and could not move;
	// Reason says why and RetryAt when it is tried again.
	RekeyFileRetrying = "retrying"
	// RekeyFileOutside is a file outside the plan's folders, under a plan
	// that stops at them: nothing takes it until the plan is widened.
	RekeyFileOutside = "outside"
)

// RekeyPendingFile is one file still on the previous key.
type RekeyPendingFile struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Dir   string `json:"dir"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`

	// State is one of the RekeyFile constants above.
	State string `json:"state"`

	// HeldOn names the account a held file is waiting for.
	HeldOn string `json:"held_on,omitempty"`

	// Reason is why the last try on a retrying file failed, and RetryAt
	// when it is tried again. Both empty otherwise.
	Reason  string    `json:"reason,omitempty"`
	RetryAt time.Time `json:"retry_at,omitzero"`
}

// RekeyPendingPage is a window onto the pending files, in the order the plan
// takes them.
type RekeyPendingPage struct {
	Files  []RekeyPendingFile `json:"files"`
	Offset int                `json:"offset"`
	Limit  int                `json:"limit"`

	// Folder is the folder the page is confined to, "/" for the whole
	// vault; Total and Bytes are what every pending file under it comes
	// to, not only the page's.
	Folder string `json:"folder"`
	Total  int    `json:"total"`
	Bytes  int64  `json:"bytes"`
}

// DefaultRekeyPage is how many files a page holds when none is asked for,
// and MaxRekeyPage the ceiling on asking for more. A row is a path and a
// state, so a page is cheap; the ceiling is for the screen, not the wire.
const (
	DefaultRekeyPage = 50
	MaxRekeyPage     = 500
)

// PendingRekeyFiles lists the main vault's files still on the previous key
// under folder ("/" or "" for all of them), in the order the plan takes
// them: the plan's folders first in their order, smallest file first within
// each, then everything else smallest first, then the files left for last in
// the same order. With no plan, smallest first throughout. Files the plan is
// passing over — held, or failed lately — keep their place in the order and
// say why they are not moving.
//
// offset and limit window the list. A limit of zero takes the default,
// anything past MaxRekeyPage is trimmed to it, and an offset past the end is
// an empty page rather than an error: a file that moves while the list is
// open is allowed to shorten it. A folder that is not in the index is an
// error, so a mistyped one is not mistaken for an empty one.
func (v *Vault) PendingRekeyFiles(folder string, offset, limit int) (*RekeyPendingPage, error) {
	now := time.Now().UTC()
	// Before the index lock, never under it: see unreachableAccounts.
	down := v.unreachableAccounts()

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	folder = CleanDir(folder)
	if folder != "/" && !v.manifest.FolderExists(folder) {
		return nil, fmt.Errorf("no such folder: %s", folder)
	}
	if limit <= 0 {
		limit = DefaultRekeyPage
	}
	if limit > MaxRekeyPage {
		limit = MaxRekeyPage
	}
	if offset < 0 {
		offset = 0
	}

	page := &RekeyPendingPage{Files: []RekeyPendingFile{}, Offset: offset, Limit: limit, Folder: folder}
	var all []RekeyPendingFile
	for _, f := range v.pendingRekeyOrderLocked(now, down) {
		if !underDir(f.Dir, folder) {
			continue
		}
		page.Total++
		page.Bytes += f.Bytes
		all = append(all, f)
	}
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

// pendingRekeyOrderLocked is every pending file in the main vault, in the
// order nextRekeyLocked would reach them, each with its state. The caller
// holds at least the read lock.
func (v *Vault) pendingRekeyOrderLocked(now time.Time, down map[string]bool) []RekeyPendingFile {
	plan := v.manifest.Rekey

	v.rekeyMu.Lock()
	moving := v.rekey.currentID
	retry := make(map[string]time.Time, len(v.rekey.skip))
	for id, until := range v.rekey.skip {
		if now.Before(until) {
			retry[id] = until
		}
	}
	why := make(map[string]string, len(retry))
	for id := range retry {
		why[id] = v.rekey.why[id]
	}
	v.rekeyMu.Unlock()

	describe := func(e *Entry, outside bool) RekeyPendingFile {
		f := RekeyPendingFile{ID: e.ID, Path: e.Path(), Dir: e.Dir, Name: e.Name, Bytes: e.Size, State: RekeyFileQueued}
		switch {
		case e.ID == moving:
			f.State = RekeyFileMoving
		case outside:
			f.State = RekeyFileOutside
		case heldBy(e, down) != "":
			f.State = RekeyFileHeld
			f.HeldOn = heldBy(e, down)
			if cfg, ok := v.configForLocked(f.HeldOn); ok {
				f.HeldOn = cfg.Name
			}
		case !retry[e.ID].IsZero():
			f.State = RekeyFileRetrying
			f.Reason = why[e.ID]
			f.RetryAt = retry[e.ID]
		case plan.isDeferred(e.ID):
			f.State = RekeyFileDeferred
		}
		return f
	}

	// smallest first, then by path, which is the order pick takes them in.
	byTurn := func(files []*Entry) {
		sort.Slice(files, func(i, j int) bool {
			if files[i].Size != files[j].Size {
				return files[i].Size < files[j].Size
			}
			return files[i].Path() < files[j].Path()
		})
	}

	listed := map[string]bool{}
	var out []RekeyPendingFile
	// take appends the pending files under dir that are not listed yet and
	// match deferred, in their turn.
	take := func(dir string, deferred bool, outside bool) {
		var files []*Entry
		for _, e := range v.manifest.Entries {
			if e.KeyID == v.dataKeyID || listed[e.ID] || !underDir(e.Dir, dir) {
				continue
			}
			if plan.isDeferred(e.ID) != deferred {
				continue
			}
			files = append(files, e)
		}
		byTurn(files)
		for _, e := range files {
			listed[e.ID] = true
			out = append(out, describe(e, outside))
		}
	}

	if plan == nil {
		take("/", false, false)
		return out
	}
	for _, deferred := range []bool{false, true} {
		for _, f := range plan.Folders {
			take(f, deferred, false)
		}
		if !plan.OnlyFolders {
			take("/", deferred, false)
		}
	}
	// Under a plan that stops at its folders, the rest is outside it: listed
	// after everything the plan will take, deferred or not, since neither
	// moves before the plan is widened.
	for _, deferred := range []bool{false, true} {
		take("/", deferred, true)
	}
	return out
}
