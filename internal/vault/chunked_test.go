package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
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

// chunkedVault is a test vault whose uploads are cut into small chunks, so a
// modest payload still exercises the multi-chunk paths.
func chunkedVault(t *testing.T, accounts int, chunkSize uint32) (*Vault, []string) {
	t.Helper()
	v, roots := newTestVault(t, accounts)
	v.mu.Lock()
	v.chunkSize = chunkSize
	v.mu.Unlock()
	return v, roots
}

// objectsUnder counts the stored objects across the folders standing in for
// accounts, ignoring the manifest backups that also live there.
func objectsUnder(t *testing.T, roots []string) int {
	t.Helper()
	count := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sand") && e.Name() != "manifest.sand" {
				count++
			}
		}
	}
	return count
}

func TestUploadStoresChunksAndFetchesThemBack(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := bytes.Repeat([]byte("a film is a great many bytes long "), 300) // ~10 KB
	entry, warnings, err := v.Upload(ctx, "/", "film.mkv", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v (%v)", err, warnings)
	}
	if !entry.Chunked() {
		t.Fatal("the upload was not stored in chunks")
	}
	if entry.ChunkCount < 2 {
		t.Fatalf("chunk count = %d, want several", entry.ChunkCount)
	}
	if entry.ChunkSize != 1024 {
		t.Errorf("chunk size = %d, want 1024", entry.ChunkSize)
	}

	// Three parts per chunk, each on its own account.
	if got, want := objectsUnder(t, roots), entry.ChunkCount*archive.PartCount; got != want {
		t.Errorf("stored %d objects, want %d", got, want)
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file did not come back as it went in")
	}
}

// Losing one account costs nothing on the read path: every chunk still has two
// parts, which is the whole point of the split.
func TestChunkedReadSurvivesAnAccountGoingDark(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := bytes.Repeat([]byte("still readable with one cloud gone "), 200)
	entry, _, err := v.Upload(ctx, "/", "resilient.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Wipe one account's objects entirely, as an outage or a deleted folder
	// would.
	dark := roots[0]
	files, err := os.ReadDir(dark)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(dark, f.Name())); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch with one account dark: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the file rebuilt from two accounts does not match")
	}
}

func TestDeleteErasesEveryChunk(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := bytes.Repeat([]byte("erase all of me "), 400)
	entry, _, err := v.Upload(ctx, "/", "doomed.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.ChunkCount < 2 {
		t.Fatalf("chunk count = %d, want several", entry.ChunkCount)
	}

	if _, err := v.Delete(ctx, entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Every chunk, not just the one the shard names.
	if left := objectsUnder(t, roots); left != 0 {
		t.Errorf("%d object(s) left behind after deleting a chunked file", left)
	}
}

func TestOverwriteErasesEveryChunkOfTheReplacedFile(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	first, _, err := v.Upload(ctx, "/", "notes.bin", bytes.Repeat([]byte("the first draft "), 400), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	second, _, err := v.Upload(ctx, "/", "notes.bin", []byte("a much shorter second draft"),
		UploadOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("Upload overwrite: %v", err)
	}

	if got, want := objectsUnder(t, roots), second.ChunkCount*archive.PartCount; got != want {
		t.Errorf("stored %d objects after the overwrite, want %d — the replaced file's chunks are still there",
			got, want)
	}
	if _, err := v.Entry(first.ID); err == nil {
		t.Error("the replaced entry is still in the index")
	}
}

// A vault written before chunking existed must keep working untouched.
func TestWholeFileEntriesAreStillReadable(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	payload := []byte("stored the way the old build stored things")
	placed, err := v.scatter(ctx, "legacy.txt", payload, nil, false)
	if err != nil {
		t.Fatalf("scatter: %v", err)
	}

	entry := &Entry{
		ID:        "legacy-entry",
		Dir:       "/",
		Name:      "legacy.txt",
		Size:      int64(len(payload)),
		Hash:      hex.EncodeToString(placed.originalHash[:]),
		ArchiveID: placed.archiveID,
		KeyID:     placed.keyID,
		Shards:    placed.shards,
	}
	if entry.Chunked() {
		t.Fatal("a whole-file entry should not report as chunked")
	}

	v.mu.Lock()
	v.manifest.add(entry)
	v.mu.Unlock()

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch of a whole-file entry: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the whole-file entry did not come back as it went in")
	}
}

func TestHealthReportsChunkSampling(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "sampled.bin", bytes.Repeat([]byte("health "), 2000), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	health, err := v.Health(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Recoverable {
		t.Error("a freshly stored file reports as unrecoverable")
	}
	if health.ChunkCount != entry.ChunkCount {
		t.Errorf("health reports %d chunks, want %d", health.ChunkCount, entry.ChunkCount)
	}
	if health.ChunksSampled == 0 || health.ChunksSampled > healthSampleLimit {
		t.Errorf("sampled %d chunks, want between 1 and %d", health.ChunksSampled, healthSampleLimit)
	}
	for _, s := range health.Shards {
		if !s.Present {
			t.Errorf("part %d reports missing: %s", s.Part, s.Error)
		}
	}
}

func TestSampleChunks(t *testing.T) {
	if got := sampleChunks(false, 0); len(got) != 1 || got[0] != 0 {
		t.Errorf("a whole-file entry sampled %v, want [0]", got)
	}
	if got := sampleChunks(true, 3); len(got) != 3 {
		t.Errorf("a 3-chunk file sampled %v, want all of them", got)
	}

	// A big file samples both ends, so a part missing from the tail is caught.
	got := sampleChunks(true, 1000)
	if len(got) != healthSampleLimit {
		t.Fatalf("sampled %d chunks, want %d", len(got), healthSampleLimit)
	}
	if got[0] != 0 {
		t.Errorf("sample starts at %d, want the first chunk", got[0])
	}
	if got[len(got)-1] != 999 {
		t.Errorf("sample ends at %d, want the last chunk", got[len(got)-1])
	}
}
