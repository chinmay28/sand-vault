package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// 'sand replicate': second copies of chosen folders, on clouds already
// connected, in a folder of their own on each.

func replicateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replicate",
		Short: "Keep a second copy of chosen folders on clouds of your choosing",
		Long: `A replica is a second, independent placement of a file: the same bytes cut
again, under their own archive ID and the current key, into a *replica folder*
on each of the clouds the policy names. Those clouds are ones already
connected; the replica folder is a second folder beside the one their parts
are in, and it is what keeps a replica apart from a part — nothing on a cloud
names a file, and nothing on a replica says it is one.

With one cloud a replica is the whole sealed file on that cloud, which is the
one placement in SAND where a single account holds a file. With several it is
cut k-of-n like anything else.

  sand replicate set --cloud r2-cold=sand-replica --cloud nas=/mnt/nas/replica \
      --cloud drive="Vault replicas" --scheme 2-of-3 --per-day 10G
  sand replicate add /photos
  sand replicate status
  sand replicate run

Replicas are made by 'sand serve' while the vault is open, at the pace --per-day
sets, or by 'sand replicate run' in the foreground. 'sand get --from replica'
reads one.`,
	}
	cmd.AddCommand(
		replicateStatusCmd(),
		replicateSetCmd(),
		replicateAddCmd(),
		replicateRemoveCmd(),
		replicateExcludeCmd(),
		replicateDropCmd(),
		replicateRunCmd(),
	)
	return cmd
}

func replicateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the policy and how far the copies have got",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			return printReplicationStatus(v)
		},
	}
}

func printReplicationStatus(v *vault.Vault) error {
	status, err := v.ReplicationStatus(time.Now())
	if err != nil {
		return err
	}
	configs, err := v.Providers()
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, cfg := range configs {
		names[cfg.ID] = cfg.Name
	}

	p := status.Policy
	if p == nil {
		fmt.Println("Replication: not set")
		if len(status.Folders) > 0 {
			fmt.Println("Replica folders kept:")
			for id, folder := range status.Folders {
				fmt.Printf("  %-20s %s\n", names[id], folder)
			}
		}
		return nil
	}

	code := p.Scheme().String()
	if p.Scheme().IsWholeCopy() {
		code = "whole copy"
	}
	fmt.Printf("Replication: %s across %d cloud(s)", code, len(p.Accounts))
	if p.BytesPerDay > 0 {
		fmt.Printf(", %s a day", formatBytes(p.BytesPerDay))
	}
	if p.Paused {
		fmt.Print(", paused")
	}
	fmt.Println()
	for _, id := range p.Accounts {
		fmt.Printf("  %-20s %s\n", names[id], status.Folders[id])
	}
	fmt.Printf("Replicated: %d file(s), %s on the clouds\n", status.Replicated, formatBytes(status.ReplicaBytes))
	if status.Pending > 0 {
		fmt.Printf("Pending:    %d file(s), %s", status.Pending, formatBytes(status.PendingBytes))
		if status.Stale > 0 {
			fmt.Printf(" (%d to make again)", status.Stale)
		}
		if status.ETA > 0 {
			fmt.Printf(", about %s at this pace", status.ETA.Round(time.Minute))
		}
		fmt.Println()
	}
	if status.Current != "" {
		fmt.Printf("Copying:    %s (%s of %s)\n", status.Current, formatBytes(status.CurrentDone), formatBytes(status.CurrentSize))
	}
	for _, w := range status.Warnings {
		fmt.Printf("Warning:    %s\n", w)
	}
	return nil
}

