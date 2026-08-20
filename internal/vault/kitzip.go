package vault

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// The zip a recovery kit arrives in, and the two plain files beside the sealed
// one that make it legible to somebody who has lost everything including the
// app.

const (
	// KitFile is the sealed envelope: the kit itself.
	KitFile = "kit.sand"

	// KitManifestFile is a straight copy of what SyncManifestBackup writes to
	// every account. It costs nothing — the vault seals it anyway — and it
	// keeps the two commands that already work with no vault, no network and
	// no accounts working from the kit alone.
	KitManifestFile = "manifest.sand"

	// KitFingerprintFile says what this kit is without opening it.
	KitFingerprintFile = "fingerprint.txt"

	// KitReadmeFile is written for a person who found a zip on a drive in a
	// drawer years from now and has no idea what SAND is.
	KitReadmeFile = "README.txt"
)

// ErrKitInSyncedFolder is returned when a kit would be written into a folder
// one of this vault's own accounts is syncing.
var ErrKitInSyncedFolder = errors.New("that folder is one this vault scatters parts across")

// ExportKit builds a recovery kit and streams it to w as a zip.
//
// Nothing is written to a temporary file at any point: the archive is assembled
// straight into the writer, so a kit never exists on disk anywhere but where
// the user put it. The recovery code comes back in the fingerprint and goes
// nowhere else — see KitFingerprint.Code.
func (v *Vault) ExportKit(opts KitExportOptions, w io.Writer) (*KitFingerprint, error) {
	secretKind := KitSecretCode
	secret := ""
	code := ""

	if opts.UseVaultPassword {
		// Checked before anything is built, so a kit is never sealed under a
		// password the user has mistyped and cannot open.
		if err := v.VerifyPassword(opts.Password); err != nil {
			return nil, err
		}
		secretKind = KitSecretPassword
		secret = opts.Password
	} else {
		generated, err := NewKitCode()
		if err != nil {
			return nil, err
		}
		code = generated
		// The KDF is fed the bare symbols, not the hyphenated form somebody
		// writes down, so how the code was grouped on the paper never matters.
		bare, err := NormalizeKitCode(generated)
		if err != nil {
			return nil, err
		}
		secret = bare
	}

	kitID := newKitID()

	// The sidecar is written on a debounce, so whatever was counted in the last
	// few seconds is still only in memory. A kit that carried the file as it
	// happens to sit on disk would quietly lose those reads, which is a small
	// thing to get wrong in the one artefact whose whole job is losing nothing.
	v.flushReadHistory()
	v.AwaitReadHistory()

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	kit := v.buildKitLocked(kitID)
	vaultKey := append([]byte(nil), v.vaultKey...)
	kdf := v.store.KDF
	v.mu.RUnlock()
	defer crypto.ZeroBytes(vaultKey)

	sealedKit, err := SealKit(kit, secretKind, secret)
	if err != nil {
		return nil, fmt.Errorf("sealing the recovery kit: %w", err)
	}

	// The same bytes SyncManifestBackup would put on every account this second.
	manifestBlob, err := sealBackup(&kit.Snapshot, kdf, vaultKey)
	if err != nil {
		return nil, fmt.Errorf("sealing the manifest copy: %w", err)
	}

	sum := sha256.Sum256(sealedKit)
	fingerprint := &KitFingerprint{
		KitID:      kit.KitID,
		CreatedAt:  kit.CreatedAt,
		AppVersion: kit.AppVersion,
		Secret:     secretKind,
		Accounts:   len(kit.Accounts),
		Files:      len(kit.Snapshot.Manifest.Entries),
		SubVaults:  len(kit.Snapshot.SubVaults),
		Size:       int64(len(sealedKit)),
		SHA256:     hex.EncodeToString(sum[:]),
		Code:       code,
	}
	for _, e := range kit.Snapshot.Manifest.Entries {
		fingerprint.Bytes += e.Size
	}

	zw := zip.NewWriter(w)
	files := []struct {
		name string
		data []byte
	}{
		{KitReadmeFile, []byte(kitReadme(fingerprint))},
		{KitFile, sealedKit},
		{KitManifestFile, manifestBlob},
		{KitFingerprintFile, []byte(kitFingerprintText(fingerprint))},
	}
	for _, f := range files {
		// Deflate throughout: the manifest is JSON and compresses several to
		// one, which is the difference between a kit somebody mails themselves
		// and one they do not, on a vault with a hundred thousand entries.
		hdr := &zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: kit.CreatedAt}
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		if _, err := entry.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	if err := v.recordKitExport(fingerprint); err != nil {
		return nil, err
	}
	return fingerprint, nil
}

// recordKitExport remembers what was just made: the staleness the settings
// panel reads, and the code it can show again.
func (v *Vault) recordKitExport(f *KitFingerprint) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	v.store.LastKitExportAt = f.CreatedAt
	v.store.LastKitID = f.KitID
	v.store.LastKitSecret = f.Secret
	v.store.LastKitFileCount = len(v.manifest.Entries)
	v.store.LastKitAccounts = make([]string, 0, len(v.providers))
	for _, cfg := range v.providers {
		v.store.LastKitAccounts = append(v.store.LastKitAccounts, cfg.ID)
	}

	if f.Code != "" {
		if v.settings == nil {
			v.settings = &vaultSettings{}
		}
		if v.settings.KitCodes == nil {
			v.settings.KitCodes = map[string]string{}
		}
		v.settings.KitCodes[f.KitID] = f.Code
	}
	return v.persistLocked()
}

