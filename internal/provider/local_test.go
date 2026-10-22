package provider

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestLocalPingRoundTrip is the happy path: a directory that does not exist
// yet, inside one that does, is created and proved writable.
func TestLocalPingRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root not created: %v", err)
	}
	// The probe must not be left behind — it would show up as a phantom shard
	// in List and be counted against the account.
	if _, err := os.Stat(filepath.Join(root, ".sand-write-probe")); !os.IsNotExist(err) {
		t.Fatalf("probe file left behind: %v", err)
	}
}

// localAt is a connected local account at root: built, Pinged so the folder
// exists, and ready to take parts.
func localAt(t *testing.T, root string) Provider {
	t.Helper()
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	return p
}

// TestLocalPingNeedsTheParent is the unmounted-drive case. udisks removes
// /media/<user>/<Drive> when the drive is unplugged, so a Ping that built the
// whole chain would create the account folder on the system disk under the
// empty mount point and report the account healthy. It must refuse, say why,
// and leave nothing behind.
func TestLocalPingNeedsTheParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Drive", "SANDVault")
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	err = p.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping created a folder whose parent does not exist")
	}
	if !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("missing parent lacks the unmounted-drive hint: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "Drive")); !os.IsNotExist(statErr) {
		t.Errorf("Ping created the parent: %v", statErr)
	}

	// Once the drive is there, the folder inside it is Ping's to create.
	if err := os.Mkdir(filepath.Join(base, "Drive"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with the parent present: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("root not created inside its parent: %v", err)
	}
}

// TestLocalDeleteKeepsTheRoot pins the fix for a vanished account folder.
// Keys are flat, so the object's parent directory is the root itself, and an
// older Delete pruned "the now-empty parent" after removing the last part —
// taking the folder with it, on a drive whose top level the service could
// not write to put it back.
func TestLocalDeleteKeepsTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SANDVault")
	p := localAt(t, root)
	ctx := context.Background()

	if err := p.Put(ctx, "abc-c0000000-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Delete(ctx, "abc-c0000000-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("deleting the last part removed the account folder: %v", err)
	}
	// And a second delete of the same part is still the no-op callers rely on.
	if err := p.Delete(ctx, "abc-c0000000-p1.sand"); err != nil {
		t.Errorf("deleting an already-deleted part in a folder that exists: %v", err)
	}
}

// TestLocalRejectsKeysNamingTheRoot checks that no key resolves to the
// account folder itself. "" was the dangerous one: Delete("") removed the
// root, and Put("") failed with a message about a directory.
func TestLocalRejectsKeysNamingTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SANDVault")
	p := localAt(t, root)
	ctx := context.Background()

	for _, key := range []string{"", ".", "/", "./", "//", "a/..", "/./"} {
		if err := p.Put(ctx, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) accepted a key that names the root", key)
		}
		if err := p.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) accepted a key that names the root", key)
		}
		if _, err := p.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) accepted a key that names the root", key)
		}
		if _, err := p.Stat(ctx, key); err == nil {
			t.Errorf("Stat(%q) accepted a key that names the root", key)
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			t.Fatalf("key %q took the account folder with it: %v", key, err)
		}
	}
	// A real key is still fine, so the refusal is about the root and nothing
	// wider.
	if err := p.Put(ctx, "abc-p1.sand", []byte("x")); err != nil {
		t.Errorf("Put of an ordinary key: %v", err)
	}
}

// TestLocalPutNeedsTheRoot checks that a write never creates the account
// folder. Before an account is connected the folder is Ping's to make; after
// it has gone — the drive unmounted, the folder deleted — a Put that quietly
// made a new one would be writing parts to the wrong place.
func TestLocalPutNeedsTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SANDVault")
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	ctx := context.Background()

	err = p.Put(ctx, "abc-p1.sand", []byte("part"))
	if err == nil {
		t.Fatal("Put into a folder that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("Put into a missing folder does not say what is missing: %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("Put created the account folder: %v", statErr)
	}

	// Ping makes the folder; from then on Put works.
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put after Ping: %v", err)
	}

	// The folder goes — and Put must not put it back.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("removing the root: %v", err)
	}
	if err := p.Put(ctx, "abc-p2.sand", []byte("part")); err == nil {
		t.Fatal("Put into a folder that has gone succeeded")
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("Put recreated the account folder: %v", statErr)
	}
}

