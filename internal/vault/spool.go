package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Reading a file that is stored whole.
//
// The pre-chunking format has no seams. Its parts are halves of one compressed,
// encrypted blob, so the only way to read a byte in the middle is to rebuild all
// of it — which is what chunking replaced, and why reading such a file schedules
// its conversion (rechunk.go). Until that conversion lands, though, every read
// still has to rebuild the whole thing.
//
// Rebuilding it *into memory* is what made that dangerous. A player asks for a
// file from several connections at once and opens a fresh one on every seek, so
// "the whole file in RAM" became "the whole file in RAM, several times over", on
// a machine that may have less RAM than the film has bytes. A Raspberry Pi
// serving a 2 GB film went to swap and took the rest of the box with it.
//
// So it is rebuilt onto local disk instead, once, and shared: concurrent readers
// of the same file wait on the one gather rather than each starting their own,
// and the spool is unlinked when the last of them lets go. The disk it wants is
// disk an upload already spools through (stream.go), and what used to scale with
// readers × file size now scales with neither.
//
// It does not make the gather itself cheap — the old format cannot be read in
// pieces, so one rebuild still passes through memory. What it removes is paying
// that once per request, and holding the result afterwards.

// errSpoolGone is what a reader waiting on a rebuild gets when the vault locked
// underneath it.
var errSpoolGone = errors.New("the vault locked while this file was being read")

// spoolCache holds the rebuilt copies currently being read.
type spoolCache struct {
	dir string

	mu    sync.Mutex
	files map[string]*spooled
}

// spooled is one file's rebuilt copy and how many readers still want it.
type spooled struct {
	ready chan struct{}

	// Written once by the rebuilder, before ready is closed.
	path string
	size int64
	hash [32]byte
	err  error

	// refs and unlinked are guarded by the cache's mutex. A spool is unlinked
	// exactly once, by whoever takes the last reference away — which is the
	// last reader normally, and clear when the vault locks first.
	refs     int
	unlinked bool
}

func newSpoolCache(dir string) *spoolCache {
	return &spoolCache{dir: dir, files: map[string]*spooled{}}
}

// spoolReader is one reader's view of a rebuilt file: an independent offset over
// shared bytes, which gives up its claim on Close.
type spoolReader struct {
	*os.File
	cache *spoolCache
	entry *spooled
	once  sync.Once
}

func (r *spoolReader) Close() error {
	err := r.File.Close()
	r.once.Do(func() { r.cache.drop(r.entry) })
	return err
}

// open rebuilds a file onto disk and returns a reader over it, or joins the
// rebuild already running for it.
//
// Whichever caller arrives first does the gather and the rest wait on it. That
// is the difference between one rebuild and one per connection, which for a
// player opening a film is the difference between working and not.
func (c *spoolCache) open(ctx context.Context, id string, gather func(context.Context) ([]byte, error)) (*spoolReader, *spooled, error) {
	c.mu.Lock()
	entry, joined := c.files[id]
	if joined {
		entry.refs++
	} else {
		entry = &spooled{ready: make(chan struct{}), refs: 1}
		c.files[id] = entry
	}
	c.mu.Unlock()

	if !joined {
		// Not under the mutex: this is a gather from the cloud accounts, and
		// holding the cache shut for it would serialize every other file too.
		entry.path, entry.size, entry.hash, entry.err = c.rebuild(ctx, gather)
		if entry.err != nil {
			c.forget(id, entry)
		}
		close(entry.ready)
	}

	select {
	case <-entry.ready:
	case <-ctx.Done():
		// A reader that gives up mid-rebuild stops waiting; the rebuild carries
		// on for whoever else is waiting on it.
		c.drop(entry)
		return nil, nil, ctx.Err()
	}

	if entry.err != nil {
		c.drop(entry)
		return nil, nil, entry.err
	}

	// A handle of its own, so two readers seeking independently do not move
	// each other's offset.
	f, err := os.Open(entry.path)
	if err != nil {
		c.drop(entry)
		if c.wasUnlinked(entry) {
			return nil, nil, errSpoolGone
		}
		return nil, nil, fmt.Errorf("reopening the rebuilt copy: %w", err)
	}
	return &spoolReader{File: f, cache: c, entry: entry}, entry, nil
}

