package tvaccess

import "time"

// Throttle bounds how many wrong codes are tried in a window.
//
// Two limits, because each answers a different attacker. PerClient stops one
// device hammering away; Global stops one person with many addresses, which on
// a home network is anybody who can change their IP. The global limit is also
// what sets the odds: at its defaults, thirty guesses per ten minutes is under
// four and a half thousand a day against a space of a million codes — a chance
// of well under one in two hundred of finding a given code in the day it is
// live, and that only for somebody who already has a foot on the network and
// spends the whole day trying.
//
// The cost of the global limit is that a guesser can keep everybody out while
// they guess. That is the right way round: a TV that cannot pair for ten
// minutes is a nuisance, a folder that can be opened by brute force is not.
type Throttle struct {
	PerClient int
	Global    int
	Window    time.Duration
}

// DefaultThrottle is what a zero Throttle means.
var DefaultThrottle = Throttle{PerClient: 5, Global: 30, Window: 10 * time.Minute}

// throttle keeps the times of recent failures.
//
// A log of times rather than a counter that resets, so the limit is a sliding
// window: a guesser cannot spend the whole allowance in the last second of one
// window and again in the first second of the next.
type throttle struct {
	limits  Throttle
	global  []time.Time
	clients map[string][]time.Time
}

func newThrottle(limits Throttle) *throttle {
	if limits.PerClient <= 0 {
		limits.PerClient = DefaultThrottle.PerClient
	}
	if limits.Global <= 0 {
		limits.Global = DefaultThrottle.Global
	}
	if limits.Window <= 0 {
		limits.Window = DefaultThrottle.Window
	}
	return &throttle{limits: limits, clients: map[string][]time.Time{}}
}

// allow reports whether a client may try a code now, and when it is refused,
// how long until the oldest failure counting against it falls out of the
// window.
func (t *throttle) allow(client string, now time.Time) (time.Duration, bool) {
	t.prune(now)
	if len(t.global) >= t.limits.Global {
		return t.global[0].Add(t.limits.Window).Sub(now), false
	}
	if seen := t.clients[client]; len(seen) >= t.limits.PerClient {
		return seen[0].Add(t.limits.Window).Sub(now), false
	}
	return 0, true
}

// fail records a wrong code.
func (t *throttle) fail(client string, now time.Time) {
	t.global = append(t.global, now)
	t.clients[client] = append(t.clients[client], now)
}

// prune drops failures older than the window, and clients left with none, so
// the map does not grow with every address that ever mistyped.
func (t *throttle) prune(now time.Time) {
	cutoff := now.Add(-t.limits.Window)
	t.global = dropBefore(t.global, cutoff)
	for client, seen := range t.clients {
		seen = dropBefore(seen, cutoff)
		if len(seen) == 0 {
			delete(t.clients, client)
			continue
		}
		t.clients[client] = seen
	}
}

// dropBefore removes the leading times not after cutoff. The log is in the
// order failures happened, so they are always a prefix.
func dropBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return times
	}
	return append(times[:0:0], times[i:]...)
}
