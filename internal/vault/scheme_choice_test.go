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
