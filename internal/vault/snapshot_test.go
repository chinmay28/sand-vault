package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/snapshot"
)

// Each key in these tests is a few kilobytes of work rather than half a
// gigabyte.
func init() {
	snapshotKDFParams = crypto.Argon2Params{Time: 1, Memory: 64, Threads: 1}
}

// snapshotsOnDisk is the snapshot part files in a folder, by name.
func snapshotsOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if snapshot.IsPartName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func takeTo(t *testing.T, v *Vault, dir string, target SnapshotTarget, partSize int64) *SnapshotReport {
	t.Helper()
	report, err := v.TakeSnapshot(context.Background(), MainScope, dir, SnapshotRequest{
		Target: target, Password: "snap-password", PartSize: partSize,
	})
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	return report
}

func restoreFrom(t *testing.T, v *Vault, dest string, target SnapshotTarget, name string, replace bool) *SnapshotReport {
	t.Helper()
	report, err := v.RestoreSnapshot(context.Background(), MainScope, dest, RestoreRequest{
		Target: target, Name: name, Password: "snap-password", Replace: replace,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	return report
}

// A folder snapshotted to one cloud comes back from it whole after the folder
// is gone: every file's bytes, its modified time, and the empty folders.
func TestSnapshotToACloudAndBack(t *testing.T) {
	v, roots := newTestVault(t, 3)
	store(t, v, "/photos", "a.jpg", "aaa")
	store(t, v, "/photos/2019", "b.jpg", "bbbb")
	store(t, v, "/elsewhere", "c.txt", "not in the folder")
	if err := v.Mkdir(MainScope, "/photos/empty"); err != nil {
		t.Fatal(err)
	}
	taken := time.Date(2019, 8, 3, 10, 22, 0, 0, time.UTC)
	if _, err := v.Retime(MainScope, []FileTime{{Path: "/photos/a.jpg", Mod: taken}}); err != nil {
		t.Fatalf("Retime: %v", err)
	}

	target := SnapshotTarget{Account: accountIDs(t, v)[1]}
	report := takeTo(t, v, "/photos", target, 0)

	if report.Files != 2 || report.Folders != 2 || report.Bytes != 7 || report.Where != "cloud-b" {
		t.Errorf("the report says %+v", report)
	}
	if got := snapshotsOnDisk(t, roots[1]); len(got) != 1 || got[0] != report.Parts[0] {
		t.Errorf("cloud-b holds %v, the report names %v", got, report.Parts)
	}
	for _, other := range []string{roots[0], roots[2]} {
		if got := snapshotsOnDisk(t, other); len(got) != 0 {
			t.Errorf("a snapshot kept on one cloud left parts on another: %v", got)
		}
	}

	if _, err := v.Rmdir(context.Background(), MainScope, "/photos", true, nil); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}

	back := restoreFrom(t, v, "/photos", target, report.Name, false)
	if back.Restored != 2 || back.Skipped+back.Kept+back.Failed != 0 || back.Stopped {
		t.Errorf("the restore says %+v", back)
	}
	if back.Of == nil || back.Of.Path != "/photos" {
		t.Errorf("the restore does not say what the snapshot was of: %+v", back.Of)
	}
	if read(t, v, "/photos/a.jpg") != "aaa" || read(t, v, "/photos/2019/b.jpg") != "bbbb" {
		t.Error("the files came back different")
	}
	if !v.FolderExists(MainScope, "/photos/empty") {
		t.Error("the empty folder did not come back")
	}
	if got := v.manifest.ByPath("/photos/a.jpg").ModifiedAt; !SameModTime(got, taken) {
		t.Errorf("a.jpg came back modified %v, want %v", got, taken)
	}
	if read(t, v, "/elsewhere/c.txt") != "not in the folder" {
		t.Error("a file outside the folder was disturbed")
	}
}

// Restored somewhere else, the snapshot's tree lands in that folder rather
// than under the old folder's name.
func TestSnapshotRestoresIntoAnotherFolder(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/photos/2019", "b.jpg", "bbbb")
	target := SnapshotTarget{Account: accountIDs(t, v)[0]}
	report := takeTo(t, v, "/photos", target, 0)

	restoreFrom(t, v, "/old/copy", target, report.Name, false)
	if read(t, v, "/old/copy/2019/b.jpg") != "bbbb" {
		t.Error("the file is not where a restore into /old/copy should put it")
	}
	if read(t, v, "/photos/2019/b.jpg") != "bbbb" {
		t.Error("the original was disturbed")
	}
}

// Over the limit, a snapshot on a cloud is several objects, none over it, and
// restores from them.
func TestSnapshotOnACloudSplitsIntoParts(t *testing.T) {
	v, roots := newTestVault(t, 3)
	noise := make([]byte, 3<<20)
	rand.Read(noise)
	store(t, v, "/big", "noise.bin", string(noise))

	target := SnapshotTarget{Account: accountIDs(t, v)[0]}
	report := takeTo(t, v, "/big", target, SnapshotMinPartSize)
	if len(report.Parts) < 3 {
		t.Fatalf("3 MB of noise in 1 MB parts made %d parts", len(report.Parts))
	}
	for _, name := range report.Parts {
		info, err := os.Stat(filepath.Join(roots[0], name))
		if err != nil {
			t.Fatalf("part %s is not on the cloud: %v", name, err)
		}
		if info.Size() > SnapshotMinPartSize {
			t.Errorf("%s is %d bytes, over the limit", name, info.Size())
		}
	}

	sets, err := v.ListSnapshots(context.Background(), target)
	if err != nil || len(sets) != 1 || !sets[0].Complete || len(sets[0].Parts) != len(report.Parts) {
		t.Fatalf("the listing says %+v, %v", sets, err)
	}

	restoreFrom(t, v, "/back", target, report.Name, false)
	if !bytes.Equal([]byte(read(t, v, "/back/noise.bin")), noise) {
		t.Error("the file came back different")
	}
}

// A machine takes a snapshot in a folder of its own, as plain files, and
// gives it back.
func TestSnapshotToAMachineAndBack(t *testing.T) {
	v, id, root := importFixture(t)
	noise := make([]byte, 2<<20)
	rand.Read(noise)
	store(t, v, "/docs", "noise.bin", string(noise))
	store(t, v, "/docs/letters", "one.txt", "dear sir")

	target := SnapshotTarget{Machine: id, Dir: "backups/snaps"}
	report := takeTo(t, v, "/docs", target, SnapshotMinPartSize)

	dir := filepath.Join(root, "backups", "snaps")
	onMachine := snapshotsOnDisk(t, dir)
	if len(onMachine) != len(report.Parts) || len(onMachine) < 2 {
		t.Fatalf("the machine holds %v, the report names %v", onMachine, report.Parts)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(onMachine) {
		t.Errorf("something besides the parts was left in the folder: %v", entries)
	}
	if !strings.Contains(report.Where, "vps") || !strings.Contains(report.Where, "backups/snaps") {
		t.Errorf("the report says it is kept at %q", report.Where)
	}

	if _, err := v.Rmdir(context.Background(), MainScope, "/docs", true, nil); err != nil {
		t.Fatal(err)
	}
	back := restoreFrom(t, v, "/docs", target, report.Name, false)
	if back.Restored != 2 || back.Failed != 0 {
		t.Errorf("the restore says %+v", back)
	}
	if !bytes.Equal([]byte(read(t, v, "/docs/noise.bin")), noise) || read(t, v, "/docs/letters/one.txt") != "dear sir" {
		t.Error("the files came back different")
	}
}

// With no limit, a machine gets one file however big the snapshot is.
func TestSnapshotOnAMachineIsOneFileByDefault(t *testing.T) {
	v, id, root := importFixture(t)
	noise := make([]byte, 2<<20)
	rand.Read(noise)
	store(t, v, "/docs", "noise.bin", string(noise))

	report := takeTo(t, v, "/docs", SnapshotTarget{Machine: id}, 0)
	if len(report.Parts) != 1 || !strings.HasSuffix(report.Parts[0], report.Name+snapshot.Extension) {
		t.Fatalf("got parts %v", report.Parts)
	}
	if got := snapshotsOnDisk(t, root); len(got) != 1 {
		t.Errorf("the machine holds %v", got)
	}
}

// A restore passes over what is already here, leaves a different file alone
// unless told to replace it, and never stores a second copy beside either.
func TestRestoreRespectsWhatIsAlreadyHere(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/p", "same.txt", "unchanged")
	store(t, v, "/p", "edited.txt", "as snapshotted")
	store(t, v, "/p", "gone.txt", "will be deleted")
	target := SnapshotTarget{Account: accountIDs(t, v)[0]}
	report := takeTo(t, v, "/p", target, 0)

	if _, _, err := v.Upload(context.Background(), MainScope, "/p", "edited.txt", []byte("edited since"), UploadOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Delete(context.Background(), v.manifest.ByPath("/p/gone.txt").ID); err != nil {
		t.Fatal(err)
	}

	back := restoreFrom(t, v, "/p", target, report.Name, false)
	if back.Restored != 1 || back.Skipped != 1 || back.Kept != 1 || back.Failed != 0 {
		t.Fatalf("the restore says %+v", back)
	}
	if len(back.Results) != 1 || !back.Results[0].Kept || back.Results[0].Path != "/p/edited.txt" {
		t.Errorf("the kept file should have a line of its own: %+v", back.Results)
	}
	if read(t, v, "/p/edited.txt") != "edited since" {
		t.Error("a different file was written over without being asked")
	}
	if read(t, v, "/p/gone.txt") != "will be deleted" {
		t.Error("the deleted file did not come back")
	}
	if n := len(v.manifest.Descendants("/p")); n != 3 {
		t.Errorf("/p holds %d files after the restore, want 3 — nothing stored beside", n)
	}

	replaced := restoreFrom(t, v, "/p", target, report.Name, true)
	if replaced.Restored != 1 || replaced.Skipped != 2 || replaced.Kept != 0 {
		t.Fatalf("the replacing restore says %+v", replaced)
	}
	if read(t, v, "/p/edited.txt") != "as snapshotted" {
		t.Error("Replace did not put the snapshot's file back")
	}
	if n := len(v.manifest.Descendants("/p")); n != 3 {
		t.Errorf("/p holds %d files after replacing, want 3", n)
	}
}

// The wrong password restores nothing and says what was wrong.
func TestRestoreWithTheWrongPassword(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/p", "a.txt", "aaa")
	target := SnapshotTarget{Account: accountIDs(t, v)[0]}
	report := takeTo(t, v, "/p", target, 0)

	_, err := v.RestoreSnapshot(context.Background(), MainScope, "/q", RestoreRequest{
		Target: target, Name: report.Name, Password: "not it",
	})
	if !errors.Is(err, snapshot.ErrPassword) {
		t.Fatalf("got %v, want ErrPassword", err)
	}
	if v.FolderExists(MainScope, "/q") {
		t.Error("a folder was made for a restore that never opened")
	}

	if _, err := v.InspectSnapshot(context.Background(), target, report.Name, "not it"); !errors.Is(err, snapshot.ErrPassword) {
		t.Errorf("inspecting with the wrong password: %v", err)
	}
	info, err := v.InspectSnapshot(context.Background(), target, report.Name, "snap-password")
	if err != nil || info.Path != "/p" || info.Files != 1 || info.Bytes != 3 {
		t.Errorf("inspecting says %+v, %v", info, err)
	}
}

// The vault's own password does not open a snapshot, and the snapshot's
// parts are sealed: neither a file's name nor its contents are in them.
func TestSnapshotIsSealedUnderItsOwnPassword(t *testing.T) {
	v, roots := newTestVault(t, 3)
	store(t, v, "/taxes", "return-2025.txt", strings.Repeat("account 12345678 ", 100))
	target := SnapshotTarget{Account: accountIDs(t, v)[0]}
	report := takeTo(t, v, "/taxes", target, 0)

	if _, err := v.InspectSnapshot(context.Background(), target, report.Name, testPassword); !errors.Is(err, snapshot.ErrPassword) {
		t.Errorf("the vault's password opened the snapshot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(roots[0], report.Parts[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"return-2025", "taxes", "account 12345678"} {
		if bytes.Contains(data, []byte(plain)) || strings.Contains(report.Parts[0], plain) {
			t.Errorf("the part holds %q in the clear", plain)
		}
	}
}

// A snapshot that is stopped leaves no parts behind.
func TestStoppedSnapshotLeavesNothing(t *testing.T) {
	v, roots := newTestVault(t, 3)
	noise := make([]byte, 3<<20)
	rand.Read(noise)
	store(t, v, "/big", "noise.bin", string(noise))

	ctx, cancel := context.WithCancel(context.Background())
	_, err := v.TakeSnapshot(ctx, MainScope, "/big", SnapshotRequest{
		Target:   SnapshotTarget{Account: accountIDs(t, v)[0]},
		Password: "pw", PartSize: SnapshotMinPartSize,
		OnProgress: func(at SnapshotProgress) {
			// Once a part has been stored, so that there is something to erase.
			if at.Parts > 1 {
				cancel()
			}
		},
	})
	cancel()
	if err == nil {
		t.Fatal("a stopped snapshot reported success")
	}
	if left := snapshotsOnDisk(t, roots[0]); len(left) != 0 {
		t.Errorf("a stopped snapshot left %v", left)
	}
}

// Snapshots are listed newest first and deleted whole — and deleting one
// touches nothing else on the account.
func TestListAndDeleteSnapshots(t *testing.T) {
	v, roots := newTestVault(t, 3)
	store(t, v, "/p", "a.txt", "aaa")
	target := SnapshotTarget{Account: accountIDs(t, v)[0]}

	empty, err := v.ListSnapshots(context.Background(), target)
	if err != nil || len(empty) != 0 {
		t.Fatalf("an account with no snapshots lists %v, %v", empty, err)
	}

	report := takeTo(t, v, "/p", target, 0)
	before, _ := os.ReadDir(roots[0])

	sets, err := v.ListSnapshots(context.Background(), target)
	if err != nil || len(sets) != 1 || sets[0].Name != report.Name || sets[0].Bytes != report.Stored {
		t.Fatalf("the listing says %+v, %v", sets, err)
	}
	if err := v.DeleteSnapshot(context.Background(), target, report.Name); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	after, _ := os.ReadDir(roots[0])
	if len(after) != len(before)-1 || len(snapshotsOnDisk(t, roots[0])) != 0 {
		t.Errorf("deleting the snapshot left %d of %d objects", len(after), len(before))
	}
	if read(t, v, "/p/a.txt") != "aaa" {
		t.Error("the vault's own parts were disturbed")
	}
	if err := v.DeleteSnapshot(context.Background(), target, report.Name); err == nil {
		t.Error("deleting a snapshot that is gone reported success")
	}
}

// A machine folder nothing has been put in lists as empty rather than as an
// error.
func TestListSnapshotsOnAnUnusedMachineFolder(t *testing.T) {
	v, id, _ := importFixture(t)
	sets, err := v.ListSnapshots(context.Background(), SnapshotTarget{Machine: id, Dir: "never/made"})
	if err != nil || len(sets) != 0 {
		t.Fatalf("got %v, %v", sets, err)
	}
}

// The sweep for parts the index has lost track of does not take a snapshot's
// part for one of them.
func TestOrphanScanLeavesSnapshotsAlone(t *testing.T) {
	v, roots := newTestVault(t, 3)
	store(t, v, "/p", "a.txt", "aaa")
	takeTo(t, v, "/p", SnapshotTarget{Account: accountIDs(t, v)[0]}, 0)

	scan, err := v.ScanForOrphans(context.Background())
	if err != nil {
		t.Fatalf("ScanOrphans: %v", err)
	}
	if scan.Found || scan.Objects != 0 {
		t.Errorf("the scan took a snapshot for stray parts: %+v", scan)
	}
	if len(snapshotsOnDisk(t, roots[0])) != 1 {
		t.Error("the snapshot is gone")
	}
}

func TestSnapshotRefusals(t *testing.T) {
	v, id, _ := importFixture(t)
	store(t, v, "/p", "a.txt", "aaa")
	if err := v.Mkdir(MainScope, "/hollow"); err != nil {
		t.Fatal(err)
	}
	account := accountIDs(t, v)[0]
	ctx := context.Background()

	cases := []struct {
		name string
		dir  string
		req  SnapshotRequest
	}{
		{"no password", "/p", SnapshotRequest{Target: SnapshotTarget{Account: account}}},
		{"nowhere to keep it", "/p", SnapshotRequest{Password: "pw"}},
		{"two places", "/p", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Account: account, Machine: id}}},
		{"a folder on a cloud", "/p", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Account: account, Dir: "x"}}},
		{"an unknown account", "/p", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Account: "nope"}}},
		{"an unknown machine", "/p", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Machine: "nope"}}},
		{"outside the machine's root", "/p", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Machine: id, Dir: "../up"}}},
		{"no such folder", "/missing", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Account: account}}},
		{"an empty folder", "/hollow", SnapshotRequest{Password: "pw", Target: SnapshotTarget{Account: account}}},
		{"a part too small", "/p", SnapshotRequest{Password: "pw", PartSize: 10, Target: SnapshotTarget{Account: account}}},
		{"a part too big for a cloud", "/p", SnapshotRequest{Password: "pw", PartSize: SnapshotCloudMaxPartSize + 1, Target: SnapshotTarget{Account: account}}},
	}
	for _, c := range cases {
		if _, err := v.TakeSnapshot(ctx, MainScope, c.dir, c.req); err == nil {
			t.Errorf("%s: the snapshot was taken", c.name)
		}
	}
}

