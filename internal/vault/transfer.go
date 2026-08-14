package vault

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// Upload encodes data into encrypted parts, scatters them across the
// connected accounts according to the vault's placement policy, and records
// the result in the index.
//
// It returns the new entry plus any non-fatal warnings, such as one account
// being unreachable while enough others accepted their part.
func (v *Vault) Upload(ctx context.Context, dir, name string, data []byte, overwrite bool) (*Entry, []string, error) {
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

	placed, err := v.scatter(ctx, name, data)
	if err != nil {
		return nil, placed.warnings, err
	}
	shards, warnings := placed.shards, placed.warnings

	now := time.Now().UTC()
	entry := &Entry{
		ID:         uuid.NewString(),
		Dir:        dir,
		Name:       name,
		Size:       int64(len(data)),
		Hash:       hex.EncodeToString(placed.originalHash[:]),
		MIME:       DetectMIME(name, data),
		ArchiveID:  placed.archiveID,
		KeyID:      placed.keyID,
		CreatedAt:  now,
		ModifiedAt: now,
		Shards:     shards,
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		v.deleteShards(context.WithoutCancel(ctx), shards)
		return nil, warnings, ErrLocked
	}

	var replaced *Entry
	if existing := v.manifest.ByPath(JoinPath(dir, name)); existing != nil {
		if overwrite {
			replaced = existing
			v.manifest.remove(existing.ID)
		} else {
			entry.Name = v.manifest.uniqueName(dir, name)
		}
	}

	v.manifest.add(entry)
	err = v.persistLocked()
	if err != nil {
		v.manifest.remove(entry.ID)
		if replaced != nil {
			v.manifest.add(replaced)
		}
	}
	v.mu.Unlock()

	if err != nil {
		v.deleteShards(context.WithoutCancel(ctx), shards)
		return nil, warnings, err
	}

	// The replaced version's parts are now unreferenced; clean them up on a
	// best-effort basis so a failure here does not fail the upload.
	if replaced != nil {
		v.deleteShards(context.WithoutCancel(ctx), replaced.Shards)
	}

	if len(shards) < archive.PartCount {
		warnings = append(warnings, fmt.Sprintf(
			"stored %d of %d parts — the file is recoverable but has no spare copy",
			len(shards), archive.PartCount))
	}

	return entry, warnings, nil
}

// placement is the outcome of encoding one file and scattering its parts: what
// landed, where, and under which key generation.
type placement struct {
	archiveID    string
	keyID        string
	originalHash [32]byte
	shards       []Shard
	warnings     []string
}

// scatter encodes data into encrypted parts under the vault's active data key
// and places them across the connected accounts.
//
// It is the half of an upload that touches the network, and re-encrypting a
// file after a password change is the same operation with a different reason:
// both hand it plaintext and get back the parts that now hold it. Whatever it
// records in the returned shards is really on the accounts — on too few parts
// landing it erases the ones that did and returns an error, so a caller never
// has to clean up after a failure of its own.
func (v *Vault) scatter(ctx context.Context, name string, data []byte) (placement, error) {
	// Snapshot everything the transfer needs, then release the lock: the
	// network round-trips below must not block browsing.
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return placement{}, ErrLocked
	}
	out := placement{keyID: v.dataKeyID}
	shardPassword := v.shardPasswordLocked()
	policy := v.store.Policy
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	if len(configs) == 0 {
		return out, fmt.Errorf("connect at least one cloud account before uploading")
	}

	encoded, err := archive.EncodeBytes(data, name, shardPassword)
	if err != nil {
		return out, err
	}
	out.archiveID = hex.EncodeToString(encoded.ArchiveID[:])
	out.originalHash = encoded.OriginalHash

	byID := make(map[string]provider.Config, len(configs))
	ids := make([]string, 0, len(configs))
	for _, cfg := range configs {
		ids = append(ids, cfg.ID)
		byID[cfg.ID] = cfg
	}

	// Seeding from the archive ID rotates which account receives part 1, so
	// load spreads evenly instead of always landing on the first account.
	seed := binary.BigEndian.Uint64(encoded.ArchiveID[:8])
	plan, err := BuildPlan(ids, policy, seed)
	if err != nil {
		return out, err
	}

	type putResult struct {
		shard Shard
		err   error
	}

	results := make(chan putResult, len(plan))
	var wg sync.WaitGroup

	for part, providerID := range plan {
		cfg := byID[providerID]
		blob := encoded.Parts[part-1]
		key := ShardKey(out.archiveID, part)

		wg.Add(1)
		go func(part int, cfg provider.Config, key string, blob []byte) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err != nil {
				results <- putResult{err: fmt.Errorf("part %d → %s: %w", part, cfg.Name, err)}
				return
			}
			if err := p.Put(ctx, key, blob); err != nil {
				results <- putResult{err: fmt.Errorf("part %d → %s: %w", part, cfg.Name, err)}
				return
			}
			results <- putResult{shard: Shard{
				Part:         part,
				ProviderID:   cfg.ID,
				ProviderName: cfg.Name,
				ProviderKind: string(cfg.Kind),
				Key:          key,
				Size:         int64(len(blob)),
			}}
		}(part, cfg, key, blob)
	}

	wg.Wait()
	close(results)

	for r := range results {
		if r.err != nil {
			out.warnings = append(out.warnings, r.err.Error())
			continue
		}
		out.shards = append(out.shards, r.shard)
	}
	sort.Slice(out.shards, func(i, j int) bool { return out.shards[i].Part < out.shards[j].Part })

	if len(out.shards) < archive.MinPartsToRestore {
		// Not enough parts landed to ever rebuild the file — undo the ones
		// that did rather than leaving orphaned blobs on people's accounts.
		v.deleteShards(context.WithoutCancel(ctx), out.shards)
		err := fmt.Errorf("stored only %d of %d parts, need at least %d: %s",
			len(out.shards), archive.PartCount, archive.MinPartsToRestore,
			strings.Join(out.warnings, "; "))
		out.shards = nil
		return out, err
	}

	return out, nil
}

