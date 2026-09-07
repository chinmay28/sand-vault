package vault

import (
	"fmt"
	"time"
)

// Putting a file's own modification time back.
//
// Every upload used to stamp the entry with the moment it landed, so a folder
// of photographs taken over ten years arrived in the vault all dated the same
// afternoon. Uploads and imports now carry the file's own time across
// (UploadOptions.ModifiedAt) — but that does nothing for what is already
// stored, and re-uploading a folder to fix it would mean sending every byte of
// it again for a field.
//
// So the times can be put right on their own. Uploading or importing the same
// files again corrects the stamps of the ones the vault already holds, moving
// no bytes at all, and says how many it corrected. Nothing else about the
// entries is touched: same ID, same parts, same accounts, same thumbnails.

// FileTime names one stored file and the modification time it should carry.
type FileTime struct {
	// Path is the file's full path in the vault, as JoinPath writes one.
	Path string

	// Mod is the time the file was last modified where it came from. The zero
	// value is "no time to offer", and leaves the entry alone.
	Mod time.Time
}

// Retime records the modification times files brought with them, on files the
// vault already holds, and reports how many entries it changed.
//
// A path naming nothing is ignored rather than refused: this answers a question
// about a choice of files, and a file of that choice not being here is an
// ordinary answer — it is the one that is about to be uploaded. A time that
// already agrees to the second is left alone, so asking twice changes nothing
// and reports nothing.
//
// The index is written once, at the end, and only if something actually
// changed. Nothing is read from or written to any account: a modification time
// lives in the index alone, which is what makes correcting ten thousand of them
// a single cheap write rather than ten thousand transfers.
func (v *Vault) Retime(scope Scope, want []FileTime) (int, error) {
	if len(want) == 0 {
		return 0, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return 0, err
	}

	// The entries first, then the write: a persist that fails must not leave
	// half the choice retimed in memory and the other half not, so what was
	// changed is remembered well enough to be put back.
	type change struct {
		entry *Entry
		was   time.Time
	}
	var changed []change
	for _, f := range want {
		if f.Mod.IsZero() {
			continue
		}
		entry := m.ByPath(f.Path)
		if entry == nil || SameModTime(entry.ModifiedAt, f.Mod) {
			continue
		}
		changed = append(changed, change{entry: entry, was: entry.ModifiedAt})
		entry.ModifiedAt = f.Mod.UTC()
	}
	if len(changed) == 0 {
		return 0, nil
	}

	if err := v.persistLocked(); err != nil {
		for _, c := range changed {
			c.entry.ModifiedAt = c.was
		}
		return 0, fmt.Errorf("could not record the corrected times: %w", err)
	}
	return len(changed), nil
}

// SameModTime reports whether two modification times are the same time.
//
// Compared to the second, because that is the resolution the times being
// compared actually have: SFTP and most filesystems keep whole seconds, a
// browser hands over milliseconds, and the vault stores whatever it was given.
// A file would otherwise read as changed against a copy of itself for the sake
// of digits nothing on either side is claiming to know.
func SameModTime(a, b time.Time) bool {
	return a.UTC().Truncate(time.Second).Equal(b.UTC().Truncate(time.Second))
}
