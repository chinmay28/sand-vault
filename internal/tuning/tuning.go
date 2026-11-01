package tuning

import (
	"fmt"
	"sort"
)

const (
	kib = 1 << 10
	mib = 1 << 20
	gib = 1 << 30
)

// Tuning is every knob SAND sizes to the machine.
//
// Each field is read by exactly one place: the vault for the windows, cache
// and chunk size, the compressor for its level and threads, the provider
// layer for its connection pool, and the automation for the two ceilings an
// unattended job will not go past. None of them changes what is stored —
// every file records the chunk size it was cut with, and compression is
// self-describing — so a Tuning can change under a running vault and the
// files already on the clouds are read exactly as before.
type Tuning struct {
	// ChunkSize is the plaintext length new uploads are cut into. Larger
	// means fewer objects and requests per file; smaller means a seek into a
	// film fetches less before the first frame.
	ChunkSize int64 `json:"chunk_size"`

	// UploadWindow is how many chunks an upload encodes and pushes at once.
	// Bytes in flight are about window × 2.5 × chunk size.
	UploadWindow int `json:"upload_window"`

	// ReadAhead is how many chunks a sequential reader fetches ahead of the
	// one being read, so a download or an export is not one round trip per
	// chunk. Zero reads one chunk at a time.
	ReadAhead int `json:"read_ahead"`

	// ChunkCacheBytes is how much decrypted chunk data the vault keeps for
	// players seeking around a file. It holds plaintext and is dropped on
	// lock.
	ChunkCacheBytes int64 `json:"chunk_cache_bytes"`

	// CompressLevel is the zstd speed level: 1 fastest, 2 default, 3 better
	// compression, 4 best. Higher costs CPU per chunk for smaller parts.
	CompressLevel int `json:"compress_level"`

	// CompressThreads is how many threads one chunk's compression may use.
	CompressThreads int `json:"compress_threads"`

	// TransferWindow is how many objects a move between clouds, a thumbnail
	// repack, or an automated check keeps in flight at once.
	TransferWindow int `json:"transfer_window"`

	// EraseWindow is how many files a delete erases abreast. Sweeps that
	// erase bare objects rather than files run at twice this.
	EraseWindow int `json:"erase_window"`

	// ConnsPerHost is how many idle connections the provider layer keeps
	// open to each cloud, so a window of requests reuses connections rather
	// than opening one per request.
	ConnsPerHost int `json:"conns_per_host"`

	// RebuildLimit is the largest file an unattended repair will rebuild
	// unless the folder's policy says otherwise.
	RebuildLimit int64 `json:"rebuild_limit"`

	// BundleLimit is the largest stored git bundle an unattended refresh
	// will re-fetch unless the folder's policy says otherwise.
	BundleLimit int64 `json:"bundle_limit"`
}

// Unit says how a knob's value is read and shown.
type Unit string

const (
	// UnitBytes is a size, shown as MiB or GiB.
	UnitBytes Unit = "bytes"
	// UnitCount is a plain number of things in flight.
	UnitCount Unit = "count"
	// UnitLevel is a position on a scale with named ends.
	UnitLevel Unit = "level"
)

// Knob describes one field of a Tuning: what it is called in JSON and to a
// person, what it does, and the range it may be set to. The list is what the
// settings page is drawn from, so a new field here is a new line there.
type Knob struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
	Unit  Unit   `json:"unit"`
	Min   int64  `json:"min"`
	Max   int64  `json:"max"`
	// Step is the granularity a size is set in, so a chunk size cannot be
	// an odd number of bytes. Zero means any value in range.
	Step int64 `json:"step,omitempty"`
	// Restart is true for a knob that takes effect only when the server is
	// restarted. Everything else applies the moment it is saved.
	Restart bool `json:"restart,omitempty"`
}

