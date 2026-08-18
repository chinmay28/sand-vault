package vault

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Which cloud answers a read, and how quickly.
//
// Every read is a race. gather asks all n accounts holding a shard and rebuilds
// from the first k distinct ones to arrive, cutting the rest off mid-flight
// (transfer.go, and gatherChunk in chunked.go for a chunked file). That is what
// makes a wide code quick to read as well as hard to lose — 4-of-6 reads from
// whichever four clouds are quickest today.
//
// It also means the vault runs a timed comparison of every account against
// every other one, hundreds of times over a single film, and used to throw the
// result away. This keeps it: how many races each account was entered into, how
// many it won — a win being an answer that arrived in time to be part of the
// rebuild — and what it did when it did not win.
//
// The point of keeping it is that the race hides a slow account right up until
// the moment it becomes a failure. Nothing downloads any slower for one cloud
// falling behind; it simply stops contributing, and the others carry the vault.
//
// Counting is by the day. A day is the smallest window somebody asks about and
// the largest one that can be summed into every other — this month, this year
// and all time are additions over the same buckets — and one bucket per account
// per day is small enough to keep for over a year in a file measured in
// kilobytes. What survives a restart, and how, is readhistory.go.
type readStats struct {
	mu sync.Mutex

	// since is when the oldest surviving figure was recorded. It is what the
	// panel means by "counting since", and it moves only when the history is
	// forgotten.
	since time.Time

	// names is what each account was called the last time it answered, so a
	// row survives the account being disconnected — what a cloud was doing
	// before somebody removed it is exactly what they may be about to want to
	// explain.
	names map[string]readAccountName

	// total is every fetch ever recorded, kept apart from the daily buckets so
	// that all time survives them being pruned.
	total readBucket
	days  map[string]*readBucket

	// dayKey is the bucket now falls in, cached with the moment it stops being
	// today: the hot path formats a date once a day rather than once a shard.
	dayKey  string
	dayEnds time.Time

	// The sidecar is written by whoever set flush — the vault, which is what
	// holds the key it is sealed under. rev counts recorded fetches so a write
	// that started before the latest one does not mark it saved.
	rev       uint64
	savedRev  uint64
	dirty     bool
	flushing  bool
	lastFlush time.Time
	flush     func()

	// saving tracks the flush goroutine, so a caller that is about to go away
	// — a CLI command ending, a test cleaning up its directory — can wait for
	// the write rather than leaving a file appearing behind it.
	saving sync.WaitGroup

	// tail tracks the goroutines still recording what the losers of a decided
	// race eventually did. Nothing waits on it in the app — the reader is long
	// gone by then — but a test that asserts on the losing side has to.
	tail sync.WaitGroup
}

// readAccountName is what an account was called, kept alongside the figures so
// the history can name an account the vault no longer has.
type readAccountName struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// readBucket is one span of time: what the vault did, and what each account did
// inside it. A day is one of these and so is all time.
type readBucket struct {
	Races      int64                  `json:"races"`
	Shortfalls int64                  `json:"shortfalls"`
	Accounts   map[string]*readCounts `json:"accounts"`
}

func (b *readBucket) counterFor(id string) *readCounts {
	if b.Accounts == nil {
		b.Accounts = map[string]*readCounts{}
	}
	c := b.Accounts[id]
	if c == nil {
		c = &readCounts{}
		b.Accounts[id] = c
	}
	return c
}

// readCounts is one account's tally inside one bucket. Durations are kept as
// durations and rounded to milliseconds only when a report is built.
type readCounts struct {
	Fetches  int64 `json:"fetches"`
	Wins     int64 `json:"wins"`
	Late     int64 `json:"late"`
	Aborted  int64 `json:"aborted"`
	Failures int64 `json:"failures"`

	// Answers is wins + late: the fetches that finished, which are the only
	// ones a duration means anything for. An aborted fetch was stopped by us
	// and a failed one never arrived, so folding either into an average would
	// make a cloud look fast for having been given up on.
	Answers int64         `json:"answers"`
	Bytes   int64         `json:"bytes"`
	Total   time.Duration `json:"total"`
	Fastest time.Duration `json:"fastest"`
	Slowest time.Duration `json:"slowest"`

	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	LastAnswerAt time.Time `json:"last_answer_at,omitzero"`
}

