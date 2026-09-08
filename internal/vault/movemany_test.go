package vault

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// where every file in a vault is, as a set of full paths.
func filedAt(t *testing.T, v *Vault) map[string]bool {
	t.Helper()

	v.mu.RLock()
	defer v.mu.RUnlock()

	out := map[string]bool{}
	for _, e := range v.manifest.Entries {
		out[e.Path()] = true
	}
	return out
}

func moveMany(t *testing.T, v *Vault, orders ...MoveOrder) *MoveReport {
	t.Helper()

	report, err := v.MoveMany(context.Background(), orders)
	if err != nil {
		t.Fatalf("MoveMany: %v", err)
	}
	return report
}

// The whole of it: a folder's files filed into two others, in one write.
func TestMoveManyMovesEveryFileItIsGiven(t *testing.T) {
	v, _ := newTestVault(t, 3)

	one := fileDated(t, v, "/photos", "one.jpg", at("2026-01-14T09:00:00Z"))
	two := fileDated(t, v, "/photos", "two.jpg", at("2026-01-30T09:00:00Z"))
	three := fileDated(t, v, "/photos", "three.jpg", at("2025-07-04T09:00:00Z"))
	if err := v.Mkdirs(MainScope, []string{"/photos/2026/January", "/photos/2025/July"}); err != nil {
		t.Fatalf("Mkdirs: %v", err)
	}

	report := moveMany(t, v,
		MoveOrder{ID: one.ID, Dir: "/photos/2026/January"},
		MoveOrder{ID: two.ID, Dir: "/photos/2026/January"},
		MoveOrder{ID: three.ID, Dir: "/photos/2025/July"},
	)

	if report.Moved != 3 || len(report.Missing) > 0 || len(report.Refused) > 0 {
		t.Fatalf("report = %+v, want 3 moved and nothing else", report)
	}
	want := map[string]bool{
		"/photos/2026/January/one.jpg": true,
		"/photos/2026/January/two.jpg": true,
		"/photos/2025/July/three.jpg":  true,
	}
	if got := filedAt(t, v); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("files are at %v, want %v", got, want)
	}

	// And the times they were filed by are untouched, or filing the same
	// folder twice would put everything under today. Same rule as Move.
	if got := v.manifest.ByID(one.ID).ModifiedAt; !SameModTime(got, at("2026-01-14T09:00:00Z")) {
		t.Errorf("modified time = %v, want it left alone", got)
	}
}