// CheckKitDestination refuses a path inside a folder this vault's own accounts
// are syncing.
//
// It is the exact mistake the credential separation exists to prevent: the kit
// would be uploaded to the very cloud whose credentials it contains, by that
// cloud's own sync client, in the background, silently. The browser cannot see
// where a download lands and says it in a line instead; the CLI can, and does
// this.
func (v *Vault) CheckKitDestination(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil // Not a judgement this can make; let the write speak.
	}

	v.mu.RLock()
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	for _, cfg := range configs {
		// Only a folder the backend says is on *this* machine. Box and OneDrive
		// both call theirs "folder" and both mean a folder inside somebody
		// else's service, so matching on the name would resolve "sand" against
		// the working directory and refuse a perfectly safe destination.
		key, root := configuredPath(cfg)
		if key == "" {
			continue
		}
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootAbs, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return fmt.Errorf("%w: %s is inside %s, which %s is connected to — the kit would be "+
			"uploaded to the very account whose credentials it carries",
			ErrKitInSyncedFolder, abs, rootAbs, cfg.Name)
	}
	return nil
}

// WriteKitTo builds a kit straight into a file, refusing a destination that
// would sync it into one of this vault's own accounts.
func (v *Vault) WriteKitTo(path string, opts KitExportOptions) (*KitFingerprint, error) {
	if err := v.CheckKitDestination(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	fingerprint, err := v.ExportKit(opts, f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return fingerprint, nil
}

// kitFingerprintText is the plain-text summary in the zip. Nothing in it is a
// secret: account *names* are a map of somebody's life and stay inside the
// envelope with everything else, while "4 accounts" is what a person needs to
// know whether they are holding the right kit.
func kitFingerprintText(f *KitFingerprint) string {
	opened := "a 25-character recovery code (not in this file)"
	if f.Secret == KitSecretPassword {
		opened = "the vault password in use when it was made (not in this file)"
	}
	return strings.Join([]string{
		"SAND recovery kit",
		"",
		fmt.Sprintf("Kit id      %s", f.KitID),
		fmt.Sprintf("Created     %s", f.CreatedAt.Format(time.RFC3339)),
		fmt.Sprintf("Built by    sand %s", f.AppVersion),
		fmt.Sprintf("Opened by   %s", opened),
		fmt.Sprintf("Accounts    %d", f.Accounts),
		fmt.Sprintf("Files       %d (%s)", f.Files, formatBytes(f.Bytes)),
		fmt.Sprintf("Sub vaults  %d", f.SubVaults),
		fmt.Sprintf("%-11s sha256:%s  (%d bytes)", KitFile, f.SHA256, f.Size),
		"",
	}, "\n")
}

// formatBytes writes a size the way the rest of SAND does, so a figure read
// off fingerprint.txt matches what the browser showed.
func formatBytes(b int64) string {
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

// kitReadme is the one file here written for a person rather than a program.
func kitReadme(f *KitFingerprint) string {
	secret := "a 25-character recovery code that was shown to you when this kit was made"
	if f.Secret == KitSecretPassword {
		secret = "the SAND vault password that was in use when this kit was made"
	}

	return fmt.Sprintf(`SAND RECOVERY KIT
=================

This archive is a complete backup of the *index* of a SAND vault: the list of
files it held, the map of which cloud account holds which encrypted part of
each one, the keys that open them, and the credentials for every account.

It does not contain the files themselves. Those are still out on the cloud
accounts, cut into encrypted parts. This is what finds them again.

WHAT OPENS IT
-------------

%s.

The secret is deliberately NOT in this archive. Without it, nothing in here can
be opened by anyone — including you. If you have lost it and the machine that
made this kit still works, SAND can show it to you again under
Settings -> Recovery kit -> Show code.

HOW TO USE IT
-------------

  1. Install SAND (https://github.com/chinmay28/sand-vault).
  2. On the "Create your vault" screen, choose "I have a recovery kit".
  3. Give it this zip and the secret above.

Every cloud account reconnects on its own, and the file tree comes back as it
was. Any account whose sign-in has expired in the meantime is offered as a
single "sign in again" button rather than stopping the recovery.

At the command line, the same thing is:

    sand vault kit import <this file>

IF SAND WILL NOT RUN AT ALL
---------------------------

%s in this archive is the same file SAND keeps on each of your cloud accounts.
Any build of SAND can read it with nothing else installed and no network:

    sand manifest ls %s            # the file tree, from your vault password
    sand restore --manifest %s \
        --parts <two part files>       # one whole file, offline

Those need the *vault password*, not the code above.

WHAT THIS FILE IS WORTH
-----------------------

Everything. Somebody holding this archive AND its secret can read every file in
the vault and sign in to every cloud account it names. Somebody holding only
this archive has 120 bits of random secret in their way and cannot do anything
at all.

Keep the two apart. Together, in one place, they are the vault.

Kit id %s, created %s.
`,
		secret,
		KitManifestFile,
		KitManifestFile,
		KitManifestFile,
		f.KitID,
		f.CreatedAt.Format("2 January 2006"))
}
