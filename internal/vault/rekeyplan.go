package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// A password change re-encrypts every stored file, and rekey.go explains why
// that cannot be one atomic act. This file is about the other half of the same
// fact: it does not have to be one *fast* act either. A terabyte vault moved as
// quickly as the clouds allow is a day of saturated upload on every account, a
// locked-out account wherever there is a daily cap, and a home connection that
// is unusable until it ends. So the change can carry a plan — so much a day,
// these folders first — and the files move on that schedule, one at a time,
// while the vault is open under the server.
//
// What the plan does not change is the security of the arrangement. The
// password is changed the moment the change commits and the old one opens
// nothing from then on (rekey.go); what a slower migration leaves longer is the
// window in which somebody holding the *old* password and an *old* copy of the
// vault file or a manifest backup can still read the parts that have not moved.
// The status says which files those are and how long they will stay that way,
// and the file list marks them, so that window is a visible thing with a
// visible end rather than something that happens to be true.
//
// One rule follows from the two halves being allowed to drift apart: a second
// password change is refused while a first is still moving files. A vault that
// could hold three generations at once is a vault whose owner has to reason
// about three; two — the current key and the previous one — is what the file
// list can mark, what the status can report, and what a person can hold in
// their head. ErrRekeyPending is that refusal.

// ErrRekeyPending is returned by a password change asked for while the files
// from the previous change are still being re-encrypted. Finish that one first,
// quickly (MigrateFiles) or on its plan.
var ErrRekeyPending = errors.New("a previous password change is still re-encrypting")

// ErrSkipped is what a move ends with when the file was deferred while it was
// moving: not a failure, and the file is on the previous key as it was.
var ErrSkipped = errors.New("skipped by request")

// ErrMigrationBusy is returned when a re-encryption pass is asked for while one
// is already running — the paced step and the finish-now pass share one slot,
// so two of them never gather and scatter the same file.
var ErrMigrationBusy = errors.New("a re-encryption is already running")

// RekeyPendingError says how much a refused password change was blocked by.
type RekeyPendingError struct {
	Files int
	Bytes int64
}

func (e *RekeyPendingError) Error() string {
	return fmt.Sprintf("a previous password change is still taking effect: %d file(s) (%s) "+
		"are still on the previous key; finish re-encrypting them before changing the password again",
		e.Files, formatBytes(e.Bytes))
}

// Is lets errors.Is(err, ErrRekeyPending) match the typed form.
func (e *RekeyPendingError) Is(target error) bool { return target == ErrRekeyPending }

// RekeySettings is the part of a plan a person sets.
type RekeySettings struct {
	// BytesPerDay is the budget. Zero means nothing moves on its own: the
	// files wait for MigrateFiles — "finish now" — or for a budget.
	BytesPerDay int64 `json:"bytes_per_day"`

	// FullSpeed moves everything as fast as the clouds take it, with no
	// meter, while BytesPerDay is kept for the pace to fall back to. It is
	// "finish now" as a plan rather than as a request held open: the server
	// works through the files on its own (RekeyStep, back to back), so the
	// browser that asked can be closed, locked or put in a pocket, and the
	// status says where it has got to. Folders, OnlyFolders, Paused and the
	// deferred list mean what they mean at any pace.
	FullSpeed bool `json:"full_speed,omitempty"`

	// Folders are re-encrypted first, in this order, before anything else.
	// Vault paths; "/" names the root's own files and everything under it.
	Folders []string `json:"folders,omitempty"`

	// OnlyFolders stops the plan after those folders rather than going on to
	// the rest of the vault. The rest stays on the previous key, readable,
	// and reported, until the plan is widened or the files are finished by
	// hand.
	OnlyFolders bool `json:"only_folders,omitempty"`

	// Paused holds the plan without forgetting it.
	Paused bool `json:"paused,omitempty"`

	// KeepUnlocked asks the server not to let the idle timer lock the vault
	// while the plan has files left to move. Without it, the plan runs only
	// while the vault happens to be open, which on a desktop is "while
	// somebody is using it" and on a server is "until the idle timeout".
	// With it, a vault with a plan stays open — a security trade the person
	// makes knowingly, which is why it is a setting and not the default.
	KeepUnlocked bool `json:"keep_unlocked,omitempty"`
}

// RekeyPlan is a re-encryption in flight: what was asked for, how far the
// meter has got, and what there was to do when it started. It lives in the
// manifest — the folders it names are index — and goes when the last file has
// moved.
type RekeyPlan struct {
	RekeySettings

	StartedAt time.Time `json:"started_at,omitzero"`

	// Files and Bytes are what was on the previous key when the plan began,
	// so progress can be a fraction of something. Bytes are original sizes,
	// which is what a person means by "how much is left".
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	// Level and LevelAt are the meter (see Pacer), carried so that a restart
	// resumes the pace rather than starting a fresh hour of budget.
	Level   int64     `json:"level,omitempty"`
	LevelAt time.Time `json:"level_at,omitzero"`

	// Deferred names the files somebody asked to have left for last — a film
	// that keeps failing on an account that is down, a file that is in the
	// way — by entry ID. They are moved after everything else, by the paced
	// step and by a finish-now pass alike, and a file being moved when it is
	// deferred is stopped where it is and left on the previous key. In the
	// plan rather than in memory because "come back to it later" has to
	// survive a restart.
	Deferred []string `json:"deferred,omitempty"`
}

