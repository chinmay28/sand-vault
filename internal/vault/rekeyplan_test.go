package vault

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// reopenAs is reopen for a vault whose password has changed.
func reopenAs(t *testing.T, v *Vault, password string) *Vault {
	t.Helper()
	v.AwaitBackupSync()
	v.Lock()
	fresh, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fresh.Unlock(password); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(fresh.AwaitBackupSync)
	t.Cleanup(fresh.AwaitReadHistory)
	return fresh
}

// uploadSized stores a file of n bytes in dir.
func uploadSized(t *testing.T, v *Vault, dir, name string, n int) *Entry {
	t.Helper()
	payload := []byte(strings.Repeat("x", n))
	if dir != "/" {
		if err := v.Mkdir(MainScope, dir); err != nil && !strings.Contains(err.Error(), "exists") {
			t.Fatalf("Mkdir %s: %v", dir, err)
		}
	}
	entry, _, err := v.Upload(context.Background(), MainScope, dir, name, payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload %s/%s: %v", dir, name, err)
	}
	return entry
}

func TestASecondPasswordChangeIsRefusedWhileFilesAreStillMoving(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/", "waiting.txt", 100)

	if _, err := v.ChangePassword(ctx, testPassword, "the second password", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// Open, and refused before anything is touched.
	_, err := v.ChangePassword(ctx, "the second password", "a third", true)
	var pending *RekeyPendingError
	if !errors.As(err, &pending) || !errors.Is(err, ErrRekeyPending) {
		t.Fatalf("second change = %v, want ErrRekeyPending", err)
	}
	if pending.Files != 1 || pending.Bytes != 100 {
		t.Errorf("refusal counted %d file(s), %d bytes; want 1 and 100", pending.Files, pending.Bytes)
	}

	// Locked, where the open index is not there to ask, the file is.
	locked, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := locked.ChangePassword(ctx, "the second password", "a third", true); !errors.Is(err, ErrRekeyPending) {
		t.Fatalf("second change on a locked vault = %v, want ErrRekeyPending", err)
	}
	// And the refusal left the vault as it was: the second password opens it.
	if err := locked.Unlock("the second password"); err != nil {
		t.Fatalf("Unlock after a refused change: %v", err)
	}
	locked.Lock()

	// Finishing the first change is what lifts the refusal.
	if _, err := v.MigrateFiles(ctx, nil); err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if _, err := v.ChangePassword(ctx, "the second password", "a third", true); err != nil {
		t.Fatalf("change after finishing: %v", err)
	}
}

func TestASubVaultPasswordChangeIsRefusedWhileItsFilesAreStillMoving(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)
	ctx := context.Background()

	if _, _, err := v.Upload(ctx, Scope(id), "/", "p60.pdf", []byte("private"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.ChangeSubVaultPassword(ctx, id, subPassword, "second", false); err != nil {
		t.Fatalf("ChangeSubVaultPassword: %v", err)
	}
	if _, err := v.ChangeSubVaultPassword(ctx, id, "second", "third", true); !errors.Is(err, ErrRekeyPending) {
		t.Fatalf("second sub vault change = %v, want ErrRekeyPending", err)
	}
	if _, err := v.MigrateFilesIn(ctx, Scope(id), nil, nil); err != nil {
		t.Fatalf("MigrateFilesIn: %v", err)
	}
	if _, err := v.ChangeSubVaultPassword(ctx, id, "second", "third", true); err != nil {
		t.Fatalf("change after finishing: %v", err)
	}
}

func TestAPlannedPasswordChangeMovesFilesOnTheMeter(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/Photos", "big.jpg", 3000)
	uploadSized(t, v, "/Photos", "small.jpg", 1000)
	uploadSized(t, v, "/Taxes", "p60.pdf", 500)
	uploadSized(t, v, "/", "notes.txt", 200)

	// Enough budget that nothing waits, with Taxes to go first.
	report, err := v.ChangePasswordWithPlan(ctx, testPassword, "paced", &RekeySettings{
		BytesPerDay: 1 << 40,
		Folders:     []string{"Taxes/"},
	})
	if err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	if report.Remaining != 4 {
		t.Fatalf("report = %+v, want all four files left for the plan", report)
	}

	status, err := v.RekeyStatus()
	if err != nil {
		t.Fatalf("RekeyStatus: %v", err)
	}
	if status.Plan == nil || status.Plan.Folders[0] != "/Taxes" {
		t.Fatalf("plan = %+v, want Taxes queued as a clean path", status.Plan)
	}
	if status.Pending != 4 || status.Total != 4 || status.TotalBytes != 4700 {
		t.Errorf("status = pending %d of %d (%d bytes), want 4 of 4 (4700)", status.Pending, status.Total, status.TotalBytes)
	}
	if len(status.Folders) != 3 || !status.Folders[0].Queued || status.Folders[0].Path != "/Taxes" ||
		status.Folders[1].Path != "/Photos" || status.Folders[2].Path != "/" {
		t.Errorf("folders = %+v, want Taxes first then Photos and the root by size", status.Folders)
	}

	now := time.Now()
	// Taxes first, as asked; then smallest first across the rest.
	for i, want := range []string{"/Taxes/p60.pdf", "/notes.txt", "/Photos/small.jpg", "/Photos/big.jpg"} {
		step, err := v.RekeyStep(ctx, now)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !step.Moved || step.Path != want {
			t.Fatalf("step %d = %+v, want %s moved", i, step, want)
		}
	}

	step, err := v.RekeyStep(ctx, now)
	if err != nil || !step.Idle {
		t.Fatalf("step after the last file = %+v, %v; want idle", step, err)
	}
	if v.PendingMigration() != 0 {
		t.Errorf("%d file(s) still pending", v.PendingMigration())
	}
	if retiredCount(v) != 0 {
		t.Error("the previous key outlived the last file on it")
	}
	// The plan went with the last file.
	status, _ = v.RekeyStatus()
	if status.Plan != nil || !status.Done() {
		t.Errorf("status after finishing = %+v, want no plan and nothing pending", status)
	}

	// Everything still reads under the new password.
	fresh := reopenAs(t, v, "paced")
	for _, e := range fresh.manifest.Entries {
		if _, _, err := fresh.Fetch(ctx, e.ID); err != nil {
			t.Errorf("Fetch %s: %v", e.Path(), err)
		}
	}
}

func TestRekeyStepWaitsForTheMeter(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/", "a.txt", 1000)
	uploadSized(t, v, "/", "b.txt", 1000)

	// 24 bytes a day is a burst of one byte: the first file starts on it and
	// leaves a debt of 999 bytes, which at a byte an hour is a long wait.
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "slow", &RekeySettings{BytesPerDay: 24}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

	first, err := v.RekeyStep(ctx, now)
	if err != nil || !first.Moved {
		t.Fatalf("first step = %+v, %v; want a file moved", first, err)
	}
	second, err := v.RekeyStep(ctx, now)
	if err != nil || second.Moved || second.Idle {
		t.Fatalf("second step = %+v, %v; want a wait", second, err)
	}
	if second.Wait < 40*day || second.Wait > 42*day {
		t.Errorf("wait = %v, want about 41 days of debt", second.Wait)
	}

	status, _ := v.RekeyStatus()
	if status.NextInSeconds == 0 || status.ETASeconds == 0 {
		t.Errorf("status = next in %ds, eta %ds; want both set while waiting", status.NextInSeconds, status.ETASeconds)
	}
	if status.Idle {
		t.Error("a plan that is waiting is not idle")
	}

	// The charge survives a restart: the file on disk carries the debt.
	fresh := reopenAs(t, v, "slow")
	again, err := fresh.RekeyStep(ctx, now)
	if err != nil || again.Moved {
		t.Fatalf("step after reopening = %+v, %v; want still waiting", again, err)
	}
	// And once the debt is paid, the second file goes.
	later, err := fresh.RekeyStep(ctx, now.Add(second.Wait+time.Minute))
	if err != nil || !later.Moved {
		t.Fatalf("step after the wait = %+v, %v; want the second file moved", later, err)
	}
}

func TestAPlanThatStopsAtItsFoldersGoesIdle(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/Taxes", "p60.pdf", 500)
	uploadSized(t, v, "/Photos", "a.jpg", 500)

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "narrow", &RekeySettings{
		BytesPerDay: 1 << 40, Folders: []string{"/Taxes"}, OnlyFolders: true,
	}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	now := time.Now()
	if step, _ := v.RekeyStep(ctx, now); !step.Moved || step.Path != "/Taxes/p60.pdf" {
		t.Fatalf("first step = %+v, want the tax return moved", step)
	}
	step, _ := v.RekeyStep(ctx, now)
	if !step.Idle {
		t.Fatalf("step with only Taxes allowed = %+v, want idle", step)
	}
	status, _ := v.RekeyStatus()
	if !status.Idle || status.Pending != 1 {
		t.Errorf("status = idle %v, pending %d; want idle with one file left", status.Idle, status.Pending)
	}
	if !status.Plan.OnlyFolders {
		t.Error("the plan lost its OnlyFolders")
	}

	// Widening the plan picks the rest up.
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1 << 40, Folders: []string{"/Taxes"}}); err != nil {
		t.Fatalf("SetRekeyPlan: %v", err)
	}
	if step, _ := v.RekeyStep(ctx, now); !step.Moved || step.Path != "/Photos/a.jpg" {
		t.Fatalf("step after widening = %+v, want the photo moved", step)
	}
}

