package archive

import (
	"bytes"
	"fmt"

	"github.com/chinmay28/SandHarbor/internal/compress"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/sandfile"
	"github.com/chinmay28/SandHarbor/internal/splitter"
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
	if err := scheme.Storable(); err != nil {
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

	// Compressed is the chunk as the shards carried it, before decompression —
	// what ShardData recomputes every shard from.
	Compressed []byte
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

	if err := plan.Scheme.Storable(); err != nil {
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

// OpenedShard is one shard of one chunk with its seal checked and removed:
// what its cleartext header says it is, the archive description sealed inside
// it, and its share of the compressed chunk.
//
// Opening is the step that can say a shard is bad on its own evidence — the
// blob does not parse, or its tag does not verify under the key its own header
// names — and it is separate from rebuilding so that a reader holding one bad
// shard among good ones can set it aside and rebuild from the rest. See
// OpenShard and AssembleChunk.
type OpenedShard struct {
	Header *sandfile.Header
	Meta   *sandfile.Metadata
	Data   []byte
}

// Scheme is the code this shard was cut with.
func (s *OpenedShard) Scheme() Scheme { return schemeOf(s.Header) }

// Legacy reports a shard from before schemes existed: two halves and their XOR.
func (s *OpenedShard) Legacy() bool { return s.Header.Version != sandfile.ErasureFormatVersion }

// OpenShard parses one stored chunk shard and opens its seal.
//
// The key is derived from the archive ID and chunk index in the shard's own
// header, and that header is the seal's associated data, so a shard that opens
// is exactly what was written for that part of that chunk of that archive: a
// truncated object, a flipped bit, another file's shard under this key's name
// and a shard relabelled as a different part all fail here. Whether it is the
// chunk the caller *wanted* is for the caller to read off Header.
func OpenShard(blob, master []byte) (*OpenedShard, error) {
	part, err := sandfile.ReadPart(blob)
	if err != nil {
		return nil, err
	}
	switch part.Header.Version {
	case sandfile.ChunkedFormatVersion, sandfile.ErasureFormatVersion:
	default:
		return nil, fmt.Errorf("format version %d is not a chunk", part.Header.Version)
	}

	key, err := crypto.DeriveChunkKey(master, part.Header.ArchiveID, part.Header.ChunkIndex)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(key)

	meta, data, err := part.Open(key)
	if err != nil {
		return nil, err
	}
	return &OpenedShard{Header: part.Header, Meta: meta, Data: data}, nil
}

// AssembleChunk rebuilds one chunk from shards that have already been opened.
//
// It checks that they belong together — one archive, one chunk, one code, one
// file hash, no shard number twice — rebuilds the compressed chunk from any
// Scheme.Data of them, decompresses it and holds the result to the length the
// archive's chunking says this chunk has.
func AssembleChunk(shards []*OpenedShard) (*DecodedChunk, error) {
	if len(shards) == 0 {
		return nil, fmt.Errorf("no parts to rebuild a chunk from")
	}

	// The archive ID and chunk index are what says these parts belong to the
	// same chunk of the same file.
	ref := shards[0]
	byNumber := make(map[int][]byte, len(shards))
	used := make([]int, 0, len(shards))
	for _, s := range shards {
		if s.Header.ArchiveID != ref.Header.ArchiveID {
			return nil, fmt.Errorf("archive ID mismatch: parts belong to different archives")
		}
		if s.Header.ChunkIndex != ref.Header.ChunkIndex {
			return nil, fmt.Errorf("chunk index mismatch: parts belong to chunks %d and %d",
				ref.Header.ChunkIndex, s.Header.ChunkIndex)
		}
		if s.Header.Version != ref.Header.Version ||
			s.Header.DataShards != ref.Header.DataShards ||
			s.Header.TotalShards != ref.Header.TotalShards {
			return nil, fmt.Errorf("shards were cut with different codes: %s and %s",
				ref.Scheme(), s.Scheme())
		}
		if s.Meta.OriginalHash != ref.Meta.OriginalHash {
			return nil, fmt.Errorf("original hash mismatch: parts belong to different archives")
		}
		pn := int(s.Header.PartNumber)
		if _, exists := byNumber[pn]; exists {
			return nil, fmt.Errorf("duplicate shard number %d", pn)
		}
		byNumber[pn] = s.Data
		used = append(used, pn)
	}

	scheme := ref.Scheme()
	if len(byNumber) < scheme.Data {
		return nil, fmt.Errorf("rebuilding a %s chunk needs %d shards, got %d",
			scheme, scheme.Data, len(byNumber))
	}

	var compressed []byte
	var err error
	if ref.Header.Version == sandfile.ErasureFormatVersion {
		compressed, err = splitter.Reconstruct(
			byNumber, scheme.Data, scheme.Total, int(ref.Meta.CompressedSize))
	} else {
		compressed, err = splitter.ReconstructXOR(byNumber, ref.Meta.WasPadded)
	}
	if err != nil {
		return nil, fmt.Errorf("reconstructing chunk %d: %w", ref.Header.ChunkIndex, err)
	}

	data, err := compress.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompressing chunk %d: %w", ref.Header.ChunkIndex, err)
	}

	// The whole-file SHA-256 in the metadata cannot be checked from one chunk,
	// so the length is what is verifiable here. The GCM tag over each part,
	// bound to this chunk index, is what actually guarantees the bytes.
	want, err := ref.Meta.ChunkPlaintextSize(ref.Header.ChunkIndex)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != want {
		return nil, fmt.Errorf("chunk %d rebuilt to %d bytes, expected %d",
			ref.Header.ChunkIndex, len(data), want)
	}

	return &DecodedChunk{
		Index:      ref.Header.ChunkIndex,
		Data:       data,
		Meta:       ref.Meta,
		PartsUsed:  used,
		Compressed: compressed,
	}, nil
}

// ShardData recomputes what every shard of a chunk should hold, from the
// compressed chunk a rebuild produced: index i is shard i+1's share.
//
// A rebuild reads only Scheme.Data of the shards and trusts the rest to agree.
// This is how a check finds out whether they do — a shard that opens under its
// seal but does not match is one that was written from different bytes.
// Version 3 chunks, which predate schemes, are two halves and their XOR.
func ShardData(compressed []byte, scheme Scheme, legacy bool) ([][]byte, error) {
	if legacy {
		p1, p2, _ := splitter.Split(compressed)
		p3, err := splitter.XOR(p1, p2)
		if err != nil {
			return nil, err
		}
		return [][]byte{p1, p2, p3}, nil
	}
	return splitter.Encode(compressed, scheme.Data, scheme.Total)
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
//
// Every blob has to open: this is the strict form, for a caller that has
// exactly the shards it means to use. A reader racing accounts for them opens
// each as it lands and assembles what opened — see OpenShard.
func DecodeChunk(blobs [][]byte, master []byte) (*DecodedChunk, error) {
	if len(blobs) == 0 {
		return nil, fmt.Errorf("no parts to rebuild a chunk from")
	}
	shards := make([]*OpenedShard, 0, len(blobs))
	for i, blob := range blobs {
		shard, err := OpenShard(blob, master)
		if err != nil {
			return nil, fmt.Errorf("opening part %d: %w", i+1, err)
		}
		shards = append(shards, shard)
	}
	return AssembleChunk(shards)
}

// RemakeShard cuts one shard of a chunk again from enough of the others, and
// seals it as the part it replaces.
//
// A code that rebuilds a chunk from any Scheme.Data of its shards can rebuild
// any one shard the same way: the chunk is put back together, every shard is
// recomputed from it (ShardData), and the one asked for is sealed under the
// chunk's own key with a nonce of its own. The result is not byte-for-byte the
// object that was lost — the nonce differs — but it opens as the same part of
// the same chunk of the same archive, carrying the same share, and so stands
// in for it anywhere the original would have served.
//
// The shards must already be opened, which is what authenticates them, and
// must be Scheme.Data or more of one chunk; part is the shard number to make,
// 1..Scheme.Total. It may be one of the shards handed in.
//
// Nothing is returned on trust. Before it is handed back the new shard is
// opened again and the chunk rebuilt with it standing in for one of the
// shards it was made from; a result that does not give back the same bytes is
// an error rather than an object.
func RemakeShard(shards []*OpenedShard, part int, master []byte) ([]byte, error) {
	chunk, err := AssembleChunk(shards)
	if err != nil {
		return nil, err
	}
	ref := shards[0]
	scheme := ref.Scheme()
	if part < 1 || part > scheme.Total {
		return nil, fmt.Errorf("shard %d is not one a %s chunk has", part, scheme)
	}

	all, err := ShardData(chunk.Compressed, scheme, ref.Legacy())
	if err != nil {
		return nil, fmt.Errorf("recomputing the shards of chunk %d: %w", chunk.Index, err)
	}
	if len(all) != scheme.Total {
		return nil, fmt.Errorf("chunk %d recomputed to %d shards, expected %d", chunk.Index, len(all), scheme.Total)
	}

	nonce, err := crypto.GenerateNonce()
	if err != nil {
		return nil, fmt.Errorf("generating nonce for chunk %d shard %d: %w", chunk.Index, part, err)
	}
	header := *ref.Header
	header.PartNumber = uint8(part)
	header.Nonce = nonce

	key, err := crypto.DeriveChunkKey(master, header.ArchiveID, header.ChunkIndex)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(key)

	blob, err := sandfile.Seal(&header, ref.Meta, all[part-1], key)
	if err != nil {
		return nil, fmt.Errorf("writing chunk %d shard %d: %w", chunk.Index, part, err)
	}

	// The proof: the new shard and Scheme.Data−1 of the others, none of them
	// the part it replaces, must give the chunk back.
	remade, err := OpenShard(blob, master)
	if err != nil {
		return nil, fmt.Errorf("the remade shard %d of chunk %d does not open: %w", part, chunk.Index, err)
	}
	proof := []*OpenedShard{remade}
	seen := map[int]bool{part: true}
	for _, s := range shards {
		if len(proof) == scheme.Data {
			break
		}
		if n := int(s.Header.PartNumber); !seen[n] {
			seen[n] = true
			proof = append(proof, s)
		}
	}
	if len(proof) == scheme.Data {
		again, err := AssembleChunk(proof)
		if err != nil {
			return nil, fmt.Errorf("the remade shard %d of chunk %d does not rebuild it: %w", part, chunk.Index, err)
		}
		if !bytes.Equal(again.Compressed, chunk.Compressed) {
			return nil, fmt.Errorf("the remade shard %d of chunk %d rebuilds different bytes", part, chunk.Index)
		}
	}
	return blob, nil
}
