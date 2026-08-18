package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// How many clouds a file is spread over settles the erasure code it is cut
// with: three is 2-of-3, six is 4-of-6, nine is 6-of-9. Storage is 1.5× at all
// three, so what the wider ones buy is durability and a higher bar for
// collusion — and never a second shard on any one account.

// accountNames makes n stand-in account IDs, for the placement tests that care
// about the shape of a plan rather than about real providers.
func accountNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("cloud-%03d", i)
	}
	return out
}

// rootHolding finds the account directory a stored object lives in.
func rootHolding(t *testing.T, roots []string, key string) string {
	t.Helper()
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key))); err == nil {
			return root
		}
	}
	return ""
}

func TestSixAccountsStoreAFileAsFourOfSix(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "wide.txt", []byte("payload"),
		UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	if got := entry.Scheme(); got != archive.SchemeWide {
		t.Fatalf("stored as %s, want %s", got, archive.SchemeWide)
	}
	if got := len(entry.Shards); got != 6 {
		t.Fatalf("stored %d shards over six accounts, want 6", got)
	}
	if got := entry.Spare(); got != 2 {
		t.Errorf("Spare = %d, want 2 — %s survives two accounts going dark", got, archive.SchemeWide)
	}

	// The whole point of the strict policy survives the widening: an attacker
	// holding one of the six accounts holds one shard of the four a rebuild
	// takes, which is less of the file than one account of three ever held.
	seen := map[string]int{}
	for _, s := range entry.Shards {
		seen[s.ProviderID]++
	}
	if len(seen) != 6 {
		t.Fatalf("the six shards landed on %d accounts, want 6", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("account %s holds %d shards of one file, want 1", id, n)
		}
	}
}

func TestNineAccountsStoreAFileAsSixOfNine(t *testing.T) {
	v, _ := newTestVault(t, 9)
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "widest.txt", []byte("payload"),
		UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeWider {
		t.Fatalf("stored as %s, want %s", got, archive.SchemeWider)
	}
	if got := len(holders(entry)); got != 9 {
		t.Errorf("the nine shards landed on %d accounts, want 9", got)
	}
	if got := entry.Spare(); got != 3 {
		t.Errorf("Spare = %d, want 3", got)
	}
}

// Storage is the scheme's ratio, and every scheme SAND writes has the same one.
// Widening costs accounts, not bytes — which is the property that makes it
// worth offering at all.
func TestEverySchemeStoresOneAndAHalfTimesTheFile(t *testing.T) {
	for _, tc := range []struct {
		accounts int
		scheme   archive.Scheme
	}{{3, archive.SchemeDefault}, {6, archive.SchemeWide}, {9, archive.SchemeWider}} {
		t.Run(tc.scheme.String(), func(t *testing.T) {
			v, _ := newTestVault(t, tc.accounts)
			ids := accountIDs(t, v)

			payload := make([]byte, 200*1024)
			if _, err := rand.Read(payload); err != nil {
				t.Fatalf("rand: %v", err)
			}
			entry, _, err := v.Upload(context.Background(), MainScope, "/", "sized.bin", payload,
				UploadOptions{Accounts: ids})
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if got := entry.Scheme(); got != tc.scheme {
				t.Fatalf("stored as %s, want %s", got, tc.scheme)
			}

			stored := int64(0)
			for _, s := range entry.Shards {
				stored += s.Size
			}
			// Random bytes do not compress, so the stored total is the scheme's
			// ratio plus the per-shard headers and tags.
			ratio := float64(stored) / float64(len(payload))
			if ratio < 1.4 || ratio > 1.7 {
				t.Errorf("%s stored %.2f× the file, want about 1.5×", tc.scheme, ratio)
			}
		})
	}
}

