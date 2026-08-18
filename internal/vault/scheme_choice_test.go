package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// A scheme named for one file, rather than inferred from how many clouds it
// landed on. The default family answers "how wide", and only that; these are the
// files that answer "how wide, and at what tradeoff" — 3-of-5 on five clouds,
// 6-of-10 on ten, 2-of-5 when durability is worth 2.5× the bytes.

// storedShardKeys is every object one file has on the accounts, so a test can
// take some of them away.
func schemeTestPayload(t *testing.T, n int) []byte {
	t.Helper()
	payload := make([]byte, n)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return payload
}

func TestNamedSchemeCutsAFileOutsideTheDefaultFamily(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts int
		scheme   archive.Scheme
	}{
		{"three of five", 5, archive.Scheme{Data: 3, Total: 5}},
		{"two of five", 5, archive.Scheme{Data: 2, Total: 5}},
		{"six of ten", 10, archive.Scheme{Data: 6, Total: 10}},
		{"four of seven", 7, archive.Scheme{Data: 4, Total: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := newTestVault(t, tc.accounts)
			ids := accountIDs(t, v)
			payload := schemeTestPayload(t, 64*1024)

			entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "named.bin", payload,
				UploadOptions{Accounts: ids, Scheme: tc.scheme})
			if err != nil {
				t.Fatalf("Upload as %s: %v", tc.scheme, err)
			}
			if len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}

			if got := entry.Scheme(); got != tc.scheme {
				t.Fatalf("stored as %s, want %s", got, tc.scheme)
			}
			// One shard per account, which is the promise the strict policy
			// makes and which widening must not weaken.
			if got := len(entry.Shards); got != tc.scheme.Total {
				t.Fatalf("stored %d shards, want %d", got, tc.scheme.Total)
			}
			held := map[string]int{}
			for _, s := range entry.Shards {
				held[s.ProviderID]++
			}
			for id, n := range held {
				if n != 1 {
					t.Errorf("account %s holds %d shards of one file, want 1", id, n)
				}
			}

			got, _, err := v.Fetch(context.Background(), entry.ID)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("what came back is not what went up")
			}
		})
	}
}

func TestANamedSchemeSurvivesLosingEveryShardItCanSpare(t *testing.T) {
	// 2-of-5 is the point of naming a scheme at all: it survives three losses
	// where 2-of-3 survives one, and pays 2.5× rather than 1.5× for it.
	scheme := archive.Scheme{Data: 2, Total: 5}
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)
	payload := schemeTestPayload(t, 96*1024)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "fragile.bin", payload,
		UploadOptions{Accounts: ids, Scheme: scheme})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Spare(); got != scheme.Tolerance() {
		t.Fatalf("%s reports %d spare shards, want %d", scheme, got, scheme.Tolerance())
	}

	// Disconnect every account but the two a rebuild needs. Reading has to come
	// down to arithmetic over the parity shards, since the survivors are not
	// the data shards.
	for _, id := range ids[:scheme.Tolerance()] {
		if err := v.RemoveProvider(id, true); err != nil {
			t.Fatalf("RemoveProvider: %v", err)
		}
	}

	got, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after losing %d of %d accounts: %v", scheme.Tolerance(), scheme.Total, err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("what came back is not what went up")
	}
}

func TestChangingThePasswordKeepsAFileCutTheWayItWas(t *testing.T) {
	// The trap this guards: re-encryption goes back to the accounts the file is
	// already on, and five accounts round up to the default family's six. A
	// rotation that also recoded the file would be changing a tradeoff nobody
	// asked it to change.
	scheme := archive.Scheme{Data: 3, Total: 5}
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	payload := schemeTestPayload(t, 48*1024)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "rotated.bin", payload,
		UploadOptions{Accounts: ids[:5], Scheme: scheme})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := v.ChangePassword(context.Background(), testPassword, "a different password entirely", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("the file is gone after re-encryption: %v", err)
	}
	if got := after.Scheme(); got != scheme {
		t.Fatalf("re-encryption recoded the file to %s, want it left as %s", got, scheme)
	}
	if got := len(after.Shards); got != scheme.Total {
		t.Fatalf("re-encrypted onto %d shards, want %d", got, scheme.Total)
	}

	got, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after re-encryption: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("what came back is not what went up")
	}
}

func TestNamingAccountsToMigrateOntoStillSettlesTheCode(t *testing.T) {
	// The other half of the rule above. Keeping a file's own scheme is right
	// when nobody said where it should go; when somebody does, the count they
	// named settles the code as it does for an upload — otherwise reclaiming a
	// six-cloud vault onto three accounts would fail rather than narrow.
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	payload := schemeTestPayload(t, 32*1024)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "narrowing.bin", payload,
		UploadOptions{Accounts: ids})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeWide {
		t.Fatalf("stored as %s, want %s", got, archive.SchemeWide)
	}

	if _, _, _, err := v.migrateFile(context.Background(), MainScope, entry.ID, ids[:3], archive.Scheme{}); err != nil {
		t.Fatalf("migrating onto three named accounts: %v", err)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if got := after.Scheme(); got != archive.SchemeDefault {
		t.Fatalf("narrowed to %s, want %s", got, archive.SchemeDefault)
	}
	got, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after narrowing: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("what came back is not what went up")
	}
}

