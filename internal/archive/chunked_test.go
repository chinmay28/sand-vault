package archive

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chinmay28/sand-vault/internal/compress"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/sandfile"
	"github.com/chinmay28/sand-vault/internal/splitter"
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

	plan, err := PlanChunks(testArchiveID(t), "movie.mkv", sha256.Sum256(data), uint64(len(data)), chunkSize, SchemeDefault)
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
// format gives — §4.4's truth table, now per chunk.
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
		plan, err := PlanChunks(id, "f", hash, tc.size, tc.chunkSize, SchemeDefault)
		if err != nil {
			t.Fatalf("PlanChunks(%d, %d, SchemeDefault): %v", tc.size, tc.chunkSize, err)
		}
		if plan.ChunkCount != tc.want {
			t.Errorf("%d bytes in %d-byte chunks = %d chunks, want %d",
				tc.size, tc.chunkSize, plan.ChunkCount, tc.want)
		}
	}

	if _, err := PlanChunks(id, "f", hash, 100, 0, SchemeDefault); err == nil {
		t.Error("planned a zero chunk size, want an error")
	}
	if _, err := PlanChunks(id, "", hash, 100, 1024, SchemeDefault); err == nil {
		t.Error("planned an empty filename, want an error")
	}
	// A chunk size small enough to blow past the object-count ceiling is a
	// configuration error, not something to discover partway through an upload.
	if _, err := PlanChunks(id, "f", hash, uint64(MaxChunkCount)+1, 1, SchemeDefault); err == nil {
		t.Error("planned more chunks than the limit allows, want an error")
	}
}

