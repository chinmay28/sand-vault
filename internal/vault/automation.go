package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/git"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// A folder that looks after itself.
//
// Everything else in this package waits to be asked. Somebody opens the
// degraded list and sees "4 files missing a spare part"; somebody notices an
// account has gone red in the sidebar and relocates a folder off it. That works
// exactly as long as somebody is looking, and the failure this is here for is
// the one nobody is looking at: an account whose token quietly expired in March,
// a bucket that stopped answering, a part that never landed because the cloud
// holding it was refusing for the afternoon. None of those makes anything look
// broken. A file with one part gone reads back perfectly, at full speed, right
// up until the day a second cloud is unavailable and it does not read back at
// all.
//
// So a folder can be given a standing instruction instead: on this schedule,
// check that every cloud is answering and every part of every file under here
// is where the index says it is — and, if the policy says so, put back what is
// missing without asking.
//
// Three things about the shape of that are worth stating up front, because they
// are what the rest of this file is arguing for.
//
// It is per folder, not per vault. A vault is not one thing: the films are
// replaceable and the scans of the passports are not, and the folder is the
// only place that distinction is ever written down. Filing the schedule where
// the distinction already lives means "check the important folder nightly and
// never touch the film library" is a thing you can say.
//
// A repair is always a rebuild. This is not a choice; it falls out of what has
// gone wrong. Moving a part from one cloud to another means reading it off the
// first one, and every case that brings us here — a cloud that is dark, an
// account that was disconnected, a part that was never written — is a case
// where there is nothing to read. The only repair that works is the one a
// degraded file already gets: gather the parts that can be read, cut the file
// again, write a full set out over the clouds that are answering. That costs
// the whole file on the wire and it costs it in memory, which is why
// RebuildLimit exists and why it has a default rather than being optional.
//
// It keeps the recovery model rather than the placement. A 4-of-6 file whose
// sixth cloud died goes back out as 4-of-6 over six clouds that answer — same
// storage, same losses survived, same number of accounts an attacker would have
// to hold together. Only when there are not enough clouds left answering to cut
// it that way is there a decision to make, and it is not one an unattended job
// should make on its own: Narrow is off unless it is turned on, and a policy
// that cannot keep a file's own code reports it and leaves the file alone.

// Cadence is how often a policy comes round.
type Cadence string

const (
	// CadenceHourly runs an hour after the last run, and an hour after that.
	// It has no wall-clock time of its own, because the useful thing about an
	// hourly job is the interval rather than the minute it lands on.
	CadenceHourly Cadence = "hourly"

	// CadenceDaily runs at a wall-clock time, every day.
	CadenceDaily Cadence = "daily"

	// CadenceWeekly runs at a wall-clock time on one weekday.
	CadenceWeekly Cadence = "weekly"
)

// Valid reports whether c is a cadence this build knows.
func (c Cadence) Valid() bool {
	return c == CadenceHourly || c == CadenceDaily || c == CadenceWeekly
}

// AutomationTask is which job a policy does when it comes round.
//
// A folder is not one kind of thing, and neither is looking after one. The
// parts of a file going missing and a mirrored repository falling behind its
// upstream are both "this folder has drifted from what it should be", and both
// want the same schedule, the same history and the same button — but the work
// in the middle has nothing in common. So the task is a choice made per policy,
// the schedule around it is shared, and what each one does is behind its own
// config and its own result.
type AutomationTask string

const (
	// TaskShards is the storage job: every cloud answering, every part of every
	// file where the index says it went, and what is missing put back.
	TaskShards AutomationTask = "shards"

	// TaskGit is the mirror job: every tracked repository under the folder
	// asked whether its upstream has moved, and the ones that have re-fetched
	// and re-stored. See gitrepo.go.
	TaskGit AutomationTask = "git"
)

// Valid reports whether t is a task this build knows.
func (t AutomationTask) Valid() bool {
	return t == TaskShards || t == TaskGit
}

// AutomationAction is how far a policy goes: look, or look and fix.
//
// The looking half is shared — ActionCheck means the same thing whichever task
// is doing it, which is why it is one type rather than one per task — and the
// fixing half is named after the work, because "rebalance" and "pull" are
// different enough that a single word for both would be a word for neither.
type AutomationAction string

const (
	// ActionCheck looks and writes down what it found. Nothing moves, nothing
	// is rebuilt, nothing is fetched, and no byte leaves any account. It is the
	// honest starting point for a folder somebody is not yet ready to let a
	// schedule rewrite, and it is valid for every task.
	ActionCheck AutomationAction = "check"

	// ActionRebalance is the shard task's fixing half: the files that cannot be
	// read whole are rebuilt onto the clouds that are answering, keeping each
	// file's own erasure code where there are clouds enough for it. See the note
	// at the top of this file for why this is a rebuild.
	ActionRebalance AutomationAction = "rebalance"

	// ActionPull is the git task's fixing half: a repository whose upstream has
	// moved is fetched and stored again.
	ActionPull AutomationAction = "pull"
)

// Valid reports whether a is an action this build knows at all. Whether it is
// one the chosen task can carry out is a separate question, asked by fits.
func (a AutomationAction) Valid() bool {
	return a == ActionCheck || a == ActionRebalance || a == ActionPull
}

// fits reports whether this action means anything for the given task.
func (a AutomationAction) fits(task AutomationTask) bool {
	if a == ActionCheck {
		return true
	}
	switch task {
	case TaskShards:
		return a == ActionRebalance
	case TaskGit:
		return a == ActionPull
	}
	return false
}

// AutomationTrigger says what set a run going, which is the difference between
// a history somebody can read and a list of identical rows.
type AutomationTrigger string

const (
	// TriggerSchedule is the policy's own clock coming round.
	TriggerSchedule AutomationTrigger = "schedule"

	// TriggerManual is somebody pressing the button.
	TriggerManual AutomationTrigger = "manual"
)

// ErrNoAutomation is returned when a folder has no policy on it.
var ErrNoAutomation = errors.New("this folder has no automation policy")

// ErrAutomationBusy is returned when a sweep is asked for while one is already
// running. One at a time, vault-wide: a sweep can rebuild files, and two of
// them at once would be two of everything — two gathers in memory, two sets of
// uploads competing for the same accounts.
var ErrAutomationBusy = errors.New("an automation sweep is already running")

