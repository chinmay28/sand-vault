// Package snapshot is a folder packed into one sealed archive: every file and
// folder under it, compressed as hard as zstd will, encrypted under a password
// of its own, and cut into part files no bigger than the caller allows.
//
// It is deliberately not the vault's format. A file in the vault is split
// across accounts and sealed under the vault's key, so reading it back needs
// the vault. A snapshot needs its part files and its password and nothing
// else — which is the point of one: it is what is still readable when the
// vault that made it is not.
//
// # The layers, outermost first
//
//	part files   each a small plaintext header, then a slice of the sealed
//	             stream; put end to end, in order, they are the stream
//	sealed       AES-256-GCM frames under a key Argon2id makes of the password
//	compressed   one zstd stream at the library's best level
//	archive      a JSON description of the snapshot, then a tar of the tree
//
// The package knows nothing of vaults, clouds or machines. It writes parts to
// a Store and reads them back from one; see store.go for that contract.
package snapshot

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Version is the format this package writes. A reader refuses any other
// rather than guess at it.
const Version = 1

// magic opens every part file, so a part is recognisable whatever it has been
// renamed to.
const magic = "SANDSNAP"

const (
	saltSize = 16
	idSize   = 16

	// headerSize is the plaintext header every part starts with: the magic,
	// the version, the three Argon2id costs, the salt, the snapshot's ID and
	// the part's number.
	headerSize = len(magic) + 1 + 4 + 4 + 1 + saltSize + idSize + 4
)

// MinPartSize is the smallest part a snapshot may be cut into. A part has to
// hold its header and something besides, and a limit below this is a typo
// rather than a wish for ten thousand files.
const MinPartSize = 64 << 10

// The costs a reader will accept from a part header. The header is plaintext
// and whoever holds the part can rewrite it, so a figure read from one is a
// request, not a fact: without a ceiling a doctored part asks the machine
// opening it for a terabyte of memory.
const (
	maxKDFMemory  = 2 << 20 // KiB, so 2 GiB
	maxKDFTime    = 64
	maxKDFThreads = 64
)

var (
	// ErrPassword is a snapshot that would not open under the password given.
	// A damaged first part reads the same way, because telling the two apart
	// is exactly what an authenticated cipher refuses to do.
	ErrPassword = errors.New("wrong password, or the snapshot is damaged")

	// ErrDamaged is a snapshot that opened and then stopped making sense: a
	// part altered, cut short, or out of order.
	ErrDamaged = errors.New("the snapshot is damaged")

	// ErrNotSnapshot is a file that does not start the way a part does.
	ErrNotSnapshot = errors.New("not a SAND snapshot")

	// ErrIncomplete is a snapshot whose parts are not all there.
	ErrIncomplete = errors.New("the snapshot is incomplete")
)

// partHeader is what a part says about itself before the sealed bytes start.
//
// Every part carries all of it, though only the first is needed to open the
// stream: a part found on its own can then still say which snapshot it
// belongs to and where in it, and a part from another snapshot dropped among
// these is refused by name rather than by a cipher error halfway through.
type partHeader struct {
	kdf  crypto.Argon2Params
	salt [saltSize]byte
	id   [idSize]byte
	part uint32 // 1-based
}

func (h partHeader) marshal() []byte {
	out := make([]byte, 0, headerSize)
	out = append(out, magic...)
	out = append(out, Version)
	out = binary.BigEndian.AppendUint32(out, h.kdf.Time)
	out = binary.BigEndian.AppendUint32(out, h.kdf.Memory)
	out = append(out, h.kdf.Threads)
	out = append(out, h.salt[:]...)
	out = append(out, h.id[:]...)
	out = binary.BigEndian.AppendUint32(out, h.part)
	return out
}

