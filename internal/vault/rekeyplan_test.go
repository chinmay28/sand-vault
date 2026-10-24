package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestAFullSpeedPlanMovesEverythingWithoutAMeter(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/Photos", "big.jpg", 3000)
	uploadSized(t, v, "/", "notes.txt", 200)

	// A budget too small to move a byte a day: the fresh meter lets the first
	// file start, and the charge for it puts the second out of reach.
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "fast", &RekeySettings{BytesPerDay: 1}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	now := time.Now()
	step, err := v.RekeyStep(ctx, now)
	if err != nil || !step.Moved || step.Path != "/notes.txt" {
		t.Fatalf("first step = %+v, %v; want the smallest file moved", step, err)
	}
	step, err = v.RekeyStep(ctx, now)
	if err != nil || step.Moved || step.Idle || step.Wait <= 0 {
		t.Fatalf("second step = %+v, %v; want a wait on the meter", step, err)
	}

	// Finish now, as a plan: full speed over the same budget. The meter is
	// out of the way, and the status says so without a wait or an ETA.
	status, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1, FullSpeed: true})
	if err != nil {
		t.Fatalf("SetRekeyPlan: %v", err)
	}
	if !status.Plan.FullSpeed || !status.Plan.Active() || status.NextInSeconds != 0 || status.ETASeconds != 0 || status.Idle {
		t.Fatalf("status at full speed = %+v, plan %+v; want active, nothing to wait for", status, status.Plan)
	}

	// A budget is a pace, and takes the plan off full speed; the debt the
	// meter was carrying is still there, so the file waits again.
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1}); err != nil {
		t.Fatalf("SetRekeyPlan(paced): %v", err)
	}
	step, err = v.RekeyStep(ctx, now)
	if err != nil || step.Moved || step.Idle || step.Wait <= 0 {
		t.Fatalf("paced step = %+v, %v; want a wait on the meter", step, err)
	}

	// At full speed it moves at once, debt or no debt, and the plan and the
	// previous key go with the last file.
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1, FullSpeed: true}); err != nil {
		t.Fatalf("SetRekeyPlan(full speed): %v", err)
	}
	step, err = v.RekeyStep(ctx, now)
	if err != nil || !step.Moved || step.Path != "/Photos/big.jpg" || step.Wait != 0 {
		t.Fatalf("full-speed step = %+v, %v; want big.jpg moved at once", step, err)
	}
	if step, err := v.RekeyStep(ctx, now); err != nil || !step.Idle {
		t.Fatalf("step after the last file = %+v, %v; want idle", step, err)
	}
	if v.PendingMigration() != 0 || retiredCount(v) != 0 {
		t.Errorf("%d file(s) pending, %d retired key(s); want none", v.PendingMigration(), retiredCount(v))
	}
	if status, _ := v.RekeyStatus(); status.Plan != nil || !status.Done() {
		t.Errorf("status after finishing = %+v, want no plan and nothing pending", status)
	}

	fresh := reopenAs(t, v, "fast")
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

