package vault

import (
	"sync"
	"time"
)

// DefaultBackupDelay is how long the manifest backup waits between pushes
// unless the vault says otherwise. See backupPacer.
const DefaultBackupDelay = 5 * time.Second

// MaxBackupDelay is the longest a vault may be told to hold the copies on the
// accounts stale. An hour is already a long time to be unrecoverable.
const MaxBackupDelay = time.Hour

// defaultBackupDelay is what a vault that has never chosen gets. It is
// DefaultBackupDelay everywhere but in this package's tests, which run
// unpaced unless a test is about the pacing (see TestMain).
var defaultBackupDelay = DefaultBackupDelay

// backupPacer decides *when* the manifest backup is pushed to the accounts,
// given a stream of requests saying that it should be.
//
// Every index change asks for a push, and an index change is cheap — a
// bulk delete, an import, a folder of photos arriving — so the requests come
// in bursts of hundreds. A push is a round trip to every account, and a
// provider that is asked to replace the same object a hundred times in a
// minute is a provider that will start answering 429. The pacer turns the
// burst into a few pushes:
//
//   - A request that arrives when nothing has been pushed for at least the
//     gap runs at once. The first change after a quiet spell is not delayed.
//   - A request that arrives sooner than that waits out the remainder of the
//     gap, and every request that arrives while it waits is folded into it.
//   - A request that arrives while a push is running is remembered, and runs
//     once the push has finished and the gap has passed again.
//
// So pushes start at least `gap` apart, measured from the end of one to the
// start of the next, and no change waits longer than one gap plus one push.
// A gap of zero is the old behaviour: a push the moment the index changes,
// with only the overlap collapsed.
//
// The pacer knows nothing about vaults. It is handed the push to run, and
// a clock and a timer factory so that its arithmetic can be tested without
// waiting for real seconds to pass. Its methods never block except Await,
// and none of them runs the push on the caller's goroutine, which is what
// lets the vault ask for a push while holding its own lock.
type backupPacer struct {
	mu   sync.Mutex
	idle sync.Cond

	// run is the push. It is called on a goroutine of the pacer's own, never
	// with mu held, and never twice at once.
	run func(force bool)

	// now and after are time.Now and time.AfterFunc, except in tests.
	now   func() time.Time
	after func(time.Duration, func()) backupTimer

	// gap is the least time between the end of one push and the start of the
	// next.
	gap time.Duration

	// last is when the most recent push finished. Zero until one has.
	last time.Time

	// pushes counts the pushes that have finished, for anyone who needs to
	// know that one has — tests, mostly.
	pushes uint64

	running bool // a push is on its goroutine
	pending bool // a request arrived while running or held, and is owed a push
	force   bool // some request since the last push asked for a forced one
	hold    bool // Hold is on: nothing starts until Release

	// timer is the armed wait for the gap to pass, or nil. armed is its
	// generation, so that a timer stopped too late to prevent its callback
	// is recognised as stale when the callback runs.
	timer backupTimer
	armed uint64
}

// backupTimer is the part of a *time.Timer the pacer uses, so a test can
// substitute one it fires by hand.
type backupTimer interface {
	Stop() bool
}

// newBackupPacer returns a pacer that runs `run` on the real clock, at least
// `gap` apart.
func newBackupPacer(gap time.Duration, run func(force bool)) *backupPacer {
	p := &backupPacer{
		run: run,
		now: time.Now,
		after: func(d time.Duration, f func()) backupTimer {
			return time.AfterFunc(d, f)
		},
		gap: gap,
	}
	p.idle.L = &p.mu
	return p
}

// Gap is the current least time between pushes.
func (p *backupPacer) Gap() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gap
}

// SetGap changes the least time between pushes. A push already waiting is
// moved to where the new gap puts it, which may be now.
func (p *backupPacer) SetGap(gap time.Duration) {
	if gap < 0 {
		gap = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gap = gap
	if p.timer != nil {
		p.disarmLocked()
		p.startOrArmLocked()
	}
}

// Request asks for a push. With force set the push overwrites copies it
// cannot open; the flag sticks until a push has consumed it.
func (p *backupPacer) Request(force bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.force = p.force || force
	if p.hold || p.running {
		p.pending = true
		return
	}
	if p.timer != nil {
		// Already waiting; this request rides along.
		return
	}
	p.startOrArmLocked()
}

// Hold stops pushes from starting until Release. Requests made meanwhile are
// remembered and reported by Release, not run; a push already waiting out
// the gap is cancelled and counted among them.
func (p *backupPacer) Hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold = true
	if p.timer != nil {
		p.disarmLocked()
		p.pending = true
		p.idle.Broadcast()
	}
}

// Release lifts the hold and reports whether anything was asked for while it
// was on. It does not run the push itself: the caller holding things has a
// forced push of its own to make over the finished state, and runs that.
func (p *backupPacer) Release() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold = false
	pending := p.pending
	p.pending = false
	return pending
}

// Await blocks until no push is running or waiting. A push waiting out the
// gap is started at once rather than waited for: the callers that need the
// copies settled — a command about to exit, a test — want them settled now,
// not in five seconds.
//
// Under a hold it waits only for a running push; what was asked for during
// the hold stays with Release.
func (p *backupPacer) Await() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.timer != nil {
			// Including one armed by the push that just finished, for the
			// requests that arrived while it ran.
			p.disarmLocked()
			p.startLocked()
		}
		if !p.running {
			return
		}
		p.idle.Wait()
	}
}

// startOrArmLocked starts a push if the gap has passed since the last one
// finished, and otherwise arms a timer for the remainder. mu must be held,
// nothing must be running, and no timer must be armed.
func (p *backupPacer) startOrArmLocked() {
	wait := p.gap
	if !p.last.IsZero() {
		wait = p.gap - p.now().Sub(p.last)
	} else {
		// Nothing has been pushed in this process's lifetime, so there is
		// nothing to space the first push from.
		wait = 0
	}
	if wait > p.gap {
		// A clock that went backwards must not stretch the wait past the
		// gap; the gap is the promise.
		wait = p.gap
	}
	if wait <= 0 {
		p.startLocked()
		return
	}
	p.armed++
	generation := p.armed
	p.timer = p.after(wait, func() { p.fire(generation) })
}

// fire is the armed timer's callback.
func (p *backupPacer) fire(generation uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer == nil || generation != p.armed {
		// Disarmed after this callback was already on its way.
		return
	}
	p.timer = nil
	if p.hold || p.running {
		// Hold disarms, so this is belt and braces — but a stale callback
		// must never start a second push beside a running one.
		p.pending = true
		p.idle.Broadcast()
		return
	}
	p.startLocked()
}

// disarmLocked cancels the armed timer, if any. mu must be held.
func (p *backupPacer) disarmLocked() {
	if p.timer == nil {
		return
	}
	p.timer.Stop()
	p.timer = nil
	// Bump the generation so a callback that beat Stop finds itself stale.
	p.armed++
}

// startLocked runs one push on its own goroutine. mu must be held and
// nothing must be running.
func (p *backupPacer) startLocked() {
	force := p.force
	p.force = false
	p.pending = false
	p.running = true
	go func() {
		p.run(force)
		p.finished()
	}()
}

// finished is the push goroutine's last act: note the time, and either rest
// or, if something was asked for in the meantime, line up the next one.
func (p *backupPacer) finished() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = p.now()
	p.pushes++
	p.running = false
	if p.pending && !p.hold {
		p.pending = false
		p.startOrArmLocked()
	}
	p.idle.Broadcast()
}
