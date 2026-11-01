package vault

import (
	"context"
	"fmt"
	"github.com/chinmay28/sand-vault/internal/crypto"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pushes of the manifest backup are paced — a few seconds apart, so a burst
// of changes is a few pushes — and a suite that uploads, looks at the copy on
// the account and moves on in milliseconds wants them the moment they are
// asked for. So every vault these tests open is unpaced unless the test says
// otherwise. The pacing has tests of its own: backuppace_test.go for the
// arithmetic, and this file for the setting.
func TestMain(m *testing.M) {
	defaultBackupDelay = 0
	// Nearly every test here creates a vault, and a vault's password is
	// stretched at nearly half a second a time in production. Nothing in
	// this package tests the stretch itself — crypto does — so every vault
	// these tests seal is sealed cheaply. See PasswordKDF.
	PasswordKDF = crypto.TestArgon2Params()
	os.Exit(m.Run())
}

// withShippedBackupDelay puts the real default back for one test.
func withShippedBackupDelay(t *testing.T) {
	t.Helper()
	defaultBackupDelay = DefaultBackupDelay
	t.Cleanup(func() { defaultBackupDelay = 0 })
}

func TestBackupDelayDefaultsToFiveSeconds(t *testing.T) {
	withShippedBackupDelay(t)
	v, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Before there is a vault file, and after one is written without a
	// choice in it, the delay is the shipped default.
	if got := v.BackupDelay(); got != 5*time.Second {
		t.Errorf("BackupDelay before init = %s, want 5s", got)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	if got := v.BackupDelay(); got != 5*time.Second {
		t.Errorf("BackupDelay = %s, want 5s", got)
	}
	if got := v.backups.Gap(); got != 5*time.Second {
		t.Errorf("the pacer was told %s, want 5s", got)
	}
	// And the file says nothing, so a build before the setting reads it the
	// same way.
	if v.store.ManifestBackupDelaySeconds != nil {
		t.Errorf("a vault that never chose a delay wrote %d to its file", *v.store.ManifestBackupDelaySeconds)
	}
}

func TestBackupDelaySurvivesALockAndAPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 1)
	if err := v.SetBackupDelay(30 * time.Second); err != nil {
		t.Fatalf("SetBackupDelay: %v", err)
	}
	if got := v.backups.Gap(); got != 30*time.Second {
		t.Fatalf("the pacer was told %s, want 30s", got)
	}

	// Readable while locked: it is in the clear beside the on/off switch.
	v.AwaitBackupSync()
	v.Lock()
	if got := v.BackupDelay(); got != 30*time.Second {
		t.Errorf("BackupDelay while locked = %s, want 30s", got)
	}
	if err := v.SetBackupDelay(time.Second); err != ErrLocked {
		t.Errorf("SetBackupDelay on a locked vault: %v, want ErrLocked", err)
	}
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if got := v.BackupDelay(); got != 30*time.Second {
		t.Errorf("BackupDelay after unlock = %s, want 30s", got)
	}

	// A password change rewrites the vault file from scratch; every setting
	// the user made has to come across.
	if _, err := v.ChangePassword(context.Background(), testPassword, "a new password", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if got := v.BackupDelay(); got != 30*time.Second {
		t.Errorf("BackupDelay after a password change = %s, want 30s", got)
	}
	if got := v.backups.Gap(); got != 30*time.Second {
		t.Errorf("the pacer was told %s after a password change, want 30s", got)
	}
}

func TestBackupDelayRefusesWhatItCannotStore(t *testing.T) {
	v, _ := newTestVault(t, 1)
	for _, bad := range []time.Duration{-time.Second, MaxBackupDelay + time.Second, 1500 * time.Millisecond} {
		if err := v.SetBackupDelay(bad); err == nil {
			t.Errorf("SetBackupDelay(%s) was accepted", bad)
		}
	}
	if got := v.BackupDelay(); got != 0 {
		t.Errorf("a refused delay changed the setting to %s", got)
	}
	for _, good := range []time.Duration{0, time.Second, MaxBackupDelay} {
		if err := v.SetBackupDelay(good); err != nil {
			t.Errorf("SetBackupDelay(%s): %v", good, err)
		}
	}
}

// With a delay in force, a run of index changes is a run of requests the
// pacer folds together — and the copy that lands at the end of it is the
// index as it stands then, not as it stood when the first change asked.
func TestBackupDelayFoldsABurstIntoOneCurrentCopy(t *testing.T) {
	v, roots := newTestVault(t, 3)
	v.AwaitBackupSync()
	if err := v.SetBackupDelay(MaxBackupDelay); err != nil {
		t.Fatalf("SetBackupDelay: %v", err)
	}
	// The save above was itself a push, paced like any other; let it clear so
	// the burst below starts inside the gap.
	v.AwaitBackupSync()

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("file-%d.txt", i)
		if _, _, err := v.Upload(context.Background(), MainScope, "/", name, []byte(name), UploadOptions{}); err != nil {
			t.Fatalf("Upload %s: %v", name, err)
		}
	}
	// Nothing has run: the burst is waiting out an hour, as one push.
	v.backups.mu.Lock()
	running, armed := v.backups.running, v.backups.timer != nil
	v.backups.mu.Unlock()
	if running || !armed {
		t.Fatalf("after a burst inside the gap: running=%v armed=%v, want one push waiting", running, armed)
	}

	// Await brings it forward, and what lands names every file of the burst.
	v.AwaitBackupSync()
	snapshot, err := OpenBackup(readBackup(t, roots[0]), testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	if len(snapshot.Manifest.Entries) != 5 {
		t.Errorf("the copy names %d files, want all 5 of the burst", len(snapshot.Manifest.Entries))
	}
}

// A recovery kit carries every setting the user made, and this is one.
func TestBackupDelayTravelsInARecoveryKit(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if err := v.SetBackupDelay(2 * time.Minute); err != nil {
		t.Fatalf("SetBackupDelay: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	if _, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"}); err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	if got := restored.BackupDelay(); got != 2*time.Minute {
		t.Errorf("the restored vault's delay is %s, want the 2m the kit carried", got)
	}
	if got := restored.backups.Gap(); got != 2*time.Minute {
		t.Errorf("the restored vault's pacer was told %s, want 2m", got)
	}
}