// add folds one bucket's figures into another, which is how a month is built
// out of days and how a loaded file is merged into what is already counted.
func (c *readCounts) add(other *readCounts) {
	c.Fetches += other.Fetches
	c.Wins += other.Wins
	c.Late += other.Late
	c.Aborted += other.Aborted
	c.Failures += other.Failures
	c.Answers += other.Answers
	c.Bytes += other.Bytes
	c.Total += other.Total
	if other.Fastest > 0 && (c.Fastest == 0 || other.Fastest < c.Fastest) {
		c.Fastest = other.Fastest
	}
	if other.Slowest > c.Slowest {
		c.Slowest = other.Slowest
	}
	// The later of the two errors, and the later of the two answers: a summed
	// bucket says when this account last did each of them, not how many times.
	if other.LastErrorAt.After(c.LastErrorAt) {
		c.LastError = other.LastError
		c.LastErrorAt = other.LastErrorAt
	}
	if other.LastAnswerAt.After(c.LastAnswerAt) {
		c.LastAnswerAt = other.LastAnswerAt
	}
}

func (b *readBucket) add(other *readBucket) {
	b.Races += other.Races
	b.Shortfalls += other.Shortfalls
	for id, counts := range other.Accounts {
		b.counterFor(id).add(counts)
	}
}

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

func newReadStats() *readStats {
	return &readStats{names: map[string]readAccountName{}, days: map[string]*readBucket{}}
}

// Every method is safe on a nil receiver, so a Vault assembled field by field
// — which the package's own tests do — reads and writes without a recorder
// rather than panicking on one it never built.

// bucketLocked returns the bucket now belongs in, opening a new day when the
// clock has walked into one. Local time, because "today" and "this month" are
// questions about the day the person asking is having.
func (s *readStats) bucketLocked(now time.Time) *readBucket {
	if s.days == nil {
		s.days = map[string]*readBucket{}
	}
	if s.dayKey == "" || !now.Before(s.dayEnds) {
		s.dayKey = now.Format(dayFormat)
		s.dayEnds = startOfDay(now).AddDate(0, 0, 1)
	}
	bucket := s.days[s.dayKey]
	if bucket == nil {
		bucket = &readBucket{}
		s.days[s.dayKey] = bucket
	}
	if s.since.IsZero() {
		s.since = now
	}
	return bucket
}

func (s *readStats) touchedLocked(now time.Time) {
	s.rev++
	s.dirty = true
	s.maybeFlushLocked(now)
}

// race notes that a read has begun. One chunk of a chunked file is one race,
// because one chunk is what the accounts are actually racing over.
func (s *readStats) race() {
	if s == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketLocked(now).Races++
	s.total.Races++
	s.touchedLocked(now)
}

// shortfall notes a race that could not find k shards, which is a read that
// failed rather than one that was merely slow.
func (s *readStats) shortfall() {
	if s == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketLocked(now).Shortfalls++
	s.total.Shortfalls++
	s.touchedLocked(now)
}

