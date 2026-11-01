package tuning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is what the config is called, beside the vault it serves.
//
// Beside the vault rather than inside it, for two reasons. It is needed at
// Open, before there is a password to decrypt anything with. And it belongs
// to the machine, not the vault: a vault restored from a recovery kit onto a
// different box should not bring a Pi's windows with it. Nothing in it is
// secret — it is a CPU count, a memory figure and a few sizes — so it is
// plain JSON, readable and editable by hand.
const FileName = "machine.json"

// PathFor is where the config for a vault at vaultPath lives.
func PathFor(vaultPath string) string {
	return filepath.Join(filepath.Dir(vaultPath), FileName)
}

// Load reads a Config, with a missing file reading as the zero Config — which
// resolves to auto with nothing overridden, the state a fresh install is in.
// A file that is present but unreadable is an error rather than a fallback:
// quietly running on defaults past a typo would be worse than refusing.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if c.Profile != "" && !c.Profile.Valid() {
		return Config{}, fmt.Errorf("reading %s: unknown profile %q", path, c.Profile)
	}
	return c, nil
}

// Save writes a Config atomically: to a temporary file beside the target,
// then renamed over it, so a crash mid-write leaves the old file rather than
// half of the new one.
func Save(path string, c Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".machine-*.json")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	cleanup := func() { os.Remove(tmp.Name()) }
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
