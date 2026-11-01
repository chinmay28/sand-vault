package server

import (
	"log"
	"os"
	"runtime/debug"

	"github.com/chinmay28/sand-vault/internal/tuning"
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
// number came from — the cgroup's ceiling when there is one, the machine's
// total when there is not. The reading lives in internal/tuning, which sizes
// everything else to the same figure.
func availableMemory() (int64, string) {
	return tuning.DetectMemory()
}
