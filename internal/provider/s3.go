package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func init() {
	Register(Spec{
		Kind:  KindS3,
		Label: "S3-compatible storage",
		Description: "Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO, or any other " +
			"service speaking the S3 API. Leave the endpoint blank for Amazon S3.",
		Fields: []FieldSpec{
			{Key: "bucket", Label: "Bucket", Placeholder: "my-sand-shards", Required: true},
			{Key: "region", Label: "Region", Placeholder: "us-east-1", Default: "us-east-1", Required: true},
			{Key: "access_key_id", Label: "Access key ID", Required: true},
			{Key: "secret_access_key", Label: "Secret access key", Secret: true, Required: true},
			{
				Key:         "endpoint",
				Label:       "Endpoint URL",
				Placeholder: "https://<account>.r2.cloudflarestorage.com",
				Help:        "Only for non-Amazon services. Blank means Amazon S3.",
			},
			{
				Key:   "prefix",
				Label: "Key prefix",
				Help:  "Optional folder inside the bucket, e.g. sand/.",
			},
		},
	}, newS3Provider)
}

// s3Provider talks to any S3-compatible endpoint using SigV4 request signing.
// It deliberately depends on nothing but the standard library.
type s3Provider struct {
	base
	bucket    string
	region    string
	accessKey string
	secretKey string
	prefix    string

	// endpoint is the scheme://host the requests go to, and pathStyle decides
	// whether the bucket is part of the host or the first path segment.
	endpoint  *url.URL
	pathStyle bool
}

func newS3Provider(cfg Config) (Provider, error) {
	p := &s3Provider{
		base:      base{cfg: cfg},
		bucket:    cfg.Option("bucket"),
		region:    cfg.Option("region"),
		accessKey: cfg.Option("access_key_id"),
		secretKey: cfg.Option("secret_access_key"),
		prefix:    normalizePrefix(cfg.Option("prefix")),
	}

	raw := strings.TrimSpace(cfg.Option("endpoint"))
	if raw == "" {
		// Amazon S3: virtual-hosted style against the regional endpoint.
		p.endpoint = &url.URL{Scheme: "https", Host: fmt.Sprintf("s3.%s.amazonaws.com", p.region)}
		p.pathStyle = false
	} else {
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid endpoint URL: %w", err)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("invalid endpoint URL %q", raw)
		}
		p.endpoint = &url.URL{Scheme: u.Scheme, Host: u.Host}
		// Custom endpoints are overwhelmingly path-style (R2, MinIO, B2).
		p.pathStyle = true
	}

	if v := strings.TrimSpace(cfg.Option("path_style")); v != "" {
		p.pathStyle = v == "true" || v == "1" || v == "yes"
	}

	return p, nil
}

// normalizePrefix trims stray slashes and guarantees a single trailing one.
func normalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// objectURL builds the request URL for an object key.
func (p *s3Provider) objectURL(key string) *url.URL {
	u := &url.URL{Scheme: p.endpoint.Scheme, Host: p.endpoint.Host}
	if p.pathStyle {
		u.Path = "/" + p.bucket + "/" + p.prefix + key
	} else {
		u.Host = p.bucket + "." + p.endpoint.Host
		u.Path = "/" + p.prefix + key
	}
	return u
}

// bucketURL builds the request URL for a bucket-level operation.
func (p *s3Provider) bucketURL() *url.URL {
	u := &url.URL{Scheme: p.endpoint.Scheme, Host: p.endpoint.Host}
	if p.pathStyle {
		u.Path = "/" + p.bucket
	} else {
		u.Host = p.bucket + "." + p.endpoint.Host
		u.Path = "/"
	}
	return u
}

func (p *s3Provider) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(reader.Len())

	if err := p.sign(req, body); err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

func (p *s3Provider) Put(ctx context.Context, key string, data []byte) error {
	resp, err := p.do(ctx, http.MethodPut, p.objectURL(key), data)
	if err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("s3 put", resp)
	}
	return nil
}

