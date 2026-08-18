package vault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// chunkUploadWindow is how many chunks are encoded and pushed at once.
//
// A chunked upload is otherwise a sequence of round trips — a 4 GB file is 256
// of them at the default chunk size, and waiting out each one in turn would
// make storing a film slower than the whole-file path it replaces. The window
// is small because the point is to hide latency, not to open every connection
// at once.
//
// It counts chunks rather than requests, and that is what makes it scale with
// the scheme rather than against it. A wider spread puts more requests in the
// air, but they go to more accounts: each provider still sees at most
// chunkUploadWindow of them at a time, whatever n is, and rate limits are per
// provider. Memory is flat too — the bytes in flight are
// window × n × chunkSize/k, and n/k is 1.5 at every scheme, so widening from
// three clouds to thirty holds exactly as much as it did.
const chunkUploadWindow = 4

// uploadChunkSize is the chunk length new uploads are cut into, falling back to
// the default for a vault built before the field existed.
func (v *Vault) uploadChunkSize() uint32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.chunkSize == 0 {
		return archive.DefaultChunkSize
	}
	return v.chunkSize
}

// scatterChunked cuts a file into chunks and places each one across the same
// accounts, recording where under the vault's active data key.
//
// It is scatter's counterpart for the chunked format and answers to the same
// contract: what it returns in shards is really on the accounts, and on a
// failure it erases what it wrote rather than leaving orphans behind. The
// placement decision is made once, before the first chunk, because which
// accounts may hold a file is a property of the file and not of its pieces.
func (v *Vault) scatterChunked(ctx context.Context, scope Scope, name string, data []byte, sp spread, chunkSize uint32) (placement, error) {
	target, err := v.snapshotTarget(scope, sp)
	if err != nil {
		return placement{}, err
	}
	defer crypto.ZeroBytes(target.dataKey)

	out := placement{keyID: target.keyID}

	// The archive ID is minted here rather than by the encoder, because every
	// chunk of the file has to share it and the placement is seeded from it
	// before any chunk exists.
	var archiveID [16]byte
	u := uuid.New()
	copy(archiveID[:], u[:])
	out.archiveID = hex.EncodeToString(archiveID[:])
	out.originalHash = sha256.Sum256(data)

	seed := binary.BigEndian.Uint64(archiveID[:8])
	plan, scheme, err := target.planFor(seed)
	if err != nil {
		return out, err
	}
	out.scheme = scheme

	chunks, err := archive.PlanChunks(archiveID, name, out.originalHash, uint64(len(data)), chunkSize, scheme)
	if err != nil {
		return out, err
	}
	out.chunkSize = int64(chunks.ChunkSize)
	out.chunkCount = int(chunks.ChunkCount)

	next := uint32(0)
	written, failures, err := v.putChunks(ctx, target, plan, chunks, func() ([]byte, error) {
		size, err := chunks.PlaintextSize(next)
		if err != nil {
			return nil, err
		}
		offset := uint64(next) * uint64(chunks.ChunkSize)
		next++
		return data[offset : offset+size], nil
	})
	if err != nil {
		v.eraseChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, chunks.ChunkCount)
		return out, err
	}

	// A shard that failed on any chunk is not a shard this file has. Recording
	// it would claim objects that are not there, which delete, health and
	// recovery all read as fact; erasing the chunks it did manage keeps the
	// index honest at the cost of some bytes already uploaded.
	for shard, reason := range failures {
		out.warnings = append(out.warnings, reason)
		v.eraseShardChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, shard, chunks.ChunkCount)
	}

	out.shards = written
	sortShards(out.shards)

	// Because a shard is all-or-nothing across the file, every chunk has the
	// same shards standing — so this one check covers all of them.
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

