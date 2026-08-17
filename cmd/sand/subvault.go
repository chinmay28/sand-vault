package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The sub vault commands.
//
// A sub vault is a vault inside the vault with a password of its own, which
// makes every one of these a two-password command: the vault's, to open the
// file at all, and then the sub vault's. For scripts that is SAND_PASSWORD and
// SAND_SUB_PASSWORD.
//
// Working inside one is not done here but with --in on the ordinary commands:
// "sand ls --in Taxes /Papers" lists a folder in a sub vault the same way
// "sand ls /Papers" lists one in the main vault, because it is the same
// operation against a different tree.

func subVaultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sub",
		Aliases: []string{"subvault"},
		Short:   "Vaults inside the vault, each with its own password",
		Long: `A sub vault holds what should not be readable by whoever holds the main
password. It is sealed under a password of its own and nothing else, so the
main password lists it and cannot open it.

It never appears on a WebDAV mount — not while locked, and not while unlocked
either. A mounted drive is a folder every process running as you can read; what
goes in a sub vault is what should not be reachable that way.

Use --in on the ordinary commands to work inside one:

    sand sub ls
    sand sub new Taxes
    sand ls --in Taxes /
    sand put --in Taxes ./p60.pdf --path /Papers`,
	}
	cmd.AddCommand(
		subVaultLsCmd(), subVaultNewCmd(), subVaultPasswdCmd(),
		subVaultAssignCmd(), subVaultRmCmd(), subVaultScanCmd(), subVaultImportCmd(),
	)
	return cmd
}

func subVaultLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the sub vaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			subs, err := v.SubVaults()
			if err != nil {
				return err
			}
			if len(subs) == 0 {
				fmt.Fprintln(os.Stderr, "no sub vaults — 'sand sub new <name>' makes one")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tFILES\tSTORED\tID")
			for _, s := range subs {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", s.Label, s.Files, formatBytes(s.Bytes), s.ID)
			}
			return tw.Flush()
		},
	}
}

func subVaultNewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new <name>",
		Short: "Make a sub vault with a password of its own",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()
			defer v.AwaitBackupSync()

			password, err := readNewPasswordFrom("SAND_SUB_PASSWORD",
				fmt.Sprintf("Password for the %q sub vault: ", args[0]))
			if err != nil {
				return err
			}

			info, err := v.CreateSubVault(args[0], password)
			if err != nil {
				return err
			}
			fmt.Printf("Made the sub vault %q.\n", info.Label)
			fmt.Fprintln(os.Stderr,
				"There is no recovery for this password. The main password will list this sub "+
					"vault and will not open it.")
			return nil
		},
	}
}