func TestRelocateRecodesToANamedScheme(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)
	payload := schemeTestPayload(t, 40*1024)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "recoded.bin", payload,
		UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeDefault {
		t.Fatalf("stored as %s, want %s", got, archive.SchemeDefault)
	}

	want := archive.Scheme{Data: 3, Total: 5}
	plan, err := v.PlanRelocation(MainScope, entry.ID, ids, want)
	if err != nil {
		t.Fatalf("PlanRelocation: %v", err)
	}
	if plan.Recoded != 1 {
		t.Fatalf("planned %d recodes, want 1", plan.Recoded)
	}
	if plan.Files[0].To != want.String() {
		t.Fatalf("planned a recode to %s, want %s", plan.Files[0].To, want)
	}

	report, err := v.Relocate(context.Background(), MainScope, entry.ID, ids, want, nil)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if report.Recoded != 1 {
		t.Fatalf("recoded %d files, want 1", report.Recoded)
	}

	after, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry after recode: %v", err)
	}
	if got := after.Scheme(); got != want {
		t.Fatalf("ended up as %s, want %s", got, want)
	}
	got, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after recode: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("what came back is not what went up")
	}
}

func TestASchemeTheVaultWillNotWriteIsRefusedAtTheUpload(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	for _, tc := range []struct {
		name   string
		ids    []string
		scheme archive.Scheme
		want   string
	}{{
		name:   "one data shard would put the whole file on one account",
		ids:    ids[:3],
		scheme: archive.Scheme{Data: 1, Total: 3},
		want:   "at least 2 shards",
	}, {
		name:   "more shards needed than exist",
		ids:    ids[:3],
		scheme: archive.Scheme{Data: 4, Total: 3},
		want:   "more shards to rebuild than it makes",
	}, {
		name:   "an account chosen that would hold nothing",
		ids:    ids,
		scheme: archive.Scheme{Data: 2, Total: 3},
		want:   "would hold nothing",
	}, {
		name:   "fewer accounts than the code needs to stay rebuildable",
		ids:    ids[:2],
		scheme: archive.Scheme{Data: 3, Total: 5},
		want:   "at least 3",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := v.Upload(context.Background(), MainScope, "/", "refused.bin", []byte("x"),
				UploadOptions{Accounts: tc.ids, Scheme: tc.scheme})
			if err == nil {
				t.Fatalf("%s was accepted", tc.scheme)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s was refused with %q, want it to mention %q", tc.scheme, err, tc.want)
			}
		})
	}
}

func TestFilesOnTheSameCloudsCanBeCutDifferently(t *testing.T) {
	// The reason the choice is per file: a folder of holiday photos and a folder
	// of tax records do not have to answer "how many accounts must collude"
	// the same way, even when they live on the same five clouds.
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	durable := archive.Scheme{Data: 2, Total: 5}
	secret := archive.Scheme{Data: 4, Total: 5}

	photos, _, err := v.Upload(context.Background(), MainScope, "/", "photos.bin",
		schemeTestPayload(t, 20*1024), UploadOptions{Accounts: ids, Scheme: durable})
	if err != nil {
		t.Fatalf("Upload photos: %v", err)
	}
	taxes, _, err := v.Upload(context.Background(), MainScope, "/", "taxes.bin",
		schemeTestPayload(t, 20*1024), UploadOptions{Accounts: ids, Scheme: secret})
	if err != nil {
		t.Fatalf("Upload taxes: %v", err)
	}

	if got := photos.Scheme(); got != durable {
		t.Errorf("photos stored as %s, want %s", got, durable)
	}
	if got := taxes.Scheme(); got != secret {
		t.Errorf("taxes stored as %s, want %s", got, secret)
	}
	if photos.Scheme().Tolerance() <= taxes.Scheme().Tolerance() {
		t.Error("the durable file should survive more losses than the secret one")
	}
	if photos.Scheme().Data >= taxes.Scheme().Data {
		t.Error("the secret file should need more accounts to collude than the durable one")
	}
}

// The vault-wide default: a code chosen once in settings rather than per file.
// It is a preference and not a rule, which is the whole of its behaviour — it
// applies where it fits, an upload overrides it, and anything already stored
// keeps the code it was cut with.

