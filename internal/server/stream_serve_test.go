package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// heldClient is a client that takes each write only when the test lets it.
//
// A player is not a recorder. It reads when its buffer has room and stops
// when it has not, and the handler waits on it; a film is mostly that wait.
// Every write the handler makes is offered here first and goes through only
// once it is taken, so a test can stand at any point of a stream — before a
// byte has gone, between two writes, a long pause into one — and look at
// what the server believes there.
type heldClient struct {
	header  http.Header
	offered chan int
	taken   chan struct{}

	mu   sync.Mutex
	body bytes.Buffer
}

func (h *heldClient) Header() http.Header { return h.header }
func (h *heldClient) WriteHeader(int)     {}

func (h *heldClient) Write(p []byte) (int, error) {
	h.offered <- len(p)
	<-h.taken
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.body.Write(p)
}

// received is what has gone out so far.
func (h *heldClient) received() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.body.Bytes()...)
}

// offer waits for the handler's next write and reports whether one came.
// False is the handler having returned instead.
func (h *heldClient) offer(t *testing.T, done <-chan struct{}) bool {
	t.Helper()
	select {
	case <-h.offered:
		return true
	case <-done:
		return false
	case <-time.After(10 * time.Second):
		t.Fatal("the handler neither wrote nor returned")
		return false
	}
}

// take lets the write on offer go through.
func (h *heldClient) take() { h.taken <- struct{}{} }

// drain takes everything the handler still has to give.
func (h *heldClient) drain(t *testing.T, done <-chan struct{}) {
	t.Helper()
	for h.offer(t, done) {
		h.take()
	}
}

// play serves a request to a held client, on a goroutine of its own the way
// a player's request runs beside everything else the server is doing. The
// channel closes when the handler returns.
func (c *testClient) play(req *http.Request) (*heldClient, <-chan struct{}) {
	c.t.Helper()
	client := &heldClient{header: http.Header{}, offered: make(chan int), taken: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.handler.ServeHTTP(client, req)
	}()
	// A test that fails part way leaves the handler holding a write out;
	// take what is left so it can return before the vault is put away.
	c.t.Cleanup(func() {
		for {
			select {
			case <-client.offered:
				client.taken <- struct{}{}
			case <-done:
				return
			case <-time.After(10 * time.Second):
				return
			}
		}
	})
	return client, done
}

// get is a GET the way the test client's own requests are made, session
// cookie and all, for a handler that is to be played rather than recorded.
func (c *testClient) get(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "example.test"
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	return req
}

// smallChunks has the next upload cut into many small chunks with nothing
// fetched ahead of the one being read, so a stream of it goes back to the
// accounts every write or two: what happens to its context part way through
// then shows in whether the rest of the film arrives.
func (c *testClient) smallChunks() {
	c.t.Helper()
	tune := c.server.vault.Tuning()
	tune.ChunkSize = 32 << 10
	tune.ReadAhead = 0
	c.server.vault.SetTuning(tune)
}

// filmOf is n bytes no two stretches of which look alike, so a stream that
// lost or repeated a piece cannot compare equal.
func filmOf(n int) []byte {
	film := make([]byte, n)
	state := uint32(2463534242)
	for i := range film {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		film[i] = byte(state)
	}
	return film
}

// stallAfter sets how long a stream may wait on the vault, for one test.
func stallAfter(t *testing.T, limit time.Duration) {
	t.Helper()
	was := streamStallLimit
	streamStallLimit = limit
	t.Cleanup(func() { streamStallLimit = was })
}

// upkeepEvery sets how often a stream does its upkeep, for one test. Zero is
// after every write, which is what lets a test step through one.
func upkeepEvery(t *testing.T, every time.Duration) {
	t.Helper()
	was := streamUpkeepEvery
	streamUpkeepEvery = every
	t.Cleanup(func() { streamUpkeepEvery = was })
}

// longAgo ages the server's note of outside use past the idle timeout: by
// the auto-lock's reckoning, nothing outside a browser has used the vault
// for a long time.
func (c *testClient) longAgo() {
	c.server.externalMu.Lock()
	c.server.externalActivity = time.Now().Add(-2 * DefaultIdleTimeout)
	c.server.externalMu.Unlock()
}

