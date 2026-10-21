package vault

import (
	"bytes"
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// A re-encryption must never hold the file. The old move fetched the whole
// thing into one slice and scattered it from there, which on a film the size
// of the machine's memory was the kernel killing the service mid-move. This is
// the test that would have failed then: the cost of moving a file has to be
// the chunk window, whatever the file weighs.
//
// The file is made bigger than everything a move legitimately holds — the
// upload window of a few chunks and their encoded parts, the reader's one
// chunk, a small cache — so the bound is plain: the peak must stay well under
// the file's own size. The old path could not: it held the file and then some.
func TestReEncryptingDoesNotHoldTheWholeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates hundreds of megabytes")
	}

	v, _ := chunkedVault(t, 3, 16<<20)
	v.chunks = newChunkCache(1 << 20)
	ctx := context.Background()

	size := 384 << 20
	payload := make([]byte, size)
	rand.New(rand.NewSource(7)).Read(payload)
	if _, _, err := v.Upload(ctx, MainScope, "/", "film.bin", payload, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	payload = nil
	if _, err := v.ChangePassword(ctx, testPassword, "streamed", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	var report *MigrationReport
	var err error
	peak := convertPeak(func() { report, err = v.MigrateFiles(ctx, nil) })
	if err != nil || !report.Done() {
		t.Fatalf("MigrateFiles = %+v, %v", report, err)
	}
	if limit := uint64(size) * 3 / 4; peak > limit {
		t.Errorf("moving a %d MB file peaked at %d MB of heap; want under %d MB, the chunk window and not the file",
			size>>20, peak>>20, limit>>20)
	}
}

// A multi-chunk file comes through a re-encryption byte for byte, re-cut at
// the current chunk size, and still reads — which is the whole of what "no
// corruption, no lost access" means.
func TestReEncryptingAMultiChunkFileKeepsItWhole(t *testing.T) {
	v, _ := chunkedVault(t, 3, 64<<10)
	ctx := context.Background()

	payload := make([]byte, 5*(64<<10)+123)
	rand.New(rand.NewSource(3)).Read(payload)
	entry, _, err := v.Upload(ctx, MainScope, "/", "clip.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if entry.ChunkCount < 2 {
		t.Fatalf("the fixture has %d chunk(s), want several", entry.ChunkCount)
	}

	report, err := v.ChangePassword(ctx, testPassword, "whole", true)
	if err != nil || !report.Done() {
		t.Fatalf("ChangePassword = %+v, %v", report, err)
	}
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.Hash != entry.Hash {
		t.Error("the hash on record changed, which a re-encryption must never do")
	}
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after moving: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("the re-encrypted file does not read back as it was stored")
	}
	// And at an offset, which only a chunked file can do: the new chunks
	// have to agree with the index about their size.
	r, _, err := v.OpenReadSeeker(ctx, entry.ID)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	buf := make([]byte, 1000)
	if _, err := r.Seek(int64(len(payload))-1000, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("Read at an offset: %v", err)
	}
	if !bytes.Equal(buf, payload[len(payload)-1000:]) {
		t.Error("the tail read at an offset does not match")
	}
}

// A file still in the pre-chunking format is rebuilt onto disk, not into
// memory, and comes out chunked and readable.
func TestReEncryptingALegacyFileSpoolsToDisk(t *testing.T) {
	v, _ := chunkedVault(t, 3, 64<<10)
	ctx := context.Background()

	payload := make([]byte, 3*(64<<10)+7)
	rand.New(rand.NewSource(5)).Read(payload)
	entry := addWholeEntry(t, v, "legacy-old", "old.bin", payload)
	// The fixture is added in memory; a password change re-reads the index
	// from the file, so it has to be on disk to be found.
	v.mu.Lock()
	if err := v.persistLocked(); err != nil {
		v.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	v.mu.Unlock()

	report, err := v.ChangePassword(ctx, testPassword, "spooled", true)
	if err != nil || !report.Done() {
		t.Fatalf("ChangePassword = %+v, %v", report, err)
	}
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if !after.Chunked() {
		t.Error("the legacy file did not come back chunked")
	}
	if convertSpoolsOnDisk(t, v) != 0 {
		t.Error("a spool was left behind")
	}
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("the legacy file does not read back as it was stored")
	}
}

