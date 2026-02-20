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

		expectedSuffix := ".media" + string(rune('1'+i))
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


