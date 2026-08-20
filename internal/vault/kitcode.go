package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The secret that opens a recovery kit, when the kit does not answer to the
// vault password instead.
//
// A vault password is typed several times a day, which is what makes it
// memorable. A kit secret is typed once in three years — the profile of a
// secret people forget — and the event that destroys the machine is often the
// same event that destroys the password manager. Worse, a kit sealed under
// "the vault password" is sealed under the vault password *as it was on the
// day of the export*, so a kit from one year and a password changed the next
// leave somebody guessing which of their last four was current in March.
//
// So SAND generates the secret instead. It cannot be forgotten separately from
// the password because it was never the password, it is pinned to the kit
// rather than to a moment in a password history, and it has real entropy.

// kitCodeAlphabet is Crockford's base32.
//
// I, L, O and U are absent. The first three because a code gets handwritten and
// read back months later by somebody who did not write it, and 1/I, 1/l and
// 0/O are the pairs that costs. U because an alphabet of 32 symbols in
// five-character groups will eventually produce a word nobody wants printed on
// the slip of paper in their safe.
const kitCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// kitCodeEntropyBytes is 120 bits, which encodes to exactly 24 symbols with no
// padding — five bits a symbol, three whole base32 groups.
const kitCodeEntropyBytes = 15

// kitCodeSymbols is the whole code: 24 of entropy and one check symbol.
const kitCodeSymbols = 25

// kitCodeGroup is how many symbols are written between hyphens. The hyphens are
// for the hand and the eye and are not part of the value.
const kitCodeGroup = 5

// ErrKitCodeTypo is returned by NormalizeKitCode when the code is the right
// shape but its check symbol does not agree with the rest of it.
//
// Worth its own error because it is the one failure that is definitely the
// typist's and definitely fixable, and because it is settled without deriving
// anything: a wrong code costs a second and a half of Argon2id before it can be
// called wrong, and "wrong code" is the message that makes a person conclude
// their backup is dead.
var ErrKitCodeTypo = errors.New("that code has a typo in it")

// ErrKitCodeLength is returned when a code is not 25 symbols long.
var ErrKitCodeLength = errors.New("a recovery code is 25 characters long")

// kitCodeEnc encodes over the Crockford alphabet. Fifteen bytes is a multiple
// of five, so nothing is ever padded.
var kitCodeEnc = base32.NewEncoding(kitCodeAlphabet).WithPadding(base32.NoPadding)

// NewKitCode mints a recovery code: 120 bits from crypto/rand, encoded, with a
// check symbol on the end. The result is formatted for writing down.
func NewKitCode() (string, error) {
	raw := make([]byte, kitCodeEntropyBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generating a recovery code: %w", err)
	}
	body := kitCodeEnc.EncodeToString(raw)
	return FormatKitCode(body + string(kitCodeCheck(raw))), nil
}

// kitCodeCheck is the 25th symbol: the top five bits of the SHA-256 of the
// entropy the other 24 spell.
//
// Its only job is to tell "you have mistyped this" from "this is the wrong
// kit". It is not integrity — GCM is integrity — and it cannot say *where* the
// typo is: five bits over the whole code catch essentially every single-symbol
// slip and every adjacent transposition, and localise none of them.
func kitCodeCheck(raw []byte) byte {
	sum := sha256.Sum256(raw)
	return kitCodeAlphabet[sum[0]>>3]
}

// FormatKitCode writes the 25 bare symbols the way a code is meant to be read:
// five groups of five, hyphenated.
func FormatKitCode(bare string) string {
	var b strings.Builder
	for i, c := range bare {
		if i > 0 && i%kitCodeGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// NormalizeKitCode turns what somebody typed into the 25 bare symbols the KDF
// is fed, and refuses anything that is not a well-formed code.
//
// It is forgiving of everything that is a transcription artefact rather than a
// mistake: case, missing or extra hyphens, spaces anywhere, and the three
// letters the alphabet leaves out because they are confusable — a written I or
// l becomes 1, a written O becomes 0. Somebody reading their own handwriting
// back does not have to know which of each pair was meant.
//
// It runs before the KDF, which is the whole point of the check symbol: a typo
// is answered in microseconds instead of after a second and a half of Argon2id.
func NormalizeKitCode(raw string) (string, error) {
	var b strings.Builder
	b.Grow(kitCodeSymbols)

	for _, r := range raw {
		switch r {
		case '-', ' ', '\t', '\n', '\r', '_', '.':
			// Separators, however the typist grouped them.
			continue
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		switch r {
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(kitCodeAlphabet, r) {
			// Localisable, unlike a bad check symbol, so it says which
			// character rather than making somebody re-read all 25.
			return "", fmt.Errorf("%q is not a character a recovery code uses", string(r))
		}
		b.WriteRune(r)
	}

	code := b.String()
	if len(code) != kitCodeSymbols {
		return "", fmt.Errorf("%w — this one has %d", ErrKitCodeLength, len(code))
	}

	body, err := kitCodeEnc.DecodeString(code[:kitCodeSymbols-1])
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrKitCodeTypo, err)
	}
	if code[kitCodeSymbols-1] != kitCodeCheck(body) {
		return "", ErrKitCodeTypo
	}
	return code, nil
}

// LooksLikeKitCode reports whether a string is a well-formed recovery code,
// for a caller choosing between "this is a code" and "this is a password"
// without wanting an error either way.
func LooksLikeKitCode(raw string) bool {
	_, err := NormalizeKitCode(raw)
	return err == nil
}