// A file left for last is moved after everything else, by the paced step and
// by a finish-now pass, and can be put back in its turn.
func TestADeferredFileIsMovedLast(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	big := uploadSized(t, v, "/", "big.bin", 3000)
	uploadSized(t, v, "/", "small.txt", 100)
	uploadSized(t, v, "/", "medium.txt", 500)

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "deferred", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	// Smallest first would take small.txt; deferring it sends it to the back.
	small, err := v.EntryByPath(MainScope, "/small.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if path, err := v.DeferRekeyFile(small.ID); err != nil || path != "/small.txt" {
		t.Fatalf("DeferRekeyFile = %q, %v", path, err)
	}
	status, _ := v.RekeyStatus()
	if len(status.Deferred) != 1 || status.Deferred[0].Path != "/small.txt" {
		t.Fatalf("status.Deferred = %+v, want small.txt", status.Deferred)
	}

	// The choice survives a reopen: it is in the plan, not in memory.
	fresh := reopenAs(t, v, "deferred")
	now := time.Now()
	var order []string
	for i := 0; i < 3; i++ {
		step, err := fresh.RekeyStep(ctx, now)
		if err != nil || !step.Moved {
			t.Fatalf("step %d = %+v, %v", i, step, err)
		}
		order = append(order, step.Path)
	}
	if order[0] != "/medium.txt" || order[1] != "/big.bin" || order[2] != "/small.txt" {
		t.Errorf("moved in order %v, want the deferred file last", order)
	}
	if _, _, err := fresh.Fetch(ctx, big.ID); err != nil {
		t.Errorf("a moved file does not read: %v", err)
	}

	// Undeferring a file that is already moved is harmless, and the plan went
	// with the last file.
	if _, err := fresh.UndeferRekeyFile(small.ID); err != nil {
		t.Errorf("UndeferRekeyFile: %v", err)
	}
	if status, _ := fresh.RekeyStatus(); !status.Done() || status.Plan != nil {
		t.Errorf("status = %+v, want done with no plan", status)
	}
}

func TestAFinishNowPassAlsoLeavesDeferredFilesForLast(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/", "a.txt", 100)
	uploadSized(t, v, "/", "b.txt", 200)
	if _, err := v.ChangePassword(ctx, testPassword, "later", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	a, _ := v.EntryByPath(MainScope, "/a.txt")
	// No plan exists yet: deferring starts one with no budget to hold it.
	if _, err := v.DeferRekeyFile(a.ID); err != nil {
		t.Fatalf("DeferRekeyFile: %v", err)
	}
	if status, _ := v.RekeyStatus(); status.Plan == nil || status.Plan.BytesPerDay != 0 {
		t.Fatalf("status.Plan = %+v, want a plan with no budget holding the choice", status.Plan)
	}

	var order []string
	report, err := v.MigrateFiles(ctx, func(path string, done, total int) { order = append(order, path) })
	if err != nil || !report.Done() {
		t.Fatalf("MigrateFiles = %+v, %v", report, err)
	}
	if len(order) != 2 || order[0] != "/b.txt" || order[1] != "/a.txt" {
		t.Errorf("finish-now moved %v, want the deferred file last", order)
	}
}

func TestDeferringNeedsSomethingToSkip(t *testing.T) {
	v, _ := newTestVault(t, 3)
	e := uploadSized(t, v, "/", "a.txt", 100)

	if _, err := v.DeferRekeyFile(""); err == nil {
		t.Error("skipping with nothing moving was accepted")
	}
	if _, err := v.DeferRekeyFile(e.ID); err == nil {
		t.Error("deferring a file already on the current key was accepted")
	}
	if _, err := v.DeferRekeyFile("no-such-id"); err == nil {
		t.Error("deferring a file that does not exist was accepted")
	}
}

// gatedBackend is a local folder account whose uploads wait at a gate while
// the test holds it shut, so a move can be caught in the act.
type gatedBackend struct {
	provider.Provider
	gate    *gate
	entered chan<- struct{}
}

type gate struct {
	mu     sync.Mutex
	closed bool
	open   chan struct{}
}

func (g *gate) shut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		g.open = make(chan struct{})
	}
}

func (g *gate) lift() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		g.closed = false
		close(g.open)
	}
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	closed, open := g.closed, g.open
	g.mu.Unlock()
	if !closed {
		return nil
	}
	select {
	case <-open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *gatedBackend) Put(ctx context.Context, key string, data []byte) error {
	// Only a file's parts wait at the gate. The index backup is pushed on a
	// goroutine of its own with nothing to cancel it, and holding that would
	// hold the test's own cleanup forever.
	if key == BackupKey {
		return b.Provider.Put(ctx, key, data)
	}
	select {
	case b.entered <- struct{}{}:
	default:
	}
	if err := b.gate.wait(ctx); err != nil {
		return err
	}
	return b.Provider.Put(ctx, key, data)
}

