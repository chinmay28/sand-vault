package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/spf13/cobra"
)

// The privileged half of the disk health check.
//
// The service cannot read SMART itself — it runs with PrivateDevices=true, and
// weakening that so a network-facing process can send raw commands to every
// disk in the machine would be trading the vault's hardening for a feature.
// So the reading is a separate, tiny, root-run job with a fixed task: ask
// smartctl about every physical disk, distill the answer, write one small JSON
// file where the service can read it. scripts/enable-disk-health.sh installs
// it as a systemd timer; the service's health check does the joining and the
// warning (see internal/provider/disksmart.go).

// smartctlTimeout bounds one disk's answer. SMART reads are milliseconds on a
// healthy drive; a drive that takes longer than this is usually one hanging on
// bad sectors, and the scan must outlive it to report the others.
const smartctlTimeout = 30 * time.Second

func smartScanCmd() *cobra.Command {
	var (
		out      string
		smartctl string
	)

	cmd := &cobra.Command{
		Use:   "smart-scan",
		Short: "Read every disk's SMART health and write a report for the service",
		Long: `Ask smartctl about every physical disk on this machine and write what they
said to a small JSON report. The SAND service reads that report during its
cloud health check and warns on any Local folder account whose disk is
reporting trouble — pending sectors, failed self-tests, a drive that has
tripped its own failure thresholds.

Needs root (SMART is a raw device read) and smartmontools installed. Meant to
be run by the sand-smart systemd timer that scripts/enable-disk-health.sh
installs, not by hand — though running it by hand works and is how to check
what it sees.

The report carries facts, not history: each run replaces the last. Disks that
keep no SMART data — SD cards, most USB sticks — are recorded as unsupported
rather than as healthy, so nothing reads "no data" as "no problem".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSmartScan(cmd.Context(), smartctl, out, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&out, "out", provider.DefaultDiskReportPath,
		"where to write the report (the service reads this path, or $SAND_SMART_REPORT)")
	cmd.Flags().StringVar(&smartctl, "smartctl", "smartctl",
		"the smartctl binary to run")
	return cmd
}

func runSmartScan(ctx context.Context, smartctl, out string, log interface{ Write([]byte) (int, error) }) error {
	if _, err := exec.LookPath(smartctl); err != nil {
		return fmt.Errorf("%s is not installed — apt install smartmontools (or the distribution's equivalent)", smartctl)
	}

	disks, err := physicalDisks()
	if err != nil {
		return err
	}

	report := provider.DiskReport{TakenAt: time.Now()}
	for _, name := range disks {
		one := scanDisk(ctx, smartctl, name)
		report.Disks = append(report.Disks, one)

		line := "ok"
		switch {
		case one.Error != "":
			line = "error: " + one.Error
		case one.Unsupported:
			line = "no SMART data (SD card or USB stick, probably)"
		case one.Problem() != "":
			line = one.Problem()
		}
		fmt.Fprintf(log, "%s (%s): %s\n", name, orDash(one.Model), line)
	}

	if err := writeReport(out, report); err != nil {
		return err
	}
	fmt.Fprintf(log, "report written to %s\n", out)
	return nil
}

// physicalDisks names the disks worth asking: every entry in /sys/block that
// is backed by an actual device. The virtual ones — loop, ram, zram, dm-*, md*
// — either have no device directory at all or nothing SMART behind it, and
// the name prefixes cut the rest.
func physicalDisks() ([]string, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, fmt.Errorf("listing /sys/block: %w (this scan only works on Linux)", err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if hasAnyPrefix(name, "loop", "ram", "zram", "dm-", "md", "sr") {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/block", name, "device")); err != nil {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// scanDisk asks smartctl about one disk and distills the answer, folding in
// the partition names the join needs. smartctl's exit code is a bitfield where
// several bits mean "answered, and the answer is bad" — so whatever JSON came
// out is parsed first, and the exit code only matters when there was none.
//
// A disk that answers with no SMART section is asked once more as SAT: most of
// these are USB enclosures whose bridge smartctl does not know, and the ATA
// pass-through nearly every bridge speaks gets the real answer out of them. A
// device that still has nothing to say after that — an SD card, a thumb drive
// — genuinely keeps no SMART data, and stays recorded as unsupported.
func scanDisk(ctx context.Context, smartctl, name string) provider.DiskSmart {
	ctx, cancel := context.WithTimeout(ctx, smartctlTimeout)
	defer cancel()

	ask := func(extra ...string) (provider.DiskSmart, error) {
		args := append([]string{"-j", "-i", "-H", "-A", "-l", "selftest"}, extra...)
		raw, runErr := exec.CommandContext(ctx, smartctl, append(args, "/dev/"+name)...).Output()
		disk, parseErr := provider.ParseSmartctl(raw)
		if parseErr != nil {
			if runErr != nil {
				return provider.DiskSmart{}, runErr
			}
			return provider.DiskSmart{}, parseErr
		}
		return disk, nil
	}

	disk, err := ask()
	if err == nil && disk.Unsupported {
		if viaSAT, satErr := ask("-d", "sat"); satErr == nil && !viaSAT.Unsupported {
			disk = viaSAT
		}
	}
	if err != nil {
		disk = provider.DiskSmart{Error: err.Error()}
	}
	disk.Device = name
	disk.Partitions = partitionsOf(name)
	return disk
}

// partitionsOf lists a disk's partition names from /sys — "sda" carries
// "sda1"; "nvme0n1" carries "nvme0n1p1". These are the names a mount table
// points at, which makes them the join key.
func partitionsOf(disk string) []string {
	entries, err := os.ReadDir(filepath.Join("/sys/block", disk))
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), disk) {
			out = append(out, entry.Name())
		}
	}
	return out
}

// writeReport lands the report atomically — temp file, fsync, rename — and
// world-readable, because the writer is root and the reader is the service
// user.
func writeReport(path string, report provider.DiskReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sand-smart-*")
	if err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(name)
		return fmt.Errorf("writing report: %w", err)
	}
	if err := os.Chmod(name, 0644); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing report: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
