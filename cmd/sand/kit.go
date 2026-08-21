package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The recovery kit at the command line.
//
// `sand vault recover` puts the index back and leaves you to reconnect five
// clouds by hand. This puts the clouds back too — which is the afternoon that
// actually costs somebody, and the part manifest.sand cannot carry because a
// copy of it sits on every account.

func vaultKitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kit",
		Short: "One sealed file that brings the whole vault back, clouds included",
		Long: `A recovery kit is the manifest backup that never touches a cloud, and can
therefore carry the credentials.

Import one onto a fresh install and every cloud account reconnects on its own,
under the id it already had, with the file tree exactly as it was. It costs one
secret instead of a password plus five credential hunts.

Keep the kit and its recovery code apart. Together, in one place, they are the
vault.`,
	}
	cmd.AddCommand(kitExportCmd(), kitInspectCmd(), kitVerifyCmd(), kitImportCmd(), kitCodeCmd())
	return cmd
}

func kitExportCmd() *cobra.Command {
	var (
		out              string
		useVaultPassword bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write a recovery kit",
		Long: `Build a recovery kit and print the code that opens it.

The code is shown here and written nowhere else — not into the archive, not
into its filename, not into any log. It is kept in the vault itself, so
'sand vault kit code' can show it again for as long as this vault works.

With --use-vault-password the kit is sealed under your vault password instead
and no code is minted. That is a real answer for somebody whose password
manager is itself backed up, and one fewer secret to file — but the kit will
then open with the password you are using today, not with whatever you change
it to later.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			if out == "" {
				out = fmt.Sprintf("sand-recovery-kit-%s.zip", time.Now().Format("2006-01-02"))
			}
			if abs, err := filepath.Abs(out); err == nil {
				out = abs
			}

			opts := vault.KitExportOptions{UseVaultPassword: useVaultPassword}
			if useVaultPassword {
				// Asked for again rather than reused from the unlock: this
				// seals a file that leaves the machine, and typing it twice is
				// the cheapest possible confirmation of which password it is.
				password, err := readPasswordFrom("SAND_PASSWORD", "Vault password (to seal the kit): ")
				if err != nil {
					return err
				}
				opts.Password = password
			}

			fingerprint, err := v.WriteKitTo(out, opts)
			if err != nil {
				return err
			}

			fmt.Printf("Wrote %s\n", out)
			fmt.Printf("  %d account(s), %d file(s), %d sub vault(s)\n",
				fingerprint.Accounts, fingerprint.Files, fingerprint.SubVaults)
			fmt.Printf("  kit %s\n", fingerprint.KitID)

			if fingerprint.Code == "" {
				fmt.Println("\nSealed under your vault password. Nothing else opens it.")
				return nil
			}

			// On its own, with nothing around it, so that a script redirecting
			// the rest of this output still puts the one thing that must not be
			// in the file in front of a human.
			fmt.Println("\n  RECOVERY CODE")
			fmt.Printf("\n      %s\n\n", fingerprint.Code)
			fmt.Println("  Write this down. It is not inside the archive and cannot be recovered")
			fmt.Println("  from it. Keep it somewhere other than beside the kit — together, they")
			fmt.Println("  are the vault.")
			fmt.Printf("\n  You can see it again with 'sand vault kit code %s'.\n", fingerprint.KitID)
			return nil
		},
	}

	cmd.Flags().StringVar(&out, "out", "", "where to write the kit (default sand-recovery-kit-<date>.zip)")
	cmd.Flags().BoolVar(&useVaultPassword, "use-vault-password", false,
		"seal the kit under the vault password instead of a generated code")
	return cmd
}

func kitInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <kit.zip>",
		Short: "Say what a kit is, without opening it",
		Long: `Read a kit's header. No secret is needed and nothing is revealed: every field
printed here is already in the clear inside the archive.

This is what tells you which of three zips you are holding, and whether the one
in front of you wants a recovery code or a vault password.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sealed, err := readKitFile(args[0])
			if err != nil {
				return err
			}
			env, err := vault.InspectKit(sealed)
			if err != nil {
				return err
			}

			opens := "a 25-character recovery code"
			if env.Secret == vault.KitSecretPassword {
				opens = "the vault password in use when it was made"
			}
			fmt.Printf("SAND recovery kit\n")
			fmt.Printf("  Kit id     %s\n", env.KitID)
			fmt.Printf("  Created    %s\n", env.CreatedAt.Format(time.RFC3339))
			fmt.Printf("  Opened by  %s\n", opens)
			return nil
		},
	}
}

func kitVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <kit.zip>",
		Short: "Test a kit against your live accounts, changing nothing",
		Long: `An untested backup is a rumour.

This opens a kit, pings every credential inside it, checks its index against
what the accounts really hold, and says what you would actually get back if you
needed it today. Nothing is written anywhere.

It asks for the code on purpose. The failure nothing else catches is the slip
of paper that went missing in a house move — and the day this fails on that,
your vault is still alive and a fresh kit is ten seconds away.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sealed, err := readKitFile(args[0])
			if err != nil {
				return err
			}
			kit, err := openKitWithSecret(sealed)
			if err != nil {
				return err
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			report, err := v.VerifyKit(cmd.Context(), kit)
			if err != nil {
				return err
			}

			fmt.Printf("Kit %s, made %s (%d days ago)\n\n",
				report.KitID, report.KitCreated.Format("2 Jan 2006"), report.AgeDays)
			fmt.Printf("  %d of %d credentials still work\n", report.Working, len(report.Accounts))
			for _, a := range report.Accounts {
				if a.Status == vault.KitAccountConnected {
					continue
				}
				fmt.Printf("    %-28s %s — %s\n", a.Name, a.Status, a.Detail)
			}

			fmt.Printf("\n  %d of %d files in this kit could be rebuilt today\n",
				report.Recoverable, report.KitFiles)
			if report.AddedSince > 0 {
				fmt.Printf("  %d file(s) have been added since it was made\n", report.AddedSince)
			}
			for _, a := range report.AccountsAdded {
				fmt.Printf("    %-28s connected after this kit — it holds no credentials for it\n", a.Name)
			}
			printWarnings(report.Warnings)

			// Not on StaleIndex alone: a kit exported a moment ago is "stale"
			// the instant anything touches the index, and nagging about that
			// teaches people to ignore the line that matters.
			if report.AddedSince > 0 || len(report.AccountsAdded) > 0 || report.Unusable > 0 {
				fmt.Println("\nExport a fresh kit with 'sand vault kit export'.")
			}
			fmt.Println("\nIn a real recovery your clouds would almost certainly be reachable, and the")
			fmt.Println("newer index on them would close most of this gap. The figures above are the")
			fmt.Println("floor, not the forecast.")
			return nil
		},
	}
}

func kitImportCmd() *cobra.Command {
	var (
		replace        bool
		skipCloudIndex bool
	)

	cmd := &cobra.Command{
		Use:   "import <kit.zip>",
		Short: "Rebuild this machine's vault from a recovery kit",
		Long: `Create a vault from a kit: every cloud account reconnected under the id it
already had, the file tree back as it was, sub vaults present and still shut.

You choose the password the recovered vault will use from now on. It need not
be the one the lost vault used: the kit carries its own keys, and nothing in
the import is derived from what you type here.

The lost vault's own password is worth having anyway, in one case. The copies
of the index sitting on your accounts are newer than the kit — they were
rewritten every time the tree changed — and the kit carries the key that opens
them. A password change made after the kit was exported retires that key, and
then the old password is the only thing that opens them. Put it in
SAND_OLD_PASSWORD and it is tried when the kit's key is refused. Leaving it out
is a real option: it costs only the files added between the export and the
password change.

An account that will not connect does not stop this. It is recorded, the rest
carries on, and the report says which one to fix and how.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sealed, err := readKitFile(args[0])
			if err != nil {
				return err
			}
			kit, err := openKitWithSecret(sealed)
			if err != nil {
				return err
			}

			password, err := readPasswordFrom("SAND_PASSWORD",
				"A password for the recovered vault: ")
			if err != nil {
				return err
			}

			v, err := vault.Open(vaultPath(cmd))
			if err != nil {
				return err
			}
			defer closeVault(v)

			report, err := v.ImportKit(cmd.Context(), kit, vault.KitImportOptions{
				Password:       password,
				Replace:        replace,
				OldPassword:    os.Getenv("SAND_OLD_PASSWORD"),
				SkipCloudIndex: skipCloudIndex,
			})
			if err != nil {
				return err
			}
			printKitImportReport(report)
			return nil
		},
	}

	cmd.Flags().BoolVar(&replace, "replace", false,
		"import over a vault that already holds files, replacing them")
	cmd.Flags().BoolVar(&skipCloudIndex, "skip-cloud-index", false,
		"use the kit's own index rather than a newer copy from the accounts")
	return cmd
}

