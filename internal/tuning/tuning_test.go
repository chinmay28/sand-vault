package tuning

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two presets are the formula over the machines they stand for, so the
// numbers a Pi and a workstation run on are stated here as the contract.
func TestPresetsAreTheFormulaOverTheirMachines(t *testing.T) {
	pi := Derive(pi5Machine)
	want := Tuning{
		ChunkSize: 16 * mib, UploadWindow: 4, ReadAhead: 1, ChunkCacheBytes: 1 * gib,
		CompressLevel: 3, CompressThreads: 2, TransferWindow: 4, EraseWindow: 4,
		ConnsPerHost: 12, RebuildLimit: 4 * gib, BundleLimit: 4 * gib,
	}
	if pi != want {
		t.Errorf("pi5 preset:\n got %+v\nwant %+v", pi, want)
	}

	ws := Derive(workstationMachine)
	want = Tuning{
		ChunkSize: 32 * mib, UploadWindow: 12, ReadAhead: 6, ChunkCacheBytes: 4 * gib,
		CompressLevel: 3, CompressThreads: 8, TransferWindow: 8, EraseWindow: 8,
		ConnsPerHost: 32, RebuildLimit: 16 * gib, BundleLimit: 16 * gib,
	}
	if ws != want {
		t.Errorf("workstation preset:\n got %+v\nwant %+v", ws, want)
	}
}

// Every derived tuning has to pass its own validation on its own machine, at
// every size a machine could plausibly be — the formula must never produce
// something the server would then refuse.
func TestDeriveIsAlwaysValid(t *testing.T) {
	for _, cpus := range []int{0, 1, 2, 4, 8, 16, 24, 64, 256} {
		for _, mem := range []int64{0, 512 * mib, 1 * gib, 4 * gib, 8 * gib, 16 * gib, 32 * gib, 64 * gib, 256 * gib, 2 << 40} {
			m := Machine{CPUs: cpus, MemoryBytes: mem}
			if err := Derive(m).Check(m); err != nil {
				t.Errorf("Derive(%+v) does not fit its own machine: %v", m, err)
			}
		}
	}
}

// Bigger never means smaller: the workstation is above the Pi on every knob,
// and a machine too small for the floors shrinks rather than refuses.
func TestDeriveIsMonotone(t *testing.T) {
	small, large := Derive(pi5Machine), Derive(workstationMachine)
	for _, k := range knobs {
		a, _ := small.Get(k.Name)
		b, _ := large.Get(k.Name)
		if b < a {
			t.Errorf("%s: workstation (%d) is below pi5 (%d)", k.Name, b, a)
		}
	}
	tiny := Derive(Machine{CPUs: 1, MemoryBytes: 512 * mib})
	if tiny.ChunkCacheBytes >= small.ChunkCacheBytes || tiny.UploadWindow > small.UploadWindow {
		t.Errorf("a 512 MiB machine was not given less than a Pi: %+v", tiny)
	}
}

// A machine that reports nothing is treated as a Pi rather than refused.
func TestDeriveWithNothingKnownIsThePi(t *testing.T) {
	if got, want := Derive(Machine{}), Derive(pi5Machine); got != want {
		t.Errorf("Derive(zero) = %+v, want the pi5 tuning %+v", got, want)
	}
}

// Get and Set cover every knob in the table, and round-trip through it.
func TestEveryKnobIsReadableAndWritable(t *testing.T) {
	base := Derive(pi5Machine)
	for _, k := range knobs {
		if _, ok := base.Get(k.Name); !ok {
			t.Errorf("%s: Get does not know it", k.Name)
		}
		next, ok := base.Set(k.Name, k.Max)
		if !ok {
			t.Errorf("%s: Set does not know it", k.Name)
			continue
		}
		if got, _ := next.Get(k.Name); got != k.Max {
			t.Errorf("%s: set %d, read back %d", k.Name, k.Max, got)
		}
		if next == base {
			t.Errorf("%s: Set changed nothing", k.Name)
		}
	}
	if _, ok := base.Set("no_such_knob", 1); ok {
		t.Error("Set accepted a knob that does not exist")
	}
	if _, ok := base.Get("no_such_knob"); ok {
		t.Error("Get answered for a knob that does not exist")
	}
}

