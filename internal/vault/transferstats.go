package vault

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// How much has gone to each cloud, and how much has come back off it.
//
// The read race (readstats.go) answers "who is quick". It does not answer "who
// is busy": the bytes a race counts are only the shards that arrived, and only
// for reads. Uploads, moves between clouds, conversions and the index backup
// are all traffic too, and at the providers that bill for egress — or that
// throttle an account once it has moved too much in a day — they are the
// figure somebody actually needs.
//
// So every object the vault puts on an account or takes off one is metered,
// at the two calls everything goes through: Put and Get. In is towards the
// cloud (an upload), out is off it (a download), named from the cloud's side
// because the cloud is the row the panel is about.
//
// The figures ride in the read history's own buckets rather than a store of
// their own. A transfer is the same kind of fact as a read — when the vault was
// used and how much came off which cloud — so it belongs under the same seal,
// in the same daily buckets, summed into the same windows and forgotten by the
// same button. Two sidecars would be two files to keep in step through every
// lock, unlock and password change, for no gain.

// transferCounts is one account's traffic inside one bucket.
type transferCounts struct {
	// BytesIn is what was written to the account; BytesOut is what was read
	// back off it. Only completed objects count — a Get cut off mid-flight
	// returns nothing, so nothing is known about how much of it moved.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`

	Puts int64 `json:"puts"`
	Gets int64 `json:"gets"`

	// Failures are calls the account could not complete. A cancellation is
	// the vault's own doing and a missing object is an answer, so neither is
	// one — see transferFailed.
	PutFailures int64 `json:"put_failures"`
	GetFailures int64 `json:"get_failures"`

	// How long the completed calls took, summed, so a speed is BytesIn over
	// InTook: time-weighted, which is what an average speed has to be. Only
	// completed calls, for the same reason only their bytes are counted.
	InTook  time.Duration `json:"in_took,omitempty"`
	OutTook time.Duration `json:"out_took,omitempty"`

	LastAt time.Time `json:"last_at,omitzero"`
}

func (c *transferCounts) add(other *transferCounts) {
	c.InTook += other.InTook
	c.OutTook += other.OutTook
	c.BytesIn += other.BytesIn
	c.BytesOut += other.BytesOut
	c.Puts += other.Puts
	c.Gets += other.Gets
	c.PutFailures += other.PutFailures
	c.GetFailures += other.GetFailures
	if other.LastAt.After(c.LastAt) {
		c.LastAt = other.LastAt
	}
}

func (b *readBucket) transfersFor(id string) *transferCounts {
	if b.Transfers == nil {
		b.Transfers = map[string]*transferCounts{}
	}
	c := b.Transfers[id]
	if c == nil {
		c = &transferCounts{}
		b.Transfers[id] = c
	}
	return c
}

// transferDirection is which way an object moved, from the cloud's side.
type transferDirection int

const (
	// transferIn is towards the account: a Put.
	transferIn transferDirection = iota
	// transferOut is off the account: a Get.
	transferOut
)

// transferFailed reports whether an error from a Put or a Get is the account
// failing, as opposed to the vault cancelling the call or asking for something
// that was never there.
func transferFailed(err error) bool {
	return err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, provider.ErrNotFound)
}

func observeTransfer(c *transferCounts, dir transferDirection, n int64, took time.Duration, err error, now time.Time) {
	switch {
	case err == nil && dir == transferIn:
		c.Puts++
		c.BytesIn += n
		c.InTook += took
	case err == nil:
		c.Gets++
		c.BytesOut += n
		c.OutTook += took
	case !transferFailed(err):
		// Nothing moved and nothing went wrong.
		return
	case dir == transferIn:
		c.PutFailures++
	default:
		c.GetFailures++
	}
	c.LastAt = now
}

// transferred records one object moving to or from an account. n is the size
// of the object and took how long the call took; both are ignored when err is
// set. An account's replica root is
// the account, so its traffic is counted under the account's own ID.
//
// A call that neither moved anything nor failed — a cancelled Get, a missing
// object — leaves the history untouched, and so does anything at all while the
// vault is locked (see readStats.locked).
func (s *readStats) transferred(cfg provider.Config, dir transferDirection, n int, took time.Duration, err error) {
	if s == nil || cfg.ID == "" {
		return
	}
	if err != nil && !transferFailed(err) {
		return
	}
	id := accountIDOf(cfg.ID)

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return
	}

	if cfg.Name != "" || cfg.Kind != "" {
		s.names[id] = readAccountName{Name: cfg.Name, Kind: string(cfg.Kind)}
	}
	observeTransfer(s.bucketLocked(now).transfersFor(id), dir, int64(n), took, err, now)
	observeTransfer(s.total.transfersFor(id), dir, int64(n), took, err, now)
	s.touchedLocked(now)
}

