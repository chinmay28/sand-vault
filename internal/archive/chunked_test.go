package archive

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/sandfile"
	"github.com/google/uuid"
)

func testMaster() []byte { return bytes.Repeat([]byte{0x5A}, crypto.MasterKeySize) }

func testArchiveID(t *testing.T) [16]byte {
	t.Helper()
	var id [16]byte
	u := uuid.New()
	copy(id[:], u[:])
	return id
}

// encodeAll cuts data into chunks and encodes every one of them.
func encodeAll(t *testing.T, data []byte, chunkSize uint32) (ChunkPlan, []*EncodedChunk) {
	t.Helper()

	plan, err := PlanChunks(testArchiveID(t), "movie.mkv", sha256.Sum256(data), uint64(len(data)), chunkSize)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}

	chunks := make([]*EncodedChunk, 0, plan.ChunkCount)
	for index := uint32(0); index < plan.ChunkCount; index++ {
		size, err := plan.PlaintextSize(index)
		if err != nil {
			t.Fatalf("PlaintextSize(%d): %v", index, err)
		}
		offset := uint64(index) * uint64(plan.ChunkSize)
		encoded, err := EncodeChunk(plan, index, data[offset:offset+size], testMaster())
		if err != nil {
			t.Fatalf("EncodeChunk(%d): %v", index, err)
		}
		chunks = append(chunks, encoded)
	}
	return plan, chunks
}

func TestChunkedRoundTrip(t *testing.T) {
	data := make([]byte, 5000)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	_, chunks := encodeAll(t, data, 1024)
	if len(chunks) != 5 {
		t.Fatalf("5000 bytes in 1024-byte chunks made %d chunks, want 5", len(chunks))
	}

	var rebuilt []byte
	for _, chunk := range chunks {
		decoded, err := DecodeChunk([][]byte{chunk.Parts[0], chunk.Parts[1]}, testMaster())
		if err != nil {
			t.Fatalf("DecodeChunk(%d): %v", chunk.Index, err)
		}
		if decoded.Index != chunk.Index {
			t.Errorf("decoded index = %d, want %d", decoded.Index, chunk.Index)
		}
		rebuilt = append(rebuilt, decoded.Data...)
	}

	if !bytes.Equal(rebuilt, data) {
		t.Errorf("rebuilt %d bytes, want the original %d", len(rebuilt), len(data))
	}
}

// Any two of the three parts rebuild a chunk, the same guarantee the whole-file
// format gives — §4.3's truth table, now per chunk.
func TestChunkedAnyTwoParts(t *testing.T) {
	data := make([]byte, 3000)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plan, chunks := encodeAll(t, data, 4096)
	if plan.ChunkCount != 1 {
		t.Fatalf("expected a single chunk, got %d", plan.ChunkCount)
	}
	parts := chunks[0].Parts

	combos := map[string][][]byte{
		"1+2":   {parts[0], parts[1]},
		"1+3":   {parts[0], parts[2]},
		"2+3":   {parts[1], parts[2]},
		"1+2+3": {parts[0], parts[1], parts[2]},
	}
	for name, blobs := range combos {
		decoded, err := DecodeChunk(blobs, testMaster())
		if err != nil {
			t.Fatalf("DecodeChunk(%s): %v", name, err)
		}
		if !bytes.Equal(decoded.Data, data) {
			t.Errorf("%s rebuilt the wrong bytes", name)
		}
	}

	if _, err := DecodeChunk([][]byte{parts[0]}, testMaster()); err == nil {
		t.Error("rebuilt a chunk from one part, want an error")
	}
}

// Parts of two different chunks must not combine into anything, even though
// they share an archive ID and were sealed under the same master key.
func TestChunkedRejectsMixedChunks(t *testing.T) {
	data := make([]byte, 4096)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	_, chunks := encodeAll(t, data, 1024)

	if _, err := DecodeChunk([][]byte{chunks[0].Parts[0], chunks[1].Parts[1]}, testMaster()); err == nil {
		t.Error("combined parts of two different chunks, want an error")
	}
}

func TestChunkedRejectsWrongMasterKey(t *testing.T) {
	data := []byte("something worth sealing")
	_, chunks := encodeAll(t, data, 1024)

	other := testMaster()
	other[0] ^= 0xFF
	if _, err := DecodeChunk([][]byte{chunks[0].Parts[0], chunks[0].Parts[1]}, other); err == nil {
		t.Error("decoded a chunk under the wrong master key, want an error")
	}
}

