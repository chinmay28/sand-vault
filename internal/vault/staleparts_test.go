package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A password change with one cloud switched off. The file moves without that
// cloud, the part left there is on record, and when the cloud comes back the
// scan names it for what it is and the sweep finishes the change.
func TestReEncryptionMovesPastADarkCloudAndRecordsThePartItLeaves(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "spare.txt", []byte("three parts, one of them on a cloud that is away"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(entry.Shards) != 3 {
		t.Fatalf("uploaded with %d parts, want 3", len(entry.Shards))
	}
	before := placementOf(entry)
	ids := accountIDs(t, v)
	var dead, deadID string
	var oldPart Shard
	for i, id := range ids {
		if id == before[2] {
			dead, deadID = roots[i], id
		}
	}
	for _, s := range entry.Shards {
		if s.ProviderID == deadID {
			oldPart = s
		}
	}
	kept := darkenCloud(t, dead)

	// The pass checks the clouds itself before the first file, so nothing has
	// to have run a health check for the dark one to be known.
	report, err := v.ChangePassword(ctx, testPassword, "changed with a cloud down", true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if report.Migrated != 1 || !report.Done() {
		t.Fatalf("report = %+v, want the file re-encrypted past the dark cloud", report)
	}
	if joined := strings.Join(report.Warnings, "\n"); !strings.Contains(joined, "stray part") || !strings.Contains(joined, oldPart.ProviderName) {
		t.Errorf("warnings do not say a part was left on %s as a stray: %v", oldPart.ProviderName, report.Warnings)
	}

	// The file is on the current key, two parts wide — there was no fourth
	// account to take the part the dark one could not — and readable; the
	// previous key is gone with it, record or no record.
	moved, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if moved.ArchiveID == entry.ArchiveID || len(moved.Shards) != 2 {
		t.Fatalf("the file was not rewritten short of the dark cloud: archive %s → %s with %d parts",
			entry.ArchiveID, moved.ArchiveID, len(moved.Shards))
	}
	for _, s := range moved.Shards {
		if s.ProviderID == deadID {
			t.Errorf("a fresh part was placed on the dark account: %+v", s)
		}
	}
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Errorf("the moved file does not read: %v", err)
	}
	if retiredCount(v) != 0 {
		t.Error("the previous key outlived the last file on it")
	}
	if _, err := os.Stat(filepath.Join(kept, oldPart.Key)); err != nil {
		t.Fatalf("the old part was expected to be left where it was: %v", err)
	}

	// On record, with everything the sweep will want to say about it, and
	// counted everywhere the previous key is counted.
	stale, err := v.StaleParts()
	if err != nil {
		t.Fatalf("StaleParts: %v", err)
	}
	if stale.Parts != 1 || stale.Bytes != oldPart.Size || len(stale.Accounts) != 1 {
		t.Fatalf("StaleParts = %+v, want the one part", stale)
	}
	if a := stale.Accounts[0]; a.ProviderID != deadID || a.Name != oldPart.ProviderName || !a.Unreachable {
		t.Errorf("StaleParts.Accounts[0] = %+v, want %s unreachable", a, oldPart.ProviderName)
	}
	if stale.Reachable != 0 || !strings.Contains(stale.Sentence(), "still not answering") {
		t.Errorf("Sentence() = %q, want it to say the cloud is not answering yet", stale.Sentence())
	}
	v.mu.RLock()
	recorded := append([]*StalePart(nil), v.manifest.StaleParts...)
	v.mu.RUnlock()
	if len(recorded) != 1 {
		t.Fatalf("the index records %d part(s), want 1", len(recorded))
	}
	if p := recorded[0]; p.ArchiveID != entry.ArchiveID || p.Part != oldPart.Part || p.Path != "/spare.txt" ||
		p.KeyID != entry.KeyID || p.ChunkCount != entry.ChunkCount || p.LeftAt.IsZero() || p.Reason == "" {
		t.Errorf("recorded part = %+v, want the old archive, its part number, the path and the key it is under", p)
	}
	stats, _ := v.Stats()
	if stats.StaleParts != 1 || stats.StaleBytes != oldPart.Size {
		t.Errorf("Stats = %d part(s), %d bytes; want 1, %d", stats.StaleParts, stats.StaleBytes, oldPart.Size)
	}
	status, err := v.RekeyStatus()
	if err != nil {
		t.Fatalf("RekeyStatus: %v", err)
	}
	if status.StaleParts == nil || status.StaleParts.Parts != 1 {
		t.Errorf("RekeyStatus.StaleParts = %+v, want the one part", status.StaleParts)
	}
	if joined := strings.Join(status.Warnings, "\n"); !strings.Contains(joined, "previous key") {
		t.Errorf("RekeyStatus.Warnings = %v, want the part under the previous key mentioned", status.Warnings)
	}

	// The record survives the vault being shut and opened again: it is index,
	// not memory.
	v = reopenAs(t, v, "changed with a cloud down")
	if stale, _ := v.StaleParts(); stale.Parts != 1 {
		t.Fatalf("after a reopen StaleParts = %+v, want the one part still on record", stale)
	}

	// While the cloud is dark the scan cannot see the part, says the account
	// is silent, and leaves the record alone: no listing, no verdict.
	scan := orphanScan(t, v)
	if scan.PreviousKeyArchives != 0 || len(scan.Blocked) == 0 {
		t.Errorf("a scan with the cloud dark = previous-key %d, blocked %v; want nothing found and the sweep withheld",
			scan.PreviousKeyArchives, scan.Blocked)
	}
	if stale, _ := v.StaleParts(); stale.Parts != 1 {
		t.Fatalf("a scan that could not list the account forgot the part: %+v", stale)
	}

	// The cloud comes back. The check says so, the summary says the part is
	// within reach, and the scan finds it where it was left, named for what
	// it is and offered.
	restoreCloud(t, dead, kept)
	if report, err := v.CheckClouds(ctx); err != nil || report.Unhealthy != 0 {
		t.Fatalf("CheckClouds after the restore = %+v, %v; want every account up", report, err)
	}
	// The account came back holding the index copy from before the change,
	// sealed under the old password. The check that found it back queues the
	// forced push that replaces it (and the scan would not take the copy for
	// another vault's even before that lands — see
	// TestAReturningCloudsStaleIndexCopyIsNotTakenForAnotherVaults).
	v.AwaitBackupSync()
	stale, _ = v.StaleParts()
	if stale.Reachable != 1 || stale.Accounts[0].Unreachable || !strings.Contains(stale.Sentence(), "within reach") {
		t.Errorf("StaleParts after the restore = %+v (%q), want the part within reach", stale, stale.Sentence())
	}
	scan = orphanScan(t, v)
	if !scan.Found || len(scan.Blocked) > 0 {
		t.Fatalf("scan with the cloud back = found %v, blocked %v; want the part found and the sweep offered", scan.Found, scan.Blocked)
	}
	if scan.PreviousKeyArchives != 1 || scan.PreviousKey != 1 || scan.PreviousKeyBytes != oldPart.Size {
		t.Errorf("scan counts previous-key archives %d, objects %d, bytes %d; want 1, 1, %d",
			scan.PreviousKeyArchives, scan.PreviousKey, scan.PreviousKeyBytes, oldPart.Size)
	}
	if len(scan.PreviousKeyOn) != 1 || scan.PreviousKeyOn[0] != oldPart.ProviderName {
		t.Errorf("scan.PreviousKeyOn = %v, want [%s]", scan.PreviousKeyOn, oldPart.ProviderName)
	}
	if len(scan.Items) != 1 {
		t.Fatalf("scan found %d archive(s), want the one left behind: %+v", len(scan.Items), scan.Items)
	}
	if item := scan.Items[0]; item.ArchiveID != entry.ArchiveID || !item.PreviousKey || item.Path != "/spare.txt" || !item.Deletable {
		t.Errorf("scan.Items[0] = %+v, want the old archive flagged as under the previous key, with its path, deletable", item)
	}
	var account *OrphanAccount
	for i := range scan.Accounts {
		if scan.Accounts[i].ProviderID == deadID {
			account = &scan.Accounts[i]
		}
	}
	if account == nil || account.PreviousKey != 1 || account.Orphans != 1 {
		t.Errorf("the account's row = %+v, want its one orphan counted as under the previous key", account)
	}

	// The sweep erases it and the record goes with it.
	swept, err := v.SweepOrphans(ctx, nil, false, nil)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if swept.Archives != 1 || swept.Deleted != 1 {
		t.Fatalf("sweep = %+v, want the one archive erased", swept)
	}
	if _, err := os.Stat(filepath.Join(dead, oldPart.Key)); !os.IsNotExist(err) {
		t.Errorf("the old part is still on the account after the sweep: %v", err)
	}
	if stale, _ := v.StaleParts(); stale.Parts != 0 {
		t.Errorf("after the sweep StaleParts = %+v, want nothing on record", stale)
	}
	if stats, _ := v.Stats(); stats.StaleParts != 0 {
		t.Errorf("after the sweep Stats counts %d stray part(s)", stats.StaleParts)
	}
	if scan := orphanScan(t, v); scan.Found {
		t.Errorf("a scan after the sweep still finds %d archive(s)", scan.Archives)
	}
}

// A recorded part that a listing of its account does not show any more —
// erased by hand, say — is taken off the record by the scan rather than
// promised for ever.
func TestTheScanForgetsARecordedPartThatIsAlreadyGone(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "tidy.txt", []byte("somebody emptied the folder by hand"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := placementOf(entry)
	ids := accountIDs(t, v)
	var dead string
	var oldPart Shard
	for i, id := range ids {
		if id == before[1] {
			dead = roots[i]
		}
	}
	for _, s := range entry.Shards {
		if s.ProviderID == before[1] {
			oldPart = s
		}
	}
	kept := darkenCloud(t, dead)
	if report, err := v.ChangePassword(ctx, testPassword, "changed", true); err != nil || !report.Done() {
		t.Fatalf("ChangePassword = %+v, %v; want the file moved past the dark cloud", report, err)
	}
	if stale, _ := v.StaleParts(); stale.Parts != 1 {
		t.Fatalf("StaleParts = %+v, want the one part on record", stale)
	}

	// The cloud comes back without the part: whoever had it erased it.
	if err := os.Remove(filepath.Join(kept, oldPart.Key)); err != nil {
		t.Fatalf("erasing the old part by hand: %v", err)
	}
	restoreCloud(t, dead, kept)
	if _, err := v.CheckClouds(ctx); err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}

	scan := orphanScan(t, v)
	if scan.Found || scan.PreviousKeyArchives != 0 {
		t.Errorf("scan = found %v, previous-key %d; want nothing, the part is gone", scan.Found, scan.PreviousKeyArchives)
	}
	if stale, _ := v.StaleParts(); stale.Parts != 0 {
		t.Errorf("after a listing that did not show it StaleParts = %+v, want the record dropped", stale)
	}
	if stats, _ := v.Stats(); stats.StaleParts != 0 {
		t.Errorf("Stats still counts %d stray part(s)", stats.StaleParts)
	}
}

// Disconnecting the account takes its recorded parts off the record, the way
// it takes the shard records naming it: whatever is on that storage is an
// ordinary orphan there if it is ever connected back.
func TestDisconnectingTheAccountForgetsItsRecordedParts(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "gone.txt", []byte("the account is going away for good"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := placementOf(entry)
	ids := accountIDs(t, v)
	var dead, deadID string
	for i, id := range ids {
		if id == before[3] {
			dead, deadID = roots[i], id
		}
	}
	kept := darkenCloud(t, dead)
	if report, err := v.ChangePassword(ctx, testPassword, "changed", true); err != nil || !report.Done() {
		t.Fatalf("ChangePassword = %+v, %v; want the file moved past the dark cloud", report, err)
	}
	if stale, _ := v.StaleParts(); stale.Parts != 1 || stale.Accounts[0].ProviderID != deadID {
		t.Fatalf("StaleParts = %+v, want the one part on the dark account", stale)
	}

	if err := v.RemoveProvider(deadID, true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if stale, _ := v.StaleParts(); stale.Parts != 0 {
		t.Errorf("after the disconnect StaleParts = %+v, want nothing on record", stale)
	}

	// Connected back as somebody new, the part is an orphan like any other:
	// found, offered, and said nothing more about.
	restoreCloud(t, dead, kept)
	disconnectAndReconnect(t, v, dead, "returned")
	scan := orphanScan(t, v)
	if !scan.Found || scan.PreviousKeyArchives != 0 {
		t.Errorf("scan after the reconnect = found %v, previous-key %d; want the part found as a plain orphan",
			scan.Found, scan.PreviousKeyArchives)
	}
	if len(scan.Items) == 1 && (scan.Items[0].PreviousKey || scan.Items[0].Path != "") {
		t.Errorf("the reconnected account's orphan is still described as a recorded part: %+v", scan.Items[0])
	}
}

// The record itself: what is noted once stays as it was noted, what is
// forgotten is gone, and the summary says it in one sentence.
func TestStalePartsRecordKeepsOneRowPerPart(t *testing.T) {
	m := newManifest()
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	e := &Entry{ID: "f1", Dir: "/", Name: "film.mkv", ArchiveID: "abcd1234", KeyID: "k0", ChunkCount: 3}
	shards := []Shard{
		{Part: 1, ProviderID: "a", ProviderName: "cloud-a", Size: 10},
		{Part: 2, ProviderID: "b", ProviderName: "cloud-b", Size: 20},
	}
	m.noteStaleParts(stalePartsOf(MainScope, e, shards, "did not answer", at))
	m.noteStaleParts(stalePartsOf(MainScope, e, shards[:1], "again", at.Add(time.Hour)))
	if len(m.StaleParts) != 2 {
		t.Fatalf("record holds %d row(s), want 2 — the same part noted twice is one row", len(m.StaleParts))
	}
	if m.StaleParts[0].Reason != "did not answer" || !m.StaleParts[0].LeftAt.Equal(at) {
		t.Errorf("the second note overwrote the first: %+v", m.StaleParts[0])
	}
	if p := m.StaleParts[1]; p.Path != "/film.mkv" || p.ChunkCount != 3 || p.KeyID != "k0" || p.Bytes != 20 {
		t.Errorf("row = %+v, want the file's path, chunk count, key and the part's size", p)
	}
	// A sub vault's file gives up no path.
	m.noteStaleParts(stalePartsOf(Scope("sub"), &Entry{ID: "s", Dir: "/secret", Name: "x", ArchiveID: "ffff"}, shards[:1], "", at))
	if p := m.StaleParts[2]; p.Path != "" || p.Scope != Scope("sub") {
		t.Errorf("a sub vault's row carries a path: %+v", p)
	}

	if !m.forgetStaleParts(func(p *StalePart) bool { return p.ProviderID == "a" }) {
		t.Error("forgetting two rows reported no change")
	}
	if len(m.StaleParts) != 1 || m.StaleParts[0].ProviderID != "b" {
		t.Errorf("record after the forget = %+v, want cloud-b's row alone", m.StaleParts)
	}
	if m.forgetStaleParts(func(*StalePart) bool { return false }) {
		t.Error("forgetting nothing reported a change")
	}
	if !m.forgetStaleParts(func(*StalePart) bool { return true }) || m.StaleParts != nil {
		t.Errorf("record after forgetting everything = %v, want nil", m.StaleParts)
	}

	var none StalePartsSummary
	if none.Sentence() != "" {
		t.Errorf("an empty summary has something to say: %q", none.Sentence())
	}
	some := StalePartsSummary{Parts: 2, Bytes: 30, Reachable: 1, Accounts: []StalePartsAccount{
		{Name: "cloud-b", Parts: 1, Bytes: 20},
		{Name: "cloud-a", Parts: 1, Bytes: 10, Unreachable: true},
	}}
	got := some.Sentence()
	for _, want := range []string{"2 parts", "cloud-a, cloud-b", "cloud-b are within reach"} {
		if !strings.Contains(got, want) {
			t.Errorf("Sentence() = %q, want it to contain %q", got, want)
		}
	}
}

// heldBy is the rule the plan passes files over by: a part on a silent
// account holds a file only when the parts elsewhere cannot rebuild it.
func TestHeldByAsksWhetherTheFileCanBeRebuiltWithoutTheAccount(t *testing.T) {
	file := &Entry{DataShards: 2, TotalShards: 3, Shards: []Shard{
		{Part: 1, ProviderID: "a"}, {Part: 2, ProviderID: "b"}, {Part: 3, ProviderID: "c"},
	}}
	if got := heldBy(file, nil); got != "" {
		t.Errorf("nothing down holds the file on %q", got)
	}
	if got := heldBy(file, map[string]bool{"a": true}); got != "" {
		t.Errorf("one of three down holds a 2-of-3 file on %q; two parts rebuild it", got)
	}
	if got := heldBy(file, map[string]bool{"a": true, "b": true}); got != "a" {
		t.Errorf("two of three down holds the file on %q, want a", got)
	}
	short := &Entry{DataShards: 2, TotalShards: 3, Shards: []Shard{
		{Part: 1, ProviderID: "a"}, {Part: 2, ProviderID: "b"},
	}}
	if got := heldBy(short, map[string]bool{"b": true}); got != "b" {
		t.Errorf("a file already short of a part, with one of its two on a silent account, is held on %q, want b", got)
	}
	if got := heldBy(short, map[string]bool{"zzz": true}); got != "" {
		t.Errorf("an account the file is not on holds it on %q", got)
	}
}

// The cloud that was away through a password change comes back holding the
// index copy from before it, sealed under the old password. That copy is this
// vault's own, stale, and the vault knows the salt it was sealed with; the
// scan must not read it as another vault's and withhold the parts beside it.
// And the health check that finds the cloud back is what gets the copy
// replaced, rather than the next index write on a vault nobody is writing to.
func TestAReturningCloudsStaleIndexCopyIsNotTakenForAnotherVaults(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "spare.txt", []byte("one cloud away through the change"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := placementOf(entry)
	ids := accountIDs(t, v)
	var dead, deadID string
	for i, id := range ids {
		if id == before[1] {
			dead, deadID = roots[i], id
		}
	}
	kept := darkenCloud(t, dead)
	if report, err := v.ChangePassword(ctx, testPassword, "changed", true); err != nil || !report.Done() {
		t.Fatalf("ChangePassword = %+v, %v; want the file moved past the dark cloud", report, err)
	}
	// The pushes after the change reached the two clouds that answered and
	// not the third, so the vault still owes it a copy and remembers which
	// salt the copy there was sealed with.
	v.AwaitBackupSync()
	v.mu.RLock()
	owed, salt := v.store.BackupNeedsForce, strings.Join(v.store.PreviousBackupSalts, ",")
	v.mu.RUnlock()
	if !owed || salt == "" {
		t.Fatalf("after a change with a cloud away: needs force %v, previous salt %q; want the copy still owed and the salt kept", owed, salt)
	}

	// Back, and nothing has written to the index or checked the clouds since.
	// The scan alone has to get it right.
	restoreCloud(t, dead, kept)
	scan := orphanScan(t, v)
	var row *OrphanAccount
	for i := range scan.Accounts {
		if scan.Accounts[i].ProviderID == deadID {
			row = &scan.Accounts[i]
		}
	}
	if row == nil || !row.Backup || row.Foreign {
		t.Fatalf("the returning account's row = %+v, want its stale copy seen as a backup of this vault's, not a foreign one", row)
	}
	if len(scan.Blocked) > 0 || scan.Deletable != 1 || scan.PreviousKeyArchives != 1 {
		t.Errorf("scan = blocked %v, deletable %d, previous-key %d; want the part offered", scan.Blocked, scan.Deletable, scan.PreviousKeyArchives)
	}

	// The health check that finds the cloud back queues the push the vault
	// owes it, and once that lands nothing is owed and nothing is remembered.
	if report, err := v.CheckClouds(ctx); err != nil || report.Unhealthy != 0 {
		t.Fatalf("CheckClouds = %+v, %v; want every account up", report, err)
	}
	v.AwaitBackupSync()
	v.mu.RLock()
	owed, salt = v.store.BackupNeedsForce, strings.Join(v.store.PreviousBackupSalts, ",")
	v.mu.RUnlock()
	if owed || salt != "" {
		t.Errorf("after the check found the cloud back: needs force %v, previous salt %q; want the copy replaced and the salt forgotten", owed, salt)
	}
	if scan := orphanScan(t, v); len(scan.Blocked) > 0 || scan.Deletable != 1 {
		t.Errorf("scan after the push = blocked %v, deletable %d; want the part still offered", scan.Blocked, scan.Deletable)
	}
}
