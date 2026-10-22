package sftp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	pkgsftp "github.com/pkg/sftp"
)

// Writing to a machine, which the rest of this package was built not to do.
//
// The browse side of a source is read-only by design, and this file is the one
// exception: an export puts the user's own files back onto a machine they have
// a login on. It goes through the same boundary reading does — resolve, which
// refuses a path outside the root and a link pointing out of it — so the
// direction changes and the fence does not. What it will never do is write a
// shard: that is the backend's job, under names SAND invents, and keeping the
// two apart is the whole reason a source and a backend are two entries.

// tempPrefix names a file that is still being written.
//
// The same prefix the backend uses for a half-written shard, on purpose: the
// backend's listing already knows to hide it, so a source and a backend that
// happen to share a folder cannot mistake each other's leftovers for content.
const tempPrefix = ".sand-tmp-"

// TempName is a name for a file being written into dir, unique enough that two
// SAND instances writing into the same folder cannot rename each other's
// half-written file into place.
func TempName(dir string) string {
	return path.Join(dir, tempPrefix+randomSuffix())
}

// IsTempName reports whether a name is one of these, for listings that should
// not offer a file that is still arriving.
func IsTempName(name string) bool {
	return len(name) > len(tempPrefix) && name[:len(tempPrefix)] == tempPrefix
}

// asideInfix marks the previous copy of a file while a new one is renamed
// into its place — see RenameOver. It goes on the end of the name rather
// than the front so the copy stays beside the file it was, which is where
// somebody looking for it after a failure would look.
const asideInfix = ".sand-old-"

// asideName is where RenameOver keeps the file it is about to replace.
func asideName(name string) string {
	return name + asideInfix + randomSuffix()
}

// IsAsideName reports whether a name is a previous copy RenameOver set aside
// and did not get to remove, for listings that should not offer it as an
// object of its own.
func IsAsideName(name string) bool {
	return strings.Contains(name, asideInfix)
}

// IsScratchName reports whether a name is one of SAND's own working files —
// a file still arriving, or a previous copy set aside — rather than an
// object. A listing of a folder SAND writes into hides both.
func IsScratchName(name string) bool {
	return IsTempName(name) || IsAsideName(name)
}

// randomSuffix is eight random bytes as hex. Random rather than counted because
// two instances may share a folder, and a counter would have them collide.
func randomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice, and a temp name is not a
		// secret: any name nobody else is using will do.
		return "fallback"
	}
	return hex.EncodeToString(buf[:])
}

// RenameOver puts a temporary file in its final place, overwriting whatever
// is there.
//
// Two ways round, because the protocol's own rename cannot do it. SFTP v3
// leaves the behaviour of a rename onto an existing name to the server, and
// OpenSSH's answer is to refuse — so OpenSSH also ships posix-rename@openssh.com,
// which is atomic and overwrites, and is what nearly every server SAND will
// meet supports. The fallback for the ones that do not is renameAside.
func RenameOver(c *Client, from, to string) error {
	fs := c.SFTP()
	if err := fs.PosixRename(from, to); err == nil {
		return nil
	}
	return renameAside(fs, from, to)
}

// renameAside replaces to with from on a server without posix-rename, by
// renaming the old file aside, renaming the new one into place, and then
// removing the old one.
//
// Not remove-then-rename, which is the obvious fallback and the wrong one:
// between the remove and the rename there is a moment where nothing is under
// the name, and the thing that interrupts a network write is a dropped
// connection, which is common. A connection that drops there leaves the key
// empty on a server that still holds a perfectly good copy under a temporary
// name — a part lost to the vault, which repairs it, and an orphan nobody
// lists. Rename-aside has no such moment: at every point the name holds
// either the old file or the new one, and a failure after the old file has
// been moved out of the way puts it back.
//
// What a dropped connection can leave behind instead is the aside copy, when
// it drops after the new file is in place and before the old one is removed.
// That is a file nobody is missing, hidden from listings by IsAsideName, and
// at worst some space to reclaim by hand.
func renameAside(fs *pkgsftp.Client, from, to string) error {
	aside := asideName(to)
	if err := fs.Rename(to, aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing to overwrite, so the plain rename is the whole job.
			return fs.Rename(from, to)
		}
		return fmt.Errorf("setting aside the previous %s: %w", path.Base(to), err)
	}

	if err := fs.Rename(from, to); err != nil {
		// The name is empty right now. Put the old file back, so a failed
		// write is a failed write and not a vanished part.
		if restoreErr := fs.Rename(aside, to); restoreErr != nil {
			return fmt.Errorf("%w (and the previous copy could not be put back from %s: %v)",
				err, path.Base(aside), restoreErr)
		}
		return err
	}

	if err := fs.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The new file is in place and the write has succeeded; what has
		// not happened is the tidying. Reported all the same, because an
		// aside copy that cannot be removed is space the account will go on
		// spending until somebody removes it by hand, and an error is how
		// they find out there is one.
		return fmt.Errorf("removing the previous copy %s: %w", path.Base(aside), err)
	}
	return nil
}

