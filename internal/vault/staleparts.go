package vault

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Parts a re-encryption could not erase.
//
// Re-encrypting a file writes it out again under the current data key and
// erases the parts the previous key opened. When one of the accounts holding
// the file cannot be reached, the file still moves — as long as the parts on
// the accounts that answer are enough to rebuild it — and the part on the
// account that did not is left exactly where it is: sealed under a key the
// vault is retiring, pointed at by nothing, and readable by anyone who holds
// the previous password and an old copy of the vault file or of a manifest
// backup. That is precisely the thing a password change exists to get rid of,
// so it is not allowed to be forgotten.
//
// This file is the record of them. The index remembers each such part — which
// account, which archive, which shard number, what it weighed and which key it
// is under — until one of three things happens: the stray-parts scan lists the
// account and finds the object gone, a sweep erases it, or the account is
// disconnected (at which point the part is an ordinary orphan, found and
// offered by the same scan whenever that storage is connected back). While it
// is remembered, the re-encryption status says so, the vault's stats count it,
// and the scan flags the object as a stray under the previous key rather than
// as an archive nobody can say anything about.
//
// A StalePart is a note, not a reference: it keeps no data key alive. The
// retired key goes the moment the last file leaves it, as it always has, and
// the part becomes unreadable to this vault at the same instant. It is the
// previous password's copy of it that is the concern, and erasing the object
// is the only way to deal with that.

// StalePart is one shard of one file, sealed under a data key this vault has
// retired, that a re-encryption could not erase from the account holding it.
type StalePart struct {
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderKind string `json:"provider_kind"`

	// ArchiveID and Part name the object the way a Shard does; ChunkCount says
	// how many objects the part occupies (zero for a file stored whole), and
	// Bytes what they weigh together.
	ArchiveID  string `json:"archive_id"`
	Part       int    `json:"part"`
	ChunkCount int    `json:"chunk_count,omitempty"`
	Bytes      int64  `json:"bytes"`

	// KeyID is the generation the part is sealed under: the previous key, by
	// the time anybody reads this. A label, not a secret.
	KeyID string `json:"key_id,omitempty"`

	// Scope is which vault inside the file the part's file belonged to, and
	// Path where the file was when it moved. Path is carried only for the main
	// vault — a sub vault tells the main password its name and its weight and
	// nothing else, the rule the whole package keeps.
	Scope Scope  `json:"scope,omitempty"`
	Path  string `json:"path,omitempty"`

	// LeftAt is when the re-encryption moved on without it, and Reason why:
	// the account did not answer the last health check, or the erase itself
	// failed and how.
	LeftAt time.Time `json:"left_at"`
	Reason string    `json:"reason,omitempty"`
}

// key identifies the part the way the scan and the sweep meet it: one object
// set, on one account.
func (p *StalePart) key() stalePartKey {
	return stalePartKey{providerID: p.ProviderID, archiveID: p.ArchiveID, part: p.Part}
}

type stalePartKey struct {
	providerID string
	archiveID  string
	part       int
}

// noteStaleParts records parts a re-encryption left behind. A part already on
// record stays as it was recorded: the first reason is the one worth keeping,
// and a retry that fails the same way is not news.
func (m *Manifest) noteStaleParts(parts []StalePart) {
	if len(parts) == 0 {
		return
	}
	have := make(map[stalePartKey]bool, len(m.StaleParts))
	for _, p := range m.StaleParts {
		have[p.key()] = true
	}
	for i := range parts {
		p := parts[i]
		if have[p.key()] {
			continue
		}
		have[p.key()] = true
		m.StaleParts = append(m.StaleParts, &p)
	}
}

// forgetStaleParts drops every recorded part that drop answers true for, and
// reports whether anything was dropped — so a caller can skip the write when
// nothing changed.
func (m *Manifest) forgetStaleParts(drop func(*StalePart) bool) bool {
	if len(m.StaleParts) == 0 {
		return false
	}
	kept := m.StaleParts[:0]
	for _, p := range m.StaleParts {
		if drop(p) {
			continue
		}
		kept = append(kept, p)
	}
	changed := len(kept) != len(m.StaleParts)
	for i := len(kept); i < len(m.StaleParts); i++ {
		m.StaleParts[i] = nil
	}
	m.StaleParts = kept
	if len(m.StaleParts) == 0 {
		m.StaleParts = nil
	}
	return changed
}

// stalePartsByKey indexes the record for a scan, which meets objects one at a
// time and needs to ask about each.
func (m *Manifest) stalePartsByKey() map[stalePartKey]*StalePart {
	if len(m.StaleParts) == 0 {
		return nil
	}
	out := make(map[stalePartKey]*StalePart, len(m.StaleParts))
	for _, p := range m.StaleParts {
		out[p.key()] = p
	}
	return out
}

// StalePartsAccount is one account's share of the parts left under the
// previous key.
type StalePartsAccount struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Parts      int    `json:"parts"`
	Bytes      int64  `json:"bytes"`

	// Unreachable says the account did not answer its last health check, so
	// the parts on it cannot be offered for cleaning up yet. False for an
	// account that answered, and for one nobody has asked.
	Unreachable bool `json:"unreachable,omitempty"`
}

