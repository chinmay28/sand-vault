package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// dailyAt is the policy most of these tests are about: check this folder every
// morning, and put back whatever is missing.
func dailyAt(at string, action AutomationAction) Automation {
	return Automation{Enabled: true, Cadence: CadenceDaily, At: at, Action: action}
}

// darken makes a local-folder account stop answering, the way a cloud whose
// token expired does: the account is still connected and the index still points
// at it, and nothing there can be read or written. A plain file where the
// folder was is enough — the backend's own Ping creates its directory, and
// cannot create one over a file.
func darken(t *testing.T, root string) {
	t.Helper()

	// Retried, because this races with the manifest backup: that runs on a
	// goroutine of its own and recreates an account's directory every time it
	// pushes, so the window between removing the directory and putting a file
	// where it was is a window it can win. Once the file is there it cannot,
	// and the account is dark for good.
	deadline := time.Now().Add(10 * time.Second)
	for {
		os.RemoveAll(root)
		if err := os.WriteFile(root, []byte("not a folder"), 0600); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("blocking %s: %v", root, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// emptyCloud leaves an account answering but holding nothing, which is what an
// upload that happened while it was refusing leaves behind: the index records a
// part that is not there.
func emptyCloud(t *testing.T, root string) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatalf("listing %s: %v", root, err)
	}
	for _, name := range names {
		if err := os.RemoveAll(name); err != nil {
			t.Fatalf("removing %s: %v", name, err)
		}
	}
}

func TestScheduleDailyLandsOnTheNextMorning(t *testing.T) {
	loc := time.FixedZone("test", 0)
	auto := dailyAt("10:00", ActionCheck)

	from := time.Date(2026, 8, 20, 9, 0, 0, 0, loc)
	if got := auto.nextAfter(from, loc); !got.Equal(time.Date(2026, 8, 20, 10, 0, 0, 0, loc)) {
		t.Errorf("next after 09:00 = %s, want the same morning at 10:00", got)
	}

	// Exactly on the hour is not "after" it, so a run that finished at ten does
	// not immediately become due again.
	from = time.Date(2026, 8, 20, 10, 0, 0, 0, loc)
	if got := auto.nextAfter(from, loc); !got.Equal(time.Date(2026, 8, 21, 10, 0, 0, 0, loc)) {
		t.Errorf("next after 10:00 = %s, want tomorrow", got)
	}
}

func TestScheduleWeeklyPicksTheDay(t *testing.T) {
	loc := time.FixedZone("test", 0)
	auto := Automation{Cadence: CadenceWeekly, At: "03:30", Weekday: time.Sunday, Action: ActionCheck}

	// Thursday.
	from := time.Date(2026, 8, 20, 12, 0, 0, 0, loc)
	next := auto.nextAfter(from, loc)
	if next.Weekday() != time.Sunday {
		t.Errorf("next = %s, a %s, want a Sunday", next, next.Weekday())
	}
	if next.Hour() != 3 || next.Minute() != 30 {
		t.Errorf("next = %s, want 03:30", next)
	}
}

func TestScheduleHourlyCountsFromTheLastRun(t *testing.T) {
	auto := Automation{Enabled: true, Cadence: CadenceHourly, Action: ActionCheck}
	auto.LastRunAt = time.Date(2026, 8, 20, 9, 15, 0, 0, time.UTC)

	if got := auto.DueAt(); !got.Equal(auto.LastRunAt.Add(time.Hour)) {
		t.Errorf("DueAt = %s, want an hour after the last run", got)
	}
	if auto.Due(auto.LastRunAt.Add(59 * time.Minute)) {
		t.Error("due 59 minutes after the last run")
	}
	if !auto.Due(auto.LastRunAt.Add(61 * time.Minute)) {
		t.Error("not due an hour and a minute after the last run")
	}
}

func TestANewPolicyWaitsForItsFirstSlot(t *testing.T) {
	auto := dailyAt("10:00", ActionCheck)
	auto.CreatedAt = time.Now().UTC()

	if auto.Due(auto.CreatedAt.Add(time.Minute)) {
		t.Error("a policy made a minute ago is already due — making one should not start a sweep")
	}
	if !auto.Due(auto.CreatedAt.Add(48 * time.Hour)) {
		t.Error("still not due two days later")
	}
}

func TestADisabledPolicyIsNeverDue(t *testing.T) {
	auto := dailyAt("10:00", ActionCheck)
	auto.Enabled = false
	auto.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	if auto.Due(time.Now()) {
		t.Error("a policy that is switched off came up due")
	}
}

func TestSetAutomationRefusesWhatItCannotRun(t *testing.T) {
	v, _ := newTestVault(t, 3)

	for _, tc := range []struct {
		name string
		want Automation
	}{
		{"no cadence", Automation{Action: ActionCheck}},
		{"unknown cadence", Automation{Cadence: "fortnightly", At: "10:00", Action: ActionCheck}},
		{"no action", Automation{Cadence: CadenceDaily, At: "10:00"}},
		{"time with no colon", Automation{Cadence: CadenceDaily, At: "1000", Action: ActionCheck}},
		{"hour past midnight", Automation{Cadence: CadenceDaily, At: "25:00", Action: ActionCheck}},
		{"minute past sixty", Automation{Cadence: CadenceDaily, At: "10:75", Action: ActionCheck}},
		{"negative bound", Automation{Cadence: CadenceHourly, Action: ActionCheck,
			Shards: &ShardPolicy{MaxRepairs: -1}}},
		{"unknown task", Automation{Cadence: CadenceHourly, Action: ActionCheck, Task: "tidying"}},
		{"an action the task cannot do", Automation{Cadence: CadenceHourly, Action: ActionPull}},
		{"rebalancing repositories", Automation{Cadence: CadenceHourly, Task: TaskGit,
			Action: ActionRebalance}},
	} {
		if _, err := v.SetAutomation(MainScope, "/", tc.want); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestSetAutomationRefusesAFolderThatIsNotThere(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if _, err := v.SetAutomation(MainScope, "/nowhere", dailyAt("10:00", ActionCheck)); err == nil {
		t.Fatal("accepted a policy on a folder that does not exist")
	}
}

func TestEditingAPolicyKeepsWhatItHasBeenThrough(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionCheck)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	// Stand in for a run having happened.
	ran := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	v.mu.Lock()
	auto := v.manifest.Automations["/"]
	auto.LastRunAt = ran
	auto.History = []AutomationRun{{Folder: "/", Shards: &ShardResult{Whole: 7}}}
	created := auto.CreatedAt
	v.mu.Unlock()

	edited, err := v.SetAutomation(MainScope, "/", dailyAt("23:00", ActionRebalance))
	if err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	if !edited.LastRunAt.Equal(ran) {
		t.Errorf("LastRunAt = %s, want the run that already happened at %s", edited.LastRunAt, ran)
	}
	if len(edited.History) != 1 || edited.History[0].Shards.Whole != 7 {
		t.Errorf("history = %+v, want the one run carried across", edited.History)
	}
	if !edited.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %s, want the original %s", edited.CreatedAt, created)
	}
	if edited.At != "23:00" || edited.Action != ActionRebalance {
		t.Errorf("edit did not take: %+v", edited.Automation)
	}
}

func TestAutomationFollowsAFolderThatIsRenamed(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/archive"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := v.SetAutomation(MainScope, "/archive", dailyAt("10:00", ActionCheck)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	if err := v.MoveFolder(context.Background(), MainScope, "/archive", "/vault"); err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}

	moved, err := v.AutomationFor(MainScope, "/vault")
	if err != nil {
		t.Fatalf("AutomationFor: %v", err)
	}
	if moved == nil {
		t.Fatal("the renamed folder stopped being looked after")
	}
	left, err := v.AutomationFor(MainScope, "/archive")
	if err != nil {
		t.Fatalf("AutomationFor: %v", err)
	}
	if left != nil {
		t.Error("a policy is still filed under the old name")
	}
}

func TestAutomationGoesWithTheFolderItLooksAfter(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/archive/2019"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, dir := range []string{"/archive", "/archive/2019"} {
		if _, err := v.SetAutomation(MainScope, dir, dailyAt("10:00", ActionCheck)); err != nil {
			t.Fatalf("SetAutomation %s: %v", dir, err)
		}
	}

	if _, err := v.Rmdir(context.Background(), MainScope, "/archive", true); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}

	all, err := v.Automations(MainScope)
	if err != nil {
		t.Fatalf("Automations: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("policies left behind by a deleted folder: %+v", all)
	}
}

