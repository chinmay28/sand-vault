package vault

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// accountIDs returns the connected accounts' IDs in the order they were added.
func accountIDs(t *testing.T, v *Vault) []string {
	t.Helper()

	configs, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	ids := make([]string, 0, len(configs))
	for _, cfg := range configs {
		ids = append(ids, cfg.ID)
	}
	return ids
}

// holders is the set of accounts an entry's parts landed on.
func holders(e *Entry) map[string]bool {
	out := map[string]bool{}
	for _, s := range e.Shards {
		out[s.ProviderID] = true
	}
	return out
}

func TestUploadUsesThreeOfManyAccounts(t *testing.T) {
	v, _ := newTestVault(t, 6)

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "spread.bin", []byte("payload"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := len(holders(entry)); got != AccountsPerFile {
		t.Errorf("parts spread over %d accounts, want %d", got, AccountsPerFile)
	}
}

// With no default set, uploads must not all pile onto whichever three accounts
// were connected first — otherwise connecting a fourth would do nothing.
func TestUploadsWithoutADefaultSpreadOverEveryAccount(t *testing.T) {
	v, _ := newTestVault(t, 6)

	used := map[string]bool{}
	for i := 0; i < 12; i++ {
		entry, _, err := v.Upload(context.Background(), MainScope, "/", fmt.Sprintf("f%d.bin", i), []byte("x"), UploadOptions{})
		if err != nil {
			t.Fatalf("Upload %d: %v", i, err)
		}
		for id := range holders(entry) {
			used[id] = true
		}
	}
	if len(used) != 6 {
		t.Errorf("12 uploads touched %d of 6 accounts — the pick is not spreading", len(used))
	}
}

func TestUploadHonoursChosenAccounts(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)
	chosen := []string{ids[4], ids[1], ids[3]}

	entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "picked.txt", []byte("payload"),
		UploadOptions{Accounts: chosen})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	got := holders(entry)
	if len(got) != len(chosen) {
		t.Fatalf("parts landed on %d accounts, want %d", len(got), len(chosen))
	}
	for _, id := range chosen {
		if !got[id] {
			t.Errorf("chosen account %s holds no part", id)
		}
	}
}

// A choice of two accounts stores two parts. Topping it up to three would put
// the file somewhere its owner deliberately did not choose.
func TestUploadHonoursTwoChosenAccountsExactly(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)
	chosen := []string{ids[0], ids[2]}

	entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "pair.txt", []byte("payload"),
		UploadOptions{Accounts: chosen})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(entry.Shards) != 2 {
		t.Fatalf("stored %d parts over two accounts, want 2", len(entry.Shards))
	}
	for _, s := range entry.Shards {
		if s.ProviderID != chosen[0] && s.ProviderID != chosen[1] {
			t.Errorf("part %d landed on %s, which was not chosen", s.Part, s.ProviderName)
		}
	}
	// The file is recoverable but has no spare part, and says so.
	if len(warnings) == 0 {
		t.Error("storing two of three parts should warn about the missing spare")
	}

	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("round-trip mismatch: %q", data)
	}
}

func TestUploadRejectsAnAccountThatIsNotConnected(t *testing.T) {
	v, _ := newTestVault(t, 3)

	_, _, err := v.Upload(context.Background(), MainScope, "/", "nowhere.txt", []byte("x"),
		UploadOptions{Accounts: []string{"00000000-0000-0000-0000-000000000000"}})
	if err == nil {
		t.Fatal("expected an upload naming an unknown account to be refused")
	}
	if !strings.Contains(err.Error(), "no connected account") {
		t.Errorf("error should name the problem, got: %v", err)
	}
}

func TestUploadRejectsMoreAccountsThanParts(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	_, _, err := v.Upload(context.Background(), MainScope, "/", "greedy.txt", []byte("x"),
		UploadOptions{Accounts: ids[:4]})
	if err == nil {
		t.Fatal("expected choosing four accounts for a three-part file to be refused")
	}
}

func TestDefaultAccountsApplyToEveryUpload(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)
	defaults := []string{ids[1], ids[3], ids[5]}

	if err := v.SetDefaultAccounts(defaults); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	if got := v.DefaultAccounts(); len(got) != 3 {
		t.Fatalf("DefaultAccounts = %v, want the three that were set", got)
	}

	for i := 0; i < 4; i++ {
		entry, _, err := v.Upload(context.Background(), MainScope, "/", fmt.Sprintf("d%d.txt", i), []byte("x"), UploadOptions{})
		if err != nil {
			t.Fatalf("Upload %d: %v", i, err)
		}
		for _, s := range entry.Shards {
			if s.ProviderID != defaults[0] && s.ProviderID != defaults[1] && s.ProviderID != defaults[2] {
				t.Fatalf("part %d landed on %s, which is not one of the default accounts", s.Part, s.ProviderName)
			}
		}
	}
}

// A per-upload choice is for that upload alone.
func TestChosenAccountsOverrideTheDefault(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts([]string{ids[0], ids[1], ids[2]}); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}

	entry, _, err := v.Upload(context.Background(), MainScope, "/", "elsewhere.txt", []byte("x"),
		UploadOptions{Accounts: []string{ids[3], ids[4], ids[5]}})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, s := range entry.Shards {
		if s.ProviderID == ids[0] || s.ProviderID == ids[1] || s.ProviderID == ids[2] {
			t.Errorf("part %d landed on a default account the upload had overridden", s.Part)
		}
	}

	// The default is untouched, so the next upload goes back to it.
	next, _, err := v.Upload(context.Background(), MainScope, "/", "back.txt", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, s := range next.Shards {
		if s.ProviderID != ids[0] && s.ProviderID != ids[1] && s.ProviderID != ids[2] {
			t.Errorf("part %d ignored the default account list", s.Part)
		}
	}
}

