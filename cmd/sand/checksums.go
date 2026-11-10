package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// checksumsCmd is the checksum ledger and the two jobs around it, from a
// terminal. See internal/vault/checksums.go.
func checksumsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checksums",
		Short: "The checksum ledger: write it, verify files against it, fill in what is missing",
		Long: "Every file has a checksum — the SHA-256 of its contents — and " + vault.ChecksumsPath + " at the root of\n" +
			"the vault lists them all, one \"path, checksum\" a line, on the vault's default clouds.\n\n" +
			"  write    bring the ledger up to date with the index now\n" +
			"  verify   read every file under a folder and hold it to its checksum\n" +
			"  compute  read the files that have no checksum and record one",
	}

	var mbps float64
	rate := func() int64 { return int64(mbps * 1024 * 1024) }
	paced := func(c *cobra.Command) *cobra.Command {
		c.Flags().Float64Var(&mbps, "limit", 0, "read no faster than this many MB a second (0 is unlimited)")
		return c
	}

	run := func(kind vault.ChecksumKind) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			dir := "/"
			if len(args) > 0 {
				dir = args[0]
			}
			// Ctrl-C stops it with what it has done, which for a compute is
			// checksums already recorded and worth writing to the ledger.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			opts := vault.ChecksumOptions{
				Control: vault.NewChecksumControl(rate()),
				OnProgress: func(at vault.ChecksumProgress) {
					progressLine(at.Path, at.File, at.Files)
				},
			}
			var report *vault.ChecksumReport
			if kind == vault.ChecksumCompute {
				report, err = v.ComputeChecksums(ctx, scope, dir, opts)
			} else {
				report, err = v.VerifyChecksums(ctx, scope, dir, opts)
			}
			clearProgressLine()
			if err != nil {
				return err
			}

			if kind == vault.ChecksumCompute {
				fmt.Printf("%d of %d file(s) read, %s: %d checksum(s) recorded, %d unreadable, %d skipped\n",
					report.Checked, report.Files, formatBytes(report.Bytes), report.Computed, report.Unreadable, report.Skipped)
				if report.LedgerError != "" {
					fmt.Fprintf(os.Stderr, "warning: %s was not written: %s\n", vault.ChecksumsPath, report.LedgerError)
				} else if report.Ledger != nil && report.Ledger.Written {
					fmt.Printf("%s written: %d file(s)\n", vault.ChecksumsPath, report.Ledger.Entries)
				}
			} else {
				fmt.Printf("%d of %d file(s) read, %s: %d match, %d do not, %d unreadable, %d skipped\n",
					report.Checked, report.Files, formatBytes(report.Bytes), report.Matched, report.Mismatched, report.Unreadable, report.Skipped)
				if report.NoChecksum > 0 {
					fmt.Printf("%d file(s) have no checksum to check against — `sand checksums compute` records one\n", report.NoChecksum)
				}
			}
			for _, f := range report.Findings {
				fmt.Printf("  %-10s %s: %s\n", f.State, f.Path, f.Detail)
			}
			if report.Stopped {
				return fmt.Errorf("stopped before the end")
			}
			if n := report.Mismatched + report.Unreadable; n > 0 {
				return fmt.Errorf("%d file(s) need attention", n)
			}
			return nil
		}
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "write",
			Short: "Bring " + vault.ChecksumsPath + " up to date with the index",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				v, scope, err := openVaultIn(cmd)
				if err != nil {
					return err
				}
				defer closeVault(v)
				ledger, err := v.WriteChecksumLedger(context.Background(), scope)
				if err != nil {
					return err
				}
				switch {
				case ledger.Written:
					fmt.Printf("%s written: %d file(s), %s\n", ledger.Path, ledger.Entries, formatBytes(ledger.Size))
				case ledger.Stored:
					fmt.Printf("%s is already up to date: %d file(s)\n", ledger.Path, ledger.Entries)
				default:
					fmt.Println("no file has a checksum yet, so there is nothing to write")
				}
				return nil
			},
		},
		paced(&cobra.Command{
			Use:   "verify [folder]",
			Short: "Read every file under a folder and hold it to its checksum",
			Args:  cobra.MaximumNArgs(1),
			RunE:  run(vault.ChecksumVerify),
		}),
		paced(&cobra.Command{
			Use:   "compute [folder]",
			Short: "Record a checksum for every file under a folder that has none",
			Args:  cobra.MaximumNArgs(1),
			RunE:  run(vault.ChecksumCompute),
		}),
	)
	return cmd
}
