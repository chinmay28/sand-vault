package vault

import (
	"slices"
	"sort"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// How fast the vault is, in the units a connection is sold in.
//
// Three different speeds, because they answer three different questions:
//
//   - Each cloud's: bytes over time for every object sent to it or fetched
//     off it, one call at a time. This is the account's own link — what that
//     provider, from this machine, actually manages.
//   - The vault's: bytes of the file over the time it took to open or store,
//     from the first request to the rebuilt (or fully scattered) data. This is
//     what somebody waiting on a file experiences, and it is not the sum of
//     the clouds' — a rebuild is a race that waits on the k-th answer, and a
//     store waits on the slowest cloud it writes to.
//   - By file type: the vault's speed again, split by what was opened or
//     stored, since a photo, a film and a PDF are read in very different ways
//     (one small object, a stream of chunks, a few pages).
//
// A chunked file is opened a chunk at a time, so each chunk is one open. When
// a player reads ahead several chunks at once each is timed on its own, so the
// figure is the speed of one load, not the sum of those running side by side.
//
// Speeds are time-weighted — total bytes over total time — never a mean of
// per-call rates, which would let a thousand tiny thumbnails outvote one film.

// loadCounts is one kind of file's opens and stores inside one bucket.
type loadCounts struct {
	Opens     int64         `json:"opens"`
	OpenBytes int64         `json:"open_bytes"`
	OpenTook  time.Duration `json:"open_took"`

	Stores     int64         `json:"stores"`
	StoreBytes int64         `json:"store_bytes"`
	StoreTook  time.Duration `json:"store_took"`
}

func (c *loadCounts) add(other *loadCounts) {
	c.Opens += other.Opens
	c.OpenBytes += other.OpenBytes
	c.OpenTook += other.OpenTook
	c.Stores += other.Stores
	c.StoreBytes += other.StoreBytes
	c.StoreTook += other.StoreTook
}

func (b *readBucket) loadsFor(kind string) *loadCounts {
	if b.Loads == nil {
		b.Loads = map[string]*loadCounts{}
	}
	c := b.Loads[kind]
	if c == nil {
		c = &loadCounts{}
		b.Loads[kind] = c
	}
	return c
}

// kindThumbnails is the kind a folder's thumbnail pack is counted under: it is
// not one of the files in the folder, and folding it into "images" would make
// every folder anybody browses look like a photo library.
const kindThumbnails = "thumbnails"

// kindOfName is kindOf for a file that is not an entry yet — an upload on its
// way in, known only by its name.
func kindOfName(name string) string { return kindOf(&Entry{Name: name}) }

// loadDirection is which way a whole file moved: opened off the clouds, or
// stored onto them.
type loadDirection int

const (
	loadOpen loadDirection = iota
	loadStore
)

// loaded records one file — or one chunk of one — opened or stored: how many
// bytes of it and how long that took end to end. Nothing is recorded for a
// load that took no time, which no network transfer does, or one with no
// bytes.
func (s *readStats) loaded(kind string, dir loadDirection, bytes int64, took time.Duration) {
	if s == nil || kind == "" || bytes <= 0 || took <= 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return
	}
	for _, c := range []*loadCounts{s.bucketLocked(now).loadsFor(kind), s.total.loadsFor(kind)} {
		if dir == loadOpen {
			c.Opens++
			c.OpenBytes += bytes
			c.OpenTook += took
		} else {
			c.Stores++
			c.StoreBytes += bytes
			c.StoreTook += took
		}
	}
	s.touchedLocked(now)
}

