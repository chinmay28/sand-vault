package provider

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A trimmed, real smartctl -j answer: a USB drive that passes its own overall
// assessment while carrying 32 pending sectors and a self-test that failed —
// which is exactly the disk this feature exists to warn about, a drive that
// looks fine from the filesystem and is not.
const ataFixture = `{
  "model_name": "WDC WD50NDZW-11BCSS0",
  "serial_number": "WD-WX82D85M5TY8",
  "smart_status": {"passed": true},
  "temperature": {"current": 33},
  "power_on_time": {"hours": 5581},
  "ata_smart_attributes": {"table": [
    {"id": 5, "raw": {"value": 0}},
    {"id": 9, "raw": {"value": 36507225421}},
    {"id": 197, "raw": {"value": 32}},
    {"id": 198, "raw": {"value": 0}},
    {"id": 199, "raw": {"value": 0}}
  ]},
  "ata_smart_self_test_log": {"standard": {"table": [
    {"status": {"string": "Completed: read failure", "passed": false}, "lba": 4286519039},
    {"status": {"string": "Completed without error", "passed": true}}
  ]}}
}`

const nvmeFixture = `{
  "model_name": "Samsung SSD 990 PRO",
  "smart_status": {"passed": true},
  "temperature": {"current": 41},
  "power_on_time": {"hours": 1200},
  "nvme_smart_health_information_log": {
    "critical_warning": 0,
    "media_errors": 3,
    "percentage_used": 95
  }
}`

// What an SD card answers: smartctl reached it, and there is no SMART behind
// it. The absence of a verdict must read as "nothing known", never "healthy".
const unsupportedFixture = `{"model_name": "SD Card Reader"}`

func TestParseSmartctlATA(t *testing.T) {
	d, err := ParseSmartctl([]byte(ataFixture))
	if err != nil {
		t.Fatalf("ParseSmartctl: %v", err)
	}
	if !d.Passed || d.Unsupported {
		t.Errorf("a drive whose own verdict is pass parsed as passed=%v unsupported=%v", d.Passed, d.Unsupported)
	}
	if d.PendingSectors != 32 || d.ReallocatedSectors != 0 {
		t.Errorf("attributes misread: pending=%d reallocated=%d", d.PendingSectors, d.ReallocatedSectors)
	}
	if !d.SelfTestFailed || d.SelfTest != "Completed: read failure" {
		t.Errorf("newest self-test entry misread: failed=%v %q", d.SelfTestFailed, d.SelfTest)
	}
	if d.TemperatureC != 33 || d.PowerOnHours != 5581 {
		t.Errorf("temperature/hours misread: %d°C %dh", d.TemperatureC, d.PowerOnHours)
	}

	problem := d.Problem()
	for _, want := range []string{"32 sectors pending", "self-test failed"} {
		if !strings.Contains(problem, want) {
			t.Errorf("Problem() = %q, missing %q", problem, want)
		}
	}
}

func TestParseSmartctlNVMe(t *testing.T) {
	d, err := ParseSmartctl([]byte(nvmeFixture))
	if err != nil {
		t.Fatalf("ParseSmartctl: %v", err)
	}
	if d.MediaErrors != 3 || d.PercentUsed != 95 {
		t.Errorf("nvme log misread: media=%d used=%d%%", d.MediaErrors, d.PercentUsed)
	}
	problem := d.Problem()
	for _, want := range []string{"3 media errors", "95% of rated life"} {
		if !strings.Contains(problem, want) {
			t.Errorf("Problem() = %q, missing %q", problem, want)
		}
	}
}

func TestParseSmartctlUnsupported(t *testing.T) {
	d, err := ParseSmartctl([]byte(unsupportedFixture))
	if err != nil {
		t.Fatalf("ParseSmartctl: %v", err)
	}
	if !d.Unsupported {
		t.Error("a device with no smart_status parsed as supported")
	}
	if d.Problem() != "" {
		t.Errorf("an unsupported device reports a problem: %q", d.Problem())
	}
}

func TestProblemSaysNothingWhenNothingIsWrong(t *testing.T) {
	d := DiskSmart{Passed: true}
	if got := d.Problem(); got != "" {
		t.Errorf("a clean disk reports %q", got)
	}
}

