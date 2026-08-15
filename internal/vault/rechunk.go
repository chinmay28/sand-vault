package vault

import (
	"context"
	"log"
	"sync"
)

// Reading a file stored whole is what schedules its conversion to chunks.
//
// The obvious implementation converts it there and then, on the read. That
// would be wrong three times over. A read would pay for a full scatter and an
// erase before returning a byte, so pressing play on a film would wait out a
// re-upload of it. A read would have to take the write lock and persist the
// index, which rewrites manifest.sand on *every* connected account — cloud
// writes triggered by a range request. And a player asks for a file from
// several connections at once, so the same file would start converting several
// times over.
//
// So the read serves what it already gathered and leaves a note. A single
// worker drains the queue afterwards, one file at a time, through the same
// resumable path a password change uses: each file commits on its own, a locked
// or offline vault simply stops, and nothing is lost by giving up partway
// because the file is still readable exactly as it was.

// rechunkQueueDepth bounds the note-taking. Past it, further reads are not
// queued — a vault with a large backlog will get to them the next time they are
// read, and an unbounded queue of file IDs is not worth holding for that.
const rechunkQueueDepth = 256

// queueRechunk notes that a file is still stored whole and should be converted.
// It never blocks the caller and never touches the vault lock.
func (v *Vault) queueRechunk(id string) {
	v.rechunkMu.Lock()
	defer v.rechunkMu.Unlock()

	if v.rechunkOff || v.rechunkQueued[id] || len(v.rechunkQueued) >= rechunkQueueDepth {
		return
	}
	if v.rechunkQueued == nil {
		v.rechunkQueued = map[string]bool{}
	}
	v.rechunkQueued[id] = true
	v.rechunkOrder = append(v.rechunkOrder, id)

	if !v.rechunkRunning {
		v.rechunkRunning = true
		go v.drainRechunk()
	}
}

// drainRechunk converts queued files one at a time until the queue is empty.
func (v *Vault) drainRechunk() {
	for {
		v.rechunkMu.Lock()
		if len(v.rechunkOrder) == 0 {
			v.rechunkRunning = false
			v.rechunkIdle.Broadcast()
			v.rechunkMu.Unlock()
			return
		}
		id := v.rechunkOrder[0]
		v.rechunkOrder = v.rechunkOrder[1:]
		delete(v.rechunkQueued, id)
		v.rechunkMu.Unlock()

		v.rechunkOne(id)
	}
}

// rechunkOne converts a single file, or quietly declines to.
func (v *Vault) rechunkOne(id string) {
	v.mu.RLock()
	locked := v.dataKey == nil
	var chunked, gone bool
	if !locked {
		entry := v.manifest.ByID(id)
		gone = entry == nil
		chunked = entry != nil && entry.Chunked()
	}
	v.mu.RUnlock()

	// Deleted since the read, already converted by an earlier pass, or the
	// vault has locked and there is nothing to convert it with. The last is why
	// migrateFile is called without the read's context: the conversion is not
	// the read's work and must not be cancelled when the reader hangs up.
	if locked || gone || chunked {
		return
	}

	if _, _, warnings, err := v.migrateFile(context.Background(), id); err != nil {
		// Not worth failing anything over: the file is still readable exactly
		// as it was, and the next read queues it again.
		log.Printf("could not convert a stored file to chunks: %v", err)
	} else {
		for _, w := range warnings {
			log.Printf("converting a stored file to chunks: %s", w)
		}
	}
}

// AwaitRechunk blocks until no background conversion is running or queued. It
// exists for callers that need the vault settled before they look at it —
// tests, and a CLI reporting what a command left behind.
func (v *Vault) AwaitRechunk() {
	v.rechunkMu.Lock()
	defer v.rechunkMu.Unlock()
	for v.rechunkRunning {
		v.rechunkIdle.Wait()
	}
}

// SetRechunkOnRead turns the background conversion on or off. It is on by
// default; turning it off leaves files stored whole where they are, which is
// what a vault on a metered connection wants, since converting costs a download
// and an upload of everything read.
func (v *Vault) SetRechunkOnRead(on bool) {
	v.rechunkMu.Lock()
	defer v.rechunkMu.Unlock()
	v.rechunkOff = !on
	if !on {
		v.rechunkQueued = nil
		v.rechunkOrder = nil
	}
}

// forgetRechunkQueue drops anything waiting to be converted. Locking the vault
// takes the keys away, so the queue describes work that can no longer be done.
func (v *Vault) forgetRechunkQueue() {
	v.rechunkMu.Lock()
	defer v.rechunkMu.Unlock()
	v.rechunkQueued = nil
	v.rechunkOrder = nil
}

// rechunkState is the background converter's bookkeeping, held under its own
// leaf lock the way the backup syncer's is: scheduling happens while mu is
// held, and the work itself runs on its own goroutine after mu is released.
type rechunkState struct {
	rechunkMu      sync.Mutex
	rechunkIdle    sync.Cond
	rechunkQueued  map[string]bool
	rechunkOrder   []string
	rechunkRunning bool
	rechunkOff     bool
}