// Speed is an amount moved over the time it took, with the rate worked out.
type Speed struct {
	// Count is how many calls, opens or stores it is over.
	Count   int64   `json:"count"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	// Mbps is megabits per second — 10^6 bits, the unit a connection is sold
	// in. Zero when nothing was timed.
	Mbps float64 `json:"mbps"`
}

func speedOf(count, bytes int64, took time.Duration) Speed {
	s := Speed{Count: count, Bytes: bytes, Seconds: took.Seconds()}
	if s.Seconds > 0 && bytes > 0 {
		s.Mbps = float64(bytes) * 8 / s.Seconds / 1e6
	}
	return s
}

// KindSpeed is the vault's speed for one kind of file.
type KindSpeed struct {
	Kind  string `json:"kind"`
	Open  Speed  `json:"open"`
	Store Speed  `json:"store"`
}

// AccountSpeed is one cloud's own speed in each direction.
type AccountSpeed struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Connected  bool   `json:"connected"`

	// Up is sending to the cloud, Down is fetching from it.
	Up   Speed `json:"up"`
	Down Speed `json:"down"`
}

// ThroughputReport is how fast things have moved over one window.
type ThroughputReport struct {
	Window ReadWindow `json:"window"`
	From   time.Time  `json:"from,omitzero"`
	Since  time.Time  `json:"since,omitzero"`
	Days   int        `json:"days"`

	// Open and Store are the whole vault, every kind of file together.
	Open  Speed `json:"open"`
	Store Speed `json:"store"`

	// Kinds is always led by images, video and documents — the three somebody
	// asks about — even when nothing of that kind moved, then any others that
	// did, in a fixed order so the rows hold still between polls.
	Kinds []KindSpeed `json:"kinds"`

	// Accounts is every connected cloud, and every disconnected one with
	// figures in the window, fastest download first.
	Accounts []AccountSpeed `json:"accounts"`
}

// kindOrder is the order kinds are listed in.
var kindOrder = []string{"images", "video", "documents", "audio", "archives", "other", kindThumbnails}

// alwaysListed are the kinds reported even with nothing against them.
var alwaysListed = map[string]bool{"images": true, "video": true, "documents": true}

// Throughput reports how fast files have been opened and stored, and how fast
// each cloud has been, over one window.
func (v *Vault) Throughput(window ReadWindow) ThroughputReport {
	v.mu.RLock()
	connected := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()
	return v.reads.throughputReport(window, connected, time.Now())
}

func (s *readStats) throughputReport(window ReadWindow, connected []provider.Config, now time.Time) ThroughputReport {
	if s == nil {
		return ThroughputReport{Window: window, Kinds: []KindSpeed{}, Accounts: []AccountSpeed{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w := s.windowLocked(window, now)
	report := ThroughputReport{Window: window, From: w.from, Since: s.since, Days: w.days}

	var all loadCounts
	report.Kinds = []KindSpeed{}
	for _, kind := range kindOrder {
		c := w.summed.Loads[kind]
		if c == nil && !alwaysListed[kind] {
			continue
		}
		if c == nil {
			c = &loadCounts{}
		}
		all.add(c)
		report.Kinds = append(report.Kinds, KindSpeed{
			Kind:  kind,
			Open:  speedOf(c.Opens, c.OpenBytes, c.OpenTook),
			Store: speedOf(c.Stores, c.StoreBytes, c.StoreTook),
		})
	}
	// A kind this build does not name — written by a later one — still
	// counts towards the whole.
	for kind, c := range w.summed.Loads {
		if !slices.Contains(kindOrder, kind) {
			all.add(c)
		}
	}
	report.Open = speedOf(all.Opens, all.OpenBytes, all.OpenTook)
	report.Store = speedOf(all.Stores, all.StoreBytes, all.StoreTook)

	report.Accounts = []AccountSpeed{}
	seen := map[string]bool{}
	add := func(id, name, kind string, connected bool) {
		stat := s.transferStatLocked(id, name, kind, connected, w.summed.Transfers[id])
		c := w.summed.Transfers[id]
		row := AccountSpeed{ProviderID: id, Name: stat.Name, Kind: stat.Kind, Connected: connected}
		if c != nil {
			row.Up = speedOf(c.Puts, c.BytesIn, c.InTook)
			row.Down = speedOf(c.Gets, c.BytesOut, c.OutTook)
		}
		report.Accounts = append(report.Accounts, row)
	}
	for _, cfg := range connected {
		if seen[cfg.ID] {
			continue
		}
		seen[cfg.ID] = true
		add(cfg.ID, cfg.Name, string(cfg.Kind), true)
	}
	for id := range w.summed.Transfers {
		if !seen[id] {
			add(id, "", "", false)
		}
	}
	sort.Slice(report.Accounts, func(i, j int) bool {
		a, b := report.Accounts[i], report.Accounts[j]
		if a.Down.Mbps != b.Down.Mbps {
			return a.Down.Mbps > b.Down.Mbps
		}
		if a.Up.Mbps != b.Up.Mbps {
			return a.Up.Mbps > b.Up.Mbps
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ProviderID < b.ProviderID
	})
	return report
}
