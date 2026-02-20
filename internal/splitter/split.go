package splitter

import (
	"errors"
	"fmt"
)

// Split divides data into two equal halves. If the data has an odd number of
// bytes, a single 0x00 byte is appended before splitting. The wasPadded flag
// indicates whether padding was applied.
func Split(data []byte) (part1, part2 []byte, wasPadded bool) {
	d := data
	if len(d)%2 != 0 {
		d = make([]byte, len(data)+1)
		copy(d, data)
		d[len(data)] = 0x00
		wasPadded = true
	}

	mid := len(d) / 2
	part1 = make([]byte, mid)
	part2 = make([]byte, mid)
	copy(part1, d[:mid])
	copy(part2, d[mid:])
	return
}

// XOR produces the byte-wise XOR of two equal-length byte slices.
// Returns an error if the slices have different lengths.
func XOR(a, b []byte) ([]byte, error) {
	if len(a) != len(b) {
		return nil, fmt.Errorf("XOR: slices must be equal length (got %d and %d)", len(a), len(b))
	}

	result := make([]byte, len(a))
	for i := range a {
		result[i] = a[i] ^ b[i]
	}
	return result, nil
}

// Reconstruct rebuilds the original (possibly padded) compressed data from
// any two of the three parts. Parts are provided as a map keyed by part
// number (1, 2, or 3). The wasPadded flag is used to strip the trailing
// padding byte if it was added during Split.
//
// Reconstruction logic:
//   - parts 1+2: concatenate directly
//   - parts 1+3: part2 = XOR(part1, part3), then concatenate
//   - parts 2+3: part1 = XOR(part2, part3), then concatenate
func Reconstruct(parts map[int][]byte, wasPadded bool) ([]byte, error) {
	has1 := parts[1] != nil
	has2 := parts[2] != nil
	has3 := parts[3] != nil

	count := 0
	if has1 {
		count++
	}
	if has2 {
		count++
	}
	if has3 {
		count++
	}
	if count < 2 {
		return nil, errors.New("reconstruct requires at least 2 of 3 parts")
	}

	var part1, part2 []byte
	var err error

	switch {
	case has1 && has2:
		part1 = parts[1]
		part2 = parts[2]
	case has1 && has3:
		part1 = parts[1]
		part2, err = XOR(parts[1], parts[3])
		if err != nil {
			return nil, fmt.Errorf("failed to reconstruct part2: %w", err)
		}
	case has2 && has3:
		part1, err = XOR(parts[2], parts[3])
		if err != nil {
			return nil, fmt.Errorf("failed to reconstruct part1: %w", err)
		}
		part2 = parts[2]
	default:
		return nil, errors.New("invalid part combination")
	}

	if len(part1) != len(part2) {
		return nil, fmt.Errorf("parts have mismatched lengths (%d vs %d)", len(part1), len(part2))
	}

	// Concatenate part1 + part2
	result := make([]byte, len(part1)+len(part2))
	copy(result, part1)
	copy(result[len(part1):], part2)

	// Strip padding byte if it was added
	if wasPadded && len(result) > 0 {
		result = result[:len(result)-1]
	}

	return result, nil
}
