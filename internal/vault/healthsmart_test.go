package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

func TestHealthScheduleSmartIsOnUnlessSwitchedOff(t *testing.T) {
	v, _ := newTestVault(t, 1)

	// On by default: the quickstart installs the scanner, and a vault
	// without one is told so once rather than warned on every account.
	if !v.HealthSchedule().Smart {
		t.Fatal("a fresh vault has the disk check off")
	}

	saved, err := v.SetHealthSchedule(HealthSchedule{Enabled: true})
	if err != nil {
		t.Fatalf("SetHealthSchedule: %v", err)
	}
	if saved.Smart || v.HealthSchedule().Smart {
		t.Error("switching the disk check off did not stick")
	}

	if saved, err = v.SetHealthSchedule(HealthSchedule{Enabled: true, Smart: true}); err != nil {
		t.Fatalf("SetHealthSchedule: %v", err)
	}
	if !saved.Smart {
		t.Error("switching the disk check back on did not stick")
	}
}

// A scanner that was never installed is one line about the scanner, not a
// warning on every account — and no line at all when nothing sits on a disk
// it would have read.
func TestAMissingDiskReportIsSaidOnce(t *testing.T) {
	v, roots := newTestVault(t, 2)
	t.Setenv("SAND_SMART_REPORT", filepath.Join(t.TempDir(), "never-written.json"))

	health, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	for _, cloud := range health.Clouds {
		if cloud.Disk != nil || cloud.DiskWarning != "" {
			t.Errorf("%s carries a disk warning for a missing report: %+v", cloud.Name, cloud)
		}
	}
	if health.Warnings != 0 {
		t.Errorf("Warnings = %d, want none for a missing report", health.Warnings)
	}

	device := deviceUnder(t, roots[0])
	onDisk := strings.HasPrefix(device, "/dev/") &&
		!strings.HasPrefix(device, "/dev/dm-") && !strings.HasPrefix(device, "/dev/mapper/")
	switch {
	case onDisk && !strings.Contains(health.DiskScanner, "not installed"):
		t.Errorf("DiskScanner = %q, want the missing scanner named once", health.DiskScanner)
	case !onDisk && health.DiskScanner != "":
		t.Errorf("DiskScanner = %q on %s, where SMART does not apply", health.DiskScanner, device)
	}

	// Switched off, there is nothing to say either way.
	if _, err := v.SetHealthSchedule(HealthSchedule{Enabled: true}); err != nil {
		t.Fatalf("SetHealthSchedule: %v", err)
	}
	if health, _ = v.CloudHealth(); health.DiskScanner != "" {
		t.Errorf("DiskScanner = %q with the check off", health.DiskScanner)
	}
}

// The whole join, through a real vault: a local account's folder, the mount
// table under it, a scanner's report naming that disk with pending sectors,
// and the health report warning about the right account.
func TestReportWarnsOnTheDiskUnderTheAccount(t *testing.T) {
	v, roots := newTestVault(t, 1)

	// The test directory has to sit on a plain physical disk for the join to
	// have anything to say — on a tmpfs /tmp or an LVM root there is no disk
	// to name, which the feature treats as "does not apply" by design.
	device := deviceUnder(t, roots[0])
	if !strings.HasPrefix(device, "/dev/") ||
		strings.HasPrefix(device, "/dev/dm-") || strings.HasPrefix(device, "/dev/mapper/") {
		t.Skipf("test dir is on %q, which SMART does not apply to", device)
	}
	name := strings.TrimPrefix(device, "/dev/")

	reportPath := filepath.Join(t.TempDir(), "smart.json")
	report := provider.DiskReport{
		TakenAt: time.Now(),
		Disks: []provider.DiskSmart{{
			Device:         "disk-under-test",
			Partitions:     []string{name},
			Passed:         true,
			PendingSectors: 32,
		}},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAND_SMART_REPORT", reportPath)

	// Off: the report exists, the disk is bad, and nothing says so — the
	// toggle is the user's, not the scanner's.
	if _, err := v.SetHealthSchedule(HealthSchedule{Enabled: true}); err != nil {
		t.Fatalf("SetHealthSchedule: %v", err)
	}
	health, err := v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	if health.Clouds[0].Disk != nil || health.Clouds[0].DiskWarning != "" || health.Warnings != 0 || health.DiskScanner != "" {
		t.Fatalf("disk facts shown with the check off: %+v %q", health.Clouds[0], health.DiskScanner)
	}

	if _, err := v.SetHealthSchedule(HealthSchedule{Enabled: true, Smart: true}); err != nil {
		t.Fatalf("SetHealthSchedule: %v", err)
	}
	health, err = v.CloudHealth()
	if err != nil {
		t.Fatalf("CloudHealth: %v", err)
	}
	cloud := health.Clouds[0]
	if cloud.Disk == nil || cloud.Disk.Device != "disk-under-test" {
		t.Fatalf("the account did not join to its disk: %+v", cloud)
	}
	if !strings.Contains(cloud.DiskWarning, "32 sectors pending") {
		t.Errorf("DiskWarning = %q, want the pending sectors in it", cloud.DiskWarning)
	}
	if health.Warnings != 1 {
		t.Errorf("Warnings = %d, want 1", health.Warnings)
	}
	if health.DiskScanner != "" {
		t.Errorf("DiskScanner = %q, want nothing said about a fresh report", health.DiskScanner)
	}
}

// deviceUnder reads /proc/self/mounts for the device the path sits on — the
// same longest-prefix walk the join does, kept deliberately independent of it
// so the test does not vouch for the code with the code.
func deviceUnder(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Skipf("no /proc/self/mounts: %v", err)
	}
	best, source := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		point := fields[1]
		if point != "/" && !strings.HasPrefix(path, point+"/") && path != point {
			continue
		}
		if len(point) >= len(best) {
			best, source = point, fields[0]
		}
	}
	return source
}
