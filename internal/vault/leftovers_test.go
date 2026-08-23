package vault

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// leave writes a file into the vault's own directory and backdates it, which is
// what a spool abandoned by a killed process looks like from the outside: the
// right name, some size, and nothing writing to it any more.
func leave(t *testing.T, v *Vault, name string, size int, age time.Duration) string {
	t.Helper()

	path := filepath.Join(filepath.Dir(v.Path()), name)
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("backdating %s: %v", name, err)
	}
	return path
}

func leftoverNamed(scan *LeftoverScan, name string) *Leftover {
	for i := range scan.Items {
		if scan.Items[i].Name == name {
			return &scan.Items[i]
		}
	}
	return nil
}

func TestLeftoverScanFindsAnAbandonedUploadSpool(t *testing.T) {
	v, _ := newTestVault(t, 3)

	leave(t, v, ".sand-upload-1611628659", 4096, 6*time.Hour)

	scan := v.ScanForLeftovers()
	if !scan.Found || scan.Files != 1 {
		t.Fatalf("a 6-hour-old upload spool was not found: %+v", scan.Items)
	}
	item := leftoverNamed(scan, ".sand-upload-1611628659")
	if item == nil || !item.Deletable {
		t.Fatalf("the spool is not offered for deletion: %+v", item)
	}
	if item.Kind != "upload" || item.Bytes != 4096 {
		t.Fatalf("the row does not describe the file: %+v", item)
	}
	if scan.DeletableBytes != 4096 {
		t.Fatalf("deletable bytes = %d, want 4096", scan.DeletableBytes)
	}
	if scan.Dir != filepath.Dir(v.Path()) {
		t.Fatalf("scan looked in %s, not in the vault's own directory", scan.Dir)
	}
}

func TestLeftoverScanIgnoresFilesSandDidNotWrite(t *testing.T) {
	v, _ := newTestVault(t, 3)

	// Everything an ordinary vault directory holds, plus a couple of things
	// that are near misses on purpose. None of them is SAND's scratch, and the
	// vault file itself is the one that must never be offered.
	for _, name := range []string{
		"vault.sand.reads",
		"notes.txt",
		".sand-upload",              // no suffix at all
		".sand-upload-",             // the suffix is empty
		".sand-upload-abc",          // not the digits CreateTemp appends
		".sand-upload-123.bak",      // something appended after it
		".sand-uploads-123",         // a prefix that is nearly right
		"sand-upload-123",           // not hidden, so not one of ours
		".sand-tmp-123",             // a provider's temporary, not the vault's
		"my.sand-upload-123",        // the prefix is not at the start
		".sand-upload-123456789012", // 12 digits is fine; 21 would not be
	} {
		leave(t, v, name, 32, 6*time.Hour)
	}

	scan := v.ScanForLeftovers()
	if scan.Files != 1 {
		names := []string{}
		for _, item := range scan.Items {
			names = append(names, item.Name)
		}
		t.Fatalf("scan offered %d file(s) — %s — and only the 12-digit one is ours",
			scan.Files, strings.Join(names, ", "))
	}
	if scan.Items[0].Name != ".sand-upload-123456789012" {
		t.Fatalf("scan picked %s", scan.Items[0].Name)
	}

	// The vault file is the whole point of the directory. It is not a leftover
	// under any reading, and it is still there afterwards.
	report := v.SweepLeftovers(nil, false)
	if report.Deleted != 1 {
		t.Fatalf("sweep erased %d file(s), want 1", report.Deleted)
	}
	if _, err := os.Stat(v.Path()); err != nil {
		t.Fatalf("the vault file did not survive the sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path()), "notes.txt")); err != nil {
		t.Fatalf("a file SAND did not write was erased: %v", err)
	}
}

func TestLeftoverScanWillNotOfferASpoolSomethingIsStillWriting(t *testing.T) {
	v, _ := newTestVault(t, 3)

	fresh := ".sand-upload-2000000000"
	leave(t, v, fresh, 1024, time.Minute)
	stale := ".sand-upload-3000000000"
	leave(t, v, stale, 2048, 3*time.Hour)

	scan := v.ScanForLeftovers()
	if scan.Files != 2 {
		t.Fatalf("found %d file(s), want both", scan.Files)
	}
	young := leftoverNamed(scan, fresh)
	if young == nil || young.Deletable {
		t.Fatalf("a spool written to a minute ago is being offered: %+v", young)
	}
	if !strings.Contains(young.Reason, "still be running") {
		t.Fatalf("the reason does not say why it is held back: %q", young.Reason)
	}
	if scan.Deletable != 1 || scan.DeletableBytes != 2048 {
		t.Fatalf("the settled one alone should be deletable: %d file(s), %d bytes",
			scan.Deletable, scan.DeletableBytes)
	}

	// And a sweep of everything leaves it exactly where it is.
	if report := v.SweepLeftovers(nil, false); report.Deleted != 1 {
		t.Fatalf("sweep erased %d file(s), want only the settled one", report.Deleted)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path()), fresh)); err != nil {
		t.Fatalf("the live-looking spool was erased: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path()), stale)); !os.IsNotExist(err) {
		t.Fatalf("the settled spool is still there: %v", err)
	}
}

