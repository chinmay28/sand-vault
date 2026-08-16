package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// storeWhole puts a file on the accounts in the pre-chunking format and indexes
// it, which is what every file uploaded before chunking existed looks like.
func storeWhole(t *testing.T, v *Vault, name string, payload []byte) *Entry {
	t.Helper()

	placed, err := v.scatter(context.Background(), name, payload, nil, false)
	if err != nil {
		t.Fatalf("scatter: %v", err)
	}
	entry := &Entry{
		ID: "whole-" + name, Dir: "/", Name: name,
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

// readSizeRecorder remembers the largest single read made through it.
//
// This is how "it streams" is asserted here rather than by watching the heap.
// Sampling HeapAlloc cannot answer the question: it counts garbage that has not
// been swept yet as well as memory genuinely held, and the garbage grows with
// the size of the file either way — so a buffering implementation and a
// streaming one look alike. How much the scatter ever asks for at once does not
// have that problem, and it is the property that actually matters.
type readSizeRecorder struct {
	src io.ReaderAt

	mu      sync.Mutex
	largest int
	reads   int
}

func (r *readSizeRecorder) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	if len(p) > r.largest {
		r.largest = len(p)
	}
	r.reads++
	r.mu.Unlock()
	return r.src.ReadAt(p, off)
}

func (r *readSizeRecorder) stats() (largest, reads int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.largest, r.reads
}

// spoolsOnDisk counts the rebuilt copies currently sitting beside the vault.
func spoolsOnDisk(t *testing.T, v *Vault) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(v.path), ".sand-read-*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	return len(matches)
}

// The point of the whole exercise: several readers of one whole-stored file
// rebuild it once, between them, rather than once each.
//
// A player opens a fresh connection on every seek, so "once each" is what used
// to put several copies of a film in memory at the same time.
func TestConcurrentReadersShareOneRebuild(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	v.SetRechunkOnRead(false) // otherwise the first read converts it out from under the test

	payload := readerPayload(64 << 10)
	entry := storeWhole(t, v, "film.bin", payload)

	const readers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var peak int
	errs := make(chan error, readers)
	release := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
			if err != nil {
				errs <- err
				return
			}
			defer body.Close()

			got, err := io.ReadAll(body)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("reader saw %d bytes, want %d", len(got), len(payload))
				return
			}

			// Hold the read open so every reader overlaps every other.
			mu.Lock()
			if n := spoolsOnDisk(t, v); n > peak {
				peak = n
			}
			mu.Unlock()
			<-release
		}()
	}

	// Let them all get there, then let them all go.
	for len(errs) == 0 && spoolsOnDisk(t, v) == 0 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("reader: %v", err)
	}
	if peak > 1 {
		t.Errorf("%d readers of one file made %d rebuilt copies; they are meant to share one", readers, peak)
	}
	if left := spoolsOnDisk(t, v); left != 0 {
		t.Errorf("%d rebuilt copies survived the last reader closing", left)
	}
}

// Closing is what lets go of the copy, and the last close is what removes it.
func TestRebuiltCopyOutlivesOnlyItsReaders(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	v.SetRechunkOnRead(false)

	payload := readerPayload(8 << 10)
	entry := storeWhole(t, v, "film.bin", payload)

	first, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	second, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}

	if got := spoolsOnDisk(t, v); got != 1 {
		t.Fatalf("two readers made %d rebuilt copies, want 1", got)
	}

	first.Close()
	if got := spoolsOnDisk(t, v); got != 1 {
		t.Errorf("the copy went while a reader still had it open (%d on disk)", got)
	}

	// The surviving reader can still read all of it.
	if _, err := second.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(second)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the second reader could not finish: %v", err)
	}

	second.Close()
	if got := spoolsOnDisk(t, v); got != 0 {
		t.Errorf("%d rebuilt copies survived the last close", got)
	}
}

