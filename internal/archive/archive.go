package archive

import (
	"fmt"
	"os"
	"path/filepath"
)

// ArchiveMultiple archives each input file independently and returns the resulting
// media file paths grouped by part number. Index 0 holds all part-1 paths, index 1
// all part-2 paths, and index 2 all part-3 paths.
func ArchiveMultiple(inputPaths []string, password, outputDir string) ([PartCount][]string, error) {
	var result [PartCount][]string
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
	originalData, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, fmt.Errorf("reading input file: %w", err)
	}

	filename := filepath.Base(inputPath)
	encoded, err := EncodeBytes(originalData, filename, password)
	if err != nil {
		return nil, err
	}

	outputPaths := make([]string, PartCount)
	for i, blob := range encoded.Parts {
		outputPath := filepath.Join(outputDir, fmt.Sprintf("%s.media%d", filename, i+1))
		if err := os.WriteFile(outputPath, blob, 0600); err != nil {
			return nil, fmt.Errorf("writing output file for part %d: %w", i+1, err)
		}
		outputPaths[i] = outputPath
	}

	return outputPaths, nil
}
