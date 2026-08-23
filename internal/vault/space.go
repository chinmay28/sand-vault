package vault

import (
	"fmt"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// How much more will fit on an account, and whose number that is.
//
// "How full is it" and "how much more fits" are not the same question, and
// until now only the first had an answer. An account card said 284.9 GB of
// parts are here; the picker that chooses where the next file goes said the
// same thing, which is no help at all in choosing — a drive with 284.9 GB on it
// might have four terabytes free or forty megabytes.
//
// The answer comes from one of three places, and which one it came from matters
// enough to travel with it:
//
//	reported   the backend said so. A Drive knows its quota, a filesystem
//	           knows its free blocks, and both are live figures.
//	declared   the capacity somebody typed, minus what a count of the account
//	           found in it. What a bucket gets, since S3 has no quota call.
//	quota      the limit somebody set on how much of the account SAND may
//	           fill, minus what the index says SAND has put there. The only
//	           figure available on a backend that reports nothing and cannot
//	           be listed — and the only one that is about *our* share rather
//	           than about the account.
//
// An account can have both a reported figure and a quota, and then the room
// left is the smaller of the two: a quota that has run out leaves no room on a
// half-empty drive, and a full drive leaves none whatever the quota says.
const (
	SpaceReported = "reported"
	SpaceDeclared = "declared"
	SpaceQuota    = "quota"
)

// Space is what one account has left, together with the pair of figures that
// answer came from. Source is empty when nobody can say — a backend with no
// quota call, nothing counted, and no quota set — and then every other field is
// zero and means nothing rather than meaning empty.
type Space struct {
	Free   int64  `json:"free"`
	Used   int64  `json:"used"`
	Total  int64  `json:"total"`
	Source string `json:"source,omitempty"`

	// Over is how far past its quota the account already is, in bytes. Only a
	// quota produces it: a backend's own figures cannot be exceeded, they can
	// only reach zero.
	Over int64 `json:"over,omitempty"`
}

// Known reports whether anything is actually known about this account's room.
func (s Space) Known() bool { return s.Source != "" }

// spaceFor works out how much more an account can take from the three things
// that might say: what the backend reports (or what a declared capacity and a
// count of the account come to between them), and the quota set on SAND's own
// share of it.
//
// stored is the index's own accounting — the bytes of parts SAND put there —
// which is the only figure a quota can be measured against. It is known for
// every account whether or not the backend says anything, which is exactly why
// a quota answers where nothing else does.
func spaceFor(usage provider.Usage, quota, stored int64) Space {
	var space Space

	if usage.Total > 0 {
		space = Space{
			Free:   usage.Remaining(),
			Used:   min(max(usage.Used, 0), usage.Total),
			Total:  usage.Total,
			Source: SpaceReported,
		}
		if usage.Declared {
			space.Source = SpaceDeclared
		}
	}

	if quota <= 0 {
		return space
	}

	left := quota - stored
	over := int64(0)
	if left < 0 {
		over, left = -left, 0
	}

	// The binding limit is whichever leaves less, and an account already past
	// its quota is bound by it however much room the drive underneath reports.
	if space.Known() && over == 0 && left >= space.Free {
		return space
	}
	return Space{Free: left, Used: stored, Total: quota, Source: SpaceQuota, Over: over}
}

// providerLoad is what the index says one account is holding: how many parts,
// and what they weigh.
type providerLoad struct {
	Shards int
	Stored int64
}

// loadByProviderLocked adds up what every account is holding, across the main
// vault and every sub vault inside it.
//
// A shut sub vault cannot be asked, so its inventory answers for it. Leaving it
// out would draw an account as emptier than it is, and the amount missing would
// change with which sub vault happened to be open — an account's load that
// moves when you type a password is worse than no figure at all.
func (v *Vault) loadByProviderLocked() map[string]providerLoad {
	out := map[string]providerLoad{}
	add := func(id string, size int64) {
		load := out[id]
		load.Shards++
		load.Stored += size
		out[id] = load
	}

	for _, m := range v.manifestsLocked() {
		for _, e := range m.Entries {
			for _, s := range e.Shards {
				add(s.ProviderID, s.Size)
			}
		}
	}
	for _, meta := range v.manifest.SubVaults {
		if _, open := v.subs[meta.ID]; open {
			continue
		}
		for _, item := range meta.Inventory {
			for _, part := range item.Parts {
				add(part.ProviderID, part.Size)
			}
		}
	}
	return out
}

// quotaWarnings names the accounts a just-stored file has taken past the quota
// set for them, one sentence each.
//
// A warning rather than a refusal, and after the fact rather than before. The
// parts of a file are placed together: refusing the one part that would cross a
// quota mid-scatter leaves the file stored on fewer accounts than the code it
// was cut with promises, which is a worse outcome than a full cloud. So the
// upload completes and says what it did — and the picker, which knows the room
// on every account before a byte moves, is where the choice is actually made.
//
// Only the file that crosses the line says so. An upload is usually a batch and
// each file in it is warned about separately, so an account that went on
// reporting itself over would put the same sentence on four hundred rows and
// bury everything else that went wrong. Crossing is the event; being over is a
// state, and the account's own card and the upload picker are where a state
// belongs.
//
// added is what this upload put on each account, which is what makes the
// difference between the two answerable at all.
func (v *Vault) quotaWarnings(added map[string]int64) []string {
	if len(added) == 0 {
		return nil
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil
	}

	load := v.loadByProviderLocked()

	var out []string
	for _, cfg := range v.providers {
		bytes, touched := added[cfg.ID]
		if !touched || cfg.Quota <= 0 {
			continue
		}
		stored := load[cfg.ID].Stored
		if stored <= cfg.Quota || stored-bytes > cfg.Quota {
			continue
		}
		out = append(out, fmt.Sprintf(
			"this took %s past the %s quota you set for it — %s of parts are on it now, "+
				"%s over the line. Nothing was refused; raise the quota or move files off it",
			cfg.Name, formatBytes(cfg.Quota), formatBytes(stored),
			formatBytes(stored-cfg.Quota)))
	}
	return out
}

// shardBytesByProvider adds up what one file's parts weigh on each account
// they were written to.
func shardBytesByProvider(shards []Shard) map[string]int64 {
	if len(shards) == 0 {
		return nil
	}
	out := make(map[string]int64, len(shards))
	for _, s := range shards {
		out[s.ProviderID] += s.Size
	}
	return out
}
