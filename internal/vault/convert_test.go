package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// convertPeak reports the most heap live at any moment during work.
//
// The collector is turned up for the duration, because HeapAlloc counts garbage
// that has not been swept as well as memory genuinely held — at the default GOGC
// a streaming implementation and a buffering one are indistinguishable.
func convertPeak(work func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()

	stop, peaked := make(chan struct{}), make(chan uint64, 1)
	go func() {
		var stats runtime.MemStats
		var peak uint64
		for {
			select {
			case <-stop:
				peaked <- peak
				return
			default:
			}
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > peak {
				peak = stats.HeapAlloc
			}
		}
	}()

	work()
	close(stop)
	return <-peaked
}

func convertSpoolsOnDisk(t *testing.T, v *Vault) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(v.path), ".sand-convert-*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	return len(matches)
}

// The conversion has to fit in memory, or the button that offers it is just a
// politer way to reboot the machine.
//
// What matters is the coefficient, not the total. A conversion has a fixed cost
// that does not care how big the file is — the chunk pipeline it writes into,
// which is a handful of chunks in flight — and on a test-sized file that fixed
// cost dwarfs everything else. The question a 4 GB film on a 12 GB machine asks
// is how much *per byte of file*, so that is what is measured: two sizes, and
// the slope between them.
//
// The old reader materialised the file four times over — raw parts, decrypted
// parts, the concatenation, the decompressed result — measured at about three
// times the file on incompressible data. This one holds the two compressed
// halves and streams the rest, which is one.
func TestConvertingCostDoesNotMultiplyTheFile(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates hundreds of megabytes")
	}

	measure := func(size int) uint64 {
		v, _ := chunkedVault(t, 3, 16<<20)
		v.chunks = newChunkCache(1 << 20)

		// Incompressible, like a film. A compressible fixture shrinks the parts
		// to nothing and hides the cost being measured.
		payload := make([]byte, size)
		rand.New(rand.NewSource(1)).Read(payload)
		entry := addWholeEntry(t, v, "legacy", "film.bin", payload)
		payload = nil

		var err error
		peak := convertPeak(func() { _, err = v.Convert(context.Background(), entry.ID) })
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if convertSpoolsOnDisk(t, v) != 0 {
			t.Error("a conversion spool was left behind")
		}
		converted, err := v.Entry(entry.ID)
		if err != nil {
			t.Fatalf("Entry: %v", err)
		}
		if !converted.Chunked() {
			t.Fatal("the file was not converted")
		}
		return peak
	}

	const small, large = 64 << 20, 192 << 20
	smallPeak, largePeak := measure(small), measure(large)

	slope := float64(int64(largePeak)-int64(smallPeak)) / float64(large-small)
	fmt.Printf("  converting %d MB peaked at %.1f MB; %d MB peaked at %.1f MB — %.2f× the file per byte\n",
		small>>20, float64(smallPeak)/(1<<20), large>>20, float64(largePeak)/(1<<20), slope)

	// One is the two compressed halves, which the format genuinely requires:
	// GCM will not release either until the tag over all of it verifies. Much
	// above that means something is being copied that need not be.
	if slope > 1.6 {
		t.Errorf("each extra byte of file costs %.2f bytes of memory to convert; "+
			"the format needs about 1, so something is still being buffered", slope)
	}
}