func (p *s3Provider) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := p.do(ctx, http.MethodGet, p.objectURL(key), nil)
	if err != nil {
		return nil, fmt.Errorf("s3 get: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("s3 get", resp)
	}
	return readAllBody(resp)
}

func (p *s3Provider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	resp, err := p.do(ctx, http.MethodHead, p.objectURL(key), nil)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("s3 stat: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return ObjectInfo{}, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return ObjectInfo{}, httpError("s3 stat", resp)
	}
	return ObjectInfo{Key: key, Size: resp.ContentLength}, nil
}

func (p *s3Provider) Delete(ctx context.Context, key string) error {
	resp, err := p.do(ctx, http.MethodDelete, p.objectURL(key), nil)
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if !isSuccess(resp.StatusCode) {
		return httpError("s3 delete", resp)
	}
	return nil
}

// s3ListResult mirrors the subset of ListObjectsV2 output SAND needs.
type s3ListResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

func (p *s3Provider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	token := ""

	for {
		u := p.bucketURL()
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", p.prefix+prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		u.RawQuery = q.Encode()

		resp, err := p.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("s3 list: %w", err)
		}
		if !isSuccess(resp.StatusCode) {
			err := httpError("s3 list", resp)
			drainAndClose(resp)
			return nil, err
		}
		body, err := readAllBody(resp)
		drainAndClose(resp)
		if err != nil {
			return nil, fmt.Errorf("s3 list: %w", err)
		}

		var result s3ListResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("s3 list: parsing response: %w", err)
		}
		for _, item := range result.Contents {
			out = append(out, ObjectInfo{
				Key:  strings.TrimPrefix(item.Key, p.prefix),
				Size: item.Size,
			})
		}

		if !result.IsTruncated || result.NextContinuationToken == "" {
			return out, nil
		}
		token = result.NextContinuationToken
	}
}

func (p *s3Provider) Ping(ctx context.Context) error {
	// A zero-key list is the cheapest call that proves both that the bucket
	// exists and that the credentials are accepted.
	u := p.bucketURL()
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", "1")
	u.RawQuery = q.Encode()

	resp, err := p.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("cannot reach S3 endpoint: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError("s3 bucket check", resp)
	}
	return nil
}

// ---------------------------------------------------------------------------
// AWS Signature Version 4
// ---------------------------------------------------------------------------

const s3Service = "s3"

// sign adds the SigV4 authorization headers to req in place.
func (p *s3Provider) sign(req *http.Request, payload []byte) error {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := hex.EncodeToString(sha256sum(payload))

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders, signedHeaders := canonicalizeHeaders(req)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, p.region, s3Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256sum([]byte(canonicalRequest))),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+p.secretKey), dateStamp)
	key = hmacSHA256(key, p.region)
	key = hmacSHA256(key, s3Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.accessKey, scope, signedHeaders, signature))

	return nil
}

// canonicalizeHeaders returns the canonical header block and the semicolon
// separated list of signed header names.
func canonicalizeHeaders(req *http.Request) (string, string) {
	names := make([]string, 0, len(req.Header)+1)
	values := make(map[string]string, len(req.Header)+1)

	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		trimmed := make([]string, len(vals))
		for i, v := range vals {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		names = append(names, lower)
		values[lower] = strings.Join(trimmed, ",")
	}
	// Go strips Host from Header into req.Host, so add it back explicitly.
	if _, ok := values["host"]; !ok {
		names = append(names, "host")
		values["host"] = req.URL.Host
	}

	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(values[n])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalURI percent-encodes each path segment per SigV4 rules. S3 keys are
// already single-encoded in the URL, so each segment is encoded exactly once.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = uriEncode(s, false)
	}
	joined := strings.Join(segments, "/")
	if !strings.HasPrefix(joined, "/") {
		joined = "/" + joined
	}
	return joined
}

// canonicalQuery renders the query string in the sorted, encoded form SigV4
// requires.
func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vals := append([]string(nil), values[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode implements the AWS variant of percent-encoding: unreserved
// characters pass through, everything else is encoded uppercase-hex, and '/'
// is only encoded outside of paths.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sha256sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
