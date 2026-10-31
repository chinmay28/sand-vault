package vault

import (
	"sync"
	"testing"
	"time"
)

// pacerRig is a backupPacer on a clock the test turns by hand, with timers the
// test fires by hand, and a push the test holds open for as long as it likes.
// Nothing in here waits on real time except the few places a goroutine has to
// be given a moment to reach a state, and those are bounded.
type pacerRig struct {
	t *testing.T
	p *backupPacer

	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer

	// started receives the force flag of every push as it begins; release is
	// what a push waits on before it returns.
	started chan bool
	release chan struct{}
}

// fakeTimer is a timer that fires only when the test says so. Its state is
// guarded by the rig's mutex, since the pacer stops timers from the push
// goroutine while the test reads them.
type fakeTimer struct {
	r       *pacerRig
	d       time.Duration
	f       func()
	stopped bool
}

func (ft *fakeTimer) Stop() bool {
	ft.r.mu.Lock()
	defer ft.r.mu.Unlock()
	was := ft.stopped
	ft.stopped = true
	return !was
}

func newPacerRig(t *testing.T, gap time.Duration) *pacerRig {
	t.Helper()
	r := &pacerRig{
		t:       t,
		now:     time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		started: make(chan bool, 16),
		release: make(chan struct{}),
	}
	r.p = newBackupPacer(gap, func(force bool) {
		r.started <- force
		<-r.release
	})
	r.p.now = func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	}
	r.p.after = func(d time.Duration, f func()) backupTimer {
		r.mu.Lock()
		defer r.mu.Unlock()
		ft := &fakeTimer{r: r, d: d, f: f}
		r.timers = append(r.timers, ft)
		return ft
	}
	return r
}

func (r *pacerRig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

// expectStart waits for a push to begin and returns whether it was forced.
func (r *pacerRig) expectStart() bool {
	r.t.Helper()
	select {
	case force := <-r.started:
		return force
	case <-time.After(2 * time.Second):
		r.t.Fatal("no push started")
		return false
	}
}

// expectNoStart gives a push that should not happen a moment to happen.
func (r *pacerRig) expectNoStart() {
	r.t.Helper()
	select {
	case <-r.started:
		r.t.Fatal("a push started that should not have")
	case <-time.After(30 * time.Millisecond):
	}
}

// finish lets the running push return and waits for the pacer to have taken
// note of it. It watches the count of finished pushes rather than the running
// flag, because with no gap the next push starts before anyone could see the
// flag drop.
func (r *pacerRig) finish() {
	r.t.Helper()
	r.p.mu.Lock()
	before := r.p.pushes
	r.p.mu.Unlock()
	r.release <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.p.mu.Lock()
		done := r.p.pushes > before
		r.p.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	r.t.Fatal("the push never finished")
}

// armed returns the live timers: created and neither stopped nor fired.
func (r *pacerRig) armed() []*fakeTimer {
	r.mu.Lock()
	defer r.mu.Unlock()
	var live []*fakeTimer
	for _, ft := range r.timers {
		if !ft.stopped {
			live = append(live, ft)
		}
	}
	return live
}

// fire runs the one live timer, as the clock would.
func (r *pacerRig) fire() {
	r.t.Helper()
	live := r.armed()
	if len(live) != 1 {
		r.t.Fatalf("%d timers armed, want exactly one to fire", len(live))
	}
	live[0].Stop()
	r.advance(live[0].d)
	live[0].f()
}

func TestPacerRunsTheFirstRequestAtOnce(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Request(false)
	if r.expectStart() {
		t.Error("an unforced request ran a forced push")
	}
	if len(r.armed()) != 0 {
		t.Error("a push that ran at once also armed a timer")
	}
	r.finish()
}

func TestPacerHoldsABurstToOnePushPerGap(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Request(false)
	r.expectStart()
	r.finish()

	// Three changes in the two seconds after the push. One timer, for the
	// three seconds left of the gap, and nothing runs until it fires.
	r.advance(2 * time.Second)
	r.p.Request(false)
	r.p.Request(false)
	r.p.Request(false)
	r.expectNoStart()
	live := r.armed()
	if len(live) != 1 {
		t.Fatalf("%d timers armed for a burst, want one", len(live))
	}
	if live[0].d != 3*time.Second {
		t.Errorf("the timer waits %s, want the 3s left of the gap", live[0].d)
	}

	r.fire()
	r.expectStart()
	r.finish()
	r.expectNoStart()
}

func TestPacerQueuesARequestMadeDuringAPush(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Request(false)
	r.expectStart()

	// Changes while the push is out. No second push beside the first.
	r.p.Request(false)
	r.p.Request(false)
	r.expectNoStart()
	if len(r.armed()) != 0 {
		t.Error("a request during a push armed a timer instead of waiting for it")
	}

	// When it lands, the next one is spaced a whole gap from its end.
	r.finish()
	r.expectNoStart()
	live := r.armed()
	if len(live) != 1 || live[0].d != 5*time.Second {
		t.Fatalf("after a push with changes behind it: timers %+v, want one for the full gap", live)
	}
	r.fire()
	r.expectStart()
	r.finish()
}

func TestPacerWithNoGapRunsBackToBack(t *testing.T) {
	r := newPacerRig(t, 0)
	r.p.Request(false)
	r.expectStart()
	r.p.Request(false)
	r.finish()
	// The queued change runs the moment the first push is done, no timer.
	r.expectStart()
	if len(r.armed()) != 0 {
		t.Error("a pacer with no gap armed a timer")
	}
	r.finish()

	r.p.Request(false)
	r.expectStart()
	r.finish()
}

func TestPacerForceSticksUntilAPushCarriesIt(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Request(false)
	r.expectStart()
	// Asked for while a push is out: the *next* push is the forced one.
	r.p.Request(true)
	r.finish()
	r.fire()
	if !r.expectStart() {
		t.Error("a forced request made during a push was not carried by the next one")
	}
	r.finish()

	// And across a wait: a plain request arms the timer, a forced one rides
	// along, and the push that fires is forced. Then the flag is spent.
	r.advance(time.Second)
	r.p.Request(false)
	r.p.Request(true)
	r.fire()
	if !r.expectStart() {
		t.Error("a forced request that joined a waiting push was not carried by it")
	}
	r.finish()
	r.advance(10 * time.Second)
	r.p.Request(false)
	if r.expectStart() {
		t.Error("the force flag outlived the push that carried it")
	}
	r.finish()
}

func TestPacerHoldKeepsRequestsForRelease(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Hold()
	r.p.Request(false)
	r.p.Request(false)
	r.expectNoStart()
	if len(r.armed()) != 0 {
		t.Error("a request under a hold armed a timer")
	}
	if !r.p.Release() {
		t.Error("Release did not report the requests made under the hold")
	}
	// Release runs nothing itself; the caller makes its own push.
	r.expectNoStart()
	if r.p.Release() {
		t.Error("a second Release reported requests that were already handed over")
	}
}

func TestPacerHoldCancelsAWaitingPush(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Request(false)
	r.expectStart()
	r.finish()
	r.p.Request(false)
	if len(r.armed()) != 1 {
		t.Fatal("the second request did not arm a timer")
	}

	r.p.Hold()
	if len(r.armed()) != 0 {
		t.Error("Hold left the waiting push armed")
	}
	if !r.p.Release() {
		t.Error("the push Hold cancelled was not reported by Release")
	}
}

func TestPacerAwaitStartsAWaitingPushNow(t *testing.T) {
	r := newPacerRig(t, time.Hour)
	r.p.Request(false)
	r.expectStart()
	r.finish()
	r.p.Request(false)
	if len(r.armed()) != 1 {
		t.Fatal("the second request did not arm a timer")
	}

	done := make(chan struct{})
	go func() {
		r.p.Await()
		close(done)
	}()
	// Await does not sit out the hour: it starts the push and waits for it.
	r.expectStart()
	select {
	case <-done:
		t.Fatal("Await returned while the push it started was still running")
	case <-time.After(20 * time.Millisecond):
	}
	r.finish()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return once the push finished")
	}
	if len(r.armed()) != 0 {
		t.Error("the timer Await pre-empted is still armed")
	}
}