// TestLocalDeleteAndListNeedTheRoot is the unmounted-drive case for the two
// operations that used to pretend. Delete swallowed every ENOENT, so with the
// folder gone every delete "succeeded" and the caller believed the parts were
// erased; List walked nothing and returned an empty listing, which the orphan
// scan reads as "this account holds nothing" rather than "could not look".
func TestLocalDeleteAndListNeedTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SANDVault")
	p := localAt(t, root)
	ctx := context.Background()
	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("removing the root: %v", err)
	}
	if err := p.Delete(ctx, "abc-p1.sand"); err == nil {
		t.Error("Delete reported success with the account folder gone")
	} else if !strings.Contains(err.Error(), root) {
		t.Errorf("Delete's error does not name the folder: %v", err)
	}
	if listed, err := p.List(ctx, ""); err == nil {
		t.Errorf("List returned %d objects, no error, with the account folder gone", len(listed))
	} else if !strings.Contains(err.Error(), root) {
		t.Errorf("List's error does not name the folder: %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("Delete or List recreated the account folder: %v", statErr)
	}

	// With the folder back, an absent object is an idempotent delete again,
	// and an empty folder lists as empty.
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Errorf("Delete of an absent object in a folder that exists: %v", err)
	}
	if listed, err := p.List(ctx, ""); err != nil || len(listed) != 0 {
		t.Errorf("List of an empty folder = %+v, %v; want nothing and no error", listed, err)
	}
}

// TestLocalPutStoresPartsPrivately checks the mode a part ends up with now
// that nothing chmods it: CreateTemp opens it 0600, and that is what the
// rename carries over. The chmod went because exFAT and vfat refuse it, which
// failed every Put on a removable drive formatted the usual way.
func TestLocalPutStoresPartsPrivately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not a Windows thing")
	}
	root := filepath.Join(t.TempDir(), "SANDVault")
	p := localAt(t, root)
	ctx := context.Background()
	if err := p.Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "abc-p1.sand"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("part mode = %o, want 0600", mode)
	}
	// Nothing but the part is left in the folder: no temp file, no probe.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "abc-p1.sand" {
		t.Errorf("folder holds %d entries, want just the part", len(entries))
	}
}

// TestSyncDirIsBestEffort checks the directory fsync after a rename never
// turns a successful write into a failure: a path that is not a directory,
// or not there at all, is simply not synced.
func TestSyncDirIsBestEffort(t *testing.T) {
	syncDir(filepath.Join(t.TempDir(), "not-there"))
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	syncDir(file)
}

// TestLocalPingUnwritableDirectory checks that a directory the process cannot
// write to fails with a message naming the path and the reason, rather than a
// bare syscall string.
func TestLocalPingUnwritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(root, 0500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	err = p.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded on an unwritable directory")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("error does not name the path: %v", err)
	}
	if !strings.Contains(err.Error(), "no write permission") {
		t.Errorf("permission failure lacks its hint: %v", err)
	}
}

// TestLocalPingHints covers the wording each failure mode gets. The point of
// the hints is that the common causes — a sandboxed service, a folder owned
// by somebody else, an unmounted drive, a folder gone from a mounted one —
// need different repairs.
func TestLocalPingHints(t *testing.T) {
	// A hardened systemd unit is the one case where nothing is wrong with the
	// drive, so it must not be described as a mount problem.
	t.Setenv("INVOCATION_ID", "1a2b3c")
	got := hint("/data/SANDVault", syscall.EROFS)
	if !strings.Contains(got, "ProtectSystem=strict") || !strings.Contains(got, "allow-local-path.sh /data/SANDVault") {
		t.Errorf("sandboxed EROFS hint = %q", got)
	}

	// Under a mount root the unit grants, so the unit is an old one: say that,
	// since re-running the installer fixes this drive and every other.
	got = hint("/media/me/Disk", syscall.EROFS)
	if !strings.Contains(got, "Re-run the installer") || !strings.Contains(got, "allow-local-path.sh /media/me/Disk") {
		t.Errorf("sandboxed EROFS hint under a mount root = %q", got)
	}

	t.Setenv("INVOCATION_ID", "")
	got = hint("/media/me/Disk", syscall.EROFS)
	if !strings.Contains(got, "read-write") {
		t.Errorf("unsandboxed EROFS hint = %q", got)
	}
	if strings.Contains(got, "ProtectSystem=strict") {
		t.Errorf("unsandboxed EROFS hint blames systemd: %q", got)
	}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"permission", fs.ErrPermission, "no write permission"},
		{"missing", fs.ErrNotExist, "not mounted"},
		{"full", syscall.ENOSPC, "filesystem is full"},
		{"not a directory", syscall.ENOTDIR, "not a directory"},
		{"unknown", syscall.EIO, ""},
	} {
		if got := hint("/media/me/Disk", tc.err); !strings.Contains(got, tc.want) || (tc.want == "" && got != "") {
			t.Errorf("hint(%s) = %q, want it to mention %q", tc.name, got, tc.want)
		}
	}

	// Which directory is missing changes the sentence: a missing parent is an
	// unmounted drive and names the parent; a missing folder inside a parent
	// that is there says the folder is gone and that a health check brings
	// it back.
	base := t.TempDir()
	got = hint(filepath.Join(base, "Drive", "SANDVault"), fs.ErrNotExist)
	if !strings.Contains(got, filepath.Join(base, "Drive")+" does not exist") || !strings.Contains(got, "not mounted") {
		t.Errorf("missing-parent hint = %q", got)
	}
	got = hint(filepath.Join(base, "SANDVault"), fs.ErrNotExist)
	if !strings.Contains(got, "folder is gone") || !strings.Contains(got, "health check") {
		t.Errorf("missing-folder hint = %q", got)
	}
}

