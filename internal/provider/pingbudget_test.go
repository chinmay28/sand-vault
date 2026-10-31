package provider

import (
	"context"
	"testing"
	"time"
)

// budgetedBackend is a backend that says how long its Ping needs.
type budgetedBackend struct {
	Provider
	budget time.Duration
}

func (b budgetedBackend) PingBudget() time.Duration { return b.budget }

// plainBackend says nothing about its Ping.
type plainBackend struct{ Provider }

func (plainBackend) Ping(context.Context) error { return nil }

// A backend that asks for longer gets it; one that asks for less, or does not
// ask, gets the default. The default is the least any account is given, so a
// backend cannot talk its way under it.
func TestPingBudgetOf(t *testing.T) {
	fallback := 20 * time.Second
	cases := []struct {
		name string
		p    Provider
		want time.Duration
	}{
		{"declares nothing", plainBackend{}, fallback},
		{"asks for longer", budgetedBackend{budget: 2 * time.Minute}, 2 * time.Minute},
		{"asks for less", budgetedBackend{budget: 5 * time.Second}, fallback},
		{"asks for exactly the default", budgetedBackend{budget: fallback}, fallback},
		{"asks for nothing", budgetedBackend{budget: 0}, fallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PingBudgetOf(tc.p, fallback); got != tc.want {
				t.Fatalf("PingBudgetOf = %v, want %v", got, tc.want)
			}
		})
	}
}

// The Proton client backend is the reason the contract exists: every command
// is a fresh process that signs in and decrypts its way to the folder, which
// misses a twenty-second deadline on an ordinary day. It must ask for more.
func TestProtonCLIDeclaresAPingBudget(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)

	if got := PingBudgetOf(p, 20*time.Second); got != ProtonCLIPingBudget {
		t.Fatalf("a Proton account is given %v to answer a ping, want %v", got, ProtonCLIPingBudget)
	}
	if ProtonCLIPingBudget < time.Minute {
		t.Fatalf("ProtonCLIPingBudget is %v, which is not the minutes the client needs", ProtonCLIPingBudget)
	}
}
