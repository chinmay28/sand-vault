package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// rekeyFlags are the plan's settings as the command line spells them: shared
// by 'sand vault passwd', which starts a plan with the change, and 'sand vault
// rekey', which adjusts the one in flight.
type rekeyFlags struct {
	perDay       string
	fullSpeed    bool
	folders      []string
	onlyFolders  bool
	keepUnlocked bool
}

func (f *rekeyFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.perDay, "per-day", "",
		"re-encrypt this much a day rather than everything at once — 10G, 500M — "+
			"while the vault is open under 'sand serve' or 'sand vault rekey --run'")
	cmd.Flags().BoolVar(&f.fullSpeed, "full-speed", false,
		"re-encrypt everything as fast as the clouds take it, in the background under "+
			"'sand serve' or 'sand vault rekey --run', rather than on a daily budget")
	cmd.Flags().StringArrayVar(&f.folders, "folder", nil,
		"re-encrypt this folder first (repeatable, in order); needs --per-day or --full-speed")
	cmd.Flags().BoolVar(&f.onlyFolders, "only-folders", false,
		"stop after the --folder folders and leave the rest on the previous key for now")
	cmd.Flags().BoolVar(&f.keepUnlocked, "keep-unlocked", false,
		"have 'sand serve' hold its idle timer off while files are still on the previous key")
}

// set reports whether any of the plan flags was given.
func (f *rekeyFlags) set() bool {
	return f.perDay != "" || f.fullSpeed || len(f.folders) > 0 || f.onlyFolders || f.keepUnlocked
}

// settings turns the flags into a plan, over base — the plan already in force,
// or nothing.
func (f *rekeyFlags) settings(base vault.RekeySettings) (vault.RekeySettings, error) {
	out := base
	if f.perDay != "" && f.fullSpeed {
		return out, fmt.Errorf("--per-day and --full-speed are two answers to the same question; give one")
	}
	if f.perDay != "" {
		n, err := parseDailyBudget(f.perDay)
		if err != nil {
			return out, err
		}
		// A budget is a pace, so it takes a plan off full speed.
		out.BytesPerDay, out.FullSpeed = n, false
	}
	if f.fullSpeed {
		out.FullSpeed = true
	}
	if len(f.folders) > 0 {
		out.Folders = f.folders
	}
	if f.onlyFolders {
		out.OnlyFolders = true
	}
	if f.keepUnlocked {
		out.KeepUnlocked = true
	}
	if out.BytesPerDay == 0 && !out.FullSpeed && (len(out.Folders) > 0 || out.OnlyFolders || out.KeepUnlocked) {
		return out, fmt.Errorf("--folder, --only-folders and --keep-unlocked describe a background " +
			"re-encryption, so they need --per-day or --full-speed")
	}
	return out, nil
}

// parseDailyBudget reads a size the way people write one — 10G, 500M — and
// insists on a real one: no budget is spelled --pause, not --per-day 0.
func parseDailyBudget(text string) (int64, error) {
	n, err := parseSizeLimit(text, "--per-day")
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("--per-day: %q is not a daily budget — write one as a size such as 10G, "+
			"or use --pause to hold the plan", text)
	}
	return n, nil
}

