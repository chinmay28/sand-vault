package archive

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chinmay28/sand-vault/internal/sandfile"
)

// Restore reads 2 or 3 part files, decrypts, reconstructs, decompresses,
// verifies integrity, and writes the original file to the output directory.
func Restore(partPaths []string, password, outputDir string) (string, error) {
	if len(partPaths) < MinPartsToRestore || len(partPaths) > PartCount {
		return "", fmt.Errorf("need %d or %d part files, got %d",
			MinPartsToRestore, PartCount, len(partPaths))
	}

	blobs := make([][]byte, 0, len(partPaths))
	for _, path := range partPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
		blobs = append(blobs, data)
	}

	decoded, err := DecodeBytes(blobs, password)
	if err != nil {
		return "", err
	}

	outputPath := filepath.Join(outputDir, decoded.Filename)
	if err := os.WriteFile(outputPath, decoded.Data, 0644); err != nil {
		return "", fmt.Errorf("writing output: %w", err)
	}

	return outputPath, nil
}

// RestoreChunked reads the part files of a chunked archive, rebuilds it chunk
// by chunk, and writes the original file to the output directory.
//
// It is the offline half of the chunked format: the route back when the vault
// itself is gone and all that survives is a manifest backup, a password, and
// enough part files on disk. Parts may arrive in any order and for any chunks —
// they are grouped by the chunk index each one carries in the clear, and every
// chunk needs however many its own scheme calls for — two of three for anything
// written before schemes existed, four of six or six of nine for a file cut
// wider.
func RestoreChunked(partPaths []string, dataKey []byte, outputDir string) (string, error) {
	if len(partPaths) < MinPartsToRestore {
		return "", fmt.Errorf("need at least %d part files, got %d",
			MinPartsToRestore, len(partPaths))
	}

	byChunk := map[uint32][][]byte{}
	seen := map[uint32]map[uint8]bool{}
	// How many shards each chunk needs, read off the parts themselves rather
	// than assumed: an offline restore has no index to consult.
	needed := map[uint32]int{}
	for _, path := range partPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
		part, err := sandfile.ReadPart(data)
		if err != nil {
			return "", fmt.Errorf("parsing %s: %w", path, err)
		}
		switch part.Header.Version {
		case sandfile.ChunkedFormatVersion, sandfile.ErasureFormatVersion:
		default:
			return "", fmt.Errorf("%s is format version %d, not a chunk — restore it with Restore",
				path, part.Header.Version)
		}

		index := part.Header.ChunkIndex
		// More shards than a chunk needs is fine; the same shard twice is not,
		// and would otherwise reach the decoder as a duplicate.
		if seen[index] == nil {
			seen[index] = map[uint8]bool{}
		}
		if seen[index][part.Header.PartNumber] {
			continue
		}
		seen[index][part.Header.PartNumber] = true
		byChunk[index] = append(byChunk[index], data)
		needed[index] = schemeOf(part.Header).Data
	}

	// A chunk missing from the middle would otherwise splice the file back
	// together shorter than it was, and quietly.
	count := uint32(len(byChunk))
	for index := uint32(0); index < count; index++ {
		want := needed[index]
		if want == 0 {
			want = MinPartsToRestore
		}
		if len(byChunk[index]) < want {
			return "", fmt.Errorf("chunk %d has %d part(s), need %d — the parts given cover chunks %s",
				index, len(byChunk[index]), want, describeChunks(byChunk))
		}
	}

	var rebuilt []byte
	var meta *sandfile.Metadata
	for index := uint32(0); index < count; index++ {
		decoded, err := DecodeChunk(byChunk[index], dataKey)
		if err != nil {
			return "", err
		}
		if meta == nil {
			meta = decoded.Meta
			if meta.ChunkCount != count {
				return "", fmt.Errorf(
					"the archive has %d chunks but parts for %d were given — restore needs all of them",
					meta.ChunkCount, count)
			}
		}
		rebuilt = append(rebuilt, decoded.Data...)
	}

	if uint64(len(rebuilt)) != meta.OriginalSize {
		return "", fmt.Errorf("rebuilt %d bytes, expected %d", len(rebuilt), meta.OriginalSize)
	}
	if sha256.Sum256(rebuilt) != meta.OriginalHash {
		return "", fmt.Errorf("%s failed its hash check after rebuilding", meta.Filename)
	}

	outputPath := filepath.Join(outputDir, meta.Filename)
	if err := os.WriteFile(outputPath, rebuilt, 0644); err != nil {
		return "", fmt.Errorf("writing output: %w", err)
	}
	return outputPath, nil
}

// describeChunks lists the chunk indexes a set of parts covers, for an error
// message that says what is missing rather than only that something is.
func describeChunks(byChunk map[uint32][][]byte) string {
	indexes := make([]int, 0, len(byChunk))
	for index := range byChunk {
		indexes = append(indexes, int(index))
	}
	sort.Ints(indexes)
	return strings.Trim(strings.Join(strings.Fields(fmt.Sprint(indexes)), ", "), "[]")
}

// PartsAreChunked reports whether a part file belongs to a chunked archive,
// which decides how the rest of them have to be read.
func PartsAreChunked(partPath string) (bool, error) {
	data, err := os.ReadFile(partPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", partPath, err)
	}
	part, err := sandfile.ReadPart(data)
	if err != nil {
		return false, fmt.Errorf("parsing %s: %w", partPath, err)
	}
	switch part.Header.Version {
	case sandfile.ChunkedFormatVersion, sandfile.ErasureFormatVersion:
		return true, nil
	}
	return false, nil
}
