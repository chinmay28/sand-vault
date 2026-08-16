package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// readerPayload is deliberately not repetitive, so a read landing on the wrong
// chunk produces bytes that visibly differ rather than bytes that happen to
// match their neighbours.
func readerPayload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/251)
	}
	return out
}

func TestChunkedReaderReadsAtOffsets(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(5000)
	entry, _, err := v.Upload(ctx, "/", "film.mkv", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	r, err := v.OpenReader(entry.ID)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if r.Size() != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", r.Size(), len(payload))
	}

	cases := []struct{ off, n int64 }{
		{0, 10},      // the head, which is what a player reads first
		{0, 1024},    // exactly one chunk
		{1020, 10},   // straddling a chunk boundary
		{1024, 1},    // the first byte of the second chunk
		{2500, 1500}, // spanning two boundaries
		{4990, 10},   // the tail
		{0, 5000},    // the whole file through ReadAt
	}
	for _, tc := range cases {
		buf := make([]byte, tc.n)
		n, err := r.ReadAt(buf, tc.off)
		if err != nil {
			t.Errorf("ReadAt(%d, %d): %v", tc.off, tc.n, err)
			continue
		}
		if int64(n) != tc.n {
			t.Errorf("ReadAt(%d, %d) read %d bytes", tc.off, tc.n, n)
			continue
		}
		if !bytes.Equal(buf, payload[tc.off:tc.off+tc.n]) {
			t.Errorf("ReadAt(%d, %d) returned the wrong bytes", tc.off, tc.n)
		}
	}
}

func TestChunkedReaderReadAtSemantics(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(2500)
	entry, _, err := v.Upload(ctx, "/", "edges.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	r, err := v.OpenReader(entry.ID)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	// Reading past the end is io.EOF, and a read that runs into the end is a
	// short read *with* io.EOF — that is the io.ReaderAt contract.
	if _, err := r.ReadAt(make([]byte, 4), 2500); !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %v, want io.EOF", err)
	}
	buf := make([]byte, 100)
	n, err := r.ReadAt(buf, 2450)
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt running off the end = %v, want io.EOF", err)
	}
	if n != 50 {
		t.Errorf("read %d bytes off the end, want 50", n)
	}
	if !bytes.Equal(buf[:n], payload[2450:]) {
		t.Error("the short read at the end returned the wrong bytes")
	}

	if n, err := r.ReadAt(nil, 0); n != 0 || err != nil {
		t.Errorf("ReadAt with an empty buffer = %d, %v", n, err)
	}
	if _, err := r.ReadAt(make([]byte, 4), -1); err == nil {
		t.Error("ReadAt accepted a negative offset")
	}
}

// The point of the format: a small read fetches the chunk it lands in and no
// more. The cache is the visible evidence of what was actually gathered.
func TestChunkedReaderFetchesOnlyTheChunksItTouches(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "big.bin", readerPayload(20000), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.ChunkCount < 10 {
		t.Fatalf("chunk count = %d, want a file worth seeking in", entry.ChunkCount)
	}
	v.chunks.clear()

	r, err := v.OpenReader(entry.ID)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if _, err := r.ReadAt(make([]byte, 16), 10240); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	v.chunks.mu.Lock()
	fetched := len(v.chunks.items)
	v.chunks.mu.Unlock()
	if fetched != 1 {
		t.Errorf("a 16-byte read gathered %d chunks, want 1 of the %d", fetched, entry.ChunkCount)
	}
}

