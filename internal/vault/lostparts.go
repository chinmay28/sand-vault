package vault

import (
	"errors"
	"fmt"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/sandfile"
)

// Parts the index records that are not there.
//
// The index is the only record of where a file's parts went, and it has no
// way to know when one of them stops being there: a part deleted out from
// under SAND, a disk restored from last week's image, or — the common one
// on a removable disk — a part whose write was acknowledged and then lost
// when the disk was unplugged or lost power before its cache was flushed,
// which leaves a zero-length file under the right name. Until something
// goes and looks, the index counts the part, the file reads as whole, and a
// sweep that trusts the index skips it.
//
// Things do go and look: a read gathers parts, the inspector stats them, an
// automation sweep checks them, a re-encryption gathers every file in turn.
// Each of those used to learn the fact and drop it. Now they put it down
// here, so that the degraded list can show the file with the reason, a
// repair knows which part to write again rather than keeping the bad one as
// "already in place", and the inspector does not call a zero-length file
// present.
//
// Only a definite answer from an account that was answering is recorded:
// "not found", or an object too short to be a part (sandfile.MinPartSize).
// A timeout, a rate limit or an account that is down say nothing about the
// part and are not written down — the repair that follows treats a lost
// part as gone, and that must not happen to a part whose account was busy.
//
// What is here is this process's own, not the vault file's: what was true of
// the accounts an hour ago is less certain than what is true of the index,
// and anything that cares re-checks. A record names the archive the part
// belonged to, so a file rewritten since — a relocation, a re-encryption, a
// conversion — is not haunted by a part of the copy it no longer is.

// LostPart is a recorded part an account that was answering said it does
// not hold.
type LostPart struct {
	Part         int    `json:"part"`
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`

	// Reason is what the account said: the object was not found, or it was
	// too short to be a part, with its size.
	Reason string `json:"reason"`

	// At is when it was found so.
	At time.Time `json:"at"`

	archiveID string
}

// partTooShort reports whether an object of this size can be a part at all.
func partTooShort(size int64) bool { return size < int64(sandfile.MinPartSize) }

// truncatedReason is the reason recorded for an object too short to be a part.
func truncatedReason(size int64) string {
	if size == 0 {
		return "0 B on disk — truncated: the disk most likely lost power or was unplugged after " +
			"accepting the write and before it had written the part out"
	}
	return fmt.Sprintf("%s on disk, too short to be a part — truncated", formatSize(size))
}

// lostReasonFor turns what an account said about a part into a reason to
// record, or "" when it said nothing definite: an error that is not "not
// found" is no answer, and a blob long enough to be a part is one.
func lostReasonFor(blob []byte, err error) string {
	switch {
	case err != nil && errors.Is(err, provider.ErrNotFound):
		return "not found on the account — the part was deleted or the disk holding it was replaced"
	case err != nil:
		return ""
	case partTooShort(int64(len(blob))):
		return truncatedReason(int64(len(blob)))
	}
	if _, perr := sandfile.ReadPart(blob); perr != nil {
		return fmt.Sprintf("not a part file (%s): %v", formatSize(int64(len(blob))), perr)
	}
	return ""
}

// noteLostPart writes down that a part of the file is not there.
func (v *Vault) noteLostPart(id, archiveID string, s Shard, reason string) {
	if reason == "" {
		return
	}
	v.lostMu.Lock()
	defer v.lostMu.Unlock()
	if v.lost == nil {
		v.lost = map[string]map[int]LostPart{}
	}
	byPart := v.lost[id]
	if byPart == nil {
		byPart = map[int]LostPart{}
		v.lost[id] = byPart
	}
	byPart[s.Part] = LostPart{
		Part: s.Part, ProviderID: s.ProviderID, ProviderName: s.ProviderName,
		Reason: reason, At: time.Now().UTC(), archiveID: archiveID,
	}
}

// forgetLostParts drops what was recorded against a file: its parts have been
// written again.
func (v *Vault) forgetLostParts(id string) {
	v.lostMu.Lock()
	delete(v.lost, id)
	v.lostMu.Unlock()
}

// lostPartsOf is what is recorded against a file's current archive, by part.
// Nil when nothing is.
func (v *Vault) lostPartsOf(e *Entry) map[int]LostPart {
	v.lostMu.Lock()
	defer v.lostMu.Unlock()
	var out map[int]LostPart
	for part, lost := range v.lost[e.ID] {
		if lost.archiveID != e.ArchiveID {
			continue
		}
		if out == nil {
			out = map[int]LostPart{}
		}
		out[part] = lost
	}
	return out
}

// knownAbsent is every recorded loss as a relocation wants to be told it: by
// file ID, then by part. Nil when nothing is recorded, which is the common
// case and costs a relocation nothing.
func (v *Vault) knownAbsent() map[string]map[int]bool {
	v.lostMu.Lock()
	defer v.lostMu.Unlock()
	if len(v.lost) == 0 {
		return nil
	}
	out := map[string]map[int]bool{}
	for id, byPart := range v.lost {
		parts := map[int]bool{}
		for part := range byPart {
			parts[part] = true
		}
		out[id] = parts
	}
	return out
}

// lostPartsOfLocked is lostPartsOf with the archive checked against the
// index as it stands; the caller holds at least the read lock, which is what
// makes the entry it passes current.
func (v *Vault) lostPartsOfLocked(e *Entry) map[int]LostPart { return v.lostPartsOf(e) }
