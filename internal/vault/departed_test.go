package vault

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// What a disconnect remembers, and how a returning account is recognised by it.

func TestDepartedFromSamplesOnlyThatAccountsParts(t *testing.T) {
	var entries []*Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, &Entry{
			ArchiveID: fmt.Sprintf("archive-%02d", i),
			Shards: []Shard{
				{Part: 1, ProviderID: "gone"},
				{Part: 2, ProviderID: "stays"},
			},
		})
	}
	d := departedFrom(provider.Config{ID: "gone", Name: "Elements", Kind: provider.KindLocal}, entries, time.Now())
	if d.ID != "gone" || d.Name != "Elements" || d.Kind != string(provider.KindLocal) {
		t.Fatalf("record = %+v", d)
	}
	if len(d.Parts) != departedSample {
		t.Fatalf("sampled %d parts, want %d", len(d.Parts), departedSample)
	}
	for _, p := range d.Parts {
		if p.Part != 1 {
			t.Errorf("sampled part %d of %s, which another account holds", p.Part, p.ArchiveID)
		}
	}
}

func TestManifestKeepsTheNewestDepartedAndFindsThemByPart(t *testing.T) {
	m := &Manifest{}
	base := time.Now()
	for i := 0; i < departedKept+3; i++ {
		m.noteDeparted(DepartedAccount{
			ID:    fmt.Sprintf("acct-%02d", i),
			At:    base.Add(time.Duration(i) * time.Minute),
			Parts: []DepartedPart{{ArchiveID: fmt.Sprintf("arch-%02d", i), Part: 2}},
		})
	}
	if len(m.Departed) != departedKept {
		t.Fatalf("kept %d, want %d", len(m.Departed), departedKept)
	}
	if _, ok := m.departedHolding("arch-00", 2); ok {
		t.Error("the oldest record survived the cap")
	}
	d, ok := m.departedHolding(fmt.Sprintf("arch-%02d", departedKept+2), 2)
	if !ok || d.ID != fmt.Sprintf("acct-%02d", departedKept+2) {
		t.Errorf("the newest record was not found by its part: %+v, %v", d, ok)
	}
	if _, ok := m.departedHolding(fmt.Sprintf("arch-%02d", departedKept+2), 1); ok {
		t.Error("a different part number of the same archive matched")
	}

	// Noted again under the same ID replaces rather than duplicates, and
	// forgetting takes it out.
	m.noteDeparted(DepartedAccount{ID: "acct-10", At: base.Add(time.Hour)})
	count := 0
	for _, d := range m.Departed {
		if d.ID == "acct-10" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("acct-10 is on record %d times", count)
	}
	m.forgetDeparted("acct-10")
	for _, d := range m.Departed {
		if d.ID == "acct-10" {
			t.Fatal("forgetDeparted left the record")
		}
	}
}

func TestReadStatsMergeAccountFoldsEveryBucket(t *testing.T) {
	s := newReadStats()
	now := time.Now()
	for _, id := range []string{"old", "new", "other"} {
		s.record(shardFetch{shard: Shard{ProviderID: id, ProviderName: "n-" + id}, blob: make([]byte, 10), took: time.Second}, shardWon)
	}
	s.record(shardFetch{shard: Shard{ProviderID: "old"}, err: fmt.Errorf("gone")}, shardFailed)

	if s.mergeAccount("old", "old") || s.mergeAccount("", "new") || s.mergeAccount("absent", "new") {
		t.Fatal("a merge with nothing to move reported moving something")
	}
	if !s.mergeAccount("old", "new") {
		t.Fatal("the merge moved nothing")
	}

	report := s.report(WindowAll, nil, now)
	for _, a := range report.Accounts {
		if a.ProviderID == "old" {
			t.Fatalf("the merged-away account still has a row: %+v", a)
		}
	}
	got := map[string]ReadStat{}
	for _, a := range report.Accounts {
		got[a.ProviderID] = a
	}
	if n := got["new"]; n.Fetches != 3 || n.Wins != 2 || n.Failures != 1 {
		t.Errorf("new = %d fetches, %d wins, %d failures; want 3, 2, 1", n.Fetches, n.Wins, n.Failures)
	}
	if o := got["other"]; o.Fetches != 1 {
		t.Errorf("an account outside the merge changed: %+v", o)
	}
	today := s.report(WindowToday, nil, now)
	for _, a := range today.Accounts {
		if a.ProviderID == "old" {
			t.Fatal("the day bucket still counts the merged-away account")
		}
	}
}