func TestPartSizeFor(t *testing.T) {
	cases := []struct {
		asked int64
		cloud bool
		want  int64
		ok    bool
	}{
		{0, true, SnapshotCloudDefaultPartSize, true},
		{0, false, 0, true},
		{SnapshotMinPartSize, true, SnapshotMinPartSize, true},
		{SnapshotCloudMaxPartSize, true, SnapshotCloudMaxPartSize, true},
		{SnapshotCloudMaxPartSize + 1, true, 0, false},
		{50 << 30, false, 50 << 30, true},
		{SnapshotMinPartSize - 1, false, 0, false},
		{-1, false, 0, false},
	}
	for _, c := range cases {
		got, err := partSizeFor(c.asked, c.cloud)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("partSizeFor(%d, cloud=%v) = %d, %v; want %d, ok=%v", c.asked, c.cloud, got, err, c.want, c.ok)
		}
	}
}

// A locked vault takes and restores nothing.
func TestSnapshotNeedsAnUnlockedVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/p", "a.txt", "aaa")
	account := accountIDs(t, v)[0]
	v.Lock()

	_, err := v.TakeSnapshot(context.Background(), MainScope, "/p", SnapshotRequest{
		Target: SnapshotTarget{Account: account}, Password: "pw",
	})
	if !errors.Is(err, ErrLocked) {
		t.Errorf("taking: got %v, want ErrLocked", err)
	}
	if _, err := v.ListSnapshots(context.Background(), SnapshotTarget{Account: account}); !errors.Is(err, ErrLocked) {
		t.Errorf("listing: got %v, want ErrLocked", err)
	}
}
