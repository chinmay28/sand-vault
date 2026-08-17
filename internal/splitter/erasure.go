package splitter

import (
	"errors"
	"fmt"
)

// The erasure coder: k data shards, n total, any k of which rebuild the
// original.
//
// It generalises what Split and XOR do. Those produce two halves and their
// parity — two of three — and cannot produce anything wider, because over
// GF(2) there is no third independent combination of two symbols. Here the
// coefficients come from GF(2^8) (gf256.go), so a parity shard is an
// independent weighted sum of the data shards and there is room for as many as
// the field allows.
//
// The code is *systematic*: the first k shards are the plaintext cut into k
// consecutive pieces, and only the remaining n−k are computed. Two things
// follow, and both matter more than the elegance does. A read that happens to
// collect the k data shards rebuilds by concatenating them, with no field
// arithmetic at all — which is the same fast path "parts 1 and 2" always had.
// And a shard is still a slice of the compressed stream rather than a
// transform of the whole of it, so what one compromised account holds is 1/k
// of the file and not a window onto all of it.
//
// The parity rows are a Cauchy matrix, whose defining property is that every
// square submatrix is invertible. That is exactly the MDS guarantee the code
// needs — *any* k of the n shards can be inverted back to the data — and it
// comes without the change of basis a Vandermonde construction would need.

// MaxShards is the widest code this package builds.
//
// Two ceilings meet just below each other and the lower one wins. The Cauchy
// construction needs the k column keys and the n−k row keys to be distinct
// field elements, of which GF(2^8) has 256; and a shard's number travels as one
// byte in its header, which counts to 255. So 255.
const MaxShards = 255

// ValidateScheme reports whether k data shards of n total is a code this
// package can build.
func ValidateScheme(data, total int) error {
	switch {
	case data < 1:
		return fmt.Errorf("a code needs at least one data shard, got %d", data)
	case total < data:
		return fmt.Errorf("a code cannot have fewer shards (%d) than data shards (%d)", total, data)
	case total > MaxShards:
		return fmt.Errorf("a code cannot have more than %d shards, got %d", MaxShards, total)
	}
	return nil
}

// encodingMatrix builds the n×k systematic generator: an identity on top, so
// the first k shards are the data itself, and Cauchy rows beneath it for the
// parity.
//
// The Cauchy entry at parity row p, column j is 1/(x_p ⊕ y_j), with the two key
// sets drawn disjointly from the field — columns get 0…k−1 and parity rows get
// k…n−1 — so no denominator can be zero.
func encodingMatrix(data, total int) [][]byte {
	m := make([][]byte, total)
	for i := 0; i < data; i++ {
		m[i] = make([]byte, data)
		m[i][i] = 1
	}
	for p := data; p < total; p++ {
		row := make([]byte, data)
		x := byte(p)
		for j := 0; j < data; j++ {
			row[j] = gfInv(x ^ byte(j))
		}
		m[p] = row
	}
	return m
}

// Encode cuts data into `data` shards and computes the parity that brings the
// total to `total`. The result is indexed by shard number − 1, so out[0] is
// shard 1, matching how a part number is written into a header.
//
// Every shard is the same length, ceil(len(data)/k), because the input is
// zero-padded up to a multiple of k. The padding is not recorded here: the
// caller already knows the true length — it is the compressed size in the
// metadata every part carries — and Reconstruct takes it as an argument.
func Encode(data []byte, dataShards, totalShards int) ([][]byte, error) {
	if err := ValidateScheme(dataShards, totalShards); err != nil {
		return nil, err
	}

	shardLen := (len(data) + dataShards - 1) / dataShards
	if shardLen == 0 {
		// An empty input still has to produce shards, or there would be
		// nothing to store and nothing to rebuild from.
		shardLen = 1
	}

	// One buffer holding the padded plaintext, cut into the data shards. The
	// tail past len(data) is already zero.
	padded := make([]byte, shardLen*dataShards)
	copy(padded, data)

	out := make([][]byte, totalShards)
	for j := 0; j < dataShards; j++ {
		out[j] = padded[j*shardLen : (j+1)*shardLen : (j+1)*shardLen]
	}

	matrix := encodingMatrix(dataShards, totalShards)
	for p := dataShards; p < totalShards; p++ {
		shard := make([]byte, shardLen)
		for j := 0; j < dataShards; j++ {
			gfAddInto(shard, out[j], matrix[p][j])
		}
		out[p] = shard
	}
	return out, nil
}

