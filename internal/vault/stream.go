package vault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/google/uuid"
)

// UploadStream stores a file read from r, scattering it a chunk at a time so
// that memory is bounded by the chunk window rather than by the file. It is
// what a large upload arriving over the network wants, where Upload would need
// the whole thing in RAM first.
//
// The stream is spooled to a temporary file beside the vault before anything is
// sent, and the reason is the whole-file SHA-256: every chunk carries it (§7.1),
// so it has to be known before the first chunk can be sealed, and a stream only
// yields it after the last byte. Spooling trades the memory Upload would have
// used for disk, which is the point — a 40 GB film is storable either way, but
// only one of them fits.
//
// The spool holds plaintext, briefly. It lives in the vault's own directory
// rather than the system temp dir, so it inherits whatever protects the vault
// file, and it is removed on every path out including failure.
func (v *Vault) UploadStream(ctx context.Context, scope Scope, dir, name string, r io.Reader, opts UploadOptions) (*Entry, []string, error) {
	name, err := SanitizeName(name)
	if err != nil {
		return nil, nil, err
	}

	dir, err = v.destinationLocked(scope, dir)
	if err != nil {
		return nil, nil, err
	}

	spool, size, hash, err := v.spool(r)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	placed, err := v.scatterStream(ctx, scope, name, spool, size, hash,
		spread{preferred: opts.Accounts, exact: true, scheme: opts.Scheme}, v.uploadChunkSize())
	if err != nil {
		return nil, placed.warnings, err
	}

	// DetectMIME sniffs the head of the file, which is one chunk's worth at
	// most and already on local disk.
	head := make([]byte, 512)
	n, _ := spool.ReadAt(head, 0)

	return v.commitUpload(ctx, scope, dir, name, size, DetectMIME(name, head[:n]), placed, opts)
}

// UploadStreamAt stores a file the caller can already read at an offset,
// skipping the spool UploadStream would make.
//
// It exists for the HTTP upload handler. Go's multipart parser has already
// written anything large to a temporary file, so spooling it a second time
// would put a 4 GB film on disk twice — which on a Raspberry Pi's SD card is
// both slow and the difference between fitting and not. Everything else is
// identical: the whole-file hash still has to be known before the first chunk
// is sealed, so src is read once to compute it and then rewound.
func (v *Vault) UploadStreamAt(ctx context.Context, scope Scope, dir, name string, src io.ReadSeeker, size int64, opts UploadOptions) (*Entry, []string, error) {
	name, err := SanitizeName(name)
	if err != nil {
		return nil, nil, err
	}

	dir, err = v.destinationLocked(scope, dir)
	if err != nil {
		return nil, nil, err
	}

	readerAt, ok := src.(io.ReaderAt)
	if !ok {
		// Nothing to gain over the spooling path, and correctness beats the
		// optimisation: fall back rather than guess.
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return nil, nil, fmt.Errorf("rewinding the upload: %w", err)
		}
		return v.UploadStream(ctx, scope, dir, name, src, opts)
	}

	digest := sha256.New()
	if _, err := io.Copy(digest, src); err != nil {
		return nil, nil, fmt.Errorf("reading the upload: %w", err)
	}
	var hash [32]byte
	copy(hash[:], digest.Sum(nil))

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("rewinding the upload: %w", err)
	}

	placed, err := v.scatterStream(ctx, scope, name, readerAt, size, hash,
		spread{preferred: opts.Accounts, exact: true, scheme: opts.Scheme}, v.uploadChunkSize())
	if err != nil {
		return nil, placed.warnings, err
	}

	head := make([]byte, 512)
	n, _ := readerAt.ReadAt(head, 0)

	return v.commitUpload(ctx, scope, dir, name, size, DetectMIME(name, head[:n]), placed, opts)
}

// spool copies a stream to a temporary file, returning it rewound along with
// the length and hash of what went through.
func (v *Vault) spool(r io.Reader) (*os.File, int64, [32]byte, error) {
	var hash [32]byte

	f, err := os.CreateTemp(filepath.Dir(v.path), ".sand-upload-*")
	if err != nil {
		return nil, 0, hash, fmt.Errorf("creating a temporary file for the upload: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, fmt.Errorf("securing the temporary upload file: %w", err)
	}

	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, digest), r)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, fmt.Errorf("reading the upload: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, fmt.Errorf("writing the upload to disk: %w", err)
	}

	copy(hash[:], digest.Sum(nil))
	return f, size, hash, nil
}

