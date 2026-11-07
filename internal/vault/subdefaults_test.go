package vault

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// uploadLandsOn uploads a small file into a scope with no accounts of its own
// and returns the accounts its parts landed on.
func uploadLandsOn(t *testing.T, v *Vault, scope Scope, name string) map[string]bool {
	t.Helper()
	entry, _, err := v.Upload(t.Context(), scope, "/", name, []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload %s: %v", name, err)
	}
	return holders(entry)
}

func mustScheme(t *testing.T, s string) archive.Scheme {
	t.Helper()
	scheme, err := archive.ParseScheme(s)
	if err != nil {
		t.Fatalf("ParseScheme(%q): %v", s, err)
	}
	return scheme
}

func sameSet(got map[string]bool, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, id := range want {
		if !got[id] {
			return false
		}
	}
	return true
}

func TestSubVaultFollowsTheVaultDefaultUntilItHasItsOwn(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	vaultWide := []string{ids[0], ids[1], ids[2]}
	if err := v.SetDefaultAccounts(vaultWide); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))

	d, err := v.DefaultsIn(sub)
	if err != nil {
		t.Fatalf("DefaultsIn: %v", err)
	}
	if !d.Inherited || !reflect.DeepEqual(d.Accounts, vaultWide) {
		t.Fatalf("DefaultsIn = %+v, want the vault's default, inherited", d)
	}
	if got := uploadLandsOn(t, v, sub, "inherited.txt"); !sameSet(got, vaultWide) {
		t.Fatalf("parts landed on %v, want the vault's default %v", got, vaultWide)
	}
}

func TestSubVaultDefaultOverridesTheVaultsForItsOwnUploadsOnly(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	vaultWide := []string{ids[0], ids[1], ids[2]}
	own := []string{ids[3], ids[4], ids[5]}
	if err := v.SetDefaultAccounts(vaultWide); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))

	if err := v.SetDefaultsIn(sub, own, archive.Scheme{}); err != nil {
		t.Fatalf("SetDefaultsIn: %v", err)
	}

	d, err := v.DefaultsIn(sub)
	if err != nil {
		t.Fatalf("DefaultsIn: %v", err)
	}
	if d.Inherited || !reflect.DeepEqual(d.Accounts, own) {
		t.Fatalf("DefaultsIn = %+v, want the sub vault's own", d)
	}
	if got := uploadLandsOn(t, v, sub, "own.txt"); !sameSet(got, own) {
		t.Errorf("sub vault upload landed on %v, want its own default %v", got, own)
	}
	if got := uploadLandsOn(t, v, MainScope, "main.txt"); !sameSet(got, vaultWide) {
		t.Errorf("main vault upload landed on %v, want the vault's default %v", got, vaultWide)
	}
	if got := v.DefaultAccounts(); !reflect.DeepEqual(got, vaultWide) {
		t.Errorf("setting a sub vault's default changed the vault's to %v", got)
	}

	// A choice made for one upload still beats the sub vault's standing one.
	entry, _, err := v.Upload(t.Context(), sub, "/", "chosen.txt", []byte("x"),
		UploadOptions{Accounts: vaultWide})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := holders(entry); !sameSet(got, vaultWide) {
		t.Errorf("chosen upload landed on %v, want %v", got, vaultWide)
	}
}

