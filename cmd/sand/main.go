// Command sand is the CLI and local server for SAND Vault, a file store that
// splits, encrypts and scatters your files across the cloud accounts you
// connect to it.
//
// The product is "SAND Vault"; the command stays `sand`, the way Visual Studio
// Code ships as `code`. The service name, data directory and vault filename
// stay on `sand` too, so an existing install upgrades without migration.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/version"
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
		Long: `SAND Vault stores files as three encrypted parts spread over separate cloud
accounts. Any two parts rebuild the original; any one part on its own is
useless. The browser at "sand serve" reassembles files on demand.

There is also a standalone mode — "sand archive" and "sand restore" — that
produces three zip files you can move around by hand, with no accounts and no
vault involved.`,
		Version:      version.String(),
		SilenceUsage: true,
	}

	root.PersistentFlags().String("vault", "", "path to the vault file (default ~/.sand/vault.sand)")
	// Named --in rather than --sub-vault because --vault is already taken, by
	// the file on disk. "sand ls --in Taxes" also happens to read as what it
	// does: list, in Taxes.
	root.PersistentFlags().String("in", "",
		"work inside a sub vault, by name or id (prompts for its password)")

	root.AddCommand(
		versionCmd(),
		serveCmd(),
		vaultCmd(),
		subVaultCmd(),
		remoteCmd(),
		lsCmd(),
		findCmd(),
		putCmd(),
		getCmd(),
		rmCmd(),
		mkdirCmd(),
		mvCmd(),
		relocateCmd(),
		automationCmd(),
		gitCmd(),
		checkCmd(),
		archiveCmd(),
		restoreCmd(),
		manifestCmd(),
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
			fmt.Printf("SAND Vault %s\n", version.String())
			return nil
		},
	}
}
