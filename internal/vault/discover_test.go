package vault

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// abandonedVault stores files through a set of accounts and then goes away,
// leaving its manifest backup behind — which is what someone reconnecting an
// old cloud to a new vault is actually looking at.
func abandonedVault(t *testing.T, accounts int) (roots []string, password string) {
	t.Helper()
	password = "the password of the machine that died"

	dir := t.TempDir()
	old, err := Open(filepath.Join(dir, "old.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := old.Init(password, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for i := 0; i < accounts; i++ {
		root := filepath.Join(dir, "cloud", string(rune('a'+i)))
		roots = append(roots, root)
		if _, err := old.AddProvider(t.Context(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "old-cloud-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	if err := old.Mkdir(MainScope, "/Papers"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := old.Upload(t.Context(), MainScope, "/Papers", "deed.pdf", []byte("the deed"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := old.SyncManifestBackup(t.Context(), true); err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	old.AwaitBackupSync()
	old.Lock()
	return roots, password
}

// connect wires a set of local-folder roots into a vault as accounts.
func connect(t *testing.T, v *Vault, roots []string, prefix string) []string {
	t.Helper()
	ids := make([]string, 0, len(roots))
	for i, root := range roots {
		cfg, err := v.AddProvider(t.Context(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    prefix + "-" + string(rune('a'+i)),
			Options: map[string]string{"path": root},
		})
		if err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
		ids = append(ids, cfg.ID)
	}
	return ids
}

// The scan that notices a foreign vault is recovery.go's, and importing is the
// other half of it: what it finds, this brings in without replacing anything.
func TestTheRecoveryScanFindsTheVaultAnImportWouldBringIn(t *testing.T) {
	roots, _ := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	connect(t, v, roots, "reconnected")

	scan, err := v.ScanForRecovery(t.Context())
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}

	foreign := 0
	for _, src := range scan.Sources {
		if src.Backup && src.Foreign {
			foreign++
			if !strings.HasPrefix(src.Name, "reconnected") {
				t.Errorf("%s holds this vault's own backup but was called foreign", src.Name)
			}
		}
	}
	if foreign != len(roots) {
		t.Errorf("found %d account(s) holding another vault, want %d", foreign, len(roots))
	}
}

func TestImportAnAbandonedVaultAsASubVault(t *testing.T) {
	roots, oldPassword := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "mine.txt", []byte("already here"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ids := connect(t, v, roots, "reconnected")

	const importedPassword = "a password chosen at import time"
	report, err := v.ImportAsSubVault(t.Context(), ids[0], oldPassword, ImportOptions{
		Label:       "Old laptop",
		Password:    importedPassword,
		AdoptBackup: true,
	})
	if err != nil {
		t.Fatalf("ImportAsSubVault: %v", err)
	}
	if report.Files != 1 {
		t.Errorf("Files = %d, want 1", report.Files)
	}
	if report.Recoverable != 1 {
		t.Errorf("Recoverable = %d, want 1 — every part's account is connected", report.Recoverable)
	}
	if report.Relocated == 0 {
		t.Error("Relocated = 0, but every account was reconnected under a fresh id")
	}

	// The vault that was already here is untouched.
	if _, err := v.EntryByPath(MainScope, "/mine.txt"); err != nil {
		t.Errorf("the importing vault lost its own file: %v", err)
	}

	// The imported one is a sub vault, at the paths it always had.
	id := report.SubVault.ID
	entry, err := v.EntryByPath(Scope(id), "/Papers/deed.pdf")
	if err != nil {
		t.Fatalf("EntryByPath in the imported sub vault: %v", err)
	}
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch from the imported sub vault: %v", err)
	}
	if string(data) != "the deed" {
		t.Errorf("content = %q, want %q", data, "the deed")
	}

	// It answers to the password chosen during the import, and not to the one
	// the dead machine used.
	fresh := reopen(t, v)
	if err := fresh.UnlockSubVault(id, oldPassword); err == nil {
		t.Error("the old vault's password should not open the imported sub vault")
	}
	if err := fresh.UnlockSubVault(id, importedPassword); err != nil {
		t.Fatalf("UnlockSubVault with the import password: %v", err)
	}
}

func TestImportedSubVaultCanBeReEncryptedOntoAFreshKey(t *testing.T) {
	roots, oldPassword := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	ids := connect(t, v, roots, "reconnected")

	report, err := v.ImportAsSubVault(t.Context(), ids[0], oldPassword, ImportOptions{
		Label:    "Old laptop",
		Password: "a new password",
	})
	if err != nil {
		t.Fatalf("ImportAsSubVault: %v", err)
	}
	scope := Scope(report.SubVault.ID)

	entry, err := v.EntryByPath(scope, "/Papers/deed.pdf")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	// Imported files arrive on the key the old vault used, which is the key the
	// old password still opens. That is what the re-encryption is for.
	adopted := entry.KeyID

	// A rotation, then the pass that moves the files onto it. This is what the
	// app offers by default once an import lands.
	if _, err := v.ChangeSubVaultPassword(t.Context(), report.SubVault.ID, "a new password", "a newer password", true); err != nil {
		t.Fatalf("ChangeSubVaultPassword: %v", err)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.KeyID == adopted {
		t.Error("the imported files are still on the key the old password opens")
	}
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after re-encryption: %v", err)
	}
	if string(data) != "the deed" {
		t.Errorf("content = %q, want %q", data, "the deed")
	}
}

func TestImportRefusesAWrongBackupPassword(t *testing.T) {
	roots, _ := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	ids := connect(t, v, roots, "reconnected")

	if _, err := v.ImportAsSubVault(t.Context(), ids[0], "not the old password", ImportOptions{
		Label:    "Old laptop",
		Password: "a new password",
	}); err == nil {
		t.Fatal("importing with the wrong backup password should be refused")
	}
	subs, err := v.SubVaults()
	if err != nil {
		t.Fatalf("SubVaults: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("a refused import left something behind: %+v", subs)
	}
}