func TestRemoveAutomationSaysWhenThereWasNone(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.RemoveAutomation(MainScope, "/"); !errors.Is(err, ErrNoAutomation) {
		t.Errorf("RemoveAutomation on a bare folder = %v, want ErrNoAutomation", err)
	}
}

func TestNarrowSchemeHoldsTheStorageRatio(t *testing.T) {
	for _, tc := range []struct {
		have  archive.Scheme
		width int
		want  archive.Scheme
	}{
		// 1.5× held as closely as the width allows.
		{archive.SchemeWide, 3, archive.Scheme{Data: 2, Total: 3}},
		{archive.SchemeWide, 4, archive.Scheme{Data: 3, Total: 4}},
		{archive.SchemeWide, 5, archive.Scheme{Data: 3, Total: 5}},
		{archive.SchemeWider, 6, archive.Scheme{Data: 4, Total: 6}},
		// A code that already needs most of its shards is not allowed to reach
		// k = n, which would leave the file with no spare at all.
		{archive.Scheme{Data: 5, Total: 6}, 3, archive.Scheme{Data: 2, Total: 3}},
	} {
		got, err := narrowScheme(tc.have, tc.width)
		if err != nil {
			t.Errorf("narrowScheme(%s, %d): %v", tc.have, tc.width, err)
			continue
		}
		if got != tc.want {
			t.Errorf("narrowScheme(%s, %d) = %s, want %s", tc.have, tc.width, got, tc.want)
		}
		if got.Tolerance() < 1 {
			t.Errorf("narrowScheme(%s, %d) = %s, which survives no loss at all", tc.have, tc.width, got)
		}
	}
}

