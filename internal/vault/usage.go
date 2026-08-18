package vault

import (
	"context"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"
	"time"
)

// What an account is carrying, and what room it has left.
//
// The sidebar answers the first half in one line per account: parts held,
// bytes, and the quota bar where the backend reports one. This file is the
// other half — the same account taken apart, so "16.3 GB of 17.9 GB used" can
// say how much of that is SAND's doing and what the rest of it is.
//
// Everything here is read off the index. Nothing walks a drive or lists a
// bucket: the vault already knows which parts it put where and how big each
// one is, and a breakdown that cost a full traversal per account is one nobody
// would leave open.

// ProviderReport is one connected account examined on its own.
type ProviderReport struct {
	ProviderStatus

	// VaultShards and VaultStored are what every connected account holds
	// between them, so this account's share of the vault can be drawn without
	// asking a second time.
	VaultShards int   `json:"vault_shards"`
	VaultStored int64 `json:"vault_stored"`

	// Files counts the files with at least one part here, and Sole those that
	// could not be rebuilt without this account — the ones a disconnect would
	// strand. Both count every vault inside the file, since the question is
	// about the account rather than about what you can currently see.
	Files int `json:"files"`
	Sole  int `json:"sole"`

	// Kinds, Folders and Largest break the account's load down by what the
	// parts belong to, and Months says when they arrived.
	//
	// Deliberately from the main vault's index alone. A sub vault reveals its
	// name and its weight to the main password and nothing else about what is
	// inside it (see SubVaultMeta), and an open one would start naming its
	// folders here the moment somebody typed its password — in a panel opened
	// from the drawer, about an account, with no sign of which vault the row
	// came from. What every sub vault put on this account is one line instead.
	Kinds   []ProviderSlice `json:"kinds"`
	Folders []ProviderSlice `json:"folders"`
	Largest []ProviderItem  `json:"largest"`
	Months  []ProviderMonth `json:"months"`

	// SubVaults is what the vaults inside this one hold here, open or shut,
	// counted from their inventories and named by nothing.
	SubVaults ProviderSlice `json:"sub_vaults"`
}

// ProviderSlice is one row of a breakdown: what it is, how many parts of it
// sit on the account, and what they weigh there.
type ProviderSlice struct {
	Label string `json:"label"`
	Parts int    `json:"parts"`
	Bytes int64  `json:"bytes"`
}

