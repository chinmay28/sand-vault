package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// One vault on several machines, from a terminal. The browser pulls on a
// schedule while the server runs; a machine with no server running — a cron
// job, a shell on the Pi — pulls here.

func vaultSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Pull what the other machines opening this vault have written",
		Long: `Read the copies of the index on the connected accounts and fold them into
this machine's own.

The same vault can be opened on more than one machine — a desk and a Raspberry
Pi, say — with the same password and the same clouds. Each machine keeps its
own vault file, and the copies of the index every account carries for recovery
(` + vault.BackupKey + `) are how a change made on one reaches the others. The
server pulls on its own: at every unlock, every few minutes once another
machine has been seen, and before every push. This command is the same pull
for a machine with no server running on it.

A local folder is reachable only from the machine it is plugged into. Files
whose parts sit on another machine's folder are listed here but greyed out in
the browser, which names the machine that can read them; 'sand vault
instances' lists the machines.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			report, err := v.SyncIndex(cmd.Context())
			if err != nil {
				return err
			}
			v.AwaitBackupSync()
			printWarnings(report.Warnings)

			fmt.Printf("This machine:   %s\n", report.Instance)
			fmt.Printf("Copies read:    %d of %d account(s)", report.Answered, report.Asked)
			if report.Foreign > 0 {
				fmt.Printf(" (%d sealed under another password)", report.Foreign)
			}
			fmt.Println()
			switch report.Outcome {
			case vault.MergeCurrent:
				fmt.Println("Index:          current — nothing new on the accounts")
			case vault.MergeAhead:
				fmt.Println("Index:          ahead of the copies on the accounts; the next push brings them up")
			case vault.MergeAdopted:
				fmt.Printf("Index:          took the newer copy — %d file(s) added, %d removed, %d changed\n",
					report.Added, report.Removed, report.Updated)
			case vault.MergeMerged:
				fmt.Printf("Index:          combined with the other machine's — %d file(s) added, %d removed, %d changed\n",
					report.Added, report.Removed, report.Updated)
			}
			for _, line := range report.Renamed {
				fmt.Printf("Renamed:        %s — both machines stored a file there\n", line)
			}
			printInstances(report.Instances)
			return nil
		},
	}
}

func vaultInstancesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "instances",
		Short: "List the machines that open this vault",
		Long: `List every machine that has written this vault's index, this one first, with
when it last did and how many connected accounts it can reach. A local folder
counts only for the machine it is plugged into; every cloud counts for every
machine that holds its credentials.

Read from the index on this machine; 'sand vault sync' brings it up to date.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			instances, err := v.Instances()
			if err != nil {
				return err
			}
			printInstances(instances)
			if last := v.LastSync(); !last.IsZero() {
				fmt.Printf("Last pulled:    %s ago\n", humanDuration(time.Since(last).Round(time.Second)))
			}
			return nil
		},
	}
}

func printInstances(instances []vault.InstanceInfo) {
	if len(instances) == 0 {
		return
	}
	fmt.Println("Machines:")
	for _, inst := range instances {
		var notes []string
		if inst.Here {
			notes = append(notes, "this machine")
		}
		if !inst.LastSeen.IsZero() {
			notes = append(notes, "last wrote "+humanDuration(time.Since(inst.LastSeen).Round(time.Second))+" ago")
		}
		notes = append(notes, fmt.Sprintf("reaches %d account(s)", len(inst.Accounts)))
		fmt.Printf("  %-16s %s\n", inst.Name, strings.Join(notes, ", "))
	}
}