// knobs is the one table every per-field operation walks. Order is the order
// the settings page shows them in: the ones that decide throughput first,
// the ceilings last.
var knobs = []Knob{
	{Name: "chunk_size", Label: "Chunk size", Unit: UnitBytes, Min: 1 * mib, Max: 256 * mib, Step: 1 * mib,
		Hint: "What new uploads are cut into. Larger means fewer objects per file; smaller means a seek fetches less before it can play"},
	{Name: "upload_window", Label: "Upload window", Unit: UnitCount, Min: 1, Max: 64,
		Hint: "Chunks encoded and sent at once. About 2.5 × chunk size in memory per chunk in flight"},
	{Name: "read_ahead", Label: "Read-ahead", Unit: UnitCount, Min: 0, Max: 32,
		Hint: "Chunks a download or export fetches ahead of the one being read. One chunk of memory each"},
	{Name: "chunk_cache_bytes", Label: "Chunk cache", Unit: UnitBytes, Min: 16 * mib, Max: 64 * gib, Step: 16 * mib,
		Hint: "Decrypted chunks kept for players seeking around a file. Dropped when the vault locks"},
	{Name: "compress_level", Label: "Compression", Unit: UnitLevel, Min: 1, Max: 4,
		Hint: "1 is fastest, 4 is smallest. Costs CPU per chunk on the way in; changes nothing about reading"},
	{Name: "compress_threads", Label: "Compression threads", Unit: UnitCount, Min: 1, Max: 64,
		Hint: "Threads one chunk's compression may use"},
	{Name: "transfer_window", Label: "Transfer window", Unit: UnitCount, Min: 1, Max: 64,
		Hint: "Objects in flight when moving files between clouds or checking them. Each cloud rate-limits by request, so more is not always faster"},
	{Name: "erase_window", Label: "Erase window", Unit: UnitCount, Min: 1, Max: 64,
		Hint: "Files a delete erases abreast"},
	{Name: "conns_per_host", Label: "Connections per cloud", Unit: UnitCount, Min: 2, Max: 256,
		Hint: "Idle connections kept open to each cloud, so a window of requests does not open one each"},
	{Name: "rebuild_limit", Label: "Unattended rebuild limit", Unit: UnitBytes, Min: 64 * mib, Max: 1 << 40, Step: 64 * mib,
		Hint: "The largest file a scheduled repair will rebuild on its own; larger ones are named for you to repair by hand"},
	{Name: "bundle_limit", Label: "Unattended bundle limit", Unit: UnitBytes, Min: 64 * mib, Max: 1 << 40, Step: 64 * mib,
		Hint: "The largest git bundle a scheduled refresh will re-fetch on its own"},
}

// Knobs lists every field, in the order the settings page shows them.
func Knobs() []Knob {
	out := make([]Knob, len(knobs))
	copy(out, knobs)
	return out
}

// KnobNamed finds a knob by its JSON name.
func KnobNamed(name string) (Knob, bool) {
	for _, k := range knobs {
		if k.Name == name {
			return k, true
		}
	}
	return Knob{}, false
}

// Get reads one field by its JSON name. Every field is an integer of some
// kind, so one type carries all of them.
func (t Tuning) Get(name string) (int64, bool) {
	switch name {
	case "chunk_size":
		return t.ChunkSize, true
	case "upload_window":
		return int64(t.UploadWindow), true
	case "read_ahead":
		return int64(t.ReadAhead), true
	case "chunk_cache_bytes":
		return t.ChunkCacheBytes, true
	case "compress_level":
		return int64(t.CompressLevel), true
	case "compress_threads":
		return int64(t.CompressThreads), true
	case "transfer_window":
		return int64(t.TransferWindow), true
	case "erase_window":
		return int64(t.EraseWindow), true
	case "conns_per_host":
		return int64(t.ConnsPerHost), true
	case "rebuild_limit":
		return t.RebuildLimit, true
	case "bundle_limit":
		return t.BundleLimit, true
	}
	return 0, false
}

