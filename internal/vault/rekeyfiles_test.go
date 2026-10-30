package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pendingPaths is the listing's paths, in order, for a short way of saying what a
// page holds.
func pendingPaths(page *RekeyPendingPage) string {
	var out []string
	for _, f := range page.Files {
		out = append(out, f.Path)
	}
	return strings.Join(out, " ")
}

func TestPendingRekeyFilesAreListedInTheOrderThePlanTakesThem(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	uploadSized(t, v, "/Taxes", "p60.pdf", 500)
	uploadSized(t, v, "/Taxes", "return.pdf", 900)
	uploadSized(t, v, "/Photos", "holiday.jpg", 2000)
	notes := uploadSized(t, v, "/", "notes.txt", 100)

	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{BytesPerDay: 1 << 40, Folders: []string{"/Taxes"}}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}

	// The queued folder first, smallest file first within it, then the rest
	// smallest first — the order the steps take them in.
	page, err := v.PendingRekeyFiles("/", 0, 0)
	if err != nil {
		t.Fatalf("PendingRekeyFiles: %v", err)
	}
	if got := pendingPaths(page); got != "/Taxes/p60.pdf /Taxes/return.pdf /notes.txt /Photos/holiday.jpg" {
		t.Errorf("order = %q", got)
	}
	if page.Total != 4 || page.Bytes != 3500 || page.Folder != "/" || page.Limit != DefaultRekeyPage {
		t.Errorf("page = total %d, bytes %d, folder %q, limit %d; want 4, 3500, /, the default", page.Total, page.Bytes, page.Folder, page.Limit)
	}
	for _, f := range page.Files {
		if f.State != RekeyFileQueued || f.Reason != "" || f.HeldOn != "" || !f.RetryAt.IsZero() {
			t.Errorf("%s = %+v, want queued and nothing else said", f.Path, f)
		}
		if f.Dir == "" || f.Name == "" || f.ID == "" || f.Bytes == 0 {
			t.Errorf("%s is missing a field: %+v", f.Path, f)
		}
	}

	// A file left for last goes to the back, and says so.
	if _, err := v.DeferRekeyFile(notes.ID); err != nil {
		t.Fatalf("DeferRekeyFile: %v", err)
	}
	page, _ = v.PendingRekeyFiles("/", 0, 0)
	if got := pendingPaths(page); got != "/Taxes/p60.pdf /Taxes/return.pdf /Photos/holiday.jpg /notes.txt" {
		t.Errorf("order with notes deferred = %q", got)
	}
	if last := page.Files[3]; last.State != RekeyFileDeferred {
		t.Errorf("deferred file = %+v, want state deferred", last)
	}

	// Confined to a folder, the order and the figures are the folder's own.
	page, err = v.PendingRekeyFiles("/Taxes", 0, 0)
	if err != nil {
		t.Fatalf("PendingRekeyFiles(/Taxes): %v", err)
	}
	if got := pendingPaths(page); got != "/Taxes/p60.pdf /Taxes/return.pdf" || page.Total != 2 || page.Bytes != 1400 || page.Folder != "/Taxes" {
		t.Errorf("/Taxes = %q, total %d, bytes %d, folder %q", got, page.Total, page.Bytes, page.Folder)
	}
	if _, err := v.PendingRekeyFiles("/Nowhere", 0, 0); err == nil || !strings.HasPrefix(err.Error(), "no such folder") {
		t.Errorf("a folder that is not there = %v, want no such folder", err)
	}

	// Pages window the same order; past the end is empty, with the totals.
	page, _ = v.PendingRekeyFiles("/", 1, 2)
	if got := pendingPaths(page); got != "/Taxes/return.pdf /Photos/holiday.jpg" || page.Offset != 1 || page.Limit != 2 || page.Total != 4 {
		t.Errorf("page 1,2 = %q, offset %d, limit %d, total %d", got, page.Offset, page.Limit, page.Total)
	}
	page, _ = v.PendingRekeyFiles("/", 10, 2)
	if len(page.Files) != 0 || page.Total != 4 || page.Bytes != 3500 {
		t.Errorf("page past the end = %d file(s), total %d, bytes %d; want none, 4, 3500", len(page.Files), page.Total, page.Bytes)
	}
	page, _ = v.PendingRekeyFiles("/", 0, MaxRekeyPage*2)
	if page.Limit != MaxRekeyPage {
		t.Errorf("limit %d was not trimmed to %d", page.Limit, MaxRekeyPage)
	}

	// A plan that stops at its folders leaves the rest outside, after
	// everything it will take — the deferred file among them.
	if _, err := v.SetRekeyPlan(RekeySettings{BytesPerDay: 1 << 40, Folders: []string{"/Taxes"}, OnlyFolders: true}); err != nil {
		t.Fatalf("SetRekeyPlan: %v", err)
	}
	page, _ = v.PendingRekeyFiles("/", 0, 0)
	if got := pendingPaths(page); got != "/Taxes/p60.pdf /Taxes/return.pdf /Photos/holiday.jpg /notes.txt" {
		t.Errorf("order under only-folders = %q", got)
	}
	for _, f := range page.Files[2:] {
		if f.State != RekeyFileOutside {
			t.Errorf("%s = %s, want outside", f.Path, f.State)
		}
	}
	for _, f := range page.Files[:2] {
		if f.State != RekeyFileQueued {
			t.Errorf("%s = %s, want queued", f.Path, f.State)
		}
	}

	// With no plan, the files are still listed, smallest first.
	if _, err := v.MigrateFiles(ctx, nil); err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	page, _ = v.PendingRekeyFiles("/", 0, 0)
	if page.Total != 0 || len(page.Files) != 0 {
		t.Errorf("after everything moved: %d file(s), total %d", len(page.Files), page.Total)
	}
	uploadSized(t, v, "/", "later.txt", 10)
	if _, err := v.ChangePassword(ctx, "patient", "unplanned", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	page, _ = v.PendingRekeyFiles("/", 0, 0)
	if got := pendingPaths(page); got != "/later.txt /notes.txt /Taxes/p60.pdf /Taxes/return.pdf /Photos/holiday.jpg" {
		t.Errorf("order with no plan = %q", got)
	}
}

