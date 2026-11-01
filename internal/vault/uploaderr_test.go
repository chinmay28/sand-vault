package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// An upload that fails says where it was, which account refused it and what
// to do next — the three things "upload failed" left out.

func TestUploadShortOfShardsNamesTheCloudsThatRefused(t *testing.T) {
	v, roots := newTestVault(t, 3)
	// Two of three gone: the default 2-of-3 code cannot be read back from
	// one part, so the upload has to fail rather than store a file it could
	// never return.
	breakCloud(t, v, roots[1])
	breakCloud(t, v, roots[2])

	_, _, err := v.UploadStream(context.Background(), MainScope, "/", "photo.jpg",
		bytes.NewReader(bytes.Repeat([]byte("x"), 4096)), UploadOptions{})
	if err == nil {
		t.Fatal("an upload with one cloud of three answering should fail")
	}

	var ue *UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want *UploadError: %v", err, err)
	}
	if ue.Stage != UploadScattering {
		t.Errorf("stage is %q, want %q", ue.Stage, UploadScattering)
	}
	if ue.Stored != 1 || ue.Needed != 2 || ue.Total != 3 {
		t.Errorf("counted %d stored of %d, need %d; want 1 of 3, need 2", ue.Stored, ue.Total, ue.Needed)
	}
	if len(ue.Failures) != 2 {
		t.Fatalf("%d failures, want one per broken cloud: %+v", len(ue.Failures), ue.Failures)
	}
	names := map[string]bool{}
	for i, f := range ue.Failures {
		names[f.Account] = true
		if f.Kind != "local" || f.AccountID == "" || f.Reason == "" {
			t.Errorf("failure %d is missing its account: %+v", i, f)
		}
		if i > 0 && ue.Failures[i-1].Part >= f.Part {
			t.Errorf("failures are not in shard order: %+v", ue.Failures)
		}
	}
	if !names["cloud-b"] || !names["cloud-c"] {
		t.Errorf("the failures name %v, want the two broken clouds", ue.Accounts())
	}
	if names["cloud-a"] {
		t.Error("the one cloud that took its part is blamed")
	}

	// The one line still reads the way it always did, so a log loses nothing.
	msg := err.Error()
	for _, want := range []string{"stored only 1 of 3 shards", "need at least 2", "cloud-b", "cloud-c"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}

	advice := ue.Advice()
	for _, want := range []string{"Nothing of the file was kept", "cloud-b", "cloud-c", "upload the file again"} {
		if !strings.Contains(advice, want) {
			t.Errorf("advice %q does not say %q", advice, want)
		}
	}
	if strings.Contains(advice, "cloud-a") {
		t.Errorf("advice %q points at the cloud that was fine", advice)
	}
}

func TestUploadWithOneCloudDownStillStoresAndOnlyWarns(t *testing.T) {
	v, roots := newTestVault(t, 3)
	breakCloud(t, v, roots[2])

	entry, warnings, err := v.UploadStream(context.Background(), MainScope, "/", "photo.jpg",
		bytes.NewReader([]byte("payload")), UploadOptions{})
	if err != nil {
		t.Fatalf("two clouds of three is enough for 2-of-3, got: %v", err)
	}
	if len(entry.Shards) != 2 {
		t.Errorf("stored %d shards, want the 2 that could land", len(entry.Shards))
	}
	// The refusal is a warning on the file that was stored, beside the
	// standing one about how many more clouds it can lose.
	named := false
	for _, w := range warnings {
		named = named || strings.Contains(w, "→ cloud-c")
	}
	if !named {
		t.Errorf("warnings %q, want one naming cloud-c", warnings)
	}
}

func TestUploadNamingAnUnknownAccountFailsBeforeAnythingIsSent(t *testing.T) {
	v, _ := newTestVault(t, 3)

	_, _, err := v.UploadStream(context.Background(), MainScope, "/", "nowhere.txt",
		bytes.NewReader([]byte("payload")), UploadOptions{Accounts: []string{"not-an-account"}})
	var ue *UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want *UploadError: %v", err, err)
	}
	if ue.Stage != UploadPlacing {
		t.Errorf("stage is %q, want %q", ue.Stage, UploadPlacing)
	}
	if ue.Failures != nil || ue.Total != 0 {
		t.Errorf("a placing failure has no shards to count: %+v", ue)
	}
	if !strings.Contains(err.Error(), "no connected account") {
		t.Errorf("error %q lost the reason", err)
	}
	if advice := ue.Advice(); !strings.Contains(advice, "Nothing was sent") || !strings.Contains(advice, "not all connected") {
		t.Errorf("advice %q should say nothing went and why", advice)
	}
}

func TestUploadErrorOnALockedVaultSaysToUnlock(t *testing.T) {
	v, _ := newTestVault(t, 3)
	v.Lock()

	_, _, err := v.UploadStream(context.Background(), MainScope, "/", "late.txt",
		bytes.NewReader([]byte("payload")), UploadOptions{})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v, want ErrLocked through the wrapping", err)
	}
	var ue *UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want *UploadError", err)
	}
	if !strings.Contains(ue.Advice(), "Unlock it") {
		t.Errorf("advice %q should say to unlock", ue.Advice())
	}
}

