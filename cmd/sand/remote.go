package main

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	cmd.AddCommand(remoteKindsCmd(), remoteAddCmd(), remoteListCmd(), remoteEditCmd(),
		remoteTestCmd(), remoteMeasureCmd(), remoteRemoveCmd())
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
			defer closeVault(v)

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
			defer closeVault(v)

			statuses, err := v.ProviderStatuses(context.Background())
			if err != nil {
				return err
			}
			if len(statuses) == 0 {
				fmt.Println("No accounts connected. Add one with 'sand remote add'.")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tPARTS\tSTORED\tFREE\tCOLOUR\tID")
			for _, s := range statuses {
				status := "online"
				if !s.Online {
					status = "OFFLINE"
					if s.Error != "" {
						status = "OFFLINE: " + firstLine(s.Error)
					}
				}
				// An account with no colour of its own is not colourless in the
				// browser — one is picked for it — so say "auto" rather than
				// leaving a blank that reads like a missing setting.
				colour := s.Color
				if colour == "" {
					colour = "auto"
				}
				// What is left on the account, which is a different question
				// from what SAND has put there: a local folder answers with the
				// drive it sits on, shared with everything else on the machine.
				// A backend that reports no quota says nothing rather than
				// nothing-in-particular.
				free := "—"
				if room := s.Usage.Remaining(); room > 0 {
					free = formatBytes(room)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
					s.Name, s.Kind, status, s.Shards, formatBytes(s.Stored), free, colour, s.ID)
			}
			return tw.Flush()
		},
	}
}