func TestChunkedReaderIsSafeConcurrently(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(8000)
	entry, _, err := v.Upload(ctx, "/", "parallel.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	r, err := v.OpenReader(entry.ID)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	// Several readers on the same chunk and on different ones at once, which is
	// how a player with parallel connections behaves.
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64((i % 8) * 1000)
			buf := make([]byte, 500)
			if _, err := r.ReadAt(buf, off); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(buf, payload[off:off+500]) {
				errs <- errors.New("concurrent read returned the wrong bytes")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSectionReaderReadsTheWholeFile(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(4500)
	entry, _, err := v.Upload(ctx, "/", "seekable.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	r, err := v.OpenReader(entry.ID)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	got, err := io.ReadAll(r.SectionReader())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("reading the section reader end to end does not match")
	}

	// And a range out of the middle, which is what an HTTP range request
	// becomes once ServeContent has seeked.
	section := r.SectionReader()
	if _, err := section.Seek(2000, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 300)
	if _, err := io.ReadFull(section, buf); err != nil {
		t.Fatalf("ReadFull after seek: %v", err)
	}
	if !bytes.Equal(buf, payload[2000:2300]) {
		t.Error("the seeked range returned the wrong bytes")
	}
}

func TestOpenReaderRefusesAWholeFileEntry(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	placed, err := v.scatter(ctx, "legacy.txt", []byte("stored whole"), nil, false)
	if err != nil {
		t.Fatalf("scatter: %v", err)
	}
	entry := &Entry{
		ID: "whole", Dir: "/", Name: "legacy.txt",
		Size: 12, ArchiveID: placed.archiveID, KeyID: placed.keyID, Shards: placed.shards,
	}
	v.mu.Lock()
	v.manifest.add(entry)
	v.mu.Unlock()

	if _, err := v.OpenReader(entry.ID); err == nil {
		t.Error("opened a reader on a file stored whole, which cannot seek")
	}
}

func TestChunkCacheIsDroppedOnLock(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, "/", "cached.bin", readerPayload(5000), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	r, err := v.OpenReader(entry.ID)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if _, err := r.ReadAt(make([]byte, 100), 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	v.chunks.mu.Lock()
	cached := len(v.chunks.items)
	v.chunks.mu.Unlock()
	if cached == 0 {
		t.Fatal("nothing was cached, so the test proves nothing")
	}

	v.Lock()

	v.chunks.mu.Lock()
	after := len(v.chunks.items)
	v.chunks.mu.Unlock()
	if after != 0 {
		t.Errorf("%d chunk(s) of plaintext survived locking the vault", after)
	}
	// And the reader stops answering rather than carrying on from a key it
	// captured when it was opened.
	if _, err := r.ReadAt(make([]byte, 100), 2000); !errors.Is(err, ErrLocked) {
		t.Errorf("reading a locked vault = %v, want ErrLocked", err)
	}
}

func TestChunkCacheEvictsByBytes(t *testing.T) {
	c := newChunkCache(100)
	c.put("a", make([]byte, 60))
	c.put("b", make([]byte, 30))
	if _, ok := c.get("a"); !ok {
		t.Fatal("a was evicted early")
	}

	// Touching a makes b the least recently used, so b is what goes.
	c.put("c", make([]byte, 30))
	if _, ok := c.get("a"); !ok {
		t.Error("the most recently used entry was evicted")
	}
	if _, ok := c.get("b"); ok {
		t.Error("the least recently used entry survived")
	}
	if c.used > c.limit {
		t.Errorf("cache holds %d bytes, over its %d limit", c.used, c.limit)
	}

	// Something bigger than the whole budget is simply not cached.
	c.put("huge", make([]byte, 200))
	if _, ok := c.get("huge"); ok {
		t.Error("cached an entry larger than the whole budget")
	}
}

func TestUploadStreamRoundTrip(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(9000)
	entry, warnings, err := v.UploadStream(ctx, "/", "streamed.bin", bytes.NewReader(payload), UploadOptions{})
	if err != nil {
		t.Fatalf("UploadStream: %v (%v)", err, warnings)
	}
	if !entry.Chunked() {
		t.Fatal("a streamed upload was not stored in chunks")
	}
	if entry.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", entry.Size, len(payload))
	}
	if got, want := objectsUnder(t, roots), entry.ChunkCount*3; got != want {
		t.Errorf("stored %d objects, want %d", got, want)
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the streamed file did not come back as it went in")
	}

	// A streamed upload and an in-memory one must agree about the hash, since
	// both record it as the file's identity.
	inMemory, _, err := v.Upload(ctx, "/", "in-memory.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.Hash != inMemory.Hash {
		t.Errorf("streamed hash %s differs from the in-memory %s", entry.Hash, inMemory.Hash)
	}
}

func TestUploadStreamLeavesNoSpoolBehind(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	if _, _, err := v.UploadStream(ctx, "/", "tidy.bin", bytes.NewReader(readerPayload(4000)), UploadOptions{}); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
	// And on the failure path too: no accounts means the scatter cannot commit.
	failing, _ := newTestVault(t, 0)
	_, _, _ = failing.UploadStream(ctx, "/", "doomed.bin", bytes.NewReader([]byte("x")), UploadOptions{})

	for _, dir := range []string{filepath.Dir(v.Path()), filepath.Dir(failing.Path())} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".sand-upload-") {
				t.Errorf("a plaintext spool file was left behind at %s", filepath.Join(dir, e.Name()))
			}
		}
	}
}

