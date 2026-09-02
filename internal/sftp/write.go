package sftp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"
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
// meet supports. The fallback for the ones that do not is remove-then-rename,
// which has a window where the file does not exist.
func RenameOver(c *Client, from, to string) error {
	fs := c.SFTP()
	if err := fs.PosixRename(from, to); err == nil {
		return nil
	}
	if err := fs.Remove(to); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fs.Rename(from, to)
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
