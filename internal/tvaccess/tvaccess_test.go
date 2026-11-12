package tvaccess

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// clock is a hand-wound time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)}
	return New(Options{Now: c.now}), c
}

func issue(t *testing.T, s *Store, dir string, ttl time.Duration) Grant {
	t.Helper()
	g, err := s.Issue(Request{Dir: dir, Label: "Films", TTL: ttl})
	if err != nil {
		t.Fatalf("Issue(%s): %v", dir, err)
	}
	return g
}

func TestIssuedCodesAreFourCharactersFromTheAlphabet(t *testing.T) {
	s, _ := newStore(t)
	for i := 0; i < MaxGrants; i++ {
		g := issue(t, s, "/films", time.Hour)
		if len(g.Code) != CodeLength {
			t.Fatalf("code %q is %d characters, want %d", g.Code, len(g.Code), CodeLength)
		}
		for _, r := range g.Code {
			if !strings.ContainsRune(Alphabet, r) {
				t.Fatalf("code %q has %q, which is not in the alphabet", g.Code, r)
			}
		}
	}
}

func TestTheAlphabetLeavesOutLookalikes(t *testing.T) {
	for _, r := range "0O1I" {
		if strings.ContainsRune(Alphabet, r) {
			t.Errorf("alphabet contains %q, which reads as another character", r)
		}
	}
	// 256 is a multiple of the alphabet's length, so byte % len is unbiased.
	if 256%len(Alphabet) != 0 {
		t.Errorf("alphabet has %d characters; drawing from it by byte would be biased", len(Alphabet))
	}
}

