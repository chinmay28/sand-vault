package vault

import (
	"sync"
	"time"
)

// Archives this process is writing that the index does not point at yet.
//
// Every sweep in this package reasons from the index: a part object whose
// archive no entry names is abandoned, and may go. That is true of a part left
// behind by a delete that happened while its account was away, and it is
// exactly as true of a part the upload two goroutines over wrote ten seconds
// ago and has not finished writing the rest of — the index says nothing about
// either. An orphan sweep that ran during a forty-gigabyte UploadStream would
// erase thousands of freshly written chunk objects, and the commit that followed
// would record an entry pointing at nothing. The same window exists for every
// other path that scatters first and records second: a re-encryption, a format
// conversion, a recode, a thumbnail pack being saved.
//
// This file is the registry that closes it. An archive ID is held from the
// moment a scatter mints it until the scatter returns, and then for a settling
// window after that, because the index write that makes the archive the
// index's business happens in the caller, after the scatter returns, and the
// scatter has no way of knowing when. Fifteen minutes is a bound on how long a
// caller takes to get from a placement in hand to a persisted entry, not a
// measurement of it: the commit is normally milliseconds. Erring long costs a
// sweep nothing but a short wait before a genuinely abandoned archive is
// offered; erring short costs somebody's file.
//
// The contract, for the scans that consult it: an archive that is held or was
// released within the window is owned — not shown, not deletable, not held
// against anybody. Nothing here says where its parts are, only that they are
// spoken for.

// archiveSettleWindow is how long after a scatter returns its archive stays
// owned. See the file comment for why this is a bound rather than a measurement.
const archiveSettleWindow = 15 * time.Minute

// archiveFlights is the registry itself. A leaf lock: hold and release are
// called from scatter paths that are not holding mu, and the scans read it
// while they are, so nothing in here may take any other lock.
//
// The zero value is ready to use, which is what lets the Vault carry it as a
// plain field without Open having to know about it.
type archiveFlights struct {
	mu sync.Mutex

	// open is every archive a scatter is writing right now; settled is every
	// archive a scatter has finished with, by when it finished, kept until the
	// window has passed.
	open    map[string]bool
	settled map[string]time.Time

	// settle, when positive, replaces archiveSettleWindow; negative means no
	// window at all, so an archive is forgotten the moment its scatter
	// returns. now, when set, replaces time.Now. Both exist for the tests:
	// one that wants to walk the clock past the window, and the many that
	// upload, delete and scan within a second and expect the scan to find
	// what the delete left.
	settle time.Duration
	now    func() time.Time
}

func (f *archiveFlights) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *archiveFlights) window() time.Duration {
	switch {
	case f.settle < 0:
		return 0
	case f.settle > 0:
		return f.settle
	}
	return archiveSettleWindow
}

// hold marks id as being written, and returns the release to defer. Releasing
// twice is harmless: the second call finds nothing open and changes nothing.
func (f *archiveFlights) hold(id string) (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		f.open = map[string]bool{}
	}
	f.open[id] = true

	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.open[id] {
			return
		}
		delete(f.open, id)
		if f.settled == nil {
			f.settled = map[string]time.Time{}
		}
		f.settled[id] = f.clock()
	}
}

// ids is every archive currently spoken for: held, or released within the
// window. Entries whose window has passed are dropped on the way, which is the
// only housekeeping the registry needs — a scan is the one thing that reads it,
// and a scan is rare.
func (f *archiveFlights) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	now, window := f.clock(), f.window()
	out := make([]string, 0, len(f.open)+len(f.settled))
	for id := range f.open {
		out = append(out, id)
	}
	for id, at := range f.settled {
		if now.Sub(at) >= window {
			delete(f.settled, id)
			continue
		}
		if !f.open[id] {
			out = append(out, id)
		}
	}
	return out
}

// noteInFlight adds every archive the registry speaks for to owned, the map
// ownedArchivesLocked builds from the index, so that the scans need only one
// notion of "owned".
//
// A row added here is marked inFlight and left unreattachable: it is not a
// file in the tree with a record missing, it is a file that is not in the tree
// yet, and the scans treat that the way they treat a thumbnail pack — present,
// accounted for, and not theirs to touch. An archive the index already names
// is left as the index described it.
func (v *Vault) noteInFlight(owned map[string]*ownedArchive) {
	for _, id := range v.flights.ids() {
		if _, known := owned[id]; known {
			continue
		}
		owned[id] = &ownedArchive{
			parts:    map[int]bool{},
			accounts: map[string]bool{},
			inFlight: true,
		}
	}
}
