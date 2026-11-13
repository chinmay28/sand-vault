package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Keeping a stream honest for as long as it runs.
//
// A film is one request. A player opens the address once and reads it for two
// hours, pausing whenever its own buffer is full, so whatever was settled
// when the request began is, for a film, settled two hours early. Three
// things were settled there, and each went wrong in its own way:
//
//   - The vault was noted as in use once, at the first byte. With no browser
//     open beside it, the idle timeout ran out while the film was still
//     playing, the auto-lock took the keys, and the film stopped half an hour
//     in with its link voided.
//   - The request was given thirty minutes, so that a hung provider could not
//     pin a handler open forever. A film is longer than that: every one was
//     cut at the half-hour and left to its player to reconnect.
//   - A link minted under a TV code was checked against the code once.
//     Revoking the code refused the next request and did nothing about the
//     one already playing.
//
// What is here keeps all three up as the bytes go out. The deadline becomes
// a clock that runs only while the handler is waiting on the vault — which is
// the one wait a hung provider can cause, and the only one that says anything
// is wrong. Use is noted, and the link's standing asked again, every so often
// for as long as something is actually being delivered.
//
// "Being delivered" is the test on purpose, rather than "being connected". A
// player left paused holds its request open and takes nothing, and a vault
// that stayed unlocked for as long as that lasted would be one a television
// left on pause keeps open all night. Nothing going out is nobody watching:
// the idle timeout runs, the vault locks, and the player is told so when it
// next asks for more.

// streamStallLimit is how long a stream may wait on the vault for its next
// bytes before it is given up on.
//
// It is the thirty minutes the whole request used to be allowed, measured
// from the last thing the vault handed over rather than from the first. A
// variable so a test need not wait half an hour.
var streamStallLimit = 30 * time.Minute

// streamUpkeepEvery is how often a stream that is delivering bytes notes
// that the vault is in use and asks whether its link may still play. The
// auto-lock looks once a minute and a code is revoked by hand, so a second
// is prompt for both; a variable so a test can have every write do it.
var streamUpkeepEvery = time.Second

// errStreamStalled is why a stream's context ended when the vault kept it
// waiting past streamStallLimit.
//
// It is a deadline passing, and says so, because that is what it is to
// everything underneath. A deadline that moves cannot be the context's own —
// those are fixed when the context is made — so the clock cancels and gives
// this as the cause, and a Local folder account, which tells a disk that has
// stopped answering from a caller that has stopped waiting by exactly that
// difference, reads the cause (provider.ErrDiskNotAnswering).
var errStreamStalled = fmt.Errorf("the vault gave this stream nothing for too long: %w", context.DeadlineExceeded)

// errStreamRevoked is what a write comes back with once the link a stream is
// following has stopped being good, which is what ends the copy.
var errStreamRevoked = errors.New("the link this stream was following is no longer good")

// stallClock is a deadline that runs only while a handler is waiting on the
// vault.
//
// Serving a stored file alternates between two waits: on the vault, for the
// next chunk, and on the client, to take what it has been given. Only the
// first can mean something is stuck. The second is a viewer who has paused,
// or a player with a full buffer, and it can last as long as it likes — so
// the clock is stopped for it and started afresh after it.
type stallClock struct {
	limit  time.Duration
	timer  *time.Timer
	cancel context.CancelCauseFunc
}

// withStallLimit derives a context that ends, with errStreamStalled as its
// cause, once the vault has kept the handler waiting for limit at a stretch.
// The clock is running when this returns: opening the file and fetching its
// first chunk are waits on the vault like any other.
func withStallLimit(parent context.Context, limit time.Duration) (context.Context, *stallClock) {
	ctx, cancel := context.WithCancelCause(parent)
	c := &stallClock{limit: limit, cancel: cancel}
	c.timer = time.AfterFunc(limit, func() { cancel(errStreamStalled) })
	return ctx, c
}

// pause stops the clock: the handler is waiting on the client now.
func (c *stallClock) pause() { c.timer.Stop() }

// resume starts the clock again from nothing: the client has taken what it
// was given, and the handler is back to reading.
func (c *stallClock) resume() { c.timer.Reset(c.limit) }

// stop ends the clock and releases its context, for a handler's defer.
func (c *stallClock) stop() {
	c.timer.Stop()
	c.cancel(context.Canceled)
}

// streamWriter is the response a stored file is copied into.
//
// Every byte of the body passes through Write, which makes it the one place
// that knows when the handler is waiting on the client and when something
// has actually gone out. It stops the stall clock around the first and does
// the stream's upkeep after the second.
//
// It carries Write and Header/WriteHeader and nothing else that would let
// the copy go round it: a writer that offered ReadFrom would be handed the
// whole body in one call, and nothing here would see it leave.
type streamWriter struct {
	http.ResponseWriter
	clock *stallClock

	// use notes that the vault is in use, or is nil where something else
	// already holds it — a browser's session is held for the length of its
	// request.
	use func()
	// live reports whether the link being followed may still play, or is nil
	// for a link nothing can revoke.
	live func() bool

	// every is how long passes between upkeep while bytes are going out, and
	// last when it was done.
	every time.Duration
	last  time.Time

	// revoked is set once live has said no, so that nothing more is written
	// whatever the copy does next.
	revoked bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.revoked {
		return 0, errStreamRevoked
	}

	w.clock.pause()
	n, err := w.ResponseWriter.Write(p)
	w.clock.resume()

	if n > 0 && (w.use != nil || w.live != nil) {
		if now := time.Now(); now.Sub(w.last) >= w.every {
			w.last = now
			if w.live != nil && !w.live() {
				w.revoked = true
				return n, errStreamRevoked
			}
			if w.use != nil {
				w.use()
			}
		}
	}
	return n, err
}

// Flush passes a flush through where the underlying writer has one, as
// countingWriter does and for the same reason.
func (w *streamWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
