package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/spf13/cobra"
)

func vaultScanCmd() *cobra.Command {
	var (
		every string
		off   bool
	)

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "List the local disks against the index for parts that are gone or truncated",
		Long: `List every account that is a folder on this machine's disks and hold the
listing against the index: a part that is not there, or is there as a file
too short to be a part — what a removable disk leaves behind when it loses
power after accepting a write — is written down as missing, so 'Files missing
a part' shows the file with the reason and a repair writes the part again.

'sand serve' does this on its own, daily by default, while the vault is
unlocked. --every and --off change that schedule; with neither, this runs one
scan now and prints what it found. Clouds are not scanned here: a folder
automation checks a cloud's parts on a schedule of its own.

  sand vault scan               # scan the local disks now
  sand vault scan --every 6h    # scan every six hours from now on
  sand vault scan --off         # stop scanning on a schedule`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			if off || every != "" {
				schedule := vault.PartScanSchedule{Enabled: !off}
				if every != "" {
					interval, err := time.ParseDuration(every)
					if err != nil {
						return fmt.Errorf("--every takes a duration like 6h, 24h or 168h: %w", err)
					}
					schedule.Minutes = int(interval / time.Minute)
					schedule.Enabled = true
				}
				saved, err := v.SetPartScanSchedule(schedule)
				if err != nil {
					return err
				}
				if !saved.Enabled {
					fmt.Println("Scheduled scans are off.")
				} else {
					fmt.Printf("Scanning every %s from now on.\n", formatInterval(saved.Interval()))
				}
			}

			report, err := v.ScanLocalParts(context.Background())
			if err != nil {
				return err
			}
			if len(report.Accounts) == 0 {
				fmt.Println("No account is a folder on this machine's disks; there is nothing to scan.")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "DISK\tKIND\tPARTS\tMISSING\tBACK")
			for _, a := range report.Accounts {
				if a.Error != "" {
					fmt.Fprintf(tw, "%s\t%s\tnot listed: %s\t\t\n", a.Name, a.Kind, firstLine(a.Error))
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", a.Name, a.Kind, a.Parts, a.Lost, a.Recovered)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			switch {
			case report.Lost > 0:
				fmt.Printf("\n%d part(s) missing or truncated across %d file(s). 'sand files check --all' names them; "+
					"'sand relocate' on a file writes its parts again.\n", report.Lost, report.Files)
			default:
				fmt.Printf("\nEvery one of %d part(s) is where the index says it is.\n", report.Parts)
			}
			if schedule := v.PartScanSchedule(); schedule.Enabled {
				fmt.Printf("Scanned again every %s under 'sand serve' while the vault is unlocked.\n", formatInterval(schedule.Interval()))
			} else {
				fmt.Println("Scheduled scans are off — 'sand vault scan --every 24h' turns them back on.")
			}
			if report.Lost > 0 {
				return fmt.Errorf("%d part(s) need attention", report.Lost)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&every, "every", "", "How often 'sand serve' scans, as a duration (6h, 24h, 168h)")
	cmd.Flags().BoolVar(&off, "off", false, "Stop scanning on a schedule")
	return cmd
}