// DecodeChunk is for chunked parts. A whole-file part carries no chunk index
// and derives its key from a password, so it has to be turned away rather than
// misread.
func TestChunkedRejectsWholeFileParts(t *testing.T) {
	encoded, err := EncodeBytes([]byte("whole file"), "notes.txt", "correct horse")
	if err != nil {
		t.Fatalf("EncodeBytes: %v", err)
	}
	if _, err := DecodeChunk([][]byte{encoded.Parts[0], encoded.Parts[1]}, testMaster()); err == nil {
		t.Error("decoded version 2 parts as chunks, want an error")
	}
}

func TestChunkedEmptyFile(t *testing.T) {
	plan, chunks := encodeAll(t, nil, DefaultChunkSize)
	if plan.ChunkCount != 1 {
		t.Fatalf("an empty file made %d chunks, want 1", plan.ChunkCount)
	}

	decoded, err := DecodeChunk([][]byte{chunks[0].Parts[0], chunks[0].Parts[2]}, testMaster())
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	if len(decoded.Data) != 0 {
		t.Errorf("rebuilt %d bytes from an empty file", len(decoded.Data))
	}
}

func TestChunkedExactMultiple(t *testing.T) {
	data := make([]byte, 2048)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	plan, chunks := encodeAll(t, data, 1024)
	if plan.ChunkCount != 2 {
		t.Fatalf("2048 bytes in 1024-byte chunks made %d chunks, want 2", plan.ChunkCount)
	}

	var rebuilt []byte
	for _, chunk := range chunks {
		decoded, err := DecodeChunk([][]byte{chunk.Parts[1], chunk.Parts[2]}, testMaster())
		if err != nil {
			t.Fatalf("DecodeChunk(%d): %v", chunk.Index, err)
		}
		rebuilt = append(rebuilt, decoded.Data...)
	}
	if !bytes.Equal(rebuilt, data) {
		t.Error("an exact multiple did not round-trip")
	}
}

func TestPlanChunksMath(t *testing.T) {
	id := [16]byte{}
	hash := [32]byte{}

	cases := []struct {
		size      uint64
		chunkSize uint32
		want      uint32
	}{
		{0, 1024, 1},    // an empty file is still one chunk
		{1, 1024, 1},    //
		{1024, 1024, 1}, // exactly full
		{1025, 1024, 2}, // one byte over
		{40 << 20, 16 << 20, 3},
	}
	for _, tc := range cases {
		plan, err := PlanChunks(id, "f", hash, tc.size, tc.chunkSize)
		if err != nil {
			t.Fatalf("PlanChunks(%d, %d): %v", tc.size, tc.chunkSize, err)
		}
		if plan.ChunkCount != tc.want {
			t.Errorf("%d bytes in %d-byte chunks = %d chunks, want %d",
				tc.size, tc.chunkSize, plan.ChunkCount, tc.want)
		}
	}

	if _, err := PlanChunks(id, "f", hash, 100, 0); err == nil {
		t.Error("planned a zero chunk size, want an error")
	}
	if _, err := PlanChunks(id, "", hash, 100, 1024); err == nil {
		t.Error("planned an empty filename, want an error")
	}
	// A chunk size small enough to blow past the object-count ceiling is a
	// configuration error, not something to discover partway through an upload.
	if _, err := PlanChunks(id, "f", hash, uint64(MaxChunkCount)+1, 1); err == nil {
		t.Error("planned more chunks than the limit allows, want an error")
	}
}

func TestEncodeChunkRejectsWrongLength(t *testing.T) {
	plan, err := PlanChunks(testArchiveID(t), "f", [32]byte{}, 2048, 1024)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}

	// Chunk 0 is a full chunk; handing it a short slice would silently shorten
	// the file, so it has to be refused.
	if _, err := EncodeChunk(plan, 0, make([]byte, 512), testMaster()); err == nil {
		t.Error("encoded a short middle chunk, want an error")
	}
	if _, err := EncodeChunk(plan, 2, make([]byte, 1024), testMaster()); err == nil {
		t.Error("encoded a chunk past the end of the plan, want an error")
	}
}

