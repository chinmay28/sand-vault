package vault

import (
	"fmt"
	"math/rand/v2"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// Policy decides how shards are spread over the connected accounts.
type Policy string

const (
	// PolicyStrict never lets a single account hold two shards of the same
	// file, at any width. k shards are enough to reconstruct, so this keeps the
	// promise that compromising one cloud account reveals nothing — and keeps it
	// harder as the spread grows, since a wider scheme raises k. With only two
	// accounts connected it costs the third, redundant shard.
	PolicyStrict Policy = "strict"

	// PolicyRedundant always writes the whole scheme, doubling up on an account
	// when fewer are connected than it has shards. This survives an account
	// going offline with one or two connected, but an attacker who breaks into
	// the doubled-up account holds enough to reconstruct.
	PolicyRedundant Policy = "redundant"
)

// Valid reports whether p is a known policy.
func (p Policy) Valid() bool {
	return p == PolicyStrict || p == PolicyRedundant
}

// AccountsPerFile is how many accounts an upload spreads over when nobody has
// said which ones to use — the width of the default scheme, one account per
// shard.
const AccountsPerFile = 3

// SchemeFor returns the erasure code that spreads a file over exactly n
// accounts: two thirds of 3m shards, for m groups of three (archive.Scheme).
//
// One or two accounts is not a scheme so much as a shortfall, and it is allowed
// for the same reason it always was: a vault with two clouds connected should
// store files rather than refuse to. Those get the default code with whatever
// shards there is room for — see BuildPlan.
func SchemeFor(accounts int) (archive.Scheme, error) {
	if accounts > 0 && accounts < archive.AccountsPerGroup {
		return archive.SchemeDefault, nil
	}
	return archive.SchemeFor(accounts)
}

// ValidSpread reports whether n accounts is a shape a file can be laid out
// over. It is the same question SchemeFor answers, asked where only yes or no
// is wanted.
//
// None is valid, and means exactly that: no accounts were named, so nothing has
// been chosen and the caller's own default applies. Only a non-empty selection
// has to name a scheme.
func ValidSpread(n int) bool {
	if n == 0 {
		return true
	}
	_, err := SchemeFor(n)
	return err == nil
}

// ErrSpread is what ValidSpread failing has to say to whoever chose the
// accounts.
func ErrSpread(n int) error {
	if ValidSpread(n) {
		return nil
	}
	_, err := SchemeFor(n)
	return err
}

// checkSpread reports whether n chosen accounts can hold a file cut with the
// given scheme.
//
// It is ValidSpread's counterpart for a scheme somebody named. There is no
// arithmetic left to do — the code is already settled — so the only question
// left is whether every account chosen has a shard to hold.
//
// Fewer accounts than shards is allowed, and is the shortfall a vault with two
// clouds has always been allowed to store into: the shards there is no room for
// go unwritten and the file has fewer spares than its scheme allows for. How
// far that may go is a question about the placement policy rather than about
// the scheme, so it is BuildPlan's to answer and not asked here.
//
// More accounts than shards is refused, because it would leave an account
// somebody deliberately chose holding nothing — a choice quietly ignored rather
// than a tradeoff accepted.
func checkSpread(n int, scheme archive.Scheme) error {
	if err := scheme.Check(); err != nil {
		return err
	}
	if n > scheme.Total {
		return fmt.Errorf(
			"%s has %d shards and %d accounts were chosen — %d of them would hold nothing, "+
				"so either unpick them or cut wider",
			scheme, scheme.Total, n, n-scheme.Total)
	}
	return nil
}

// spreadWidth is how many accounts a file wants to be on, given how many it
// already prefers and how many are connected.
//
// Three by default. A file that prefers more than that is one already cut with
// a wider code, and it wants to stay that way: the preference is rounded up to
// the next scheme so that a six-account file which lost one account is filled
// back up to six rather than being narrowed — narrowing would mean re-encoding
// it, which is not something a top-up is allowed to decide.
//
// What is not connected cannot be used, so the answer is capped at the widest
// scheme that actually fits, and at everything there is when that is fewer than
// one scheme's worth.
func spreadWidth(available, preferred int) int {
	// The narrowest spread wide enough for what the file already prefers:
	// its count rounded up to whole groups, and never below one group.
	want := (preferred + archive.AccountsPerGroup - 1) /
		archive.AccountsPerGroup * archive.AccountsPerGroup
	if want < AccountsPerFile {
		want = AccountsPerFile
	}
	if want > archive.MaxAccounts {
		want = archive.MaxAccounts
	}
	if want <= available {
		return want
	}

	// More than there is to give. The widest spread that fits, or everything
	// there is when not even one group does — a vault with two clouds stores
	// files rather than refusing to.
	if available < archive.AccountsPerGroup {
		return available
	}
	return archive.WidestFor(available)
}

// SelectAccounts chooses which of the connected accounts one file's shards go
// to.
//
// preferred is a starting point rather than an answer: anything in it that is
// no longer connected is dropped, and if that leaves fewer accounts than the
// file wants to be on the rest are filled in at random from whatever else is
// connected. That is what a vault with no default at all wants — three accounts
// of this file's own — and what a file being re-encrypted wants, which is the
// accounts it is already on, made back up to its scheme's width if one has gone
// away.
//
// A choice someone actually made, whether for one upload or as the vault's
// default, does not come through here: it is followed exactly.
//
// want is how many accounts to end up on. Zero leaves it to spreadWidth, which
// is what an upload that named no scheme wants; a file cut with a scheme of its
// own passes that scheme's width instead, so a 3-of-5 file tops back up to five
// accounts rather than to the six the default family would round it to.
//
// The seed makes the random half deterministic per file rather than per
// process. Passing the archive ID — 128 random bits minted for this file alone
// — is what makes consecutive uploads land on different accounts instead of
// piling onto whichever three were connected first.
func SelectAccounts(available, preferred []string, want int, seed uint64) []string {
	connected := make(map[string]bool, len(available))
	for _, id := range available {
		connected[id] = true
	}

	chosen := make([]string, 0, len(preferred))
	taken := make(map[string]bool, len(preferred))
	for _, id := range preferred {
		if !connected[id] || taken[id] {
			continue
		}
		chosen = append(chosen, id)
		taken[id] = true
	}

	if want <= 0 {
		want = spreadWidth(len(available), len(chosen))
	} else if want > len(available) {
		// What is not connected cannot be used, however wide the scheme is. The
		// shards there is no room for simply go unwritten, which BuildPlan
		// reports as the shortfall it is.
		want = len(available)
	}
	if len(chosen) >= want {
		return chosen[:want]
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
		if len(chosen) == want {
			break
		}
		chosen = append(chosen, id)
	}
	return chosen
}

// Plan maps a shard number (1..n) to the provider ID that should store it. A
// shard missing from the map is deliberately not stored.
type Plan map[int]string

// BuildPlan decides which account holds which shard of a file, across the
// accounts chosen for it — the whole connected set, the vault's defaults, or the
// handful the upload named. Whichever it is, SelectAccounts has already narrowed
// it, and the count of them is what settled the scheme.
//
// Under the strict policy every account holds exactly one shard, at every
// width. That is the property the whole design rests on and widening does not
// weaken it: an attacker holding one account of nine has one shard of six
// needed, which is less of the file than one account of three ever held.
//
// The seed rotates the starting account so that consecutive uploads do not pile
// every shard 1 onto the same provider; passing a per-file value (the archive
// ID works well) spreads load evenly across accounts.
func BuildPlan(providerIDs []string, policy Policy, scheme archive.Scheme, seed uint64) (Plan, error) {
	n := len(providerIDs)
	if n == 0 {
		return nil, fmt.Errorf("no cloud accounts to store this file on")
	}
	if !policy.Valid() {
		return nil, fmt.Errorf("unknown placement policy %q", policy)
	}

	shards := scheme.Total
	if policy == PolicyStrict {
		if n < scheme.Data {
			return nil, fmt.Errorf(
				"strict placement gives each account one shard, so %s needs at least %d of them "+
					"to keep a file rebuildable (have %d) — connect or choose another account, or "+
					"switch this vault to the redundant policy",
				scheme, scheme.Data, n)
		}
		// Fewer accounts than shards means the shards there is no room for are
		// simply not written. The file is still rebuildable — that was checked
		// above — it just has fewer spares than the scheme allows for.
		if n < shards {
			shards = n
		}
	}

	offset := int(seed % uint64(n))
	plan := Plan{}
	for i := 0; i < shards; i++ {
		plan[i+1] = providerIDs[(offset+i)%n]
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