const (
	// automationHistory is how many past runs a policy keeps. Enough to see
	// whether a folder has been quietly losing parts for a fortnight, and few
	// enough that the index does not grow without bound — this is written into
	// the encrypted vault file and replicated to every account as a backup.
	automationHistory = 8

	// automationWarningsKept caps the warnings stored with a past run. A sweep
	// over a folder whose cloud is dark can produce one warning per file, and
	// the answer to "there are nine thousand of these" is the count, not nine
	// thousand lines in the index. The run handed back to whoever asked for it
	// keeps all of them.
	automationWarningsKept = 20

	// automationPingTimeout is how long one account gets to answer the probe
	// that opens a sweep. The same twenty seconds the accounts panel gives it.
	automationPingTimeout = 20 * time.Second

	// automationCheckWindow is how many files are checked at once. Each one is
	// a handful of metadata requests across every account holding it, so this
	// is already a fan-out; the window is there to keep a folder of ten
	// thousand files from opening ten thousand of them.
	automationCheckWindow = 4

	// DefaultRebuildLimit is how large a file an unattended repair will rebuild
	// unless the policy says otherwise.
	//
	// A rebuild gathers the file into memory before it cuts it again — see
	// migrateFile — so the ceiling is not about time or bandwidth but about
	// what the machine has. SAND is meant to run on a Raspberry Pi, where a
	// scheduled job that decides at three in the morning to hold a 40 GB film
	// in RAM is a machine that stops answering ssh. Files past this are counted
	// and named in the run's report and left for somebody to repair by hand,
	// from the browser, where they can watch it happen.
	DefaultRebuildLimit int64 = 1 << 30
)

// Automation is one folder's standing instruction.
//
// It lives in the manifest, keyed by the folder it was made on, which is what
// makes it travel with a rename and vanish with a delete. It covers that folder
// and everything under it: a policy on /archive looks after /archive/2019 too,
// and a policy on / looks after the whole vault.
type Automation struct {
	// Enabled is the switch. A policy that is off keeps its settings and its
	// history and is simply never due — which is what somebody wants when they
	// are turning a schedule off for a week, rather than deleting it and typing
	// it again.
	Enabled bool `json:"enabled"`

	Cadence Cadence `json:"cadence"`

	// At is the wall-clock time a daily or weekly policy runs, written "10:00",
	// in the local time of the machine running the server. Local rather than
	// UTC because "every day at 10 am" is a statement about somebody's morning,
	// and a schedule that drifts an hour twice a year against the clock on the
	// wall is a schedule nobody trusts. Ignored by an hourly policy.
	At string `json:"at,omitempty"`

	// Weekday is which day a weekly policy runs, Sunday being zero. Ignored by
	// the other two.
	Weekday time.Weekday `json:"weekday,omitempty"`

	// Task is the job this policy does. An empty task is read as TaskShards,
	// which is what a client that has only ever heard of the storage job sends.
	Task AutomationTask `json:"task,omitempty"`

	// Action is how far it goes: look, or look and fix. Which fixing actions
	// mean anything depends on the task — see AutomationAction.fits.
	Action AutomationAction `json:"action"`

	// Shards holds the knobs the storage task has and no other, Git those of
	// the mirror task. The one named by Task is the one that is read; the other
	// is nil, and a policy switched from one task to the other keeps neither.
	//
	// They are separate structs rather than a union of every field because the
	// alternative is a policy with a dozen settings of which four apply, and no
	// way for the browser or the CLI to tell which four.
	Shards *ShardPolicy `json:"shards,omitempty"`
	Git    *GitPolicy   `json:"git,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// LastRunAt is when the last run finished, and it is what the next one is
	// counted from. A run that found nothing to do stamps it just the same, as
	// does one that could not do anything at all — the schedule is a schedule,
	// not a retry loop, and "run it now" is the answer to impatience.
	LastRunAt time.Time `json:"last_run_at,omitempty"`

	// History is the past runs, newest first, capped at automationHistory.
	History []AutomationRun `json:"history,omitempty"`
}

// ShardPolicy is what the storage task is allowed to do about what it finds.
type ShardPolicy struct {
	// Narrow allows a repair to cut a file with a smaller code than it has,
	// when there are not enough clouds answering to keep its own.
	//
	// Off by default, and the default is the important half. A 4-of-6 file
	// re-cut as 3-of-4 because two clouds were down for an hour has been
	// permanently made less durable and less secret by a temporary fault, and
	// the file cannot be widened back without another full rebuild. Left off, a
	// policy that cannot keep a file's code says so and leaves the file exactly
	// as it was, which is the recoverable answer.
	Narrow bool `json:"narrow,omitempty"`

	// MaxRepairs bounds how many files one run will rebuild. Zero is no bound.
	//
	// It is a bound on the run rather than on the folder: what is left over is
	// counted, named in the report, and picked up by the next run, worst first.
	// A vault that comes back from a bad week with four thousand short files
	// should repair them over four thousand files' worth of nights rather than
	// spend a fortnight of bandwidth in one go.
	MaxRepairs int `json:"max_repairs,omitempty"`

	// RebuildLimit is the largest file this policy will rebuild unattended.
	// Zero means DefaultRebuildLimit; a negative value means no ceiling at all,
	// which is a thing to choose deliberately on a machine with the memory for
	// it. See DefaultRebuildLimit for what the ceiling is protecting.
	RebuildLimit int64 `json:"rebuild_limit,omitempty"`
}

// GitPolicy is what the mirror task is allowed to do about what it finds.
type GitPolicy struct {
	// MaxRepos bounds how many repositories one run will re-fetch. Zero is no
	// bound. Exactly ShardPolicy.MaxRepairs and for the same reason: the ones
	// left over are counted, named, and picked up next time.
	//
	// Note that it bounds the *fetching*, not the asking. Every tracked
	// repository is still asked whether it has moved, because that costs a few
	// kilobytes; the bound is on the expensive half.
	MaxRepos int `json:"max_repos,omitempty"`

	// SizeLimit is the largest bundle this policy will re-fetch unattended.
	// Zero means DefaultBundleLimit; a negative value means no ceiling.
	//
	// The ceiling is on the stored bundle rather than on the working tree,
	// because that is the figure SAND already knows before it starts. See
	// DefaultBundleLimit.
	SizeLimit int64 `json:"size_limit,omitempty"`

	// Prune lets the task delete a stored bundle whose upstream has gone —
	// answered 404, or had its access revoked. Off by default, emphatically:
	// a repository that has been taken down is exactly the repository somebody
	// is most glad to have a copy of, and a temporary outage must never be
	// allowed to look like a reason to throw the last copy away.
	Prune bool `json:"prune,omitempty"`
}

// clone takes a copy deep enough that the caller cannot reach back into the
// index through it.
func (a *Automation) clone() *Automation {
	if a == nil {
		return nil
	}
	out := *a
	out.History = append([]AutomationRun(nil), a.History...)
	if a.Shards != nil {
		shards := *a.Shards
		out.Shards = &shards
	}
	if a.Git != nil {
		g := *a.Git
		out.Git = &g
	}
	return &out
}

// shards is the storage config with the zero value filled in, so that reading
// it never has to test for nil. A policy of another task has none, and asking
// for it is a bug rather than a default — the callers are all inside the shard
// sweep, which does not run for another task.
func (a *Automation) shards() ShardPolicy {
	if a.Shards == nil {
		return ShardPolicy{}
	}
	return *a.Shards
}

// git is the mirror config with the zero value filled in, on the same terms.
func (a *Automation) git() GitPolicy {
	if a.Git == nil {
		return GitPolicy{}
	}
	return *a.Git
}

// rebuildCeiling is the largest file this policy will rebuild, with the zero
// value resolved to the default and a negative value to no ceiling.
func (a *Automation) rebuildCeiling() int64 {
	limit := a.shards().RebuildLimit
	switch {
	case limit < 0:
		return 0
	case limit == 0:
		return DefaultRebuildLimit
	}
	return limit
}

// bundleCeiling is the largest stored bundle the mirror task will re-fetch, on
// the same terms.
func (a *Automation) bundleCeiling() int64 {
	limit := a.git().SizeLimit
	switch {
	case limit < 0:
		return 0
	case limit == 0:
		return DefaultBundleLimit
	}
	return limit
}

// FolderAutomation is a policy together with where it was made, which is what
// anything outside this package needs: the map key is the folder, and a caller
// handed a bare Automation would not know which folder it looks after.
type FolderAutomation struct {
	// Vault names which of the vaults inside the file this policy is in, empty
	// for the main one.
	Vault Scope `json:"vault,omitempty"`

	Folder string `json:"folder"`

	*Automation

	// NextRunAt is when this policy comes round again, absent while it is off.
	// Computed rather than stored: it is a function of the cadence and the last
	// run, and storing it would be storing something that can go stale.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`

	// Trouble says the last run found something worth looking at: files short of
	// their parts, files past repairing, an account that did not answer, or a
	// sweep that could not be carried out at all.
	//
	// It is one boolean because of where it is read. A folder listing carries a
	// policy without its history — see automationForLocked — and the browser
	// still has to decide whether to draw the folder's button as "looked after"
	// or as "looked after, and the last look found something". Fetching eight
	// runs' worth of warnings per folder to answer that would be absurd.
	Trouble bool `json:"trouble,omitempty"`
}