func TestPausingAPlanHoldsIt(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/", "a.txt", 100)

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "paused", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1 << 40, Paused: true}); err != nil {
		t.Fatalf("SetRekeyPlan: %v", err)
	}
	if step, _ := v.RekeyStep(ctx, time.Now()); step.Moved || !step.Idle {
		t.Fatalf("step while paused = %+v, want idle and nothing moved", step)
	}
	// A plan with no budget is the same hold under another name.
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 0}); err != nil {
		t.Fatalf("SetRekeyPlan: %v", err)
	}
	if step, _ := v.RekeyStep(ctx, time.Now()); step.Moved {
		t.Fatal("a plan with no budget moved a file")
	}
	// Finish-now still works through either.
	if report, err := v.MigrateFiles(ctx, nil); err != nil || !report.Done() {
		t.Fatalf("MigrateFiles = %+v, %v", report, err)
	}
}

func TestSetRekeyPlanChecksItsFoldersAndNeedsSomethingPending(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1}); err == nil {
		t.Error("a plan was accepted with nothing on the previous key")
	}

	uploadSized(t, v, "/Photos", "a.jpg", 100)
	if _, err := v.ChangePassword(ctx, testPassword, "later", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1, Folders: []string{"/Nowhere"}}); err == nil {
		t.Error("a plan was accepted naming a folder that does not exist")
	}
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: -1}); err == nil {
		t.Error("a negative budget was accepted")
	}
	status, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1 << 30, Folders: []string{"Photos", "/Photos/"}})
	if err != nil {
		t.Fatalf("SetRekeyPlan: %v", err)
	}
	if len(status.Plan.Folders) != 1 || status.Plan.Folders[0] != "/Photos" {
		t.Errorf("folders = %v, want the one folder, cleaned and deduplicated", status.Plan.Folders)
	}
	if status.Plan.Files != 1 || status.Plan.Bytes != 100 {
		t.Errorf("plan began over %d file(s), %d bytes; want 1 and 100", status.Plan.Files, status.Plan.Bytes)
	}
	// A planned change over an empty vault has nothing to plan and keeps none.
	if _, err := v.MigrateFiles(ctx, nil); err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if _, err := v.Delete(ctx, v.manifest.Entries[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.ChangePasswordWithPlan(ctx, "later", "empty", &RekeySettings{BytesPerDay: 1}); err != nil {
		t.Fatalf("ChangePasswordWithPlan on an empty vault: %v", err)
	}
	if status, _ := v.RekeyStatus(); status.Plan != nil {
		t.Errorf("an empty vault kept a plan: %+v", status.Plan)
	}
}

func TestRekeyStepLeavesAFileItCannotReadAloneForAWhile(t *testing.T) {
	v, roots := newTestVault(t, 2)
	ctx := context.Background()
	fine := uploadSized(t, v, "/", "fine.txt", 100)
	stranded := uploadSized(t, v, "/", "stranded.txt", 50)

	missing := shardPath(roots, stranded.Shards[0].Key)
	saved, err := os.ReadFile(missing)
	if err != nil {
		t.Fatalf("reading the part: %v", err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatalf("removing the part: %v", err)
	}

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	now := time.Now()

	// Smallest first means the stranded file is tried first — and fails.
	step, err := v.RekeyStep(ctx, now)
	if err != nil || step.Moved || step.Warning == "" || !strings.Contains(step.Warning, "/stranded.txt") {
		t.Fatalf("step on the stranded file = %+v, %v; want a warning naming it", step, err)
	}
	// Then the other goes, with the stranded one passed over.
	if step, _ := v.RekeyStep(ctx, now); !step.Moved || step.Path != "/fine.txt" {
		t.Fatalf("next step = %+v, want fine.txt moved", step)
	}
	// And the plan rests rather than retrying every tick.
	if step, _ := v.RekeyStep(ctx, now); step.Moved || step.Idle || step.Wait == 0 {
		t.Fatalf("step with only a skipped file left = %+v, want a wait", step)
	}
	status, _ := v.RekeyStatus()
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "/stranded.txt") {
		t.Errorf("status.Warnings = %v, want the one failure", status.Warnings)
	}
	if _, _, err := v.Fetch(ctx, fine.ID); err != nil {
		t.Errorf("the moved file does not read: %v", err)
	}

	// Its hour up, the file is tried again, and with its part back it moves.
	if err := os.WriteFile(missing, saved, 0600); err != nil {
		t.Fatalf("restoring the part: %v", err)
	}
	later := now.Add(rekeyRetryAfter + time.Minute)
	if step, _ := v.RekeyStep(ctx, later); !step.Moved || step.Path != "/stranded.txt" {
		t.Fatalf("step after the retry window = %+v, want stranded.txt moved", step)
	}
	if v.PendingMigration() != 0 {
		t.Error("files still pending after everything moved")
	}
}

func TestOnlyOneReEncryptionPassRunsAtATime(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/", "a.txt", 100)
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "busy", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}

	// Stand in for a finish-now pass that is mid-file.
	v.migrateMu.Lock()
	if _, err := v.MigrateFiles(ctx, nil); !errors.Is(err, ErrMigrationBusy) {
		t.Errorf("MigrateFiles while busy = %v, want ErrMigrationBusy", err)
	}
	step, err := v.RekeyStep(ctx, time.Now())
	if err != nil || step.Moved || step.Wait == 0 {
		t.Errorf("RekeyStep while busy = %+v, %v; want a short wait", step, err)
	}
	v.migrateMu.Unlock()

	if step, _ := v.RekeyStep(ctx, time.Now()); !step.Moved {
		t.Errorf("RekeyStep once the slot is free = %+v, want the file moved", step)
	}
}

func TestStatsNameTheCurrentKey(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	entry := uploadSized(t, v, "/", "a.txt", 100)

	stats, err := v.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.KeyID == "" || stats.KeyID != entry.KeyID {
		t.Fatalf("Stats.KeyID = %q, want the key the upload was sealed under (%q)", stats.KeyID, entry.KeyID)
	}
	if _, err := v.ChangePassword(ctx, testPassword, "rotated", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	after, _ := v.Stats()
	if after.KeyID == stats.KeyID {
		t.Error("Stats.KeyID did not change with the key")
	}
	// Which is how a file list tells the two apart.
	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if listing.Files[0].KeyID == after.KeyID {
		t.Error("a file still on the previous key reads as current")
	}
}