// Fetch gathers enough parts to rebuild a file, decrypts it, and returns the
// original bytes. It reads from every account holding a part at once and
// stops as soon as the minimum number of parts have arrived, so one slow or
// offline account does not hold up the download.
func (v *Vault) Fetch(ctx context.Context, id string) ([]byte, *Entry, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, nil, ErrLocked
	}
	entry := v.manifest.ByID(id)
	if entry == nil {
		v.mu.RUnlock()
		return nil, nil, fmt.Errorf("no such file: %s", id)
	}
	// Not necessarily the key new uploads use: a file waiting its turn in a
	// password change's re-encryption is still sealed under the old one.
	shardPassword, err := v.shardPasswordForLocked(entry.KeyID)
	configs := v.configsForLocked(entry.Shards)
	v.mu.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", entry.Path(), err)
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type getResult struct {
		part int
		blob []byte
		err  error
	}
	results := make(chan getResult, len(entry.Shards))

	for _, shard := range entry.Shards {
		cfg, ok := configs[shard.ProviderID]
		if !ok {
			results <- getResult{part: shard.Part, err: fmt.Errorf(
				"part %d: account %q is no longer connected", shard.Part, shard.ProviderName)}
			continue
		}

		go func(shard Shard, cfg provider.Config) {
			p, err := v.buildProvider(cfg)
			if err != nil {
				results <- getResult{part: shard.Part, err: fmt.Errorf("part %d: %w", shard.Part, err)}
				return
			}
			blob, err := p.Get(fetchCtx, shard.Key)
			if err != nil {
				results <- getResult{part: shard.Part, err: fmt.Errorf(
					"part %d from %s: %w", shard.Part, cfg.Name, err)}
				return
			}
			results <- getResult{part: shard.Part, blob: blob}
		}(shard, cfg)
	}

	var blobs [][]byte
	var failures []string
	for i := 0; i < len(entry.Shards); i++ {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		blobs = append(blobs, r.blob)
		if len(blobs) >= archive.MinPartsToRestore {
			break
		}
	}

	if len(blobs) < archive.MinPartsToRestore {
		return nil, nil, fmt.Errorf(
			"could not gather %d parts for %s (got %d): %s",
			archive.MinPartsToRestore, entry.Path(), len(blobs), strings.Join(failures, "; "))
	}

	decoded, err := archive.DecodeBytes(blobs, shardPassword)
	if err != nil {
		return nil, nil, fmt.Errorf("rebuilding %s: %w", entry.Path(), err)
	}
	return decoded.Data, entry, nil
}

