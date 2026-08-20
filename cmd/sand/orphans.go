package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Finding the parts on the accounts that the index has stopped pointing at,
// and — once somebody has looked at the figure — erasing them.
//
// The gap opens when a file is deleted while one of the clouds holding it is
// disconnected. The delete reaches the accounts that are connected and cannot
// reach the one that is not, and reconnecting that cloud gives it a fresh
// internal ID, so nothing ever goes back for the parts it left. See
// internal/vault/orphans.go.

func vaultSweepCmd() *cobra.Command {
	var (
		yes     bool
		verbose bool
	)

	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Find parts on your accounts that no file points at any more",
		Long: `Ask every connected account what it is holding and subtract what the index
accounts for. Whatever is left over is storage being paid for by nobody.

The usual way that happens is a delete that could not finish. Erasing a file
erases its parts from the accounts holding them — but only the ones connected
at the time. Disconnect a cloud, delete some files without it, and the parts on
it stay put; reconnecting it gives that account a new internal ID, so nothing
ever goes back for them.

This lists what it finds. Nothing is erased without --yes.

It refuses to sweep at all in the cases where "no file points at it" is not the
same as "nobody wants it": an account carrying an index this vault did not
write, an account that would not answer, and a vault that holds no files of its
own on accounts it has never written an index to — which is what a machine
waiting to be recovered looks like.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			scan, err := v.ScanForOrphans(cmd.Context())
			if err != nil {
				return err
			}
			printWarnings(scan.Warnings)

			if !scan.Found {
				fmt.Println("Every part on your accounts belongs to a file this vault still has.")
				return nil
			}

			fmt.Printf("%d archive(s) in %d object(s) (%s) on your accounts, pointed at by nothing:\n",
				scan.Archives, scan.Objects, formatBytes(scan.Bytes))
			for _, account := range scan.Accounts {
				if account.Orphans == 0 {
					continue
				}
				fmt.Printf("  %-24s %d object(s), %s of the %s it holds\n",
					account.Name, account.Orphans, formatBytes(account.OrphanBytes),
					formatBytes(account.Bytes))
			}
			if verbose {
				for _, item := range scan.Items {
					fmt.Printf("    %s on %s — %d object(s), %s\n",
						item.ArchiveID, item.ProviderName, item.Objects, formatBytes(item.Bytes))
				}
				if scan.ItemsTruncated > 0 {
					fmt.Printf("    …and %d more\n", scan.ItemsTruncated)
				}
			}

			if len(scan.Blocked) > 0 {
				fmt.Println()
				fmt.Println("Not offering to erase any of it:")
				for _, reason := range scan.Blocked {
					fmt.Printf("  - %s\n", reason)
				}
				return nil
			}
			if scan.Deletable == 0 {
				fmt.Println()
				fmt.Println("None of it can be erased safely — see the reasons beside each account above.")
				return nil
			}

			if !yes {
				fmt.Println()
				fmt.Printf("Erasing them would free %s. Run again with --yes to do it.\n",
					formatBytes(scan.DeletableBytes))
				return nil
			}

			report, err := v.SweepOrphans(cmd.Context(), nil, false)
			if report != nil {
				printWarnings(report.Warnings)
				printWarnings(report.Skipped)
				fmt.Printf("Erased %d object(s) across %d archive(s), freeing %s.\n",
					report.Deleted, report.Archives, formatBytes(report.Bytes))
			}
			return err
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "erase what is found rather than only listing it")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "list every abandoned archive, not just the totals")
	return cmd
}