// Any n − k of a wide file's accounts can go dark. This wipes exactly that many
// and reads the file back from what is left, which is the claim the scheme
// exists to make.
func TestEverySchemeSurvivesItsToleranceGoingDark(t *testing.T) {
	for _, tc := range []struct {
		accounts int
		scheme   archive.Scheme
	}{{3, archive.SchemeDefault}, {6, archive.SchemeWide}, {9, archive.SchemeWider}} {
		t.Run(tc.scheme.String(), func(t *testing.T) {
			v, roots := newTestVault(t, tc.accounts)
			ids := accountIDs(t, v)

			payload := make([]byte, 96*1024)
			if _, err := rand.Read(payload); err != nil {
				t.Fatalf("rand: %v", err)
			}
			entry, _, err := v.Upload(context.Background(), MainScope, "/", "durable.bin", payload,
				UploadOptions{Accounts: ids})
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			// The manifest backup is pushed in the background into these very
			// directories, so let it land before pulling them out from under it.
			v.AwaitBackupSync()

			wiped := 0
			for _, s := range entry.Shards {
				if wiped == tc.scheme.Tolerance() {
					break
				}
				root := rootHolding(t, roots, ChunkShardKey(entry.ArchiveID, 0, s.Part))
				if root == "" {
					t.Fatalf("could not find the account holding shard %d", s.Part)
				}
				if err := os.RemoveAll(root); err != nil {
					t.Fatalf("removing account root: %v", err)
				}
				wiped++
			}
			if wiped != tc.scheme.Tolerance() {
				t.Fatalf("wiped %d accounts, want %d", wiped, tc.scheme.Tolerance())
			}

			data, _, err := v.Fetch(context.Background(), entry.ID)
			if err != nil {
				t.Fatalf("Fetch with %d of %d accounts down: %v", wiped, tc.accounts, err)
			}
			if !bytes.Equal(data, payload) {
				t.Error("the rebuilt file does not match the original")
			}
		})
	}
}

// One past the tolerance is not "nearly enough": the file is gone, and the read
// has to say so rather than hand back something plausible.
func TestOneAccountPastTheToleranceIsUnreadable(t *testing.T) {
	v, roots := newTestVault(t, 6)
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "fragile.txt", []byte("four of six"),
		UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	wiped := 0
	for _, s := range entry.Shards {
		if wiped == archive.SchemeWide.Tolerance()+1 {
			break
		}
		if root := rootHolding(t, roots, ChunkShardKey(entry.ArchiveID, 0, s.Part)); root != "" {
			_ = os.RemoveAll(root)
			wiped++
		}
	}

	if _, _, err := v.Fetch(context.Background(), entry.ID); err == nil {
		t.Fatalf("read a %s file with %d accounts gone", archive.SchemeWide, wiped)
	}
}

func TestSixAccountDefaultAppliesToEveryUpload(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts(ids); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "bydefault.txt", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeWide {
		t.Errorf("stored as %s, want %s — the six-account default was not honoured",
			got, archive.SchemeWide)
	}
}

// With nothing chosen and nothing stored as a default, an upload still takes
// three clouds. A vault does not silently start paying for six.
func TestDefaultSpreadIsStillThreeClouds(t *testing.T) {
	v, _ := newTestVault(t, 9)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "ordinary.txt", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := len(holders(entry)); got != AccountsPerFile {
		t.Errorf("an upload with no choice landed on %d accounts, want %d", got, AccountsPerFile)
	}
	if got := entry.Scheme(); got != archive.SchemeDefault {
		t.Errorf("stored as %s, want %s", got, archive.SchemeDefault)
	}
}

func TestUploadRejectsACountThatNamesNoScheme(t *testing.T) {
	v, _ := newTestVault(t, 9)
	ids := accountIDs(t, v)

	for _, n := range []int{4, 5, 7, 8} {
		t.Run(fmt.Sprintf("%d accounts", n), func(t *testing.T) {
			_, _, err := v.Upload(context.Background(), MainScope, "/", fmt.Sprintf("odd%d.txt", n), []byte("x"),
				UploadOptions{Accounts: ids[:n]})
			if err == nil {
				t.Fatalf("expected %d accounts to be refused — it names no scheme", n)
			}
		})
	}
}

