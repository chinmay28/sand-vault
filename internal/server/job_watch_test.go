package server

import (
	"context"
	"testing"
	"time"
)

// The watch's contract beyond what a scan uses: runs are scoped, a second ask
// for the same scope joins, one for another scope is turned away without
// disturbing the run in its way, and a finished answer can be forgotten
// without touching a running one.

type countProgress struct{ N int }
type countResult struct{ Total int }

func heldJob() (jobFunc[countProgress, countResult], chan struct{}, chan struct{}) {
	release := make(chan struct{})
	began := make(chan struct{}, 1)
	run := func(ctx context.Context, onProgress func(countProgress)) (*countResult, error) {
		onProgress(countProgress{N: 1})
		began <- struct{}{}
		select {
		case <-release:
			return &countResult{Total: 7}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return run, release, began
}

func TestJobWatchScopesItsRuns(t *testing.T) {
	jw := newJobWatch[countProgress, countResult]()
	run, release, began := heldJob()

	first, outcome := jw.start("cloud-a", run, time.Minute)
	if outcome != jobStarted || first.Scope != "cloud-a" || !first.Running {
		t.Fatalf("start = %+v, %v; want a running run scoped to cloud-a", first, outcome)
	}
	<-began

	joined, outcome := jw.start("cloud-a", run, time.Minute)
	if outcome != jobJoined || joined.ID != first.ID {
		t.Fatalf("the same scope did not join: %+v, %v", joined, outcome)
	}

	other, _, _ := heldJob()
	busy, outcome := jw.start("cloud-b", other, time.Minute)
	if outcome != jobBusy || busy.ID != first.ID || busy.Scope != "cloud-a" {
		t.Fatalf("another scope was not turned away with the run in its way: %+v, %v", busy, outcome)
	}

	// forgetFinished leaves a running run where it is.
	jw.forgetFinished()
	if !jw.running() || jw.status().ID != first.ID {
		t.Fatal("forgetFinished dropped a running run")
	}

	close(release)
	awaitScan(t, "the run to finish", func() bool { return !jw.running() })
	done := jw.status()
	if done.Result == nil || done.Result.Total != 7 || done.Scope != "cloud-a" {
		t.Fatalf("finished status = %+v, want the result and its scope", done)
	}

	// Once it is over, another scope starts afresh.
	next, release2, began2 := heldJob()
	fresh, outcome := jw.start("cloud-b", next, time.Minute)
	if outcome != jobStarted || fresh.ID == first.ID || fresh.Scope != "cloud-b" {
		t.Fatalf("a start after the finish = %+v, %v; want a fresh run for cloud-b", fresh, outcome)
	}
	<-began2
	close(release2)
	awaitScan(t, "the second run to finish", func() bool { return !jw.running() })

	jw.forgetFinished()
	if status := jw.status(); status.ID != "" || status.Result != nil {
		t.Fatalf("forgetFinished kept a finished run: %+v", status)
	}
}