func remoteEditCmd() *cobra.Command {
	var (
		name     string
		color    string
		capacity string
		settings []string
	)

	cmd := &cobra.Command{
		Use:     "edit <name-or-id>",
		Aliases: []string{"rename", "set"},
		Short:   "Change an account's name, colour, declared capacity, or settings",
		Long: `Change what a connected account is called, the colour it wears, how big
you say it is, or how it reaches the backend.

The colour is the stripe down the account's card in the browser and the shade
of every part badge for a file it holds, which is what makes "which clouds is
this file on" a question you answer by eye. Give it as a hex value; without one
the browser picks a colour and keeps it as accounts come and go.

The capacity is for the backends that cannot answer for themselves. A bucket
reports no quota — S3 has never had a call for it — so what SAND put there has
nothing to be drawn against until you say what the bucket holds: the cap set in
the provider's console, or simply how much of it this vault may fill. Nothing
enforces it; it is the figure the usage bar is measured against, and it pairs
with 'sand remote measure', which counts what is actually in there.

  sand remote edit r2-cold --name r2-archive
  sand remote edit r2-cold --color '#38bdf8'
  sand remote edit r2-cold --color auto
  sand remote edit b2-cold --capacity '10 GB'
  sand remote edit b2-cold --capacity none

None of those three touches the credentials or the parts on the account:
nothing is uploaded, downloaded or re-encrypted by renaming a cloud.

--set is the exception, and is the one edit that reaches the account: it
changes the settings the connection is made from — a rotated access key, a
re-pasted refresh token, a moved bucket. Name them the way 'sand remote kinds'
lists them, once per setting, and SAND connects with the result before storing
it, so credentials the provider will not accept are refused rather than saved
over the ones that still work.

  sand remote edit s3-cold --set secret_access_key=...
  sand remote edit s3-cold --set endpoint=https://s3.eu-central-003.backblazeb2.com
  sand remote edit gdrive --set refresh_token=1//0g...

Changing where an account stores parts — its bucket, prefix, folder or path —
does not move the parts already there. SAND will look for them in the new place
and not find them; run 'sand check --all' after such a change.

For an account you sign in to, the browser's Edit dialog can put it through the
provider's consent screen again instead, which is easier than finding a refresh
token by hand.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("color") &&
				!cmd.Flags().Changed("capacity") && !cmd.Flags().Changed("set") {
				return fmt.Errorf("nothing to change — pass --name, --color, --capacity, or --set")
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			cfg, err := findProvider(v, args[0])
			if err != nil {
				return err
			}

			var edit vault.ProviderEdit
			if cmd.Flags().Changed("name") {
				edit.Name = &name
			}
			if cmd.Flags().Changed("color") {
				// "auto" is how you say "no colour of my own" on a command line,
				// where an empty --color= is easy to type by accident.
				chosen := color
				if strings.EqualFold(strings.TrimSpace(chosen), "auto") {
					chosen = ""
				}
				edit.Color = &chosen
			}
			if cmd.Flags().Changed("capacity") {
				// "none" is how you clear it on a command line, for the same
				// reason "auto" clears a colour.
				typed := capacity
				if strings.EqualFold(strings.TrimSpace(typed), "none") {
					typed = ""
				}
				bytes, err := provider.ParseCapacity(typed)
				if err != nil {
					return err
				}
				edit.Capacity = &bytes
			}
			if len(settings) > 0 {
				if edit.Options, err = parseSettings(settings); err != nil {
					return err
				}
			}

			updated, err := v.UpdateProvider(cmd.Context(), cfg.ID, edit)
			if err != nil {
				return err
			}

			shade := updated.Color
			if shade == "" {
				shade = "auto"
			}
			held := "no declared capacity"
			if updated.Capacity > 0 {
				held = "holds " + formatBytes(updated.Capacity)
			}
			fmt.Printf("%s (%s) — colour %s, %s\n", updated.Name, updated.Kind, shade, held)

			// The settings by name only. Half of them are credentials, and a
			// command that echoes a freshly pasted secret back onto the screen
			// puts it in a scrollback and a shell history.
			if len(edit.Options) > 0 {
				keys := make([]string, 0, len(edit.Options))
				for key := range edit.Options {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				fmt.Printf("reconnected — new %s\n", strings.Join(keys, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "a new label for this account")
	cmd.Flags().StringVar(&color, "color", "", "hex colour such as '#38bdf8', or 'auto' to let the browser pick")
	cmd.Flags().StringVar(&capacity, "capacity", "",
		"how big this account is, for backends that do not report it — '10 GB', or 'none' to clear it")
	cmd.Flags().StringArrayVar(&settings, "set", nil,
		"a connection setting as key=value, repeatable — see 'sand remote kinds' for the keys")
	return cmd
}

// parseSettings reads repeated --set key=value flags into an option map.
//
// An empty value is kept rather than skipped: --set folder_id= is how you clear
// an optional setting back to the backend's default, and is a different thing
// from not passing the flag at all.
func parseSettings(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--set %q is not a setting — write it as key=value", pair)
		}
		out[key] = value
	}
	return out, nil
}

// remoteMeasureCmd counts what is on an account that cannot say.
func remoteMeasureCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "measure <name-or-id>",
		Aliases: []string{"count"},
		Short:   "Count what is on an account by listing it",
		Long: `Count what is actually on an account, for the backends with no way to say.

A bucket reports no quota and no total, so the only honest figure is the sum of
a full listing — every object in it, SAND's parts and whatever else already
lives there. That costs a request per thousand objects, billed as a transaction
at some providers, so nothing takes it on a schedule: this command, and the
Count button in the account's Stats panel, are the two things that ask.

The figure is kept until the vault closes, so the browser draws it afterwards
without paying for it again. Pair it with 'sand remote edit --capacity' to give
it something to be measured against.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			cfg, err := findProvider(v, args[0])
			if err != nil {
				return err
			}
			usage, err := v.MeasureProvider(context.Background(), cfg.ID)
			if err != nil {
				return err
			}

			fmt.Printf("%s holds %s\n", cfg.Name, formatBytes(usage.Used))
			if usage.Total > 0 {
				fmt.Printf("  of the %s you declared for it · %s left\n",
					formatBytes(usage.Total), formatBytes(usage.Remaining()))
			} else {
				fmt.Printf("  with no capacity to measure that against — set one with " +
					"'sand remote edit --capacity'\n")
			}
			return nil
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
			defer closeVault(v)

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
			defer closeVault(v)

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
