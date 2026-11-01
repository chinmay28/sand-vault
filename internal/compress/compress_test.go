package compress

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestCompressDecompress_RoundTrip(t *testing.T) {
	original := []byte("Hello, SAND! This is a test of the compression layer.")

	compressed, err := Compress(original)
	if err != nil {
		t.Fatalf("compression failed: %v", err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatalf("decompression failed: %v", err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("round-trip failed: decompressed data differs from original")
	}
}

func TestCompress_ReducesSize(t *testing.T) {
	// Highly compressible data
	original := []byte(strings.Repeat("ABCDEFGH", 1000))

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	if len(compressed) >= len(original) {
		t.Fatalf("compression should reduce size for repetitive data: original=%d compressed=%d",
			len(original), len(compressed))
	}
}

func TestCompressDecompress_EmptyInput(t *testing.T) {
	compressed, err := Compress([]byte{})
	if err != nil {
		t.Fatalf("compression of empty input failed: %v", err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatalf("decompression of empty input failed: %v", err)
	}

	if len(decompressed) != 0 {
		t.Fatalf("expected empty output, got %d bytes", len(decompressed))
	}
}

func TestCompressDecompress_SingleByte(t *testing.T) {
	original := []byte{0x42}

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("single byte round-trip failed")
	}
}

func TestCompressDecompress_AllZeroBytes(t *testing.T) {
	original := make([]byte, 10000)

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	// All zeros should compress very well
	if len(compressed) >= len(original)/2 {
		t.Fatalf("all-zeros should compress dramatically: original=%d compressed=%d",
			len(original), len(compressed))
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("all-zeros round-trip failed")
	}
}

func TestCompressDecompress_RandomData(t *testing.T) {
	// Random data is incompressible — verify it still round-trips correctly
	original := make([]byte, 4096)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("random data round-trip failed")
	}
}

func TestCompressDecompress_LargePayload(t *testing.T) {
	// 2MB of text-like data
	original := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40000))

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("large payload round-trip failed")
	}
}

func TestCompressDecompress_BinaryData(t *testing.T) {
	// All possible byte values
	original := make([]byte, 256)
	for i := range original {
		original[i] = byte(i)
	}

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("binary data round-trip failed")
	}
}

func TestCompressDecompress_OddLength(t *testing.T) {
	// Specifically test odd-length inputs since SAND pads these
	for _, size := range []int{1, 3, 5, 7, 99, 1001, 65537} {
		original := make([]byte, size)
		for i := range original {
			original[i] = byte(i % 256)
		}

		compressed, err := Compress(original)
		if err != nil {
			t.Fatalf("compression failed for size %d: %v", size, err)
		}

		decompressed, err := Decompress(compressed)
		if err != nil {
			t.Fatalf("decompression failed for size %d: %v", size, err)
		}

		if !bytes.Equal(original, decompressed) {
			t.Fatalf("round-trip failed for odd size %d", size)
		}
	}
}

func TestDecompress_InvalidData(t *testing.T) {
	_, err := Decompress([]byte("this is not valid zstd data"))
	if err == nil {
		t.Fatal("decompressing invalid data should fail")
	}
}

func TestDecompress_TruncatedData(t *testing.T) {
	original := []byte("some data to compress and then truncate")
	compressed, _ := Compress(original)

	// Truncate the compressed data
	truncated := compressed[:len(compressed)/2]

	_, err := Decompress(truncated)
	if err == nil {
		t.Fatal("decompressing truncated data should fail")
	}
}

func TestCompressDecompress_UnicodeContent(t *testing.T) {
	original := []byte("Unicode test: 日本語 العربية 한국어 emoji: 🔐🗂️💾")

	compressed, err := Compress(original)
	if err != nil {
		t.Fatal(err)
	}

	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Fatal("unicode round-trip failed")
	}
}

// Configure changes the level the next chunk is compressed at, and what was
// compressed at any level decompresses the same.
func TestConfigureChangesTheLevelAndNothingElse(t *testing.T) {
	t.Cleanup(func() { Configure(Options{Level: DefaultLevel, Threads: DefaultThreads}) })

	// Repetitive enough to compress at every level, long enough that the
	// levels differ.
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog; ", 4000))

	sizes := map[int]int{}
	for level := 1; level <= 4; level++ {
		Configure(Options{Level: level, Threads: 1})
		if got := Current(); got.Level != level || got.Threads != 1 {
			t.Fatalf("Current() = %+v after Configure(level %d)", got, level)
		}
		out, err := Compress(data)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Decompress(out)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if !bytes.Equal(back, data) {
			t.Fatalf("level %d: round trip changed the data", level)
		}
		sizes[level] = len(out)
	}
	if sizes[4] > sizes[1] {
		t.Errorf("best compression (%d bytes) is larger than fastest (%d)", sizes[4], sizes[1])
	}

	// Out-of-range values are clamped, not refused.
	Configure(Options{Level: 99, Threads: 0})
	if got := Current(); got.Level != 4 || got.Threads != 1 {
		t.Errorf("Configure did not clamp: %+v", got)
	}
	if _, err := Compress(data); err != nil {
		t.Errorf("compressing after a clamped Configure: %v", err)
	}
}
