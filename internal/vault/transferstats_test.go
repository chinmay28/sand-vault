package vault

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// How much has moved to and from each cloud. These are about the meter at Put
// and Get, and about the board built from what it counted.

// seedTransfer puts a day's traffic straight into the recorder, which the
// ordinary path would file under today.
func seedTransfer(s *readStats, day time.Time, id string, in, out int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := day.Format(dayFormat)
	bucket := s.days[key]
	if bucket == nil {
		bucket = &readBucket{}
		s.days[key] = bucket
	}
	counts := transferCounts{BytesIn: in, BytesOut: out, Puts: 1, Gets: 1, LastAt: day}
	bucket.transfersFor(id).add(&counts)
	s.total.transfersFor(id).add(&counts)
	s.names[id] = readAccountName{Name: id, Kind: "local"}
	if s.since.IsZero() || day.Before(s.since) {
		s.since = day
	}
}

func transferOf(t *testing.T, report TransferReport, id string) TransferStat {
	t.Helper()
	for _, a := range report.Accounts {
		if a.ProviderID == id {
			return a
		}
	}
	t.Fatalf("account %s is not on the transfer board: %+v", id, report.Accounts)
	return TransferStat{}
}

func TestTransfersCountUploadsInAndReadsOut(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	forgetReads(t, v)

	body := make([]byte, 64*1024)
	for i := range body {
		body[i] = byte(i * 7)
	}
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "data.bin", body, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// The index backup is traffic as well, but it is pushed in the background;
	// let it land so the figures below are only what this test caused.
	v.AwaitBackupSync()

	uploaded := v.TransferStats(WindowToday)
	if len(uploaded.Accounts) != 3 {
		t.Fatalf("accounts = %d, want all 3 connected", len(uploaded.Accounts))
	}
	var sumIn int64
	for _, a := range uploaded.Accounts {
		if a.Puts == 0 || a.BytesIn == 0 {
			t.Errorf("%s: %d puts, %d bytes in after an upload that put a part on every account",
				a.Name, a.Puts, a.BytesIn)
		}
		if a.PutFailures != 0 || a.GetFailures != 0 {
			t.Errorf("%s: failures recorded with every account online: %+v", a.Name, a)
		}
		if !a.Connected {
			t.Errorf("%s reported as disconnected", a.Name)
		}
		sumIn += a.BytesIn
	}
	if uploaded.BytesIn != sumIn {
		t.Errorf("board total in = %d, accounts add up to %d", uploaded.BytesIn, sumIn)
	}

	// What comes back off the clouds is counted apart from what went on.
	forgetReads(t, v)
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)

	read := v.TransferStats(WindowToday)
	if read.BytesIn != 0 {
		t.Errorf("bytes in = %d after a read alone", read.BytesIn)
	}
	// A rebuild needs at least k shards' worth, and a shard is a kth of the
	// file — so at least the file's compressed size came off the clouds. The
	// body above compresses, so the floor is just "something".
	if read.BytesOut == 0 {
		t.Errorf("nothing came off the clouds for a read that worked")
	}
	var gets int64
	for _, a := range read.Accounts {
		gets += a.Gets
	}
	if want := int64(entry.Scheme().Data); gets < want {
		t.Errorf("gets = %d, want at least the %d a rebuild needs", gets, want)
	}
}

func TestTransfersIgnoreCallsThatMovedNothingAndFailedAtNothing(t *testing.T) {
	cfg := provider.Config{ID: "cloud-a", Name: "a", Kind: provider.KindLocal}

	for _, err := range []error{
		context.Canceled,
		fmt.Errorf("part 2: %w", context.Canceled),
		context.DeadlineExceeded,
		provider.ErrNotFound,
	} {
		s := newReadStats()
		s.transferred(cfg, transferOut, 0, err)
		if s.recorded() {
			t.Errorf("%v was recorded: the vault called it off, or the object was never there", err)
		}
	}

	s := newReadStats()
	s.transferred(cfg, transferIn, 100, errors.New("503 slow down"))
	s.transferred(cfg, transferOut, 0, errors.New("connection reset"))
	a := transferOf(t, s.transferReport(WindowToday, nil, time.Now()), "cloud-a")
	if a.PutFailures != 1 || a.GetFailures != 1 {
		t.Errorf("failures = %d put, %d get; want 1 and 1", a.PutFailures, a.GetFailures)
	}
	if a.BytesIn != 0 || a.Puts != 0 {
		t.Errorf("a failed put counted as %d bytes over %d puts", a.BytesIn, a.Puts)
	}
}

func TestTransfersToAReplicaRootCountAgainstTheAccount(t *testing.T) {
	s := newReadStats()
	s.transferred(provider.Config{ID: "cloud-a", Name: "a"}, transferIn, 10, nil)
	s.transferred(provider.Config{ID: replicaID("cloud-a"), Name: "a"}, transferIn, 5, nil)

	report := s.transferReport(WindowToday, nil, time.Now())
	if len(report.Accounts) != 1 {
		t.Fatalf("accounts = %d, want the one account its two roots belong to: %+v",
			len(report.Accounts), report.Accounts)
	}
	if a := report.Accounts[0]; a.BytesIn != 15 || a.Puts != 2 {
		t.Errorf("account = %d bytes over %d puts, want 15 over 2", a.BytesIn, a.Puts)
	}
}

