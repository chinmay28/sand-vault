package vault

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// partFile is where a file's part sits among the local folders standing in
// for accounts, whichever layout the file is stored in.
func partFile(t *testing.T, roots []string, e *Entry, part int) string {
	t.Helper()
	for _, s := range e.Shards {
		if s.Part != part {
			continue
		}
		key := s.Key
		if e.Chunked() {
			key = ChunkShardKey(e.ArchiveID, 0, part)
		}
		if full := shardPath(roots, key); full != "" {
			return full
		}
	}
	t.Fatalf("part %d of %s is on no account", part, e.Path())
	return ""
}

// truncate leaves a zero-length file where a part was: what a removable disk
// leaves behind when it loses power after accepting the write.
func truncate(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("truncating %s: %v", path, err)
	}
}

func uploadWide(t *testing.T, v *Vault, name string, n int) ([]byte, *Entry) {
	t.Helper()
	payload := []byte(strings.Repeat("photo ", n))
	entry, _, err := v.Upload(context.Background(), MainScope, "/", name, payload, UploadOptions{Scheme: archive.Scheme{Data: 3, Total: 4}})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.Redundancy() != 4 {
		t.Fatalf("uploaded with %d parts, want 4", entry.Redundancy())
	}
	return payload, entry
}

func TestAZeroLengthPartIsMissingNotPresent(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	payload, entry := uploadWide(t, v, "photo.jpg", 400)
	truncate(t, partFile(t, roots, entry, 4))

	// The inspector says the part is not there, and why, and that the file
	// still reads from the other three — with nothing to spare.
	health, err := v.Health(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for _, s := range health.Shards {
		if s.Part == 4 {
			if s.Present || !strings.Contains(s.Error, "truncated") {
				t.Errorf("part 4 = present %v, %q; want absent and called truncated", s.Present, s.Error)
			}
		} else if !s.Present {
			t.Errorf("part %d = %q, want present", s.Part, s.Error)
		}
	}
	if !health.Recoverable || health.Spare != 0 {
		t.Errorf("health = recoverable %v, spare %d; want recoverable with no spare", health.Recoverable, health.Spare)
	}

	// And the degraded list carries it, with the reason.
	page, err := v.Degraded(0, 0)
	if err != nil {
		t.Fatalf("Degraded: %v", err)
	}
	if page.Total != 1 || page.Unreadable != 0 {
		t.Fatalf("degraded = %d file(s), %d unreadable; want the one, readable", page.Total, page.Unreadable)
	}
	row := page.Files[0]
	if row.Stored != 3 || row.Missing != 1 || row.Spare != 0 || !row.Readable {
		t.Errorf("row = stored %d, missing %d, spare %d, readable %v", row.Stored, row.Missing, row.Spare, row.Readable)
	}
	if len(row.Lost) != 1 || row.Lost[0].Part != 4 || !strings.Contains(row.Lost[0].Reason, "0 B") || row.Lost[0].ProviderName == "" {
		t.Errorf("row.Lost = %+v, want part 4 on a named account, 0 B", row.Lost)
	}

	// A repair onto the same clouds writes the part again rather than
	// keeping the zero-length file as "already in place".
	var accounts []string
	for _, s := range entry.Shards {
		accounts = append(accounts, s.ProviderID)
	}
	report, err := v.Relocate(ctx, MainScope, entry.ID, accounts, archive.Scheme{Data: 3, Total: 4}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.Relocated != 1 {
		t.Fatalf("report = %+v, want the file put right", report)
	}
	fixed := v.manifest.ByID(entry.ID)
	if info, err := os.Stat(partFile(t, roots, fixed, 4)); err != nil || info.Size() == 0 {
		t.Errorf("part 4 after the repair: %v, size %d; want it written again", err, info.Size())
	}
	if page, _ := v.Degraded(0, 0); page.Total != 0 {
		t.Errorf("degraded after the repair = %d file(s), want none", page.Total)
	}
	if data, _, err := v.Fetch(ctx, entry.ID); err != nil || string(data) != string(payload) {
		t.Errorf("Fetch after the repair: %v", err)
	}
}

func TestAReadGoesPastACorruptPartToASpare(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	payload, entry := uploadWide(t, v, "photo.jpg", 400)

	// A zero-length part loses the race to a good one, whichever three the
	// race would otherwise have picked.
	truncate(t, partFile(t, roots, entry, 3))
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("Fetch with part 3 truncated: %v", err)
	}

	// Two parts gone is one too many, and the read names both losses — and
	// writes them down, so the degraded list can show them.
	truncate(t, partFile(t, roots, entry, 1))
	if err := os.Remove(partFile(t, roots, entry, 2)); err != nil {
		t.Fatalf("removing part 2: %v", err)
	}
	if _, _, err := v.Fetch(ctx, entry.ID); err == nil || !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Fetch with two parts gone = %v, want both losses named", err)
	}
	page, _ := v.Degraded(0, 0)
	if page.Total != 1 || page.Unreadable != 1 || len(page.Files[0].Lost) < 2 || page.Files[0].Readable {
		t.Errorf("degraded = %+v, want the file unreadable with its losses listed", page)
	}
}

func TestAReEncryptionMovesAFilePastATruncatedPart(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	payload, entry := uploadWide(t, v, "photo.jpg", 400)
	truncate(t, partFile(t, roots, entry, 3))

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{FullSpeed: true}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	step, err := v.RekeyStep(ctx, time.Now())
	if err != nil || !step.Moved || step.Path != "/photo.jpg" {
		t.Fatalf("step = %+v, %v; want the photo moved past its truncated part", step, err)
	}
	moved := v.manifest.ByID(entry.ID)
	if moved.Redundancy() != 4 {
		t.Errorf("re-encrypted with %d parts, want a full set of 4", moved.Redundancy())
	}
	for part := 1; part <= 4; part++ {
		if info, err := os.Stat(partFile(t, roots, moved, part)); err != nil || info.Size() == 0 {
			t.Errorf("part %d after the move: %v", part, err)
		}
	}
	if page, _ := v.Degraded(0, 0); page.Total != 0 {
		t.Errorf("degraded after the move = %d file(s), want none: the loss was of the old copy", page.Total)
	}
	if data, _, err := v.Fetch(ctx, entry.ID); err != nil || string(data) != string(payload) {
		t.Errorf("Fetch after the move: %v", err)
	}
}

func TestAnAutomationSweepRepairsATruncatedPart(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	payload, entry := uploadWide(t, v, "photo.jpg", 400)
	truncate(t, partFile(t, roots, entry, 2))

	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionRebalance)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	run, err := v.RunAutomation(ctx, MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Shards.Short != 1 || run.Shards.Repaired != 1 || run.Shards.Failed != 0 {
		t.Fatalf("run = %+v, want the one short file rebuilt", run.Shards)
	}
	fixed := v.manifest.ByID(entry.ID)
	if info, err := os.Stat(partFile(t, roots, fixed, 2)); err != nil || info.Size() == 0 {
		t.Errorf("part 2 after the sweep: %v; want it written again", err)
	}
	if data, _, err := v.Fetch(ctx, entry.ID); err != nil || string(data) != string(payload) {
		t.Errorf("Fetch after the sweep: %v", err)
	}
}