// AutomationRun is what one sweep came to.
//
// The common half is the schedule's: which folder, which task, when, and which
// accounts were answering. What the task itself found is behind Shards or Git,
// because a count of files short of a part means nothing to the mirror job and
// a count of repositories means nothing to the storage one — flattening both
// into one struct would make every run a dozen fields of which half are always
// zero, with nothing to say which half.
type AutomationRun struct {
	Folder     string            `json:"folder"`
	Vault      Scope             `json:"vault,omitempty"`
	Trigger    AutomationTrigger `json:"trigger"`
	Task       AutomationTask    `json:"task"`
	Action     AutomationAction  `json:"action"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`

	// Accounts is how many were connected when the sweep opened, and Offline
	// names the ones that did not answer the probe. The names rather than the
	// IDs, because this is read by a person a fortnight later.
	Accounts int      `json:"accounts"`
	Offline  []string `json:"offline,omitempty"`

	// Shards is what the storage task found, Git what the mirror task found.
	// Exactly the one named by Task is set.
	Shards *ShardResult `json:"shards,omitempty"`
	Git    *GitResult   `json:"git,omitempty"`

	Warnings []string `json:"warnings,omitempty"`

	// Error is set when the sweep could not be carried out at all — the vault
	// locked mid-run, no account answering, no git on the machine. Whichever
	// result is set then holds whatever had been reached.
	Error string `json:"error,omitempty"`
}

// ShardResult is what one sweep of the storage task came to.
type ShardResult struct {
	// Files is how many were under the folder, and Checked how many this run
	// actually looked at — they differ when an inner folder's own policy had
	// already looked at some of them in the same sweep.
	Files   int `json:"files"`
	Checked int `json:"checked"`

	// The three states a checked file can be in, which between them account for
	// every one of them. Whole means every part the index records was found
	// where it records it. Short means at least one was not, and enough were to
	// rebuild the file. AtRisk means not even enough to rebuild: the file
	// cannot be read right now, and no amount of moving parts around will
	// change that until whatever is holding them comes back.
	Whole  int `json:"whole"`
	Short  int `json:"short"`
	AtRisk int `json:"at_risk"`

	// What the repair half did. Repaired is files rebuilt onto answering
	// clouds, PartsWritten the parts that took, and Bytes what went over the
	// wire — a rebuild is the file down once and up once, so this is the real
	// cost of the night.
	Repaired     int   `json:"repaired"`
	PartsWritten int   `json:"parts_written"`
	Bytes        int64 `json:"bytes"`

	// Failed is files a repair was attempted on and did not finish. Deferred is
	// files that needed one and did not get one this time: past the run's
	// repair bound, past the rebuild ceiling, or with too few clouds answering
	// to keep the file's code. Both are named in Warnings.
	Failed   int `json:"failed"`
	Deferred int `json:"deferred"`
}

// clean reports whether the storage sweep found nothing worth saying.
func (r *ShardResult) clean() bool {
	return r == nil || (r.Short == 0 && r.AtRisk == 0 && r.Failed == 0 && r.Deferred == 0)
}

