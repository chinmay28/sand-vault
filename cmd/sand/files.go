package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/chinmay28/sand-vault/internal/archive"
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

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			listing, err := v.List(scope, path)
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

// describeSpread summarizes which accounts hold a file's shards, and under
// which code — a 6-of-9 file lists nine of them.
func describeSpread(e *vault.Entry) string {
	if len(e.Shards) == 0 {
		return "UNRECOVERABLE"
	}
	names := make([]string, 0, len(e.Shards))
	for _, s := range e.Shards {
		names = append(names, fmt.Sprintf("s%d:%s", s.Part, s.ProviderName))
	}
	label := e.Scheme().String() + " " + strings.Join(names, " ")
	if e.Redundancy() < e.Scheme().Total {
		label += " (degraded)"
	}
	return label
}

// missingShards counts the shards a health check could not find.
func missingShards(health *vault.FileHealth) int {
	missing := 0
	for _, s := range health.Shards {
		if !s.Present {
			missing++
		}
	}
	return missing
}

func findCmd() *cobra.Command {
	var (
		within string
		kind   string
		limit  int
		long   bool
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
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			results, err := v.Search(vault.SearchOptions{
				Vault: scope,
				Query: args[0],
				Dir:   within,
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

	cmd.Flags().StringVar(&within, "path", "/", "only search inside this folder")
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
		scheme    string
	)

	cmd := &cobra.Command{
		Use:   "put <path>...",
		Short: "Upload files or whole folders into the vault",
		Long: `Upload files or whole folders into the vault.

A folder goes up as a folder: everything under it is uploaded, however deep,
and lands under a folder of the same name inside --path. Each file in it is
still compressed, split and scattered on its own, so a folder is a convenience
here and not a different kind of thing in the vault.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			chosen, err := resolveAccountNames(v, accounts)
			if err != nil {
				return err
			}
			cut, err := parseSchemeFlag(scheme)
			if err != nil {
				return err
			}

			opts := vault.UploadOptions{Overwrite: overwrite, Accounts: chosen, Scheme: cut}

			ctx := context.Background()
			for _, arg := range args {
				info, err := os.Stat(arg)
				if err != nil {
					return fmt.Errorf("reading %s: %w", arg, err)
				}
				if info.IsDir() {
					if err := putTree(ctx, v, scope, dest, arg, opts); err != nil {
						return err
					}
					continue
				}
				if err := putFile(ctx, v, scope, dest, arg, opts); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dest, "path", "/", "destination folder inside the vault — a named folder lands under it")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing file with the same name")
	cmd.Flags().StringSliceVar(&accounts, "accounts", nil,
		"accounts to scatter these files over, by name or id (default: the vault's, or three at random)")
	cmd.Flags().StringVar(&scheme, "scheme", "",
		"the code to cut with, k-of-n — 3-of-5, 6-of-10 (default: 2m-of-3m, from how many accounts were named)")
	return cmd
}

// putFile uploads one file and says where it landed.
//
// Streamed from disk rather than read into memory first: a folder of films is
// exactly the thing this command is now asked to take, and reading each one
// whole would cost its size in RAM on a machine that may be a Raspberry Pi.
func putFile(ctx context.Context, v *vault.Vault, scope vault.Scope, dir, source string, opts vault.UploadOptions) error {
	f, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("reading %s: %w", source, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading %s: %w", source, err)
	}

	entry, warnings, err := v.UploadStreamAt(ctx, scope, dir, filepath.Base(source), f, info.Size(), opts)
	if err != nil {
		return fmt.Errorf("uploading %s: %w", source, err)
	}
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	fmt.Printf("%s → %s  [%s]\n", source, entry.Path(), describeSpread(entry))
	return nil
}

// putTree uploads a folder and everything under it, keeping its shape.
//
// The folder itself is kept: putting ./photos into / gives /photos, not the
// contents of photos strewn across the root. Empty folders are made too — a
// folder that arrives with its empty corners missing is not the folder that was
// uploaded.
//
// Symlinks are not followed. WalkDir does not follow them by default, and that
// is the behaviour to want here: a link pointing back up its own tree would
// otherwise be uploaded forever, and a link out of the tree would quietly put
// somebody's home directory in the vault.
func putTree(ctx context.Context, v *vault.Vault, scope vault.Scope, dir, source string, opts vault.UploadOptions) error {
	label, err := folderName(source)
	if err != nil {
		return err
	}
	root := vault.JoinPath(vault.CleanDir(dir), label)

	// The walk starts at what the path points at rather than at the path, so
	// naming a symlinked folder uploads the folder instead of reporting that a
	// symlink is not a regular file. Links found inside it are still left
	// alone — this resolves the one the person named, nothing more.
	walk := source
	if resolved, err := filepath.EvalSymlinks(source); err == nil {
		walk = resolved
	}

	return filepath.WalkDir(walk, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}

		rel, err := filepath.Rel(walk, name)
		if err != nil {
			return err
		}
		target := root
		if rel != "." {
			for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
				target = vault.JoinPath(target, segment)
			}
		}

		if entry.IsDir() {
			// Made on the way down, so the files inside it have somewhere to go
			// and so a folder holding nothing still arrives.
			if err := v.Mkdir(scope, target); err != nil {
				return fmt.Errorf("making %s: %w", target, err)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			fmt.Fprintf(os.Stderr, "skipping %s: not a regular file\n", name)
			return nil
		}
		return putFile(ctx, v, scope, path.Dir(target), name, opts)
	})
}

// folderName is what an uploaded folder is called in the vault. Normally the
// last part of the path given, but "." and ".." and a bare separator name a
// folder without saying its name, so those are resolved against where the
// command is being run from before the name is taken.
func folderName(source string) (string, error) {
	name := filepath.Base(filepath.Clean(source))
	if name != "." && name != ".." && name != string(os.PathSeparator) {
		return name, nil
	}

	full, err := filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", source, err)
	}
	name = filepath.Base(full)
	if name == string(os.PathSeparator) {
		return "", fmt.Errorf("%s is the whole filesystem — name a folder to upload", source)
	}
	return name, nil
}

// parseSchemeFlag reads a --scheme value. Empty is not a malformed scheme but
// the absence of a choice, which leaves the code to how many accounts were
// named.
func parseSchemeFlag(raw string) (archive.Scheme, error) {
	if strings.TrimSpace(raw) == "" {
		return archive.Scheme{}, nil
	}
	return archive.ParseScheme(strings.TrimSpace(raw))
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
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			entry, err := resolveEntry(v, scope, args[0])
			if err != nil {
				return err
			}

			target := output
			if target == "" {
				target = entry.Name
			} else if info, err := os.Stat(target); err == nil && info.IsDir() {
				target = filepath.Join(target, entry.Name)
			}

			// Copied chunk by chunk into the file rather than rebuilt in memory
			// and written out: a 4 GB film should cost a 4 GB file on disk and
			// not 4 GB of RAM on the way there.
			body, _, err := v.OpenReadSeeker(context.Background(), entry.ID)
			if err != nil {
				if errors.Is(err, vault.ErrNeedsConversion) {
					return fmt.Errorf("%w\n\nConvert it first: sand vault convert %q", err, entry.Path())
				}
				return err
			}

			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
			if err != nil {
				return fmt.Errorf("writing %s: %w", target, err)
			}
			written, err := io.Copy(out, body)
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				os.Remove(target)
				return fmt.Errorf("writing %s: %w", target, err)
			}
			fmt.Printf("Wrote %s (%s)\n", target, formatBytes(written))
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
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			ctx := context.Background()
			target := args[0]

			// A path that names a folder is only ever a folder delete.
			if listing, listErr := v.List(scope, target); listErr == nil && vault.CleanDir(target) != "/" {
				_ = listing
				warnings, err := v.Rmdir(ctx, scope, target, recursive)
				printWarnings(warnings)
				if err != nil {
					return err
				}
				fmt.Printf("Deleted folder %s\n", vault.CleanDir(target))
				return nil
			}

			entry, err := resolveEntry(v, scope, target)
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
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			if err := v.Mkdir(scope, args[0]); err != nil {
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
		Short: "Rename or move a file or a folder",
		Long: `Rename or move a file — or a folder and everything under it — within the
vault. Only the index changes: the encrypted parts stay exactly where they are
on your cloud accounts, whichever of the two this is.

  sand mv /draft.txt /final/published.txt   rename, and move it
  sand mv /draft.txt /final                 into a folder, keeping the name
  sand mv /photos/2024 /archive/2024        a folder, with everything in it`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			// A folder first: it is the only reading of the source that cannot
			// also be a file, since the two namespaces never collide.
			if from := vault.CleanDir(args[0]); from != "/" && v.FolderExists(scope, from) {
				return moveFolderTo(v, scope, from, args[1])
			}

			entry, err := resolveEntry(v, scope, args[0])
			if err != nil {
				return err
			}
			// Move updates the entry in place, so record where it came from
			// before handing it over.
			from := entry.Path()

			dir, name := splitPath(args[1])
			// "sand mv a.txt /folder" moves into the folder, keeping the name.
			if _, listErr := v.List(scope, args[1]); listErr == nil {
				dir, name = vault.CleanDir(args[1]), entry.Name
			}

			moved, err := v.Move(context.Background(), entry.ID, dir, name)
			if err != nil {
				return err
			}
			fmt.Printf("%s → %s\n", from, moved.Path())
			return nil
		},
	}
}

// moveFolderTo carries out the folder half of "sand mv", where the destination
// reads the same way it does for a file: a folder that already exists means
// "inside it, keeping the name", and anything else is the new name in full.
func moveFolderTo(v *vault.Vault, scope vault.Scope, from, target string) error {
	to := vault.CleanDir(target)
	if v.FolderExists(scope, to) {
		_, name := splitPath(from)
		to = vault.JoinPath(to, name)
	}

	if err := v.MoveFolder(context.Background(), scope, from, to); err != nil {
		return err
	}
	fmt.Printf("%s → %s\n", from, to)
	return nil
}

func relocateCmd() *cobra.Command {
	var (
		accounts []string
		scheme   string
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "relocate <path-or-id>",
		Short: "Move a file or a folder onto different cloud accounts",
		Long: `Move a file — or a folder and everything under it — onto the cloud accounts
you name.

Only what has to move moves. A part already on one of the chosen accounts stays
exactly where it is, so swapping one cloud out of three copies one part rather
than rewriting the file. What does move is carried across as the encrypted blob
it already is: nothing is decrypted, nothing is re-encrypted, and the file keeps
its identity, its hash and its chunk layout.

Naming a different *number* of accounts changes the scheme the file is cut with
— m groups of three give 2m-of-3m — and no shard survives that change, so the
file is gathered and written out again rather than moved. That costs the whole
file on the wire, and --dry-run says so before it happens.

Each file is committed on its own, so this is safe to interrupt and safe to
repeat — run it again and it moves whatever is still in the wrong place.

  sand relocate /photos --accounts box,s3,drive
  sand relocate /taxes --accounts box,s3,drive,dropbox,onedrive,proton
  sand relocate /notes.txt --accounts box,s3 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(accounts) == 0 {
				return fmt.Errorf("name the accounts to move onto with --accounts")
			}

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			chosen, err := resolveAccountNames(v, accounts)
			if err != nil {
				return err
			}
			cut, err := parseSchemeFlag(scheme)
			if err != nil {
				return err
			}

			plan, err := v.PlanRelocation(scope, args[0], chosen, cut)
			if err != nil {
				return err
			}
			printRelocationPlan(plan)

			if dryRun {
				// Only the dry run prints the plan's warnings here. The real
				// run carries every one of them into its report, and saying
				// them twice reads as two problems.
				printWarnings(plan.Warnings)
				return nil
			}
			// Recoded counts too: a file whose scheme is changing has no shard
			// to move, so a move that is only a recode would otherwise stop
			// here having printed a plan it never carried out.
			if plan.Moves == 0 && plan.Drops == 0 && plan.Recoded == 0 {
				return nil
			}

			report, err := v.Relocate(cmd.Context(), scope, args[0], chosen, cut, progressLine)
			clearProgressLine()
			printWarnings(report.Warnings)
			if err != nil {
				return err
			}

			fmt.Printf("Moved %d part(s), %s, across %d file(s).\n",
				report.PartsMoved, formatBytes(report.Bytes), report.Relocated)
			if report.PartsDrop > 0 {
				fmt.Printf("Erased %d spare part(s) the chosen accounts had no room for.\n", report.PartsDrop)
			}
			if !report.Done() {
				return fmt.Errorf("%d file(s) did not fully move — run it again once the accounts are answering",
					report.Partial+report.Failed)
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&accounts, "accounts", nil,
		"the accounts to move onto, by name or id; ending up under another scheme rebuilds the file")
	cmd.Flags().StringVar(&scheme, "scheme", "",
		"the code to end up cut with, k-of-n — 3-of-5, 6-of-10 (default: 2m-of-3m, from how many accounts were named)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would move, and move nothing")
	return cmd
}

// printRelocationPlan says what a move comes to before it starts: how much of
// the work is already done, what has to travel, and what it will cost on each
// account.
func printRelocationPlan(plan *vault.RelocationPlan) {
	what := "file"
	if plan.Folder {
		what = "folder"
	}
	fmt.Printf("%s %s: %d file(s), %d already in place\n", what, plan.Path, plan.Total, plan.Unchanged)

	// Recodes first, because they are the expensive half and saying "nothing to
	// move" above them reads as "nothing to do" when the whole file is about to
	// come down and go back up.
	if plan.Recoded > 0 {
		fmt.Printf("%d file(s) to rebuild under a different scheme, %s in all\n",
			plan.Recoded, formatBytes(plan.RecodeBytes))
		for _, f := range plan.Files {
			if f.Recode {
				fmt.Printf("  %s\t%s → %s\t%s\n", f.Path, f.From, f.To, formatBytes(f.Bytes))
			}
		}
	}

	switch {
	case plan.Moves == 0 && plan.Drops == 0:
		if plan.Recoded == 0 {
			fmt.Println("Nothing to move — every part is already on one of those accounts.")
		}
		return
	case plan.Moves == 0:
		// Narrowing the accounts rather than changing them: nothing travels,
		// and "0 parts to move" is not what is about to happen.
		fmt.Printf("Nothing to move, %d spare part(s) to erase\n", plan.Drops)
	default:
		fmt.Printf("%d part(s) to move, %s in all\n", plan.Moves, formatBytes(plan.Bytes))
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, f := range plan.Files {
		for _, m := range f.Moves {
			fmt.Fprintf(tw, "  %s\tpart %d\t%s → %s\t%s\n",
				f.Path, m.Part, m.FromName, m.ToName, formatBytes(m.Bytes))
		}
	}
	tw.Flush()
	if plan.Truncated {
		fmt.Printf("  … and more (showing the first %d file(s))\n", len(plan.Files))
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

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			ctx := context.Background()
			var targets []*vault.Entry

			if all {
				targets, err = collectAll(v, scope, "/")
				if err != nil {
					return err
				}
			} else {
				entry, err := resolveEntry(v, scope, args[0])
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
				} else if missing := missingShards(health); missing > 0 {
					status = "degraded"
					problems++
				}

				fmt.Printf("%-40s %s  [%s, %d spare]\n",
					health.Path, status, health.Scheme, health.Spare)
				for _, s := range health.Shards {
					mark := "✓"
					detail := formatBytes(s.Size)
					if !s.Present {
						mark = "✗"
						detail = s.Error
					}
					fmt.Printf("   %s shard %d on %-20s %s\n", mark, s.Part, s.ProviderName, detail)
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
func collectAll(v *vault.Vault, scope vault.Scope, dir string) ([]*vault.Entry, error) {
	listing, err := v.List(scope, dir)
	if err != nil {
		return nil, err
	}

	out := append([]*vault.Entry{}, listing.Files...)
	for _, folder := range listing.Folders {
		nested, err := collectAll(v, scope, vault.JoinPath(dir, folder))
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

// progressLine redraws a one-line "where it has got to" on stderr.
//
// Piped or redirected, it prints nothing at all rather than a page of half-
// overwritten lines and a stray escape sequence: a progress indicator is for
// somebody watching, and the report at the end is what a script reads.
func progressLine(path string, done, total int) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	fmt.Fprintf(os.Stderr, "\r\033[K[%d/%d] %s", done, total, path)
}

// clearProgressLine takes the progress line back off the screen, so whatever
// is printed next starts on a line of its own.
func clearProgressLine() {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}
