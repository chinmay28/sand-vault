package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/git"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// The repositories a vault is keeping a copy of, from the command line.
//
// A repository in a vault is one file — a git bundle holding its whole history
// — so these subcommands are mostly about the one thing a bundle cannot do for
// itself: notice that its upstream has moved. Keeping them current on a
// schedule is `sand automation set <folder> --task git`; this is how you put
// one in, see what is there, and bring one up to date by hand.

func gitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Keep copies of git repositories in the vault, and keep them current",
		Long: `A repository can be stored in a vault as a single file.

It is a git bundle: the whole history, every branch and every tag, in one file
that "git clone" reads directly. That is what makes it worth keeping — the copy
needs SAND to be made and stored, and nothing at all to be used again.

  sand git track https://github.com/chinmay28/sand-vault.git --into /code
  sand git list /code
  sand git refresh /code/sand-vault.bundle
  sand git rebuild /code/sand-vault.bundle --accounts a,b,c

Refreshing is cheap when nothing has changed. The upstream is asked what refs it
has, which is a few kilobytes and no objects at all; only a repository that has
actually moved costs a fetch. To have that happen on its own:

  sand automation set /code --weekly sun,04:00 --task git --action pull

SAND borrows the git already on this machine rather than carrying its own, so a
repository it can reach is exactly one you can reach from here — your SSH keys,
your credential helper, your ~/.ssh/config.`,
	}
	cmd.AddCommand(gitTrackCmd(), gitListCmd(), gitRefreshCmd(), gitRebuildCmd(), gitUntrackCmd())
	return cmd
}

func gitTrackCmd() *cobra.Command {
	var (
		into     string
		accounts []string
		scheme   string
	)

	cmd := &cobra.Command{
		Use:   "track <url>",
		Short: "Store a repository in the vault, and remember where it came from",
		Long: `Mirror a repository and store it as a bundle.

The whole history comes down once, which for a large project is the expensive
part; every refresh after it costs only the difference. The bundle is stored
like any other file — same accounts, same erasure code, same encryption — and
is named after the repository, so /code holds sand-vault.bundle rather than a
row of identifiers.

  sand git track https://github.com/chinmay28/sand-vault.git --into /code
  sand git track git@github.com:me/private.git --into /code --accounts a,b,c`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !git.Available() {
				return fmt.Errorf(
					"%w — SAND borrows the git already on the machine rather than carrying "+
						"its own, so this needs git installed", git.ErrUnavailable)
			}
			parsed, err := parseSchemeFlag(scheme)
			if err != nil {
				return err
			}

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			fmt.Printf("Mirroring %s — the first copy is the whole history.\n", args[0])
			started := time.Now()

			repo, warnings, err := v.TrackRepo(cmd.Context(), scope, into, args[0],
				vault.UploadOptions{Accounts: accounts, Scheme: parsed})
			if err != nil {
				return err
			}
			printWarnings(warnings)

			fmt.Printf("Stored %s (%s, %d refs, %d commits) in %s.\n",
				repo.Path, formatBytes(repo.Size), repo.Refs, repo.Commits,
				time.Since(started).Round(time.Second))
			return nil
		},
	}

	cmd.Flags().StringVar(&into, "into", "/", "folder to store the bundle in")
	cmd.Flags().StringSliceVar(&accounts, "accounts", nil,
		"accounts to spread the bundle over, by name or ID")
	cmd.Flags().StringVar(&scheme, "scheme", "",
		"erasure code to cut the bundle with, as k-of-n")
	return cmd
}

func gitListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [folder]",
		Short: "Show the repositories stored under a folder",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "/"
			if len(args) == 1 {
				dir = args[0]
			}

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			repos, err := v.TrackedRepos(scope, dir)
			if err != nil {
				return err
			}
			if len(repos) == 0 {
				fmt.Printf("No repositories stored under %s.\n", dir)
				return nil
			}

			for _, repo := range repos {
				printRepo(repo)
			}
			return nil
		},
	}
}

func gitRefreshCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "refresh [path]",
		Short: "Bring a stored repository up to date with its upstream",
		Long: `Ask a repository's upstream whether it has moved, and fetch it if it has.

Asking is cheap — a few kilobytes of ref advertisement, no objects — so a
repository that has not changed costs almost nothing and says so.

  sand git refresh /code/sand-vault.bundle
  sand git refresh /code --all`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !git.Available() {
				return fmt.Errorf("%w — this needs git installed", git.ErrUnavailable)
			}
			target := "/"
			if len(args) == 1 {
				target = args[0]
			}

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			repos, err := gitRefreshTargets(v, scope, target, all)
			if err != nil {
				return err
			}

			var updated, failed int
			for _, repo := range repos {
				after, moved, err := v.RefreshRepo(cmd.Context(), scope, repo.ID)
				switch {
				case err != nil:
					failed++
					fmt.Printf("%-40s %v\n", repo.Path, err)
				case moved:
					updated++
					fmt.Printf("%-40s updated — %d refs, %d commits, %s\n",
						after.Path, after.Refs, after.Commits, formatBytes(after.Size))
				default:
					fmt.Printf("%-40s already up to date\n", repo.Path)
				}
			}

			if len(repos) > 1 {
				fmt.Printf("\n%d checked, %d updated, %d failed.\n", len(repos), updated, failed)
			}
			if failed > 0 {
				return fmt.Errorf("%d repositor%s could not be brought up to date",
					failed, plural(failed, "y", "ies"))
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false,
		"treat the path as a folder and refresh every repository under it")
	return cmd
}

