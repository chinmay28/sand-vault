package vault

import (
	"context"
	"errors"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/checksums"
)

func ledgerOf(t *testing.T, v *Vault, scope Scope) (*Entry, map[string]string) {
	t.Helper()
	e, err := v.EntryByPath(scope, ChecksumsPath)
	if err != nil {
		t.Fatalf("no ledger in %q: %v", scope, err)
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
	return e, out
}

func putIn(t *testing.T, v *Vault, scope Scope, name string, data []byte) *Entry {
	t.Helper()
	e, _, err := v.Upload(context.Background(), scope, "/", name, data, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload %s: %v", name, err)
	}
	return copyEntry(e)
}

// Each vault inside the file keeps its own ledger, of its own files, on its
// own default clouds — and neither says anything about the other's.
func TestEachSubVaultKeepsItsOwnLedgerOnItsOwnClouds(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)
	vaultWide, own := []string{ids[0], ids[1], ids[2]}, []string{ids[3], ids[4], ids[5]}
	if err := v.SetDefaultAccounts(vaultWide); err != nil {
		t.Fatal(err)
	}
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))
	if err := v.SetDefaultsIn(sub, own, archive.Scheme{}); err != nil {
		t.Fatal(err)
	}

	open, secret := noise(700, 1), noise(900, 2)
	putIn(t, v, MainScope, "open.bin", open)
	putIn(t, v, sub, "secret.bin", secret)

	// The timer's call writes both.
	if err := v.WriteChecksumLedgers(ctx); err != nil {
		t.Fatalf("WriteChecksumLedgers: %v", err)
	}
	mainEntry, mainLedger := ledgerOf(t, v, MainScope)
	subEntry, subLedger := ledgerOf(t, v, sub)

	if len(mainLedger) != 1 || mainLedger["/open.bin"] != sumOf(open) {
		t.Errorf("main ledger = %v", mainLedger)
	}
	if len(subLedger) != 1 || subLedger["/secret.bin"] != sumOf(secret) {
		t.Errorf("sub vault ledger = %v", subLedger)
	}
	if got := holders(mainEntry); !sameSet(got, vaultWide) {
		t.Errorf("the main ledger is on %v, want the vault's default %v", got, vaultWide)
	}
	if got := holders(subEntry); !sameSet(got, own) {
		t.Errorf("the sub vault's ledger is on %v, want its own default %v", got, own)
	}

	// A change in the sub vault alone rewrites the sub vault's ledger alone.
	more := noise(300, 3)
	putIn(t, v, sub, "more.bin", more)
	for i := 0; i < 3; i++ { // until it has gone quiet
		if err := v.WriteChecksumLedgers(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if again, _ := ledgerOf(t, v, MainScope); again.ID != mainEntry.ID {
		t.Error("the main ledger was rewritten for a change in a sub vault")
	}
	if _, subLedger = ledgerOf(t, v, sub); subLedger["/more.bin"] != sumOf(more) || len(subLedger) != 2 {
		t.Errorf("sub vault ledger after the change = %v", subLedger)
	}
}

// A shut sub vault is passed over by the timer rather than failing it, and
// its ledger catches up once it is open again.
func TestALockedSubVaultIsSkippedAndCatchesUpWhenOpened(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))
	putIn(t, v, MainScope, "open.bin", noise(500, 1))
	secret := noise(500, 2)
	putIn(t, v, sub, "secret.bin", secret)

	if err := v.LockSubVault(string(sub)); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteChecksumLedgers(ctx); err != nil {
		t.Fatalf("with a sub vault shut: %v", err)
	}
	if _, main := ledgerOf(t, v, MainScope); len(main) != 1 {
		t.Errorf("main ledger = %v", main)
	}
	if _, err := v.WriteChecksumLedger(ctx, sub); !errors.Is(err, ErrSubVaultLocked) {
		t.Errorf("writing a shut sub vault's ledger = %v, want ErrSubVaultLocked", err)
	}

	if err := v.UnlockSubVault(string(sub), subPassword); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteChecksumLedgers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ledger := ledgerOf(t, v, sub); ledger["/secret.bin"] != sumOf(secret) {
		t.Errorf("after reopening: %v", ledger)
	}
}

// The jobs walk the vault they were pointed at and no other, and a file in a
// sub vault that has no checksum is given one there.
func TestChecksumJobsStayInsideTheirSubVault(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	ctx := context.Background()
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))
	putIn(t, v, MainScope, "open.bin", noise(500, 1))
	data := noise(6000, 2)
	lacked := putIn(t, v, sub, "secret.bin", data)
	putIn(t, v, sub, "other.bin", noise(500, 3))

	v.mu.Lock()
	v.subs[string(sub)].manifest.ByID(lacked.ID).Hash = ""
	err := v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	survey, err := v.SurveyChecksums(sub, "/")
	if err != nil || survey.Files != 2 || survey.With != 1 || survey.Without != 1 {
		t.Fatalf("survey = %+v, %v", survey, err)
	}
	computed, err := v.ComputeChecksums(ctx, sub, "/", ChecksumOptions{})
	if err != nil || computed.Computed != 1 || computed.Vault != sub {
		t.Fatalf("compute = %+v, %v", computed, err)
	}
	if got := hashOf(t, v, lacked.ID); got != sumOf(data) {
		t.Errorf("recorded %q in the sub vault, want the file's checksum", got)
	}
	if computed.Ledger == nil || !computed.Ledger.Written {
		t.Fatalf("the sub vault's ledger was not written: %+v", computed)
	}
	if _, ledger := ledgerOf(t, v, sub); ledger["/secret.bin"] != sumOf(data) || len(ledger) != 2 {
		t.Errorf("sub vault ledger = %v", ledger)
	}
	// The main vault was not walked, and got no ledger from a job that was
	// not about it.
	if _, err := v.EntryByPath(MainScope, ChecksumsPath); err == nil {
		t.Error("a compute in a sub vault wrote the main vault's ledger")
	}

	verified, err := v.VerifyChecksums(ctx, sub, "/", ChecksumOptions{})
	if err != nil || verified.Files != 2 || verified.Matched != 2 {
		t.Fatalf("verify = %+v, %v", verified, err)
	}
}

