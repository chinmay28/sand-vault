package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Whether the disk under a local folder is quietly dying.
//
// The health check pings every account, and a local folder answers that ping
// for as long as the filesystem does — which is long after the disk has
// started to go. A drive with unreadable sectors still mounts, still lists,
// still serves every part that does not sit on the damage, and the first
// anything notices is a part that cannot be read back, by which time the
// question is how many others are on the same patch. The disk has known for
// weeks: it keeps counters for exactly this — sectors it could not read,
// sectors it has already given up on and remapped — behind SMART, and they
// move before files break.
//
// SAND cannot read those counters itself. The service runs sandboxed —
// PrivateDevices=true, so there is no /dev to open — and that is the right
// trade: a network-facing service holding cloud credentials has no business
// with raw disk access. So the reading happens outside: a root-run one-shot
// (sand smart-scan, installed as a systemd timer by scripts/quickstart.sh, or
// by scripts/enable-disk-health.sh on an install made some other way) asks
// smartctl about every physical disk and writes what it found to a small JSON
// file in the data directory. What this file implements is the sandboxed
// half: reading that report back and joining it to a local account's folder
// through the mount table, so the health panel can put "32 sectors pending
// reallocation" next to the account sitting on them.
//
// The join is deliberately loose about what it does not know. A folder on a
// network share, a tmpfs, or under a device-mapper volume has no single
// physical disk to speak for, and answers with nothing rather than a warning
// — SMART not applying is not a problem to fix. A report that is missing or
// stale, on the other hand, is a problem with a repair (install the scanner;
// check the timer), and each account on a physical disk says so, because a
// check that defaults on above a scanner that was never installed would
// otherwise read as a clean bill of health for that drive. Accounts on
// network shares say nothing either way: a scanner that nothing would read is
// not news.

// DefaultDiskReportPath is where the scanner writes and the service reads when
// nobody says otherwise: the data directory the systemd unit grants, so the
// sandboxed service can read it without any new ReadWritePaths line.
const DefaultDiskReportPath = "/var/lib/sand/smart.json"

// DiskReportMaxAge is how old the report may be before the join stops trusting
// it quietly. The timer the installer writes runs hourly; a report a day old
// is two dozen missed slots, which is a scanner that has stopped, not one that
// is running a little behind.
const DiskReportMaxAge = 24 * time.Hour

// DiskReportPath answers where the report lives, honouring the override a
// development run outside systemd needs (the scanner takes the same path via
// --out).
func DiskReportPath() string {
	if p := os.Getenv("SAND_SMART_REPORT"); p != "" {
		return p
	}
	return DefaultDiskReportPath
}

// DiskReport is what one run of the scanner found: every physical disk on the
// machine, at one moment.
type DiskReport struct {
	TakenAt time.Time   `json:"taken_at"`
	Disks   []DiskSmart `json:"disks"`
}

