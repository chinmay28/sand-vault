package vault

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Which cloud answers a read, and how quickly.
//
// Every read is a race. gather asks all n accounts holding a file for their
// shard and rebuilds from the first k distinct ones to arrive, cutting the
// rest off mid-flight (transfer.go, and gatherChunk in chunked.go for a
// chunked file). That is what makes a wide code quick to read as well as hard
// to lose — 4-of-6 reads from whichever four clouds are quickest today.
//
// It also means the vault runs a timed comparison of every account against
// every other one, hundreds of times over a single film, and has until now
// thrown the result away. This keeps it: how many races each account was
// entered into, how many it won — a win being an answer that arrived in time
// to be part of the rebuild — and what it did when it did not win.
//
// The point of keeping it is that the race hides a slow account right up until
// the moment it becomes a failure. Nothing downloads any slower for one cloud
// falling behind; it simply stops contributing, and the other five carry the
// vault. An account winning none of the races it enters is holding shards
// nobody has been able to use in weeks, which is worth knowing before the day
// two of the others go offline and it is suddenly load-bearing.
//
// The figures are this process's own. Nothing here is written to the vault
// file: a read would otherwise mean a write — on every chunk of every stream —
// to the one file everything else depends on, and a counter is not worth that.
// Since says when counting started, and Reset starts it again.

// shardFetch is one account's answer in a read's race: which shard was asked
// for, what came back, and how long the asking took.
//
// Shared by both race sites rather than declared inside each, so that what is
// recorded about a whole-file read and about one chunk of a chunked one cannot
// drift apart.
type shardFetch struct {
	shard Shard
	blob  []byte
	took  time.Duration
	err   error
}

// readOutcome is what became of one account's answer.
type readOutcome int

const (
	// shardWon: it arrived while the rebuild still wanted that shard, and was
	// used. This is the figure the whole panel is about.
	shardWon readOutcome = iota
	// shardLate: the account answered, but the rebuild was already served —
	// either k shards had arrived first, or another account holds the same
	// shard number and got there first.
	shardLate
	// shardAborted: cut off in flight because enough shards had arrived. Not a
	// fault: it is the read path doing what it is designed to do. Counted
	// apart from failures so that a cloud which is merely slower than four
	// others is never mistaken for one that is breaking.
	shardAborted
	// shardFailed: the account could not answer at all.
	shardFailed
)

// readCounter is one account's running tally. Durations are kept as durations
// and rounded to milliseconds only when the report is built.
type readCounter struct {
	name string
	kind string

	fetches  int64
	wins     int64
	late     int64
	aborted  int64
	failures int64

	// answers is wins + late: the fetches that finished, which are the only
	// ones a duration means anything for. An aborted fetch was stopped by us
	// and a failed one never arrived, so folding either into an average would
	// make a cloud look fast for having been given up on.
	answers int64
	bytes   int64
	total   time.Duration
	fastest time.Duration
	slowest time.Duration

	lastError   string
	lastErrorAt time.Time
	lastAnswer  time.Time
}

type readStats struct {
	mu         sync.Mutex
	since      time.Time
	races      int64
	shortfalls int64
	accounts   map[string]*readCounter

	// tail tracks the goroutines still recording what the losers of a decided
	// race eventually did. Nothing waits on it in the app — the reader is long
	// gone by then — but a test that asserts on the losing side has to.
	tail sync.WaitGroup
}

func newReadStats() *readStats {
	return &readStats{since: time.Now(), accounts: map[string]*readCounter{}}
}

// Every method is safe on a nil receiver, so a Vault assembled field by field
// — which the package's own tests do — reads and writes without a recorder
// rather than panicking on one it never built.

// race notes that a read has begun. One chunk of a chunked file is one race,
// because one chunk is what the accounts are actually racing over.
func (s *readStats) race() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.races++
	s.mu.Unlock()
}

// shortfall notes a race that could not find k shards, which is a read that
// failed rather than one that was merely slow.
func (s *readStats) shortfall() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.shortfalls++
	s.mu.Unlock()
}

func (s *readStats) record(f shardFetch, out readOutcome) {
	if s == nil || f.shard.ProviderID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.accounts[f.shard.ProviderID]
	if c == nil {
		c = &readCounter{}
		s.accounts[f.shard.ProviderID] = c
	}
	// Named off the shard rather than off the account list, so a row survives
	// the account being disconnected: what a cloud was doing before somebody
	// removed it is exactly the thing they may be about to want to explain.
	if f.shard.ProviderName != "" {
		c.name = f.shard.ProviderName
	}
	if f.shard.ProviderKind != "" {
		c.kind = f.shard.ProviderKind
	}

	c.fetches++
	switch out {
	case shardWon:
		c.wins++
	case shardLate:
		c.late++
	case shardAborted:
		c.aborted++
	case shardFailed:
		c.failures++
		if f.err != nil {
			c.lastError = f.err.Error()
			c.lastErrorAt = time.Now()
		}
	}

	if out == shardWon || out == shardLate {
		c.answers++
		c.bytes += int64(len(f.blob))
		c.total += f.took
		if c.fastest == 0 || f.took < c.fastest {
			c.fastest = f.took
		}
		if f.took > c.slowest {
			c.slowest = f.took
		}
		c.lastAnswer = time.Now()
	}
}