// isDeferred reports whether the plan leaves a file for last.
func (p *RekeyPlan) isDeferred(id string) bool {
	if p == nil {
		return false
	}
	for _, d := range p.Deferred {
		if d == id {
			return true
		}
	}
	return false
}

// Active reports whether the plan wants files moved on its own: at full
// speed, or on a budget, and not paused.
func (p *RekeyPlan) Active() bool {
	return p != nil && (p.FullSpeed || p.BytesPerDay > 0) && !p.Paused
}

// pacer builds the meter from the plan's stored state. adopt writes it back.
// A plan at full speed gets an unmetered pacer, which leaves Level and LevelAt
// as they were, so a plan slowed down again takes up the pace it had banked.
func (p *RekeyPlan) pacer() *Pacer {
	if p.FullSpeed {
		return &Pacer{Level: p.Level, At: p.LevelAt}
	}
	return &Pacer{Rate: p.BytesPerDay, Level: p.Level, At: p.LevelAt}
}

func (p *RekeyPlan) adopt(m *Pacer) {
	p.Level, p.LevelAt = m.Level, m.At
}

// RekeyFolder is one folder's share of what is still on the previous key.
type RekeyFolder struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`

	// Queued says the plan names this folder.
	Queued bool `json:"queued"`
}

// RekeyStatus is where a re-encryption stands.
type RekeyStatus struct {
	// Pending counts the files still on the previous key, and PendingBytes
	// their original sizes.
	Pending      int   `json:"pending"`
	PendingBytes int64 `json:"pending_bytes"`

	// StaleReplicas counts the replicas still sealed under a key other than
	// the current one. Not in Pending — a replica is not re-encrypted but
	// made again by the replication worker, or erased (dropStaleReplicas) —
	// and reported separately because each one keeps the previous key, and
	// so the previous password, alive after Pending has reached zero.
	StaleReplicas int `json:"stale_replicas,omitempty"`

	// Total and TotalBytes are what there was to move when the plan began —
	// or what there is now, when nothing has been planned — so that the
	// fraction done is Pending over Total. Never less than Pending.
	Total      int   `json:"total"`
	TotalBytes int64 `json:"total_bytes"`

	Plan *RekeyPlan `json:"plan,omitempty"`

	// Since is when the password changed, which is when the previous key
	// became the previous key.
	Since time.Time `json:"since,omitzero"`

	// NextInSeconds is how long the meter makes the next file wait, and
	// ETASeconds how long the whole of what is pending takes at the plan's
	// pace. Both zero when nothing is planned, paused, or left.
	NextInSeconds int64 `json:"next_in_seconds,omitempty"`
	ETASeconds    int64 `json:"eta_seconds,omitempty"`

	// Idle says the plan has done everything it is allowed to do and files
	// are still pending — OnlyFolders, and those folders are finished.
	Idle bool `json:"idle"`

	// Folders is what is pending, folder by folder: the plan's own folders
	// first in their order, then every top-level folder with files left.
	Folders []RekeyFolder `json:"folders"`

	// Running says a file is being moved right now, and Current which; Done
	// and Size are how far through it the scatter has got, in bytes of the
	// original. All in memory and this process's own.
	Running     bool   `json:"running"`
	Current     string `json:"current,omitempty"`
	CurrentID   string `json:"current_id,omitempty"`
	CurrentDone int64  `json:"current_done,omitempty"`
	CurrentSize int64  `json:"current_size,omitempty"`

	// Deferred lists the pending files that have been left for last.
	Deferred []RekeyFile `json:"deferred,omitempty"`

	// Held counts the pending files the plan is leaving where they are
	// because too few of their parts are on accounts that answered the last
	// health check to rebuild them, and HeldOn names the accounts that did
	// not answer. A file with enough parts within reach is not held: it is
	// re-encrypted from those, and the part on the silent account is left
	// behind and written down (StaleParts). One that cannot be read without
	// the silent account cannot be moved by anything, so trying it would be
	// a failure an hour at a time; the plan waits for the account instead,
	// and this is how the wait is explained. Both empty when nothing is
	// held, so a status from before they existed reads the same.
	Held   int      `json:"held,omitempty"`
	HeldOn []string `json:"held_on,omitempty"`

	// Retrying counts the pending files the plan tried lately and could
	// not move, which it leaves alone for a while (rekeyRetryAfter), and
	// RetryInSeconds is how long until the first of them is tried again.
	// Why each one failed is in Warnings, and file by file in
	// PendingRekeyFiles. Both zero when nothing is waiting this way, so a
	// plan that has nothing it can take reads as resting rather than
	// stuck.
	Retrying       int   `json:"retrying,omitempty"`
	RetryInSeconds int64 `json:"retry_in_seconds,omitempty"`

	// StaleParts is what re-encryptions so far have left behind under the
	// previous key on accounts they could not reach: how many parts, what
	// they weigh, and on which accounts, with whether each is answering
	// now. Nil when nothing is on record. See staleparts.go.
	StaleParts *StalePartsSummary `json:"stale_parts,omitempty"`

	// Warnings are the files the plan could not move lately, with why. They
	// are retried after a while; this is so the reason is visible meanwhile.
	Warnings []string `json:"warnings,omitempty"`
}

// Done reports whether every file is on the current key.
func (s *RekeyStatus) Done() bool { return s != nil && s.Pending == 0 }

// RekeyFile names one file in a status.
type RekeyFile struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// rekeyRetryAfter is how long a file that would not move is left alone before
// the plan tries it again. Long enough that a cloud that is down for the
// evening does not cost a log line a minute, short enough that one back by
// morning is picked up.
const rekeyRetryAfter = time.Hour

// maxRekeyWarnings caps what Warnings carries: the latest, not the lot.
const maxRekeyWarnings = 10

// rekeyMemory is what this process remembers about a plan that is not worth
// writing to disk: which files it is leaving alone for a while and why, and
// which it is moving at the moment.
type rekeyMemory struct {
	// skip is when each file that would not move may be tried again, and
	// why keeps the reason it would not, for as long as skip holds it.
	skip     map[string]time.Time
	why      map[string]string
	warnings []string

	// The file being moved right now, by whichever pass is moving it, how
	// far through it is, and the cancel that stops it. See moveTracked.
	current   string
	currentID string
	done      int64
	size      int64
	cancel    context.CancelFunc
	// finished is closed when the move in flight has let go of the file —
	// the way a delete that pulled cancel waits for it. See moveTracked.
	finished chan struct{}
}

// pendingLocked counts the files in a scope that are on a key other than the
// scope's current one, with their original sizes. The caller holds the lock.
func (v *Vault) pendingLocked(scope Scope) (int, int64) {
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return 0, 0
	}
	active, err := v.dataKeyIDForLocked(scope)
	if err != nil {
		return 0, 0
	}
	return pendingIn(m, active)
}

// retiredPendingIn counts the files still on a generation this vault itself
// retired — the ones a previous password change has yet to move — leaving out
// files sealed under another vault's key, which an assignment with the
// re-encryption deferred leaves behind. Those are pending too, and the plan
// moves them, but they are not a password change half done, and they must not
// stop the next one. The guard on a password change asks this; the status and
// the file list ask pendingIn.
func retiredPendingIn(m *Manifest, active string, retired map[string][]byte) (files int, bytes int64) {
	for _, e := range m.Entries {
		if e.KeyID == active {
			continue
		}
		if _, ours := retired[e.KeyID]; !ours {
			continue
		}
		files++
		bytes += e.Size
	}
	return files, bytes
}

// retiredPendingLocked is retiredPendingIn for a scope. The caller holds the
// lock; a scope that is not open answers zero.
func (v *Vault) retiredPendingLocked(scope Scope) (int, int64) {
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return 0, 0
	}
	if scope.Main() {
		return retiredPendingIn(m, v.dataKeyID, v.retired)
	}
	sub, ok := v.subs[string(scope)]
	if !ok {
		return 0, 0
	}
	return retiredPendingIn(m, sub.dataKeyID, sub.retired)
}

// pendingIn is pendingLocked over an index and key already in hand.
func pendingIn(m *Manifest, active string) (files int, bytes int64) {
	for _, e := range m.Entries {
		if e.KeyID != active {
			files++
			bytes += e.Size
		}
	}
	return files, bytes
}

// normalizeRekeySettings cleans a plan's folders and checks them against the
// index, so a plan never names a folder that is not there.
func normalizeRekeySettings(s RekeySettings, m *Manifest) (RekeySettings, error) {
	if s.BytesPerDay < 0 {
		return s, fmt.Errorf("the daily budget cannot be negative")
	}
	seen := map[string]bool{}
	var folders []string
	for _, raw := range s.Folders {
		dir := CleanDir(raw)
		if seen[dir] {
			continue
		}
		if !m.FolderExists(dir) {
			return s, fmt.Errorf("no such folder: %s", dir)
		}
		seen[dir] = true
		folders = append(folders, dir)
	}
	s.Folders = folders
	if len(folders) == 0 {
		// Nothing named means nothing to stop after.
		s.OnlyFolders = false
	}
	return s, nil
}

// newRekeyPlan starts a plan over what is pending now.
func newRekeyPlan(s RekeySettings, m *Manifest, active string, now time.Time) *RekeyPlan {
	files, bytes := pendingIn(m, active)
	return &RekeyPlan{
		RekeySettings: s,
		StartedAt:     now,
		Files:         files,
		Bytes:         bytes,
	}
}

// SetRekeyPlan sets how the main vault's outstanding re-encryption proceeds,
// starting a plan if there is none and adjusting the one there is: a budget
// change keeps whatever the meter had banked, capped at the new burst, and a
// folder change takes effect at the next file.
//
// Refused when nothing is on the previous key, because a plan then has nothing
// to be a plan about. It is the password change that creates the work, and the
// change takes its plan directly (ChangePasswordWithPlan).
func (v *Vault) SetRekeyPlan(s RekeySettings) (*RekeyStatus, error) {
	now := time.Now().UTC()

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return nil, ErrLocked
	}
	s, err := normalizeRekeySettings(s, v.manifest)
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}
	if n, _ := pendingIn(v.manifest, v.dataKeyID); n == 0 {
		v.mu.Unlock()
		return nil, fmt.Errorf("every file is already on the current key; there is nothing to plan")
	}

	plan := v.manifest.Rekey
	if plan == nil {
		plan = newRekeyPlan(s, v.manifest, v.dataKeyID, now)
		v.manifest.Rekey = plan
	} else {
		plan.RekeySettings = s
		if m := plan.pacer(); !m.Unmetered() && plan.Level > m.burst() {
			plan.Level = m.burst()
		}
	}
	err = v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return v.RekeyStatus()
}

// pruneRekeyPlanLocked drops a plan whose work is done. Called from
// persistLocked, so the plan goes with the last file that leaves the previous
// key — and the previous key goes in the same write (pruneRetiredKeysLocked).
func (v *Vault) pruneRekeyPlanLocked() {
	if v.manifest.Rekey == nil {
		return
	}
	if n, _ := pendingIn(v.manifest, v.dataKeyID); n == 0 {
		v.manifest.Rekey = nil
		v.rekeyMu.Lock()
		v.rekey = rekeyMemory{}
		v.rekeyMu.Unlock()
	}
}

// RekeyStatus reports where the main vault's re-encryption stands.
func (v *Vault) RekeyStatus() (*RekeyStatus, error) {
	now := time.Now().UTC()
	// Before the index lock, never under it: see unreachableAccounts.
	down := v.unreachableAccounts()

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	status := &RekeyStatus{
		Since:   v.store.LastPasswordChangeAt,
		Folders: []RekeyFolder{},
	}
	status.Pending, status.PendingBytes = pendingIn(v.manifest, v.dataKeyID)
	status.Total, status.TotalBytes = status.Pending, status.PendingBytes
	status.StaleReplicas = v.staleReplicasLocked()

	if plan := v.manifest.Rekey; plan != nil {
		copied := *plan
		copied.Folders = append([]string(nil), plan.Folders...)
		status.Plan = &copied
		if plan.Files > status.Total {
			status.Total = plan.Files
		}
		if plan.Bytes > status.TotalBytes {
			status.TotalBytes = plan.Bytes
		}
		if plan.Active() && status.Pending > 0 {
			meter := plan.pacer()
			if ready, wait := meter.Ready(now); !ready {
				status.NextInSeconds = int64(wait.Seconds() + 0.5)
			}
			// What the plan is allowed to move, which is less than what is
			// pending when it stops after its folders.
			allowed := status.PendingBytes
			if plan.OnlyFolders {
				allowed = 0
				for _, f := range plan.Folders {
					_, b := pendingUnder(v.manifest, v.dataKeyID, f)
					allowed += b
				}
			}
			if eta := meter.ETA(now, allowed); eta > 0 {
				status.ETASeconds = int64(eta.Seconds() + 0.5)
			}
			_, idle := v.nextRekeyLocked(now, down)
			status.Idle = idle
			for id, files := range v.heldByUnreachableLocked(down) {
				status.Held += files
				name := id
				if cfg, ok := v.configForLocked(id); ok {
					name = cfg.Name
				}
				status.HeldOn = append(status.HeldOn, name)
			}
			sort.Strings(status.HeldOn)
		}
	}

	status.Folders = v.rekeyFoldersLocked()

	if plan := v.manifest.Rekey; plan != nil {
		for _, id := range plan.Deferred {
			e := v.manifest.ByID(id)
			if e == nil || e.KeyID == v.dataKeyID {
				continue
			}
			status.Deferred = append(status.Deferred, RekeyFile{ID: e.ID, Path: e.Path(), Bytes: e.Size})
		}
	}

	v.rekeyMu.Lock()
	status.Current = v.rekey.current
	status.CurrentID = v.rekey.currentID
	status.CurrentDone, status.CurrentSize = v.rekey.done, v.rekey.size
	status.Running = v.rekey.current != ""
	status.Warnings = append([]string(nil), v.rekey.warnings...)
	var soonest time.Time
	for id, until := range v.rekey.skip {
		if !now.Before(until) {
			continue
		}
		// A file moved by a finish-now pass after it failed here, or
		// deleted, is not waiting on anything.
		if e := v.manifest.ByID(id); e == nil || e.KeyID == v.dataKeyID {
			continue
		}
		status.Retrying++
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	v.rekeyMu.Unlock()
	if status.Retrying > 0 {
		status.RetryInSeconds = int64(soonest.Sub(now).Seconds() + 0.5)
	}

	// Said in Warnings as well as in its own fields, so that whatever shows
	// the warnings today shows why nothing is moving without being taught
	// the new fields first.
	if status.Held > 0 {
		noun := "files are"
		if status.Held == 1 {
			noun = "file is"
		}
		status.Warnings = append(status.Warnings, fmt.Sprintf(
			"%d %s waiting for %s, which did not answer the last health check — "+
				"too few of their parts are on the accounts that did to rebuild them without it",
			status.Held, noun, strings.Join(status.HeldOn, ", ")))
	}
	// And the parts that moves so far have left behind, for the same reason:
	// they are the previous password's last purchase on the files, and a
	// status that counted down to zero without mentioning them would be
	// saying the change was finished when it was not quite.
	if stale := v.stalePartsLocked(down); stale.Parts > 0 {
		status.StaleParts = &stale
		status.Warnings = append(status.Warnings, stale.Sentence())
	}
	return status, nil
}

// unreachableAccounts is the set of connected accounts whose last health check
// failed, by ID. An account nothing has asked yet is not in it: "we have not
// looked" is not a reason to hold a file back.
//
// It takes healthMu on its own and must be called before the index lock, not
// under it — healthMu is a leaf the ping paths write to, and the rule that
// keeps it from deadlocking against the index is that nothing holds mu while
// asking for it. The answer is a snapshot, which is all a decision about
// which file to move next needs.
func (v *Vault) unreachableAccounts() map[string]bool {
	v.healthMu.Lock()
	defer v.healthMu.Unlock()

	var down map[string]bool
	for id, one := range v.healthSeen {
		if one.Checked && !one.Healthy {
			if down == nil {
				down = map[string]bool{}
			}
			down[id] = true
		}
	}
	return down
}

// moveTracked moves one file with the move on record — which file, how far
// along, and a cancel that stops it — so the status can say and the skip
// control can act. It is the one way both the paced step and a finish-now
// pass move a file.
//
// The cancel is the file's own: pulling it ends this move and nothing else,
// and the caller tells a skip (context.Canceled with the pass's own context
// still live) from a shutdown.
func (v *Vault) moveTracked(ctx context.Context, scope Scope, id string, accounts []string) (path string, size int64, warnings []string, err error) {
	fileCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	v.mu.RLock()
	if m, err := v.manifestForLocked(scope); err == nil {
		if e := m.ByID(id); e != nil {
			path = e.Path()
		}
	}
	v.mu.RUnlock()

	finished := make(chan struct{})
	v.rekeyMu.Lock()
	v.rekey.current, v.rekey.currentID, v.rekey.cancel = path, id, cancel
	v.rekey.done, v.rekey.size = 0, 0
	v.rekey.finished = finished
	v.rekeyMu.Unlock()
	defer func() {
		v.rekeyMu.Lock()
		v.rekey.current, v.rekey.currentID, v.rekey.cancel = "", "", nil
		v.rekey.done, v.rekey.size = 0, 0
		v.rekey.finished = nil
		v.rekeyMu.Unlock()
		close(finished)
	}()

	// No scheme: re-encrypting keeps each file cut the way it already is,
	// whatever that is.
	path, size, warnings, err = v.migrateFile(fileCtx, scope, id, accounts, archive.Scheme{}, func(done, size int64) {
		v.rekeyMu.Lock()
		v.rekey.done, v.rekey.size = done, size
		v.rekeyMu.Unlock()
	})
	// A failure after the file's own cancel was pulled, with the caller's
	// context still live, is the skip and not a fault — whatever shape the
	// scatter's own report of it took on the way up.
	if err != nil && fileCtx.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("%w: %s", ErrSkipped, path)
	}
	return path, size, warnings, err
}

// DeferRekeyFile leaves one of the main vault's pending files for last — the
// one being moved right now when id is empty — and, if it is the one being
// moved, stops that move where it is. The file stays on the previous key,
// readable as ever, and is taken up again after everything else; Undefer puts
// it back in its turn. Returns the file's path.
//
// A plan is started if there is none to hold the choice, with no budget: the
// files then wait for a pace or for a finish-now, with this one at the back.
func (v *Vault) DeferRekeyFile(id string) (string, error) {
	v.rekeyMu.Lock()
	if id == "" {
		id = v.rekey.currentID
	}
	v.rekeyMu.Unlock()
	if id == "" {
		return "", fmt.Errorf("nothing is being re-encrypted right now, so there is nothing to skip")
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return "", ErrLocked
	}
	e := v.manifest.ByID(id)
	if e == nil {
		v.mu.Unlock()
		return "", fmt.Errorf("no such file: %s", id)
	}
	path := e.Path()
	if e.KeyID == v.dataKeyID {
		v.mu.Unlock()
		return path, fmt.Errorf("%s is already on the current key", path)
	}
	plan := v.manifest.Rekey
	if plan == nil {
		plan = newRekeyPlan(RekeySettings{}, v.manifest, v.dataKeyID, time.Now().UTC())
		v.manifest.Rekey = plan
	}
	if !plan.isDeferred(id) {
		plan.Deferred = append(plan.Deferred, id)
	}
	err := v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		return path, err
	}

	// Stopped after it is written down, so a move that ends with a cancel
	// finds the file already deferred when it looks for the next one.
	v.rekeyMu.Lock()
	if v.rekey.currentID == id && v.rekey.cancel != nil {
		v.rekey.cancel()
	}
	v.rekeyMu.Unlock()
	return path, nil
}

// UndeferRekeyFile puts a deferred file back in its turn.
func (v *Vault) UndeferRekeyFile(id string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return "", ErrLocked
	}
	path := id
	if e := v.manifest.ByID(id); e != nil {
		path = e.Path()
	}
	plan := v.manifest.Rekey
	if plan == nil || !plan.isDeferred(id) {
		return path, nil
	}
	kept := plan.Deferred[:0]
	for _, d := range plan.Deferred {
		if d != id {
			kept = append(kept, d)
		}
	}
	plan.Deferred = kept
	return path, v.persistLocked()
}

// DeleteRekeyFile erases one of the main vault's pending files — the one being
// moved right now when id is empty — instead of re-encrypting it. It is the
// other answer to a file that will not move: a skip leaves it for last, this
// leaves it out. Every part is erased from every account, under whichever key
// it is on, and the plan has one file fewer to take.
//
// A file in the middle of a move has that move stopped first and the delete
// waits for it to let go, so the two are never erasing the same parts at
// once; a move cancelled this way ends as a skip does, with nothing charged.
// ctx bounds that wait and the erase. Returns the file's path, and a warning
// per part that could not be erased, as Delete does.
//
// A file already on the current key is refused: it is not a pending file,
// and a row that moved while the list was open should not be a delete the
// list did not mean.
func (v *Vault) DeleteRekeyFile(ctx context.Context, id string) (string, []string, error) {
	v.rekeyMu.Lock()
	if id == "" {
		id = v.rekey.currentID
	}
	v.rekeyMu.Unlock()
	if id == "" {
		return "", nil, fmt.Errorf("nothing is being re-encrypted right now, so there is nothing to delete")
	}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return "", nil, ErrLocked
	}
	e := v.manifest.ByID(id)
	if e == nil {
		v.mu.RUnlock()
		return "", nil, fmt.Errorf("no such file: %s", id)
	}
	path := e.Path()
	onCurrent := e.KeyID == v.dataKeyID
	v.mu.RUnlock()
	if onCurrent {
		return path, nil, fmt.Errorf("%s is already on the current key; delete it from its folder instead", path)
	}

	// Held out of the chooser's reach for as long as this takes, so the
	// step that follows a cancelled move does not pick the same file up
	// again and start reading the parts the delete is erasing. The hold is
	// the one a failed move gets, far enough out that it cannot lapse, and
	// it goes with the file below.
	v.rekeyMu.Lock()
	if v.rekey.skip == nil {
		v.rekey.skip = map[string]time.Time{}
	}
	if v.rekey.why == nil {
		v.rekey.why = map[string]string{}
	}
	v.rekey.skip[id] = time.Now().Add(24 * time.Hour)
	v.rekey.why[id] = "it is being deleted"
	// Stop the move if it is this file's, and wait for it to let go. The
	// cancel reads as a skip to the pass that was moving it (see
	// moveTracked), which is right: nothing moved, nothing is charged.
	var finished chan struct{}
	if v.rekey.currentID == id && v.rekey.cancel != nil {
		finished = v.rekey.finished
		v.rekey.cancel()
	}
	v.rekeyMu.Unlock()

	// Whatever comes of it, nothing is left to remember about the file: no
	// hold and no reason. Deleted, there is nothing to hold; not deleted,
	// it is back in its turn as it was.
	forget := func() {
		v.rekeyMu.Lock()
		delete(v.rekey.skip, id)
		delete(v.rekey.why, id)
		v.rekeyMu.Unlock()
	}

	if finished != nil {
		select {
		case <-finished:
		case <-ctx.Done():
			forget()
			return path, nil, fmt.Errorf("%s is still being re-encrypted and did not stop in time: %w", path, ctx.Err())
		}
	}

	warnings, err := v.Delete(ctx, id)
	forget()
	if err != nil {
		return path, warnings, err
	}

	// Nor a place at the back of the queue. The plan's list is tidied in
	// memory and written with the next change; a name on it that no file
	// carries is skipped everywhere it is read, so nothing waits on that
	// write.
	v.mu.Lock()
	if plan := v.manifest.Rekey; plan != nil && plan.isDeferred(id) {
		kept := plan.Deferred[:0]
		for _, d := range plan.Deferred {
			if d != id {
				kept = append(kept, d)
			}
		}
		plan.Deferred = kept
	}
	v.mu.Unlock()
	return path, warnings, nil
}

// pendingUnder counts the pending files in and below one folder.
func pendingUnder(m *Manifest, active, dir string) (files int, bytes int64) {
	dir = CleanDir(dir)
	for _, e := range m.Entries {
		if e.KeyID == active || !underDir(e.Dir, dir) {
			continue
		}
		files++
		bytes += e.Size
	}
	return files, bytes
}

// topFolder is the first segment of a folder path, or "/" for the root.
func topFolder(dir string) string {
	if dir == "/" || dir == "" {
		return "/"
	}
	rest := strings.TrimPrefix(dir, "/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return "/" + rest[:i]
	}
	return "/" + rest
}

// rekeyFoldersLocked breaks what is pending down by folder: the plan's folders
// in their own order, then every top-level folder with something left, largest
// first.
func (v *Vault) rekeyFoldersLocked() []RekeyFolder {
	out := []RekeyFolder{}
	queued := map[string]bool{}
	if plan := v.manifest.Rekey; plan != nil {
		for _, f := range plan.Folders {
			files, bytes := pendingUnder(v.manifest, v.dataKeyID, f)
			out = append(out, RekeyFolder{Path: f, Files: files, Bytes: bytes, Queued: true})
			queued[f] = true
		}
	}

	rest := map[string]*RekeyFolder{}
	for _, e := range v.manifest.Entries {
		if e.KeyID == v.dataKeyID {
			continue
		}
		top := topFolder(e.Dir)
		if queued[top] {
			continue
		}
		f := rest[top]
		if f == nil {
			f = &RekeyFolder{Path: top}
			rest[top] = f
		}
		f.Files++
		f.Bytes += e.Size
	}
	others := make([]RekeyFolder, 0, len(rest))
	for _, f := range rest {
		others = append(others, *f)
	}
	sort.Slice(others, func(i, j int) bool {
		if others[i].Bytes != others[j].Bytes {
			return others[i].Bytes > others[j].Bytes
		}
		return others[i].Path < others[j].Path
	})
	return append(out, others...)
}

// nextRekeyLocked picks the file the plan should move next: the smallest
// pending file in the first of the plan's folders that still has one, then —
// unless the plan stops at its folders — the smallest pending file anywhere.
// Smallest first because the count of files on the previous key is what the
// person is watching, and the budget is in bytes either way.
//
// idle is true when nothing the plan allows is left, which with files still
// pending means the plan stopped at its folders. Files the plan has lately
// failed on are passed over until their time comes round.
//
// down is the accounts whose last health check failed (unreachableAccounts,
// taken before the lock). A file that cannot be rebuilt from its parts on the
// other accounts is passed over too: nothing can read it, so trying it while
// the account is down is a failure and an hour's skip per file, which on a
// vault of thousands meant a drive unplugged for the afternoon left the plan
// nibbling at one failure a minute for days. Those files wait for the account
// instead, and move the moment a check finds it back. A file that *can* be
// rebuilt without the silent account is not held: migrateFile re-encrypts it
// from the parts within reach and writes down the part it leaves behind. An
// account nobody has checked is not held against a file.
// heldByUnreachableLocked counts what is waiting this way.
//
// The caller holds at least the read lock.
func (v *Vault) nextRekeyLocked(now time.Time, down map[string]bool) (entry *Entry, idle bool) {
	plan := v.manifest.Rekey
	if plan == nil {
		return nil, true
	}

	v.rekeyMu.Lock()
	skip := make(map[string]bool, len(v.rekey.skip))
	for id, until := range v.rekey.skip {
		if now.Before(until) {
			skip[id] = true
		}
	}
	v.rekeyMu.Unlock()

	// pick is the smallest pending file under dir that is not being left
	// alone, and whether there was any pending file under dir at all — the
	// difference between "nothing to do" and "nothing to do yet". Files left
	// for last are passed over until deferred is set, which the caller does
	// once everything else is done.
	pick := func(dir string, deferred bool) (best *Entry, any bool) {
		for _, e := range v.manifest.Entries {
			if e.KeyID == v.dataKeyID || !underDir(e.Dir, dir) {
				continue
			}
			if plan.isDeferred(e.ID) != deferred {
				continue
			}
			any = true
			if skip[e.ID] || heldBy(e, down) != "" {
				continue
			}
			if best == nil || e.Size < best.Size || (e.Size == best.Size && e.Path() < best.Path()) {
				best = e
			}
		}
		return best, any
	}

	waiting := false
	for _, deferred := range []bool{false, true} {
		for _, f := range plan.Folders {
			e, any := pick(f, deferred)
			if e != nil {
				return e, false
			}
			waiting = waiting || any
		}
		if plan.OnlyFolders {
			continue
		}
		e, any := pick("/", deferred)
		if e != nil {
			return e, false
		}
		waiting = waiting || any
	}
	return nil, !waiting
}

// heldBy is the first of the unreachable accounts a file has a part on, when
// the parts on the accounts that answered are too few to rebuild it — or ""
// when the file can be re-encrypted now, with or without every account.
func heldBy(e *Entry, down map[string]bool) string {
	if len(down) == 0 {
		return ""
	}
	first := ""
	within := map[int]bool{}
	for _, s := range e.Shards {
		if down[s.ProviderID] {
			if first == "" {
				first = s.ProviderID
			}
			continue
		}
		within[s.Part] = true
	}
	if first == "" || len(within) >= e.Scheme().Data {
		return ""
	}
	return first
}

// heldByUnreachableLocked counts the pending files nextRekeyLocked is holding
// back for an account that did not answer, by account ID — each file once,
// under the first such account it is on, so the counts sum to files rather
// than parts. The caller holds at least the read lock.
func (v *Vault) heldByUnreachableLocked(down map[string]bool) map[string]int {
	if len(down) == 0 || v.manifest.Rekey == nil {
		return nil
	}
	var held map[string]int
	for _, e := range v.manifest.Entries {
		if e.KeyID == v.dataKeyID {
			continue
		}
		if id := heldBy(e, down); id != "" {
			if held == nil {
				held = map[string]int{}
			}
			held[id]++
		}
	}
	return held
}

// RekeyStep is what one paced step came to.
type RekeyStep struct {
	// Moved says a file was re-encrypted, and Path and Bytes which.
	Moved bool
	Path  string
	Bytes int64

	// Wait is how long the caller should leave it before asking again: the
	// meter's answer when nothing moved because of it, a short pause when a
	// finish-now pass has the slot. Zero means ask again when convenient.
	Wait time.Duration

	// Idle says nothing the plan allows is left — no plan, paused, done, or
	// stopped at its folders.
	Idle bool

	// Skipped says the file being moved was deferred by request and the move
	// stopped; Path names it. Not a failure, and not charged.
	Skipped bool

	// Warning is why a file that was tried did not move.
	Warning string
}

// RekeyStep moves at most one file under the main vault's plan, if the plan
// and its meter allow one now. It is what the server calls on a tick; the CLI's
// foreground pace runs the same step in a loop.
//
// The meter is charged before the file moves rather than after, under the same
// lock that chooses it, so the charge rides in the index write the move itself
// makes and a restart in between cannot forget it. A file that would not move
// is refunded and left alone for a while, with the reason kept for the status.
func (v *Vault) RekeyStep(ctx context.Context, now time.Time) (RekeyStep, error) {
	if !v.migrateMu.TryLock() {
		return RekeyStep{Wait: 30 * time.Second}, nil
	}
	defer v.migrateMu.Unlock()

	// Before the index lock, never under it: see unreachableAccounts.
	down := v.unreachableAccounts()

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return RekeyStep{}, ErrLocked
	}
	plan := v.manifest.Rekey
	if !plan.Active() {
		v.mu.Unlock()
		return RekeyStep{Idle: true}, nil
	}
	entry, idle := v.nextRekeyLocked(now, down)
	if idle {
		v.mu.Unlock()
		return RekeyStep{Idle: true}, nil
	}
	if entry == nil {
		// Everything left is being left alone for now. A file waiting on an
		// account that did not answer is picked up by the health check that
		// finds the account back, which runs at least every MinHealthInterval,
		// so that is the longest worth waiting between looks.
		wait := rekeyRetryAfter / 4
		if len(v.heldByUnreachableLocked(down)) > 0 {
			wait = MinHealthInterval
		}
		v.mu.Unlock()
		return RekeyStep{Wait: wait}, nil
	}
	meter := plan.pacer()
	ready, wait := meter.Ready(now)
	plan.adopt(meter)
	if !ready {
		v.mu.Unlock()
		return RekeyStep{Wait: wait}, nil
	}
	meter.Spend(now, entry.Size)
	plan.adopt(meter)
	id, path, size := entry.ID, entry.Path(), entry.Size
	v.mu.Unlock()

	// No accounts: the file is re-sealed where it is, which is all a password
	// change asked for.
	moved, _, warnings, err := v.moveTracked(ctx, MainScope, id, nil)
	if err == nil && moved != "" {
		return RekeyStep{Moved: true, Path: path, Bytes: size, Warning: strings.Join(warnings, "; ")}, nil
	}

	// Deleted while it was being chosen, or would not move: either way the
	// budget was not spent on it.
	v.mu.Lock()
	if plan := v.manifest.Rekey; plan != nil {
		meter := plan.pacer()
		meter.Refund(now, size)
		plan.adopt(meter)
		if err != nil {
			// Written down, so a restart does not re-charge a file that did
			// not move. A failure to write is not worth a second warning: the
			// next move's write carries the same state.
			_ = v.persistLocked()
		}
	}
	v.mu.Unlock()

	if err == nil {
		return RekeyStep{}, nil
	}
	if errors.Is(err, ErrLocked) {
		return RekeyStep{}, err
	}
	if errors.Is(err, ErrSkipped) {
		// Deferred by request mid-move. Not a failure, so not a warning and
		// not left alone for an hour: it is at the back of the queue now.
		return RekeyStep{Path: path, Skipped: true}, nil
	}
	if ctx.Err() != nil {
		// The caller's own context went: a shutdown, not a fault of the file.
		return RekeyStep{}, ctx.Err()
	}
	warning := err.Error()
	v.rekeyMu.Lock()
	if v.rekey.skip == nil {
		v.rekey.skip = map[string]time.Time{}
	}
	if v.rekey.why == nil {
		v.rekey.why = map[string]string{}
	}
	v.rekey.skip[id] = now.Add(rekeyRetryAfter)
	v.rekey.why[id] = warning
	v.rekey.warnings = append(v.rekey.warnings, warning)
	if len(v.rekey.warnings) > maxRekeyWarnings {
		v.rekey.warnings = v.rekey.warnings[len(v.rekey.warnings)-maxRekeyWarnings:]
	}
	v.rekeyMu.Unlock()
	return RekeyStep{Path: path, Warning: warning}, nil
}

// RekeyKeepsUnlocked reports whether a plan with files left asks the server to
// hold the idle timer off. Cheap, for a loop that asks every tick.
func (v *Vault) RekeyKeepsUnlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return false
	}
	plan := v.manifest.Rekey
	if !plan.Active() || !plan.KeepUnlocked {
		return false
	}
	n, _ := pendingIn(v.manifest, v.dataKeyID)
	return n > 0
}
