package splitter

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// ---------------------------------------------------------------------------
// Split Tests
// ---------------------------------------------------------------------------

func TestSplit_EvenLength(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}

	part1, part2, wasPadded := Split(data)

	if wasPadded {
		t.Fatal("even-length data should not be padded")
	}
	if len(part1) != 3 || len(part2) != 3 {
		t.Fatalf("expected 3+3, got %d+%d", len(part1), len(part2))
	}
	if !bytes.Equal(part1, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("part1 mismatch: %v", part1)
	}
	if !bytes.Equal(part2, []byte{0x04, 0x05, 0x06}) {
		t.Fatalf("part2 mismatch: %v", part2)
	}
}

func TestSplit_OddLength(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05}

	part1, part2, wasPadded := Split(data)

	if !wasPadded {
		t.Fatal("odd-length data should be padded")
	}
	if len(part1) != 3 || len(part2) != 3 {
		t.Fatalf("expected 3+3 after padding, got %d+%d", len(part1), len(part2))
	}
	if !bytes.Equal(part1, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("part1 mismatch: %v", part1)
	}
	if !bytes.Equal(part2, []byte{0x04, 0x05, 0x00}) {
		t.Fatalf("part2 mismatch (should end with 0x00 pad): %v", part2)
	}
}

func TestSplit_SingleByte(t *testing.T) {
	data := []byte{0xFF}

	part1, part2, wasPadded := Split(data)

	if !wasPadded {
		t.Fatal("single byte should be padded")
	}
	if len(part1) != 1 || len(part2) != 1 {
		t.Fatalf("expected 1+1, got %d+%d", len(part1), len(part2))
	}
	if part1[0] != 0xFF {
		t.Fatalf("part1 should be 0xFF, got 0x%02X", part1[0])
	}
	if part2[0] != 0x00 {
		t.Fatalf("part2 should be 0x00 (pad), got 0x%02X", part2[0])
	}
}

func TestSplit_TwoBytes(t *testing.T) {
	data := []byte{0xAA, 0xBB}

	part1, part2, wasPadded := Split(data)

	if wasPadded {
		t.Fatal("two bytes should not be padded")
	}
	if len(part1) != 1 || len(part2) != 1 {
		t.Fatalf("expected 1+1, got %d+%d", len(part1), len(part2))
	}
	if part1[0] != 0xAA || part2[0] != 0xBB {
		t.Fatal("data mismatch")
	}
}

func TestSplit_EmptyInput(t *testing.T) {
	part1, part2, wasPadded := Split([]byte{})

	if wasPadded {
		t.Fatal("empty input should not be padded")
	}
	if len(part1) != 0 || len(part2) != 0 {
		t.Fatal("empty input should produce empty parts")
	}
}

func TestSplit_DoesNotMutateInput(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03}
	data := make([]byte, len(original))
	copy(data, original)

	Split(data)

	if !bytes.Equal(data, original) {
		t.Fatal("Split should not mutate the input slice")
	}
}

func TestSplit_EqualParts(t *testing.T) {
	// Test a range of sizes to ensure parts are always equal
	for size := 0; size <= 100; size++ {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i)
		}

		part1, part2, _ := Split(data)
		if len(part1) != len(part2) {
			t.Fatalf("size %d: parts are unequal (%d vs %d)", size, len(part1), len(part2))
		}
	}
}

// ---------------------------------------------------------------------------
// XOR Tests
// ---------------------------------------------------------------------------

func TestXOR_Basic(t *testing.T) {
	a := []byte{0xFF, 0x00, 0xAA}
	b := []byte{0x0F, 0xF0, 0x55}
	expected := []byte{0xF0, 0xF0, 0xFF}

	result, err := XOR(a, b)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, expected) {
		t.Fatalf("XOR mismatch: got %v, want %v", result, expected)
	}
}

func TestXOR_SelfXOR_IsZero(t *testing.T) {
	data := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	result, err := XOR(data, data)
	if err != nil {
		t.Fatal(err)
	}

	expected := make([]byte, len(data))
	if !bytes.Equal(result, expected) {
		t.Fatal("XOR of data with itself should be all zeros")
	}
}

func TestXOR_WithZero_IsIdentity(t *testing.T) {
	data := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	zeros := make([]byte, len(data))

	result, err := XOR(data, zeros)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, data) {
		t.Fatal("XOR with zeros should return original data")
	}
}

func TestXOR_Commutativity(t *testing.T) {
	a := []byte{0x12, 0x34, 0x56, 0x78}
	b := []byte{0x9A, 0xBC, 0xDE, 0xF0}

	ab, _ := XOR(a, b)
	ba, _ := XOR(b, a)

	if !bytes.Equal(ab, ba) {
		t.Fatal("XOR should be commutative: a^b == b^a")
	}
}