func (s *readStats) record(f shardFetch, out readOutcome) {
	if s == nil || f.shard.ProviderID == "" {
		return
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if f.shard.ProviderName != "" || f.shard.ProviderKind != "" {
		s.names[f.shard.ProviderID] = readAccountName{
			Name: f.shard.ProviderName,
			Kind: f.shard.ProviderKind,
		}
	}

	// Every fetch lands in two places: the day it happened on, which is what
	// the windows are summed from, and the all-time total, which is what
	// survives the day being pruned.
	observe(s.bucketLocked(now).counterFor(f.shard.ProviderID), f, out, now)
	observe(s.total.counterFor(f.shard.ProviderID), f, out, now)
	s.touchedLocked(now)
}

func observe(c *readCounts, f shardFetch, out readOutcome, now time.Time) {
	c.Fetches++
	switch out {
	case shardWon:
		c.Wins++
	case shardLate:
		c.Late++
	case shardAborted:
		c.Aborted++
	case shardFailed:
		c.Failures++
		if f.err != nil {
			c.LastError = f.err.Error()
			c.LastErrorAt = now
		}
	}

	if out == shardWon || out == shardLate {
		c.Answers++
		c.Bytes += int64(len(f.blob))
		c.Total += f.took
		if c.Fastest == 0 || f.took < c.Fastest {
			c.Fastest = f.took
		}
		if f.took > c.Slowest {
			c.Slowest = f.took
		}
		c.LastAnswerAt = now
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
	s.since = time.Time{}
	s.names = map[string]readAccountName{}
	s.total = readBucket{}
	s.days = map[string]*readBucket{}
	s.dayKey = ""
	s.rev++
	s.dirty = false
	s.savedRev = s.rev
}

// ReadWindow is a span the figures can be asked for.
type ReadWindow string

const (
	WindowToday ReadWindow = "today"
	WindowMonth ReadWindow = "month"
	WindowYear  ReadWindow = "year"
	WindowAll   ReadWindow = "all"
)

// ParseReadWindow reads the window a request asked for. An empty string is
// today, which is the one the panel opens on.
func ParseReadWindow(s string) (ReadWindow, error) {
	switch ReadWindow(strings.TrimSpace(strings.ToLower(s))) {
	case "":
		return WindowToday, nil
	case WindowToday:
		return WindowToday, nil
	case WindowMonth:
		return WindowMonth, nil
	case WindowYear:
		return WindowYear, nil
	case WindowAll:
		return WindowAll, nil
	}
	return "", errors.New(`window must be one of "today", "month", "year" or "all"`)
}

// ReadStat is one account's standing in the read race, over whichever window
// was asked for.
type ReadStat struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Connected is false for an account that has been removed since it last
	// answered. Its record stays until the history is forgotten.
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

	// Trend is this account's standing across the window, in order. Absent
	// where the window is too short to have a shape — today is one bucket, and
	// a line through one point says nothing.
	Trend []ReadTrendPoint `json:"trend,omitempty"`
}

// ReadTrendPoint is one span of a window: a day, or several of them where the
// window is longer than the chart has room for.
type ReadTrendPoint struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Days    int       `json:"days"`
	Fetches int64     `json:"fetches"`
	Wins    int64     `json:"wins"`
	// AverageMS is over the answers inside this span alone.
	AverageMS float64 `json:"average_ms"`
}

// ReadReport is the whole board over one window: every account that raced in
// it, plus every connected account that did not.
//
// The ones with nothing to show are in it on purpose. An account with no wins
// is the finding this panel exists for, and an account with no wins is also
// the one a report built only from what was recorded would leave out.
type ReadReport struct {
	Window ReadWindow `json:"window"`

	// From is the start of the window, and is absent for all time — which
	// starts wherever the figures do. Since is where they do.
	From  time.Time `json:"from,omitzero"`
	Since time.Time `json:"since,omitzero"`

	Races      int64      `json:"races"`
	Shortfalls int64      `json:"shortfalls"`
	Accounts   []ReadStat `json:"accounts"`

	// Days is how many daily buckets the window was summed from, so the panel
	// can say what "all time" actually reaches back over.
	Days int `json:"days"`
}

// ReadStats reports which accounts have been answering this vault's reads over
// one window.
func (v *Vault) ReadStats(window ReadWindow) ReadReport {
	v.mu.RLock()
	connected := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()
	return v.reads.report(window, connected, time.Now())
}

// ForgetReadStats erases the history, in memory and on disk.
func (v *Vault) ForgetReadStats() error {
	v.reads.reset()
	return removeReadHistory(v.path)
}

// trendPoints is how many spans a window is drawn as, at most. A year is 365
// days and a sparkline is a couple of hundred pixels: past this the chart is
// drawing detail nobody can see and the answer is carrying it.
const trendPoints = 32

