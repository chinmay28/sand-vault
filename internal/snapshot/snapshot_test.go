package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// memStore is a Store in memory, with the same promise a real one makes: a
// part is under no name until it is committed.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte

	// failCommit, when set, fails the commit of the nth part and every one
	// after it.
	failCommit int
	commits    int
	pending    int
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

type memPart struct {
	store *memStore
	buf   bytes.Buffer
	done  bool
}

func (s *memStore) Create(ctx context.Context) (Part, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending++
	return &memPart{store: s}, nil
}

func (p *memPart) Write(b []byte) (int, error) { return p.buf.Write(b) }

func (p *memPart) Commit(name string) error {
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	if s.failCommit > 0 && s.commits >= s.failCommit {
		return errors.New("the store said no")
	}
	if _, taken := s.objects[name]; taken {
		return fmt.Errorf("%s is already there", name)
	}
	s.objects[name] = append([]byte(nil), p.buf.Bytes()...)
	p.done = true
	s.pending--
	return nil
}

func (p *memPart) Abort() error {
	if !p.done {
		p.done = true
		p.store.mu.Lock()
		p.store.pending--
		p.store.mu.Unlock()
	}
	return nil
}

func (s *memStore) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[name]
	if !ok {
		return nil, fmt.Errorf("%s is not there", name)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *memStore) List(ctx context.Context) ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Object
	for name, data := range s.objects {
		out = append(out, Object{Name: name, Size: int64(len(data))})
	}
	return out, nil
}

func (s *memStore) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, name)
	return nil
}

func (s *memStore) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name := range s.objects {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// quickKDF keeps the tests from spending half a gigabyte and a second and a
// half on every key.
var quickKDF = crypto.Argon2Params{Time: 1, Memory: 64, Threads: 1}

type testFile struct {
	path string
	data []byte
	hash string
}

// pack writes a snapshot of some folders and files and returns what Close
// said.
func pack(t *testing.T, store Store, password string, partSize int64, folders []string, files []testFile) Result {
	t.Helper()
	info := Info{
		Path: "/photos", Name: "photos", Files: len(files), Folders: len(folders),
		TakenAt: time.Date(2026, 11, 11, 19, 22, 33, 0, time.UTC),
	}
	for _, f := range files {
		info.Bytes += int64(len(f.data))
	}
	w, err := Create(context.Background(), store, password, info, Options{PartSize: partSize, KDF: quickKDF})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mod := time.Date(2019, 8, 3, 10, 22, 0, 0, time.UTC)
	for _, dir := range folders {
		if err := w.AddFolder(dir, mod); err != nil {
			t.Fatalf("AddFolder(%s): %v", dir, err)
		}
	}
	for _, f := range files {
		err := w.AddFile(File{Path: f.path, Size: int64(len(f.data)), Mod: mod, SHA256: f.hash}, bytes.NewReader(f.data))
		if err != nil {
			t.Fatalf("AddFile(%s): %v", f.path, err)
		}
	}
	result, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return result
}

// unpack reads a snapshot to its end and returns what it held.
func unpack(t *testing.T, store Store, name, password string) (Info, []string, map[string][]byte, error) {
	t.Helper()
	set, err := Find(context.Background(), store, name)
	if err != nil {
		return Info{}, nil, nil, err
	}
	r, err := Open(context.Background(), store, set, password)
	if err != nil {
		return Info{}, nil, nil, err
	}
	defer r.Close()

	var folders []string
	files := map[string][]byte{}
	for {
		item, err := r.Next()
		if err == io.EOF {
			return r.Info(), folders, files, nil
		}
		if err != nil {
			return r.Info(), folders, files, err
		}
		if item.Dir {
			folders = append(folders, item.Path)
			continue
		}
		data, err := io.ReadAll(item.Body)
		if err != nil {
			return r.Info(), folders, files, err
		}
		files[item.Path] = data
	}
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		t.Fatal(err)
	}
	return out
}

