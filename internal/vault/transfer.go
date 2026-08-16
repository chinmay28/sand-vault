package vault

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// UploadOptions is everything about an upload except the bytes themselves.
type UploadOptions struct {
	// Overwrite replaces a file of the same name in the destination folder
	// instead of storing this one beside it under a numbered name.
	Overwrite bool

	// Accounts names the connected accounts this file's parts should go to,
	// overriding the vault's default for this one upload. Empty hands the
	// choice to the default, and failing that to a random pick.
	//
	// It is honoured exactly, as a stored default is: naming two accounts
	// stores two parts and warns about the missing spare, because a deliberate
	// choice of which clouds may hold a file must not be widened behind the
	// user's back — that is the whole thing SAND is for.
	Accounts []string
}

// Upload encodes data into encrypted parts, scatters them across the accounts
// chosen for the file according to the vault's placement policy, and records
// the result in the index.
//
// It returns the new entry plus any non-fatal warnings, such as one account
// being unreachable while enough others accepted their part.
func (v *Vault) Upload(ctx context.Context, dir, name string, data []byte, opts UploadOptions) (*Entry, []string, error) {
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

	// Either the upload's own choice or, left to itself, the vault's default —
	// and whichever it turns out to be is followed exactly. Only a vault with
	// neither picks accounts of its own, inside the scatter.
	placed, err := v.scatterChunked(ctx, name, data, opts.Accounts, true, v.uploadChunkSize())
	if err != nil {
		return nil, placed.warnings, err
	}
	return v.commitUpload(ctx, dir, name, int64(len(data)), DetectMIME(name, data), placed, opts)
}