// A pre-chunking file is left exactly as it is by a read. Converting one costs a
// download and a re-upload of the whole thing, which is not something a read
// should start on your behalf — see convert.go.
func TestReadingAWholeFileEntryLeavesItAlone(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(4000)
	entry := addWholeEntry(t, v, "legacy", "legacy.bin", payload)

	originalKeys := make([]string, 0, len(entry.Shards))
	for _, s := range entry.Shards {
		originalKeys = append(originalKeys, s.Key)
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the whole-file read did not match")
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.Chunked() {
		t.Error("a read converted the file; conversion is meant to be asked for")
	}
	for _, key := range originalKeys {
		if full := shardPath(roots, key); full == "" {
			t.Errorf("a read erased the file's original part %s", key)
		}
	}
}

// addWholeEntry stores a file in the pre-chunking format and indexes it, which
// is what every file uploaded before chunking existed looks like.
func addWholeEntry(t *testing.T, v *Vault, id, name string, payload []byte) *Entry {
	t.Helper()

	placed, err := v.scatter(context.Background(), name, payload, nil, false)
	if err != nil {
		t.Fatalf("scatter: %v", err)
	}
	entry := &Entry{
		ID: id, Dir: "/", Name: name,
		Size:      int64(len(payload)),
		Hash:      hex.EncodeToString(placed.originalHash[:]),
		ArchiveID: placed.archiveID,
		KeyID:     placed.keyID,
		Shards:    placed.shards,
	}
	v.mu.Lock()
	v.manifest.add(entry)
	v.mu.Unlock()

	if entry.Chunked() {
		t.Fatal("the fixture was meant to be stored whole")
	}
	return entry
}

// OpenReadSeeker is the random-access door, and a pre-chunking file cannot be
// opened through it — that format has no seams, so answering for a byte in the
// middle means rebuilding all of it. It says so rather than doing it.
func TestOpenReadSeekerRefusesAPreChunkingFile(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	chunkedPayload := readerPayload(5000)
	chunked, _, err := v.Upload(ctx, "/", "chunked.bin", chunkedPayload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	wholePayload := readerPayload(3000)
	placed, err := v.scatter(ctx, "whole.bin", wholePayload, nil, false)
	if err != nil {
		t.Fatalf("scatter: %v", err)
	}
	whole := &Entry{
		ID: "whole", Dir: "/", Name: "whole.bin",
		Size:      int64(len(wholePayload)),
		Hash:      hex.EncodeToString(placed.originalHash[:]),
		ArchiveID: placed.archiveID,
		KeyID:     placed.keyID,
		Shards:    placed.shards,
	}
	v.mu.Lock()
	v.manifest.add(whole)
	v.mu.Unlock()

	// The chunked one opens, reads end to end, and seeks into the middle —
	// which is what a range request becomes.
	body, entry, err := v.OpenReadSeeker(ctx, chunked.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker on a chunked file: %v", err)
	}
	if entry.Size != int64(len(chunkedPayload)) {
		t.Errorf("size = %d, want %d", entry.Size, len(chunkedPayload))
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, chunkedPayload) {
		t.Error("read the wrong bytes end to end")
	}
	if _, err := body.Seek(1200, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 300)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatalf("ReadFull after seek: %v", err)
	}
	if !bytes.Equal(buf, chunkedPayload[1200:1500]) {
		t.Error("the seek landed in the wrong place")
	}

	// The pre-chunking one is refused, and the refusal says which answer it
	// wants: convert, not retry.
	if _, _, err := v.OpenReadSeeker(ctx, whole.ID); err == nil {
		t.Fatal("a pre-chunking file was opened for random access")
	} else if !errors.Is(err, ErrNeedsConversion) {
		t.Errorf("refusal was %v, want ErrNeedsConversion so callers can offer to convert", err)
	} else if !strings.Contains(err.Error(), "whole.bin") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}