// GitResult is what one sweep of the mirror task came to.
type GitResult struct {
	// Repos is how many tracked repositories were under the folder, and Checked
	// how many were actually asked — they differ when an inner folder's own
	// policy had already asked about some of them in the same tick.
	Repos   int `json:"repos"`
	Checked int `json:"checked"`

	// Current is the good case and the common one: the upstream still holds
	// exactly what the stored bundle holds, settled by one ref advertisement
	// and no transfer at all. Updated is the ones that had moved and were
	// fetched and stored again.
	Current int `json:"current"`
	Updated int `json:"updated"`

	// Gone is repositories whose upstream did not answer at all — deleted,
	// renamed, or access revoked. They are counted rather than acted on: see
	// GitPolicy.Prune for why the stored copy stays.
	Gone int `json:"gone,omitempty"`

	// Pruned is the stored bundles deleted because their upstream is gone and
	// the policy was told it may. Zero unless GitPolicy.Prune is on.
	Pruned int `json:"pruned,omitempty"`

	// Commits is how many new commits arrived across every repository updated,
	// which is the one figure that says what a week of somebody's project
	// actually amounted to.
	Commits int `json:"commits,omitempty"`

	// Failed is repositories a refresh was attempted on and did not finish.
	// Deferred is ones that had moved and were not fetched this time: past the
	// run's bound, or past the size ceiling. Both are named in Warnings.
	Failed   int `json:"failed"`
	Deferred int `json:"deferred"`

	// Bytes is what the newly stored bundles came to.
	Bytes int64 `json:"bytes"`
}

// clean reports whether the mirror sweep found nothing worth saying. An updated
// repository is not trouble — it is the job working.
func (r *GitResult) clean() bool {
	return r == nil || (r.Failed == 0 && r.Deferred == 0 && r.Gone == 0)
}

// Clean reports whether the sweep found nothing to fix and nothing to say.
func (r *AutomationRun) Clean() bool {
	return r != nil && r.Error == "" && len(r.Offline) == 0 &&
		r.Shards.clean() && r.Git.clean()
}

// stored is the copy kept in the index: the same run with its warnings capped.
func (r *AutomationRun) stored() AutomationRun {
	out := *r
	if len(out.Warnings) > automationWarningsKept {
		kept := append([]string(nil), out.Warnings[:automationWarningsKept]...)
		kept = append(kept, fmt.Sprintf("… and %d more, not kept",
			len(out.Warnings)-automationWarningsKept))
		out.Warnings = kept
	} else {
		out.Warnings = append([]string(nil), out.Warnings...)
	}
	return out
}

// ---------------------------------------------------------------------------
// The schedule
// ---------------------------------------------------------------------------

// check validates a policy at the point somebody is setting it, so that a
// mistyped time is refused where it was typed rather than silently running at
// midnight for a year.
func (a *Automation) check() error {
	// An unnamed task is the storage one, which is what a client that predates
	// there being a choice sends, and what somebody setting a policy without
	// saying which job means.
	if a.Task == "" {
		a.Task = TaskShards
	}
	if !a.Task.Valid() {
		return fmt.Errorf("%q is not a task — choose %s or %s", a.Task, TaskShards, TaskGit)
	}
	if !a.Cadence.Valid() {
		return fmt.Errorf("%q is not a cadence — choose %s, %s or %s",
			a.Cadence, CadenceHourly, CadenceDaily, CadenceWeekly)
	}
	if !a.Action.Valid() {
		return fmt.Errorf("%q is not an action — choose %s, %s or %s",
			a.Action, ActionCheck, ActionRebalance, ActionPull)
	}
	if !a.Action.fits(a.Task) {
		return fmt.Errorf("the %s task cannot %s — choose %s or %s",
			a.Task, a.Action, ActionCheck, fixFor(a.Task))
	}
	if a.Cadence != CadenceHourly {
		if _, _, err := parseClock(a.At); err != nil {
			return err
		}
	}
	if a.Cadence == CadenceWeekly && (a.Weekday < time.Sunday || a.Weekday > time.Saturday) {
		return fmt.Errorf("%d is not a day of the week — 0 is Sunday and 6 is Saturday", a.Weekday)
	}

	// Keep only the config the chosen task reads. A policy switched from one
	// job to the other must not carry the settings of the job it no longer
	// does, waiting to surprise somebody who switches it back.
	switch a.Task {
	case TaskShards:
		a.Git = nil
		if a.shards().MaxRepairs < 0 {
			return fmt.Errorf("a repair bound of %d is not a number of files — use 0 for no bound",
				a.shards().MaxRepairs)
		}
	case TaskGit:
		a.Shards = nil
		if a.git().MaxRepos < 0 {
			return fmt.Errorf("a bound of %d is not a number of repositories — use 0 for no bound",
				a.git().MaxRepos)
		}
		if a.Action == ActionPull && !git.Available() {
			return fmt.Errorf(
				"this machine has no git for SAND to borrow, so a policy that fetches would "+
					"fail every time it came round — install git, or set the policy to %s",
				ActionCheck)
		}
	}
	return nil
}

// fixFor names the fixing action of a task, for the message that explains why
// the one somebody chose is not it.
func fixFor(task AutomationTask) AutomationAction {
	if task == TaskGit {
		return ActionPull
	}
	return ActionRebalance
}

// parseClock reads a wall-clock time written "10:00" or "9:30".
func parseClock(at string) (hour, minute int, err error) {
	text := strings.TrimSpace(at)
	h, m, ok := strings.Cut(text, ":")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not a time of day — write one as HH:MM, such as 10:00", at)
	}
	hour, err = strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q is not a time of day — the hour has to be 0 to 23", at)
	}
	minute, err = strconv.Atoi(strings.TrimSpace(m))
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is not a time of day — the minute has to be 0 to 59", at)
	}
	return hour, minute, nil
}

// nextAfter is the first moment strictly after `after` that this policy is due,
// reckoned in loc.
//
// A time that will not parse is read as midnight rather than refused. Nothing
// this build writes can produce one — check() runs first — but a hand-edited
// vault file can, and a folder that quietly runs at the wrong hour is a better
// failure than a folder that stops being looked after.
func (a *Automation) nextAfter(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	from := after.In(loc)

	if a.Cadence == CadenceHourly {
		return after.Add(time.Hour)
	}

	hour, minute, err := parseClock(a.At)
	if err != nil {
		hour, minute = 0, 0
	}
	next := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, loc)
	for !next.After(from) {
		next = next.AddDate(0, 0, 1)
	}
	if a.Cadence == CadenceWeekly {
		for next.Weekday() != a.Weekday {
			next = next.AddDate(0, 0, 1)
		}
	}
	return next
}

// DueAt is when this policy next comes round, counted from its last run — or
// from when it was made, for one that has never run. A policy created at nine
// with a ten o'clock schedule first runs at ten, not the moment it is saved:
// making a policy is not asking for a sweep, and there is a button for that.
func (a *Automation) DueAt() time.Time {
	anchor := a.LastRunAt
	if anchor.IsZero() {
		anchor = a.CreatedAt
	}
	return a.nextAfter(anchor, time.Local)
}