func TestMountSourceFor(t *testing.T) {
	table := "/dev/root / ext4 rw 0 0\n" +
		"tmpfs /tmp tmpfs rw 0 0\n" +
		"/dev/sda1 /media/user/Old ext4 rw 0 0\n" +
		"/dev/sdb1 /media/user/Old ext4 rw 0 0\n" + // mounted over: later wins
		"/dev/sdc1 /media/user/My\\040Drive ext4 rw 0 0\n"

	cases := []struct{ path, want string }{
		{"/home/user/folder", "/dev/root"},
		{"/tmp/sand", "tmpfs"},
		{"/media/user/Old/SANDVault", "/dev/sdb1"},
		{"/media/user/My Drive/SANDVault", "/dev/sdc1"},
	}
	for _, c := range cases {
		if got := mountSourceFor(table, c.path); got != c.want {
			t.Errorf("mountSourceFor(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestDiskHealthIn(t *testing.T) {
	table := "/dev/root / ext4 rw 0 0\n" +
		"tmpfs /tmp tmpfs rw 0 0\n" +
		"/dev/mapper/vg-data /data ext4 rw 0 0\n" +
		"/dev/sda1 /media/user/Drive ext4 rw 0 0\n"
	now := time.Now()
	report := DiskReport{
		TakenAt: now.Add(-time.Hour),
		Disks: []DiskSmart{
			{Device: "sda", Partitions: []string{"sda1"}, Passed: true, PendingSectors: 32},
		},
	}

	// A folder on a physical disk joins to it through its partition, and the
	// disk's problem is the warning.
	disk, warn := diskHealthIn("/media/user/Drive/SANDVault", table, "r.json", report, nil, now)
	if disk == nil || disk.Device != "sda" {
		t.Fatalf("no join to sda: disk=%v", disk)
	}
	if !strings.Contains(warn, "32 sectors pending") {
		t.Errorf("warning = %q, want the pending sectors in it", warn)
	}

	// Not on a physical disk: not a warning, not anything.
	for _, path := range []string{"/tmp/sand", "/data/sand"} {
		if disk, warn := diskHealthIn(path, table, "r.json", report, nil, now); disk != nil || warn != "" {
			t.Errorf("diskHealthIn(%q) = %v, %q — SMART does not apply there", path, disk, warn)
		}
	}

	// A missing report is a scanner that was never installed, and the warning
	// carries the repair.
	_, warn = diskHealthIn("/media/user/Drive/SANDVault", table, "r.json", DiskReport{}, errors.New("no such file"), now)
	if !strings.Contains(warn, "enable-disk-health.sh") {
		t.Errorf("missing report warns %q, want the install step in it", warn)
	}

	// A stale report is a scanner that has stopped.
	old := report
	old.TakenAt = now.Add(-3 * 24 * time.Hour)
	_, warn = diskHealthIn("/media/user/Drive/SANDVault", table, "r.json", old, nil, now)
	if !strings.Contains(warn, "timer") {
		t.Errorf("stale report warns %q, want the timer named", warn)
	}

	// A disk the report has never seen — plugged in since the last scan.
	fresh := DiskReport{TakenAt: now, Disks: []DiskSmart{{Device: "sdb", Partitions: []string{"sdb1"}, Passed: true}}}
	_, warn = diskHealthIn("/media/user/Drive/SANDVault", table, "r.json", fresh, nil, now)
	if !strings.Contains(warn, "no entry for sda1") {
		t.Errorf("unknown disk warns %q, want it named", warn)
	}

	// A disk smartctl could not read at all.
	broken := DiskReport{TakenAt: now, Disks: []DiskSmart{{Device: "sda", Partitions: []string{"sda1"}, Error: "device timed out"}}}
	_, warn = diskHealthIn("/media/user/Drive/SANDVault", table, "r.json", broken, nil, now)
	if !strings.Contains(warn, "device timed out") {
		t.Errorf("unreadable disk warns %q, want the reason in it", warn)
	}

	// A clean disk: joined, shown, and no warning to raise.
	clean := DiskReport{TakenAt: now, Disks: []DiskSmart{{Device: "sda", Partitions: []string{"sda1"}, Passed: true, TemperatureC: 34}}}
	disk, warn = diskHealthIn("/media/user/Drive/SANDVault", table, "r.json", clean, nil, now)
	if disk == nil || warn != "" {
		t.Errorf("clean disk: disk=%v warn=%q, want the disk and silence", disk, warn)
	}
}
