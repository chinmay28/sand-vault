package archive

import (
	"fmt"

	"github.com/chinmay28/sand-vault/internal/splitter"
)

// A Scheme is how many shards a chunk becomes and how many of them rebuild it:
// k of n.
//
// SAND writes one family of them, **2m of 3m**: clouds are chosen in groups of
// three, and two thirds of the shards rebuild the file. That single rule is the
// whole of it — there is no list of blessed widths, and adding clouds keeps
// working the same way:
//
//	clouds  scheme    storage   losses survived   accounts needed to rebuild
//	     3  2-of-3      1.5×           1                      2
//	     6  4-of-6      1.5×           2                      4
//	     9  6-of-9      1.5×           3                      6
//	    12  8-of-12     1.5×           4                      8
//	     …
//	  3m    2m-of-3m    1.5×           m                     2m
//
// Holding the ratio at 2/3 is what makes the choice a free one. Storage is n/k
// of the file, which is 1.5× at every width, so a vault that grows from three
// clouds to thirty does not store one extra byte. What it spends is accounts,
// and what it buys grows linearly in both columns on the right.
//
// The right-hand column is the one worth dwelling on. It is how many accounts an
// attacker must hold *together* before the shards they have are enough to
// reassemble a file, and it rises with the width — so a wider vault is not
// merely more durable, it is harder to collude against. Meanwhile each account
// holds one shard of 2m, so what a single compromised provider plus the data key
// yields shrinks as the vault grows: a thirtieth of a file at 20-of-30, against
// a half at 2-of-3.
type Scheme struct {
	// Data is k, how many shards a rebuild needs. Total is n, how many exist.
	Data  int
	Total int
}

// AccountsPerGroup is the unit a spread is built from. A group of three
// accounts contributes two data shards and one parity shard, which is what
// fixes the ratio at 1.5× however many groups there are.
const AccountsPerGroup = 3

// dataPerGroup is how many of a group's three shards are data.
const dataPerGroup = 2

// MaxAccounts is the widest spread the format and the coder allow.
//
// A shard's number is one byte in its header, so there can be at most 255 of
// them, and a spread has to be whole groups — which puts the ceiling at 85
// groups. It is far past any plausible vault; the limit is stated so that the
// error a caller gets is a number rather than a surprise.
const MaxAccounts = 255 / AccountsPerGroup * AccountsPerGroup

// SchemeDefault is what an upload uses when nobody says otherwise, whatever is
// connected. Widening is a decision, not something a vault drifts into by
// having accounts available.
var SchemeDefault = Scheme{Data: dataPerGroup, Total: AccountsPerGroup}

// The next two widths, named because they are the ones that come up in prose
// and in tests. They are nothing but SchemeFor(6) and SchemeFor(9); the family
// does not stop here.
var (
	SchemeWide  = Scheme{Data: 4, Total: 6}
	SchemeWider = Scheme{Data: 6, Total: 9}
)

// SchemeFor returns the scheme that spreads a file over exactly n accounts.
//
// The count is the whole input because that is how the choice is actually made:
// somebody picks clouds, and how many they picked settles the code. Any positive
// multiple of three names one, up to MaxAccounts.
func SchemeFor(accounts int) (Scheme, error) {
	switch {
	case accounts <= 0:
		return Scheme{}, fmt.Errorf("choose the clouds to store on — none were named")
	case accounts%AccountsPerGroup != 0:
		return Scheme{}, fmt.Errorf(
			"%d clouds is not a spread SAND can cut — choose a multiple of %d (%d, %d, %d, …), "+
				"which gives %s, %s, %s and so on",
			accounts, AccountsPerGroup,
			AccountsPerGroup, 2*AccountsPerGroup, 3*AccountsPerGroup,
			SchemeDefault, SchemeWide, SchemeWider)
	case accounts > MaxAccounts:
		return Scheme{}, fmt.Errorf(
			"%d clouds is past the %d a shard number can count to", accounts, MaxAccounts)
	}
	return SchemeForGroups(accounts / AccountsPerGroup), nil
}

// SchemeForGroups is the rule itself: m groups of three accounts give 2m data
// shards of 3m.
func SchemeForGroups(groups int) Scheme {
	return Scheme{Data: dataPerGroup * groups, Total: AccountsPerGroup * groups}
}

// WidestFor is the widest spread that fits in n connected accounts — n rounded
// down to whole groups, and never below one group.
func WidestFor(accounts int) int {
	widest := accounts / AccountsPerGroup * AccountsPerGroup
	if widest > MaxAccounts {
		widest = MaxAccounts
	}
	if widest < AccountsPerGroup {
		return AccountsPerGroup
	}
	return widest
}

// Groups is how many groups of three the spread is built from.
func (s Scheme) Groups() int { return s.Total / AccountsPerGroup }

// Valid reports whether s is a scheme SAND writes.
func (s Scheme) Valid() bool {
	if s.Total <= 0 || s.Total > MaxAccounts || s.Total%AccountsPerGroup != 0 {
		return false
	}
	return s == SchemeForGroups(s.Groups())
}

// Tolerance is how many of the shards can be lost with the file still
// rebuildable: n − k, which is one per group.
func (s Scheme) Tolerance() int { return s.Total - s.Data }

// String is how a scheme is written wherever a person reads it.
func (s Scheme) String() string { return fmt.Sprintf("%d-of-%d", s.Data, s.Total) }

// ShardSize is how many bytes one shard of a chunk of the given compressed
// length occupies, before the header and tag each part carries.
func (s Scheme) ShardSize(compressed int) int {
	if s.Data <= 0 {
		return 0
	}
	return (compressed + s.Data - 1) / s.Data
}

// check validates a scheme at the point it is about to be used, so a zero value
// read out of an old index row is caught rather than dividing by zero.
func (s Scheme) check() error {
	if !s.Valid() {
		return fmt.Errorf(
			"%s is not a scheme SAND writes — they are %d data shards of %d, for any number of "+
				"groups up to %d accounts", s, dataPerGroup, AccountsPerGroup, MaxAccounts)
	}
	return splitter.ValidateScheme(s.Data, s.Total)
}

// LegacyScheme is how a file written before schemes existed is described. Every
// version 1 to 3 part belongs to one, because two of three was the only code
// those formats could express.
func LegacyScheme() Scheme { return SchemeDefault }
