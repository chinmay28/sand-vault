package vault

import (
	"context"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// How fast the vault is. These are about the timings, the arithmetic that
// turns them into Mbps, and which kind of file a load is counted under.

func TestSpeedIsBitsPerSecondInMegabits(t *testing.T) {
	// 1.25 MB in one second is ten megabits.
	if got := speedOf(1, 1_250_000, time.Second).Mbps; got < 9.999 || got > 10.001 {
		t.Errorf("Mbps = %v, want 10", got)
	}
	if got := speedOf(0, 0, 0); got.Mbps != 0 {
		t.Errorf("nothing timed gave %v Mbps", got.Mbps)
	}
	if got := speedOf(1, 100, 0); got.Mbps != 0 {
		t.Errorf("bytes with no time gave %v Mbps rather than none", got.Mbps)
	}
}

// An average speed is total bytes over total time. A mean of the per-load
// rates would let one quick small load outweigh a long slow one.
func TestSpeedsAreTimeWeighted(t *testing.T) {
	s := newReadStats()
	s.loaded("video", loadOpen, 1_000_000, time.Second)          // 8 Mbps
	s.loaded("video", loadOpen, 1_000_000, 100*time.Millisecond) // 80 Mbps

	report := s.throughputReport(WindowToday, nil, time.Now())
	want := 2_000_000 * 8 / 1.1 / 1e6
	if got := report.Open.Mbps; got < want-0.01 || got > want+0.01 {
		t.Errorf("open = %v Mbps, want %v — total bytes over total time", got, want)
	}
	if report.Open.Count != 2 {
		t.Errorf("opens = %d, want 2", report.Open.Count)
	}
}

func TestThroughputListsTheThreeKindsAskedAboutEvenWhenIdle(t *testing.T) {
	s := newReadStats()
	s.loaded("audio", loadStore, 10, time.Millisecond)
	s.loaded(kindThumbnails, loadOpen, 10, time.Millisecond)
	report := s.throughputReport(WindowToday, nil, time.Now())

	var kinds []string
	for _, k := range report.Kinds {
		kinds = append(kinds, k.Kind)
	}
	want := []string{"images", "video", "documents", "audio", kindThumbnails}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	// The whole vault is every kind together.
	if report.Open.Count != 1 || report.Store.Count != 1 {
		t.Errorf("vault = %d opens, %d stores; want 1 and 1", report.Open.Count, report.Store.Count)
	}
}

func TestLoadsThatMovedNothingAreNotCounted(t *testing.T) {
	s := newReadStats()
	s.loaded("images", loadOpen, 0, time.Second)
	s.loaded("images", loadOpen, 10, 0)
	s.loaded("", loadOpen, 10, time.Second)
	if s.recorded() {
		t.Errorf("a load with no bytes, no time or no kind was recorded")
	}
}

func TestEachCloudsSpeedIsItsOwnCallsTimed(t *testing.T) {
	s := newReadStats()
	cfg := provider.Config{ID: "cloud-a", Name: "a"}
	s.transferred(cfg, transferIn, 1_250_000, time.Second, nil)     // 10 Mbps up
	s.transferred(cfg, transferOut, 2_500_000, time.Second, nil)    // 20 Mbps down
	s.transferred(cfg, transferOut, 0, time.Hour, context.Canceled) // cut off: not timed

	report := s.throughputReport(WindowToday, []provider.Config{cfg, {ID: "idle", Name: "idle"}}, time.Now())
	if len(report.Accounts) != 2 {
		t.Fatalf("accounts = %d, want both connected", len(report.Accounts))
	}
	a := report.Accounts[0]
	if a.ProviderID != "cloud-a" {
		t.Fatalf("fastest first: got %s at the top", a.ProviderID)
	}
	if a.Up.Mbps < 9.99 || a.Up.Mbps > 10.01 || a.Down.Mbps < 19.99 || a.Down.Mbps > 20.01 {
		t.Errorf("up %v / down %v Mbps, want 10 / 20", a.Up.Mbps, a.Down.Mbps)
	}
	if idle := report.Accounts[1]; idle.Up.Mbps != 0 || idle.Down.Mbps != 0 || !idle.Connected {
		t.Errorf("an idle connected cloud: %+v", idle)
	}
}

func TestOpeningAndStoringAreTimedByKind(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	forgetReads(t, v)

	photo := make([]byte, 200_000)
	for i := range photo {
		photo[i] = byte(i * 13)
	}
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "beach.jpg", photo, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Fetch(context.Background(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)
	v.AwaitBackupSync()

	report := v.Throughput(WindowToday)
	var images KindSpeed
	for _, k := range report.Kinds {
		if k.Kind == "images" {
			images = k
		}
	}
	if images.Store.Count != 1 || images.Store.Bytes != int64(len(photo)) || images.Store.Mbps <= 0 {
		t.Errorf("storing a .jpg: %+v", images.Store)
	}
	if images.Open.Count == 0 || images.Open.Bytes != int64(len(photo)) || images.Open.Mbps <= 0 {
		t.Errorf("opening a .jpg: %+v", images.Open)
	}
	if report.Open.Bytes != images.Open.Bytes {
		t.Errorf("vault open bytes = %d, want the image's %d", report.Open.Bytes, images.Open.Bytes)
	}
	for _, a := range report.Accounts {
		if a.Up.Mbps <= 0 {
			t.Errorf("%s: no upload speed after taking a part", a.Name)
		}
	}
}

func TestLoadsSurviveTheHistoryFile(t *testing.T) {
	s := newReadStats()
	s.loaded("video", loadOpen, 1_000, time.Millisecond)
	file, _ := s.snapshot()
	again := newReadStats()
	again.merge(file)
	if got := again.throughputReport(WindowAll, nil, time.Now()).Open.Bytes; got != 1_000 {
		t.Errorf("open bytes after a round trip = %d, want 1000", got)
	}
}
