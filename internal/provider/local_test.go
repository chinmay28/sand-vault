package provider

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestLocalPingRoundTrip is the happy path: a directory that does not exist
// yet is created and proved writable.
func TestLocalPingRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault", "nested")
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
// the hints is that the three common causes — a sandboxed service, a folder
// owned by somebody else, an unmounted drive — need different repairs.
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