func TestSetDefaultAccountsTakesSchemeWidthsOnly(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts(ids[:5]); err == nil {
		t.Fatal("expected a default of five accounts to be refused")
	}
	if err := v.SetDefaultAccounts(ids); err != nil {
		t.Fatalf("a default of six should be accepted: %v", err)
	}
	if got := v.DefaultAccounts(); len(got) != 6 {
		t.Errorf("DefaultAccounts = %v, want all six", got)
	}
}

// A six-account default that loses an account becomes a three-account default
// rather than a five-account one, which names no scheme at all.
func TestDisconnectTrimsAWideDefaultToASchemeWidth(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts(ids); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	if err := v.RemoveProvider(ids[4], true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}

	got := v.DefaultAccounts()
	if len(got) != AccountsPerFile {
		t.Fatalf("DefaultAccounts = %v, want it trimmed to %d", got, AccountsPerFile)
	}
	for _, id := range got {
		if id == ids[4] {
			t.Error("the disconnected account is still in the default")
		}
	}
}

// Re-encrypting after a password change is not a move, and it is not a
// narrowing either: a 4-of-6 file comes back as a 4-of-6 file.
func TestMigrationKeepsAWideSchemeWide(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "wide.txt", []byte("payload"), UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := v.ChangePassword(ctx, testPassword, "a-much-longer-new-password", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	after := v.manifest.ByID(entry.ID)
	if after == nil {
		t.Fatal("the file went missing during re-encryption")
	}
	if got := after.Scheme(); got != archive.SchemeWide {
		t.Errorf("came back as %s after re-encryption, want %s", got, archive.SchemeWide)
	}
	if got := len(holders(after)); got != 6 {
		t.Errorf("the file came back on %d accounts, want 6", got)
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after re-encryption: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("round-trip mismatch: %q", data)
	}
}

// The rule, not a list: every multiple of three names a scheme, all the way to
// what a one-byte shard number can count.
func TestSchemeForAccountCounts(t *testing.T) {
	for accounts, want := range map[int]archive.Scheme{
		1:  archive.SchemeDefault,
		2:  archive.SchemeDefault,
		3:  archive.SchemeDefault,
		6:  archive.SchemeWide,
		9:  archive.SchemeWider,
		12: {Data: 8, Total: 12},
		30: {Data: 20, Total: 30},
		99: {Data: 66, Total: 99},
		archive.MaxAccounts: {
			Data:  archive.MaxAccounts / 3 * 2,
			Total: archive.MaxAccounts,
		},
	} {
		got, err := SchemeFor(accounts)
		if err != nil {
			t.Errorf("SchemeFor(%d): %v", accounts, err)
			continue
		}
		if got != want {
			t.Errorf("SchemeFor(%d) = %s, want %s", accounts, got, want)
		}
		if got.Total%3 != 0 || got.Data*3 != got.Total*2 {
			t.Errorf("SchemeFor(%d) = %s, which is not 2m-of-3m", accounts, got)
		}
	}

	// The counts between groups, and the one past the ceiling.
	for _, n := range []int{4, 5, 7, 8, 10, 11, 100, archive.MaxAccounts + 3} {
		if _, err := SchemeFor(n); err == nil {
			t.Errorf("SchemeFor(%d) named a scheme, want a refusal", n)
		}
	}
}

// Every scheme in the family stores the same 1.5×, which is the property that
// makes a wider vault free in bytes. Checked as arithmetic over the whole
// family rather than over the handful a vault test can afford to stand up.
func TestTheWholeFamilyStoresOneAndAHalf(t *testing.T) {
	for accounts := 3; accounts <= archive.MaxAccounts; accounts += 3 {
		scheme, err := SchemeFor(accounts)
		if err != nil {
			t.Fatalf("SchemeFor(%d): %v", accounts, err)
		}
		if ratio := float64(scheme.Total) / float64(scheme.Data); ratio != 1.5 {
			t.Fatalf("%s stores %.3f×, want 1.5×", scheme, ratio)
		}
		if want := accounts / 3; scheme.Tolerance() != want {
			t.Errorf("%s survives %d losses, want %d — one per group",
				scheme, scheme.Tolerance(), want)
		}
	}
}

func TestBuildPlanGivesEveryAccountOneShard(t *testing.T) {
	for _, tc := range []struct {
		scheme archive.Scheme
		ids    []string
	}{
		{archive.SchemeDefault, []string{"a", "b", "c"}},
		{archive.SchemeWide, []string{"a", "b", "c", "d", "e", "f"}},
		{archive.SchemeWider, []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{archive.SchemeForGroups(5), accountNames(15)},
		{archive.SchemeForGroups(20), accountNames(60)},
	} {
		t.Run(tc.scheme.String(), func(t *testing.T) {
			plan, err := BuildPlan(tc.ids, PolicyStrict, tc.scheme, 5)
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			if len(plan) != tc.scheme.Total {
				t.Fatalf("plan places %d shards, want %d", len(plan), tc.scheme.Total)
			}
			seen := map[string]bool{}
			for shard, id := range plan {
				if shard < 1 || shard > tc.scheme.Total {
					t.Errorf("plan names shard %d, outside 1..%d", shard, tc.scheme.Total)
				}
				if seen[id] {
					t.Errorf("account %s holds more than one shard", id)
				}
				seen[id] = true
			}
			if len(seen) != len(tc.ids) {
				t.Errorf("the plan used %d of %d accounts", len(seen), len(tc.ids))
			}
		})
	}
}

// A file that lost one of its six accounts is filled back up to six, not down
// to three: SelectAccounts rounds a preference up to the next scheme.
func TestSelectAccountsRefillsToASchemeWidth(t *testing.T) {
	available := []string{"a", "b", "c", "d", "e", "f", "g"}

	cases := []struct {
		name      string
		preferred []string
		want      int
	}{
		{"nothing preferred", nil, 3},
		{"a file on three", []string{"a", "b", "c"}, 3},
		{"a file on three that lost one", []string{"a", "b"}, 3},
		{"a file on six", []string{"a", "b", "c", "d", "e", "f"}, 6},
		{"a file on six that lost one", []string{"a", "b", "c", "d", "e"}, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectAccounts(available, tc.preferred, 0, 7)
			if len(got) != tc.want {
				t.Fatalf("SelectAccounts chose %d accounts (%v), want %d", len(got), got, tc.want)
			}
			seen := map[string]bool{}
			for _, id := range got {
				if seen[id] {
					t.Errorf("account %s chosen twice", id)
				}
				seen[id] = true
			}
			for _, id := range tc.preferred {
				if !seen[id] {
					t.Errorf("preferred account %s was dropped", id)
				}
			}
		})
	}
}

// Nothing can be filled in that is not connected, so a preference wider than
// the vault falls back to the widest scheme that fits.
func TestSelectAccountsCannotExceedWhatIsConnected(t *testing.T) {
	got := SelectAccounts([]string{"a", "b", "c", "d"}, []string{"a", "b", "c", "d"}, 0, 3)
	if len(got) != AccountsPerFile {
		t.Errorf("SelectAccounts chose %d accounts (%v), want %d", len(got), got, AccountsPerFile)
	}
}

// A large file is chunked, and placement is decided once for the whole file —
// so every chunk has to land in the same two copies as the rest.
func TestChunkedFileOverSixAccountsRoundTrips(t *testing.T) {
	v, roots := newTestVault(t, 6)
	ids := accountIDs(t, v)

	payload := make([]byte, 5*int(archive.DefaultChunkSize)/2)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "film.bin", payload,
		UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !entry.Chunked() {
		t.Fatal("the payload should have been stored chunked")
	}
	if got := entry.Scheme(); got != archive.SchemeWide {
		t.Fatalf("stored as %s, want %s", got, archive.SchemeWide)
	}

	// Every copy has to be whole across every chunk, or a read part-way
	// through the file would fall over.
	for index := 0; index < entry.ChunkCount; index++ {
		for _, s := range entry.Shards {
			key := ChunkShardKey(entry.ArchiveID, index, s.Part)
			if rootHolding(t, roots, key) == "" {
				t.Fatalf("chunk %d of part %d is missing from every account", index, s.Part)
			}
		}
	}

	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the chunked round-trip does not match the original")
	}
}