// A film is one request, and it is still being watched an hour after it
// began. Use was noted once, as the request started, so with no browser open
// beside it the idle timeout ran out mid-film and the auto-lock took the
// keys: the film stopped at the half-hour with its link voided. Bytes going
// out are use, for as long as they go.
func TestAStreamCountsAsUseForAsLongAsBytesGoOut(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	c.smallChunks()
	upkeepEvery(t, 0)

	film := filmOf(512 << 10)
	link := c.mintStream(c.upload("film.mkv", "/", film)["id"].(string))

	client, done := c.play(c.get(link))
	if !client.offer(t, done) {
		t.Fatal("the stream ended before it wrote anything")
	}

	// The request began long ago and nothing has gone out since.
	c.longAgo()
	if c.server.externalActive() {
		t.Fatal("a stream that has delivered nothing since the idle timeout ran out still counts as use")
	}

	// One write goes out, and that is somebody watching.
	client.take()
	if !client.offer(t, done) {
		t.Fatal("the stream ended after one write")
	}
	if !c.server.externalActive() {
		t.Error("bytes going out to a player did not count as use of the vault")
	}

	// A player left paused holds its request open and takes nothing. That is
	// not use, however long it lasts: a television left on pause must not be
	// what keeps a vault unlocked all night.
	c.longAgo()
	if c.server.externalActive() {
		t.Error("a player holding its request open and taking nothing kept the vault in use")
	}

	client.take()
	client.drain(t, done)
	if !bytes.Equal(client.received(), film) {
		t.Errorf("the stream delivered %d bytes that are not the film's %d", len(client.received()), len(film))
	}
}

// A stream used to be given thirty minutes in all, so that a hung provider
// could not pin its handler open forever. Every film is longer than that,
// and every film was cut at the half-hour. The limit is on the vault going
// quiet now: neither how long the film has been playing nor how long the
// viewer paused counts against it.
func TestAStreamIsNotCutOffForLastingOrForAPausedPlayer(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	c.smallChunks()
	const limit = 250 * time.Millisecond
	stallAfter(t, limit)

	film := filmOf(512 << 10)
	link := c.mintStream(c.upload("film.mkv", "/", film)["id"].(string))

	client, done := c.play(c.get(link))
	for i := 0; i < 2; i++ {
		if !client.offer(t, done) {
			t.Fatalf("the stream ended after %d writes", i)
		}
		client.take()
	}

	// The viewer pauses, for several times as long as the vault would be
	// allowed to keep the stream waiting, with a write held out all the
	// while. Most of the film is still on the accounts, so what follows has
	// to be fetched with whatever is left of the stream's context.
	if !client.offer(t, done) {
		t.Fatal("the stream ended before the pause")
	}
	time.Sleep(4 * limit)
	client.take()

	client.drain(t, done)
	if got := client.received(); !bytes.Equal(got, film) {
		t.Errorf("after a pause longer than the limit the stream delivered %d of the film's %d bytes",
			len(got), len(film))
	}
}

// The same limit, and the same exemption, for a file read through the app:
// a film played in the page and a large download over a slow link are as
// long as whoever is reading makes them.
func TestContentIsNotCutOffForASlowReader(t *testing.T) {
	c := newTestClient(t)
	c.setup("read-me-slowly", 3)
	c.smallChunks()
	const limit = 250 * time.Millisecond
	stallAfter(t, limit)

	film := filmOf(512 << 10)
	id := c.upload("film.mkv", "/", film)["id"].(string)

	client, done := c.play(c.get("/api/files/" + id + "/content"))
	if !client.offer(t, done) {
		t.Fatal("the read ended before it wrote anything")
	}
	time.Sleep(4 * limit)
	client.take()

	client.drain(t, done)
	if got := client.received(); !bytes.Equal(got, film) {
		t.Errorf("a slow reader was given %d of the file's %d bytes", len(got), len(film))
	}
}

// Revoking a code is meant to stop the film that is playing on it, and the
// link was only ever checked as a request began — which, for a player that
// makes one request a film, is before the film. The half-hour cut used to
// bound that by accident. With the cut gone the code is asked after as the
// film goes out.
func TestRevokingACodeStopsTheFilmAlreadyPlaying(t *testing.T) {
	c, _ := tvLibrary(t)
	c.smallChunks()
	upkeepEvery(t, 0)

	film := filmOf(512 << 10)
	id := c.upload("A.Long.Film.1999.mkv", "/films", film)["id"].(string)
	code := c.mintTVCode("/films", 60)
	token := c.pairTV(code)
	_, link := c.tvDo(http.MethodPost, "/api/tv/items/"+id+"/play", token, nil)

	client, done := c.play(c.get(link["url"].(string)))
	if !client.offer(t, done) {
		t.Fatal("the film ended before it wrote anything")
	}
	client.take()
	if !client.offer(t, done) {
		t.Fatal("the film ended after one write")
	}

	if w, _ := c.json(http.MethodDelete, "/api/tv/codes/"+code, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}

	// The write that was already on offer goes out; nothing after it does.
	client.take()
	if client.offer(t, done) {
		t.Fatal("the film went on being written after its code was revoked")
	}
	if got := len(client.received()); got == 0 || got >= len(film) {
		t.Errorf("a film whose code was revoked part way delivered %d of its %d bytes", got, len(film))
	}
}

