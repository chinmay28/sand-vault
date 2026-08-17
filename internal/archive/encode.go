package archive

import (
	"crypto/sha256"
	"fmt"

	"github.com/chinmay28/sand-vault/internal/compress"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/sandfile"
	"github.com/chinmay28/sand-vault/internal/splitter"
	"github.com/google/uuid"
)

// PartCount is the number of parts the whole-file format produces, and the
// number of shards the default scheme has. A file cut with a wider scheme has
// more; see Scheme.
const PartCount = 3

// FormatVersionWhole is the whole-file layout this package writes, named here so
// the decoder can say which versions it is able to open.
const FormatVersionWhole = sandfile.FormatVersion

// MinPartsToRestore is how many of the PartCount parts are required to
// reconstruct the original data.
const MinPartsToRestore = 2

// Encoded is the in-memory result of encoding a single file: three
// self-describing part blobs plus the metadata shared by all of them.
type Encoded struct {
	ArchiveID      [16]byte
	Filename       string
	OriginalHash   [32]byte
	OriginalSize   uint64
	CompressedSize uint64

	// Parts holds the serialized part files indexed by part number - 1,
	// so Parts[0] is part 1, Parts[1] is part 2 and Parts[2] is part 3.
	Parts [PartCount][]byte
}

// Decoded is the in-memory result of reconstructing a file from its parts.
type Decoded struct {
	Data      []byte
	Filename  string
	ArchiveID [16]byte
	Size      uint64
	PartsUsed []int
}

// EncodeBytes runs the full archive pipeline in memory: compress, split, XOR
// for redundancy, then encrypt each part under a key derived from password.
// Every call generates a fresh archive ID, salt and per-part nonces.
func EncodeBytes(data []byte, filename, password string) (*Encoded, error) {
	if filename == "" {
		return nil, fmt.Errorf("filename must not be empty")
	}

	enc := &Encoded{
		Filename:     filename,
		OriginalHash: sha256.Sum256(data),
		OriginalSize: uint64(len(data)),
	}

	compressed, err := compress.Compress(data)
	if err != nil {
		return nil, fmt.Errorf("compressing: %w", err)
	}
	enc.CompressedSize = uint64(len(compressed))

	part1, part2, wasPadded := splitter.Split(compressed)
	part3, err := splitter.XOR(part1, part2)
	if err != nil {
		return nil, fmt.Errorf("generating XOR part: %w", err)
	}

	argonParams := crypto.DefaultArgon2Params()
	salt, err := crypto.GenerateSalt(argonParams.SaltLen)
	if err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}

	key := crypto.DeriveKey(password, salt, argonParams)
	defer crypto.ZeroBytes(key)

	archiveUUID := uuid.New()
	copy(enc.ArchiveID[:], archiveUUID[:])

	// Every part of a file carries the same metadata block, sealed alongside
	// its own share of the data. Nothing here is written in the clear.
	meta := &sandfile.Metadata{
		Filename:       filename,
		OriginalHash:   enc.OriginalHash,
		OriginalSize:   enc.OriginalSize,
		CompressedSize: enc.CompressedSize,
		WasPadded:      wasPadded,
	}

	parts := [PartCount][]byte{part1, part2, part3}
	for i, partData := range parts {
		partNum := uint8(i + 1)

		nonce, err := crypto.GenerateNonce()
		if err != nil {
			return nil, fmt.Errorf("generating nonce for part %d: %w", partNum, err)
		}

		header := &sandfile.Header{
			Version:       sandfile.FormatVersion,
			PartNumber:    partNum,
			ArchiveID:     enc.ArchiveID,
			Salt:          salt,
			Argon2Time:    argonParams.Time,
			Argon2Memory:  argonParams.Memory,
			Argon2Threads: argonParams.Threads,
			Nonce:         nonce,
		}

		blob, err := sandfile.Seal(header, meta, partData, key)
		if err != nil {
			return nil, fmt.Errorf("writing part file for part %d: %w", partNum, err)
		}

		enc.Parts[i] = blob
	}

	return enc, nil
}

// DecodeBytes reconstructs the original data from any 2 or 3 part blobs.
// The blobs may arrive in any order and carry their own part numbers.
func DecodeBytes(blobs [][]byte, password string) (*Decoded, error) {
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

		// A chunked part derives its key from the vault's data key rather than a
		// password, and carries no Argon2 parameters to derive one with. Turning
		// it away here is what stops those zeroed parameters reaching the KDF,
		// which treats them as a programming error and panics.
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

	// The archive ID is the only thing readable before decryption, so it is
	// what tells us up front that these parts belong together.
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

	// Only the password-derived formats can be opened here. A chunked part
	// carries no salt and no Argon2 parameters — its key comes from a vault's
	// data key — so without this the stretch below would be handed a zero-length
	// salt and panic rather than say what was wrong.
	if refHeader.Version != FormatVersionWhole && refHeader.Version != sandfile.LegacyFormatVersion {
		return nil, fmt.Errorf(
			"these are format version %d parts, written by a vault — they cannot be opened by a "+
				"password alone, and need the manifest backup that carries the vault's data key",
			refHeader.Version)
	}

	argonParams := crypto.Argon2Params{
		Time:    refHeader.Argon2Time,
		Memory:  refHeader.Argon2Memory,
		Threads: refHeader.Argon2Threads,
		SaltLen: uint32(len(refHeader.Salt)),
		KeyLen:  32,
	}

	key := crypto.DeriveKey(password, refHeader.Salt, argonParams)
	defer crypto.ZeroBytes(key)

	var refMeta *sandfile.Metadata
	decryptedParts := make(map[int][]byte, len(parsed))
	used := make([]int, 0, len(parsed))
	for pn, p := range parsed {
		meta, partData, err := p.Open(key)
		if err != nil {
			return nil, fmt.Errorf("decrypting part %d: %w", pn, err)
		}
		// Each part carries its own copy of the file's metadata; they have to
		// agree, or these are parts of two different files that happen to share
		// an archive ID.
		if refMeta == nil {
			refMeta = meta
		} else if meta.OriginalHash != refMeta.OriginalHash {
			return nil, fmt.Errorf("original hash mismatch: parts belong to different archives")
		}
		decryptedParts[pn] = partData
		used = append(used, pn)
	}

	compressed, err := splitter.ReconstructXOR(decryptedParts, refMeta.WasPadded)
	if err != nil {
		return nil, fmt.Errorf("reconstructing data: %w", err)
	}

	decompressed, err := compress.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}

	if hash := sha256.Sum256(decompressed); hash != refMeta.OriginalHash {
		return nil, fmt.Errorf("integrity check failed: SHA-256 hash mismatch")
	}
	if uint64(len(decompressed)) != refMeta.OriginalSize {
		return nil, fmt.Errorf("size mismatch: expected %d, got %d",
			refMeta.OriginalSize, len(decompressed))
	}

	sortInts(used)
	return &Decoded{
		Data:      decompressed,
		Filename:  refMeta.Filename,
		ArchiveID: refHeader.ArchiveID,
		Size:      refMeta.OriginalSize,
		PartsUsed: used,
	}, nil
}

// ArchiveIDOf reports the archive ID a part belongs to. It is the one thing a
// part says about itself without a key, and it is what lets a caller group
// loose parts by file, or look one up in a manifest.
func ArchiveIDOf(blob []byte) ([16]byte, error) {
	part, err := sandfile.ReadPart(blob)
	if err != nil {
		return [16]byte{}, err
	}
	return part.Header.ArchiveID, nil
}

// sortInts is a tiny insertion sort; PartsUsed never holds more than 3 items.
func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}
