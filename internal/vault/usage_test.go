package vault

import (
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
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

// A capacity somebody typed is a denominator, and a denominator with no
// numerator draws an account as empty. So it waits for a figure something has
// actually taken, and never overrides one the backend reports itself.
func TestDeclaredCapacityWaitsForACount(t *testing.T) {
	const declared = int64(10 << 30)

	for _, tc := range []struct {
		name      string
		usage     provider.Usage
		capacity  int64
		wantTotal int64
		wantOwn   bool // the total is the account holder's figure
	}{
		{
			name:      "nothing counted",
			usage:     provider.Usage{},
			capacity:  declared,
			wantTotal: 0,
		},
		{
			name:      "counted",
			usage:     provider.Usage{Used: 1 << 20, Measured: true},
			capacity:  declared,
			wantTotal: declared,
			wantOwn:   true,
		},
		{
			name:      "counted empty",
			usage:     provider.Usage{Used: 0, Measured: true},
			capacity:  declared,
			wantTotal: declared,
			wantOwn:   true,
		},
		{
			name:      "the backend has a quota of its own",
			usage:     provider.Usage{Used: 250, Total: 1000},
			capacity:  declared,
			wantTotal: 1000,
		},
		{
			name:      "counted, nobody declaring",
			usage:     provider.Usage{Used: 1 << 20, Measured: true},
			capacity:  0,
			wantTotal: 0,
		},
	} {
		got := withDeclaredCapacity(tc.usage, tc.capacity)
		if got.Total != tc.wantTotal {
			t.Errorf("%s: total = %d, want %d", tc.name, got.Total, tc.wantTotal)
		}
		if got.Declared != tc.wantOwn {
			t.Errorf("%s: declared = %v, want %v", tc.name, got.Declared, tc.wantOwn)
		}
	}
}

// The declared capacity is stored with the account, like its name and its
// colour: nothing about it reaches the backend, and it survives a lock.
func TestUpdateProviderStoresTheDeclaredCapacity(t *testing.T) {
	v, _ := newTestVault(t, 1)

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	id := statuses[0].ID

	capacity := int64(10 << 30)
	if _, err := v.UpdateProvider(id, ProviderEdit{Capacity: &capacity}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	fresh := reopen(t, v)
	after, err := fresh.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses after reopening: %v", err)
	}
	if after[0].Capacity != capacity {
		t.Errorf("capacity after reopening = %d, want %d", after[0].Capacity, capacity)
	}
	// A local folder measures the drive under it, so the figure it reports is
	// the one drawn against — a declared capacity does not overrule an account
	// that can answer for itself.
	if after[0].Usage.Declared {
		t.Errorf("a declared capacity overruled the drive's own: %+v", after[0].Usage)
	}

	cleared := int64(0)
	if _, err := fresh.UpdateProvider(id, ProviderEdit{Capacity: &cleared}); err != nil {
		t.Fatalf("clearing the capacity: %v", err)
	}
	if again, _ := fresh.ProviderStatuses(t.Context()); again[0].Capacity != 0 {
		t.Errorf("capacity = %d after being cleared", again[0].Capacity)
	}

	negative := int64(-1)
	if _, err := fresh.UpdateProvider(id, ProviderEdit{Capacity: &negative}); err == nil {
		t.Error("an account was allowed to hold a negative number of bytes")
	}
}

// Only the backends with no other way of answering can be counted, and a
// folder on a disk is not one of them: statfs already knows.
func TestMeasureProviderRefusesABackendThatCanAnswer(t *testing.T) {
	v, _ := newTestVault(t, 1)

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	if statuses[0].Measurable {
		t.Error("a local folder says it needs counting")
	}
	if _, err := v.MeasureProvider(t.Context(), statuses[0].ID); err == nil {
		t.Error("a local folder was counted rather than asked")
	}
	if _, err := v.MeasureProvider(t.Context(), "no-such-account"); err == nil {
		t.Error("an account that is not connected was measured")
	}
}
