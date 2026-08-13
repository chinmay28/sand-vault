package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// The commands here exist for the day the vault file is gone. Every connected
// account carries a copy of the encrypted index, so a password plus any one
// account rebuilds the vault — and a password plus enough loose part files
// rebuilds a single file with no vault and no network at all.

func vaultBackupCmd() *cobra.Command {
	var (
		force   bool
		enable  bool
		disable bool
	)

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write the encrypted index to every connected account",
		Long: `Replicate the vault's index to each connected account as ` + vault.BackupKey + `.

The copy is encrypted under your vault password and carries its own key
derivation parameters, so it can be opened with the password alone — no vault
file required. It holds the file tree, the map of which account holds which
part, and the key those parts are encrypted under. It never holds the
credentials for any account.

This runs automatically whenever the index changes. Use this command to force
it, or to turn it off with --disable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if enable && disable {
				return fmt.Errorf("pick one of --enable or --disable")
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			switch {
			case disable:
				warnings, err := v.SetBackupEnabled(cmd.Context(), false)
				if err != nil {
					return err
				}
				printWarnings(warnings)
				fmt.Println("Manifest backup off. Existing copies have been erased from the connected accounts.")
				fmt.Println("Losing the vault file now means losing every stored file — back it up yourself.")
				return nil

			case enable:
				if _, err := v.SetBackupEnabled(cmd.Context(), true); err != nil {
					return err
				}
			}

			if !v.BackupEnabled() {
				return fmt.Errorf("manifest backup is off for this vault — turn it on with 'sand vault backup --enable'")
			}

			warnings, err := v.SyncManifestBackup(cmd.Context(), force)
			printWarnings(warnings)
			if err != nil {
				return err
			}

			accounts, err := v.Providers()
			if err != nil {
				return err
			}
			fmt.Printf("Wrote %s to %d of %d account(s).\n",
				vault.BackupKey, len(accounts)-len(warnings), len(accounts))
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite a backup written by a different vault or an older password")
	cmd.Flags().BoolVar(&enable, "enable", false, "turn the manifest backup on for this vault")
	cmd.Flags().BoolVar(&disable, "disable", false,
		"turn it off and erase the copies already on the accounts")
	return cmd
}

func vaultRecoverCmd() *cobra.Command {
	var (
		from           string
		dryRun         bool
		backupPassword string
	)

	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Rebuild a lost vault from an account's copy of the index",
		Long: `Rebuild this vault's index from the copy stored on a connected account.

Start from a fresh vault, connect at least the accounts holding your parts,
then run this. The recovered index brings back the folder tree, the file names,
and the key needed to read the parts, so files can be opened again straight
away.

Reconnecting an account gives it a new internal ID, so this asks each connected
account which parts it actually holds and re-points the index at whichever
account answers. Accounts you have not reconnected show up as unreachable
parts; connect them and run it again.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer v.Lock()

			accounts, err := v.Providers()
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				return fmt.Errorf("connect the accounts holding your parts first, with 'sand remote add'")
			}

			source := accounts[0]
			if from != "" {
				if source, err = findProvider(v, from); err != nil {
					return err
				}
			}

			// The backup was sealed under the password of the vault that is
			// gone, which need not be the password of the vault doing the
			// recovering — so it is read separately.
			password := backupPassword
			if password == "" {
				if password, err = readPasswordFrom("SAND_BACKUP_PASSWORD",
					"Password of the vault being recovered: "); err != nil {
					return err
				}
			}

			snapshot, err := v.FetchBackup(cmd.Context(), source.ID, password)
			if err != nil {
				return err
			}
			fmt.Printf("Read a backup from %s, written %s.\n",
				source.Name, snapshot.CreatedAt.Local().Format("2006-01-02 15:04"))

			report, err := v.Recover(cmd.Context(), snapshot, dryRun)
			if err != nil {
				return err
			}
			printWarnings(report.Warnings)

			verb := "Recovered"
			if dryRun {
				verb = "Would recover"
			}
			fmt.Printf("%s %d file(s) in %d folder(s).\n", verb, report.Files, report.Folders)
			if report.Relocated > 0 {
				fmt.Printf("  %d part(s) re-pointed at the accounts now holding them.\n", report.Relocated)
			}
			if report.Unreachable > 0 {
				fmt.Printf("  %d part(s) are on accounts that are not connected.\n", report.Unreachable)
			}
			if report.Recoverable < report.Files {
				fmt.Printf("  %d file(s) do not have enough reachable parts to open — connect the missing accounts and run this again.\n",
					report.Files-report.Recoverable)
			}
			if dryRun {
				fmt.Println("\nNothing was changed. Run again without --dry-run to apply.")
				return nil
			}

			// This vault is now the legitimate owner of these accounts, so it
			// replaces the backups the lost vault left behind — they are still
			// sealed under the old password.
			v.AwaitBackupSync()
			warnings, err := v.SyncManifestBackup(cmd.Context(), true)
			printWarnings(warnings)
			if err != nil {
				return err
			}
			fmt.Printf("\nThe accounts now carry a backup under this vault's password.\n")
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "",
		"account to read the backup from (defaults to the first connected account)")
	cmd.Flags().StringVar(&backupPassword, "backup-password", "",
		"password of the vault being recovered (prompted for, or read from SAND_BACKUP_PASSWORD)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be recovered without changing anything")
	return cmd
}

func manifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Read a manifest backup file directly",
		Long: `Inspect a ` + vault.BackupKey + ` file taken from one of your accounts.

These commands need nothing but the file and the password that wrote it, which
makes them the last line of recovery: they work on a machine that has never
seen this vault, with no accounts connected.`,
	}
	cmd.AddCommand(manifestLsCmd())
	return cmd
}

func manifestLsCmd() *cobra.Command {
	var long bool

	cmd := &cobra.Command{
		Use:   "ls <manifest.sand>",
		Short: "Print the file tree recorded in a manifest backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			snapshot, err := openManifestFile(args[0])
			if err != nil {
				return err
			}

			entries := append([]*vault.Entry(nil), snapshot.Manifest.Entries...)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Path() < entries[j].Path() })

			fmt.Printf("Backup written %s — %d file(s), %d account(s), %s placement\n\n",
				snapshot.CreatedAt.Local().Format("2006-01-02 15:04"),
				len(entries), len(snapshot.Accounts), snapshot.Policy)

			for _, e := range entries {
				fmt.Printf("%-40s %10s\n", e.Path(), formatBytes(e.Size))
				if !long {
					continue
				}
				for _, s := range e.Shards {
					fmt.Printf("    part %d  %-20s %s\n", s.Part, s.ProviderName, s.Key)
				}
			}

			if len(entries) == 0 {
				fmt.Println("(the backup records no files)")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&long, "long", false, "also show which account holds each part, and under what key")
	return cmd
}

// openManifestFile reads a manifest backup off disk and decrypts it.
func openManifestFile(path string) (*vault.Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	password, err := readPassword("Vault password (the one that wrote this backup): ")
	if err != nil {
		return nil, err
	}
	snapshot, err := vault.OpenBackup(data, password)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// restoreWithManifest resolves the secret and destination for a restore that
// was handed a manifest backup.
//
// Parts written by the vault are encrypted under its random data key, not under
// the user's password, so a password alone cannot open them. The manifest
// carries that key, which is what turns "two part files and a password" into a
// recovered file.
func restoreWithManifest(manifestPath string, partPaths []string, preserveTree bool, outputDir string) (password, dir, vaultPath string, err error) {
	snapshot, err := openManifestFile(manifestPath)
	if err != nil {
		return "", "", "", err
	}
	password, err = snapshot.ShardPassword()
	if err != nil {
		return "", "", "", err
	}

	dir = outputDir
	if entry := entryForParts(snapshot, partPaths); entry != nil {
		vaultPath = entry.Path()
		if preserveTree && entry.Dir != "/" {
			dir = filepath.Join(outputDir, filepath.FromSlash(strings.TrimPrefix(entry.Dir, "/")))
			if err := os.MkdirAll(dir, 0700); err != nil {
				return "", "", "", fmt.Errorf("creating %s: %w", dir, err)
			}
		}
	}
	return password, dir, vaultPath, nil
}

// entryForParts finds the manifest entry the given part files belong to, by
// the archive ID every part carries in the clear.
func entryForParts(snapshot *vault.Snapshot, partPaths []string) *vault.Entry {
	for _, path := range partPaths {
		blob, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		id, err := archive.ArchiveIDOf(blob)
		if err != nil {
			continue
		}
		hexID := hex.EncodeToString(id[:])
		for _, e := range snapshot.Manifest.Entries {
			if strings.EqualFold(e.ArchiveID, hexID) {
				return e
			}
		}
	}
	return nil
}
