package vault

import (
	"fmt"

	"github.com/sand-project/sand/internal/archive"
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

// Plan maps a part number (1..3) to the provider ID that should store it.
// A part missing from the map is deliberately not stored.
type Plan map[int]string

// BuildPlan decides where each part of a file goes.
//
// The seed rotates the starting account so that consecutive uploads do not
// pile every part 1 onto the same provider; passing a per-file value (the
// archive ID works well) spreads load evenly across accounts.
func BuildPlan(providerIDs []string, policy Policy, seed uint64) (Plan, error) {
	n := len(providerIDs)
	if n == 0 {
		return nil, fmt.Errorf("no cloud accounts connected")
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
				"strict placement needs at least %d connected accounts so that no single "+
					"account holds enough parts to reconstruct a file (have %d) — connect another "+
					"account, or switch this vault to the redundant policy",
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
func ShardKey(archiveID string, part int) string {
	return fmt.Sprintf("sand/%s/p%d.media", archiveID, part)
}