// What goes in comes back: the folders, empty ones included, every file's
// bytes, and the description at the front.
func TestSnapshotRoundTrip(t *testing.T) {
	store := newMemStore()
	files := []testFile{
		{path: "hike.jpg", data: random(t, 3000), hash: "abc123"},
		{path: "2019/notes.txt", data: []byte(strings.Repeat("the same line\n", 500))},
		{path: "2019/empty.bin", data: nil},
	}
	result := pack(t, store, "correct horse", 0, []string{"2019", "nothing-here"}, files)

	if len(result.Parts) != 1 || result.Parts[0] != result.Name+Extension {
		t.Fatalf("an unlimited snapshot should be one file named for itself, got %v", result.Parts)
	}

	info, folders, got, err := unpack(t, store, result.Name, "correct horse")
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if info.Path != "/photos" || info.Files != 3 || info.Folders != 2 || info.Version != Version {
		t.Errorf("the description came back as %+v", info)
	}
	if !strings.HasPrefix(result.Name, "snapshot-20261111T192233Z-"+info.ID[:8]) {
		t.Errorf("the name %s does not carry the time and the ID %s", result.Name, info.ID)
	}
	if strings.Join(folders, ",") != "2019,nothing-here" {
		t.Errorf("folders came back as %v", folders)
	}
	for _, f := range files {
		if !bytes.Equal(got[f.path], f.data) {
			t.Errorf("%s came back different", f.path)
		}
	}
}

// A file's hash and time ride along with it.
func TestSnapshotKeepsHashAndTime(t *testing.T) {
	store := newMemStore()
	result := pack(t, store, "pw", 0, nil, []testFile{{path: "a.txt", data: []byte("hello"), hash: "deadbeef"}})

	set, _ := Find(context.Background(), store, result.Name)
	r, err := Open(context.Background(), store, set, "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	item, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if item.SHA256 != "deadbeef" || item.Size != 5 {
		t.Errorf("item came back as %+v", item)
	}
	if want := time.Date(2019, 8, 3, 10, 22, 0, 0, time.UTC); !item.Mod.Equal(want) {
		t.Errorf("modified %v, want %v", item.Mod, want)
	}
}

// Nothing a store is handed says what the snapshot is of: not the names of
// the parts, and not a byte inside them.
func TestSnapshotLeaksNothingReadable(t *testing.T) {
	store := newMemStore()
	secret := "tax-return-2025.pdf"
	pack(t, store, "pw", 0, []string{"private-folder"}, []testFile{
		{path: secret, data: []byte(strings.Repeat("account number 12345678 ", 200))},
	})
	for _, name := range store.names() {
		if strings.Contains(name, "photos") {
			t.Errorf("the part name %s carries the folder's name", name)
		}
		data := store.objects[name]
		for _, plain := range []string{secret, "private-folder", "account number", "/photos"} {
			if bytes.Contains(data, []byte(plain)) {
				t.Errorf("%s holds %q in the clear", name, plain)
			}
		}
	}
}

// Repetitive data is stored in far less room than it takes.
func TestSnapshotCompresses(t *testing.T) {
	store := newMemStore()
	data := []byte(strings.Repeat("all work and no play makes jack a dull boy\n", 50_000))
	result := pack(t, store, "pw", 0, nil, []testFile{{path: "a.txt", data: data}, {path: "b.txt", data: data}})
	if result.Bytes > int64(len(data))/100 {
		t.Errorf("%d bytes of repetition were stored in %d", 2*len(data), result.Bytes)
	}
}

// Past the limit a snapshot becomes several files, none over the limit, named
// in order with the last saying it is the last — and they read back as one.
func TestSnapshotSplitsIntoParts(t *testing.T) {
	store := newMemStore()
	const limit = MinPartSize
	files := []testFile{
		{path: "a.bin", data: random(t, 150_000)},
		{path: "deep/b.bin", data: random(t, 90_000)},
	}
	result := pack(t, store, "pw", limit, []string{"deep"}, files)

	if len(result.Parts) < 4 {
		t.Fatalf("240 KB of noise in 64 KB parts made only %d parts", len(result.Parts))
	}
	var total int64
	for i, name := range result.Parts {
		size := int64(len(store.objects[name]))
		total += size
		if size > limit {
			t.Errorf("%s is %d bytes, over the limit of %d", name, size, limit)
		}
		last := i == len(result.Parts)-1
		if !last && size != limit {
			t.Errorf("%s is %d bytes: every part but the last should be full", name, size)
		}
		if want := partName(result.Name, i+1, last); name != want {
			t.Errorf("part %d is called %s, want %s", i+1, name, want)
		}
	}
	if total != result.Bytes {
		t.Errorf("the parts add up to %d, the result says %d", total, result.Bytes)
	}

	_, _, got, err := unpack(t, store, result.Name, "pw")
	if err != nil {
		t.Fatalf("reading the parts back: %v", err)
	}
	for _, f := range files {
		if !bytes.Equal(got[f.path], f.data) {
			t.Errorf("%s came back different", f.path)
		}
	}
}

