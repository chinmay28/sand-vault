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

// Two machines running under one name bump one counter, so neither copy can
// be trusted to be "later": the pull combines rather than adopts, and says
// what is wrong.
func TestMachinesSharingANameCombineRatherThanAdopt(t *testing.T) {
	one, _ := newTestVaultOn(t, "raspberrypi", 3)
	two := secondMachine(t, one, "raspberrypi")

	storeAt(t, one, "on-one.txt", "one")
	// The second machine's push lands over the first's: neither knows of the
	// other yet. The first machine's file is still in its own vault file,
	// which is what the next pull works from.
	storeAt(t, two, "on-two.txt", "two")

	// Same clock, different write time: the first machine sees the collision
	// and combines rather than taking the copy as its own later version —
	// which would have dropped on-one.txt from every index there is.
	report := pull(t, one)
	want := []string{"on-one.txt", "on-two.txt"}
	if got := rootNames(t, one); !equal(got, want) {
		t.Fatalf("the first machine lists %v, want %v — its own file was dropped", got, want)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "SAND_INSTANCE") {
		t.Errorf("the pull did not say the name is shared: %v", report.Warnings)
	}
	// Ahead under the shared counter: the second machine combines too.
	pull(t, two)
	if got := rootNames(t, two); !equal(got, want) {
		t.Errorf("the second machine lists %v, want %v", got, want)
	}
	// And the echo stops: a combine that combines nothing is not pushed.
	pushes := pushCount(one)
	pull(t, one)
	pull(t, two)
	pull(t, one)
	if pushCount(one) != pushes {
		t.Error("two machines under one name keep pushing the same index back and forth")
	}
	if !one.hasCompany() || !two.hasCompany() {
		t.Error("a machine that found its name shared does not count as having company")
	}
}

