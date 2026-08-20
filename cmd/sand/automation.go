package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The standing instructions a folder has been given, from the command line.
//
// The schedules themselves are kept by `sand serve`, because that is the
// process that is running at ten in the morning. These four subcommands are how
// you write one down, read what the last few came to, and start one by hand
// without waiting for its slot — which is the thing you want the moment you
// have finished reconnecting a cloud and would like to know whether it worked.

func automationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "automation",
		Short: "Standing instructions for a folder: check it on a schedule, and repair it",
		Long: `A folder can be told to look after itself.

On the schedule you give it, every cloud is asked whether it is there and every
part of every file under the folder is checked against the index that says where
it went. A policy set to rebalance then rebuilds whatever came back short onto
the clouds that answered, keeping each file's own erasure code where there are
clouds enough for it.

The schedules are kept by "sand serve" while the vault is unlocked, so a machine
meant to keep them wants a long --idle-timeout. Nothing is lost when it is shut:
a policy whose slot passed comes up due the moment the vault is opened again.

  sand automation set /archive --daily 10:00 --action rebalance
  sand automation list
  sand automation run /archive
  sand automation remove /archive`,
	}
	cmd.AddCommand(
		automationListCmd(),
		automationSetCmd(),
		automationRemoveCmd(),
		automationRunCmd(),
	)
	return cmd
}

func automationListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show every folder that has a policy, and what its last run found",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, _, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			policies, err := v.AllAutomations()
			if err != nil {
				return err
			}
			if len(policies) == 0 {
				fmt.Println("No folder has a policy yet. Give one to a folder with 'sand automation set'.")
				return nil
			}

			for _, p := range policies {
				printAutomation(p)
			}
			return nil
		},
	}
}

func automationSetCmd() *cobra.Command {
	var (
		hourly       bool
		daily        string
		weekly       string
		action       string
		narrow       bool
		maxRepairs   int
		rebuildLimit string
		disabled     bool
	)

	cmd := &cobra.Command{
		Use:   "set <folder>",
		Short: "Give a folder a schedule, or change the one it has",
		Long: `Give a folder a standing instruction.

Exactly one of --hourly, --daily or --weekly says how often. --daily and
--weekly take a wall-clock time in the local timezone of the machine running
the server, and --weekly takes a day in front of it.

--action check looks and writes down what it found, moving nothing. --action
rebalance also puts back what is missing, by rebuilding the files that came back
short onto the clouds that answered.

A rebuild gathers the whole file into memory before it cuts it again, so an
unattended one is capped: files larger than --rebuild-limit are counted, named,
and left for you to repair by hand where you can watch it. --max-repairs bounds
how many files one run will rebuild at all; what is left over is picked up by
the next run, worst first.

Editing a policy keeps its history and its last-run time, so changing the hour
does not make the folder immediately due.

  sand automation set /archive --daily 10:00 --action rebalance
  sand automation set /films --weekly sun,03:00 --action check
  sand automation set /taxes --daily 02:00 --action rebalance --rebuild-limit 8G`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			policy := vault.Automation{
				Enabled: !disabled,
				Action:  vault.AutomationAction(strings.TrimSpace(action)),
				Narrow:  narrow,

				MaxRepairs: maxRepairs,
			}

			named := 0
			if hourly {
				named++
				policy.Cadence = vault.CadenceHourly
			}
			if daily != "" {
				named++
				policy.Cadence = vault.CadenceDaily
				policy.At = daily
			}
			if weekly != "" {
				named++
				policy.Cadence = vault.CadenceWeekly
				day, at, err := parseWeekly(weekly)
				if err != nil {
					return err
				}
				policy.Weekday, policy.At = day, at
			}
			if named != 1 {
				return fmt.Errorf("say how often with exactly one of --hourly, --daily or --weekly")
			}

			limit, err := parseRebuildLimit(rebuildLimit)
			if err != nil {
				return err
			}
			policy.RebuildLimit = limit

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			stored, err := v.SetAutomation(scope, args[0], policy)
			if err != nil {
				return err
			}
			printAutomation(*stored)
			return nil
		},
	}

	cmd.Flags().BoolVar(&hourly, "hourly", false, "check every hour")
	cmd.Flags().StringVar(&daily, "daily", "", "check every day at this local time, as HH:MM")
	cmd.Flags().StringVar(&weekly, "weekly", "",
		"check weekly, as day,HH:MM — for example sun,03:00")
	cmd.Flags().StringVar(&action, "action", string(vault.ActionCheck),
		"what to do about what it finds: check, or rebalance")
	cmd.Flags().BoolVar(&narrow, "narrow", false,
		"allow a repair to cut a file with a smaller code when there are not enough clouds "+
			"answering to keep its own (off by default: narrowing cannot be undone without "+
			"another full rebuild)")
	cmd.Flags().IntVar(&maxRepairs, "max-repairs", 0,
		"how many files one run may rebuild, 0 for no bound")
	cmd.Flags().StringVar(&rebuildLimit, "rebuild-limit", "",
		"largest file an unattended repair will rebuild, such as 2G — 'none' for no ceiling "+
			"(default 1G)")
	cmd.Flags().BoolVar(&disabled, "disabled", false,
		"store the policy but do not run it on its schedule")
	return cmd
}

func automationRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <folder>",
		Aliases: []string{"rm"},
		Short:   "Take the policy off a folder, history and all",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			if err := v.RemoveAutomation(scope, args[0]); err != nil {
				return err
			}
			fmt.Printf("%s no longer has a policy.\n", args[0])
			return nil
		},
	}
}

func automationRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <folder>",
		Short: "Carry out a folder's policy now, without waiting for its slot",
		Long: `Run a folder's policy immediately, whether or not it is due and whether or not
it is switched on.

It stamps the last-run time exactly as a scheduled run does: having just checked
a folder, there is no reason to check it again in an hour.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			run, err := v.RunAutomation(cmd.Context(), scope, args[0])
			if err != nil {
				return err
			}
			printAutomationRun(run, "")
			printWarnings(run.Warnings)

			if run.Error != "" {
				return fmt.Errorf("%s", run.Error)
			}
			if run.AtRisk > 0 {
				return fmt.Errorf("%d file(s) cannot be rebuilt from what is reachable", run.AtRisk)
			}
			return nil
		},
	}
}

// printAutomation writes one policy and the last thing it did.
func printAutomation(p vault.FolderAutomation) {
	where := p.Folder
	if p.Vault != "" {
		where = string(p.Vault) + ":" + p.Folder
	}
	state := "on"
	if !p.Enabled {
		state = "off"
	}

	fmt.Printf("%-30s %-16s %-10s %s\n", where, describeCadence(p.Automation), p.Action, state)
	if p.NextRunAt != nil {
		fmt.Printf("   next   %s\n", p.NextRunAt.Local().Format("Mon 2 Jan 15:04"))
	}
	if p.Narrow {
		fmt.Println("   narrowing a file's code is allowed when there are too few clouds answering")
	}
	if p.MaxRepairs > 0 {
		fmt.Printf("   at most %d file(s) rebuilt per run\n", p.MaxRepairs)
	}
	if len(p.History) > 0 {
		printAutomationRun(&p.History[0], "   last   ")
	}
	fmt.Println()
}

// describeCadence writes a schedule the way somebody would say it.
func describeCadence(a *vault.Automation) string {
	switch a.Cadence {
	case vault.CadenceHourly:
		return "hourly"
	case vault.CadenceDaily:
		return "daily " + a.At
	case vault.CadenceWeekly:
		return strings.ToLower(a.Weekday.String()[:3]) + " " + a.At
	}
	return string(a.Cadence)
}

// printAutomationRun writes what one sweep came to, in a line or two.
func printAutomationRun(run *vault.AutomationRun, prefix string) {
	when := run.FinishedAt.Local().Format("2 Jan 15:04")

	if run.Error != "" {
		fmt.Printf("%s%s  %s\n", prefix, when, run.Error)
		return
	}
	fmt.Printf("%s%s  %d checked, %d whole, %d short, %d past repairing\n",
		prefix, when, run.Checked, run.Whole, run.Short, run.AtRisk)

	pad := strings.Repeat(" ", len(prefix))
	if run.Repaired > 0 || run.Failed > 0 || run.Deferred > 0 {
		fmt.Printf("%s%*s  %d rebuilt (%s), %d failed, %d left for later\n",
			pad, len(when), "", run.Repaired, formatBytes(run.Bytes), run.Failed, run.Deferred)
	}
	if len(run.Offline) > 0 {
		fmt.Printf("%s%*s  no answer from %s\n",
			pad, len(when), "", strings.Join(run.Offline, ", "))
	}
}

// parseWeekly reads "sun,03:00" into a day and a time.
func parseWeekly(spec string) (time.Weekday, string, error) {
	day, at, ok := strings.Cut(strings.TrimSpace(spec), ",")
	if !ok {
		return 0, "", fmt.Errorf(
			"write a weekly schedule as day,HH:MM — for example sun,03:00 (got %q)", spec)
	}
	name := strings.ToLower(strings.TrimSpace(day))
	for d := time.Sunday; d <= time.Saturday; d++ {
		full := strings.ToLower(d.String())
		if name == full || name == full[:3] {
			return d, strings.TrimSpace(at), nil
		}
	}
	return 0, "", fmt.Errorf("%q is not a day of the week", day)
}

// parseRebuildLimit reads a size written the way people write one — 512M, 8G —
// or "none" for no ceiling at all. Empty leaves the default in place.
func parseRebuildLimit(text string) (int64, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(text))
	switch trimmed {
	case "":
		return 0, nil
	case "NONE", "OFF", "0":
		// Negative is how the vault records "no ceiling"; zero means default.
		return -1, nil
	}

	unit := int64(1)
	switch trimmed[len(trimmed)-1] {
	case 'K':
		unit = 1 << 10
	case 'M':
		unit = 1 << 20
	case 'G':
		unit = 1 << 30
	case 'T':
		unit = 1 << 40
	}
	digits := trimmed
	if unit > 1 {
		digits = trimmed[:len(trimmed)-1]
	}

	var n int64
	if _, err := fmt.Sscanf(digits, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf(
			"%q is not a size — write one as a number with K, M, G or T, such as 2G, "+
				"or 'none' for no ceiling", text)
	}
	return n * unit, nil
}
