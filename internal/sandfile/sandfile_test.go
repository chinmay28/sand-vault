package sandfile

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

func testKey() []byte { return bytes.Repeat([]byte{0x2B}, 32) }

func makeTestHeader(partNum uint8) *Header {
	nonce := make([]byte, crypto.NonceSize)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	return &Header{
		Version:       FormatVersion,
		PartNumber:    partNum,
		ArchiveID:     [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		Salt:          bytes.Repeat([]byte{0xDE}, saltLength),
		Argon2Time:    3,
		Argon2Memory:  65536,
		Argon2Threads: 4,
		Nonce:         nonce,
	}
}

func makeTestMetadata(filename string) *Metadata {
	return &Metadata{
		Filename:       filename,
		OriginalHash:   [32]byte{0xAA, 0xBB, 0xCC},
		OriginalSize:   12345,
		CompressedSize: 6789,
		WasPadded:      true,
	}
}

// ---------------------------------------------------------------------------
// Seal / Open round trip
// ---------------------------------------------------------------------------

func TestSealOpenRoundTrip(t *testing.T) {
	h := makeTestHeader(2)
	meta := makeTestMetadata("secret.docx")
	partData := []byte("this half of the compressed stream")

	blob, err := Seal(h, meta, partData, testKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	part, err := ReadPart(blob)
	if err != nil {
		t.Fatalf("ReadPart: %v", err)
	}
	if part.Header.Version != FormatVersion {
		t.Errorf("version = %d, want %d", part.Header.Version, FormatVersion)
	}
	if part.Header.PartNumber != 2 {
		t.Errorf("part number = %d, want 2", part.Header.PartNumber)
	}
	if part.Header.ArchiveID != h.ArchiveID {
		t.Error("archive ID mismatch")
	}
	if !bytes.Equal(part.Header.Salt, h.Salt) {
		t.Error("salt mismatch")
	}
	if part.Header.Argon2Time != 3 || part.Header.Argon2Memory != 65536 || part.Header.Argon2Threads != 4 {
		t.Error("argon2 parameters mismatch")
	}
	if !bytes.Equal(part.Header.Nonce, h.Nonce) {
		t.Error("nonce mismatch")
	}

	gotMeta, gotData, err := part.Open(testKey())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if gotMeta.Filename != meta.Filename {
		t.Errorf("filename = %q, want %q", gotMeta.Filename, meta.Filename)
	}
	if gotMeta.OriginalHash != meta.OriginalHash {
		t.Error("original hash mismatch")
	}
	if gotMeta.OriginalSize != meta.OriginalSize || gotMeta.CompressedSize != meta.CompressedSize {
		t.Error("size mismatch")
	}
	if !gotMeta.WasPadded {
		t.Error("WasPadded should survive the round trip")
	}
	if !bytes.Equal(gotData, partData) {
		t.Fatal("part data mismatch")
	}
}

func TestSealOpen_AllPartNumbers(t *testing.T) {
	for _, pn := range []uint8{1, 2, 3} {
		blob, err := Seal(makeTestHeader(pn), makeTestMetadata("test.bin"), []byte("x"), testKey())
		if err != nil {
			t.Fatalf("part %d: Seal: %v", pn, err)
		}
		part, err := ReadPart(blob)
		if err != nil {
			t.Fatalf("part %d: ReadPart: %v", pn, err)
		}
		if part.Header.PartNumber != pn {
			t.Fatalf("part number = %d, want %d", part.Header.PartNumber, pn)
		}
	}
}

func TestSealOpen_EmptyPartData(t *testing.T) {
	blob, err := Seal(makeTestHeader(3), makeTestMetadata("empty.bin"), []byte{}, testKey())
	if err != nil {
		t.Fatal(err)
	}
	part, err := ReadPart(blob)
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := part.Open(testKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty part data, got %d bytes", len(data))
	}
}

func TestSealOpen_LargePartData(t *testing.T) {
	partData := bytes.Repeat([]byte{0xBB}, 1<<20) // 1MB
	blob, err := Seal(makeTestHeader(1), makeTestMetadata("big.zip"), partData, testKey())
	if err != nil {
		t.Fatal(err)
	}
	part, _ := ReadPart(blob)
	_, got, err := part.Open(testKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, partData) {
		t.Fatal("large part data mismatch")
	}
}

func TestSealOpen_UnicodeFilename(t *testing.T) {
	name := "документ_🔐.pdf"
	blob, _ := Seal(makeTestHeader(1), makeTestMetadata(name), []byte("d"), testKey())
	part, _ := ReadPart(blob)
	meta, _, err := part.Open(testKey())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Filename != name {
		t.Fatalf("filename = %q, want %q", meta.Filename, name)
	}
}

func TestSealOpen_EmptyAndMaxLengthFilenames(t *testing.T) {
	for _, name := range []string{"", strings.Repeat("a", MaxFilenameLength)} {
		blob, err := Seal(makeTestHeader(1), makeTestMetadata(name), []byte("d"), testKey())
		if err != nil {
			t.Fatalf("filename of length %d: Seal: %v", len(name), err)
		}
		part, _ := ReadPart(blob)
		meta, _, err := part.Open(testKey())
		if err != nil {
			t.Fatalf("filename of length %d: Open: %v", len(name), err)
		}
		if meta.Filename != name {
			t.Errorf("filename of length %d did not survive the round trip", len(name))
		}
	}
}

// ---------------------------------------------------------------------------
// What a part reveals without the key
// ---------------------------------------------------------------------------

// A part is handed to a cloud account that is not trusted with its contents.
// Version 1 wrote the filename into the cleartext header, so anyone holding a
// single part could read it; version 2 must not.
func TestSealedPartLeaksNoMetadata(t *testing.T) {
	meta := makeTestMetadata("Q4 layoffs - confidential.txt")
	blob, err := Seal(makeTestHeader(1), meta, []byte("half the compressed stream"), testKey())
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(blob, []byte("Q4 layoffs")) {
		t.Error("the filename appears in cleartext inside the part file")
	}
	if bytes.Contains(blob, meta.OriginalHash[:]) {
		t.Error("the plaintext hash appears in cleartext inside the part file")
	}
	sizeBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(sizeBytes, meta.OriginalSize)
	if bytes.Contains(blob, sizeBytes) {
		t.Error("the original size appears in cleartext inside the part file")
	}

	// What is left in the clear is only what a reader needs to derive the key.
	part, err := ReadPart(blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := part.Open(bytes.Repeat([]byte{0x01}, 32)); err == nil {
		t.Error("Open should fail under the wrong key")
	}
}

// The cleartext header is the GCM associated data, so re-labelling a part —
// claiming part 1 is part 3, say — has to fail the tag check.
func TestOpenRejectsATamperedHeader(t *testing.T) {
	blob, err := Seal(makeTestHeader(1), makeTestMetadata("test.bin"), []byte("data"), testKey())
	if err != nil {
		t.Fatal(err)
	}
	blob[5] = 3 // part number

	part, err := ReadPart(blob)
	if err != nil {
		t.Fatalf("ReadPart: %v", err)
	}
	if _, _, err := part.Open(testKey()); err == nil {
		t.Fatal("Open should reject a part whose header was edited")
	}
}

// ---------------------------------------------------------------------------
// Parsing and validation
// ---------------------------------------------------------------------------

func TestHeader_MagicBytes(t *testing.T) {
	data, _ := MarshalHeader(makeTestHeader(1))
	if !bytes.Equal(data[:4], []byte("SAND")) {
		t.Fatalf("magic bytes: got %v, want SAND", data[:4])
	}
}

func TestReadPart_InvalidMagic(t *testing.T) {
	if _, err := ReadPart([]byte("NOPE\x02\x01")); err == nil {
		t.Fatal("should reject invalid magic")
	}
}

func TestReadPart_TooShort(t *testing.T) {
	if _, err := ReadPart([]byte{0x53, 0x41}); err == nil {
		t.Fatal("should reject too-short data")
	}
}

func TestReadPart_GarbageData(t *testing.T) {
	if _, err := ReadPart([]byte("this is not a SAND file")); err == nil {
		t.Fatal("should fail on garbage data")
	}
}

func TestReadPart_Truncated(t *testing.T) {
	blob, _ := Seal(makeTestHeader(1), makeTestMetadata("test.bin"), bytes.Repeat([]byte{0xAA}, 100), testKey())
	if _, err := ReadPart(blob[:len(blob)-50]); err == nil {
		t.Fatal("should fail on a truncated file")
	}
}

func TestReadPart_UnsupportedVersion(t *testing.T) {
	blob, _ := Seal(makeTestHeader(1), makeTestMetadata("test.bin"), []byte("d"), testKey())
	blob[4] = 99
	if _, err := ReadPart(blob); err == nil {
		t.Fatal("should reject an unsupported version")
	}
}

func TestMarshalHeader_InvalidPartNumber(t *testing.T) {
	for _, pn := range []uint8{0, 4, 255} {
		h := makeTestHeader(1)
		h.PartNumber = pn
		if _, err := MarshalHeader(h); err == nil {
			t.Fatalf("should reject part number %d", pn)
		}
	}
}

func TestMarshalHeader_WrongSaltLength(t *testing.T) {
	h := makeTestHeader(1)
	h.Salt = []byte{0x01, 0x02}
	if _, err := MarshalHeader(h); err == nil {
		t.Fatal("should reject the wrong salt length")
	}
}

func TestMarshalHeader_WrongNonceLength(t *testing.T) {
	h := makeTestHeader(1)
	h.Nonce = []byte{0x01}
	if _, err := MarshalHeader(h); err == nil {
		t.Fatal("should reject the wrong nonce length")
	}
}

func TestMarshalHeader_RefusesToWriteAnOlderVersion(t *testing.T) {
	h := makeTestHeader(1)
	h.Version = LegacyFormatVersion
	if _, err := MarshalHeader(h); err == nil {
		t.Fatal("should refuse to write a legacy-version header")
	}
}

func TestMarshalMetadata_FilenameTooLong(t *testing.T) {
	meta := makeTestMetadata(strings.Repeat("x", MaxFilenameLength+1))
	if _, err := MarshalMetadata(meta); err == nil {
		t.Fatal("should reject a filename over the maximum length")
	}
}

// ---------------------------------------------------------------------------
// Version 1 parts, still readable
// ---------------------------------------------------------------------------

// sealV1 writes a part in the original layout: the file's metadata sat in the
// cleartext header, and the payload held nothing but the part data.
func sealV1(h *Header, m *Metadata, partData, key []byte) ([]byte, error) {
	buf := new(bytes.Buffer)
	buf.Write(Magic[:])
	buf.WriteByte(LegacyFormatVersion)
	buf.WriteByte(h.PartNumber)
	buf.Write(h.ArchiveID[:])
	buf.Write(m.OriginalHash[:])
	binary.Write(buf, binary.BigEndian, m.OriginalSize)
	binary.Write(buf, binary.BigEndian, m.CompressedSize)
	if m.WasPadded {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	name := []byte(m.Filename)
	binary.Write(buf, binary.BigEndian, uint16(len(name)))
	buf.Write(name)
	buf.Write(h.Salt)
	binary.Write(buf, binary.BigEndian, h.Argon2Time)
	binary.Write(buf, binary.BigEndian, h.Argon2Memory)
	buf.WriteByte(h.Argon2Threads)
	buf.Write(h.Nonce)

	headerBytes := append([]byte(nil), buf.Bytes()...)
	ciphertext, err := crypto.Encrypt(key, h.Nonce, partData, headerBytes)
	if err != nil {
		return nil, err
	}
	binary.Write(buf, binary.BigEndian, uint32(len(ciphertext)))
	buf.Write(ciphertext)
	return buf.Bytes(), nil
}

func TestReadPart_OpensLegacyParts(t *testing.T) {
	h := makeTestHeader(2)
	meta := makeTestMetadata("legacy-archive.pdf")
	partData := []byte("payload written by an older build")

	blob, err := sealV1(h, meta, partData, testKey())
	if err != nil {
		t.Fatalf("sealV1: %v", err)
	}

	part, err := ReadPart(blob)
	if err != nil {
		t.Fatalf("ReadPart: %v", err)
	}
	if part.Header.Version != LegacyFormatVersion {
		t.Fatalf("version = %d, want %d", part.Header.Version, LegacyFormatVersion)
	}

	gotMeta, gotData, err := part.Open(testKey())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if gotMeta.Filename != meta.Filename {
		t.Errorf("filename = %q, want %q", gotMeta.Filename, meta.Filename)
	}
	if gotMeta.OriginalSize != meta.OriginalSize || !gotMeta.WasPadded {
		t.Error("legacy metadata did not survive the read")
	}
	if !bytes.Equal(gotData, partData) {
		t.Fatal("legacy part data mismatch")
	}

	// And the reason for the new version: a legacy part says the filename out
	// loud to anyone holding it.
	if !bytes.Contains(blob, []byte("legacy-archive.pdf")) {
		t.Error("expected the legacy layout to carry the filename in cleartext")
	}
}