func TestEncodeChunkRejectsWrongLength(t *testing.T) {
	plan, err := PlanChunks(testArchiveID(t), "f", [32]byte{}, 2048, 1024, SchemeDefault)
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

// A chunk's shards say which code they belong to, in the clear, because a
// reader has to know k and n before it can invert anything — and because the
// header is associated data, so a shard cannot be relabelled into another
// scheme without failing its tag.
func TestChunkedShardsCarryTheirScheme(t *testing.T) {
	for _, scheme := range []Scheme{SchemeDefault, SchemeWide, SchemeWider, SchemeForGroups(12)} {
		t.Run(scheme.String(), func(t *testing.T) {
			plan, err := PlanChunks(testArchiveID(t), "f.bin", [32]byte{}, 4096, 1024, scheme)
			if err != nil {
				t.Fatalf("PlanChunks: %v", err)
			}
			encoded, err := EncodeChunk(plan, 0, make([]byte, 1024), testMaster())
			if err != nil {
				t.Fatalf("EncodeChunk: %v", err)
			}
			if len(encoded.Parts) != scheme.Total {
				t.Fatalf("encoded %d shards, want %d", len(encoded.Parts), scheme.Total)
			}

			for i, blob := range encoded.Parts {
				part, err := sandfile.ReadPart(blob)
				if err != nil {
					t.Fatalf("ReadPart(%d): %v", i+1, err)
				}
				if part.Header.Version != sandfile.ErasureFormatVersion {
					t.Errorf("shard %d is version %d, want %d",
						i+1, part.Header.Version, sandfile.ErasureFormatVersion)
				}
				if int(part.Header.DataShards) != scheme.Data ||
					int(part.Header.TotalShards) != scheme.Total {
					t.Errorf("shard %d says %d-of-%d, want %s",
						i+1, part.Header.DataShards, part.Header.TotalShards, scheme)
				}
				if part.Header.PartNumber != uint8(i+1) {
					t.Errorf("shard at index %d numbers itself %d", i, part.Header.PartNumber)
				}
			}
		})
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

// encodeChunkV3 writes a chunk the way builds before schemes did: two halves
// and their XOR, sealed under format version 3.
//
// The encoder no longer produces this, which is exactly why the test needs its
// own copy. Every vault written before this change is full of these, and the
// promise is that they keep working untouched — so the shape of them has to be
// pinned somewhere that fails loudly if the reader drifts.
func encodeChunkV3(t *testing.T, plan ChunkPlan, index uint32, plaintext, master []byte) [][]byte {
	t.Helper()

	compressed, err := compress.Compress(plaintext)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	part1, part2, wasPadded := splitter.Split(compressed)
	part3, err := splitter.XOR(part1, part2)
	if err != nil {
		t.Fatalf("XOR: %v", err)
	}

	key, err := crypto.DeriveChunkKey(master, plan.ArchiveID, index)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}
	meta := &sandfile.Metadata{
		Filename:       plan.Filename,
		OriginalHash:   plan.OriginalHash,
		OriginalSize:   plan.OriginalSize,
		CompressedSize: uint64(len(compressed)),
		WasPadded:      wasPadded,
		ChunkCount:     plan.ChunkCount,
		ChunkSize:      plan.ChunkSize,
	}

	var out [][]byte
	for i, part := range [][]byte{part1, part2, part3} {
		nonce, err := crypto.GenerateNonce()
		if err != nil {
			t.Fatalf("nonce: %v", err)
		}
		blob, err := sandfile.Seal(&sandfile.Header{
			Version:    sandfile.ChunkedFormatVersion,
			PartNumber: uint8(i + 1),
			ArchiveID:  plan.ArchiveID,
			ChunkIndex: index,
			Nonce:      nonce,
		}, meta, part, key)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		out = append(out, blob)
	}
	return out
}

// A vault written before schemes existed is full of version 3 chunks, and they
// have to keep opening on any two of their three parts.
func TestVersionThreeChunksStillDecode(t *testing.T) {
	plaintext := make([]byte, 3000)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plan, err := PlanChunks(testArchiveID(t), "old.bin", sha256.Sum256(plaintext),
		uint64(len(plaintext)), 4096, SchemeDefault)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	parts := encodeChunkV3(t, plan, 0, plaintext, testMaster())

	for _, pair := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		decoded, err := DecodeChunk([][]byte{parts[pair[0]], parts[pair[1]]}, testMaster())
		if err != nil {
			t.Fatalf("parts %d and %d: DecodeChunk: %v", pair[0]+1, pair[1]+1, err)
		}
		if !bytes.Equal(decoded.Data, plaintext) {
			t.Errorf("parts %d and %d rebuilt the wrong bytes", pair[0]+1, pair[1]+1)
		}
	}
}

// Old and new shards of the same chunk index must never be mixed into one
// rebuild: they are different codes over different data, and a decoder that
// tried would produce plausible nonsense.
func TestChunksOfDifferentSchemesAreNotMixed(t *testing.T) {
	plaintext := make([]byte, 3000)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}
	id := testArchiveID(t)
	hash := sha256.Sum256(plaintext)

	oldPlan, err := PlanChunks(id, "old.bin", hash, uint64(len(plaintext)), 4096, SchemeDefault)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	old := encodeChunkV3(t, oldPlan, 0, plaintext, testMaster())

	fresh, err := EncodeChunk(oldPlan, 0, plaintext, testMaster())
	if err != nil {
		t.Fatalf("EncodeChunk: %v", err)
	}

	if _, err := DecodeChunk([][]byte{old[0], fresh.Parts[1]}, testMaster()); err == nil {
		t.Error("mixed a version 3 part with a version 4 shard, want a refusal")
	}
}

// The named schemes are conveniences, not definitions. If one of them ever
// stopped agreeing with the rule, prose and code would part company silently.
func TestNamedSchemesFollowTheRule(t *testing.T) {
	for groups, named := range map[int]Scheme{1: SchemeDefault, 2: SchemeWide, 3: SchemeWider} {
		if want := SchemeForGroups(groups); named != want {
			t.Errorf("%s is not %d group(s) of the rule, which is %s", named, groups, want)
		}
		if !named.Valid() {
			t.Errorf("%s does not validate", named)
		}
	}

	// And the rule holds across the family, all the way to the ceiling a
	// one-byte shard number sets.
	for accounts := AccountsPerGroup; accounts <= MaxAccounts; accounts += AccountsPerGroup {
		s, err := SchemeFor(accounts)
		if err != nil {
			t.Fatalf("SchemeFor(%d): %v", accounts, err)
		}
		if s.Total != accounts || s.Data*3 != s.Total*2 {
			t.Fatalf("SchemeFor(%d) = %s, want 2m-of-3m", accounts, s)
		}
		if s.Tolerance() != s.Groups() {
			t.Errorf("%s survives %d losses, want one per group (%d)",
				s, s.Tolerance(), s.Groups())
		}
	}
	if _, err := SchemeFor(MaxAccounts + AccountsPerGroup); err == nil {
		t.Error("a spread past the ceiling was accepted")
	}
}