// StalePartsSummary is what the vault remembers leaving behind under the
// previous key, and where.
type StalePartsSummary struct {
	Parts    int                 `json:"parts"`
	Bytes    int64               `json:"bytes"`
	Accounts []StalePartsAccount `json:"accounts"`

	// Reachable counts the parts on accounts that answered their last health
	// check — the ones a stray-parts scan would find now and a sweep could
	// erase.
	Reachable int `json:"reachable"`
}

// Names lists the accounts holding such parts, alphabetically, for a sentence.
func (s StalePartsSummary) Names() []string {
	names := make([]string, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}

// Sentence says what is on record in one line, or "" when nothing is: the
// line the re-encryption status, the CLI and the browser all carry.
func (s StalePartsSummary) Sentence() string {
	if s.Parts == 0 {
		return ""
	}
	noun := "parts"
	if s.Parts == 1 {
		noun = "part"
	}
	var down, up []string
	for _, a := range s.Accounts {
		if a.Unreachable {
			down = append(down, a.Name)
		} else {
			up = append(up, a.Name)
		}
	}
	sort.Strings(down)
	sort.Strings(up)
	line := fmt.Sprintf("%d %s under the previous key %s left on %s when the files holding them were "+
		"re-encrypted without that account",
		s.Parts, noun, map[bool]string{true: "was", false: "were"}[s.Parts == 1],
		strings.Join(s.Names(), ", "))
	switch {
	case len(up) == 0:
		line += fmt.Sprintf(" — %s is still not answering; they will be offered for cleaning up as stray parts when it is back",
			strings.Join(down, ", "))
	case len(down) == 0:
		line += " — they are within reach now: run the stray-parts scan and clean them up"
	default:
		line += fmt.Sprintf(" — the ones on %s are within reach now: run the stray-parts scan and clean them up",
			strings.Join(up, ", "))
	}
	return line
}

// StaleParts reports the parts the main index remembers leaving under the
// previous key, grouped by account. It costs no request: the record is in
// the index, and whether each account is reachable is what the last health
// check said.
func (v *Vault) StaleParts() (StalePartsSummary, error) {
	// Before the index lock, never under it: see unreachableAccounts.
	down := v.unreachableAccounts()

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return StalePartsSummary{}, ErrLocked
	}
	return v.stalePartsLocked(down), nil
}

// stalePartsLocked is StaleParts for a caller already holding at least the
// read lock, given the unreachable set it took beforehand.
func (v *Vault) stalePartsLocked(down map[string]bool) StalePartsSummary {
	out := StalePartsSummary{Accounts: []StalePartsAccount{}}
	if len(v.manifest.StaleParts) == 0 {
		return out
	}
	byAccount := map[string]*StalePartsAccount{}
	for _, p := range v.manifest.StaleParts {
		row := byAccount[p.ProviderID]
		if row == nil {
			name := p.ProviderName
			if cfg, ok := v.configForLocked(p.ProviderID); ok {
				name = cfg.Name
			}
			row = &StalePartsAccount{ProviderID: p.ProviderID, Name: name, Unreachable: down[p.ProviderID]}
			byAccount[p.ProviderID] = row
		}
		row.Parts++
		row.Bytes += p.Bytes
		out.Parts++
		out.Bytes += p.Bytes
		if !row.Unreachable {
			out.Reachable++
		}
	}
	for _, row := range byAccount {
		out.Accounts = append(out.Accounts, *row)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Name < out.Accounts[j].Name })
	return out
}

// stalePartsOf turns the shards of an entry that could not be erased into the
// record of them. reason says why, in words a status can repeat.
func stalePartsOf(scope Scope, e *Entry, shards []Shard, reason string, at time.Time) []StalePart {
	out := make([]StalePart, 0, len(shards))
	for _, s := range shards {
		p := StalePart{
			ProviderID:   s.ProviderID,
			ProviderName: s.ProviderName,
			ProviderKind: s.ProviderKind,
			ArchiveID:    e.ArchiveID,
			Part:         s.Part,
			ChunkCount:   e.ChunkCount,
			Bytes:        s.Size,
			KeyID:        e.KeyID,
			Scope:        scope,
			LeftAt:       at,
			Reason:       reason,
		}
		if scope.Main() {
			p.Path = e.Path()
		}
		out = append(out, p)
	}
	return out
}

// noteStalePartsLocked writes parts left behind into the main index and
// persists it. The caller holds the write lock. A failure to write is
// returned for the caller to report; the record stays in memory either way,
// and the next write carries it.
func (v *Vault) noteStalePartsLocked(parts []StalePart) error {
	if len(parts) == 0 {
		return nil
	}
	v.manifest.noteStaleParts(parts)
	return v.persistLocked()
}

// forgetStaleParts drops recorded parts under the write lock, and writes the
// index if anything went. Called by the scan that found an object gone and by
// the sweep that erased one; a failure to write is reported, not fatal, since
// the next write carries the same state.
func (v *Vault) forgetStaleParts(drop func(*StalePart) bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	if !v.manifest.forgetStaleParts(drop) {
		return nil
	}
	return v.persistLocked()
}
