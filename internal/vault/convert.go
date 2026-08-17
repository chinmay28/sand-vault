package vault

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Moving a file out of the pre-chunking format.
//
// Everything SAND stores today is chunked, and everything that serves a file
// reads it that way — at an offset, a chunk at a time, for a cost that does not
// grow with the file. The one thing that cannot is a file stored before chunking
// existed, because that format is a single sealed blob with no seams to read
// between. Those files are refused by the read path (ErrNeedsConversion) and
// converted here instead.
//
// It is deliberately something you ask for rather than something a read does to
// you. The earlier design converted in the background on first read, which meant
// an operation costing a download and a re-upload of the whole file started
// itself, at the worst possible moment, on a machine that might not survive it —
// and if it did not survive, nothing committed, so the next read started it
// again. Asking first also means the user hears the honest answer: this takes a
// while, and it moves your data.
//
// The conversion is bounded. It reads the old format the one way that format
// supports — sequentially, end to end — and streams what comes out into the
// chunked writer, so what it holds is one file's worth of compressed halves
// rather than the three or four copies DecodeBytes would make.

// ConversionReport describes what a conversion did.
type ConversionReport struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ChunkCount int    `json:"chunk_count"`

	// Warnings are the non-fatal parts: an account that would not give up the
	// file's old parts, most often. The file is converted either way.
	Warnings []string `json:"warnings,omitempty"`
}

// NeedsConversion reports whether a file is still in the pre-chunking format.
func (v *Vault) NeedsConversion(id string) (bool, error) {
	entry, err := v.Entry(id)
	if err != nil {
		return false, err
	}
	return !entry.Chunked(), nil
}

// PendingConversion lists every file still stored in the old format, so a
// caller can say how much there is to do and offer to work through it.
//
// Every vault currently open, because every one of them may hold files from
// before chunking existed and the answer to "how much is left" should not
// change with which sub vault happens to be shut. Files inside a sub vault that
// is closed are not counted, for the same reason they are not listed: nothing
// can read that index.
func (v *Vault) PendingConversion() []*Entry {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.manifest == nil {
		return nil
	}

	var out []*Entry
	for _, m := range v.manifestsLocked() {
		for _, entry := range m.Descendants("/") {
			if !entry.Chunked() {
				snapshot := *entry
				snapshot.Shards = append([]Shard(nil), entry.Shards...)
				out = append(out, &snapshot)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path() < out[j].Path() })
	return out
}

// Convert rewrites one pre-chunking file into the chunked format, in place: the
// index moves onto the new parts in a single write, so the file is readable
// throughout — as it was until that write, and chunked after it.
//
// A file already chunked is left alone and reported as such rather than being
// re-uploaded for nothing.
func (v *Vault) Convert(ctx context.Context, id string) (*ConversionReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	scope, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	stale := *entry
	stale.Shards = append([]Shard(nil), entry.Shards...)
	v.mu.RUnlock()

	if stale.Chunked() {
		return &ConversionReport{
			Path: stale.Path(), Size: stale.Size, ChunkCount: stale.ChunkCount,
		}, nil
	}

	// Rebuilt onto disk rather than into memory. This is the one unavoidable
	// whole-file materialisation in the old format, and disk is where it
	// belongs: an SD card can hold a film, and the RAM around it cannot.
	spool, size, hash, err := v.rebuildLegacyToDisk(ctx, &stale)
	if err != nil {
		return nil, fmt.Errorf("%s could not be converted: %w", stale.Path(), err)
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	// Back to the accounts it was already on, so converting is not also a move.
	current := make([]string, 0, len(stale.Shards))
	for _, s := range stale.Shards {
		current = append(current, s.ProviderID)
	}

	placed, err := v.scatterStream(ctx, scope, stale.Name, spool, size, hash, current, false, v.uploadChunkSize())
	report := &ConversionReport{Path: stale.Path(), Size: size, Warnings: placed.warnings}
	if err != nil {
		return report, fmt.Errorf("storing the converted %s: %w", stale.Path(), err)
	}
	report.ChunkCount = placed.chunkCount

	fresh := &Entry{
		ArchiveID:  placed.archiveID,
		Shards:     placed.shards,
		ChunkSize:  placed.chunkSize,
		ChunkCount: placed.chunkCount,
	}

	v.mu.Lock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		v.deleteEntryShards(context.WithoutCancel(ctx), fresh)
		return report, err
	}
	e := m.ByID(id)
	if e == nil {
		// Deleted while it was being converted; the new parts are referenced by
		// nothing and go the way the old ones would have.
		v.mu.Unlock()
		report.Warnings = append(report.Warnings,
			v.deleteEntryShards(context.WithoutCancel(ctx), fresh)...)
		return report, nil
	}

	previous := *e
	e.ArchiveID = placed.archiveID
	e.KeyID = placed.keyID
	e.Shards = placed.shards
	e.ChunkSize = placed.chunkSize
	e.ChunkCount = placed.chunkCount
	// ModifiedAt is left alone: the file did not change, only how it is stored.
	err = v.persistLocked()
	if err != nil {
		*e = previous
	}
	v.mu.Unlock()

	if err != nil {
		v.deleteEntryShards(context.WithoutCancel(ctx), fresh)
		return report, fmt.Errorf("recording the converted %s: %w", stale.Path(), err)
	}

	// The old parts are unreferenced now. A failure to erase them is reported
	// rather than fatal — the index already points at the new ones.
	for _, w := range v.deleteEntryShards(context.WithoutCancel(ctx), &stale) {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("%s: an old part is still on its account — %s", stale.Path(), w))
	}
	return report, nil
}

