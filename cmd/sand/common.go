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
	first, err := readPassword(prompt)
	if err != nil {
		return "", err
	}
	if os.Getenv("SAND_PASSWORD") != "" || !term.IsTerminal(int(os.Stdin.Fd())) {
		return first, nil
	}

	second, err := readPassword("Confirm password: ")
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
func resolveEntry(v *vault.Vault, ref string) (*vault.Entry, error) {
	if entry, err := v.Entry(ref); err == nil {
		return entry, nil
	}

	dir, name := splitPath(ref)
	if name == "" {
		return nil, fmt.Errorf("%s is a folder, not a file", ref)
	}
	listing, err := v.List(dir)
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