// scatterStream is scatterChunked reading from a file rather than a slice. The
// two differ only in where a chunk's plaintext comes from; everything about
// placement, per-part all-or-nothing and rollback is the same.
func (v *Vault) scatterStream(ctx context.Context, scope Scope, name string, src io.ReaderAt, size int64, hash [32]byte, sp spread, chunkSize uint32) (placement, error) {
	target, err := v.snapshotTarget(scope, sp)
	if err != nil {
		return placement{}, err
	}
	defer crypto.ZeroBytes(target.dataKey)

	out := placement{keyID: target.keyID, originalHash: hash}

	var archiveID [16]byte
	u := uuid.New()
	copy(archiveID[:], u[:])
	out.archiveID = hex.EncodeToString(archiveID[:])

	seed := binary.BigEndian.Uint64(archiveID[:8])
	plan, scheme, err := target.planFor(seed)
	if err != nil {
		return out, err
	}
	out.scheme = scheme

	chunks, err := archive.PlanChunks(archiveID, name, hash, uint64(size), chunkSize, scheme)
	if err != nil {
		return out, err
	}
	out.chunkSize = int64(chunks.ChunkSize)
	out.chunkCount = int(chunks.ChunkCount)

	next := uint32(0)
	written, failures, err := v.putChunks(ctx, target, plan, chunks, func() ([]byte, error) {
		want, err := chunks.PlaintextSize(next)
		if err != nil {
			return nil, err
		}
		// A fresh buffer per chunk: the goroutine encoding the previous one is
		// still reading from its own.
		buf := make([]byte, want)
		if _, err := io.ReadFull(io.NewSectionReader(src, int64(next)*int64(chunks.ChunkSize), int64(want)), buf); err != nil {
			return nil, fmt.Errorf("reading chunk %d back from the spool: %w", next, err)
		}
		next++
		return buf, nil
	})
	if err != nil {
		v.eraseChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, chunks.ChunkCount)
		return out, err
	}

	for shard, reason := range failures {
		out.warnings = append(out.warnings, reason)
		v.eraseShardChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, shard, chunks.ChunkCount)
	}

	out.shards = written
	sortShards(out.shards)

	if distinctShards(out.shards) < scheme.Data {
		v.eraseChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, chunks.ChunkCount)
		err := fmt.Errorf("stored only %d of %d shards, need at least %d: %s",
			distinctShards(out.shards), scheme.Total, scheme.Data,
			strings.Join(out.warnings, "; "))
		out.shards = nil
		return out, err
	}

	return out, nil
}

// commitUpload records a scattered file in the index, which is the half of an
// upload that both Upload and UploadStream do identically once the bytes are on
// the accounts.
func (v *Vault) commitUpload(ctx context.Context, scope Scope, dir, name string, size int64, mime string, placed placement, opts UploadOptions) (*Entry, []string, error) {
	shards, warnings := placed.shards, placed.warnings

	now := time.Now().UTC()
	entry := &Entry{
		ID:          uuid.NewString(),
		Dir:         dir,
		Name:        name,
		Size:        size,
		Hash:        hex.EncodeToString(placed.originalHash[:]),
		MIME:        mime,
		ArchiveID:   placed.archiveID,
		KeyID:       placed.keyID,
		CreatedAt:   now,
		ModifiedAt:  now,
		Shards:      shards,
		ChunkSize:   placed.chunkSize,
		ChunkCount:  placed.chunkCount,
		DataShards:  placed.scheme.Data,
		TotalShards: placed.scheme.Total,
	}

	v.mu.Lock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		v.deleteEntryShards(context.WithoutCancel(ctx), entry)
		return nil, warnings, err
	}

	var replaced *Entry
	if existing := m.ByPath(JoinPath(dir, name)); existing != nil {
		if opts.Overwrite {
			replaced = existing
			m.remove(existing.ID)
		} else {
			entry.Name = m.uniqueName(dir, name)
		}
	}

	m.add(entry)
	err = v.persistLocked()
	if err != nil {
		m.remove(entry.ID)
		if replaced != nil {
			m.add(replaced)
		}
	}
	v.mu.Unlock()

	if err != nil {
		v.deleteEntryShards(context.WithoutCancel(ctx), entry)
		return nil, warnings, err
	}

	// The replaced version's parts are now unreferenced; clean them up on a
	// best-effort basis so a failure here does not fail the upload.
	if replaced != nil {
		v.deleteEntryShards(context.WithoutCancel(ctx), replaced)
		// Its thumbnail showed the old contents and is keyed by an ID nothing
		// points at any more.
		v.removeThumbs(context.WithoutCancel(ctx), scope, dir, replaced.ID)
	}

	if stored := distinctShards(shards); stored < placed.scheme.Total {
		spare := stored - placed.scheme.Data
		warnings = append(warnings, fmt.Sprintf(
			"stored %d of the %d shards %s calls for — the file is recoverable, but only %d more "+
				"account(s) can go dark before it is not",
			stored, placed.scheme.Total, placed.scheme, spare))
	}

	return entry, warnings, nil
}