// rebuildLegacyToDisk gathers a pre-chunking file and writes its plaintext to a
// temporary file beside the vault, returning it rewound with the length and hash
// of what went through.
//
// Beside the vault rather than in the system temp directory, for the reason
// UploadStream spools there — it is plaintext, so it should inherit whatever
// protects the vault file — and because /tmp on a Raspberry Pi is usually a
// tmpfs, which would put the file back in the memory this exists to keep it out
// of.
func (v *Vault) rebuildLegacyToDisk(ctx context.Context, entry *Entry) (*os.File, int64, [32]byte, error) {
	var hash [32]byte

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, 0, hash, ErrLocked
	}
	shardPassword, err := v.shardPasswordForLocked(entry.KeyID)
	configs := v.configsForLocked(entry.Shards)
	v.mu.RUnlock()
	if err != nil {
		return nil, 0, hash, err
	}

	blobs, err := v.fetchLegacyParts(ctx, entry, configs)
	if err != nil {
		return nil, 0, hash, err
	}

	f, err := os.CreateTemp(filepath.Dir(v.path), ".sand-convert-*")
	if err != nil {
		return nil, 0, hash, fmt.Errorf("creating a temporary file for the conversion: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, fmt.Errorf("securing the temporary file: %w", err)
	}

	meta, err := archive.DecodeLegacyTo(blobs, shardPassword, f)
	// The halves were decrypted in place inside those buffers, so let go of
	// them before the scatter starts rather than holding a file's worth of
	// plaintext through it.
	for i := range blobs {
		blobs[i] = nil
	}
	blobs = nil
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, fmt.Errorf("writing the rebuilt file to disk: %w", err)
	}

	size := int64(meta.OriginalSize)
	// DecodeLegacyTo has already checked this against the archive's own record;
	// checking it against the index too is what catches an index that has drifted
	// from what is actually stored.
	if entry.Hash != "" && hex.EncodeToString(meta.OriginalHash[:]) != entry.Hash {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, hash, fmt.Errorf(
			"%s does not match the hash the index recorded for it", entry.Path())
	}
	return f, size, meta.OriginalHash, nil
}

