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
			defer closeVault(v)

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
		resume         bool
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
parts.

Connect those accounts later and run 'sand vault recover --resume', which asks
the accounts the same question again and re-points whatever is now within
reach. It needs no password: the key was adopted by the recovery that ran
first, and what was missing is a reachable copy of the parts.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			accounts, err := v.Providers()
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				return fmt.Errorf("connect the accounts holding your parts first, with 'sand remote add'")
			}

			if resume {
				report, err := v.Reconcile(cmd.Context(), dryRun)
				if err != nil {
					return err
				}
				printWarnings(report.Warnings)
				printRecoveryReport(report, dryRun)
				if dryRun {
					fmt.Println("\nNothing was changed. Run again without --dry-run to apply.")
				}
				return nil
			}

			// A vault that already holds files cannot adopt a second snapshot,
			// but if those files are the half-reachable remains of an earlier
			// recovery there is still something to be done about them.
			if stats, err := v.Stats(); err == nil && stats.Unresolved > 0 {
				return fmt.Errorf(
					"this vault already holds %d file(s), %d of them stranded on accounts that "+
						"are not connected — connect those accounts and run "+
						"'sand vault recover --resume' to finish the recovery that brought them here",
					stats.Files, stats.Stranded)
			}

			source := accounts[0]
			if from != "" {
				if source, err = findProvider(v, from); err != nil {
					return err
				}
			} else if scan, err := v.ScanForRecovery(cmd.Context()); err == nil {
				// Every account carries the same copy, so any one of them will
				// do — but not one holding *this* vault's backup, which the old
				// password cannot open. Prefer an account still carrying the
				// index of the vault that is gone.
				for _, candidate := range scan.Sources {
					if candidate.Backup && candidate.Foreign {
						if cfg, err := findProvider(v, candidate.ProviderID); err == nil {
							source = cfg
						}
						break
					}
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

			printRecoveryReport(report, dryRun)
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
	cmd.Flags().BoolVar(&resume, "resume", false,
		"finish an earlier recovery by re-pointing the index at accounts that are now connected")
	return cmd
}

// printRecoveryReport says what came back and — the part worth the ink — what
// did not, and which account would have to be reconnected to change that.
func printRecoveryReport(report *vault.RecoveryReport, dryRun bool) {
	verb := "Recovered"
	if dryRun {
		verb = "Would recover"
	}
	fmt.Printf("%s %d of %d file(s) in %d folder(s) — %s of %s.\n",
		verb, report.Recoverable, report.Files, report.Folders,
		formatBytes(report.RecoverableBytes), formatBytes(report.Bytes))

	if report.Relocated > 0 {
		fmt.Printf("  %d part(s) re-pointed at the accounts now holding them.\n", report.Relocated)
	}
	if report.Degraded > 0 {
		fmt.Printf("  %d file(s) came back with no spare part left.\n", report.Degraded)
	}
	if report.Unreachable > 0 {
		fmt.Printf("  %d part(s) are on accounts that are not connected.\n", report.Unreachable)
	}

	if report.Complete() {
		return
	}

	fmt.Printf("\nNot recovered: %d of %d file(s), %s of %s.\n",
		report.Lost, report.Files, formatBytes(report.LostBytes), formatBytes(report.Bytes))

	if len(report.MissingAccounts) > 0 {
		fmt.Println("\nConnect these accounts, then 'sand vault recover --resume':")
		for _, account := range report.MissingAccounts {
			note := "spare parts only"
			if account.Blocking {
				note = fmt.Sprintf("%d file(s) cannot be opened without it", account.Files)
			}
			fmt.Printf("  %-24s %-10s %d part(s) — %s\n",
				account.Name, account.Kind, account.Parts, note)
		}
	}

	if len(report.Missing) > 0 {
		fmt.Println("\nFiles still missing:")
		for _, file := range report.Missing {
			fmt.Printf("  %-40s %10s  %d of %d part(s) found\n",
				file.Path, formatBytes(file.Size), file.PartsFound, file.PartsNeeded)
		}
		if report.MissingTruncated > 0 {
			fmt.Printf("  … and %d more\n", report.MissingTruncated)
		}
	}
}

func vaultReclaimCmd() *cobra.Command {
	var accounts []string

	cmd := &cobra.Command{
		Use:   "reclaim",
		Short: "Re-encrypt recovered files under this vault's own key",
		Long: `Take recovered files off the key of the vault they came from.

Recovery adopts the lost vault's data key, because that key is the only thing
that opens the parts already sitting on your accounts. That gets the files
back, and it leaves something behind: those parts are still encrypted under the
old key, which the old password still derives — through any copy of the old
` + vault.BackupKey + `, including ones this vault never got to overwrite.

This ends that. A fresh data key is sealed under your current password, every
file is rebuilt onto it, and the parts under the old key are erased. Your
password does not change.

Since every file is gathered and scattered anyway, --account is the moment to
say where they should live: name the clouds you actually mean to keep using,
rather than the ones the vault that died happened to choose. Left out, each
file goes back to the accounts it is already on.

It costs a download and an upload of the whole vault. Files stay readable
throughout, and stopping it is safe — whatever moved stays moved, and
'sand vault migrate' finishes the rest.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			chosen, err := resolveAccountNames(v, accounts)
			if err != nil {
				return err
			}

			stats, err := v.Stats()
			if err != nil {
				return err
			}
			if stats.Files == 0 {
				fmt.Println("This vault holds no files, so there is nothing to re-encrypt.")
				return nil
			}
			fmt.Printf("Re-encrypting %d file(s) (%s) under a key of this vault's own",
				stats.Files, formatBytes(stats.Bytes))
			if len(chosen) > 0 {
				fmt.Printf(", onto %d chosen cloud(s)", len(chosen))
			}
			fmt.Println("…")

			report, err := v.Reclaim(cmd.Context(), chosen, func(path string, done, total int) {
				fmt.Printf("  [%d/%d] %s\n", done, total, path)
			})
			if report != nil {
				printWarnings(report.Warnings)
				fmt.Printf("Re-encrypted %d of %d file(s) (%s).\n",
					report.Migrated, report.Pending, formatBytes(report.Bytes))
				if report.Remaining > 0 {
					fmt.Printf("%d file(s) are still on the old key and still readable — "+
						"fix what is reported above and run 'sand vault migrate' to finish.\n",
						report.Remaining)
				} else {
					fmt.Println("The old key opens nothing that is still stored.")
				}
			}
			return err
		},
	}

	cmd.Flags().StringSliceVar(&accounts, "account", nil,
		"cloud account to store the re-encrypted files on, by name or id (repeatable)")
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
// recovered file. It can carry more than one: a backup written while a password
// change was still re-encrypting describes files on either side of the change,
// so the key is chosen per file wherever the parts can be traced to one.
// manifestRestore is what a manifest backup yields for a restore: where to put
// the file, what it was called in the vault, and the secret that opens its
// parts — spelled as a password for a file stored whole, and as raw key
// material for one stored in chunks, which derives a key per chunk from it.
type manifestRestore struct {
	password  string
	dataKey   []byte
	dir       string
	vaultPath string
}

func restoreWithManifest(manifestPath string, partPaths []string, preserveTree bool, outputDir string) (*manifestRestore, error) {
	snapshot, err := openManifestFile(manifestPath)
	if err != nil {
		return nil, err
	}

	out := &manifestRestore{dir: outputDir}
	keyID := ""
	if entry := entryForParts(snapshot, partPaths); entry != nil {
		out.vaultPath = entry.Path()
		keyID = entry.KeyID
		if preserveTree && entry.Dir != "/" {
			out.dir = filepath.Join(outputDir, filepath.FromSlash(strings.TrimPrefix(entry.Dir, "/")))
			if err := os.MkdirAll(out.dir, 0700); err != nil {
				return nil, fmt.Errorf("creating %s: %w", out.dir, err)
			}
		}
	} else {
		keyID = snapshot.KeyID
	}

	// Both forms of the same key, because which one is needed depends on the
	// format of the parts themselves rather than on anything the manifest says.
	if out.password, err = snapshot.ShardPasswordFor(keyID); err != nil {
		return nil, err
	}
	if out.dataKey, err = snapshot.DataKeyFor(keyID); err != nil {
		return nil, err
	}
	return out, nil
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
