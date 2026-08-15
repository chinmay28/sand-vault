package archive

import (
	"fmt"

	"github.com/chinmay28/sand-vault/internal/compress"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/sandfile"
	"github.com/chinmay28/sand-vault/internal/splitter"
)

// DefaultChunkSize is the plaintext length of every chunk but an archive's
// last.
//
// It is a trade between how much has to be fetched to answer a seek and how
// many objects a file becomes. Each chunk splits in half, so opening one costs
// about ChunkSize bytes of transfer across the two accounts that win the race —
// roughly a second on a home connection at 16 MiB, which is what a player waits
// before the first frame. Going smaller shortens that wait but multiplies the
// object count, and the cloud APIs SAND writes to rate-limit by request rather
// than by byte.
const DefaultChunkSize = 16 << 20

// MaxChunkCount bounds how many chunks one archive may be cut into, so a
// corrupt or hostile metadata block cannot make a reader allocate unboundedly.
// At the default chunk size this is far past any plausible file.
const MaxChunkCount = 1 << 22

// ChunkPlan is everything about a chunked archive that is fixed before the
// first chunk is encoded. Every chunk of the archive carries a copy of it, so
// any two parts of any one chunk are enough to describe the whole file.
type ChunkPlan struct {
	ArchiveID    [16]byte
	Filename     string
	OriginalHash [32]byte // SHA-256 of the whole plaintext
	OriginalSize uint64
	ChunkSize    uint32
	ChunkCount   uint32
}

// PlanChunks works out how a file of the given size is cut up.
//
// A zero-length file is one empty chunk rather than none, so that every stored
// file has at least one object and an empty file round-trips like any other.
func PlanChunks(archiveID [16]byte, filename string, originalHash [32]byte, originalSize uint64, chunkSize uint32) (ChunkPlan, error) {
	if filename == "" {
		return ChunkPlan{}, fmt.Errorf("filename must not be empty")
	}
	if chunkSize == 0 {
		return ChunkPlan{}, fmt.Errorf("chunk size must not be zero")
	}

	count := uint64(1)
	if originalSize > 0 {
		count = (originalSize + uint64(chunkSize) - 1) / uint64(chunkSize)
	}
	if count > MaxChunkCount {
		return ChunkPlan{}, fmt.Errorf(
			"file of %d bytes needs %d chunks of %d, over the %d limit — use a larger chunk size",
			originalSize, count, chunkSize, MaxChunkCount)
	}

	return ChunkPlan{
		ArchiveID:    archiveID,
		Filename:     filename,
		OriginalHash: originalHash,
		OriginalSize: originalSize,
		ChunkSize:    chunkSize,
		ChunkCount:   uint32(count),
	}, nil
}

// PlaintextSize returns the plaintext length of the given chunk. Every chunk is
// ChunkSize bytes except the last, which holds whatever remains.
func (p ChunkPlan) PlaintextSize(index uint32) (uint64, error) {
	if index >= p.ChunkCount {
		return 0, fmt.Errorf("chunk %d out of range: archive has %d", index, p.ChunkCount)
	}
	offset := uint64(index) * uint64(p.ChunkSize)
	if remaining := p.OriginalSize - offset; remaining < uint64(p.ChunkSize) {
		return remaining, nil
	}
	return uint64(p.ChunkSize), nil
}

// EncodedChunk is one chunk's three parts, ready to scatter.
type EncodedChunk struct {
	Index          uint32
	CompressedSize uint64

	// Parts holds the serialized part files indexed by part number - 1, the
	// same way Encoded does.
	Parts [PartCount][]byte
}

// DecodedChunk is one chunk rebuilt from its parts.
type DecodedChunk struct {
	Index     uint32
	Data      []byte
	Meta      *sandfile.Metadata
	PartsUsed []int
}

