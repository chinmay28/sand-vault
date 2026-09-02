package vault

import (
	"path/filepath"
	"testing"
	"time"
)

// A vault that has never chosen a lifetime gets the default, and one that has
// keeps it across a reopen.
func TestLinkLifetimeDefaultsAndPersists(t *testing.T) {
	v, _ := newTestVault(t, 1)

	if got := v.LinkLifetime(); got != DefaultLinkLifetime {
		t.Fatalf("a fresh vault's links last %v, want %v", got, DefaultLinkLifetime)
	}
	if v.LinkHours() != 3 {
		t.Errorf("LinkHours = %d, want 3", v.LinkHours())
	}

	hours, err := v.SetLinkHours(12)
	if err != nil {
		t.Fatalf("SetLinkHours: %v", err)
	}
	if hours != 12 || v.LinkLifetime() != 12*time.Hour {
		t.Errorf("set to 12, read back %d / %v", hours, v.LinkLifetime())
	}

	// Kept in the clear, so it is readable again before the vault is unlocked.
	reopened, err := Open(filepath.Join(filepath.Dir(v.Path()), "vault.sand"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.LinkLifetime(); got != 12*time.Hour {
		t.Errorf("after a reopen links last %v, want 12h", got)
	}

	// Zero puts the default back.
	if _, err := v.SetLinkHours(0); err != nil {
		t.Fatalf("SetLinkHours(0): %v", err)
	}
	if v.LinkLifetime() != DefaultLinkLifetime {
		t.Errorf("zero did not restore the default: %v", v.LinkLifetime())
	}
}

// The bounds are refused, and a locked vault cannot be changed.
func TestLinkLifetimeRefusesNonsense(t *testing.T) {
	v, _ := newTestVault(t, 1)

	for _, hours := range []int{-1, MaxLinkHours + 1, 1000} {
		if _, err := v.SetLinkHours(hours); err == nil {
			t.Errorf("%d hours was accepted", hours)
		}
	}
	if _, err := v.SetLinkHours(MinLinkHours); err != nil {
		t.Errorf("the minimum was refused: %v", err)
	}
	if _, err := v.SetLinkHours(MaxLinkHours); err != nil {
		t.Errorf("the maximum was refused: %v", err)
	}

	v.Lock()
	if _, err := v.SetLinkHours(2); err == nil {
		t.Error("a locked vault took a new lifetime")
	}
	// Still readable locked, as the default or as what was chosen.
	if v.LinkLifetime() != MaxLinkHours*time.Hour {
		t.Errorf("locked, the lifetime reads %v", v.LinkLifetime())
	}
}