func TestSubVaultDefaultIsSealedWithTheSubVault(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	own := []string{ids[3], ids[4], ids[5]}
	id := createSubVault(t, v, "Taxes", subPassword)
	if err := v.SetDefaultsIn(Scope(id), own, mustScheme(t, "2-of-3")); err != nil {
		t.Fatalf("SetDefaultsIn: %v", err)
	}

	fresh := reopen(t, v)

	// Shut, it cannot be read or changed.
	if _, err := fresh.DefaultsIn(Scope(id)); !errors.Is(err, ErrSubVaultLocked) {
		t.Fatalf("DefaultsIn on a shut sub vault = %v, want ErrSubVaultLocked", err)
	}
	if err := fresh.SetDefaultsIn(Scope(id), own, archive.Scheme{}); !errors.Is(err, ErrSubVaultLocked) {
		t.Fatalf("SetDefaultsIn on a shut sub vault = %v, want ErrSubVaultLocked", err)
	}
	// And nothing in the clear names the accounts it chose.
	raw, err := os.ReadFile(fresh.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range own {
		if bytes.Contains(raw, []byte(acct)) {
			t.Errorf("the vault file names %s in the clear", acct)
		}
	}
	if sf := readStoreFile(t, fresh.Path()); len(sf.DefaultAccounts) != 0 {
		t.Errorf("the vault-wide default became %v", sf.DefaultAccounts)
	}

	// Open, it is exactly what was set.
	if err := fresh.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	d, err := fresh.DefaultsIn(Scope(id))
	if err != nil {
		t.Fatalf("DefaultsIn: %v", err)
	}
	if d.Inherited || !reflect.DeepEqual(d.Accounts, own) {
		t.Fatalf("after a reopen DefaultsIn = %+v, want %v", d, own)
	}
}

func TestClearingASubVaultDefaultGoesBackToTheVaults(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	vaultWide := []string{ids[0], ids[1], ids[2]}
	if err := v.SetDefaultAccounts(vaultWide); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))
	if err := v.SetDefaultsIn(sub, []string{ids[3], ids[4], ids[5]}, archive.Scheme{}); err != nil {
		t.Fatalf("SetDefaultsIn: %v", err)
	}

	if err := v.SetDefaultsIn(sub, nil, archive.Scheme{}); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	d, err := v.DefaultsIn(sub)
	if err != nil {
		t.Fatalf("DefaultsIn: %v", err)
	}
	if !d.Inherited || !reflect.DeepEqual(d.Accounts, vaultWide) {
		t.Fatalf("after clearing DefaultsIn = %+v, want the vault's, inherited", d)
	}
}

func TestSubVaultDefaultIsJudgedLikeTheVaults(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))

	cases := []struct {
		name   string
		ids    []string
		scheme string
	}{
		{"unknown account", []string{ids[0], ids[1], "not-an-account"}, ""},
		{"the same account twice", []string{ids[0], ids[0], ids[1]}, ""},
		{"a single account", []string{ids[0]}, ""},
		{"a scheme wider than the accounts", []string{ids[0], ids[1], ids[2]}, "3-of-5"},
		{"a scheme with no accounts", nil, "3-of-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var scheme archive.Scheme
			if tc.scheme != "" {
				scheme = mustScheme(t, tc.scheme)
			}
			if err := v.SetDefaultsIn(sub, tc.ids, scheme); err == nil {
				t.Fatalf("expected %s to be refused", tc.name)
			}
			if d, _ := v.DefaultsIn(sub); !d.Inherited {
				t.Errorf("a refused selection was stored anyway: %+v", d)
			}
		})
	}
}

func TestSubVaultSchemeCutsItsUploads(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	sub := Scope(createSubVault(t, v, "Taxes", subPassword))
	five := ids[:5]
	if err := v.SetDefaultsIn(sub, five, mustScheme(t, "3-of-5")); err != nil {
		t.Fatalf("SetDefaultsIn: %v", err)
	}
	d, err := v.DefaultsIn(sub)
	if err != nil {
		t.Fatalf("DefaultsIn: %v", err)
	}
	if d.Scheme != "3-of-5" {
		t.Fatalf("DefaultsIn scheme = %q, want 3-of-5", d.Scheme)
	}
	if got := uploadLandsOn(t, v, sub, "wide.txt"); !sameSet(got, five) {
		t.Fatalf("parts landed on %v, want all five defaults", got)
	}
	// The vault itself still has no default, and so no scheme either.
	if got := v.DefaultScheme(); got != (archive.Scheme{}) {
		t.Errorf("the vault's default scheme became %s", got)
	}
}