func TestIssueRecordsTheGrant(t *testing.T) {
	s, c := newStore(t)
	g, err := s.Issue(Request{Scope: "sub1", Dir: "/films", Label: "  Films ", TTL: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if g.Scope != "sub1" || g.Dir != "/films" || g.Label != "Films" {
		t.Errorf("grant = %+v", g)
	}
	if !g.CreatedAt.Equal(c.t) || !g.ExpiresAt.Equal(c.t.Add(2*time.Hour)) {
		t.Errorf("created %v expires %v", g.CreatedAt, g.ExpiresAt)
	}
	if got := s.Grants(); len(got) != 1 || got[0].Code != g.Code {
		t.Errorf("Grants() = %+v", got)
	}
}

func TestIssueRefusesALifetimeOutOfBounds(t *testing.T) {
	s, _ := newStore(t)
	for _, ttl := range []time.Duration{0, MinTTL - time.Second, MaxTTL + time.Second, -time.Hour} {
		if _, err := s.Issue(Request{Dir: "/", TTL: ttl}); !errors.Is(err, ErrBadTTL) {
			t.Errorf("TTL %s: err = %v, want ErrBadTTL", ttl, err)
		}
	}
	for _, ttl := range []time.Duration{MinTTL, MaxTTL} {
		if _, err := s.Issue(Request{Dir: "/", TTL: ttl}); err != nil {
			t.Errorf("TTL %s at the bound: %v", ttl, err)
		}
	}
}

func TestIssueCapsLiveGrants(t *testing.T) {
	s, c := newStore(t)
	for i := 0; i < MaxGrants; i++ {
		issue(t, s, "/", time.Hour)
	}
	if _, err := s.Issue(Request{Dir: "/", TTL: time.Hour}); !errors.Is(err, ErrTooManyGrants) {
		t.Fatalf("err = %v, want ErrTooManyGrants", err)
	}
	// Expired grants stop counting.
	c.advance(time.Hour)
	if _, err := s.Issue(Request{Dir: "/", TTL: time.Hour}); err != nil {
		t.Fatalf("after the others expired: %v", err)
	}
}

func TestIssueNeverHandsOutALiveCode(t *testing.T) {
	// A random source that yields the same four bytes twice, then different
	// ones: the second issue has to draw again rather than reuse the code.
	src := bytes.NewReader([]byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4})
	s := New(Options{Random: src})
	a, err := s.Issue(Request{Dir: "/", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Issue(Request{Dir: "/", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if a.Code == b.Code {
		t.Fatalf("two live grants share code %q", a.Code)
	}
	if a.Code != "AAAA" || b.Code != "BCDE" {
		t.Errorf("codes = %q, %q", a.Code, b.Code)
	}
}

func TestIssueFailsOnABrokenRandomSource(t *testing.T) {
	s := New(Options{Random: bytes.NewReader(nil)})
	if _, err := s.Issue(Request{Dir: "/", TTL: time.Hour}); err == nil {
		t.Fatal("issued a code with no randomness to draw it from")
	}
}

func TestRedeemPairsADevice(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)

	dev, err := s.Redeem("10.0.0.5", strings.ToLower(g.Code[:2])+"-"+g.Code[2:])
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if len(dev.Token) < 40 {
		t.Errorf("token %q is too short to be a bearer secret", dev.Token)
	}
	if dev.Grant.Code != g.Code || dev.Grant.Devices != 1 {
		t.Errorf("device grant = %+v", dev.Grant)
	}

	got, ok := s.Authenticate(dev.Token)
	if !ok || got.Dir != "/films" || got.Code != g.Code {
		t.Fatalf("Authenticate = %+v, %v", got, ok)
	}
	if _, ok := s.Authenticate(dev.Token + "x"); ok {
		t.Error("a token that was never minted authenticated")
	}
	if _, ok := s.Authenticate(""); ok {
		t.Error("an empty token authenticated")
	}
}

func TestRedeemRefusesAnUnknownCode(t *testing.T) {
	s, _ := newStore(t)
	issue(t, s, "/films", time.Hour)
	if _, err := s.Redeem("c", "ZZZZZ"); !errors.Is(err, ErrBadCode) {
		t.Errorf("err = %v, want ErrBadCode", err)
	}
}

func TestADeviceLastsAsLongAsItsCode(t *testing.T) {
	s, c := newStore(t)
	g := issue(t, s, "/films", time.Hour)
	dev, err := s.Redeem("c", g.Code)
	if err != nil {
		t.Fatal(err)
	}

	c.advance(time.Hour - time.Second)
	if _, ok := s.Authenticate(dev.Token); !ok {
		t.Fatal("device dropped before its code expired")
	}
	if !s.Live(g.Code) {
		t.Fatal("code not live before it expired")
	}
	c.advance(time.Second)
	if _, ok := s.Authenticate(dev.Token); ok {
		t.Error("device still authenticates after its code expired")
	}
	if s.Live(g.Code) {
		t.Error("code still live after it expired")
	}
	if _, err := s.Redeem("c", g.Code); !errors.Is(err, ErrBadCode) {
		t.Errorf("redeeming an expired code: %v, want ErrBadCode", err)
	}
	if len(s.Grants()) != 0 {
		t.Error("expired grant still listed")
	}
}

func TestRevokeUnpairsEveryDevice(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)
	other := issue(t, s, "/shows", time.Hour)

	a, _ := s.Redeem("a", g.Code)
	b, _ := s.Redeem("b", g.Code)
	keep, _ := s.Redeem("c", other.Code)

	if !s.Revoke(strings.ToLower(g.Code)) {
		t.Fatal("Revoke reported the code was not live")
	}
	for _, tok := range []string{a.Token, b.Token} {
		if _, ok := s.Authenticate(tok); ok {
			t.Error("a device under a revoked code still authenticates")
		}
	}
	if _, ok := s.Authenticate(keep.Token); !ok {
		t.Error("revoking one code unpaired a device under another")
	}
	if s.Revoke(g.Code) {
		t.Error("revoking twice reported success twice")
	}
}

func TestForgetUnpairsOneDevice(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)
	a, _ := s.Redeem("a", g.Code)
	b, _ := s.Redeem("b", g.Code)

	if !s.Forget(a.Token) {
		t.Fatal("Forget reported the token was not paired")
	}
	if _, ok := s.Authenticate(a.Token); ok {
		t.Error("forgotten device still authenticates")
	}
	got, ok := s.Authenticate(b.Token)
	if !ok || got.Devices != 1 {
		t.Errorf("the other device: %+v, %v", got, ok)
	}
	if s.Forget(a.Token) {
		t.Error("forgetting twice reported success twice")
	}
}

func TestACodePairsAtMostSoManyDevices(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)
	for i := 0; i < MaxDevicesPerGrant; i++ {
		if _, err := s.Redeem(fmt.Sprint(i), g.Code); err != nil {
			t.Fatalf("device %d: %v", i, err)
		}
	}
	if _, err := s.Redeem("one-more", g.Code); !errors.Is(err, ErrTooManyDevices) {
		t.Errorf("err = %v, want ErrTooManyDevices", err)
	}
}

func TestWrongCodesAreThrottledPerClient(t *testing.T) {
	s, c := newStore(t)
	g := issue(t, s, "/films", 24*time.Hour)

	for i := 0; i < DefaultThrottle.PerClient; i++ {
		if _, err := s.Redeem("guesser", "ZZZZ"); !errors.Is(err, ErrBadCode) {
			t.Fatalf("guess %d: %v", i, err)
		}
		c.advance(time.Second)
	}

	// Refused now even with the right code: a throttle that let a correct
	// guess through would not be stopping anybody.
	_, err := s.Redeem("guesser", g.Code)
	var throttled *ThrottledError
	if !errors.As(err, &throttled) || !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want ThrottledError", err)
	}
	want := DefaultThrottle.Window - time.Duration(DefaultThrottle.PerClient)*time.Second
	if throttled.RetryAfter != want {
		t.Errorf("RetryAfter = %s, want %s", throttled.RetryAfter, want)
	}

	// Somebody else on the network is unaffected.
	if _, err := s.Redeem("living-room", g.Code); err != nil {
		t.Errorf("another client was throttled too: %v", err)
	}

	// And the guesser is let back in once the window slides past.
	c.advance(throttled.RetryAfter)
	if _, err := s.Redeem("guesser", g.Code); err != nil {
		t.Errorf("after the window: %v", err)
	}
}