// A snapshot with nothing in it is still one, and still opens.
func TestSnapshotOfNothing(t *testing.T) {
	store := newMemStore()
	result := pack(t, store, "pw", 0, nil, nil)
	_, folders, files, err := unpack(t, store, result.Name, "pw")
	if err != nil {
		t.Fatalf("reading an empty snapshot: %v", err)
	}
	if len(folders)+len(files) != 0 {
		t.Errorf("an empty snapshot held %v %v", folders, files)
	}
}

// The wrong password is told as that, and before anything is read out.
func TestSnapshotWrongPassword(t *testing.T) {
	store := newMemStore()
	result := pack(t, store, "right", 0, nil, []testFile{{path: "a", data: []byte("x")}})
	_, _, _, err := unpack(t, store, result.Name, "wrong")
	if !errors.Is(err, ErrPassword) {
		t.Fatalf("got %v, want ErrPassword", err)
	}
}

func TestSnapshotNeedsAPassword(t *testing.T) {
	if _, err := Create(context.Background(), newMemStore(), "", Info{}, Options{KDF: quickKDF}); err == nil {
		t.Fatal("a snapshot was started with no password")
	}
}

func TestSnapshotRefusesATinyPartSize(t *testing.T) {
	_, err := Create(context.Background(), newMemStore(), "pw", Info{}, Options{KDF: quickKDF, PartSize: 100})
	if err == nil {
		t.Fatal("a part size of 100 bytes was accepted")
	}
}

// One flipped bit anywhere past the header is caught rather than restored.
func TestSnapshotDetectsDamage(t *testing.T) {
	store := newMemStore()
	result := pack(t, store, "pw", MinPartSize, nil, []testFile{{path: "a.bin", data: random(t, 3*frameSize)}})

	// A part well past the first frame: damage inside the first frame is
	// indistinguishable from a wrong password, and is reported as either.
	victim := result.Parts[len(result.Parts)-2]
	store.objects[victim][headerSize+1000] ^= 0x01

	_, _, _, err := unpack(t, store, result.Name, "pw")
	if !errors.Is(err, ErrDamaged) {
		t.Fatalf("got %v, want ErrDamaged", err)
	}
}

// A last part cut short at a frame boundary would look whole to a cipher that
// only checked frames. The final frame is what catches it.
func TestSnapshotDetectsTruncation(t *testing.T) {
	store := newMemStore()
	result := pack(t, store, "pw", 0, nil, []testFile{{path: "a.bin", data: random(t, 3*frameSize)}})

	// Walk the frames to find where the last one starts, and cut there.
	name := result.Parts[0]
	data := store.objects[name]
	at, lastStart := headerSize, 0
	for at < len(data) {
		lastStart = at
		length := int(uint32(data[at])<<24|uint32(data[at+1])<<16|uint32(data[at+2])<<8|uint32(data[at+3])) &^ finalBit
		at += 4 + length
	}
	store.objects[name] = data[:lastStart]

	_, _, _, err := unpack(t, store, result.Name, "pw")
	if !errors.Is(err, ErrDamaged) {
		t.Fatalf("got %v, want ErrDamaged", err)
	}
}

