package vault

import (
	"context"
	"os"
	"testing"
	"time"
)

// The figures outlive the process now, which is the whole point of asking for
// this year: a window longer than an afternoon is only worth offering if what
// it counts survives a restart.

// seedDay puts a day's worth of figures straight into the recorder. Recording
// them the ordinary way would file them all under today, which is exactly what
// a test about windows cannot use.
func seedDay(s *readStats, day time.Time, id string, wins, fetches int64, answer time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := day.Format(dayFormat)
	bucket := s.days[key]
	if bucket == nil {
		bucket = &readBucket{}
		s.days[key] = bucket
	}
	counts := readCounts{
		Fetches: fetches,
		Wins:    wins,
		Aborted: fetches - wins,
		Answers: wins,
		Bytes:   wins * 1024,
		Total:   answer * time.Duration(wins),
		Fastest: answer,
		Slowest: answer,
	}
	bucket.Races += wins
	bucket.counterFor(id).add(&counts)
	s.total.Races += wins
	s.total.counterFor(id).add(&counts)
	s.names[id] = readAccountName{Name: id, Kind: "local"}
	if s.since.IsZero() || day.Before(s.since) {
		s.since = day
	}
}

func TestWindowsSumTheDaysTheyCover(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	s := newReadStats()

	seedDay(s, now, "cloud-a", 10, 20, 40*time.Millisecond)                     // today
	seedDay(s, now.AddDate(0, 0, -4), "cloud-a", 30, 60, 60*time.Millisecond)   // this month
	seedDay(s, now.AddDate(0, -3, 0), "cloud-a", 100, 200, 80*time.Millisecond) // this year
	seedDay(s, now.AddDate(-1, 0, 0), "cloud-a", 7, 14, 90*time.Millisecond)    // last year

	cases := []struct {
		window ReadWindow
		wins   int64
	}{
		{WindowToday, 10},
		{WindowMonth, 40},
		{WindowYear, 140},
		{WindowAll, 147},
	}
	for _, tc := range cases {
		report := s.report(tc.window, nil, now)
		if len(report.Accounts) != 1 {
			t.Fatalf("%s: accounts = %d, want 1", tc.window, len(report.Accounts))
		}
		if got := report.Accounts[0].Wins; got != tc.wins {
			t.Errorf("%s: wins = %d, want %d", tc.window, got, tc.wins)
		}
		if report.Window != tc.window {
			t.Errorf("report says it is %s, asked for %s", report.Window, tc.window)
		}
	}

	// A window that reaches back past the days it can still see says so: all
	// time is a running total, and only the shape of it thins out.
	if all := s.report(WindowAll, nil, now); all.Days != 4 {
		t.Errorf("all time was summed from %d days, want the 4 recorded", all.Days)
	}
	if today := s.report(WindowToday, nil, now); today.From.IsZero() {
		t.Errorf("a window with a start did not say where it starts")
	}
	if all := s.report(WindowAll, nil, now); !all.From.IsZero() {
		t.Errorf("all time claimed to start at %v", all.From)
	}
}

// An average over a window is over that window's answers, not a mean of means:
// a day with one slow answer must not outweigh a day with two hundred quick
// ones.
func TestWindowAveragesWeighByAnswer(t *testing.T) {
	now := time.Date(2026, 8, 18, 9, 0, 0, 0, time.Local)
	s := newReadStats()

	seedDay(s, now, "cloud-a", 1, 1, 500*time.Millisecond)
	seedDay(s, now.AddDate(0, 0, -1), "cloud-a", 99, 99, 10*time.Millisecond)

	got := s.report(WindowMonth, nil, now).Accounts[0].AverageMS
	want := (500.0 + 99*10) / 100
	if got < want-0.01 || got > want+0.01 {
		t.Errorf("average = %v ms, want %v — weighted by how many answers each day carried", got, want)
	}
	// The extremes are the extremes of the whole window.
	if a := s.report(WindowMonth, nil, now).Accounts[0]; a.FastestMS != 10 || a.SlowestMS != 500 {
		t.Errorf("range = %v–%v ms, want 10–500", a.FastestMS, a.SlowestMS)
	}
}

func TestTrendIsCutIntoAtMostThirtyTwoSpans(t *testing.T) {
	now := time.Date(2026, 12, 31, 12, 0, 0, 0, time.Local)
	s := newReadStats()
	for day := 0; day < 300; day++ {
		seedDay(s, now.AddDate(0, 0, -day), "cloud-a", 5, 10, 30*time.Millisecond)
	}

	trend := s.report(WindowYear, nil, now).Accounts[0].Trend
	if len(trend) == 0 {
		t.Fatalf("a year of days drew no trend at all")
	}
	if len(trend) > trendPoints {
		t.Errorf("trend has %d points, more than the %d a sparkline can show", len(trend), trendPoints)
	}
	var wins int64
	for i, point := range trend {
		wins += point.Wins
		if point.To.Before(point.From) {
			t.Errorf("point %d ends before it starts", i)
		}
		if i > 0 && point.From.Before(trend[i-1].To.AddDate(0, 0, -1)) {
			t.Errorf("point %d starts before the one before it ended", i)
		}
	}
	if wins != 300*5 {
		t.Errorf("the trend accounts for %d wins, want every one of the 1500", wins)
	}

	// One day is not a shape. Today is a single bucket and gets no line drawn
	// through it.
	if trend := s.report(WindowToday, nil, now).Accounts[0].Trend; len(trend) != 0 {
		t.Errorf("today drew a %d-point trend out of one day", len(trend))
	}
}