func TestLeftoverScanSaysNothingAboutASpoolThisProcessIsWriting(t *testing.T) {
	v, _ := newTestVault(t, 3)

	// Old enough that the settling window would offer it, and held because
	// this process is the one filling it. Only the holder can tell the
	// difference, which is what the register is for.
	name := ".sand-upload-4000000000"
	path := leave(t, v, name, 8192, 5*time.Hour)
	v.holdSpool(path)

	if scan := v.ScanForLeftovers(); scan.Found {
		t.Fatalf("a spool this process is writing was reported as a leftover: %+v", scan.Items)
	}
	if report := v.SweepLeftovers([]string{name}, false); report.Deleted != 0 {
		t.Fatalf("a held spool was erased")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the held spool is gone: %v", err)
	}

	// Let go of it — which is what an upload that died without cleaning up
	// amounts to — and it is a leftover like any other.
	v.releaseSpool(path)
	if scan := v.ScanForLeftovers(); !scan.Found || !scan.Items[0].Deletable {
		t.Fatalf("the spool was not offered once it was released: %+v", scan)
	}
}

func TestLeftoverSweepTakesOnlyWhatWasNamed(t *testing.T) {
	v, _ := newTestVault(t, 3)

	keep := ".sand-convert-5000000000"
	go_ := ".sand-upload-6000000000"
	leave(t, v, keep, 128, 4*time.Hour)
	leave(t, v, go_, 256, 4*time.Hour)

	report := v.SweepLeftovers([]string{go_}, false)
	if report.Deleted != 1 || report.Bytes != 256 {
		t.Fatalf("sweep erased %d file(s), %d bytes; want one of 256", report.Deleted, report.Bytes)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path()), keep)); err != nil {
		t.Fatalf("an unnamed leftover was erased: %v", err)
	}

	// A name that is not a leftover of this vault's reaches nothing, however it
	// is dressed up: the sweep only ever acts on what its own fresh scan found.
	report = v.SweepLeftovers([]string{"../../etc/passwd", "vault.sand", go_}, false)
	if report.Deleted != 0 {
		t.Fatalf("sweep erased %d file(s) it should not have found", report.Deleted)
	}
	if len(report.Skipped) != 3 {
		t.Fatalf("sweep skipped %d name(s), want all three said so: %v", len(report.Skipped), report.Skipped)
	}
	if _, err := os.Stat(v.Path()); err != nil {
		t.Fatalf("the vault file did not survive: %v", err)
	}
}

func TestLeftoverSweepDryRunErasesNothing(t *testing.T) {
	v, _ := newTestVault(t, 3)

	name := ".sand-upload-7000000000"
	leave(t, v, name, 512, 4*time.Hour)

	report := v.SweepLeftovers(nil, true)
	if report.Deleted != 1 || report.Bytes != 512 {
		t.Fatalf("the dry run promised %d file(s), %d bytes", report.Deleted, report.Bytes)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path()), name)); err != nil {
		t.Fatalf("a dry run erased the file: %v", err)
	}
}

func TestLeftoverScanWeighsAWorkspaceByWhatIsInside(t *testing.T) {
	v, _ := newTestVault(t, 3)

	dir := filepath.Join(filepath.Dir(v.Path()), ".sand-git-8000000000")
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	inner := filepath.Join(dir, "objects", "pack")
	if err := os.WriteFile(inner, bytes.Repeat([]byte{'g'}, 3000), 0600); err != nil {
		t.Fatalf("writing the pack: %v", err)
	}
	old := time.Now().Add(-4 * time.Hour)
	for _, path := range []string{inner, filepath.Join(dir, "objects"), dir} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("backdating %s: %v", path, err)
		}
	}

	scan := v.ScanForLeftovers()
	item := leftoverNamed(scan, ".sand-git-8000000000")
	if item == nil || !item.Dir {
		t.Fatalf("the workspace was not found as a directory: %+v", scan.Items)
	}
	if item.Bytes != 3000 {
		t.Fatalf("the workspace weighs %d, want the 3000 bytes inside it", item.Bytes)
	}

	// A file written inside it now is a mirror still being cloned, and the
	// newest write anywhere inside is what the settling window reads.
	if err := os.WriteFile(filepath.Join(dir, "objects", "pack.tmp"), []byte("more"), 0600); err != nil {
		t.Fatalf("writing into the workspace: %v", err)
	}
	item = leftoverNamed(v.ScanForLeftovers(), ".sand-git-8000000000")
	if item == nil || item.Deletable {
		t.Fatalf("a workspace being written into is being offered: %+v", item)
	}
}