func replicateSetCmd() *cobra.Command {
	var (
		clouds []string
		scheme string
		perDay string
		pause  bool
		resume bool
		clear  bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Choose the replication clouds, their replica folders, and the code",
		Long: `Each --cloud names a connected account and the folder on it replicas go to,
as name=folder. The folder is of the same kind as the account's own folder
setting — a folder at the top of a Drive or a Dropbox, a key prefix in a
bucket, a directory for a folder on this machine — and it has to sit beside
the parts folder, not inside it. An account whose parts are at the root cannot
take replicas until its parts have a folder of their own.

One --cloud makes every replica a whole copy. More need a code: --scheme k-of-n
as wide as the clouds named, or the default for the count. --per-day paces the
copies, as it does a password change's re-encryption.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			if clear {
				if err := v.SetReplication(vault.ReplicationSettings{}); err != nil {
					return err
				}
				fmt.Println("Replication cleared. Replicas on the current key stay where they are; any still sealed under a retired key were erased.")
				return nil
			}

			current, folders, err := v.Replication()
			if err != nil {
				return err
			}
			settings := vault.ReplicationSettings{}
			if current != nil {
				settings.BytesPerDay = current.BytesPerDay
				settings.Paused = current.Paused
				settings.Scheme = current.Scheme()
				for _, id := range current.Accounts {
					settings.Accounts = append(settings.Accounts, vault.ReplicaAccount{ID: id, ReplicaFolder: folders[id]})
				}
			}
			if len(clouds) > 0 {
				settings.Accounts = nil
				settings.Scheme = archive.Scheme{}
				for _, spec := range clouds {
					name, folder, ok := strings.Cut(spec, "=")
					if !ok {
						return fmt.Errorf("--cloud wants name=folder, got %q", spec)
					}
					ids, err := resolveAccountNames(v, []string{name})
					if err != nil {
						return err
					}
					settings.Accounts = append(settings.Accounts, vault.ReplicaAccount{ID: ids[0], ReplicaFolder: folder})
				}
			}
			if scheme != "" {
				if strings.EqualFold(scheme, "whole") {
					settings.Scheme = archive.WholeCopy
				} else if settings.Scheme, err = archive.ParseScheme(scheme); err != nil {
					return err
				}
			}
			if perDay != "" {
				n, err := parseDailyBudget(perDay)
				if err != nil {
					return err
				}
				settings.BytesPerDay = n
			}
			if pause {
				settings.Paused = true
			}
			if resume {
				settings.Paused = false
			}
			if len(settings.Accounts) == 0 {
				return fmt.Errorf("name at least one cloud with --cloud name=folder, or --clear")
			}
			if err := v.SetReplication(settings); err != nil {
				return err
			}
			return printReplicationStatus(v)
		},
	}
	cmd.Flags().StringArrayVar(&clouds, "cloud", nil, "a replication cloud and its replica folder, name=folder (repeatable)")
	cmd.Flags().StringVar(&scheme, "scheme", "", "how a replica is cut: k-of-n as wide as the clouds, or 'whole' for one")
	cmd.Flags().StringVar(&perDay, "per-day", "", "copy at most this much a day — 10G, 500M; 0 for no limit")
	cmd.Flags().BoolVar(&pause, "pause", false, "stop making replicas, keeping the policy; replicas still on a retired key are erased")
	cmd.Flags().BoolVar(&resume, "resume", false, "start again after --pause")
	cmd.Flags().BoolVar(&clear, "clear", false, "forget the policy; replicas on the current key stay, ones on a retired key are erased")
	return cmd
}

func replicateFlagCmd(use, short string, enabled *bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <path>...",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			for _, path := range args {
				if err := v.SetReplicate(path, enabled); err != nil {
					return err
				}
			}
			return printReplicationStatus(v)
		},
	}
}

func replicateAddCmd() *cobra.Command {
	on := true
	return replicateFlagCmd("add", "Replicate a folder, or one file, and everything under it", &on)
}

func replicateRemoveCmd() *cobra.Command {
	return replicateFlagCmd("remove", "Put a folder or file back to following its parent; replicas already made stay", nil)
}

func replicateExcludeCmd() *cobra.Command {
	off := false
	return replicateFlagCmd("exclude", "Keep a folder or file out of replication whatever its parent says", &off)
}

func replicateDropCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drop <path-or-id>",
		Short: "Erase one file's replica now; its parts are untouched",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			entry, err := resolveEntry(v, vault.MainScope, args[0])
			if err != nil {
				return err
			}
			warnings, err := v.DropReplica(context.Background(), entry.ID)
			for _, w := range warnings {
				fmt.Fprintln(os.Stderr, "warning:", w)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Dropped the replica of %s\n", entry.Path())
			return nil
		},
	}
}

func replicateRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Make the replicas that are wanted, here in the foreground, until none is",
		Long: `Under 'sand serve' this happens on its own while the vault is open. This is
the same worker in the foreground, for a machine that does not run the server:
it keeps to --per-day, and Ctrl-C stops it between files.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			for {
				step, err := v.ReplicateStep(ctx, time.Now())
				if err != nil {
					if ctx.Err() != nil {
						fmt.Println("Stopped.")
						return nil
					}
					return err
				}
				switch {
				case step.Idle:
					fmt.Println("Every replicated file has a current replica.")
					return nil
				case step.Done:
					fmt.Printf("Replicated %s (%s)\n", step.Path, formatBytes(step.Bytes))
					if step.Warning != "" {
						fmt.Fprintln(os.Stderr, "warning:", step.Warning)
					}
				case step.Warning != "":
					fmt.Fprintln(os.Stderr, "warning:", step.Warning)
				case step.Wait > 0:
					fmt.Printf("Waiting %s for the daily budget…\n", step.Wait.Round(time.Second))
					select {
					case <-ctx.Done():
						fmt.Println("Stopped.")
						return nil
					case <-time.After(step.Wait):
					}
				}
			}
		},
	}
}
