package vault

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestEntryChunked(t *testing.T) {
	chunked := &Entry{Size: 40 << 20, ChunkSize: 16 << 20, ChunkCount: 3}
	if !chunked.Chunked() {
		t.Error("an entry with a chunk size and count does not report as chunked")
	}

	whole := &Entry{Size: 40 << 20}
	if whole.Chunked() {
		t.Error("an entry stored whole reports as chunked")
	}
}

func TestEntryChunkIndexAt(t *testing.T) {
	e := &Entry{Size: 40 << 20, ChunkSize: 16 << 20, ChunkCount: 3}

	for offset, want := range map[int64]int{
		0:              0,
		(16 << 20) - 1: 0,
		16 << 20:       1,
		32 << 20:       2,
		(40 << 20) - 1: 2,
	} {
		if got := e.ChunkIndexAt(offset); got != want {
			t.Errorf("offset %d is in chunk %d, want %d", offset, got, want)
		}
	}

	// A file stored whole is one chunk, and asking must not divide by zero.
	whole := &Entry{Size: 40 << 20}
	if got := whole.ChunkIndexAt(1 << 20); got != 0 {
		t.Errorf("a whole-file entry put offset in chunk %d, want 0", got)
	}
}

// The chunking fields are absent from an entry stored whole, so upgrading does
// not rewrite every record in the index.
func TestEntryChunkFieldsOmittedWhenWhole(t *testing.T) {
	encoded, err := json.Marshal(&Entry{ID: "abc", Name: "notes.txt"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{"chunk_size", "chunk_count"} {
		if strings.Contains(string(encoded), field) {
			t.Errorf("a whole-file entry serialized %s: %s", field, encoded)
		}
	}

	encoded, err = json.Marshal(&Entry{ID: "abc", ChunkSize: 16 << 20, ChunkCount: 3})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{"chunk_size", "chunk_count"} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("a chunked entry did not serialize %s: %s", field, encoded)
		}
	}
}

// Listing an account lexically has to return a file's chunks in order: that is
// the order the recovery path walks them in.
func TestChunkShardKeySortsByChunk(t *testing.T) {
	keys := []string{
		ChunkShardKey("archive", 10, 1),
		ChunkShardKey("archive", 2, 1),
		ChunkShardKey("archive", 100, 1),
		ChunkShardKey("archive", 0, 1),
	}
	sort.Strings(keys)

	want := []string{
		ChunkShardKey("archive", 0, 1),
		ChunkShardKey("archive", 2, 1),
		ChunkShardKey("archive", 10, 1),
		ChunkShardKey("archive", 100, 1),
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("sorted keys = %v, want %v", keys, want)
		}
	}
}

func TestChunkShardKeyIsDistinct(t *testing.T) {
	// A chunk's key must differ from every other chunk's and every other
	// part's, and must never collide with a whole-file key.
	seen := map[string]bool{ShardKey("archive", 1): true}
	for chunk := 0; chunk < 4; chunk++ {
		for part := 1; part <= 3; part++ {
			key := ChunkShardKey("archive", chunk, part)
			if seen[key] {
				t.Errorf("duplicate object key %q", key)
			}
			seen[key] = true
		}
	}
}
