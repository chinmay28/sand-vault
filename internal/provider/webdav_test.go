package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// webdavListing stands up a server whose PROPFIND answers with the given
// multistatus body, and a backend configured at /remote.php/dav/files/alice
// with the folder "sand" — the Nextcloud shape.
func webdavListing(t *testing.T, multistatus string) (Provider, *[]string) {
	t.Helper()
	var requests []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			w.Write([]byte(multistatus))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.Close)

	p, err := New(Config{Kind: KindWebDAV, Options: map[string]string{
		"url":      stub.URL + "/remote.php/dav/files/alice",
		"username": "alice",
		"password": "app-password",
		"prefix":   "sand",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, &requests
}

// The hrefs in a listing are the server's spelling, not the client's: a proxy
// that drops /remote.php, a canonical user id in another case, a folder shown
// in the casing it was made with, a full URL rather than a path, a
// percent-encoded name. Trimming the configured path off any of those trims
// nothing and the account lists as empty. The listed collection's own
// response says what the base is, and properties are read from whichever
// propstat says 200 — not the first, which is the 404 group when the server
// has one.
func TestWebDAVListReadsTheServersOwnSpelling(t *testing.T) {
	p, _ := webdavListing(t, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/dav/files/ALICE/Sand/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/files/ALICE/Sand/abc-p1.sand</d:href>
    <d:propstat>
      <d:prop><d:getcontentlength/><d:resourcetype/></d:prop>
      <d:status>HTTP/1.1 404 Not Found</d:status>
    </d:propstat>
    <d:propstat>
      <d:prop><d:getcontentlength>14</d:getcontentlength><d:resourcetype/></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>http://cloud.example.test/dav/files/ALICE/Sand/abc%2Dp2.sand</d:href>
    <d:propstat>
      <d:status>HTTP/1.1 200 OK</d:status>
      <d:prop><d:getcontentlength>7</d:getcontentlength><d:resourcetype/></d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`)

	listed, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("List = %+v, want two parts", listed)
	}
	if listed[0].Key != "abc-p1.sand" || listed[0].Size != 14 {
		t.Errorf("first part = %+v, want abc-p1.sand of 14 bytes from the 200 propstat", listed[0])
	}
	if listed[1].Key != "abc-p2.sand" || listed[1].Size != 7 {
		t.Errorf("second part = %+v, want abc-p2.sand decoded from its URL", listed[1])
	}
}

// A server that includes no entry for the collection itself still lists,
// against the configured path, case aside.
func TestWebDAVListFallsBackToTheConfiguredPath(t *testing.T) {
	p, _ := webdavListing(t, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/files/Alice/SAND/abc-p1.sand</d:href>
    <d:propstat>
      <d:prop><d:getcontentlength>3</d:getcontentlength><d:resourcetype/></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`)

	listed, err := p.List(context.Background(), "abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "abc-p1.sand" || listed[0].Size != 3 {
		t.Errorf("List = %+v, want abc-p1.sand", listed)
	}
}

// An entry that cannot be related to the folder at all is an error, not a
// skipped line: a listing is complete or it fails, because the orphan scan
// deletes what a listing does not name.
func TestWebDAVListRefusesAnEntryItCannotPlace(t *testing.T) {
	p, _ := webdavListing(t, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/somewhere/else/abc-p1.sand</d:href>
    <d:propstat>
      <d:prop><d:getcontentlength>3</d:getcontentlength><d:resourcetype/></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`)

	if listed, err := p.List(context.Background(), ""); err == nil {
		t.Errorf("List = %+v, nil; want an error for an href outside the folder", listed)
	}
}

// A response whose only propstat is a failure has no properties to read, and
// a resource with no readable properties is left out rather than listed as a
// zero-byte part.
func TestWebDAVListSkipsAResourceWithNoFoundProperties(t *testing.T) {
	p, _ := webdavListing(t, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/files/alice/sand/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/sand/abc-p1.sand</d:href>
    <d:propstat>
      <d:prop><d:getcontentlength/><d:resourcetype/></d:prop>
      <d:status>HTTP/1.1 403 Forbidden</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`)

	listed, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("List = %+v, want nothing from a resource whose properties were refused", listed)
	}
}

func TestDavPathDecodesWhatServersSend(t *testing.T) {
	cases := map[string]string{
		"/dav/files/alice/sand/abc-p1.sand":            "/dav/files/alice/sand/abc-p1.sand",
		"/dav/files/alice/sand/with%20space.sand":      "/dav/files/alice/sand/with space.sand",
		"https://cloud.example.test/dav/a%2Db.sand":    "/dav/a-b.sand",
		" /dav/files/alice/sand/trimmed.sand ":         "/dav/files/alice/sand/trimmed.sand",
		"https://cloud.example.test/dav/files/alice/":  "/dav/files/alice/",
		"/dav/files/alice/sand/%zz-not-an-escape.sand": "/dav/files/alice/sand/%zz-not-an-escape.sand",
	}
	for in, want := range cases {
		if got := davPath(in); got != want {
			t.Errorf("davPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The empty key is the prefix collection, and a DELETE of a collection takes
// everything in it. It is refused before any request is made.
func TestWebDAVRefusesKeysThatNameTheFolder(t *testing.T) {
	p, requests := webdavListing(t, `<d:multistatus xmlns:d="DAV:"></d:multistatus>`)
	ctx := context.Background()

	for _, key := range []string{"", ".", "..", "../abc-p1.sand", "sub/abc-p1.sand"} {
		if err := p.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) was allowed", key)
		}
		if _, err := p.Get(ctx, key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want a refusal that is not ErrNotFound", key, err)
		}
		if _, err := p.Stat(ctx, key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want a refusal that is not ErrNotFound", key, err)
		}
		if err := p.Put(ctx, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) was allowed", key)
		}
	}
	for _, r := range *requests {
		if strings.HasPrefix(r, "DELETE ") {
			t.Errorf("a refused key still reached the server: %s", r)
		}
	}
}
