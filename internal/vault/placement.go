package vault

import (
	"fmt"
	"math/rand/v2"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// Policy decides how shards are spread over the connected accounts.
type Policy string

const (
	// PolicyStrict never lets a single account hold two parts of the same
	// file. Two parts are enough to reconstruct, so this keeps the promise
	// that compromising one cloud account reveals nothing. With only two
	// accounts connected it costs the third, redundant part.
	PolicyStrict Policy = "strict"

	// PolicyRedundant always writes all three parts, doubling up on an
	// account when fewer than three are connected. This survives an account
	// going offline, but an attacker who breaks into the doubled-up account
	// holds enough to reconstruct.
	PolicyRedundant Policy = "redundant"
)

// Valid reports whether p is a known policy.
func (p Policy) Valid() bool {
	return p == PolicyStrict || p == PolicyRedundant
}

// AccountsPerFile is how many accounts a file is spread over: one per part, so
// that no account holds enough of a file to rebuild it. It is also how many
// accounts an upload picks when nobody has said which ones to use.
const AccountsPerFile = archive.PartCount

// SelectAccounts chooses which of the connected accounts one file's parts go
// to.
//
// preferred is a starting point rather than an answer: anything in it that is
// no longer connected is dropped, and if that leaves fewer accounts than a
// file has parts the rest are filled in at random from whatever else is
// connected. That is what a vault with no default at all wants — three
// accounts of this file's own — and what a file being re-encrypted wants, which
// is the accounts it is already on, made back up to three if one has gone away.
//
// A choice someone actually made, whether for one upload or as the vault's
// default, does not come through here: it is followed exactly.
//
// The seed makes the random half deterministic per file rather than per
// process. Passing the archive ID — 128 random bits minted for this file alone
// — is what makes consecutive uploads land on different accounts instead of
// piling onto whichever three were connected first.
func SelectAccounts(available, preferred []string, seed uint64) []string {
	connected := make(map[string]bool, len(available))
	for _, id := range available {
		connected[id] = true
	}

	chosen := make([]string, 0, AccountsPerFile)
	taken := make(map[string]bool, AccountsPerFile)
	for _, id := range preferred {
		if len(chosen) == AccountsPerFile {
			break
		}
		if !connected[id] || taken[id] {
			continue
		}
		chosen = append(chosen, id)
		taken[id] = true
	}
	if len(chosen) == AccountsPerFile {
		return chosen
	}

	rest := make([]string, 0, len(available))
	for _, id := range available {
		if !taken[id] {
			rest = append(rest, id)
		}
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })

	for _, id := range rest {
		if len(chosen) == AccountsPerFile {
			break
		}
		chosen = append(chosen, id)
	}
	return chosen
}

// Plan maps a part number (1..3) to the provider ID that should store it.
// A part missing from the map is deliberately not stored.
type Plan map[int]string

// BuildPlan decides where each part of a file goes across the accounts chosen
// for it — the whole connected set, the vault's defaults, or the handful the
// upload named. Whichever it is, SelectAccounts has already narrowed it.
//
// The seed rotates the starting account so that consecutive uploads do not
// pile every part 1 onto the same provider; passing a per-file value (the
// archive ID works well) spreads load evenly across accounts.
func BuildPlan(providerIDs []string, policy Policy, seed uint64) (Plan, error) {
	n := len(providerIDs)
	if n == 0 {
		return nil, fmt.Errorf("no cloud accounts to store this file on")
	}
	if !policy.Valid() {
		return nil, fmt.Errorf("unknown placement policy %q", policy)
	}

	offset := int(seed % uint64(n))
	plan := Plan{}

	switch policy {
	case PolicyStrict:
		if n < archive.MinPartsToRestore {
			return nil, fmt.Errorf(
				"strict placement needs at least %d accounts so that no single "+
					"account holds enough parts to reconstruct a file (have %d) — connect or "+
					"choose another account, or switch this vault to the redundant policy",
				archive.MinPartsToRestore, n)
		}
		parts := archive.PartCount
		if n < parts {
			parts = n
		}
		for i := 0; i < parts; i++ {
			plan[i+1] = providerIDs[(offset+i)%n]
		}

	case PolicyRedundant:
		for i := 0; i < archive.PartCount; i++ {
			plan[i+1] = providerIDs[(offset+i)%n]
		}
	}

	return plan, nil
}

// ShardKey is the object key a part is stored under on its provider. Keys are
// derived only from the random archive ID, so an observer with access to a
// single account learns nothing about the file beyond its size.
//
// The key is a single flat filename with no directory components. Every
// backend already puts SAND's objects somewhere of their own — a folder on
// Dropbox, Box and OneDrive, a prefix on S3 and WebDAV, the chosen directory
// for a local or sync folder — so nesting another "sand" folder inside that
// only buried the parts one level deeper. Staying flat also means Google
// Drive, which has no paths and stores each part as a plain file, ends up with
// exactly the same part names as everywhere else.
func ShardKey(archiveID string, part int) string {
	return fmt.Sprintf("%s-p%d.sand", archiveID, part)
}

// ChunkShardKey is the object key one part of one chunk is stored under. It is
// ShardKey with the chunk index spliced in, zero-padded so that listing an
// account lexically returns a file's chunks in order — which is what the
// recovery path in §3.7 walks when it asks an account what it holds.
//
// The index is visible on the account, and that gives away no more than the
// flat form already did. A file's objects were always groupable by their shared
// archive ID, and its size was always readable from the part sizes; a chunk
// count is the same fact arrived at by counting instead of adding.
func ChunkShardKey(archiveID string, chunkIndex, part int) string {
	return fmt.Sprintf("%s-c%07d-p%d.sand", archiveID, chunkIndex, part)
}
