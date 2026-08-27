package vault

import (
	"os"
	"testing"
	"time"
)

// breakCloud makes a connected local folder unreachable the way a NAS that has
// been switched off is unreachable: the path is still configured and there is
// nothing usable at the end of it. A plain file where the folder was is what
// does it, because a folder cannot be made at a path a file already occupies —
// which is true for root as well, so the test means the same thing in a
// container as it does on a laptop.
func breakCloud(t *testing.T, root string) {
	t.Helper()

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("clearing %s: %v", root, err)
	}
	if err := os.WriteFile(root, []byte("not a folder"), 0600); err != nil {
		t.Fatalf("blocking %s: %v", root, err)
	}
}

// The whole point of the feature, in one test: a cloud goes dark and the vault
// can say so without anybody having opened anything.
func TestCheckCloudsFindsTheOneThatIsDown(t *testing.T) {
	v, roots := newTestVault(t, 3)

	report, err := v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	if report.Accounts != 3 || report.Healthy != 3 || report.Unhealthy != 0 {
		t.Fatalf("three working clouds report %d healthy, %d unhealthy of %d",
			report.Healthy, report.Unhealthy, report.Accounts)
	}
	if report.CheckedAt.IsZero() {
		t.Error("a check that ran reports no time")
	}

	breakCloud(t, roots[1])

	report, err = v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	if report.Unhealthy != 1 || report.Healthy != 2 {
		t.Fatalf("one dead cloud of three reports %d healthy, %d unhealthy",
			report.Healthy, report.Unhealthy)
	}

	// Worst first, so the account that needs attention is the one at the top of
	// the list rather than somewhere in the middle of seventeen.
	worst := report.Clouds[0]
	if worst.Healthy || worst.Error == "" {
		t.Errorf("the list leads with %+v, want the unreachable one with its reason", worst)
	}
	if worst.FailingSince.IsZero() {
		t.Error("an unreachable cloud reports no time it started failing")
	}
	if worst.Name != "cloud-b" {
		t.Errorf("the unreachable cloud is reported as %q, want cloud-b", worst.Name)
	}
}

// How long it has been down is a different fact from when it was last checked,
// and the one that decides whether this is a blip or a fortnight.
func TestFailingSinceSurvivesLaterChecks(t *testing.T) {
	v, roots := newTestVault(t, 2)
	breakCloud(t, roots[0])

	first, err := v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	since := first.Clouds[0].FailingSince
	if since.IsZero() {
		t.Fatal("no failing-since on the first check")
	}

	second, err := v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	again := second.Clouds[0]
	if !again.FailingSince.Equal(since) {
		t.Errorf("failing since moved from %v to %v between checks", since, again.FailingSince)
	}
	if again.CheckedAt.Before(since) {
		t.Errorf("checked at %v, before it started failing at %v", again.CheckedAt, since)
	}

	// And it starts again from scratch once the account has come back and gone
	// away a second time, rather than reporting the first outage forever.
	if err := os.Remove(roots[0]); err != nil {
		t.Fatalf("unblocking: %v", err)
	}
	if report, err := v.CheckClouds(t.Context()); err != nil {
		t.Fatalf("CheckClouds: %v", err)
	} else if report.Unhealthy != 0 {
		t.Fatalf("a repaired cloud is still unhealthy: %+v", report.Clouds)
	}

	breakCloud(t, roots[0])
	third, err := v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	if !third.Clouds[0].FailingSince.After(since) {
		t.Errorf("a second outage reports the first one's start, %v", third.Clouds[0].FailingSince)
	}
}