// Set writes one field by its JSON name, returning the changed Tuning. It
// does not range-check — Validate does, over the whole — so that a caller
// building one can set every field and hear every complaint at once.
func (t Tuning) Set(name string, value int64) (Tuning, bool) {
	switch name {
	case "chunk_size":
		t.ChunkSize = value
	case "upload_window":
		t.UploadWindow = int(value)
	case "read_ahead":
		t.ReadAhead = int(value)
	case "chunk_cache_bytes":
		t.ChunkCacheBytes = value
	case "compress_level":
		t.CompressLevel = int(value)
	case "compress_threads":
		t.CompressThreads = int(value)
	case "transfer_window":
		t.TransferWindow = int(value)
	case "erase_window":
		t.EraseWindow = int(value)
	case "conns_per_host":
		t.ConnsPerHost = int(value)
	case "rebuild_limit":
		t.RebuildLimit = value
	case "bundle_limit":
		t.BundleLimit = value
	default:
		return t, false
	}
	return t, true
}

// Validate reports every field outside its range, so a settings page can
// mark each bad line rather than the first.
func (t Tuning) Validate() error {
	var errs []string
	for _, k := range knobs {
		value, _ := t.Get(k.Name)
		if value < k.Min || value > k.Max {
			errs = append(errs, fmt.Sprintf("%s: %d is outside %d–%d", k.Name, value, k.Min, k.Max))
			continue
		}
		if k.Step > 0 && value%k.Step != 0 {
			errs = append(errs, fmt.Sprintf("%s: %d is not a multiple of %d", k.Name, value, k.Step))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return &ValidationError{Problems: errs}
}

// ValidationError lists what Validate found, one line per field.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d settings are out of range: %s", len(e.Problems), joinProblems(e.Problems))
}

func joinProblems(problems []string) string {
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

// Footprint estimates the memory a Tuning commits to at once: one upload at
// full window, one sequential reader at full read-ahead, and the cache full.
// It is the figure Check holds against the machine, and the one the
// settings page shows beside the numbers so that somebody raising three of
// them at once sees what they add up to.
func (t Tuning) Footprint() int64 {
	// A chunk in flight is its plaintext, its compressed form and its shards,
	// which together come to about two and a half times the chunk.
	upload := int64(t.UploadWindow) * t.ChunkSize * 5 / 2
	ahead := int64(t.ReadAhead+1) * t.ChunkSize
	return upload + ahead + t.ChunkCacheBytes
}

// footprintShare is the most of a machine's memory a Tuning may commit to.
// Past it the collector has nowhere to go: GOMEMLIMIT is set to 85% of the
// same figure, and what the heap holds beyond these buffers — the index,
// connections, the runtime itself — needs the rest.
const footprintShare = 0.5

// Check is Validate plus the one constraint that depends on the machine: the
// footprint must fit. A machine with no known memory is not checked against,
// since there is nothing to check it against.
func (t Tuning) Check(m Machine) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if m.MemoryBytes <= 0 {
		return nil
	}
	ceiling := int64(float64(m.MemoryBytes) * footprintShare)
	if footprint := t.Footprint(); footprint > ceiling {
		return fmt.Errorf("these settings commit %s at once, more than half the %s this machine has",
			FormatBytes(footprint), FormatBytes(m.MemoryBytes))
	}
	return nil
}

// FormatBytes writes a size the way the settings page does: whole MiB below
// a GiB, one decimal of GiB above.
func FormatBytes(n int64) string {
	switch {
	case n >= gib:
		whole := n / gib
		tenths := (n % gib) * 10 / gib
		if tenths == 0 {
			return fmt.Sprintf("%d GiB", whole)
		}
		return fmt.Sprintf("%d.%d GiB", whole, tenths)
	case n >= mib:
		return fmt.Sprintf("%d MiB", n/mib)
	case n >= kib:
		return fmt.Sprintf("%d KiB", n/kib)
	}
	return fmt.Sprintf("%d B", n)
}
