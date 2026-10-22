package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

func TestArchiveFlightsHoldOutlastsTheRelease(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &archiveFlights{now: func() time.Time { return clock }}

	has := func(id string) bool {
		for _, got := range f.ids() {
			if got == id {
				return true
			}
		}
		return false
	}

	release := f.hold("a")
	if !has("a") {
		t.Fatal("a held archive is not listed")
	}
	release()
	if !has("a") {
		t.Fatal("an archive released a moment ago is already forgotten — the caller has not committed yet")
	}

	// Releasing twice changes nothing, which is what lets the scatter defer it
	// without caring how it returned.
	release()
	if !has("a") {
		t.Fatal("a second release dropped the archive")
	}

	clock = clock.Add(archiveSettleWindow - time.Second)
	if !has("a") {
		t.Fatal("forgotten before the window passed")
	}
	clock = clock.Add(2 * time.Second)
	if has("a") {
		t.Fatal("still listed after the window passed")
	}

	// Held again after settling: the hold wins over the stale settle stamp.
	f.hold("a")
	if !has("a") {
		t.Fatal("a re-held archive is not listed")
	}
}

func TestAnUploadRegistersItsArchive(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.flights.settle = 0 // the real window, which newTestVault closes

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", []byte("spoken for"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, id := range v.flights.ids() {
		if id == entry.ArchiveID {
			return
		}
	}
	t.Fatalf("the archive %s an upload just scattered is not in the registry", entry.ArchiveID)
}

// The bug this registry exists for: parts written by a scatter that has not
// been recorded yet look, to a sweep, exactly like parts nobody wants.
func TestOrphanSweepLeavesAnArchiveInFlightAlone(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	// Something in the index, so the empty-vault guard is not what holds the
	// sweep back.
	if _, _, err := v.Upload(ctx, MainScope, "/", "ours.txt", []byte("stored"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// The scatter of a large upload, part way through: a few chunk objects of
	// an archive the index has never heard of, and the ID held.
	clock := time.Now()
	v.flights.settle = 0 // the real window, which newTestVault closes
	v.flights.now = func() time.Time { return clock }
	const id = "0123456789abcdef0123456789abcdef"
	var keys []string
	for chunk := 0; chunk < 3; chunk++ {
		key := ChunkShardKey(id, chunk, 1)
		keys = append(keys, key)
		if err := os.WriteFile(filepath.Join(roots[0], key), []byte("half written"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", key, err)
		}
	}
	release := v.flights.hold(id)

	scan := orphanScan(t, v)
	if scan.Found || scan.Archives != 0 || scan.Deletable != 0 {
		t.Fatalf("an archive being written was reported as abandoned: %+v", scan)
	}
	if len(scan.Strays) != 0 {
		t.Fatalf("an archive being written was reported as a stray: %+v", scan.Strays)
	}

	report, err := v.SweepOrphans(ctx, nil, false, nil)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if report.Archives != 0 || report.Deleted != 0 {
		t.Fatalf("the sweep erased an archive being written: %+v", report)
	}
	// Asked for by name, it is refused rather than erased.
	report, err = v.SweepOrphans(ctx, []OrphanTarget{{ProviderID: providerHolding(t, v, roots[0]), ArchiveID: id}}, false, nil)
	if err != nil {
		t.Fatalf("SweepOrphans by target: %v", err)
	}
	if report.Deleted != 0 || len(report.Skipped) != 1 {
		t.Fatalf("a named target in flight was not refused: %+v", report)
	}
	for _, key := range keys {
		if _, err := os.Stat(filepath.Join(roots[0], key)); err != nil {
			t.Fatalf("%s is gone: %v", key, err)
		}
	}

	// The scatter returns and its caller never commits — a crash, say. Once
	// the window has passed the parts are what they look like.
	release()
	scan = orphanScan(t, v)
	if scan.Found {
		t.Fatalf("reported the moment the scatter returned, before its caller could commit: %+v", scan)
	}
	clock = clock.Add(archiveSettleWindow + time.Second)
	scan = orphanScan(t, v)
	if !scan.Found || scan.Archives != 1 || scan.Deletable != len(keys) {
		t.Fatalf("an archive nothing committed is still protected after the window: %+v", scan)
	}
	if report, err := v.SweepOrphans(ctx, nil, false, nil); err != nil || report.Deleted != len(keys) {
		t.Fatalf("SweepOrphans after the window = %+v, %v; want %d objects erased", report, err, len(keys))
	}
}

// An archive in flight makes the vault's parts spoken for; it does not make
// the vault a vault that holds files. The recovery guard reads the index.
func TestAnArchiveInFlightDoesNotLiftTheRecoveryGuard(t *testing.T) {
	owned := map[string]*ownedArchive{
		"inflight": {parts: map[int]bool{}, accounts: map[string]bool{}, inFlight: true},
	}
	accounts := []OrphanAccount{{Name: "borrowed", Orphans: 3, Foreign: true}}
	blocked := orphanGuard(owned, accounts)
	if len(blocked) != 1 || !strings.Contains(blocked[0], "waiting to be recovered") {
		t.Fatalf("blocked = %v, want the recovery guard", blocked)
	}
}

// The version scan reads the same registry: a superseded chunk of an archive
// being written is a stored file's, not a part no file points at.
func TestStaleVersionsKnowAnArchiveInFlight(t *testing.T) {
	ctx := context.Background()
	v, stub, configs := newVersionedVault(t, 1)

	const id = "0123456789abcdef0123456789abcdef"
	key := ChunkShardKey(id, 0, 1)
	console, err := provider.New(provider.Config{Kind: provider.KindS3, Options: stub.Options(configs[0].Name)})
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	// A chunk put twice, the way a retried upload leaves a bucket: one stale
	// version under the current one.
	for i := 0; i < 2; i++ {
		if err := console.Put(ctx, key, []byte(fmt.Sprintf("attempt %d", i))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	defer v.flights.hold(id)()

	scan := versionScan(t, v)
	rows := rowsFor(scan, key)
	if len(rows) != 1 {
		t.Fatalf("rows for %s = %+v, want one", key, scan.Items)
	}
	if rows[0].What != "a part of a stored file" {
		t.Errorf("What = %q, want the archive read as a stored file's", rows[0].What)
	}
}