// ProviderItem is one file, weighed by what it put on this account rather than
// by its own size — a file spread over six accounts leaves a sixth of itself
// on each.
type ProviderItem struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`  // the file itself, before splitting
	Bytes int64  `json:"bytes"` // what its parts weigh on this account
	Parts int    `json:"parts"`
}

// ProviderMonth is one month's worth of arrivals, oldest first.
type ProviderMonth struct {
	Month string `json:"month"` // YYYY-MM
	Parts int    `json:"parts"`
	Bytes int64  `json:"bytes"`
}

// These cap the lists that could otherwise be as long as the vault. Enough rows
// to see the shape of what is stored, few enough to read at a glance and to
// draw on a phone.
const (
	topFolders = 6
	topFiles   = 8
	topMonths  = 12
)

// ProviderStats reports one account in full: its health and quota, exactly as
// the sidebar has them, plus what its load is made of.
//
// The account is pinged as part of the answer — this is the panel somebody
// opens when they want to know where a drive stands, and a stale online dot
// there would be worse than a slow one.
func (v *Vault) ProviderStats(ctx context.Context, id string) (ProviderReport, error) {
	statuses, err := v.ProviderStatuses(ctx)
	if err != nil {
		return ProviderReport{}, err
	}

	report := ProviderReport{}
	found := false
	for _, st := range statuses {
		report.VaultShards += st.Shards
		report.VaultStored += st.Stored
		if st.ID == id {
			report.ProviderStatus = st
			found = true
		}
	}
	if !found {
		return ProviderReport{}, fmt.Errorf("no connected account with id %s", id)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return ProviderReport{}, ErrLocked
	}

	// Counted against the accounts that would still be connected, exactly as
	// the disconnect guard counts (see RemoveProvider): a part sitting on an
	// account this vault is no longer wired into is already out of reach and
	// must not prop up a file that only looks safe.
	surviving := map[string]bool{}
	for _, cfg := range v.providers {
		if cfg.ID != id {
			surviving[cfg.ID] = true
		}
	}

	kinds := map[string]*ProviderSlice{}
	folders := map[string]*ProviderSlice{}
	months := map[string]*ProviderMonth{}
	subs := map[string]bool{}
	var largest []ProviderItem

	for scope, m := range v.manifestsLocked() {
		for _, e := range m.Entries {
			parts, bytes := 0, int64(0)
			for _, s := range e.Shards {
				if s.ProviderID == id {
					parts++
					bytes += s.Size
				}
			}
			if parts == 0 {
				continue
			}
			report.Files++
			// Counted against the file's own code: a 4-of-6 file can lose two
			// accounts and still be read, a 2-of-3 one only one.
			if elsewhere(e.Shards, surviving) < e.Scheme().Data {
				report.Sole++
			}

			if !scope.Main() {
				// An open sub vault's files count towards the account's load
				// and towards what a disconnect would strand, and stop there.
				subs[string(scope)] = true
				report.SubVaults.Parts += parts
				report.SubVaults.Bytes += bytes
				continue
			}

			add(kinds, kindOf(e), parts, bytes)
			add(folders, e.Dir, parts, bytes)
			month := e.CreatedAt.UTC().Format("2006-01")
			if row, ok := months[month]; ok {
				row.Parts += parts
				row.Bytes += bytes
			} else {
				months[month] = &ProviderMonth{Month: month, Parts: parts, Bytes: bytes}
			}
			largest = append(largest, ProviderItem{
				Path: e.Path(), Size: e.Size, Bytes: bytes, Parts: parts,
			})
		}
	}

	// A shut sub vault cannot be asked what it holds, so its inventory answers
	// for it — the same substitution ProviderStatuses makes, for the same
	// reason: an account drawn as emptier than it is whenever a sub vault
	// happens to be locked would be reporting the session, not the account.
	for _, meta := range v.manifest.SubVaults {
		if _, open := v.subs[meta.ID]; open {
			continue
		}
		for _, item := range meta.Inventory {
			parts, bytes := 0, int64(0)
			reachable := map[int]bool{}
			for _, p := range item.Parts {
				if p.ProviderID == id {
					parts++
					bytes += p.Size
					continue
				}
				if surviving[p.ProviderID] {
					reachable[p.Part] = true
				}
			}
			if parts == 0 {
				continue
			}
			report.Files++
			if len(reachable) < item.Scheme().Data {
				report.Sole++
			}
			subs[meta.ID] = true
			report.SubVaults.Parts += parts
			report.SubVaults.Bytes += bytes
		}
	}

	// Named by their number and nothing else — which sub vault put what on an
	// account is as far as this panel goes into one.
	if len(subs) > 0 {
		report.SubVaults.Label = fmt.Sprintf("%d sub vault", len(subs))
		if len(subs) != 1 {
			report.SubVaults.Label += "s"
		}
	}

	report.Kinds = ranked(kinds, 0)
	report.Folders = ranked(folders, topFolders)
	report.Months = recentMonths(months, topMonths)

	sort.Slice(largest, func(i, j int) bool {
		if largest[i].Bytes != largest[j].Bytes {
			return largest[i].Bytes > largest[j].Bytes
		}
		return largest[i].Path < largest[j].Path
	})
	if len(largest) > topFiles {
		largest = largest[:topFiles]
	}
	report.Largest = largest

	return report, nil
}

// elsewhere counts the distinct parts of a file that sit on one of the given
// accounts — what would still be reachable if the account being reported on
// went away.
func elsewhere(shards []Shard, surviving map[string]bool) int {
	parts := map[int]bool{}
	for _, s := range shards {
		if surviving[s.ProviderID] {
			parts[s.Part] = true
		}
	}
	return len(parts)
}

func add(into map[string]*ProviderSlice, label string, parts int, bytes int64) {
	row, ok := into[label]
	if !ok {
		row = &ProviderSlice{Label: label}
		into[label] = row
	}
	row.Parts += parts
	row.Bytes += bytes
}

// ranked orders a breakdown heaviest first and, past limit rows, gathers the
// tail into one "other" so the parts still add up to the whole. limit of 0
// keeps every row.
func ranked(rows map[string]*ProviderSlice, limit int) []ProviderSlice {
	out := make([]ProviderSlice, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Label < out[j].Label
	})
	if limit <= 0 || len(out) <= limit {
		return out
	}
	rest := ProviderSlice{Label: "other"}
	for _, row := range out[limit:] {
		rest.Parts += row.Parts
		rest.Bytes += row.Bytes
	}
	return append(out[:limit:limit], rest)
}

// recentMonths returns the last months of arrivals, oldest first, with the
// quiet months in between kept as zeroes: a gap drawn as a gap is the point of
// the chart, and dropping the empty columns would draw a steady trickle as a
// wall.
func recentMonths(rows map[string]*ProviderMonth, limit int) []ProviderMonth {
	if len(rows) == 0 {
		return nil
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	first, err := time.Parse("2006-01", keys[0])
	if err != nil {
		return nil
	}
	last, err := time.Parse("2006-01", keys[len(keys)-1])
	if err != nil {
		return nil
	}

	var out []ProviderMonth
	for at := first; !at.After(last); at = at.AddDate(0, 1, 0) {
		key := at.Format("2006-01")
		if row, ok := rows[key]; ok {
			out = append(out, *row)
			continue
		}
		out = append(out, ProviderMonth{Month: key})
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// kindOf names the sort of thing a file is, in the six words somebody would
// use to describe what is filling a drive. The type recorded at upload leads;
// a file that arrived without one — an .mkv on a machine with no mime table is
// the usual case — is named from its extension instead.
func kindOf(e *Entry) string {
	mimeType := strings.ToLower(strings.TrimSpace(strings.SplitN(e.MIME, ";", 2)[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = strings.ToLower(mime.TypeByExtension(path.Ext(e.Name)))
		mimeType = strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0])
	}

	switch {
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "image/"):
		return "images"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case mimeType == "application/pdf",
		strings.HasPrefix(mimeType, "text/"),
		strings.Contains(mimeType, "word"),
		strings.Contains(mimeType, "spreadsheet"),
		strings.Contains(mimeType, "presentation"),
		mimeType == "application/json",
		mimeType == "application/rtf":
		return "documents"
	case archiveExts[strings.ToLower(path.Ext(e.Name))],
		strings.Contains(mimeType, "zip"),
		strings.Contains(mimeType, "tar"),
		strings.Contains(mimeType, "compressed"):
		return "archives"
	}

	// Go's built-in table stops at the handful of web types, so the formats a
	// vault is actually full of arrive unnamed. The extensions worth catching
	// are the media ones — the rest is honestly "other".
	switch strings.ToLower(path.Ext(e.Name)) {
	case ".mkv", ".avi", ".wmv", ".flv", ".m2ts", ".ts", ".mov", ".m4v", ".mpg", ".mpeg", ".3gp":
		return "video"
	case ".heic", ".heif", ".raw", ".cr2", ".nef", ".arw", ".dng":
		return "images"
	case ".flac", ".m4a", ".opus", ".wma", ".aac":
		return "audio"
	case ".epub", ".mobi", ".azw3", ".pages", ".numbers", ".key":
		return "documents"
	}
	return "other"
}

var archiveExts = map[string]bool{
	".zip": true, ".tar": true, ".gz": true, ".tgz": true, ".bz2": true,
	".xz": true, ".7z": true, ".rar": true, ".zst": true, ".iso": true,
	".dmg": true,
}