var gatedOnce sync.Once

func registerGatedBackend(t *testing.T, g *gate, entered chan<- struct{}) provider.Kind {
	t.Helper()
	kind := provider.Kind("gated-local")
	gatedOnce.Do(func() {
		provider.Register(provider.Spec{
			Kind:        kind,
			Label:       "Gated folder",
			Description: "A local folder whose uploads wait at a gate the test holds.",
			Fields:      []provider.FieldSpec{{Key: "path", Label: "Path"}},
		}, func(cfg provider.Config) (provider.Provider, error) {
			local := cfg
			local.Kind = provider.KindLocal
			inner, err := provider.New(local)
			if err != nil {
				return nil, err
			}
			return &gatedBackend{Provider: inner, gate: currentGate, entered: currentEntered}, nil
		})
	})
	currentGate, currentEntered = g, entered
	return kind
}

// The registry holds one constructor per kind for the life of the process, so
// the test that is running hands it the gate to use.
var (
	currentGate    *gate
	currentEntered chan<- struct{}
)

// Skipping the file being moved stops that move where it is, leaves the file
// on the previous key and readable, charges nothing, and takes it up again
// after everything else.
func TestSkippingTheFileBeingMovedStopsIt(t *testing.T) {
	g := &gate{}
	t.Cleanup(g.lift)
	entered := make(chan struct{}, 16)
	kind := registerGatedBackend(t, g, entered)

	v, err := Open(t.TempDir() + "/vault.sand")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := v.AddProvider(ctx, provider.Config{
			Kind: kind, Name: "gated-" + string(rune('a'+i)),
			Options: map[string]string{"path": t.TempDir()},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	first := uploadSized(t, v, "/", "first.bin", 400)
	uploadSized(t, v, "/", "second.bin", 900)

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "skip", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	// Drain anything the change itself put through the gate.
	for len(entered) > 0 {
		<-entered
	}

	// Shut the gate, start a step, and catch it with first.bin half way out.
	g.shut()
	stepped := make(chan RekeyStep, 1)
	go func() {
		step, _ := v.RekeyStep(ctx, time.Now())
		stepped <- step
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the move never reached an upload")
	}
	status, _ := v.RekeyStatus()
	if !status.Running || status.Current != "/first.bin" {
		t.Fatalf("status = running %v, current %q; want first.bin moving", status.Running, status.Current)
	}

	path, err := v.DeferRekeyFile("")
	if err != nil || path != "/first.bin" {
		t.Fatalf("DeferRekeyFile = %q, %v", path, err)
	}
	var step RekeyStep
	select {
	case step = <-stepped:
	case <-time.After(10 * time.Second):
		t.Fatal("the step did not stop after the skip")
	}
	if !step.Skipped || step.Moved || step.Path != "/first.bin" || step.Warning != "" {
		t.Fatalf("step = %+v, want first.bin skipped, nothing moved, no warning", step)
	}
	g.lift()

	// Still on the previous key, still readable, nothing charged.
	if v.PendingMigration() != 2 {
		t.Errorf("%d file(s) pending, want both", v.PendingMigration())
	}
	if _, _, err := v.Fetch(ctx, first.ID); err != nil {
		t.Errorf("the skipped file does not read: %v", err)
	}
	status, _ = v.RekeyStatus()
	if status.Plan.Level != status.Plan.pacer().burst() {
		t.Errorf("the meter was charged for a skipped file: level %d", status.Plan.Level)
	}
	if status.Running || len(status.Deferred) != 1 {
		t.Errorf("status = running %v, deferred %+v; want idle with first.bin deferred", status.Running, status.Deferred)
	}

	// The rest first, then the skipped file.
	for i, want := range []string{"/second.bin", "/first.bin"} {
		step, err := v.RekeyStep(ctx, time.Now())
		if err != nil || !step.Moved || step.Path != want {
			t.Fatalf("step %d = %+v, %v; want %s moved", i, step, err, want)
		}
	}
	if v.PendingMigration() != 0 {
		t.Error("files still pending after everything moved")
	}
}
