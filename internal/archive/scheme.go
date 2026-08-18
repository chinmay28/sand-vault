package archive

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chinmay28/sand-vault/internal/splitter"
)

// A Scheme is how many shards a chunk becomes and how many of them rebuild it:
// k of n.
//
// Any k of n the coder can build is a scheme SAND writes — 2-of-3, 3-of-5,
// 6-of-10, 20-of-30. What k and n are chosen to be is a decision with three
// consequences, and they do not move together:
//
//	scheme    storage   losses survived   accounts an attacker needs together
//	2-of-3      1.50×           1                          2
//	3-of-5      1.67×           2                          3
//	2-of-5      2.50×           3                          2
//	6-of-9      1.50×           3                          6
//	6-of-10     1.67×           4                          6
//	20-of-30    1.50×          10                         20
//
// Storage is n/k. Losses survived is n−k. The last column is k, and it is the
// one worth dwelling on: it is how many accounts an attacker must hold
// *together* before the shards they have are enough to reassemble a file, and
// lowering k to buy durability spends exactly that. 2-of-5 survives three
// losses where 2-of-3 survives one, but two accounts still rebuild the file and
// each of them now holds half the plaintext — it buys durability with storage
// and with secrecy, not with storage alone.
//
// The 2m-of-3m family (SchemeFor, SchemeForGroups) is what a count of clouds
// names when nobody chooses otherwise, because it holds storage at 1.5× while
// both other columns grow with the width. It is a default, not a limit.
type Scheme struct {
	// Data is k, how many shards a rebuild needs. Total is n, how many exist.
	Data  int
	Total int
}

// AccountsPerGroup is the unit the default family is built from. A group of
// three accounts contributes two data shards and one parity shard, which is what
// fixes that family's ratio at 1.5× however many groups there are.
const AccountsPerGroup = 3

// dataPerGroup is how many of a group's three shards are data.
const dataPerGroup = 2

// MaxAccounts is the widest spread the format and the coder allow.
//
// A shard's number is one byte in its header, so there can be at most 255 of
// them. It is far past any plausible vault; the limit is stated so that the
// error a caller gets is a number rather than a surprise.
const MaxAccounts = splitter.MaxShards

// MinData is the narrowest code worth writing. One data shard is a whole copy
// of the file on every account it lands on, which is replication rather than
// splitting, and it hands the plaintext to anyone who breaks into any single
// account. SAND refuses it: the promise that one account reveals nothing is not
// a setting.
const MinData = 2

// SchemeDefault is what an upload uses when nobody says otherwise, whatever is
// connected. Widening is a decision, not something a vault drifts into by
// having accounts available.
var SchemeDefault = Scheme{Data: dataPerGroup, Total: AccountsPerGroup}

// The next two widths of the default family, named because they are the ones
// that come up in prose and in tests. They are nothing but SchemeFor(6) and
// SchemeFor(9); the family does not stop here.
var (
	SchemeWide  = Scheme{Data: 4, Total: 6}
	SchemeWider = Scheme{Data: 6, Total: 9}
)

// SchemeOf is a code somebody chose: k of n, checked.
//
// It is the counterpart of SchemeFor. Where that one answers "what should n
// clouds be cut as", this one takes both numbers as given and only says whether
// they are a code SAND can write.
func SchemeOf(data, total int) (Scheme, error) {
	s := Scheme{Data: data, Total: total}
	if err := s.Check(); err != nil {
		return Scheme{}, err
	}
	return s, nil
}

// ParseScheme reads a scheme written the way people write one: "2-of-3",
// "6-of-10". It is the inverse of String, and it is what an API field or a
// command-line flag carrying a scheme is parsed with.
func ParseScheme(s string) (Scheme, error) {
	text := strings.TrimSpace(strings.ToLower(s))
	data, total, ok := strings.Cut(text, "-of-")
	if !ok {
		return Scheme{}, fmt.Errorf(
			"%q is not a scheme — write one as k-of-n, such as %s or %s", s, SchemeDefault, SchemeWide)
	}
	k, err := strconv.Atoi(strings.TrimSpace(data))
	if err != nil {
		return Scheme{}, fmt.Errorf("%q is not a scheme — %q is not a number of shards", s, data)
	}
	n, err := strconv.Atoi(strings.TrimSpace(total))
	if err != nil {
		return Scheme{}, fmt.Errorf("%q is not a scheme — %q is not a number of shards", s, total)
	}
	return SchemeOf(k, n)
}

