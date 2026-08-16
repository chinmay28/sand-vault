package server

import (
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Telling Go's garbage collector how much room it actually has.
//
// Go sizes the heap against how much is already live, not against how much the
// machine has: by default it lets the heap grow to roughly twice the live set
// and only then collects. That is the right instinct on a machine with room to
// spare and the wrong one on a Raspberry Pi, where the moment the heap outgrows
// physical memory the kernel starts swapping and — on an SD card — the whole
// box stops answering, ssh included, long before anything is killed.
//
// GOMEMLIMIT changes the question the collector asks from "how much is live?" to
// "how close am I to the ceiling?". Past it, Go collects continuously rather
// than growing. That turns what would have been a machine-wide stall into a slow
// process, which is a far better failure: slow is visible and recoverable, and a
// swapping Pi is neither.
//
// It is a soft limit on purpose. Go will exceed it rather than fail an
// allocation, so this is not a substitute for the unit's MemoryMax — it is what
// keeps the process from ever getting there.
const (
	// memLimitFraction is how much of what we are allowed to use we aim to stay
	// under. The gap absorbs what the runtime holds outside the Go heap, which
	// GOMEMLIMIT does not account for, and leaves the collector somewhere to
	// stand before the hard limit kills the process.
	memLimitFraction = 0.85

	// minMemLimit is the floor below which a discovered limit is treated as
	// nonsense rather than obeyed. A limit under this would have the collector
	// running flat out against a heap it can never satisfy.
	minMemLimit = 256 << 20
)

// applyMemoryLimit sets GOMEMLIMIT from whatever the process is actually allowed
// to use, unless the environment already says otherwise.
//
// The value comes from the cgroup the service runs in when there is one — which
// is where the systemd unit's MemoryMax lands — and from the machine's total
// memory when there is not. Reading it at runtime rather than computing it at
// install time means it follows the limit rather than a guess about it: raise
// MemoryMax with `systemctl edit`, restart, and this follows.
func applyMemoryLimit() {
	// An explicit GOMEMLIMIT is the operator talking; do not argue.
	if _, set := os.LookupEnv("GOMEMLIMIT"); set {
		return
	}

	available, source := availableMemory()
	if available < minMemLimit {
		return
	}

	limit := int64(float64(available) * memLimitFraction)
	debug.SetMemoryLimit(limit)
	log.Printf("memory: aiming to stay under %d MiB of the %d MiB %s",
		limit>>20, available>>20, source)
}

// availableMemory reports how many bytes this process may use, and where that
// number came from.
func availableMemory() (int64, string) {
	if limit, ok := cgroupMemoryLimit(); ok {
		return limit, "its cgroup allows"
	}
	if total, ok := totalSystemMemory(); ok {
		return total, "this machine has"
	}
	return 0, ""
}

// cgroupMemoryLimit reads the memory ceiling of the cgroup this process is in.
//
// Only cgroup v2 is consulted. Every distribution SAND is deployed on — and
// Raspberry Pi OS in particular — has used the unified hierarchy for years, and
// a v1 fallback would be more code reading more files to answer the same
// question on machines that no longer exist.
func cgroupMemoryLimit() (int64, bool) {
	for _, path := range []string{
		// The limit as the process sees it from inside the cgroup namespace,
		// which is where a systemd service and a container both find theirs.
		"/sys/fs/cgroup/memory.max",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(raw))
		// "max" means no limit was set, which is not a number and not a limit.
		if text == "" || text == "max" {
			continue
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || value <= 0 {
			continue
		}
		return value, true
	}
	return 0, false
}

// totalSystemMemory reports the machine's physical memory.
func totalSystemMemory() (int64, bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}
