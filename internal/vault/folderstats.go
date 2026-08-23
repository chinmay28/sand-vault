package vault

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// What one folder is holding, in the handful of figures worth reading before
// doing anything to it.
//
// The browser knows how big each file in front of it is; what it cannot see is
// the weight of a folder, because that lives in the levels below. So the folder
// menu asks — how much is under here, in how many files, spread over how many
// accounts — and this answers, from one walk of the index. It is the `du -sh`
// of a folder plus the two things `du` cannot know: what the parts weigh once
// erasure coding has widened them, and which clouds they went to.
//
// Nothing is contacted and nothing is decrypted beyond what is already open,
// which is the same bargain Survey makes next door. The difference is the size
// of the answer: Survey names every file so the organizer can plan a move over
// them, and this counts them instead, because a menu header wants six numbers
// rather than ten thousand names.

// FolderStats is what sits at or below one folder.
type FolderStats struct {
	Path string `json:"path"`

	// Files and Bytes are everything at or below the folder, however deep —
	// the question a folder row raises and its own listing cannot answer.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	// Folders counts the folders strictly below this one. The folder itself is
	// not among them, the same way it is not among Survey's.
	Folders int `json:"folders"`

	// Stored is what the parts of those files weigh across every account, which
	// is larger than Bytes by whatever the erasure coding adds — a 2-of-3 file
	// is stored one and a half times over. It is the figure an account's free
	// space is spent in, so it is the honest answer to "what is this costing
	// me", and Bytes is the answer to "how much is in here".
	Stored int64 `json:"stored_bytes"`

	// Clouds are the accounts holding a part of something under this folder,
	// named for display and counted for the header. An account that has since
	// been disconnected still appears: its parts are still recorded as being
	// there, and a folder that reads as living on two clouds when one of them
	// is gone would be a comfortable lie.
	Clouds []string `json:"clouds"`

	// Degraded counts the files under here short of a part — the same reading
	// the vault-wide figure takes, narrowed to this folder.
	Degraded int `json:"degraded"`

	// Newest is when the most recently modified file under here changed, and is
	// absent for a folder holding no files at all. A pointer because that
	// absence has to survive the trip: a zero time.Time is encoded rather than
	// dropped, and the year 1 is not a date anybody wants read out to them.
	Newest *time.Time `json:"newest,omitempty"`
}

// FolderStats counts what is under a folder.
func (v *Vault) FolderStats(scope Scope, dir string) (*FolderStats, error) {
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

	out := &FolderStats{Path: dir, Clouds: []string{}}
	var newest time.Time
	for _, f := range m.AllFolders() {
		if below(f, dir) {
			out.Folders++
		}
	}

	// What each account is called now, so a renamed one is named by the name it
	// answers to rather than by the one it wore when the parts went up. A shard
	// naming an account this vault is no longer connected to keeps the name
	// recorded with it, which is the whole reason that name is recorded.
	connected := map[string]string{}
	for _, cfg := range v.providers {
		connected[cfg.ID] = cfg.Name
	}

	// Keyed by account rather than by the name shown, so two accounts that were
	// given the same label count as the two clouds they are.
	clouds := map[string]string{}
	for _, e := range m.Descendants(dir) {
		out.Files++
		out.Bytes += e.Size
		if e.ModifiedAt.After(newest) {
			newest = e.ModifiedAt
		}
		if e.Redundancy() < e.Scheme().Total {
			out.Degraded++
		}
		for _, sh := range e.Shards {
			out.Stored += sh.Size
			name := connected[sh.ProviderID]
			if name == "" {
				name = sh.ProviderName
			}
			if name == "" {
				name = sh.ProviderID
			}
			clouds[sh.ProviderID] = name
		}
	}

	if !newest.IsZero() {
		out.Newest = &newest
	}
	for _, name := range clouds {
		out.Clouds = append(out.Clouds, name)
	}
	sort.SliceStable(out.Clouds, func(i, j int) bool {
		return strings.ToLower(out.Clouds[i]) < strings.ToLower(out.Clouds[j])
	})
	return out, nil
}
