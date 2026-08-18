package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalProviderRoundTrip(t *testing.T) {
	root := t.TempDir()
	p, err := New(Config{Kind: KindLocal, Name: "disk", Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "abc123-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := p.Get(ctx, "abc123-p1.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	info, err := p.Stat(ctx, "abc123-p1.sand")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Stat size = %d, want %d", info.Size, len(payload))
	}

	objects, err := p.List(ctx, "abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "abc123-p1.sand" {
		t.Errorf("List = %+v, want one shard key", objects)
	}

	if err := p.Delete(ctx, "abc123-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "abc123-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	// Deleting twice is a no-op, not an error: shard cleanup runs on paths
	// that may already be gone.
	if err := p.Delete(ctx, "abc123-p1.sand"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// TestLocalProviderContainsEscapingKeys checks that a key trying to climb out
// of the account's directory is neutralized rather than followed. Keys are
// generated internally today, but a provider root is a blast radius worth
// keeping closed.
func TestLocalProviderContainsEscapingKeys(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "vault-root")

	p, err := New(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	for _, key := range []string{"../../escaped.sand", "/../../escaped.sand", "a/../../../escaped.sand"} {
		if err := p.Put(ctx, key, []byte("nope")); err != nil {
			continue // rejecting outright is fine too
		}
		if _, err := os.Stat(filepath.Join(parent, "escaped.sand")); err == nil {
			t.Fatalf("key %q wrote a file outside the provider root", key)
		}
	}

	// The write should have landed inside the root instead.
	if _, err := os.Stat(filepath.Join(root, "escaped.sand")); err != nil {
		t.Errorf("expected the escaping key to be rewritten inside the root: %v", err)
	}
}

func TestLocalProviderWriteIsAtomic(t *testing.T) {
	root := t.TempDir()
	p, _ := New(Config{Kind: KindLocal, Options: map[string]string{"path": root}})
	ctx := context.Background()

	if err := p.Put(ctx, "shard.sand", []byte("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Put(ctx, "shard.sand", []byte("second")); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}

	got, _ := p.Get(ctx, "shard.sand")
	if string(got) != "second" {
		t.Errorf("content = %q, want second", got)
	}

	// No temp files should survive a successful write.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sand-tmp-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestNewRejectsMissingRequiredOptions(t *testing.T) {
	if _, err := New(Config{Kind: KindLocal, Options: map[string]string{}}); err == nil {
		t.Fatal("expected an error when a required option is missing")
	}
	if _, err := New(Config{Kind: Kind("nope")}); err == nil {
		t.Fatal("expected an error for an unknown provider kind")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	p, err := New(Config{Kind: KindS3, Options: map[string]string{
		"bucket":            "shards",
		"access_key_id":     "AKIA",
		"secret_access_key": "secret",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.Config().Option("region"); got != "us-east-1" {
		t.Errorf("region default = %q, want us-east-1", got)
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	cfg := Config{Kind: KindS3, Options: map[string]string{
		"bucket":            "shards",
		"access_key_id":     "AKIAEXAMPLE",
		"secret_access_key": "super-secret-value",
	}}

	redacted := cfg.Redacted()
	if redacted.Options["secret_access_key"] == "super-secret-value" {
		t.Error("secret_access_key was not redacted")
	}
	if redacted.Options["bucket"] != "shards" {
		t.Error("non-secret options should survive redaction")
	}
	if cfg.Options["secret_access_key"] != "super-secret-value" {
		t.Error("Redacted mutated the original config")
	}
}

func TestSpecsCoverEveryKind(t *testing.T) {
	specs := Specs()
	found := map[Kind]bool{}
	for _, s := range specs {
		found[s.Kind] = true
		if s.Label == "" || s.Description == "" {
			t.Errorf("%s is missing a label or description for the connect form", s.Kind)
		}
	}
	for _, kind := range []Kind{KindLocal, KindS3, KindWebDAV, KindGDrive, KindDropbox} {
		if !found[kind] {
			t.Errorf("no spec registered for %s", kind)
		}
	}
}

// TestS3SigV4AgainstStubEndpoint drives the S3 backend against a local stub to
// check the request shape and that signing produces the headers AWS requires.
func TestS3SigV4AgainstStubEndpoint(t *testing.T) {
	var gotAuth, gotSHA, gotDate, gotPath, gotMethod string
	stored := map[string][]byte{}

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSHA = r.Header.Get("X-Amz-Content-Sha256")
		gotDate = r.Header.Get("X-Amz-Date")
		gotPath = r.URL.Path
		gotMethod = r.Method

		switch r.Method {
		case http.MethodPut:
			body := make([]byte, r.ContentLength)
			r.Body.Read(body)
			stored[r.URL.Path] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				w.Header().Set("Content-Type", "application/xml")
				w.Write([]byte(`<?xml version="1.0"?><ListBucketResult>` +
					`<IsTruncated>false</IsTruncated>` +
					`<Contents><Key>abc-p1.sand</Key><Size>42</Size></Contents>` +
					`</ListBucketResult>`))
				return
			}
			body, ok := stored[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)
		case http.MethodDelete:
			delete(stored, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer stub.Close()

	p, err := New(Config{Kind: KindS3, Name: "stub", Options: map[string]string{
		"bucket":            "shards",
		"region":            "us-east-1",
		"access_key_id":     "AKIAIOSFODNN7EXAMPLE",
		"secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"endpoint":          stub.URL,
		"prefix":            "vault/",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	payload := []byte("shard contents")
	if err := p.Put(ctx, "abc-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %s", gotMethod)
	}
	// Custom endpoints use path style, so the bucket and prefix are in the path.
	if gotPath != "/shards/vault/abc-p1.sand" {
		t.Errorf("path = %q, want /shards/vault/abc-p1.sand", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "SignedHeaders=") || !strings.Contains(gotAuth, "Signature=") {
		t.Errorf("Authorization header is missing SigV4 components: %q", gotAuth)
	}
	if len(gotSHA) != 64 {
		t.Errorf("X-Amz-Content-Sha256 = %q, want a hex sha256", gotSHA)
	}
	if !strings.HasSuffix(gotDate, "Z") || len(gotDate) != 16 {
		t.Errorf("X-Amz-Date = %q, want ISO8601 basic format", gotDate)
	}

	got, err := p.Get(ctx, "abc-p1.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	if _, err := p.Get(ctx, "missing-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on a missing key = %v, want ErrNotFound", err)
	}

	objects, err := p.List(ctx, "abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "abc-p1.sand" || objects[0].Size != 42 {
		t.Errorf("List = %+v", objects)
	}

	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestS3AmazonEndpointUsesVirtualHostStyle(t *testing.T) {
	p, err := New(Config{Kind: KindS3, Options: map[string]string{
		"bucket":            "my-bucket",
		"region":            "eu-west-2",
		"access_key_id":     "AKIA",
		"secret_access_key": "secret",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s3p := p.(*s3Provider)
	got := s3p.objectURL("x-p1.sand").String()
	want := "https://my-bucket.s3.eu-west-2.amazonaws.com/x-p1.sand"
	if got != want {
		t.Errorf("objectURL = %q, want %q", got, want)
	}
}

func TestCanonicalURIEncodesSegments(t *testing.T) {
	cases := map[string]string{
		"/simple/path":    "/simple/path",
		"/with space":     "/with%20space",
		"/tilde~and-dot.": "/tilde~and-dot.",
		"/plus+sign":      "/plus%2Bsign",
		"":                "/",
	}
	for in, want := range cases {
		if got := canonicalURI(in); got != want {
			t.Errorf("canonicalURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWebDAVRoundTrip drives the WebDAV backend against a minimal in-memory
// server implementing just the verbs SAND uses.
func TestWebDAVRoundTrip(t *testing.T) {
	stored := map[string][]byte{}
	var sawAuth bool

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok && user == "alice" && pass == "app-password" {
			sawAuth = true
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"></d:multistatus>`))
		case http.MethodPut:
			body := make([]byte, r.ContentLength)
			r.Body.Read(body)
			stored[r.URL.Path] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodHead, http.MethodGet:
			body, ok := stored[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "14")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Write(body)
		case http.MethodDelete:
			delete(stored, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer stub.Close()

	p, err := New(Config{Kind: KindWebDAV, Options: map[string]string{
		"url":      stub.URL + "/remote.php/dav/files/alice",
		"username": "alice",
		"password": "app-password",
		"prefix":   "sand",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !sawAuth {
		t.Error("credentials were never sent")
	}

	payload := []byte("shard contents")
	if err := p.Put(ctx, "abc-p2.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := stored["/remote.php/dav/files/alice/sand/abc-p2.sand"]; !ok {
		t.Errorf("stored under unexpected path; have %v", keysOf(stored))
	}

	got, err := p.Get(ctx, "abc-p2.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Get returned %q", got)
	}

	if _, err := p.Get(ctx, "missing.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on a missing key = %v, want ErrNotFound", err)
	}
	if err := p.Delete(ctx, "abc-p2.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestWebDAVPingReportsBadCredentials(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer stub.Close()

	p, _ := New(Config{Kind: KindWebDAV, Options: map[string]string{
		"url":      stub.URL,
		"username": "alice",
		"password": "wrong",
	}})

	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("Ping with bad credentials = %v, want a credentials error", err)
	}
}

func TestDropboxRequiresAToken(t *testing.T) {
	_, err := New(Config{Kind: KindDropbox, Options: map[string]string{"prefix": "sand"}})
	if err == nil || !strings.Contains(err.Error(), "refresh token") {
		t.Errorf("New without any token = %v, want a token error", err)
	}
}

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"/":        "",
		"sand":     "sand/",
		"/sand/":   "sand/",
		"  sand  ": "sand/",
		"a/b":      "a/b/",
	}
	for in, want := range cases {
		if got := normalizePrefix(in); got != want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeColor(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"   ":     "",
		"#38bdf8": "#38bdf8",
		"#38BDF8": "#38bdf8",
		"38bdf8":  "#38bdf8",
		" #abc ":  "#aabbcc",
		"#ABC":    "#aabbcc",
	}
	for in, want := range cases {
		got, err := NormalizeColor(in)
		if err != nil {
			t.Errorf("NormalizeColor(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeColor(%q) = %q, want %q", in, got, want)
		}
	}

	// A colour that is not one has to be refused rather than stored and drawn
	// as nothing: the browser paints this straight into a style.
	for _, bad := range []string{"chartreuse", "#12345", "#ggg", "rgb(1,2,3)", "#38bdf8;"} {
		if _, err := NormalizeColor(bad); err == nil {
			t.Errorf("NormalizeColor(%q) was accepted", bad)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCoversNameTheServicesBehindAProtocol pins the data the browser's
// catalogue is drawn from. Two backends are protocols rather than services,
// and "S3-compatible storage" is a true label that answers none of the
// questions people actually arrive with — whether their Wasabi bucket, their
// Seafile server or their Google Cloud Storage bucket can hold a part.
func TestCoversNameTheServicesBehindAProtocol(t *testing.T) {
	for _, kind := range []Kind{KindS3, KindWebDAV, KindLocal} {
		spec, ok := SpecFor(kind)
		if !ok {
			t.Fatalf("%s is not registered", kind)
		}
		if len(spec.Covers) == 0 {
			t.Errorf("%s names no services, so the catalogue has nothing to show", kind)
		}
		seen := map[string]bool{}
		for _, service := range spec.Covers {
			if service.Name == "" {
				t.Errorf("%s: a covered service with no name", kind)
			}
			if seen[service.Name] {
				t.Errorf("%s: %q listed twice", kind, service.Name)
			}
			seen[service.Name] = true
			// The endpoint or URL is the only thing that differs between
			// services on a shared protocol, so it is the one thing the entry
			// has to say.
			if kind != KindLocal && service.Hint == "" {
				t.Errorf("%s: %q says nothing about what to put in the endpoint", kind, service.Name)
			}
		}
	}

	// A few that are the whole point: reachable today, named nowhere else.
	s3, _ := SpecFor(KindS3)
	named := map[string]bool{}
	for _, service := range s3.Covers {
		named[service.Name] = true
	}
	for _, want := range []string{"Amazon S3", "Google Cloud Storage", "Wasabi", "Storj"} {
		if !named[want] {
			t.Errorf("the S3 backend does not admit to reaching %s", want)
		}
	}

	// A backend whose label is already the service needs no list.
	dropbox, _ := SpecFor(KindDropbox)
	if len(dropbox.Covers) != 0 {
		t.Errorf("Dropbox covers %+v — its label already says it", dropbox.Covers)
	}
}
