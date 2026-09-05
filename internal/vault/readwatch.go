package vault

import "time"

// Where a read has got to, for whoever is waiting on it.
//
// Opening a file is a race between the accounts holding its parts, and then a
// decryption, and then a stream — and from the browser all three are one
// dark rectangle until the first byte lands. For a photo on a slow cloud that
// is several seconds of nothing, indistinguishable from a hang. The read path
// knows exactly what it is waiting on at every moment; this is how it says so.
//
// An observer is handed to OpenReadSeeker with WatchRead and hears one event
// per step. It is a window onto a request that is running and nothing more:
// no job state, nothing written down, and a reader opened without one costs
// nothing extra. Events are delivered from the goroutine doing the read, so an
// observer must return quickly and must not call back into the vault.

// ReadEventKind says which step of a read an event marks.
type ReadEventKind int

const (
	// ReadChunkWaiting: a chunk is needed and is not in the cache. Which
	// accounts will be asked is not yet known — another reader may already be
	// fetching this very chunk, in which case this read waits on that fetch
	// and hears nothing more until ReadChunkReady.
	ReadChunkWaiting ReadEventKind = iota
	// ReadChunkStarted: this read is asking the accounts itself. Asked names
	// the shards it sent for.
	ReadChunkStarted
	// ReadShardArrived: one account has answered with its part.
	ReadShardArrived
	// ReadShardFailed: one account could not produce its part. Err says why.
	ReadShardFailed
	// ReadChunkDecrypting: enough parts are back; the chunk is being rebuilt
	// and decrypted.
	ReadChunkDecrypting
	// ReadChunkReady: the chunk's plaintext is in hand and bytes can flow.
	ReadChunkReady
	// ReadChunkFailed: the chunk could not be rebuilt. Err says why.
	ReadChunkFailed
)

// String names the kind, for logs and tests.
func (k ReadEventKind) String() string {
	switch k {
	case ReadChunkWaiting:
		return "chunk-waiting"
	case ReadChunkStarted:
		return "chunk-started"
	case ReadShardArrived:
		return "shard-arrived"
	case ReadShardFailed:
		return "shard-failed"
	case ReadChunkDecrypting:
		return "chunk-decrypting"
	case ReadChunkReady:
		return "chunk-ready"
	case ReadChunkFailed:
		return "chunk-failed"
	}
	return "unknown"
}

// ReadEvent is one step of one read.
type ReadEvent struct {
	Kind ReadEventKind

	// Chunk is which chunk the event is about, counting from 0, out of Chunks.
	Chunk  int
	Chunks int

	// Needed is how many parts rebuild a chunk of this file.
	Needed int

	// Asked lists the shards sent for, on ReadChunkStarted. Each names the
	// account holding it.
	Asked []Shard

	// Shard is the part an account answered for, on the shard events.
	Shard Shard

	// Took is how long that account took to answer, on the shard events.
	Took time.Duration

	// Err is what went wrong, on the failure events.
	Err error
}

// ReadObserver hears where a read has got to. See ReadEvent.
type ReadObserver interface {
	ObserveRead(ReadEvent)
}

// ReadOption adjusts how a file is opened for reading.
type ReadOption func(*ChunkedReader)

// WatchRead reports the read's progress to obs. A nil observer is the same as
// not watching.
func WatchRead(obs ReadObserver) ReadOption {
	return func(r *ChunkedReader) { r.observer = obs }
}

// notifyRead delivers one event to an observer, if there is one, filling in
// the two figures every event carries: how many chunks the file has and how
// many parts rebuild one.
func notifyRead(obs ReadObserver, entry *Entry, ev ReadEvent) {
	if obs == nil {
		return
	}
	ev.Chunks = entry.ChunkCount
	ev.Needed = entry.Scheme().Data
	obs.ObserveRead(ev)
}
