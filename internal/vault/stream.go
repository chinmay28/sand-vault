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
	"sort"
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
func (v *Vault) UploadStream(ctx context.Context, dir, name string, r io.Reader, opts UploadOptions) (*Entry, []string, error) {
	name, err := SanitizeName(name)
	if err != nil {
		return nil, nil, err
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, nil, ErrLocked
	}
	dir = CleanDir(dir)
	if !v.manifest.FolderExists(dir) {
		v.mu.RUnlock()
		return nil, nil, fmt.Errorf("no such folder: %s", dir)
	}
	v.mu.RUnlock()

	spool, size, hash, err := v.spool(r)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	placed, err := v.scatterStream(ctx, name, spool, size, hash, opts.Accounts, true, v.uploadChunkSize())
	if err != nil {
		return nil, placed.warnings, err
	}

	// DetectMIME sniffs the head of the file, which is one chunk's worth at
	// most and already on local disk.
	head := make([]byte, 512)
	n, _ := spool.ReadAt(head, 0)

	return v.commitUpload(ctx, dir, name, size, DetectMIME(name, head[:n]), placed, opts)
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
func (v *Vault) scatterStream(ctx context.Context, name string, src io.ReaderAt, size int64, hash [32]byte, preferred []string, exact bool, chunkSize uint32) (placement, error) {
	target, err := v.snapshotTarget(preferred, exact)
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
	plan, err := target.planFor(seed)
	if err != nil {
		return out, err
	}

	chunks, err := archive.PlanChunks(archiveID, name, hash, uint64(size), chunkSize)
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

	for part, reason := range failures {
		out.warnings = append(out.warnings, reason)
		v.erasePartChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, part, chunks.ChunkCount)
	}

	out.shards = written
	sort.Slice(out.shards, func(i, j int) bool { return out.shards[i].Part < out.shards[j].Part })

	if len(out.shards) < archive.MinPartsToRestore {
		v.eraseChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, chunks.ChunkCount)
		err := fmt.Errorf("stored only %d of %d parts, need at least %d: %s",
			len(out.shards), archive.PartCount, archive.MinPartsToRestore,
			strings.Join(out.warnings, "; "))
		out.shards = nil
		return out, err
	}

	return out, nil
}

// commitUpload records a scattered file in the index, which is the half of an
// upload that both Upload and UploadStream do identically once the bytes are on
// the accounts.
func (v *Vault) commitUpload(ctx context.Context, dir, name string, size int64, mime string, placed placement, opts UploadOptions) (*Entry, []string, error) {
	shards, warnings := placed.shards, placed.warnings

	now := time.Now().UTC()
	entry := &Entry{
		ID:         uuid.NewString(),
		Dir:        dir,
		Name:       name,
		Size:       size,
		Hash:       hex.EncodeToString(placed.originalHash[:]),
		MIME:       mime,
		ArchiveID:  placed.archiveID,
		KeyID:      placed.keyID,
		CreatedAt:  now,
		ModifiedAt: now,
		Shards:     shards,
		ChunkSize:  placed.chunkSize,
		ChunkCount: placed.chunkCount,
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		v.deleteEntryShards(context.WithoutCancel(ctx), entry)
		return nil, warnings, ErrLocked
	}

	var replaced *Entry
	if existing := v.manifest.ByPath(JoinPath(dir, name)); existing != nil {
		if opts.Overwrite {
			replaced = existing
			v.manifest.remove(existing.ID)
		} else {
			entry.Name = v.manifest.uniqueName(dir, name)
		}
	}

	v.manifest.add(entry)
	err := v.persistLocked()
	if err != nil {
		v.manifest.remove(entry.ID)
		if replaced != nil {
			v.manifest.add(replaced)
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
		v.removeThumbs(context.WithoutCancel(ctx), dir, replaced.ID)
	}

	if len(shards) < archive.PartCount {
		warnings = append(warnings, fmt.Sprintf(
			"stored %d of %d parts — the file is recoverable but has no spare copy",
			len(shards), archive.PartCount))
	}

	return entry, warnings, nil
}
