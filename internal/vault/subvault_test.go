package vault

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

const subPassword = "a different secret entirely"

// createSubVault makes a sub vault on a test vault and returns its ID.
func createSubVault(t *testing.T, v *Vault, label, password string) string {
	t.Helper()
	info, err := v.CreateSubVault(label, password)
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	if !info.Unlocked {
		t.Fatal("a freshly created sub vault should be open")
	}
	return info.ID
}

// reopen closes the handle on a vault file and opens a fresh one, which is what
// separates "this is in memory" from "this is on disk".
func reopen(t *testing.T, v *Vault) *Vault {
	t.Helper()
	v.AwaitBackupSync()
	v.Lock()

	fresh, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fresh.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	t.Cleanup(fresh.AwaitBackupSync)
	return fresh
}

// readStoreFile parses the vault file from disk, which is the only honest way
// to ask what a main password would actually be handed.
func readStoreFile(t *testing.T, path string) *storeFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading vault file: %v", err)
	}
	var sf storeFile
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("parsing vault file: %v", err)
	}
	return &sf
}

func TestSubVaultSurvivesAReopen(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), MainScope, "/", "public.txt", []byte("nothing secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to main: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to sub vault: %v", err)
	}

	fresh := reopen(t, v)

	// The main password alone opens the vault and finds only the main file.
	listing, err := fresh.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List(main): %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "public.txt" {
		t.Fatalf("main listing = %v, want just public.txt", fileNames(listing.Files))
	}

	// The sub vault is there, named, and shut.
	subs, err := fresh.SubVaults()
	if err != nil {
		t.Fatalf("SubVaults: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub vault, got %d", len(subs))
	}
	if subs[0].Label != "Taxes" {
		t.Errorf("Label = %q, want Taxes", subs[0].Label)
	}
	if subs[0].Unlocked {
		t.Error("a sub vault should not be opened by the main password")
	}
	if subs[0].Files != 1 {
		t.Errorf("Files = %d, want 1 — the inventory should count a shut sub vault", subs[0].Files)
	}

	// Listing it while shut asks for a password rather than answering.
	if _, err := fresh.List(Scope(id), "/"); !errors.Is(err, ErrSubVaultLocked) {
		t.Fatalf("List(sub) while locked = %v, want ErrSubVaultLocked", err)
	}

	if err := fresh.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	listing, err = fresh.List(Scope(id), "/")
	if err != nil {
		t.Fatalf("List(sub): %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "private.txt" {
		t.Fatalf("sub listing = %v, want just private.txt", fileNames(listing.Files))
	}

	// And the file itself opens, which is the part that proves the data keys
	// came back rather than only the index.
	data, _, err := fresh.Fetch(t.Context(), listing.Files[0].ID)
	if err != nil {
		t.Fatalf("Fetch from sub vault: %v", err)
	}
	if string(data) != "very secret" {
		t.Errorf("content = %q, want %q", data, "very secret")
	}
}

func TestMainPasswordRevealsNothingInsideASubVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "p60-2019.pdf", []byte("payslip"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to sub vault: %v", err)
	}
	if err := v.Mkdir(Scope(id), "/Correspondence"); err != nil {
		t.Fatalf("Mkdir in sub vault: %v", err)
	}
	v.AwaitBackupSync()

	// Everything the main password can reach, as one blob of text.
	sf := readStoreFile(t, v.Path())
	reachable, err := unsealStore(sf, testPassword)
	if err != nil {
		t.Fatalf("unsealStore: %v", err)
	}
	defer reachable.zero()

	blob, err := json.Marshal(reachable.manifest)
	if err != nil {
		t.Fatalf("marshalling the main manifest: %v", err)
	}
	// The sealed sub vault record travels in the same file, so include it: the
	// claim is that nothing readable leaks, not that the bytes are absent.
	raw, err := json.Marshal(sf.SubVaults)
	if err != nil {
		t.Fatalf("marshalling the sub vault records: %v", err)
	}
	visible := string(blob) + string(raw)

	for _, secret := range []string{"p60-2019.pdf", "Correspondence"} {
		if strings.Contains(visible, secret) {
			t.Errorf("%q is readable with only the main password", secret)
		}
	}
	// The name is meant to be visible — it is how you know which one to open.
	if !strings.Contains(visible, "Taxes") {
		t.Error("the sub vault's label should be readable with the main password")
	}
}

func TestSubVaultOutlivesAMainPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	entry, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload to sub vault: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "public.txt", []byte("nothing secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to main: %v", err)
	}

	const newMain = "an entirely new main password"
	report, err := v.ChangePassword(t.Context(), testPassword, newMain, true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	// Only the main vault's file is re-encrypted. The sub vault's is on a key
	// the change never touched, so it is not pending — it is not involved.
	if report.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (the main vault's only file)", report.Pending)
	}
	if !report.Done() {
		t.Errorf("migration did not finish: %+v", report)
	}

	v.AwaitBackupSync()
	v.Lock()

	fresh, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(fresh.AwaitBackupSync)
	if err := fresh.Unlock(newMain); err != nil {
		t.Fatalf("Unlock with the new main password: %v", err)
	}

	// The sub vault's own password is unchanged by a main password change, and
	// what is inside is still readable.
	if err := fresh.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault after a main password change: %v", err)
	}
	data, _, err := fresh.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch from sub vault after a main password change: %v", err)
	}
	if string(data) != "very secret" {
		t.Errorf("content = %q, want %q", data, "very secret")
	}
}

func TestLockedSubVaultSectionIsCarriedThroughUntouched(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)
	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to sub vault: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	before := readStoreFile(t, v.Path())
	if len(before.SubVaults) != 1 {
		t.Fatalf("expected 1 sub vault record, got %d", len(before.SubVaults))
	}

	// Churn the main vault: uploads, folders, deletions. None of it may disturb
	// a section whose key is not in memory.
	if err := v.Mkdir(MainScope, "/scratch"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(t.Context(), MainScope, "/scratch", "noise.txt", []byte("churn"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Delete(t.Context(), entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	v.AwaitBackupSync()

	after := readStoreFile(t, v.Path())
	if len(after.SubVaults) != 1 {
		t.Fatalf("expected 1 sub vault record after the churn, got %d", len(after.SubVaults))
	}
	if after.SubVaults[0].Section != before.SubVaults[0].Section {
		t.Error("a locked sub vault's section was rewritten by main-vault activity")
	}
	if after.SubVaults[0].DataKey != before.SubVaults[0].DataKey {
		t.Error("a locked sub vault's data key was rewritten by main-vault activity")
	}

	// And it still opens.
	if err := v.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault after the churn: %v", err)
	}
}

func TestSubVaultRefusesTheWrongPassword(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	// The main password is a wrong password here, which is the whole point.
	if err := v.UnlockSubVault(id, testPassword); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("UnlockSubVault with the main password = %v, want ErrWrongPassword", err)
	}
	if err := v.UnlockSubVault(id, "not it either"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("UnlockSubVault with a wrong password = %v, want ErrWrongPassword", err)
	}
}

func TestLockedSubVaultKeyIsToldApartFromAMissingOne(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	v.mu.RLock()
	subKeyID := v.subs[id].dataKeyID
	v.mu.RUnlock()

	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()

	// A key belonging to a shut sub vault asks for a password.
	_, err := v.dataKeyForLocked(subKeyID)
	if !errors.Is(err, ErrSubVaultLocked) {
		t.Errorf("dataKeyForLocked(sub vault's key) = %v, want ErrSubVaultLocked", err)
	}
	if !strings.Contains(err.Error(), "Taxes") {
		t.Errorf("error %q should name the sub vault to open", err)
	}

	// A key belonging to nothing at all is still a corrupt index.
	if _, err := v.dataKeyForLocked("a key no vault has ever held"); errors.Is(err, ErrSubVaultLocked) {
		t.Error("an unknown key should not be reported as a locked sub vault")
	}
}

func TestSubVaultNamesMustBeDistinct(t *testing.T) {
	v, _ := newTestVault(t, 3)
	createSubVault(t, v, "Taxes", subPassword)

	if _, err := v.CreateSubVault("taxes", "another password"); err == nil {
		t.Fatal("expected a second sub vault named the same to be refused")
	}
	if _, err := v.CreateSubVault("  ", subPassword); err == nil {
		t.Fatal("expected a nameless sub vault to be refused")
	}
	if _, err := v.CreateSubVault("Photos", "   "); err == nil {
		t.Fatal("expected a sub vault with an empty password to be refused")
	}
}

func TestVersionTwoVaultUpgradesOnWrite(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "before.txt", []byte("written at version 2"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()
	v.Lock()

	// Rewrite the file as a version 2 vault, which is what a build from before
	// sub vaults existed would have left behind.
	sf := readStoreFile(t, v.Path())
	sf.Version = 2
	sf.SubVaults = nil
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if err := os.WriteFile(v.Path(), data, 0o600); err != nil {
		t.Fatalf("writing the version 2 vault: %v", err)
	}

	fresh, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open a version 2 vault: %v", err)
	}
	t.Cleanup(fresh.AwaitBackupSync)
	if err := fresh.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock a version 2 vault: %v", err)
	}
	listing, err := fresh.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 {
		t.Fatalf("expected the version 2 vault's file to still be there, got %v", fileNames(listing.Files))
	}

	// The first change upgrades it.
	if err := fresh.Mkdir(MainScope, "/anything"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if got := readStoreFile(t, fresh.Path()).Version; got != StoreVersion {
		t.Errorf("version after a write = %d, want %d", got, StoreVersion)
	}
}

func TestFutureVaultVersionIsRefused(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	v.Lock()

	sf := readStoreFile(t, v.Path())
	sf.Version = StoreVersion + 1
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if err := os.WriteFile(v.Path(), data, 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := readStore(v.Path()); err == nil {
		t.Fatal("expected a vault from a newer build to be refused rather than partly understood")
	}
}

func fileNames(files []*Entry) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}

func TestAssignMovesAFolderIntoASubVaultKeepingItsPath(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if err := v.Mkdir(MainScope, "/Papers/2019"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(t.Context(), MainScope, "/Papers/2019", "p60.pdf", []byte("payslip"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "elsewhere.txt", []byte("stays put"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	report, err := v.Assign(t.Context(), MainScope, "/Papers", Scope(id), true)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if report.Files != 1 {
		t.Errorf("Files = %d, want 1", report.Files)
	}

	// Gone from the main vault, folder and all.
	if v.FolderExists(MainScope, "/Papers") {
		t.Error("/Papers should have left the main vault")
	}
	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List(main): %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "elsewhere.txt" {
		t.Errorf("main listing = %v, want just elsewhere.txt", fileNames(listing.Files))
	}

	// Arrived in the sub vault at exactly the path it left.
	if !v.FolderExists(Scope(id), "/Papers/2019") {
		t.Fatal("/Papers/2019 should exist in the sub vault")
	}
	moved, err := v.EntryByPath(Scope(id), "/Papers/2019/p60.pdf")
	if err != nil {
		t.Fatalf("EntryByPath in the sub vault: %v", err)
	}
	if moved.ID != entry.ID {
		t.Errorf("the assigned file has a different ID: %s, want %s", moved.ID, entry.ID)
	}

	// And it reads, which after a migration means it reads off the sub vault's
	// own key rather than the one it arrived under.
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after assignment: %v", err)
	}
	if string(data) != "payslip" {
		t.Errorf("content = %q, want %q", data, "payslip")
	}
	if pending := v.PendingMigrationIn(Scope(id)); pending != 0 {
		t.Errorf("%d file(s) still on the old key after a migrating assignment", pending)
	}
}

func TestAssignedFileIsReadableBeforeItHasMigrated(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	entry, _, err := v.Upload(t.Context(), MainScope, "/", "notes.txt", []byte("in transit"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Assigned without waiting for the re-encryption, which is how the app does
	// it: the move is instant and the key work happens behind it.
	if _, err := v.Assign(t.Context(), MainScope, "/notes.txt", Scope(id), false); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if pending := v.PendingMigrationIn(Scope(id)); pending != 1 {
		t.Fatalf("PendingMigrationIn = %d, want 1", pending)
	}

	// Readable in the meantime — the key it arrived under came with it.
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch before the migration: %v", err)
	}
	if string(data) != "in transit" {
		t.Errorf("content = %q, want %q", data, "in transit")
	}

	// And it survives being closed and opened again in that state, which is the
	// case a crash mid-migration leaves behind.
	fresh := reopen(t, v)
	if err := fresh.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	if data, _, err = fresh.Fetch(t.Context(), entry.ID); err != nil {
		t.Fatalf("Fetch after a reopen mid-migration: %v", err)
	}
	if string(data) != "in transit" {
		t.Errorf("content = %q, want %q", data, "in transit")
	}

	report, err := fresh.MigrateFilesIn(t.Context(), Scope(id), nil, nil)
	if err != nil {
		t.Fatalf("MigrateFilesIn: %v", err)
	}
	if !report.Done() {
		t.Errorf("migration did not finish: %+v", report)
	}
}

func TestAssignBackToTheMainVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	entry, _, err := v.Upload(t.Context(), Scope(id), "/", "receipt.pdf", []byte("a receipt"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), Scope(id), "/receipt.pdf", MainScope, true); err != nil {
		t.Fatalf("Assign back: %v", err)
	}

	if _, err := v.EntryByPath(Scope(id), "/receipt.pdf"); err == nil {
		t.Error("the file should have left the sub vault")
	}
	back, err := v.EntryByPath(MainScope, "/receipt.pdf")
	if err != nil {
		t.Fatalf("EntryByPath in the main vault: %v", err)
	}
	if back.ID != entry.ID {
		t.Errorf("ID = %s, want %s", back.ID, entry.ID)
	}

	// It is on the main vault's key now, so locking the sub vault leaves it
	// readable — which is the whole meaning of having taken it out.
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the sub vault was locked: %v", err)
	}
	if string(data) != "a receipt" {
		t.Errorf("content = %q, want %q", data, "a receipt")
	}
}

func TestAssignRenamesOnACollisionRatherThanOverwriting(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "notes.txt", []byte("already here"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to sub: %v", err)
	}
	incoming, _, err := v.Upload(t.Context(), MainScope, "/", "notes.txt", []byte("arriving"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload to main: %v", err)
	}

	report, err := v.Assign(t.Context(), MainScope, "/notes.txt", Scope(id), true)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if report.Renamed != 1 {
		t.Errorf("Renamed = %d, want 1", report.Renamed)
	}

	listing, err := v.List(Scope(id), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 2 {
		t.Fatalf("sub vault holds %v, want both files", fileNames(listing.Files))
	}
	// Neither copy was lost.
	for _, want := range []string{"already here", "arriving"} {
		found := false
		for _, f := range listing.Files {
			if data, _, err := v.Fetch(t.Context(), f.ID); err == nil && string(data) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the file holding %q did not survive the collision", want)
		}
	}
	if _, _, err := v.Fetch(t.Context(), incoming.ID); err != nil {
		t.Errorf("the assigned file is unreadable: %v", err)
	}
}

func TestSubVaultPasswordChangeRotatesItsKey(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	entry, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := entry.KeyID

	const newSub = "a second sub vault password"
	report, err := v.ChangeSubVaultPassword(t.Context(), id, subPassword, newSub, true)
	if err != nil {
		t.Fatalf("ChangeSubVaultPassword: %v", err)
	}
	if !report.Done() {
		t.Errorf("migration did not finish: %+v", report)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.KeyID == before {
		t.Error("the data key was not rotated, so the old password still opens the parts")
	}

	fresh := reopen(t, v)
	if err := fresh.UnlockSubVault(id, subPassword); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("the old sub vault password still opens it: %v", err)
	}
	if err := fresh.UnlockSubVault(id, newSub); err != nil {
		t.Fatalf("UnlockSubVault with the new password: %v", err)
	}
	data, _, err := fresh.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the sub vault's password changed: %v", err)
	}
	if string(data) != "very secret" {
		t.Errorf("content = %q, want %q", data, "very secret")
	}
}

func TestSubVaultPasswordChangeRefusesTheWrongOldPassword(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, err := v.ChangeSubVaultPassword(t.Context(), id, "not the old one", "a new one", false); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("ChangeSubVaultPassword with a wrong old password = %v, want ErrWrongPassword", err)
	}
	// Still opens with what it always did.
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}
	if err := v.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("the original password stopped working after a refused change: %v", err)
	}
}

func TestDeleteSubVaultErasesItsPartsFromTheAccounts(t *testing.T) {
	v, roots := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "public.txt", []byte("nothing secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := countStoredParts(t, roots)

	warnings, err := v.DeleteSubVault(t.Context(), id, false)
	if err != nil {
		t.Fatalf("DeleteSubVault: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	after := countStoredParts(t, roots)
	if after >= before {
		t.Errorf("%d part file(s) before, %d after — the sub vault's parts were not erased", before, after)
	}
	subs, err := v.SubVaults()
	if err != nil {
		t.Fatalf("SubVaults: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("the sub vault is still listed: %+v", subs)
	}
	// The main vault is untouched.
	if _, err := v.EntryByPath(MainScope, "/public.txt"); err != nil {
		t.Errorf("the main vault's file went with it: %v", err)
	}
}

func TestDeleteALockedSubVaultNeedsForceAndStillErasesIt(t *testing.T) {
	v, roots := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)
	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := countStoredParts(t, roots)
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	if _, err := v.DeleteSubVault(t.Context(), id, false); err == nil {
		t.Fatal("deleting a locked sub vault should ask for confirmation first")
	}

	// Forced, it goes — and so do its parts, which only the inventory knew
	// about. This is what the inventory is for.
	if _, err := v.DeleteSubVault(t.Context(), id, true); err != nil {
		t.Fatalf("DeleteSubVault --force: %v", err)
	}
	if after := countStoredParts(t, roots); after >= before {
		t.Errorf("%d part file(s) before, %d after — a locked sub vault's parts were left behind", before, after)
	}
}

// countStoredParts counts the .sand objects across the local-folder accounts
// standing in for clouds.
func countStoredParts(t *testing.T, roots []string) int {
	t.Helper()
	total := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sand") && e.Name() != "manifest.sand" {
				total++
			}
		}
	}
	return total
}

func TestBackupCarriesSubVaultsThroughARecovery(t *testing.T) {
	original, roots := newTestVault(t, 3)
	id := createSubVault(t, original, "Taxes", subPassword)

	if _, _, err := original.Upload(t.Context(), MainScope, "/", "public.txt", []byte("nothing secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload to main: %v", err)
	}
	entry, _, err := original.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload to sub: %v", err)
	}
	if _, err := original.SyncManifestBackup(t.Context(), true); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	original.AwaitBackupSync()
	original.Lock()

	// A fresh machine: a new vault, a new password, the same accounts.
	fresh, err := Open(filepath.Join(t.TempDir(), "recovered.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fresh.Init("a password on the new machine", PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(fresh.AwaitBackupSync)
	for i, root := range roots {
		if _, err := fresh.AddProvider(t.Context(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "cloud-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	providers, err := fresh.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	snapshot, err := fresh.FetchBackup(t.Context(), providers[0].ID, testPassword)
	if err != nil {
		t.Fatalf("FetchBackup: %v", err)
	}
	if _, err := fresh.Recover(t.Context(), snapshot, false); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The sub vault came back sealed, named, and openable by its own password —
	// which is neither the vault that was lost nor the one on this machine.
	subs, err := fresh.SubVaults()
	if err != nil {
		t.Fatalf("SubVaults: %v", err)
	}
	if len(subs) != 1 || subs[0].Label != "Taxes" {
		t.Fatalf("recovered sub vaults = %+v, want one called Taxes", subs)
	}
	if subs[0].Unlocked {
		t.Error("a recovered sub vault should arrive shut")
	}
	if err := fresh.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault after recovery: %v", err)
	}
	data, _, err := fresh.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch from a recovered sub vault: %v", err)
	}
	if string(data) != "very secret" {
		t.Errorf("content = %q, want %q", data, "very secret")
	}
}

func TestRecoverRefusesToReplaceExistingSubVaults(t *testing.T) {
	v, _ := newTestVault(t, 3)
	createSubVault(t, v, "Taxes", subPassword)

	// A snapshot of anything at all; the guard fires before it is looked at.
	v.mu.RLock()
	snapshot := v.snapshotLocked()
	v.mu.RUnlock()

	if _, err := v.Recover(t.Context(), snapshot, false); err == nil {
		t.Fatal("recovering over a vault that holds sub vaults should be refused")
	} else if !strings.Contains(err.Error(), "sub vault") {
		t.Errorf("error should say why: %v", err)
	}
}

// The same guarantee as TestSubVaultOutlivesAMainPasswordChange, with the sub
// vault shut — which is the case that actually broke.
//
// An open sub vault is re-sealed from memory by the write that follows a
// password change, so it survives even if the change forgot to carry its
// record across. A locked one has no copy anywhere but the store file, so
// forgetting it destroys the sub vault outright and leaves nothing to say what
// was lost.
func TestALockedSubVaultSurvivesAMainPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	entry, _, err := v.Upload(t.Context(), Scope(id), "/", "private.txt", []byte("very secret"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	const newMain = "an entirely new main password"
	if _, err := v.ChangePassword(t.Context(), testPassword, newMain, true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	v.AwaitBackupSync()
	v.Lock()

	fresh, err := Open(v.Path())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(fresh.AwaitBackupSync)
	if err := fresh.Unlock(newMain); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	subs, err := fresh.SubVaults()
	if err != nil {
		t.Fatalf("SubVaults: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("the sub vault did not survive the password change: %+v", subs)
	}
	if err := fresh.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault after a main password change: %v", err)
	}
	data, _, err := fresh.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != "very secret" {
		t.Errorf("content = %q, want %q", data, "very secret")
	}
}

// No key ever crosses a vault boundary, in either direction.
//
// This is the guarantee a sub vault exists for, and an earlier version of
// Assign broke it in the most direct way available: it copied the source's data
// key into the destination so the moved file would read immediately. A vault's
// files share one active generation, so assigning a single receipt out of a sub
// vault handed the main password everything else in it — and snapshotLocked
// replicates every retired key to every connected account, alongside the
// inventory naming the objects. Main password plus one cloud account was a
// complete break.
func TestAssignmentNeverHandsOverTheOtherVaultsKey(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	// Two files in the sub vault. Only one of them ever leaves.
	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "stays.txt", []byte("most secret"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "leaves.txt", []byte("mundane"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	v.mu.RLock()
	subKeyID := v.subs[id].dataKeyID
	mainKeyID := v.dataKeyID
	v.mu.RUnlock()

	// Deferred migration, which is what the app does: the move is instant and
	// the re-encryption runs behind it.
	if _, err := v.Assign(t.Context(), Scope(id), "/leaves.txt", MainScope, false); err != nil {
		t.Fatalf("Assign out: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	v.mu.RLock()
	_, adopted := v.retired[subKeyID]
	snapshot := v.snapshotLocked()
	v.mu.RUnlock()

	if adopted {
		t.Error("the main vault holds the sub vault's data key after an assignment out")
	}
	for _, k := range snapshot.Keys {
		if k.ID == subKeyID {
			t.Error("the manifest backup carries the sub vault's data key — the main password " +
				"and the inventory would decrypt the whole sub vault")
		}
	}

	// The file that left is on a key only the sub vault holds, so it reads while
	// that vault is open and asks for a password when it is not. Honest either
	// way, and never readable without the sub vault's own password.
	moved, err := v.EntryByPath(MainScope, "/leaves.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if _, _, err := v.Fetch(t.Context(), moved.ID); !errors.Is(err, ErrSubVaultLocked) {
		t.Errorf("Fetch of an unmigrated assigned-out file = %v, want ErrSubVaultLocked", err)
	}

	// And the mirror: assigning in must not hand the sub vault the main key.
	if err := v.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "public.txt", []byte("ordinary"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), MainScope, "/public.txt", Scope(id), false); err != nil {
		t.Fatalf("Assign in: %v", err)
	}

	v.mu.RLock()
	_, mainAdopted := v.subs[id].retired[mainKeyID]
	v.mu.RUnlock()
	if mainAdopted {
		t.Error("the sub vault holds the main vault's active data key, so its password " +
			"would unwrap the main vault's stored parts")
	}
}

// Once the re-encryption has run, the moved file is on the destination's own
// key and needs nothing from the vault it came from.
func TestAMigratedAssignmentNeedsNothingFromTheOldVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "receipt.pdf", []byte("a receipt"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), Scope(id), "/receipt.pdf", MainScope, true); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	entry, err := v.EntryByPath(MainScope, "/receipt.pdf")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after a migrated assignment: %v", err)
	}
	if string(data) != "a receipt" {
		t.Errorf("content = %q, want %q", data, "a receipt")
	}
}

// A main password change works while the main index still names a sub vault's
// key generation, which is what an unmigrated assignment leaves behind.
func TestPasswordChangeWithAnUnmigratedAssignment(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "receipt.pdf", []byte("a receipt"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), Scope(id), "/receipt.pdf", MainScope, false); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	const newMain = "an entirely new main password"
	if _, err := v.ChangePassword(t.Context(), testPassword, newMain, false); err != nil {
		t.Fatalf("ChangePassword with an unmigrated assignment: %v", err)
	}

	// The file is still where it was put, still on the sub vault's key, and
	// still readable once that vault is open.
	if err := v.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	entry, err := v.EntryByPath(MainScope, "/receipt.pdf")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if _, _, err := v.Fetch(t.Context(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}