// TestUnderMountRoot pins the match the hint shares with the unit's
// ReadWritePaths= lines: a root itself and anything below it, and nothing that
// merely starts with the same letters.
func TestUnderMountRoot(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/media", true},
		{"/media/me/Disk/SANDVault", true},
		{"/media/", true},
		{"/run/media/me/Disk", true},
		{"/mnt/nas/sand", true},
		{"/srv/sand", true},
		{"/mediakit/sand", false},
		{"/data/SANDVault", false},
		{"/home/me/sand", false},
		{"/", false},
	} {
		if got := underMountRoot(tc.path); got != tc.want {
			t.Errorf("underMountRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestLocalPingErrorUnwrapsSyscall checks that the message the UI shows leads
// with the reason rather than repeating the probe file's path.
func TestLocalPingErrorCause(t *testing.T) {
	perr := &fs.PathError{Op: "open", Path: "/x/.sand-write-probe", Err: syscall.EROFS}
	if got := cause(perr); got != "read-only file system" {
		t.Errorf("cause = %q", got)
	}
	if got := cause(errUnwrappable{}); got != "boom" {
		t.Errorf("cause of a plain error = %q", got)
	}
}

type errUnwrappable struct{}

func (errUnwrappable) Error() string { return "boom" }

func TestMountedReadOnly(t *testing.T) {
	const table = `/dev/root / ext4 rw,relatime 0 0
/dev/sdb1 /media/me/Backup ext4 ro,nosuid,nodev 0 0
/dev/sdc1 /media/me/Scratch\040Disk exfat rw,uid=1000 0 0
`
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/media/me/Backup/SANDVault", true},
		{"/media/me/Backup", true},
		{"/media/me/Scratch Disk/SANDVault", false},
		{"/var/lib/sand", false},
		// A prefix match on the string alone would wrongly claim this one,
		// which is a sibling directory of the read-only mount, not under it.
		{"/media/me/BackupOther", false},
	} {
		if got := mountedReadOnlyIn(table, tc.path); got != tc.want {
			t.Errorf("mountedReadOnlyIn(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestLocalUsageReportsTheDrive checks that a local folder answers with the
// filesystem it sits on rather than with what SAND put in it: the whole point
// of the figure is the other things on the disk, which the index cannot see.
func TestLocalUsageReportsTheDrive(t *testing.T) {
	root := t.TempDir()
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	reporter, ok := p.(UsageReporter)
	if !ok {
		t.Fatal("a local folder does not report usage")
	}

	usage, err := reporter.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Total <= 0 {
		t.Fatalf("drive reports no size: %+v", usage)
	}
	if usage.Used < 0 || usage.Used > usage.Total {
		t.Errorf("used %d is not somewhere inside %d", usage.Used, usage.Total)
	}
	if usage.Free < 0 || usage.Free > usage.Total {
		t.Errorf("free %d is not somewhere inside %d", usage.Free, usage.Total)
	}
	// An empty temp directory holds nothing, so anything used is other things
	// on the same disk — which is exactly the figure the account card needs.
	if usage.Remaining() != usage.Free {
		t.Errorf("Remaining() = %d, want the measured %d", usage.Remaining(), usage.Free)
	}
}

// TestLocalUsageBeforeTheFolderExists checks the drive still answers for a
// folder that has not been created yet — a removable disk that is mounted but
// whose SAND folder is one Ping away.
func TestLocalUsageBeforeTheFolderExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not", "there", "yet")
	p, err := newLocalProvider(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	usage, err := p.(UsageReporter).Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Total <= 0 {
		t.Fatalf("drive reports no size: %+v", usage)
	}
}

// TestUsageRemaining covers the arithmetic the account card leans on: a
// backend that measured its free space is believed, one that only has a quota
// is subtracted, and an account over its quota has no room rather than
// negative room.
func TestUsageRemaining(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		want  int64
	}{
		{"measured", Usage{Used: 300, Total: 1000, Free: 650}, 650},
		{"quota only", Usage{Used: 300, Total: 1000}, 700},
		{"over quota", Usage{Used: 1200, Total: 1000}, 0},
		{"unknown", Usage{}, 0},
	}
	for _, tc := range cases {
		if got := tc.usage.Remaining(); got != tc.want {
			t.Errorf("%s: Remaining() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