// Parts in the wrong order, or one from another snapshot under this one's
// name, are refused by what they say about themselves.
func TestSnapshotRefusesMisplacedParts(t *testing.T) {
	t.Run("swapped", func(t *testing.T) {
		store := newMemStore()
		result := pack(t, store, "pw", MinPartSize, nil, []testFile{{path: "a.bin", data: random(t, 200_000)}})
		a, b := result.Parts[0], result.Parts[1]
		store.objects[a], store.objects[b] = store.objects[b], store.objects[a]

		_, _, _, err := unpack(t, store, result.Name, "pw")
		if !errors.Is(err, ErrDamaged) {
			t.Fatalf("got %v, want ErrDamaged", err)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		store, other := newMemStore(), newMemStore()
		result := pack(t, store, "pw", MinPartSize, nil, []testFile{{path: "a.bin", data: random(t, 200_000)}})
		foreign := pack(t, other, "pw", MinPartSize, nil, []testFile{{path: "a.bin", data: random(t, 200_000)}})
		store.objects[result.Parts[1]] = other.objects[foreign.Parts[1]]

		_, _, _, err := unpack(t, store, result.Name, "pw")
		if !errors.Is(err, ErrDamaged) {
			t.Fatalf("got %v, want ErrDamaged", err)
		}
	})
}

// A snapshot missing a part is listed as incomplete and will not open.
func TestSnapshotMissingPart(t *testing.T) {
	store := newMemStore()
	result := pack(t, store, "pw", MinPartSize, nil, []testFile{{path: "a.bin", data: random(t, 200_000)}})

	for _, drop := range []int{1, len(result.Parts) - 1} {
		kept := store.objects[result.Parts[drop]]
		delete(store.objects, result.Parts[drop])

		set, err := Find(context.Background(), store, result.Name)
		if err != nil {
			t.Fatal(err)
		}
		if set.Complete {
			t.Errorf("without part %d the snapshot still reads as complete", drop+1)
		}
		if _, err := Open(context.Background(), store, set, "pw"); !errors.Is(err, ErrIncomplete) {
			t.Errorf("without part %d: got %v, want ErrIncomplete", drop+1, err)
		}
		store.objects[result.Parts[drop]] = kept
	}
}

// A listing sorts a store's files into snapshots, newest first, and passes
// over what is not a part.
func TestListGroupsParts(t *testing.T) {
	sets := group([]Object{
		{Name: "snapshot-20260101T000000Z-aaaaaaaa.sandsnap", Size: 10},
		{Name: "snapshot-20260301T000000Z-bbbbbbbb.part002.last.sandsnap", Size: 5},
		{Name: "snapshot-20260301T000000Z-bbbbbbbb.part001.sandsnap", Size: 7},
		{Name: "snapshot-20260201T000000Z-cccccccc.part001.sandsnap", Size: 3},
		{Name: "manifest.sand", Size: 99},
		{Name: "0123456789abcdef-p1.sand", Size: 99},
		{Name: "snapshot-nonsense.sandsnap", Size: 99},
	})
	if len(sets) != 3 {
		t.Fatalf("got %d snapshots, want 3: %+v", len(sets), sets)
	}
	newest := sets[0]
	if newest.Name != "snapshot-20260301T000000Z-bbbbbbbb" || !newest.Complete || newest.Bytes != 12 || len(newest.Parts) != 2 {
		t.Errorf("the two-part snapshot came back as %+v", newest)
	}
	if !strings.Contains(newest.Parts[0], "part001") {
		t.Errorf("its parts are out of order: %v", newest.Parts)
	}
	if sets[1].Complete {
		t.Errorf("a snapshot with no last part reads as complete: %+v", sets[1])
	}
	if !sets[2].Complete || sets[2].Name != "snapshot-20260101T000000Z-aaaaaaaa" {
		t.Errorf("the one-file snapshot came back as %+v", sets[2])
	}
}

func TestPartNamesRoundTrip(t *testing.T) {
	base := baseName(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), [idSize]byte{0xab, 0x12, 0xcd, 0x34})
	for _, c := range []struct {
		part int
		last bool
	}{{1, true}, {1, false}, {2, false}, {7, true}, {1234, false}} {
		name := partName(base, c.part, c.last)
		got, ok := parseName(name)
		if !ok || got.base != base || got.part != c.part || got.last != c.last {
			t.Errorf("%s read back as %+v (%v)", name, got, ok)
		}
		if !IsPartName(name) {
			t.Errorf("%s is not recognised as a part", name)
		}
	}
	if IsPartName("holiday.jpg") || IsPartName(base+".part000"+Extension) {
		t.Error("a name that is no part's was taken for one")
	}
}