// Due reports whether this policy should run now.
//
// It catches up rather than counting missed slots. A machine that was off for
// three days comes back with one sweep owing, not three: the point is the state
// of the files, and checking it three times in a row would find the same answer
// three times.
func (a *Automation) Due(now time.Time) bool {
	return a.Enabled && !now.Before(a.DueAt())
}

// ---------------------------------------------------------------------------
// Reading and writing the policies
// ---------------------------------------------------------------------------

// AutomationFor returns the policy on one folder, or nil when it has none.
// Only the folder itself: unlike the film-lookup switch, a policy is not
// inherited downwards, because it already covers everything under it.
func (v *Vault) AutomationFor(scope Scope, dir string) (*FolderAutomation, error) {
	dir = CleanDir(dir)

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	auto := m.Automations[dir]
	if auto == nil {
		return nil, nil
	}
	return decorate(scope, dir, auto.clone()), nil
}

// automationForLocked is AutomationFor without the lock or the copy, for the
// callers that are already holding the read lock and are about to serialize the
// result rather than keep it. The caller must hold at least the read lock.
// The history is left off: a listing needs to know that the folder is looked
// after, on what schedule, and how the last run went, and a folder of films
// should not carry eight runs' worth of warnings on every navigation. The
// dialog that shows the history asks for the policy itself.
func (v *Vault) automationForLocked(scope Scope, m *Manifest, dir string) *FolderAutomation {
	auto := m.Automations[CleanDir(dir)]
	if auto == nil {
		return nil
	}
	// Decorated first and trimmed after: whether the last run found anything is
	// worked out from the history, and is the one thing about it a listing needs.
	fa := decorate(scope, CleanDir(dir), auto.clone())
	fa.History = nil
	return fa
}

// Automations lists the policies in one vault, nearest the root first.
func (v *Vault) Automations(scope Scope) ([]FolderAutomation, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	return automationsIn(scope, m), nil
}

// AllAutomations lists every policy this vault can currently see: the main
// vault's, and those of each sub vault that is open.
//
// A shut sub vault's policies are not here and are not run, and that is the
// only answer available — its index is sealed under a password nobody has
// entered. Opening it puts them back in the schedule.
func (v *Vault) AllAutomations() ([]FolderAutomation, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	out := automationsIn(MainScope, v.manifest)
	ids := make([]string, 0, len(v.subs))
	for id := range v.subs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, automationsIn(Scope(id), v.subs[id].manifest)...)
	}
	return out, nil
}

// automationsIn collects one manifest's policies, folder order.
func automationsIn(scope Scope, m *Manifest) []FolderAutomation {
	out := make([]FolderAutomation, 0, len(m.Automations))
	dirs := make([]string, 0, len(m.Automations))
	for dir := range m.Automations {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		out = append(out, *decorate(scope, dir, m.Automations[dir].clone()))
	}
	return out
}

// decorate wraps a policy with where it lives, when it is next due, and whether
// the last time it ran it found anything.
func decorate(scope Scope, dir string, auto *Automation) *FolderAutomation {
	fa := &FolderAutomation{Vault: scope, Folder: dir, Automation: auto}
	if auto.Enabled {
		next := auto.DueAt()
		fa.NextRunAt = &next
	}
	if len(auto.History) > 0 {
		fa.Trouble = !auto.History[0].Clean()
	}
	return fa
}

// SetAutomation puts a policy on a folder, replacing whatever was there.
//
// The history and the last-run time of an existing policy are carried across
// rather than reset: editing the hour a folder is checked at does not unsay
// what the last four checks found, and it should not make the folder
// immediately due either.
func (v *Vault) SetAutomation(scope Scope, dir string, want Automation) (*FolderAutomation, error) {
	dir = CleanDir(dir)
	if err := want.check(); err != nil {
		return nil, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	if !m.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	now := time.Now().UTC()
	fresh := want
	fresh.CreatedAt = now
	fresh.UpdatedAt = now
	fresh.History = nil
	fresh.LastRunAt = time.Time{}
	if previous := m.Automations[dir]; previous != nil {
		fresh.CreatedAt = previous.CreatedAt
		fresh.LastRunAt = previous.LastRunAt
		fresh.History = append([]AutomationRun(nil), previous.History...)
	}

	previous, had := m.Automations[dir]
	if m.Automations == nil {
		m.Automations = map[string]*Automation{}
	}
	m.Automations[dir] = &fresh
	if err := v.persistLocked(); err != nil {
		// Put the index back the way the file on disk still has it.
		if had {
			m.Automations[dir] = previous
		} else {
			delete(m.Automations, dir)
			if len(m.Automations) == 0 {
				m.Automations = nil
			}
		}
		return nil, err
	}
	return decorate(scope, dir, fresh.clone()), nil
}

// RemoveAutomation takes the policy off a folder, history and all. Removing one
// is not the same as turning it off — Enabled is for that — and this is the
// only way the history goes away short of deleting the folder.
func (v *Vault) RemoveAutomation(scope Scope, dir string) error {
	dir = CleanDir(dir)

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return err
	}
	previous, had := m.Automations[dir]
	if !had {
		return ErrNoAutomation
	}

	delete(m.Automations, dir)
	if len(m.Automations) == 0 {
		m.Automations = nil
	}
	if err := v.persistLocked(); err != nil {
		if m.Automations == nil {
			m.Automations = map[string]*Automation{}
		}
		m.Automations[dir] = previous
		return err
	}
	return nil
}

// dropAutomations forgets the policies on a folder and everything under it,
// which is what deleting the folder means. The counterpart of dropMovieFolders,
// and the reason a deleted folder does not leave a schedule behind sweeping a
// tree that is not there.
func (m *Manifest) dropAutomations(dir string) {
	dir = CleanDir(dir)
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	for stored := range m.Automations {
		if stored == dir || strings.HasPrefix(stored, prefix) {
			delete(m.Automations, stored)
		}
	}
	if len(m.Automations) == 0 {
		m.Automations = nil
	}
}

// ---------------------------------------------------------------------------
// Running them
// ---------------------------------------------------------------------------

