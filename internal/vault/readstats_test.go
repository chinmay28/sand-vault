package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// The race a read runs is a measurement of the accounts, taken hundreds of
// times a day and, until now, thrown away. These are about keeping it.

// waitForReads settles the goroutines that record what the losers of a decided
// race did. Only a test needs to: a read never waits for them.
func waitForReads(v *Vault) { v.reads.waitForTail() }

func forgetReads(t *testing.T, v *Vault) {
	t.Helper()
	if err := v.ForgetReadStats(); err != nil {
		t.Fatalf("ForgetReadStats: %v", err)
	}
}

func TestReadStatsRecordsWhoAnsweredTheRace(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("the quick brown fox jumps over the lazy dog\n"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// Only the read is being counted, so the writing is not.
	forgetReads(t, v)

	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)

	report := v.ReadStats(WindowToday)
	if report.Races == 0 {
		t.Fatalf("no races recorded for a file that was read")
	}
	if report.Shortfalls != 0 {
		t.Errorf("shortfalls = %d on a read that succeeded", report.Shortfalls)
	}
	if len(report.Accounts) != 3 {
		t.Fatalf("accounts = %d, want all 3 connected", len(report.Accounts))
	}

	scheme := entry.Scheme()
	var wins, fetches int64
	for _, a := range report.Accounts {
		if a.Fetches != a.Wins+a.Late+a.Aborted+a.Failures {
			t.Errorf("%s: %d fetches but %d+%d+%d+%d outcomes",
				a.Name, a.Fetches, a.Wins, a.Late, a.Aborted, a.Failures)
		}
		if a.Failures != 0 {
			t.Errorf("%s failed a fetch with every account online: %s", a.Name, a.LastError)
		}
		if !a.Connected {
			t.Errorf("%s reported as disconnected", a.Name)
		}
		wins += a.Wins
		fetches += a.Fetches
	}

	// Every account was asked once per race and exactly k of them were used.
	if want := report.Races * int64(scheme.Data); wins != want {
		t.Errorf("wins = %d across %d races of %s, want %d", wins, report.Races, scheme, want)
	}
	if want := report.Races * 3; fetches != want {
		t.Errorf("fetches = %d, want %d — every account is asked, every race", fetches, want)
	}
	// The winners are the ones a duration is kept for, and a local folder
	// cannot answer in no time at all.
	for _, a := range report.Accounts {
		if a.Wins > 0 && a.AverageMS <= 0 {
			t.Errorf("%s won %d races with no time recorded", a.Name, a.Wins)
		}
		if a.Wins > 0 && a.Bytes <= 0 {
			t.Errorf("%s won %d races having delivered nothing", a.Name, a.Wins)
		}
	}
}

