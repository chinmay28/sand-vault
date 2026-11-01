// Package tuning sizes SAND to the machine it is running on.
//
// SAND was written for a Raspberry Pi, and every constant that decides how
// much is in flight or in memory at once — the chunk a file is cut into, how
// many chunks an upload keeps in the air, how much decrypted data a reader
// keeps — was chosen for one. The same binary on a workstation with sixteen
// times the memory and six times the cores was being held to the same
// numbers, which is safe and slow.
//
// This package is the one place those numbers come from. A Machine says what
// the process has to work with, a Tuning is every knob, Derive is the formula
// that turns one into the other, and a Config is what the operator chose: a
// profile, a machine of their own when the detected one is wrong, and any
// knob they set by hand. Nothing in here touches a vault; the vault, the
// compressor and the provider layer each take the Tuning handed to them.
package tuning

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Machine is what the process has to work with.
type Machine struct {
	// CPUs is how many threads the process may run at once.
	CPUs int `json:"cpus"`
	// MemoryBytes is how much memory the process may use — the cgroup's
	// ceiling when there is one, which is where a systemd unit's MemoryMax
	// lands, and the machine's total when there is not.
	MemoryBytes int64 `json:"memory_bytes"`
}

// cgroupRoot and procRoot are where the kernel's answers are read from. They
// are variables so a test can point them at a directory of its own making.
var (
	cgroupRoot = "/sys/fs/cgroup"
	procRoot   = "/proc"
)

// Detect reports the machine the process is running on.
//
// It never fails: a platform without cgroups or /proc gets the Go runtime's
// view of the CPU count and a memory figure of zero, which Derive treats as
// "assume a small machine" rather than as a reason to stop.
func Detect() Machine {
	cpus, _ := DetectCPUs()
	mem, _ := DetectMemory()
	return Machine{CPUs: cpus, MemoryBytes: mem}
}

// DetectMemory reports how many bytes this process may use, and where that
// number came from: "its cgroup allows" or "this machine has". Zero and an
// empty source when neither could be read.
func DetectMemory() (int64, string) {
	if limit, ok := cgroupMemoryLimit(); ok {
		return limit, "its cgroup allows"
	}
	if total, ok := totalSystemMemory(); ok {
		return total, "this machine has"
	}
	return 0, ""
}

// DetectCPUs reports how many threads the process may run at once, and where
// the number came from. A cgroup CPU quota — a unit's CPUQuota — is honoured
// when it is set and smaller than the machine; otherwise it is the runtime's
// count, which already respects an affinity mask.
func DetectCPUs() (int, string) {
	cpus := runtime.NumCPU()
	if quota, ok := cgroupCPUQuota(); ok && quota < cpus {
		return quota, "its cgroup allows"
	}
	return cpus, "this machine has"
}

// cgroupMemoryLimit reads the memory ceiling of the cgroup this process is in.
//
// Only cgroup v2 is consulted. Every distribution SAND is deployed on — and
// Raspberry Pi OS in particular — has used the unified hierarchy for years.
func cgroupMemoryLimit() (int64, bool) {
	text, ok := readTrimmed(cgroupRoot + "/memory.max")
	if !ok || text == "" || text == "max" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

// cgroupCPUQuota reads the CPU ceiling of the cgroup this process is in, as a
// whole number of CPUs rounded up: "150000 100000" is one and a half CPUs,
// which is two threads' worth of work to schedule.
func cgroupCPUQuota() (int, bool) {
	text, ok := readTrimmed(cgroupRoot + "/cpu.max")
	if !ok {
		return 0, false
	}
	fields := strings.Fields(text)
	if len(fields) != 2 || fields[0] == "max" {
		return 0, false
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || quota <= 0 {
		return 0, false
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, false
	}
	cpus := int((quota + period - 1) / period)
	if cpus < 1 {
		cpus = 1
	}
	return cpus, true
}

// totalSystemMemory reports the machine's physical memory.
func totalSystemMemory() (int64, bool) {
	raw, err := os.ReadFile(procRoot + "/meminfo")
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

func readTrimmed(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}