func vaultRekeyCmd() *cobra.Command {
	var (
		flags  rekeyFlags
		pause  bool
		resume bool
		all    bool
		run    bool
		skip   string
		unskip string
	)

	cmd := &cobra.Command{
		Use:   "rekey",
		Short: "Show or set how a password change's re-encryption is paced",
		Long: `Show where the re-encryption behind a password change has got to, and change
how it proceeds.

A password change rotates the key your files are stored under, and every file
has to be rebuilt onto the new one. Done all at once that is a day of saturated
upload on every account; 'sand vault passwd --per-day 10G' spreads it out
instead, and this command adjusts the pace, the order and the holds while it
runs. The files move while the vault is open under 'sand serve', or in the
foreground here with --run. 'sand vault migrate' moves everything now and
holds the terminal; --full-speed has the server do the same in the background.

Until a file has moved, it is readable by anyone holding your previous password
and an old copy of the vault file or of a manifest backup — which is what the
figures here are counting down. A second password change is refused until they
reach zero.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			status, err := v.RekeyStatus()
			if err != nil {
				return err
			}

			// Leaving a file for last. "." is the one being moved right now,
			// which only 'sand serve' or another 'rekey --run' can be moving
			// from here, so from this process it usually names a path.
			if skip != "" {
				id := ""
				if skip != "." {
					e, err := v.EntryByPath(vault.MainScope, skip)
					if err != nil {
						return err
					}
					id = e.ID
				}
				path, err := v.DeferRekeyFile(id)
				if err != nil {
					return err
				}
				fmt.Printf("%s is left for last.\n", path)
			}
			if unskip != "" {
				e, err := v.EntryByPath(vault.MainScope, unskip)
				if err != nil {
					return err
				}
				path, err := v.UndeferRekeyFile(e.ID)
				if err != nil {
					return err
				}
				fmt.Printf("%s is back in its turn.\n", path)
			}
			if skip != "" || unskip != "" {
				if status, err = v.RekeyStatus(); err != nil {
					return err
				}
			}

			if flags.set() || pause || resume || all {
				if status.Done() {
					return fmt.Errorf("every file is already on the current key; there is nothing to plan")
				}
				base := vault.RekeySettings{}
				if status.Plan != nil {
					base = status.Plan.RekeySettings
				}
				settings, err := flags.settings(base)
				if err != nil {
					return err
				}
				if pause {
					settings.Paused = true
				}
				if resume {
					settings.Paused = false
				}
				if all {
					settings.Folders, settings.OnlyFolders = nil, false
				}
				if status, err = v.SetRekeyPlan(settings); err != nil {
					return err
				}
			}

			if run {
				return runPacedRekey(cmd.Context(), v)
			}
			printRekeyStatus(status)
			return nil
		},
	}

	flags.bind(cmd)
	cmd.Flags().BoolVar(&pause, "pause", false, "hold the plan without forgetting it")
	cmd.Flags().BoolVar(&resume, "resume", false, "take a paused plan up again")
	cmd.Flags().BoolVar(&all, "all", false, "forget the --folder order and take everything in turn")
	cmd.Flags().BoolVar(&run, "run", false,
		"run the plan in the foreground until everything has moved or Ctrl-C")
	cmd.Flags().StringVar(&skip, "skip", "",
		"leave this file (a vault path) for last, after everything else; '.' is the file being moved now")
	cmd.Flags().StringVar(&unskip, "unskip", "",
		"put a skipped file (a vault path) back in its turn")
	return cmd
}

// runPacedRekey moves files on the plan, here and now, waiting out the meter
// between them. It is the loop 'sand serve' runs, for a machine with no server.
func runPacedRekey(ctx context.Context, v *vault.Vault) error {
	for {
		step, err := v.RekeyStep(ctx, time.Now())
		if err != nil {
			return err
		}
		switch {
		case step.Moved:
			fmt.Printf("  re-encrypted %s (%s)\n", step.Path, formatBytes(step.Bytes))
			if step.Warning != "" {
				fmt.Fprintf(os.Stderr, "warning: %s\n", step.Warning)
			}
			continue
		case step.Skipped:
			fmt.Printf("  skipped %s, left for last\n", step.Path)
			continue
		case step.Warning != "":
			fmt.Fprintf(os.Stderr, "warning: %s\n", step.Warning)
			continue
		case step.Idle:
			status, err := v.RekeyStatus()
			if err != nil {
				return err
			}
			if status.Done() {
				fmt.Println("Done. Every file is on the current key, and the previous key is gone.")
				return nil
			}
			printRekeyStatus(status)
			if status.Plan == nil || !status.Plan.Active() {
				return fmt.Errorf("the plan is not running — 'sand vault rekey --resume', or --per-day to give it a budget")
			}
			return fmt.Errorf("the plan stops at its folders and those are done — 'sand vault rekey --all' takes the rest")
		}

		wait := step.Wait
		if wait <= 0 {
			wait = 5 * time.Second
		}
		fmt.Fprintf(os.Stderr, "  next file in %s\n", humanDuration(wait))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// printRekeyStatus writes the status the way 'sand vault status' writes its
// lines, one fact each.
func printRekeyStatus(s *vault.RekeyStatus) {
	if s.Done() {
		fmt.Println("Every file is stored under the vault's current key.")
		return
	}
	fmt.Printf("Previous key:     %d of %d file(s) still on it (%s of %s)\n",
		s.Pending, s.Total, formatBytes(s.PendingBytes), formatBytes(s.TotalBytes))
	if !s.Since.IsZero() {
		fmt.Printf("Password changed: %s\n", s.Since.Local().Format("2 Jan 2006 15:04"))
	}

	plan := s.Plan
	switch {
	case plan == nil || (plan.BytesPerDay == 0 && !plan.FullSpeed):
		fmt.Println("Pace:             not planned — 'sand vault migrate' moves everything now, or give")
		fmt.Println("                  'sand vault rekey --per-day 10G' a budget")
	case plan.Paused && plan.FullSpeed:
		fmt.Println("Pace:             paused at full speed — 'sand vault rekey --resume'")
	case plan.Paused:
		fmt.Printf("Pace:             paused at %s a day — 'sand vault rekey --resume'\n", formatBytes(plan.BytesPerDay))
	case plan.FullSpeed:
		fmt.Println("Pace:             full speed — as fast as the clouds take it, in the background")
	default:
		line := fmt.Sprintf("Pace:             %s a day", formatBytes(plan.BytesPerDay))
		if s.ETASeconds > 0 {
			line += fmt.Sprintf(" · about %s left", humanDuration(time.Duration(s.ETASeconds)*time.Second))
		}
		if s.NextInSeconds > 0 {
			line += fmt.Sprintf(" · next file in %s", humanDuration(time.Duration(s.NextInSeconds)*time.Second))
		}
		fmt.Println(line)
	}
	if plan != nil && len(plan.Folders) > 0 {
		var parts []string
		for _, f := range s.Folders {
			if !f.Queued {
				continue
			}
			if f.Files == 0 {
				parts = append(parts, f.Path+" (done)")
			} else {
				parts = append(parts, fmt.Sprintf("%s (%d file(s), %s)", f.Path, f.Files, formatBytes(f.Bytes)))
			}
		}
		fmt.Printf("Folders first:    %s\n", strings.Join(parts, ", "))
		if plan.OnlyFolders {
			fmt.Println("Then:             nothing — the rest waits ('sand vault rekey --all' takes it)")
		} else {
			fmt.Println("Then:             everything else")
		}
	}
	if plan != nil && plan.KeepUnlocked {
		fmt.Println("Idle timer:       held off by 'sand serve' while files are still on the previous key")
	}
	if s.Idle && plan != nil && plan.Active() {
		fmt.Println("State:            idle — the plan has done everything it was allowed to")
	}
	if s.Running {
		line := fmt.Sprintf("Moving now:       %s", s.Current)
		if s.CurrentSize > 0 {
			line += fmt.Sprintf(" · %s of %s", formatBytes(s.CurrentDone), formatBytes(s.CurrentSize))
		}
		fmt.Println(line)
	}
	if len(s.Deferred) > 0 {
		var parts []string
		for _, f := range s.Deferred {
			parts = append(parts, fmt.Sprintf("%s (%s)", f.Path, formatBytes(f.Bytes)))
		}
		fmt.Printf("Left for last:    %s\n", strings.Join(parts, ", "))
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

// humanDuration says a wait the way a person would: "about 3 days", "2h 10m",
// "40s". Nothing finer than a second.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 36*time.Hour:
		days := int((d + 12*time.Hour) / (24 * time.Hour))
		return fmt.Sprintf("%d days", days)
	case d >= time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d <= 0:
		return "now"
	default:
		return fmt.Sprintf("%ds", int((d+time.Second-1)/time.Second))
	}
}

// rekeyRefusal rewrites the vault's refusal of a second password change with
// the commands that lift it.
func rekeyRefusal(err error) error {
	if errors.Is(err, vault.ErrRekeyPending) {
		return fmt.Errorf("%w — 'sand vault migrate' moves them now; 'sand vault rekey' shows the plan", err)
	}
	return err
}
