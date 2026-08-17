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

	// Scheme is the erasure code every chunk of this archive is cut with. It is
	// fixed for the file, because a reader seeking into the middle of one must
	// not have to discover that its chunks disagree about how many shards they
	// have.
	Scheme Scheme
}

// PlanChunks works out how a file of the given size is cut up.
//
// A zero-length file is one empty chunk rather than none, so that every stored
// file has at least one object and an empty file round-trips like any other.
func PlanChunks(archiveID [16]byte, filename string, originalHash [32]byte, originalSize uint64, chunkSize uint32, scheme Scheme) (ChunkPlan, error) {
	if filename == "" {
		return ChunkPlan{}, fmt.Errorf("filename must not be empty")
	}
	if chunkSize == 0 {
		return ChunkPlan{}, fmt.Errorf("chunk size must not be zero")
	}
	if err := scheme.check(); err != nil {
		return ChunkPlan{}, err
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
		Scheme:       scheme,
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

// EncodedChunk is one chunk's shards, ready to scatter.
type EncodedChunk struct {
	Index          uint32
	CompressedSize uint64

	// Parts holds the serialized part files indexed by shard number − 1, the
	// same way Encoded does. There are Scheme.Total of them.
	Parts [][]byte

	// Scheme is the code they were cut with, repeated here so a caller that
	// planned the archive does not have to carry the plan alongside.
	Scheme Scheme
}

// schemeOf reads the code a parsed shard belongs to. Anything older than
// version 4 predates schemes and is two of three by construction.
func schemeOf(h *sandfile.Header) Scheme {
	if h.Version != sandfile.ErasureFormatVersion {
		return LegacyScheme()
	}
	return Scheme{Data: int(h.DataShards), Total: int(h.TotalShards)}
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

	if err := plan.Scheme.check(); err != nil {
		return nil, err
	}

	compressed, err := compress.Compress(plaintext)
	if err != nil {
		return nil, fmt.Errorf("compressing chunk %d: %w", index, err)
	}

	shards, err := splitter.Encode(compressed, plan.Scheme.Data, plan.Scheme.Total)
	if err != nil {
		return nil, fmt.Errorf("encoding chunk %d as %s: %w", index, plan.Scheme, err)
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
	// WasPadded is not written from version 4 on. The coder pads the compressed
	// bytes up to a multiple of k, and CompressedSize already records what the
	// true length was, so the flag has nothing left to say that the number does
	// not say exactly.
	meta := &sandfile.Metadata{
		Filename:       plan.Filename,
		OriginalHash:   plan.OriginalHash,
		OriginalSize:   plan.OriginalSize,
		CompressedSize: uint64(len(compressed)),
		ChunkCount:     plan.ChunkCount,
		ChunkSize:      plan.ChunkSize,
	}

	out := &EncodedChunk{
		Index:          index,
		CompressedSize: uint64(len(compressed)),
		Scheme:         plan.Scheme,
		Parts:          make([][]byte, plan.Scheme.Total),
	}
	for i, shard := range shards {
		shardNum := uint8(i + 1)

		nonce, err := crypto.GenerateNonce()
		if err != nil {
			return nil, fmt.Errorf("generating nonce for chunk %d shard %d: %w", index, shardNum, err)
		}

		header := &sandfile.Header{
			Version:     sandfile.ErasureFormatVersion,
			PartNumber:  shardNum,
			ArchiveID:   plan.ArchiveID,
			ChunkIndex:  index,
			DataShards:  uint8(plan.Scheme.Data),
			TotalShards: uint8(plan.Scheme.Total),
			Nonce:       nonce,
		}

		blob, err := sandfile.Seal(header, meta, shard, key)
		if err != nil {
			return nil, fmt.Errorf("writing chunk %d shard %d: %w", index, shardNum, err)
		}
		out.Parts[i] = blob
	}

	return out, nil
}

// DecodeChunk rebuilds one chunk from enough of its shards. The blobs may
// arrive in any order and carry their own shard numbers and, from format
// version 4, the code they belong to.
//
// It reads both layouts. A version 3 chunk is two of three under the XOR
// construction, which is what every file written before schemes existed is; a
// version 4 chunk says in its header how many shards it was cut into and how
// many rebuild it. Which one is in hand is read off the parts rather than
// passed in, because a vault holds both at once — widening does not rewrite
// what is already stored.
func DecodeChunk(blobs [][]byte, master []byte) (*DecodedChunk, error) {
	if len(blobs) == 0 {
		return nil, fmt.Errorf("no parts to rebuild a chunk from")
	}

	parsed := make(map[int]*sandfile.Part, len(blobs))
	for i, blob := range blobs {
		part, err := sandfile.ReadPart(blob)
		if err != nil {
			return nil, fmt.Errorf("parsing part %d: %w", i+1, err)
		}
		switch part.Header.Version {
		case sandfile.ChunkedFormatVersion, sandfile.ErasureFormatVersion:
		default:
			return nil, fmt.Errorf("part %d is format version %d, not a chunk",
				i+1, part.Header.Version)
		}

		pn := int(part.Header.PartNumber)
		if _, exists := parsed[pn]; exists {
			return nil, fmt.Errorf("duplicate shard number %d", pn)
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
		if p.Header.Version != refHeader.Version ||
			p.Header.DataShards != refHeader.DataShards ||
			p.Header.TotalShards != refHeader.TotalShards {
			return nil, fmt.Errorf("shards were cut with different codes: %s and %s",
				schemeOf(refHeader), schemeOf(p.Header))
		}
	}

	scheme := schemeOf(refHeader)
	if len(parsed) < scheme.Data {
		return nil, fmt.Errorf("rebuilding a %s chunk needs %d shards, got %d",
			scheme, scheme.Data, len(parsed))
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

	var compressed []byte
	if refHeader.Version == sandfile.ErasureFormatVersion {
		compressed, err = splitter.Reconstruct(
			decryptedParts, scheme.Data, scheme.Total, int(refMeta.CompressedSize))
	} else {
		compressed, err = splitter.ReconstructXOR(decryptedParts, refMeta.WasPadded)
	}
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