// Abandoning a snapshot leaves nothing: the part being written is discarded
// and the ones already stored are erased.
func TestAbortLeavesNothing(t *testing.T) {
	store := newMemStore()
	w, err := Create(context.Background(), store, "pw", Info{}, Options{PartSize: MinPartSize, KDF: quickKDF})
	if err != nil {
		t.Fatal(err)
	}
	data := random(t, 4*frameSize)
	if err := w.AddFile(File{Path: "a.bin", Size: int64(len(data))}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if len(store.names()) == 0 {
		t.Fatal("the test wants parts already stored before the abort")
	}
	w.Abort(context.Background())

	if left := store.names(); len(left) != 0 {
		t.Errorf("parts were left behind: %v", left)
	}
	if store.pending != 0 {
		t.Errorf("%d parts were left open", store.pending)
	}
}

// A store that refuses a part fails the snapshot rather than leaving a hole
// in it.
func TestStoreFailureFailsTheSnapshot(t *testing.T) {
	store := newMemStore()
	store.failCommit = 2
	w, err := Create(context.Background(), store, "pw", Info{}, Options{PartSize: MinPartSize, KDF: quickKDF})
	if err != nil {
		t.Fatal(err)
	}
	data := random(t, 4*frameSize)
	err = w.AddFile(File{Path: "a.bin", Size: int64(len(data))}, bytes.NewReader(data))
	if err == nil {
		_, err = w.Close()
	}
	if err == nil {
		t.Fatal("a refused part did not fail the snapshot")
	}
	w.Abort(context.Background())
	if left := store.names(); len(left) != 0 {
		t.Errorf("parts were left behind: %v", left)
	}
}

// A file that turns out shorter than it said is an error, not a short file.
func TestAddFileChecksTheLength(t *testing.T) {
	w, err := Create(context.Background(), newMemStore(), "pw", Info{}, Options{KDF: quickKDF})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile(File{Path: "a", Size: 10}, strings.NewReader("short")); err == nil {
		t.Fatal("five bytes were accepted as ten")
	}
}

func TestPathsMustStayInsideTheFolder(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "/etc/passwd", "../up", "a/../../b", "a//b", "./a"} {
		if _, err := cleanRel(bad); err == nil {
			t.Errorf("%q was accepted as a path inside the folder", bad)
		}
	}
	for _, good := range []string{"a", "a/b", "a/b/", "..hidden", "a b/c.d"} {
		if _, err := cleanRel(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}

// A cancelled context stops a snapshot being written.
func TestCancelStopsWriting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w, err := Create(ctx, newMemStore(), "pw", Info{}, Options{KDF: quickKDF})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	data := random(t, 8*frameSize)
	err = w.AddFile(File{Path: "a.bin", Size: int64(len(data))}, bytes.NewReader(data))
	if err == nil {
		_, err = w.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// A header asking for absurd key costs is refused before any are paid.
func TestHeaderCostsAreBounded(t *testing.T) {
	header := partHeader{kdf: crypto.Argon2Params{Time: 1, Memory: maxKDFMemory + 1, Threads: 1}, part: 1}
	if _, err := readHeader(bytes.NewReader(header.marshal())); !errors.Is(err, ErrDamaged) {
		t.Fatalf("got %v, want ErrDamaged", err)
	}
	if _, err := readHeader(strings.NewReader("this is not a snapshot part at all, only some text of the right length")); !errors.Is(err, ErrNotSnapshot) {
		t.Fatalf("got %v, want ErrNotSnapshot", err)
	}
}

// Written and Parts are read by a progress bar while the compressor writes
// from a goroutine of its own. Run under -race, this is the test that says
// the two may be asked at any time.
func TestProgressCanBeReadWhileWriting(t *testing.T) {
	w, err := Create(context.Background(), newMemStore(), "pw", Info{}, Options{PartSize: MinPartSize, KDF: quickKDF, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = w.Written(), w.Parts()
			}
		}
	}()

	data := random(t, 4*frameSize)
	if err := w.AddFile(File{Path: "a.bin", Size: int64(len(data))}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	result, err := w.Close()
	close(stop)
	<-watched
	if err != nil {
		t.Fatal(err)
	}
	if w.Written() != result.Bytes || w.Parts() != len(result.Parts) {
		t.Errorf("progress ended at %d bytes in %d parts, the result says %d in %d",
			w.Written(), w.Parts(), result.Bytes, len(result.Parts))
	}
}
