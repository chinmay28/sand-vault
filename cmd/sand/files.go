package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

func lsCmd() *cobra.Command {
	var long bool

	cmd := &cobra.Command{
		Use:   "ls [path]",
		Short: "List a folder in the vault",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/"
			if len(args) == 1 {
				path = args[0]
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			listing, err := v.List(path)
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, folder := range listing.Folders {
				fmt.Fprintf(tw, "%s/\t\t\t\n", folder)
			}
			for _, f := range listing.Files {
				spread := describeSpread(f)
				if long {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
						f.Name, formatBytes(f.Size), f.ModifiedAt.Local().Format("2006-01-02 15:04"), spread, f.ID)
				} else {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
						f.Name, formatBytes(f.Size), f.ModifiedAt.Local().Format("2006-01-02 15:04"), spread)
				}
			}
			return tw.Flush()
		},
	}

	cmd.Flags().BoolVarP(&long, "long", "l", false, "also show entry IDs")
	return cmd
}

// describeSpread summarizes which accounts hold a file's parts.
func describeSpread(e *vault.Entry) string {
	if len(e.Shards) == 0 {
		return "UNRECOVERABLE"
	}
	names := make([]string, 0, len(e.Shards))
	for _, s := range e.Shards {
		names = append(names, fmt.Sprintf("p%d:%s", s.Part, s.ProviderName))
	}
	label := strings.Join(names, " ")
	if len(e.Shards) < 3 {
		label += " (degraded)"
	}
	return label
}

func findCmd() *cobra.Command {
	var (
		scope string
		kind  string
		limit int
		long  bool
	)

	cmd := &cobra.Command{
		Use:   "find <query>",
		Short: "Search the vault for a file or folder",
		Long: `Search the file index by name.

A bare word matches any name containing it, ignoring case. Use * and ? for
wildcards ("*.jpg", "report-202?.pdf"), and a query with a / in it is matched
against the whole path ("photos/2024") rather than the name alone.

The index is only readable while the vault is open, so this is the only way to
search at all — no connected account can be asked what it is holding.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			results, err := v.Search(vault.SearchOptions{
				Query: args[0],
				Dir:   scope,
				Kind:  vault.SearchKind(kind),
				Limit: limit,
			})
			if err != nil {
				return err
			}
			if len(results.Hits) == 0 {
				fmt.Fprintf(os.Stderr, "no matches for %q\n", results.Query)
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, hit := range results.Hits {
				if hit.File == nil {
					fmt.Fprintf(tw, "%s/\t\t\t\n", hit.Path)
					continue
				}
				f := hit.File
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s",
					hit.Path, formatBytes(f.Size),
					f.ModifiedAt.Local().Format("2006-01-02 15:04"), describeSpread(f))
				if long {
					fmt.Fprintf(tw, "\t%s", f.ID)
				}
				fmt.Fprintln(tw)
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			if results.Truncated {
				fmt.Fprintf(os.Stderr,
					"showing %d of %d matches — narrow the query or raise --limit\n",
					len(results.Hits), results.Matched)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&scope, "path", "/", "only search inside this folder")
	cmd.Flags().StringVar(&kind, "type", string(vault.SearchAll), "what to look for: all, file or folder")
	cmd.Flags().IntVar(&limit, "limit", vault.DefaultSearchLimit, "most matches to print")
	cmd.Flags().BoolVarP(&long, "long", "l", false, "also show entry IDs")
	return cmd
}

func putCmd() *cobra.Command {
	var (
		dest      string
		overwrite bool
		accounts  []string
	)

	cmd := &cobra.Command{
		Use:   "put <file>...",
		Short: "Upload files into the vault",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			chosen, err := resolveAccountNames(v, accounts)
			if err != nil {
				return err
			}

			ctx := context.Background()
			for _, path := range args {
				data, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("reading %s: %w", path, err)
				}

				entry, warnings, err := v.Upload(ctx, dest, filepath.Base(path), data, vault.UploadOptions{
					Overwrite: overwrite,
					Accounts:  chosen,
				})
				if err != nil {
					return fmt.Errorf("uploading %s: %w", path, err)
				}
				for _, warning := range warnings {
					fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
				}
				fmt.Printf("%s → %s  [%s]\n", path, entry.Path(), describeSpread(entry))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dest, "path", "/", "destination folder inside the vault")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing file with the same name")
	cmd.Flags().StringSliceVar(&accounts, "accounts", nil,
		"accounts to scatter these files over, by name or id (default: the vault's, or three at random)")
	return cmd
}

// resolveAccountNames turns what someone typed on the command line into
// connected account IDs. Names are what the rest of the CLI prints, so they are
// what it accepts; an ID still works for a name that is awkward to type.
func resolveAccountNames(v *vault.Vault, wanted []string) ([]string, error) {
	if len(wanted) == 0 {
		return nil, nil
	}

	configs, err := v.Providers()
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(wanted))
	for _, want := range wanted {
		want = strings.TrimSpace(want)
		matched := ""
		for _, cfg := range configs {
			if cfg.ID == want || strings.EqualFold(cfg.Name, want) {
				matched = cfg.ID
				break
			}
		}
		if matched == "" {
			return nil, fmt.Errorf("no connected account called %q — 'sand remote ls' lists them", want)
		}
		out = append(out, matched)
	}
	return out, nil
}

func getCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "get <path-or-id>",
		Short: "Download and decrypt a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			entry, err := resolveEntry(v, args[0])
			if err != nil {
				return err
			}

			data, _, err := v.Fetch(context.Background(), entry.ID)
			if err != nil {
				return err
			}

			target := output
			if target == "" {
				target = entry.Name
			} else if info, err := os.Stat(target); err == nil && info.IsDir() {
				target = filepath.Join(target, entry.Name)
			}

			if err := os.WriteFile(target, data, 0600); err != nil {
				return fmt.Errorf("writing %s: %w", target, err)
			}
			fmt.Printf("Wrote %s (%s)\n", target, formatBytes(int64(len(data))))
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "output file or directory")
	return cmd
}

func rmCmd() *cobra.Command {
	var recursive bool

	cmd := &cobra.Command{
		Use:   "rm <path-or-id>",
		Short: "Delete a file, or a folder with --recursive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			ctx := context.Background()
			target := args[0]

			// A path that names a folder is only ever a folder delete.
			if listing, listErr := v.List(target); listErr == nil && vault.CleanDir(target) != "/" {
				_ = listing
				warnings, err := v.Rmdir(ctx, target, recursive)
				printWarnings(warnings)
				if err != nil {
					return err
				}
				fmt.Printf("Deleted folder %s\n", vault.CleanDir(target))
				return nil
			}

			entry, err := resolveEntry(v, target)
			if err != nil {
				return err
			}
			warnings, err := v.Delete(ctx, entry.ID)
			printWarnings(warnings)
			if err != nil {
				return err
			}
			fmt.Printf("Deleted %s\n", entry.Path())
			return nil
		},
	}

	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "delete a folder and everything in it")
	return cmd
}

func mkdirCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <path>",
		Short: "Create a folder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			if err := v.Mkdir(args[0]); err != nil {
				return err
			}
			fmt.Printf("Created %s\n", vault.CleanDir(args[0]))
			return nil
		},
	}
}

func mvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mv <path-or-id> <new-path>",
		Short: "Rename or move a file",
		Long: `Rename or move a file within the vault. Only the index changes — the
encrypted parts stay exactly where they are on your cloud accounts.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			entry, err := resolveEntry(v, args[0])
			if err != nil {
				return err
			}
			// Move updates the entry in place, so record where it came from
			// before handing it over.
			from := entry.Path()

			dir, name := splitPath(args[1])
			// "sand mv a.txt /folder" moves into the folder, keeping the name.
			if _, listErr := v.List(args[1]); listErr == nil {
				dir, name = vault.CleanDir(args[1]), entry.Name
			}

			moved, err := v.Move(entry.ID, dir, name)
			if err != nil {
				return err
			}
			fmt.Printf("%s → %s\n", from, moved.Path())
			return nil
		},
	}
}

func checkCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "check [path-or-id]",
		Short: "Verify that every part of a file is still where it should be",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !all {
				return fmt.Errorf("give a file to check, or pass --all")
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			ctx := context.Background()
			var targets []*vault.Entry

			if all {
				targets, err = collectAll(v, "/")
				if err != nil {
					return err
				}
			} else {
				entry, err := resolveEntry(v, args[0])
				if err != nil {
					return err
				}
				targets = []*vault.Entry{entry}
			}

			problems := 0
			for _, entry := range targets {
				health, err := v.Health(ctx, entry.ID)
				if err != nil {
					return err
				}

				status := "ok"
				if !health.Recoverable {
					status = "UNRECOVERABLE"
					problems++
				} else {
					missing := 0
					for _, s := range health.Shards {
						if !s.Present {
							missing++
						}
					}
					if missing > 0 || len(health.Shards) < 3 {
						status = "degraded"
						problems++
					}
				}

				fmt.Printf("%-40s %s\n", health.Path, status)
				for _, s := range health.Shards {
					mark := "✓"
					detail := formatBytes(s.Size)
					if !s.Present {
						mark = "✗"
						detail = s.Error
					}
					fmt.Printf("   %s part %d on %-20s %s\n", mark, s.Part, s.ProviderName, detail)
				}
			}

			if problems > 0 {
				return fmt.Errorf("%d file(s) need attention", problems)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "check every file in the vault")
	return cmd
}

// collectAll walks the vault namespace and returns every entry.
func collectAll(v *vault.Vault, dir string) ([]*vault.Entry, error) {
	listing, err := v.List(dir)
	if err != nil {
		return nil, err
	}

	out := append([]*vault.Entry{}, listing.Files...)
	for _, folder := range listing.Folders {
		nested, err := collectAll(v, vault.JoinPath(dir, folder))
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

func printWarnings(warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
}
