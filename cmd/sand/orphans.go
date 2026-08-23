package main

import (
	"fmt"

	"github.com/chinmay28/sand-vault/internal/vault"
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
waiting to be recovered looks like.

The same question is asked of the vault's own directory, which is where SAND
writes its working files. An upload is spooled to disk before it is sent, so a
process killed mid-upload leaves a file the size of whatever was being uploaded
and nothing ever goes back for it. Only the names SAND writes are looked at, and
only once nothing has written to one for an hour, so an upload running in
another window is never taken out from under it.`,
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

			// The local half, before the clouds: it is the same housekeeping,
			// it is the bigger figure — an interrupted upload leaves the whole
			// file it was sending — and it is the one thing here that can be
			// acted on without asking a single account.
			sweepLeftovers(v, scan.Leftovers, yes, verbose)

			if scan.Reattachable > 0 {
				// Said first, because it is the better news and the one that
				// costs nothing: these are not rubbish, they are shards of
				// files that are still here.
				fmt.Printf("%d shard(s) of %d file(s) are on your accounts with no record pointing at them — "+
					"run 'sand vault reattach' to put them back.\n\n", scan.Reattachable, scan.StrayFiles)
			}
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

func vaultReattachCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "reattach",
		Short: "Put back the index records for shards still sitting on your clouds",
		Long: `Find shards of files you still have that the index has no record of, and
record them again.

Disconnecting a cloud drops the index records naming it — an index that still
claimed them would be lying about what can be retrieved — and leaves the objects
themselves alone, because SAND has no business deleting from an account you are
telling it to stop using. Those two facts do not meet again on their own:
reconnecting gives the account a new internal ID, and 'sand vault recover
--resume' re-points records rather than inventing them, so there is nothing left
to re-point. The file goes on reporting a missing spare part while the part sits
on a cloud you are connected to.

This is the repair. Not a byte is transferred: a part's object key is derived
from the archive ID and the shard number, so the object is already exactly where
a record would say it is, and putting the record back is a single index write.

Purely additive — nothing is erased and no key is touched, and a file can only
come out of it with more shards than it went in with. Nothing is written without
--yes.

It does not check the contents, any more than a recovery does. Run
'sand vault check' afterwards to have the accounts asked whether the parts are
really there.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			preview, err := v.ReattachShards(cmd.Context(), true)
			if err != nil {
				return err
			}
			printWarnings(preview.Warnings)
			printWarnings(preview.Skipped)

			if preview.Shards == 0 {
				fmt.Println("Every shard on your accounts already has a record pointing at it.")
				return nil
			}
			fmt.Printf("%d shard(s) of %d file(s) (%s) are on your clouds with nothing pointing at them.\n",
				preview.Shards, preview.Files, formatBytes(preview.Bytes))

			if !yes {
				fmt.Println()
				fmt.Println("Recording them again moves no data at all. Run again with --yes to do it.")
				return nil
			}

			report, err := v.ReattachShards(cmd.Context(), false)
			if report != nil {
				printWarnings(report.Skipped)
				fmt.Printf("Recorded %d shard(s) across %d file(s). No data was transferred.\n",
					report.Shards, report.Files)
				for _, path := range report.Restored {
					fmt.Printf("  restored to full spread: %s\n", path)
				}
				fmt.Println("Run 'sand vault check' to have the accounts confirm the parts are really there.")
			}
			return err
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "write the records back rather than only listing them")
	return cmd
}

// sweepLeftovers reports the working files SAND left in its own directory, and
// erases them when the command was asked to erase.
//
// The disk half of the same tidy-up. It is said first and separately because it
// has nothing to do with the accounts: no cloud is asked anything, none of
// orphanGuard's refusals apply, and the room it gives back is on the machine
// the command is running on rather than on somebody's quota. Any error is the
// scan's own, and a directory that would not be read is a warning rather than
// a failure: it should not stop the accounts being asked their half of the same
// question.
func sweepLeftovers(v *vault.Vault, scan *vault.LeftoverScan, yes, verbose bool) {
	if scan == nil || !scan.Found {
		return
	}
	printWarnings(scan.Warnings)

	fmt.Printf("%d working file(s) (%s) in %s that nothing is using:\n",
		scan.Files, formatBytes(scan.Bytes), scan.Dir)
	if verbose {
		for _, item := range scan.Items {
			fmt.Printf("    %-28s %8s  %s\n", item.Name, formatBytes(item.Bytes), item.What)
			if item.Reason != "" {
				fmt.Printf("      %s\n", item.Reason)
			}
		}
		if scan.ItemsTruncated > 0 {
			fmt.Printf("    …and %d more\n", scan.ItemsTruncated)
		}
	}

	if scan.Deletable == 0 {
		fmt.Println("  None of them has been idle long enough to be sure it is finished with.")
		fmt.Println()
		return
	}
	if !yes {
		fmt.Printf("  Erasing %d of them would free %s on this machine. Run again with --yes to do it.\n",
			scan.Deletable, formatBytes(scan.DeletableBytes))
		fmt.Println()
		return
	}

	report := v.SweepLeftovers(nil, false)
	printWarnings(report.Warnings)
	printWarnings(report.Skipped)
	fmt.Printf("  Erased %d working file(s), freeing %s on this machine.\n",
		report.Deleted, formatBytes(report.Bytes))
	fmt.Println()
}