func TestRebalanceTargetKeepsTheCodeAndPrefersWhereItAlreadyIs(t *testing.T) {
	entry := &Entry{
		DataShards: 2, TotalShards: 3,
		Shards: []Shard{
			{Part: 1, ProviderID: "a"},
			{Part: 2, ProviderID: "b"},
			{Part: 3, ProviderID: "dark"},
		},
	}

	accounts, scheme, err := rebalanceTarget(entry, []string{"a", "b", "c", "d"}, false)
	if err != nil {
		t.Fatalf("rebalanceTarget: %v", err)
	}
	if scheme != archive.SchemeDefault {
		t.Errorf("scheme = %s, want the file's own %s", scheme, archive.SchemeDefault)
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %v, want three", accounts)
	}
	if accounts[0] != "a" || accounts[1] != "b" {
		t.Errorf("accounts = %v, want the two it is already on first", accounts)
	}
	if accounts[2] == "dark" {
		t.Error("the account that is not answering was chosen to hold a part")
	}
}

func TestRebalanceTargetWillNotNarrowUnlessTold(t *testing.T) {
	entry := &Entry{
		DataShards: 4, TotalShards: 6,
		Shards: []Shard{{Part: 1, ProviderID: "a"}, {Part: 2, ProviderID: "b"}},
	}

	if _, _, err := rebalanceTarget(entry, []string{"a", "b", "c", "d"}, false); err == nil {
		t.Fatal("a 4-of-6 file was narrowed onto four clouds without being asked")
	}

	accounts, scheme, err := rebalanceTarget(entry, []string{"a", "b", "c", "d"}, true)
	if err != nil {
		t.Fatalf("rebalanceTarget with narrowing on: %v", err)
	}
	if scheme != (archive.Scheme{Data: 3, Total: 4}) {
		t.Errorf("scheme = %s, want 3-of-4", scheme)
	}
	if len(accounts) != 4 {
		t.Errorf("accounts = %v, want all four", accounts)
	}
}