// fetchLegacyParts collects enough parts to rebuild a pre-chunking file.
//
// Enough, and no more. Each part of the old format is half the compressed file,
// so a third one is half a film of memory bought for nothing — and cancelling a
// download already in flight does not reliably give that memory back, because a
// provider reading from a local disk finishes long before it looks at a context.
// So only the minimum are started, and the spare is fetched if one of them
// fails rather than in case one does.
//
// The cost is latency when an account is slow or gone: a failure is discovered
// before the replacement starts. Converting is already a minutes-long operation
// bounded by bandwidth, and the memory is the thing that decides whether the
// machine survives it.
func (v *Vault) fetchLegacyParts(ctx context.Context, entry *Entry, configs map[string]provider.Config) ([][]byte, error) {
	type source struct {
		shard Shard
		cfg   provider.Config
	}
	// Ordered so that every distinct part comes before any second copy of one.
	// Two copies of part 1 rebuild nothing, so the spares are worth reading only
	// after a part with no other source has failed.
	var sources []source
	var spares []source
	seen := map[int]bool{}
	for _, shard := range entry.Shards {
		cfg, ok := configs[shard.ProviderID]
		if !ok {
			continue
		}
		if seen[shard.Part] {
			spares = append(spares, source{shard: shard, cfg: cfg})
			continue
		}
		seen[shard.Part] = true
		sources = append(sources, source{shard: shard, cfg: cfg})
	}
	if len(sources) < archive.MinPartsToRestore {
		return nil, fmt.Errorf("%s needs %d parts and only %d of them are on connected accounts",
			entry.Path(), archive.MinPartsToRestore, len(sources))
	}
	sources = append(sources, spares...)

	fetch := func(s source) ([]byte, error) {
		p, err := v.buildProvider(s.cfg)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", s.shard.Part, err)
		}
		blob, err := p.Get(ctx, s.shard.Key)
		if err != nil {
			return nil, fmt.Errorf("part %d from %s: %w", s.shard.Part, s.cfg.Name, err)
		}
		return blob, nil
	}

	// The first MinPartsToRestore together, then the spares one at a time only
	// if they are needed.
	type partResult struct {
		part int
		blob []byte
		err  error
	}
	wanted := sources[:archive.MinPartsToRestore]
	results := make(chan partResult, len(wanted))
	for _, s := range wanted {
		go func(s source) {
			blob, err := fetch(s)
			results <- partResult{part: s.shard.Part, blob: blob, err: err}
		}(s)
	}

	held := map[int][]byte{}
	var failures []string
	for range wanted {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		held[r.part] = r.blob
	}

	for i := archive.MinPartsToRestore; i < len(sources) && len(held) < archive.MinPartsToRestore; i++ {
		if _, already := held[sources[i].shard.Part]; already {
			continue
		}
		blob, err := fetch(sources[i])
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		held[sources[i].shard.Part] = blob
	}

	if len(held) < archive.MinPartsToRestore {
		return nil, fmt.Errorf("could not gather %d parts for %s (got %d): %v",
			archive.MinPartsToRestore, entry.Path(), len(held), failures)
	}
	return collectParts(held), nil
}

// ConvertAll works through every file still in the old format, one at a time,
// reporting each as it goes.
//
// One at a time on purpose: each conversion is a download and an upload of a
// whole file, and running several at once would multiply both the bandwidth and
// the memory by exactly the number of them.
func (v *Vault) ConvertAll(ctx context.Context, onProgress func(*ConversionReport, error)) ([]*ConversionReport, error) {
	if !v.Unlocked() {
		// PendingConversion cannot see the index while locked, so without this
		// a locked vault would look like a vault with nothing left to convert.
		return nil, ErrLocked
	}

	pending := v.PendingConversion()
	reports := make([]*ConversionReport, 0, len(pending))

	for _, entry := range pending {
		if err := ctx.Err(); err != nil {
			return reports, err
		}
		report, err := v.Convert(ctx, entry.ID)
		if onProgress != nil {
			onProgress(report, err)
		}
		if report != nil {
			reports = append(reports, report)
		}
		// One file that will not convert costs that file. A locked vault costs
		// the pass, because nothing after it can succeed either.
		if errors.Is(err, ErrLocked) {
			return reports, err
		}
	}
	return reports, nil
}

// sweepConversionSpools removes rebuilt copies left behind by a process that
// died mid-conversion. Nothing else writes these names, and a live one is only
// ever open, so removing them at startup is safe.
func sweepConversionSpools(vaultPath string) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(vaultPath), ".sand-convert-*"))
	if err != nil {
		return
	}
	for _, path := range matches {
		os.Remove(path)
	}
}
