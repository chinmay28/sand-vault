package vault

import (
	"sort"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Accounts that were disconnected, and enough about each to recognise it if it
// comes back.
//
// Disconnecting a cloud drops every record naming it, and connecting the same
// storage again gives it a new ID — nothing in the index says the two are one.
// The reattach (ReattachShards) is what reunites the parts with their files;
// this is what lets it say *which* departed account the returning one was, so
// what was kept about the old ID — its read history, today — can follow it to
// the new one rather than lingering as a "disconnected" row beside it.
//
// The proof is a handful of the parts the departed account held, by archive and
// part number. An archive ID is 128 random bits, so a returning account found
// holding one of those exact parts is that storage and nothing else. It is a
// sample rather than the full list because one match is the whole proof, and
// the index should not grow by an account's worth of rows per disconnect.
//
// Kept in the main index, sealed with it: archive IDs say which files lived
// where, which is index rather than configuration. Parts in sub vaults are not
// sampled — the main password has no business recording their archives.

// DepartedAccount is a disconnected account, as remembered.
type DepartedAccount struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`

	// Parts is the sample: parts the account held when it went.
	Parts []DepartedPart `json:"parts,omitempty"`
}

// DepartedPart names one part by its archive and number.
type DepartedPart struct {
	ArchiveID string `json:"archive_id"`
	Part      int    `json:"part"`
}

const (
	// departedSample is how many parts are kept per departed account. Any one
	// is proof; more than one survives some of those files being deleted or
	// rewritten before the storage comes back.
	departedSample = 16
	// departedKept is how many departed accounts are remembered, newest
	// kept. Past this the oldest is forgotten, and its read history stays a
	// "disconnected" row, which is what it was before any of this.
	departedKept = 32
)

// departedFrom builds the record for an account about to be disconnected,
// sampling the parts the given entries hold on it. Entries are taken in the
// order given and the first departedSample parts found are kept.
func departedFrom(cfg provider.Config, entries []*Entry, at time.Time) DepartedAccount {
	d := DepartedAccount{ID: cfg.ID, Name: cfg.Name, Kind: string(cfg.Kind), At: at}
	for _, e := range entries {
		for _, sh := range e.Shards {
			if sh.ProviderID != cfg.ID {
				continue
			}
			d.Parts = append(d.Parts, DepartedPart{ArchiveID: e.ArchiveID, Part: sh.Part})
			if len(d.Parts) == departedSample {
				return d
			}
		}
	}
	return d
}

// noteDeparted remembers an account that went, replacing any earlier record
// under the same ID and keeping the newest departedKept.
func (m *Manifest) noteDeparted(d DepartedAccount) {
	m.forgetDeparted(d.ID)
	m.Departed = append(m.Departed, d)
	if len(m.Departed) > departedKept {
		sort.SliceStable(m.Departed, func(i, j int) bool { return m.Departed[i].At.Before(m.Departed[j].At) })
		m.Departed = append([]DepartedAccount(nil), m.Departed[len(m.Departed)-departedKept:]...)
	}
}

// forgetDeparted drops the record under id, if there is one.
func (m *Manifest) forgetDeparted(id string) {
	kept := m.Departed[:0]
	for _, d := range m.Departed {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	m.Departed = kept
	if len(m.Departed) == 0 {
		m.Departed = nil
	}
}

// departedHolding answers which departed account's sample names this exact
// part, or false when none does.
func (m *Manifest) departedHolding(archiveID string, part int) (DepartedAccount, bool) {
	for _, d := range m.Departed {
		for _, p := range d.Parts {
			if p.ArchiveID == archiveID && p.Part == part {
				return d, true
			}
		}
	}
	return DepartedAccount{}, false
}
