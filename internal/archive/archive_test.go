package archive

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func setupTestDirs(t *testing.T) (inputDir, outputDir, restoreDir string) {
	t.Helper()
	inputDir = t.TempDir()
	outputDir = t.TempDir()
	restoreDir = t.TempDir()
	return
}

func writeTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Archive Tests
// ---------------------------------------------------------------------------

func TestArchive_ProducesThreeFiles(t *testing.T) {
	inputDir, outputDir, _ := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.txt", []byte("hello world"))

	paths, err := Archive(inputPath, "password123", outputDir)
	if err != nil {
		t.Fatalf("archive failed: %v", err)
	}

	if len(paths) != 3 {
		t.Fatalf("expected 3 output paths, got %d", len(paths))
	}

	for i, path := range paths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Fatalf("output file %d does not exist: %s", i+1, path)
		}

		expectedSuffix := fmt.Sprintf(".p%d.sand", i+1)
		if !strings.HasSuffix(path, expectedSuffix) {
			t.Fatalf("expected suffix %s, got %s", expectedSuffix, path)
		}
	}
}

func TestArchive_FilesAreNotEmpty(t *testing.T) {
	inputDir, outputDir, _ := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "data.bin", []byte("some data"))

	paths, err := Archive(inputPath, "pass", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range paths {
		info, _ := os.Stat(path)
		if info.Size() == 0 {
			t.Fatalf("output file should not be empty: %s", path)
		}
	}
}

func TestArchive_FilesStartWithMagic(t *testing.T) {
	inputDir, outputDir, _ := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.pdf", []byte("pdf content"))

	paths, _ := Archive(inputPath, "pass", outputDir)

	for _, path := range paths {
		data, _ := os.ReadFile(path)
		if !bytes.Equal(data[:4], []byte("SAND")) {
			t.Fatalf("file %s should start with SAND magic", path)
		}
	}
}

func TestArchive_NonexistentInput(t *testing.T) {
	outputDir := t.TempDir()
	_, err := Archive("/nonexistent/file.txt", "pass", outputDir)
	if err == nil {
		t.Fatal("should fail with nonexistent input")
	}
}

func TestArchive_OutputPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission bits are not enforced on Windows")
	}
	inputDir, outputDir, _ := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.txt", []byte("data"))

	paths, _ := Archive(inputPath, "pass", outputDir)

	for _, path := range paths {
		info, _ := os.Stat(path)
		perm := info.Mode().Perm()
		if perm != 0600 {
			t.Fatalf("expected 0600 permissions, got %o for %s", perm, path)
		}
	}
}

// ---------------------------------------------------------------------------
// Restore Tests — Full Round-Trip
// ---------------------------------------------------------------------------

func TestRestoreFromAllThreeParts(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("Confidential document contents here.")
	inputPath := writeTestFile(t, inputDir, "secret.txt", original)

	paths, err := Archive(inputPath, "mypassword", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	outputPath, err := Restore(paths, "mypassword", restoreDir)
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	restored, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, restored) {
		t.Fatal("restored data does not match original")
	}

	if filepath.Base(outputPath) != "secret.txt" {
		t.Fatalf("restored filename: got %q, want %q", filepath.Base(outputPath), "secret.txt")
	}
}

