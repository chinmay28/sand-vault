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
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "address to bind to")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", server.DefaultIdleTimeout,
		"re-lock the vault after this much inactivity")
	return cmd
}

func vaultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Create and manage the vault",
	}
	cmd.AddCommand(vaultInitCmd(), vaultStatusCmd(), vaultPasswdCmd(), vaultPolicyCmd())
	return cmd
}

func vaultInitCmd() *cobra.Command {
	var policy string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a new vault",
		Long: `Create the encrypted index that tracks your connected accounts and stored
files. The password protects the index and the account credentials; file
contents are encrypted under a random key stored inside it, so changing the
password later does not re-upload anything.`,
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
			return nil
		},
	}
}

func vaultPasswdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "passwd",
		Short: "Change the vault password",
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
			if err := v.ChangePassword(old, next); err != nil {
				return err
			}

			fmt.Println("Password changed. Stored files were not re-uploaded.")
			return nil
		},
	}
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