// The write actually landed, rather than the manifest being right only in the
// memory of the process that changed it.
func TestMoveManySurvivesTheVaultBeingReopened(t *testing.T) {
	v, _ := newTestVault(t, 3)

	e := fileDated(t, v, "/in", "paper.pdf", at("2024-03-02T10:00:00Z"))
	if err := v.Mkdir(MainScope, "/filed"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	moveMany(t, v, MoveOrder{ID: e.ID, Dir: "/filed", Name: "March.pdf"})

	again := reopen(t, v)
	if got := again.manifest.ByID(e.ID); got == nil || got.Path() != "/filed/March.pdf" {
		t.Fatalf("after reopening the file is at %v, want /filed/March.pdf", got)
	}
}

// Filing a half-filed folder finishes it rather than refusing it, which is what
// makes a run that stopped part-way safe to press again.
func TestMoveManyIsIdempotent(t *testing.T) {
	v, _ := newTestVault(t, 3)

	e := fileDated(t, v, "/photos", "one.jpg", at("2026-01-14T09:00:00Z"))
	if err := v.Mkdir(MainScope, "/photos/2026"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	order := MoveOrder{ID: e.ID, Dir: "/photos/2026"}

	first := moveMany(t, v, order)
	second := moveMany(t, v, order)

	if first.Moved != 1 || second.Moved != 1 {
		t.Errorf("moved %d then %d, want 1 both times", first.Moved, second.Moved)
	}
	if len(second.Refused) > 0 {
		t.Errorf("the second run refused %v, want a file already where it belongs to be settled", second.Refused)
	}
	if got := v.manifest.ByID(e.ID).Path(); got != "/photos/2026/one.jpg" {
		t.Errorf("file is at %s, want /photos/2026/one.jpg", got)
	}
}

// An order that cannot be carried out says so and stands aside; the ones around
// it are unaffected. Anything else and one bad row in a plan of ten thousand
// would be the whole plan.
func TestMoveManyRefusesOneOrderWithoutLosingTheRest(t *testing.T) {
	v, _ := newTestVault(t, 3)

	one := fileDated(t, v, "/in", "one.jpg", at("2026-01-14T09:00:00Z"))
	two := fileDated(t, v, "/in", "two.jpg", at("2026-01-15T09:00:00Z"))
	three := fileDated(t, v, "/in", "three.jpg", at("2026-01-16T09:00:00Z"))
	fileDated(t, v, "/out", "two.jpg", at("2020-01-01T09:00:00Z"))

	report := moveMany(t, v,
		MoveOrder{ID: one.ID, Dir: "/out"},
		// A name the destination already holds.
		MoveOrder{ID: two.ID, Dir: "/out"},
		// A folder that is not there.
		MoveOrder{ID: three.ID, Dir: "/nowhere"},
		// A file that is not there.
		MoveOrder{ID: "not-a-file", Dir: "/out"},
	)

	if report.Moved != 1 {
		t.Errorf("moved %d, want 1", report.Moved)
	}
	if len(report.Refused) != 2 {
		t.Fatalf("refused %v, want two lines", report.Refused)
	}
	if !strings.Contains(report.Refused[0], "/in/two.jpg") || !strings.Contains(report.Refused[0], "already exists") {
		t.Errorf("refusal %q does not name the collision", report.Refused[0])
	}
	if !strings.Contains(report.Refused[1], "no such folder") {
		t.Errorf("refusal %q does not name the missing folder", report.Refused[1])
	}
	if len(report.Missing) != 1 || report.Missing[0] != "not-a-file" {
		t.Errorf("missing = %v, want [not-a-file]", report.Missing)
	}

	if got := filedAt(t, v); !got["/out/one.jpg"] || !got["/in/two.jpg"] || !got["/in/three.jpg"] {
		t.Errorf("files are at %v: the refused two must be exactly where they were", got)
	}
}

// The orders are applied in the order given, each against what the ones before
// it left — the same thing they would have meant one at a time.
func TestMoveManyAppliesOrdersAgainstEachOther(t *testing.T) {
	v, _ := newTestVault(t, 3)

	held := fileDated(t, v, "/in", "wanted.jpg", at("2026-01-14T09:00:00Z"))
	waiting := fileDated(t, v, "/in", "other.jpg", at("2026-01-15T09:00:00Z"))

	report := moveMany(t, v,
		// The first frees the name, the second takes it.
		MoveOrder{ID: held.ID, Name: "renamed.jpg"},
		MoveOrder{ID: waiting.ID, Name: "wanted.jpg"},
	)

	if report.Moved != 2 || len(report.Refused) > 0 {
		t.Fatalf("report = %+v, want both applied", report)
	}
	if got := v.manifest.ByID(waiting.ID).Path(); got != "/in/wanted.jpg" {
		t.Errorf("the second file is at %s, want /in/wanted.jpg", got)
	}
}

// Half an order is a whole order: a rename that does not move, and a move that
// does not rename.
func TestMoveManyLeavesTheHalfItIsNotGiven(t *testing.T) {
	v, _ := newTestVault(t, 3)

	renamed := fileDated(t, v, "/in", "before.jpg", at("2026-01-14T09:00:00Z"))
	moved := fileDated(t, v, "/in", "stays.jpg", at("2026-01-15T09:00:00Z"))
	if err := v.Mkdir(MainScope, "/out"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	moveMany(t, v,
		MoveOrder{ID: renamed.ID, Name: "after.jpg"},
		MoveOrder{ID: moved.ID, Dir: "/out"},
	)

	if got := v.manifest.ByID(renamed.ID).Path(); got != "/in/after.jpg" {
		t.Errorf("renamed file is at %s, want /in/after.jpg", got)
	}
	if got := v.manifest.ByID(moved.ID).Path(); got != "/out/stays.jpg" {
		t.Errorf("moved file is at %s, want /out/stays.jpg", got)
	}
}

// A name the vault will not store is refused for that file alone, rather than
// being sanitized into something nobody asked for or failing the batch.
func TestMoveManyRefusesAnImpossibleName(t *testing.T) {
	v, _ := newTestVault(t, 3)

	e := fileDated(t, v, "/in", "one.jpg", at("2026-01-14T09:00:00Z"))
	report := moveMany(t, v, MoveOrder{ID: e.ID, Name: ".."})

	if report.Moved != 0 || len(report.Refused) != 1 {
		t.Fatalf("report = %+v, want the one order refused", report)
	}
	if got := v.manifest.ByID(e.ID).Path(); got != "/in/one.jpg" {
		t.Errorf("file is at %s, want it where it was", got)
	}
}

func TestMoveManyOnALockedVaultRefusesEverything(t *testing.T) {
	v, _ := newTestVault(t, 3)
	e := fileDated(t, v, "/in", "one.jpg", at("2026-01-14T09:00:00Z"))
	v.Lock()

	if _, err := v.MoveMany(context.Background(), []MoveOrder{{ID: e.ID, Dir: "/"}}); err != ErrLocked {
		t.Errorf("MoveMany on a locked vault = %v, want ErrLocked", err)
	}
}

func TestMoveManyWithNothingToDoDoesNothing(t *testing.T) {
	v, _ := newTestVault(t, 3)

	report, err := v.MoveMany(context.Background(), nil)
	if err != nil {
		t.Fatalf("MoveMany: %v", err)
	}
	if report.Moved != 0 || len(report.Missing) != 0 || len(report.Refused) != 0 {
		t.Errorf("report = %+v, want an empty one", report)
	}
}

// A picture follows its file, exactly as it does through Move — the batch is
// only meant to be cheaper, not to lose anything.
func TestMoveManyCarriesTheThumbnails(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	one := fileDated(t, v, "/photos", "one.jpg", at("2026-01-14T09:00:00Z"))
	two := fileDated(t, v, "/photos", "two.jpg", at("2026-01-15T09:00:00Z"))
	stays := fileDated(t, v, "/photos", "stays.jpg", at("2026-01-16T09:00:00Z"))
	wantOne := storeThumb(t, v, one.ID, "picture-of-one")
	wantTwo := storeThumb(t, v, two.ID, "picture-of-two")
	storeThumb(t, v, stays.ID, "picture-of-stays")
	if err := v.Mkdir(MainScope, "/photos/2026"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	moveMany(t, v,
		MoveOrder{ID: one.ID, Dir: "/photos/2026"},
		MoveOrder{ID: two.ID, Dir: "/photos/2026"},
	)

	for id, want := range map[string][]byte{one.ID: wantOne, two.ID: wantTwo} {
		got, err := v.Thumb(ctx, id)
		if err != nil {
			t.Fatalf("Thumb after the move: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("thumbnail = %q, want %q", got, want)
		}
	}

	// And the folder they left has forgotten them, keeping the one that stayed.
	left := v.ThumbIDs(MainScope, "/photos")
	if len(left) != 1 || left[0] != stays.ID {
		t.Errorf("/photos still holds thumbnails %v, want only the file that stayed", left)
	}
	if arrived := v.ThumbIDs(MainScope, "/photos/2026"); len(arrived) != 2 {
		t.Errorf("/photos/2026 holds thumbnails %v, want both", arrived)
	}
}

// The point of the whole file: a pack is written once per folder, not once per
// file. This is what makes filing ten thousand photographs finish — moveThumb
// per file uploads both packs for every picture, which is quadratic in the size
// of the folder.
func TestMoveManyWritesEachFoldersThumbnailsOnce(t *testing.T) {
	v, counts := countingVault(t)

	var ids []string
	for i := 0; i < 6; i++ {
		e := fileDated(t, v, "/photos", fmt.Sprintf("photo-%d.jpg", i), at("2026-01-14T09:00:00Z"))
		storeThumb(t, v, e.ID, fmt.Sprintf("picture-%d", i))
		ids = append(ids, e.ID)
	}
	if err := v.Mkdir(MainScope, "/photos/2026"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	orders := make([]MoveOrder, 0, len(ids))
	for _, id := range ids {
		orders = append(orders, MoveOrder{ID: id, Dir: "/photos/2026"})
	}

	counts.reset()
	moveMany(t, v, orders...)
	stored := counts.puts()

	// Two folders change hands, and a pack is one archive across three
	// accounts: three parts written for the folder gaining the pictures and
	// three for the folder losing them. One at a time would be six of each.
	if stored > 2*3 {
		t.Errorf("moving %d files stored %d parts, want no more than %d — a pack per folder, not per file",
			len(ids), stored, 2*3)
	}
	if stored == 0 {
		t.Error("moving the files stored nothing at all, so the thumbnails cannot have been carried")
	}
}

// --- Mkdirs ------------------------------------------------------------

func TestMkdirsMakesEveryFolderAndItsAncestors(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdirs(MainScope, []string{"/photos/2026/January", "/photos/2026/March", "/photos/2025/July"}); err != nil {
		t.Fatalf("Mkdirs: %v", err)
	}

	for _, dir := range []string{
		"/photos", "/photos/2026", "/photos/2026/January", "/photos/2026/March",
		"/photos/2025", "/photos/2025/July",
	} {
		if !v.manifest.FolderExists(dir) {
			t.Errorf("%s was not created", dir)
		}
	}
}

// A folder already there is not an error: the plan that asks for it does not
// know which of its folders exist, and half of them usually do.
func TestMkdirsIsIdempotent(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdirs(MainScope, []string{"/photos/2026"}); err != nil {
		t.Fatalf("Mkdirs: %v", err)
	}
	if err := v.Mkdirs(MainScope, []string{"/photos/2026", "/photos/2025"}); err != nil {
		t.Fatalf("Mkdirs again: %v", err)
	}

	folders, err := v.Folders(MainScope)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	seen := map[string]int{}
	for _, f := range folders {
		seen[f]++
	}
	if seen["/photos/2026"] != 1 {
		t.Errorf("/photos/2026 appears %d times, want once", seen["/photos/2026"])
	}
}

// Either all of them or none: a list with a bad path in it must not leave the
// folders before it behind, or a refused call would still have changed the tree.
func TestMkdirsLeavesNothingBehindWhenOneIsImpossible(t *testing.T) {
	v, _ := newTestVault(t, 3)
	fileDated(t, v, "/in", "taken", at("2026-01-14T09:00:00Z"))

	err := v.Mkdirs(MainScope, []string{"/wanted", "/in/taken"})
	if err == nil {
		t.Fatal("Mkdirs over a path held by a file = nil, want an error")
	}
	if v.manifest.FolderExists("/wanted") {
		t.Error("/wanted was left behind by a refused Mkdirs")
	}
}

// --- A counting account -------------------------------------------------

// countingVault is a vault on three accounts that keep what they are given in
// memory and count what they were asked to store. It is how the pack-per-folder
// property above is measured: the index backup is written under a key of its
// own (BackupKey) and is not counted, so what is left is the archives.
func countingVault(t *testing.T) (*Vault, *putCounter) {
	t.Helper()

	kind := provider.Kind("counting")
	provider.Register(provider.Spec{
		Kind:        kind,
		Label:       "Counting Cloud",
		Description: "An account that remembers what it was given, and how often.",
		Fields:      []provider.FieldSpec{{Key: "token", Label: "Token", Required: true}},
	}, func(cfg provider.Config) (provider.Provider, error) {
		store, ok := countingStores.Load(cfg.Options["token"])
		if !ok {
			return nil, fmt.Errorf("no counting store called %q", cfg.Options["token"])
		}
		return &countingBackend{cfg: cfg, counter: store.(*putCounter)}, nil
	})

	counter := &putCounter{objects: map[string][]byte{}}
	token := t.Name()
	countingStores.Store(token, counter)
	t.Cleanup(func() { countingStores.Delete(token) })

	v, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)

	for i := 0; i < 3; i++ {
		_, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    kind,
			Name:    fmt.Sprintf("counting-%d", i),
			Options: map[string]string{"token": token},
		})
		if err != nil {
			t.Fatalf("AddProvider %d: %v", i, err)
		}
	}
	return v, counter
}

var countingStores sync.Map

type putCounter struct {
	mu      sync.Mutex
	objects map[string][]byte
	stored  int
}

func (c *putCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = 0
}

func (c *putCounter) puts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stored
}

type countingBackend struct {
	cfg     provider.Config
	counter *putCounter
}

func (b *countingBackend) Config() provider.Config { return b.cfg }

func (b *countingBackend) Put(_ context.Context, key string, data []byte) error {
	b.counter.mu.Lock()
	defer b.counter.mu.Unlock()
	// The index backup goes to every account on every write and is not what is
	// being counted here.
	if key != BackupKey {
		b.counter.stored++
	}
	b.counter.objects[b.cfg.ID+"/"+key] = append([]byte(nil), data...)
	return nil
}

func (b *countingBackend) Get(_ context.Context, key string) ([]byte, error) {
	b.counter.mu.Lock()
	defer b.counter.mu.Unlock()
	blob, ok := b.counter.objects[b.cfg.ID+"/"+key]
	if !ok {
		return nil, provider.ErrNotFound
	}
	return blob, nil
}

func (b *countingBackend) Stat(_ context.Context, key string) (provider.ObjectInfo, error) {
	b.counter.mu.Lock()
	defer b.counter.mu.Unlock()
	blob, ok := b.counter.objects[b.cfg.ID+"/"+key]
	if !ok {
		return provider.ObjectInfo{}, provider.ErrNotFound
	}
	return provider.ObjectInfo{Key: key, Size: int64(len(blob))}, nil
}

func (b *countingBackend) Delete(_ context.Context, key string) error {
	b.counter.mu.Lock()
	defer b.counter.mu.Unlock()
	delete(b.counter.objects, b.cfg.ID+"/"+key)
	return nil
}

func (b *countingBackend) List(_ context.Context, prefix string) ([]provider.ObjectInfo, error) {
	b.counter.mu.Lock()
	defer b.counter.mu.Unlock()

	var out []provider.ObjectInfo
	for key, blob := range b.counter.objects {
		key, ok := strings.CutPrefix(key, b.cfg.ID+"/")
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, provider.ObjectInfo{Key: key, Size: int64(len(blob))})
	}
	return out, nil
}

func (b *countingBackend) Ping(context.Context) error { return nil }
