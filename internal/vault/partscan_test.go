package vault

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTheLocalDiskScanFindsLostAndTruncatedParts(t *testing.T) {
	v, roots := newTestVault(t, 4)
	ctx := context.Background()
	payload, entry := uploadWide(t, v, "photo.jpg", 400)
	_, other := uploadWide(t, v, "notes.txt", 20)

	// Nothing wrong yet: a scan covers every disk and records nothing.
	report, err := v.ScanLocalParts(ctx)
	if err != nil {
		t.Fatalf("ScanLocalParts: %v", err)
	}
	if len(report.Accounts) != 4 || report.Parts != 8 || report.Lost != 0 || report.Files != 0 || report.Unlisted != 0 {
		t.Fatalf("clean scan = %+v, want 4 disks, 8 parts, nothing lost", report)
	}

	// One part truncated, one gone: the scan writes both down, against the
	// right file, with the reason.
	truncate(t, partFile(t, roots, entry, 4))
	if err := os.Remove(partFile(t, roots, other, 1)); err != nil {
		t.Fatalf("removing a part: %v", err)
	}
	report, err = v.ScanLocalParts(ctx)
	if err != nil {
		t.Fatalf("ScanLocalParts: %v", err)
	}
	if report.Lost != 2 || report.Files != 2 || report.TookMs < 0 || report.RanAt.IsZero() {
		t.Errorf("scan = %+v, want two parts lost over two files", report)
	}
	lost := v.lostPartsOf(entry)
	if len(lost) != 1 || !strings.Contains(lost[4].Reason, "truncated") {
		t.Errorf("photo's losses = %+v, want part 4 truncated", lost)
	}
	lost = v.lostPartsOf(other)
	if len(lost) != 1 || !strings.Contains(lost[1].Reason, "not found") {
		t.Errorf("notes' losses = %+v, want part 1 not found", lost)
	}
	page, _ := v.Degraded(0, 0)
	if page.Total != 2 {
		t.Errorf("degraded after the scan = %d file(s), want both", page.Total)
	}
	// The file with a spare still reads, past the truncated part.
	if data, _, err := v.Fetch(ctx, entry.ID); err != nil || string(data) != string(payload) {
		t.Errorf("Fetch after the scan: %v", err)
	}

	// The status carries the report and the schedule, daily by default.
	status, err := v.PartScanStatus()
	if err != nil {
		t.Fatalf("PartScanStatus: %v", err)
	}
	if status.Last == nil || status.Last.Lost != 2 || status.LocalAccounts != 4 || status.Running {
		t.Errorf("status = %+v", status)
	}
	if !status.Schedule.Enabled || status.Schedule.Interval() != DefaultPartScanInterval {
		t.Errorf("schedule = %+v, want on and daily", status.Schedule)
	}
	if want := report.RanAt.Add(DefaultPartScanInterval); !status.NextAt.Equal(want) {
		t.Errorf("next at %v, want %v", status.NextAt, want)
	}
	if v.PartScanDue(time.Now()) {
		t.Error("a scan is due right after one ran")
	}
	if !v.PartScanDue(time.Now().Add(DefaultPartScanInterval + time.Minute)) {
		t.Error("a scan is not due a day later")
	}

	// A repair writes the truncated part again, and the next scan finds
	// nothing wrong with the photo; the notes' part is still gone.
	var accounts []string
	for _, s := range entry.Shards {
		accounts = append(accounts, s.ProviderID)
	}
	if _, err := v.Relocate(ctx, MainScope, entry.ID, accounts, entry.Scheme(), nil); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	report, _ = v.ScanLocalParts(ctx)
	if report.Lost != 1 || report.Files != 1 {
		t.Errorf("scan after the repair = %+v, want the notes' part alone", report)
	}

	// A part that comes back is taken off the record by the scan that sees
	// it. Put the notes' part back by relocating them, then say it is lost
	// as an earlier look might have, and let the scan find it there.
	accounts = accounts[:0]
	for _, s := range other.Shards {
		accounts = append(accounts, s.ProviderID)
	}
	if _, err := v.Relocate(ctx, MainScope, other.ID, accounts, other.Scheme(), nil); err != nil {
		t.Fatalf("Relocate notes: %v", err)
	}
	v.noteLostPart(other.ID, v.manifest.ByID(other.ID).ArchiveID, Shard{Part: 1, ProviderID: other.Shards[0].ProviderID}, "test")
	report, _ = v.ScanLocalParts(ctx)
	if report.Lost != 0 || report.Recovered != 1 {
		t.Errorf("scan after the part is back = %+v, want one recovered", report)
	}
	if page, _ := v.Degraded(0, 0); page.Total != 0 {
		t.Errorf("degraded after everything is back = %d, want none", page.Total)
	}
}

func TestTheLocalDiskScanScheduleIsKeptAndBounded(t *testing.T) {
	v, _ := newTestVault(t, 2)

	if s := v.PartScanSchedule(); !s.Enabled || s.Minutes != 24*60 {
		t.Fatalf("a fresh vault scans %+v, want daily and on", s)
	}
	if _, err := v.SetPartScanSchedule(PartScanSchedule{Enabled: true, Minutes: 5}); err == nil {
		t.Error("a five-minute scan was accepted")
	}
	if _, err := v.SetPartScanSchedule(PartScanSchedule{Enabled: true, Minutes: 60 * 24 * 60}); err == nil {
		t.Error("a two-month scan was accepted")
	}
	saved, err := v.SetPartScanSchedule(PartScanSchedule{Enabled: true, Minutes: 6 * 60})
	if err != nil || saved.Minutes != 360 {
		t.Fatalf("SetPartScanSchedule = %+v, %v", saved, err)
	}
	// Off keeps the interval.
	saved, _ = v.SetPartScanSchedule(PartScanSchedule{Enabled: false})
	if saved.Enabled || saved.Minutes != 360 {
		t.Errorf("switched off = %+v, want off at 360", saved)
	}
	if v.PartScanDue(time.Now()) {
		t.Error("a scan is due with the schedule off")
	}
	if status, _ := v.PartScanStatus(); !status.NextAt.IsZero() {
		t.Errorf("a switched-off scan still names a next time, %v", status.NextAt)
	}

	// Kept across a reopen, and across a password change.
	fresh := reopenAs(t, v, testPassword)
	if s := fresh.PartScanSchedule(); s.Enabled || s.Minutes != 360 {
		t.Errorf("after a reopen the schedule is %+v", s)
	}
	if _, err := fresh.ChangePassword(context.Background(), testPassword, "another", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	again := reopenAs(t, fresh, "another")
	if s := again.PartScanSchedule(); s.Enabled || s.Minutes != 360 {
		t.Errorf("after a password change the schedule is %+v", s)
	}

	// Switched back on, a process that has never scanned is due at once.
	if _, err := again.SetPartScanSchedule(PartScanSchedule{Enabled: true}); err != nil {
		t.Fatalf("switching on: %v", err)
	}
	if !again.PartScanDue(time.Now()) {
		t.Error("a scan is not due on a process that has never run one")
	}
}