// The scenario as it happens: a cloud is read from, disconnected, connected
// again as a new account, and its parts put back. The old account's read
// history becomes the new one's — proven by a part the disconnect sampled.
func TestReattachMovesTheDepartedAccountsReadsOntoItsReturn(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	entry, _, err := v.Upload(ctx, MainScope, "/", "read.txt", []byte("read before the cloud went"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	forgetReads(t, v)
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	waitForReads(v)

	victim, back := cycleAccount(t, v, "renamed-on-return")
	before := readsOf(v, victim.ID)
	if before.Fetches == 0 {
		t.Fatal("the departing account was never read from; nothing to carry over")
	}

	report, err := v.ReattachShards(ctx, ReattachOptions{})
	if err != nil {
		t.Fatalf("ReattachShards: %v", err)
	}
	if len(report.Merged) != 1 {
		t.Fatalf("merged = %+v, want the one departed account", report.Merged)
	}
	m := report.Merged[0]
	if m.From != victim.ID || m.Into != back || m.By != "parts" || m.FromName != victim.Name {
		t.Errorf("merge = %+v, want %s → %s by parts", m, victim.ID, back)
	}
	for _, a := range v.ReadStats(WindowAll).Accounts {
		if a.ProviderID == victim.ID {
			t.Errorf("the departed account still has a row of its own: %+v", a)
		}
	}
	if after := readsOf(v, back); after.Fetches != before.Fetches {
		t.Errorf("the returning account carries %d fetches, want the departed one's %d", after.Fetches, before.Fetches)
	}
	if len(v.manifest.Departed) != 0 {
		t.Errorf("the departed record outlived being recognised: %+v", v.manifest.Departed)
	}
}

// A cloud disconnected before samples were kept has nothing to be proven by,
// so its name is taken instead — but only when exactly one disconnected
// account answers to it.
func TestReattachFallsBackToTheOneDisconnectedAccountOfThatName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ghosts   int
		wantMove bool
	}{
		{"one by that name", 0, true},
		{"two by that name", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			v, _ := newTestVault(t, 3)
			entry, _, err := v.Upload(ctx, MainScope, "/", "read.txt", []byte("read before the cloud went"), UploadOptions{})
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			forgetReads(t, v)
			if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			waitForReads(v)

			accounts, _ := v.Providers()
			victim := accounts[0]
			// Another disconnected account by the same name and kind, as
			// far as the history knows.
			for i := 0; i < tc.ghosts; i++ {
				v.reads.record(shardFetch{shard: Shard{
					ProviderID: fmt.Sprintf("ghost-%d", i), ProviderName: victim.Name, ProviderKind: string(victim.Kind),
				}}, shardWon)
			}

			_, back := cycleAccount(t, v, victim.Name)
			// As a vault from before samples: nothing on record.
			v.mu.Lock()
			v.manifest.Departed = nil
			v.mu.Unlock()

			report, err := v.ReattachShards(ctx, ReattachOptions{})
			if err != nil {
				t.Fatalf("ReattachShards: %v", err)
			}
			if !tc.wantMove {
				if len(report.Merged) != 0 {
					t.Fatalf("guessed between two accounts: %+v", report.Merged)
				}
				return
			}
			if len(report.Merged) != 1 || report.Merged[0].From != victim.ID || report.Merged[0].By != "name" {
				t.Fatalf("merged = %+v, want %s by name", report.Merged, victim.ID)
			}
			if readsOf(v, back).Fetches == 0 {
				t.Error("the returning account carries nothing")
			}
		})
	}
}

// cycleAccount disconnects the first account and connects its storage again
// under name, the way somebody does it by hand. It answers the departed config
// and the new ID.
func cycleAccount(t *testing.T, v *Vault, name string) (provider.Config, string) {
	t.Helper()
	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	victim := accounts[0]
	if err := v.RemoveProvider(victim.ID, true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	added, err := v.AddProvider(context.Background(), provider.Config{
		Kind: victim.Kind, Name: name, Options: victim.Options,
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return victim, added.ID
}

func readsOf(v *Vault, id string) ReadStat {
	for _, a := range v.ReadStats(WindowAll).Accounts {
		if a.ProviderID == id {
			return a
		}
	}
	return ReadStat{}
}

// An account recognised on its return but never read from has no history to
// carry: nothing is reported merged, and its record is still forgotten.
func TestReattachRecognisesAnUnreadAccountQuietly(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "unread.txt", []byte("never read"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	forgetReads(t, v)
	cycleAccount(t, v, "back")
	if len(v.manifest.Departed) != 1 {
		t.Fatalf("the disconnect left %d departed record(s), want 1", len(v.manifest.Departed))
	}

	report, err := v.ReattachShards(ctx, ReattachOptions{})
	if err != nil {
		t.Fatalf("ReattachShards: %v", err)
	}
	if report.Shards == 0 {
		t.Fatal("nothing was put back")
	}
	if len(report.Merged) != 0 {
		t.Errorf("reported carrying over history that did not exist: %+v", report.Merged)
	}
	if len(v.manifest.Departed) != 0 {
		t.Errorf("the departed record outlived being recognised: %+v", v.manifest.Departed)
	}
}
