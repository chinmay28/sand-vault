package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/vault"
)

func remoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remote",
		Aliases: []string{"account", "cloud"},
		Short:   "Connect and manage cloud accounts",
	}
	cmd.AddCommand(remoteKindsCmd(), remoteAddCmd(), remoteListCmd(), remoteTestCmd(), remoteRemoveCmd())
	return cmd
}

func remoteKindsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kinds",
		Short: "List the backends SAND can connect to and their settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, spec := range provider.Specs() {
				fmt.Printf("%s (%s)\n  %s\n", spec.Label, spec.Kind, spec.Description)
				if spec.OAuth != nil {
					fmt.Printf("  Easiest from the browser: 'sand serve', then Connect a cloud → %s.\n",
						spec.Label)
					if !spec.OAuth.Configured {
						fmt.Printf("  Set %s (and its secret) to skip registering an app each time.\n",
							spec.OAuth.ClientIDEnv)
					}
				}
				for _, f := range spec.Fields {
					flags := []string{}
					if f.Required {
						flags = append(flags, "required")
					}
					if f.Secret {
						flags = append(flags, "secret")
					}
					if f.Default != "" {
						flags = append(flags, "default: "+f.Default)
					}
					suffix := ""
					if len(flags) > 0 {
						suffix = "  [" + strings.Join(flags, ", ") + "]"
					}
					fmt.Printf("    --set %s=…  %s%s\n", f.Key, f.Label, suffix)
					if f.Help != "" {
						fmt.Printf("        %s\n", f.Help)
					}
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func remoteAddCmd() *cobra.Command {
	var (
		name     string
		settings []string
	)

	cmd := &cobra.Command{
		Use:   "add <kind>",
		Short: "Connect a cloud account",
		Long: `Connect a cloud account as a shard destination.

Run 'sand remote kinds' to see the available backends and which settings each
one needs. Settings are supplied as --set key=value pairs, for example:

  sand remote add local --name offsite --set path=/mnt/backup/sand
  sand remote add s3 --name r2 --set bucket=shards --set access_key_id=… \
      --set secret_access_key=… --set endpoint=https://<acct>.r2.cloudflarestorage.com`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options := map[string]string{}
			for _, kv := range settings {
				key, value, found := strings.Cut(kv, "=")
				if !found {
					return fmt.Errorf("--set expects key=value, got %q", kv)
				}
				options[strings.TrimSpace(key)] = value
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			cfg, err := v.AddProvider(context.Background(), provider.Config{
				Kind:    provider.Kind(args[0]),
				Name:    name,
				Options: options,
			})
			if err != nil {
				return err
			}

			fmt.Printf("Connected %s (%s) — id %s\n", cfg.Name, cfg.Kind, cfg.ID)

			if accounts, err := v.Providers(); err == nil && len(accounts) < 3 && v.Policy() == vault.PolicyStrict {
				fmt.Printf("You have %d account(s) connected. Strict placement uses one account per part, "+
					"so connect %d more for full three-part redundancy.\n", len(accounts), 3-len(accounts))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "a label for this account (defaults to the backend name)")
	cmd.Flags().StringArrayVar(&settings, "set", nil, "backend setting as key=value (repeatable)")
	return cmd
}

func remoteListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Show connected accounts and their status",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			statuses, err := v.ProviderStatuses(context.Background())
			if err != nil {
				return err
			}
			if len(statuses) == 0 {
				fmt.Println("No accounts connected. Add one with 'sand remote add'.")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tPARTS\tSTORED\tID")
			for _, s := range statuses {
				status := "online"
				if !s.Online {
					status = "OFFLINE"
					if s.Error != "" {
						status = "OFFLINE: " + firstLine(s.Error)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
					s.Name, s.Kind, status, s.Shards, formatBytes(s.Stored), s.ID)
			}
			return tw.Flush()
		},
	}
}

func remoteTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name-or-id>",
		Short: "Check that an account is reachable",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			cfg, err := findProvider(v, args[0])
			if err != nil {
				return err
			}
			if err := v.TestProvider(context.Background(), cfg.ID); err != nil {
				return fmt.Errorf("%s is not reachable: %w", cfg.Name, err)
			}
			fmt.Printf("%s is online\n", cfg.Name)
			return nil
		},
	}
}

func remoteRemoveCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "remove <name-or-id>",
		Aliases: []string{"rm"},
		Short:   "Disconnect a cloud account",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			cfg, err := findProvider(v, args[0])
			if err != nil {
				return err
			}
			if err := v.RemoveProvider(cfg.ID, force); err != nil {
				return err
			}
			fmt.Printf("Disconnected %s. The parts it held were left in place; delete them there if you want them gone.\n", cfg.Name)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "disconnect even if it makes files unrecoverable")
	return cmd
}

// findProvider resolves an account by exact ID or case-insensitive name.
func findProvider(v *vault.Vault, ref string) (provider.Config, error) {
	accounts, err := v.Providers()
	if err != nil {
		return provider.Config{}, err
	}
	for _, cfg := range accounts {
		if cfg.ID == ref {
			return cfg, nil
		}
	}
	for _, cfg := range accounts {
		if strings.EqualFold(cfg.Name, ref) {
			return cfg, nil
		}
	}
	return provider.Config{}, fmt.Errorf("no connected account matching %q", ref)
}

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
