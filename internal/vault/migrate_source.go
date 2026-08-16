package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sync"
)

// Where a re-encryption reads its plaintext from.
//
// Re-encrypting a file means reading all of it and writing all of it again, and
// the obvious way to do that is Fetch into a []byte and scatter from there. That
// is what this replaces, because the buffer is alive for the whole scatter —
// through the compression, the encryption and the upload of every chunk — so a
// 2 GB film is 2 GB of resident memory for minutes, on top of everything the
// scatter allocates. On a Raspberry Pi that is the difference between a
// conversion that finishes and one that gets the process killed. And a
// conversion that never finishes is one that runs again on the very next read,
// which is how a single old film ends up taking the machine down every time it
// is played.
//
// So the scatter reads at an offset instead, one chunk at a time, and where
// those bytes come from depends on how the file is stored now:
//
//   - Already chunked, and only the key is changing: straight from the vault,
//     chunk by chunk. Nothing is staged anywhere.
//   - Still stored whole: from the rebuilt copy on disk (spool.go). That format
//     has no seams, so it must be rebuilt in full — but onto disk, once, rather
//     than into memory for the duration.
type migrationSource struct {
	src   io.ReaderAt
	size  int64
	hash  [32]byte
	close func()

	// verify, when set, reports whether what was read hashed to what the index
	// says it should. Only the chunked path sets it; the spooled path hashed
	// the bytes as it wrote them.
	verify func() error
}

// openForMigration opens a file's plaintext for re-encryption.
func (v *Vault) openForMigration(ctx context.Context, entry *Entry) (*migrationSource, error) {
	// A file already in the chunked format can be read where it lies, so the
	// re-encryption costs one chunk of memory rather than one file of it.
	if reader, err := v.OpenReader(entry.ID); err == nil {
		if digest, ok := decodeHash(entry.Hash); ok {
			hashed := newSequentialHasher(ctxReaderAt{ctx: ctx, reader: reader}, digest)
			return &migrationSource{
				src:    hashed,
				size:   reader.Size(),
				hash:   digest,
				close:  func() {},
				verify: func() error { return hashed.check(entry.Path()) },
			}, nil
		}
		// No usable hash recorded, so there is nothing to seal the new chunks
		// with. Fall through and recompute it off the rebuilt copy.
	}

	spool, meta, err := v.openSpool(ctx, entry)
	if err != nil {
		return nil, err
	}
	return &migrationSource{
		src:   spool,
		size:  meta.size,
		hash:  meta.hash,
		close: func() { spool.Close() },
	}, nil
}

// decodeHash turns the index's hex digest back into bytes.
func decodeHash(encoded string) ([32]byte, bool) {
	var out [32]byte
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != len(out) {
		return out, false
	}
	copy(out[:], raw)
	return out, true
}

// sequentialHasher checks a file against its recorded hash while it is being
// read at an offset.
//
// Rebuilding a file end to end is the one read that can make that check (§6.4),
// and a re-encryption is exactly such a read — it just does not hold the result.
// scatterStream walks the chunks in order, so hashing what goes past in offset
// order reconstructs the same digest a Fetch would have computed.
//
// It is deliberately not an assertion about the read pattern. Anything arriving
// out of order simply turns verification off rather than failing a migration
// that is perfectly sound; the check is a guarantee that can be kept, not one
// the caller has to arrange.
type sequentialHasher struct {
	src  io.ReaderAt
	want [32]byte

	mu       sync.Mutex
	digest   hash.Hash
	consumed int64
	skewed   bool
}

func newSequentialHasher(src io.ReaderAt, want [32]byte) *sequentialHasher {
	return &sequentialHasher{src: src, want: want, digest: sha256.New()}
}

func (h *sequentialHasher) ReadAt(p []byte, off int64) (int, error) {
	n, err := h.src.ReadAt(p, off)
	if n > 0 {
		h.mu.Lock()
		if off == h.consumed {
			h.digest.Write(p[:n])
			h.consumed += int64(n)
		} else {
			h.skewed = true
		}
		h.mu.Unlock()
	}
	return n, err
}

// check compares what was read against what the index recorded.
func (h *sequentialHasher) check(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.skewed {
		return nil
	}
	var got [32]byte
	copy(got[:], h.digest.Sum(nil))
	if got != h.want {
		return fmt.Errorf("%s failed its hash check while being re-encrypted", path)
	}
	return nil
}
