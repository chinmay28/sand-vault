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

// PartCount is the number of parts produced for every archived file.
const PartCount = 3

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

	parts := [PartCount][]byte{part1, part2, part3}
	for i, partData := range parts {
		partNum := uint8(i + 1)

		nonce, err := crypto.GenerateNonce()
		if err != nil {
			return nil, fmt.Errorf("generating nonce for part %d: %w", partNum, err)
		}

		header := &sandfile.Header{
			Version:        sandfile.FormatVersion,
			PartNumber:     partNum,
			ArchiveID:      enc.ArchiveID,
			OriginalHash:   enc.OriginalHash,
			OriginalSize:   enc.OriginalSize,
			CompressedSize: enc.CompressedSize,
			WasPadded:      wasPadded,
			Filename:       filename,
			Salt:           salt,
			Argon2Time:     argonParams.Time,
			Argon2Memory:   argonParams.Memory,
			Argon2Threads:  argonParams.Threads,
			Nonce:          nonce,
		}

		// The marshaled header doubles as GCM associated data, binding the
		// ciphertext to its own metadata and part number.
		headerBytes, err := sandfile.MarshalHeader(header)
		if err != nil {
			return nil, fmt.Errorf("marshaling header for part %d: %w", partNum, err)
		}

		encryptedPayload, err := crypto.Encrypt(key, nonce, partData, headerBytes)
		if err != nil {
			return nil, fmt.Errorf("encrypting part %d: %w", partNum, err)
		}

		blob, err := sandfile.WritePart(header, encryptedPayload)
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

	type parsedPart struct {
		header    *sandfile.Header
		headerAD  []byte
		encrypted []byte
	}

	parsed := make(map[int]*parsedPart, len(blobs))
	for i, blob := range blobs {
		header, headerBytes, encPayload, err := sandfile.ReadPart(blob)
		if err != nil {
			return nil, fmt.Errorf("parsing part %d: %w", i+1, err)
		}

		pn := int(header.PartNumber)
		if _, exists := parsed[pn]; exists {
			return nil, fmt.Errorf("duplicate part number %d", pn)
		}
		parsed[pn] = &parsedPart{header: header, headerAD: headerBytes, encrypted: encPayload}
	}

	// All parts must come from the same archive before we try to combine them.
	var refHeader *sandfile.Header
	for _, p := range parsed {
		if refHeader == nil {
			refHeader = p.header
			continue
		}
		if p.header.ArchiveID != refHeader.ArchiveID {
			return nil, fmt.Errorf("archive ID mismatch: parts belong to different archives")
		}
		if p.header.OriginalHash != refHeader.OriginalHash {
			return nil, fmt.Errorf("original hash mismatch: parts belong to different archives")
		}
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

	decryptedParts := make(map[int][]byte, len(parsed))
	used := make([]int, 0, len(parsed))
	for pn, p := range parsed {
		plaintext, err := crypto.Decrypt(key, p.header.Nonce, p.encrypted, p.headerAD)
		if err != nil {
			return nil, fmt.Errorf("decrypting part %d: %w", pn, err)
		}
		// gcm.Open returns nil (not []byte{}) for empty plaintext; normalize to empty slice
		// so that Reconstruct's nil checks correctly detect the part as present.
		if plaintext == nil {
			plaintext = []byte{}
		}
		decryptedParts[pn] = plaintext
		used = append(used, pn)
	}

	compressed, err := splitter.Reconstruct(decryptedParts, refHeader.WasPadded)
	if err != nil {
		return nil, fmt.Errorf("reconstructing data: %w", err)
	}

	decompressed, err := compress.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}

	if hash := sha256.Sum256(decompressed); hash != refHeader.OriginalHash {
		return nil, fmt.Errorf("integrity check failed: SHA-256 hash mismatch")
	}
	if uint64(len(decompressed)) != refHeader.OriginalSize {
		return nil, fmt.Errorf("size mismatch: expected %d, got %d",
			refHeader.OriginalSize, len(decompressed))
	}

	sortInts(used)
	return &Decoded{
		Data:      decompressed,
		Filename:  refHeader.Filename,
		ArchiveID: refHeader.ArchiveID,
		Size:      refHeader.OriginalSize,
		PartsUsed: used,
	}, nil
}

// PeekFilename returns the original filename recorded in a part blob's
// header without decrypting the payload.
func PeekFilename(blob []byte) (string, error) {
	header, _, err := sandfile.UnmarshalHeader(blob)
	if err != nil {
		return "", err
	}
	return header.Filename, nil
}

// sortInts is a tiny insertion sort; PartsUsed never holds more than 3 items.
func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}
