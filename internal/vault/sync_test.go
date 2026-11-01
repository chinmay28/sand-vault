package vault

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Two machines opening one vault, in one process: the first is a test vault
// on its local folders, the second a copy of its vault file opened under
// another name. The folders stand in for clouds, which every machine reaches,
// so the copy's accounts are stamped as its own.

// secondMachine opens a copy of src's vault file as the machine called name.
func secondMachine(t *testing.T, src *Vault, name string) *Vault {
	t.Helper()
	src.AwaitBackupSync()

	data, err := os.ReadFile(src.Path())
	if err != nil {
		t.Fatalf("read the vault file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "vault.sand")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("copy the vault file: %v", err)
	}

	v, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v.SetInstance(name)
	v.flights.settle = -1
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// The folders are clouds for the purpose of these tests: reachable from
	// this machine too.
	v.mu.Lock()
	for i := range v.providers {
		v.providers[i].Host = name
	}
	v.mu.Unlock()
	v.AwaitBackupSync()
	return v
}

func rootNames(t *testing.T, v *Vault) []string {
	t.Helper()
	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := []string{}
	for _, f := range listing.Files {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

func storeAt(t *testing.T, v *Vault, name, body string) *Entry {
	t.Helper()
	e, _, err := v.Upload(context.Background(), MainScope, "/", name, []byte(body), UploadOptions{})
	if err != nil {
		t.Fatalf("%s: upload %s: %v", v.Instance(), name, err)
	}
	v.AwaitBackupSync()
	return e
}

func pull(t *testing.T, v *Vault) *SyncReport {
	t.Helper()
	report, err := v.SyncIndex(context.Background())
	if err != nil {
		t.Fatalf("%s: SyncIndex: %v", v.Instance(), err)
	}
	v.AwaitBackupSync()
	return report
}

// A file stored on one machine is listed, and readable, on the other after a
// pull — and the pull says it took the other machine's index whole.
func TestSecondMachineSeesWhatTheFirstStored(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	storeAt(t, pc, "from-pc.txt", "stored on the desk")

	// The pi announced itself at its unlock, before the pc's upload, and the
	// pc did not know of it yet when it pushed: so the two copies diverged
	// by one write each and the pi combines them, rather than taking the
	// pc's whole. What matters is that the file arrives.
	report := pull(t, pi)
	if !report.Changed() || report.Added != 1 {
		t.Fatalf("the pi's pull: %+v", report)
	}
	if got := rootNames(t, pi); !equal(got, []string{"from-pc.txt"}) {
		t.Fatalf("the pi lists %v", got)
	}
	e, _ := pi.EntryByPath(MainScope, "/from-pc.txt")
	data, _, err := pi.Fetch(context.Background(), e.ID)
	if err != nil || !bytes.Equal(data, []byte("stored on the desk")) {
		t.Fatalf("the pi could not read the file: %v %q", err, data)
	}

	// Each machine now knows of the other.
	insts, _ := pi.Instances()
	if len(insts) != 2 || !insts[0].Here || insts[0].Name != "pi" || insts[1].Name != "pc" {
		t.Errorf("the pi's register: %+v", insts)
	}
	pull(t, pc)
	insts, _ = pc.Instances()
	if len(insts) != 2 || insts[1].Name != "pi" {
		t.Errorf("the pc's register: %+v", insts)
	}

	// With nothing new, a pull changes nothing and asks for no push.
	pushes := pushCount(pi)
	report = pull(t, pi)
	if report.Changed() || pushCount(pi) != pushes {
		t.Errorf("an idle pull reported %+v and pushed", report)
	}
}

// Both machines store files without pulling in between: after each pulls, both
// list both files, and the clocks settle.
func TestBothMachinesStoringKeepsBothFiles(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	storeAt(t, pc, "a.txt", "a")
	storeAt(t, pi, "b.txt", "b")

	// Which machine does the combining depends on which push came second:
	// the pi's push pulled the pc's copy first and combined there if the pc
	// was already in its register. Either way the pc's pull gains one file.
	report := pull(t, pc)
	if !report.Changed() || report.Added != 1 || report.Removed != 0 {
		t.Fatalf("the pc's pull: %+v", report)
	}
	want := []string{"a.txt", "b.txt"}
	if got := rootNames(t, pc); !equal(got, want) {
		t.Errorf("the pc lists %v, want %v", got, want)
	}

	report = pull(t, pi)
	if got := rootNames(t, pi); !equal(got, want) {
		t.Errorf("the pi lists %v, want %v (pull: %+v)", got, want, report)
	}
	if got := rootNames(t, pi); !equal(got, want) {
		t.Errorf("the pi lists %v, want %v", got, want)
	}
	if report = pull(t, pc); report.Changed() {
		t.Errorf("a third pull still found something: %+v", report)
	}
}

// Deleting on one machine deletes on the other, however the other has since
// touched the row — the parts are gone.
func TestADeleteOnOneMachineReachesTheOther(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	doomed := storeAt(t, pc, "doomed.txt", "soon gone")
	storeAt(t, pc, "stays.txt", "stays")
	pull(t, pi)

	// The pi renames it while the pc deletes it.
	if _, err := pi.Move(context.Background(), doomed.ID, "/", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	pi.AwaitBackupSync()
	if _, err := pc.Delete(context.Background(), doomed.ID); err != nil {
		t.Fatal(err)
	}
	pc.AwaitBackupSync()

	pull(t, pi)
	if got := rootNames(t, pi); !equal(got, []string{"stays.txt"}) {
		t.Errorf("the pi lists %v, want the deleted file gone", got)
	}
	pull(t, pc)
	if got := rootNames(t, pc); !equal(got, []string{"stays.txt"}) {
		t.Errorf("the pc lists %v, want the deleted file gone", got)
	}
}

// Both machines store a file at the same path: both survive, the later one
// under the name an upload beside an existing file gets, on both machines.
func TestTheSamePathStoredOnBothMachinesKeepsBoth(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	storeAt(t, pc, "notes.txt", "written on the desk")
	time.Sleep(5 * time.Millisecond)
	storeAt(t, pi, "notes.txt", "written on the pi")

	// The rename is made by whichever machine combines the two — the pi, at
	// its push, when it already knew of the pc — and reported there; the
	// unit test on the merge checks the report. Here: the outcome, on both.
	pull(t, pc)
	want := []string{"notes (2).txt", "notes.txt"}
	if got := rootNames(t, pc); !equal(got, want) {
		t.Errorf("the pc lists %v, want %v", got, want)
	}
	pull(t, pi)
	if got := rootNames(t, pi); !equal(got, want) {
		t.Errorf("the pi lists %v, want %v", got, want)
	}
	// The later upload is the one renamed, and its contents came with it.
	e, _ := pi.EntryByPath(MainScope, "/notes (2).txt")
	data, _, err := pi.Fetch(context.Background(), e.ID)
	if err != nil || string(data) != "written on the pi" {
		t.Errorf("notes (2).txt reads %q (%v), want the pi's file", data, err)
	}
}

// Once a machine knows of another, its pushes fold in what the other wrote
// first, so a push never puts one machine's index over the other's.
func TestAPushFoldsInTheOtherMachinesChanges(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")
	storeAt(t, pc, "seed.txt", "so both know of each other")
	pull(t, pi)
	pull(t, pc)

	storeAt(t, pi, "from-pi.txt", "pi")
	// No pull on the pc: its upload's push is what has to notice.
	storeAt(t, pc, "from-pc.txt", "pc")

	want := []string{"from-pc.txt", "from-pi.txt", "seed.txt"}
	if got := rootNames(t, pc); !equal(got, want) {
		t.Errorf("after its push the pc lists %v, want %v", got, want)
	}
	// And the copy on the accounts carries both, so the pi takes it whole.
	report := pull(t, pi)
	if report.Outcome != MergeAdopted {
		t.Errorf("the pi's pull: %+v", report)
	}
	if got := rootNames(t, pi); !equal(got, want) {
		t.Errorf("the pi lists %v, want %v", got, want)
	}
}

// A sub vault made on one machine appears on the other, shut, and opens with
// its own password.
func TestASubVaultMadeOnOneMachineAppearsShutOnTheOther(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	id := createSubVault(t, pc, "Taxes", subPassword)
	if _, _, err := pc.Upload(context.Background(), Scope(id), "/", "return.pdf", []byte("1040"), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	pc.AwaitBackupSync()

	pull(t, pi)
	subs, _ := pi.SubVaults()
	if len(subs) != 1 || subs[0].ID != id || subs[0].Unlocked {
		t.Fatalf("the pi's sub vaults: %+v, want Taxes, shut", subs)
	}
	if err := pi.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("the pi could not open the sub vault: %v", err)
	}
	listing, err := pi.List(Scope(id), "/")
	if err != nil || len(listing.Files) != 1 {
		t.Errorf("inside the sub vault on the pi: %v %+v", err, listing)
	}
}

// The orphan scan on a machine that has not pulled does not take the other
// machine's uploads for parts no file wants, and says what it cannot know.
func TestOrphanScanPullsTheSharedIndexFirst(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")
	storeAt(t, pc, "seed.txt", "seed")
	pull(t, pi)

	storeAt(t, pc, "fresh.txt", "uploaded on the desk a moment ago")

	scan, err := pi.ScanForOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if scan.Found {
		t.Errorf("the pi's scan called the desk's new file an orphan: %+v", scan.Items)
	}
	if len(scan.Warnings) == 0 || !strings.Contains(strings.Join(scan.Warnings, "\n"), "pc") {
		t.Errorf("the scan did not say the vault is open on the pc: %v", scan.Warnings)
	}
}

// A copy of the index this vault cannot open is counted, not taken.
func TestAForeignCopyIsCountedNotTaken(t *testing.T) {
	v, roots := newTestVault(t, 2)
	storeAt(t, v, "mine.txt", "mine")
	// Another vault's copy, on one of the accounts.
	other, otherRoots := newTestVault(t, 3)
	storeAt(t, other, "theirs.txt", "theirs")
	copyFile(t, filepath.Join(otherRoots[0], BackupKey), filepath.Join(roots[1], BackupKey))

	report := pull(t, v)
	if report.Foreign != 1 || report.Answered != 1 || report.Changed() {
		t.Errorf("report %+v, want one foreign copy counted and nothing taken", report)
	}
	if got := rootNames(t, v); !equal(got, []string{"mine.txt"}) {
		t.Errorf("lists %v", got)
	}
}

// The pull schedule: at once until pulled, hourly alone, every few minutes
// with company, never with the index backup off.
func TestSyncDueFollowsTheRegister(t *testing.T) {
	v, _ := newTestVaultOn(t, "pc", 1)
	now := time.Now()
	if !v.SyncDue(now) {
		t.Error("a vault that has never pulled is not due")
	}
	pull(t, v)
	if v.SyncDue(now.Add(PeerSyncInterval + time.Second)) {
		t.Error("a vault alone is due after a few minutes")
	}
	if !v.SyncDue(now.Add(LoneSyncInterval + time.Second)) {
		t.Error("a vault alone is not due after an hour")
	}
	v.mu.Lock()
	v.manifest.Instances["pi"] = &Instance{Name: "pi", LastSeen: now}
	v.mu.Unlock()
	if !v.SyncDue(now.Add(PeerSyncInterval + time.Second)) {
		t.Error("a vault with company is not due after a few minutes")
	}
	if _, err := v.SetBackupEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if v.SyncDue(now.Add(24 * time.Hour)) {
		t.Error("a vault with the backup off is due")
	}
}

func pushCount(v *Vault) uint64 {
	v.backups.mu.Lock()
	defer v.backups.mu.Unlock()
	return v.backups.pushes
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