// resolveAccounts turns an explicit account selection into the list of IDs to
// place across. An account that is not connected is an error rather than
// something to skip over: quietly narrowing a chosen set would put the file on
// fewer accounts than the person choosing believed it was going to.
func resolveAccounts(selected []string, byID map[string]provider.Config) ([]string, error) {
	out := make([]string, 0, len(selected))
	seen := map[string]bool{}
	for _, id := range selected {
		cfg, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("no connected account with id %s", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("%s is listed twice — each part of a file goes to a different account", cfg.Name)
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > AccountsPerFile {
		return nil, fmt.Errorf("a file has only %d parts — choose at most %d accounts (got %d)",
			archive.PartCount, AccountsPerFile, len(out))
	}
	return out, nil
}

// placement is the outcome of encoding one file and scattering its parts: what
// landed, where, and under which key generation.
type placement struct {
	archiveID    string
	keyID        string
	originalHash [32]byte
	shards       []Shard
	warnings     []string

	// chunkSize and chunkCount describe how the file was cut up, and are set
	// only by the chunked path. Zero means the file was stored whole.
	chunkSize  int64
	chunkCount int
}

// transferTarget is the snapshot a scatter works from: the keys in force and
// the accounts in play, taken from the vault in one pass under the lock so that
// the network round-trips which follow can run without holding it (§13).
//
// The two scatter paths share it because they must make the same decision.
// Which accounts may hold a file is the question SAND exists to answer, and a
// chunked upload has to answer it once for the file rather than once per chunk
// — otherwise a large file would drift across every connected account as it
// uploaded, which is precisely what the placement policy forbids.
type transferTarget struct {
	keyID         string
	shardPassword string
	dataKey       []byte
	policy        Policy
	ids           []string
	byID          map[string]provider.Config
	preferred     []string

	// chosen is set only when the caller named accounts exactly, in which case
	// it is the whole answer and nothing is added to it.
	chosen []string
}

// snapshotTarget takes what a scatter needs from the vault and settles which
// accounts are in play, before anything is encoded: a selection naming an
// account that is not connected is a mistake to report, not work to do.
func (v *Vault) snapshotTarget(preferred []string, exact bool) (*transferTarget, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	t := &transferTarget{
		keyID:         v.dataKeyID,
		shardPassword: v.shardPasswordLocked(),
		dataKey:       append([]byte(nil), v.dataKey...),
		policy:        v.store.Policy,
	}
	defaults := append([]string(nil), v.store.DefaultAccounts...)
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	if len(configs) == 0 {
		return nil, fmt.Errorf("connect at least one cloud account before uploading")
	}

	t.byID = make(map[string]provider.Config, len(configs))
	t.ids = make([]string, 0, len(configs))
	for _, cfg := range configs {
		t.ids = append(t.ids, cfg.ID)
		t.byID[cfg.ID] = cfg
	}

	t.preferred = preferred
	if len(t.preferred) == 0 {
		// Nothing was chosen for this file, so the vault's standing answer
		// applies. Anything in it that has since been disconnected is dropped
		// rather than failing the upload: it is a leftover, not a request.
		for _, id := range defaults {
			if _, ok := t.byID[id]; ok {
				t.preferred = append(t.preferred, id)
			}
		}
	}

	if exact && len(t.preferred) > 0 {
		var err error
		if t.chosen, err = resolveAccounts(t.preferred, t.byID); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// planFor decides which account holds which part. The seed rotates the starting
// account per file; see SelectAccounts and BuildPlan.
func (t *transferTarget) planFor(seed uint64) (Plan, error) {
	chosen := t.chosen
	if chosen == nil {
		chosen = SelectAccounts(t.ids, t.preferred, seed)
	}
	return BuildPlan(chosen, t.policy, seed)
}

// scatter encodes data into encrypted parts under the vault's active data key
// and places them across the accounts chosen for the file.
//
// preferred is where the parts should go. Left empty it falls back to the
// vault's default accounts, and failing that to a random pick — a vault with
// several accounts connected spreads each file over three of them rather than
// always the same three.
//
// With exact set, a preference is the whole answer: nothing is added to it and
// every account in it must still be connected, because it came from someone
// deliberately choosing which clouds may hold this file. Without it a
// preference is a starting point to be filled in from what is connected, which
// is what re-encrypting a file wants — it belongs back where it was, and back
// to three parts if it had lost one.
//
// It is the half of an upload that touches the network, and re-encrypting a
// file after a password change is the same operation with a different reason:
// both hand it plaintext and get back the parts that now hold it. Whatever it
// records in the returned shards is really on the accounts — on too few parts
// landing it erases the ones that did and returns an error, so a caller never
// has to clean up after a failure of its own.
func (v *Vault) scatter(ctx context.Context, name string, data []byte, preferred []string, exact bool) (placement, error) {
	target, err := v.snapshotTarget(preferred, exact)
	if err != nil {
		return placement{}, err
	}
	defer crypto.ZeroBytes(target.dataKey)

	out := placement{keyID: target.keyID}
	byID := target.byID

	encoded, err := archive.EncodeBytes(data, name, target.shardPassword)
	if err != nil {
		return out, err
	}
	out.archiveID = hex.EncodeToString(encoded.ArchiveID[:])
	out.originalHash = encoded.OriginalHash

	// Seeding from the archive ID picks this file's accounts and rotates which
	// of them receives part 1, so load spreads evenly instead of every upload
	// landing the same way on the same accounts.
	seed := binary.BigEndian.Uint64(encoded.ArchiveID[:8])
	plan, err := target.planFor(seed)
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
	snapshot := *entry
	snapshot.Shards = append([]Shard(nil), entry.Shards...)
	v.mu.RUnlock()

	if snapshot.Chunked() {
		data, err := v.gatherChunkedFile(ctx, &snapshot)
		if err != nil {
			return nil, nil, err
		}
		return data, entry, nil
	}

	data, err := v.gather(ctx, snapshot.Shards, snapshot.KeyID, snapshot.Path())
	if err != nil {
		return nil, nil, err
	}
	return data, entry, nil
}

// gather collects enough of a set of parts to rebuild what they encode and
// decrypts it. It is the read half of scatter, and everything the vault stores
// goes through it — a file's parts and a folder's thumbnail pack alike.
//
// label names the thing being read, for error messages: a path for a file, a
// description for anything else.
func (v *Vault) gather(ctx context.Context, shards []Shard, keyID, label string) ([]byte, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	// Not necessarily the key new uploads use: a file waiting its turn in a
	// password change's re-encryption is still sealed under the old one.
	shardPassword, err := v.shardPasswordForLocked(keyID)
	configs := v.configsForLocked(shards)
	v.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", label, err)
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type getResult struct {
		part int
		blob []byte
		err  error
	}
	results := make(chan getResult, len(shards))

	for _, shard := range shards {
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
	for i := 0; i < len(shards); i++ {
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
		return nil, fmt.Errorf(
			"could not gather %d parts for %s (got %d): %s",
			archive.MinPartsToRestore, label, len(blobs), strings.Join(failures, "; "))
	}

	decoded, err := archive.DecodeBytes(blobs, shardPassword)
	if err != nil {
		return nil, fmt.Errorf("rebuilding %s: %w", label, err)
	}
	return decoded.Data, nil
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
	doomed := *entry
	doomed.Shards = append([]Shard(nil), entry.Shards...)
	dir := entry.Dir
	v.mu.RUnlock()

	warnings := v.deleteEntryShards(ctx, &doomed)

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return warnings, ErrLocked
	}
	v.manifest.remove(id)
	// Whatever film it was matched to goes with it, in the same write: a
	// stored title outliving the file it described would show up as a phantom
	// in nothing at all, but it would sit in the index forever.
	v.manifest.forgetMovies(id)
	err := v.persistLocked()
	v.mu.Unlock()

	if err != nil {
		return warnings, err
	}

	// After the file itself is gone, so a failure to rewrite the pack cannot
	// keep a deleted file in the listing.
	v.removeThumbs(ctx, dir, id)
	return warnings, nil
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
		warnings = append(warnings, v.deleteEntryShards(ctx, e)...)
		ids = append(ids, e.ID)
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return warnings, ErrLocked
	}
	for _, id := range ids {
		v.manifest.remove(id)
	}
	v.manifest.forgetMovies(ids...)
	v.manifest.removeFolders(dir)
	v.manifest.dropMovieFolders(dir)
	err := v.persistLocked()
	v.mu.Unlock()

	if err != nil {
		return warnings, err
	}

	// The folder and everything under it is gone, and so are the thumbnails
	// that were stored a folder at a time.
	v.dropThumbFolders(ctx, dir)
	return warnings, nil
}

// deleteShards erases a set of parts of a file stored whole, returning a
// warning per failure. A chunked file has more than one object per part, so it
// goes through deleteEntryShards instead.
func (v *Vault) deleteShards(ctx context.Context, shards []Shard) []string {
	return v.deleteStoredShards(ctx, "", 0, shards)
}

// deleteEntryShards erases every object an entry occupies, whichever format it
// was stored in.
func (v *Vault) deleteEntryShards(ctx context.Context, entry *Entry) []string {
	return v.deleteStoredShards(ctx, entry.ArchiveID, entry.ChunkCount, entry.Shards)
}

// deleteStoredShards erases the objects a set of parts occupies.
//
// A chunkCount of zero is a file stored whole: one object per part, named by
// the shard itself. Anything higher is a chunked file, where a shard stands for
// one object per chunk — all of which have to go, or disconnecting the account
// later reports space held by a file that no longer exists.
func (v *Vault) deleteStoredShards(ctx context.Context, archiveID string, chunkCount int, shards []Shard) []string {
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
				err = deleteShardObjects(ctx, p, archiveID, chunkCount, shard)
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

// deleteShardObjects erases the one object a whole-file shard names, or every
// chunk's object when the file was stored chunked.
//
// An object that is already gone is not a failure: rollback runs over chunks
// that may never have been written, and erasing twice must be safe.
func deleteShardObjects(ctx context.Context, p provider.Provider, archiveID string, chunkCount int, shard Shard) error {
	if chunkCount <= 0 {
		return p.Delete(ctx, shard.Key)
	}

	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	window := make(chan struct{}, chunkUploadWindow*archive.PartCount)

	for index := 0; index < chunkCount; index++ {
		wg.Add(1)
		window <- struct{}{}
		go func(index int) {
			defer wg.Done()
			defer func() { <-window }()

			err := p.Delete(ctx, ChunkShardKey(archiveID, index, shard.Part))
			if err == nil || errors.Is(err, provider.ErrNotFound) {
				return
			}
			mu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("chunk %d: %w", index, err)
			}
			mu.Unlock()
		}(index)
	}
	wg.Wait()
	return firstErr
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

	// ChunksSampled is how many of a chunked file's chunks were actually
	// checked, and ChunkCount how many it has. They are equal for a small file
	// and for one stored whole; where they differ, the answer is drawn from a
	// sample and Recoverable means "the sampled chunks can be rebuilt".
	ChunksSampled int `json:"chunks_sampled,omitempty"`
	ChunkCount    int `json:"chunk_count,omitempty"`
}

// healthSampleLimit caps how many chunks one health check stats per part.
//
// Answering exactly would mean a request per chunk per part — for a 40 GB video
// that is thousands of round trips to draw one row of badges, which is not a
// question a file listing can afford to ask. A part is written to every chunk or
// erased from all of them (scatterChunked), so a handful of chunks spread across
// the file catches a part that never landed as well as a full scan would. What
// it can miss is damage done later to the middle of a file by something other
// than SAND; `sand check --all` is the exhaustive answer.
const healthSampleLimit = 4

// Health checks a file's parts against the accounts holding them, without
// downloading any data. For a chunked file it samples chunks rather than
// walking all of them — see healthSampleLimit.
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
	path, archiveID := entry.Path(), entry.ArchiveID
	chunked, chunkCount := entry.Chunked(), entry.ChunkCount
	configs := v.configsForLocked(shards)
	v.mu.RUnlock()

	sample := sampleChunks(chunked, chunkCount)
	health := &FileHealth{
		ID: id, Path: path,
		Shards:        make([]ShardHealth, len(shards)),
		ChunksSampled: len(sample),
	}
	if chunked {
		health.ChunkCount = chunkCount
	}

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

			// A part counts as present only if every sampled chunk has it: one
			// missing chunk is enough to make the part useless for that stretch
			// of the file.
			var total int64
			for _, index := range sample {
				key := shard.Key
				if chunked {
					key = ChunkShardKey(archiveID, index, shard.Part)
				}
				info, err := p.Stat(ctx, key)
				if err != nil {
					health.Shards[i].Error = err.Error()
					return
				}
				total += info.Size
			}
			health.Shards[i].Present = true
			// For a chunked file this is what the sample weighed, not the whole
			// part; the recorded shard size is the figure for that.
			health.Shards[i].Size = total
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

// sampleChunks picks which chunks a health check stats: all of them for a small
// file, otherwise the first, the last, and an even spread between, so that a
// part missing from one end is not missed.
func sampleChunks(chunked bool, chunkCount int) []int {
	if !chunked || chunkCount <= 1 {
		return []int{0}
	}
	if chunkCount <= healthSampleLimit {
		all := make([]int, chunkCount)
		for i := range all {
			all[i] = i
		}
		return all
	}

	sample := make([]int, healthSampleLimit)
	last := healthSampleLimit - 1
	for i := range sample {
		sample[i] = i * (chunkCount - 1) / last
	}
	return sample
}
