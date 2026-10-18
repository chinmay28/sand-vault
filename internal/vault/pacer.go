package vault

import "time"

// A re-encryption that moves every file as fast as the clouds will take it is
// the right answer for a vault of a few gigabytes and the wrong one for a vault
// of a few terabytes: every account gets a day of saturated upload, the ones
// with a daily transfer cap lock the whole vault out of them, and the home
// connection the vault sits behind is unusable until it is over. So a password
// change can be told how much to move per day instead, and this is the meter.
//
// It is a token bucket counted in bytes. The level rises at Rate bytes per day,
// continuously, and never above the burst allowance; a file may start whenever
// the level is not negative, and the whole of the file is charged when it does.
// A file bigger than the burst therefore drives the level into debt, and
// nothing else starts until the debt has refilled — a 40 GB film at 10 GB a day
// is four quiet days — so the average over any stretch longer than the biggest
// file is exactly Rate, and the burst is as big as one file and no bigger.
//
// The burst is an hour's worth by default. A day's worth would let a vault that
// had been shut for a week move a week of budget the moment it opened, which is
// the saturated day this exists to prevent; an hour's worth lets the meter
// smooth over the gaps between ticks and nothing more.
//
// Pure: no clock of its own, no I/O, no lock. The time is passed in so that it
// can be tested without waiting, and so the vault can persist Level and At
// beside the plan they belong to and carry on across a restart.
type Pacer struct {
	// Rate is the budget, in bytes per day. Zero or less means unmetered:
	// Ready always answers yes and Spend changes nothing.
	Rate int64

	// Burst is the most that can accumulate while nothing is moving. Zero
	// means an hour's worth of Rate.
	Burst int64

	// Level is how much may start right now, and is negative while a file
	// bigger than the burst is being paid off. At is when Level was last
	// true; the zero time means the meter has never been read and starts
	// full.
	Level int64
	At    time.Time
}

const day = 24 * time.Hour

// Unmetered reports whether the pacer lets everything through.
func (p *Pacer) Unmetered() bool { return p == nil || p.Rate <= 0 }

// burst is the ceiling the level refills to.
func (p *Pacer) burst() int64 {
	if p.Burst > 0 {
		return p.Burst
	}
	if b := p.Rate / 24; b > 0 {
		return b
	}
	return 1
}

// refill brings Level up to date with the time that has passed. A clock that
// went backwards adds nothing and moves At, so the lost interval is not paid
// twice.
func (p *Pacer) refill(now time.Time) {
	if p.Unmetered() {
		return
	}
	ceiling := p.burst()
	if p.At.IsZero() {
		p.Level = ceiling
		p.At = now
		return
	}
	elapsed := now.Sub(p.At)
	if elapsed <= 0 {
		p.At = now
		return
	}
	// Float rather than integer arithmetic: a terabyte a day over a week is
	// past what int64 holds as bytes × nanoseconds.
	earned := int64(float64(p.Rate) * elapsed.Seconds() / day.Seconds())
	if p.Level+earned >= ceiling {
		p.Level = ceiling
		p.At = now
		return
	}
	p.Level += earned
	// Only the time that bought whole bytes is spent. The fraction of a byte
	// left over stays in the clock rather than being thrown away, so a meter
	// read every few seconds earns exactly what one read once a day does.
	p.At = p.At.Add(p.timeToEarn(earned))
}

// Ready reports whether a file may start now, and if not, how long until one
// could.
func (p *Pacer) Ready(now time.Time) (bool, time.Duration) {
	if p.Unmetered() {
		return true, 0
	}
	p.refill(now)
	if p.Level >= 0 {
		return true, 0
	}
	return false, p.timeToEarn(-p.Level)
}

// Spend charges a file that has started. The level may go negative, and that
// is the design: the debt is what spaces the next file out.
func (p *Pacer) Spend(now time.Time, n int64) {
	if p.Unmetered() || n <= 0 {
		return
	}
	p.refill(now)
	p.Level -= n
}

// Refund gives back a charge for a file that did not move after all, so a
// cloud that would not answer costs no budget. Capped at the burst like any
// other refill.
func (p *Pacer) Refund(now time.Time, n int64) {
	if p.Unmetered() || n <= 0 {
		return
	}
	p.refill(now)
	if p.Level+n > p.burst() {
		p.Level = p.burst()
	} else {
		p.Level += n
	}
}

// ETA says how long it will take to move remaining bytes at the current rate,
// given what is already in the bucket. Zero when unmetered, or when nothing is
// left.
func (p *Pacer) ETA(now time.Time, remaining int64) time.Duration {
	if p.Unmetered() || remaining <= 0 {
		return 0
	}
	p.refill(now)
	owed := remaining - p.Level
	if owed <= 0 {
		return 0
	}
	return p.timeToEarn(owed)
}

// timeToEarn is how long the meter takes to accumulate n bytes, rounded up
// to a whole millisecond so that a wait, once over, has always earned what it
// was waiting for.
func (p *Pacer) timeToEarn(n int64) time.Duration {
	if n <= 0 {
		return 0
	}
	seconds := float64(n) / float64(p.Rate) * day.Seconds()
	d := time.Duration(seconds * float64(time.Second))
	if rounded := d.Truncate(time.Millisecond); rounded < d || d == 0 {
		d = rounded + time.Millisecond
	}
	return d
}