func TestEncodeChunkCarriesArchiveDescription(t *testing.T) {
	data := make([]byte, 3000)
	plan, chunks := encodeAll(t, data, 1024)

	// Every chunk repeats the whole archive's description, so any two parts of
	// any one chunk still say what file they came from.
	for _, chunk := range chunks {
		decoded, err := DecodeChunk([][]byte{chunk.Parts[0], chunk.Parts[1]}, testMaster())
		if err != nil {
			t.Fatalf("DecodeChunk(%d): %v", chunk.Index, err)
		}
		if decoded.Meta.Filename != plan.Filename {
			t.Errorf("chunk %d names %q, want %q", chunk.Index, decoded.Meta.Filename, plan.Filename)
		}
		if decoded.Meta.OriginalSize != plan.OriginalSize {
			t.Errorf("chunk %d reports size %d, want %d",
				chunk.Index, decoded.Meta.OriginalSize, plan.OriginalSize)
		}
		if decoded.Meta.ChunkCount != plan.ChunkCount || decoded.Meta.ChunkSize != plan.ChunkSize {
			t.Errorf("chunk %d reports %d chunks of %d, want %d of %d", chunk.Index,
				decoded.Meta.ChunkCount, decoded.Meta.ChunkSize, plan.ChunkCount, plan.ChunkSize)
		}
		if decoded.Meta.OriginalHash != plan.OriginalHash {
			t.Errorf("chunk %d carries the wrong whole-file hash", chunk.Index)
		}
	}
}

func TestChunkedPartsAreVersionThree(t *testing.T) {
	_, chunks := encodeAll(t, []byte("data"), 1024)

	for i, blob := range chunks[0].Parts {
		part, err := sandfile.ReadPart(blob)
		if err != nil {
			t.Fatalf("ReadPart(%d): %v", i+1, err)
		}
		if part.Header.Version != sandfile.ChunkedFormatVersion {
			t.Errorf("part %d is version %d, want %d",
				i+1, part.Header.Version, sandfile.ChunkedFormatVersion)
		}
		if part.Header.PartNumber != uint8(i+1) {
			t.Errorf("part at index %d numbers itself %d", i, part.Header.PartNumber)
		}
	}
}

func TestRestoreChunkedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 5000)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	_, chunks := encodeAll(t, data, 1024)

	// Two parts of each chunk, the way an offline restore finds them: enough to
	// rebuild, and deliberately not the same two every time.
	var paths []string
	for _, chunk := range chunks {
		for _, part := range []int{int(chunk.Index) % 3, (int(chunk.Index) + 1) % 3} {
			path := filepath.Join(dir, fmt.Sprintf("c%d-p%d.sand", chunk.Index, part+1))
			if err := os.WriteFile(path, chunk.Parts[part], 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			paths = append(paths, path)
		}
	}

	out := t.TempDir()
	restored, err := RestoreChunked(paths, testMaster(), out)
	if err != nil {
		t.Fatalf("RestoreChunked: %v", err)
	}
	got, err := os.ReadFile(restored)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("the offline restore does not match the original")
	}
}

// A chunk missing from the middle must be refused, not spliced over.
func TestRestoreChunkedRefusesAGap(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 4096)
	_, chunks := encodeAll(t, data, 1024)

	var paths []string
	for _, chunk := range chunks {
		if chunk.Index == 2 {
			continue // the gap
		}
		for part := 0; part < 2; part++ {
			path := filepath.Join(dir, fmt.Sprintf("c%d-p%d.sand", chunk.Index, part+1))
			if err := os.WriteFile(path, chunk.Parts[part], 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			paths = append(paths, path)
		}
	}

	if _, err := RestoreChunked(paths, testMaster(), t.TempDir()); err == nil {
		t.Error("restored an archive with a chunk missing, want an error")
	}
}

func TestRestoreChunkedRejectsWholeFileParts(t *testing.T) {
	dir := t.TempDir()
	encoded, err := EncodeBytes([]byte("whole file"), "notes.txt", "a password")
	if err != nil {
		t.Fatalf("EncodeBytes: %v", err)
	}

	var paths []string
	for i := 0; i < 2; i++ {
		path := filepath.Join(dir, fmt.Sprintf("p%d.sand", i+1))
		if err := os.WriteFile(path, encoded.Parts[i], 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		paths = append(paths, path)
	}

	if _, err := RestoreChunked(paths, testMaster(), t.TempDir()); err == nil {
		t.Error("restored version 2 parts as chunks, want an error")
	}

	chunked, err := PartsAreChunked(paths[0])
	if err != nil {
		t.Fatalf("PartsAreChunked: %v", err)
	}
	if chunked {
		t.Error("a whole-file part reports as chunked")
	}
}
