package server

import (
	"testing"
	"time"
)

// The store's contract, apart from HTTP: a session is live until its idle
// deadline passes with nothing running under it, and every use moves the
// deadline on.

func TestSessionValidateExtendsAndRejectsStrangers(t *testing.T) {
	store := newSessionStore(200 * time.Millisecond)
	token, err := store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if store.validate("") {
		t.Error("an empty token validated")
	}
	if store.validate("not-" + token) {
		t.Error("a token never issued validated")
	}

	// Used every 50ms, a 200ms session never lapses.
	for i := 0; i < 4; i++ {
		time.Sleep(50 * time.Millisecond)
		if !store.validate(token) {
			t.Fatalf("a session in steady use lapsed on use %d", i)
		}
	}

	time.Sleep(400 * time.Millisecond)
	if store.validate(token) {
		t.Error("a session left alone past its deadline still validated")
	}
	if store.sweep() != 0 {
		t.Error("a lapsed session still counted as live")
	}
}

func TestSessionPeekDoesNotCountAsUse(t *testing.T) {
	store := newSessionStore(100 * time.Millisecond)
	token, err := store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if store.peek("") || store.peek("other") {
		t.Error("peek accepted a token that was never issued")
	}

	// Peeked at every 20ms, a 100ms session lapses all the same: a poll is
	// not somebody using the vault.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		store.peek(token)
		time.Sleep(20 * time.Millisecond)
	}
	if store.peek(token) {
		t.Error("peeking kept a session alive past its deadline")
	}
	if store.validate(token) {
		t.Error("a session that lapsed under peeks came back on validate")
	}
}

func TestSessionAcquireHoldsItPastTheDeadline(t *testing.T) {
	store := newSessionStore(100 * time.Millisecond)
	token, err := store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, ok := store.acquire(""); ok {
		t.Error("an empty token was acquired")
	}
	release, ok := store.acquire(token)
	if !ok {
		t.Fatal("a fresh session could not be acquired")
	}

	// Well past the deadline, with the request still running.
	time.Sleep(300 * time.Millisecond)
	if store.sweep() != 1 {
		t.Error("a session with a request in flight was swept")
	}
	if !store.peek(token) {
		t.Error("a session with a request in flight did not validate")
	}

	// The request ends, and the deadline runs from now — a full timeout of
	// grace, not none.
	release()
	time.Sleep(30 * time.Millisecond)
	if !store.validate(token) {
		t.Error("a session lapsed straight after its request returned")
	}

	// And with nothing running, it lapses as any session does.
	time.Sleep(250 * time.Millisecond)
	if store.sweep() != 0 {
		t.Error("a released session outlived its deadline")
	}
}

func TestSessionReleaseIsSafeTwiceAndAfterALock(t *testing.T) {
	store := newSessionStore(time.Minute)
	token, err := store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	release, ok := store.acquire(token)
	if !ok {
		t.Fatal("acquire")
	}
	// An explicit lock while the request runs: the request finishing must
	// not bring the session back, and finishing twice must not take the
	// count below zero for a session that is reissued.
	store.clear()
	release()
	release()
	if store.peek(token) {
		t.Error("a cleared session came back when its request returned")
	}
	if store.sweep() != 0 {
		t.Error("a cleared store counts sessions")
	}

	// The same for revoke, which is one tab locking while another uploads.
	token, err = store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	release, _ = store.acquire(token)
	if store.revoke(token) {
		t.Error("revoking the only session reports others remaining")
	}
	release()
	if store.peek(token) {
		t.Error("a revoked session came back when its request returned")
	}
}

func TestSessionTwoRequestsInFlightReleaseIndependently(t *testing.T) {
	store := newSessionStore(100 * time.Millisecond)
	token, err := store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	first, _ := store.acquire(token)
	second, _ := store.acquire(token)

	first()
	time.Sleep(250 * time.Millisecond)
	if store.sweep() != 1 {
		t.Error("the second request in flight did not hold the session")
	}
	second()
	time.Sleep(250 * time.Millisecond)
	if store.sweep() != 0 {
		t.Error("a session with both requests returned outlived its deadline")
	}
}
