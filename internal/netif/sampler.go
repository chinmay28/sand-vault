package netif

import (
	"math"
	"net/netip"
	"sync"
	"time"
)

// RateWindow is the widest gap between two readings that a send rate is
// worked out across. Wider than that, the rate would say how things were
// rather than how they are, so the reading only starts the next one.
const RateWindow = 30 * time.Second

// Sample is one reading of a link, and how fast it was sending.
type Sample struct {
	Link

	// At is when the link was read.
	At time.Time

	// SendBitsPerSecond is how fast the interface sent between the reading
	// before this one and this one — everything it sent, not one
	// connection's share. Rated is false when there is nothing to work that
	// out from: no reading before this one, or one more than RateWindow
	// earlier, a count that went backwards, a different interface, or a
	// system that keeps no count.
	SendBitsPerSecond int64
	Rated             bool
}

// A Sampler reads links when asked, but no more often than once an interval
// for each address, and works out how fast an interface is sending from one
// reading of it to the next. Asked more often than that, it answers with the
// last reading. It is safe for concurrent use.
type Sampler struct {
	read  func(netip.Addr) (Link, error)
	every time.Duration

	mu   sync.Mutex
	last map[netip.Addr]Sample
}

// NewSampler returns a Sampler that reads links with read — ForAddress, or a
// stand-in for it — at most once every interval for each address.
func NewSampler(read func(netip.Addr) (Link, error), every time.Duration) *Sampler {
	return &Sampler{read: read, every: every, last: map[netip.Addr]Sample{}}
}

// Sample reports the link of the interface holding addr as it is at now, or
// as it was at the last reading if that is less than the interval old. A
// failed reading is returned as its error and leaves the last good one in
// place, to be rated against.
func (s *Sampler) Sample(addr netip.Addr, now time.Time) (Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, seen := s.last[addr]
	if seen {
		if age := now.Sub(prev.At); age >= 0 && age < s.every {
			return prev, nil
		}
	}

	link, err := s.read(addr)
	if err != nil {
		return Sample{}, err
	}
	cur := Sample{Link: link, At: now}
	if seen {
		cur.SendBitsPerSecond, cur.Rated = sendRate(prev, cur)
	}
	s.last[addr] = cur
	return cur, nil
}

// sendRate is how fast an interface sent between two readings of it.
func sendRate(prev, cur Sample) (int64, bool) {
	gap := cur.At.Sub(prev.At)
	switch {
	case !prev.Counted || !cur.Counted:
		return 0, false
	case prev.Name != cur.Name:
		// The address moved to another interface: two counts of two things.
		return 0, false
	case cur.SentBytes < prev.SentBytes:
		// The count started again, as it does when an interface goes down
		// and comes back.
		return 0, false
	case gap <= 0 || gap > RateWindow:
		return 0, false
	}
	bits := float64(cur.SentBytes-prev.SentBytes) * 8
	return int64(math.Round(bits / gap.Seconds())), true
}
