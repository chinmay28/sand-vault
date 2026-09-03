package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Old versions of SAND's objects on the buckets that keep them, and — once
// somebody has looked at the figure — erasing them. See
// internal/vault/versions.go.

func vaultPruneCmd() *cobra.Command {
	var (
		yes      bool
		verbose  bool
		accounts []string
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Erase old versions of SAND's objects from buckets that keep every version",
		Long: `Ask every account that keeps versions what it is storing beneath the objects
it shows, and erase the versions SAND would never read back.

A bucket with versioning switched on — which Backblaze B2 is out of the box,
under the name "keep all versions" — never overwrites and never deletes. Every
write of the index backup (manifest.sand) adds a copy beneath the previous one,
and SAND rewrites it on every change to the index, so a bucket that has been in
use for a while holds one copy of the index per upload, rename and move ever
made. Every part SAND has deleted is still there too, under a delete marker.
None of it shows in a plain listing — which is what the usage bar and 'sand
remote measure' count — and all of it is billed.

Only SAND's own objects are looked at: the index backup and parts, named the
way SAND names them. Anything else in the bucket keeps its history, counted so
you can see where the room went. The current version of every object stays
exactly where it is, whether or not a file points at it; a part nothing wants
is 'sand vault sweep' business. And a part the index still points at whose
current version is a delete marker — deleted from the bucket's console, or by a
lifecycle rule — is held back with the reason beside it, because the versions
beneath that marker are the only copies left.

This lists what it finds. Nothing is erased without --yes.

Buckets that keep every version keep doing it: to stop the history piling up
again, set the bucket's lifecycle to keep only the latest version (on B2,
"Keep only the last version of the file"; on S3, a rule that expires
noncurrent versions and removes expired delete markers).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			var ids []string
			for _, ref := range accounts {
				cfg, err := findProvider(v, ref)
				if err != nil {
					return err
				}
				ids = append(ids, cfg.ID)
			}

			scan, err := v.ScanForStaleVersions(cmd.Context())
			if err != nil {
				return err
			}
			printWarnings(scan.Warnings)

			if scan.Versioned == 0 {
				fmt.Println("None of your accounts keeps old versions, so there is nothing to prune.")
				return nil
			}
			if !scan.Found {
				fmt.Println("Every versioned account is storing only the current version of each object.")
				return nil
			}

			fmt.Printf("%d stale version(s) (%s) of SAND's objects, beneath what your buckets show:\n",
				scan.Stale, formatBytes(scan.StaleBytes))
			for _, account := range scan.Accounts {
				if !account.Versioned || account.Error != "" {
					continue
				}
				fmt.Printf("  %-24s %d current object(s), %s · %d stale version(s), %s, %d delete marker(s)\n",
					account.Name, account.Current, formatBytes(account.CurrentBytes),
					account.Stale, formatBytes(account.StaleBytes), account.Markers)
				if account.Other > 0 {
					fmt.Printf("  %-24s and %d old version(s), %s, of files that are not SAND's — left alone\n",
						"", account.Other, formatBytes(account.OtherBytes))
				}
			}
			if verbose {
				for _, item := range scan.Items {
					fmt.Printf("    %s on %s — %s, %d version(s), %d marker(s), %s\n",
						item.Key, item.ProviderName, item.What, item.Versions, item.Markers, formatBytes(item.Bytes))
					if item.Reason != "" {
						fmt.Printf("      not erased: %s\n", item.Reason)
					}
				}
				if scan.ItemsTruncated > 0 {
					fmt.Printf("    …and %d more\n", scan.ItemsTruncated)
				}
			}

			if scan.Deletable == 0 {
				fmt.Println()
				fmt.Println("None of it can be erased safely — run with --verbose to see the reason beside each.")
				return nil
			}
			if !yes {
				fmt.Println()
				fmt.Printf("Erasing them would free %s. Run again with --yes to do it.\n",
					formatBytes(scan.DeletableBytes))
				return nil
			}

			report, err := v.SweepStaleVersions(cmd.Context(), ids, false, func(done, total int) {
				progressLine("erased", done, total)
			})
			clearProgressLine()
			if report != nil {
				printWarnings(report.Warnings)
				printWarnings(report.Skipped)
				fmt.Printf("Erased %d stale version(s), %d of them delete markers, freeing %s.\n",
					report.Deleted, report.Markers, formatBytes(report.Bytes))
			}
			return err
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "erase what is found rather than only listing it")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "list every object with stale versions, not just the totals")
	cmd.Flags().StringArrayVar(&accounts, "account", nil,
		"only erase from this account, by name or id (repeatable; the listing always covers every account)")
	return cmd
}