// Validate names every problem, not the first.
func TestValidateNamesEveryProblem(t *testing.T) {
	bad := Derive(pi5Machine)
	bad.UploadWindow = 0
	bad.CompressLevel = 9
	bad.ChunkSize = 16*mib + 1
	err := bad.Validate()
	if err == nil {
		t.Fatal("an out-of-range tuning validated")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error is %T, want *ValidationError", err)
	}
	if len(verr.Problems) != 3 {
		t.Errorf("got %d problems, want 3: %v", len(verr.Problems), verr.Problems)
	}
	for _, want := range []string{"upload_window", "compress_level", "chunk_size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// Check refuses a tuning that would commit more than half the machine.
func TestCheckHoldsTheFootprintAgainstTheMachine(t *testing.T) {
	tiny := Machine{CPUs: 4, MemoryBytes: 2 * gib}
	tn := Derive(tiny)
	if err := tn.Check(tiny); err != nil {
		t.Fatalf("the derived tuning does not fit: %v", err)
	}
	tn.ChunkCacheBytes = 4 * gib
	if err := tn.Check(tiny); err == nil {
		t.Error("a 4 GiB cache on a 2 GiB machine passed")
	}
	// No memory known, nothing to hold it against.
	if err := tn.Check(Machine{}); err != nil {
		t.Errorf("a machine with unknown memory refused a tuning: %v", err)
	}
}

func TestResolveAutoUsesTheDetectedMachine(t *testing.T) {
	r, err := Resolve(Config{}, workstationMachine)
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != ProfileAuto {
		t.Errorf("profile = %q, want auto", r.Profile)
	}
	if r.Machine != workstationMachine || r.Detected != workstationMachine {
		t.Errorf("machine = %+v, detected = %+v", r.Machine, r.Detected)
	}
	if r.Effective != Derive(workstationMachine) {
		t.Errorf("effective = %+v, want the workstation tuning", r.Effective)
	}
	if len(r.Overrides) != 0 {
		t.Errorf("overrides = %v, want none", r.Overrides)
	}
}

func TestResolveFixedProfileIgnoresTheDetectedMachine(t *testing.T) {
	r, err := Resolve(Config{Profile: ProfilePi5}, workstationMachine)
	if err != nil {
		t.Fatal(err)
	}
	if r.Machine != pi5Machine {
		t.Errorf("machine = %+v, want the pi5 preset's", r.Machine)
	}
	if r.Detected != workstationMachine {
		t.Errorf("detected = %+v, want what was found", r.Detected)
	}
	if r.Effective != Derive(pi5Machine) {
		t.Errorf("effective = %+v, want the pi5 tuning", r.Effective)
	}
}

func TestResolveCustomNeedsAMachine(t *testing.T) {
	if _, err := Resolve(Config{Profile: ProfileCustom}, pi5Machine); err == nil {
		t.Error("a custom profile with no machine resolved")
	}
	given := Machine{CPUs: 8, MemoryBytes: 32 * gib}
	r, err := Resolve(Config{Profile: ProfileCustom, Machine: &given}, pi5Machine)
	if err != nil {
		t.Fatal(err)
	}
	if r.Machine != given {
		t.Errorf("machine = %+v, want the one given", r.Machine)
	}
}

func TestResolveAppliesOverridesAndDropsNoOps(t *testing.T) {
	c := Config{
		Profile: ProfilePi5,
		Overrides: map[string]int64{
			"read_ahead":   4,
			"chunk_size":   16 * mib, // the default already
			"erase_window": 8,
		},
	}
	r, err := Resolve(c, pi5Machine)
	if err != nil {
		t.Fatal(err)
	}
	if r.Effective.ReadAhead != 4 || r.Effective.EraseWindow != 8 {
		t.Errorf("overrides not applied: %+v", r.Effective)
	}
	if r.Defaults.ReadAhead != 1 {
		t.Errorf("defaults changed by an override: %+v", r.Defaults)
	}
	if _, kept := r.Overrides["chunk_size"]; kept {
		t.Error("an override equal to the default was kept as an override")
	}
	if len(r.Overrides) != 2 {
		t.Errorf("overrides = %v, want read_ahead and erase_window", r.Overrides)
	}
}

func TestResolveRefusesWhatItCannotApply(t *testing.T) {
	cases := map[string]Config{
		"unknown profile": {Profile: "mainframe"},
		"unknown knob":    {Overrides: map[string]int64{"turbo": 1}},
		"out of range":    {Overrides: map[string]int64{"upload_window": 0}},
		"does not fit":    {Profile: ProfilePi5, Overrides: map[string]int64{"chunk_cache_bytes": 12 * gib}},
	}
	for name, c := range cases {
		if _, err := Resolve(c, pi5Machine); err == nil {
			t.Errorf("%s: resolved", name)
		}
	}
}

// A workstation profile on a machine that is actually smaller is held to
// the smaller machine: the profile cannot conjure memory.
func TestResolveHoldsTheFootprintAgainstTheSmallerOfTheTwo(t *testing.T) {
	detected := Machine{CPUs: 4, MemoryBytes: 4 * gib}
	_, err := Resolve(Config{Profile: ProfileWorkstation}, detected)
	if err == nil {
		t.Error("the workstation tuning fit a 4 GiB machine")
	}
}

func TestLoadAndSaveRoundTrip(t *testing.T) {
	path := PathFor(filepath.Join(t.TempDir(), "vault.sand"))

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if c.Profile != "" || c.Machine != nil || len(c.Overrides) != 0 {
		t.Errorf("a missing file loaded as %+v, want the zero config", c)
	}

	given := Machine{CPUs: 8, MemoryBytes: 32 * gib}
	want := Config{Profile: ProfileCustom, Machine: &given, Overrides: map[string]int64{"read_ahead": 3}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != want.Profile || *got.Machine != given || got.Overrides["read_ahead"] != 3 {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("Save left %d files beside the vault, want just the config", len(entries))
	}
}

func TestLoadRefusesABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("a broken file loaded")
	}
	os.WriteFile(path, []byte(`{"profile":"mainframe"}`), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("an unknown profile loaded")
	}
}

// Detection reads what the kernel says, so it is tested against files of
// the test's own making rather than the machine the test happens to run on.
func TestDetectReadsCgroupAndMeminfo(t *testing.T) {
	dir := t.TempDir()
	cg, proc := filepath.Join(dir, "cgroup"), filepath.Join(dir, "proc")
	os.MkdirAll(cg, 0o755)
	os.MkdirAll(proc, 0o755)
	t.Cleanup(func() { cgroupRoot, procRoot = "/sys/fs/cgroup", "/proc" })
	cgroupRoot, procRoot = cg, proc

	os.WriteFile(filepath.Join(proc, "meminfo"), []byte("MemTotal:       16384000 kB\nMemFree: 1 kB\n"), 0o644)

	// No cgroup files at all: the machine's figures.
	mem, source := DetectMemory()
	if mem != 16384000<<10 || source != "this machine has" {
		t.Errorf("DetectMemory = %d, %q", mem, source)
	}
	if _, source := DetectCPUs(); source != "this machine has" {
		t.Errorf("DetectCPUs source = %q", source)
	}

	// A cgroup with no limit set reads as no limit.
	os.WriteFile(filepath.Join(cg, "memory.max"), []byte("max\n"), 0o644)
	os.WriteFile(filepath.Join(cg, "cpu.max"), []byte("max 100000\n"), 0o644)
	if mem, _ := DetectMemory(); mem != 16384000<<10 {
		t.Errorf("'max' was taken as a limit: %d", mem)
	}
	if _, source := DetectCPUs(); source != "this machine has" {
		t.Errorf("'max' was taken as a CPU quota")
	}

	// A real limit wins over the machine.
	os.WriteFile(filepath.Join(cg, "memory.max"), []byte("4294967296\n"), 0o644)
	os.WriteFile(filepath.Join(cg, "cpu.max"), []byte("150000 100000\n"), 0o644)
	mem, source = DetectMemory()
	if mem != 4*gib || source != "its cgroup allows" {
		t.Errorf("DetectMemory = %d, %q", mem, source)
	}
	cpus, source := DetectCPUs()
	if cpus != 2 || source != "its cgroup allows" {
		t.Errorf("DetectCPUs = %d, %q; a 1.5 CPU quota is two threads' worth", cpus, source)
	}

	m := Detect()
	if m.CPUs != 2 || m.MemoryBytes != 4*gib {
		t.Errorf("Detect = %+v", m)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 16 * mib: "16 MiB", 1536 * kib: "1 MiB",
		1 * gib: "1 GiB", 1536 * mib: "1.5 GiB", 4 * gib: "4 GiB",
	}
	for n, want := range cases {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