func subVaultPasswdCmd() *cobra.Command {
	var noMigrate bool

	cmd := &cobra.Command{
		Use:   "passwd <name>",
		Short: "Change a sub vault's password and re-encrypt what it stores",
		Long: `Change one sub vault's password and rotate the key its files are stored under.

It works the way the vault's own password change does, and for the same reason:
the parts on your accounts are encrypted under a key held inside the sub vault
rather than under the password, so changing the password only means something if
that key changes too. A fresh key is generated and every file in the sub vault is
rebuilt onto it. Nothing is unreadable while that runs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()
			defer v.AwaitBackupSync()

			id, err := subVaultIDFor(v, args[0])
			if err != nil {
				return err
			}

			old, err := readPasswordFrom("SAND_SUB_PASSWORD",
				fmt.Sprintf("Current password for %q: ", args[0]))
			if err != nil {
				return err
			}
			if err := v.UnlockSubVault(id, old); err != nil {
				return err
			}
			next, err := readNewPasswordFrom("SAND_NEW_SUB_PASSWORD", "New password: ")
			if err != nil {
				return err
			}

			report, err := v.ChangeSubVaultPassword(cmd.Context(), id, old, next, false)
			if err != nil {
				return err
			}
			fmt.Printf("Password changed for %q.\n", args[0])

			if noMigrate {
				if report.Remaining > 0 {
					fmt.Printf("%d file(s) are still stored under the old key — until "+
						"'sand sub passwd' has finished migrating them, the old password would "+
						"still open their parts.\n", report.Remaining)
				}
				return nil
			}
			return runScopedMigration(cmd, v, vault.Scope(id))
		},
	}

	cmd.Flags().BoolVar(&noMigrate, "no-migrate", false,
		"change the password now and leave the files on the old key")
	return cmd
}

func subVaultAssignCmd() *cobra.Command {
	var out bool

	cmd := &cobra.Command{
		Use:   "assign <path> <name>",
		Short: "Move a file or a folder into a sub vault",
		Long: `Move a file or folder from the main vault into a sub vault, or with --out, back
the other way.

The path is kept. A folder assigned from /Papers/2019 arrives in the sub vault
at /Papers/2019, so sending it back puts it where it was.

Nothing is uploaded or downloaded to make the move — the index changes and the
parts stay where they are. The files do have to be re-encrypted onto the
destination's key afterwards, which this waits for, because until it finishes
the vault they came from can still read them.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()
			defer v.AwaitBackupSync()

			scope, err := unlockSubVault(v, args[1])
			if err != nil {
				return err
			}

			from, to := vault.MainScope, scope
			if out {
				from, to = scope, vault.MainScope
			}

			report, err := v.Assign(cmd.Context(), from, args[0], to, true)
			if report != nil {
				printWarnings(report.Warnings)
			}
			if err != nil {
				return err
			}

			where := fmt.Sprintf("into %q", args[1])
			if out {
				where = fmt.Sprintf("out of %q", args[1])
			}
			fmt.Printf("Moved %d file(s) %s.\n", report.Files, where)
			if report.Renamed > 0 {
				fmt.Printf("%d landed under a different name, because the destination already "+
					"had one at that path.\n", report.Renamed)
			}
			if report.Migration != nil && report.Migration.Remaining > 0 {
				fmt.Printf("%d file(s) are still on the key they arrived under — run "+
					"'sand sub passwd' or try again to finish.\n", report.Migration.Remaining)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&out, "out", false, "move out of the sub vault and back into the main one")
	return cmd
}

func subVaultRmCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a sub vault and erase everything in it",
		Long: `Delete a sub vault. Its parts are erased from your accounts, not merely
forgotten.

A locked sub vault needs --force, because what is about to be erased cannot be
listed. It can still be erased: the vault keeps an inventory of the objects each
sub vault owns — where they are and how big, and nothing about what they are —
so a sub vault whose password is gone does not leave its parts on your accounts
for good.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()
			defer v.AwaitBackupSync()

			id, err := subVaultIDFor(v, args[0])
			if err != nil {
				return err
			}

			warnings, err := v.DeleteSubVault(cmd.Context(), id, force)
			printWarnings(warnings)
			if err != nil {
				return err
			}
			fmt.Printf("Deleted the sub vault %q.\n", args[0])
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "delete it even though it is locked and cannot be listed")
	return cmd
}

func subVaultScanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Look for another vault's index on the connected accounts",
		Long: `Ask each connected account what it is holding, and say which of them carry an
index this vault cannot open.

Every vault replicates its index to every account it is connected to, so an
account you have used before carries one. Reconnecting an old cloud after a
machine died means you are looking straight at your own files — 'sand sub
import' brings them in as a sub vault of this one, which is the case
'sand vault recover' refuses because this vault already holds files.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			scan, err := v.ScanForRecovery(cmd.Context())
			if err != nil {
				return err
			}

			others := 0
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, src := range scan.Sources {
				what := "no index"
				switch {
				case src.Backup && src.Foreign:
					what = "another vault"
					others++
				case src.Backup:
					what = "this vault"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d part(s)\t%s\n",
					src.Name, what, src.Parts, formatBytes(src.Bytes))
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			if others == 0 {
				fmt.Fprintln(os.Stderr, "no other vault's index on any connected account")
				return nil
			}
			fmt.Fprintf(os.Stderr,
				"\n%d account(s) hold another vault's index — 'sand sub import <account>' "+
					"brings one in as a sub vault of this one.\n", others)
			return nil
		},
	}
}

