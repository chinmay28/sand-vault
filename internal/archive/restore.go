package archive

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sand-project/sand/internal/compress"
	"github.com/sand-project/sand/internal/crypto"
	"github.com/sand-project/sand/internal/mediafile"
	"github.com/sand-project/sand/internal/splitter"
)

// Restore reads 2 or 3 media files, decrypts, reconstructs, decompresses,
// verifies integrity, and writes the original file to the output directory.
func Restore(mediaPaths []string, password, outputDir string) (string, error) {
	if len(mediaPaths) < 2 || len(mediaPaths) > 3 {
		return "", fmt.Errorf("need 2 or 3 media files, got %d", len(mediaPaths))
	}

	// 1. Read and parse all provided media files
	type parsedPart struct {
		header    *mediafile.Header
		headerAD  []byte
		encrypted []byte
	}

	parsed := make(map[int]*parsedPart)

	for _, path := range mediaPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}

		header, headerBytes, encPayload, err := mediafile.ReadMediaFile(data)
		if err != nil {
			return "", fmt.Errorf("parsing %s: %w", path, err)
		}

		pn := int(header.PartNumber)
		if _, exists := parsed[pn]; exists {
			return "", fmt.Errorf("duplicate part number %d", pn)
		}

		parsed[pn] = &parsedPart{
			header:    header,
			headerAD:  headerBytes,
			encrypted: encPayload,
		}
	}

	// 2. Validate all parts belong to the same archive
	var refHeader *mediafile.Header
	for _, p := range parsed {
		if refHeader == nil {
			refHeader = p.header
			continue
		}
		if p.header.ArchiveID != refHeader.ArchiveID {
			return "", fmt.Errorf("archive ID mismatch: parts belong to different archives")
		}
		if p.header.OriginalHash != refHeader.OriginalHash {
			return "", fmt.Errorf("original hash mismatch: parts belong to different archives")
		}
	}

	// 3. Derive key (all parts share the same salt and argon2 params)
	argonParams := crypto.Argon2Params{
		Time:    refHeader.Argon2Time,
		Memory:  refHeader.Argon2Memory,
		Threads: refHeader.Argon2Threads,
		SaltLen: uint32(len(refHeader.Salt)),
		KeyLen:  32,
	}

	key := crypto.DeriveKey(password, refHeader.Salt, argonParams)
	defer crypto.ZeroBytes(key)

	// 4. Decrypt all provided parts
	decryptedParts := make(map[int][]byte)
	for pn, p := range parsed {
		plaintext, err := crypto.Decrypt(key, p.header.Nonce, p.encrypted, p.headerAD)
		if err != nil {
			return "", fmt.Errorf("decrypting part %d: %w", pn, err)
		}
		// gcm.Open returns nil (not []byte{}) for empty plaintext; normalize to empty slice
		// so that Reconstruct's nil checks correctly detect the part as present.
		if plaintext == nil {
			plaintext = []byte{}
		}
		decryptedParts[pn] = plaintext
	}

	// 5. Reconstruct compressed data
	compressed, err := splitter.Reconstruct(decryptedParts, refHeader.WasPadded)
	if err != nil {
		return "", fmt.Errorf("reconstructing data: %w", err)
	}

	// 6. Decompress
	decompressed, err := compress.Decompress(compressed)
	if err != nil {
		return "", fmt.Errorf("decompressing: %w", err)
	}

	// 7. Verify SHA-256 hash
	hash := sha256.Sum256(decompressed)
	if hash != refHeader.OriginalHash {
		return "", fmt.Errorf("integrity check failed: SHA-256 hash mismatch")
	}

	// 8. Verify size
	if uint64(len(decompressed)) != refHeader.OriginalSize {
		return "", fmt.Errorf("size mismatch: expected %d, got %d", refHeader.OriginalSize, len(decompressed))
	}

	// 9. Write output file
	outputPath := filepath.Join(outputDir, refHeader.Filename)
	if err := os.WriteFile(outputPath, decompressed, 0644); err != nil {
		return "", fmt.Errorf("writing output: %w", err)
	}

	return outputPath, nil
}
