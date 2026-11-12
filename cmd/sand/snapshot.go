package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// snapshotCmd is snapshots of a folder from a terminal: take one, list the
// ones a place is keeping, bring one back, erase one. See
// internal/vault/snapshot.go.
func snapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Snapshots of a folder: one compressed, password-sealed archive on a cloud or a machine",
		Long: "A snapshot is everything under a folder packed into one archive: compressed as hard as\n" +
			"zstd will, sealed under a password of its own, and kept on one connected cloud or on a\n" +
			"machine you have a login on. Over --max-size it is cut into part files, which are put\n" +
			"back together when it is restored.\n\n" +
			"  take     pack a folder into a snapshot\n" +
			"  ls       the snapshots a place is keeping\n" +
			"  restore  unpack one into a folder\n" +
			"  rm       erase one\n\n" +
			"Say where with --cloud <account> or --machine <name> [--dir <folder on it>].\n" +
			"The snapshot's password is asked for, or read from SAND_SNAPSHOT_PASSWORD.",
	}

	var cloud, machine, dir string
	placed := func(c *cobra.Command) *cobra.Command {
		c.Flags().StringVar(&cloud, "cloud", "", "keep it on this connected account (name or id)")
		c.Flags().StringVar(&machine, "machine", "", "keep it on this machine (name or id)")
		c.Flags().StringVar(&dir, "dir", "", "the folder on the machine, under its root")
		return c
	}
	// target turns the flags into a place, resolving names to IDs.
	target := func(v *vault.Vault) (vault.SnapshotTarget, error) {
		switch {
		case cloud != "" && machine != "":
			return vault.SnapshotTarget{}, fmt.Errorf("give --cloud or --machine, not both")
		case cloud != "":
			cfg, err := findProvider(v, cloud)
			if err != nil {
				return vault.SnapshotTarget{}, err
			}
			return vault.SnapshotTarget{Account: cfg.ID, Dir: dir}, nil
		case machine != "":
			id, err := findMachine(v, machine)
			if err != nil {
				return vault.SnapshotTarget{}, err
			}
			return vault.SnapshotTarget{Machine: id, Dir: dir}, nil
		}
		return vault.SnapshotTarget{}, fmt.Errorf("say where: --cloud <account> or --machine <name>")
	}

	var maxSize string
	take := placed(&cobra.Command{
		Use:   "take [folder]",
		Short: "Pack a folder into a snapshot",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			partSize, err := provider.ParseSize(maxSize)
			if err != nil {
				return err
			}
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			where, err := target(v)
			if err != nil {
				return err
			}
			password, err := readNewPasswordFrom("SAND_SNAPSHOT_PASSWORD", "Password for the snapshot: ")
			if err != nil {
				return err
			}

			// Ctrl-C stops it, and what had been written is erased.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			report, err := v.TakeSnapshot(ctx, scope, folderArg(args, 0), vault.SnapshotRequest{
				Target:   where,
				Password: password,
				PartSize: partSize,
				OnProgress: func(at vault.SnapshotProgress) {
					progressLine(at.Path, at.File, at.Files)
				},
			})
			clearProgressLine()
			if err != nil {
				return err
			}
			fmt.Printf("%s: %d file(s) in %d folder(s), %s packed into %s on %s\n",
				report.Name, report.Files, report.Folders, formatBytes(report.Bytes), formatBytes(report.Stored), report.Where)
			for _, part := range report.Parts {
				fmt.Printf("  %s\n", part)
			}
			return nil
		},
	})
	take.Flags().StringVar(&maxSize, "max-size", "",
		"the most one part file may be, like 2G or 500M (default: no limit on a machine, 64M on a cloud)")

	list := placed(&cobra.Command{
		Use:   "ls",
		Short: "The snapshots a place is keeping",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			where, err := target(v)
			if err != nil {
				return err
			}
			sets, err := v.ListSnapshots(context.Background(), where)
			if err != nil {
				return err
			}
			if len(sets) == 0 {
				fmt.Println("no snapshots there")
				return nil
			}
			for _, set := range sets {
				state := ""
				if !set.Complete {
					state = "  INCOMPLETE"
				}
				fmt.Printf("%s  %s  %d part(s)  %s%s\n",
					set.Name, set.TakenAt.Local().Format("2006-01-02 15:04"), len(set.Parts), formatBytes(set.Bytes), state)
			}
			return nil
		},
	})

	var replace bool
	restore := placed(&cobra.Command{
		Use:   "restore <snapshot> [folder]",
		Short: "Unpack a snapshot into a folder (default: the root)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			where, err := target(v)
			if err != nil {
				return err
			}
			password, err := readPasswordFrom("SAND_SNAPSHOT_PASSWORD", "Password for the snapshot: ")
			if err != nil {
				return err
			}

			// Ctrl-C stops it with what has arrived, which is whole; running
			// it again carries on.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			report, err := v.RestoreSnapshot(ctx, scope, folderArg(args, 1), vault.RestoreRequest{
				Target:   where,
				Name:     strings.TrimSuffix(args[0], ".sandsnap"),
				Password: password,
				Replace:  replace,
				OnProgress: func(at vault.SnapshotProgress) {
					progressLine(at.Path, at.File, at.Files)
				},
			})
			clearProgressLine()
			if err != nil {
				return err
			}
			fmt.Printf("%s into %s: %d restored, %d already there, %d left alone, %d failed\n",
				report.Name, report.Path, report.Restored, report.Skipped, report.Kept, report.Failed)
			for _, line := range report.Results {
				fmt.Printf("  %s: %s%s\n", line.Path, line.Reason, line.Error)
			}
			if report.Omitted > 0 {
				fmt.Printf("  … and %d more\n", report.Omitted)
			}
			if report.Stopped {
				return fmt.Errorf("stopped before the end — run it again to carry on")
			}
			if report.Failed > 0 {
				return fmt.Errorf("%d file(s) were not restored", report.Failed)
			}
			return nil
		},
	})
	restore.Flags().BoolVar(&replace, "replace", false,
		"write the snapshot's file over a different file already under its name")

	remove := placed(&cobra.Command{
		Use:   "rm <snapshot>",
		Short: "Erase a snapshot from where it is kept",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			where, err := target(v)
			if err != nil {
				return err
			}
			name := strings.TrimSuffix(args[0], ".sandsnap")
			if err := v.DeleteSnapshot(context.Background(), where, name); err != nil {
				return err
			}
			fmt.Printf("%s erased\n", name)
			return nil
		},
	})

	cmd.AddCommand(take, list, restore, remove)
	return cmd
}

// folderArg is the folder named at a position, or the root.
func folderArg(args []string, at int) string {
	if len(args) > at {
		return args[at]
	}
	return "/"
}

// findMachine resolves a machine by exact ID or case-insensitive name.
func findMachine(v *vault.Vault, ref string) (string, error) {
	sources, err := v.Sources()
	if err != nil {
		return "", err
	}
	for _, s := range sources {
		if s.ID == ref {
			return s.ID, nil
		}
	}
	for _, s := range sources {
		if strings.EqualFold(s.Name, ref) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no machine matching %q", ref)
}