// rebuild gathers a file and writes it to a temporary file beside the vault,
// hashing it on the way past.
//
// Beside the vault rather than in the system temp directory, for the reason
// UploadStream spools there: it holds plaintext, so it should inherit whatever
// protects the vault file. On a Pi there is a second reason — /tmp is commonly a
// tmpfs, which would put the file straight back into the RAM this exists to keep
// it out of.
func (c *spoolCache) rebuild(ctx context.Context, gather func(context.Context) ([]byte, error)) (string, int64, [32]byte, error) {
	var hash [32]byte

	data, err := gather(ctx)
	if err != nil {
		return "", 0, hash, err
	}

	f, err := os.CreateTemp(c.dir, ".sand-read-*")
	if err != nil {
		return "", 0, hash, fmt.Errorf("creating a temporary file for the rebuilt copy: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", 0, hash, fmt.Errorf("securing the temporary file: %w", err)
	}
	defer f.Close()

	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, digest), bytes.NewReader(data))
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, hash, fmt.Errorf("writing the rebuilt copy to disk: %w", err)
	}

	copy(hash[:], digest.Sum(nil))
	return f.Name(), size, hash, nil
}

// drop gives up one claim on a spool, unlinking it with the last one.
func (c *spoolCache) drop(entry *spooled) {
	c.mu.Lock()
	entry.refs--
	unlink := entry.refs <= 0 && !entry.unlinked && entry.path != ""
	if unlink {
		entry.unlinked = true
	}
	// Only if this entry is still the one indexed under its id: clear may have
	// detached it, and a later read may have started a fresh one.
	if entry.refs <= 0 {
		for id, indexed := range c.files {
			if indexed == entry {
				delete(c.files, id)
				break
			}
		}
	}
	path := entry.path
	c.mu.Unlock()

	if unlink {
		os.Remove(path)
	}
}

// forget detaches a failed rebuild so the next read starts a fresh one rather
// than joining a cached failure.
func (c *spoolCache) forget(id string, entry *spooled) {
	c.mu.Lock()
	if c.files[id] == entry {
		delete(c.files, id)
	}
	c.mu.Unlock()
}

func (c *spoolCache) wasUnlinked(entry *spooled) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return entry.unlinked
}

// clear unlinks every spool, which is what locking the vault does to them: they
// are decrypted copies of stored files, so they go the way the chunk cache and
// the thumbnails go.
//
// A reader still holding a handle carries on reading — unlinking a file does not
// disturb an open descriptor — for the same reason chunkCache.clear releases its
// buffers rather than zeroing them: a reader mid-copy must not have the ground
// moved under it. What matters is that a locked vault can open nothing new, and
// it cannot; the entries are detached, so the next read gathers again and meets
// ErrLocked.
func (c *spoolCache) clear() {
	c.mu.Lock()
	var paths []string
	for id, entry := range c.files {
		delete(c.files, id)
		if entry.path != "" && !entry.unlinked {
			entry.unlinked = true
			paths = append(paths, entry.path)
		}
	}
	c.mu.Unlock()

	for _, path := range paths {
		os.Remove(path)
	}
}

// sweepSpools removes rebuilt copies left behind by a process that died holding
// them — an OOM kill, or a power cut mid-film. Nothing else writes these names,
// and a live one is only ever open, so removing them at startup is safe.
func sweepSpools(vaultPath string) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(vaultPath), ".sand-read-*"))
	if err != nil {
		return
	}
	for _, path := range matches {
		os.Remove(path)
	}
}
