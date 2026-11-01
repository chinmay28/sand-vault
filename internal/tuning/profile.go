package tuning

import (
	"fmt"
	"sort"
)

// Profile names how a Tuning is chosen.
type Profile string

const (
	// ProfileAuto derives the tuning from the machine that is detected at
	// startup. It is the default, and what a fresh install runs on.
	ProfileAuto Profile = "auto"
	// ProfilePi5 is a Raspberry Pi 5 with 16 GB: four cores on an SD card
	// or an NVMe hat, the machine SAND was written for.
	ProfilePi5 Profile = "pi5"
	// ProfileWorkstation is a desktop-class machine: a recent Intel or AMD
	// part with 64 GB, fast local disk and nothing else to do.
	ProfileWorkstation Profile = "workstation"
	// ProfileCustom derives the tuning from a machine the operator describes,
	// for when detection is wrong — a container that reports the host's
	// memory, say — or when the machine has other work to leave room for.
	ProfileCustom Profile = "custom"
)

// Profiles lists every profile a Config may name, in the order a settings
// page offers them.
func Profiles() []Profile {
	return []Profile{ProfileAuto, ProfilePi5, ProfileWorkstation, ProfileCustom}
}

// Valid reports whether p is a profile this package knows.
func (p Profile) Valid() bool {
	for _, known := range Profiles() {
		if p == known {
			return true
		}
	}
	return false
}

// Label is the profile as a settings page names it.
func (p Profile) Label() string {
	switch p {
	case ProfileAuto:
		return "Automatic"
	case ProfilePi5:
		return "Raspberry Pi 5 · 16 GB"
	case ProfileWorkstation:
		return "Workstation · 64 GB"
	case ProfileCustom:
		return "Custom"
	}
	return string(p)
}

// Machines the presets stand for. A preset is Derive over one of these, so
// the two fixed profiles and the custom one are the same formula over three
// different inputs rather than three tables that have to agree.
var (
	pi5Machine         = Machine{CPUs: 4, MemoryBytes: 16 * gib}
	workstationMachine = Machine{CPUs: 24, MemoryBytes: 64 * gib}
)

// PresetMachine is the machine a fixed profile stands for, and false for the
// profiles that take their machine from elsewhere.
func PresetMachine(p Profile) (Machine, bool) {
	switch p {
	case ProfilePi5:
		return pi5Machine, true
	case ProfileWorkstation:
		return workstationMachine, true
	}
	return Machine{}, false
}

// Derive is the formula: the Tuning a machine of this size should run.
//
// It is monotone in both inputs and clamped at both ends, so a machine
// smaller than a Pi gets the Pi's numbers and nothing larger than a
// workstation gets more than one. What each knob follows:
//
//   - Chunk size follows memory, in one step: 16 MiB under 32 GB, 32 MiB from
//     there. A seek into a film fetches a chunk before the first frame, so the
//     size is kept where a home connection answers it in a second or two.
//   - The windows follow the CPU count, since each chunk in flight is a
//     thread's worth of compression and encryption, and are capped where a
//     consumer cloud starts rate-limiting rather than where the machine does.
//   - The cache follows memory: a sixteenth of it, so a 16 GB Pi keeps a
//     gigabyte of film and a 64 GB workstation keeps four.
//   - The ceilings follow memory too, though not because a rebuild is held in
//     memory any more: a scheduled job on a small machine should still not
//     decide on its own to spend the night on one file.
//
// A machine with no known memory is treated as the Pi, which is the safe
// error.
func Derive(m Machine) Tuning {
	if m.MemoryBytes <= 0 {
		m.MemoryBytes = pi5Machine.MemoryBytes
	}
	if m.CPUs <= 0 {
		m.CPUs = pi5Machine.CPUs
	}

	chunk := int64(16 * mib)
	if m.MemoryBytes >= 32*gib {
		chunk = 32 * mib
	}

	t := Tuning{
		ChunkSize:       chunk,
		UploadWindow:    clampInt(m.CPUs/2, 4, 12),
		ReadAhead:       clampInt(m.CPUs/4, 1, 6),
		ChunkCacheBytes: clampInt64(m.MemoryBytes/16, 128*mib, 8*gib) / (16 * mib) * (16 * mib),
		CompressLevel:   3,
		CompressThreads: clampInt(m.CPUs/2, 1, 8),
		TransferWindow:  clampInt(m.CPUs, 4, 8),
		EraseWindow:     clampInt(m.CPUs, 4, 8),
		RebuildLimit:    clampInt64(m.MemoryBytes/4, 1*gib, 16*gib) / (64 * mib) * (64 * mib),
		BundleLimit:     clampInt64(m.MemoryBytes/4, 2*gib, 16*gib) / (64 * mib) * (64 * mib),
	}
	// Enough idle connections that a full upload window to one cloud, with a
	// transfer running beside it, reuses connections rather than opening
	// them.
	t.ConnsPerHost = clampInt(2*t.UploadWindow+t.TransferWindow, 8, 32)
	return fit(t, m)
}