// MkdirUnder makes a directory under root, and everything above it, and
// returns the path on the server it made.
//
// Through resolve rather than a plain join, so that a component of the path
// that is a link is followed only if it stays inside the root — a folder made
// through a link pointing out of the source's folder would be a folder made
// somewhere the source was never allowed to see.
func (c *Client) MkdirUnder(root, rel string) (string, error) {
	root = CleanPath(root)
	if root == "" {
		root = "/"
	}
	full, err := c.resolve(root, rel)
	if err != nil {
		return "", err
	}
	if err := c.sftp.MkdirAll(full); err != nil {
		return "", fmt.Errorf("cannot make the folder %s: %w", displayPath(root, full), err)
	}
	return full, nil
}

// ErrExists is refusing to write over a file that is already there.
var ErrExists = errors.New("a file with this name is already there")

// WriteOptions says how one file should land.
type WriteOptions struct {
	// Overwrite replaces a file already at the name. Without it, a file that
	// is there is left alone and the write fails with ErrExists — before a
	// byte is sent, since sending a film to find out is a poor way to ask.
	Overwrite bool

	// ModTime is stamped on the file once it is in place, when set. It is
	// what lets a later export see that this copy is the one it already made.
	ModTime time.Time
}

// WriteUnder writes r to rel under root and reports how many bytes it wrote.
//
// The file goes to a temporary name in its own directory and is renamed into
// place only once every byte is there and the handle is closed. So the name
// the caller asked for is either the whole file or nothing: a connection that
// drops halfway leaves a temp file behind and never a short file under a real
// name. That is the property an export's skip rule rests on — see
// vault.ExportToSource.
func (c *Client) WriteUnder(root, rel string, r io.Reader, opts WriteOptions) (int64, error) {
	root = CleanPath(root)
	if root == "" {
		root = "/"
	}
	full, err := c.resolve(root, rel)
	if err != nil {
		return 0, err
	}
	if full == root {
		return 0, fmt.Errorf("cannot write to the source's folder itself: name a file")
	}

	if !opts.Overwrite {
		// Asked up front rather than left to the rename, because servers
		// differ: OpenSSH refuses a rename onto an existing name and others
		// quietly replace it, and a rule that holds on some servers is not a
		// rule. The window between this and the rename is one a second
		// writer would have to hit on purpose.
		if _, err := c.sftp.Lstat(full); err == nil {
			return 0, fmt.Errorf("%s: %w", displayPath(root, full), ErrExists)
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("cannot check %s: %w", displayPath(root, full), err)
		}
	}

	dir := path.Dir(full)
	if err := c.sftp.MkdirAll(dir); err != nil {
		return 0, fmt.Errorf("cannot make the folder %s: %w", displayPath(root, dir), err)
	}

	tmp := TempName(dir)
	f, err := c.sftp.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("cannot create %s: %w", displayPath(root, full), err)
	}
	// Before the bytes rather than after: these are the user's own files in
	// the clear, and on a server with a permissive umask the window between
	// creating and chmod-ing is a window where everyone can read them.
	if err := c.sftp.Chmod(tmp, 0o600); err != nil {
		f.Close()
		c.sftp.Remove(tmp)
		return 0, fmt.Errorf("cannot set permissions on %s: %w", displayPath(root, full), err)
	}

	// The file's own ReadFrom keeps many writes in flight rather than one
	// packet per round trip, which is the difference between a link's speed
	// and 200 KB/s to a server 150 ms away — so the reader is handed over
	// whole rather than copied through here. See progressReader.WriteTo in
	// the vault for the same point made from the other side.
	n, err := f.ReadFrom(r)
	if err != nil {
		f.Close()
		c.sftp.Remove(tmp)
		return n, fmt.Errorf("writing %s: %w", displayPath(root, full), err)
	}
	if err := f.Close(); err != nil {
		c.sftp.Remove(tmp)
		return n, fmt.Errorf("closing %s: %w", displayPath(root, full), err)
	}

	if opts.Overwrite {
		err = RenameOver(c, tmp, full)
	} else {
		// The protocol's own rename, which OpenSSH refuses onto an existing
		// name — so on the servers that can, the check above is made a second
		// time at the last moment.
		err = c.sftp.Rename(tmp, full)
	}
	if err != nil {
		c.sftp.Remove(tmp)
		return n, fmt.Errorf("putting %s in place: %w", displayPath(root, full), err)
	}

	// Best effort. A server that will not take a timestamp still holds the
	// file, and a later export reads the newer time as "not older than the
	// copy in the vault", which is the right answer for a file that is there.
	if !opts.ModTime.IsZero() {
		_ = c.sftp.Chtimes(full, opts.ModTime, opts.ModTime)
	}
	return n, nil
}