func TestPendingRekeyFilesSayWhyAFileIsNotMoving(t *testing.T) {
	v, roots := newTestVault(t, 2)
	ctx := context.Background()
	uploadSized(t, v, "/", "fine.txt", 100)
	stranded := uploadSized(t, v, "/", "stranded.txt", 50)
	missing := shardPath(roots, stranded.Shards[0].Key)
	saved, err := os.ReadFile(missing)
	if err != nil {
		t.Fatalf("reading the part: %v", err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatalf("removing the part: %v", err)
	}
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	now := time.Now()

	// Before anything is tried, nothing is waiting on a retry.
	status, _ := v.RekeyStatus()
	if status.Retrying != 0 || status.RetryInSeconds != 0 {
		t.Errorf("status before a try = retrying %d in %ds", status.Retrying, status.RetryInSeconds)
	}

	// The stranded file fails, the fine one moves, and the list says which
	// is which: the failure, with why and when it is tried again.
	if step, _ := v.RekeyStep(ctx, now); step.Moved || step.Warning == "" {
		t.Fatalf("step on the stranded file = %+v, want a warning", step)
	}
	if step, _ := v.RekeyStep(ctx, now); !step.Moved || step.Path != "/fine.txt" {
		t.Fatalf("next step = %+v, want fine.txt moved", step)
	}
	page, err := v.PendingRekeyFiles("/", 0, 0)
	if err != nil {
		t.Fatalf("PendingRekeyFiles: %v", err)
	}
	if len(page.Files) != 1 || page.Files[0].Path != "/stranded.txt" {
		t.Fatalf("pending = %q, want the stranded file alone", pendingPaths(page))
	}
	f := page.Files[0]
	if f.State != RekeyFileRetrying || !strings.Contains(f.Reason, "/stranded.txt") {
		t.Errorf("stranded = %+v, want retrying with a reason naming it", f)
	}
	if until := f.RetryAt.Sub(now); until < rekeyRetryAfter-time.Minute || until > rekeyRetryAfter+time.Minute {
		t.Errorf("retry at %v from now, want about %v", until, rekeyRetryAfter)
	}
	status, _ = v.RekeyStatus()
	if status.Retrying != 1 || status.RetryInSeconds <= 0 || status.RetryInSeconds > int64(rekeyRetryAfter.Seconds()) {
		t.Errorf("status = retrying %d in %ds, want 1 within the hour", status.Retrying, status.RetryInSeconds)
	}

	// Moved by a finish-now pass meanwhile — its part back, and the pass
	// not bound by the plan's hour — the file is no longer waiting on
	// anything, however long the skip has left.
	if err := os.WriteFile(missing, saved, 0o600); err != nil {
		t.Fatalf("restoring the part: %v", err)
	}
	if _, err := v.MigrateFiles(ctx, nil); err != nil {
		t.Fatalf("MigrateFiles: %v", err)
	}
	status, _ = v.RekeyStatus()
	if status.Retrying != 0 || status.RetryInSeconds != 0 {
		t.Errorf("after the file moved, retrying = %d in %ds, want nothing", status.Retrying, status.RetryInSeconds)
	}
}

func TestPendingRekeyFilesNameTheAccountAHeldFileWaitsFor(t *testing.T) {
	// Two accounts under the strict policy is two parts per file and both
	// needed, so a file with one of them on a dark account cannot be read.
	v, roots := newTestVault(t, 2)
	ctx := context.Background()
	uploadSized(t, v, "/", "held.txt", 50)
	if _, err := v.ChangePasswordWithPlan(ctx, testPassword, "patient", &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	kept := filepath.Join(t.TempDir(), "away")
	if err := os.Rename(roots[0], kept); err != nil {
		t.Fatalf("moving the account's folder away: %v", err)
	}
	if err := os.WriteFile(roots[0], []byte("not a folder"), 0o600); err != nil {
		t.Fatalf("blocking the account's folder: %v", err)
	}
	if report, err := v.CheckClouds(ctx); err != nil || report.Unhealthy != 1 {
		t.Fatalf("CheckClouds = %+v, %v; want one account down", report, err)
	}

	page, err := v.PendingRekeyFiles("/", 0, 0)
	if err != nil {
		t.Fatalf("PendingRekeyFiles: %v", err)
	}
	if len(page.Files) != 1 || page.Files[0].State != RekeyFileHeld || page.Files[0].HeldOn != "cloud-a" {
		t.Errorf("held file = %+v, want held on cloud-a", page.Files)
	}
}
