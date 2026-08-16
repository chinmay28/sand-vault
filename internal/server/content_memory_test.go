package server

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// peakHeapDuring reports the most heap that was live at any moment while work
// ran, sampled rather than taken from a before/after pair.
//
// The collector is turned up for the duration. HeapAlloc counts garbage that has
// not been swept as well as memory genuinely held, and on an allocation-heavy
// path the two are indistinguishable at the default GOGC — which is how an
// earlier attempt at this measurement managed to report a streaming
// implementation and a buffering one as identical.
func peakHeapDuring(t *testing.T, work func()) uint64 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()

	stop, peaked := make(chan struct{}), make(chan uint64, 1)
	go func() {
		var stats runtime.MemStats
		var peak uint64
		for {
			select {
			case <-stop:
				peaked <- peak
				return
			default:
			}
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > peak {
				peak = stats.HeapAlloc
			}
		}
	}()

	work()
	close(stop)
	return <-peaked
}

// Serving a range must not cost the file.
//
// This is the failure that took a 16 GB Raspberry Pi down: the endpoint rebuilt
// the whole file to answer any request, so a <video> asking for a few hundred
// kilobytes of a 4 GB film allocated the 4 GB — several times over, once per
// request in flight. What the browser asks for is bounded; what it cost was not.
//
// The assertion is that cost does not scale with the file. A file four times
// larger must not cost anything like four times as much to serve one megabyte
// of, and the ceiling here is deliberately far below what buffering would need.
func TestServingARangeDoesNotScaleWithTheFile(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates hundreds of megabytes")
	}

	measure := func(size int) uint64 {
		c := newTestClient(t)
		c.setup("range-serving-passphrase", 3)

		// Incompressible, because a film is. A compressible fixture shrinks to
		// nothing in the parts and hides the very cost being measured.
		payload := make([]byte, size)
		rand.New(rand.NewSource(1)).Read(payload)
		file := c.upload("film.bin", "/", payload)
		payload = nil

		id := file["id"].(string)
		var peak uint64
		for attempt := 0; attempt < 2; attempt++ {
			peak = peakHeapDuring(t, func() {
				req := httptest.NewRequest(http.MethodGet, "/api/files/"+id+"/content", nil)
				req.Host = "example.test"
				req.Header.Set("Range", "bytes=0-1048575")
				for _, cookie := range c.cookies {
					req.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				c.handler.ServeHTTP(w, req)
				if w.Code != http.StatusPartialContent {
					t.Fatalf("ranged GET = %d, want 206: %s", w.Code, w.Body.String())
				}
				io.Copy(io.Discard, w.Body)
			})
		}
		return peak
	}

	const small, large = 32 << 20, 128 << 20
	smallPeak, largePeak := measure(small), measure(large)

	fmt.Printf("  serving 1 MB of a %d MB file peaked at %.1f MB live\n", small>>20, float64(smallPeak)/(1<<20))
	fmt.Printf("  serving 1 MB of a %d MB file peaked at %.1f MB live\n", large>>20, float64(largePeak)/(1<<20))

	// Buffering would put the file itself on the heap, so the larger file would
	// cost at least 96 MB more than the smaller one. Streaming costs the chunk
	// window either way; the growth allowed here is a wide margin around that.
	growth := int64(largePeak) - int64(smallPeak)
	if growth > (large-small)/4 {
		t.Errorf("a %d MB file cost %.1f MB more to serve one megabyte of than a %d MB file did; "+
			"the endpoint is still rebuilding the whole file",
			large>>20, float64(growth)/(1<<20), small>>20)
	}
	if largePeak > large {
		t.Errorf("serving one megabyte of a %d MB file held %.1f MB live — more than the file",
			large>>20, float64(largePeak)/(1<<20))
	}
}

// A file that cannot be read at an offset has to reach the browser as an offer
// to convert, not as a generic failure — the dialog is driven off this code.
//
// That a pre-chunking file produces this error is the vault's business and is
// asserted there; what belongs here is that the error survives the trip.
func TestNeedsConversionReachesTheBrowserAsAnOffer(t *testing.T) {
	w := httptest.NewRecorder()
	vaultErrorResponse(w, fmt.Errorf("/Videos/film.mov: %w", vault.ErrNeedsConversion))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 — the request is fine, the file's format is not", w.Code)
	}
	var body APIError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the error: %v", err)
	}
	if body.Code != "NEEDS_CONVERSION" {
		t.Errorf("code = %q, want NEEDS_CONVERSION so the browser can offer to convert", body.Code)
	}
	if !strings.Contains(body.Error, "film.mov") {
		t.Errorf("the message does not name the file: %q", body.Error)
	}
}