func TestWrongCodesAreThrottledAcrossClients(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)

	for i := 0; i < DefaultThrottle.Global; i++ {
		s.Redeem(fmt.Sprintf("10.0.0.%d", i), "ZZZZ")
	}
	if _, err := s.Redeem("fresh-address", g.Code); !errors.Is(err, ErrThrottled) {
		t.Errorf("err = %v, want ErrThrottled once the global allowance is spent", err)
	}
}

func TestARightCodeDoesNotCountAsAFailure(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)
	for i := 0; i < MaxDevicesPerGrant; i++ {
		if _, err := s.Redeem("tv", g.Code); err != nil {
			t.Fatalf("pairing %d: %v", i, err)
		}
	}
}

func TestGrantsAreListedNewestFirst(t *testing.T) {
	s, c := newStore(t)
	first := issue(t, s, "/a", time.Hour)
	c.advance(time.Minute)
	second := issue(t, s, "/b", time.Hour)

	got := s.Grants()
	if len(got) != 2 || got[0].Code != second.Code || got[1].Code != first.Code {
		t.Errorf("Grants() = %+v", got)
	}
}

func TestClearEndsEverything(t *testing.T) {
	s, _ := newStore(t)
	g := issue(t, s, "/films", time.Hour)
	dev, _ := s.Redeem("tv", g.Code)
	s.Clear()
	if _, ok := s.Authenticate(dev.Token); ok || s.Live(g.Code) || len(s.Grants()) != 0 {
		t.Error("something survived Clear")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"abcd":    "ABCD",
		" ab-cd ": "ABCD",
		"A B C D": "ABCD",
		"":        "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThrottlePrunesQuietClients(t *testing.T) {
	th := newThrottle(Throttle{})
	now := time.Now()
	th.fail("a", now)
	th.fail("b", now)
	th.allow("c", now.Add(DefaultThrottle.Window+time.Second))
	if len(th.clients) != 0 || len(th.global) != 0 {
		t.Errorf("stale failures kept: %d clients, %d global", len(th.clients), len(th.global))
	}
}