func TestTheVaultsDefaultSchemeCutsUploadsThatChooseNothing(t *testing.T) {
	scheme := archive.Scheme{Data: 3, Total: 5}
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	if err := v.SetDefaults(ids, scheme); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if got := v.DefaultScheme(); got != scheme {
		t.Fatalf("default reads back as %s, want %s", got, scheme)
	}

	// Nothing named for this upload at all: no accounts, no code.
	entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "settled.bin",
		schemeTestPayload(t, 24*1024), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if got := entry.Scheme(); got != scheme {
		t.Fatalf("cut %s, want the vault's default %s", got, scheme)
	}
	if got := len(entry.Shards); got != scheme.Total {
		t.Fatalf("stored %d shards, want %d", got, scheme.Total)
	}
}

func TestAnUploadOverridesTheVaultsDefaultScheme(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)
	if err := v.SetDefaults(ids, archive.Scheme{Data: 3, Total: 5}); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	want := archive.Scheme{Data: 2, Total: 5}
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "louder.bin",
		schemeTestPayload(t, 16*1024), UploadOptions{Accounts: ids, Scheme: want})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := entry.Scheme(); got != want {
		t.Fatalf("cut %s, want the upload's own %s", got, want)
	}
}

func TestTheVaultsDefaultSchemeOnlyAppliesWhereItFits(t *testing.T) {
	// A default of 3-of-5 is a statement about five accounts. A file
	// deliberately sent to three is 2-of-3, exactly as it would be with no
	// default set — the alternative is an upload failing because settings named
	// a width it does not have.
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	if err := v.SetDefaults(ids[:5], archive.Scheme{Data: 3, Total: 5}); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	for _, tc := range []struct {
		name     string
		accounts []string
		want     archive.Scheme
	}{
		{"three accounts", ids[:3], archive.SchemeDefault},
		{"six accounts", ids, archive.SchemeWide},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, _, err := v.Upload(context.Background(), MainScope, "/", tc.name+".bin",
				schemeTestPayload(t, 8*1024), UploadOptions{Accounts: tc.accounts})
			if err != nil {
				t.Fatalf("Upload onto %d accounts: %v", len(tc.accounts), err)
			}
			if got := entry.Scheme(); got != tc.want {
				t.Fatalf("cut %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTheDefaultSchemeAndItsAccountsAreSetTogether(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	for _, tc := range []struct {
		name     string
		accounts []string
		scheme   archive.Scheme
		want     string
	}{
		{"a code wider than the accounts under it", ids[:3], archive.Scheme{Data: 3, Total: 5}, "pick 5"},
		{"a code narrower than them", ids, archive.Scheme{Data: 3, Total: 5}, "pick 5"},
		{"a code with no accounts at all", nil, archive.Scheme{Data: 3, Total: 5}, "names 5 accounts"},
		{"a code that is not one", ids[:3], archive.Scheme{Data: 1, Total: 3}, "at least 2 shards"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.SetDefaults(tc.accounts, tc.scheme)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refused with %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestADefaultSchemeIsOnlyStoredWhenItSaysSomethingNew(t *testing.T) {
	// 4-of-6 against six accounts is what six accounts already mean. Storing it
	// would freeze a default that is not a choice, and would then have to be
	// cleared by hand after every change to the list.
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	if err := v.SetDefaults(ids, archive.SchemeWide); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if got := v.DefaultScheme(); got != (archive.Scheme{}) {
		t.Fatalf("stored %s as a default, want the count to keep naming it", got)
	}
	stats, err := v.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.DefaultScheme != "" {
		t.Fatalf("status reports %q, want it empty", stats.DefaultScheme)
	}
}

func TestDisconnectingAnAccountDoesNotLeaveAStrandedDefaultScheme(t *testing.T) {
	// 3-of-5 has nothing to say about the four accounts left, and a default that
	// names a width the vault does not have would fail every upload after it.
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)
	if err := v.SetDefaults(ids, archive.Scheme{Data: 3, Total: 5}); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if err := v.RemoveProvider(ids[0], true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if got := v.DefaultScheme(); got != (archive.Scheme{}) {
		t.Fatalf("default scheme survived as %s, want it cleared", got)
	}
	// And what is left has to still name a code by itself, which four does not.
	if got := len(v.DefaultAccounts()); got != 3 {
		t.Fatalf("default trimmed to %d accounts, want 3", got)
	}

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "after.bin",
		schemeTestPayload(t, 8*1024), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload after disconnecting: %v", err)
	}
	if got := entry.Scheme(); got != archive.SchemeDefault {
		t.Fatalf("cut %s, want %s", got, archive.SchemeDefault)
	}
}

func TestChangingTheDefaultSchemeLeavesStoredFilesAlone(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	before, _, err := v.Upload(context.Background(), MainScope, "/", "earlier.bin",
		schemeTestPayload(t, 12*1024), UploadOptions{Accounts: ids[:3]})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if err := v.SetDefaults(ids, archive.Scheme{Data: 2, Total: 5}); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	after, err := v.Entry(before.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if got := after.Scheme(); got != archive.SchemeDefault {
		t.Fatalf("a stored file became %s, want it left as %s", got, archive.SchemeDefault)
	}
}