// putChunks encodes and pushes every chunk of an archive, drawing each chunk's
// plaintext from next.
//
// next is called from this goroutine only, once per chunk and in order, so it
// can be a read from a stream as easily as a slice of something already in
// memory. Encoding and uploading then happen on a bounded window of goroutines
// behind it, which is what keeps a large file from becoming a long sequence of
// round trips without letting it become an unbounded number of them either.
//
// It returns one record per shard that landed on *every* chunk, and a reason
// per shard that did not. An error means the upload cannot be salvaged at all —
// reading or encoding failed, or the vault went away underneath it — and the
// caller erases what was written.
func (v *Vault) putChunks(
	ctx context.Context,
	target *transferTarget,
	plan Plan,
	chunks archive.ChunkPlan,
	next func() ([]byte, error),
) ([]Shard, map[int]string, error) {
	var mu sync.Mutex
	sizes := map[int]int64{}
	failures := map[int]string{}
	var fatal error

	putCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	window := make(chan struct{}, chunkUploadWindow)
	var outer sync.WaitGroup

	for index := uint32(0); index < chunks.ChunkCount; index++ {
		mu.Lock()
		stop := fatal != nil
		mu.Unlock()
		if stop {
			break
		}

		plaintext, err := next()
		if err != nil {
			mu.Lock()
			if fatal == nil {
				fatal = fmt.Errorf("chunk %d: %w", index, err)
				cancel()
			}
			mu.Unlock()
			break
		}

		outer.Add(1)
		window <- struct{}{}

		go func(index uint32, plaintext []byte) {
			defer outer.Done()
			defer func() { <-window }()

			encoded, err := archive.EncodeChunk(chunks, index, plaintext, target.dataKey)
			if err != nil {
				mu.Lock()
				if fatal == nil {
					fatal = fmt.Errorf("chunk %d: %w", index, err)
					cancel()
				}
				mu.Unlock()
				return
			}
			v.putChunkParts(putCtx, target, plan, chunks.ArchiveID, index, encoded, &mu, sizes, failures)
		}(index, plaintext)
	}

	outer.Wait()

	if fatal != nil {
		return nil, nil, fatal
	}

	archiveID := hex.EncodeToString(chunks.ArchiveID[:])
	shards := make([]Shard, 0, len(plan))
	for number, providerID := range plan {
		if _, bad := failures[number]; bad {
			continue
		}
		cfg := target.byID[providerID]
		shards = append(shards, Shard{
			Part:         number,
			ProviderID:   cfg.ID,
			ProviderName: cfg.Name,
			ProviderKind: string(cfg.Kind),
			// Chunk zero's key. The rest follow from ChunkShardKey, which is
			// why one record still describes one shard of the whole file.
			Key:  ChunkShardKey(archiveID, 0, number),
			Size: sizes[number],
		})
	}
	return shards, failures, nil
}

// putChunkParts pushes one chunk's shards concurrently, recording the bytes
// each account took and the first reason each shard had for refusing.
func (v *Vault) putChunkParts(
	ctx context.Context,
	target *transferTarget,
	plan Plan,
	archiveID [16]byte,
	index uint32,
	encoded *archive.EncodedChunk,
	mu *sync.Mutex,
	sizes map[int]int64,
	failures map[int]string,
) {
	idHex := hex.EncodeToString(archiveID[:])

	var wg sync.WaitGroup
	for number, providerID := range plan {
		cfg := target.byID[providerID]
		blob := encoded.Parts[number-1]
		key := ChunkShardKey(idHex, int(index), number)

		wg.Add(1)
		go func(number int, cfg provider.Config, key string, blob []byte) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err == nil {
				err = p.Put(ctx, key, blob)
			}

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if _, already := failures[number]; !already {
					failures[number] = fmt.Sprintf("shard %d → %s: %v", number, cfg.Name, err)
				}
				return
			}
			sizes[number] += int64(len(blob))
		}(number, cfg, key, blob)
	}
	wg.Wait()
}

// eraseChunks removes every object an archive's chunks could have written,
// across every copy of every part in the plan. It is the rollback for an upload
// that cannot be committed, and runs best-effort: an object that was never
// written answers not-found, which is the outcome wanted anyway.
func (v *Vault) eraseChunks(ctx context.Context, target *transferTarget, plan Plan, archiveID string, chunkCount uint32) {
	for number := range plan {
		v.eraseShardChunks(ctx, target, plan, archiveID, number, chunkCount)
	}
}