func TestHealthCountsAgainstTheFilesOwnScheme(t *testing.T) {
	v, roots := newTestVault(t, 6)
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "checked.txt", []byte("x"),
		UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()

	// Two of the six go: 4-of-6 needs four, so the file is still recoverable
	// with no spare left.
	gone := 0
	for _, s := range entry.Shards {
		if gone == archive.SchemeWide.Tolerance() {
			break
		}
		if root := rootHolding(t, roots, ChunkShardKey(entry.ArchiveID, 0, s.Part)); root != "" {
			_ = os.RemoveAll(root)
			gone++
		}
	}

	health, err := v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Recoverable {
		t.Error("four of six shards standing should still be recoverable")
	}
	if health.Scheme != archive.SchemeWide.String() {
		t.Errorf("health reports scheme %q, want %q", health.Scheme, archive.SchemeWide)
	}
	if health.Spare != 0 {
		t.Errorf("Spare = %d with the tolerance used up, want 0", health.Spare)
	}
}

// Twelve clouds is not a width anyone wrote down; it follows from the rule.
// This is the end-to-end proof that the rule holds outside the three widths
// that get named in prose — an upload, a read, and a read after four of the
// twelve have gone dark.
func TestATwelveCloudVaultCutsFilesEightOfTwelve(t *testing.T) {
	v, roots := newTestVault(t, 12)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := make([]byte, 300*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	entry, warnings, err := v.Upload(ctx, MainScope, "/", "twelve.bin", payload, UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	want := archive.Scheme{Data: 8, Total: 12}
	if got := entry.Scheme(); got != want {
		t.Fatalf("stored as %s, want %s", got, want)
	}
	if got := len(holders(entry)); got != 12 {
		t.Fatalf("the twelve shards landed on %d accounts, want 12", got)
	}
	v.AwaitBackupSync()
	for _, s := range entry.Shards {
		if s.Part < 1 || s.Part > 12 {
			t.Errorf("shard numbered %d, outside 1..12", s.Part)
		}
	}

	// Four of twelve go dark — the tolerance — and the file still reads.
	gone := 0
	for _, s := range entry.Shards {
		if gone == want.Tolerance() {
			break
		}
		if root := rootHolding(t, roots, ChunkShardKey(entry.ArchiveID, 0, s.Part)); root != "" {
			_ = os.RemoveAll(root)
			gone++
		}
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch with %d of 12 accounts down: %v", gone, err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the rebuilt file does not match the original")
	}
}

// Widening past the named widths is the same operation as widening to them: a
// re-encode, priced as one.
func TestRelocatingToTwelveCloudsRecodes(t *testing.T) {
	v, _ := newTestVault(t, 12)
	ctx := context.Background()
	ids := accountIDs(t, v)

	payload := []byte("three clouds now, twelve afterwards\n")
	entry, _, err := v.Upload(ctx, MainScope, "/", "grow.txt", payload, UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	plan, err := v.PlanRelocation(MainScope, entry.ID, ids, archive.Scheme{})
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Recoded != 1 || plan.Moves != 0 {
		t.Fatalf("plan re-encodes %d and moves %d, want 1 and 0", plan.Recoded, plan.Moves)
	}
	if plan.Files[0].To != "8-of-12" {
		t.Errorf("plan targets %s, want 8-of-12", plan.Files[0].To)
	}

	if _, err := v.Relocate(ctx, MainScope, entry.ID, ids, archive.Scheme{}, nil); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	after := v.manifest.ByID(entry.ID)
	if got := after.Scheme(); got != (archive.Scheme{Data: 8, Total: 12}) {
		t.Fatalf("file is stored as %s, want 8-of-12", got)
	}

	data, _, err := v.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("the re-encoded file does not match the original")
	}
}
