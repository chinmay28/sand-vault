package server

import (
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Where a file being opened has got to, for the browser waiting on it.
//
// The same bargain as the erase watch: a window onto a request that is
// running, and nothing more. The browser picks a token, puts it on the content
// request as ?watch=, and asks GET /api/reads/watch/{token} while it waits.
// What it hears is what the read path is doing right now — which accounts it
// is waiting on, that it is decrypting, how much has been sent — so a photo
// on a slow cloud is a sentence about the cloud rather than a dark box.
//
// Nothing here is written down and nothing outlives the read by more than a
// moment: a finished read lingers long enough for the last poll to see how it
// ended, and then goes.

// readPhase names the step a read is on.
type readPhase string

const (
	// readOpening: the request has arrived and the file is being looked up.
	readOpening readPhase = "opening"
	// readGathering: parts are being fetched from the accounts.
	readGathering readPhase = "gathering"
	// readDecrypting: enough parts are back; the chunk is being rebuilt.
	readDecrypting readPhase = "decrypting"
	// readSending: plaintext is flowing to the browser.
	readSending readPhase = "sending"
	// readDone: the response is complete.
	readDone readPhase = "done"
	// readFailed: the read stopped with an error, which Error carries.
	readFailed readPhase = "failed"
)

// readAccount is one account asked for a part of the chunk being gathered.
type readAccount struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Part       int    `json:"part"`
	// State is "waiting" until the account answers, then "arrived" or "failed".
	State string `json:"state"`
	// TookMS is how long the account took to answer, once it has.
	TookMS float64 `json:"took_ms,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// readProgress is what a poll of the window sees.
type readProgress struct {
	Phase readPhase `json:"phase"`

	// Chunk is the chunk being worked on, counting from 0, out of Chunks.
	Chunk  int `json:"chunk"`
	Chunks int `json:"chunks"`

	// Needed is how many parts rebuild a chunk; Have is how many of the
	// current chunk's are back.
	Needed int `json:"needed"`
	Have   int `json:"have"`

	// Accounts is every account asked for the current chunk, in part order.
	Accounts []readAccount `json:"accounts"`

	// Sent is how many bytes of plaintext have gone to the browser, out of
	// Size — the file's length, or the length of the range asked for.
	Sent int64 `json:"sent"`
	Size int64 `json:"size"`

	Error string `json:"error,omitempty"`

	// Updated is when the read last said anything.
	Updated time.Time `json:"updated"`
}

// readWatchTTL is how long a finished read stays answerable. The browser polls
// twice a second, so a few seconds is plenty for its last look.
const readWatchTTL = 30 * time.Second

// readWatchStaleAfter is how long a read that has gone quiet is kept: a
// browser that closed the tab mid-fetch leaves a ticket nobody will finish.
const readWatchStaleAfter = 10 * time.Minute

// readWatchLimit caps how many reads are watched at once. Past it the oldest
// is forgotten; a browser cannot fill memory by minting tokens.
const readWatchLimit = 256

// readTokenPattern is what a token may look like: something the browser
// generated, not something it typed.
var readTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// validReadToken reports whether a token is one the window will take.
func validReadToken(token string) bool {
	return readTokenPattern.MatchString(token)
}

type readWatch struct {
	mu      sync.Mutex
	byToken map[string]*readTicket
	now     func() time.Time
}

func newReadWatch() *readWatch {
	return &readWatch{byToken: map[string]*readTicket{}, now: time.Now}
}

// readTicket is one watched read. It is the vault's observer for that read,
// the counter for the bytes the handler writes, and the record a poll reads.
type readTicket struct {
	watch *readWatch
	token string

	mu       sync.Mutex
	progress readProgress
	// finished is when the read ended, zero while it is running.
	finished time.Time
}

// open starts watching a read under token, replacing any read the token
// named before — a player asks for a film in many range requests, and the
// window follows the latest.
func (rw *readWatch) open(token string) *readTicket {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	now := rw.now()
	rw.sweepLocked(now)

	ticket := &readTicket{watch: rw, token: token}
	ticket.progress = readProgress{Phase: readOpening, Updated: now}
	rw.byToken[token] = ticket
	return ticket
}

// get answers with where the read under token has got to.
func (rw *readWatch) get(token string) (readProgress, bool) {
	rw.mu.Lock()
	ticket, ok := rw.byToken[token]
	rw.mu.Unlock()
	if !ok {
		return readProgress{}, false
	}
	return ticket.snapshot(), true
}

// running counts the reads still in flight.
func (rw *readWatch) running() int {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	n := 0
	for _, ticket := range rw.byToken {
		if ticket.snapshotFinished().IsZero() {
			n++
		}
	}
	return n
}

// sweepLocked forgets reads nobody will ask about again: finished ones past
// their grace, quiet ones past theirs, and the oldest of too many.
func (rw *readWatch) sweepLocked(now time.Time) {
	for token, ticket := range rw.byToken {
		finished := ticket.snapshotFinished()
		updated := ticket.snapshot().Updated
		if (!finished.IsZero() && now.Sub(finished) > readWatchTTL) ||
			now.Sub(updated) > readWatchStaleAfter {
			delete(rw.byToken, token)
		}
	}
	if len(rw.byToken) < readWatchLimit {
		return
	}
	type aged struct {
		token   string
		updated time.Time
	}
	var all []aged
	for token, ticket := range rw.byToken {
		all = append(all, aged{token, ticket.snapshot().Updated})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].updated.Before(all[j].updated) })
	for _, a := range all[:len(all)-readWatchLimit+1] {
		delete(rw.byToken, a.token)
	}
}

func (t *readTicket) snapshot() readProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.progress
	out.Accounts = append([]readAccount(nil), t.progress.Accounts...)
	return out
}

func (t *readTicket) snapshotFinished() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// update applies one change under the lock and stamps it.
func (t *readTicket) update(change func(p *readProgress)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished.IsZero() {
		// A read that has ended has nothing more to say; a late shard
		// answer or a trailing write must not reopen it.
		return
	}
	change(&t.progress)
	t.progress.Updated = t.watch.now()
}

// opened records that the file was found and how big it is.
func (t *readTicket) opened(size int64) {
	t.update(func(p *readProgress) { p.Size = size })
}

// ObserveRead is the vault's side of the window. See vault.ReadEvent.
func (t *readTicket) ObserveRead(ev vault.ReadEvent) {
	t.update(func(p *readProgress) {
		p.Chunk, p.Chunks, p.Needed = ev.Chunk, ev.Chunks, ev.Needed
		switch ev.Kind {
		case vault.ReadChunkWaiting:
			p.Phase = readGathering
			p.Have = 0
			p.Accounts = nil
		case vault.ReadChunkStarted:
			p.Phase = readGathering
			p.Have = 0
			p.Accounts = make([]readAccount, 0, len(ev.Asked))
			for _, shard := range ev.Asked {
				p.Accounts = append(p.Accounts, readAccount{
					ProviderID: shard.ProviderID,
					Name:       shard.ProviderName,
					Kind:       shard.ProviderKind,
					Part:       shard.Part,
					State:      "waiting",
				})
			}
		case vault.ReadShardArrived, vault.ReadShardFailed:
			state, errText := "arrived", ""
			if ev.Kind == vault.ReadShardFailed {
				state = "failed"
				if ev.Err != nil {
					errText = ev.Err.Error()
				}
			} else {
				p.Have++
			}
			for i := range p.Accounts {
				if p.Accounts[i].Part == ev.Shard.Part && p.Accounts[i].ProviderID == ev.Shard.ProviderID {
					p.Accounts[i].State = state
					p.Accounts[i].TookMS = float64(ev.Took) / float64(time.Millisecond)
					p.Accounts[i].Error = errText
				}
			}
		case vault.ReadChunkDecrypting:
			p.Phase = readDecrypting
		case vault.ReadChunkReady:
			// Sending follows immediately: the bytes are in hand and the
			// handler's next write is what moves them.
			p.Phase = readSending
		case vault.ReadChunkFailed:
			p.Phase = readFailed
			if ev.Err != nil {
				p.Error = ev.Err.Error()
			}
		}
	})
}

// sent counts bytes written to the browser.
func (t *readTicket) sent(n int) {
	if n <= 0 {
		return
	}
	t.update(func(p *readProgress) {
		p.Phase = readSending
		p.Sent += int64(n)
	})
}

// finish closes the window on the read: done, or failed with err.
func (t *readTicket) finish(err error) {
	t.update(func(p *readProgress) {
		if err != nil {
			p.Phase = readFailed
			if p.Error == "" {
				p.Error = err.Error()
			}
		} else if p.Phase != readFailed {
			p.Phase = readDone
		}
	})
	t.mu.Lock()
	if t.finished.IsZero() {
		t.finished = t.watch.now()
	}
	t.mu.Unlock()
}

// countingWriter counts what a handler writes, for the ticket. It carries
// only Write and Header/WriteHeader on purpose: http.ServeContent needs
// nothing else, and a wrapper that hid Flush would only matter for a body
// that is streamed rather than served.
type countingWriter struct {
	http.ResponseWriter
	ticket *readTicket
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.ticket.sent(n)
	return n, err
}

// Flush passes a flush through where the underlying writer has one, so a
// watched response is streamed exactly the way an unwatched one is.
func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