// readHeader reads one part's header and refuses what it cannot be.
func readHeader(r io.Reader) (partHeader, error) {
	buf := make([]byte, headerSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return partHeader{}, fmt.Errorf("%w: the file is too short to be a part", ErrNotSnapshot)
	}
	if string(buf[:len(magic)]) != magic {
		return partHeader{}, ErrNotSnapshot
	}
	at := len(magic)
	if v := buf[at]; v != Version {
		return partHeader{}, fmt.Errorf("snapshot format %d is not one this version of SAND reads", v)
	}
	at++

	var h partHeader
	h.kdf.Time = binary.BigEndian.Uint32(buf[at:])
	h.kdf.Memory = binary.BigEndian.Uint32(buf[at+4:])
	h.kdf.Threads = buf[at+8]
	h.kdf.SaltLen, h.kdf.KeyLen = saltSize, 32
	at += 9
	copy(h.salt[:], buf[at:])
	at += saltSize
	copy(h.id[:], buf[at:])
	at += idSize
	h.part = binary.BigEndian.Uint32(buf[at:])

	switch {
	case h.kdf.Time == 0 || h.kdf.Time > maxKDFTime,
		h.kdf.Memory == 0 || h.kdf.Memory > maxKDFMemory,
		h.kdf.Threads == 0 || h.kdf.Threads > maxKDFThreads:
		return partHeader{}, fmt.Errorf("%w: its header asks for key costs no snapshot is made with", ErrDamaged)
	case h.part == 0:
		return partHeader{}, fmt.Errorf("%w: a part numbered zero", ErrDamaged)
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

// Extension ends every part file's name.
const Extension = ".sandsnap"

// stampLayout is the moment a snapshot was taken, as its name carries it.
const stampLayout = "20060102T150405Z"

// A part file is named for when its snapshot was taken and by the first bytes
// of its ID, and for nothing else. The folder it is a snapshot of is not in
// the name: a folder's name is as private as what is in it, and the name of a
// file on somebody else's storage is the one thing here that is not sealed.
//
//	snapshot-20261111T192233Z-ab12cd34.sandsnap                a whole snapshot
//	snapshot-20261111T192233Z-ab12cd34.part001.sandsnap        the first of several
//	snapshot-20261111T192233Z-ab12cd34.part003.last.sandsnap   the last of them
//
// The last part says that it is the last, which is what lets a listing tell a
// finished snapshot from one that was interrupted without opening anything.
var namePattern = regexp.MustCompile(
	`^(snapshot-([0-9]{8}T[0-9]{6}Z)-[0-9a-f]{8})(?:\.part([0-9]{3,6})(\.last)?)?` + regexp.QuoteMeta(Extension) + `$`)

// baseName is the name every part of one snapshot starts with.
func baseName(takenAt time.Time, id [idSize]byte) string {
	return "snapshot-" + takenAt.UTC().Format(stampLayout) + "-" + hex.EncodeToString(id[:4])
}

// partName is what one part is stored as. A snapshot that fits in one file is
// that file, with no part number to explain.
func partName(base string, part int, last bool) string {
	switch {
	case last && part == 1:
		return base + Extension
	case last:
		return fmt.Sprintf("%s.part%03d.last%s", base, part, Extension)
	default:
		return fmt.Sprintf("%s.part%03d%s", base, part, Extension)
	}
}

// parsedName is a part file's name read back.
type parsedName struct {
	base    string
	takenAt time.Time
	part    int
	last    bool
}

// parseName reads a part's name, and reports whether it is one at all.
func parseName(name string) (parsedName, bool) {
	match := namePattern.FindStringSubmatch(name)
	if match == nil {
		return parsedName{}, false
	}
	takenAt, err := time.Parse(stampLayout, match[2])
	if err != nil {
		return parsedName{}, false
	}
	out := parsedName{base: match[1], takenAt: takenAt, part: 1, last: true}
	if match[3] != "" {
		part, err := strconv.Atoi(match[3])
		if err != nil || part < 1 {
			return parsedName{}, false
		}
		out.part, out.last = part, match[4] != ""
	}
	return out, true
}

// IsPartName reports whether a file's name is one a snapshot part is stored
// under.
func IsPartName(name string) bool {
	_, ok := parseName(name)
	return ok
}

// ---------------------------------------------------------------------------
// Sets
// ---------------------------------------------------------------------------

// Set is one snapshot as a listing of its store shows it: the part files that
// make it up, read from their names alone.
type Set struct {
	// Name is what the snapshot is called — the start of every part's name.
	Name string `json:"name"`

	// TakenAt is when it was taken, from the name.
	TakenAt time.Time `json:"taken_at"`

	// Parts are the part files there, in the order they are read.
	Parts []string `json:"parts"`

	// Bytes is what the parts add up to.
	Bytes int64 `json:"bytes"`

	// Complete says every part from the first to the one marked last is
	// there. A snapshot that was interrupted, or has since lost a part, is
	// listed all the same — it is still taking up room, and still deletable —
	// and cannot be restored.
	Complete bool `json:"complete"`
}

// group sorts a store's objects into the snapshots they are parts of. Objects
// that are not parts are passed over: a store is a folder other things live
// in too.
func group(objects []Object) []Set {
	type member struct {
		parsedName
		name string
		size int64
	}
	byBase := map[string][]member{}
	for _, o := range objects {
		if p, ok := parseName(o.Name); ok {
			byBase[p.base] = append(byBase[p.base], member{p, o.Name, o.Size})
		}
	}

	sets := make([]Set, 0, len(byBase))
	for base, members := range byBase {
		sort.Slice(members, func(i, j int) bool { return members[i].part < members[j].part })

		set := Set{Name: base, TakenAt: members[0].takenAt, Complete: true}
		for i, m := range members {
			set.Parts = append(set.Parts, m.name)
			set.Bytes += m.size
			// In order from one, with the last of them — and only the last —
			// saying so.
			if m.part != i+1 || m.last != (i == len(members)-1) {
				set.Complete = false
			}
		}
		sets = append(sets, set)
	}

	// Newest first, which is the one somebody restoring most often wants.
	sort.Slice(sets, func(i, j int) bool {
		if !sets[i].TakenAt.Equal(sets[j].TakenAt) {
			return sets[i].TakenAt.After(sets[j].TakenAt)
		}
		return sets[i].Name < sets[j].Name
	})
	return sets
}
