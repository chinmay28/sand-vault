package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// memBackend is an account in memory whose handling of the index backup the
// test can steer: hold the next write of it until told, fail the next write
// of it, or fail every read of it. Parts go through untouched.
type memBackend struct {
	cfg provider.Config

	mu      sync.Mutex
	objects map[string][]byte

	// holdNext makes the next backup write wait on gate, announcing itself
	// on entered first. failNext fails the next backup write instead; getErr
	// is what every backup read answers with while set.
	holdNext bool
	failNext bool
	getErr   error
	gate     chan struct{}
	entered  chan struct{}
}

func (b *memBackend) Config() provider.Config { return b.cfg }

func (b *memBackend) Put(ctx context.Context, key string, data []byte) error {
	if key == BackupKey {
		b.mu.Lock()
		hold, fail := b.holdNext, b.failNext
		b.holdNext, b.failNext = false, false
		b.mu.Unlock()
		if hold {
			b.entered <- struct{}{}
			<-b.gate
		}
		if fail {
			return errors.New("the account is offline")
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = append([]byte(nil), data...)
	return nil
}

func (b *memBackend) Get(ctx context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if key == BackupKey && b.getErr != nil {
		return nil, b.getErr
	}
	data, ok := b.objects[key]
	if !ok {
		return nil, provider.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (b *memBackend) Stat(ctx context.Context, key string) (provider.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	if !ok {
		return provider.ObjectInfo{}, provider.ErrNotFound
	}
	return provider.ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (b *memBackend) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	return nil
}

func (b *memBackend) List(ctx context.Context, prefix string) ([]provider.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []provider.ObjectInfo
	for key, data := range b.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, provider.ObjectInfo{Key: key, Size: int64(len(data))})
		}
	}
	return out, nil
}

func (b *memBackend) Ping(context.Context) error { return nil }

// backup is the index backup the account holds right now, or nil.
func (b *memBackend) backup() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.objects[BackupKey]
}

// memVault is an unlocked vault on three in-memory accounts, with a handle
// on each.
func memVault(t *testing.T) (*Vault, []*memBackend) {
	t.Helper()

	backends := map[string]*memBackend{}
	var mu sync.Mutex
	kind := provider.Kind("memory-" + t.Name())
	provider.Register(provider.Spec{
		Kind:        kind,
		Label:       "Memory",
		Description: "An account in memory, for tests.",
		Fields:      []provider.FieldSpec{{Key: "name", Label: "Name"}},
	}, func(cfg provider.Config) (provider.Provider, error) {
		mu.Lock()
		defer mu.Unlock()
		name := cfg.Options["name"]
		if b, ok := backends[name]; ok {
			b.cfg = cfg
			return b, nil
		}
		b := &memBackend{cfg: cfg, objects: map[string][]byte{},
			gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		backends[name] = b
		return b, nil
	})

	v, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)

	var out []*memBackend
	for _, name := range []string{"a", "b", "c"} {
		if _, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    kind,
			Name:    "memory-" + name,
			Options: map[string]string{"name": name},
		}); err != nil {
			t.Fatalf("AddProvider %s: %v", name, err)
		}
		mu.Lock()
		out = append(out, backends[name])
		mu.Unlock()
	}
	v.AwaitBackupSync()
	return v, out
}