// fit shrinks a tuning until its footprint is within what Check allows on
// the machine, on machines smaller than the floors above were written for.
// The cache goes first, since it is a convenience; then the read-ahead; then
// the upload window, which is the last thing that can give. A chunk has to
// fit however small the machine, so the loop stops there.
func fit(t Tuning, m Machine) Tuning {
	ceiling := int64(float64(m.MemoryBytes) * footprintShare)
	for t.Footprint() > ceiling {
		switch {
		case t.ChunkCacheBytes > 16*mib:
			t.ChunkCacheBytes = max(t.ChunkCacheBytes/2/(16*mib)*(16*mib), 16*mib)
		case t.ReadAhead > 0:
			t.ReadAhead--
		case t.UploadWindow > 1:
			t.UploadWindow--
		default:
			return t
		}
	}
	return t
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Config is what the operator chose, as it is stored on disk.
//
// It is deliberately not the Tuning itself. Storing the derived numbers
// would freeze them: a later release that found a better formula, or a
// machine that gained memory, would be held to what was written down. What
// is stored is the decision — which profile, what machine if a custom one,
// and the knobs set by hand — and the numbers are derived from it every time.
type Config struct {
	// Profile is which way the defaults are chosen. Empty reads as auto.
	Profile Profile `json:"profile,omitempty"`
	// Machine is the one a custom profile derives from. Ignored by every
	// other profile, and kept so that switching away and back loses nothing.
	Machine *Machine `json:"machine,omitempty"`
	// Overrides are the knobs set by hand, by JSON name, on top of whatever
	// the profile derived. Reset to defaults is this map emptied.
	Overrides map[string]int64 `json:"overrides,omitempty"`
}

// Resolved is a Config worked out on a machine: the defaults the profile
// gave, the overrides on top, and what the two add up to. It is what the
// settings page is drawn from and what the server applies.
type Resolved struct {
	Profile Profile `json:"profile"`
	// Machine is what the defaults were derived from: the detected one for
	// auto, the preset's for a fixed profile, the operator's for custom.
	Machine Machine `json:"machine"`
	// Detected is what the process found on its own, shown so the operator
	// can see what auto would use and whether custom is telling the truth.
	Detected  Machine          `json:"detected"`
	Defaults  Tuning           `json:"defaults"`
	Overrides map[string]int64 `json:"overrides"`
	Effective Tuning           `json:"effective"`
}

// Resolve works a Config out on the detected machine. The result is checked,
// so a Config that resolves is one the server can apply.
func Resolve(c Config, detected Machine) (Resolved, error) {
	profile := c.Profile
	if profile == "" {
		profile = ProfileAuto
	}
	if !profile.Valid() {
		return Resolved{}, fmt.Errorf("unknown profile %q", profile)
	}

	var machine Machine
	switch profile {
	case ProfileAuto:
		machine = detected
	case ProfileCustom:
		if c.Machine == nil || c.Machine.CPUs <= 0 || c.Machine.MemoryBytes <= 0 {
			return Resolved{}, fmt.Errorf("a custom profile needs the machine's CPUs and memory")
		}
		machine = *c.Machine
	default:
		machine, _ = PresetMachine(profile)
	}

	defaults := Derive(machine)
	effective := defaults
	overrides := map[string]int64{}
	for _, name := range sortedKeys(c.Overrides) {
		value := c.Overrides[name]
		next, ok := effective.Set(name, value)
		if !ok {
			return Resolved{}, fmt.Errorf("no such setting %q", name)
		}
		// An override equal to the default is no override; recording it
		// would show a line as changed that is not.
		if current, _ := defaults.Get(name); current == value {
			continue
		}
		effective = next
		overrides[name] = value
	}

	// The footprint is held against the machine the defaults came from — for
	// a fixed profile that is the preset's, which is the operator saying
	// "treat this box as a Pi". Against the detected one when it is known
	// and smaller, since no profile can conjure memory the process does not
	// have.
	check := machine
	if detected.MemoryBytes > 0 && detected.MemoryBytes < check.MemoryBytes {
		check = detected
	}
	if err := effective.Check(check); err != nil {
		return Resolved{}, err
	}

	return Resolved{
		Profile:   profile,
		Machine:   machine,
		Detected:  detected,
		Defaults:  defaults,
		Overrides: overrides,
		Effective: effective,
	}, nil
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
