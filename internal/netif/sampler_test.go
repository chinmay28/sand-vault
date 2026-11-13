package netif

import (
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeLinks stands in for ForAddress: an interface whose count the test moves
// along, and a tally of how often it was read.
type fakeLinks struct {
	mu    sync.Mutex
	link  Link
	err   error
	reads int
	asked []netip.Addr
}

func (f *fakeLinks) read(addr netip.Addr) (Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.asked = append(f.asked, addr)
	return f.link, f.err
}

func (f *fakeLinks) send(bytes uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.link.SentBytes += bytes
}

var (
	here  = netip.MustParseAddr("192.168.1.10")
	start = time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
)

func newFake() *fakeLinks {
	return &fakeLinks{link: Link{Name: "enp5s0", Kind: Ethernet, Mbps: 1000, SentBytes: 1 << 40, Counted: true}}
}

func TestTheFirstReadingHasNoRateAndTheNextOneDoes(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, 2*time.Second)

	first, err := s.Sample(here, start)
	if err != nil {
		t.Fatal(err)
	}
	if first.Rated || first.Name != "enp5s0" || first.Mbps != 1000 || !first.At.Equal(start) {
		t.Fatalf("first = %+v, want the link, unrated, read at the start", first)
	}

	// 5 MB in 5 s is 8 Mb/s.
	fake.send(5_000_000)
	second, err := s.Sample(here, start.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Rated || second.SendBitsPerSecond != 8_000_000 {
		t.Fatalf("second = %d b/s (rated %v), want 8000000", second.SendBitsPerSecond, second.Rated)
	}
	if len(fake.asked) != 2 || fake.asked[0] != here {
		t.Errorf("read was asked %v, want the sampled address twice", fake.asked)
	}
}

func TestAskingWithinTheIntervalAnswersFromTheLastReading(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, 2*time.Second)
	if _, err := s.Sample(here, start); err != nil {
		t.Fatal(err)
	}
	fake.send(1_000_000)
	for _, after := range []time.Duration{0, time.Second, 1999 * time.Millisecond} {
		got, err := s.Sample(here, start.Add(after))
		if err != nil {
			t.Fatal(err)
		}
		if !got.At.Equal(start) || got.SentBytes != 1<<40 {
			t.Errorf("at +%v: %+v, want the reading taken at the start", after, got)
		}
	}
	if fake.reads != 1 {
		t.Errorf("read %d times, want once", fake.reads)
	}

	// At the interval it reads again, and rates the two seconds since.
	got, _ := s.Sample(here, start.Add(2*time.Second))
	if fake.reads != 2 || got.SendBitsPerSecond != 4_000_000 {
		t.Errorf("at the interval: %d reads, %d b/s; want 2 reads, 4000000 b/s", fake.reads, got.SendBitsPerSecond)
	}
}

func TestAClockThatWentBackReadsAgain(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, 2*time.Second)
	s.Sample(here, start)
	got, _ := s.Sample(here, start.Add(-time.Second))
	if fake.reads != 2 || got.Rated {
		t.Errorf("%d reads, rated %v; want a fresh reading with no rate across a negative gap", fake.reads, got.Rated)
	}
}

func TestARateIsNotWorkedOutAcrossTooLongAGap(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, 2*time.Second)
	s.Sample(here, start)

	fake.send(1_000_000)
	late, _ := s.Sample(here, start.Add(RateWindow+time.Second))
	if late.Rated {
		t.Errorf("rated across %v: %d b/s", RateWindow+time.Second, late.SendBitsPerSecond)
	}
	// But it starts the next one.
	fake.send(1_000_000)
	next, _ := s.Sample(here, late.At.Add(4*time.Second))
	if !next.Rated || next.SendBitsPerSecond != 2_000_000 {
		t.Errorf("after the gap: %d b/s (rated %v), want 2000000", next.SendBitsPerSecond, next.Rated)
	}
}

func TestNoRateFromCountsThatCannotBeCompared(t *testing.T) {
	cases := map[string]func(f *fakeLinks){
		"the count started again": func(f *fakeLinks) { f.link.SentBytes = 10 },
		"another interface":       func(f *fakeLinks) { f.link.Name = "wlan0"; f.link.SentBytes += 1000 },
		"no count any more":       func(f *fakeLinks) { f.link.Counted = false },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFake()
			s := NewSampler(fake.read, 0)
			s.Sample(here, start)
			change(fake)
			if got, _ := s.Sample(here, start.Add(5*time.Second)); got.Rated {
				t.Errorf("rated %d b/s", got.SendBitsPerSecond)
			}
		})
	}

	t.Run("never counted", func(t *testing.T) {
		fake := newFake()
		fake.link.Counted = false
		s := NewSampler(fake.read, 0)
		s.Sample(here, start)
		if got, _ := s.Sample(here, start.Add(5*time.Second)); got.Rated {
			t.Errorf("rated %d b/s", got.SendBitsPerSecond)
		}
	})
}

func TestAFailedReadingKeepsTheLastGoodOne(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, 0)
	s.Sample(here, start)

	fake.err = errors.New("gone")
	if _, err := s.Sample(here, start.Add(time.Second)); err == nil {
		t.Fatal("a failed reading was not reported")
	}

	fake.err = nil
	fake.send(3_000_000)
	got, err := s.Sample(here, start.Add(3*time.Second))
	if err != nil || !got.Rated || got.SendBitsPerSecond != 8_000_000 {
		t.Errorf("after a failure: %d b/s (rated %v, err %v); want 8000000 against the last good reading",
			got.SendBitsPerSecond, got.Rated, err)
	}
}

func TestEachAddressHasItsOwnReadings(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, 2*time.Second)
	there := netip.MustParseAddr("10.0.0.5")
	s.Sample(here, start)
	if got, _ := s.Sample(there, start.Add(time.Second)); fake.reads != 2 || got.Rated {
		t.Errorf("another address answered from the first one's reading: %d reads, rated %v", fake.reads, got.Rated)
	}
}

func TestSamplingFromManyRequestsAtOnce(t *testing.T) {
	fake := newFake()
	s := NewSampler(fake.read, time.Hour)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Sample(here, start); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if fake.reads != 1 {
		t.Errorf("16 requests at once read the link %d times, want once", fake.reads)
	}
}