// A sub vault deleted on one machine is let go of on the other, open or not.
func TestASubVaultDeletedOnOneMachineIsDroppedOnTheOther(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	id := createSubVault(t, pc, "Taxes", subPassword)
	pc.AwaitBackupSync()
	pull(t, pi)
	if err := pi.UnlockSubVault(id, subPassword); err != nil {
		t.Fatal(err)
	}

	if _, err := pc.DeleteSubVault(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	pc.AwaitBackupSync()

	pull(t, pi)
	subs, _ := pi.SubVaults()
	if len(subs) != 0 {
		t.Errorf("the pi still lists %+v", subs)
	}
	pi.mu.RLock()
	records, open := len(pi.store.SubVaults), len(pi.subs)
	pi.mu.RUnlock()
	if records != 0 || open != 0 {
		t.Errorf("the pi holds %d record(s) and %d open sub vault(s), want none", records, open)
	}
	// And it does not come back on the pc's next pull.
	pull(t, pc)
	subs, _ = pc.SubVaults()
	if len(subs) != 0 {
		t.Errorf("the deleted sub vault came back on the pc: %+v", subs)
	}
}

// Disconnecting an account another machine still reaches leaves the shared
// index's records alone: the parts are exactly where they say, readable there.
func TestDisconnectingAnAccountAPeerReachesKeepsItsRecords(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	e := storeAt(t, pc, "shared.txt", "shared")
	cfgs, _ := pc.Providers()
	gone := cfgs[0].ID

	pc.mu.Lock()
	pc.manifest.Instances["pi"] = &Instance{Name: "pi", LastSeen: time.Now(), Accounts: []string{gone}}
	pc.mu.Unlock()

	if err := pc.RemoveProvider(gone, true); err != nil {
		t.Fatal(err)
	}
	after, _ := pc.Entry(e.ID)
	if len(after.Shards) != 3 {
		t.Errorf("the file keeps %d shard record(s), want all 3 — the pi still reaches the account", len(after.Shards))
	}
	if left, _ := pc.Providers(); len(left) != 2 {
		t.Errorf("%d accounts connected, want 2", len(left))
	}

	// Without a peer reaching it, the records go as they always did — and
	// so do those of the account disconnected earlier, which no machine
	// reaches any more.
	pc.mu.Lock()
	delete(pc.manifest.Instances, "pi")
	pc.mu.Unlock()
	if err := pc.RemoveProvider(cfgs[1].ID, true); err != nil {
		t.Fatal(err)
	}
	after, _ = pc.Entry(e.ID)
	if len(after.Shards) != 1 {
		t.Errorf("the file keeps %d shard record(s), want only the connected account's", len(after.Shards))
	}
}

// A copy written by a build from before machines shared an index carries no
// clock and no tombstones, and is never combined in: it would bring back
// every file deleted since.
func TestACopyWithoutAClockIsLeftAlone(t *testing.T) {
	deleted := row("deleted-since", "/", "old.txt", t0)
	stale := index(nil, deleted, row("kept", "/", "kept.txt", t0))
	stale.UpdatedAt = t0
	current := index(map[string]uint64{"pc": 3}, row("kept", "/", "kept.txt", t0))
	current.UpdatedAt = t2

	merged, rep := mergeIndexesAs(current, stale, looksShared(current, stale, "pc"))
	if rep.Outcome != MergeAhead || len(merged.Entries) != 1 {
		t.Errorf("an old copy was folded in: %+v, %d rows", rep, len(merged.Entries))
	}
}

// Two copies with one clock and two write times are two machines under one
// name; a copy ahead of this machine under its own counter is the same.
func TestLooksSharedReadsTheClocks(t *testing.T) {
	a := index(map[string]uint64{"pi": 2})
	a.UpdatedAt = t1
	b := index(map[string]uint64{"pi": 2})
	b.UpdatedAt = t2
	if !looksShared(a, b, "pi") {
		t.Error("equal clocks and different write times did not read as a shared name")
	}
	b.UpdatedAt = t1
	if looksShared(a, b, "pi") {
		t.Error("the same copy twice read as a shared name")
	}
	c := index(map[string]uint64{"pi": 3})
	if !looksShared(a, c, "pi") {
		t.Error("a copy ahead under this machine's own name did not read as shared")
	}
	d := index(map[string]uint64{"pi": 2, "pc": 1})
	d.UpdatedAt = t2
	if looksShared(a, d, "pi") {
		t.Error("another machine's ordinary later write read as a shared name")
	}
}

// Saving a local account's settings from a machine claims the disk for it,
// changed or not — the way back for a machine that changed its name.
func TestSavingALocalAccountsSettingsClaimsItForThisMachine(t *testing.T) {
	v, _ := newTestVaultOn(t, "old-name", 1)
	cfgs, _ := v.Providers()
	v.SetInstance("new-name")

	statuses, _ := v.ProviderStatuses(context.Background())
	if statuses[0].Elsewhere != "old-name" {
		t.Fatalf("after the rename the disk reads %+v, want elsewhere=old-name", statuses[0])
	}
	updated, err := v.UpdateProvider(context.Background(), cfgs[0].ID, ProviderEdit{
		Options: map[string]string{"path": cfgs[0].Option("path")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Host != "new-name" {
		t.Errorf("after saving, the disk carries host %q, want new-name", updated.Host)
	}
}

// A machine that wrote the index a moment ago may be storing a file right
// now, and the sweep waits it out.
func TestOrphanSweepWaitsOutARecentPeerWrite(t *testing.T) {
	v, _ := newTestVaultOn(t, "pc", 3)
	v.SetArchiveSettleWindow(15 * time.Minute)
	// The register is edited in memory below; a push still in flight from
	// connecting the accounts would carry the edit to the copies, and the
	// scan's own pull would then read it back. Let the pushes settle first.
	v.AwaitBackupSync()
	v.mu.Lock()
	v.manifest.Instances["pi"] = &Instance{Name: "pi", LastSeen: time.Now()}
	v.mu.Unlock()

	scan, err := v.ScanForOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Blocked) == 0 || !strings.Contains(scan.Blocked[0], "pi") {
		t.Errorf("the sweep is not blocked by the pi's recent write: %v", scan.Blocked)
	}

	// The window covers the settle window and the longest a push may be
	// held back, so an hour is not yet quiet enough and two hours is.
	v.mu.Lock()
	v.manifest.Instances["pi"].LastSeen = time.Now().Add(-time.Hour)
	v.mu.Unlock()
	scan, _ = v.ScanForOrphans(context.Background())
	if len(scan.Blocked) == 0 {
		t.Error("an hour of quiet, inside the longest backup delay, did not block the sweep")
	}
	v.mu.Lock()
	v.manifest.Instances["pi"].LastSeen = time.Now().Add(-2 * time.Hour)
	v.mu.Unlock()
	scan, _ = v.ScanForOrphans(context.Background())
	if len(scan.Blocked) != 0 {
		t.Errorf("two hours of quiet still blocks the sweep: %v", scan.Blocked)
	}
}

// Moving a folder leaves the old path deleted and the new one not.
func TestMovingAFolderTombstonesThePathItLeaves(t *testing.T) {
	m := newManifest()
	if err := m.Mkdir("/a/b"); err != nil {
		t.Fatal(err)
	}
	m.moveFolder("/a", "/c")
	if _, ok := m.FolderTombstones["/a"]; !ok {
		t.Error("the folder moved away from is not marked deleted")
	}
	if _, ok := m.FolderTombstones["/a/b"]; !ok {
		t.Error("the subfolder moved away from is not marked deleted")
	}
	if _, ok := m.FolderTombstones["/c"]; ok {
		t.Error("the folder moved to is marked deleted")
	}
}

// A file stored into a sub vault on one machine is not swept as an orphan
// by the other, whose own copy of the inventory never saw it.
func TestOrphanScanOwnsTheOtherMachinesSubVaultFiles(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")
	id := createSubVault(t, pc, "Taxes", subPassword)
	pc.AwaitBackupSync()
	pull(t, pi) // the pi now holds the sub vault shut, with an empty inventory

	if _, _, err := pc.Upload(context.Background(), Scope(id), "/", "return.pdf", []byte("1040"), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	pc.AwaitBackupSync()

	scan, err := pi.ScanForOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if scan.Found {
		t.Errorf("the pi's scan would sweep the pc's sub vault file: %+v", scan.Items)
	}
}

// Deleting a sub vault deletes the whole of it, on every machine, open there
// or not — and the deleting machine is told what it could not count.
func TestADeletedSubVaultIsShutOnTheOtherMachineToo(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")
	id := createSubVault(t, pc, "Taxes", subPassword)
	pc.AwaitBackupSync()
	pull(t, pi)
	pull(t, pc) // so the pc knows of the pi
	if err := pi.UnlockSubVault(id, subPassword); err != nil {
		t.Fatal(err)
	}

	warnings, err := pc.DeleteSubVault(context.Background(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "pi") {
		t.Errorf("the delete did not say what the pi may have stored was not counted: %v", warnings)
	}
	pc.AwaitBackupSync()

	pull(t, pi)
	subs, _ := pi.SubVaults()
	pi.mu.RLock()
	records, open := len(pi.store.SubVaults), len(pi.subs)
	pi.mu.RUnlock()
	if len(subs) != 0 || records != 0 || open != 0 {
		t.Errorf("the pi still has the deleted sub vault: %d listed, %d record(s), %d open", len(subs), records, open)
	}
}

// A push from a machine that does not know of this one yet lands over this
// one's copy; what it covered — a file stored here, this machine's own entry
// in the register — is pushed again at this machine's next pull, even when
// that pull brought nothing new in.
func TestAPullRepushesWhatAnotherMachinesPushCovered(t *testing.T) {
	pc, _ := newTestVaultOn(t, "pc", 3)
	pi := secondMachine(t, pc, "pi")

	// The pi stores a file (and announces itself); the pc, knowing nothing
	// of the pi, writes its own copy over it.
	storeAt(t, pi, "on-pi.txt", "pi")
	pc.mu.Lock()
	err := pc.persistLocked()
	pc.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	pc.AwaitBackupSync()

	// The pi's pull brings nothing in, and still puts its file and its entry
	// back on the accounts.
	report := pull(t, pi)
	if report.Added != 0 {
		t.Fatalf("the pi's pull gained %d rows, want none", report.Added)
	}
	pull(t, pc)
	if got := rootNames(t, pc); !equal(got, []string{"on-pi.txt"}) {
		t.Errorf("the pc lists %v, want the pi's file", got)
	}
	insts, _ := pc.Instances()
	if len(insts) != 2 {
		t.Errorf("the pc's register: %+v, want the pi in it", insts)
	}
}