// EncodeChunk compresses, splits, XORs and seals one chunk of an archive.
//
// It is EncodeBytes applied to a slice of a file rather than the whole of one,
// with two differences that matter. The key comes from HKDF over the vault's
// data key and this chunk's index instead of an Argon2id pass over a password,
// because there is no password here to stretch and a chunked file would pay
// that cost thousands of times. And the chunk index sits in the cleartext
// header, which makes it associated data: a chunk resealed at a different
// offset in the same file fails its tag rather than silently rebuilding a file
// whose middle has been rearranged.
func EncodeChunk(plan ChunkPlan, index uint32, plaintext, master []byte) (*EncodedChunk, error) {
	want, err := plan.PlaintextSize(index)
	if err != nil {
		return nil, err
	}
	if uint64(len(plaintext)) != want {
		return nil, fmt.Errorf("chunk %d of %d must be %d bytes, got %d",
			index, plan.ChunkCount, want, len(plaintext))
	}

	compressed, err := compress.Compress(plaintext)
	if err != nil {
		return nil, fmt.Errorf("compressing chunk %d: %w", index, err)
	}

	part1, part2, wasPadded := splitter.Split(compressed)
	part3, err := splitter.XOR(part1, part2)
	if err != nil {
		return nil, fmt.Errorf("generating XOR part for chunk %d: %w", index, err)
	}

	key, err := crypto.DeriveChunkKey(master, plan.ArchiveID, index)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(key)

	// Every part of every chunk carries the whole archive's description. It is
	// duplicated ChunkCount times over, which sounds wasteful and is not: about
	// a hundred bytes against a 16 MiB chunk. What it buys is the property §7
	// already had — any two parts are self-sufficient — surviving the move to
	// chunking, so a recovered chunk still knows what file it belongs to.
	meta := &sandfile.Metadata{
		Filename:       plan.Filename,
		OriginalHash:   plan.OriginalHash,
		OriginalSize:   plan.OriginalSize,
		CompressedSize: uint64(len(compressed)),
		WasPadded:      wasPadded,
		ChunkCount:     plan.ChunkCount,
		ChunkSize:      plan.ChunkSize,
	}

	out := &EncodedChunk{Index: index, CompressedSize: uint64(len(compressed))}
	parts := [PartCount][]byte{part1, part2, part3}
	for i, partData := range parts {
		partNum := uint8(i + 1)

		nonce, err := crypto.GenerateNonce()
		if err != nil {
			return nil, fmt.Errorf("generating nonce for chunk %d part %d: %w", index, partNum, err)
		}

		header := &sandfile.Header{
			Version:    sandfile.ChunkedFormatVersion,
			PartNumber: partNum,
			ArchiveID:  plan.ArchiveID,
			ChunkIndex: index,
			Nonce:      nonce,
		}

		blob, err := sandfile.Seal(header, meta, partData, key)
		if err != nil {
			return nil, fmt.Errorf("writing chunk %d part %d: %w", index, partNum, err)
		}
		out.Parts[i] = blob
	}

	return out, nil
}

// DecodeChunk rebuilds one chunk from any two or three of its parts. The blobs
// may arrive in any order and carry their own part numbers.
func DecodeChunk(blobs [][]byte, master []byte) (*DecodedChunk, error) {
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
		if part.Header.Version != sandfile.ChunkedFormatVersion {
			return nil, fmt.Errorf("part %d is format version %d, not a chunk",
				i+1, part.Header.Version)
		}

		pn := int(part.Header.PartNumber)
		if _, exists := parsed[pn]; exists {
			return nil, fmt.Errorf("duplicate part number %d", pn)
		}
		parsed[pn] = part
	}

	// The archive ID and chunk index are the only things readable before
	// decryption, and together they are what says these parts belong to the
	// same chunk of the same file.
	var refHeader *sandfile.Header
	for _, p := range parsed {
		if refHeader == nil {
			refHeader = p.Header
			continue
		}
		if p.Header.ArchiveID != refHeader.ArchiveID {
			return nil, fmt.Errorf("archive ID mismatch: parts belong to different archives")
		}
		if p.Header.ChunkIndex != refHeader.ChunkIndex {
			return nil, fmt.Errorf("chunk index mismatch: parts belong to chunks %d and %d",
				refHeader.ChunkIndex, p.Header.ChunkIndex)
		}
	}

	key, err := crypto.DeriveChunkKey(master, refHeader.ArchiveID, refHeader.ChunkIndex)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(key)

	var refMeta *sandfile.Metadata
	decryptedParts := make(map[int][]byte, len(parsed))
	used := make([]int, 0, len(parsed))
	for pn, p := range parsed {
		meta, partData, err := p.Open(key)
		if err != nil {
			return nil, fmt.Errorf("decrypting chunk %d part %d: %w", refHeader.ChunkIndex, pn, err)
		}
		if refMeta == nil {
			refMeta = meta
		} else if meta.OriginalHash != refMeta.OriginalHash {
			return nil, fmt.Errorf("original hash mismatch: parts belong to different archives")
		}
		decryptedParts[pn] = partData
		used = append(used, pn)
	}

	compressed, err := splitter.Reconstruct(decryptedParts, refMeta.WasPadded)
	if err != nil {
		return nil, fmt.Errorf("reconstructing chunk %d: %w", refHeader.ChunkIndex, err)
	}

	data, err := compress.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompressing chunk %d: %w", refHeader.ChunkIndex, err)
	}

	// The whole-file SHA-256 in the metadata cannot be checked from one chunk,
	// so the length is what is verifiable here. The GCM tag over each part,
	// bound to this chunk index, is what actually guarantees the bytes.
	want, err := refMeta.ChunkPlaintextSize(refHeader.ChunkIndex)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != want {
		return nil, fmt.Errorf("chunk %d rebuilt to %d bytes, expected %d",
			refHeader.ChunkIndex, len(data), want)
	}

	return &DecodedChunk{
		Index:     refHeader.ChunkIndex,
		Data:      data,
		Meta:      refMeta,
		PartsUsed: used,
	}, nil
}
