package vault

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

func setAutoPrune(t *testing.T, v *Vault, id string, on bool) provider.Config {
	t.Helper()

	cfg, err := v.UpdateProvider(context.Background(), id, ProviderEdit{AutoPrune: &on})
	if err != nil {
		t.Fatalf("UpdateProvider auto-prune=%v: %v", on, err)
	}
	return cfg
}

func TestAutoPruneIsASettingOnlyABucketCanHave(t *testing.T) {
	ctx := context.Background()
	v, _, configs := newVersionedVault(t, 3)

	// Off is where every account starts, and nothing is due.
	if v.AutoPruneDue(time.Now()) {
		t.Fatal("a vault with nothing asking for it has a prune due")
	}

	updated := setAutoPrune(t, v, configs[0].ID, true)
	if !updated.AutoPrune {
		t.Fatalf("the setting did not take: %+v", updated)
	}
	if !v.AutoPruneDue(time.Now()) {
		t.Error("an account that just asked for it is not due")
	}

	// It survives a lock and an unlock, since it is in the vault file.
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, cfg := range accounts {
		if cfg.ID == configs[0].ID && !cfg.AutoPrune {
			t.Error("the setting did not survive a lock")
		}
		if cfg.ID != configs[0].ID && cfg.AutoPrune {
			t.Errorf("%s was never asked and has the setting on", cfg.Name)
		}
	}

	// And it can be switched off again.
	if updated := setAutoPrune(t, v, configs[0].ID, false); updated.AutoPrune {
		t.Error("the setting did not clear")
	}

	// A folder on a disk keeps nothing beneath its files, and is told so.
	local, _ := newTestVault(t, 1)
	locals, _ := local.Providers()
	on := true
	_, err = local.UpdateProvider(ctx, locals[0].ID, ProviderEdit{AutoPrune: &on})
	if err == nil || !strings.Contains(err.Error(), "keeps no old versions") {
		t.Errorf("auto-prune on a local folder: %v, want a refusal that says why", err)
	}
	// Off on such an account is not an error: it is already off.
	off := false
	if _, err := local.UpdateProvider(ctx, locals[0].ID, ProviderEdit{AutoPrune: &off}); err != nil {
		t.Errorf("switching auto-prune off on a folder: %v", err)
	}
}

func TestAutoPruneSweepsOnlyTheAccountsThatAskedAndKeepsTime(t *testing.T) {
	ctx := context.Background()
	v, stub, configs := newVersionedVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("deleted on all three"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Delete(ctx, entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	setAutoPrune(t, v, configs[1].ID, true)
	v.AwaitBackupSync()

	before := versionScan(t, v)
	var asked VersionAccount
	for _, a := range before.Accounts {
		if a.ProviderID == configs[1].ID {
			asked = a
		}
	}
	if !asked.AutoPrune || asked.LastPrune != nil || asked.Deletable == 0 {
		t.Fatalf("before the first run the account row should say it is scheduled and never pruned: %+v", asked)
	}

	report, err := v.AutoPrune(ctx)
	if err != nil {
		t.Fatalf("AutoPrune: %v", err)
	}
	if report.Deleted != asked.Deletable {
		t.Errorf("the scheduled prune erased %d, the account had %d to erase", report.Deleted, asked.Deletable)
	}
	if outcome, ok := report.Accounts[configs[1].ID]; !ok || outcome.Deleted != report.Deleted {
		t.Errorf("no per-account outcome for the account pruned: %+v", report.Accounts)
	}
	if len(report.Accounts) != 1 {
		t.Errorf("accounts touched: %+v, want only the one that asked", report.Accounts)
	}

	// The bucket that asked is tidy; the two that did not are as they were.
	if got := stub.Versions(configs[1].Name); len(got) != len(stub.Objects(configs[1].Name)) {
		t.Errorf("%s still stores %d version(s) of %d object(s)", configs[1].Name, len(got), len(stub.Objects(configs[1].Name)))
	}
	after := versionScan(t, v)
	for _, a := range after.Accounts {
		switch {
		case a.ProviderID == configs[1].ID && a.Stale != 0:
			t.Errorf("%s was pruned and still reports %d stale", a.Name, a.Stale)
		case a.ProviderID == configs[1].ID && (a.LastPrune == nil || a.LastPrune.Deleted != report.Deleted || a.LastPrune.At.IsZero()):
			t.Errorf("%s does not say what the prune did: %+v", a.Name, a.LastPrune)
		case a.ProviderID != configs[1].ID && a.Stale == 0:
			t.Errorf("%s never asked and was pruned anyway", a.Name)
		case a.ProviderID != configs[1].ID && a.LastPrune != nil:
			t.Errorf("%s never asked and carries a prune record: %+v", a.Name, a.LastPrune)
		}
	}

	// Not due again until tomorrow, and due again after.
	if v.AutoPruneDue(time.Now()) {
		t.Error("due again straight after running")
	}
	if !v.AutoPruneDue(time.Now().Add(AutoPruneInterval + time.Minute)) {
		t.Error("not due a day later")
	}

	// A disconnected account takes its record with it.
	if err := v.RemoveProvider(configs[1].ID, true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if v.lastPrune(configs[1].ID) != nil {
		t.Error("a disconnected account still has a prune record")
	}
	if v.AutoPruneDue(time.Now().Add(AutoPruneInterval + time.Minute)) {
		t.Error("with nobody asking any more, a prune is still due")
	}
}
