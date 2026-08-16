package archive

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"

	"github.com/chinmay28/sand-vault/internal/compress"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/sandfile"
	"github.com/chinmay28/sand-vault/internal/splitter"
)

// Reading a pre-chunking archive without holding it all.
//
// DecodeBytes is the original reader and it materialises the file four times
// over: the raw parts, the decrypted parts, the two of them concatenated, and
// the decompressed result — all alive at once. Measured on incompressible data
// that comes to roughly three times the file, so a 4 GB film wants around 12 GB
// and a Raspberry Pi with 16 GB does not survive it.
//
// The format cannot be read at an offset — each part is sealed under one
// AES-GCM tag, so no plaintext can be released before the whole part has been
// verified, which is exactly why chunking replaced it. But it can be read
// *sequentially*, which is all a conversion needs, and that is what this does:
//
//   - each part is decrypted in place, reusing the buffer it arrived in rather
//     than allocating a second copy of it
//   - the two halves are concatenated by reading them one after the other
//     rather than into a third buffer
//   - decompression streams to the caller's writer instead of returning a slice
//
// What remains is the one thing the format really does require: both halves of
// the compressed form in memory at once — one file's worth, not three.
//
// DecodeBytes stays for what it is good at, which is small payloads already in
// hand: thumbnail packs, and standalone restore.

// LegacyMeta is what a pre-chunking archive says about the file inside it.
type LegacyMeta struct {
	Filename     string
	OriginalSize uint64
	OriginalHash [32]byte
	ArchiveID    [16]byte
	PartsUsed    []int
}

// DecodeLegacyTo rebuilds a pre-chunking archive into w and reports what it
// held. blobs are the raw parts, of which at least MinPartsToRestore must be
// present; they are consumed — decryption happens in place, so a caller must
// not read them afterwards.
//
// The hash recorded when the file was stored is checked against what actually
// came out, which is the same guarantee DecodeBytes gives and the reason a
// conversion can be trusted to have converted the right bytes.
func DecodeLegacyTo(blobs [][]byte, password string, w io.Writer) (*LegacyMeta, error) {
	if len(blobs) < MinPartsToRestore || len(blobs) > PartCount {
		return nil, fmt.Errorf("need %d or %d parts, got %d",
			MinPartsToRestore, PartCount, len(blobs))
	}

	parsed := make(map[int]*sandfile.Part, len(blobs))
	for i, blob := range blobs {
		part, err := sandfile.ReadPart(blob)
		if err != nil {
			return nil, fmt.Errorf("parsing part %d: %w", i+1, err)
		}
		if part.Header.Version == sandfile.ChunkedFormatVersion {
			return nil, fmt.Errorf(
				"part %d is a chunk (format version %d) — read it with DecodeChunk",
				i+1, part.Header.Version)
		}
		pn := int(part.Header.PartNumber)
		if _, exists := parsed[pn]; exists {
			return nil, fmt.Errorf("duplicate part number %d", pn)
		}
		parsed[pn] = part
	}

	var refHeader *sandfile.Header
	for _, p := range parsed {
		if refHeader == nil {
			refHeader = p.Header
			continue
		}
		if p.Header.ArchiveID != refHeader.ArchiveID {
			return nil, fmt.Errorf("archive ID mismatch: parts belong to different archives")
		}
	}

	key := crypto.DeriveKey(password, refHeader.Salt, crypto.Argon2Params{
		Time:    refHeader.Argon2Time,
		Memory:  refHeader.Argon2Memory,
		Threads: refHeader.Argon2Threads,
		SaltLen: uint32(len(refHeader.Salt)),
		KeyLen:  32,
	})
	defer crypto.ZeroBytes(key)

	var refMeta *sandfile.Metadata
	halves := make(map[int][]byte, len(parsed))
	used := make([]int, 0, len(parsed))
	for pn, p := range parsed {
		// In place: the ciphertext is half a film and there is no reason to
		// hold a second copy of it while GCM verifies the first.
		meta, partData, err := p.OpenInPlace(key)
		if err != nil {
			return nil, fmt.Errorf("decrypting part %d: %w", pn, err)
		}
		if refMeta == nil {
			refMeta = meta
		} else if meta.OriginalHash != refMeta.OriginalHash {
			return nil, fmt.Errorf("original hash mismatch: parts belong to different archives")
		}
		halves[pn] = partData
		used = append(used, pn)
	}

	// Reconstruct decides which two halves make the file and computes the
	// missing one by XOR when the parity part is standing in. Only the XOR case
	// allocates, and only one half's worth.
	first, second, err := splitter.Halves(halves)
	if err != nil {
		return nil, fmt.Errorf("reconstructing data: %w", err)
	}

	// The two halves in order, as one stream. io.MultiReader is what keeps this
	// from being a third full-length buffer.
	compressed := io.MultiReader(bytes.NewReader(first), bytes.NewReader(second))
	if refMeta.WasPadded {
		// A file of odd length had a zero byte appended before it was split, so
		// the last byte of the second half is not part of it.
		compressed = io.LimitReader(compressed, int64(len(first)+len(second))-1)
	}

	digest := sha256.New()
	size, err := compress.DecompressStream(compressed, io.MultiWriter(w, digest))
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}

	if uint64(size) != refMeta.OriginalSize {
		return nil, fmt.Errorf("size mismatch: expected %d, got %d", refMeta.OriginalSize, size)
	}
	if got := sum32(digest); got != refMeta.OriginalHash {
		return nil, fmt.Errorf("integrity check failed: SHA-256 hash mismatch")
	}

	sortInts(used)
	return &LegacyMeta{
		Filename:     refMeta.Filename,
		OriginalSize: refMeta.OriginalSize,
		OriginalHash: refMeta.OriginalHash,
		ArchiveID:    refHeader.ArchiveID,
		PartsUsed:    used,
	}, nil
}

func sum32(h hash.Hash) [32]byte {
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
