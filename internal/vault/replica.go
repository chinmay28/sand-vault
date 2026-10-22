package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// A replica is a second, independent placement of a stored file.
//
// The parts stay exactly where they are and exactly as they are. A replica is
// the same bytes cut again — under its own archive ID, its own code, and the
// vault's current data key — and scattered into the *replica folders* of the
// accounts the replication policy names. Each of those accounts is one that
// is already connected; the replica folder is a second root on it, kept apart
// from the parts folder by provider.Open (§ provider/replica.go), and the
// folder is the only thing that tells a replica object from a part. Nothing
// on a cloud names the file, and nothing on a replica object says it is one.
//
// Two shapes. With several replication accounts a replica is cut k-of-n like
// anything else, and the same guarantees hold for it. With one account the
// replica is the whole sealed file on that one cloud — archive.WholeCopy —
// which is the one placement in SAND that sets aside "no single account holds
// a file". It is allowed here and nowhere else, because somebody chose it for
// a second copy and was told what it means.
//
// A replica is derived data. Everything about it can be rebuilt from the
// parts, so a recovery drops it and the worker makes it again; and it is
// never the copy a file depends on unless somebody asks to read from it.

// Replica is one file's second placement. Immutable once committed: see the
// note on Entry.Replica.
type Replica struct {
	ArchiveID   string  `json:"archive_id"`
	KeyID       string  `json:"key_id,omitempty"`
	Shards      []Shard `json:"shards"`
	DataShards  int     `json:"data_shards"`
	TotalShards int     `json:"total_shards"`
	ChunkSize   int64   `json:"chunk_size"`
	ChunkCount  int     `json:"chunk_count"`

	// Hash is the plaintext hash the replica was cut from, which is the
	// entry's Hash at the time. A file whose content changes underneath its
	// replica — an overwrite keeps the entry's ID — is a replica to make
	// again, and this is how that is noticed.
	Hash string `json:"hash"`

	CreatedAt time.Time `json:"created_at"`
}

// Scheme is the code the replica was cut with.
func (r *Replica) Scheme() archive.Scheme {
	return archive.Scheme{Data: r.DataShards, Total: r.TotalShards}
}

// Redundancy reports how many distinct shards the replica still records.
func (r *Replica) Redundancy() int { return distinctShards(r.Shards) }

// Recoverable reports whether enough shards are recorded to rebuild from the
// replica alone.
func (r *Replica) Recoverable() bool { return r.Redundancy() >= r.Scheme().Data }

// ReplicationPolicy is the vault's standing instruction for second copies.
type ReplicationPolicy struct {
	// Accounts are the connected accounts replicas go to, each of which has
	// a replica folder. One account means every replica is a whole copy.
	Accounts []string `json:"accounts"`

	// DataShards and TotalShards are the code a replica is cut with. Equal to
	// 1 and 1 for a single account; otherwise a code SAND writes, as wide as
	// the account list.
	DataShards  int `json:"data_shards"`
	TotalShards int `json:"total_shards"`

	// BytesPerDay is the budget the worker keeps to, in original bytes. Zero
	// means unmetered.
	BytesPerDay int64 `json:"bytes_per_day,omitempty"`

	// Paused stops the worker without forgetting anything.
	Paused bool `json:"paused,omitempty"`

	// Level and LevelAt are the meter (see Pacer), carried so a restart
	// resumes the pace rather than starting on a fresh hour of budget.
	Level   int64     `json:"level,omitempty"`
	LevelAt time.Time `json:"level_at,omitzero"`
}

// Scheme is the code the policy cuts replicas with.
func (p *ReplicationPolicy) Scheme() archive.Scheme {
	return archive.Scheme{Data: p.DataShards, Total: p.TotalShards}
}

// Active reports whether the worker should be making replicas.
func (p *ReplicationPolicy) Active() bool {
	return p != nil && len(p.Accounts) > 0 && !p.Paused
}

func (p *ReplicationPolicy) pacer() *Pacer {
	return &Pacer{Rate: p.BytesPerDay, Level: p.Level, At: p.LevelAt}
}

