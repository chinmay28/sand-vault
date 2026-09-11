package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// httpClient is shared by every network-backed provider. Shards are small and
// requests are independent, so a single pooled client is plenty.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

// drainAndClose consumes the remainder of a response body so the connection
// can be reused, then closes it.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// httpError builds an error carrying the status line and a bounded snippet of
// the response body, which is where every one of these APIs hides the reason
// a request was rejected.
func httpError(op string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := terseBody(string(snippet))
	if msg == "" || msg == resp.Status {
		return fmt.Errorf("%s: %s", op, resp.Status)
	}
	return fmt.Errorf("%s: %s: %s", op, resp.Status, msg)
}

var (
	htmlTitle = regexp.MustCompile(`(?is)<title>\s*(.*?)\s*</title>`)
	htmlTag   = regexp.MustCompile(`<[^>]*>`)
)

// terseBody turns a response body into something that can sit in an error
// message. A JSON or plain-text body is kept as it is. An HTML body — which
// is what a proxy in front of an API answers with when the API is not there
// — is reduced to its title, since a page of markup says nothing a status
// line did not.
func terseBody(body string) string {
	msg := strings.TrimSpace(body)
	if !strings.HasPrefix(msg, "<") {
		return msg
	}
	if m := htmlTitle.FindStringSubmatch(msg); m != nil {
		return strings.Join(strings.Fields(m[1]), " ")
	}
	return strings.Join(strings.Fields(htmlTag.ReplaceAllString(msg, " ")), " ")
}

// Retrying.
//
// A request that fails because the far end had a bad moment — a 502 from a
// proxy whose upstream was restarting, a 429 from a rate limiter, a reset
// connection — is not the same as one the service refused, and treating the
// two alike turns a moment's hiccup into a failed upload of a whole file. A
// few tries with a growing pause between them absorb the first kind without
// hiding the second.
const (
	retryAttempts = 5

	// retryBaseDelay is the first pause; each one after it doubles, so the
	// five attempts span about eight seconds. A variable so tests need not
	// wait through it.
	retryBaseDelay = 500 * time.Millisecond
)

var retryDelay = retryBaseDelay

// retryStatus reports whether a status is one a moment's wait may fix: the
// rate-limit answer, and the ones a proxy or an overloaded server gives
// rather than the API itself.
func retryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509:
		return true
	}
	return code >= 520 && code <= 524 // Cloudflare's family of "the origin did not answer"
}

// doWithRetry sends a request, trying again after a network error or a
// retryable status until it succeeds, gets a definitive answer, or runs out
// of attempts. The last response is returned whatever its status, so the
// caller reads the failure the same way it would without retries.
//
// The body is rewound through the request's GetBody between attempts, which
// http.NewRequest fills in for the bytes and strings readers every request
// here is built from; a request that cannot be rewound is sent once.
// Reports whether more than one attempt was made, since a caller that
// cannot tell whether the first try landed may have cleaning up to do.
func doWithRetry(ctx context.Context, req *http.Request) (resp *http.Response, retried bool, err error) {
	canRewind := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	for attempt := 0; ; attempt++ {
		resp, err = httpClient.Do(req)
		if err == nil && !retryStatus(resp.StatusCode) {
			return resp, retried, nil
		}
		if attempt == retryAttempts-1 || !canRewind {
			return resp, retried, err
		}

		wait := retryDelay << attempt
		if resp != nil {
			if after, ok := retryAfter(resp); ok {
				wait = after
			}
			drainAndClose(resp)
		}
		if req.GetBody != nil {
			body, rewindErr := req.GetBody()
			if rewindErr != nil {
				return resp, retried, err
			}
			req.Body = body
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
			return nil, retried, err
		case <-time.After(wait):
		}
		retried = true
	}
}

// retryAfter reads the pause a rate limiter asks for, when it names one.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 60 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 && wait <= time.Minute {
			return wait, true
		}
	}
	return 0, false
}

// isSuccess reports whether a status code is 2xx.
func isSuccess(code int) bool { return code >= 200 && code < 300 }

// MaxObjectSize caps how large a single downloaded shard may be. Shards are
// held in memory during reconstruction, so this is a guard against a hostile
// or misconfigured endpoint streaming until the process runs out of memory.
const MaxObjectSize = 2 << 30 // 2 GiB

// readAllBody reads a response body up to MaxObjectSize.
func readAllBody(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxObjectSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxObjectSize {
		return nil, fmt.Errorf("object exceeds maximum size of %d bytes", MaxObjectSize)
	}
	return data, nil
}