func (s *readStats) report(window ReadWindow, connected []provider.Config, now time.Time) ReadReport {
	if s == nil {
		return ReadReport{Window: window, Accounts: []ReadStat{}}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	report := ReadReport{Window: window, Since: s.since}

	var summed readBucket
	var spans []readSpan
	if window == WindowAll {
		summed.add(&s.total)
		// All time is the total, which outlives the daily buckets; the shape
		// of it is whatever days are still kept.
		spans = s.spansLocked(s.dayKeysLocked(""), now)
		report.Days = len(s.days)
	} else {
		from, prefix := windowStart(window, now)
		report.From = from
		keys := s.dayKeysLocked(prefix)
		for _, key := range keys {
			summed.add(s.days[key])
		}
		spans = s.spansLocked(keys, now)
		report.Days = len(keys)
	}

	report.Races = summed.Races
	report.Shortfalls = summed.Shortfalls
	report.Accounts = make([]ReadStat, 0, len(summed.Accounts)+len(connected))

	seen := map[string]bool{}
	for _, cfg := range connected {
		seen[cfg.ID] = true
		report.Accounts = append(report.Accounts,
			s.statLocked(cfg.ID, cfg.Name, string(cfg.Kind), true, summed.Accounts[cfg.ID], spans))
	}
	for id, counts := range summed.Accounts {
		if seen[id] {
			continue
		}
		report.Accounts = append(report.Accounts, s.statLocked(id, "", "", false, counts, spans))
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

// readSpan is one column of a trend: the days it covers, and where they fall.
type readSpan struct {
	keys []string
	from time.Time
	to   time.Time
}

// dayKeysLocked lists the recorded days matching a prefix — "2026-08" for a
// month, "2026" for a year, the whole key for a day, "" for everything — in
// order. Day keys sort chronologically because they are written the only way a
// date can be both readable and sortable.
func (s *readStats) dayKeysLocked(prefix string) []string {
	keys := make([]string, 0, len(s.days))
	for key := range s.days {
		if prefix == "" || strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// spansLocked cuts a run of days into at most trendPoints columns. A month is
// one column per day; a year is a fortnight or so per column, which is the
// right resolution for a chart the width of a table row.
func (s *readStats) spansLocked(keys []string, now time.Time) []readSpan {
	if len(keys) < 3 {
		// One or two columns is not a shape, and drawing it as one invites
		// somebody to read a trend out of a single day.
		return nil
	}

	per := (len(keys) + trendPoints - 1) / trendPoints
	spans := make([]readSpan, 0, (len(keys)+per-1)/per)
	for at := 0; at < len(keys); at += per {
		end := at + per
		if end > len(keys) {
			end = len(keys)
		}
		group := keys[at:end]
		from, _ := time.ParseInLocation(dayFormat, group[0], now.Location())
		to, _ := time.ParseInLocation(dayFormat, group[len(group)-1], now.Location())
		spans = append(spans, readSpan{keys: group, from: from, to: to.AddDate(0, 0, 1)})
	}
	return spans
}

func (s *readStats) statLocked(id, name, kind string, connected bool, c *readCounts, spans []readSpan) ReadStat {
	stat := ReadStat{ProviderID: id, Name: name, Kind: kind, Connected: connected}
	if stat.Name == "" {
		stat.Name = s.names[id].Name
	}
	if stat.Kind == "" {
		stat.Kind = s.names[id].Kind
	}
	if stat.Name == "" {
		stat.Name = id
	}

	if c != nil {
		stat.Fetches = c.Fetches
		stat.Wins = c.Wins
		stat.Late = c.Late
		stat.Aborted = c.Aborted
		stat.Failures = c.Failures
		stat.Bytes = c.Bytes
		stat.LastError = c.LastError
		stat.LastErrorAt = c.LastErrorAt
		stat.LastAnswerAt = c.LastAnswerAt
		if c.Answers > 0 {
			stat.AverageMS = millis(c.Total / time.Duration(c.Answers))
			stat.FastestMS = millis(c.Fastest)
			stat.SlowestMS = millis(c.Slowest)
		}
	}

	for _, span := range spans {
		point := ReadTrendPoint{From: span.from, To: span.to, Days: len(span.keys)}
		var counts readCounts
		for _, key := range span.keys {
			day := s.days[key]
			if day == nil || day.Accounts[id] == nil {
				continue
			}
			counts.add(day.Accounts[id])
		}
		point.Fetches = counts.Fetches
		point.Wins = counts.Wins
		if counts.Answers > 0 {
			point.AverageMS = millis(counts.Total / time.Duration(counts.Answers))
		}
		stat.Trend = append(stat.Trend, point)
	}
	return stat
}

// windowStart is where a window begins, and the day-key prefix that selects it.
func windowStart(window ReadWindow, now time.Time) (time.Time, string) {
	switch window {
	case WindowMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), now.Format("2006-01")
	case WindowYear:
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()), now.Format("2006")
	default:
		return startOfDay(now), now.Format(dayFormat)
	}
}

func startOfDay(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
}

// millis renders a duration in milliseconds to one decimal place. A local disk
// answers in tenths of one, and an integer count of milliseconds would report
// the whole of a folder-backed account as instantaneous.
func millis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