// Converting must not change the file, and must leave it readable at an offset —
// which is the entire point of doing it.
func TestConvertingPreservesTheFileAndMakesItSeekable(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	ctx := context.Background()

	payload := readerPayload(40000)
	entry := addWholeEntry(t, v, "legacy", "film.bin", payload)

	oldKeys := make([]string, 0, len(entry.Shards))
	for _, s := range entry.Shards {
		oldKeys = append(oldKeys, s.Key)
	}

	// Before: refused for random access.
	if _, _, err := v.OpenReadSeeker(ctx, entry.ID); !errors.Is(err, ErrNeedsConversion) {
		t.Fatalf("a pre-chunking file was not refused: %v", err)
	}

	report, err := v.Convert(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if report.Size != int64(len(payload)) {
		t.Errorf("report says %d bytes, want %d", report.Size, len(payload))
	}

	converted, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if !converted.Chunked() {
		t.Fatal("the file is still stored whole")
	}
	if converted.Hash != entry.Hash {
		t.Error("the conversion changed the file's hash")
	}
	if converted.Size != entry.Size {
		t.Errorf("size changed from %d to %d", entry.Size, converted.Size)
	}

	// After: reads at an offset, which is what the refusal was asking for.
	body, _, err := v.OpenReadSeeker(ctx, entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker after conversion: %v", err)
	}
	if _, err := body.Seek(15000, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 500)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if !bytes.Equal(buf, payload[15000:15500]) {
		t.Error("the converted file reads back wrong at an offset")
	}

	// End to end too.
	whole, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after conversion: %v", err)
	}
	if !bytes.Equal(whole, payload) {
		t.Error("the converted file did not match the original")
	}

	// And the parts it used to live on are gone.
	for _, key := range oldKeys {
		if full := shardPath(roots, key); full != "" {
			t.Errorf("a pre-chunking part survived the conversion at %s", full)
		}
	}
}

// Converting something already chunked is a no-op rather than a re-upload.
func TestConvertingAnAlreadyChunkedFileDoesNothing(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	ctx := context.Background()

	payload := readerPayload(20000)
	entry, _, err := v.Upload(ctx, MainScope, "/", "already.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	report, err := v.Convert(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if report.ChunkCount != entry.ChunkCount {
		t.Errorf("report says %d chunks, want the %d it already had", report.ChunkCount, entry.ChunkCount)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.ArchiveID != entry.ArchiveID {
		t.Error("an already-chunked file was re-uploaded for nothing")
	}
}

// The browser needs to know how much there is before it offers to do it.
func TestPendingConversionListsOnlyTheOldFormat(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, MainScope, "/", "new.bin", readerPayload(5000), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	old1 := addWholeEntry(t, v, "old-1", "one.bin", readerPayload(3000))
	addWholeEntry(t, v, "old-2", "two.bin", readerPayload(7000))

	pending := v.PendingConversion()
	if len(pending) != 2 {
		t.Fatalf("PendingConversion listed %d files, want the 2 in the old format", len(pending))
	}
	for _, entry := range pending {
		if entry.Chunked() {
			t.Errorf("%s is chunked and should not be listed", entry.Path())
		}
	}

	if _, err := v.Convert(ctx, old1.ID); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got := len(v.PendingConversion()); got != 1 {
		t.Errorf("after converting one, %d are still pending, want 1", got)
	}
}

// ConvertAll works through the backlog and stops at a locked vault rather than
// grinding on through files it cannot read.
func TestConvertAllWorksThroughTheBacklog(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		addWholeEntry(t, v, fmt.Sprintf("old-%d", i), fmt.Sprintf("film-%d.bin", i), readerPayload(6000+i))
	}

	var seen int
	reports, err := v.ConvertAll(ctx, func(*ConversionReport, error) { seen++ })
	if err != nil {
		t.Fatalf("ConvertAll: %v", err)
	}
	if len(reports) != 3 || seen != 3 {
		t.Fatalf("converted %d files (%d progress calls), want 3", len(reports), seen)
	}
	if got := len(v.PendingConversion()); got != 0 {
		t.Errorf("%d files still pending after ConvertAll", got)
	}
}

// A vault that locks mid-pass stops the pass; there is nothing it could do next.
func TestConvertAllStopsOnALockedVault(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	addWholeEntry(t, v, "old-1", "one.bin", readerPayload(3000))

	v.Lock()
	if _, err := v.ConvertAll(context.Background(), nil); !errors.Is(err, ErrLocked) {
		t.Errorf("ConvertAll on a locked vault = %v, want ErrLocked", err)
	}
}

// A process killed mid-conversion leaves its spool behind; opening the vault is
// where that is cleaned up, since nothing else knows the file is dead.
func TestOpeningTheVaultSweepsAbandonedConversionSpools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.sand")

	stale := filepath.Join(dir, ".sand-convert-abandoned")
	if err := os.WriteFile(stale, []byte("plaintext from a process that died"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an abandoned conversion spool survived opening the vault")
	}
}