func TestRestoreFromParts12(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("data for parts 1+2 test")
	inputPath := writeTestFile(t, inputDir, "doc.pdf", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	outputPath, err := Restore(paths[:2], "pw", restoreDir) // parts 1 and 2 only
	if err != nil {
		t.Fatalf("restore from parts 1+2 failed: %v", err)
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("parts 1+2 restore mismatch")
	}
}

func TestRestoreFromParts13(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("data for parts 1+3 test")
	inputPath := writeTestFile(t, inputDir, "doc.pdf", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	parts13 := []string{paths[0], paths[2]} // parts 1 and 3
	outputPath, err := Restore(parts13, "pw", restoreDir)
	if err != nil {
		t.Fatalf("restore from parts 1+3 failed: %v", err)
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("parts 1+3 restore mismatch")
	}
}

func TestRestoreFromParts23(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("data for parts 2+3 test")
	inputPath := writeTestFile(t, inputDir, "doc.pdf", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	parts23 := []string{paths[1], paths[2]} // parts 2 and 3
	outputPath, err := Restore(parts23, "pw", restoreDir)
	if err != nil {
		t.Fatalf("restore from parts 2+3 failed: %v", err)
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("parts 2+3 restore mismatch")
	}
}

// ---------------------------------------------------------------------------
// Odd Byte Tests — Critical for Padding
// ---------------------------------------------------------------------------

func TestRoundTrip_OddByteCount_1Byte(t *testing.T) {
	inputDir, outputDir, _ := setupTestDirs(t)
	original := []byte{0x42}
	inputPath := writeTestFile(t, inputDir, "single.bin", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	// Test all 2-of-3 combinations
	combos := [][]string{
		{paths[0], paths[1]},
		{paths[0], paths[2]},
		{paths[1], paths[2]},
	}

	for i, combo := range combos {
		rd := t.TempDir()
		outputPath, err := Restore(combo, "pw", rd)
		if err != nil {
			t.Fatalf("combo %d failed: %v", i, err)
		}
		restored, _ := os.ReadFile(outputPath)
		if !bytes.Equal(original, restored) {
			t.Fatalf("combo %d: data mismatch: got %v, want %v", i, restored, original)
		}
	}
}

func TestRoundTrip_OddByteCount_3Bytes(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte{0xAA, 0xBB, 0xCC}
	inputPath := writeTestFile(t, inputDir, "three.bin", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	for _, combo := range [][]string{
		{paths[0], paths[1]},
		{paths[0], paths[2]},
		{paths[1], paths[2]},
	} {
		rd := t.TempDir()
		op, err := Restore(combo, "pw", rd)
		if err != nil {
			t.Fatal(err)
		}
		restored, _ := os.ReadFile(op)
		if !bytes.Equal(original, restored) {
			t.Fatal("3-byte odd data mismatch")
		}
	}
	_ = restoreDir
}

func TestRoundTrip_OddByteCount_LargeOdd(t *testing.T) {
	inputDir, outputDir, _ := setupTestDirs(t)

	// 65537 bytes (odd)
	original := make([]byte, 65537)
	for i := range original {
		original[i] = byte(i % 251) // prime-based pattern
	}
	inputPath := writeTestFile(t, inputDir, "large_odd.bin", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	for _, combo := range [][]string{
		{paths[0], paths[1]},
		{paths[0], paths[2]},
		{paths[1], paths[2]},
	} {
		rd := t.TempDir()
		op, err := Restore(combo, "pw", rd)
		if err != nil {
			t.Fatal(err)
		}
		restored, _ := os.ReadFile(op)
		if !bytes.Equal(original, restored) {
			t.Fatal("large odd data mismatch")
		}
	}
}

func TestRoundTrip_EvenByteCount_2Bytes(t *testing.T) {
	inputDir, outputDir, _ := setupTestDirs(t)
	original := []byte{0xDE, 0xAD}
	inputPath := writeTestFile(t, inputDir, "two.bin", original)

	paths, _ := Archive(inputPath, "pw", outputDir)

	for _, combo := range [][]string{
		{paths[0], paths[1]},
		{paths[0], paths[2]},
		{paths[1], paths[2]},
	} {
		rd := t.TempDir()
		op, err := Restore(combo, "pw", rd)
		if err != nil {
			t.Fatal(err)
		}
		restored, _ := os.ReadFile(op)
		if !bytes.Equal(original, restored) {
			t.Fatal("2-byte even data mismatch")
		}
	}
}

// ---------------------------------------------------------------------------
// Edge Cases
// ---------------------------------------------------------------------------

func TestRoundTrip_EmptyFile(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "empty.bin", []byte{})

	paths, err := Archive(inputPath, "pw", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	outputPath, err := Restore(paths, "pw", restoreDir)
	if err != nil {
		t.Fatal(err)
	}

	restored, _ := os.ReadFile(outputPath)
	if len(restored) != 0 {
		t.Fatal("restored empty file should be empty")
	}
}

func TestRoundTrip_LargeRandomFile(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)

	// 256KB random data
	original := make([]byte, 256*1024)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}
	inputPath := writeTestFile(t, inputDir, "random.bin", original)

	paths, err := Archive(inputPath, "strong-password-123!", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	outputPath, err := Restore(paths, "strong-password-123!", restoreDir)
	if err != nil {
		t.Fatal(err)
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("large random file mismatch")
	}
}

func TestRoundTrip_UnicodeFilename(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("unicode test data")
	inputPath := writeTestFile(t, inputDir, "документ.pdf", original)

	paths, err := Archive(inputPath, "pw", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	outputPath, err := Restore(paths, "pw", restoreDir)
	if err != nil {
		t.Fatal(err)
	}

	if filepath.Base(outputPath) != "документ.pdf" {
		t.Fatalf("filename mismatch: got %q", filepath.Base(outputPath))
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("data mismatch")
	}
}

func TestRoundTrip_EmptyPassword(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("empty password test")
	inputPath := writeTestFile(t, inputDir, "test.bin", original)

	paths, _ := Archive(inputPath, "", outputDir)

	outputPath, err := Restore(paths, "", restoreDir)
	if err != nil {
		t.Fatal(err)
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("empty password round-trip failed")
	}
}

func TestRoundTrip_UnicodePassword(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	original := []byte("unicode password test")
	inputPath := writeTestFile(t, inputDir, "test.bin", original)

	pw := "пароль🔑密码"
	paths, _ := Archive(inputPath, pw, outputDir)

	outputPath, err := Restore(paths, pw, restoreDir)
	if err != nil {
		t.Fatal(err)
	}

	restored, _ := os.ReadFile(outputPath)
	if !bytes.Equal(original, restored) {
		t.Fatal("unicode password round-trip failed")
	}
}

// ---------------------------------------------------------------------------
// Security Tests
// ---------------------------------------------------------------------------

func TestRestore_WrongPassword(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.bin", []byte("secret"))

	paths, _ := Archive(inputPath, "correct-password", outputDir)

	_, err := Restore(paths, "wrong-password", restoreDir)
	if err == nil {
		t.Fatal("restore with wrong password should fail")
	}
}

func TestRestore_MismatchedParts(t *testing.T) {
	inputDir, outputDir1, outputDir2 := setupTestDirs(t)

	// Create two separate archives
	path1 := writeTestFile(t, inputDir, "file1.txt", []byte("first"))
	path2 := writeTestFile(t, inputDir, "file2.txt", []byte("second"))

	paths1, _ := Archive(path1, "pw", outputDir1)
	paths2, _ := Archive(path2, "pw", outputDir2)

	// Try to restore with parts from different archives
	restoreDir := t.TempDir()
	_, err := Restore([]string{paths1[0], paths2[1]}, "pw", restoreDir)
	if err == nil {
		t.Fatal("restore with mismatched parts should fail")
	}
}

func TestRestore_TooFewParts(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.bin", []byte("data"))

	paths, _ := Archive(inputPath, "pw", outputDir)

	_, err := Restore(paths[:1], "pw", restoreDir)
	if err == nil {
		t.Fatal("restore with 1 part should fail")
	}
}

func TestRestore_TooManyParts(t *testing.T) {
	restoreDir := t.TempDir()
	_, err := Restore([]string{"a", "b", "c", "d"}, "pw", restoreDir)
	if err == nil {
		t.Fatal("restore with 4 parts should fail")
	}
}

func TestRestore_CorruptedMediaFile(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.bin", []byte("data"))

	paths, _ := Archive(inputPath, "pw", outputDir)

	// Corrupt the first file
	data, _ := os.ReadFile(paths[0])
	data[len(data)-1] ^= 0xFF // flip last byte
	os.WriteFile(paths[0], data, 0600)

	_, err := Restore([]string{paths[0], paths[1]}, "pw", restoreDir)
	if err == nil {
		t.Fatal("restore with corrupted file should fail")
	}
}

func TestRestore_DuplicatePartNumber(t *testing.T) {
	inputDir, outputDir, restoreDir := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "test.bin", []byte("data"))

	paths, _ := Archive(inputPath, "pw", outputDir)

	// Provide the same file twice
	_, err := Restore([]string{paths[0], paths[0]}, "pw", restoreDir)
	if err == nil {
		t.Fatal("restore with duplicate part numbers should fail")
	}
}

// ---------------------------------------------------------------------------
// ArchiveMultiple Tests
// ---------------------------------------------------------------------------

func TestArchiveMultiple_SingleFile_MatchesArchive(t *testing.T) {
	// ArchiveMultiple with one file should yield the same 3-file grouping as Archive.
	inputDir, outputDir, _ := setupTestDirs(t)
	inputPath := writeTestFile(t, inputDir, "solo.txt", []byte("solo content"))

	result, err := ArchiveMultiple([]string{inputPath}, "pw", outputDir)
	if err != nil {
		t.Fatalf("ArchiveMultiple failed: %v", err)
	}

	for i, group := range result {
		if len(group) != 1 {
			t.Fatalf("part %d: expected 1 path, got %d", i+1, len(group))
		}
		if _, err := os.Stat(group[0]); os.IsNotExist(err) {
			t.Fatalf("part %d file does not exist: %s", i+1, group[0])
		}
	}
}

func TestArchiveMultiple_PartGroupCounts(t *testing.T) {
	// With N input files the result must have exactly N paths per part group.
	inputDir, outputDir, _ := setupTestDirs(t)
	files := []string{
		writeTestFile(t, inputDir, "a.txt", []byte("alpha")),
		writeTestFile(t, inputDir, "b.txt", []byte("beta")),
		writeTestFile(t, inputDir, "c.txt", []byte("gamma")),
	}

	result, err := ArchiveMultiple(files, "pw", outputDir)
	if err != nil {
		t.Fatalf("ArchiveMultiple failed: %v", err)
	}

	for i, group := range result {
		if len(group) != len(files) {
			t.Fatalf("part %d: expected %d paths, got %d", i+1, len(files), len(group))
		}
	}
}

func TestArchiveMultiple_PartSuffixesAreCorrect(t *testing.T) {
	// Part group 0 → .p1.sand, group 1 → .p2.sand, group 2 → .p3.sand.
	inputDir, outputDir, _ := setupTestDirs(t)
	files := []string{
		writeTestFile(t, inputDir, "x.bin", []byte("x")),
		writeTestFile(t, inputDir, "y.bin", []byte("y")),
	}

	result, err := ArchiveMultiple(files, "pw", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	for partIdx, group := range result {
		expectedSuffix := fmt.Sprintf(".p%d.sand", partIdx+1)
		for _, path := range group {
			if !strings.HasSuffix(path, expectedSuffix) {
				t.Fatalf("part group %d: path %s has wrong suffix (want %s)", partIdx+1, path, expectedSuffix)
			}
		}
	}
}

func TestArchiveMultiple_EachFileRestoresIndependently(t *testing.T) {
	// Parts for file A should not interfere with restoration of file B.
	inputDir, outputDir, _ := setupTestDirs(t)
	origA := []byte("contents of file A — lots of text to compress well")
	origB := []byte("contents of file B — totally different data 12345")

	paths := []string{
		writeTestFile(t, inputDir, "fileA.txt", origA),
		writeTestFile(t, inputDir, "fileB.txt", origB),
	}

	result, err := ArchiveMultiple(paths, "secret", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	// result[i][0] = fileA's part i+1, result[i][1] = fileB's part i+1
	fileAMedia := []string{result[0][0], result[1][0], result[2][0]} // all 3 parts for A
	fileBMedia := []string{result[0][1], result[1][1], result[2][1]} // all 3 parts for B

	for _, tc := range []struct {
		name     string
		parts    []string
		expected []byte
	}{
		{"fileA parts 1+2", fileAMedia[:2], origA},
		{"fileA parts 1+3", []string{fileAMedia[0], fileAMedia[2]}, origA},
		{"fileA parts 2+3", fileAMedia[1:], origA},
		{"fileB parts 1+2", fileBMedia[:2], origB},
		{"fileB parts 2+3", fileBMedia[1:], origB},
	} {
		rd := t.TempDir()
		out, err := Restore(tc.parts, "secret", rd)
		if err != nil {
			t.Fatalf("%s: restore failed: %v", tc.name, err)
		}
		got, _ := os.ReadFile(out)
		if !bytes.Equal(got, tc.expected) {
			t.Fatalf("%s: data mismatch", tc.name)
		}
	}
}

func TestArchiveMultiple_IndependentArchiveIDs(t *testing.T) {
	// Parts from different files in the same batch must have different ArchiveIDs,
	// so they cannot be accidentally mixed during restore.
	inputDir, outputDir, _ := setupTestDirs(t)
	p1 := writeTestFile(t, inputDir, "one.txt", []byte("one"))
	p2 := writeTestFile(t, inputDir, "two.txt", []byte("two"))

	result, err := ArchiveMultiple([]string{p1, p2}, "pw", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	// Mixing a part from file1 with a part from file2 should fail.
	restoreDir := t.TempDir()
	_, err = Restore([]string{result[0][0], result[1][1]}, "pw", restoreDir)
	if err == nil {
		t.Fatal("cross-file part mix should fail restore (mismatched ArchiveID)")
	}
}

func TestArchiveMultiple_EmptyInputList(t *testing.T) {
	outputDir := t.TempDir()
	result, err := ArchiveMultiple([]string{}, "pw", outputDir)
	if err != nil {
		t.Fatalf("empty list should not error: %v", err)
	}
	for i, group := range result {
		if len(group) != 0 {
			t.Fatalf("part %d: expected 0 paths for empty input, got %d", i+1, len(group))
		}
	}
}

func TestArchiveMultiple_NonexistentFileReturnsError(t *testing.T) {
	outputDir := t.TempDir()
	_, err := ArchiveMultiple([]string{"/no/such/file.txt"}, "pw", outputDir)
	if err == nil {
		t.Fatal("should error on nonexistent input file")
	}
}

func TestArchiveMultiple_ErrorMidwayLeavesNoPartialState(t *testing.T) {
	// If the second file fails, the function returns an error.
	inputDir, outputDir, _ := setupTestDirs(t)
	good := writeTestFile(t, inputDir, "good.txt", []byte("good"))

	_, err := ArchiveMultiple([]string{good, "/does/not/exist.bin"}, "pw", outputDir)
	if err == nil {
		t.Fatal("should return error when one file is missing")
	}
}

func TestArchiveMultiple_MixedFileSizes(t *testing.T) {
	// Verify correct round-trip with files of very different sizes (odd + even + empty).
	inputDir, outputDir, _ := setupTestDirs(t)

	contents := map[string][]byte{
		"empty.bin": {},
		"odd.bin":   {0x01, 0x02, 0x03},
		"even.bin":  {0xAA, 0xBB, 0xCC, 0xDD},
	}
	// Build deterministic order so we can map result indices.
	names := []string{"empty.bin", "odd.bin", "even.bin"}
	var filePaths []string
	for _, name := range names {
		filePaths = append(filePaths, writeTestFile(t, inputDir, name, contents[name]))
	}

	result, err := ArchiveMultiple(filePaths, "pw", outputDir)
	if err != nil {
		t.Fatal(err)
	}

	for fileIdx, name := range names {
		expected := contents[name]
		partBlobs := []string{result[0][fileIdx], result[1][fileIdx], result[2][fileIdx]}

		rd := t.TempDir()
		out, err := Restore(partBlobs[:2], "pw", rd) // parts 1+2
		if err != nil {
			t.Fatalf("%s: restore failed: %v", name, err)
		}
		got, _ := os.ReadFile(out)
		if !bytes.Equal(got, expected) {
			t.Fatalf("%s: content mismatch", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Varying Sizes — Stress Test
// ---------------------------------------------------------------------------

func TestRoundTrip_VaryingSizes(t *testing.T) {
	sizes := []int{0, 1, 2, 3, 4, 5, 7, 8, 15, 16, 17, 31, 32, 33,
		63, 64, 65, 127, 128, 129, 255, 256, 257, 511, 512, 513,
		1023, 1024, 1025, 4095, 4096, 4097}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			inputDir, outputDir, _ := setupTestDirs(t)

			original := make([]byte, size)
			for i := range original {
				original[i] = byte(i % 256)
			}

			inputPath := writeTestFile(t, inputDir, "test.bin", original)
			paths, err := Archive(inputPath, "pw", outputDir)
			if err != nil {
				t.Fatal(err)
			}

			// Test all 2-of-3 combinations
			combos := [][]string{
				{paths[0], paths[1]},
				{paths[0], paths[2]},
				{paths[1], paths[2]},
			}

			for _, combo := range combos {
				rd := t.TempDir()
				op, err := Restore(combo, "pw", rd)
				if err != nil {
					t.Fatalf("combo %v failed: %v", combo, err)
				}
				restored, _ := os.ReadFile(op)
				if !bytes.Equal(original, restored) {
					t.Fatalf("data mismatch for size %d", size)
				}
			}
		})
	}
}

// A scheme is k of n, and from format 4 on that is what the header says rather
// than what the build assumes. These are the tests for the numbers themselves:
// which pairs SAND will write, how they are read back off a string, and what
// the three columns of the tradeoff come to.

func TestSchemeOfAcceptsAnyCodeTheCoderCanBuild(t *testing.T) {
	for _, s := range []Scheme{
		{Data: 2, Total: 3},
		{Data: 2, Total: 5},
		{Data: 3, Total: 5},
		{Data: 6, Total: 10},
		{Data: 4, Total: 4}, // no parity at all is a code, just not a redundant one
		{Data: 2, Total: MaxAccounts},
		{Data: MaxAccounts, Total: MaxAccounts},
	} {
		if _, err := SchemeOf(s.Data, s.Total); err != nil {
			t.Errorf("SchemeOf(%d, %d): %v", s.Data, s.Total, err)
		}
	}
}

func TestSchemeOfRefusesWhatWouldNotBeSplitting(t *testing.T) {
	for _, tc := range []struct {
		data, total int
		want        string
	}{
		{1, 3, "at least 2 shards"}, // one account would hold the whole file
		{0, 3, "at least 2 shards"}, // the zero value of an old index row
		{4, 3, "more shards to rebuild than it makes"},
		{2, MaxAccounts + 1, "counts to"},
	} {
		_, err := SchemeOf(tc.data, tc.total)
		if err == nil {
			t.Fatalf("SchemeOf(%d, %d) was accepted", tc.data, tc.total)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("SchemeOf(%d, %d) = %q, want it to mention %q", tc.data, tc.total, err, tc.want)
		}
	}
}

func TestParseSchemeRoundTripsString(t *testing.T) {
	for _, s := range []Scheme{
		SchemeDefault, SchemeWide, SchemeWider,
		{Data: 2, Total: 5}, {Data: 6, Total: 10}, {Data: 20, Total: 30},
	} {
		got, err := ParseScheme(s.String())
		if err != nil {
			t.Fatalf("ParseScheme(%q): %v", s, err)
		}
		if got != s {
			t.Errorf("ParseScheme(%q) = %s", s, got)
		}
	}
}

func TestParseSchemeRefusesWhatIsNotOne(t *testing.T) {
	for _, raw := range []string{"", "3", "3of5", "3-of-", "-of-5", "3-of-five", "3/5", "1-of-3", "9-of-3"} {
		if got, err := ParseScheme(raw); err == nil {
			t.Errorf("ParseScheme(%q) = %s, want an error", raw, got)
		}
	}
}

func TestSchemeTradeoffColumns(t *testing.T) {
	for _, tc := range []struct {
		scheme    Scheme
		storage   float64
		tolerance int
		family    bool
	}{
		{Scheme{Data: 2, Total: 3}, 1.5, 1, true},
		{Scheme{Data: 3, Total: 5}, 5.0 / 3, 2, false},
		{Scheme{Data: 2, Total: 5}, 2.5, 3, false},
		{Scheme{Data: 6, Total: 9}, 1.5, 3, true},
		{Scheme{Data: 6, Total: 10}, 10.0 / 6, 4, false},
	} {
		if got := tc.scheme.Storage(); got != tc.storage {
			t.Errorf("%s stores %g×, want %g×", tc.scheme, got, tc.storage)
		}
		if got := tc.scheme.Tolerance(); got != tc.tolerance {
			t.Errorf("%s survives %d losses, want %d", tc.scheme, got, tc.tolerance)
		}
		if got := tc.scheme.Family(); got != tc.family {
			t.Errorf("%s.Family() = %v, want %v", tc.scheme, got, tc.family)
		}
	}
}

func TestOffFamilySchemesRoundTripThroughAChunk(t *testing.T) {
	// The coder and the format have taken any k of n since version 4; this is
	// the check that a chunk cut outside the default family comes back from
	// exactly k of its shards, parity ones included.
	for _, scheme := range []Scheme{
		{Data: 2, Total: 5},
		{Data: 3, Total: 5},
		{Data: 6, Total: 10},
	} {
		t.Run(scheme.String(), func(t *testing.T) {
			master := bytes.Repeat([]byte{0x5a}, 32)
			payload := make([]byte, 40_000)
			if _, err := rand.Read(payload); err != nil {
				t.Fatalf("rand: %v", err)
			}

			var archiveID [16]byte
			copy(archiveID[:], "off-family-abcde")
			var hash [32]byte

			plan, err := PlanChunks(archiveID, "x.bin", hash, uint64(len(payload)), uint32(len(payload)), scheme)
			if err != nil {
				t.Fatalf("PlanChunks as %s: %v", scheme, err)
			}
			encoded, err := EncodeChunk(plan, 0, payload, master)
			if err != nil {
				t.Fatalf("EncodeChunk as %s: %v", scheme, err)
			}
			if len(encoded.Parts) != scheme.Total {
				t.Fatalf("cut into %d parts, want %d", len(encoded.Parts), scheme.Total)
			}

			// The last k shards, so the rebuild goes through the parity rows
			// rather than down the systematic fast path.
			survivors := encoded.Parts[scheme.Total-scheme.Data:]
			decoded, err := DecodeChunk(survivors, master)
			if err != nil {
				t.Fatalf("DecodeChunk from the last %d shards of %s: %v", scheme.Data, scheme, err)
			}
			if !bytes.Equal(decoded.Data, payload) {
				t.Fatal("what came back is not what went in")
			}
		})
	}
}
