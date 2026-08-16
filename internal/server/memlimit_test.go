package server

import (
	"os"
	"runtime/debug"
	"testing"
)

// The machine's memory is the fallback when there is no cgroup limit, and on
// anything SAND runs on it should be readable.
func TestTotalSystemMemoryIsReadable(t *testing.T) {
	total, ok := totalSystemMemory()
	if !ok {
		t.Skip("no /proc/meminfo on this platform")
	}
	if total < minMemLimit {
		t.Errorf("total system memory reported as %d bytes, which cannot be right", total)
	}
}

// Whatever it finds, the limit it sets has to leave room: GOMEMLIMIT does not
// account for what the runtime holds outside the Go heap, so aiming at the
// ceiling would mean sailing past it.
func TestMemoryLimitLeavesHeadroom(t *testing.T) {
	available, source := availableMemory()
	if available == 0 {
		t.Skip("no memory limit discoverable here")
	}
	limit := int64(float64(available) * memLimitFraction)
	if limit >= available {
		t.Errorf("limit %d is not below the %d bytes %s", limit, available, source)
	}
	if limit <= 0 {
		t.Errorf("limit came out as %d", limit)
	}
}

// An operator who sets GOMEMLIMIT means it, so nothing here may overrule them.
func TestAnExplicitLimitIsLeftAlone(t *testing.T) {
	// A value low enough to be obviously ours if it changes, but high enough
	// that the collector is not left thrashing while the test runs.
	const explicit = 512 << 20

	before := debug.SetMemoryLimit(explicit)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	t.Setenv("GOMEMLIMIT", "512MiB")
	applyMemoryLimit()

	if got := debug.SetMemoryLimit(-1); got != explicit {
		t.Errorf("an explicitly set GOMEMLIMIT was overwritten: limit is now %d, want %d", got, explicit)
	}
}

// Nonsense in the cgroup file is ignored rather than obeyed. "max" is the
// common one — it is what the file says when no limit is set at all.
func TestAnAbsentCgroupLimitFallsBackToTheMachine(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/memory.max"); err == nil {
		// There is a real file here; the fallback is exercised by whatever it
		// says, and asserting on this machine's cgroup would be asserting on
		// the CI runner's configuration.
		t.Skip("this machine has a cgroup memory file")
	}
	if _, ok := cgroupMemoryLimit(); ok {
		t.Error("a limit was reported from a cgroup file that does not exist")
	}
	if _, source := availableMemory(); source != "this machine has" {
		t.Errorf("expected the fallback to the machine's own memory, got %q", source)
	}
}
