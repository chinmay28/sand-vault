package vault

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// dawdlingBackend is a cloud that answers every ping, but only after a while
// — which is what Proton's client looks like from here, since each command is
// a process that signs in and decrypts its way to the folder before it can say
// the folder is there. Nothing is wrong with it; it is slow.
type dawdlingBackend struct {
	cfg    provider.Config
	takes  time.Duration
	budget time.Duration
}

func (b *dawdlingBackend) Config() provider.Config                   { return b.cfg }
func (b *dawdlingBackend) Put(context.Context, string, []byte) error { return nil }
func (b *dawdlingBackend) Delete(context.Context, string) error      { return nil }
func (b *dawdlingBackend) Get(context.Context, string) ([]byte, error) {
	return nil, provider.ErrNotFound
}
func (b *dawdlingBackend) Stat(context.Context, string) (provider.ObjectInfo, error) {
	return provider.ObjectInfo{}, provider.ErrNotFound
}
func (b *dawdlingBackend) List(context.Context, string) ([]provider.ObjectInfo, error) {
	return nil, nil
}

func (b *dawdlingBackend) Ping(ctx context.Context) error {
	select {
	case <-time.After(b.takes):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// budgetedDawdler is a dawdlingBackend that has said how long it needs.
type budgetedDawdler struct{ *dawdlingBackend }

func (b budgetedDawdler) PingBudget() time.Duration { return b.budget }

// registerDawdlers registers two kinds of slow cloud, alike in every way but
// one: the first says it needs longer than the default deadline, the second
// says nothing.
func registerDawdlers(t *testing.T, takes, budget time.Duration) (declares, silent provider.Kind) {
	t.Helper()

	declares = provider.Kind("dawdles-and-says-so")
	provider.Register(provider.Spec{
		Kind:        declares,
		Label:       "Slow Cloud That Says So",
		Description: "A backend that takes its time and has declared as much.",
		Fields:      []provider.FieldSpec{{Key: "path", Label: "Path"}},
	}, func(cfg provider.Config) (provider.Provider, error) {
		return budgetedDawdler{&dawdlingBackend{cfg: cfg, takes: takes, budget: budget}}, nil
	})

	silent = provider.Kind("dawdles-quietly")
	provider.Register(provider.Spec{
		Kind:        silent,
		Label:       "Slow Cloud",
		Description: "A backend that takes its time and has not said so.",
		Fields:      []provider.FieldSpec{{Key: "path", Label: "Path"}},
	}, func(cfg provider.Config) (provider.Provider, error) {
		return &dawdlingBackend{cfg: cfg, takes: takes}, nil
	})
	return declares, silent
}

// A backend that has said its pings take minutes is given minutes. The health
// check gave every account the same twenty seconds, and a Proton account —
// whose client needs longer than that on an ordinary day — was filed as
// unreachable at every sweep: "proton-drive filesystem: context deadline
// exceeded" in the sidebar, red, for an account that was fine. Now the sweep
// waits as long as the backend asked for, and only an account that has not
// asked is held to the default.
func TestASweepGivesASlowBackendTheTimeItAskedFor(t *testing.T) {
	v, _ := newTestVault(t, 1)
	v.AwaitBackupSync()

	was := healthPingTimeout
	healthPingTimeout = 100 * time.Millisecond
	t.Cleanup(func() { healthPingTimeout = was })

	// Both answer after four times the default deadline; one has asked for
	// twenty times it.
	declares, silent := registerDawdlers(t, 400*time.Millisecond, 2*time.Second)
	for _, kind := range []provider.Kind{declares, silent} {
		cfg := provider.Config{Kind: kind, Name: string(kind), Options: map[string]string{"path": "/"}}
		if _, err := v.AddProvider(context.Background(), cfg); err != nil {
			t.Fatalf("AddProvider(%s): %v", kind, err)
		}
	}
	v.AwaitBackupSync()

	report, err := v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	if report.Healthy != 2 || report.Unhealthy != 1 {
		t.Fatalf("got %d healthy, %d unhealthy; want 2 and 1: %+v", report.Healthy, report.Unhealthy, report.Clouds)
	}
	for _, c := range report.Clouds {
		switch c.Kind {
		case declares:
			if !c.Healthy {
				t.Errorf("the backend that asked for %v was cut off: %s", 2*time.Second, c.Error)
			}
		case silent:
			if c.Healthy {
				t.Error("the backend that asked for nothing was given more than the default")
			}
			if !strings.Contains(c.Error, context.DeadlineExceeded.Error()) {
				t.Errorf("the silent backend's error is %q, which is not the deadline", c.Error)
			}
		}
	}
}

// The sidebar's listing stops waiting before the ping does, and says how much
// longer it is waiting — which for a slow backend is the time it asked for,
// not the default it was never held to.
func TestListingNamesTheBudgetItIsStillWaitingOn(t *testing.T) {
	v, _ := newTestVault(t, 1)
	v.AwaitBackupSync()

	was := healthPingTimeout
	healthPingTimeout = 100 * time.Millisecond
	t.Cleanup(func() { healthPingTimeout = was })
	wasWait := statusWait
	statusWait = 50 * time.Millisecond
	t.Cleanup(func() { statusWait = wasWait })

	declares, _ := registerDawdlers(t, 400*time.Millisecond, 2*time.Second)
	cfg := provider.Config{Kind: declares, Name: "patient", Options: map[string]string{"path": "/"}}
	added, err := v.AddProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	v.AwaitBackupSync()

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	for _, st := range statuses {
		if st.ID != added.ID {
			continue
		}
		if st.Online {
			t.Fatal("a backend that takes 400ms answered a 50ms listing")
		}
		if !strings.Contains(st.Error, "still waiting up to 2s") {
			t.Errorf("the slow account's error is %q, which does not name the 2s it asked for", st.Error)
		}
	}

	// And the ping behind the listing ran to that budget, so the health record
	// heard the account answer.
	v.AwaitStatusPings()
	report, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	for _, c := range report.Clouds {
		if c.ID == added.ID && !c.Healthy {
			t.Errorf("the slow account was filed as down (%s) though it answered within its budget", c.Error)
		}
	}
}