// Two readers must not share a file offset.
func TestRebuiltCopyGivesEachReaderItsOwnOffset(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	v.SetRechunkOnRead(false)

	payload := readerPayload(4 << 10)
	entry := storeWhole(t, v, "film.bin", payload)

	first, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	defer first.Close()
	second, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	defer second.Close()

	if _, err := first.Seek(1000, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(second, head); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(head, payload[:16]) {
		t.Error("one reader's seek moved the other's offset")
	}
}

// Locking takes the keys away, so the decrypted copies on disk go with them —
// the same thing that happens to cached chunks and thumbnails.
func TestLockingRemovesRebuiltCopies(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	v.SetRechunkOnRead(false)

	payload := readerPayload(8 << 10)
	entry := storeWhole(t, v, "film.bin", payload)

	body, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	if got := spoolsOnDisk(t, v); got != 1 {
		t.Fatalf("expected one rebuilt copy, got %d", got)
	}

	v.Lock()

	if got := spoolsOnDisk(t, v); got != 0 {
		t.Errorf("%d decrypted copies survived locking the vault", got)
	}
	// The reader holding it open is not yanked out from under; it just cannot
	// be joined by anyone new.
	body.Close()
}

// A process killed holding a rebuilt copy leaves it on disk. Opening the vault
// is where that gets cleaned up, because nothing else knows the file is dead.
func TestOpeningTheVaultSweepsAbandonedCopies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.sand")

	stale := filepath.Join(dir, ".sand-read-abandoned")
	if err := os.WriteFile(stale, []byte("plaintext from a process that died"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an abandoned rebuilt copy survived opening the vault")
	}
}

// The memory claim, made directly: converting a whole-stored file must not
// allocate on the order of the file's size.
//
// This is the failure that took a Raspberry Pi down — migrateFile gathered the
// whole film into a buffer and held it across the entire scatter, so converting
// a file larger than free memory got the process killed. Being killed meant the
// conversion never committed, so the next read queued it again, and the same
// film took the machine down every time it was played.
func TestConvertingAWholeFileReadsOneChunkAtATime(t *testing.T) {
	const chunkSize = 64 << 10
	const size = 8 << 20 // 128 chunks, so a buffering read would be unmistakable

	v, _ := chunkedVault(t, 3, chunkSize)
	ctx := context.Background()

	payload := readerPayload(size)
	entry := storeWhole(t, v, "film.bin", payload)

	source, err := v.openForMigration(ctx, entry)
	if err != nil {
		t.Fatalf("openForMigration: %v", err)
	}
	defer source.close()

	// The old format has no seams, so it is rebuilt in full — but onto disk,
	// which is the whole point.
	if spoolsOnDisk(t, v) != 1 {
		t.Fatal("a whole-stored file was not rebuilt onto disk")
	}
	if source.size != size {
		t.Fatalf("source reports %d bytes, want %d", source.size, size)
	}

	recorder := &readSizeRecorder{src: source.src}
	placed, err := v.scatterStream(ctx, "film.bin", recorder, source.size, source.hash, nil, false, chunkSize)
	if err != nil {
		t.Fatalf("scatterStream: %v", err)
	}
	if placed.chunkCount != size/chunkSize {
		t.Errorf("scattered %d chunks, want %d", placed.chunkCount, size/chunkSize)
	}

	largest, reads := recorder.stats()
	t.Logf("scattering %d bytes made %d reads, largest %d bytes", size, reads, largest)
	if largest > chunkSize {
		t.Errorf("the scatter asked for %d bytes at once from a %d-byte file; "+
			"it is meant to read a chunk (%d) at a time, not to buffer",
			largest, size, chunkSize)
	}
	if reads < size/chunkSize {
		t.Errorf("only %d reads for %d chunks — the file was not walked chunk by chunk",
			reads, size/chunkSize)
	}
}

// End to end: the conversion the background worker actually runs leaves the file
// chunked, byte for byte, and cleans up after itself.
func TestConvertingAWholeFileLeavesItIntact(t *testing.T) {
	v, _ := chunkedVault(t, 3, 64<<10)
	ctx := context.Background()

	payload := readerPayload(2 << 20)
	entry := storeWhole(t, v, "film.bin", payload)

	if _, _, _, err := v.migrateFile(ctx, entry.ID); err != nil {
		t.Fatalf("migrateFile: %v", err)
	}

	converted, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if !converted.Chunked() {
		t.Fatal("the file was not converted to chunks")
	}
	if converted.Hash != entry.Hash {
		t.Error("the conversion changed the file's hash")
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after conversion: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the converted file did not match the original")
	}
	if left := spoolsOnDisk(t, v); left != 0 {
		t.Errorf("%d rebuilt copies survived the conversion", left)
	}
}

// A re-key of a file that is already chunked should not stage anything at all:
// it can be read where it lies, chunk by chunk.
func TestRekeyingAChunkedFileStagesNothing(t *testing.T) {
	v, _ := chunkedVault(t, 3, 64<<10)
	ctx := context.Background()

	payload := readerPayload(2 << 20)
	entry, _, err := v.Upload(ctx, "/", "film.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !entry.Chunked() {
		t.Fatal("the fixture was meant to be chunked")
	}

	source, err := v.openForMigration(ctx, entry)
	if err != nil {
		t.Fatalf("openForMigration: %v", err)
	}
	defer source.close()

	// Nothing staged: an already-chunked file is read where it lies.
	if left := spoolsOnDisk(t, v); left != 0 {
		t.Errorf("re-keying a chunked file wrote %d copies to disk; it should read them in place", left)
	}

	recorder := &readSizeRecorder{src: source.src}
	if _, err := v.scatterStream(ctx, "film.bin", recorder, source.size, source.hash, nil, false, 64<<10); err != nil {
		t.Fatalf("scatterStream: %v", err)
	}
	if largest, _ := recorder.stats(); largest > 64<<10 {
		t.Errorf("the scatter asked for %d bytes at once, want at most one chunk", largest)
	}
	source.close()

	if _, _, _, err := v.migrateFile(ctx, entry.ID); err != nil {
		t.Fatalf("migrateFile: %v", err)
	}

	got, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the re-keyed file did not match the original")
	}
}