func (p *ReplicationPolicy) adopt(m *Pacer) { p.Level, p.LevelAt = m.Level, m.At }

func (p *ReplicationPolicy) names(id string) bool {
	for _, a := range p.Accounts {
		if a == id {
			return true
		}
	}
	return false
}

// ReplicaAccount is one account's part in the policy as a caller sets it:
// which account, and where on it replicas go.
type ReplicaAccount struct {
	ID            string `json:"id"`
	ReplicaFolder string `json:"replica_folder"`
}

// ReplicationSettings is what SetReplication takes.
type ReplicationSettings struct {
	Accounts []ReplicaAccount `json:"accounts"`
	// Scheme is "k-of-n", or empty to take the default for the count: the
	// whole copy for one account, and the default family's code otherwise.
	Scheme      archive.Scheme `json:"-"`
	BytesPerDay int64          `json:"bytes_per_day"`
	Paused      bool           `json:"paused"`
}

// ---------------------------------------------------------------------------
// The second root

// replicaSuffix marks a live-provider key as an account opened at its replica
// folder. The live cache is keyed by config ID, and the two roots of one
// account have to be two entries in it; the suffix is how, and accountIDOf
// is the way back. It never reaches the index — a replica's shards name the
// account by its real ID, like a part's do.
const replicaSuffix = "#replicas"

func replicaID(accountID string) string { return accountID + replicaSuffix }

// accountIDOf is the account an ID in the live cache belongs to, whichever
// root it was opened at.
func accountIDOf(liveID string) string { return strings.TrimSuffix(liveID, replicaSuffix) }

// replicaConfig is the config an account is opened with at its replica
// folder: the backend's own folder setting swapped for the replica folder,
// and the ID suffixed so the live cache keeps it apart from the parts root.
func replicaConfig(cfg provider.Config) (provider.Config, error) {
	at, err := cfg.AtRole(provider.Replicas)
	if err != nil {
		return provider.Config{}, err
	}
	at.ID = replicaID(cfg.ID)
	return at, nil
}

// configsAtLocked is configsForLocked at a role: the configs a set of shards
// is read or erased through. At the Replicas role each is the account at its
// replica folder, keyed by the account's real ID so a shard's ProviderID
// still finds it. An account that has lost its replica folder since the
// shard was written is absent, like a disconnected one. The caller must hold
// at least the read lock.
func (v *Vault) configsAtLocked(role provider.Role, shards []Shard) map[string]provider.Config {
	if role != provider.Replicas {
		return v.configsForLocked(shards)
	}
	out := make(map[string]provider.Config, len(shards))
	for _, s := range shards {
		if _, done := out[s.ProviderID]; done {
			continue
		}
		cfg, ok := v.configForLocked(s.ProviderID)
		if !ok {
			continue
		}
		at, err := replicaConfig(cfg)
		if err != nil {
			continue
		}
		out[s.ProviderID] = at
	}
	return out
}

