package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/checksums"
)

// forgetChecksum blanks a file's recorded hash, which is what a file
// recovered without its index looks like.
func forgetChecksum(t *testing.T, v *Vault, id string) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.manifest.ByID(id).Hash = ""
	if err := v.persistLocked(); err != nil {
		t.Fatal(err)
	}
}

func hashOf(t *testing.T, v *Vault, id string) string {
	t.Helper()
	e, err := v.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	return e.Hash
}

func readLedger(t *testing.T, v *Vault) map[string]string {
	t.Helper()
	e, err := v.EntryByPath(MainScope, ChecksumsPath)
	if err != nil {
		t.Fatalf("no ledger: %v", err)
	}
	data, _, err := v.Fetch(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := checksums.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range lines {
		out[l.Path] = l.Sum
	}
	return out
}

func sumOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestLedgerListsEveryFileByPathAndIsLeftAloneWhenNothingChanged(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	ctx := context.Background()
	a, b := noise(5000, 1), noise(100, 2)
	upload(t, v, "a.bin", a)
	if err := v.Mkdir(MainScope, "/deep/er"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Upload(ctx, MainScope, "/deep/er", "b, with a comma.bin", b, UploadOptions{}); err != nil {
		t.Fatal(err)
	}

	info, err := v.WriteChecksumLedger(ctx, MainScope)
	if err != nil || !info.Written || info.Entries != 2 {
		t.Fatalf("first write: %+v, %v", info, err)
	}
	got := readLedger(t, v)
	want := map[string]string{"/a.bin": sumOf(a), "/deep/er/b, with a comma.bin": sumOf(b)}
	if len(got) != 2 || got["/a.bin"] != want["/a.bin"] || got["/deep/er/b, with a comma.bin"] != want["/deep/er/b, with a comma.bin"] {
		t.Fatalf("ledger = %v, want %v", got, want)
	}

	// Unchanged: the same file stays, under the same ID.
	again, err := v.WriteChecksumLedger(ctx, MainScope)
	if err != nil || again.Written || again.ID != info.ID {
		t.Fatalf("second write: %+v, %v", again, err)
	}

	// A move changes a path, so the ledger is written again and follows it.
	e, _ := v.EntryByPath(MainScope, "/a.bin")
	if _, err := v.Move(ctx, e.ID, "/deep", "moved.bin"); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteChecksumLedgers(ctx); err != nil {
		t.Fatal(err)
	}
	got = readLedger(t, v)
	if got["/deep/moved.bin"] != sumOf(a) || got["/a.bin"] != "" {
		t.Fatalf("after the move: %v", got)
	}
	// And the timer's call goes quiet once it has caught up.
	before, _ := v.EntryByPath(MainScope, ChecksumsPath)
	for i := 0; i < 3; i++ {
		if err := v.WriteChecksumLedgers(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := v.EntryByPath(MainScope, ChecksumsPath); after.ID != before.ID {
		t.Error("the ledger was rewritten with nothing changed")
	}
}

func TestAnEmptyVaultGetsNoLedger(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	info, err := v.WriteChecksumLedger(context.Background(), MainScope)
	if err != nil || info.Stored || info.Written {
		t.Fatalf("got %+v, %v", info, err)
	}
}

func TestVerifyFindsWhatDoesNotMatchAndWhatCannotBeRead(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	ctx := context.Background()
	if err := v.Mkdir(MainScope, "/d/sub"); err != nil {
		t.Fatal(err)
	}
	put := func(dir, name string, data []byte) *Entry {
		e, _, err := v.Upload(ctx, MainScope, dir, name, data, UploadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return copyEntry(e)
	}
	put("/d", "good.bin", noise(9000, 1))
	put("/d/sub", "deeper.bin", noise(3000, 2))
	wrong := put("/d", "wrong.bin", noise(3000, 3))
	gone := put("/d/sub", "gone.bin", noise(3000, 4))
	none := put("/d", "none.bin", noise(3000, 5))
	put("/", "outside.bin", noise(3000, 6))
	v.AwaitBackupSync()

	// A checksum that is not the file's, a file two of whose three shards are
	// damaged, and a file with no checksum at all.
	v.mu.Lock()
	v.manifest.ByID(wrong.ID).Hash = sumOf([]byte("something else"))
	v.mu.Unlock()
	flipByte(t, shardFile(t, roots, gone, 0, 1))
	flipByte(t, shardFile(t, roots, gone, 0, 2))
	forgetChecksum(t, v, none.ID)
	v.chunks.clear()

	var last ChecksumProgress
	report, err := v.VerifyChecksums(ctx, MainScope, "/d", ChecksumOptions{
		OnProgress: func(p ChecksumProgress) { last = p },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 4 || report.Checked != 4 || report.Matched != 2 ||
		report.Mismatched != 1 || report.Unreadable != 1 || report.NoChecksum != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	for _, f := range report.Findings {
		switch f.ID {
		case wrong.ID:
			if f.State != ChecksumMismatch || f.Got != sumOf(noise(3000, 3)) || f.Expected == f.Got {
				t.Errorf("wrong.bin: %+v", f)
			}
		case gone.ID:
			if f.State != ChecksumUnreadable {
				t.Errorf("gone.bin: %+v", f)
			}
		default:
			t.Errorf("unexpected finding %+v", f)
		}
	}
	if last.Files != 4 || last.Bytes != last.Total {
		t.Errorf("last progress = %+v", last)
	}
	// It only reads.
	if hashOf(t, v, none.ID) != "" {
		t.Error("a verify recorded a checksum")
	}
}

func TestComputeFillsInOnlyWhatIsMissingAndWritesTheLedger(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	ctx := context.Background()
	data := noise(10_000, 1)
	had := upload(t, v, "had.bin", noise(500, 2))
	lacked := upload(t, v, "lacked.bin", data)
	forgetChecksum(t, v, lacked.ID)

	survey, err := v.SurveyChecksums(MainScope, "/")
	if err != nil || survey.With != 1 || survey.Without != 1 || survey.WithoutBytes != 10_000 {
		t.Fatalf("survey = %+v, %v", survey, err)
	}

	report, err := v.ComputeChecksums(ctx, MainScope, "/", ChecksumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 1 || report.Computed != 1 || len(report.Findings) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if got := hashOf(t, v, lacked.ID); got != sumOf(data) {
		t.Errorf("recorded %q, want %q", got, sumOf(data))
	}
	if report.Ledger == nil || !report.Ledger.Written || report.LedgerError != "" {
		t.Fatalf("ledger = %+v (%s)", report.Ledger, report.LedgerError)
	}
	ledger := readLedger(t, v)
	if ledger["/lacked.bin"] != sumOf(data) || ledger["/had.bin"] != hashOf(t, v, had.ID) || len(ledger) != 2 {
		t.Errorf("ledger = %v", ledger)
	}

	// Nothing left to do, and the ledger itself is never one of the files.
	again, err := v.ComputeChecksums(ctx, MainScope, "/", ChecksumOptions{})
	if err != nil || again.Files != 0 || again.Computed != 0 {
		t.Fatalf("second run = %+v, %v", again, err)
	}
	verify, err := v.VerifyChecksums(ctx, MainScope, "/", ChecksumOptions{})
	if err != nil || verify.Files != 2 || verify.Matched != 2 {
		t.Fatalf("verify = %+v, %v", verify, err)
	}
}

func TestRecordChecksumNeverReplacesOne(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	e := upload(t, v, "a.bin", noise(100, 1))
	other := sumOf([]byte("other"))

	if ok, err := v.RecordChecksum(e.ID, e.ArchiveID, other); ok || err != nil {
		t.Fatalf("replaced a checksum: %v, %v", ok, err)
	}
	forgetChecksum(t, v, e.ID)
	if ok, _ := v.RecordChecksum(e.ID, "another-archive", other); ok {
		t.Fatal("recorded against a placement the bytes were not read from")
	}
	if _, err := v.RecordChecksum(e.ID, e.ArchiveID, "not a checksum"); err == nil {
		t.Fatal("recorded something that is not a checksum")
	}
	if ok, err := v.RecordChecksum(e.ID, e.ArchiveID, other); !ok || err != nil || hashOf(t, v, e.ID) != other {
		t.Fatalf("did not record: %v, %v", ok, err)
	}
}

func TestTheRateIsKeptAndCanBeChangedWhileRunning(t *testing.T) {
	v, _ := chunkedVault(t, 3, 64<<10)
	upload(t, v, "a.bin", noise(600<<10, 1)) // 600 KiB
	v.chunks.clear()

	// 400 KiB/s: the last of three 256 KiB reads is owed for at about 1.5 s.
	ctl := NewChecksumControl(400 << 10)
	started := time.Now()
	report, err := v.VerifyChecksums(context.Background(), MainScope, "/", ChecksumOptions{Control: ctl})
	if err != nil || report.Matched != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if took := time.Since(started); took < 1200*time.Millisecond || took > 5*time.Second {
		t.Errorf("600 KiB at 400 KiB/s took %v", took)
	}

	// Started at a crawl and then let go: it must not serve out the crawl.
	ctl = NewChecksumControl(1 << 10)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ctl.SetRate(0)
	}()
	started = time.Now()
	if _, err := v.VerifyChecksums(context.Background(), MainScope, "/", ChecksumOptions{Control: ctl}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("a job let go after 300 ms still took %v", took)
	}
}

func TestPauseHoldsItAndCancelKeepsWhatWasDone(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		upload(t, v, name, noise(2000, int64(len(name))))
	}
	v.chunks.clear()

	ctl := NewChecksumControl(0)
	ctl.Pause()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	files := make(chan int, 64)
	done := make(chan *ChecksumReport, 1)
	go func() {
		report, err := v.VerifyChecksums(ctx, MainScope, "/", ChecksumOptions{
			Control:    ctl,
			OnProgress: func(p ChecksumProgress) { files <- p.File },
		})
		if err != nil {
			t.Errorf("VerifyChecksums: %v", err)
		}
		done <- report
	}()

	select {
	case <-done:
		t.Fatal("a paused job finished")
	case <-time.After(400 * time.Millisecond):
	}

	// Let exactly the first file through, then hold again and cancel.
	ctl.Resume()
	for f := range files {
		if f >= 2 {
			break
		}
	}
	ctl.Pause()
	cancel()
	report := <-done
	if !report.Stopped || report.Checked < 1 || report.Checked >= 3 || report.Matched != report.Checked {
		t.Fatalf("report = %+v", report)
	}
}

func TestChecksumJobsRefuseALockedVault(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	upload(t, v, "a.bin", noise(100, 1))
	v.Lock()
	if _, err := v.WriteChecksumLedger(context.Background(), MainScope); !errors.Is(err, ErrLocked) {
		t.Errorf("WriteChecksumLedger = %v", err)
	}
	if err := v.WriteChecksumLedgers(context.Background()); !errors.Is(err, ErrLocked) {
		t.Errorf("WriteChecksumLedgers = %v", err)
	}
}
