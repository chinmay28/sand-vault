package vault

import (
	"context"
	"fmt"
	"testing"
)

// degrade takes an account away from a file, the way a forced disconnect does:
// the shard record goes, the file stays, and what is left is a file short of
// the spread it was uploaded with.
func degrade(t *testing.T, v *Vault, id string, parts int) {
	t.Helper()

	e, err := v.Entry(id)
	if err != nil {
		t.Fatalf("Entry %s: %v", id, err)
	}
	if len(e.Shards) < parts {
		t.Fatalf("file has %d parts, cannot drop %d", len(e.Shards), parts)
	}
	v.mu.Lock()
	e.Shards = e.Shards[:len(e.Shards)-parts]
	v.mu.Unlock()
}

func TestDegradedListsOnlyTheFilesShortOfAFullSet(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	whole, _, err := v.Upload(ctx, MainScope, "/", "whole.txt", []byte("all three parts"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	short, _, err := v.Upload(ctx, MainScope, "/", "short.txt", []byte("one part never landed"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	degrade(t, v, short.ID, 1)

	page, err := v.Degraded(0, 0)
	if err != nil {
		t.Fatalf("Degraded: %v", err)
	}
	if page.Total != 1 || len(page.Files) != 1 {
		t.Fatalf("listed %d of %d files, want 1 of 1", len(page.Files), page.Total)
	}

	got := page.Files[0]
	if got.ID != short.ID {
		t.Errorf("listed %s, want the short file %s", got.Path, short.Path())
	}
	if got.Path != "/short.txt" {
		t.Errorf("Path = %q, want /short.txt", got.Path)
	}
	if got.Stored != 2 || got.Missing != 1 || got.TotalShards != 3 || got.DataShards != 2 {
		t.Errorf("%d of %d stored under %d-of-%d, want 2 of 3 under 2-of-3",
			got.Stored, got.TotalShards, got.DataShards, got.TotalShards)
	}
	if !got.Readable || got.Spare != 0 {
		t.Errorf("readable=%v spare=%d, want a readable file with nothing left to spare",
			got.Readable, got.Spare)
	}
	if len(got.Shards) != 2 {
		t.Errorf("carries %d placements, want the 2 that did land", len(got.Shards))
	}
	if page.Unreadable != 0 {
		t.Errorf("Unreadable = %d, want 0 — the file still has its threshold", page.Unreadable)
	}
	if page.Bytes != short.Size {
		t.Errorf("Bytes = %d, want %d — the whole file counts, not the parts left",
			page.Bytes, short.Size)
	}

	// The count in the panel and the list behind it have to be the same set of
	// files, or the number names something the dialog cannot show.
	stats, err := v.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Degraded != page.Total {
		t.Errorf("Stats says %d degraded, the list has %d", stats.Degraded, page.Total)
	}
	if whole.Redundancy() != 3 {
		t.Errorf("the whole file lost a part: %d", whole.Redundancy())
	}
}

func TestDegradedPutsTheWorstFilesFirst(t *testing.T) {
	// Spread over all six accounts, which is 4-of-6: wide enough for "missing
	// one" and "missing two" to be different states rather than both coming to
	// no spare at all.
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	spread := UploadOptions{Accounts: accountIDs(t, v)}

	one, _, err := v.Upload(ctx, MainScope, "/", "one-short.txt", []byte("five of six"), spread)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	two, _, err := v.Upload(ctx, MainScope, "/", "two-short.txt", []byte("four of six"), spread)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	dead, _, err := v.Upload(ctx, MainScope, "/", "unreadable.txt", []byte("three of six"), spread)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	degrade(t, v, one.ID, 1)
	degrade(t, v, two.ID, 2)
	degrade(t, v, dead.ID, 3)

	page, err := v.Degraded(0, 0)
	if err != nil {
		t.Fatalf("Degraded: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("Total = %d, want 3", page.Total)
	}

	want := []string{"/unreadable.txt", "/two-short.txt", "/one-short.txt"}
	for i, path := range want {
		if page.Files[i].Path != path {
			t.Errorf("row %d is %s, want %s — worst first", i, page.Files[i].Path, path)
		}
	}
	if page.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1", page.Unreadable)
	}
	if page.Files[0].Readable {
		t.Error("the file below its threshold reports itself readable")
	}
	if page.Files[2].Spare != 1 {
		t.Errorf("the file missing one of six has %d spare, want 1", page.Files[2].Spare)
	}
}

func TestDegradedPagesWithoutRepeatingOrSkippingAFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	const files = 7
	for i := 0; i < files; i++ {
		entry, _, err := v.Upload(ctx, MainScope, "/",
			fmt.Sprintf("file%02d.txt", i), []byte("short one"), UploadOptions{})
		if err != nil {
			t.Fatalf("Upload %d: %v", i, err)
		}
		degrade(t, v, entry.ID, 1)
	}

	seen := map[string]bool{}
	for offset := 0; offset < files; offset += 3 {
		page, err := v.Degraded(offset, 3)
		if err != nil {
			t.Fatalf("Degraded(%d): %v", offset, err)
		}
		if page.Total != files {
			t.Errorf("page at %d says %d in total, want %d", offset, page.Total, files)
		}
		if page.Offset != offset || page.Limit != 3 {
			t.Errorf("page reports offset %d limit %d, want %d and 3", page.Offset, page.Limit, offset)
		}
		want := 3
		if remaining := files - offset; remaining < want {
			want = remaining
		}
		if len(page.Files) != want {
			t.Errorf("page at %d holds %d rows, want %d", offset, len(page.Files), want)
		}
		for _, f := range page.Files {
			if seen[f.ID] {
				t.Errorf("%s appears on two pages", f.Path)
			}
			seen[f.ID] = true
		}
	}
	if len(seen) != files {
		t.Errorf("paging showed %d files, want all %d", len(seen), files)
	}

	// Past the end is an empty page rather than an error: a file repaired while
	// the dialog is open is allowed to shorten the list under it.
	page, err := v.Degraded(1000, 3)
	if err != nil {
		t.Fatalf("Degraded past the end: %v", err)
	}
	if len(page.Files) != 0 || page.Total != files {
		t.Errorf("past the end: %d rows of %d, want 0 of %d", len(page.Files), page.Total, files)
	}
}

func TestDegradedHoldsItsPageSizeToTheCeiling(t *testing.T) {
	v, _ := newTestVault(t, 3)

	page, err := v.Degraded(0, 0)
	if err != nil {
		t.Fatalf("Degraded: %v", err)
	}
	if page.Limit != DefaultDegradedPage {
		t.Errorf("no limit asked for gave %d, want the default %d", page.Limit, DefaultDegradedPage)
	}

	page, err = v.Degraded(-5, MaxDegradedPage*10)
	if err != nil {
		t.Fatalf("Degraded: %v", err)
	}
	if page.Limit != MaxDegradedPage {
		t.Errorf("Limit = %d, want it trimmed to %d", page.Limit, MaxDegradedPage)
	}
	if page.Offset != 0 {
		t.Errorf("Offset = %d, want a negative offset read as the start", page.Offset)
	}
}

func TestDegradedNeedsAnUnlockedVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.Lock()
	if _, err := v.Degraded(0, 0); err != ErrLocked {
		t.Errorf("Degraded on a locked vault: %v, want ErrLocked", err)
	}
}
