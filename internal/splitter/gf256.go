package splitter

// Arithmetic in GF(2^8), the field the erasure coder is built on.
//
// The whole reason this field exists here is that the original scheme —
// halves A and B plus the parity A⊕B — cannot be widened. XOR is addition in
// GF(2), and over GF(2) there are exactly three non-zero combinations of two
// symbols: A, B, and A⊕B. A fourth shard would have to repeat one of them.
//
// Move to GF(2^8) and every byte value becomes a coefficient, so a parity
// shard can be any independent combination of the data shards and there are
// enough of them to spare. That is what turns "two of three" into "k of n" —
// see erasure.go, which uses nothing from this file but mul, div and inv.
//
// The field is bytes: addition is XOR (its own inverse, so subtraction is the
// same operation), and multiplication is polynomial multiplication modulo the
// primitive polynomial below. Multiplication is done through logarithm tables
// rather than bit by bit, which turns it into two lookups and an add.

// primitivePoly is x^8 + x^4 + x^3 + x^2 + 1, the conventional choice for
// Reed–Solomon codes. Any primitive polynomial of degree 8 would do; what
// matters is that 2 generates the whole multiplicative group, which is what
// makes the log tables below cover every non-zero byte exactly once.
const primitivePoly = 0x11d

var (
	// gfExp[i] is 2^i in the field, laid out twice end to end so that a sum of
	// two logarithms — which can reach 508 — can be looked up without folding
	// it back into range first.
	gfExp [512]byte

	// gfLog[v] is the power of 2 that equals v. gfLog[0] is meaningless and
	// never read: zero has no logarithm, and every caller special-cases it.
	gfLog [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)

		// x *= 2, reduced modulo the primitive polynomial when it overflows a
		// byte. Doubling is a left shift because the field's characteristic is
		// two.
		x <<= 1
		if x&0x100 != 0 {
			x ^= primitivePoly
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

// gfMul multiplies two field elements.
func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// gfDiv divides a by b. Dividing by zero is a programming error rather than a
// runtime condition — the coder only ever divides by a pivot it has already
// found to be non-zero — so it panics rather than growing an error return
// through every matrix operation.
func gfDiv(a, b byte) byte {
	if b == 0 {
		panic("splitter: division by zero in GF(256)")
	}
	if a == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])-int(gfLog[b])+255]
}

// gfInv is the multiplicative inverse of a non-zero element.
func gfInv(a byte) byte { return gfDiv(1, a) }

// gfAddInto adds src scaled by c into dst, byte by byte: dst ⊕= c·src.
//
// It is the only place in the coder that touches bulk data, so both encoding
// and decoding come down to a handful of calls to it. Scaling by zero is a
// no-op and scaling by one is a plain XOR; both are common enough in a
// systematic code to be worth skipping the table lookups for.
func gfAddInto(dst, src []byte, c byte) {
	switch c {
	case 0:
		return
	case 1:
		for i := range dst {
			dst[i] ^= src[i]
		}
		return
	}
	logC := int(gfLog[c])
	for i, v := range src {
		if v != 0 {
			dst[i] ^= gfExp[logC+int(gfLog[v])]
		}
	}
}