// meteredPut is p.Put, counted. Every write of an object to an account goes
// through here, so the transfer figures cannot miss one.
func (v *Vault) meteredPut(ctx context.Context, p provider.Provider, cfg provider.Config, key string, data []byte) error {
	started := time.Now()
	err := p.Put(ctx, key, data)
	v.reads.transferred(cfg, transferIn, len(data), time.Since(started), err)
	return err
}

// meteredGet is p.Get, counted.
func (v *Vault) meteredGet(ctx context.Context, p provider.Provider, cfg provider.Config, key string) ([]byte, error) {
	started := time.Now()
	data, err := p.Get(ctx, key)
	v.reads.transferred(cfg, transferOut, len(data), time.Since(started), err)
	return data, err
}

// TransferStat is one account's traffic over a window.
type TransferStat struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`

	// Connected is false for an account removed since it last moved anything.
	Connected bool `json:"connected"`

	// BytesIn went to the account; BytesOut came off it.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`

	Puts        int64 `json:"puts"`
	Gets        int64 `json:"gets"`
	PutFailures int64 `json:"put_failures"`
	GetFailures int64 `json:"get_failures"`

	LastAt time.Time `json:"last_at,omitzero"`
}

// TransferTrendPoint is the whole vault's traffic over one span of a window.
type TransferTrendPoint struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Days     int       `json:"days"`
	BytesIn  int64     `json:"bytes_in"`
	BytesOut int64     `json:"bytes_out"`
}

// TransferReport is every account's traffic over one window, plus every
// connected account that moved nothing in it — an idle account is a finding
// as much as a busy one.
type TransferReport struct {
	Window ReadWindow `json:"window"`
	From   time.Time  `json:"from,omitzero"`
	Since  time.Time  `json:"since,omitzero"`

	// The vault's totals over the window: the sum of the accounts below.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`

	Accounts []TransferStat `json:"accounts"`

	// Trend is the vault's traffic across the window, in order, or absent
	// where the window is too short to have a shape.
	Trend []TransferTrendPoint `json:"trend,omitempty"`

	Days int `json:"days"`
}

// TransferStats reports how much each account has had written to it and read
// off it over one window.
func (v *Vault) TransferStats(window ReadWindow) TransferReport {
	v.mu.RLock()
	connected := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()
	return v.reads.transferReport(window, connected, time.Now())
}

func (s *readStats) transferReport(window ReadWindow, connected []provider.Config, now time.Time) TransferReport {
	if s == nil {
		return TransferReport{Window: window, Accounts: []TransferStat{}}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	w := s.windowLocked(window, now)
	report := TransferReport{Window: window, From: w.from, Since: s.since, Days: w.days}

	report.Accounts = make([]TransferStat, 0, len(w.summed.Transfers)+len(connected))
	seen := map[string]bool{}
	for _, cfg := range connected {
		if seen[cfg.ID] {
			continue
		}
		seen[cfg.ID] = true
		report.Accounts = append(report.Accounts,
			s.transferStatLocked(cfg.ID, cfg.Name, string(cfg.Kind), true, w.summed.Transfers[cfg.ID]))
	}
	for id, counts := range w.summed.Transfers {
		if seen[id] {
			continue
		}
		report.Accounts = append(report.Accounts, s.transferStatLocked(id, "", "", false, counts))
	}
	for _, a := range report.Accounts {
		report.BytesIn += a.BytesIn
		report.BytesOut += a.BytesOut
	}

	// Busiest first, ties by name so idle accounts hold still between polls.
	sort.Slice(report.Accounts, func(i, j int) bool {
		a, b := report.Accounts[i], report.Accounts[j]
		if ta, tb := a.BytesIn+a.BytesOut, b.BytesIn+b.BytesOut; ta != tb {
			return ta > tb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ProviderID < b.ProviderID
	})

	for _, span := range w.spans {
		point := TransferTrendPoint{From: span.from, To: span.to, Days: len(span.keys)}
		for _, key := range span.keys {
			day := s.days[key]
			if day == nil {
				continue
			}
			for _, c := range day.Transfers {
				point.BytesIn += c.BytesIn
				point.BytesOut += c.BytesOut
			}
		}
		report.Trend = append(report.Trend, point)
	}
	return report
}

func (s *readStats) transferStatLocked(id, name, kind string, connected bool, c *transferCounts) TransferStat {
	stat := TransferStat{ProviderID: id, Name: name, Kind: kind, Connected: connected}
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
		stat.BytesIn = c.BytesIn
		stat.BytesOut = c.BytesOut
		stat.Puts = c.Puts
		stat.Gets = c.Gets
		stat.PutFailures = c.PutFailures
		stat.GetFailures = c.GetFailures
		stat.LastAt = c.LastAt
	}
	return stat
}