// Locking a sub vault under a running job ends the job as stopped, saying
// why — not as a list of files that "could not be read".
func TestLockingASubVaultStopsItsJobCleanly(t *testing.T) {
	for _, kind := range []ChecksumKind{ChecksumVerify, ChecksumCompute} {
		t.Run(string(kind), func(t *testing.T) {
			v, _ := chunkedVault(t, 3, 4096)
			sub := Scope(createSubVault(t, v, "Taxes", subPassword))
			var ids []string
			for _, name := range []string{"a.bin", "b.bin", "c.bin", "d.bin"} {
				ids = append(ids, putIn(t, v, sub, name, noise(3000, int64(len(ids)+1))).ID)
			}
			if kind == ChecksumCompute {
				v.mu.Lock()
				for _, id := range ids {
					v.subs[string(sub)].manifest.ByID(id).Hash = ""
				}
				err := v.persistLocked()
				v.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			v.chunks.clear()

			// Held, let through the first file, held again — then the sub
			// vault is shut and the job let go into it.
			ctl := NewChecksumControl(0)
			ctl.Pause()
			files := make(chan int, 256)
			done := make(chan *ChecksumReport, 1)
			go func() {
				report, err := v.runChecksums(context.Background(), kind, sub, "/", ChecksumOptions{
					Control:    ctl,
					OnProgress: func(p ChecksumProgress) { files <- p.File },
				})
				if err != nil {
					t.Errorf("the job ended in an error: %v", err)
				}
				done <- report
			}()
			ctl.Resume()
			for f := range files {
				if f >= 2 {
					break
				}
			}
			ctl.Pause()
			if err := v.LockSubVault(string(sub)); err != nil {
				t.Fatal(err)
			}
			ctl.Resume()

			report := <-done
			if !report.Stopped || report.StoppedBy != "the sub vault was locked" {
				t.Fatalf("report = %+v", report)
			}
			if report.Unreadable != 0 || len(report.Findings) != 0 {
				t.Errorf("a shut vault was reported as damage: %+v", report.Findings)
			}
			if report.Checked < 1 || report.Checked >= 4 {
				t.Errorf("checked = %d, want what it reached before the lock", report.Checked)
			}
			if report.LedgerError != "" {
				t.Errorf("tried to write the ledger of a shut vault: %s", report.LedgerError)
			}

			// What a compute recorded before the lock is in the sub vault's
			// index, and is there when it is opened again.
			if kind == ChecksumCompute {
				if err := v.UnlockSubVault(string(sub), subPassword); err != nil {
					t.Fatal(err)
				}
				kept := 0
				for _, id := range ids {
					if hashOf(t, v, id) != "" {
						kept++
					}
				}
				if kept != report.Computed || kept < 1 {
					t.Errorf("%d checksums kept, the report says %d were recorded", kept, report.Computed)
				}
			}
		})
	}
}

// The deep check and its repair end the same way.
func TestLockingASubVaultStopsTheDeepCheckCleanly(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	ctx := context.Background()
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))
	bad := putIn(t, v, sub, "a.bin", noise(3000, 1))
	putIn(t, v, sub, "b.bin", noise(3000, 2))
	v.AwaitBackupSync()
	flipByte(t, shardFile(t, roots, bad, 0, 2))
	v.chunks.clear()

	whole, err := v.CheckFolder(ctx, sub, "/", CheckOptions{})
	if err != nil || whole.Checked != 2 || whole.Repairable != 1 || whole.Flagged[0].Vault != sub {
		t.Fatalf("check in a sub vault = %+v, %v", whole, err)
	}

	// Shut as the check reaches its second file.
	locked := false
	report, err := v.CheckFolder(ctx, sub, "/", CheckOptions{
		OnProgress: func(p CheckProgress) {
			if p.File == 2 && !locked {
				locked = true
				if err := v.LockSubVault(string(sub)); err != nil {
					t.Error(err)
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("CheckFolder: %v", err)
	}
	if !report.Stopped || report.StoppedBy != "the sub vault was locked" || report.Checked != 1 || report.Skipped != 0 {
		t.Fatalf("report = %+v", report)
	}

	// And a repair asked of a shut sub vault does not fail file by file.
	repair, err := v.RepairChecked(ctx, whole.Flagged, nil)
	if err != nil || !repair.Stopped || repair.StoppedBy == "" || repair.Failed != 0 {
		t.Fatalf("repair = %+v, %v", repair, err)
	}

	// Open again, the repair goes through.
	if err := v.UnlockSubVault(string(sub), subPassword); err != nil {
		t.Fatal(err)
	}
	if repair, err = v.RepairChecked(ctx, whole.Flagged, nil); err != nil || repair.Repaired != 1 {
		t.Fatalf("repair once reopened = %+v, %v", repair, err)
	}
}