func TestRebalanceTargetRefusesTooFewClouds(t *testing.T) {
	entry := &Entry{DataShards: 2, TotalShards: 3, Shards: []Shard{{Part: 1, ProviderID: "a"}}}

	if _, _, err := rebalanceTarget(entry, []string{"a", "b"}, true); err == nil {
		t.Fatal("laid a file out over two clouds, where one of them holds enough to rebuild it")
	}
}

func TestCheckOnlyReportsAMissingPartAndMovesNothing(t *testing.T) {
	v, roots := newTestVault(t, 3)

	payload := []byte("the parts of this file are about to stop agreeing\n")
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	before := append([]Shard(nil), entry.Shards...)

	// One cloud is answering and holding nothing, which is what an upload that
	// happened while it was refusing leaves behind.
	emptyCloud(t, roots[2])

	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionCheck)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}

	if run.Shards.Checked != 1 || run.Shards.Short != 1 || run.Shards.Whole != 0 || run.Shards.AtRisk != 0 {
		t.Errorf("run = %+v, want one file short", run)
	}
	if run.Shards.Repaired != 0 || run.Shards.Bytes != 0 {
		t.Errorf("a check-only policy moved something: %+v", run)
	}
	if len(run.Warnings) == 0 {
		t.Error("nothing was said about the missing part")
	}

	after := v.manifest.ByID(entry.ID)
	if len(after.Shards) != len(before) {
		t.Errorf("shards = %d, want the %d it started with — nothing should have been rewritten",
			len(after.Shards), len(before))
	}
}

func TestRebalanceRebuildsOntoTheCloudsThatAnswer(t *testing.T) {
	v, roots := newTestVault(t, 4)

	payload := []byte("a file that will lose one of its three clouds and get it back\n")
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Whichever three of the four it landed on, take one of them out entirely.
	held := map[string]bool{}
	for _, s := range entry.Shards {
		held[s.ProviderID] = true
	}
	providers, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	var lost string
	for i, cfg := range providers {
		if held[cfg.ID] {
			lost = cfg.ID
			darken(t, roots[i])
			break
		}
	}
	if lost == "" {
		t.Fatal("the file landed on no account at all")
	}

	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionRebalance)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}

	if len(run.Offline) != 1 {
		t.Errorf("offline = %v, want the one cloud that was taken out", run.Offline)
	}
	if run.Shards.Short != 1 {
		t.Errorf("run = %+v, want one file short of a full set", run)
	}
	if run.Shards.Repaired != 1 || run.Shards.Failed != 0 {
		t.Fatalf("run = %+v, want the file rebuilt", run)
	}

	rebuilt := v.manifest.ByID(entry.ID)
	if got := rebuilt.Scheme(); got != archive.SchemeDefault {
		t.Errorf("scheme after the repair = %s, want the %s it was cut with", got, archive.SchemeDefault)
	}
	if got := rebuilt.Redundancy(); got != archive.SchemeDefault.Total {
		t.Errorf("parts recorded = %d, want a full set of %d", got, archive.SchemeDefault.Total)
	}
	for _, s := range rebuilt.Shards {
		if s.ProviderID == lost {
			t.Errorf("part %d was put back on the cloud that is not answering", s.Part)
		}
	}

	// And the whole point of it: the file still reads.
	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the repair: %v", err)
	}
	if string(data) != string(payload) {
		t.Error("the rebuilt file does not match what was stored")
	}
}

