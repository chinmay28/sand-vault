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
	"time"

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
	// stores two shards and warns about the missing spare, because a deliberate
	// choice of which clouds may hold a file must not be widened behind the
	// user's back — that is the whole thing SAND is for.
	//
	// How many accounts are named is also what settles the erasure code, unless
	// Scheme says otherwise: three is 2-of-3, six is 4-of-6, nine is 6-of-9
	// (archive.Scheme).
	Accounts []string

	// Scheme is the erasure code this one file is cut with, in place of the one
	// its account count would name. The zero value means no choice was made and
	// the count settles it, which is what almost every upload wants.
	//
	// Naming one is how a file gets a code outside the default family — 3-of-5
	// on five clouds, 6-of-10 on ten — and how a file gets a different tradeoff
	// from its neighbours on the same accounts. It is per file rather than per
	// vault because that is where the tradeoff lives: k is how many accounts an
	// attacker needs together, and a folder of holiday photos and a folder of
	// tax records do not have to answer that question the same way.
	Scheme archive.Scheme

	// OnScattered, when set, is called as the file's bytes leave for the
	// accounts, with how many have gone out of how many there are.
	//
	// It counts bytes read out of the spool on their way to being sealed, which
	// runs at most a chunk window ahead of what has actually landed — tens of
	// megabytes on a file worth watching, and the alternative is a bar that
	// only moves when a whole chunk finishes. It is called from the goroutine
	// driving the upload and must not block.
	//
	// Only the paths that stream honour it: an upload with no reader behind it
	// has nothing to report between "started" and "done".
	OnScattered func(done, size int64)
}

// Upload encodes data into encrypted parts, scatters them across the accounts
// chosen for the file according to the vault's placement policy, and records
// the result in the index.
//
// It returns the new entry plus any non-fatal warnings, such as one account
// being unreachable while enough others accepted their part.
func (v *Vault) Upload(ctx context.Context, scope Scope, dir, name string, data []byte, opts UploadOptions) (*Entry, []string, error) {
	name, err := SanitizeName(name)
	if err != nil {
		return nil, nil, err
	}

	dir, err = v.destinationLocked(scope, dir)
	if err != nil {
		return nil, nil, err
	}

	// Either the upload's own choice or, left to itself, the vault's default —
	// and whichever it turns out to be is followed exactly. Only a vault with
	// neither picks accounts of its own, inside the scatter.
	placed, err := v.scatterChunked(ctx, scope, name, data,
		spread{preferred: opts.Accounts, exact: true, scheme: opts.Scheme}, v.uploadChunkSize())
	if err != nil {
		return nil, placed.warnings, err
	}
	return v.commitUpload(ctx, scope, dir, name, int64(len(data)), DetectMIME(name, data), placed, opts)
}

// spread is what a scatter is told about where a file goes and how it is cut.
//
// The two travel together because neither settles anything on its own: without
// a scheme it is the count of accounts that names the code, and with one it is
// the scheme that says how many accounts the file wants to be on. Every path
// that writes shards — an upload, a re-encryption, a recode onto other clouds —
// hands one of these to snapshotTarget.
type spread struct {
	// preferred is where the parts should go. Left empty it falls back to the
	// vault's default accounts, and failing that to a random pick.
	preferred []string

	// exact makes a preference the whole answer: nothing is added to it and
	// every account in it must still be connected, because it came from someone
	// deliberately choosing which clouds may hold this file.
	exact bool

	// scheme is the code to cut with. The zero value leaves it to be settled by
	// how many accounts are chosen, which is what an upload that named none
	// wants; a file being re-encrypted passes the code it is already cut with,
	// so that a password change does not quietly recode it.
	scheme archive.Scheme
}