// DueAutomations lists the policies whose time has come, deepest folder first.
//
// Deepest first is what makes overlapping policies behave. A policy on
// /archive/2019 and a policy on /archive both cover the same files, and running
// the inner one first means those files are attributed to the folder somebody
// was most specific about — the outer sweep then skips them rather than
// checking every one of them a second time.
func (v *Vault) DueAutomations(now time.Time) ([]FolderAutomation, error) {
	all, err := v.AllAutomations()
	if err != nil {
		return nil, err
	}
	due := make([]FolderAutomation, 0, len(all))
	for _, fa := range all {
		if fa.Due(now) {
			due = append(due, fa)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		di, dj := depthOf(due[i].Folder), depthOf(due[j].Folder)
		if di != dj {
			return di > dj
		}
		if due[i].Vault != due[j].Vault {
			return due[i].Vault < due[j].Vault
		}
		return due[i].Folder < due[j].Folder
	})
	return due, nil
}

// depthOf is how far a folder is from the root.
func depthOf(dir string) int {
	if dir == "/" || dir == "" {
		return 0
	}
	return strings.Count(dir, "/")
}

// RunDueAutomations runs every policy that is due and returns what each one
// came to. It is what a scheduler calls on its tick.
//
// A file covered by more than one due policy is checked once, by the deepest of
// them. Nothing is returned when nothing is due, which is almost every tick.
func (v *Vault) RunDueAutomations(ctx context.Context, now time.Time) ([]*AutomationRun, error) {
	due, err := v.DueAutomations(now)
	if err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}

	seen := map[string]bool{}
	runs := make([]*AutomationRun, 0, len(due))
	for _, fa := range due {
		if err := ctx.Err(); err != nil {
			return runs, err
		}
		run, err := v.runAutomation(ctx, fa.Vault, fa.Folder, TriggerSchedule, seen)
		if err != nil {
			if errors.Is(err, ErrLocked) || errors.Is(err, ErrAutomationBusy) {
				return runs, err
			}
			// A folder deleted between the listing and the run, or a sub vault
			// locked in between. Nothing to report and nothing to stop for.
			continue
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// AutomationRunning reports which folder's sweep is in flight, if any. It is
// what lets the browser draw a run in progress rather than offering a button
// that would only be refused.
func (v *Vault) AutomationRunning() (string, bool) {
	v.autoMu.Lock()
	defer v.autoMu.Unlock()
	return v.autoActive, v.autoActive != ""
}

// RunAutomation carries out one folder's policy now, whether or not it is due
// and whether or not it is switched on. It is the "run it now" button, and it
// stamps the last-run time exactly as a scheduled run does — having just
// checked a folder, there is no reason to check it again in an hour.
func (v *Vault) RunAutomation(ctx context.Context, scope Scope, dir string) (*AutomationRun, error) {
	return v.runAutomation(ctx, scope, dir, TriggerManual, nil)
}

// runAutomation is the sweep. seen, when given, is the set of file IDs an
// earlier policy in the same tick has already accounted for; it is added to.
func (v *Vault) runAutomation(ctx context.Context, scope Scope, dir string, trigger AutomationTrigger, seen map[string]bool) (*AutomationRun, error) {
	dir = CleanDir(dir)

	// One sweep at a time, vault-wide. Claimed before anything is read so that
	// two ticks landing together cannot both decide the coast is clear.
	v.autoMu.Lock()
	if v.autoActive != "" {
		busy := v.autoActive
		v.autoMu.Unlock()
		return nil, fmt.Errorf("%w (%s)", ErrAutomationBusy, busy)
	}
	v.autoActive = dir
	v.autoMu.Unlock()
	defer func() {
		v.autoMu.Lock()
		v.autoActive = ""
		v.autoMu.Unlock()
	}()

	// Everything the sweep needs, taken in one pass under the lock: the network
	// work that follows must not hold it (§13).
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	stored := m.Automations[dir]
	if stored == nil {
		v.mu.RUnlock()
		return nil, ErrNoAutomation
	}
	if !m.FolderExists(dir) {
		v.mu.RUnlock()
		return nil, fmt.Errorf("no such folder: %s", dir)
	}
	policy := stored.clone()
	found := m.Descendants(dir)
	entries := make([]*Entry, 0, len(found))
	for _, e := range found {
		entries = append(entries, copyEntry(e))
	}
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path() < entries[j].Path() })

	run := &AutomationRun{
		Folder:    dir,
		Vault:     scope,
		Trigger:   trigger,
		Task:      policy.Task,
		Action:    policy.Action,
		StartedAt: time.Now().UTC(),
		Accounts:  len(configs),
	}

	// Half the question is "are all the clouds accessible", and it is asked
	// once for the folder rather than once per file. It is also what keeps the
	// other half cheap: a part on an account that did not answer is known to be
	// unreachable without asking that account about it ten thousand times, each
	// one waiting out its own timeout.
	online, offline := v.probeAccounts(ctx, configs)
	for _, cfg := range configs {
		if reason, dark := offline[cfg.ID]; dark {
			run.Offline = append(run.Offline, cfg.Name)
			run.Warnings = append(run.Warnings,
				fmt.Sprintf("%s did not answer: %s", cfg.Name, reason))
		}
	}
	if len(online) == 0 {
		run.Error = "no connected account answered, so nothing could be checked"
		return v.finishAutomation(scope, dir, run)
	}

	byID := make(map[string]provider.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}
	dark := map[string]bool{}
	for id := range offline {
		dark[id] = true
	}

	// Everything to here is the schedule's: which folder, which files, which
	// accounts are answering. What to do with them is the task's.
	switch policy.Task {
	case TaskGit:
		v.sweepGit(ctx, scope, policy, entries, run, seen)
	default:
		v.sweepShards(ctx, scope, policy, entries, run, online, byID, dark, seen)
	}
	return v.finishAutomation(scope, dir, run)
}

