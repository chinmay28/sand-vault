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
		task         string
		action       string
		narrow       bool
		maxRepairs   int
		rebuildLimit string
		maxRepos     int
		sizeLimit    string
		prune        bool
		disabled     bool
	)

	cmd := &cobra.Command{
		Use:   "set <folder>",
		Short: "Give a folder a schedule, or change the one it has",
		Long: `Give a folder a standing instruction.

Exactly one of --hourly, --daily or --weekly says how often. --daily and
--weekly take a wall-clock time in the local timezone of the machine running
the server, and --weekly takes a day in front of it.

--task says which job. "shards" is the storage one: every cloud asked whether
it is there, and every part of every file checked against the index that says
where it went. "git" is the mirror one: every repository stored under the folder
asked whether its upstream has moved (see "sand git").

--action check looks and writes down what it found, changing nothing, and means
that for either task. The fixing half is named after the work: --action
rebalance rebuilds the files that came back short onto the clouds that answered,
and --action pull fetches the repositories whose upstream has moved.

A rebuild gathers the whole file into memory before it cuts it again, so an
unattended one is capped: files larger than --rebuild-limit are counted, named,
and left for you to repair by hand where you can watch it. --max-repairs bounds
how many files one run will rebuild at all; what is left over is picked up by
the next run, worst first. --max-repos and --size-limit are the same two bounds
for the mirror task.

Editing a policy keeps its history and its last-run time, so changing the hour
does not make the folder immediately due.

  sand automation set /archive --daily 10:00 --action rebalance
  sand automation set /films --weekly sun,03:00 --action check
  sand automation set /taxes --daily 02:00 --action rebalance --rebuild-limit 8G
  sand automation set /code --weekly sun,04:00 --task git --action pull`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			policy := vault.Automation{
				Enabled: !disabled,
				Task:    vault.AutomationTask(strings.TrimSpace(task)),
				Action:  vault.AutomationAction(strings.TrimSpace(action)),
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

			rebuild, err := parseSizeLimit(rebuildLimit, "--rebuild-limit")
			if err != nil {
				return err
			}
			bundle, err := parseSizeLimit(sizeLimit, "--size-limit")
			if err != nil {
				return err
			}
			// Both are filled in and the vault keeps whichever its task reads,
			// so there is one place deciding which knobs belong to which job.
			policy.Shards = &vault.ShardPolicy{
				Narrow:       narrow,
				MaxRepairs:   maxRepairs,
				RebuildLimit: rebuild,
			}
			policy.Git = &vault.GitPolicy{
				MaxRepos:  maxRepos,
				SizeLimit: bundle,
				Prune:     prune,
			}

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
	cmd.Flags().StringVar(&task, "task", string(vault.TaskShards),
		"which job: shards (parts of files) or git (mirrored repositories)")
	cmd.Flags().StringVar(&action, "action", string(vault.ActionCheck),
		"what to do about what it finds: check, rebalance (shards) or pull (git)")
	cmd.Flags().BoolVar(&narrow, "narrow", false,
		"allow a repair to cut a file with a smaller code when there are not enough clouds "+
			"answering to keep its own (off by default: narrowing cannot be undone without "+
			"another full rebuild)")
	cmd.Flags().IntVar(&maxRepairs, "max-repairs", 0,
		"how many files one run may rebuild, 0 for no bound")
	cmd.Flags().StringVar(&rebuildLimit, "rebuild-limit", "",
		"largest file an unattended repair will rebuild, such as 2G — 'none' for no ceiling "+
			"(default 1G)")
	cmd.Flags().IntVar(&maxRepos, "max-repos", 0,
		"how many repositories one run may fetch, 0 for no bound (git)")
	cmd.Flags().StringVar(&sizeLimit, "size-limit", "",
		"largest stored bundle an unattended refresh will fetch, such as 4G — 'none' for no "+
			"ceiling (default 2G, git)")
	cmd.Flags().BoolVar(&prune, "prune", false,
		"delete a stored repository whose upstream has gone (off by default: an outage and a "+
			"deletion look the same from here, and the copy may be the last one)")
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
			if run.Shards != nil && run.Shards.AtRisk > 0 {
				return fmt.Errorf("%d file(s) cannot be rebuilt from what is reachable",
					run.Shards.AtRisk)
			}
			if run.Git != nil && run.Git.Failed > 0 {
				return fmt.Errorf("%d repositor%s could not be brought up to date",
					run.Git.Failed, plural(run.Git.Failed, "y", "ies"))
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

	fmt.Printf("%-30s %-16s %-6s %-10s %s\n",
		where, describeCadence(p.Automation), p.Task, p.Action, state)
	if p.NextRunAt != nil {
		fmt.Printf("   next   %s\n", p.NextRunAt.Local().Format("Mon 2 Jan 15:04"))
	}
	if p.Shards != nil {
		if p.Shards.Narrow {
			fmt.Println("   narrowing a file's code is allowed when there are too few clouds answering")
		}
		if p.Shards.MaxRepairs > 0 {
			fmt.Printf("   at most %d file(s) rebuilt per run\n", p.Shards.MaxRepairs)
		}
	}
	if p.Git != nil {
		if p.Git.MaxRepos > 0 {
			fmt.Printf("   at most %d repositor%s fetched per run\n",
				p.Git.MaxRepos, plural(p.Git.MaxRepos, "y", "ies"))
		}
		if p.Git.Prune {
			fmt.Println("   a repository whose upstream has gone is deleted")
		}
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
	pad := strings.Repeat(" ", len(prefix))

	switch {
	case run.Git != nil:
		g := run.Git
		fmt.Printf("%s%s  %d checked, %d up to date, %d updated\n",
			prefix, when, g.Checked, g.Current, g.Updated)
		if g.Updated > 0 && g.Commits > 0 {
			fmt.Printf("%s%*s  %d new commit(s), %s stored\n",
				pad, len(when), "", g.Commits, formatBytes(g.Bytes))
		}
		if g.Gone > 0 || g.Failed > 0 || g.Deferred > 0 || g.Pruned > 0 {
			fmt.Printf("%s%*s  %d upstream gone, %d failed, %d left for later, %d pruned\n",
				pad, len(when), "", g.Gone, g.Failed, g.Deferred, g.Pruned)
		}
	default:
		res := run.Shards
		if res == nil {
			res = &vault.ShardResult{}
		}
		fmt.Printf("%s%s  %d checked, %d whole, %d short, %d past repairing\n",
			prefix, when, res.Checked, res.Whole, res.Short, res.AtRisk)
		if res.Repaired > 0 || res.Failed > 0 || res.Deferred > 0 {
			fmt.Printf("%s%*s  %d rebuilt (%s), %d failed, %d left for later\n",
				pad, len(when), "", res.Repaired, formatBytes(res.Bytes), res.Failed, res.Deferred)
		}
	}

	if len(run.Offline) > 0 {
		fmt.Printf("%s%*s  no answer from %s\n",
			pad, len(when), "", strings.Join(run.Offline, ", "))
	}
}

// plural picks the ending for a count, so a line reads as English.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

// parseSizeLimit reads a size written the way people write one — 512M, 8G — or
// "none" for no ceiling at all. Empty leaves the default in place. flag names
// the option it came from, so a typo is refused where it was typed.
func parseSizeLimit(text, flag string) (int64, error) {
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
			"%s: %q is not a size — write one as a number with K, M, G or T, such as 2G, "+
				"or 'none' for no ceiling", flag, text)
	}
	return n * unit, nil
}