func TestTransferWindowsSumTheDaysTheyCover(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	s := newReadStats()

	seedTransfer(s, now, "cloud-a", 1, 10)
	seedTransfer(s, now.AddDate(0, 0, -4), "cloud-a", 2, 20)
	seedTransfer(s, now.AddDate(0, -3, 0), "cloud-a", 4, 40)
	seedTransfer(s, now.AddDate(-1, 0, 0), "cloud-a", 8, 80)

	cases := []struct {
		window  ReadWindow
		in, out int64
	}{
		{WindowToday, 1, 10},
		{WindowMonth, 3, 30},
		{WindowYear, 7, 70},
		{WindowAll, 15, 150},
	}
	for _, tc := range cases {
		report := s.transferReport(tc.window, nil, now)
		if report.BytesIn != tc.in || report.BytesOut != tc.out {
			t.Errorf("%s: %d in / %d out, want %d / %d",
				tc.window, report.BytesIn, report.BytesOut, tc.in, tc.out)
		}
		if a := transferOf(t, report, "cloud-a"); a.BytesIn != tc.in {
			t.Errorf("%s: account in = %d, want %d", tc.window, a.BytesIn, tc.in)
		}
	}

	// The trend is the vault's traffic span by span, and adds up to the window.
	all := s.transferReport(WindowAll, nil, now)
	if len(all.Trend) != 4 {
		t.Fatalf("trend = %d points over 4 recorded days, want 4", len(all.Trend))
	}
	var in, out int64
	for _, p := range all.Trend {
		in += p.BytesIn
		out += p.BytesOut
	}
	if in != 15 || out != 150 {
		t.Errorf("trend adds up to %d / %d, want 15 / 150", in, out)
	}
	if today := s.transferReport(WindowToday, nil, now); today.Trend != nil {
		t.Errorf("one day drawn as a trend: %+v", today.Trend)
	}
}

func TestTransferBoardListsIdleAccountsAndRanksTheBusiestFirst(t *testing.T) {
	now := time.Now()
	s := newReadStats()
	seedTransfer(s, now, "busy", 100, 100)
	seedTransfer(s, now, "quiet", 1, 0)
	seedTransfer(s, now, "gone", 50, 0)

	connected := []provider.Config{
		{ID: "idle", Name: "idle"},
		{ID: "quiet", Name: "quiet"},
		{ID: "busy", Name: "busy"},
	}
	report := s.transferReport(WindowToday, connected, now)

	var order []string
	for _, a := range report.Accounts {
		order = append(order, a.ProviderID)
	}
	want := []string{"busy", "gone", "quiet", "idle"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if transferOf(t, report, "gone").Connected {
		t.Errorf("an account no longer connected reported as connected")
	}
	if !transferOf(t, report, "idle").Connected {
		t.Errorf("a connected account that moved nothing reported as disconnected")
	}
}

func TestTransfersFollowAReconnectedAccountAndSurviveTheHistoryFile(t *testing.T) {
	now := time.Now()
	s := newReadStats()
	seedTransfer(s, now, "old", 7, 9)
	if !s.mergeAccount("old", "new") {
		t.Fatalf("a merge of an account with only transfers against it moved nothing")
	}
	report := s.transferReport(WindowAll, nil, now)
	if a := transferOf(t, report, "new"); a.BytesIn != 7 || a.BytesOut != 9 {
		t.Errorf("merged account = %d / %d, want 7 / 9", a.BytesIn, a.BytesOut)
	}
	if today := s.transferReport(WindowToday, nil, now); len(today.Accounts) != 1 {
		t.Errorf("the day's bucket still holds the old ID: %+v", today.Accounts)
	}

	// The snapshot written to disk and merged back carries them.
	file, _ := s.snapshot()
	again := newReadStats()
	again.merge(file)
	if a := transferOf(t, again.transferReport(WindowToday, nil, now), "new"); a.BytesIn != 7 {
		t.Errorf("after a round trip through the history file: %+v", a)
	}
}

func TestTransfersSurviveLockAndUnlock(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt",
		[]byte("write me, then lock the vault"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	before := v.TransferStats(WindowAll)
	if before.BytesIn == 0 {
		t.Fatalf("nothing recorded to save")
	}
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// At least what was saved: unlocking may push the index backup again.
	if after := v.TransferStats(WindowAll); after.BytesIn < before.BytesIn {
		t.Errorf("bytes in = %d after unlocking, want at least the %d saved", after.BytesIn, before.BytesIn)
	}
}

// A Put can still land after the vault is locked — a backup in flight — and
// counting it would put a lone figure in a recorder whose real contents are on
// disk, which a lock-screen password change would then save over them.
func TestTransfersAreNotCountedWhileTheVaultIsLocked(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	v.Lock()

	v.reads.transferred(provider.Config{ID: "cloud-a", Name: "a"}, transferIn, 10, nil)
	if v.reads.recorded() {
		t.Fatalf("a transfer was counted into a locked vault's history")
	}

	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	v.reads.transferred(provider.Config{ID: "cloud-a", Name: "a"}, transferIn, 10, nil)
	if !v.reads.recorded() {
		t.Errorf("unlocking did not start counting transfers again")
	}
}
