package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/server"
	"github.com/chinmay28/sand-vault/internal/vault"
)

func serveCmd() *cobra.Command {
	var (
		port        int
		bind        string
		idleTimeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the local web server and file browser",
		RunE: func(cmd *cobra.Command, args []string) error {
			s := &server.Server{
				Bind:        bind,
				Port:        port,
				VaultPath:   vaultPath(cmd),
				IdleTimeout: idleTimeout,
			}
			return s.Start()
		},
	}

	// 8123 rather than 8080: 8080 is the most contested port on a developer
	// machine, and a vault that silently fails to bind — or worse, that you
	// reach and find is somebody else's service — is a bad first experience.
	cmd.Flags().IntVar(&port, "port", server.DefaultPort, "port to listen on")
	cmd.Flags().StringVar(&bind, "bind", server.DefaultBind, "address to bind to")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", server.DefaultIdleTimeout,
		"re-lock the vault after this much inactivity")
	return cmd
}

func vaultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Create and manage the vault",
	}
	cmd.AddCommand(vaultInitCmd(), vaultStatusCmd(), vaultPasswdCmd(), vaultMigrateCmd(),
		vaultPolicyCmd(), vaultDefaultsCmd(), vaultBackupCmd(), vaultRecoverCmd())
	return cmd
}

func vaultInitCmd() *cobra.Command {
	var policy string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a new vault",
		Long: `Create the encrypted index that tracks your connected accounts and stored
files. The password protects the index and the account credentials; file
contents are encrypted under a random key stored inside it, which is rotated —
and every file rebuilt onto the new one — if you ever change the password.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := vault.Open(vaultPath(cmd))
			if err != nil {
				return err
			}
			if v.Initialized() {
				return fmt.Errorf("a vault already exists at %s", v.Path())
			}

			password, err := readNewPassword("New vault password: ")
			if err != nil {
				return err
			}
			if err := v.Init(password, vault.Policy(policy)); err != nil {
				return err
			}

			fmt.Printf("Created vault at %s (placement policy: %s)\n", v.Path(), v.Policy())
			fmt.Println("Next: connect at least two cloud accounts with 'sand remote add'.")
			return nil
		},
	}

	cmd.Flags().StringVar(&policy, "policy", string(vault.PolicyStrict),
		"shard placement: strict (never two parts on one account) or redundant (always store all three)")
	return cmd
}

func vaultStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what the vault holds",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			stats, err := v.Stats()
			if err != nil {
				return err
			}

			fmt.Printf("Vault:            %s\n", v.Path())
			fmt.Printf("Placement policy: %s\n", stats.Policy)
			fmt.Printf("Accounts:         %d\n", stats.Accounts)
			fmt.Printf("Files:            %d in %d folder(s)\n", stats.Files, stats.Folders)
			fmt.Printf("Original size:    %s\n", formatBytes(stats.Bytes))
			fmt.Printf("Stored size:      %s (compressed, split and encrypted)\n", formatBytes(stats.StoredBytes))
			if stats.Degraded > 0 {
				fmt.Printf("Degraded:         %d file(s) are stored with fewer than 3 parts\n", stats.Degraded)
			}
			if stats.Pending > 0 {
				fmt.Printf("Awaiting re-key:  %d file(s) are still under the previous key — run 'sand vault migrate'\n",
					stats.Pending)
			}
			return nil
		},
	}
}

func vaultPasswdCmd() *cobra.Command {
	var noMigrate bool

	cmd := &cobra.Command{
		Use:   "passwd",
		Short: "Change the vault password and re-encrypt what it stores",
		Long: `Change the password and rotate the key your files are stored under.

The password is changed immediately. Because the parts on your accounts are
encrypted under a key held inside the vault rather than under the password
itself, changing the password only means something if that key changes too —
otherwise anyone with the old password and an old copy of the vault file, or of
the backup on any connected account, could still read every part.

So a fresh key is generated and every file is rebuilt onto it: each one is
gathered from its parts, re-encrypted, scattered again, and the parts the old
key opened are erased. That is a download and an upload per file, and it can
take a while. Nothing is unreadable while it runs, and it can be interrupted:
'sand vault migrate' picks up wherever it stopped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := vault.Open(vaultPath(cmd))
			if err != nil {
				return err
			}
			if !v.Initialized() {
				return fmt.Errorf("no vault at %s", v.Path())
			}

			old, err := readPassword("Current password: ")
			if err != nil {
				return err
			}
			next, err := readNewPassword("New password: ")
			if err != nil {
				return err
			}

			// The password change and the re-encryption are run as two steps
			// so this can report on the second one as it goes.
			if _, err := v.ChangePassword(cmd.Context(), old, next, false); err != nil {
				return err
			}
			defer v.Lock()
			// The copies of the index on the accounts are sealed under the old
			// password and carry the key being retired, so the push replacing
			// them has to land before this process goes away.
			defer v.AwaitBackupSync()
			fmt.Println("Password changed.")

			if noMigrate {
				if pending := v.PendingMigration(); pending > 0 {
					fmt.Printf("%d file(s) are still stored under the old key — "+
						"until 'sand vault migrate' has run, the old password and a copy of the "+
						"old vault file would still open their parts.\n", pending)
				}
				return nil
			}
			return runMigration(cmd, v)
		},
	}

	cmd.Flags().BoolVar(&noMigrate, "no-migrate", false,
		"change the password now and leave the files on the old key for 'sand vault migrate'")
	return cmd
}

func vaultMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Re-encrypt files still stored under an old key",
		Long: `Finish the re-encryption a password change started.

Run this after a password change that was interrupted, deferred with
--no-migrate, or held up by an account that was offline at the time. It moves
whatever is still on the old key and erases the parts left behind. Files that
have already moved are not touched, so running it again is free.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			if v.PendingMigration() == 0 {
				fmt.Println("Every file is already stored under the vault's current key.")
				return nil
			}
			return runMigration(cmd, v)
		},
	}
}

// runMigration re-encrypts everything left on an old key, reporting as it goes:
// a large vault can be at this for a long time, and silence would be
// indistinguishable from a hang.
func runMigration(cmd *cobra.Command, v *vault.Vault) error {
	pending := v.PendingMigration()
	if pending == 0 {
		return nil
	}
	fmt.Printf("Re-encrypting %d file(s) under the new key…\n", pending)

	report, err := v.MigrateFiles(cmd.Context(), func(path string, done, total int) {
		fmt.Printf("  [%d/%d] %s\n", done, total, path)
	})
	if report != nil {
		printWarnings(report.Warnings)
		fmt.Printf("Re-encrypted %d of %d file(s) (%s).\n",
			report.Migrated, report.Pending, formatBytes(report.Bytes))
		if report.Remaining > 0 {
			fmt.Printf("%d file(s) could not be moved and are still readable under the old key, "+
				"which the vault keeps until they are — fix what is reported above and run "+
				"'sand vault migrate'.\n", report.Remaining)
		}
	}
	if err != nil {
		return err
	}

	// The copies on the accounts carry the keys, so let the push settle before
	// the process exits and the report claims the change is complete.
	v.AwaitBackupSync()
	return nil
}

func vaultDefaultsCmd() *cobra.Command {
	var clear bool

	cmd := &cobra.Command{
		Use:   "defaults [account]...",
		Short: "Show or set the accounts uploads go to by default",
		Long: `Every file is split into three parts, each of which goes to a different
account. With more than three connected, this is which three an upload uses
when it does not say otherwise — 'sand put --accounts' overrides it per upload.

With no default set, each file picks its own three at random, so a large vault
still spreads over everything connected instead of filling the same three.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			if len(args) == 0 && !clear {
				return printDefaultAccounts(v)
			}
			if len(args) > 0 && clear {
				return fmt.Errorf("--clear takes no accounts")
			}

			chosen, err := resolveAccountNames(v, args)
			if err != nil {
				return err
			}
			if err := v.SetDefaultAccounts(chosen); err != nil {
				return err
			}
			if clear {
				fmt.Println("Default cleared — each upload now picks its accounts at random.")
				return nil
			}
			return printDefaultAccounts(v)
		},
	}

	cmd.Flags().BoolVar(&clear, "clear", false, "forget the default and pick accounts per upload instead")
	return cmd
}

// printDefaultAccounts names the vault's default accounts, resolving the
// stored IDs against what is connected so the output reads like the rest of
// the CLI rather than like the index.
func printDefaultAccounts(v *vault.Vault) error {
	defaults := v.DefaultAccounts()
	if len(defaults) == 0 {
		fmt.Println("No default set — each upload picks its accounts at random.")
		return nil
	}

	configs, err := v.Providers()
	if err != nil {
		return err
	}
	for _, id := range defaults {
		name := id
		for _, cfg := range configs {
			if cfg.ID == id {
				name = fmt.Sprintf("%s (%s)", cfg.Name, cfg.Kind)
				break
			}
		}
		fmt.Println(name)
	}
	return nil
}

func vaultPolicyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "policy [strict|redundant]",
		Short: "Show or change the shard placement policy",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			if len(args) == 0 {
				fmt.Println(v.Policy())
				return nil
			}
			if err := v.SetPolicy(vault.Policy(args[0])); err != nil {
				return err
			}
			fmt.Printf("Placement policy set to %s (applies to new uploads)\n", v.Policy())
			return nil
		},
	}
}