func TestPacerAwaitDrainsWhatArrivesDuringThePush(t *testing.T) {
	r := newPacerRig(t, time.Hour)
	r.p.Request(false)
	r.expectStart()

	done := make(chan struct{})
	go func() {
		r.p.Await()
		close(done)
	}()
	// A change lands while Await is waiting on the first push. The second
	// push it earns is started at once rather than an hour later.
	r.p.Request(false)
	r.finish()
	r.expectStart()
	r.finish()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return once the copies were settled")
	}
}

func TestPacerAwaitWithNothingToDoReturns(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	done := make(chan struct{})
	go func() {
		r.p.Await()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Await blocked with nothing running or waiting")
	}
}

func TestPacerSetGapMovesAWaitingPush(t *testing.T) {
	r := newPacerRig(t, 10*time.Second)
	r.p.Request(false)
	r.expectStart()
	r.finish()
	r.advance(2 * time.Second)
	r.p.Request(false)
	live := r.armed()
	if len(live) != 1 || live[0].d != 8*time.Second {
		t.Fatalf("timers %+v, want one for the 8s left", live)
	}

	// Lengthening re-arms for the new remainder.
	r.p.SetGap(30 * time.Second)
	live = r.armed()
	if len(live) != 1 || live[0].d != 28*time.Second {
		t.Fatalf("after lengthening the gap: timers %+v, want one for 28s", live)
	}
	r.expectNoStart()

	// Shortening to less than has already passed runs it now.
	r.p.SetGap(time.Second)
	if len(r.armed()) != 0 {
		t.Error("shortening the gap below the time already passed left the timer armed")
	}
	r.expectStart()
	r.finish()
	if r.p.Gap() != time.Second {
		t.Errorf("Gap reads %s, want 1s", r.p.Gap())
	}
}

func TestPacerIgnoresATimerThatFiresAfterBeingDisarmed(t *testing.T) {
	r := newPacerRig(t, 5*time.Second)
	r.p.Request(false)
	r.expectStart()
	r.finish()
	r.p.Request(false)
	stale := r.armed()[0]

	// Await pre-empts the timer and runs the push. A real timer whose
	// callback was already on its way when Stop was called still runs it;
	// that must not start a second push.
	go r.p.Await()
	r.expectStart()
	stale.f()
	r.expectNoStart()
	r.finish()
	r.expectNoStart()
}