// The clock is what stands where the half-hour deadline stood: it ends the
// stream's context when the vault has kept it waiting too long, and as a
// deadline passing rather than a cancellation, which is the difference a
// Local folder account reads between a disk that has stopped answering and a
// caller that has stopped waiting.
func TestStallClockRunsOutWhenTheVaultGivesNothing(t *testing.T) {
	ctx, clock := withStallLimit(context.Background(), 30*time.Millisecond)
	defer clock.stop()

	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the clock never ran out")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, errStreamStalled) {
		t.Errorf("cause = %v, want errStreamStalled", cause)
	}
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("cause = %v does not read as a deadline passing", cause)
	}
}

// Waiting on the client is not waiting on the vault. A paused viewer, or a
// player with a full buffer, can hold a write out for as long as it likes.
func TestStallClockDoesNotRunWhileTheClientIsWaitedOn(t *testing.T) {
	const limit = 50 * time.Millisecond
	ctx, clock := withStallLimit(context.Background(), limit)
	defer clock.stop()

	clock.pause()
	time.Sleep(4 * limit)
	if err := ctx.Err(); err != nil {
		t.Fatalf("the clock ran out while it was stopped: %v", context.Cause(ctx))
	}

	// And started again, it runs.
	clock.resume()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the clock never ran out once it was started again")
	}
}

// Each wait on the vault gets the whole limit. A film is thousands of short
// ones, and what they add up to is the length of the film.
func TestStallClockStartsAfreshAfterEveryWrite(t *testing.T) {
	const limit = 400 * time.Millisecond
	ctx, clock := withStallLimit(context.Background(), limit)
	defer clock.stop()

	for i := 0; i < 8; i++ {
		time.Sleep(limit / 4)
		clock.pause()
		clock.resume()
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("waits that came to twice the limit, none of them long, ran the clock out: %v", context.Cause(ctx))
	}
}

// The context ends with the handler, and with the request it was derived
// from, neither of which is the vault having stalled.
func TestStallClockEndsWithItsHandlerAndItsRequest(t *testing.T) {
	ctx, clock := withStallLimit(context.Background(), time.Hour)
	clock.stop()
	if ctx.Err() == nil {
		t.Error("stopping the clock left its context open")
	}
	if errors.Is(context.Cause(ctx), errStreamStalled) {
		t.Error("a handler that returned reads as a stream that stalled")
	}

	request, hangUp := context.WithCancel(context.Background())
	ctx, clock = withStallLimit(request, time.Hour)
	defer clock.stop()
	hangUp()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the client hanging up did not end the stream's context")
	}
	if errors.Is(context.Cause(ctx), errStreamStalled) {
		t.Error("a client that hung up reads as a stream that stalled")
	}
}

// Upkeep is done as bytes go out and no oftener than it was asked for: a
// lock clock touched once a second is touched often enough.
func TestStreamWriterDoesItsUpkeepEverySoOften(t *testing.T) {
	_, clock := withStallLimit(context.Background(), time.Hour)
	defer clock.stop()

	used := 0
	w := &streamWriter{
		ResponseWriter: httptest.NewRecorder(),
		clock:          clock,
		use:            func() { used++ },
		every:          time.Hour,
	}
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("some of a film")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if used != 1 {
		t.Errorf("five writes inside one interval noted use %d times, want once", used)
	}

	// Nothing written is nothing delivered.
	w.every = 0
	if _, err := w.Write(nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if used != 1 {
		t.Errorf("an empty write noted use")
	}
	if _, err := w.Write([]byte("more of it")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if used != 2 {
		t.Errorf("a write after the interval noted use %d times in all, want 2", used)
	}
}

// Once the link is no longer good nothing more is written, whatever the
// copy does next, and a stream on its way out is not use of the vault.
func TestStreamWriterStopsWhenItsLinkIsNoLongerGood(t *testing.T) {
	_, clock := withStallLimit(context.Background(), time.Hour)
	defer clock.stop()

	rec := httptest.NewRecorder()
	good := true
	used := 0
	w := &streamWriter{
		ResponseWriter: rec,
		clock:          clock,
		use:            func() { used++ },
		live:           func() bool { return good },
	}
	if _, err := w.Write([]byte("reel one ")); err != nil {
		t.Fatalf("Write on a good link: %v", err)
	}

	good = false
	if _, err := w.Write([]byte("reel two ")); !errors.Is(err, errStreamRevoked) {
		t.Fatalf("Write after the link went bad = %v, want errStreamRevoked", err)
	}
	if n, err := w.Write([]byte("reel three")); n != 0 || !errors.Is(err, errStreamRevoked) {
		t.Errorf("a later Write = %d, %v; want nothing written and errStreamRevoked", n, err)
	}
	if got := rec.Body.String(); got != "reel one reel two " {
		t.Errorf("wrote %q; the write in hand when the link went bad is the last", got)
	}
	if used != 1 {
		t.Errorf("use was noted %d times, want once: a revoked stream is nobody watching", used)
	}
}