// resolveAccounts turns an explicit account selection into the list of IDs to
// place across. An account that is not connected is an error rather than
// something to skip over: quietly narrowing a chosen set would put the file on
// fewer accounts than the person choosing believed it was going to.
//
// scheme, when non-zero, is the code the file will be cut with, and it is what
// the count is then checked against — a count that names no scheme of its own
// is fine once a scheme has been named for it.
func resolveAccounts(selected []string, byID map[string]provider.Config, scheme archive.Scheme) ([]string, error) {
	out := make([]string, 0, len(selected))
	seen := map[string]bool{}
	for _, id := range selected {
		cfg, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("no connected account with id %s", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("%s is listed twice — every shard of a file goes to a different account", cfg.Name)
		}
		seen[id] = true
		out = append(out, id)
	}
	if scheme != (archive.Scheme{}) {
		if err := checkSpread(len(out), scheme); err != nil {
			return nil, err
		}
		return out, nil
	}
	if !ValidSpread(len(out)) {
		return nil, ErrSpread(len(out))
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
	scheme       archive.Scheme
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

	// scheme is the code named for this file, or the zero value when nothing
	// was named for it. See spread.
	scheme archive.Scheme

	// fallback is the vault's own default code, applied only where it fits the
	// accounts a file actually lands on. See schemeFor.
	fallback archive.Scheme
}

// schemeFor settles which code a file going to n accounts is cut with.
//
// Three answers in order of how deliberate they are. A scheme named for this
// file wins outright, because somebody chose it for these bytes. The vault's
// default comes next, and only where it fits — a default of 3-of-5 is a
// statement about five accounts and has nothing to say about a file
// deliberately sent to six, which is 4-of-6 as it would have been with no
// default set at all. Failing both, the count settles it, which is what every
// upload did before a scheme could be named.
func (t *transferTarget) schemeFor(n int) (archive.Scheme, error) {
	if t.scheme != (archive.Scheme{}) {
		return t.scheme, nil
	}
	if t.fallback != (archive.Scheme{}) && t.fallback.Total == n {
		return t.fallback, nil
	}
	return SchemeFor(n)
}

// width is how many accounts a file wants to be on before anything has been
// chosen for it: the width of whichever code is going to cut it, or zero to
// leave it to the default family's rounding.
func (t *transferTarget) width() int {
	if t.scheme != (archive.Scheme{}) {
		return t.scheme.Total
	}
	if t.fallback != (archive.Scheme{}) {
		return t.fallback.Total
	}
	return 0
}

// snapshotTarget takes what a scatter needs from the vault and settles which
// accounts are in play, before anything is encoded: a selection naming an
// account that is not connected is a mistake to report, not work to do.
// The scope decides one thing and one thing only: which data key the parts are
// sealed under. Where they are allowed to land is a vault-wide question —
// the policy, the connected accounts and the default selection are the same
// whichever vault inside the file the bytes belong to, because they are answers
// about storage rather than about secrecy.
func (v *Vault) snapshotTarget(scope Scope, sp spread) (*transferTarget, error) {
	if sp.scheme != (archive.Scheme{}) {
		if err := sp.scheme.Check(); err != nil {
			return nil, err
		}
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	keyID, err := v.dataKeyIDForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	dataKey, err := v.dataKeyForLocked(keyID)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	t := &transferTarget{
		keyID:         keyID,
		shardPassword: shardPasswordFor(dataKey),
		dataKey:       dataKey,
		policy:        v.store.Policy,
		scheme:        sp.scheme,
		fallback:      v.defaultSchemeLocked(),
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

	t.preferred = sp.preferred
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

	if sp.exact && len(t.preferred) > 0 {
		// The vault's default makes a count like five a spread that five
		// accounts would not otherwise be, so it has to be in hand before the
		// count is judged. It applies only where it fits.
		against := t.scheme
		if against == (archive.Scheme{}) && t.fallback.Total == len(t.preferred) {
			against = t.fallback
		}
		chosen, err := resolveAccounts(t.preferred, t.byID, against)
		if err != nil {
			return nil, err
		}
		// The accounts are settled, so the code is too — and a count that names
		// none, with nothing else naming one either, is a mistake to report
		// before anything is encoded rather than after.
		if _, err := t.schemeFor(len(chosen)); err != nil {
			return nil, err
		}
		t.chosen = chosen
	}
	return t, nil
}

// planFor decides which account holds which shard, and which code the file is
// cut with — the second follows from how many accounts the first settled on.
// The seed rotates the starting account per file; see SelectAccounts and
// BuildPlan.
func (t *transferTarget) planFor(seed uint64) (Plan, archive.Scheme, error) {
	chosen := t.chosen
	if chosen == nil {
		// A code that is going to cut this file says how many accounts to end
		// up on; without one the width is the default family's, rounded up from
		// what the file already prefers.
		chosen = SelectAccounts(t.ids, t.preferred, t.width(), seed)
	}

	scheme, err := t.schemeFor(len(chosen))
	if err != nil {
		return nil, scheme, err
	}
	plan, err := BuildPlan(chosen, t.policy, scheme, seed)
	return plan, scheme, err
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
func (v *Vault) scatter(ctx context.Context, scope Scope, name string, data []byte, sp spread) (placement, error) {
	target, err := v.snapshotTarget(scope, sp)
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
	plan, scheme, err := target.planFor(seed)
	if err != nil {
		return out, err
	}
	// EncodeBytes writes the whole-file format, which predates schemes and can
	// only ever produce the default code's three parts. The one caller left on
	// this path narrows its account list to match; anything else is a bug here
	// rather than something to paper over at scatter time.
	if scheme != archive.SchemeDefault {
		return out, fmt.Errorf(
			"the whole-file format is %s only, cannot scatter as %s", archive.SchemeDefault, scheme)
	}
	out.scheme = scheme

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
	sortShards(out.shards)

	if distinctShards(out.shards) < scheme.Data {
		// Not enough shards landed to ever rebuild the file — undo the ones
		// that did rather than leaving orphaned blobs on people's accounts.
		v.deleteShards(context.WithoutCancel(ctx), out.shards)
		err := fmt.Errorf("stored only %d of %d shards, need at least %d: %s",
			distinctShards(out.shards), scheme.Total, scheme.Data,
			strings.Join(out.warnings, "; "))
		out.shards = nil
		return out, err
	}

	return out, nil
}

// sortShards puts a file's stored objects in shard order, so that two runs of
// the same scatter record the same index rows whatever order the network
// answered in.
func sortShards(shards []Shard) {
	sort.Slice(shards, func(i, j int) bool { return shards[i].Part < shards[j].Part })
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
	_, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
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

	data, err := v.gather(ctx, snapshot.Shards, snapshot.Scheme(), snapshot.KeyID, snapshot.Path())
	if err != nil {
		return nil, nil, err
	}
	return data, entry, nil
}

// gather collects enough of a set of parts to rebuild what they encode and
// decrypts it. It is the read half of scatter, and everything the vault stores
// goes through it — a file's parts and a folder's thumbnail pack alike.
//
// It races the accounts and takes the first Scheme.Data *distinct* shards to
// answer, which is what makes a wider code faster to read as well as harder to
// lose: 4-of-6 reads from whichever four of the six clouds are quickest today,
// and the two slowest never enter into it.
//
// label names the thing being read, for error messages: a path for a file, a
// description for anything else.
func (v *Vault) gather(ctx context.Context, shards []Shard, scheme archive.Scheme, keyID, label string) ([]byte, error) {
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

	// Buffered for every shard, so an account that loses the race can still
	// put down what it found and go: nothing here blocks on a reader that has
	// already been served, and the tail of the race is recorded off to one
	// side rather than waited for. See readstats.go.
	results := make(chan shardFetch, len(shards))
	v.reads.race()

	for _, shard := range shards {
		cfg, ok := configs[shard.ProviderID]
		if !ok {
			results <- shardFetch{shard: shard, err: fmt.Errorf(
				"part %d: account %q is no longer connected", shard.Part, shard.ProviderName)}
			continue
		}

		go func(shard Shard, cfg provider.Config) {
			started := time.Now()
			p, err := v.buildProvider(cfg)
			if err != nil {
				results <- shardFetch{shard: shard, took: time.Since(started),
					err: fmt.Errorf("part %d: %w", shard.Part, err)}
				return
			}
			blob, err := p.Get(fetchCtx, shard.Key)
			if err != nil {
				results <- shardFetch{shard: shard, took: time.Since(started), err: fmt.Errorf(
					"part %d from %s: %w", shard.Part, cfg.Name, err)}
				return
			}
			results <- shardFetch{shard: shard, blob: blob, took: time.Since(started)}
		}(shard, cfg)
	}

	held := map[int][]byte{}
	var failures []string
	taken := 0
	for i := 0; i < len(shards); i++ {
		r := <-results
		taken++
		if r.err != nil {
			v.reads.record(r, lostOutcome(r))
			failures = append(failures, r.err.Error())
			continue
		}
		if _, already := held[r.shard.Part]; already {
			v.reads.record(r, shardLate)
			continue
		}
		held[r.shard.Part] = r.blob
		v.reads.record(r, shardWon)
		if len(held) >= scheme.Data {
			break
		}
	}
	// Whatever is still in flight now is about to be cancelled by the deferred
	// cancel above. What each of those accounts does with that — answers
	// anyway, gives up, or was already failing — is worth recording and is not
	// worth waiting for.
	v.reads.drainLater(results, len(shards)-taken)

	if len(held) < scheme.Data {
		v.reads.shortfall()
		return nil, fmt.Errorf(
			"could not gather %d shards for %s (got %d): %s",
			scheme.Data, label, len(held), strings.Join(failures, "; "))
	}

	decoded, err := archive.DecodeBytes(collectParts(held), shardPassword)
	if err != nil {
		return nil, fmt.Errorf("rebuilding %s: %w", label, err)
	}
	return decoded.Data, nil
}

// collectParts hands the gathered blobs to the decoder in part order. The
// decoder reads each blob's own header to know which part it is, so the order
// is for reproducibility rather than for correctness.
func collectParts(held map[int][]byte) [][]byte {
	parts := make([]int, 0, len(held))
	for part := range held {
		parts = append(parts, part)
	}
	sort.Ints(parts)

	out := make([][]byte, 0, len(held))
	for _, part := range parts {
		out = append(out, held[part])
	}
	return out
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
	scope, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	doomed := *entry
	doomed.Shards = append([]Shard(nil), entry.Shards...)
	dir := entry.Dir
	v.mu.RUnlock()

	warnings := v.deleteEntryShards(ctx, &doomed)

	v.mu.Lock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.Unlock()
		return warnings, err
	}
	m.remove(id)
	// Whatever film it was matched to goes with it, in the same write: a
	// stored title outliving the file it described would show up as a phantom
	// in nothing at all, but it would sit in the index forever.
	m.forgetMovies(id)
	m.forgetRepos(id)
	// And any folder that was told to wear this file's picture goes back to
	// choosing one for itself, rather than pointing at a file that is gone.
	m.forgetFolderArt(id)
	err = v.persistLocked()
	v.mu.Unlock()

	if err != nil {
		return warnings, err
	}

	// After the file itself is gone, so a failure to rewrite the pack cannot
	// keep a deleted file in the listing.
	v.removeThumbs(ctx, scope, dir, id)
	return warnings, nil
}

// eraseWindow is how many doomed files have their parts erased at once, by
// Rmdir and DeleteMany alike.
//
// One at a time is what made a big delete slow: each file already erases its
// own parts in parallel, but a file's round is over only when the slowest of
// its accounts has answered, and a folder of three hundred files paid that
// worst-of-three latency three hundred times in a row. A few files abreast
// overlaps the waits without turning the delete into the burst of requests
// per account that gets rate-limited.
const eraseWindow = 4

// eraseEntries erases the parts of the entries given, in order and a few
// abreast, and returns how many it got to along with a warning per part that
// could not be erased. It touches no index: the caller takes the attempted
// entries out of the manifest afterwards, in one write, whatever came of
// their parts — a dead account must not pin a file in the browser forever.
//
// ctx done means stop: no further file is started, the ones in flight are
// allowed to finish their round rather than be left half erased, and the
// count returned is what the caller may take out of the index. Everything
// after it is exactly as it was.
//
// onProgress, when given, is told how many files are done out of how many:
// once with (0, total) before the erasing starts, then once per file, in
// order. It is a window for whoever is waiting, nothing more.
func (v *Vault) eraseEntries(ctx context.Context, doomed []*Entry, onProgress func(done, total int)) (attempted int, warnings []string) {
	if onProgress != nil {
		onProgress(0, len(doomed))
	}

	// A stop is "start no more", not "drop everything": a file whose round
	// has begun finishes it, or it would be gone from the index with parts
	// still on the accounts for the orphan sweep to find later.
	inflight := context.WithoutCancel(ctx)

	var mu sync.Mutex
	var wg sync.WaitGroup
	finished := 0
	window := make(chan struct{}, eraseWindow)
	for _, e := range doomed {
		if ctx.Err() != nil {
			break
		}
		attempted++
		wg.Add(1)
		window <- struct{}{}
		go func(e *Entry) {
			defer wg.Done()
			defer func() { <-window }()
			found := v.deleteEntryShards(inflight, e)
			mu.Lock()
			warnings = append(warnings, found...)
			finished++
			// Under the lock, so the counts leave in the order they were
			// taken — two goroutines reporting outside it could hand a
			// watcher 2 and then 1, and a bar that steps backwards reads as
			// a bug in whatever is drawing it.
			if onProgress != nil {
				onProgress(finished, len(doomed))
			}
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	return attempted, warnings
}

// DeleteReport is what DeleteMany came to.
type DeleteReport struct {
	// Deleted counts the files taken out of the index. Every part of each was
	// asked for on every account holding one; any that could not be erased
	// are in Warnings, and the file is gone from the listing either way.
	Deleted int `json:"deleted"`

	// Missing lists the IDs that named no file. Most often that is a batch
	// tried again after a failure part-way through: the files it already took
	// are not an error the second time.
	Missing []string `json:"missing"`

	// Warnings are parts left behind on accounts, one line each, naming the
	// file, the part and the account.
	Warnings []string `json:"warnings"`
}

// DeleteMany removes a set of files, wherever in the vault they sit, in one
// index write.
//
// Delete, one file at a time, pays the full cost of a change on every call:
// a round of erasures bounded by the slowest account, then the whole index
// re-sealed and written to disk, then the folder's thumbnail pack rewritten.
// Seven thousand duplicates deleted that way is seven thousand of each. Here
// the parts are erased a few files abreast (see eraseWindow), the index is
// written once for the lot, and each folder's thumbnail pack is rewritten
// once, however many of its files went.
//
// An ID given twice is one file, deleted once. An ID naming nothing is
// reported in Missing rather than failing the batch, and the rest go ahead.
//
// ctx done part-way is a stop rather than a failure: no further file is
// started, the ones in flight finish, the index is written for exactly the
// files that were erased — Deleted says how many, in the order given — and
// the error returned is ctx.Err(), so the caller can tell a batch cut short
// from one that ran out. The files after it are untouched. Any other error
// is for the vault as a whole — locked, or the index could not be written —
// never for one file.
func (v *Vault) DeleteMany(ctx context.Context, ids []string, onProgress func(done, total int)) (*DeleteReport, error) {
	report := &DeleteReport{Missing: []string{}, Warnings: []string{}}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	seen := make(map[string]bool, len(ids))
	doomed := make([]*Entry, 0, len(ids))
	scopes := make([]Scope, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		scope, entry, ok := v.scopeOfEntryLocked(id)
		if !ok {
			report.Missing = append(report.Missing, id)
			continue
		}
		// A copy, as Delete takes: the erasing runs outside the lock, and the
		// manifest must not be read while it does.
		copied := *entry
		copied.Shards = append([]Shard(nil), entry.Shards...)
		doomed = append(doomed, &copied)
		scopes = append(scopes, scope)
	}
	v.mu.RUnlock()

	attempted, warnings := v.eraseEntries(ctx, doomed, onProgress)
	report.Warnings = append(report.Warnings, warnings...)
	report.Deleted = attempted
	stopped := ctx.Err()
	if attempted == 0 {
		return report, stopped
	}

	// Only what was erased leaves the index; a stop leaves the rest exactly
	// where it was, parts and record alike.
	type folder struct {
		scope Scope
		dir   string
	}
	byScope := map[Scope][]string{}
	byFolder := map[folder][]string{}
	for i, e := range doomed[:attempted] {
		byScope[scopes[i]] = append(byScope[scopes[i]], e.ID)
		key := folder{scopes[i], e.Dir}
		byFolder[key] = append(byFolder[key], e.ID)
	}

	v.mu.Lock()
	for scope, ids := range byScope {
		m, err := v.manifestForLocked(scope)
		if err != nil {
			v.mu.Unlock()
			return report, err
		}
		for _, id := range ids {
			m.remove(id)
		}
		// Everything a file's record carried goes with it, in the same write
		// — see Delete for why each of these matters.
		m.forgetMovies(ids...)
		m.forgetRepos(ids...)
		m.forgetFolderArt(ids...)
	}
	err := v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		return report, err
	}

	// After the files are gone, so a failure to rewrite a pack cannot keep a
	// deleted file in the listing — and once per folder, not once per file.
	// On a context that may already be done, because this is tidying after
	// files that are gone whichever way the batch ended.
	tidy := context.WithoutCancel(ctx)
	for key, ids := range byFolder {
		v.removeThumbs(tidy, key.scope, key.dir, ids...)
	}
	return report, stopped
}

// Rmdir removes a folder. Without recursive it refuses to touch a folder that
// still has contents.
//
// onProgress, when given, is told how many of the doomed files have had their
// parts erased so far — once with (0, total) before the erasing starts, then
// once per file. It is a window for whoever is waiting on the request, nothing
// more: no job state, nothing written down. Calls arrive in order.
//
// ctx done part-way is a stop, as for DeleteMany: the files already erased
// leave the index, the folder and everything not yet reached stay as they
// were, and the error returned is ctx.Err().
func (v *Vault) Rmdir(ctx context.Context, scope Scope, dir string, recursive bool, onProgress func(done, total int)) ([]string, error) {
	dir = CleanDir(dir)
	if dir == "/" {
		return nil, fmt.Errorf("cannot remove the root folder")
	}

	v.mu.RLock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	if !m.FolderExists(dir) {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such folder: %s", dir)
	}
	doomed := m.Descendants(dir)
	subfolders, files := m.Children(dir)
	v.mu.RUnlock()

	if !recursive && (len(subfolders) > 0 || len(files) > 0) {
		return nil, fmt.Errorf("%s is not empty", dir)
	}

	// Read before the erasing rather than after: these are the manifest's own
	// entries, and a move under a delete is the one race worth a line.
	ids := make([]string, len(doomed))
	dirs := make([]string, len(doomed))
	for i, e := range doomed {
		ids[i] = e.ID
		dirs[i] = e.Dir
	}
	attempted, warnings := v.eraseEntries(ctx, doomed, onProgress)
	stopped := ctx.Err()
	whole := attempted == len(doomed) && stopped == nil

	v.mu.Lock()
	if m, err = v.manifestForLocked(scope); err != nil {
		v.mu.Unlock()
		return warnings, err
	}
	for _, id := range ids[:attempted] {
		m.remove(id)
	}
	m.forgetMovies(ids[:attempted]...)
	m.forgetRepos(ids[:attempted]...)
	m.forgetFolderArt(ids[:attempted]...)
	if whole {
		m.removeFolders(dir)
		m.dropMovieFolders(dir)
		m.dropAutomations(dir)
		m.dropFolderArt(dir)
	}
	err = v.persistLocked()
	v.mu.Unlock()

	if err != nil {
		return warnings, err
	}

	tidy := context.WithoutCancel(ctx)
	if !whole {
		// Stopped part-way: what was erased has left the index and its
		// folders' packs; the folder itself and everything after stay
		// exactly as they were, and the caller is told it was cut short.
		byDir := map[string][]string{}
		for i := 0; i < attempted; i++ {
			byDir[dirs[i]] = append(byDir[dirs[i]], ids[i])
		}
		for d, gone := range byDir {
			v.removeThumbs(tidy, scope, d, gone...)
		}
		return warnings, stopped
	}

	// The folder and everything under it is gone, and so are the thumbnails
	// that were stored a folder at a time.
	v.dropThumbFolders(tidy, scope, dir)
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

	// Scheme is the code the file was cut with, written as "4-of-6". Needed is
	// how many shards a rebuild takes, and Spare how many more than that are
	// currently reachable — the number of further accounts that could go dark
	// before the file was at risk.
	Scheme string `json:"scheme"`
	Needed int    `json:"needed"`
	Spare  int    `json:"spare"`

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
	_, entry, ok := v.scopeOfEntryLocked(id)
	if !ok {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such file: %s", id)
	}
	shards := append([]Shard(nil), entry.Shards...)
	path, archiveID := entry.Path(), entry.ArchiveID
	chunked, chunkCount := entry.Chunked(), entry.ChunkCount
	scheme := entry.Scheme()
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

	present := map[int]bool{}
	for _, s := range health.Shards {
		if s.Present {
			present[s.Part] = true
		}
	}
	health.Recoverable = len(present) >= scheme.Data
	health.Scheme = scheme.String()
	health.Needed = scheme.Data
	health.Spare = len(present) - scheme.Data
	if health.Spare < 0 {
		health.Spare = 0
	}
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
