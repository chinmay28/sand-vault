// Package davfs presents an unlocked vault as a WebDAV filesystem, so that a
// player or a file manager can mount it instead of driving the HTTP API.
//
// It is an adapter and nothing more. Every read goes through the vault's
// ChunkedReader, so seeking into a film fetches the chunk it lands in rather
// than the film; every write goes through UploadStream, so a large upload is
// bounded by the chunk window rather than by memory. Nothing here decides where
// a file is stored, what may hold it, or how it is encrypted — those answers
// stay in internal/vault, where they can be reasoned about once.
package davfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
	"golang.org/x/net/webdav"
)

// mainVault is the only scope this adapter ever addresses.
//
// A sub vault is deliberately unreachable over WebDAV — not merely hidden while
// it is locked, but absent even while it is open in the app. A mounted drive is
// the broadest possible exposure of a vault: it is a folder on the desktop that
// every process running as that user can read, that a backup agent will happily
// copy to somewhere else, and that stays mounted long after the person who
// mounted it has walked away. What goes in a sub vault is what should not be
// reachable that way.
//
// Expressing it as a constant rather than a flag is the point. There is no
// configuration that turns this on, and no request that can ask for a different
// scope, so the guarantee holds by construction rather than by every call site
// remembering it.
const mainVault = vault.MainScope

// FileSystem adapts a vault to webdav.FileSystem.
type FileSystem struct {
	Vault *vault.Vault
}

// New returns a WebDAV filesystem over a vault.
func New(v *vault.Vault) *FileSystem { return &FileSystem{Vault: v} }

// cleanName turns a WebDAV request path into a vault path: absolute, slash
// separated, no trailing slash except at the root.
//
// A path that climbs out of the root with .. is refused rather than clamped.
// Clamping would silently answer for a different file than the one asked for,
// and a client that meant to escape should hear so.
func cleanName(name string) (string, error) {
	if name == "" {
		return "/", nil
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}

	// Checked by walking the segments rather than by inspecting the cleaned
	// result, because path.Clean does not report an escape — it quietly clamps
	// one, turning /../etc into /etc. Clamping is harmless here, since these
	// paths address a virtual namespace and never reach the host filesystem,
	// but answering for a different path than the one asked for is not
	// something to do silently.
	depth := 0
	for _, segment := range strings.Split(name, "/") {
		switch segment {
		case "", ".":
		case "..":
			if depth--; depth < 0 {
				return "", os.ErrPermission
			}
		default:
			depth++
		}
	}

	cleaned := path.Clean(name)
	if cleaned != "/" {
		cleaned = strings.TrimSuffix(cleaned, "/")
	}
	return cleaned, nil
}

// split separates a path into its parent folder and its own name.
func split(name string) (dir, base string) {
	if name == "/" {
		return "/", ""
	}
	dir, base = path.Split(name)
	return vault.CleanDir(dir), base
}

// vaultError maps a vault error onto one the WebDAV handler understands, so a
// missing file becomes 404 rather than 500 and a locked vault becomes 401.
//
// Anything else passes through: a real failure should reach the client as one
// rather than being flattened into "not found", which would tell a sync client
// the file had been deleted and invite it to delete its own copy.
func vaultError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vault.ErrLocked):
		return err
	case errors.Is(err, vault.ErrNeedsConversion):
		// Refused rather than served, which is the point: rebuilding a
		// pre-chunking film in full to answer one range request is what took
		// the machine down. A player gets a failure it can show; converting the
		// file is done from the browser or the CLI, not implicitly by a read.
		return os.ErrPermission
	case strings.HasPrefix(err.Error(), "no such file"),
		strings.HasPrefix(err.Error(), "no such folder"):
		return os.ErrNotExist
	default:
		return err
	}
}

// Stat describes a file or folder.
func (fs *FileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	clean, err := cleanName(name)
	if err != nil {
		return nil, err
	}

	if fs.Vault.FolderExists(mainVault, clean) {
		return dirInfo(clean), nil
	}
	entry, err := fs.Vault.EntryByPath(mainVault, clean)
	if err != nil {
		return nil, vaultError(err)
	}
	return fileInfo{entry: entry}, nil
}

// Mkdir creates a folder.
func (fs *FileSystem) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	clean, err := cleanName(name)
	if err != nil {
		return err
	}
	if clean == "/" {
		return os.ErrExist
	}
	if fs.Vault.FolderExists(mainVault, clean) {
		return os.ErrExist
	}
	// The parent has to be there already: MKCOL creates one collection, and a
	// client relying on it to build a whole path would otherwise get a tree it
	// never asked for.
	if parent, _ := split(clean); !fs.Vault.FolderExists(mainVault, parent) {
		return os.ErrNotExist
	}
	return vaultError(fs.Vault.Mkdir(mainVault, clean))
}

// RemoveAll deletes a file, or a folder and everything under it.
func (fs *FileSystem) RemoveAll(ctx context.Context, name string) error {
	clean, err := cleanName(name)
	if err != nil {
		return err
	}
	if clean == "/" {
		return os.ErrPermission
	}

	if fs.Vault.FolderExists(mainVault, clean) {
		_, err := fs.Vault.Rmdir(ctx, mainVault, clean, true)
		return vaultError(err)
	}

	entry, err := fs.Vault.EntryByPath(mainVault, clean)
	if err != nil {
		return vaultError(err)
	}
	_, err = fs.Vault.Delete(ctx, entry.ID)
	return vaultError(err)
}

