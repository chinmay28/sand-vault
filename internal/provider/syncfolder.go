package provider

// Some services have no API a third party can use, and a desktop client that
// keeps a folder on this machine in step with the account. From the vault's
// point of view those are the same thing: a part is encrypted long before it
// reaches the folder, so the client carrying it up is in exactly the position
// every other account is in — holding one fragment, able to do nothing with
// it. The backend is therefore the local folder one plus a form field and a
// guess at where the client put its folder.
//
// Everything here is that table. A service joins it in a dozen lines; the
// connect dialog, the CLI and the folder picker all build themselves from the
// spec it produces. The one thing to check before adding a service is whether
// its client evicts — replaces a synced file with a placeholder under a
// different name when disk runs short. A client that does needs its own
// backend, as iCloud Drive does in icloud.go; a client that fetches on read,
// including the on-demand virtual drives several of these mount, needs
// nothing.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// syncFolder describes one such service.
type syncFolder struct {
	kind        Kind
	label       string
	description string
	docsURL     string
	order       int

	// fieldLabel names the folder in the connect form, in the service's own
	// words — "MEGA folder" rather than "Directory".
	fieldLabel string
	fieldHelp  string

	// folders lists the places this machine's client is likely to have put its
	// folder, most likely first. The first one that exists pre-fills the form.
	folders func(home string) []string
}

// syncFolders is the register. Order runs from the services most people have
// to the ones most people have not.
var syncFolders = []syncFolder{
	{
		// Proton Drive has no public API. What it does have is a desktop
		// client that keeps a folder on this machine in step with the account,
		// and rclone's reverse-engineered backend. SAND takes the folder; the
		// alternative, for a machine with no Proton client, is `rclone serve
		// webdav` in front of rclone's protondrive backend, connected through
		// the WebDAV kind.
		kind:  KindProton,
		label: "Proton Drive",
		description: "Your Proton Drive folder, kept in step by the Proton Drive desktop app. " +
			"Proton publishes no API, so SAND writes its parts into the synced folder and lets " +
			"Proton carry them up. On a headless box, use rclone's Proton Drive backend behind " +
			"`rclone serve webdav` and connect it as WebDAV instead.",
		docsURL:    "https://proton.me/support/drive-windows-app",
		order:      31,
		fieldLabel: "Proton Drive folder",
		fieldHelp: "The folder the Proton Drive app syncs. SAND creates a subfolder of its " +
			"own inside it and only ever writes encrypted parts.",
		folders: func(home string) []string {
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
			return candidates
		},
	},
	{
		kind:  KindMega,
		label: "MEGA",
		description: "Your MEGA folder, kept in step by the MEGA desktop app. MEGA's own API is " +
			"a bespoke crypto stack rather than anything standard, so SAND writes its parts into " +
			"the synced folder instead. On a headless box, run rclone's MEGA backend behind " +
			"`rclone serve webdav` and connect that as WebDAV.",
		docsURL:    "https://help.mega.io/installs-apps/desktop",
		order:      32,
		fieldLabel: "MEGA folder",
		fieldHelp: "A folder inside the one MEGA syncs. SAND creates it if it does not exist " +
			"and only ever writes encrypted parts.",
		folders: func(home string) []string {
			return []string{
				filepath.Join(home, "MEGA"),
				filepath.Join(home, "MEGAsync"),
			}
		},
	},
	{
		kind:  KindJottacloud,
		label: "Jottacloud",
		description: "Your Jottacloud folder, kept in step by the Jottacloud desktop app — " +
			"Norwegian storage, on Norwegian law. On a headless box, rclone's Jottacloud backend " +
			"behind `rclone serve webdav` connects as WebDAV instead.",
		docsURL:    "https://docs.jottacloud.com/en/articles/1372069-jottacloud-for-desktop",
		order:      33,
		fieldLabel: "Jottacloud folder",
		fieldHelp: "A folder inside the one Jottacloud syncs. SAND creates it if it does not " +
			"exist and only ever writes encrypted parts.",
		folders: func(home string) []string {
			return []string{filepath.Join(home, "Jottacloud")}
		},
	},
	{
		kind:  KindSyncCom,
		label: "Sync.com",
		description: "Your Sync folder, kept in step by the Sync.com desktop app. Sync.com " +
			"publishes no API for third parties, so the folder is the way in: SAND writes its " +
			"parts there and the app carries them up.",
		docsURL:    "https://www.sync.com/help/how-do-i-install-sync-on-my-computer/",
		order:      34,
		fieldLabel: "Sync folder",
		fieldHelp: "A folder inside the one Sync.com syncs. Keep it stored on this machine " +
			"rather than online-only, so a part can be read back without the app fetching it.",
		folders: func(home string) []string {
			return []string{filepath.Join(home, "Sync")}
		},
	},
	{
		kind:  KindTresorit,
		label: "Tresorit",
		description: "A folder inside a tresor the Tresorit client keeps in step. Tresorit " +
			"publishes no API for third parties; the parts are encrypted before they reach the " +
			"folder either way, so this is the same arrangement as any other account.",
		docsURL:    "https://support.tresorit.com/hc/en-us/articles/216114397",
		order:      35,
		fieldLabel: "Tresorit folder",
		fieldHelp: "A folder inside a synced tresor — any folder you have made one. Tresorit " +
			"Drive works too, since it fetches a file when something reads it.",
		folders: func(home string) []string {
			return []string{
				filepath.Join(home, "Tresorit"),
				filepath.Join(home, "Tresors"),
			}
		},
	},
	{
		kind:  KindIcedrive,
		label: "Icedrive",
		description: "Your Icedrive folder — either one the app syncs or its mounted drive, " +
			"which fetches a file when something reads it. Icedrive's API is not open to third " +
			"parties, so the folder is how parts get there.",
		docsURL:    "https://icedrive.net/help",
		order:      36,
		fieldLabel: "Icedrive folder",
		fieldHelp: "A folder Icedrive syncs, or one on the drive it mounts. SAND creates it if " +
			"it does not exist and only ever writes encrypted parts.",
		folders: func(home string) []string {
			return []string{
				filepath.Join(home, "Icedrive"),
				filepath.Join(home, "IcedriveSync"),
			}
		},
	},
}