// drainLater records what the accounts still in flight when the race was
// decided eventually did.
//
// The winners are recorded by the reader itself, in the loop that picks them,
// so a read's own thread only ever pays for the shards it used. The losers are
// nobody's hurry: the channel they answer on is buffered wide enough for all
// of them, so this reads whatever is left at whatever speed it arrives and
// then goes away. n is how many answers have not been taken yet.
func (s *readStats) drainLater(results <-chan shardFetch, n int) {
	if s == nil || n <= 0 {
		return
	}
	s.tail.Add(1)
	go func() {
		defer s.tail.Done()
		for i := 0; i < n; i++ {
			f := <-results
			s.record(f, lostOutcome(f))
		}
	}()
}

// waitForTail blocks until every straggler from every decided race has been
// recorded. For tests; the app has no reason to wait for a figure.
func (s *readStats) waitForTail() {
	if s == nil {
		return
	}
	s.tail.Wait()
}

// lostOutcome reads an answer that did not win: one that came back after the
// race was decided, and one that came back as an error while it was still
// open.
//
// A cancellation is the read path's own doing — cancel() fires the moment k
// shards are in hand — so it is an abort and not a fault. Anything else is the
// account genuinely failing to answer, and is counted as such whether or not
// anybody was still waiting for it.
func lostOutcome(f shardFetch) readOutcome {
	switch {
	case f.err == nil:
		return shardLate
	case errors.Is(f.err, context.Canceled):
		return shardAborted
	default:
		return shardFailed
	}
}

func (s *readStats) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.since = time.Now()
	s.races = 0
	s.shortfalls = 0
	s.accounts = map[string]*readCounter{}
}

// ReadStat is one account's standing in the read race.
type ReadStat struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Connected is false for an account that has been removed since it last
	// answered. Its record stays until the counters are reset.
	Connected bool `json:"connected"`

	// Fetches is every shard this account was asked for, and the four figures
	// under it are what came of them. They add up to Fetches.
	Fetches  int64 `json:"fetches"`
	Wins     int64 `json:"wins"`
	Late     int64 `json:"late"`
	Aborted  int64 `json:"aborted"`
	Failures int64 `json:"failures"`

	// Bytes is what this account actually delivered — the shards that arrived,
	// won or late — rather than what it holds.
	Bytes int64 `json:"bytes"`

	// How long an answer takes, over the fetches that finished. Zero when the
	// account has not finished one.
	AverageMS float64 `json:"average_ms"`
	FastestMS float64 `json:"fastest_ms"`
	SlowestMS float64 `json:"slowest_ms"`

	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	LastAnswerAt time.Time `json:"last_answer_at,omitzero"`
}

// ReadReport is the whole board: every account that has raced, plus every
// connected account that has not, since counting started.
//
// The ones with nothing to show are in it on purpose. An account with no wins
// is the finding this panel exists for, and an account with no wins is also
// the one a report built only from what was recorded would leave out.
type ReadReport struct {
	Since      time.Time  `json:"since"`
	Races      int64      `json:"races"`
	Shortfalls int64      `json:"shortfalls"`
	Accounts   []ReadStat `json:"accounts"`
}

// ReadStats reports which accounts have been answering this vault's reads.
func (v *Vault) ReadStats() ReadReport {
	v.mu.RLock()
	connected := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()
	return v.reads.report(connected)
}

// ResetReadStats starts the counting again from now.
func (v *Vault) ResetReadStats() { v.reads.reset() }

func (s *readStats) report(connected []provider.Config) ReadReport {
	if s == nil {
		return ReadReport{Accounts: []ReadStat{}}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	report := ReadReport{
		Since:      s.since,
		Races:      s.races,
		Shortfalls: s.shortfalls,
		Accounts:   make([]ReadStat, 0, len(s.accounts)+len(connected)),
	}

	seen := map[string]bool{}
	for _, cfg := range connected {
		seen[cfg.ID] = true
		report.Accounts = append(report.Accounts, statFor(cfg.ID, cfg.Name, string(cfg.Kind), true, s.accounts[cfg.ID]))
	}
	for id, c := range s.accounts {
		if seen[id] {
			continue
		}
		report.Accounts = append(report.Accounts, statFor(id, c.name, c.kind, false, c))
	}

	// Winners first, which is the order somebody reads this in: the question
	// is who is carrying the reads, and the answer to "who is not" is the
	// bottom of the same list. Ties fall back to the name so the board does
	// not reshuffle itself between two idle accounts on every refresh.
	sort.Slice(report.Accounts, func(i, j int) bool {
		a, b := report.Accounts[i], report.Accounts[j]
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		if a.Fetches != b.Fetches {
			return a.Fetches > b.Fetches
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ProviderID < b.ProviderID
	})
	return report
}

func statFor(id, name, kind string, connected bool, c *readCounter) ReadStat {
	stat := ReadStat{ProviderID: id, Name: name, Kind: kind, Connected: connected}
	if c == nil {
		return stat
	}
	if stat.Name == "" {
		stat.Name = c.name
	}
	if stat.Kind == "" {
		stat.Kind = c.kind
	}
	stat.Fetches = c.fetches
	stat.Wins = c.wins
	stat.Late = c.late
	stat.Aborted = c.aborted
	stat.Failures = c.failures
	stat.Bytes = c.bytes
	stat.LastError = c.lastError
	stat.LastErrorAt = c.lastErrorAt
	stat.LastAnswerAt = c.lastAnswer
	if c.answers > 0 {
		stat.AverageMS = millis(c.total / time.Duration(c.answers))
		stat.FastestMS = millis(c.fastest)
		stat.SlowestMS = millis(c.slowest)
	}
	return stat
}

// millis renders a duration in milliseconds to one decimal place. A local disk
// answers in tenths of one, and an integer count of milliseconds would report
// the whole of a folder-backed account as instantaneous.
func millis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