// DiskSmart is one disk's own account of its health, distilled from smartctl
// to the counters that move before files break. Zero in any counter means
// "none" and "not reported" alike — the distinction never changes what anyone
// does, and only values above zero are ever acted on.
type DiskSmart struct {
	// Device is the kernel's name for the disk — "sda" — and Partitions are
	// the names under it at scan time. They are the join key to the mount
	// table, and they are only as stable as the bus: a drive replugged since
	// the scan may have moved, which the join reports as "no entry" and the
	// next scan repairs.
	Device     string   `json:"device"`
	Partitions []string `json:"partitions,omitempty"`

	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`

	// Unsupported marks a device smartctl could reach but that keeps no SMART
	// data — an SD card, most USB thumb drives. Not a worry, just a disk with
	// nothing to say.
	Unsupported bool `json:"unsupported,omitempty"`

	// Passed is the drive's own overall verdict. A drive that says no here has
	// tripped its own failure thresholds and is not asking for a second
	// opinion.
	Passed bool `json:"passed"`

	// The ATA counters worth watching. Pending sectors are ones the drive
	// could not read and is waiting to remap on the next write — they are the
	// earliest signal and the one that maps directly onto "a part on this
	// patch will not read back". Reallocated and uncorrectable are the later
	// chapters of the same story. CRC errors are the odd one out: they count
	// corruption on the cable, not the platter, and their repair is a new
	// cable.
	PendingSectors       int64 `json:"pending_sectors,omitempty"`
	ReallocatedSectors   int64 `json:"reallocated_sectors,omitempty"`
	UncorrectableSectors int64 `json:"uncorrectable_sectors,omitempty"`
	CRCErrors            int64 `json:"crc_errors,omitempty"`

	// The NVMe equivalents, from the health log of a drive that has one.
	MediaErrors     int64 `json:"media_errors,omitempty"`
	CriticalWarning int   `json:"critical_warning,omitempty"`
	PercentUsed     int   `json:"percent_used,omitempty"`

	TemperatureC int   `json:"temperature_c,omitempty"`
	PowerOnHours int64 `json:"power_on_hours,omitempty"`

	// SelfTest is the most recent entry in the drive's self-test log, as the
	// drive words it — "Completed without error", "Completed: read failure" —
	// and SelfTestFailed is that entry's own verdict. Self-tests are the one
	// signal here somebody has to start by hand (smartctl -t long), so most
	// disks report nothing.
	SelfTest       string `json:"self_test,omitempty"`
	SelfTestFailed bool   `json:"self_test_failed,omitempty"`

	// Error records a disk smartctl could not read at all, with the reason.
	Error string `json:"error,omitempty"`
}

// Problem distills the counters into the sentence the health panel shows, or
// "" for a disk with nothing wrong. Several problems come back joined — a
// failing drive usually fails several counters at once, and dropping all but
// one would understate it.
func (d DiskSmart) Problem() string {
	if d.Unsupported || d.Error != "" {
		return ""
	}
	var out []string
	if !d.Passed {
		out = append(out, "the drive's own health assessment reports failure — replace it")
	}
	if d.SelfTestFailed {
		out = append(out, "the last self-test failed ("+d.SelfTest+")")
	}
	if d.PendingSectors > 0 {
		out = append(out, fmt.Sprintf("%d sectors pending reallocation — unreadable until something rewrites them", d.PendingSectors))
	}
	if d.ReallocatedSectors > 0 {
		out = append(out, fmt.Sprintf("%d sectors reallocated", d.ReallocatedSectors))
	}
	if d.UncorrectableSectors > 0 {
		out = append(out, fmt.Sprintf("%d sectors uncorrectable", d.UncorrectableSectors))
	}
	if d.CRCErrors > 0 {
		out = append(out, fmt.Sprintf("%d interface CRC errors — usually the cable, not the disk", d.CRCErrors))
	}
	if d.CriticalWarning != 0 {
		out = append(out, "the controller reports a critical warning")
	}
	if d.MediaErrors > 0 {
		out = append(out, fmt.Sprintf("%d media errors", d.MediaErrors))
	}
	if d.PercentUsed >= 90 {
		out = append(out, fmt.Sprintf("%d%% of rated life used", d.PercentUsed))
	}
	return strings.Join(out, "; ")
}

// ParseSmartctl distills smartctl's JSON (-j) for one device. It is called by
// the scanner, but lives here so the service, the scanner and the tests all
// agree on what the fields mean.
//
// smartctl's exit code is a bitfield in which several bits mean "I answered
// and the answer is bad news", so the caller must parse whatever JSON came
// out rather than gate on the code; this function only needs the bytes.
func ParseSmartctl(out []byte) (DiskSmart, error) {
	var raw struct {
		ModelName    string `json:"model_name"`
		SerialNumber string `json:"serial_number"`
		SmartStatus  *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Temperature struct {
			Current int `json:"current"`
		} `json:"temperature"`
		PowerOnTime struct {
			Hours int64 `json:"hours"`
		} `json:"power_on_time"`
		AtaSmartAttributes struct {
			Table []struct {
				ID  int `json:"id"`
				Raw struct {
					Value int64 `json:"value"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
		AtaSmartSelfTestLog struct {
			Standard struct {
				Table []struct {
					Status struct {
						String string `json:"string"`
						Passed bool   `json:"passed"`
					} `json:"status"`
				} `json:"table"`
			} `json:"standard"`
		} `json:"ata_smart_self_test_log"`
		NVMeLog *struct {
			CriticalWarning int   `json:"critical_warning"`
			MediaErrors     int64 `json:"media_errors"`
			PercentageUsed  int   `json:"percentage_used"`
		} `json:"nvme_smart_health_information_log"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return DiskSmart{}, fmt.Errorf("parsing smartctl output: %w", err)
	}

	d := DiskSmart{
		Model:        raw.ModelName,
		Serial:       raw.SerialNumber,
		TemperatureC: raw.Temperature.Current,
		PowerOnHours: raw.PowerOnTime.Hours,
	}

	// No overall verdict is what an SD card or a thumb drive looks like:
	// smartctl reached the device, and the device keeps no SMART data to
	// report. Everything below would read as zeros — which is why the flag
	// exists, so "nothing wrong" and "nothing known" stay different answers.
	if raw.SmartStatus == nil {
		d.Unsupported = true
		return d, nil
	}
	d.Passed = raw.SmartStatus.Passed

	for _, attr := range raw.AtaSmartAttributes.Table {
		// The raw value of some vendor attributes packs several counters into
		// one integer's high bytes. The four read here are plain counts on
		// every drive in circulation, but the mask keeps a packed one from
		// reading as billions.
		value := attr.Raw.Value & 0xFFFFFFFF
		switch attr.ID {
		case 5:
			d.ReallocatedSectors = value
		case 197:
			d.PendingSectors = value
		case 198:
			d.UncorrectableSectors = value
		case 199:
			d.CRCErrors = value
		}
	}

	// The log is newest first, and only the newest entry is news: a failure
	// three tests ago that the last test did not reproduce was a patch that
	// has since been rewritten.
	if tests := raw.AtaSmartSelfTestLog.Standard.Table; len(tests) > 0 {
		d.SelfTest = tests[0].Status.String
		d.SelfTestFailed = !tests[0].Status.Passed
	}

	if raw.NVMeLog != nil {
		d.CriticalWarning = raw.NVMeLog.CriticalWarning
		d.MediaErrors = raw.NVMeLog.MediaErrors
		d.PercentUsed = raw.NVMeLog.PercentageUsed
	}
	return d, nil
}

// ReadDiskReport reads what the scanner last wrote.
func ReadDiskReport(path string) (DiskReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DiskReport{}, err
	}
	var report DiskReport
	if err := json.Unmarshal(data, &report); err != nil {
		return DiskReport{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return report, nil
}

// DiskHealth is the scanner's report as one health report reads it: once,
// with the mount table beside it, and then asked about each local folder in
// turn. Reading it per account would cost a file and a mount-table walk per
// account for the same answer.
type DiskHealth struct {
	path   string
	report DiskReport
	err    error
	table  string
	now    time.Time
}

// ReadDiskHealth reads the report and the mount table for a health report
// about to be built.
func ReadDiskHealth() *DiskHealth {
	table, _ := os.ReadFile("/proc/self/mounts")
	path := DiskReportPath()
	report, err := ReadDiskReport(path)
	return newDiskHealth(string(table), path, report, err, time.Now())
}

// newDiskHealth is ReadDiskHealth against inputs already in hand, which is the
// form the tests exercise.
func newDiskHealth(table, path string, report DiskReport, err error, now time.Time) *DiskHealth {
	return &DiskHealth{path: path, report: report, err: err, table: table, now: now}
}

// For joins a local folder to the disk under it and answers with that disk's
// standing: the disk's own report when the scanner has one, and a warning when
// there is something to say — about the disk, or about the report itself.
//
// Both answers empty means SMART simply does not apply here: a network share,
// a tmpfs, a mapped volume, or a machine with no mount table to read. That is
// a fact, not a finding, and it must not colour anything amber.
func (h *DiskHealth) For(root string) (*DiskSmart, string) {
	if h.table == "" {
		// Not Linux, or no proc. The same non-answer as a network share.
		return nil, ""
	}
	source := mountSourceFor(h.table, root)
	if !strings.HasPrefix(source, "/dev/") {
		// tmpfs, NFS, cifs — no physical disk of its own to ask after.
		return nil, ""
	}
	name := strings.TrimPrefix(source, "/dev/")
	if strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "mapper/") {
		// A device-mapper volume — LVM, LUKS — can span several disks, and
		// naming one of them would claim more than the join knows. Resolving
		// the stack through /sys is a job for a future that needs it.
		return nil, ""
	}

	if h.err != nil {
		// A missing report is a scanner that was never installed or has
		// never run, and the warning carries the repair.
		return nil, "no disk report at " + h.path +
			" — the SMART scanner is not installed or has never run." +
			" The quickstart installs it: re-run the install one-liner, or on an install made" +
			" another way, sudo scripts/enable-disk-health.sh"
	}

	// A stale report is a scanner that has stopped; the disk it names still
	// speaks for itself, under that caveat.
	stale := ""
	if age := h.now.Sub(h.report.TakenAt); age > DiskReportMaxAge {
		stale = fmt.Sprintf("the disk report is %s old — the sand-smart timer does not seem to be running"+
			" (systemctl status sand-smart.timer). ", age.Round(time.Hour))
	}

	for _, disk := range h.report.Disks {
		if disk.Device != name && !contains(disk.Partitions, name) {
			continue
		}
		found := disk
		if found.Error != "" {
			return &found, stale + "smartctl could not read " + found.Device + ": " + found.Error
		}
		return &found, strings.TrimSpace(stale + found.Problem())
	}
	return nil, stale + "the disk report has no entry for " + name +
		" — taken before the drive was plugged in? The next scan will include it."
}

func contains(list []string, want string) bool {
	for _, one := range list {
		if one == want {
			return true
		}
	}
	return false
}

// mountSourceFor answers which device the mount under path is backed by —
// "/dev/sda1", "tmpfs", "//nas/share" — by the same longest-prefix,
// later-entries-win walk mountedReadOnlyIn does, because it is reading the
// same table for a different column.
func mountSourceFor(table, path string) string {
	best, source := "", ""
	for _, line := range strings.Split(table, "\n") {
		// device mountpoint fstype options dump pass
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		point := unescapeMount(fields[1])
		if point != "/" && !strings.HasPrefix(path, point+"/") && path != point {
			continue
		}
		if len(point) < len(best) {
			continue
		}
		best = point
		source = unescapeMount(fields[0])
	}
	return source
}