// Locking a vault saves what it counted; unlocking it again reads it back.
func TestReadHistorySurvivesLockAndUnlock(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("read me, then lock the vault"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)

	before := v.ReadStats(WindowAll)
	if before.Races == 0 {
		t.Fatalf("nothing recorded to save")
	}

	v.Lock()
	if _, err := os.Stat(readHistoryPath(v.Path())); err != nil {
		t.Fatalf("locking the vault wrote no history: %v", err)
	}
	// It is sealed: the figures say which clouds this vault is on and when it
	// is read, which is the same kind of thing the index itself is.
	raw, err := os.ReadFile(readHistoryPath(v.Path()))
	if err != nil {
		t.Fatalf("reading the history back: %v", err)
	}
	if len(raw) > 0 && raw[0] == '{' {
		t.Errorf("the read history was written in the clear")
	}

	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	after := v.ReadStats(WindowAll)
	if after.Races != before.Races {
		t.Errorf("races = %d after unlocking, want the %d that were saved", after.Races, before.Races)
	}
	if len(after.Accounts) != len(before.Accounts) {
		t.Errorf("accounts = %d after unlocking, want %d", len(after.Accounts), len(before.Accounts))
	}
	for _, a := range after.Accounts {
		for _, b := range before.Accounts {
			if a.ProviderID != b.ProviderID {
				continue
			}
			if a.Wins != b.Wins || a.Fetches != b.Fetches {
				t.Errorf("%s came back as %d/%d, saved as %d/%d",
					a.Name, a.Wins, a.Fetches, b.Wins, b.Fetches)
			}
		}
	}
}

// A password change rotates the key everything is sealed under, this file
// included. It is rewritten as part of the change rather than left to be found
// unreadable later.
func TestReadHistorySurvivesAPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("counted under one password, read under another"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)
	before := v.ReadStats(WindowAll).Races

	// Saved first, and deliberately: a change that arrives with nothing new to
	// write is the case that used to leave a file on disk sealed under a key
	// nothing would ever hold again.
	v.flushReadHistory()

	// The re-encryption is deferred, because migrating every file is itself a
	// read of every file — it would add races of its own and the test would be
	// measuring its own setup.
	const newPassword = "a different correct horse"
	if _, err := v.ChangePassword(context.Background(), testPassword, newPassword, false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	v.Lock()
	if err := v.Unlock(newPassword); err != nil {
		t.Fatalf("Unlock with the new password: %v", err)
	}

	if got := v.ReadStats(WindowAll).Races; got != before {
		t.Errorf("races = %d after a password change, want the %d counted before it", got, before)
	}
}

// The same, with nothing in memory to write back: a password can be changed on
// a locked vault, and the history on disk is still sealed under the key that
// change retires. It has to be read with the old key before it can be written
// with the new one.
func TestReadHistorySurvivesAPasswordChangeOnALockedVault(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("counted, saved, then locked away"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)
	before := v.ReadStats(WindowAll).Races

	// Locking saves what was counted and drops it: from here the only copy is
	// the file, sealed under the key the change is about to retire.
	v.Lock()

	const newPassword = "typed at a lock screen"
	if _, err := v.ChangePassword(context.Background(), testPassword, newPassword, false); err != nil {
		t.Fatalf("ChangePassword on a locked vault: %v", err)
	}
	if err := v.Unlock(newPassword); err != nil {
		t.Fatalf("Unlock with the new password: %v", err)
	}

	if got := v.ReadStats(WindowAll).Races; got != before {
		t.Errorf("races = %d after a password change on a locked vault, want %d", got, before)
	}
}

func TestForgettingTakesTheFileWithIt(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("about to be forgotten"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)
	v.flushReadHistory()

	if _, err := os.Stat(readHistoryPath(v.Path())); err != nil {
		t.Fatalf("no history to forget: %v", err)
	}
	if err := v.ForgetReadStats(); err != nil {
		t.Fatalf("ForgetReadStats: %v", err)
	}
	if _, err := os.Stat(readHistoryPath(v.Path())); !os.IsNotExist(err) {
		t.Errorf("the history file outlived being forgotten: %v", err)
	}
	if got := v.ReadStats(WindowAll).Races; got != 0 {
		t.Errorf("races = %d after forgetting", got)
	}
}

// A sidecar that will not open is a fresh start, not a broken vault.
func TestAnUnreadableHistoryIsSteppedOver(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := os.WriteFile(readHistoryPath(v.Path()), []byte("not a sealed anything at all"), 0600); err != nil {
		t.Fatalf("writing rubbish: %v", err)
	}

	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("a vault refused to open over an unreadable history file: %v", err)
	}
	if got := v.ReadStats(WindowAll).Races; got != 0 {
		t.Errorf("races = %d from a file that could not be read", got)
	}
}

// The daily buckets are pruned past the horizon; all time is not.
func TestPruningKeepsTheTotalItDropsTheDays(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.Local)
	s := newReadStats()
	for day := 0; day < historyDays+40; day++ {
		seedDay(s, now.AddDate(0, 0, -day), "cloud-a", 2, 4, 20*time.Millisecond)
	}

	s.mu.Lock()
	s.pruneLocked()
	kept := len(s.days)
	s.mu.Unlock()

	if kept != historyDays {
		t.Errorf("kept %d daily buckets, want %d", kept, historyDays)
	}
	if got := s.report(WindowAll, nil, now).Accounts[0].Wins; got != int64((historyDays+40)*2) {
		t.Errorf("all time = %d wins after pruning, want every one of the %d recorded",
			got, (historyDays+40)*2)
	}
}
