package archive

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/sand-project/sand/internal/compress"
	"github.com/sand-project/sand/internal/crypto"
	"github.com/sand-project/sand/internal/mediafile"
	"github.com/sand-project/sand/internal/splitter"
)

// ArchiveMultiple archives each input file independently and returns the resulting
// media file paths grouped by part number. Index 0 holds all part-1 paths, index 1
// all part-2 paths, and index 2 all part-3 paths.
func ArchiveMultiple(inputPaths []string, password, outputDir string) ([3][]string, error) {
	var result [3][]string
	for _, inputPath := range inputPaths {
		paths, err := Archive(inputPath, password, outputDir)
		if err != nil {
			return result, fmt.Errorf("archiving %s: %w", filepath.Base(inputPath), err)
		}
		for i, p := range paths {
			result[i] = append(result[i], p)
		}
	}
	return result, nil
}

// Archive reads the input file, compresses, splits, XORs, encrypts, and writes
// three .media files to the output directory.
func Archive(inputPath, password, outputDir string) ([]string, error) {
	// 1. Read the input file
	originalData, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, fmt.Errorf("reading input file: %w", err)
	}

	// 2. Hash the original file
	hash := sha256.Sum256(originalData)
	originalSize := uint64(len(originalData))
	filename := filepath.Base(inputPath)

	// 3. Compress
	compressed, err := compress.Compress(originalData)
	if err != nil {
		return nil, fmt.Errorf("compressing: %w", err)
	}
	compressedSize := uint64(len(compressed))

	// 4. Split (pads if odd length)
	part1, part2, wasPadded := splitter.Split(compressed)

	// 5. XOR to produce part3
	part3, err := splitter.XOR(part1, part2)
	if err != nil {
		return nil, fmt.Errorf("generating XOR part: %w", err)
	}

	// 6. Derive encryption key
	argonParams := crypto.DefaultArgon2Params()
	salt, err := crypto.GenerateSalt(argonParams.SaltLen)
	if err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}

	key := crypto.DeriveKey(password, salt, argonParams)
	defer crypto.ZeroBytes(key)

	// 7. Generate archive ID
	archiveUUID := uuid.New()
	var archiveID [16]byte
	copy(archiveID[:], archiveUUID[:])

	// 8. Encrypt each part and write media files
	parts := [][]byte{part1, part2, part3}
	outputPaths := make([]string, 3)

	for i, partData := range parts {
		partNum := uint8(i + 1)

		nonce, err := crypto.GenerateNonce()
		if err != nil {
			return nil, fmt.Errorf("generating nonce for part %d: %w", partNum, err)
		}

		header := &mediafile.Header{
			Version:        mediafile.FormatVersion,
			PartNumber:     partNum,
			ArchiveID:      archiveID,
			OriginalHash:   hash,
			OriginalSize:   originalSize,
			CompressedSize: compressedSize,
			WasPadded:      wasPadded,
			Filename:       filename,
			Salt:           salt,
			Argon2Time:     argonParams.Time,
			Argon2Memory:   argonParams.Memory,
			Argon2Threads:  argonParams.Threads,
			Nonce:          nonce,
		}

		// Marshal header to use as GCM Associated Data
		headerBytes, err := mediafile.MarshalHeader(header)
		if err != nil {
			return nil, fmt.Errorf("marshaling header for part %d: %w", partNum, err)
		}

		// Encrypt with header as AD
		encryptedPayload, err := crypto.Encrypt(key, nonce, partData, headerBytes)
		if err != nil {
			return nil, fmt.Errorf("encrypting part %d: %w", partNum, err)
		}

		// Write complete media file
		fileData, err := mediafile.WriteMediaFile(header, encryptedPayload)
		if err != nil {
			return nil, fmt.Errorf("writing media file for part %d: %w", partNum, err)
		}

		outputPath := filepath.Join(outputDir, fmt.Sprintf("%s.media%d", filename, partNum))
		if err := os.WriteFile(outputPath, fileData, 0600); err != nil {
			return nil, fmt.Errorf("writing output file for part %d: %w", partNum, err)
		}

		outputPaths[i] = outputPath
	}

	return outputPaths, nil
}
