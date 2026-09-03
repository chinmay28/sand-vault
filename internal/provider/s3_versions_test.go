package provider

import (
	"context"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider/s3test"
)

// A versioned bucket keeps what a plain listing hides: every Put and every
// Delete. The backend has to be able to see all of it and erase it by version,
// which is the whole difference between tidying a bucket and appearing to.

func versionedS3(t *testing.T, stub *s3test.Server, bucket string) Versioner {
	t.Helper()

	options := stub.Options(bucket)
	options["prefix"] = "vault/"
	p, err := New(Config{Kind: KindS3, Name: "stub", Options: options})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	versioner, ok := p.(Versioner)
	if !ok {
		t.Fatal("the S3 backend does not list versions")
	}
	return versioner
}

func TestS3ListVersionsSeesWhatAPlainListingHides(t *testing.T) {
	stub := s3test.New()
	defer stub.Close()
	ctx := context.Background()
	p := versionedS3(t, stub, "shards")

	// Three writes of the index backup, one part written once, and one part
	// deleted — which on this bucket is a marker on top of the data.
	for _, body := range []string{"index one", "index two", "index three"} {
		if err := p.(Provider).Put(ctx, "manifest.sand", []byte(body)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := p.(Provider).Put(ctx, "abc-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.(Provider).Put(ctx, "old-p1.sand", []byte("part that gets deleted")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.(Provider).Delete(ctx, "old-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	listed, err := p.(Provider).List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("a plain listing shows %d object(s), want the 2 live ones: %+v", len(listed), listed)
	}

	versions, err := p.ListVersions(ctx, "")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 6 {
		t.Fatalf("listed %d version(s), want 3 + 1 + 1 + a marker = 6: %+v", len(versions), versions)
	}

	// Sorted by key and newest first, keys with the bucket prefix taken off,
	// and exactly one latest per key.
	byKey := map[string][]ObjectVersion{}
	for _, v := range versions {
		byKey[v.Key] = append(byKey[v.Key], v)
	}
	if got := len(byKey["manifest.sand"]); got != 3 {
		t.Errorf("manifest.sand has %d version(s), want 3", got)
	}
	if m := byKey["manifest.sand"]; !m[0].Latest || m[1].Latest || m[2].Latest {
		t.Errorf("the newest index backup is not the one marked latest: %+v", m)
	}
	if m := byKey["manifest.sand"]; m[0].Size != int64(len("index three")) {
		t.Errorf("the latest index backup weighs %d, want the last write's %d", m[0].Size, len("index three"))
	}
	if old := byKey["old-p1.sand"]; len(old) != 2 || !old[0].DeleteMarker || !old[0].Latest || old[1].DeleteMarker {
		t.Errorf("the deleted part should be a marker on top of its data: %+v", old)
	}
	if abc := byKey["abc-p1.sand"]; len(abc) != 1 || !abc[0].Latest || abc[0].DeleteMarker {
		t.Errorf("the untouched part should be one live latest version: %+v", abc)
	}
	for _, v := range versions {
		if v.Modified.IsZero() {
			t.Errorf("%s %s carries no modification time", v.Key, v.VersionID)
		}
	}
}

func TestS3DeleteVersionErasesForGood(t *testing.T) {
	stub := s3test.New()
	defer stub.Close()
	ctx := context.Background()
	p := versionedS3(t, stub, "shards")

	for _, body := range []string{"one", "two"} {
		if err := p.(Provider).Put(ctx, "manifest.sand", []byte(body)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	versions, err := p.ListVersions(ctx, "")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	var stale ObjectVersion
	for _, v := range versions {
		if !v.Latest {
			stale = v
		}
	}
	if stale.VersionID == "" {
		t.Fatalf("no superseded version to erase in %+v", versions)
	}

	if err := p.DeleteVersion(ctx, stale.Key, stale.VersionID); err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	// Twice, because a sweep interrupted and rerun must not trip over what it
	// already erased.
	if err := p.DeleteVersion(ctx, stale.Key, stale.VersionID); err != nil {
		t.Fatalf("DeleteVersion of an erased version: %v", err)
	}

	after, err := p.ListVersions(ctx, "")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(after) != 1 || !after[0].Latest || after[0].Size != int64(len("two")) {
		t.Fatalf("after erasing the old version the bucket holds %+v, want only the latest", after)
	}
	if got, err := p.(Provider).Get(ctx, "manifest.sand"); err != nil || string(got) != "two" {
		t.Errorf("the latest version was disturbed: %q, %v", got, err)
	}
	if got := stub.Versions("shards"); len(got) != 1 {
		t.Errorf("the bucket still stores %d version(s)", len(got))
	}
}

func TestS3ListVersionsWalksEveryPage(t *testing.T) {
	stub := s3test.New()
	stub.PageSize = 2
	defer stub.Close()
	ctx := context.Background()
	p := versionedS3(t, stub, "shards")

	for _, key := range []string{"a-p1.sand", "b-p1.sand", "c-p1.sand"} {
		for _, body := range []string{"first", "second"} {
			if err := p.(Provider).Put(ctx, key, []byte(body)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}
	versions, err := p.ListVersions(ctx, "")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 6 {
		t.Fatalf("listed %d version(s) across pages of 2, want all 6: %+v", len(versions), versions)
	}
	seen := map[string]bool{}
	for _, v := range versions {
		if seen[v.Key+v.VersionID] {
			t.Errorf("%s %s listed twice", v.Key, v.VersionID)
		}
		seen[v.Key+v.VersionID] = true
	}

	// The prefix is honoured across pages too.
	only, err := p.ListVersions(ctx, "b-")
	if err != nil {
		t.Fatalf("ListVersions with a prefix: %v", err)
	}
	if len(only) != 2 || only[0].Key != "b-p1.sand" || only[1].Key != "b-p1.sand" {
		t.Errorf("a prefixed listing returned %+v", only)
	}
}

func TestS3ListVersionsOnAnUnversionedBucketIsJustTheObjects(t *testing.T) {
	stub := s3test.New()
	stub.Versioned = false
	defer stub.Close()
	ctx := context.Background()
	p := versionedS3(t, stub, "shards")

	for _, body := range []string{"one", "two"} {
		if err := p.(Provider).Put(ctx, "manifest.sand", []byte(body)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := p.(Provider).Put(ctx, "gone-p1.sand", []byte("part")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.(Provider).Delete(ctx, "gone-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	versions, err := p.ListVersions(ctx, "")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 || versions[0].Key != "manifest.sand" || !versions[0].Latest || versions[0].DeleteMarker {
		t.Fatalf("an unversioned bucket should list one live version per object, got %+v", versions)
	}
}