func TestXOR_Associativity(t *testing.T) {
	a := []byte{0x11, 0x22, 0x33}
	b := []byte{0x44, 0x55, 0x66}
	c := []byte{0x77, 0x88, 0x99}

	ab, _ := XOR(a, b)
	ab_c, _ := XOR(ab, c)

	bc, _ := XOR(b, c)
	a_bc, _ := XOR(a, bc)

	if !bytes.Equal(ab_c, a_bc) {
		t.Fatal("XOR should be associative: (a^b)^c == a^(b^c)")
	}
}

func TestXOR_ReversibleWithThirdPart(t *testing.T) {
	// Core SAND property: part3 = part1 ^ part2
	// Therefore: part1 = part2 ^ part3, and part2 = part1 ^ part3
	part1 := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	part2 := []byte{0xCA, 0xFE, 0xBA, 0xBE}

	part3, _ := XOR(part1, part2)

	// Recover part2 from part1 + part3
	recovered2, _ := XOR(part1, part3)
	if !bytes.Equal(recovered2, part2) {
		t.Fatal("failed to recover part2 from part1 ^ part3")
	}

	// Recover part1 from part2 + part3
	recovered1, _ := XOR(part2, part3)
	if !bytes.Equal(recovered1, part1) {
		t.Fatal("failed to recover part1 from part2 ^ part3")
	}
}

func TestXOR_UnequalLengths(t *testing.T) {
	a := []byte{0x01, 0x02}
	b := []byte{0x01, 0x02, 0x03}

	_, err := XOR(a, b)
	if err == nil {
		t.Fatal("XOR of unequal-length slices should fail")
	}
}

func TestXOR_Empty(t *testing.T) {
	result, err := XOR([]byte{}, []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatal("XOR of empty slices should be empty")
	}
}

// ---------------------------------------------------------------------------
// Reconstruct Tests
// ---------------------------------------------------------------------------

func TestReconstruct_FromParts12_EvenData(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	_ = part3 // not used for 1+2 reconstruction

	result, err := ReconstructXOR(map[int][]byte{1: part1, 2: part2}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatalf("reconstruction from parts 1+2 failed: got %v, want %v", result, original)
	}
}

func TestReconstruct_FromParts13_EvenData(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	result, err := ReconstructXOR(map[int][]byte{1: part1, 3: part3}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatalf("reconstruction from parts 1+3 failed: got %v, want %v", result, original)
	}
}

func TestReconstruct_FromParts23_EvenData(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	result, err := ReconstructXOR(map[int][]byte{2: part2, 3: part3}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatalf("reconstruction from parts 2+3 failed: got %v, want %v", result, original)
	}
}

func TestReconstruct_FromParts12_OddData(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05} // 5 bytes — odd
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	_ = part3

	result, err := ReconstructXOR(map[int][]byte{1: part1, 2: part2}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatalf("reconstruction from parts 1+2 (odd) failed: got %v, want %v", result, original)
	}
}

func TestReconstruct_FromParts13_OddData(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	result, err := ReconstructXOR(map[int][]byte{1: part1, 3: part3}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatalf("reconstruction from parts 1+3 (odd) failed: got %v, want %v", result, original)
	}
}

func TestReconstruct_FromParts23_OddData(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	result, err := ReconstructXOR(map[int][]byte{2: part2, 3: part3}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatalf("reconstruction from parts 2+3 (odd) failed: got %v, want %v", result, original)
	}
}

func TestReconstruct_FromAll3Parts(t *testing.T) {
	original := []byte("all three parts provided")
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	result, err := ReconstructXOR(map[int][]byte{1: part1, 2: part2, 3: part3}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, original) {
		t.Fatal("reconstruction from all 3 parts failed")
	}
}

func TestReconstruct_SingleByte_AllCombinations(t *testing.T) {
	original := []byte{0x42} // single byte — odd, will be padded
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	if !wasPadded {
		t.Fatal("single byte should trigger padding")
	}

	combos := []map[int][]byte{
		{1: part1, 2: part2},
		{1: part1, 3: part3},
		{2: part2, 3: part3},
	}

	for i, combo := range combos {
		result, err := ReconstructXOR(combo, wasPadded)
		if err != nil {
			t.Fatalf("combo %d failed: %v", i, err)
		}
		if !bytes.Equal(result, original) {
			t.Fatalf("combo %d: got %v, want %v", i, result, original)
		}
	}
}

func TestReconstruct_TwoByte_AllCombinations(t *testing.T) {
	original := []byte{0xAA, 0xBB} // even — no padding
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	if wasPadded {
		t.Fatal("two bytes should not be padded")
	}

	combos := []map[int][]byte{
		{1: part1, 2: part2},
		{1: part1, 3: part3},
		{2: part2, 3: part3},
	}

	for i, combo := range combos {
		result, err := ReconstructXOR(combo, wasPadded)
		if err != nil {
			t.Fatalf("combo %d failed: %v", i, err)
		}
		if !bytes.Equal(result, original) {
			t.Fatalf("combo %d: got %v, want %v", i, result, original)
		}
	}
}