// SchemeFor returns the scheme that spreads a file over exactly n accounts when
// nobody has named one.
//
// The count is the whole input because that is how the choice is usually made:
// somebody picks clouds, and how many they picked settles the code. The answer
// comes from the default family, so any positive multiple of three names one,
// up to MaxAccounts — a count in between names no default, and has to be
// spelled out as a scheme instead (SchemeOf).
func SchemeFor(accounts int) (Scheme, error) {
	switch {
	case accounts <= 0:
		return Scheme{}, fmt.Errorf("choose the clouds to store on — none were named")
	case accounts%AccountsPerGroup != 0:
		return Scheme{}, fmt.Errorf(
			"%d clouds names no scheme on its own — choose a multiple of %d (%d, %d, %d, …), "+
				"which gives %s, %s, %s and so on, or say which code to cut with "+
				"(any k-of-%d, such as %s)",
			accounts, AccountsPerGroup,
			AccountsPerGroup, 2*AccountsPerGroup, 3*AccountsPerGroup,
			SchemeDefault, SchemeWide, SchemeWider,
			accounts, Scheme{Data: suggestedData(accounts), Total: accounts})
	case accounts > MaxAccounts:
		return Scheme{}, fmt.Errorf(
			"%d clouds is past the %d a shard number can count to", accounts, MaxAccounts)
	}
	return SchemeForGroups(accounts / AccountsPerGroup), nil
}

// suggestedData is the k that holds the default family's 1.5× as closely as n
// allows, used only to make the error above name something usable.
func suggestedData(accounts int) int {
	k := accounts * dataPerGroup / AccountsPerGroup
	if k < MinData {
		return MinData
	}
	return k
}

// SchemeForGroups is the default family's rule: m groups of three accounts give
// 2m data shards of 3m.
func SchemeForGroups(groups int) Scheme {
	return Scheme{Data: dataPerGroup * groups, Total: AccountsPerGroup * groups}
}

// WidestFor is the widest default-family spread that fits in n connected
// accounts — n rounded down to whole groups, and never below one group.
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

// Groups is how many groups of three the spread is built from. It is meaningful
// only for a scheme out of the default family; Family reports whether this is
// one.
func (s Scheme) Groups() int { return s.Total / AccountsPerGroup }

// Family reports whether s belongs to the 2m-of-3m family a count of clouds
// names by itself — which is what lets a caller say "1.5×, as usual" rather
// than spelling the tradeoff out.
func (s Scheme) Family() bool {
	return s.Total%AccountsPerGroup == 0 && s == SchemeForGroups(s.Groups())
}

// Valid reports whether s is a code SAND can write: at least MinData shards to
// rebuild from, no more shards needed than exist, and no more than a shard
// number can count to.
func (s Scheme) Valid() bool { return s.Check() == nil }

// Tolerance is how many of the shards can be lost with the file still
// rebuildable: n − k.
func (s Scheme) Tolerance() int { return s.Total - s.Data }

// Storage is how many times the file's own size the scheme occupies once every
// shard is stored: n/k.
func (s Scheme) Storage() float64 {
	if s.Data <= 0 {
		return 0
	}
	return float64(s.Total) / float64(s.Data)
}

// String is how a scheme is written wherever a person reads it, and what
// ParseScheme reads back.
func (s Scheme) String() string { return fmt.Sprintf("%d-of-%d", s.Data, s.Total) }

// ShardSize is how many bytes one shard of a chunk of the given compressed
// length occupies, before the header and tag each part carries.
func (s Scheme) ShardSize(compressed int) int {
	if s.Data <= 0 {
		return 0
	}
	return (compressed + s.Data - 1) / s.Data
}

// Check validates a scheme at the point it is about to be used, so a zero value
// read out of an old index row is caught rather than dividing by zero, and so
// that a scheme somebody typed is refused where it was typed rather than deep
// in the coder.
func (s Scheme) Check() error {
	switch {
	case s.Data < MinData:
		return fmt.Errorf(
			"%s is not a scheme SAND writes — a file has to take at least %d shards to rebuild, "+
				"or a single account would hold the whole of it", s, MinData)
	case s.Total < s.Data:
		return fmt.Errorf(
			"%s is not a scheme SAND writes — it needs more shards to rebuild than it makes", s)
	case s.Total > MaxAccounts:
		return fmt.Errorf(
			"%s is not a scheme SAND writes — a shard number counts to %d", s, MaxAccounts)
	}
	return splitter.ValidateScheme(s.Data, s.Total)
}

// LegacyScheme is how a file written before schemes existed is described. Every
// version 1 to 3 part belongs to one, because two of three was the only code
// those formats could express.
func LegacyScheme() Scheme { return SchemeDefault }
