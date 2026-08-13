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
	got := hint("/media/me/Disk", syscall.EROFS)
	if !strings.Contains(got, "ProtectSystem=strict") || !strings.Contains(got, "allow-local-path.sh /media/me/Disk") {
		t.Errorf("sandboxed EROFS hint = %q", got)
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