// eraseShardChunks removes one shard's object from every chunk of an archive.
func (v *Vault) eraseShardChunks(ctx context.Context, target *transferTarget, plan Plan, archiveID string, part int, chunkCount uint32) {
	providerID, ok := plan[part]
	if !ok {
		return
	}
	cfg, ok := target.byID[providerID]
	if !ok {
		return
	}
	p, err := v.buildProvider(cfg)
	if err != nil {
		return
	}

	window := make(chan struct{}, chunkUploadWindow*archive.PartCount)
	var wg sync.WaitGroup
	for index := uint32(0); index < chunkCount; index++ {
		wg.Add(1)
		window <- struct{}{}
		go func(index uint32) {
			defer wg.Done()
			defer func() { <-window }()
			_ = p.Delete(ctx, ChunkShardKey(archiveID, int(index), part))
		}(index)
	}
	wg.Wait()
}

// gatherChunk collects enough of one chunk's shards to rebuild it. It is the
// per-chunk twin of gather: the same race across accounts, decided the same way
// by whichever Scheme.Data distinct shards answer first.
func (v *Vault) gatherChunk(ctx context.Context, entry *Entry, index int, configs map[string]provider.Config, dataKey []byte) ([]byte, error) {
	scheme := entry.Scheme()

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type getResult struct {
		part int
		blob []byte
		err  error
	}
	results := make(chan getResult, len(entry.Shards))

	pending := 0
	for _, shard := range entry.Shards {
		cfg, ok := configs[shard.ProviderID]
		if !ok {
			continue
		}
		pending++
		go func(shard Shard, cfg provider.Config) {
			p, err := v.buildProvider(cfg)
			if err != nil {
				results <- getResult{part: shard.Part, err: fmt.Errorf("part %d: %w", shard.Part, err)}
				return
			}
			blob, err := p.Get(fetchCtx, ChunkShardKey(entry.ArchiveID, index, shard.Part))
			if err != nil {
				results <- getResult{part: shard.Part, err: fmt.Errorf("part %d from %s: %w", shard.Part, cfg.Name, err)}
				return
			}
			results <- getResult{part: shard.Part, blob: blob}
		}(shard, cfg)
	}

	held := map[int][]byte{}
	var failures []string
	for i := 0; i < pending; i++ {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		if _, already := held[r.part]; already {
			continue
		}
		held[r.part] = r.blob
		if len(held) >= scheme.Data {
			break
		}
	}

	if len(held) < scheme.Data {
		return nil, fmt.Errorf("could not gather %d shards for chunk %d of %s (got %d): %s",
			scheme.Data, index, entry.Path(), len(held),
			strings.Join(failures, "; "))
	}

	decoded, err := archive.DecodeChunk(collectParts(held), dataKey)
	if err != nil {
		return nil, fmt.Errorf("rebuilding chunk %d of %s: %w", index, entry.Path(), err)
	}
	if decoded.Index != uint32(index) {
		return nil, fmt.Errorf("chunk %d of %s answered as chunk %d",
			index, entry.Path(), decoded.Index)
	}
	return decoded.Data, nil
}

// readSnapshot is what a chunked read needs from the vault, taken under the
// lock once so the fetches that follow do not hold it.
type readSnapshot struct {
	dataKey []byte
	configs map[string]provider.Config
}

func (v *Vault) snapshotRead(entry *Entry) (*readSnapshot, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.dataKey == nil {
		return nil, ErrLocked
	}
	// Not necessarily the key new uploads use: a file waiting its turn in a
	// password change's re-encryption is still sealed under the old one.
	key, err := v.dataKeyForLocked(entry.KeyID)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", entry.Path(), err)
	}
	return &readSnapshot{dataKey: key, configs: v.configsForLocked(entry.Shards)}, nil
}

// gatherChunkedFile rebuilds a whole chunked file, chunk by chunk, and checks
// the result against the hash recorded when it was stored. Reading end to end
// is the one path that can still make that check — see §6.4.
func (v *Vault) gatherChunkedFile(ctx context.Context, entry *Entry) ([]byte, error) {
	snap, err := v.snapshotRead(entry)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(snap.dataKey)

	out := make([]byte, 0, entry.Size)
	for index := 0; index < entry.ChunkCount; index++ {
		chunk, err := v.gatherChunk(ctx, entry, index, snap.configs, snap.dataKey)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}

	if int64(len(out)) != entry.Size {
		return nil, fmt.Errorf("%s rebuilt to %d bytes, expected %d",
			entry.Path(), len(out), entry.Size)
	}
	if got := hex.EncodeToString(sha256Sum(out)); got != entry.Hash {
		return nil, fmt.Errorf("%s failed its hash check after rebuilding", entry.Path())
	}
	return out, nil
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
