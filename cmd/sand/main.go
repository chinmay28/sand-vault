// Command sand is the CLI and local server for SAND, a file store that
// splits, encrypts and scatters your files across the cloud accounts you
// connect to it.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sand-project/sand/internal/version"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "sand",
		Short: "Split, encrypt and scatter files across your cloud accounts",
		Long: `SAND stores files as three encrypted parts spread over separate cloud
accounts. Any two parts rebuild the original; any one part on its own is
useless. The browser at "sand serve" reassembles files on demand.

There is also a standalone mode — "sand archive" and "sand restore" — that
produces three zip files you can move around by hand, with no accounts and no
vault involved.`,
		Version:      version.String(),
		SilenceUsage: true,
	}

	root.PersistentFlags().String("vault", "", "path to the vault file (default ~/.sand/vault.sand)")

	root.AddCommand(
		versionCmd(),
		serveCmd(),
		vaultCmd(),
		remoteCmd(),
		lsCmd(),
		putCmd(),
		getCmd(),
		rmCmd(),
		mkdirCmd(),
		mvCmd(),
		checkCmd(),
		archiveCmd(),
		restoreCmd(),
	)
	return root
}

// versionCmd prints the running build. The quick-start installer reads this to
// report what it rolled back to, so keep the output a single stable line.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("sand %s\n", version.String())
			return nil
		},
	}
}
