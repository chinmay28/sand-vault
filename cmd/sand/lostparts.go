package main

import (
	"fmt"
	"strings"

	"github.com/chinmay28/sand-vault/internal/vault"
	"github.com/spf13/cobra"
)

// Finding the parts the index records on an account that the account is not
// holding, and — once somebody has read the list — putting them back. See
// internal/vault/lostparts.go.

func vaultRestorePartsCmd() *cobra.Command {
	var (
		yes     bool
		verbose bool
		clouds  []string
	)

	cmd := &cobra.Command{
		Use:   "restore-parts",
		Short: "Find parts a cloud should be holding and is not, and remake them there",
		Long: `Ask every connected account what it is holding and hold that against the
index. A part the index records on an account that the account does not list is
lost: a folder emptied by hand, a bucket restored from an old backup, a sync
client that tidied the files away.

Nothing else notices. The file still shows a full set of parts and still reads
back — on its spares — until a second cloud is unavailable. This lists what is
gone. Nothing is written without --yes.

With --yes each lost object is cut again from the file's other parts and
written back to the account the index records it on, under the name it had.
That costs the parts a read needs, down, and the one lost object, up — not the
whole file — and the index is not written at all: it was right about where the
part belongs. Nothing is erased and nothing is overwritten; an object the
account turns out to hold is left exactly as it is.

--cloud names an account, by name or ID, to put parts back on; give it more
than once for several. Every account is listed either way, since a part is
remade from the others.

One thing a listing cannot tell apart: an account that has been emptied, and
an account pointed at the wrong folder. Both hold none of what the index
records there. Such an account is marked, and is the thing to check before
--yes — writing the parts into the wrong folder helps nobody.

Not covered: a file stored in the format from before chunking (run
'sand vault convert'), a locked sub vault's files (unlock it first),
thumbnails and replicas, which are remade by their own means.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			var accounts []string
			for _, name := range clouds {
				cfg, err := findProvider(v, name)
				if err != nil {
					return err
				}
				accounts = append(accounts, cfg.ID)
			}

			scan, err := v.ScanForLostParts(cmd.Context(), vault.LostScanOptions{})
			if err != nil {
				return err
			}
			printWarnings(scan.Warnings)

			if !scan.Found {
				fmt.Println("Every part the index records is on the cloud it is recorded on.")
				return nil
			}

			fmt.Printf("%d part(s) of %d file(s) — %d object(s), about %s — are not where the index records them:\n",
				scan.Parts, scan.Files, scan.Objects, formatBytes(scan.Bytes))
			for _, account := range scan.Accounts {
				if account.Parts == 0 {
					continue
				}
				line := fmt.Sprintf("  %-24s %d part(s) of %d file(s), %d of the %d object(s) recorded there",
					account.Name, account.Parts, account.Files, account.Lost, account.Recorded)
				if account.Emptied {
					line += " — it holds NONE of them: emptied, or pointed at the wrong folder?"
				}
				fmt.Println(line)
			}
			if verbose {
				for _, item := range scan.Items {
					name := item.Path
					if name == "" {
						name = "a file in a sub vault · " + item.ArchiveID
					}
					line := fmt.Sprintf("    %s — part %d on %s, %d of %d object(s)",
						name, item.Part, item.ProviderName, item.Objects, item.Of)
					if !item.Restorable {
						line += " — cannot be put back: " + item.Reason
					}
					fmt.Println(line)
				}
				if scan.ItemsTruncated > 0 {
					fmt.Printf("    …and %d more\n", scan.ItemsTruncated)
				}
			}
			if withheld := scan.Parts - scan.Restorable; withheld > 0 {
				fmt.Printf("\n%d of them cannot be remade from the parts that are left (--verbose says why).\n", withheld)
			}
			if scan.Restorable == 0 {
				return nil
			}

			if !yes {
				fmt.Println()
				fmt.Printf("%d part(s) can be put back. Run again with --yes to do it.\n", scan.Restorable)
				return nil
			}

			fmt.Println()
			last := ""
			report, err := v.RestoreLostParts(cmd.Context(), vault.LostRestoreOptions{
				Accounts: accounts,
				OnProgress: func(at vault.LostRestoreProgress) {
					if at.Stage != vault.RestoreStageRestoring || at.Path == "" || at.Path == last {
						return
					}
					last = at.Path
					fmt.Printf("  [%d/%d] %s → %s\n", at.PartsDone+1, at.Parts, at.Path, at.Cloud)
				},
			})
			if err != nil {
				return err
			}

			fmt.Printf("\n%d part(s) of %d file(s) put back: %d object(s), %s written",
				report.Parts, report.Files, report.Objects, formatBytes(report.Bytes))
			if len(report.On) > 0 {
				fmt.Printf(" to %s", strings.Join(report.On, ", "))
			}
			fmt.Println(".")
			if report.AlreadyThere > 0 {
				fmt.Printf("%d object(s) were there after all, and were left as they are.\n", report.AlreadyThere)
			}
			for _, f := range report.Failures {
				name := f.Path
				if name == "" {
					name = "a file in a sub vault"
				}
				fmt.Printf("  not put back: %s, part %d on %s — %s\n", name, f.Part, f.ProviderName, f.Reason)
			}
			if report.FailuresTruncated > 0 {
				fmt.Printf("  …and %d more\n", report.FailuresTruncated)
			}
			if report.Stopped {
				return fmt.Errorf("stopped before it had finished: %s", report.StoppedBy)
			}
			if report.Failed > 0 {
				return fmt.Errorf("%d part(s) could not be put back", report.Failed)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "remake the lost parts and write them back")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "list every lost part, not just the totals")
	cmd.Flags().StringArrayVar(&clouds, "cloud", nil, "put parts back on this account only (name or ID; repeatable)")
	return cmd
}