// A push snapshots under the lock and then takes its time at the accounts. A
// password change landing in between sets the flag that says the copies
// must be overwritten and queues a forced push behind the slow one; the slow
// one must not be the push that clears it — the copies it wrote answer to
// the old password. Here the forced push then fails on one account, which is
// exactly when the flag has to still be standing.
func TestASlowPushUnderTheOldPasswordDoesNotClearTheForceFlag(t *testing.T) {
	v, accounts := memVault(t)
	ctx := context.Background()

	for _, b := range accounts {
		b.mu.Lock()
		b.holdNext = true
		b.mu.Unlock()
	}
	// A change to the index schedules the push that is about to be held.
	if err := v.Mkdir(MainScope, "/slow"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, b := range accounts {
		<-b.entered
	}

	// The push is at the accounts, sealed under the old password. The change
	// queues a forced push behind it.
	if _, err := v.ChangePassword(ctx, testPassword, "new password", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if !v.store.BackupNeedsForce {
		t.Fatal("the password change should have flagged the copies for overwriting")
	}

	// The forced push will lose one account.
	accounts[2].mu.Lock()
	accounts[2].failNext = true
	accounts[2].mu.Unlock()
	for _, b := range accounts {
		close(b.gate)
	}
	v.AwaitBackupSync()

	v.mu.RLock()
	flag := v.store.BackupNeedsForce
	v.mu.RUnlock()
	if !flag {
		t.Fatal("the slow push under the old password cleared the force flag; the account it lost holds a copy the vault cannot open and nothing says to overwrite it")
	}
	if _, err := OpenBackup(accounts[2].backup(), "new password"); err == nil {
		t.Fatal("the account the forced push lost should still hold the old copy")
	}

	// The next push forces again, and the account comes right.
	if warnings, err := v.SyncManifestBackup(ctx, false); err != nil || len(warnings) != 0 {
		t.Fatalf("SyncManifestBackup: %v %v", warnings, err)
	}
	if _, err := OpenBackup(accounts[2].backup(), "new password"); err != nil {
		t.Errorf("the lost account was not brought current by the next push: %v", err)
	}
	v.mu.RLock()
	flag = v.store.BackupNeedsForce
	v.mu.RUnlock()
	if flag {
		t.Error("the force flag should clear once every copy is current")
	}
}

// foreignBackup is an index backup sealed by some other vault: a real
// envelope under a key this vault does not have.
func foreignBackup(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	salt := make([]byte, 16)
	rand.Read(key)
	rand.Read(salt)
	kdf := kdfParams{Salt: base64.StdEncoding.EncodeToString(salt), Time: 1, Memory: 8, Threads: 1}
	blob, err := sealBackup(&Snapshot{Version: backupVersion, Manifest: newManifest()}, kdf, key)
	if err != nil {
		t.Fatalf("sealBackup: %v", err)
	}
	return blob
}

// A read that cannot say what an account holds must not be taken as leave to
// overwrite it: the account may hold the only backup of a vault that died.
func TestAnUndecidableGuardReadLeavesTheAccountAlone(t *testing.T) {
	v, accounts := memVault(t)
	ctx := context.Background()

	foreign := foreignBackup(t)
	stranger := accounts[1]
	stranger.mu.Lock()
	stranger.objects[BackupKey] = foreign
	stranger.getErr = errors.New("timeout")
	stranger.mu.Unlock()
	// The guard remembers per unlock, and the accounts were checked when
	// they were connected; a fresh unlock asks again.
	relock(t, v, testPassword)
	v.AwaitBackupSync()

	warnings, err := v.SyncManifestBackup(ctx, false)
	if err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "could not tell") {
		t.Errorf("warnings = %v, want one saying the account could not be checked", warnings)
	}
	if string(stranger.backup()) != string(foreign) {
		t.Fatal("a read that timed out was taken as leave to overwrite another vault's backup")
	}

	// Once the account answers, the guard sees the stranger's copy for what
	// it is and still leaves it alone — it was not marked as checked.
	stranger.mu.Lock()
	stranger.getErr = nil
	stranger.mu.Unlock()
	warnings, err = v.SyncManifestBackup(ctx, false)
	if err != nil {
		t.Fatalf("SyncManifestBackup: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "another vault's recovery backup") {
		t.Errorf("warnings = %v, want the foreign-backup refusal", warnings)
	}
	if string(stranger.backup()) != string(foreign) {
		t.Fatal("the stranger's backup was overwritten")
	}
}