func TestShutSubVaultDefaultIsPrunedWhenAnAccountGoes(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	vaultWide := []string{ids[0], ids[1], ids[2]}
	if err := v.SetDefaultAccounts(vaultWide); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	id := createSubVault(t, v, "Taxes", subPassword)
	if err := v.SetDefaultsIn(Scope(id), []string{ids[3], ids[4]}, archive.Scheme{}); err != nil {
		t.Fatalf("SetDefaultsIn: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	// Disconnected while the sub vault is shut, so its index cannot be told.
	if err := v.RemoveProvider(ids[4], false); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if err := v.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}

	// One account is too few to be a default, so the vault's stands in.
	d, err := v.DefaultsIn(Scope(id))
	if err != nil {
		t.Fatalf("DefaultsIn: %v", err)
	}
	if !d.Inherited || !reflect.DeepEqual(d.Accounts, vaultWide) {
		t.Fatalf("DefaultsIn = %+v, want the vault's default to stand in", d)
	}
	if got := uploadLandsOn(t, v, Scope(id), "after.txt"); !sameSet(got, vaultWide) {
		t.Fatalf("parts landed on %v, want the vault's default %v", got, vaultWide)
	}
}

func TestSubVaultListingCarriesItsDefaultsOnlyWhileOpen(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ids := accountIDs(t, v)
	id := createSubVault(t, v, "Taxes", subPassword)
	own := []string{ids[1], ids[2], ids[3]}
	if err := v.SetDefaultsIn(Scope(id), own, archive.Scheme{}); err != nil {
		t.Fatalf("SetDefaultsIn: %v", err)
	}

	subs, err := v.SubVaults()
	if err != nil || len(subs) != 1 {
		t.Fatalf("SubVaults = %v, %v", subs, err)
	}
	if subs[0].Defaults == nil || !reflect.DeepEqual(subs[0].Defaults.Accounts, own) {
		t.Fatalf("open sub vault listed defaults %+v, want %v", subs[0].Defaults, own)
	}

	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}
	subs, _ = v.SubVaults()
	if subs[0].Defaults != nil {
		t.Errorf("a shut sub vault listed its defaults: %+v", subs[0].Defaults)
	}
}

func TestPruneDefaults(t *testing.T) {
	all := func(string) bool { return true }
	without := func(gone ...string) func(string) bool {
		return func(id string) bool {
			for _, g := range gone {
				if g == id {
					return false
				}
			}
			return true
		}
	}
	six := []string{"a", "b", "c", "d", "e", "f"}

	cases := []struct {
		name        string
		accounts    []string
		scheme      string
		connected   func(string) bool
		wantAccts   []string
		wantSchemes string
	}{
		{"nothing gone", []string{"a", "b", "c"}, "", all, []string{"a", "b", "c"}, ""},
		{"a scheme loses its width", []string{"a", "b", "c", "d", "e"}, "3-of-5", without("e"),
			[]string{"a", "b", "c"}, ""},
		{"six losing one falls to three", six, "", without("f"), []string{"a", "b", "c"}, ""},
		{"too few left is none", []string{"a", "b"}, "", without("b"), nil, ""},
		{"an unparseable scheme is no scheme", []string{"a", "b", "c"}, "nonsense", all,
			[]string{"a", "b", "c"}, ""},
		{"a scheme that still fits stays", []string{"a", "b", "c", "d", "e"}, "3-of-5", all,
			[]string{"a", "b", "c", "d", "e"}, "3-of-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAccts, gotScheme := pruneDefaults(tc.accounts, tc.scheme, tc.connected)
			if !reflect.DeepEqual(gotAccts, tc.wantAccts) || gotScheme != tc.wantSchemes {
				t.Fatalf("pruneDefaults = %v, %q; want %v, %q", gotAccts, gotScheme, tc.wantAccts, tc.wantSchemes)
			}
		})
	}
}