// The figure is about the accounts as they are now, not as they were when the
// last check ran.
func TestCloudHealthFollowsTheConnectedAccounts(t *testing.T) {
	v, roots := newTestVault(t, 3)
	breakCloud(t, roots[2])

	if _, err := v.CheckClouds(t.Context()); err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	var broken string
	for _, st := range statuses {
		if !st.Online {
			broken = st.ID
		}
	}
	if broken == "" {
		t.Fatal("no account reported offline")
	}

	// Disconnecting the dead account is one of the two ways somebody answers
	// this figure, and the count has to follow them there — a cloud that is no
	// longer connected cannot be unhealthy.
	if err := v.RemoveProvider(broken, true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	report, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	if report.Accounts != 2 || report.Unhealthy != 0 {
		t.Errorf("after disconnecting the dead cloud: %d accounts, %d unhealthy",
			report.Accounts, report.Unhealthy)
	}
}

// Drawing the accounts panel pings every account, which is the same sweep this
// runs on a timer — so it counts as one, and the hourly check does not go out
// and repeat it a minute later.
func TestDrawingThePanelCountsAsACheck(t *testing.T) {
	v, _ := newTestVault(t, 2)

	if v.HealthCheckDue(time.Now()) != true {
		t.Fatal("a vault that has never been checked is not due")
	}
	if _, err := v.ProviderStatuses(t.Context()); err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}

	report, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	if report.Healthy != 2 || report.Unchecked != 0 {
		t.Errorf("the panel's own ping left %d checked healthy, %d unchecked",
			report.Healthy, report.Unchecked)
	}
	if v.HealthCheckDue(time.Now()) {
		t.Error("a check is due immediately after the panel pinged everything")
	}
	// And it does come round again once the interval has passed.
	if !v.HealthCheckDue(time.Now().Add(DefaultHealthInterval + time.Minute)) {
		t.Error("no check due an hour and a minute after the last one")
	}
}

// Testing one account is not a sweep. It says nothing about the other sixteen,
// so it must not push the check that would have asked them out by an hour.
func TestTestingOneAccountIsNotASweep(t *testing.T) {
	v, _ := newTestVault(t, 2)
	configs, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}

	if err := v.TestProvider(t.Context(), configs[0].ID); err != nil {
		t.Fatalf("TestProvider: %v", err)
	}
	if !v.HealthCheckDue(time.Now()) {
		t.Error("testing one account satisfied the whole-vault check")
	}

	report, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	if report.Healthy != 1 || report.Unchecked != 1 {
		t.Errorf("after testing one of two: %d healthy, %d unchecked",
			report.Healthy, report.Unchecked)
	}
}

// The schedule is a setting on the vault, so it survives a lock like the
// placement policy does — and is readable while the vault is shut, since the
// scheduler consults it before there is anything to unlock.
func TestHealthScheduleIsStored(t *testing.T) {
	v, _ := newTestVault(t, 1)

	if s := v.HealthSchedule(); !s.Enabled || s.Interval() != DefaultHealthInterval {
		t.Fatalf("a fresh vault checks %+v, want hourly and on", s)
	}

	if _, err := v.SetHealthSchedule(HealthSchedule{Enabled: true, Minutes: 15}); err != nil {
		t.Fatalf("SetHealthSchedule: %v", err)
	}
	v.Lock()
	if s := v.HealthSchedule(); s.Minutes != 15 {
		t.Errorf("a locked vault reports %+v, want the 15 minutes it was set to", s)
	}
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if s := v.HealthSchedule(); s.Minutes != 15 || !s.Enabled {
		t.Errorf("after unlocking: %+v", s)
	}

	// Switching it off keeps the interval, so switching it back on returns to
	// the schedule somebody chose rather than to the default.
	off := false
	if _, err := v.SetHealthSchedule(HealthSchedule{Enabled: off}); err != nil {
		t.Fatalf("SetHealthSchedule off: %v", err)
	}
	s := v.HealthSchedule()
	if s.Enabled || s.Minutes != 15 {
		t.Errorf("switched off, the schedule is %+v", s)
	}
	if v.HealthCheckDue(time.Now()) {
		t.Error("a check is due on a vault whose check is switched off")
	}

	for _, bad := range []int{1, 8 * 24 * 60} {
		if _, err := v.SetHealthSchedule(HealthSchedule{Enabled: true, Minutes: bad}); err == nil {
			t.Errorf("%d minutes was accepted as an interval", bad)
		}
	}
}

// A locked vault reports nothing rather than an hour-old ping of accounts whose
// names it can no longer read.
func TestCloudHealthNeedsAnOpenVault(t *testing.T) {
	v, _ := newTestVault(t, 2)
	if _, err := v.CheckClouds(t.Context()); err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}

	v.Lock()
	if _, err := v.CloudHealth(); err != ErrLocked {
		t.Errorf("a locked vault answers with %v, want ErrLocked", err)
	}
	if _, err := v.CheckClouds(t.Context()); err != ErrLocked {
		t.Errorf("checking a locked vault gives %v, want ErrLocked", err)
	}

	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	report, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	if report.Unchecked != 2 || report.Healthy != 0 {
		t.Errorf("a freshly unlocked vault reports %d unchecked, %d healthy — "+
			"what was found before the lock should have gone with the keys",
			report.Unchecked, report.Healthy)
	}
}