func TestRebalanceLeavesAFilePastTheRebuildCeiling(t *testing.T) {
	v, roots := newTestVault(t, 4)

	payload := []byte("small on disk, but this policy will refuse to rebuild anything at all\n")
	if _, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", payload, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])
	emptyCloud(t, roots[2])
	emptyCloud(t, roots[3])

	policy := dailyAt("10:00", ActionRebalance)
	policy.Shards = &ShardPolicy{RebuildLimit: 1} // one byte: everything is past it
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}

	if run.Shards.Repaired != 0 {
		t.Errorf("run = %+v, want nothing rebuilt", run)
	}
	if run.Shards.Deferred != 1 {
		t.Errorf("Deferred = %d, want the one file left for somebody to do by hand", run.Shards.Deferred)
	}
}

func TestAFileTooFarGoneIsSaidSoRatherThanAttempted(t *testing.T) {
	v, roots := newTestVault(t, 3)

	payload := []byte("this one loses two of its three parts\n")
	if _, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", payload, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])

	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionRebalance)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}

	if run.Shards.AtRisk != 1 {
		t.Errorf("run = %+v, want the file counted as past repairing", run)
	}
	if run.Shards.Repaired != 0 || run.Shards.Failed != 0 {
		t.Errorf("run = %+v, want no rebuild even attempted", run)
	}
}

func TestARunIsWrittenIntoTheFoldersHistory(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionCheck)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	for i := 0; i < automationHistory+2; i++ {
		if _, err := v.RunAutomation(context.Background(), MainScope, "/"); err != nil {
			t.Fatalf("RunAutomation %d: %v", i, err)
		}
	}

	auto, err := v.AutomationFor(MainScope, "/")
	if err != nil {
		t.Fatalf("AutomationFor: %v", err)
	}
	if len(auto.History) != automationHistory {
		t.Errorf("history holds %d runs, want it capped at %d", len(auto.History), automationHistory)
	}
	if auto.LastRunAt.IsZero() {
		t.Error("the last-run time was not stamped")
	}
	if auto.NextRunAt == nil || !auto.NextRunAt.After(auto.LastRunAt) {
		t.Errorf("NextRunAt = %v, want a time after the last run", auto.NextRunAt)
	}
	if !auto.History[0].FinishedAt.After(auto.History[1].FinishedAt) {
		t.Error("history is not newest first")
	}
}

func TestRunningOneFolderIsRefusedWhenThereIsNoPolicy(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if _, err := v.RunAutomation(context.Background(), MainScope, "/"); !errors.Is(err, ErrNoAutomation) {
		t.Errorf("RunAutomation on a bare folder = %v, want ErrNoAutomation", err)
	}
}

func TestDueAutomationsRunTheDeepestFolderFirst(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/archive/2019"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, dir := range []string{"/", "/archive", "/archive/2019"} {
		if _, err := v.SetAutomation(MainScope, dir, dailyAt("10:00", ActionCheck)); err != nil {
			t.Fatalf("SetAutomation %s: %v", dir, err)
		}
	}

	due, err := v.DueAutomations(time.Now().Add(48 * time.Hour))
	if err != nil {
		t.Fatalf("DueAutomations: %v", err)
	}
	if len(due) != 3 {
		t.Fatalf("due = %d policies, want 3", len(due))
	}
	want := []string{"/archive/2019", "/archive", "/"}
	for i, folder := range want {
		if due[i].Folder != folder {
			t.Errorf("due[%d] = %s, want %s", i, due[i].Folder, folder)
		}
	}

	// Nothing is due before the first slot comes round.
	if now, err := v.DueAutomations(time.Now()); err != nil || len(now) != 0 {
		t.Errorf("DueAutomations(now) = %d policies (%v), want none", len(now), err)
	}
}

