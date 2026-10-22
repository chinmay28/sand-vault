package vault

import (
	"context"
	"testing"
)

// The tests in this file are about what a rebuilt index — recovered from a
// backup, imported from a kit, or brought in as a sub vault — keeps of the
// original beyond its files: the key generations only a sub vault points at,
// and the records the index carries about its folders and files.

// lentVault is a vault whose sub vault holds a file still sealed under a main
// generation the main vault has since retired: uploaded to the main vault,
// assigned into the sub vault, and then the main password changed. Only the
// sub vault's borrowed-key list names that generation from outside the sealed
// section. It returns the vault, its roots, the sub vault's id and the new
// main password, with the sub vault locked and the backups current.
func lentVault(t *testing.T) (*Vault, []string, string, string) {
	t.Helper()
	ctx := context.Background()
	source, roots := newTestVault(t, 3)
	id := createSubVault(t, source, "Taxes", subPassword)
	if _, _, err := source.Upload(ctx, MainScope, "/", "stays.txt", []byte("ordinary"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := source.Upload(ctx, MainScope, "/", "moved.txt", []byte("assigned in"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := source.Assign(ctx, MainScope, "/moved.txt", Scope(id), false); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	const newMain = "an entirely new main password"
	if _, err := source.ChangePassword(ctx, testPassword, newMain, true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if err := source.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}
	if _, err := source.SyncManifestBackup(ctx, true); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	source.AwaitBackupSync()
	return source, roots, id, newMain
}

// A retired main generation that only a sub vault's borrowed-key list still
// names has to come back with a recovery, or the sub vault's file opens for
// nobody — silently, because the index naming it cannot be read by the main
// password.
func TestRecoverKeepsAKeyOnlyASubVaultBorrows(t *testing.T) {
	ctx := context.Background()
	_, roots, id, newMain := lentVault(t)
	snapshot, err := OpenBackup(readBackup(t, roots[0]), newMain)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	if len(snapshot.Keys) != 2 {
		t.Fatalf("the backup carries %d key generations, want 2", len(snapshot.Keys))
	}

	target := reconnect(t, "a completely different password", roots)
	if _, err := target.Recover(ctx, snapshot, false); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	target.AwaitBackupSync()
	if retiredCount(target) != 1 {
		t.Errorf("retired = %d after the recovery, want the borrowed generation kept", retiredCount(target))
	}
	if err := target.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	e, err := target.EntryByPath(Scope(id), "/moved.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	data, _, err := target.Fetch(ctx, e.ID)
	if err != nil {
		t.Fatalf("the sub vault's borrowed-key file is unreadable after recovery: %v", err)
	}
	if string(data) != "assigned in" {
		t.Errorf("Fetch = %q", data)
	}
}

// The same generation through a kit import, which installs the index the
// clouds hold.
func TestKitImportKeepsAKeyOnlyASubVaultBorrows(t *testing.T) {
	ctx := context.Background()
	source, _, id, newMain := lentVault(t)
	if err := source.Unlock(newMain); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, source)
	source.AwaitBackupSync()
	source.Lock()

	restored := freshVault(t)
	if _, err := restored.ImportKit(ctx, openTestKit(t, zipped, fingerprint.Code), KitImportOptions{Password: "new password"}); err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	restored.AwaitBackupSync()
	if err := restored.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	e, err := restored.EntryByPath(Scope(id), "/moved.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if _, _, err := restored.Fetch(ctx, e.ID); err != nil {
		t.Fatalf("the sub vault's borrowed-key file is unreadable after the kit import: %v", err)
	}
}

// And through an import as a sub vault: the old main vault becomes a sub
// vault whose retired keys are the old generations, and its own sub vault
// lands beside it, borrowing one of them. The borrowed-key list has to be in
// the index before the first write, or that write prunes the key away.
func TestImportAsSubVaultKeepsAKeyANestedSubVaultBorrows(t *testing.T) {
	ctx := context.Background()
	_, roots, id, newMain := lentVault(t)

	target, _ := newTestVault(t, 3)
	ids := connect(t, target, roots, "reconnected")
	report, err := target.ImportAsSubVault(ctx, ids[0], newMain, ImportOptions{
		Label:    "Old laptop",
		Password: "the imported vault's password",
	})
	if err != nil {
		t.Fatalf("ImportAsSubVault: %v", err)
	}
	target.AwaitBackupSync()

	// The imported vault itself is open; its nested sub vault is a shut
	// sibling with its own password.
	if _, err := target.EntryByPath(Scope(report.SubVault.ID), "/stays.txt"); err != nil {
		t.Fatalf("the imported vault's own file is missing: %v", err)
	}
	if err := target.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	e, err := target.EntryByPath(Scope(id), "/moved.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	// The generation is what this is about. The file's parts are a separate
	// matter: an import does not yet rewrite a nested sibling's shard records
	// onto the reconnected accounts the way a recovery's AccountRemap does,
	// so the key being here is checked directly rather than through a read.
	target.mu.RLock()
	key, err := target.dataKeyForLocked(e.KeyID)
	target.mu.RUnlock()
	if err != nil {
		t.Fatalf("the generation the nested sub vault's file is sealed under was dropped by the import: %v", err)
	}
	if len(key) != DataKeySize {
		t.Fatalf("key is %d bytes", len(key))
	}
}

// indexedVault is a vault whose index says more about its files and folders
// than where the parts are: a standing instruction, a chosen picture, a film
// folder, a mirrored repository, a replicated folder and a replication policy.
func describedVault(t *testing.T) (*Vault, []string, *Entry) {
	t.Helper()
	ctx := context.Background()
	v, roots := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/films"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/films", "mirror.bundle", []byte("a bundle"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ids, _ := v.Providers()

	v.mu.Lock()
	v.manifest.Automations = map[string]*Automation{"/films": {Enabled: true, Cadence: CadenceDaily, At: "03:00", Action: ActionCheck}}
	v.manifest.FolderArt = map[string]string{"/films": entry.ID}
	v.manifest.MovieFolders = map[string]*MovieFolder{"/films": {}}
	v.manifest.Repos = map[string]*GitSource{entry.ID: {URL: "https://example.com/mirror.git"}}
	v.manifest.ReplicatedFolders = map[string]bool{"/films": true}
	v.manifest.Replication = &ReplicationPolicy{Accounts: []string{ids[0].ID, ids[1].ID, ids[2].ID}, DataShards: 2, TotalShards: 3}
	err = v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := v.SyncManifestBackup(ctx, true); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	v.AwaitBackupSync()
	return v, roots, entry
}

// checkIndexCarried is what every rebuild should have kept of indexedVault's
// index, in the given scope.
func checkIndexCarried(t *testing.T, v *Vault, scope Scope, entry *Entry) {
	t.Helper()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if a := m.Automations["/films"]; a == nil || a.Action != ActionCheck {
		t.Errorf("the automation on /films was lost: %+v", m.Automations)
	}
	if m.FolderArt["/films"] != entry.ID {
		t.Errorf("the folder art on /films was lost: %+v", m.FolderArt)
	}
	if _, ok := m.MovieFolders["/films"]; !ok {
		t.Errorf("the film folder was lost: %+v", m.MovieFolders)
	}
	if r := m.Repos[entry.ID]; r == nil || r.URL != "https://example.com/mirror.git" {
		t.Errorf("the tracked repository was lost: %+v", m.Repos)
	}
}

func TestRecoverCarriesTheIndexBeyondTheFiles(t *testing.T) {
	ctx := context.Background()
	_, roots, entry := describedVault(t)
	snapshot, err := OpenBackup(readBackup(t, roots[0]), testPassword)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	target := reconnect(t, "a completely different password", roots)
	if _, err := target.Recover(ctx, snapshot, false); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	target.AwaitBackupSync()

	checkIndexCarried(t, target, MainScope, entry)
	if !target.manifest.ReplicatedFolders["/films"] {
		t.Errorf("the replicated folders were lost: %+v", target.manifest.ReplicatedFolders)
	}
	// The policy names accounts of the vault that was lost, which the
	// reconnected ones are not.
	if target.manifest.Replication != nil {
		t.Errorf("a recovery carried a replication policy naming accounts that no longer exist: %+v", target.manifest.Replication)
	}
}

func TestKitImportCarriesTheIndexBeyondTheFiles(t *testing.T) {
	ctx := context.Background()
	v, _, entry := describedVault(t)
	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	if _, err := restored.ImportKit(ctx, openTestKit(t, zipped, fingerprint.Code), KitImportOptions{Password: "new password"}); err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	restored.AwaitBackupSync()

	checkIndexCarried(t, restored, MainScope, entry)
	if !restored.manifest.ReplicatedFolders["/films"] {
		t.Errorf("the replicated folders were lost: %+v", restored.manifest.ReplicatedFolders)
	}
	// A kit import keeps every account's id, so the policy still means
	// what it meant.
	if p := restored.manifest.Replication; p == nil || len(p.Accounts) != 3 {
		t.Errorf("the replication policy was lost: %+v", p)
	}
}

func TestImportAsSubVaultCarriesTheIndexBeyondTheFiles(t *testing.T) {
	ctx := context.Background()
	v, roots, entry := describedVault(t)
	v.Lock()

	target, _ := newTestVault(t, 3)
	ids := connect(t, target, roots, "reconnected")
	report, err := target.ImportAsSubVault(ctx, ids[0], testPassword, ImportOptions{
		Label:    "Old laptop",
		Password: "the imported vault's password",
	})
	if err != nil {
		t.Fatalf("ImportAsSubVault: %v", err)
	}
	target.AwaitBackupSync()
	checkIndexCarried(t, target, Scope(report.SubVault.ID), entry)
}