// sweepShards is the storage task: every part of every file under the folder,
// against the accounts the index says are holding them, and what is missing put
// back where the policy allows it.
func (v *Vault) sweepShards(
	ctx context.Context,
	scope Scope,
	policy *Automation,
	entries []*Entry,
	run *AutomationRun,
	online map[string]bool,
	byID map[string]provider.Config,
	dark map[string]bool,
	seen map[string]bool,
) {
	res := &ShardResult{Files: len(entries)}
	run.Shards = res

	states := v.checkEntries(ctx, entries, online, byID, seen)
	res.Checked = len(states)

	needy := make([]fileState, 0, len(states))
	for _, st := range states {
		switch {
		case st.reachable >= st.scheme.Total:
			res.Whole++
			continue
		case st.reachable >= st.scheme.Data:
			res.Short++
		default:
			res.AtRisk++
		}
		needy = append(needy, st)
	}

	// Worst first, so that a run with a repair bound spends it on the files
	// closest to being unreadable rather than on whichever came first
	// alphabetically.
	sort.SliceStable(needy, func(i, j int) bool {
		si := needy[i].reachable - needy[i].scheme.Data
		sj := needy[j].reachable - needy[j].scheme.Data
		if si != sj {
			return si < sj
		}
		return needy[i].entry.Path() < needy[j].entry.Path()
	})

	for _, st := range needy {
		if st.reachable < st.scheme.Data {
			run.Warnings = append(run.Warnings, fmt.Sprintf(
				"%s can be read from only %d of the %d parts its %s code needs — "+
					"nothing here can repair that until whatever is holding the rest comes back",
				st.entry.Path(), st.reachable, st.scheme.Data, st.scheme))
			continue
		}
		run.Warnings = append(run.Warnings, fmt.Sprintf(
			"%s has %d of its %d parts on a cloud that did not answer or was never written",
			st.entry.Path(), st.scheme.Total-st.reachable, st.scheme.Total))
	}

	if policy.Action != ActionRebalance {
		return
	}
	v.repairFiles(ctx, scope, policy, needy, online, dark, run)
}

// fileState is one file as the check found it.
type fileState struct {
	entry  *Entry
	scheme archive.Scheme

	// reachable is how many distinct parts were found where the index says they
	// are, and absent names the recorded parts that were not — which is what a
	// repair has to be told, since the index by itself still believes in them.
	reachable int
	absent    map[int]bool
}

// probeAccounts asks every connected account whether it is there, all at once.
// online is by ID for the ones that answered; offline is by ID for the ones
// that did not, carrying what they said instead.
func (v *Vault) probeAccounts(ctx context.Context, configs []provider.Config) (online map[string]bool, offline map[string]string) {
	online, offline = map[string]bool{}, map[string]string{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()

			fail := func(reason string) {
				mu.Lock()
				offline[cfg.ID] = reason
				mu.Unlock()
			}

			p, err := v.buildProvider(cfg)
			if err != nil {
				fail(err.Error())
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, automationPingTimeout)
			defer cancel()
			if err := p.Ping(pingCtx); err != nil {
				fail(err.Error())
				return
			}
			mu.Lock()
			online[cfg.ID] = true
			mu.Unlock()
		}(cfg)
	}
	wg.Wait()
	return online, offline
}