func init() {
	for _, service := range syncFolders {
		Register(service.spec(), service.factory())
	}
}

// spec is the backend's entry in the registry, and with it the connect form.
func (s syncFolder) spec() Spec {
	fallback := s.defaultPath()
	return Spec{
		Kind:        s.kind,
		Label:       s.label,
		Description: s.description,
		DocsURL:     s.docsURL,
		Order:       s.order,
		Fields: []FieldSpec{
			{
				Key:         "path",
				Label:       s.fieldLabel,
				Placeholder: fallback,
				Default:     fallback,
				Help:        s.fieldHelp,
				Required:    true,
				Directory:   true,
			},
		},
	}
}

// factory builds this service's constructor. The label rides along so a
// failure can name the client the account holder would have to go and fix.
func (s syncFolder) factory() func(Config) (Provider, error) {
	label := s.label
	return func(cfg Config) (Provider, error) {
		p, err := newLocalProvider(cfg)
		if err != nil {
			return nil, err
		}
		local, ok := p.(*localProvider)
		if !ok {
			return nil, fmt.Errorf("%s: unexpected folder backend %T", label, p)
		}
		return &syncFolderProvider{localProvider: local, client: label}, nil
	}
}

// defaultPath pre-fills the folder field, so the common setup is a button
// rather than a path typed from memory. It prefers a folder that is actually
// there; failing that it names the likeliest one, which at least says what
// kind of answer the field wants.
func (s syncFolder) defaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := s.folders(home)
	if len(candidates) == 0 {
		return ""
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Join(candidate, "sand")
		}
	}
	return filepath.Join(candidates[0], "sand")
}

// syncFolderProvider is the local folder backend with one extra question asked
// at connect time.
type syncFolderProvider struct {
	*localProvider
	client string
}

// Ping refuses a folder whose parent is not there. The plain local backend
// creates a whole path happily, which is right for a disk and wrong here: if
// the folder the client syncs does not exist, the client is not running on
// this machine, and creating `~/MEGA/sand` underneath it would produce an
// account that accepts every part and uploads none of them. Naming a new
// folder inside one that does exist stays fine — that is the normal way to
// connect one.
func (p *syncFolderProvider) Ping(ctx context.Context) error {
	parent := filepath.Dir(p.root)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return fmt.Errorf("%s does not exist, so nothing written to %s would ever leave this "+
			"machine — check that %s is installed and signed in here, then pick a folder inside "+
			"the one it keeps in step", parent, p.root, p.client)
	}
	return p.localProvider.Ping(ctx)
}
