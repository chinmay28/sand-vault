package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// placement is where each of an entry's parts ended up, by part number.
func placementOf(e *Entry) map[int]string {
	out := map[int]string{}
	for _, s := range e.Shards {
		out[s.Part] = s.ProviderID
	}
	return out
}

// objectsIn counts the stored parts sitting in one account's folder, ignoring
// the manifest backup that also lives there.
func objectsIn(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sand") && e.Name() != "manifest.sand" {
			count++
		}
	}
	return count
}

func TestRelocateMovesOnlyThePartThatHasTo(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := []byte("three clouds now, two of them the same afterwards\n")
	entry, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", payload, UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	before := placementOf(entry)
	// Swap out exactly one of the three: the account holding part 3 goes, the
	// fourth account takes its place.
	stays, leaving := []string{before[1], before[2]}, before[3]
	targets := append(append([]string{}, stays...), ids[3])

	plan, err := v.PlanRelocation(MainScope, "/notes.txt", targets, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Moves != 1 {
		t.Fatalf("plan moves %d parts, want 1: %+v", plan.Moves, plan.Files)
	}
	if plan.Drops != 0 || plan.Unchanged != 0 {
		t.Errorf("plan drops=%d unchanged=%d, want 0/0", plan.Drops, plan.Unchanged)
	}
	if got := plan.Files[0].Moves[0]; got.Part != 3 || got.From != leaving || got.To != ids[3] {
		t.Errorf("planned move = %+v, want part 3 from %s to %s", got, leaving, ids[3])
	}
	if len(plan.Files[0].Stay) != 2 {
		t.Errorf("expected 2 parts to stay put, got %v", plan.Files[0].Stay)
	}

	report, err := v.Relocate(ctx, MainScope, "/notes.txt", targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if !report.Done() || report.Relocated != 1 || report.PartsMoved != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Bytes == 0 {
		t.Error("report claims no bytes moved")
	}

	moved, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	after := placementOf(moved)
	if after[1] != before[1] || after[2] != before[2] {
		t.Errorf("parts 1 and 2 moved: %v → %v", before, after)
	}
	if after[3] != ids[3] {
		t.Errorf("part 3 is on %s, want %s", after[3], ids[3])
	}

	// The archive ID, the key generation and the object keys are untouched:
	// nothing was re-encrypted, only carried across.
	if moved.ArchiveID != entry.ArchiveID || moved.KeyID != entry.KeyID {
		t.Errorf("archive id or key generation changed: %+v → %+v", entry, moved)
	}
	for _, s := range moved.Shards {
		// The key names the file and the part, never the account, which is
		// exactly what lets a part be carried across without rewriting it.
		want := ShardKey(moved.ArchiveID, s.Part)
		if moved.Chunked() {
			want = ChunkShardKey(moved.ArchiveID, 0, s.Part)
		}
		if s.Key != want {
			t.Errorf("part %d key = %q, want %q", s.Part, s.Key, want)
		}
	}

	// The part is really on the new account and really gone from the old one.
	if got := objectsIn(t, roots[3]); got != 1 {
		t.Errorf("the new account holds %d objects, want 1", got)
	}
	for i, id := range ids {
		if id == leaving && objectsIn(t, roots[i]) != 0 {
			t.Errorf("the old account still holds %d objects", objectsIn(t, roots[i]))
		}
	}

	// And the file still rebuilds from where it is now.
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after relocating: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("content changed: got %q", data)
	}
}

func TestRelocateToTheSameAccountsDoesNothing(t *testing.T) {
	v, roots := newTestVault(t, 3)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "still.txt", []byte("no work to do"), UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := objectsUnder(t, roots)

	// Named in a different order, which must not be mistaken for a different
	// answer: placement is a set of accounts, not a sequence.
	shuffled := []string{ids[2], ids[0], ids[1]}

	plan, err := v.PlanRelocation(MainScope, entry.ID, shuffled, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Moves != 0 || plan.Drops != 0 || plan.Unchanged != 1 {
		t.Fatalf("plan = moves %d, drops %d, unchanged %d — want nothing to do",
			plan.Moves, plan.Drops, plan.Unchanged)
	}

	report, err := v.Relocate(ctx, MainScope, entry.ID, shuffled, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.PartsMoved != 0 || report.Bytes != 0 || report.Unchanged != 1 {
		t.Errorf("report = %+v, want an untouched file", report)
	}
	if got := objectsUnder(t, roots); got != before {
		t.Errorf("object count changed from %d to %d", before, got)
	}
}

func TestRelocateFolderRecursively(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	if err := v.Mkdir(MainScope, "/photos/2024"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	first, _, err := v.Upload(ctx, MainScope, "/photos", "a.txt", []byte("alpha"), UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload a: %v", err)
	}
	nested, _, err := v.Upload(ctx, MainScope, "/photos/2024", "b.txt", []byte("bravo"), UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload b: %v", err)
	}
	// Outside the folder, and so out of scope.
	outside, _, err := v.Upload(ctx, MainScope, "/", "c.txt", []byte("charlie"), UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload c: %v", err)
	}

	targets := ids[3:]
	report, err := v.Relocate(ctx, MainScope, "/photos", targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if !report.Folder || report.Total != 2 || report.Relocated != 2 || report.PartsMoved != 6 {
		t.Fatalf("report = %+v, want 2 files and 6 parts moved", report)
	}

	want := map[string]bool{targets[0]: true, targets[1]: true, targets[2]: true}
	for _, id := range []string{first.ID, nested.ID} {
		e, err := v.Entry(id)
		if err != nil {
			t.Fatalf("Entry: %v", err)
		}
		for _, s := range e.Shards {
			if !want[s.ProviderID] {
				t.Errorf("%s part %d is still on %s", e.Path(), s.Part, s.ProviderName)
			}
		}
	}

	untouched, err := v.Entry(outside.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	for _, s := range untouched.Shards {
		if want[s.ProviderID] {
			t.Errorf("a file outside the folder moved: part %d is on %s", s.Part, s.ProviderName)
		}
	}

	data, _, err := v.Fetch(ctx, nested.ID)
	if err != nil {
		t.Fatalf("Fetch after relocating: %v", err)
	}
	if string(data) != "bravo" {
		t.Errorf("content = %q, want bravo", data)
	}
}

func TestRelocateChunkedFileCarriesEveryChunk(t *testing.T) {
	v, roots := chunkedVault(t, 4, 1024)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := bytes.Repeat([]byte("a film is a great many bytes long "), 300) // ~10 KB
	entry, _, err := v.Upload(ctx, MainScope, "/", "film.mkv", payload, UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !entry.Chunked() {
		t.Fatal("expected a chunked upload")
	}
	chunks := entry.ChunkCount

	before := placementOf(entry)
	targets := []string{before[1], before[2], ids[3]}

	plan, err := v.PlanRelocation(MainScope, entry.ID, targets, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if got := plan.Files[0].Moves[0].Objects; got != chunks {
		t.Errorf("plan says %d objects for the moving part, want %d", got, chunks)
	}

	report, err := v.Relocate(ctx, MainScope, entry.ID, targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if !report.Done() || report.PartsMoved != 1 {
		t.Fatalf("report = %+v", report)
	}

	// Every chunk's object came across, and none was left behind.
	if got := objectsIn(t, roots[3]); got != chunks {
		t.Errorf("the new account holds %d objects, want %d", got, chunks)
	}
	for i, id := range ids {
		if id == before[3] && objectsIn(t, roots[i]) != 0 {
			t.Errorf("the old account still holds %d chunk objects", objectsIn(t, roots[i]))
		}
	}

	moved, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if moved.ChunkCount != chunks || moved.ChunkSize != entry.ChunkSize {
		t.Errorf("chunk layout changed: %d×%d → %d×%d",
			entry.ChunkCount, entry.ChunkSize, moved.ChunkCount, moved.ChunkSize)
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after relocating: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the rebuilt film does not match what went in")
	}
}

func TestRelocateOntoTwoAccountsDropsTheSpare(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "squeeze.txt", []byte("three into two"), UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := placementOf(entry)

	// Two of the three it is already on: nothing needs to move, and the third
	// part has nowhere to live under the strict policy.
	targets := []string{before[1], before[2]}

	plan, err := v.PlanRelocation(MainScope, entry.ID, targets, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Moves != 0 || plan.Drops != 1 {
		t.Fatalf("plan = %d moves, %d drops — want 0 and 1", plan.Moves, plan.Drops)
	}
	if len(plan.Warnings) == 0 {
		t.Error("dropping a part should be said out loud")
	}

	report, err := v.Relocate(ctx, MainScope, entry.ID, targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.PartsDrop != 1 || report.PartsMoved != 0 {
		t.Fatalf("report = %+v", report)
	}

	moved, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if len(moved.Shards) != 2 {
		t.Fatalf("file kept %d parts, want 2", len(moved.Shards))
	}
	for _, s := range moved.Shards {
		if s.ProviderID != targets[0] && s.ProviderID != targets[1] {
			t.Errorf("part %d is on %s, outside the chosen two", s.Part, s.ProviderName)
		}
	}

	// Two parts are still enough to rebuild it.
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Errorf("Fetch after dropping the spare: %v", err)
	}
}

func TestRelocateRedundantPolicyDoublesUp(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	ids := accountIDs(t, v)

	if err := v.SetPolicy(PolicyRedundant); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/", "double.txt", []byte("all three, two homes"), UploadOptions{
		Accounts: ids,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	targets := ids[:2]
	report, err := v.Relocate(ctx, MainScope, entry.ID, targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.PartsDrop != 0 {
		t.Errorf("redundant placement should keep all three parts, dropped %d", report.PartsDrop)
	}

	moved, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if len(moved.Shards) != 3 {
		t.Fatalf("kept %d parts, want 3", len(moved.Shards))
	}
	allowed := map[string]bool{targets[0]: true, targets[1]: true}
	for _, s := range moved.Shards {
		if !allowed[s.ProviderID] {
			t.Errorf("part %d is on %s, outside the chosen two", s.Part, s.ProviderName)
		}
	}
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Errorf("Fetch after doubling up: %v", err)
	}
}

func TestRelocateRefusesOneAccountUnderStrict(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ids := accountIDs(t, v)

	if _, _, err := v.Upload(context.Background(), MainScope, "/", "one.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	_, err := v.Relocate(context.Background(), MainScope, "/one.txt", ids[:1], archive.Scheme{}, nil)
	if err == nil {
		t.Fatal("expected strict placement to refuse a single account")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Errorf("error should explain the policy, got: %v", err)
	}
}

func TestRelocateRejectsUnknownAndRepeatedAccounts(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ids := accountIDs(t, v)

	if _, _, err := v.Upload(context.Background(), MainScope, "/", "x.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	cases := map[string][]string{
		"unknown account": {ids[0], ids[1], "not-an-account"},
		"repeated":        {ids[0], ids[0], ids[1]},
		"too many":        {ids[0], ids[1], ids[2], ids[0]},
		"none at all":     {},
	}
	for name, accounts := range cases {
		if _, err := v.PlanRelocation(MainScope, "/x.txt", accounts, archive.Scheme{}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRelocateUnknownTargetAndLockedVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ids := accountIDs(t, v)

	if _, err := v.PlanRelocation(MainScope, "/nowhere.txt", ids, archive.Scheme{}); err == nil {
		t.Error("expected an error for a path that names nothing")
	}

	v.Lock()
	if _, err := v.Relocate(context.Background(), MainScope, "/nowhere.txt", ids, archive.Scheme{}, nil); !errors.Is(err, ErrLocked) {
		t.Errorf("locked vault returned %v, want ErrLocked", err)
	}
}

func TestRelocateReportsAnUnreachableAccountAndResumes(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "flaky.txt", []byte("one account is asleep"), UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := placementOf(entry)

	// Make the account holding part 1 unreadable by replacing its folder with
	// a file, which is what a local backend does when the disk is gone.
	var brokenRoot string
	for i, id := range ids {
		if id == before[1] {
			brokenRoot = roots[i]
		}
	}
	kept := filepath.Join(t.TempDir(), "away")
	if err := os.Rename(brokenRoot, kept); err != nil {
		t.Fatalf("moving the account's folder away: %v", err)
	}
	if err := os.WriteFile(brokenRoot, []byte("not a folder"), 0o600); err != nil {
		t.Fatalf("blocking the account's folder: %v", err)
	}

	// Everything moves to the fourth account and the two it is already on stay,
	// except part 1, which cannot be read.
	targets := []string{before[2], before[3], ids[3]}
	report, err := v.Relocate(ctx, MainScope, entry.ID, targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.Done() {
		t.Error("a part that could not be copied should not report a clean move")
	}
	if report.Partial != 1 || report.PartsMoved != 0 {
		t.Errorf("report = %+v, want one partial file", report)
	}
	if len(report.Warnings) == 0 {
		t.Error("expected a warning naming the unreachable account")
	}

	// The part is still recorded where it always was: nothing was lost by
	// failing to copy it.
	stuck, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if placementOf(stuck)[1] != before[1] {
		t.Errorf("part 1 was rewritten despite the copy failing: %v", placementOf(stuck))
	}
	if len(stuck.Shards) != 3 {
		t.Errorf("file has %d parts, want 3 — a failed move must not lose one", len(stuck.Shards))
	}

	// With the account back, running it again finishes the job.
	if err := os.Remove(brokenRoot); err != nil {
		t.Fatalf("unblocking: %v", err)
	}
	if err := os.Rename(kept, brokenRoot); err != nil {
		t.Fatalf("restoring the account's folder: %v", err)
	}

	again, err := v.Relocate(ctx, MainScope, entry.ID, targets, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate again: %v", err)
	}
	if !again.Done() || again.PartsMoved != 1 {
		t.Fatalf("second pass = %+v", again)
	}
	final := placementOf(stuck)
	if final, err = func() (map[int]string, error) {
		e, err := v.Entry(entry.ID)
		if err != nil {
			return nil, err
		}
		return placementOf(e), nil
	}(); err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if final[1] != ids[3] {
		t.Errorf("part 1 is on %s, want the fourth account", final[1])
	}
}

func TestRelocateLeavesADisconnectedAccountsPartAlone(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "orphan.txt", []byte("one part is out of reach"), UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Rewrite one shard to name an account that was never connected, which is
	// the state a recovered vault can be left in.
	v.mu.Lock()
	v.manifest.ByID(entry.ID).Shards[0].ProviderID = "long-gone"
	err = v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	plan, err := v.PlanRelocation(MainScope, entry.ID, ids[1:], archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if len(plan.Files[0].Stranded) != 1 {
		t.Fatalf("expected one stranded part, got %+v", plan.Files[0])
	}
	if len(plan.Warnings) == 0 {
		t.Error("a stranded part should be said out loud")
	}

	if _, err := v.Relocate(ctx, MainScope, entry.ID, ids[1:], archive.Scheme{}, nil); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if len(after.Shards) != 3 {
		t.Errorf("a stranded part was dropped: %d parts left", len(after.Shards))
	}
	stranded := false
	for _, s := range after.Shards {
		if s.ProviderID == "long-gone" {
			stranded = true
		}
	}
	if !stranded {
		t.Error("the stranded part's record should be left exactly as it was")
	}
}

func TestRelocateCarriesFolderThumbnails(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	if err := v.Mkdir(MainScope, "/album"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/album", "shot.jpg", []byte("pretend jpeg"), UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetThumb(ctx, entry.ID, []byte("pretend thumbnail bytes")); err != nil {
		t.Fatalf("SetThumb: %v", err)
	}

	targets := ids[3:]
	if _, err := v.Relocate(ctx, MainScope, "/album", targets, archive.Scheme{}, nil); err != nil {
		t.Fatalf("Relocate: %v", err)
	}

	want := map[string]bool{targets[0]: true, targets[1]: true, targets[2]: true}
	v.mu.RLock()
	pack := v.manifest.Thumbs["/album"]
	v.mu.RUnlock()
	if pack == nil {
		t.Fatal("the folder lost its thumbnail pack")
	}
	for _, s := range pack.Shards {
		if !want[s.ProviderID] {
			t.Errorf("thumbnail part %d is still on %s", s.Part, s.ProviderName)
		}
	}

	// And the picture still comes back.
	got, err := v.Thumb(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Thumb after relocating: %v", err)
	}
	if len(got) == 0 {
		t.Error("the thumbnail came back empty")
	}
}

func TestPlanRelocationSummarizesWithoutTouchingAnything(t *testing.T) {
	v, roots := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		if _, _, err := v.Upload(ctx, MainScope, "/", name, bytes.Repeat([]byte("z"), 4096), UploadOptions{
			Accounts: ids[:3],
		}); err != nil {
			t.Fatalf("Upload %s: %v", name, err)
		}
	}
	before := objectsUnder(t, roots)

	plan, err := v.PlanRelocation(MainScope, "/", ids[3:], archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Total != 3 || plan.Moves != 9 {
		t.Fatalf("plan = %d files, %d moves — want 3 and 9", plan.Total, plan.Moves)
	}
	if !plan.Folder || plan.Path != "/" {
		t.Errorf("plan should describe the root folder, got path %q folder=%v", plan.Path, plan.Folder)
	}
	if plan.Bytes <= 0 {
		t.Error("plan should say how much has to move")
	}

	// Every byte leaving one account arrives at another.
	var out, in int64
	for _, n := range plan.Outgoing {
		out += n
	}
	for _, n := range plan.Incoming {
		in += n
	}
	if out != plan.Bytes || in != plan.Bytes {
		t.Errorf("outgoing %d / incoming %d do not add up to %d", out, in, plan.Bytes)
	}
	leaving := make([]string, 0, len(plan.Outgoing))
	for id := range plan.Outgoing {
		leaving = append(leaving, id)
	}
	sort.Strings(leaving)
	if len(leaving) != 3 {
		t.Errorf("expected three accounts to be emptied, got %v", leaving)
	}

	if got := objectsUnder(t, roots); got != before {
		t.Errorf("planning moved something: %d → %d objects", before, got)
	}
}

func TestPlanRelocationTruncatesItsDetail(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	// One more than the preview cap, so the totals and the rows disagree.
	total := relocationPreviewLimit + 1
	for i := 0; i < total; i++ {
		name := "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
		if _, _, err := v.Upload(ctx, MainScope, "/", name, []byte("x"), UploadOptions{Accounts: ids[:3]}); err != nil {
			t.Fatalf("Upload %s: %v", name, err)
		}
	}

	plan, err := v.PlanRelocation(MainScope, "/", ids[:3], archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Total != total {
		t.Errorf("plan counted %d files, want %d", plan.Total, total)
	}
	if !plan.Truncated || len(plan.Files) != relocationPreviewLimit {
		t.Errorf("expected %d rows and a truncation flag, got %d rows truncated=%v",
			relocationPreviewLimit, len(plan.Files), plan.Truncated)
	}
	if plan.Unchanged != total {
		t.Errorf("every file is already in place, but %d were counted as unchanged", plan.Unchanged)
	}
}

func TestRelocateReportsProgress(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	for _, name := range []string{"one.txt", "two.txt"} {
		if _, _, err := v.Upload(ctx, MainScope, "/", name, []byte(name), UploadOptions{Accounts: ids[:3]}); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	}

	var seen []string
	var lastTotal int
	if _, err := v.Relocate(ctx, MainScope, "/", ids[3:], archive.Scheme{}, func(path string, done, total int) {
		seen = append(seen, path)
		lastTotal = total
		if done != len(seen) {
			t.Errorf("progress reported %d done on call %d", done, len(seen))
		}
	}); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if len(seen) != 2 || lastTotal != 2 {
		t.Errorf("progress saw %v of %d", seen, lastTotal)
	}
}

func TestRelocatePlanIsIndependentOfShardOrder(t *testing.T) {
	// The planner must answer the same way whichever order the index happens to
	// list a file's parts in, or a relocation would move different parts on
	// different runs.
	byID := map[string]provider.Config{
		"a": {ID: "a", Name: "A", Kind: provider.KindLocal},
		"b": {ID: "b", Name: "B", Kind: provider.KindLocal},
		"c": {ID: "c", Name: "C", Kind: provider.KindLocal},
		"d": {ID: "d", Name: "D", Kind: provider.KindLocal},
	}
	shards := []Shard{
		{Part: 3, ProviderID: "c", Size: 30},
		{Part: 1, ProviderID: "a", Size: 10},
		{Part: 2, ProviderID: "b", Size: 20},
	}
	entry := &Entry{ID: "x", Name: "x.txt", Dir: "/", ArchiveID: "ff", Shards: shards}

	plan := planFileRelocation(MainScope, entry, []string{"a", "b", "d"}, byID, 1, archive.SchemeDefault, relocationOptions{})
	if len(plan.Moves) != 1 || plan.Moves[0].Part != 3 || plan.Moves[0].To != "d" {
		t.Fatalf("plan = %+v, want part 3 to d", plan.Moves)
	}
	if len(plan.Stay) != 2 {
		t.Errorf("expected parts 1 and 2 to stay, got %v", plan.Stay)
	}
	if plan.Moves[0].Bytes != 30 {
		t.Errorf("move weighs %d, want the shard's 30", plan.Moves[0].Bytes)
	}
}

// ---------------------------------------------------------------------------
// Changing the scheme a file is stored under
// ---------------------------------------------------------------------------

// Asking for six clouds having lived on three is not a move at all: a 2-of-3
// file and a 4-of-6 file share no shards, so the file is gathered, cut again
// and written out. The plan says so before it happens.
func TestRelocateToSixCloudsRecodesTheFile(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := []byte("three clouds now, four of six afterwards\n")
	entry, _, err := v.Upload(ctx, MainScope, "/", "widen.txt", payload, UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeDefault {
		t.Fatalf("uploaded as %s, want %s", got, archive.SchemeDefault)
	}

	plan, err := v.PlanRelocation(MainScope, entry.ID, ids, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Recoded != 1 {
		t.Fatalf("plan re-encodes %d files, want 1", plan.Recoded)
	}
	if plan.Moves != 0 {
		t.Errorf("plan moves %d shards, want none — nothing is portable across schemes", plan.Moves)
	}
	if plan.RecodeBytes == 0 {
		t.Error("a re-encode has to be priced, not reported as free")
	}
	fp := plan.Files[0]
	if fp.From != "2-of-3" || fp.To != "4-of-6" {
		t.Errorf("plan says %s → %s, want 2-of-3 → 4-of-6", fp.From, fp.To)
	}
	// The whole point of saying so: the estimate is loud about the cost.
	if len(plan.Warnings) == 0 {
		t.Error("re-encoding a file should warn that the whole of it moves")
	}

	report, err := v.Relocate(ctx, MainScope, entry.ID, ids, archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if !report.Done() {
		t.Fatalf("relocation did not finish: %+v", report.Warnings)
	}
	if report.Recoded != 1 {
		t.Errorf("report re-encoded %d files, want 1", report.Recoded)
	}

	after := v.manifest.ByID(entry.ID)
	if got := after.Scheme(); got != archive.SchemeWide {
		t.Fatalf("file is stored as %s, want %s", got, archive.SchemeWide)
	}
	if len(after.Shards) != 6 {
		t.Fatalf("the file has %d shards, want 6", len(after.Shards))
	}
	perAccount := map[string]int{}
	for _, sh := range after.Shards {
		perAccount[sh.ProviderID]++
	}
	if len(perAccount) != 6 {
		t.Errorf("the six shards landed on %d accounts, want 6", len(perAccount))
	}
	for id, n := range perAccount {
		if n != 1 {
			t.Errorf("account %s holds %d shards of one file, want 1", id, n)
		}
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after re-encoding: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the re-encoded file does not match the original")
	}
}

// A 4-of-6 file survives any two of its accounts going dark, which is the whole
// reason for choosing it.
func TestAWideFileSurvivesTwoAccountsGoingDark(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := bytes.Repeat([]byte("durable across six clouds\n"), 5000)
	entry, _, err := v.Upload(ctx, MainScope, "/", "durable.bin", payload, UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeWide {
		t.Fatalf("uploaded as %s, want %s", got, archive.SchemeWide)
	}

	for _, id := range ids[:2] {
		if err := v.RemoveProvider(id, true); err != nil {
			t.Fatalf("RemoveProvider: %v", err)
		}
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch with two of six accounts gone: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the file rebuilt from four shards does not match the original")
	}
}

// Narrowing back to three re-encodes in the other direction, and erases every
// shard the wide scheme had written.
func TestRelocateBackToThreeCloudsRecodesAndCleansUp(t *testing.T) {
	v, roots := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := []byte("six clouds now, three afterwards\n")
	entry, _, err := v.Upload(ctx, MainScope, "/", "narrow.txt", payload, UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	report, err := v.Relocate(ctx, MainScope, entry.ID, ids[:3], archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if !report.Done() {
		t.Fatalf("relocation did not finish: %+v", report.Warnings)
	}

	after := v.manifest.ByID(entry.ID)
	if got := after.Scheme(); got != archive.SchemeDefault {
		t.Fatalf("file is stored as %s, want %s", got, archive.SchemeDefault)
	}
	kept := map[string]bool{ids[0]: true, ids[1]: true, ids[2]: true}
	for _, sh := range after.Shards {
		if !kept[sh.ProviderID] {
			t.Errorf("shard %d is on %s, which was not chosen", sh.Part, sh.ProviderName)
		}
	}
	// Nothing of the old wide layout is left behind on the dropped accounts.
	for _, root := range roots[3:] {
		if n := objectsIn(t, root); n != 0 {
			t.Errorf("%s still holds %d object(s) of the narrowed file", root, n)
		}
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after narrowing: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the narrowed file does not match the original")
	}
}

// Moving a wide file sideways — six clouds to six different clouds — is still a
// shard-by-shard copy, because the scheme has not changed.
func TestRelocateBetweenSixCloudsMovesShardsRatherThanRecoding(t *testing.T) {
	v, _ := newTestVault(t, 7)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := []byte("sideways, not rebuilt\n")
	entry, _, err := v.Upload(ctx, MainScope, "/", "sideways.txt", payload, UploadOptions{Accounts: ids[:6]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Swap one of the six for the seventh account.
	targets := append(append([]string{}, ids[:5]...), ids[6])
	plan, err := v.PlanRelocation(MainScope, entry.ID, targets, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Recoded != 0 {
		t.Fatalf("same-width move re-encoded %d files, want none", plan.Recoded)
	}
	if plan.Moves != 1 {
		t.Fatalf("plan moves %d shards, want 1", plan.Moves)
	}

	if _, err := v.Relocate(ctx, MainScope, entry.ID, targets, archive.Scheme{}, nil); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	after := v.manifest.ByID(entry.ID)
	if got := after.Scheme(); got != archive.SchemeWide {
		t.Errorf("the scheme changed to %s during a sideways move", got)
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the moved file does not match the original")
	}
}

// A relocation is offered the same widths an upload is.
func TestRelocateRejectsACountThatNamesNoScheme(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "odd.txt", []byte("x"), UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.PlanRelocation(MainScope, entry.ID, ids[:4], archive.Scheme{}); err == nil {
		t.Fatal("expected a relocation onto four accounts to be refused")
	}
}

func TestRelocateRebuildsAFileThatIsShortAPart(t *testing.T) {
	// A file uploaded while one of its accounts was refusing is short a shard
	// for good: the shard is on no account, so there is nothing to copy from
	// and moving the ones that did land would leave the file just as short on
	// the new clouds. It has to be cut again — which is the whole point of the
	// "files missing a spare part" list being able to send one somewhere.
	v, _ := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := []byte("two parts of three ever landed")
	entry, _, err := v.Upload(ctx, MainScope, "/", "short.txt", payload, UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Forget the third part the way a forced disconnect does.
	v.mu.Lock()
	live := v.manifest.ByID(entry.ID)
	live.Shards = live.Shards[:2]
	err = v.persistLocked()
	v.mu.Unlock()
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Same width, same code, and still not a shard-by-shard move.
	plan, err := v.PlanRelocation(MainScope, entry.ID, ids[1:], archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	row := plan.Files[0]
	if !row.Recode || !row.Repair {
		t.Fatalf("plan is recode=%v repair=%v, want both — the file is short a shard",
			row.Recode, row.Repair)
	}
	if row.Missing != 1 {
		t.Errorf("Missing = %d, want 1", row.Missing)
	}
	if plan.Recoded != 1 || plan.Moves != 0 {
		t.Errorf("plan says %d rebuilt and %d shards moved, want 1 and 0", plan.Recoded, plan.Moves)
	}
	if len(plan.Warnings) == 0 {
		t.Error("a rebuild costs the whole file and should be said out loud")
	}

	report, err := v.Relocate(ctx, MainScope, entry.ID, ids[1:], archive.Scheme{}, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.Recoded != 1 {
		t.Errorf("Recoded = %d, want 1", report.Recoded)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if after.Redundancy() != after.Scheme().Total {
		t.Errorf("still %d of %d shards after the rebuild",
			after.Redundancy(), after.Scheme().Total)
	}
	for _, s := range after.Shards {
		if s.ProviderID == ids[0] {
			t.Errorf("shard %d stayed on the account it was moved off", s.Part)
		}
	}

	// And it is still the same file.
	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("rebuilt file reads back as %q, want %q", data, payload)
	}

	// Nothing is left in the list the accounts panel counts.
	page, err := v.Degraded(0, 0)
	if err != nil {
		t.Fatalf("Degraded: %v", err)
	}
	if page.Total != 0 {
		t.Errorf("%d file(s) still short after the repair", page.Total)
	}
}

func TestRelocateLeavesAWholeFileAlone(t *testing.T) {
	// The other half of the rule above: a file with all its shards, asked for
	// the accounts it is already on, is still not rebuilt. Repairing must not
	// turn every relocation into a re-upload.
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "whole.txt", []byte("all present"), UploadOptions{
		Accounts: ids,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	plan, err := v.PlanRelocation(MainScope, entry.ID, ids, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Recoded != 0 || plan.Moves != 0 || plan.Unchanged != 1 {
		t.Errorf("plan says %d rebuilt, %d moved, %d unchanged; want 0, 0 and 1",
			plan.Recoded, plan.Moves, plan.Unchanged)
	}
}

// A watched relocation reports as it goes: the file it is on, and bytes
// crossed against the plan's total — which is what a progress bar draws.
func TestRelocateWatchedReportsBytesAgainstThePlan(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := bytes.Repeat([]byte("a byte counted is a byte drawn\n"), 200)
	entry, _, err := v.Upload(ctx, MainScope, "/", "watched.txt", payload, UploadOptions{
		Accounts: ids[:3],
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := placementOf(entry)
	targets := []string{before[1], before[2], ids[3]}

	var seen []RelocationProgress
	report, err := v.RelocateWatched(ctx, MainScope, "/watched.txt", targets, archive.Scheme{},
		func(at RelocationProgress) { seen = append(seen, at) })
	if err != nil {
		t.Fatalf("RelocateWatched: %v", err)
	}
	if report.Relocated != 1 {
		t.Fatalf("relocated %d files, want 1: %+v", report.Relocated, report)
	}
	if len(seen) == 0 {
		t.Fatal("a watched relocation reported nothing")
	}

	// The file is announced before a byte of it moves.
	first := seen[0]
	if first.Path != "/watched.txt" || first.File != 1 || first.Files != 1 || first.Done != 0 {
		t.Errorf("first report = %+v, want /watched.txt as file 1 of 1, none done", first)
	}
	if first.Total <= 0 {
		t.Errorf("the bar has no denominator: total = %d", first.Total)
	}

	// Every report stays inside the plan's bill, and the last one closes it
	// out: bytes match what the run says moved, and the file counts as done.
	last := seen[len(seen)-1]
	for _, at := range seen {
		if at.Bytes > at.Total {
			t.Errorf("reported %d of %d bytes", at.Bytes, at.Total)
		}
	}
	if last.Done != 1 {
		t.Errorf("last report says %d files done, want 1", last.Done)
	}
	if last.Bytes != report.Bytes {
		t.Errorf("last report says %d bytes, the run says %d", last.Bytes, report.Bytes)
	}
	if last.Bytes <= 0 {
		t.Error("no bytes were ever reported")
	}
}