// unsuffix puts the account's real ID on shards a scatter at the replica
// root recorded under the suffixed one.
func unsuffix(shards []Shard) []Shard {
	out := make([]Shard, len(shards))
	for i, s := range shards {
		s.ProviderID = accountIDOf(s.ProviderID)
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// The policy

// Replication returns the standing policy, or nil when none is set, and the
// replica folder of every connected account that has one.
func (v *Vault) Replication() (*ReplicationPolicy, map[string]string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, nil, ErrLocked
	}
	folders := map[string]string{}
	for _, cfg := range v.providers {
		if cfg.ReplicaFolder != "" {
			folders[cfg.ID] = cfg.ReplicaFolder
		}
	}
	var policy *ReplicationPolicy
	if p := v.manifest.Replication; p != nil {
		copied := *p
		copied.Accounts = append([]string(nil), p.Accounts...)
		policy = &copied
	}
	return policy, folders, nil
}

// SetReplication sets the policy: which accounts, where on each, how a
// replica is cut, and how much a day. An empty account list clears it, and
// leaves every replica already made where it is.
//
// Each account's replica folder is stored on the account itself, where
// provider.Open reads it, and checked there: a folder that could not keep
// replicas apart from parts is refused before anything is written. An account
// that is no longer in the policy keeps its folder, since replicas made
// earlier are still read through it.
func (v *Vault) SetReplication(s ReplicationSettings) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	if len(s.Accounts) == 0 {
		if v.manifest.Replication == nil {
			return nil
		}
		previous := v.manifest.Replication
		v.manifest.Replication = nil
		if err := v.persistLocked(); err != nil {
			v.manifest.Replication = previous
			return err
		}
		return nil
	}

	seen := map[string]bool{}
	for _, a := range s.Accounts {
		if seen[a.ID] {
			return fmt.Errorf("%s is listed twice — every shard of a replica goes to a different account", a.ID)
		}
		seen[a.ID] = true
	}

	scheme := s.Scheme
	if scheme == (archive.Scheme{}) {
		if len(s.Accounts) == 1 {
			scheme = archive.WholeCopy
		} else {
			var err error
			if scheme, err = SchemeFor(len(s.Accounts)); err != nil {
				return err
			}
		}
	}
	switch {
	case len(s.Accounts) == 1 && !scheme.IsWholeCopy():
		return fmt.Errorf("one replication cloud holds the whole copy; %s needs %d clouds", scheme, scheme.Total)
	case len(s.Accounts) > 1 && scheme.IsWholeCopy():
		return fmt.Errorf("a whole copy goes to one cloud; choose a code for %d", len(s.Accounts))
	case !scheme.IsWholeCopy():
		if err := scheme.Check(); err != nil {
			return err
		}
		if scheme.Total != len(s.Accounts) {
			return fmt.Errorf("%s names %d clouds and %d were chosen", scheme, scheme.Total, len(s.Accounts))
		}
	}
	if s.BytesPerDay < 0 {
		return fmt.Errorf("the daily budget cannot be negative")
	}

	// Every account checked, and every folder validated against its own
	// parts folder, before anything is changed: a half-applied policy would
	// be a policy naming an account that refused its folder.
	ids := make([]string, 0, len(s.Accounts))
	updated := make([]provider.Config, len(v.providers))
	copy(updated, v.providers)
	for _, a := range s.Accounts {
		idx := -1
		for i, cfg := range updated {
			if cfg.ID == a.ID {
				idx = i
			}
		}
		if idx < 0 {
			return fmt.Errorf("no connected account with id %s", a.ID)
		}
		cfg := updated[idx]
		cfg.ReplicaFolder = strings.TrimSpace(a.ReplicaFolder)
		if cfg.ReplicaFolder == "" {
			return fmt.Errorf("%s needs a replica folder", cfg.Name)
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		updated[idx] = cfg
		ids = append(ids, a.ID)
	}

	previousProviders, previousPolicy := v.providers, v.manifest.Replication
	policy := &ReplicationPolicy{
		Accounts:    ids,
		DataShards:  scheme.Data,
		TotalShards: scheme.Total,
		BytesPerDay: s.BytesPerDay,
		Paused:      s.Paused,
	}
	if previousPolicy != nil {
		// The meter carries over: a new budget is a new rate on the same
		// level, not a fresh hour of it.
		policy.Level, policy.LevelAt = previousPolicy.Level, previousPolicy.LevelAt
	}
	v.providers = updated
	v.manifest.Replication = policy
	if err := v.persistLocked(); err != nil {
		v.providers, v.manifest.Replication = previousProviders, previousPolicy
		return err
	}
	// An account whose replica folder moved is an account whose live replica
	// root points at the old one.
	for _, cfg := range updated {
		v.forgetLiveReplica(cfg.ID)
	}
	return nil
}

// forgetLiveReplica drops the replica-root instance of one account from the
// live cache, leaving the parts root alone.
func (v *Vault) forgetLiveReplica(accountID string) {
	v.liveMu.Lock()
	dropped := v.live[replicaID(accountID)]
	delete(v.live, replicaID(accountID))
	v.liveMu.Unlock()
	closeProvider(dropped)
}

// pruneReplicationLocked takes a disconnected account out of the policy.
// What is left may no longer be a policy at all — 2-of-3 has nothing to say
// about two accounts — and then the policy goes rather than quietly narrowing
// to something nobody chose. The caller must hold the write lock.
func (v *Vault) pruneReplicationLocked(surviving map[string]bool) {
	p := v.manifest.Replication
	if p == nil {
		return
	}
	kept := p.Accounts[:0:0]
	for _, id := range p.Accounts {
		if surviving[id] {
			kept = append(kept, id)
		}
	}
	if len(kept) == len(p.Accounts) {
		return
	}
	if len(kept) != p.TotalShards {
		v.manifest.Replication = nil
		return
	}
	p.Accounts = kept
}

// ---------------------------------------------------------------------------
// What replicates

// SetReplicate says whether a folder, or one file, is replicated. A folder's
// answer is inherited by everything under it until a nearer folder or a file
// says otherwise; nil puts a folder or file back to inheriting. Main vault
// only: a replica is a main-vault placement.
func (v *Vault) SetReplicate(path string, enabled *bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	m := v.manifest

	if e := m.ByPath(path); e != nil {
		previous := e.Replicate
		e.Replicate = enabled
		if err := v.persistLocked(); err != nil {
			e.Replicate = previous
			return err
		}
		return nil
	}

	dir := CleanDir(path)
	if dir != "/" && !m.FolderExists(dir) {
		return fmt.Errorf("no such file or folder: %s", path)
	}
	previous, had := m.ReplicatedFolders[dir]
	if enabled == nil {
		delete(m.ReplicatedFolders, dir)
		if len(m.ReplicatedFolders) == 0 {
			m.ReplicatedFolders = nil
		}
	} else {
		if m.ReplicatedFolders == nil {
			m.ReplicatedFolders = map[string]bool{}
		}
		m.ReplicatedFolders[dir] = *enabled
	}
	if err := v.persistLocked(); err != nil {
		if had {
			if m.ReplicatedFolders == nil {
				m.ReplicatedFolders = map[string]bool{}
			}
			m.ReplicatedFolders[dir] = previous
		} else {
			delete(m.ReplicatedFolders, dir)
		}
		return err
	}
	return nil
}

// FolderReplicate is a folder's standing on replication: whether files in it
// replicate, and which folder said so.
type FolderReplicate struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source,omitempty"`
}

