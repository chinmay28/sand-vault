package splitter

import (
	"bytes"
	"crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"sort"
	"testing"
)

// The family SAND writes: 2m data shards of 3m, for any number of groups. The
// widths below are a spread of small, middling and large rather than a list of
// blessed ones — the code has no list, and neither should the test.
var schemes = func() []struct {
	name        string
	data, total int
} {
	var out []struct {
		name        string
		data, total int
	}
	for _, groups := range []int{1, 2, 3, 4, 7, 15, 40, 85} {
		data, total := 2*groups, 3*groups
		out = append(out, struct {
			name        string
			data, total int
		}{fmt.Sprintf("%d-of-%d", data, total), data, total})
	}
	return out
}()

// subsetLimit is how many k-subsets a case will enumerate before switching to
// random sampling. C(12,8) is 495 and C(18,12) is 18564, but C(120,80) has more
// digits than the file has bytes — so past this the claim is checked on a
// sample, which is what a property of *every* subset needs when every is not
// enumerable.
const subsetLimit = 20000

// subsets returns every way of choosing k of the shard numbers 1…n, or a random
// sample of them when there are too many to walk.
func subsets(t *testing.T, total, k int) [][]int {
	t.Helper()

	if binomial(total, k) <= subsetLimit {
		var out [][]int
		var walk func(start int, chosen []int)
		walk = func(start int, chosen []int) {
			if len(chosen) == k {
				out = append(out, append([]int(nil), chosen...))
				return
			}
			for i := start; i <= total; i++ {
				walk(i+1, append(chosen, i))
			}
		}
		walk(1, nil)
		return out
	}

	// A sample, plus the two subsets most likely to be special-cased wrong: the
	// data shards alone (the systematic fast path) and the parity-heavy tail.
	out := [][]int{}
	all := make([]int, total)
	for i := range all {
		all[i] = i + 1
	}
	out = append(out, append([]int(nil), all[:k]...))
	out = append(out, append([]int(nil), all[total-k:]...))

	rng := mrand.New(mrand.NewPCG(uint64(total), uint64(k)))
	for i := 0; i < 200; i++ {
		shuffled := append([]int(nil), all...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		pick := append([]int(nil), shuffled[:k]...)
		sort.Ints(pick)
		out = append(out, pick)
	}
	return out
}

// binomial is C(n, k), saturating rather than overflowing — the caller only
// wants to know whether it is small enough to enumerate.
func binomial(n, k int) int {
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := 1; i <= k; i++ {
		result = result * (n - k + i) / i
		if result > subsetLimit {
			return subsetLimit + 1
		}
	}
	return result
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// The load-bearing test: for every scheme, every combination of exactly k
// surviving shards rebuilds the original byte for byte.
func TestEveryCombinationOfKShardsRebuilds(t *testing.T) {
	for _, s := range schemes {
		for _, size := range []int{1, 2, 5, 17, 1024, 4097} {
			t.Run(fmt.Sprintf("%s/%d bytes", s.name, size), func(t *testing.T) {
				original := randomBytes(t, size)
				shards, err := Encode(original, s.data, s.total)
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				if len(shards) != s.total {
					t.Fatalf("got %d shards, want %d", len(shards), s.total)
				}

				for _, keep := range subsets(t, s.total, s.data) {
					held := map[int][]byte{}
					for _, index := range keep {
						held[index] = shards[index-1]
					}
					got, err := Reconstruct(held, s.data, s.total, size)
					if err != nil {
						t.Fatalf("shards %v: Reconstruct: %v", keep, err)
					}
					if !bytes.Equal(got, original) {
						t.Fatalf("shards %v rebuilt the wrong bytes", keep)
					}
				}
			})
		}
	}
}

// One shard short is not "nearly enough" — it is nothing, and has to say so
// rather than returning something plausible.
func TestFewerThanKShardsIsRefused(t *testing.T) {
	for _, s := range schemes {
		t.Run(s.name, func(t *testing.T) {
			original := randomBytes(t, 2048)
			shards, err := Encode(original, s.data, s.total)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			for _, keep := range subsets(t, s.total, s.data-1) {
				held := map[int][]byte{}
				for _, index := range keep {
					held[index] = shards[index-1]
				}
				if _, err := Reconstruct(held, s.data, s.total, len(original)); err == nil {
					t.Fatalf("shards %v rebuilt a file with one shard too few", keep)
				}
			}
		})
	}
}

// Every shard is 1/k of the file, so the whole set is n/k of it — 1.5× at
// every one of SAND's schemes, which is the property that makes widening free
// in storage terms.
func TestStorageOverheadIsTheSchemeRatio(t *testing.T) {
	for _, s := range schemes {
		t.Run(s.name, func(t *testing.T) {
			original := randomBytes(t, 6000)
			shards, err := Encode(original, s.data, s.total)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}

			shardLen := (len(original) + s.data - 1) / s.data
			stored := 0
			for i, shard := range shards {
				if len(shard) != shardLen {
					t.Errorf("shard %d is %d bytes, want %d", i+1, len(shard), shardLen)
				}
				stored += len(shard)
			}

			if want := s.total * shardLen; stored != want {
				t.Errorf("stored %d bytes across the shards, want %d", stored, want)
			}

			// Every scheme in the family is 2m of 3m, so the code itself stores
			// 1.5× — that constant is the reason widening is free.
			if ratio := float64(s.total) / float64(s.data); ratio != 1.5 {
				t.Fatalf("%s is not a 2m-of-3m scheme (ratio %.2f)", s.name, ratio)
			}

			// What is actually written is 1.5× plus the padding up to a multiple
			// of k, so the slack scales with k rather than being a fixed fudge.
			// At a real chunk size that padding is noise — 170 bytes against
			// 16 MiB — but at 6 kB across 170 shards it is not, and a test that
			// pretended otherwise would be hiding a real cost of very wide
			// schemes on very small inputs.
			slack := 1.5 * (1 + float64(s.data)/float64(len(original)))
			if ratio := float64(stored) / float64(len(original)); ratio > slack {
				t.Errorf("stored %.3f× the original, want at most %.3f×", ratio, slack)
			}
		})
	}
}

// The data shards are consecutive slices of the input, which is what keeps a
// single compromised account holding 1/k of the file rather than a window onto
// all of it — and what lets the common read path concatenate instead of
// solving anything.
func TestDataShardsAreSlicesOfTheInput(t *testing.T) {
	for _, s := range schemes {
		t.Run(s.name, func(t *testing.T) {
			original := randomBytes(t, s.data*100)
			shards, err := Encode(original, s.data, s.total)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			for j := 0; j < s.data; j++ {
				want := original[j*100 : (j+1)*100]
				if !bytes.Equal(shards[j], want) {
					t.Errorf("data shard %d is not the matching slice of the input", j+1)
				}
			}
		})
	}
}

// A parity shard on its own must not be the plaintext wearing a hat. This is
// not the confidentiality guarantee — encryption is — but a parity shard that
// echoed its inputs would mean the coefficients were wrong.
func TestParityShardsAreNotTheData(t *testing.T) {
	for _, s := range schemes {
		t.Run(s.name, func(t *testing.T) {
			original := bytes.Repeat([]byte("plaintext-marker"), 64)
			shards, err := Encode(original, s.data, s.total)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			for p := s.data; p < s.total; p++ {
				if bytes.Contains(shards[p], []byte("plaintext-marker")) {
					t.Errorf("parity shard %d carries the input in the clear", p+1)
				}
				for j := 0; j < s.data; j++ {
					if bytes.Equal(shards[p], shards[j]) {
						t.Errorf("parity shard %d is a copy of data shard %d", p+1, j+1)
					}
				}
			}
		})
	}
}

// Mismatched shard lengths mean shards from two different files, or a
// truncated download. Either way the rebuild must fail rather than produce
// something that passes for a file.
func TestMismatchedShardLengthsAreRefused(t *testing.T) {
	shards, err := Encode(randomBytes(t, 1024), 4, 6)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	held := map[int][]byte{1: shards[0], 2: shards[1], 3: shards[2], 4: shards[3][:10]}
	if _, err := Reconstruct(held, 4, 6, 1024); err == nil {
		t.Error("rebuilt from shards of different lengths, want a refusal")
	}
}

func TestSchemeValidation(t *testing.T) {
	for _, bad := range [][2]int{{0, 3}, {-1, 3}, {4, 3}, {2, 300}} {
		if err := ValidateScheme(bad[0], bad[1]); err == nil {
			t.Errorf("ValidateScheme(%d, %d) accepted an impossible code", bad[0], bad[1])
		}
	}
	for _, s := range schemes {
		if err := ValidateScheme(s.data, s.total); err != nil {
			t.Errorf("ValidateScheme(%s): %v", s.name, err)
		}
	}
}

// A file that compresses to nothing still has to produce shards, or there
// would be nothing to store and nothing to rebuild from.
func TestEmptyInputStillProducesShards(t *testing.T) {
	for _, s := range schemes {
		t.Run(s.name, func(t *testing.T) {
			shards, err := Encode(nil, s.data, s.total)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if len(shards) != s.total {
				t.Fatalf("got %d shards, want %d", len(shards), s.total)
			}
			held := map[int][]byte{}
			for i := s.total - s.data; i < s.total; i++ {
				held[i+1] = shards[i]
			}
			got, err := Reconstruct(held, s.data, s.total, 0)
			if err != nil {
				t.Fatalf("Reconstruct: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("rebuilt %d bytes from an empty input", len(got))
			}
		})
	}
}

// The field itself, checked independently of the code built on it.
func TestFieldArithmetic(t *testing.T) {
	for a := 1; a < 256; a++ {
		if got := gfMul(byte(a), gfInv(byte(a))); got != 1 {
			t.Fatalf("%d × %d⁻¹ = %d, want 1", a, a, got)
		}
		for b := 1; b < 256; b++ {
			if gfMul(byte(a), byte(b)) != gfMul(byte(b), byte(a)) {
				t.Fatalf("multiplication is not commutative at %d, %d", a, b)
			}
			if got := gfDiv(gfMul(byte(a), byte(b)), byte(b)); got != byte(a) {
				t.Fatalf("(%d × %d) ÷ %d = %d, want %d", a, b, b, got, a)
			}
		}
	}
	if gfMul(0, 5) != 0 || gfMul(5, 0) != 0 {
		t.Error("multiplication by zero is not zero")
	}
}
