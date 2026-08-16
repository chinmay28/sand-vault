package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// recoveredVault plays the disaster out and returns the vault that took the
// files over, plus the backup the dead one left behind — saved off an account
// before the recovery replaces it, which is exactly the copy somebody could
// have taken at any point while the old vault was alive.
func recoveredVault(t *testing.T, roots []string) (*Vault, []byte) {
	t.Helper()
	ctx := context.Background()

	stolen, err := os.ReadFile(filepath.Join(roots[0], BackupKey))
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}

	fresh := reconnect(t, "the replacement password", roots)
	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(ctx, accounts[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}
	if _, err := fresh.Recover(ctx, snapshot, false); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return fresh, stolen
}

// storedObjects lists every part key sitting on the given account folders.
func storedObjects(t *testing.T, roots []string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("reading %s: %v", root, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), shardSuffix) && e.Name() != BackupKey {
				out[e.Name()] = true
			}
		}
	}
	return out
}

func TestRecoveryLeavesTheOldKeyOpeningTheParts(t *testing.T) {
	roots := storedVault(t)
	fresh, stolen := recoveredVault(t, roots)
	t.Cleanup(fresh.AwaitBackupSync)

	// The state a recovery deliberately leaves: the vault opens under its own
	// password, and the parts on the accounts are still on the dead vault's
	// key. This is not a bug — that key is the only thing that opens them —
	// but it is why reclaiming exists, so it is worth pinning down.
	old, err := OpenBackup(stolen, testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	if fresh.dataKeyID != old.KeyID {
		t.Fatalf("the recovered vault is on generation %q, want the lost vault's %q",
			fresh.dataKeyID, old.KeyID)
	}

	// And the old backup still names objects that are really there, which is
	// what makes it worth anything to whoever holds it.
	live := storedObjects(t, roots)
	for _, entry := range old.Manifest.Entries {
		for _, shard := range entry.Shards {
			if !live[shard.Key] {
				t.Errorf("%s: %s is already gone", entry.Path(), shard.Key)
			}
		}
	}
}

func TestReclaimTakesTheFilesOffTheDeadVaultsKey(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)
	fresh, stolen := recoveredVault(t, roots)
	t.Cleanup(fresh.AwaitBackupSync)

	old, err := OpenBackup(stolen, testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	before := fresh.dataKeyID

	report, err := fresh.Reclaim(ctx, nil, nil)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if report.Migrated != 2 || !report.Done() {
		t.Fatalf("report = %+v, want both files moved and nothing left", report.MigrationReport)
	}
	if report.KeyID == before || report.KeyID == old.KeyID {
		t.Fatalf("the vault is still on generation %q", report.KeyID)
	}
	if pending := fresh.PendingMigration(); pending != 0 {
		t.Errorf("%d file(s) are still on an older key", pending)
	}

	// The point of the whole exercise: every object the old backup pointed at
	// is gone, so the key it carries opens nothing that is still there.
	live := storedObjects(t, roots)
	for _, entry := range old.Manifest.Entries {
		for _, shard := range entry.Shards {
			if live[shard.Key] {
				t.Errorf("%s: %s is still on the accounts under the old key",
					entry.Path(), shard.Key)
			}
		}
	}
	if len(live) == 0 {
		t.Fatal("the files were re-encrypted to nowhere")
	}

	// The retired generation is dropped once nothing names it, so the vault
	// stops carrying the dead one's key at all.
	fresh.mu.RLock()
	retired := len(fresh.retired)
	fresh.mu.RUnlock()
	if retired != 0 {
		t.Errorf("the vault still holds %d retired key(s)", retired)
	}

	// And the files still open, which is the part none of this may cost.
	listing, err := fresh.List("/work")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _, err := fresh.Fetch(ctx, listing.Files[0].ID)
	if err != nil {
		t.Fatalf("Fetch after reclaiming: %v", err)
	}
	if string(got) != "the quarterly numbers" {
		t.Fatalf("reclaimed file = %q", got)
	}
}

// The vault has to keep saying that its key is not its own, because the moment
// to act on that is rarely the moment the recovery finishes — the transfer is
// the whole vault twice over, and it waits for the network you want.
func TestAnInheritedKeyIsReportedUntilItIsReplaced(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)

	before, _ := newTestVault(t, 2)
	if stats, err := before.Stats(); err != nil || stats.InheritedKey {
		t.Errorf("a vault that minted its own key should not report an inherited one (%v)", err)
	}

	fresh, _ := recoveredVault(t, roots)
	t.Cleanup(fresh.AwaitBackupSync)

	stats, err := fresh.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.InheritedKey {
		t.Fatal("a recovered vault is on the dead vault's key and should say so")
	}

	// It has to survive a lock and unlock: this is a property of the vault
	// file, not of the process that happened to run the recovery.
	fresh.Lock()
	if err := fresh.Unlock("the replacement password"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if stats, err := fresh.Stats(); err != nil || !stats.InheritedKey {
		t.Errorf("reopening lost the warning (%v)", err)
	}

	if _, err := fresh.Reclaim(ctx, nil, nil); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if stats, err := fresh.Stats(); err != nil || stats.InheritedKey {
		t.Errorf("the key is this vault's own now, so the warning should be gone (%v)", err)
	}

	// And that too has to be on disk rather than in memory.
	fresh.Lock()
	if err := fresh.Unlock("the replacement password"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if stats, err := fresh.Stats(); err != nil || stats.InheritedKey {
		t.Errorf("reopening brought the warning back (%v)", err)
	}
}

func TestReclaimPutsTheFilesOnTheCloudsChosen(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)
	fresh, _ := recoveredVault(t, roots)
	t.Cleanup(fresh.AwaitBackupSync)

	// A cloud the dead vault never used, which is the case this is for: the
	// accounts a recovery lands on are the ones somebody else chose.
	elsewhere := t.TempDir()
	added, err := fresh.AddProvider(ctx, provider.Config{
		Kind:    provider.KindLocal,
		Name:    "somewhere-new",
		Options: map[string]string{"path": elsewhere},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	// The new account and two of the reconnected ones — so one of the original
	// three is deliberately left out.
	chosen := []string{added.ID}
	for _, cfg := range accounts {
		if cfg.ID != added.ID && len(chosen) < 3 {
			chosen = append(chosen, cfg.ID)
		}
	}
	dropped := ""
	for _, cfg := range accounts {
		if !contains(chosen, cfg.ID) {
			dropped = cfg.ID
		}
	}
	if dropped == "" {
		t.Fatal("the test meant to leave one account out")
	}

	report, err := fresh.Reclaim(ctx, chosen, nil)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if !report.Done() {
		t.Fatalf("report = %+v, want every file moved", report.MigrationReport)
	}

	listing, err := fresh.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	entries := append(listing.Files, mustList(t, fresh, "/work").Files...)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files, got %d", len(entries))
	}
	for _, entry := range entries {
		if len(entry.Shards) != 3 {
			t.Errorf("%s has %d parts, want a full set on the chosen clouds",
				entry.Path(), len(entry.Shards))
		}
		for _, shard := range entry.Shards {
			if !contains(chosen, shard.ProviderID) {
				t.Errorf("%s: part %d landed on %s, which was not chosen",
					entry.Path(), shard.Part, shard.ProviderName)
			}
			if shard.ProviderID == dropped {
				t.Errorf("%s: part %d is still on the account left out", entry.Path(), shard.Part)
			}
		}
	}

	// The new cloud is really carrying parts, rather than the selection having
	// been quietly ignored.
	if objects := storedObjects(t, []string{elsewhere}); len(objects) == 0 {
		t.Error("nothing was written to the cloud that was chosen")
	}

	// And a file still opens, from clouds it was never stored on before.
	got, _, err := fresh.Fetch(ctx, entries[0].ID)
	if err != nil {
		t.Fatalf("Fetch after moving clouds: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the relocated file came back empty")
	}
}

func TestReclaimRefusesASelectionThatCannotHoldAFile(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)
	fresh, _ := recoveredVault(t, roots)
	t.Cleanup(fresh.AwaitBackupSync)

	accounts, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	before := fresh.dataKeyID

	// One account under the strict policy is not a placement, it is a single
	// point of failure — and it has to be refused before the key rotates,
	// leaving the vault exactly as it was.
	if _, err := fresh.Reclaim(ctx, []string{accounts[0].ID}, nil); err == nil {
		t.Fatal("reclaiming onto a single account should be refused")
	}
	if fresh.dataKeyID != before {
		t.Error("a refused reclaim rotated the key anyway")
	}
	if pending := fresh.PendingMigration(); pending != 0 {
		t.Errorf("a refused reclaim left %d file(s) mid-migration", pending)
	}

	if _, err := fresh.Reclaim(ctx, []string{"not-an-account"}, nil); err == nil {
		t.Fatal("reclaiming onto an unknown account should be refused")
	}
	if fresh.dataKeyID != before {
		t.Error("an unknown account rotated the key anyway")
	}
}

func TestReclaimIsResumableWhenItIsInterrupted(t *testing.T) {
	ctx := context.Background()
	roots := storedVault(t)
	fresh, _ := recoveredVault(t, roots)
	t.Cleanup(fresh.AwaitBackupSync)

	// The key rotates in one write and the files move one at a time, so a
	// reclaim that stops halfway leaves a vault that is readable and knows what
	// is left — the same shape a password change leaves behind.
	stopped, cancel := context.WithCancel(ctx)
	cancel()

	report, err := fresh.Reclaim(stopped, nil, nil)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if report.Migrated != 0 || report.Remaining != 2 {
		t.Fatalf("report = %+v, want nothing moved and everything outstanding",
			report.MigrationReport)
	}

	// Still readable, on the key it has not left yet.
	listing := mustList(t, fresh, "/work")
	if _, _, err := fresh.Fetch(ctx, listing.Files[0].ID); err != nil {
		t.Fatalf("Fetch after an interrupted reclaim: %v", err)
	}

	// And finishing is the ordinary migration, not a second reclaim: rotating
	// again would strand the files that already moved.
	finished, err := fresh.MigrateFiles(ctx, nil)
	if err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	if !finished.Done() || finished.Migrated != 2 {
		t.Fatalf("finishing = %+v, want both files moved", finished)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func mustList(t *testing.T, v *Vault, dir string) *Listing {
	t.Helper()
	listing, err := v.List(dir)
	if err != nil {
		t.Fatalf("List(%s): %v", dir, err)
	}
	return listing
}
