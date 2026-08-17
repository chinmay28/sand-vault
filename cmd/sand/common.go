package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/chinmay28/sand-vault/internal/server"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// vaultPath resolves the vault location from the --vault flag, the SAND_VAULT
// environment variable, or the default.
func vaultPath(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("vault"); p != "" {
		return p
	}
	return server.DefaultVaultPath()
}

// openVault opens and unlocks the vault, prompting for the password unless
// SAND_PASSWORD is set.
func openVault(cmd *cobra.Command) (*vault.Vault, error) {
	v, err := vault.Open(vaultPath(cmd))
	if err != nil {
		return nil, err
	}
	if !v.Initialized() {
		return nil, fmt.Errorf("no vault at %s — run 'sand vault init' first", v.Path())
	}

	password, err := readPassword("Vault password: ")
	if err != nil {
		return nil, err
	}
	if err := v.Unlock(password); err != nil {
		return nil, err
	}
	return v, nil
}

// closeVault locks the vault at the end of a command, after letting the
// manifest backup catch up.
//
// The wait is the point. Changing the index schedules a push of the encrypted
// copy every account carries, and it runs on its own goroutine so that an
// upload never waits on three network round-trips it does not need. A process
// that exits the moment the upload is done therefore leaves the copies
// describing a vault one file behind — and locking the vault on the way out
// aborts the push outright, since it needs the keys that just went away. The
// file most likely to be missing from a recovery is then the one that was
// stored last, which is the worst possible one to lose.
//
// Every command that opens the vault ends here, whether or not it writes: a
// push left over from an earlier command that exited too early is finished by
// the next one, and one with nothing to do returns immediately.
func closeVault(v *vault.Vault) {
	v.AwaitBackupSync()
	v.Lock()
}

// openVaultIn opens and unlocks the vault, and if --in named a sub vault,
// opens that too and returns its scope.
//
// Two passwords, asked for one at a time, because they are two different
// secrets and the second one is the point: a script driving this needs
// SAND_PASSWORD for the vault and SAND_SUB_PASSWORD for what is inside it.
func openVaultIn(cmd *cobra.Command) (*vault.Vault, vault.Scope, error) {
	v, err := openVault(cmd)
	if err != nil {
		return nil, vault.MainScope, err
	}

	name, _ := cmd.Flags().GetString("in")
	if strings.TrimSpace(name) == "" {
		return v, vault.MainScope, nil
	}

	scope, err := unlockSubVault(v, name)
	if err != nil {
		return nil, vault.MainScope, err
	}
	return v, scope, nil
}

// unlockSubVault resolves a sub vault by name or ID and opens it.
func unlockSubVault(v *vault.Vault, name string) (vault.Scope, error) {
	subs, err := v.SubVaults()
	if err != nil {
		return vault.MainScope, err
	}

	id := ""
	for _, s := range subs {
		if s.ID == name || strings.EqualFold(s.Label, name) {
			id = s.ID
			break
		}
	}
	if id == "" {
		return vault.MainScope, fmt.Errorf("no sub vault called %q — 'sand vault sub ls' lists them", name)
	}

	password, err := readPasswordFrom("SAND_SUB_PASSWORD", fmt.Sprintf("Password for the %q sub vault: ", name))
	if err != nil {
		return vault.MainScope, err
	}
	if err := v.UnlockSubVault(id, password); err != nil {
		return vault.MainScope, err
	}
	return vault.Scope(id), nil
}

// readPassword reads a password from SAND_PASSWORD, or prompts without echo.
func readPassword(prompt string) (string, error) {
	return readPasswordFrom("SAND_PASSWORD", prompt)
}

// readPasswordFrom is readPassword against a named environment variable.
// Recovery needs two different passwords in one command — the vault being
// opened and the backup being read — so each needs its own way in for scripts.
func readPasswordFrom(env, prompt string) (string, error) {
	if pw := os.Getenv(env); pw != "" {
		return pw, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		// Non-interactive: read a single line from stdin so the CLI can be
		// driven from a script or a pipe.
		line, err := pipedInput().ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(raw), nil
}

// stdin is buffered once for the whole process. A fresh bufio.Reader per read
// would swallow whatever followed the first line into a buffer it then threw
// away, so a command asking for two passwords down one pipe — `passwd`, given
// the old one and the new one — would find the second line gone.
var stdin *bufio.Reader

func pipedInput() *bufio.Reader {
	if stdin == nil {
		stdin = bufio.NewReader(os.Stdin)
	}
	return stdin
}

// readNewPassword prompts twice and checks the two entries match.
func readNewPassword(prompt string) (string, error) {
	return readNewPasswordFrom("SAND_PASSWORD", prompt)
}

// readNewPasswordFrom is readNewPassword against a named environment variable.
// A sub vault command asks for two or three different passwords in one run —
// the vault's, the sub vault's, and sometimes a new one — so each needs its own
// way in for a script.
func readNewPasswordFrom(env, prompt string) (string, error) {
	first, err := readPasswordFrom(env, prompt)
	if err != nil {
		return "", err
	}
	if os.Getenv(env) != "" || !term.IsTerminal(int(os.Stdin.Fd())) {
		return first, nil
	}

	second, err := readPasswordFrom(env, "Confirm password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("passwords do not match")
	}
	return first, nil
}

// formatBytes renders a byte count in human units.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 4; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// splitPath separates a full browser path into its folder and leaf name.
func splitPath(full string) (dir, name string) {
	cleaned := vault.CleanDir(full)
	if cleaned == "/" {
		return "/", ""
	}
	idx := strings.LastIndex(cleaned, "/")
	if idx <= 0 {
		return "/", cleaned[1:]
	}
	return cleaned[:idx], cleaned[idx+1:]
}

// resolveEntry finds a file by browser path or by entry ID.
func resolveEntry(v *vault.Vault, scope vault.Scope, ref string) (*vault.Entry, error) {
	if entry, err := v.Entry(ref); err == nil {
		return entry, nil
	}

	dir, name := splitPath(ref)
	if name == "" {
		return nil, fmt.Errorf("%s is a folder, not a file", ref)
	}
	listing, err := v.List(scope, dir)
	if err != nil {
		return nil, err
	}
	for _, e := range listing.Files {
		if e.Name == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("no such file: %s", ref)
}
