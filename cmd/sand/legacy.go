package main

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/archive"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// The standalone commands below predate the cloud-connected vault and are kept
// because they need nothing but a password: archive some files, get three zips,
// put them wherever you like by hand. Restoring needs any two of the three.

func archiveCmd() *cobra.Command {
	var (
		password  string
		outputDir string
	)

	cmd := &cobra.Command{
		Use:   "archive <file> [file...]",
		Short: "Split and encrypt files into three zip archives (no accounts needed)",
		Long: `Archive one or more files into sand-p1.zip, sand-p2.zip and sand-p3.zip.

Each input file is compressed, split in two, given a third XOR redundancy part,
and encrypted. Every zip holds one part per input file, so distributing the
three zips to three different places gives you the same any-two-of-three
recovery the connected-cloud mode automates.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" {
				var err error
				password, err = readNewPassword("Encryption password: ")
				if err != nil {
					return err
				}
			}
			if err := os.MkdirAll(outputDir, 0700); err != nil {
				return fmt.Errorf("creating output directory: %w", err)
			}

			partPaths, err := archive.ArchiveMultiple(args, password, outputDir)
			if err != nil {
				return err
			}

			for i := 0; i < archive.PartCount; i++ {
				zipPath := filepath.Join(outputDir, fmt.Sprintf("sand-p%d.zip", i+1))
				if err := writeZip(zipPath, partPaths[i]); err != nil {
					return err
				}
				// The loose .sand parts only existed to build the zips.
				for _, p := range partPaths[i] {
					os.Remove(p)
				}
				fmt.Printf("  %s (%d file(s))\n", zipPath, len(partPaths[i]))
			}

			noun := "files"
			if len(args) == 1 {
				noun = "file"
			}
			fmt.Printf("\nArchive complete — %d %s split across 3 zips.\n", len(args), noun)
			fmt.Println("Store each zip somewhere separate. Any 2 zips of the 3 restore everything.")
			return nil
		},
	}

	cmd.Flags().StringVar(&password, "password", "", "encryption password (prompted if omitted)")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "where to write the zip archives")
	return cmd
}

// writeZip bundles files into a zip archive.
func writeZip(zipPath string, filePaths []string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", zipPath, err)
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	for _, fp := range filePaths {
		data, err := os.ReadFile(fp)
		if err != nil {
			zw.Close()
			return fmt.Errorf("reading %s: %w", fp, err)
		}
		entry, err := zw.Create(filepath.Base(fp))
		if err != nil {
			zw.Close()
			return err
		}
		if _, err := entry.Write(data); err != nil {
			zw.Close()
			return err
		}
	}
	return zw.Close()
}

func restoreCmd() *cobra.Command {
	var (
		parts        string
		password     string
		outputDir    string
		manifestPath string
		preserveTree bool
	)

	cmd := &cobra.Command{
		Use:   "restore --parts <file1>,<file2>[,file3]",
		Short: "Rebuild an original file from any two of its parts",
		Long: `Rebuild a file from any two of its three parts.

Parts written by 'sand archive' are encrypted under the password you typed, so
those need nothing else. Parts pulled out of your cloud accounts are encrypted
under the vault's own random key, so for those, pass --manifest pointing at the
` + vault.BackupKey + ` file from any of your accounts: it carries that key, and
your vault password opens it. That combination — enough parts, the manifest,
and the password — rebuilds a file with no vault and no network.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var partPaths []string
			for _, p := range strings.Split(parts, ",") {
				if trimmed := strings.TrimSpace(p); trimmed != "" {
					partPaths = append(partPaths, trimmed)
				}
			}
			if len(partPaths) < archive.MinPartsToRestore {
				return fmt.Errorf("need at least %d parts, got %d", archive.MinPartsToRestore, len(partPaths))
			}
			if err := os.MkdirAll(outputDir, 0700); err != nil {
				return fmt.Errorf("creating output directory: %w", err)
			}

			// Which restore this is depends on how the parts were written, not
			// on what was asked for: a vault writes chunks, standalone mode
			// writes whole files, and both land in .sand files that look alike.
			chunked, err := archive.PartsAreChunked(partPaths[0])
			if err != nil {
				return err
			}

			dir := outputDir
			vaultPath := ""
			var dataKey []byte
			if manifestPath != "" {
				if password != "" {
					return fmt.Errorf("--password is the vault password when --manifest is used; it is prompted for")
				}
				resolved, err := restoreWithManifest(manifestPath, partPaths, preserveTree, outputDir)
				if err != nil {
					return err
				}
				password, dataKey, dir, vaultPath = resolved.password, resolved.dataKey, resolved.dir, resolved.vaultPath
			} else if chunked {
				// A chunk's key is derived from the vault's random data key, so
				// no password on its own opens one. The manifest backup is the
				// only thing outside the vault that carries that key.
				return fmt.Errorf(
					"these parts were written by a vault and cannot be opened by a password alone — " +
						"pass the manifest.sand from one of the accounts with --manifest")
			} else if password == "" {
				var err error
				password, err = readPassword("Decryption password: ")
				if err != nil {
					return err
				}
			}

			var outputPath string
			if chunked {
				outputPath, err = archive.RestoreChunked(partPaths, dataKey, dir)
			} else {
				outputPath, err = archive.Restore(partPaths, password, dir)
			}
			if err != nil {
				return err
			}

			fmt.Printf("Restored %s\n", outputPath)
			if vaultPath != "" {
				fmt.Printf("  it was stored in the vault at %s\n", vaultPath)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&parts, "parts", "",
		"comma-separated .sand part files — two of a whole file's three, or two of every chunk's")
	cmd.Flags().StringVar(&password, "password", "", "decryption password (prompted if omitted)")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "where to write the restored file")
	cmd.Flags().StringVar(&manifestPath, "manifest", "",
		"a "+vault.BackupKey+" file from one of your accounts, holding the key these parts were stored under")
	cmd.Flags().BoolVar(&preserveTree, "preserve-tree", false,
		"with --manifest, recreate the folders the file sat in inside the output directory")
	cmd.MarkFlagRequired("parts")
	return cmd
}