func TestReconstruct_LargeOddData_AllCombinations(t *testing.T) {
	// 1023 bytes — odd
	original := make([]byte, 1023)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}

	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	if !wasPadded {
		t.Fatal("1023 bytes should be padded")
	}

	combos := []struct {
		name  string
		parts map[int][]byte
	}{
		{"parts 1+2", map[int][]byte{1: part1, 2: part2}},
		{"parts 1+3", map[int][]byte{1: part1, 3: part3}},
		{"parts 2+3", map[int][]byte{2: part2, 3: part3}},
	}

	for _, combo := range combos {
		result, err := ReconstructXOR(combo.parts, wasPadded)
		if err != nil {
			t.Fatalf("%s failed: %v", combo.name, err)
		}
		if !bytes.Equal(result, original) {
			t.Fatalf("%s produced wrong result", combo.name)
		}
	}
}

func TestReconstruct_LargeEvenData_AllCombinations(t *testing.T) {
	// 1024 bytes — even
	original := make([]byte, 1024)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}

	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	if wasPadded {
		t.Fatal("1024 bytes should not be padded")
	}

	combos := []struct {
		name  string
		parts map[int][]byte
	}{
		{"parts 1+2", map[int][]byte{1: part1, 2: part2}},
		{"parts 1+3", map[int][]byte{1: part1, 3: part3}},
		{"parts 2+3", map[int][]byte{2: part2, 3: part3}},
	}

	for _, combo := range combos {
		result, err := ReconstructXOR(combo.parts, wasPadded)
		if err != nil {
			t.Fatalf("%s failed: %v", combo.name, err)
		}
		if !bytes.Equal(result, original) {
			t.Fatalf("%s produced wrong result", combo.name)
		}
	}
}

func TestReconstruct_FewerThan2Parts(t *testing.T) {
	part1 := []byte{0x01, 0x02}

	_, err := ReconstructXOR(map[int][]byte{1: part1}, false)
	if err == nil {
		t.Fatal("should fail with fewer than 2 parts")
	}

	_, err = ReconstructXOR(map[int][]byte{}, false)
	if err == nil {
		t.Fatal("should fail with empty map")
	}
}

func TestReconstruct_EmptyData(t *testing.T) {
	original := []byte{}
	part1, part2, wasPadded := Split(original)
	part3, _ := XOR(part1, part2)

	result, err := ReconstructXOR(map[int][]byte{1: part1, 2: part2, 3: part3}, wasPadded)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatal("empty data should reconstruct to empty")
	}
}

// ---------------------------------------------------------------------------
// End-to-End Split + Reconstruct Tests with Various Sizes
// ---------------------------------------------------------------------------

func TestSplitReconstruct_VaryingSizes(t *testing.T) {
	// Test a range of interesting sizes, focusing on edge cases
	sizes := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 15, 16, 17, 31, 32, 33,
		63, 64, 65, 100, 255, 256, 257, 1000, 1023, 1024, 1025,
		4095, 4096, 4097, 65535, 65536, 65537}

	for _, size := range sizes {
		original := make([]byte, size)
		if size > 0 {
			for i := range original {
				original[i] = byte(i % 256)
			}
		}

		part1, part2, wasPadded := Split(original)
		part3, err := XOR(part1, part2)
		if err != nil {
			t.Fatalf("size %d: XOR failed: %v", size, err)
		}

		// Test all three 2-of-3 combinations
		for _, combo := range []struct {
			name  string
			parts map[int][]byte
		}{
			{"1+2", map[int][]byte{1: part1, 2: part2}},
			{"1+3", map[int][]byte{1: part1, 3: part3}},
			{"2+3", map[int][]byte{2: part2, 3: part3}},
		} {
			result, err := ReconstructXOR(combo.parts, wasPadded)
			if err != nil {
				t.Fatalf("size %d, %s: reconstruct failed: %v", size, combo.name, err)
			}
			if !bytes.Equal(result, original) {
				t.Fatalf("size %d, %s: data mismatch", size, combo.name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Property: Padding is correctly tracked
// ---------------------------------------------------------------------------

func TestSplit_PaddingFlag_Correctness(t *testing.T) {
	for size := 0; size <= 100; size++ {
		data := make([]byte, size)
		_, _, wasPadded := Split(data)

		expectedPadded := size%2 != 0
		if wasPadded != expectedPadded {
			t.Fatalf("size %d: wasPadded=%v, expected %v", size, wasPadded, expectedPadded)
		}
	}
}

// ---------------------------------------------------------------------------
// Property: XOR part3 is correct
// ---------------------------------------------------------------------------

func TestXOR_Part3_Verification(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	part1, part2, _ := Split(data)
	part3, _ := XOR(part1, part2)

	// Manually verify: part1=[01,02,03,04], part2=[05,06,07,08]
	// part3[0] = 0x01 ^ 0x05 = 0x04
	// part3[1] = 0x02 ^ 0x06 = 0x04
	// part3[2] = 0x03 ^ 0x07 = 0x04
	// part3[3] = 0x04 ^ 0x08 = 0x0C
	expected := []byte{0x04, 0x04, 0x04, 0x0C}
	if !bytes.Equal(part3, expected) {
		t.Fatalf("part3 XOR mismatch: got %v, want %v", part3, expected)
	}
}
