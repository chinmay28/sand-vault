package vault

import (
	"testing"
	"time"
)

const gb = int64(1 << 30)

// daysToEarn is the time a meter at rate bytes per day takes to accumulate n.
func daysToEarn(n, rate int64) time.Duration {
	return time.Duration(float64(n) / float64(rate) * float64(day))
}

func TestPacerStartsWithAnHourOfBudget(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 24 * gb}

	ok, wait := p.Ready(now)
	if !ok || wait != 0 {
		t.Fatalf("Ready on a fresh meter = %v, %v; want yes, now", ok, wait)
	}
	if p.Level != gb {
		t.Errorf("Level = %d, want one hour's worth (%d)", p.Level, gb)
	}
}

func TestPacerAFileBiggerThanTheBurstPaysItselfOff(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 10 * gb}

	// A 40 GB film on a 10 GB/day budget: it starts, because the bucket is
	// not negative, and then nothing else does for close to four days.
	if ok, _ := p.Ready(now); !ok {
		t.Fatal("the first file should start")
	}
	p.Spend(now, 40*gb)

	ok, wait := p.Ready(now)
	if ok {
		t.Fatal("a second file started on a bucket in debt")
	}
	// 40 GB minus the hour's worth it had: 39.58 GB owed at 10 GB/day.
	wantWait := daysToEarn(40*gb-10*gb/24, 10*gb)
	if diff := (wait - wantWait).Abs(); diff > time.Second {
		t.Errorf("wait = %v, want about %v", wait, wantWait)
	}

	// Half way there is still no.
	if ok, _ := p.Ready(now.Add(2 * day)); ok {
		t.Error("ready after two days with four days of debt")
	}
	// And once the debt is paid, yes.
	if ok, wait := p.Ready(now.Add(wantWait + time.Second)); !ok {
		t.Errorf("not ready once the debt is paid, wait = %v", wait)
	}
}

func TestPacerAveragesToTheRate(t *testing.T) {
	// Files of assorted sizes, moved whenever the meter allows, over thirty
	// days: what moved should be thirty days of budget, give or take one
	// file.
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 10 * gb}
	sizes := []int64{3 * gb, 700 << 20, 12 * gb, 50 << 20, 8 * gb, 1 * gb}

	var moved int64
	now := start
	for i := 0; now.Before(start.Add(30 * day)); i++ {
		ok, wait := p.Ready(now)
		if !ok {
			now = now.Add(wait)
			continue
		}
		size := sizes[i%len(sizes)]
		p.Spend(now, size)
		moved += size
		// A move takes some time of its own; the meter does not care.
		now = now.Add(10 * time.Minute)
	}

	budget := 30 * 10 * gb
	if moved < budget-12*gb || moved > budget+12*gb {
		t.Errorf("moved %d bytes in thirty days, want within one file of %d", moved, budget)
	}
}

func TestPacerNeverBanksMoreThanTheBurst(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 24 * gb}
	p.Ready(now)

	// A week shut should not be a week of budget on reopening.
	p.Ready(now.Add(7 * day))
	if p.Level != gb {
		t.Errorf("Level after a week idle = %d, want the burst (%d)", p.Level, gb)
	}
}

func TestPacerAClockThatWentBackwardsEarnsNothing(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 24 * gb}
	p.Ready(now)
	p.Spend(now, 3*gb)
	before := p.Level

	p.Ready(now.Add(-time.Hour))
	if p.Level != before {
		t.Errorf("Level changed from %d to %d on a clock that went back", before, p.Level)
	}
	if !p.At.Equal(now.Add(-time.Hour)) {
		t.Error("At did not follow the clock, so the lost hour will be earned twice")
	}
}

func TestPacerRefundUndoesACharge(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 24 * gb}
	p.Ready(now)
	p.Spend(now, 5*gb)
	p.Refund(now, 5*gb)

	if p.Level != gb {
		t.Errorf("Level after a refund = %d, want back at the burst (%d)", p.Level, gb)
	}
	// A refund cannot bank more than the burst either.
	p.Refund(now, 50*gb)
	if p.Level != gb {
		t.Errorf("Level after an oversized refund = %d, want the burst", p.Level)
	}
}

func TestPacerETA(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	p := &Pacer{Rate: 10 * gb}
	p.Ready(now)

	// 100 GB outstanding at 10 GB/day with an hour already banked.
	eta := p.ETA(now, 100*gb)
	want := daysToEarn(100*gb-10*gb/24, 10*gb)
	if diff := (eta - want).Abs(); diff > time.Second {
		t.Errorf("ETA = %v, want about %v", eta, want)
	}
	if p.ETA(now, 0) != 0 {
		t.Error("nothing outstanding should have no ETA")
	}
	if p.ETA(now, 100<<20) != 0 {
		t.Error("less than what is banked should move at once")
	}
}

func TestPacerUnmeteredLetsEverythingThrough(t *testing.T) {
	now := time.Now()
	for _, p := range []*Pacer{nil, {}, {Rate: -1}} {
		if ok, wait := p.Ready(now); !ok || wait != 0 {
			t.Errorf("%+v: Ready = %v, %v; want yes, now", p, ok, wait)
		}
		p.Spend(now, 1<<40)
		if ok, _ := p.Ready(now); !ok {
			t.Errorf("%+v: not ready after a spend", p)
		}
		if p.ETA(now, 1<<40) != 0 {
			t.Errorf("%+v: an unmetered meter has an ETA", p)
		}
	}
}
