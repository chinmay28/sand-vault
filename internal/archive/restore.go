package archive

import (
	"fmt"
	"os"
	"path/filepath"
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
