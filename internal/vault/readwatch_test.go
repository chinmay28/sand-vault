package vault

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
)

// The read path saying where it has got to.
//
// A watcher of a read hears a chunk being sent for, each account answering,
// the decryption and the chunk arriving — in that order, once per chunk, and
// naming the accounts by what the file records them as.

// recordedReads collects everything an observer is told, in order.
type recordedReads struct {
	mu     sync.Mutex
	events []ReadEvent
}

func (r *recordedReads) ObserveRead(ev ReadEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordedReads) kinds() []ReadEventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReadEventKind, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Kind
	}
	return out
}

func (r *recordedReads) ofKind(kind ReadEventKind) []ReadEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ReadEvent
	for _, ev := range r.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestWatchedReadReportsEveryStepOfEveryChunk(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	payload := readerPayload(2500) // three chunks
	entry, _, err := v.Upload(ctx, MainScope, "/", "watched.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	heard := &recordedReads{}
	body, _, err := v.OpenReadSeeker(ctx, entry.ID, WatchRead(heard))
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Every chunk was sent for, rebuilt and handed over, in order.
	for _, kind := range []ReadEventKind{ReadChunkWaiting, ReadChunkStarted, ReadChunkDecrypting, ReadChunkReady} {
		got := heard.ofKind(kind)
		if len(got) != 3 {
			t.Fatalf("%v: heard %d, want one per chunk (3): %v", kind, len(got), heard.kinds())
		}
		for i, ev := range got {
			if ev.Chunk != i {
				t.Errorf("%v #%d is about chunk %d", kind, i, ev.Chunk)
			}
			if ev.Chunks != 3 || ev.Needed != 2 {
				t.Errorf("%v #%d says %d chunks, %d needed; want 3 and 2", kind, i, ev.Chunks, ev.Needed)
			}
		}
	}

	// Sent for: every part, each naming its account.
	started := heard.ofKind(ReadChunkStarted)[0]
	if len(started.Asked) != 3 {
		t.Fatalf("asked %d shards, want 3", len(started.Asked))
	}
	for _, shard := range started.Asked {
		if shard.ProviderName == "" || shard.ProviderID == "" {
			t.Errorf("a shard was sent for without naming its account: %+v", shard)
		}
	}

	// Enough answered to rebuild each chunk, and nobody failed.
	if arrived := heard.ofKind(ReadShardArrived); len(arrived) < 3*2 {
		t.Errorf("%d parts arrived across 3 chunks needing 2 each", len(arrived))
	}
	if failed := heard.ofKind(ReadShardFailed); len(failed) != 0 {
		t.Errorf("parts failed with every account up: %+v", failed)
	}

	// Within a chunk, the steps come in the order they happen.
	kinds := heard.kinds()
	rank := map[ReadEventKind]int{
		ReadChunkWaiting: 0, ReadChunkStarted: 1, ReadShardArrived: 2, ReadShardFailed: 2,
		ReadChunkDecrypting: 3, ReadChunkReady: 4,
	}
	chunk, last := -1, 0
	for i, kind := range kinds {
		if kind == ReadChunkWaiting {
			chunk++
			last = -1
		}
		if rank[kind] < last {
			t.Fatalf("event %d (%v) came after a later step in chunk %d: %v", i, kind, chunk, kinds)
		}
		last = rank[kind]
	}
}

func TestWatchedReadNamesTheAccountThatFailed(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "short.bin", readerPayload(500), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// One account goes dark. Two remain, which is enough to rebuild it.
	if err := os.RemoveAll(roots[0]); err != nil {
		t.Fatalf("removing an account's folder: %v", err)
	}

	heard := &recordedReads{}
	body, _, err := v.OpenReadSeeker(ctx, entry.ID, WatchRead(heard))
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("ReadAll with two of three accounts: %v", err)
	}

	// The race may finish before the missing account has reported its
	// failure; when it does report, it says which account it was.
	for _, ev := range heard.ofKind(ReadShardFailed) {
		if ev.Shard.ProviderName != "cloud-a" {
			t.Errorf("the failure names %q, want cloud-a", ev.Shard.ProviderName)
		}
		if ev.Err == nil {
			t.Error("a failed part carries no error")
		}
	}
	if len(heard.ofKind(ReadChunkReady)) != 1 {
		t.Errorf("the chunk never arrived: %v", heard.kinds())
	}
}

func TestWatchedReadReportsAChunkThatCannotBeRebuilt(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "gone.bin", readerPayload(500), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, root := range roots[:2] {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("removing an account's folder: %v", err)
		}
	}

	heard := &recordedReads{}
	body, _, err := v.OpenReadSeeker(ctx, entry.ID, WatchRead(heard))
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("the file was rebuilt from one part")
	}

	failed := heard.ofKind(ReadChunkFailed)
	if len(failed) != 1 || failed[0].Err == nil {
		t.Fatalf("the failure was not reported: %v", heard.kinds())
	}
	if len(heard.ofKind(ReadChunkReady)) != 0 {
		t.Error("a chunk that could not be rebuilt was reported ready")
	}
}

// A reader opened without an observer is exactly the reader there was.
func TestUnwatchedReadIsSilent(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "quiet.bin", readerPayload(100), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	body, _, err := v.OpenReadSeeker(ctx, entry.ID, WatchRead(nil))
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
}

func TestReadEventKindsHaveNames(t *testing.T) {
	for kind := ReadChunkWaiting; kind <= ReadChunkFailed; kind++ {
		if kind.String() == "unknown" {
			t.Errorf("kind %d has no name", kind)
		}
	}
	if ReadEventKind(99).String() != "unknown" {
		t.Error("an unknown kind is not called unknown")
	}
}