func subVaultImportCmd() *cobra.Command {
	var label string
	var keepBackup bool

	cmd := &cobra.Command{
		Use:   "import <account>",
		Short: "Bring a vault found on an account in as a sub vault",
		Long: `Import the vault whose index is on an account, as a sub vault of this one.

This is what to reach for when 'sand vault recover' refuses because this vault
already holds files. A backup carries an index, a data key and a password that
opens it, which is exactly what a sub vault is — so the found vault lands beside
yours rather than replacing it.

You are asked for two passwords: the old vault's, to open its index, and the one
the sub vault will answer to from here. The second costs nothing — the old data
key is adopted as it stands, so no file is re-encrypted by the import itself —
and it is worth choosing, because otherwise you are stuck with a password picked
on a machine that is gone. Run 'sand sub passwd' afterwards to rotate the key
too, which is what stops the old password opening the parts.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()
			defer v.AwaitBackupSync()

			providerID, err := resolveAccountID(v, args[0])
			if err != nil {
				return err
			}

			old, err := readPasswordFrom("SAND_BACKUP_PASSWORD", "Password of the vault being imported: ")
			if err != nil {
				return err
			}
			next, err := readNewPasswordFrom("SAND_SUB_PASSWORD", "Password for the imported sub vault: ")
			if err != nil {
				return err
			}

			report, err := v.ImportAsSubVault(cmd.Context(), providerID, old, vault.ImportOptions{
				Label:       label,
				Password:    next,
				AdoptBackup: !keepBackup,
			})
			if report != nil {
				printWarnings(report.Warnings)
			}
			if err != nil {
				return err
			}

			fmt.Printf("Imported %d file(s) in %d folder(s) as the sub vault %q.\n",
				report.Files, report.Folders, report.SubVault.Label)
			if report.Unreachable > 0 {
				fmt.Printf("%d part(s) are on accounts that are not connected here — connect them "+
					"and run 'sand check --all' to pick them up.\n", report.Unreachable)
			}
			if report.Recoverable < report.Files {
				fmt.Printf("%d of %d file(s) have enough parts to rebuild right now.\n",
					report.Recoverable, report.Files)
			}
			fmt.Fprintln(os.Stderr,
				"The imported files are still on the key the old password opens. "+
					"'sand sub passwd' rotates it and re-encrypts them.")
			return nil
		},
	}

	cmd.Flags().StringVar(&label, "name", "", "what to call the imported sub vault (default: the account's name)")
	cmd.Flags().BoolVar(&keepBackup, "keep-backup", false,
		"leave the old index on that account, which also stops this vault backing up to it")
	return cmd
}

// subVaultIDFor resolves a sub vault by name or ID without opening it.
func subVaultIDFor(v *vault.Vault, name string) (string, error) {
	subs, err := v.SubVaults()
	if err != nil {
		return "", err
	}
	for _, s := range subs {
		if s.ID == name || strings.EqualFold(s.Label, name) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no sub vault called %q — 'sand sub ls' lists them", name)
}

// resolveAccountID turns an account name or ID into an ID, the way the file
// commands resolve the accounts named for an upload.
func resolveAccountID(v *vault.Vault, name string) (string, error) {
	ids, err := resolveAccountNames(v, []string{name})
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no connected account called %q", name)
	}
	return ids[0], nil
}

// runScopedMigration re-encrypts one vault's outstanding files, reporting as it
// goes. It is runMigration for a sub vault.
func runScopedMigration(cmd *cobra.Command, v *vault.Vault, scope vault.Scope) error {
	report, err := v.MigrateFilesIn(cmd.Context(), scope, nil, progressLine)
	clearProgressLine()
	if report != nil {
		printWarnings(report.Warnings)
	}
	if err != nil {
		return err
	}
	if report.Pending == 0 {
		return nil
	}

	fmt.Printf("Re-encrypted %d of %d file(s), %s.\n",
		report.Migrated, report.Pending, formatBytes(report.Bytes))
	if report.Remaining > 0 {
		return fmt.Errorf("%d file(s) are still on the old key; run the command again", report.Remaining)
	}
	return nil
}