// gitRefreshTargets works out which repositories a refresh was pointed at: one
// named file, or everything under a folder.
func gitRefreshTargets(v *vault.Vault, scope vault.Scope, target string, all bool) ([]vault.TrackedRepo, error) {
	if all {
		repos, err := v.TrackedRepos(scope, target)
		if err != nil {
			return nil, err
		}
		if len(repos) == 0 {
			return nil, fmt.Errorf("no repositories are stored under %s", target)
		}
		return repos, nil
	}

	entry, err := v.EntryByPath(scope, target)
	if err != nil {
		return nil, err
	}
	repo, err := v.TrackedRepo(scope, entry.ID)
	if err != nil {
		return nil, err
	}
	return []vault.TrackedRepo{*repo}, nil
}

func gitRebuildCmd() *cobra.Command {
	var (
		accounts []string
		scheme   string
	)

	cmd := &cobra.Command{
		Use:   "rebuild <path>",
		Short: "Store a repository afresh from its upstream, on the accounts named",
		Long: `Mirror a tracked repository again from its upstream and store the bundle
over the one that is there, on the accounts named.

This is the way back for a bundle whose parts have gone past reading. A refresh
reads the stored bundle to fetch only the difference, and a relocation moves
its shards; neither has anything to work with once too few accounts answer.
The upstream still holds the whole history, and a rebuild reads nothing else:
the repository comes down whole, exactly as it did the first time, and goes
back up as a fresh set of parts on the clouds you name, cut the way those
clouds call for. The old parts are erased wherever they can still be reached.

It is offered whatever state the stored bundle is in — a bundle that still
reads is no reason to refuse putting it on other clouds from the upstream —
and it costs what the first copy cost, so it is not the way to keep one
current. That is 'sand git refresh'.

  sand git rebuild /code/sand-vault.bundle --accounts box,s3,drive
  sand git rebuild /code/sand-vault.bundle --accounts a,b,c,d,e --scheme 3-of-5`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !git.Available() {
				return fmt.Errorf("%w — this needs git installed", git.ErrUnavailable)
			}
			if len(accounts) == 0 {
				return fmt.Errorf("--accounts has to name the clouds the repository should be stored on")
			}
			parsed, err := parseSchemeFlag(scheme)
			if err != nil {
				return err
			}

			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			entry, err := v.EntryByPath(scope, args[0])
			if err != nil {
				return err
			}
			repo, err := v.TrackedRepo(scope, entry.ID)
			if err != nil {
				return err
			}

			fmt.Printf("Mirroring %s afresh — the whole history comes down, and the stored copy is not read.\n", repo.URL)
			started := time.Now()

			after, warnings, err := v.RebuildRepo(cmd.Context(), scope, repo.ID, accounts, parsed)
			if err != nil {
				return err
			}
			printWarnings(warnings)

			fmt.Printf("Stored %s afresh (%s, %d refs, %d commits) in %s.\n",
				after.Path, formatBytes(after.Size), after.Refs, after.Commits,
				time.Since(started).Round(time.Second))
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&accounts, "accounts", nil,
		"accounts to spread the fresh bundle over, by name or ID (required)")
	cmd.Flags().StringVar(&scheme, "scheme", "",
		"erasure code to cut it with, as k-of-n (default: from how many accounts were named)")
	return cmd
}

func gitUntrackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "untrack <path>",
		Short: "Stop following a repository's upstream, keeping the stored bundle",
		Long: `Forget that a stored bundle is a mirror of anything.

The file stays exactly where it is — it is a complete repository, and still
cloneable. What stops is SAND asking its upstream whether it has moved.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			entry, err := v.EntryByPath(scope, args[0])
			if err != nil {
				return err
			}
			if err := v.UntrackRepo(scope, entry.ID); err != nil {
				return err
			}
			fmt.Printf("%s is no longer followed. The file is still there.\n", args[0])
			return nil
		},
	}
}

// printRepo writes one stored repository and what is known about it.
func printRepo(repo vault.TrackedRepo) {
	fmt.Printf("%-38s %s\n", repo.Path, repo.URL)

	detail := fmt.Sprintf("%s, %d refs", formatBytes(repo.Size), repo.Refs)
	if repo.Head != "" {
		detail += ", " + repo.Head
	}
	if repo.Commits > 0 {
		detail += fmt.Sprintf(", %d commits", repo.Commits)
	}
	fmt.Printf("   %s\n", detail)

	fmt.Printf("   fetched %s", repo.FetchedAt.Local().Format("2 Jan 2006 15:04"))
	if !repo.CheckedAt.IsZero() && repo.CheckedAt.After(repo.FetchedAt) {
		fmt.Printf(", checked %s", repo.CheckedAt.Local().Format("2 Jan 2006 15:04"))
	}
	fmt.Println()

	if repo.Gone {
		fmt.Printf("   upstream has stopped answering: %s\n", strings.TrimSpace(repo.Reason))
	}
	fmt.Println()
}