func TestASweepChecksEachFileOnceAcrossOverlappingPolicies(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/archive"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		if _, _, err := v.Upload(context.Background(), MainScope, "/archive", name,
			[]byte(name), UploadOptions{}); err != nil {
			t.Fatalf("Upload %s: %v", name, err)
		}
	}
	if _, _, err := v.Upload(context.Background(), MainScope, "/", "top.txt",
		[]byte("top"), UploadOptions{}); err != nil {
		t.Fatalf("Upload top.txt: %v", err)
	}

	for _, dir := range []string{"/", "/archive"} {
		if _, err := v.SetAutomation(MainScope, dir, dailyAt("10:00", ActionCheck)); err != nil {
			t.Fatalf("SetAutomation %s: %v", dir, err)
		}
	}

	runs, err := v.RunDueAutomations(context.Background(), time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("RunDueAutomations: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ran %d policies, want 2", len(runs))
	}

	// The inner folder ran first and took its two files with it; the outer one
	// saw all three under it and had one left to check.
	inner, outer := runs[0], runs[1]
	if inner.Folder != "/archive" || inner.Shards.Checked != 2 {
		t.Errorf("inner run = %+v, want /archive with 2 checked", inner)
	}
	if outer.Folder != "/" || outer.Shards.Files != 3 || outer.Shards.Checked != 1 {
		t.Errorf("outer run = %+v, want / with 3 files and 1 checked", outer)
	}
}

func TestASweepSaysSoWhenNoCloudAnswers(t *testing.T) {
	v, roots := newTestVault(t, 3)
	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionRebalance)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	for _, root := range roots {
		darken(t, root)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Error == "" {
		t.Error("a sweep with every cloud dark reported no error")
	}
	if len(run.Offline) != 3 {
		t.Errorf("offline = %v, want all three", run.Offline)
	}
}

func TestOnlyOneSweepRunsAtATime(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionCheck)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	v.autoMu.Lock()
	v.autoActive = "/somewhere"
	v.autoMu.Unlock()

	_, err := v.RunAutomation(context.Background(), MainScope, "/")
	if !errors.Is(err, ErrAutomationBusy) {
		t.Errorf("RunAutomation while one is running = %v, want ErrAutomationBusy", err)
	}

	v.autoMu.Lock()
	v.autoActive = ""
	v.autoMu.Unlock()
	if _, err := v.RunAutomation(context.Background(), MainScope, "/"); err != nil {
		t.Errorf("RunAutomation once the other finished: %v", err)
	}
}

func TestAutomationNeedsAnUnlockedVault(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionCheck)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	v.Lock()

	if _, err := v.Automations(MainScope); !errors.Is(err, ErrLocked) {
		t.Errorf("Automations while locked = %v, want ErrLocked", err)
	}
	if _, err := v.RunAutomation(context.Background(), MainScope, "/"); !errors.Is(err, ErrLocked) {
		t.Errorf("RunAutomation while locked = %v, want ErrLocked", err)
	}
	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionCheck)); !errors.Is(err, ErrLocked) {
		t.Errorf("SetAutomation while locked = %v, want ErrLocked", err)
	}
}

// The case that is easiest to get wrong: the cloud is answering perfectly, and
// is simply not holding what the index says it is. Nothing about the index
// betrays that, so a repair that trusted the index would decide the file was
// already where it should be and do nothing at all.
func TestRebalancePutsBackAPartLostFromACloudThatIsAnswering(t *testing.T) {
	v, roots := newTestVault(t, 3)

	payload := []byte("one part of this is about to vanish from a cloud that is fine\n")
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "notes.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	emptyCloud(t, roots[0])

	if _, err := v.SetAutomation(MainScope, "/", dailyAt("10:00", ActionRebalance)); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if len(run.Offline) != 0 {
		t.Errorf("offline = %v, want none — every cloud answered", run.Offline)
	}
	if run.Shards.Short != 1 || run.Shards.Repaired != 1 || run.Shards.Failed != 0 {
		t.Fatalf("run = %+v, want the file found short and rebuilt", run)
	}

	// A second sweep has nothing left to do.
	again, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation again: %v", err)
	}
	if again.Shards.Whole != 1 || again.Shards.Short != 0 {
		t.Errorf("second run = %+v, want the file whole", again)
	}

	data, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch after the repair: %v", err)
	}
	if string(data) != string(payload) {
		t.Error("the rebuilt file does not match what was stored")
	}
}