// Reconstruct rebuilds the original from any `data` of the shards, keyed by
// shard number (1…total). size is the true length of what was encoded, which
// is what the padding is trimmed back to.
//
// Extra shards beyond the k needed are ignored rather than checked against the
// others: they would agree, and reading them to find that out is work a read
// path that already raced for the first k should not pay for.
func Reconstruct(shards map[int][]byte, dataShards, totalShards, size int) ([]byte, error) {
	if err := ValidateScheme(dataShards, totalShards); err != nil {
		return nil, err
	}

	// The lowest-numbered shards, so that the same set of survivors always
	// rebuilds the same way and the identity fast path is preferred whenever
	// the data shards are among them.
	have := make([]int, 0, dataShards)
	shardLen := -1
	for index := 1; index <= totalShards && len(have) < dataShards; index++ {
		shard, ok := shards[index]
		if !ok || shard == nil {
			continue
		}
		if shardLen < 0 {
			shardLen = len(shard)
		} else if len(shard) != shardLen {
			return nil, fmt.Errorf("shard %d is %d bytes, expected %d — the shards do not belong together",
				index, len(shard), shardLen)
		}
		have = append(have, index)
	}
	if len(have) < dataShards {
		return nil, fmt.Errorf("rebuilding needs %d of the %d shards, got %d",
			dataShards, totalShards, len(have))
	}
	if size > shardLen*dataShards {
		return nil, fmt.Errorf("%d shards of %d bytes cannot hold %d bytes",
			dataShards, shardLen, size)
	}

	rebuilt := make([]byte, 0, shardLen*dataShards)

	// The fast path, and the common one: the survivors are the data shards
	// themselves, so the original is their concatenation and no arithmetic
	// happens at all.
	systematic := true
	for i, index := range have {
		if index != i+1 {
			systematic = false
			break
		}
	}
	if systematic {
		for _, index := range have {
			rebuilt = append(rebuilt, shards[index]...)
		}
		return rebuilt[:size], nil
	}

	matrix := encodingMatrix(dataShards, totalShards)
	square := make([][]byte, dataShards)
	for i, index := range have {
		square[i] = matrix[index-1]
	}
	inverse, err := invert(square)
	if err != nil {
		return nil, fmt.Errorf("rebuilding from shards %v: %w", have, err)
	}

	for j := 0; j < dataShards; j++ {
		piece := make([]byte, shardLen)
		for i, index := range have {
			gfAddInto(piece, shards[index], inverse[j][i])
		}
		rebuilt = append(rebuilt, piece...)
	}
	return rebuilt[:size], nil
}

// invert inverts a square matrix over GF(2^8) by Gauss–Jordan elimination,
// reducing [M | I] to [I | M⁻¹].
//
// A singular matrix is not something a correct caller can produce — every
// square submatrix of the generator is invertible, which is the whole point of
// the Cauchy rows — so an error here means shards were mislabelled rather than
// that the code failed.
func invert(m [][]byte) ([][]byte, error) {
	n := len(m)
	work := make([][]byte, n)
	inv := make([][]byte, n)
	for i := range m {
		if len(m[i]) != n {
			return nil, errors.New("cannot invert a matrix that is not square")
		}
		work[i] = append([]byte(nil), m[i]...)
		inv[i] = make([]byte, n)
		inv[i][i] = 1
	}

	for col := 0; col < n; col++ {
		pivot := -1
		for row := col; row < n; row++ {
			if work[row][col] != 0 {
				pivot = row
				break
			}
		}
		if pivot < 0 {
			return nil, errors.New("the shards given do not form an invertible set")
		}
		work[col], work[pivot] = work[pivot], work[col]
		inv[col], inv[pivot] = inv[pivot], inv[col]

		if scale := work[col][col]; scale != 1 {
			factor := gfInv(scale)
			for j := 0; j < n; j++ {
				work[col][j] = gfMul(work[col][j], factor)
				inv[col][j] = gfMul(inv[col][j], factor)
			}
		}

		for row := 0; row < n; row++ {
			if row == col || work[row][col] == 0 {
				continue
			}
			factor := work[row][col]
			for j := 0; j < n; j++ {
				work[row][j] ^= gfMul(work[col][j], factor)
				inv[row][j] ^= gfMul(inv[col][j], factor)
			}
		}
	}
	return inv, nil
}
