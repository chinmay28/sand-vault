package vault

import (
	"strings"
	"testing"
)

// The breakdown, and the line it will not cross. A sub vault's weight belongs
// in an account's figures — leaving it out would draw the account as emptier
// than it is — and its file names belong to its own password.
func TestProviderStatsKeepsSubVaultsToAFigure(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), MainScope, "/", "holiday.jpg",
		[]byte(strings.Repeat("a photograph ", 400)), UploadOptions{}); err != nil {
		t.Fatalf("upload to the main vault: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "p60-2019.pdf",
		[]byte(strings.Repeat("a payslip ", 400)), UploadOptions{}); err != nil {
		t.Fatalf("upload to the sub vault: %v", err)
	}

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	account := statuses[0].ID

	open, err := v.ProviderStats(t.Context(), account)
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}

	// Both files leant on the account, and both are counted.
	if open.Files != 2 {
		t.Errorf("files = %d, want both the main vault's and the sub vault's", open.Files)
	}
	if open.SubVaults.Parts == 0 || open.SubVaults.Bytes == 0 {
		t.Errorf("the open sub vault put nothing on the account: %+v", open.SubVaults)
	}
	if open.SubVaults.Label != "1 sub vault" {
		t.Errorf("sub vault label = %q", open.SubVaults.Label)
	}

	// Only the main vault's file is named, and only its kinds and folders are
	// broken out — the sub vault is open, and the panel still says nothing
	// about what is inside it.
	if named := paths(open); len(named) != 1 || named[0] != "/holiday.jpg" {
		t.Errorf("largest files name %v", named)
	}
	for _, kind := range open.Kinds {
		if kind.Label == "documents" {
			t.Errorf("the sub vault's pdf turned up in the breakdown: %+v", open.Kinds)
		}
	}

	// Locking it changes none of that: the figures come from the inventory
	// while the index behind them cannot be read.
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}
	shut, err := v.ProviderStats(t.Context(), account)
	if err != nil {
		t.Fatalf("ProviderStats while locked: %v", err)
	}
	if shut.Files != open.Files || shut.SubVaults.Parts != open.SubVaults.Parts {
		t.Errorf("locking changed the account's figures: %+v then %+v", open.SubVaults, shut.SubVaults)
	}
	if shut.Stored != open.Stored {
		t.Errorf("stored = %d while locked, %d while open", shut.Stored, open.Stored)
	}
}

// An account holding a part of something nothing else can rebuild is what the
// disconnect guard refuses on. The panel counts it the same way, against the
// accounts that would survive the disconnect.
func TestProviderStatsCountsTheLastCopy(t *testing.T) {
	v, _ := newTestVault(t, 3)

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	two := []string{statuses[0].ID, statuses[1].ID}

	if _, _, err := v.Upload(t.Context(), MainScope, "/", "pinned.txt",
		[]byte(strings.Repeat("pinned ", 300)), UploadOptions{Accounts: two}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// A 2-of-3 file with both its parts on two accounts needs both of them.
	for _, id := range two {
		report, err := v.ProviderStats(t.Context(), id)
		if err != nil {
			t.Fatalf("ProviderStats: %v", err)
		}
		if report.Sole != 1 {
			t.Errorf("account %s: sole = %d, want 1", id, report.Sole)
		}
	}

	spare, err := v.ProviderStats(t.Context(), statuses[2].ID)
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if spare.Files != 0 || spare.Sole != 0 {
		t.Errorf("the account holding nothing claims %d files, %d sole", spare.Files, spare.Sole)
	}
	// It still knows what the whole vault weighs, which is what the panel
	// draws an account's share against.
	if spare.VaultStored <= 0 {
		t.Errorf("vault_stored = %d", spare.VaultStored)
	}
}

func paths(report ProviderReport) []string {
	out := make([]string, 0, len(report.Largest))
	for _, item := range report.Largest {
		out = append(out, item.Path)
	}
	return out
}