func TestLeftoverSweepRemovesAWorkspaceWholesale(t *testing.T) {
	v, _ := newTestVault(t, 3)

	dir := filepath.Join(filepath.Dir(v.Path()), ".sand-git-9000000000")
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "refs", "head"), []byte("abc"), 0600); err != nil {
		t.Fatalf("writing a ref: %v", err)
	}
	old := time.Now().Add(-4 * time.Hour)
	for _, path := range []string{filepath.Join(dir, "refs", "head"), filepath.Join(dir, "refs"), dir} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("backdating %s: %v", path, err)
		}
	}

	if report := v.SweepLeftovers(nil, false); report.Deleted != 1 {
		t.Fatalf("sweep erased %d, want the workspace", report.Deleted)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the workspace is still there: %v", err)
	}
}

func TestOrphanScanCarriesTheVaultsOwnDirectory(t *testing.T) {
	v, _ := newTestVault(t, 3)

	leave(t, v, ".sand-upload-1234567890", 2048, 4*time.Hour)

	scan := orphanScan(t, v)
	if scan.Leftovers == nil || !scan.Leftovers.Found {
		t.Fatalf("the orphan scan did not carry the local leftovers: %+v", scan.Leftovers)
	}
	if scan.Leftovers.Bytes != 2048 {
		t.Fatalf("leftover bytes = %d, want 2048", scan.Leftovers.Bytes)
	}
	// The clouds are clean, and the two answers stay separate: a spool on this
	// disk is not an abandoned part on somebody's account.
	if scan.Found {
		t.Fatalf("a leftover on disk was counted as an abandoned part: %+v", scan.Items)
	}
}

func TestAnInterruptedUploadIsFoundWhereItWasLeft(t *testing.T) {
	v, _ := newTestVault(t, 3)

	// A real spool, made the way UploadStream makes one and then abandoned the
	// way a killed process abandons one: written, and never released.
	f, size, _, err := v.spool(strings.NewReader(strings.Repeat("film", 1024)))
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	f.Close()
	if size != 4096 {
		t.Fatalf("spooled %d bytes", size)
	}
	name := filepath.Base(f.Name())

	// Still held, so it is nobody's business but the upload's.
	if scan := v.ScanForLeftovers(); scan.Found {
		t.Fatalf("a spool an upload is holding was reported: %+v", scan.Items)
	}

	// The process dies. Nothing releases it and nothing removes it; a later
	// process opening the same vault sees only a file with the right name.
	v.releaseSpool(f.Name())
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(f.Name(), old, old); err != nil {
		t.Fatalf("backdating the spool: %v", err)
	}

	scan := v.ScanForLeftovers()
	item := leftoverNamed(scan, name)
	if item == nil || !item.Deletable || item.Bytes != 4096 {
		t.Fatalf("the abandoned spool was not offered: %+v", scan.Items)
	}
	if report := v.SweepLeftovers(nil, false); report.Deleted != 1 || report.Bytes != 4096 {
		t.Fatalf("sweep freed %d bytes across %d file(s)", report.Bytes, report.Deleted)
	}
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Fatalf("the spool is still on disk: %v", err)
	}
}

func TestAFinishedUploadLeavesNothingBehind(t *testing.T) {
	v, _ := newTestVault(t, 3)

	_, _, err := v.UploadStream(context.Background(), MainScope, "/", "film.bin",
		strings.NewReader(strings.Repeat("payload", 4096)), UploadOptions{})
	if err != nil {
		t.Fatalf("UploadStream: %v", err)
	}

	// The spool is removed on the way out, and the register is empty again —
	// a hold that outlived its upload would quietly hide a real leftover
	// forever.
	if scan := v.ScanForLeftovers(); scan.Found {
		t.Fatalf("an upload that finished left %+v behind", scan.Items)
	}
	if held := v.heldSpools(); len(held) != 0 {
		t.Fatalf("the upload is still holding %v", held)
	}
}
