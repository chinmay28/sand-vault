package vault

import (
	"errors"
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

// The other answer to what the scan found: the old vault nobody wants back.
func TestDiscardingAFoundVaultTakesTheAccountOver(t *testing.T) {
	roots, _ := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	ids := connect(t, v, roots, "reconnected")

	report, err := v.DiscardFoundVault(t.Context(), ids[0])
	if err != nil {
		t.Fatalf("DiscardFoundVault: %v", err)
	}
	if report.Account != "reconnected-a" {
		t.Errorf("Account = %q, want the account it was told to clear", report.Account)
	}
	if !report.Claimed {
		t.Error("replication is on, so this vault's own index should be going there")
	}
	v.AwaitBackupSync()

	scan, err := v.ScanForRecovery(t.Context())
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	for _, src := range scan.Sources {
		switch src.ProviderID {
		case ids[0]:
			if src.Foreign {
				t.Error("the discarded index is still being offered")
			}
			// The point of taking the account over: the guard that was
			// refusing it has nothing left to protect, so this vault's own
			// index goes there like any other account's.
			if !src.Backup {
				t.Error("nothing replaced the index that was removed")
			}
		case ids[1], ids[2]:
			// One row, one account. The same old vault's index sits on the
			// other two, and each is its own decision.
			if !src.Foreign {
				t.Errorf("%s stopped offering its index, which nobody asked for", src.Name)
			}
		}
	}
}

// What is left behind, and where it can be dealt with. The parts of the
// discarded vault stay on the account — but a foreign index is exactly what
// makes the orphan scan withhold one, so removing it is what puts that storage
// in front of somebody with a size on it.
func TestDiscardingAFoundVaultOffersItsPartsToTheSweep(t *testing.T) {
	roots, _ := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	ids := connect(t, v, roots, "reconnected")

	before := orphanScan(t, v)
	if before.Deletable != 0 {
		t.Fatalf("%d object(s) were offered while another vault's index was there", before.Deletable)
	}

	for _, id := range ids {
		if _, err := v.DiscardFoundVault(t.Context(), id); err != nil {
			t.Fatalf("DiscardFoundVault: %v", err)
		}
	}
	v.AwaitBackupSync()

	after := orphanScan(t, v)
	if after.Deletable == 0 {
		t.Error("the discarded vault's parts are still withheld from the sweep")
	}
	for _, item := range after.Items {
		if !item.Deletable {
			t.Errorf("a row is still withheld: %s", item.Reason)
		}
	}
}

// The one thing this must never do, because the row it is reached from was
// drawn from a scan that may have gone stale.
func TestDiscardRefusesThisVaultsOwnIndex(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "mine.txt", []byte("already here"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	scan, err := v.ScanForRecovery(t.Context())
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	own := scan.Sources[0]
	if !own.Backup || own.Foreign {
		t.Fatalf("%s is not holding this vault's own index, so there is nothing to refuse", own.Name)
	}

	if _, err := v.DiscardFoundVault(t.Context(), own.ProviderID); !errors.Is(err, ErrNotForeign) {
		t.Fatalf("DiscardFoundVault on this vault's own index = %v, want ErrNotForeign", err)
	}

	// And the refusal left it where it was, which is the whole point: that copy
	// is what a recovery reads.
	after, err := v.ScanForRecovery(t.Context())
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	if !after.Sources[0].Backup {
		t.Error("the refused discard deleted this vault's own index anyway")
	}
}

// An account holding nothing has nothing to discard, and says so rather than
// reporting a success it did not have.
func TestDiscardOnAnAccountWithNoIndex(t *testing.T) {
	roots, _ := abandonedVault(t, 3)

	v, _ := newTestVault(t, 3)
	v.AwaitBackupSync()
	ids := connect(t, v, roots, "reconnected")

	if _, err := v.DiscardFoundVault(t.Context(), ids[0]); err != nil {
		t.Fatalf("DiscardFoundVault: %v", err)
	}
	v.AwaitBackupSync()

	// Twice. The second time the index it was pointed at is this vault's own,
	// so the refusal is the one that matters most.
	if _, err := v.DiscardFoundVault(t.Context(), ids[0]); !errors.Is(err, ErrNotForeign) {
		t.Fatalf("discarding twice = %v, want ErrNotForeign", err)
	}
}
