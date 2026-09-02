package vault

import (
	"fmt"
	"time"
)

// How long a download link stays good.
//
// A folder handed back as a zip is fetched through a bearer link — an address
// that carries its own credential, so a download manager or another device can
// follow it with no session. That makes its lifetime a setting worth owning:
// it is a link to a folder in the clear, and how long one should stay live is
// a judgement about the network it travels, not a constant. Three hours is the
// default — long enough to carry the address to the machine with the disk for
// it, short enough that one left in a chat log is dead by the evening — and
// the vault's owner can move it either way.
//
// The setting is kept in the clear beside the placement policy and the health
// schedule, and gives away less than either: a number of hours says nothing
// about what the vault holds or where.

// DefaultLinkLifetime is how long a download link lasts unless the vault says
// otherwise.
const DefaultLinkLifetime = 3 * time.Hour

// MinLinkHours and MaxLinkHours bound what the setting will take. Under an
// hour the link cannot reliably be carried anywhere; past a week it is not a
// temporary credential any more.
const (
	MinLinkHours = 1
	MaxLinkHours = 24 * 7
)

// LinkLifetime is how long a download link stays good without being used.
//
// Readable while the vault is locked, like the health schedule: it is a
// setting kept in the clear, and the ticket store asking it has no reason to
// need the keys.
func (v *Vault) LinkLifetime() time.Duration {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.linkLifetimeLocked()
}

func (v *Vault) linkLifetimeLocked() time.Duration {
	if v.store == nil || v.store.LinkHours <= 0 {
		return DefaultLinkLifetime
	}
	return time.Duration(v.store.LinkHours) * time.Hour
}

// LinkHours is the same setting as a count of hours, which is how it crosses
// to the browser.
func (v *Vault) LinkHours() int {
	return int(v.LinkLifetime() / time.Hour)
}

// SetLinkHours changes how long a download link lasts. Zero puts the default
// back.
func (v *Vault) SetLinkHours(hours int) (int, error) {
	if hours != 0 && (hours < MinLinkHours || hours > MaxLinkHours) {
		return 0, fmt.Errorf("a download link can last between %d hour and %d hours",
			MinLinkHours, MaxLinkHours)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return 0, ErrLocked
	}

	before := v.store.LinkHours
	v.store.LinkHours = hours
	if err := v.persistLocked(); err != nil {
		v.store.LinkHours = before
		return 0, err
	}
	return int(v.linkLifetimeLocked() / time.Hour), nil
}