// An account that never wins is the whole point of the panel, so it has to be
// in the answer — including the one that has not been asked at all yet.
func TestReadStatsListsAccountsThatHaveNotRaced(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("something to read back"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// A fourth account, connected after the file was stored: it holds no shard
	// of it and so enters no race.
	cfg, err := v.AddProvider(context.Background(), provider.Config{
		Kind:    provider.KindLocal,
		Name:    "cloud-latecomer",
		Options: map[string]string{"path": t.TempDir()},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	newcomer := cfg.ID

	found := false
	for _, a := range v.ReadStats(WindowToday).Accounts {
		if a.ProviderID != newcomer {
			continue
		}
		found = true
		if a.Fetches != 0 || a.Wins != 0 {
			t.Errorf("a newly connected account has raced: %+v", a)
		}
		if !a.Connected {
			t.Errorf("newly connected account reported as disconnected")
		}
	}
	if !found {
		t.Errorf("an account with no races is missing from the report")
	}
}

// A cloud that cannot answer is counted apart from one that merely lost, and
// the read still succeeds on the other two.
func TestReadStatsCountsFailuresApartFromLosses(t *testing.T) {
	v, roots := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("stored while all three were up"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// One account's storage taken out from under it, which is what a cloud
	// that has stopped answering looks like from here. Under the strict policy
	// every account holds a shard of every file, so the first one will do.
	broken := v.providers[0].ID
	if err := os.RemoveAll(roots[0]); err != nil {
		t.Fatalf("breaking an account: %v", err)
	}
	forgetReads(t, v)

	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch with one account down: %v", err)
	}
	waitForReads(v)

	for _, a := range v.ReadStats(WindowToday).Accounts {
		if a.ProviderID != broken {
			continue
		}
		if a.Failures == 0 {
			t.Errorf("the account whose files were deleted recorded no failure: %+v", a)
		}
		if a.LastError == "" {
			t.Errorf("a failure with nothing said about it: %+v", a)
		}
		if a.Wins != 0 {
			t.Errorf("an account with no files won %d races", a.Wins)
		}
	}
}

// A read that cannot find k shards is a different thing from a slow one, and
// is counted as such.
func TestReadStatsCountsShortfalls(t *testing.T) {
	v, roots := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("about to become unreachable"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, root := range roots[:2] {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("breaking an account: %v", err)
		}
	}
	forgetReads(t, v)

	if _, _, err := v.Fetch(context.Background(), entry.ID); err == nil {
		t.Fatalf("Fetch succeeded with only one account left")
	}
	waitForReads(v)

	if got := v.ReadStats(WindowToday).Shortfalls; got == 0 {
		t.Errorf("shortfalls = 0 after a read that could not gather enough shards")
	}
}

func TestForgetReadStatsStartsAgain(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("read once, then forgotten"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)

	before := v.ReadStats(WindowToday)
	if before.Races == 0 {
		t.Fatalf("nothing recorded to forget")
	}

	forgetReads(t, v)
	after := v.ReadStats(WindowToday)
	if after.Races != 0 || after.Shortfalls != 0 {
		t.Errorf("forgetting left %d races and %d shortfalls", after.Races, after.Shortfalls)
	}
	if !after.Since.IsZero() {
		t.Errorf("forgetting left a start date of %v", after.Since)
	}
	// The accounts stay — they are still connected — with nothing against them.
	if len(after.Accounts) != 3 {
		t.Errorf("accounts = %d after forgetting, want the 3 connected", len(after.Accounts))
	}
	for _, a := range after.Accounts {
		if a.Fetches != 0 || a.Wins != 0 || a.AverageMS != 0 {
			t.Errorf("%s kept a figure across the forgetting: %+v", a.Name, a)
		}
	}
}

// Losing a race is not failing one: the read path cancels the accounts it no
// longer needs, and blaming them for stopping would make every wide code look
// broken.
func TestLostOutcomeSeparatesCancellationFromFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want readOutcome
	}{
		{"answered late", nil, shardLate},
		{"cut off", context.Canceled, shardAborted},
		{"wrapped cancellation", fmt.Errorf("part 2 from cloud-b: %w", context.Canceled), shardAborted},
		{"genuinely failed", errors.New("403 from the account"), shardFailed},
	}
	for _, tc := range cases {
		if got := lostOutcome(shardFetch{err: tc.err}); got != tc.want {
			t.Errorf("%s: outcome %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The averages are over the fetches that finished. An aborted fetch was cut
// off by us and a failed one never arrived; averaging either in would make the
// slowest account look quick for having been given up on.
func TestReadStatsAveragesOnlyWhatArrived(t *testing.T) {
	s := newReadStats()
	shard := Shard{Part: 1, ProviderID: "p1", ProviderName: "cloud-a", ProviderKind: "local"}

	s.record(shardFetch{shard: shard, blob: make([]byte, 10), took: 20 * time.Millisecond}, shardWon)
	s.record(shardFetch{shard: shard, blob: make([]byte, 10), took: 40 * time.Millisecond}, shardLate)
	s.record(shardFetch{shard: shard, took: 9 * time.Second}, shardAborted)
	s.record(shardFetch{shard: shard, took: 9 * time.Second, err: errors.New("timeout")}, shardFailed)

	stats := s.report(WindowAll, nil, time.Now()).Accounts
	if len(stats) != 1 {
		t.Fatalf("accounts = %d, want 1", len(stats))
	}
	a := stats[0]
	if a.Fetches != 4 || a.Wins != 1 || a.Late != 1 || a.Aborted != 1 || a.Failures != 1 {
		t.Fatalf("outcomes not kept apart: %+v", a)
	}
	if a.Connected {
		t.Errorf("an account nobody is connected to reported as connected")
	}
	if a.AverageMS != 30 || a.FastestMS != 20 || a.SlowestMS != 40 {
		t.Errorf("timings = %v/%v/%v, want 30/20/40 over the two that arrived",
			a.AverageMS, a.FastestMS, a.SlowestMS)
	}
	if a.Bytes != 20 {
		t.Errorf("bytes = %d, want the 20 that actually arrived", a.Bytes)
	}
	if a.Name != "cloud-a" || a.Kind != "local" {
		t.Errorf("a disconnected account lost its name: %+v", a)
	}
}

// The board is read top down, so the accounts carrying the reads are at the
// top and the one carrying none is at the bottom where it can be seen.
func TestReadStatsRanksWinnersFirst(t *testing.T) {
	s := newReadStats()
	for i, spec := range []struct {
		id   string
		wins int
	}{{"quiet", 0}, {"middling", 2}, {"busiest", 5}} {
		shard := Shard{Part: i + 1, ProviderID: spec.id, ProviderName: spec.id}
		s.record(shardFetch{shard: shard, took: time.Millisecond}, shardAborted)
		for w := 0; w < spec.wins; w++ {
			s.record(shardFetch{shard: shard, took: time.Millisecond}, shardWon)
		}
	}

	order := []string{}
	for _, a := range s.report(WindowAll, nil, time.Now()).Accounts {
		order = append(order, a.ProviderID)
	}
	want := []string{"busiest", "middling", "quiet"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
