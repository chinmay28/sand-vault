package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newTestICloud builds the backend over a temp folder, with the container
// check disabled — the checks it drives are about a real machine's iCloud
// Drive, and every other test here is about what happens inside the folder.
func newTestICloud(t *testing.T, root string) *icloudProvider {
	t.Helper()
	p, err := newICloudProvider(Config{Kind: KindICloud, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newICloudProvider: %v", err)
	}
	ic := p.(*icloudProvider)
	ic.containers = nil
	ic.poll = time.Millisecond
	ic.timeout = 100 * time.Millisecond
	return ic
}

// evict does to a file what macOS does when it reclaims the disk: takes the
// contents away and leaves a placeholder under a name the vault never wrote.
func evict(t *testing.T, root, key string, contents []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove: %v", err)
	}
	// The real placeholder is a small plist of metadata. Nothing reads it, so
	// its only job here is to be a couple of hundred bytes that are not the
	// shard.
	if err := os.WriteFile(stubFor(full), make([]byte, 176), 0600); err != nil {
		t.Fatalf("writing placeholder: %v", err)
	}
	_ = contents
}

// TestICloudEvictedShardIsFetched is the whole point of the backend: a part
// the Mac threw away months ago still reads back.
func TestICloudEvictedShardIsFetched(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	ctx := context.Background()

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "abc123-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	evict(t, root, "abc123-p1.sand", payload)

	asked := 0
	p.download = func(_ context.Context, path string) error {
		asked++
		// What the sync daemon does when it hands a file back.
		if err := os.WriteFile(path, payload, 0600); err != nil {
			return err
		}
		return os.Remove(stubFor(path))
	}

	got, err := p.Get(ctx, "abc123-p1.sand")
	if err != nil {
		t.Fatalf("Get on an evicted shard: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}
	if asked != 1 {
		t.Errorf("asked iCloud %d times, want 1", asked)
	}

	// And once it is back on disk, nobody is asked again.
	if _, err := p.Get(ctx, "abc123-p1.sand"); err != nil {
		t.Fatalf("Get on a materialized shard: %v", err)
	}
	if asked != 1 {
		t.Errorf("asked iCloud %d times for a shard already on disk, want 1", asked)
	}
}

// TestICloudEvictedShardIsPresent covers the two answers that decide whether a
// vault looks healthy: an evicted part is still a part, under the key it was
// stored as.
func TestICloudEvictedShardIsPresent(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	ctx := context.Background()

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "abc123-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	evict(t, root, "abc123-p1.sand", payload)

	info, err := p.Stat(ctx, "abc123-p1.sand")
	if err != nil {
		t.Fatalf("Stat on an evicted shard: %v", err)
	}
	if info.Key != "abc123-p1.sand" {
		t.Errorf("Stat key = %q", info.Key)
	}
	if info.Size != 0 {
		t.Errorf("Stat size = %d, want 0 — the placeholder's own size is not the shard's", info.Size)
	}

	objects, err := p.List(ctx, "abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "abc123-p1.sand" {
		t.Fatalf("List = %+v, want the key the vault wrote", objects)
	}
	if objects[0].Size != 0 {
		t.Errorf("List size = %d, want 0", objects[0].Size)
	}
}

// TestICloudListPrefersTheCopyOnDisk covers the moment after a download, when
// the file is back and its placeholder has not been swept up yet: one key, and
// the size of the copy that is actually here.
func TestICloudListPrefersTheCopyOnDisk(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	ctx := context.Background()

	payload := []byte("encrypted shard bytes")
	full := filepath.Join(root, "abc123-p1.sand")
	if err := os.WriteFile(full, payload, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(stubFor(full), make([]byte, 176), 0600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	objects, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("List = %+v, want one shard", objects)
	}
	if objects[0].Key != "abc123-p1.sand" || objects[0].Size != int64(len(payload)) {
		t.Errorf("List = %+v, want the shard on disk with its real size", objects[0])
	}
}

// TestICloudDeleteRemovesPlaceholder: a deleted shard must not come back in
// List as a key Get can never satisfy.
func TestICloudDeleteRemovesPlaceholder(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	ctx := context.Background()

	if err := p.Put(ctx, "abc123-p1.sand", []byte("shard")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	evict(t, root, "abc123-p1.sand", nil)

	if err := p.Delete(ctx, "abc123-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	objects, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("List after Delete = %+v, want nothing", objects)
	}
	if _, err := p.Stat(ctx, "abc123-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat after Delete = %v, want ErrNotFound", err)
	}
}

// TestICloudPutClearsPlaceholder: writing over an evicted shard leaves the
// folder describing one shard per key.
func TestICloudPutClearsPlaceholder(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	ctx := context.Background()

	if err := p.Put(ctx, "abc123-p1.sand", []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	evict(t, root, "abc123-p1.sand", nil)

	if err := p.Put(ctx, "abc123-p1.sand", []byte("new")); err != nil {
		t.Fatalf("Put over an evicted shard: %v", err)
	}
	if _, err := os.Stat(stubFor(filepath.Join(root, "abc123-p1.sand"))); !os.IsNotExist(err) {
		t.Errorf("placeholder survived the write: %v", err)
	}
	got, err := p.Get(ctx, "abc123-p1.sand")
	if err != nil || string(got) != "new" {
		t.Errorf("Get = %q, %v, want the new bytes", got, err)
	}
}

// TestICloudMissingShard: no file and no placeholder is a missing shard, and
// must not turn into a download that nobody can satisfy.
func TestICloudMissingShard(t *testing.T) {
	p := newTestICloud(t, t.TempDir())
	ctx := context.Background()
	p.download = func(context.Context, string) error {
		t.Error("asked iCloud for a shard that was never stored")
		return nil
	}

	if _, err := p.Get(ctx, "nothing-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if _, err := p.Stat(ctx, "nothing-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
}

// TestICloudDownloadThatNeverArrives: a sync daemon that takes the request and
// does nothing has to end in an error that says what is actually wrong.
func TestICloudDownloadThatNeverArrives(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	ctx := context.Background()

	if err := p.Put(ctx, "abc123-p1.sand", []byte("shard")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	evict(t, root, "abc123-p1.sand", nil)
	p.download = func(context.Context, string) error { return nil }

	_, err := p.Get(ctx, "abc123-p1.sand")
	if err == nil {
		t.Fatal("Get succeeded on a download that never landed")
	}
	if !strings.Contains(err.Error(), "evicted") {
		t.Errorf("error does not explain eviction: %v", err)
	}
}

// TestICloudGetHonoursContext: a cancelled request stops waiting rather than
// sitting out the whole download timeout.
func TestICloudGetHonoursContext(t *testing.T) {
	root := t.TempDir()
	p := newTestICloud(t, root)
	p.timeout = time.Hour

	if err := p.Put(context.Background(), "abc123-p1.sand", []byte("shard")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	evict(t, root, "abc123-p1.sand", nil)
	p.download = func(context.Context, string) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Get(ctx, "abc123-p1.sand"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
}

// TestICloudKey pins the name mapping in both directions, including the names
// that only look like placeholders.
func TestICloudKey(t *testing.T) {
	for _, tc := range []struct {
		on      string
		want    string
		evicted bool
	}{
		{"abc123-p1.sand", "abc123-p1.sand", false},
		{".abc123-p1.sand.icloud", "abc123-p1.sand", true},
		{"chunks/7/.abc123-p1.sand.icloud", "chunks/7/abc123-p1.sand", true},
		{"chunks/7/abc123-p1.sand", "chunks/7/abc123-p1.sand", false},
		// Somebody else's dotfile that happens to end in .icloud.
		{".icloud", ".icloud", false},
		{"notes.icloud", "notes.icloud", false},
		{".hidden", ".hidden", false},
	} {
		got, evicted := icloudKey(tc.on)
		if got != tc.want || evicted != tc.evicted {
			t.Errorf("icloudKey(%q) = %q, %v, want %q, %v", tc.on, got, evicted, tc.want, tc.evicted)
		}
	}

	if got := stubFor(filepath.Join("root", "abc123-p1.sand")); got != filepath.Join("root", ".abc123-p1.sand.icloud") {
		t.Errorf("stubFor = %q", got)
	}
}

// TestICloudPingRejectsFolderOutsideDrive: a writable folder is not the test.
// ~/Documents would take every shard SAND gave it and keep them all on the one
// machine that can die.
func TestICloudPingRejectsFolderOutsideDrive(t *testing.T) {
	home := t.TempDir()
	container := filepath.Join(home, "Mobile Documents")
	if err := os.MkdirAll(container, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	outside := newTestICloud(t, filepath.Join(home, "Documents", "sand"))
	outside.containers = []string{container}
	err := outside.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping accepted a folder iCloud Drive does not sync")
	}
	if !strings.Contains(err.Error(), "not inside iCloud Drive") {
		t.Errorf("error does not say why: %v", err)
	}

	inside := newTestICloud(t, filepath.Join(container, "com~apple~CloudDocs", "sand"))
	inside.containers = []string{container}
	if err := inside.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a folder inside iCloud Drive: %v", err)
	}
}

// TestICloudPingWithoutTheClient tells the two failures apart: a folder in the
// wrong place is a different repair from iCloud Drive not being turned on.
func TestICloudPingWithoutTheClient(t *testing.T) {
	home := t.TempDir()
	p := newTestICloud(t, filepath.Join(home, "Documents", "sand"))
	p.containers = []string{filepath.Join(home, "Mobile Documents")}

	err := p.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded with no iCloud Drive on the machine")
	}
	if !strings.Contains(err.Error(), "not set up on this machine") {
		t.Errorf("error does not name the real problem: %v", err)
	}
}

// TestICloudDownloadOffMac: the request needs a Mac, and says so rather than
// failing as a missing command.
func TestICloudDownloadOffMac(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this is a Mac, where the request is a real one")
	}
	err := requestICloudDownload(context.Background(), "/x/abc123-p1.sand")
	if err == nil || !strings.Contains(err.Error(), "not a Mac") {
		t.Errorf("requestICloudDownload = %v, want it to say this is not a Mac", err)
	}
}

// TestICloudRegistered: the connect dialog builds itself from the registry, so
// the spec is what makes the backend exist at all.
func TestICloudRegistered(t *testing.T) {
	spec, ok := SpecFor(KindICloud)
	if !ok {
		t.Fatal("iCloud Drive is not registered")
	}
	if len(spec.Fields) != 1 || spec.Fields[0].Key != "path" || !spec.Fields[0].Directory {
		t.Errorf("spec fields = %+v, want one browsable path", spec.Fields)
	}
	if spec.OAuth != nil {
		t.Error("iCloud Drive has no sign-in")
	}
	if _, err := New(Config{Kind: KindICloud, Options: map[string]string{"path": t.TempDir()}}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// TestICloudPingFollowsSymlinks: a Mac reaches its home directory through a
// firmlink, so the folder and the container are routinely spelled two
// different ways for the same place. Comparing the spellings would refuse a
// folder squarely inside iCloud Drive.
func TestICloudPingFollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "Volumes", "Data", "Mobile Documents")
	if err := os.MkdirAll(filepath.Join(real, "com~apple~CloudDocs"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "Mobile Documents")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The folder is named through the link, the container through the real
	// path — and the other way round.
	for _, tc := range []struct{ container, root string }{
		{real, filepath.Join(link, "com~apple~CloudDocs", "sand")},
		{link, filepath.Join(real, "com~apple~CloudDocs", "sand")},
	} {
		p := newTestICloud(t, tc.root)
		p.containers = []string{tc.container}
		if err := p.Ping(context.Background()); err != nil {
			t.Errorf("Ping(container %s, folder %s): %v", tc.container, tc.root, err)
		}
	}
}
