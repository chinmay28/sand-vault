package vault

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// stallingBackend is a cloud that takes its time answering, so a test can put
// something in the middle of an import rather than racing it.
//
// A real account does exactly this: the import's slow phase is phase 2, which
// pings every account and asks each what it holds, and on a fresh install with
// four clouds behind a domestic connection that is minutes, not seconds.
type stallingBackend struct {
	cfg    provider.Config
	gate   <-chan struct{}
	pinged chan<- struct{}
	// Shared across every account the kit restores — each gets its own
	// backend, and only the first one through announces it.
	once *sync.Once
}

func (b *stallingBackend) Config() provider.Config                   { return b.cfg }
func (b *stallingBackend) Put(context.Context, string, []byte) error { return nil }
func (b *stallingBackend) Delete(context.Context, string) error      { return nil }
func (b *stallingBackend) Get(context.Context, string) ([]byte, error) {
	return nil, provider.ErrNotFound
}
func (b *stallingBackend) Stat(context.Context, string) (provider.ObjectInfo, error) {
	return provider.ObjectInfo{}, provider.ErrNotFound
}
func (b *stallingBackend) List(context.Context, string) ([]provider.ObjectInfo, error) {
	return nil, nil
}

func (b *stallingBackend) Ping(ctx context.Context) error {
	b.once.Do(func() { close(b.pinged) })
	select {
	case <-b.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func registerStallingBackend(t *testing.T, gate <-chan struct{}, pinged chan<- struct{}) provider.Kind {
	t.Helper()

	once := &sync.Once{}

	kind := provider.Kind("stalls-on-ping")
	provider.Register(provider.Spec{
		Kind:        kind,
		Label:       "Slow Cloud",
		Description: "A backend that does not answer until the test says so.",
		Fields:      []provider.FieldSpec{{Key: "path", Label: "Path"}},
	}, func(cfg provider.Config) (provider.Provider, error) {
		return &stallingBackend{cfg: cfg, gate: gate, pinged: pinged, once: once}, nil
	})
	return kind
}

// An import must not lose the keys out from under itself.
//
// ImportKit unlocks the vault in phase 1 and does not hand out a session until
// phase 7 has finished, so for the whole of the slow middle there is an
// unlocked vault that nothing is holding open. Anything that locks in that
// window — the server's idle sweep is the one that actually does it, every
// minute, because a fresh install has no session for it to count — takes the
// data key away before phase 3 installs the index.
//
// What that leaves on disk is the worst possible shape: a vault under the new
// password with every account restored and not one file in it.
func TestImportKitSurvivesALockInFlight(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("keep me\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.Mkdir(MainScope, "/papers"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	// The kit's account is rewritten to a backend that stalls, so the lock
	// lands in phase 2 every time rather than whenever the scheduler feels
	// like it.
	gate := make(chan struct{})
	pinged := make(chan struct{})
	kind := registerStallingBackend(t, gate, pinged)
	kit := openTestKit(t, zipped, fingerprint.Code)
	for i := range kit.Accounts {
		kit.Accounts[i].Kind = kind
	}

	restored := freshVault(t)

	type outcome struct {
		report *KitImportReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "the new password"})
		done <- outcome{report, err}
	}()

	// Wait until the import is genuinely mid-flight — phase 1 has written the
	// vault and phase 2 is out at the accounts — then do what the idle sweep
	// does.
	select {
	case <-pinged:
	case <-time.After(30 * time.Second):
		t.Fatal("the import never reached the accounts")
	}
	if !restored.Unlocked() {
		t.Fatal("the vault is locked before the lock under test — phase 1 did not adopt the kit")
	}
	restored.Lock()
	close(gate)

	var got outcome
	select {
	case got = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the import never returned")
	}

	if got.err != nil {
		t.Fatalf("a lock in flight killed the import: %v", got.err)
	}
	if errors.Is(got.err, ErrLocked) {
		t.Fatal("the import returned ErrLocked")
	}
	if got.report.Files != 1 {
		t.Errorf("Files = %d, want 1 — the index was not installed", got.report.Files)
	}

	// The hold delays the lock, it does not swallow it: the sweep asked for
	// the keys to go, and once the import is done they go.
	if restored.Unlocked() {
		t.Error("the lock that arrived mid-import was dropped rather than deferred")
	}

	// The half-built vault is the real damage: accounts restored, tree empty.
	// Whatever happened to the keys, what is on disk has to be all of the kit
	// or none of it.
	if !restored.Unlocked() {
		if err := restored.Unlock("the new password"); err != nil {
			t.Fatalf("Unlock after the import: %v", err)
		}
	}
	listing, err := restored.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List after the import: %v", err)
	}
	if len(listing.Files) == 0 && len(listing.Folders) == 0 {
		t.Error("the recovered vault has the accounts and none of the files — " +
			"this is the shape a user reports as \"my clouds came back but there is nothing in them\"")
	}
}
