package mediafile

import (
	"bytes"
	"testing"

	"github.com/sand-project/sand/internal/crypto"
)

func makeTestHeader(partNum uint8, filename string) *Header {
	nonce := make([]byte, crypto.NonceSize)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	return &Header{
		Version:        FormatVersion,
		PartNumber:     partNum,
		ArchiveID:      [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		OriginalHash:   [32]byte{0xAA, 0xBB, 0xCC},
		OriginalSize:   12345,
		CompressedSize: 6789,
		WasPadded:      true,
		Filename:       filename,
		Salt:           bytes.Repeat([]byte{0xDE}, 16),
		Argon2Time:     3,
		Argon2Memory:   65536,
		Argon2Threads:  4,
		Nonce:          nonce,
	}
}

// ---------------------------------------------------------------------------
// Header Marshal/Unmarshal Round-Trip
// ---------------------------------------------------------------------------

func TestHeaderRoundTrip(t *testing.T) {
	h := makeTestHeader(1, "passport.pdf")

	data, err := MarshalHeader(h)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	h2, consumed, err := UnmarshalHeader(data)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if consumed != len(data) {
		t.Fatalf("consumed %d bytes, expected %d", consumed, len(data))
	}

	// Verify all fields
	if h2.Version != h.Version {
		t.Errorf("version: got %d, want %d", h2.Version, h.Version)
	}
	if h2.PartNumber != h.PartNumber {
		t.Errorf("part number: got %d, want %d", h2.PartNumber, h.PartNumber)
	}
	if h2.ArchiveID != h.ArchiveID {
		t.Error("archive ID mismatch")
	}
	if h2.OriginalHash != h.OriginalHash {
		t.Error("original hash mismatch")
	}
	if h2.OriginalSize != h.OriginalSize {
		t.Errorf("original size: got %d, want %d", h2.OriginalSize, h.OriginalSize)
	}
	if h2.CompressedSize != h.CompressedSize {
		t.Errorf("compressed size: got %d, want %d", h2.CompressedSize, h.CompressedSize)
	}
	if h2.WasPadded != h.WasPadded {
		t.Errorf("was padded: got %v, want %v", h2.WasPadded, h.WasPadded)
	}
	if h2.Filename != h.Filename {
		t.Errorf("filename: got %q, want %q", h2.Filename, h.Filename)
	}
	if !bytes.Equal(h2.Salt, h.Salt) {
		t.Error("salt mismatch")
	}
	if h2.Argon2Time != h.Argon2Time {
		t.Errorf("argon2 time: got %d, want %d", h2.Argon2Time, h.Argon2Time)
	}
	if h2.Argon2Memory != h.Argon2Memory {
		t.Errorf("argon2 memory: got %d, want %d", h2.Argon2Memory, h.Argon2Memory)
	}
	if h2.Argon2Threads != h.Argon2Threads {
		t.Errorf("argon2 threads: got %d, want %d", h2.Argon2Threads, h.Argon2Threads)
	}
	if !bytes.Equal(h2.Nonce, h.Nonce) {
		t.Error("nonce mismatch")
	}
}

func TestHeaderRoundTrip_AllPartNumbers(t *testing.T) {
	for _, pn := range []uint8{1, 2, 3} {
		h := makeTestHeader(pn, "test.bin")
		data, err := MarshalHeader(h)
		if err != nil {
			t.Fatalf("part %d: marshal failed: %v", pn, err)
		}
		h2, _, err := UnmarshalHeader(data)
		if err != nil {
			t.Fatalf("part %d: unmarshal failed: %v", pn, err)
		}
		if h2.PartNumber != pn {
			t.Fatalf("part %d: got %d", pn, h2.PartNumber)
		}
	}
}

func TestHeaderRoundTrip_WasPaddedFalse(t *testing.T) {
	h := makeTestHeader(1, "even.bin")
	h.WasPadded = false

	data, _ := MarshalHeader(h)
	h2, _, _ := UnmarshalHeader(data)

	if h2.WasPadded {
		t.Fatal("WasPadded should be false")
	}
}

func TestHeaderRoundTrip_EmptyFilename(t *testing.T) {
	h := makeTestHeader(1, "")
	data, err := MarshalHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	h2, _, err := UnmarshalHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Filename != "" {
		t.Fatalf("expected empty filename, got %q", h2.Filename)
	}
}

func TestHeaderRoundTrip_LongFilename(t *testing.T) {
	name := ""
	for i := 0; i < MaxFilenameLength; i++ {
		name += "a"
	}
	h := makeTestHeader(1, name)
	data, err := MarshalHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	h2, _, err := UnmarshalHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Filename != name {
		t.Fatal("long filename mismatch")
	}
}

func TestHeaderRoundTrip_UnicodeFilename(t *testing.T) {
	h := makeTestHeader(1, "документ_🔐.pdf")
	data, err := MarshalHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	h2, _, err := UnmarshalHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Filename != "документ_🔐.pdf" {
		t.Fatalf("unicode filename mismatch: got %q", h2.Filename)
	}
}

// ---------------------------------------------------------------------------
// Magic Bytes
// ---------------------------------------------------------------------------

func TestHeader_MagicBytes(t *testing.T) {
	h := makeTestHeader(1, "test.bin")
	data, _ := MarshalHeader(h)

	if !bytes.Equal(data[:4], []byte("SAND")) {
		t.Fatalf("magic bytes: got %v, want SAND", data[:4])
	}
}

func TestUnmarshal_InvalidMagic(t *testing.T) {
	data := []byte("NOPE" + "\x01\x01") // wrong magic
	_, _, err := UnmarshalHeader(data)
	if err == nil {
		t.Fatal("should reject invalid magic")
	}
}

func TestUnmarshal_TooShort(t *testing.T) {
	_, _, err := UnmarshalHeader([]byte{0x53, 0x41}) // "SA" — too short
	if err == nil {
		t.Fatal("should reject too-short data")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestMarshal_InvalidPartNumber(t *testing.T) {
	for _, pn := range []uint8{0, 4, 255} {
		h := makeTestHeader(1, "test.bin")
		h.PartNumber = pn
		_, err := MarshalHeader(h)
		if err == nil {
			t.Fatalf("should reject part number %d", pn)
		}
	}
}

func TestMarshal_FilenameTooLong(t *testing.T) {
	name := ""
	for i := 0; i < MaxFilenameLength+1; i++ {
		name += "x"
	}
	h := makeTestHeader(1, name)
	_, err := MarshalHeader(h)
	if err == nil {
		t.Fatal("should reject filename exceeding max length")
	}
}

func TestMarshal_WrongSaltLength(t *testing.T) {
	h := makeTestHeader(1, "test.bin")
	h.Salt = []byte{0x01, 0x02} // too short
	_, err := MarshalHeader(h)
	if err == nil {
		t.Fatal("should reject wrong salt length")
	}
}

func TestMarshal_WrongNonceLength(t *testing.T) {
	h := makeTestHeader(1, "test.bin")
	h.Nonce = []byte{0x01} // too short
	_, err := MarshalHeader(h)
	if err == nil {
		t.Fatal("should reject wrong nonce length")
	}
}

func TestUnmarshal_UnsupportedVersion(t *testing.T) {
	h := makeTestHeader(1, "test.bin")
	data, _ := MarshalHeader(h)
	data[4] = 99 // corrupt version
	_, _, err := UnmarshalHeader(data)
	if err == nil {
		t.Fatal("should reject unsupported version")
	}
}

// ---------------------------------------------------------------------------
// Full Media File Write/Read
// ---------------------------------------------------------------------------

func TestWriteReadMediaFile_RoundTrip(t *testing.T) {
	h := makeTestHeader(2, "secret.docx")
	payload := []byte("encrypted-payload-bytes-here")

	fileData, err := WriteMediaFile(h, payload)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	h2, headerBytes, payload2, err := ReadMediaFile(fileData)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if h2.PartNumber != 2 {
		t.Errorf("part number: got %d, want 2", h2.PartNumber)
	}
	if h2.Filename != "secret.docx" {
		t.Errorf("filename: got %q", h2.Filename)
	}
	if !bytes.Equal(payload2, payload) {
		t.Fatal("payload mismatch")
	}
	if len(headerBytes) == 0 {
		t.Fatal("header bytes should not be empty")
	}

	// Verify header bytes match what MarshalHeader produces
	expectedHeader, _ := MarshalHeader(h)
	if !bytes.Equal(headerBytes, expectedHeader) {
		t.Fatal("header bytes should match MarshalHeader output")
	}
}

func TestWriteReadMediaFile_EmptyPayload(t *testing.T) {
	h := makeTestHeader(3, "empty.bin")
	fileData, err := WriteMediaFile(h, []byte{})
	if err != nil {
		t.Fatal(err)
	}

	_, _, payload, err := ReadMediaFile(fileData)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		t.Fatal("expected empty payload")
	}
}

func TestReadMediaFile_Truncated(t *testing.T) {
	h := makeTestHeader(1, "test.bin")
	payload := bytes.Repeat([]byte{0xAA}, 100)
	fileData, _ := WriteMediaFile(h, payload)

	// Truncate the file
	truncated := fileData[:len(fileData)-50]
	_, _, _, err := ReadMediaFile(truncated)
	if err == nil {
		t.Fatal("should fail on truncated file")
	}
}

func TestReadMediaFile_GarbageData(t *testing.T) {
	_, _, _, err := ReadMediaFile([]byte("this is not a SAND file"))
	if err == nil {
		t.Fatal("should fail on garbage data")
	}
}

func TestWriteReadMediaFile_LargePayload(t *testing.T) {
	h := makeTestHeader(1, "big.zip")
	payload := bytes.Repeat([]byte{0xBB}, 1<<20) // 1MB

	fileData, err := WriteMediaFile(h, payload)
	if err != nil {
		t.Fatal(err)
	}

	_, _, payload2, err := ReadMediaFile(fileData)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload2, payload) {
		t.Fatal("large payload mismatch")
	}
}
