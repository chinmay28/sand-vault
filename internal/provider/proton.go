package provider

import (
	"os"
	"path/filepath"
	"runtime"
)

// Proton Drive has no public API. What it does have is a desktop client that
// keeps a folder on this machine in step with the account, and rclone's
// reverse-engineered backend. SAND takes the folder: parts written there are
// already encrypted before they are handed over, so the sync client uploading
// them is the same arrangement as any other account — a place that holds one
// fragment and cannot do anything with it.
//
// The alternative, for a machine with no Proton client, is `rclone serve
// webdav` in front of rclone's protondrive backend, connected through the
// WebDAV kind.
func init() {
	Register(Spec{
		Kind:  KindProton,
		Label: "Proton Drive",
		Description: "Your Proton Drive folder, kept in step by the Proton Drive desktop app. " +
			"Proton publishes no API, so SAND writes its parts into the synced folder and lets " +
			"Proton carry them up. On a headless box, use rclone's Proton Drive backend behind " +
			"`rclone serve webdav` and connect it as WebDAV instead.",
		DocsURL: "https://proton.me/support/drive-windows-app",
		Order:   30,
		Fields: []FieldSpec{
			{
				Key:         "path",
				Label:       "Proton Drive folder",
				Placeholder: protonDefaultPath(),
				Default:     protonDefaultPath(),
				Help: "The folder the Proton Drive app syncs. SAND creates a subfolder of its " +
					"own inside it and only ever writes encrypted parts.",
				Required: true,
			},
		},
	}, newLocalProvider)
}

// protonDefaultPath guesses where the Proton Drive client put its folder,
// preferring one that actually exists so the field arrives pre-filled and
// correct on the common setups.
func protonDefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	candidates := []string{
		filepath.Join(home, "Proton Drive"),
		filepath.Join(home, "ProtonDrive"),
	}
	switch runtime.GOOS {
	case "darwin":
		candidates = append([]string{
			filepath.Join(home, "Library", "CloudStorage", "ProtonDrive"),
		}, candidates...)
	case "windows":
		candidates = append(candidates, filepath.Join(home, "OneDrive", "Proton Drive"))
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Join(candidate, "sand")
		}
	}
	return filepath.Join(candidates[0], "sand")
}