func TestFailedAtKeepsTheStageAnErrorAlreadyHas(t *testing.T) {
	inner := failedAt(UploadReading, errors.New("short read"))
	wrapped := fmt.Errorf("chunk 3: %w", inner)

	outer := failedAt(UploadScattering, wrapped)
	var ue *UploadError
	if !errors.As(outer, &ue) {
		t.Fatalf("error is %T, want *UploadError", outer)
	}
	if ue.Stage != UploadReading {
		t.Errorf("stage is %q, want the inner %q", ue.Stage, UploadReading)
	}
	if outer.Error() != "chunk 3: short read" {
		t.Errorf("message is %q, want the wrapping kept", outer.Error())
	}
	if failedAt(UploadReading, nil) != nil {
		t.Error("nil stays nil")
	}
}

func TestShortOfShardsOrdersFailuresByPart(t *testing.T) {
	e := shortOfShards(1, 2, 3, map[int]ShardFailure{
		3: {Part: 3, Account: "c", Reason: "no"},
		2: {Part: 2, Account: "b", Reason: "no"},
	})
	if e.Failures[0].Part != 2 || e.Failures[1].Part != 3 {
		t.Errorf("failures %+v, want shard 2 before shard 3", e.Failures)
	}
	if got, want := e.Error(), "stored only 1 of 3 shards, need at least 2: shard 2 → b: no; shard 3 → c: no"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := e.Accounts(); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("Accounts() = %v, want [b c]", got)
	}
}

func TestClassifyRefusalReadsWhatACloudSaid(t *testing.T) {
	cases := []struct {
		reason string
		want   refusal
	}{
		{"google drive put: 403 Forbidden: storageQuotaExceeded", refusalNoRoom},
		{"dropbox upload: 507 Insufficient Storage", refusalNoRoom},
		{"write /mnt/nas/parts/x: no space left on device — the filesystem is full.", refusalNoRoom},
		{"onedrive put: 429 Too Many Requests", refusalThrottled},
		{"box upload: 401 Unauthorized", refusalSignIn},
		{"google drive put: 403 Forbidden", refusalSignIn},
		{"refreshing the token: invalid_grant", refusalSignIn},
		{"this Proton account is signed out — sign in again", refusalSignIn},
		{"Put \"https://x/\": dial tcp: lookup x: no such host", refusalUnreachable},
		{"context deadline exceeded", refusalUnreachable},
		{"webdav put: 503 Service Unavailable", refusalUnreachable},
		{"mkdir /mnt/nas/parts: not a directory — part of that path is a file, not a directory.", refusalUnreachable},
		{"something nobody has seen before", refusalUnknown},
	}
	for _, tc := range cases {
		if got := classifyRefusal(tc.reason); got != tc.want {
			t.Errorf("classifyRefusal(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func TestRefusalAdvicePointsAtTheRightPlace(t *testing.T) {
	cases := []struct {
		failure ShardFailure
		want    string
	}{
		{ShardFailure{Account: "Drive", Kind: "gdrive", Reason: "403 storageQuotaExceeded"}, "no room left"},
		{ShardFailure{Account: "Drive", Kind: "gdrive", Reason: "401 Unauthorized"}, "sign in to it again"},
		{ShardFailure{Account: "Drive", Kind: "gdrive", Reason: "429 Too Many Requests"}, "slow down"},
		{ShardFailure{Account: "Drive", Kind: "gdrive", Reason: "dial tcp: i/o timeout"}, "Cloud health"},
		{ShardFailure{Account: "NAS", Kind: "local", Reason: "not a directory"}, "drive is mounted"},
		{ShardFailure{Account: "NAS", Kind: "local", Reason: "mystery"}, "mounted and writable"},
		{ShardFailure{Account: "Drive", Kind: "gdrive", Reason: "mystery"}, "Cloud health"},
	}
	for _, tc := range cases {
		got := refusalAdvice(tc.failure)
		if !strings.HasPrefix(got, tc.failure.Account) {
			t.Errorf("advice %q should lead with the account", got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("advice for %q is %q, want it to say %q", tc.failure.Reason, got, tc.want)
		}
	}
}

func TestAdviceSaysSomethingAtEveryStage(t *testing.T) {
	for _, stage := range []UploadStage{UploadPlacing, UploadReading, UploadScattering, UploadRecording} {
		e := &UploadError{Stage: stage, Err: errors.New("boom")}
		if e.Advice() == "" {
			t.Errorf("no advice at stage %q", stage)
		}
	}
	interrupted := &UploadError{Stage: UploadScattering, Err: context.Canceled}
	if !strings.Contains(interrupted.Advice(), "interrupted") {
		t.Errorf("a cancelled scatter should say it was interrupted, got %q", interrupted.Advice())
	}
	// One account named once, however many shards it refused.
	twice := shortOfShards(0, 2, 3, map[int]ShardFailure{
		1: {Part: 1, AccountID: "x", Account: "Drive", Kind: "gdrive", Reason: "401"},
		2: {Part: 2, AccountID: "x", Account: "Drive", Kind: "gdrive", Reason: "401"},
	})
	if n := strings.Count(twice.Advice(), "Drive turned"); n != 1 {
		t.Errorf("the account is named %d times in %q, want once", n, twice.Advice())
	}
	if !strings.Contains(twice.Advice(), "no part landed anywhere") {
		t.Errorf("nothing landed, and the advice should say so: %q", twice.Advice())
	}
}