// A default naming two accounts means two accounts, even with five connected.
// Quietly making it up to three would put files on clouds the person who set
// it deliberately left out.
func TestATwoAccountDefaultIsNotWidened(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts([]string{ids[0], ids[1]}); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}

	entry, warnings, err := v.Upload(context.Background(), MainScope, "/", "narrow.txt", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got := holders(entry)
	if len(got) != 2 || !got[ids[0]] || !got[ids[1]] {
		t.Fatalf("parts landed on %v, want only the two default accounts", got)
	}
	// The cost of the narrower default is said out loud rather than assumed.
	if len(warnings) == 0 {
		t.Error("storing two of three parts should warn about the missing spare")
	}
}

func TestSetDefaultAccountsRejectsBadSelections(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ids := accountIDs(t, v)

	cases := []struct {
		name string
		ids  []string
	}{
		{"unknown account", []string{ids[0], ids[1], "not-an-account"}},
		{"the same account twice", []string{ids[0], ids[0], ids[1]}},
		{"more accounts than parts", ids},
		{"a single account", []string{ids[0]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := v.SetDefaultAccounts(tc.ids); err == nil {
				t.Fatalf("expected %s to be refused", tc.name)
			}
			if got := v.DefaultAccounts(); len(got) != 0 {
				t.Errorf("a refused selection was stored anyway: %v", got)
			}
		})
	}
}

func TestSetDefaultAccountsSurvivesALockAndUnlock(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ids := accountIDs(t, v)
	defaults := []string{ids[0], ids[2], ids[3]}

	if err := v.SetDefaultAccounts(defaults); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	got := v.DefaultAccounts()
	if len(got) != len(defaults) {
		t.Fatalf("DefaultAccounts = %v, want %v", got, defaults)
	}
	for i, id := range defaults {
		if got[i] != id {
			t.Errorf("default %d = %s, want %s", i, got[i], id)
		}
	}
}

func TestDisconnectingAnAccountDropsItFromTheDefault(t *testing.T) {
	v, _ := newTestVault(t, 5)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts([]string{ids[0], ids[1], ids[2]}); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	if err := v.RemoveProvider(ids[1], false); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}

	got := v.DefaultAccounts()
	if len(got) != 2 || got[0] != ids[0] || got[1] != ids[2] {
		t.Fatalf("DefaultAccounts = %v, want the two accounts still connected", got)
	}

	// And what is left is still a usable default rather than a broken one.
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "after.txt", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	landed := holders(entry)
	if len(landed) != 2 || !landed[ids[0]] || !landed[ids[2]] {
		t.Errorf("parts landed on %v, want the two accounts left in the default", landed)
	}
}

// Below two surviving accounts a default is no longer a spread, so it is
// dropped in favour of the per-file pick.
func TestADefaultTooSmallToUseIsCleared(t *testing.T) {
	v, _ := newTestVault(t, 4)
	ids := accountIDs(t, v)

	if err := v.SetDefaultAccounts([]string{ids[0], ids[1]}); err != nil {
		t.Fatalf("SetDefaultAccounts: %v", err)
	}
	if err := v.RemoveProvider(ids[1], true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if got := v.DefaultAccounts(); len(got) != 0 {
		t.Errorf("DefaultAccounts = %v, want it cleared", got)
	}
}

// Re-encrypting after a password change is not a move: a file goes back to the
// accounts it was already on.
func TestMigrationKeepsAFileOnItsOwnAccounts(t *testing.T) {
	v, _ := newTestVault(t, 6)
	ctx := context.Background()
	ids := accountIDs(t, v)

	entry, _, err := v.Upload(ctx, MainScope, "/", "pinned.txt", []byte("stays put"),
		UploadOptions{Accounts: []string{ids[1], ids[2], ids[4]}})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := holders(entry)

	if _, err := v.ChangePassword(ctx, testPassword, "a different password entirely", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	moved, err := v.Entry(entry.ID)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	after := holders(moved)
	if len(after) != len(before) {
		t.Fatalf("file now spans %d accounts, was %d", len(after), len(before))
	}
	for id := range before {
		if !after[id] {
			t.Errorf("re-encrypting moved the file off account %s", id)
		}
	}
}

func TestSelectAccountsFillsInFromWhatIsConnected(t *testing.T) {
	available := []string{"a", "b", "c", "d", "e"}

	// A stale preference is dropped and made up from the rest.
	chosen := SelectAccounts(available, []string{"gone", "b"}, 0, 7)
	if len(chosen) != AccountsPerFile {
		t.Fatalf("chose %v, want %d accounts", chosen, AccountsPerFile)
	}
	if chosen[0] != "b" {
		t.Errorf("chose %v, want the connected preference first", chosen)
	}
	seen := map[string]bool{}
	for _, id := range chosen {
		if seen[id] {
			t.Fatalf("chose %v, which repeats an account", chosen)
		}
		seen[id] = true
	}

	// Fewer accounts than a file has parts is not an error here: strict
	// placement is what refuses it, with an explanation.
	if got := SelectAccounts([]string{"a"}, nil, 0, 1); len(got) != 1 {
		t.Errorf("chose %v from one connected account, want just it", got)
	}

	// The same file always chooses the same way, whatever else has run.
	first := SelectAccounts(available, nil, 0, 42)
	second := SelectAccounts(available, nil, 0, 42)
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("same seed chose %v then %v", first, second)
	}
}
