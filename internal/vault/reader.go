package vault

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// DefaultChunkCacheBytes is how much decrypted chunk data one vault keeps in
// memory.
//
// A player seeking around a film asks for the same chunk repeatedly — the one
// holding the index it just jumped into — and refetching it from two clouds
// every time would make scrubbing unusable. The cache is small on purpose: it
// holds plaintext, so it is dropped the moment the vault locks, and it is
// measured in bytes rather than chunks because chunk size is per file.
const DefaultChunkCacheBytes = 128 << 20

// chunkCache is a bounded LRU of decrypted chunks, shared by every reader on a
// vault so that two players on the same file do not each pay for it.
//
// It holds plaintext. Lock drops it along with the keys, for the same reason
// §3.4 drops the index: a locked vault must not be able to answer from
// something it read earlier.
type chunkCache struct {
	mu    sync.Mutex
	limit int64
	used  int64
	items map[string]*list.Element
	order *list.List // front is most recently used
}

type cachedChunk struct {
	key  string
	data []byte
}

func newChunkCache(limit int64) *chunkCache {
	if limit <= 0 {
		limit = DefaultChunkCacheBytes
	}
	return &chunkCache{
		limit: limit,
		items: map[string]*list.Element{},
		order: list.New(),
	}
}

// chunkCacheKey names one chunk of one archive. The archive ID rather than the
// entry ID, so that re-encrypting a file — which mints a new archive ID —
// leaves the old chunks unreachable instead of stale.
func chunkCacheKey(archiveID string, index int) string {
	return fmt.Sprintf("%s/%d", archiveID, index)
}

func (c *chunkCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cachedChunk).data, true
}

func (c *chunkCache) put(key string, data []byte) {
	if int64(len(data)) > c.limit {
		// One chunk larger than the whole budget would evict everything and
		// then itself; not caching it at all is the honest outcome.
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return
	}

	el := c.order.PushFront(&cachedChunk{key: key, data: data})
	c.items[key] = el
	c.used += int64(len(data))

	for c.used > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cachedChunk)
		c.order.Remove(oldest)
		delete(c.items, entry.key)
		c.used -= int64(len(entry.data))
	}
}

// clear drops every cached chunk, which is what locking the vault does to it.
//
// The bytes are released rather than zeroed, the same way the thumbnail cache
// releases its pictures. A reader that has already been handed a chunk may
// still be copying out of it, and overwriting it underneath that reader would
// be a data race that quietly returns zeros — so the reference goes and the
// memory becomes garbage. What matters for §3.4 is that a locked vault can no
// longer answer from it, and it cannot.
func (c *chunkCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = map[string]*list.Element{}
	c.order = list.New()
	c.used = 0
}

// chunkFlight collapses concurrent misses on the same chunk into one fetch. A
// player opening a file asks for its head from several connections at once, and
// without this each would gather the same chunk from the same two accounts.
type chunkFlight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	data []byte
	err  error
}

func newChunkFlight() *chunkFlight {
	return &chunkFlight{calls: map[string]*flightCall{}}
}

// do runs fetch for key unless another caller already is, in which case it
// waits for that one and shares its result.
func (f *chunkFlight) do(key string, fetch func() ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	if call, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-call.done
		return call.data, call.err
	}
	call := &flightCall{done: make(chan struct{})}
	f.calls[key] = call
	f.mu.Unlock()

	call.data, call.err = fetch()

	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	close(call.done)

	return call.data, call.err
}

// ChunkedReader reads a stored file at arbitrary offsets, fetching only the
// chunks an offset actually touches.
//
// It implements io.ReaderAt rather than io.ReadSeeker because that is the
// primitive both things wanting it are built from: a FUSE mount is handed an
// offset and a length directly, and an http.File's Seek-then-Read is a thin
// wrapper over one (see SectionReader). Making the reader the narrower of the
// two means the filesystem layers stay adapters rather than reimplementations.
//
// A reader is safe for concurrent use, which matters because that is exactly
// how a player uses one.
//
// It deliberately holds no key. Every miss re-reads the vault's data key under
// the lock and zeroes its copy afterwards, so a reader left open across a lock
// stops being able to read rather than carrying on from a key it captured.
type ChunkedReader struct {
	v     *Vault
	entry *Entry
}

// ErrNeedsConversion is returned for a file still stored in the pre-chunking
// format, which cannot be read at an offset.
//
// It is a distinct error because it wants a distinct answer: not "this failed"
// but "convert this first, then ask again". Everything above — the HTTP API, the
// WebDAV share, the browser — turns it into that offer rather than into a
// retry, and Convert is what satisfies it.
var ErrNeedsConversion = errors.New(
	"this file is stored in the format SAND used before chunking and has to be converted before it can be streamed")

