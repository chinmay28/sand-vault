package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// S3 answers a missing key and a missing bucket with the same 404, and only
// the error document tells them apart. A bucket that is mistyped, deleted or
// out of the key's reach must read as a broken account — not as an account on
// which every part is missing and every delete succeeds.

// s3ErrorDoc is the XML S3 puts in a 404.
func s3ErrorDoc(code, message string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message>`+
		`<BucketName>shards</BucketName></Error>`, code, message)
}

// missingBucketS3 is an endpoint on which the bucket does not exist: every
// call is a 404 with NoSuchBucket, and a HEAD is a bodiless 404.
func missingBucketS3(t *testing.T) Provider {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			w.Write([]byte(s3ErrorDoc("NoSuchBucket", "The specified bucket does not exist")))
		}
	}))
	t.Cleanup(stub.Close)

	p, err := New(Config{Kind: KindS3, Name: "stub", Options: map[string]string{
		"bucket":            "shards",
		"region":            "us-east-1",
		"access_key_id":     "AKIA",
		"secret_access_key": "secret",
		"endpoint":          stub.URL,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestS3MissingBucketIsAnErrorNotAMissingPart(t *testing.T) {
	p := missingBucketS3(t)
	ctx := context.Background()

	if _, err := p.Get(ctx, "abc-p1.sand"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get on a missing bucket = %v, want an error that is not ErrNotFound", err)
	} else if !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Errorf("Get error does not name the cause: %v", err)
	}
	if err := p.Delete(ctx, "abc-p1.sand"); err == nil {
		t.Error("Delete on a missing bucket reported success")
	}
	if err := p.(Versioner).DeleteVersion(ctx, "abc-p1.sand", "v1"); err == nil {
		t.Error("DeleteVersion on a missing bucket reported success")
	}
	if _, err := p.List(ctx, ""); err == nil {
		t.Error("List on a missing bucket came back empty rather than failing")
	}

	// A HEAD carries no body, so Stat cannot tell the two apart and answers
	// ErrNotFound as it always did — which is why Ping has to be the call
	// that notices the bucket is gone.
	if _, err := p.Stat(ctx, "abc-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat on a missing bucket = %v, want ErrNotFound", err)
	}
	err := p.Ping(ctx)
	if err == nil {
		t.Fatal("Ping on a missing bucket reported healthy")
	}
	if !strings.Contains(err.Error(), `bucket "shards" does not exist`) {
		t.Errorf("Ping does not say the bucket is missing: %v", err)
	}
}

// The 404 that does mean a missing key keeps meaning it.
func TestS3NoSuchKeyIsStillNotFound(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		code := "NoSuchKey"
		if r.URL.Query().Has("versionId") {
			code = "NoSuchVersion"
		}
		w.Write([]byte(s3ErrorDoc(code, "The specified key does not exist.")))
	}))
	defer stub.Close()

	p, err := New(Config{Kind: KindS3, Name: "stub", Options: map[string]string{
		"bucket":            "shards",
		"region":            "us-east-1",
		"access_key_id":     "AKIA",
		"secret_access_key": "secret",
		"endpoint":          stub.URL,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if _, err := p.Get(ctx, "abc-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a missing key = %v, want ErrNotFound", err)
	}
	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Errorf("Delete of a missing key = %v, want nil", err)
	}
	if err := p.(Versioner).DeleteVersion(ctx, "abc-p1.sand", "v1"); err != nil {
		t.Errorf("DeleteVersion of a missing version = %v, want nil", err)
	}
}

// A 404 with no error document — a proxy's, or a server that is not S3 at
// all — says nothing about the key, and nothing is what Get and Delete can
// safely conclude from it.
func TestS3BodilessNotFoundOnAGetIsAnError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer stub.Close()

	p, err := New(Config{Kind: KindS3, Name: "stub", Options: map[string]string{
		"bucket":            "shards",
		"region":            "us-east-1",
		"access_key_id":     "AKIA",
		"secret_access_key": "secret",
		"endpoint":          stub.URL,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := p.Get(ctx, "abc-p1.sand"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get with a bodiless 404 = %v, want an error that is not ErrNotFound", err)
	}
	if err := p.Delete(ctx, "abc-p1.sand"); err == nil {
		t.Error("Delete with a bodiless 404 reported success")
	}
}