// Re-encrypting reads the file end to end, which is the one moment its
// whole-file hash can be checked — so it still is, and a mismatch stops the new
// parts being committed over parts that are known good.
func TestRekeyingVerifiesTheWholeFileHash(t *testing.T) {
	v, _ := chunkedVault(t, 3, 64<<10)
	ctx := context.Background()

	payload := readerPayload(256 << 10)
	entry, _, err := v.Upload(ctx, "/", "film.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Claim a hash the bytes do not have, which is what corruption would look
	// like from the index's side.
	v.mu.Lock()
	indexed := v.manifest.ByID(entry.ID)
	original := indexed.Hash
	indexed.Hash = hex.EncodeToString(bytes.Repeat([]byte{0xAA}, 32))
	v.mu.Unlock()

	_, _, _, err = v.migrateFile(ctx, entry.ID)
	if err == nil {
		t.Fatal("re-encrypting a file whose bytes do not match its hash was allowed")
	}

	// The index must still point at the parts it did before.
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.ArchiveID != entry.ArchiveID {
		t.Error("a failed verification still moved the index onto the new parts")
	}

	v.mu.Lock()
	v.manifest.ByID(entry.ID).Hash = original
	v.mu.Unlock()
}

// The switch has to hold on the spooled read path too, not just on Fetch —
// that is the path a player actually takes, and the one that stages a copy on
// disk it would rather not stage.
//
// Until now the switch existed but nothing could reach it; it is a serve flag
// now, for a vault on a metered connection (converting costs a full download
// and re-upload of whatever is read) or a machine short of the disk to stage a
// large file while it converts.
func TestRechunkOnReadAppliesToTheSpooledReadPath(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(8 << 10)
	entry := storeWhole(t, v, "film.bin", payload)

	v.SetRechunkOnRead(false)

	body, _, err := v.OpenReadSeeker(ctx, entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("the read did not return the file")
	}

	v.AwaitRechunk()

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.Chunked() {
		t.Error("the file was converted despite the conversion being turned off")
	}

	// And back on again, the same read converts it.
	v.SetRechunkOnRead(true)
	body, _, err = v.OpenReadSeeker(ctx, entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	io.Copy(io.Discard, body)
	body.Close()
	v.AwaitRechunk()

	converted, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if !converted.Chunked() {
		t.Error("turning the conversion back on did not convert the file")
	}
}