// Rename moves a file or a folder, which for the vault is an index change
// rather than a transfer: the parts stay exactly where they are, and a folder
// full of them moves in one write.
func (fs *FileSystem) Rename(ctx context.Context, oldName, newName string) error {
	oldClean, err := cleanName(oldName)
	if err != nil {
		return err
	}
	newClean, err := cleanName(newName)
	if err != nil {
		return err
	}
	if oldClean == "/" || newClean == "/" {
		return os.ErrPermission
	}

	if fs.Vault.FolderExists(mainVault, oldClean) {
		return vaultError(fs.Vault.MoveFolder(ctx, mainVault, oldClean, newClean))
	}

	entry, err := fs.Vault.EntryByPath(mainVault, oldClean)
	if err != nil {
		return vaultError(err)
	}
	dir, base := split(newClean)
	if !fs.Vault.FolderExists(mainVault, dir) {
		return os.ErrNotExist
	}
	_, err = fs.Vault.Move(ctx, entry.ID, dir, base)
	return vaultError(err)
}

// OpenFile opens a file for reading, or begins one for writing.
//
// Which of the two it is comes from the flags: the handler opens for write with
// O_RDWR|O_CREATE|O_TRUNC before copying a PUT body in, and for read with no
// flags at all. A write handle streams into the vault as it is written rather
// than collecting the body first.
func (fs *FileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	clean, err := cleanName(name)
	if err != nil {
		return nil, err
	}

	writing := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if !writing {
		return fs.openRead(ctx, clean)
	}
	if clean == "/" || fs.Vault.FolderExists(mainVault, clean) {
		return nil, os.ErrPermission
	}

	dir, base := split(clean)
	if !fs.Vault.FolderExists(mainVault, dir) {
		return nil, os.ErrNotExist
	}
	if base == "" {
		return nil, os.ErrPermission
	}

	// Appending stores the file again with the new bytes on the end, because
	// the vault stores whole files. What it does not do is hold either half in
	// memory: the existing file is read back as a stream and the new bytes
	// follow it into the same streaming upload, so the cost is bandwidth rather
	// than RAM. O_TRUNC wins if both are set, which is what os.OpenFile does.
	var prefix io.Reader
	if flag&os.O_APPEND != 0 && flag&os.O_TRUNC == 0 {
		if existing, err := fs.Vault.EntryByPath(mainVault, clean); err == nil {
			body, _, err := fs.Vault.OpenReadSeeker(ctx, existing.ID)
			if err != nil {
				return nil, vaultError(err)
			}
			prefix = body
		}
	}

	return newWriteFile(ctx, fs.Vault, dir, base, prefix), nil
}

// openRead opens a folder for listing or a file for reading.
func (fs *FileSystem) openRead(ctx context.Context, clean string) (webdav.File, error) {
	if fs.Vault.FolderExists(mainVault, clean) {
		listing, err := fs.Vault.List(mainVault, clean)
		if err != nil {
			return nil, vaultError(err)
		}
		return newDirFile(clean, listing), nil
	}

	entry, err := fs.Vault.EntryByPath(mainVault, clean)
	if err != nil {
		return nil, vaultError(err)
	}
	return newReadFile(ctx, fs.Vault, entry)
}

// ---------------------------------------------------------------------------
// FileInfo
// ---------------------------------------------------------------------------

// fileInfo describes a stored file to a WebDAV client.
type fileInfo struct{ entry *vault.Entry }

func (f fileInfo) Name() string       { return f.entry.Name }
func (f fileInfo) Size() int64        { return f.entry.Size }
func (f fileInfo) Mode() os.FileMode  { return 0o444 }
func (f fileInfo) ModTime() time.Time { return f.entry.ModifiedAt }
func (f fileInfo) IsDir() bool        { return false }
func (f fileInfo) Sys() any           { return f.entry }

// dirFileInfo describes a folder. The vault records no timestamp for one — a
// folder is a path that files sit under, not an object — so it reports the zero
// time, which clients treat as unknown.
type dirFileInfo struct{ name string }

func dirInfo(p string) os.FileInfo {
	name := path.Base(p)
	if p == "/" {
		name = "/"
	}
	return dirFileInfo{name: name}
}

func (d dirFileInfo) Name() string       { return d.name }
func (d dirFileInfo) Size() int64        { return 0 }
func (d dirFileInfo) Mode() os.FileMode  { return os.ModeDir | 0o555 }
func (d dirFileInfo) ModTime() time.Time { return time.Time{} }
func (d dirFileInfo) IsDir() bool        { return true }
func (d dirFileInfo) Sys() any           { return nil }

// ---------------------------------------------------------------------------
// Directories
// ---------------------------------------------------------------------------

// dirFile is a folder opened for listing. The listing is taken once, when the
// folder is opened, so a PROPFIND walking it sees one consistent answer.
type dirFile struct {
	path    string
	entries []os.FileInfo
	offset  int
}

func newDirFile(p string, listing *vault.Listing) *dirFile {
	out := make([]os.FileInfo, 0, len(listing.Folders)+len(listing.Files))
	for _, folder := range listing.Folders {
		out = append(out, dirFileInfo{name: path.Base(folder)})
	}
	for _, entry := range listing.Files {
		out = append(out, fileInfo{entry: entry})
	}
	return &dirFile{path: p, entries: out}
}

func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Read([]byte) (int, error)   { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error)  { return 0, os.ErrInvalid }
func (d *dirFile) Stat() (os.FileInfo, error) { return dirInfo(d.path), nil }
func (d *dirFile) Seek(int64, int) (int64, error) {
	return 0, os.ErrInvalid
}

// Readdir follows os.File's contract: a count of zero or less returns
// everything at once, and a positive count pages through, ending with io.EOF.
func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		rest := d.entries[d.offset:]
		d.offset = len(d.entries)
		return rest, nil
	}
	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}
	end := d.offset + count
	if end > len(d.entries) {
		end = len(d.entries)
	}
	page := d.entries[d.offset:end]
	d.offset = end
	return page, nil
}