// OpenReader returns a reader over a stored file.
//
// A file stored whole has no chunks to fetch individually, so it is refused
// here rather than silently rebuilt in full behind an interface that promises
// cheap seeks.
func (v *Vault) OpenReader(id string) (*ChunkedReader, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.dataKey == nil {
		return nil, ErrLocked
	}
	_, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
		return nil, fmt.Errorf("no such file: %s", id)
	}
	if !entry.Chunked() {
		return nil, fmt.Errorf("%s: %w", entry.Path(), ErrNeedsConversion)
	}

	snapshot := *entry
	snapshot.Shards = append([]Shard(nil), entry.Shards...)
	return &ChunkedReader{v: v, entry: &snapshot}, nil
}

// Size is the length of the file in bytes.
func (r *ChunkedReader) Size() int64 { return r.entry.Size }

// Entry is the index record the reader was opened on.
func (r *ChunkedReader) Entry() *Entry { return r.entry }

// ReadAt fills p with bytes from off, following io.ReaderAt: it reads until p
// is full or the file ends, and a short read always comes with an error.
func (r *ChunkedReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadAtContext(context.Background(), p, off)
}

// ReadAtContext is ReadAt with a context, so a client that hangs up mid-seek
// stops the fetches it started.
func (r *ChunkedReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.entry.Size {
		return 0, io.EOF
	}

	read := 0
	for read < len(p) {
		pos := off + int64(read)
		if pos >= r.entry.Size {
			return read, io.EOF
		}

		index := r.entry.ChunkIndexAt(pos)
		chunk, err := r.chunkAt(ctx, index)
		if err != nil {
			return read, err
		}

		within := pos - int64(index)*r.entry.ChunkSize
		if within >= int64(len(chunk)) {
			// The manifest and the stored chunk disagree about how long the
			// chunk is; reporting it beats returning whatever is at hand.
			return read, fmt.Errorf("chunk %d of %s is %d bytes, too short for offset %d",
				index, r.entry.Path(), len(chunk), pos)
		}
		read += copy(p[read:], chunk[within:])
	}
	return read, nil
}

// chunkAt returns one chunk's plaintext, from the cache when it is there and
// from the accounts when it is not.
func (r *ChunkedReader) chunkAt(ctx context.Context, index int) ([]byte, error) {
	key := chunkCacheKey(r.entry.ArchiveID, index)
	if data, ok := r.v.chunks.get(key); ok {
		return data, nil
	}

	return r.v.flight.do(key, func() ([]byte, error) {
		// Another caller may have finished while this one waited for its turn.
		if data, ok := r.v.chunks.get(key); ok {
			return data, nil
		}

		// Re-read the key every time rather than holding one: a reader open
		// across a lock must stop reading, not carry on from what it captured.
		snap, err := r.v.snapshotRead(r.entry)
		if err != nil {
			return nil, err
		}
		defer crypto.ZeroBytes(snap.dataKey)

		data, err := r.v.gatherChunk(ctx, r.entry, index, snap.configs, snap.dataKey)
		if err != nil {
			return nil, err
		}
		r.v.chunks.put(key, data)
		return data, nil
	})
}

// SectionReader is the whole file as an io.ReadSeeker, which is the shape an
// http.File wants. Ranges are served by seeking it, and each seek costs only
// the chunks it lands in.
func (r *ChunkedReader) SectionReader() *io.SectionReader {
	return io.NewSectionReader(r, 0, r.entry.Size)
}

// ctxReaderAt carries a caller's context into ReadAt, which io.SectionReader
// has nowhere to put. Without it a client that hangs up mid-seek would leave
// the gathers it started running to completion against three cloud accounts.
type ctxReaderAt struct {
	ctx    context.Context
	reader *ChunkedReader
}

func (c ctxReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return c.reader.ReadAtContext(c.ctx, p, off)
}

// OpenReadSeeker reads a stored file at an offset, so that serving a range
// costs the chunks that range covers rather than the file.
//
// It refuses a file still in the pre-chunking format. That format has no seams —
// its parts are halves of one sealed blob — so the only way to answer for a byte
// in the middle is to rebuild all of it, and doing that behind an interface that
// promises cheap seeks is how a 4 GB film became 12 GB of resident memory on a
// machine asked for the first megabyte.
//
// The honest answer is to say so. ErrNeedsConversion is not a failure to read
// the file; it is the file being in a format this door cannot open, and the
// caller is expected to offer Convert rather than to retry. Conversion is a
// sequential read, which that format handles perfectly well — see convert.go.
func (v *Vault) OpenReadSeeker(ctx context.Context, id string) (io.ReadSeeker, *Entry, error) {
	reader, err := v.OpenReader(id)
	if err != nil {
		return nil, nil, err
	}
	return io.NewSectionReader(
		ctxReaderAt{ctx: ctx, reader: reader}, 0, reader.Size()), reader.Entry(), nil
}
