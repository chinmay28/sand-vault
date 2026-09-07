package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerseBodyReducesHTMLToItsTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"message":"nope"}`, `{"message":"nope"}`},
		{"plain text\n", "plain text"},
		{"<html>\r\n<head><title>500 Internal Server Error</title></head>\r\n<body><center><h1>500 Internal Server Error</h1></center><hr><center>nginx</center></body></html>",
			"500 Internal Server Error"},
		{"<p>No title <b>here</b></p>", "No title here"},
		{"", ""},
	} {
		if got := terseBody(tc.in); got != tc.want {
			t.Errorf("terseBody(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRetryStatusPicksTheTransientOnes(t *testing.T) {
	for code, want := range map[int]bool{
		200: false, 400: false, 401: false, 404: false, 409: false, 422: false,
		429: true, 500: true, 502: true, 503: true, 504: true, 509: true, 520: true, 524: true, 525: false,
	} {
		if got := retryStatus(code); got != want {
			t.Errorf("retryStatus(%d) = %v", code, got)
		}
	}
}

func TestDoWithRetryResendsTheBodyAndHonoursRetryAfter(t *testing.T) {
	restore := retryDelay
	retryDelay = time.Millisecond
	defer func() { retryDelay = restore }()

	var calls atomic.Int32
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer server.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, bytes.NewReader([]byte("payload")))
	started := time.Now()
	resp, retried, err := doWithRetry(context.Background(), req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("doWithRetry = %v, %v", resp, err)
	}
	drainAndClose(resp)
	if !retried || calls.Load() != 3 {
		t.Errorf("retried = %v after %d calls, want true after 3", retried, calls.Load())
	}
	for i, b := range bodies {
		if b != "payload" {
			t.Errorf("attempt %d received body %q; the body was not rewound", i+1, b)
		}
	}
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Errorf("Retry-After: 1 was not honoured, all attempts took %v", elapsed)
	}
}

func TestDoWithRetryStopsOnADefinitiveAnswerAndOnContext(t *testing.T) {
	restore := retryDelay
	retryDelay = time.Millisecond
	defer func() { retryDelay = restore }()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/forbidden") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/forbidden", nil)
	resp, retried, err := doWithRetry(context.Background(), req)
	if err != nil || resp.StatusCode != http.StatusForbidden || retried || calls.Load() != 1 {
		t.Errorf("a 403 was retried: %v %v retried=%v calls=%d", resp, err, retried, calls.Load())
	}
	drainAndClose(resp)

	calls.Store(0)
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/down", nil)
	resp, retried, err = doWithRetry(context.Background(), req)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || !retried || calls.Load() != int32(retryAttempts) {
		t.Errorf("a 503 was not retried to the limit: %v %v retried=%v calls=%d", resp, err, retried, calls.Load())
	}
	drainAndClose(resp)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls.Store(0)
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/down", nil)
	if _, _, err := doWithRetry(ctx, req); err == nil {
		t.Error("a cancelled context did not stop the retries")
	}
}