func kitCodeCmd() *cobra.Command {
	var forget bool

	cmd := &cobra.Command{
		Use:   "code [kit-id]",
		Short: "Show the recovery code for a kit this vault made",
		Long: `Read back the code a kit was sealed under.

This gives nothing away: it needs an unlocked vault, and anybody with an
unlocked vault can export a fresh kit with a fresh code in ten seconds. What it
is for is the ordinary case — a working machine, a mislaid slip of paper, and
otherwise a zip that nothing on earth opens.

With no kit id it answers for the last kit this vault exported.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}

			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			if forget {
				if err := v.ForgetKitCode(id); err != nil {
					return err
				}
				fmt.Println("Forgotten. Note that this does not revoke anything: somebody holding")
				fmt.Println("that kit and that code still has it. See 'sand vault password'.")
				return nil
			}

			code, err := v.KitCode(id)
			if err != nil {
				return err
			}
			if code == "" {
				status, err := v.KitStatus()
				if err != nil {
					return err
				}
				if !status.Exported {
					return fmt.Errorf("this vault has never exported a recovery kit")
				}
				return fmt.Errorf("this vault is not holding a code for that kit — it was sealed " +
					"under the vault password, or the code was forgotten")
			}
			fmt.Println(code)
			return nil
		},
	}

	cmd.Flags().BoolVar(&forget, "forget", false, "drop the code this vault is holding")
	return cmd
}

// readKitFile reads a kit archive off disk and pulls the sealed envelope out.
func readKitFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return vault.ReadKitZip(raw)
}

// openKitWithSecret prompts for whatever the envelope says it wants, and says
// so in the right word: a code and a password are different secrets and the
// person typing one of them is having a bad day.
func openKitWithSecret(sealed []byte) (*vault.Kit, error) {
	env, err := vault.InspectKit(sealed)
	if err != nil {
		return nil, err
	}

	prompt := "Recovery code: "
	if env.Secret == vault.KitSecretPassword {
		prompt = "Vault password for this kit: "
	}
	// Its own environment variable, because an import needs two secrets at
	// once: the one that opens the kit and the one the new vault will use.
	secret, err := readPasswordFrom("SAND_KIT_CODE", prompt)
	if err != nil {
		return nil, err
	}
	return vault.OpenKit(sealed, secret)
}

// printKitImportReport leads on the shortfall, the same way the recovery
// report does, because a list of what worked is not what the person reading it
// needs.
func printKitImportReport(r *vault.KitImportReport) {
	fmt.Printf("\nRecovered from kit %s, made %s.\n\n",
		r.KitID, r.KitCreated.Format("2 Jan 2006"))

	if r.IndexSource != "kit" {
		fmt.Printf("  The index came off %s, dated %s — newer than the kit.\n",
			r.IndexName, r.IndexAt.Format("2 Jan 2006"))
	} else if r.PasswordChanged {
		fmt.Printf("  The copies of the index on your accounts do not open under this kit's\n")
		fmt.Printf("  key, so the kit's own index (%s) was used. Usually that means\n",
			r.IndexAt.Format("2 Jan 2006"))
		fmt.Printf("  the vault password changed after the kit was made.\n")
		if os.Getenv("SAND_OLD_PASSWORD") == "" {
			fmt.Printf("  Importing again into a fresh vault with SAND_OLD_PASSWORD set to the\n")
			fmt.Printf("  password the machine was using when it died would close that gap.\n")
		}
	}

	fmt.Printf("  %d of %d file(s) can be opened", r.Recoverable, r.Files)
	if r.Bytes > 0 {
		fmt.Printf(" (%s of %s)", humanBytes(r.RecoverableBytes), humanBytes(r.Bytes))
	}
	fmt.Println(".")
	if r.SubVaults > 0 {
		fmt.Printf("  %d sub vault(s) came back, still shut.\n", r.SubVaults)
	}

	fmt.Printf("\n  Accounts:\n")
	for _, a := range r.Accounts {
		switch a.Status {
		case vault.KitAccountConnected:
			fmt.Printf("    %-28s connected\n", a.Name)
		default:
			fmt.Printf("    %-28s %s — %s\n", a.Name, a.Status, a.Detail)
		}
	}

	// Gated on what it is about to say rather than on the list it reads from:
	// a clean import can still carry a stale shard record naming an account
	// nothing was lost to, and "0 file(s) cannot be opened yet" followed by an
	// empty list is a sentence that alarms and then explains nothing.
	if r.Lost > 0 {
		fmt.Printf("\n  %d file(s) cannot be opened yet. Connect these and run\n", r.Lost)
		fmt.Printf("  'sand vault recover --resume':\n")
		for _, a := range r.Blocking {
			if !a.Blocking {
				continue
			}
			fmt.Printf("    %-28s %s — holds parts of %d file(s)\n", a.Name, a.Kind, a.Files)
		}
	}
	if r.Orphans > 0 {
		fmt.Printf("\n  %d object(s) on your accounts are not named by this index. Nothing was\n", r.Orphans)
		fmt.Printf("  deleted — the index is simply older than the storage.\n")
	}
	if r.Repointed > 0 {
		fmt.Printf("\n  %d part(s) had moved and were re-pointed.\n", r.Repointed)
	}
	printWarnings(r.Warnings)

	for _, a := range r.Accounts {
		if a.Status == vault.KitAccountNeedsPath {
			fmt.Printf("\n  %s wants a folder that is not on this machine. Point it at the\n", a.Name)
			fmt.Printf("  right one and its parts come back — its id is unchanged, so the index\n")
			fmt.Printf("  is still correct.\n")
			break
		}
	}
}

// humanBytes prints a size the way the rest of SAND does.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTP"[exp])
}
