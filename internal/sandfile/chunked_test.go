package sandfile

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// chunkIndexOffset is where the chunk index sits in a version 3 header:
// magic (4) + version (1) + part number (1) + archive ID (16).
const chunkIndexOffset = 22

func makeChunkHeader(partNum uint8, chunkIndex uint32) *Header {
	nonce := make([]byte, crypto.NonceSize)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	return &Header{
		Version:    ChunkedFormatVersion,
		PartNumber: partNum,
		ArchiveID:  [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		ChunkIndex: chunkIndex,
		Nonce:      nonce,
	}
}

func makeChunkMetadata() *Metadata {
	return &Metadata{
		Filename:       "movie.mkv",
		OriginalHash:   [32]byte{0xAA, 0xBB, 0xCC},
		OriginalSize:   40 << 20,
		CompressedSize: 1234,
		WasPadded:      true,
		ChunkCount:     3,
		ChunkSize:      16 << 20,
	}
}

func TestChunkedRoundTrip(t *testing.T) {
	partData := []byte("this part's share of chunk two")
	blob, err := Seal(makeChunkHeader(2, 7), makeChunkMetadata(), partData, testKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	part, err := ReadPart(blob)
	if err != nil {
		t.Fatalf("ReadPart: %v", err)
	}
	if part.Header.Version != ChunkedFormatVersion {
		t.Errorf("version = %d, want %d", part.Header.Version, ChunkedFormatVersion)
	}
	if part.Header.ChunkIndex != 7 {
		t.Errorf("chunk index = %d, want 7", part.Header.ChunkIndex)
	}
	// Version 3 derives its key rather than stretching a password, so it has no
	// salt or Argon2 parameters to carry.
	if len(part.Header.Salt) != 0 {
		t.Errorf("version 3 header carries a %d-byte salt, want none", len(part.Header.Salt))
	}

	meta, got, err := part.Open(testKey())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, partData) {
		t.Errorf("part data = %q, want %q", got, partData)
	}
	want := makeChunkMetadata()
	if meta.Filename != want.Filename || meta.OriginalSize != want.OriginalSize ||
		meta.ChunkCount != want.ChunkCount || meta.ChunkSize != want.ChunkSize ||
		meta.WasPadded != want.WasPadded {
		t.Errorf("metadata = %+v, want %+v", meta, want)
	}
}

// The chunk index lives in the cleartext header, which is the part's associated
// data. Moving a sealed chunk to another offset in the same file must therefore
// break its tag rather than quietly rebuilding a rearranged file.
func TestChunkIndexIsAuthenticated(t *testing.T) {
	blob, err := Seal(makeChunkHeader(1, 7), makeChunkMetadata(), []byte("payload"), testKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	replayed := append([]byte(nil), blob...)
	binary.BigEndian.PutUint32(replayed[chunkIndexOffset:], 8)

	part, err := ReadPart(replayed)
	if err != nil {
		t.Fatalf("ReadPart: %v", err)
	}
	if part.Header.ChunkIndex != 8 {
		t.Fatalf("test did not relabel the chunk: index = %d", part.Header.ChunkIndex)
	}
	if _, _, err := part.Open(testKey()); err == nil {
		t.Error("opened a chunk relabelled to another index, want an authentication failure")
	}
}

func TestMarshalHeaderRejectsUnknownVersion(t *testing.T) {
	h := makeChunkHeader(1, 0)
	h.Version = 9
	if _, err := MarshalHeader(h); err == nil {
		t.Error("marshaled an unsupported version, want an error")
	}
}

func TestMarshalChunkMetadataRejectsEmptyChunking(t *testing.T) {
	noCount := makeChunkMetadata()
	noCount.ChunkCount = 0
	if _, err := MarshalChunkMetadata(noCount); err == nil {
		t.Error("marshaled metadata with no chunks, want an error")
	}

	noSize := makeChunkMetadata()
	noSize.ChunkSize = 0
	if _, err := MarshalChunkMetadata(noSize); err == nil {
		t.Error("marshaled metadata with a zero chunk size, want an error")
	}
}

// A whole-file part reports as a single chunk, so a caller can do the same
// arithmetic whatever version it is reading.
func TestWholeFilePartsReportOneChunk(t *testing.T) {
	blob, err := Seal(makeTestHeader(1), makeTestMetadata("notes.txt"), []byte("data"), testKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	part, err := ReadPart(blob)
	if err != nil {
		t.Fatalf("ReadPart: %v", err)
	}
	meta, _, err := part.Open(testKey())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if meta.ChunkCount != 1 {
		t.Errorf("chunk count = %d, want 1 for a version 2 part", meta.ChunkCount)
	}

	size, err := meta.ChunkPlaintextSize(0)
	if err != nil {
		t.Fatalf("ChunkPlaintextSize: %v", err)
	}
	if size != meta.OriginalSize {
		t.Errorf("chunk size = %d, want the whole file at %d", size, meta.OriginalSize)
	}
}

func TestChunkPlaintextSize(t *testing.T) {
	// 40 MiB in 16 MiB chunks: two full and one holding the remaining 8 MiB.
	m := &Metadata{OriginalSize: 40 << 20, ChunkCount: 3, ChunkSize: 16 << 20}

	for index, want := range map[uint32]uint64{0: 16 << 20, 1: 16 << 20, 2: 8 << 20} {
		got, err := m.ChunkPlaintextSize(index)
		if err != nil {
			t.Fatalf("chunk %d: %v", index, err)
		}
		if got != want {
			t.Errorf("chunk %d is %d bytes, want %d", index, got, want)
		}
	}

	if _, err := m.ChunkPlaintextSize(3); err == nil {
		t.Error("accepted a chunk past the end, want an error")
	}

	// An exact multiple has no short chunk at the end.
	exact := &Metadata{OriginalSize: 32 << 20, ChunkCount: 2, ChunkSize: 16 << 20}
	last, err := exact.ChunkPlaintextSize(1)
	if err != nil {
		t.Fatalf("ChunkPlaintextSize: %v", err)
	}
	if last != 16<<20 {
		t.Errorf("last chunk of an exact multiple is %d bytes, want %d", last, 16<<20)
	}
}