// A drive unplugged for the afternoon must not cost the plan a download, an
// upload and an hour's skip per file it holds a part of. Once a health check
// has said the account is down, the files on it wait for it; the others go
// on moving, and the status says what is waiting and for whom.
func TestRekeyStepMovesAFileItCanRebuildPastAnAccountThatIsDown(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	// The smaller file is on the account about to go dark; the larger is not.
	// Smallest first picks the small one, so it is the first step that shows
	// the move past the dark account.
	moving, _, err := v.Upload(ctx, MainScope, "/", "moving.txt", []byte(strings.Repeat("h", 50)), UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(ctx, MainScope, "/", "free.txt", []byte(strings.Repeat("f", 100)), UploadOptions{Accounts: ids[1:]}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	var oldPart Shard
	for _, s := range moving.Shards {
		if s.ProviderID == ids[0] {
			oldPart = s
		}
	}
	if oldPart.Key == "" {
		t.Fatal("moving.txt has no part on cloud-a")
	}

	// The first account goes dark, still connected, and the check notices.
	kept := filepath.Join(t.TempDir(), "away")
	if err := os.Rename(roots[0], kept); err != nil {
		t.Fatalf("moving the account's folder away: %v", err)
	}
	if err := os.WriteFile(roots[0], []byte("not a folder"), 0o600); err != nil {
		t.Fatalf("blocking the account's folder: %v", err)
	}
	if report, err := v.CheckClouds(ctx); err != nil || report.Unhealthy != 1 {
		t.Fatalf("CheckClouds = %+v, %v; want one account down", report, err)
	}

	// A 2-of-3 file with two parts on accounts that answer can be rebuilt, so
	// it is not held: it moves without cloud-a, and the fourth account takes
	// the part cloud-a cannot.
	now := time.Now()
	step, err := v.RekeyStep(ctx, now)
	if err != nil || !step.Moved || step.Path != "/moving.txt" {
		t.Fatalf("first step = %+v, %v; want moving.txt re-encrypted past the dark account", step, err)
	}
	if !strings.Contains(step.Warning, "stray part") || !strings.Contains(step.Warning, "cloud-a") {
		t.Errorf("step.Warning = %q, want the part left on cloud-a named as a stray", step.Warning)
	}
	moved, _ := v.Entry(moving.ID)
	if len(moved.Shards) != 3 || moved.ArchiveID == moving.ArchiveID {
		t.Fatalf("moving.txt has %d parts under archive %s, want 3 fresh parts", len(moved.Shards), moved.ArchiveID)
	}
	for _, s := range moved.Shards {
		if s.ProviderID == ids[0] {
			t.Errorf("a fresh part was placed on the dark account: %+v", s)
		}
	}
	if _, _, err := v.Fetch(ctx, moving.ID); err != nil {
		t.Errorf("the moved file does not read: %v", err)
	}
	// The part left on cloud-a is still there, under the previous key, and the
	// index knows it.
	if _, err := os.Stat(filepath.Join(kept, oldPart.Key)); err != nil {
		t.Errorf("the old part was expected to be left where it was: %v", err)
	}
	stale, err := v.StaleParts()
	if err != nil {
		t.Fatalf("StaleParts: %v", err)
	}
	if stale.Parts != 1 || len(stale.Accounts) != 1 || stale.Accounts[0].Name != "cloud-a" || !stale.Accounts[0].Unreachable {
		t.Fatalf("StaleParts = %+v, want one part on cloud-a, unreachable", stale)
	}
	status, err := v.RekeyStatus()
	if err != nil {
		t.Fatalf("RekeyStatus: %v", err)
	}
	if status.Held != 0 || status.StaleParts == nil || status.StaleParts.Parts != 1 {
		t.Errorf("status holds %d with stale %+v; want nothing held and one stray part on record", status.Held, status.StaleParts)
	}
	if joined := strings.Join(status.Warnings, "\n"); !strings.Contains(joined, "previous key") || !strings.Contains(joined, "cloud-a") {
		t.Errorf("status.Warnings = %v, want the stray part explained by account", status.Warnings)
	}

	// The other file never touched cloud-a and moves as it always did; nothing
	// is left pending, and the previous key goes with the last file.
	if step, err := v.RekeyStep(ctx, now); err != nil || !step.Moved || step.Path != "/free.txt" {
		t.Fatalf("second step = %+v, %v; want free.txt moved", step, err)
	}
	if v.PendingMigration() != 0 {
		t.Error("files still pending after everything moved")
	}
	if retiredCount(v) != 0 {
		t.Error("the previous key outlived the last file on it; a recorded stray part must not keep it")
	}
	stats, _ := v.Stats()
	if stats.StaleParts != 1 || stats.StaleBytes != oldPart.Size {
		t.Errorf("Stats counts %d stray part(s) of %d bytes, want 1 of %d", stats.StaleParts, stats.StaleBytes, oldPart.Size)
	}
}

func TestRekeyStepHoldsAFileItCannotRebuildWithoutTheAccount(t *testing.T) {
	// Two accounts under the strict policy is two parts per file and both
	// needed, so a file with one of them on a dark account cannot be read.
	v, roots := newTestVault(t, 2)
	ctx := context.Background()
	held := uploadSized(t, v, "/", "held.txt", 50)
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}

	kept := filepath.Join(t.TempDir(), "away")
	if err := os.Rename(roots[0], kept); err != nil {
		t.Fatalf("moving the account's folder away: %v", err)
	}
	if err := os.WriteFile(roots[0], []byte("not a folder"), 0o600); err != nil {
		t.Fatalf("blocking the account's folder: %v", err)
	}
	if report, err := v.CheckClouds(ctx); err != nil || report.Unhealthy != 1 {
		t.Fatalf("CheckClouds = %+v, %v; want one account down", report, err)
	}

	// Not tried: no warning, no hour-long skip, just a wait short enough that
	// the check finding the account back is acted on.
	now := time.Now()
	step, err := v.RekeyStep(ctx, now)
	if err != nil || step.Moved || step.Idle || step.Warning != "" || step.Wait != MinHealthInterval {
		t.Fatalf("step with only the held file = %+v, %v; want a quiet wait of %s", step, err, MinHealthInterval)
	}
	v.rekeyMu.Lock()
	skipped := len(v.rekey.skip)
	v.rekeyMu.Unlock()
	if skipped != 0 {
		t.Errorf("%d file(s) were skipped for an hour; a held file must not be charged a failure", skipped)
	}
	status, err := v.RekeyStatus()
	if err != nil {
		t.Fatalf("RekeyStatus: %v", err)
	}
	if status.Held != 1 || len(status.HeldOn) != 1 || status.HeldOn[0] != "cloud-a" {
		t.Errorf("status holds %d on %v, want 1 on cloud-a", status.Held, status.HeldOn)
	}
	if status.Idle || status.Pending != 1 || status.StaleParts != nil {
		t.Errorf("status = idle %v, pending %d, stale %+v; want not idle, the held file pending, nothing left behind",
			status.Idle, status.Pending, status.StaleParts)
	}
	if joined := strings.Join(status.Warnings, "\n"); !strings.Contains(joined, "cloud-a") || !strings.Contains(joined, "health check") {
		t.Errorf("status.Warnings = %v, want the hold explained by name", status.Warnings)
	}

	// The account back and checked, the held file moves on the next step,
	// with every part it had and nothing left behind.
	if err := os.Remove(roots[0]); err != nil {
		t.Fatalf("unblocking: %v", err)
	}
	if err := os.Rename(kept, roots[0]); err != nil {
		t.Fatalf("restoring the account's folder: %v", err)
	}
	if report, err := v.CheckClouds(ctx); err != nil || report.Unhealthy != 0 {
		t.Fatalf("CheckClouds after the restore = %+v, %v; want every account up", report, err)
	}
	step, err = v.RekeyStep(ctx, now)
	if err != nil || !step.Moved || step.Path != "/held.txt" {
		t.Fatalf("step after the account is back = %+v, %v; want held.txt moved", step, err)
	}
	moved, _ := v.Entry(held.ID)
	if len(moved.Shards) != 2 {
		t.Errorf("held.txt re-encrypted with %d parts, want 2", len(moved.Shards))
	}
	if v.PendingMigration() != 0 {
		t.Error("files still pending after everything moved")
	}
	status, _ = v.RekeyStatus()
	if status.Held != 0 || len(status.HeldOn) != 0 || status.StaleParts != nil {
		t.Errorf("status still holds %d on %v with stale %+v after everything moved", status.Held, status.HeldOn, status.StaleParts)
	}
	for _, root := range roots {
		if n := objectsIn(t, root); n != 1 {
			t.Errorf("%s holds %d objects after the move, want its one part", root, n)
		}
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
