package provider

import (
	"fmt"
	"io"
	"net/http"
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
	msg := strings.TrimSpace(string(snippet))
	if msg == "" {
		return fmt.Errorf("%s: %s", op, resp.Status)
	}
	return fmt.Errorf("%s: %s: %s", op, resp.Status, msg)
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