// Delete removes a file from the index and erases its parts from every
// account. Provider failures are returned as warnings: the index entry is
// dropped either way so a dead account cannot pin a file in the browser
// forever.
func (v *Vault) Delete(ctx context.Context, id string) ([]string, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	entry := v.manifest.ByID(id)
	if entry == nil {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	shards := append([]Shard(nil), entry.Shards...)
	v.mu.RUnlock()

	warnings := v.deleteShards(ctx, shards)

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return warnings, ErrLocked
	}
	v.manifest.remove(id)
	return warnings, v.persistLocked()
}

// Rmdir removes a folder. Without recursive it refuses to touch a folder that
// still has contents.
func (v *Vault) Rmdir(ctx context.Context, dir string, recursive bool) ([]string, error) {
	dir = CleanDir(dir)
	if dir == "/" {
		return nil, fmt.Errorf("cannot remove the root folder")
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	if !v.manifest.FolderExists(dir) {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such folder: %s", dir)
	}
	doomed := v.manifest.Descendants(dir)
	subfolders, files := v.manifest.Children(dir)
	v.mu.RUnlock()

	if !recursive && (len(subfolders) > 0 || len(files) > 0) {
		return nil, fmt.Errorf("%s is not empty", dir)
	}

	var warnings []string
	var ids []string
	for _, e := range doomed {
		warnings = append(warnings, v.deleteShards(ctx, e.Shards)...)
		ids = append(ids, e.ID)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return warnings, ErrLocked
	}
	for _, id := range ids {
		v.manifest.remove(id)
	}
	v.manifest.removeFolders(dir)
	return warnings, v.persistLocked()
}

// deleteShards erases a set of parts, returning a warning per failure.
func (v *Vault) deleteShards(ctx context.Context, shards []Shard) []string {
	if len(shards) == 0 {
		return nil
	}

	v.mu.RLock()
	locked := v.dataKey == nil
	configs := v.configsForLocked(shards)
	v.mu.RUnlock()
	if locked {
		return []string{"vault locked before parts could be erased"}
	}

	var mu sync.Mutex
	var warnings []string
	var wg sync.WaitGroup

	for _, shard := range shards {
		cfg, ok := configs[shard.ProviderID]
		if !ok {
			mu.Lock()
			warnings = append(warnings, fmt.Sprintf(
				"part %d left behind: account %q is no longer connected", shard.Part, shard.ProviderName))
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(shard Shard, cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err == nil {
				err = p.Delete(ctx, shard.Key)
			}
			if err != nil {
				mu.Lock()
				warnings = append(warnings, fmt.Sprintf(
					"part %d on %s: %v", shard.Part, cfg.Name, err))
				mu.Unlock()
			}
		}(shard, cfg)
	}

	wg.Wait()
	return warnings
}

// ShardHealth is the observed state of one stored part.
type ShardHealth struct {
	Shard
	Present bool   `json:"present"`
	Size    int64  `json:"observed_size"`
	Error   string `json:"error,omitempty"`
}

// FileHealth reports whether a file can still be rebuilt.
type FileHealth struct {
	ID          string        `json:"id"`
	Path        string        `json:"path"`
	Recoverable bool          `json:"recoverable"`
	Shards      []ShardHealth `json:"shards"`
}

// Health checks every part of a file against the account holding it, without
// downloading any data.
func (v *Vault) Health(ctx context.Context, id string) (*FileHealth, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	entry := v.manifest.ByID(id)
	if entry == nil {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	shards := append([]Shard(nil), entry.Shards...)
	path := entry.Path()
	configs := v.configsForLocked(shards)
	v.mu.RUnlock()

	health := &FileHealth{ID: id, Path: path, Shards: make([]ShardHealth, len(shards))}

	var wg sync.WaitGroup
	for i, shard := range shards {
		health.Shards[i] = ShardHealth{Shard: shard}

		cfg, ok := configs[shard.ProviderID]
		if !ok {
			health.Shards[i].Error = fmt.Sprintf("account %q is no longer connected", shard.ProviderName)
			continue
		}

		wg.Add(1)
		go func(i int, shard Shard, cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err != nil {
				health.Shards[i].Error = err.Error()
				return
			}
			info, err := p.Stat(ctx, shard.Key)
			if err != nil {
				health.Shards[i].Error = err.Error()
				return
			}
			health.Shards[i].Present = true
			health.Shards[i].Size = info.Size
		}(i, shard, cfg)
	}
	wg.Wait()

	present := 0
	for _, s := range health.Shards {
		if s.Present {
			present++
		}
	}
	health.Recoverable = present >= archive.MinPartsToRestore
	return health, nil
}