// folderReplicateLocked walks up from dir to the nearest folder with an
// answer. The caller must hold at least the read lock.
func folderReplicateLocked(m *Manifest, dir string) FolderReplicate {
	if len(m.ReplicatedFolders) == 0 {
		return FolderReplicate{}
	}
	for at := CleanDir(dir); ; {
		if on, ok := m.ReplicatedFolders[at]; ok {
			return FolderReplicate{Enabled: on, Source: at}
		}
		if at == "/" {
			return FolderReplicate{}
		}
		if idx := strings.LastIndex(at, "/"); idx <= 0 {
			at = "/"
		} else {
			at = at[:idx]
		}
	}
}

// replicatedLocked reports whether a file is meant to have a replica: its
// own say if it has one, and its folder's otherwise.
func replicatedLocked(m *Manifest, e *Entry) bool {
	if e.Replicate != nil {
		return *e.Replicate
	}
	return folderReplicateLocked(m, e.Dir).Enabled
}

// replicatedFoldersUnder reports, for each of dir's subfolders, whether files
// in it replicate — only the ones that do, so a vault with none carries
// nothing. The caller must hold at least the read lock.
func replicatedFoldersUnder(m *Manifest, folders []string) map[string]bool {
	if len(m.ReplicatedFolders) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, f := range folders {
		if folderReplicateLocked(m, f).Enabled {
			out[f] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Whether a replica is current

// replicaCurrentLocked reports whether a file's replica is the one the policy
// would make now: present, cut from the file as it is, on the policy's
// accounts with the policy's code, and sealed under the current key. Anything
// else is a replica to make again — the old one is erased once the new one is
// committed, so a file is never without one in between.
func (v *Vault) replicaCurrentLocked(e *Entry, p *ReplicationPolicy) bool {
	r := e.Replica
	if r == nil || p == nil {
		return false
	}
	if r.Hash != e.Hash || r.KeyID != v.dataKeyID {
		return false
	}
	if r.DataShards != p.DataShards || r.TotalShards != p.TotalShards {
		return false
	}
	if r.Redundancy() < r.TotalShards {
		return false
	}
	for _, s := range r.Shards {
		if !p.names(s.ProviderID) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The worker

// replicateRetryAfter is how long a file that would not replicate is left
// alone before being tried again.
const replicateRetryAfter = time.Hour

const maxReplicateWarnings = 20

// replicateMemory is what the worker keeps between steps and tells nobody
// to persist: which files to leave alone for a while, what went wrong, and
// what is moving right now.
type replicateMemory struct {
	skip     map[string]time.Time
	warnings []string

	current   string
	currentID string
	done      int64
	size      int64
	cancel    context.CancelFunc
}

// ReplicateStep is what one turn of the worker did.
type ReplicateStep struct {
	// Idle means nothing is waiting: every replicated file has a current
	// replica, or the policy is off.
	Idle bool `json:"idle"`
	// Wait is how long until the next step is worth taking, when the meter
	// or the single slot said not now.
	Wait time.Duration `json:"-"`
	// Done means a replica was made, of Path, Bytes long.
	Done    bool   `json:"done"`
	Path    string `json:"path,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// nextReplicateLocked picks the smallest file whose replica is wanted and
// not current, passing over the ones being left alone. nil with idle true
// means nothing is waiting at all; nil with idle false means everything
// waiting is being left alone for now. The caller must hold the lock.
func (v *Vault) nextReplicateLocked(now time.Time) (*Entry, bool) {
	p := v.manifest.Replication
	if !p.Active() {
		return nil, true
	}
	v.replicateMu.Lock()
	skip := v.replicate.skip
	v.replicateMu.Unlock()

	var best *Entry
	waiting := false
	for _, e := range v.manifest.Entries {
		if !replicatedLocked(v.manifest, e) || v.replicaCurrentLocked(e, p) {
			continue
		}
		waiting = true
		if until, ok := skip[e.ID]; ok && now.Before(until) {
			continue
		}
		if best == nil || e.Size < best.Size {
			best = e
		}
	}
	return best, !waiting
}

// ReplicateStep makes one replica, if one is wanted and the budget allows.
// It is the worker's whole loop body: the server calls it on a tick while the
// vault is unlocked, and the CLI calls it in a loop of its own.
func (v *Vault) ReplicateStep(ctx context.Context, now time.Time) (ReplicateStep, error) {
	if !v.replicateSlot.TryLock() {
		return ReplicateStep{Wait: 30 * time.Second}, nil
	}
	defer v.replicateSlot.Unlock()

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return ReplicateStep{}, ErrLocked
	}
	entry, idle := v.nextReplicateLocked(now)
	if idle {
		v.mu.Unlock()
		return ReplicateStep{Idle: true}, nil
	}
	if entry == nil {
		v.mu.Unlock()
		return ReplicateStep{Wait: replicateRetryAfter / 4}, nil
	}
	policy := v.manifest.Replication
	meter := policy.pacer()
	ready, wait := meter.Ready(now)
	policy.adopt(meter)
	if !ready {
		v.mu.Unlock()
		return ReplicateStep{Wait: wait}, nil
	}
	meter.Spend(now, entry.Size)
	policy.adopt(meter)
	id, path, size := entry.ID, entry.Path(), entry.Size
	v.mu.Unlock()

	made, warnings, err := v.replicateTracked(ctx, id)
	if err == nil && made {
		return ReplicateStep{Done: true, Path: path, Bytes: size, Warning: strings.Join(warnings, "; ")}, nil
	}

	// Deleted while it was being chosen, or would not replicate: either way
	// the budget was not spent on it.
	v.mu.Lock()
	if p := v.manifest.Replication; p != nil {
		meter := p.pacer()
		meter.Refund(now, size)
		p.adopt(meter)
		if err != nil {
			_ = v.persistLocked()
		}
	}
	v.mu.Unlock()

	if err == nil {
		return ReplicateStep{}, nil
	}
	if errors.Is(err, ErrLocked) || ctx.Err() != nil {
		return ReplicateStep{}, err
	}
	warning := fmt.Sprintf("%s: %v", path, err)
	v.replicateMu.Lock()
	if v.replicate.skip == nil {
		v.replicate.skip = map[string]time.Time{}
	}
	v.replicate.skip[id] = now.Add(replicateRetryAfter)
	v.replicate.warnings = append(v.replicate.warnings, warning)
	if len(v.replicate.warnings) > maxReplicateWarnings {
		v.replicate.warnings = v.replicate.warnings[len(v.replicate.warnings)-maxReplicateWarnings:]
	}
	v.replicateMu.Unlock()
	return ReplicateStep{Path: path, Warning: warning}, nil
}

// replicateTracked is replicateFile with the file's progress on show.
func (v *Vault) replicateTracked(ctx context.Context, id string) (bool, []string, error) {
	fileCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	v.mu.RLock()
	path := ""
	if e := v.manifest.ByID(id); e != nil {
		path = e.Path()
	}
	v.mu.RUnlock()

	v.replicateMu.Lock()
	v.replicate.current, v.replicate.currentID, v.replicate.cancel = path, id, cancel
	v.replicate.done, v.replicate.size = 0, 0
	v.replicateMu.Unlock()
	defer func() {
		v.replicateMu.Lock()
		v.replicate.current, v.replicate.currentID, v.replicate.cancel = "", "", nil
		v.replicate.done, v.replicate.size = 0, 0
		v.replicateMu.Unlock()
	}()

	return v.replicateFile(fileCtx, id, func(done, size int64) {
		v.replicateMu.Lock()
		v.replicate.done, v.replicate.size = done, size
		v.replicateMu.Unlock()
	})
}

// replicateFile makes one file's replica: the parts are read back, the bytes
// cut again under the policy's code and the current key, and scattered into
// the replica folders of the policy's accounts. The index moves in one write
// once every shard has landed, and the previous replica, if there was one,
// is erased after that — so at no point is the file without a replica it
// had, and a failure anywhere leaves the index exactly as it was.
//
// It returns false, nil when there was nothing to do: the file is gone, or
// its replica is current after all.
func (v *Vault) replicateFile(ctx context.Context, id string, progress func(done, size int64)) (bool, []string, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return false, nil, ErrLocked
	}
	entry := v.manifest.ByID(id)
	if entry == nil {
		v.mu.RUnlock()
		return false, nil, nil
	}
	policy := v.manifest.Replication
	if !policy.Active() || v.replicaCurrentLocked(entry, policy) {
		v.mu.RUnlock()
		return false, nil, nil
	}
	stale := *entry
	stale.Shards = append([]Shard(nil), entry.Shards...)
	accounts := append([]string(nil), policy.Accounts...)
	scheme := policy.Scheme()
	v.mu.RUnlock()

	placed, err := v.rescatter(ctx, MainScope, &stale, spread{
		preferred: accounts,
		exact:     true,
		scheme:    scheme,
		role:      provider.Replicas,
	}, progress)
	defer v.releaseDataKey(placed.keyID)
	if err != nil {
		return false, placed.warnings, err
	}
	// A scatter at the replica root records shards under the suffixed IDs
	// its configs carried. The index names accounts by their real IDs.
	placed.shards = unsuffix(placed.shards)
	fresh := &Replica{
		ArchiveID:   placed.archiveID,
		KeyID:       placed.keyID,
		Shards:      placed.shards,
		DataShards:  placed.scheme.Data,
		TotalShards: placed.scheme.Total,
		ChunkSize:   placed.chunkSize,
		ChunkCount:  placed.chunkCount,
		Hash:        stale.Hash,
		CreatedAt:   time.Now().UTC(),
	}
	if fresh.Hash == "" {
		fresh.Hash = hexHash(placed.originalHash)
	}

	v.mu.Lock()
	if err := v.placedKeyHeldLocked(placed.keyID); err != nil {
		v.mu.Unlock()
		warnings := v.deleteReplicaShards(context.WithoutCancel(ctx), fresh)
		return false, append(placed.warnings, warnings...), err
	}
	current := v.manifest.ByID(id)
	if current == nil || current.Hash != stale.Hash {
		// Deleted or overwritten while the copy was being made. What was
		// written answers to bytes nothing points at any more.
		v.mu.Unlock()
		warnings := v.deleteReplicaShards(context.WithoutCancel(ctx), fresh)
		return false, append(placed.warnings, warnings...), nil
	}
	previous := current.Replica
	current.Replica = fresh
	if err := v.persistLocked(); err != nil {
		current.Replica = previous
		v.mu.Unlock()
		warnings := v.deleteReplicaShards(context.WithoutCancel(ctx), fresh)
		return false, append(placed.warnings, warnings...), err
	}
	v.mu.Unlock()

	warnings := placed.warnings
	if previous != nil {
		warnings = append(warnings, v.deleteReplicaShards(context.WithoutCancel(ctx), previous)...)
	}
	return true, warnings, nil
}

// deleteReplicaShards erases a replica's objects from the replica folders.
func (v *Vault) deleteReplicaShards(ctx context.Context, r *Replica) []string {
	if r == nil {
		return nil
	}
	return v.deleteStoredShardsAt(ctx, provider.Replicas, r.ArchiveID, r.ChunkCount, r.Shards)
}

// DropReplica erases one file's replica and forgets it, which is what
// "stop replicating" means when somebody wants the space back now rather than
// at the next sweep. The file's own parts are untouched.
func (v *Vault) DropReplica(ctx context.Context, id string) ([]string, error) {
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
	doomed := entry.Replica
	v.mu.RUnlock()
	if doomed == nil {
		return nil, nil
	}

	warnings := v.deleteReplicaShards(ctx, doomed)

	v.mu.Lock()
	defer v.mu.Unlock()
	if entry := v.manifest.ByID(id); entry != nil && entry.Replica == doomed {
		entry.Replica = nil
		if err := v.persistLocked(); err != nil {
			entry.Replica = doomed
			return warnings, err
		}
	}
	return warnings, nil
}

// ---------------------------------------------------------------------------
// What the worker is doing

// ReplicationStatus is the policy and how far it has got, for a settings
// line and a progress panel.
type ReplicationStatus struct {
	Policy  *ReplicationPolicy `json:"policy,omitempty"`
	Folders map[string]string  `json:"folders,omitempty"`

	// Replicated counts the files with a current replica, Pending the ones
	// wanting one — made yet or not — and Stale the pending ones that have a
	// replica, only an outdated one. PendingBytes is what Pending weighs.
	Replicated   int   `json:"replicated"`
	Pending      int   `json:"pending"`
	Stale        int   `json:"stale"`
	PendingBytes int64 `json:"pending_bytes"`

	// ReplicaBytes is what every replica occupies across the clouds.
	ReplicaBytes int64 `json:"replica_bytes"`

	// Current is the file being copied right now, if one is.
	Current     string   `json:"current,omitempty"`
	CurrentDone int64    `json:"current_done,omitempty"`
	CurrentSize int64    `json:"current_size,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	LeftAlone   int      `json:"left_alone,omitempty"`
	// ETA is how long the meter says what is pending will take, when it is
	// metered and something is pending.
	ETA        time.Duration `json:"-"`
	ETASeconds int64         `json:"eta_seconds,omitempty"`
}

// ReplicationStatus reports the policy and the worker's progress.
func (v *Vault) ReplicationStatus(now time.Time) (ReplicationStatus, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return ReplicationStatus{}, ErrLocked
	}
	out := ReplicationStatus{Folders: map[string]string{}}
	for _, cfg := range v.providers {
		if cfg.ReplicaFolder != "" {
			out.Folders[cfg.ID] = cfg.ReplicaFolder
		}
	}
	p := v.manifest.Replication
	if p != nil {
		copied := *p
		copied.Accounts = append([]string(nil), p.Accounts...)
		out.Policy = &copied
	}
	v.replicateMu.Lock()
	skip := v.replicate.skip
	out.Warnings = append([]string(nil), v.replicate.warnings...)
	out.Current, out.CurrentDone, out.CurrentSize = v.replicate.current, v.replicate.done, v.replicate.size
	v.replicateMu.Unlock()

	for _, e := range v.manifest.Entries {
		if e.Replica != nil {
			for _, s := range e.Replica.Shards {
				out.ReplicaBytes += s.Size
			}
		}
		if !replicatedLocked(v.manifest, e) {
			continue
		}
		if v.replicaCurrentLocked(e, p) {
			out.Replicated++
			continue
		}
		out.Pending++
		out.PendingBytes += e.Size
		if e.Replica != nil {
			out.Stale++
		}
		if until, ok := skip[e.ID]; ok && now.Before(until) {
			out.LeftAlone++
		}
	}
	if p != nil && out.Pending > 0 && p.BytesPerDay > 0 {
		out.ETA = p.pacer().ETA(now, out.PendingBytes)
		out.ETASeconds = int64(out.ETA / time.Second)
	}
	return out, nil
}

// ReplicationKeepsBusy reports whether a replica is being made right now,
// which the server's idle timer should wait for.
func (v *Vault) ReplicationKeepsBusy() bool {
	v.replicateMu.Lock()
	defer v.replicateMu.Unlock()
	return v.replicate.currentID != ""
}

// ReplicationSummary is the one line about replication a vault's Stats
// carry.
type ReplicationSummary struct {
	Accounts   int    `json:"accounts"`
	Scheme     string `json:"scheme"`
	Paused     bool   `json:"paused,omitempty"`
	Replicated int    `json:"replicated"`
	Pending    int    `json:"pending"`
}

// replicationSummaryLocked is the summary for Stats. The caller must hold at
// least the read lock.
func (v *Vault) replicationSummaryLocked() *ReplicationSummary {
	p := v.manifest.Replication
	if p == nil {
		return nil
	}
	out := &ReplicationSummary{Accounts: len(p.Accounts), Scheme: p.Scheme().String(), Paused: p.Paused}
	if p.Scheme().IsWholeCopy() {
		out.Scheme = "whole"
	}
	for _, e := range v.manifest.Entries {
		if !replicatedLocked(v.manifest, e) {
			continue
		}
		if v.replicaCurrentLocked(e, p) {
			out.Replicated++
		} else {
			out.Pending++
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reading from a replica

// ReadSource says which placement a read is served from.
type ReadSource string

const (
	// ReadParts is the file's parts: every read until now.
	ReadParts ReadSource = "parts"
	// ReadReplica is the replica alone, and an error where there is none.
	ReadReplica ReadSource = "replica"
	// ReadEither is the parts, with the replica standing in for any chunk the
	// parts cannot give. Not a race: a chunk is fetched once, and the second
	// placement is asked only when the first has failed.
	ReadEither ReadSource = "either"
)

// ParseReadSource reads a source as a request names it. Empty is the parts.
func ParseReadSource(s string) (ReadSource, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(ReadParts):
		return ReadParts, nil
	case string(ReadReplica):
		return ReadReplica, nil
	case string(ReadEither):
		return ReadEither, nil
	}
	return "", fmt.Errorf("%q is not a place to read from: parts, replica or either", s)
}

// ErrNoReplica is returned when a read asks for a replica a file does not
// have.
var ErrNoReplica = errors.New("this file has no replica to read from")

// ReadFrom chooses which placement a reader is served from.
func ReadFrom(source ReadSource) ReadOption {
	return func(r *ChunkedReader) { r.source = source }
}

// replicaView is the entry as if its replica were its placement: the same
// file, the same size and hash, with the archive ID, code, chunking, key and
// shards replaced by the replica's. It is what the read path is handed so
// that nothing in it has to know a replica from a part.
func (e *Entry) replicaView() *Entry {
	r := e.Replica
	if r == nil {
		return nil
	}
	view := *e
	view.ArchiveID = r.ArchiveID
	view.KeyID = r.KeyID
	view.Shards = append([]Shard(nil), r.Shards...)
	view.DataShards, view.TotalShards = r.DataShards, r.TotalShards
	view.ChunkSize, view.ChunkCount = r.ChunkSize, r.ChunkCount
	view.Replica = nil
	return &view
}

// hexHash is a whole-file hash the way the index writes it.
func hexHash(sum [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return string(out)
}