// checkEntries counts, for each file, how many of its parts can actually be
// read right now.
//
// A part on an account that failed the probe, or on one that is no longer
// connected at all, is unreachable without asking: there is nothing to ask.
// Everything else is a metadata request per part — per sampled chunk per part
// for a chunked file, exactly as a single file's health check does, and for the
// same reason it samples.
func (v *Vault) checkEntries(ctx context.Context, entries []*Entry, online map[string]bool, byID map[string]provider.Config, seen map[string]bool) []fileState {
	states := make([]fileState, 0, len(entries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	window := make(chan struct{}, automationCheckWindow)

	for _, entry := range entries {
		if seen != nil {
			if seen[entry.ID] {
				continue
			}
			seen[entry.ID] = true
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		window <- struct{}{}
		go func(e *Entry) {
			defer wg.Done()
			defer func() { <-window }()

			state := fileState{entry: e, scheme: e.Scheme()}
			present := v.reachableParts(ctx, e, online, byID)
			state.reachable = len(present)
			state.absent = map[int]bool{}
			for _, s := range e.Shards {
				if !present[s.Part] {
					state.absent[s.Part] = true
				}
			}

			mu.Lock()
			states = append(states, state)
			mu.Unlock()
		}(entry)
	}
	wg.Wait()

	sort.Slice(states, func(i, j int) bool { return states[i].entry.Path() < states[j].entry.Path() })
	return states
}

// reachableParts is which distinct parts of one file are really there.
func (v *Vault) reachableParts(ctx context.Context, e *Entry, online map[string]bool, byID map[string]provider.Config) map[int]bool {
	sample := sampleChunks(e.Chunked(), e.ChunkCount)

	var mu sync.Mutex
	present := map[int]bool{}
	var wg sync.WaitGroup

	for _, shard := range e.Shards {
		cfg, connected := byID[shard.ProviderID]
		if !connected || !online[shard.ProviderID] {
			continue
		}

		wg.Add(1)
		go func(shard Shard, cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err != nil {
				return
			}
			// Every sampled chunk or none: one chunk missing its part makes the
			// part useless for that stretch of the file.
			for _, index := range sample {
				key := shard.Key
				if e.Chunked() {
					key = ChunkShardKey(e.ArchiveID, index, shard.Part)
				}
				if _, err := p.Stat(ctx, key); err != nil {
					return
				}
			}
			mu.Lock()
			present[shard.Part] = true
			mu.Unlock()
		}(shard, cfg)
	}
	wg.Wait()
	return present
}

// repairFiles rebuilds what it can of the files the check found wanting.
func (v *Vault) repairFiles(ctx context.Context, scope Scope, policy *Automation, needy []fileState, online map[string]bool, dark map[string]bool, run *AutomationRun) {
	answering := v.answeringAccounts(online)
	ceiling := policy.rebuildCeiling()
	cfg := policy.shards()
	res := run.Shards

	for _, st := range needy {
		if err := ctx.Err(); err != nil {
			run.Error = fmt.Sprintf("stopped part way: %v", err)
			return
		}
		// Not enough parts to gather the file from is not something a repair
		// can be attempted on: the rebuild would fail on the read.
		if st.reachable < st.scheme.Data {
			res.Deferred++
			continue
		}
		if cfg.MaxRepairs > 0 && res.Repaired+res.Failed >= cfg.MaxRepairs {
			res.Deferred++
			continue
		}
		if ceiling > 0 && st.entry.Size > ceiling {
			res.Deferred++
			run.Warnings = append(run.Warnings, fmt.Sprintf(
				"%s is %s, past this policy's %s rebuild ceiling — a repair rebuilds the whole "+
					"file, so this one is left for somebody to start by hand and watch",
				st.entry.Path(), formatSize(st.entry.Size), formatSize(ceiling)))
			continue
		}

		accounts, scheme, err := rebalanceTarget(st.entry, answering, cfg.Narrow)
		if err != nil {
			res.Deferred++
			run.Warnings = append(run.Warnings, fmt.Sprintf("%s: %v", st.entry.Path(), err))
			continue
		}

		report, err := v.relocate(ctx, scope, st.entry.ID, accounts, scheme,
			relocationOptions{
				unreachable: dark,
				absent:      map[string]map[int]bool{st.entry.ID: st.absent},
			}, nil, nil)
		if report != nil {
			run.Warnings = append(run.Warnings, report.Warnings...)
			res.PartsWritten += report.PartsMoved
			res.Bytes += report.Bytes
		}
		switch {
		case err != nil:
			res.Failed++
			run.Warnings = append(run.Warnings,
				fmt.Sprintf("%s could not be rebuilt: %v", st.entry.Path(), err))
			if errors.Is(err, ErrLocked) {
				run.Error = "the vault was locked before the sweep finished"
				return
			}
		case report.Relocated == 0:
			res.Failed++
		default:
			res.Repaired++
		}
	}
}

// answeringAccounts is the accounts that answered the probe, in the vault's own
// order so that a repair's choice is stable between runs.
func (v *Vault) answeringAccounts(online map[string]bool) []string {
	v.mu.RLock()
	defer v.mu.RUnlock()

	out := make([]string, 0, len(online))
	for _, cfg := range v.providers {
		if online[cfg.ID] {
			out = append(out, cfg.ID)
		}
	}
	return out
}

// rebalanceTarget decides where one file should be put back, and under which
// code.
//
// The rule is "keep the recovery model". A file goes back out cut exactly as it
// is cut now, over as many answering clouds as its code has shards, preferring
// the ones it is already on — same storage cost, same number of losses
// survived, same number of accounts an attacker would need together.
//
// When there are not that many clouds answering there is a real decision, and
// it is Narrow's. Left off, the file is not touched: a temporary outage must not
// be allowed to permanently narrow a file, because narrowing is not reversible
// without another full rebuild. Turned on, the code is re-cut to the width
// there is, holding n/k as close to what it was as the new width allows and
// never leaving the file without a spare part.
func rebalanceTarget(e *Entry, answering []string, narrow bool) ([]string, archive.Scheme, error) {
	have := e.Scheme()
	if err := have.Check(); err != nil {
		return nil, archive.Scheme{}, fmt.Errorf("its recorded code %s is not one SAND writes", have)
	}

	width := len(answering)
	if width > have.Total {
		width = have.Total
	}
	if width < archive.AccountsPerGroup {
		return nil, archive.Scheme{}, fmt.Errorf(
			"only %d cloud(s) are answering, and a file cannot be laid out over fewer than %d "+
				"without one of them holding enough to rebuild it on its own",
			len(answering), archive.AccountsPerGroup)
	}

	want := have
	if width < have.Total {
		if !narrow {
			return nil, archive.Scheme{}, fmt.Errorf(
				"it is cut %s and only %d cloud(s) are answering, so it cannot go back out as it "+
					"is — leaving it alone rather than narrowing it, which this policy has not "+
					"been told to do",
				have, width)
		}
		narrowed, err := narrowScheme(have, width)
		if err != nil {
			return nil, archive.Scheme{}, err
		}
		want = narrowed
	}

	// The clouds it is already on come first, so a repair moves a file as
	// little as it has to and a folder does not drift across every account in
	// the vault one outage at a time.
	answers := make(map[string]bool, len(answering))
	for _, id := range answering {
		answers[id] = true
	}
	chosen := make([]string, 0, width)
	taken := map[string]bool{}
	for _, s := range e.Shards {
		if len(chosen) == width {
			break
		}
		if answers[s.ProviderID] && !taken[s.ProviderID] {
			chosen = append(chosen, s.ProviderID)
			taken[s.ProviderID] = true
		}
	}
	for _, id := range answering {
		if len(chosen) == width {
			break
		}
		if !taken[id] {
			chosen = append(chosen, id)
			taken[id] = true
		}
	}
	return chosen, want, nil
}

// narrowScheme is the closest code to have that fits on width clouds.
//
// Closest by storage ratio: k is scaled with the width, so a 4-of-6 file — 1.5×
// — on four clouds becomes 3-of-4 rather than 2-of-4, which would have been
// 2× the storage and would have halved how many accounts an attacker needs
// together. It is never allowed to reach k = n: a file with no spare part at
// all is not a recovery model, it is a file waiting for the next outage.
func narrowScheme(have archive.Scheme, width int) (archive.Scheme, error) {
	if width < archive.AccountsPerGroup {
		return archive.Scheme{}, fmt.Errorf(
			"%d clouds is too few to cut a file over at all", width)
	}
	// Rounded to nearest rather than truncated, so 4-of-6 on five clouds is
	// 3-of-5 (1.67×) rather than 3-of-5's alternative reading.
	k := (have.Data*width*2 + have.Total) / (have.Total * 2)
	if k > width-1 {
		k = width - 1
	}
	if k < archive.MinData {
		k = archive.MinData
	}
	return archive.SchemeOf(k, width)
}

// finishAutomation stamps the run onto the policy and writes it down.
//
// A run always lands, even one that failed: the history is the record of what
// the folder has been through, and a fortnight of "no account answered" is
// exactly the thing somebody needs to be able to see.
func (v *Vault) finishAutomation(scope Scope, dir string, run *AutomationRun) (*AutomationRun, error) {
	run.FinishedAt = time.Now().UTC()

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		// Locked mid-sweep. There is nothing to write the result into and no
		// key to write it with; the run is still handed back to whoever asked.
		if run.Error == "" {
			run.Error = "the vault was locked before the sweep finished"
		}
		return run, nil
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return run, nil
	}
	auto := m.Automations[dir]
	if auto == nil {
		// Removed while it was running. Nothing to record it against.
		return run, nil
	}

	previousRun, previousHistory := auto.LastRunAt, auto.History
	auto.LastRunAt = run.FinishedAt
	history := append([]AutomationRun{run.stored()}, auto.History...)
	if len(history) > automationHistory {
		history = history[:automationHistory]
	}
	auto.History = history

	if err := v.persistLocked(); err != nil {
		auto.LastRunAt, auto.History = previousRun, previousHistory
		run.Warnings = append(run.Warnings,
			fmt.Sprintf("the result of this sweep could not be written down: %v", err))
	}
	return run, nil
}

// formatSize writes a byte count the way the reports do, since these strings
// are read by people rather than parsed.
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
