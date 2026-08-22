package sftp

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	pkgsftp "github.com/pkg/sftp"
)

// MaxEntries bounds one directory listing.
//
// A directory with a hundred thousand files in it is a real thing — a Maildir,
// a sprite dump, a camera roll that was never sorted — and neither the browser
// nor the JSON in between wants all of it. The listing says it was cut short
// rather than pretending it was complete, which is the same bargain
// handleSystemFolders strikes for folders on this machine.
const MaxEntries = 2000

// Entry is one item in a remote directory.
type Entry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Dir     bool      `json:"dir,omitempty"`
	ModTime time.Time `json:"modified,omitzero"`

	// Symlink says this entry is a link. Worth showing rather than resolving
	// away: a tree full of links behaves differently from a tree full of
	// files, and somebody choosing what to import should be able to see which
	// they have.
	Symlink bool `json:"symlink,omitempty"`

	// Unreachable marks a link that cannot be followed — either because its
	// target is missing, or because the target lies outside the source's root.
	// The entry is listed and not offered: quietly hiding it would leave a
	// directory that appears to have fewer files than `ls` shows, which reads
	// as a bug rather than as a rule.
	Unreachable bool `json:"unreachable,omitempty"`

	// Reason says why, for the entries that are unreachable.
	Reason string `json:"reason,omitempty"`
}

// Listing is one directory as the browser needs it.
type Listing struct {
	// Path is the directory that was listed, relative to the root. The root
	// itself is "".
	Path string `json:"path"`

	// Parent is the directory above it, or "" at the root. A nil parent and a
	// root-level parent are told apart by AtRoot rather than by this being
	// empty, because "" is a real path here.
	Parent string `json:"parent"`
	AtRoot bool   `json:"at_root,omitempty"`

	Entries []Entry `json:"entries"`

	// Truncated says the directory held more than MaxEntries.
	Truncated bool `json:"truncated,omitempty"`
}

// ReadDir lists one directory under root.
//
// rel is relative to root and is put through Under, so nothing outside the
// source's configured folder can be listed however the path is written — the
// browse endpoint takes rel straight from a query string, which makes this the
// boundary rather than a formality.
func (c *Client) ReadDir(root, rel string) (Listing, error) {
	root = CleanPath(root)
	if root == "" {
		root = "/"
	}
	full, err := c.resolve(root, rel)
	if err != nil {
		return Listing{}, err
	}

	infos, err := c.sftp.ReadDir(full)
	if err != nil {
		return Listing{}, fmt.Errorf("cannot list %s: %w", displayPath(root, full), err)
	}

	listing := Listing{
		Path:   relativeTo(root, full),
		AtRoot: full == root,
	}
	if !listing.AtRoot {
		listing.Parent = relativeTo(root, path.Dir(full))
	}

	for _, info := range infos {
		if len(listing.Entries) >= MaxEntries {
			listing.Truncated = true
			break
		}
		listing.Entries = append(listing.Entries, c.entry(root, full, info))
	}

	// Folders first, then by name, the way a file browser orders things. Case
	// is folded so that "Photos" and "archive" interleave the way a person
	// reading the list expects rather than the way ASCII does.
	sort.SliceStable(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return listing, nil
}

// entry describes one directory member, resolving a symlink far enough to say
// whether it can be followed.
func (c *Client) entry(root, dir string, info os.FileInfo) Entry {
	e := Entry{
		Name:    info.Name(),
		Size:    info.Size(),
		Dir:     info.IsDir(),
		ModTime: info.ModTime(),
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return e
	}

	e.Symlink = true
	full := path.Join(dir, info.Name())

	target, err := c.sftp.ReadLink(full)
	if err != nil {
		e.Unreachable, e.Reason = true, "this link cannot be read"
		return e
	}
	// A relative target is relative to the directory holding the link, not to
	// the root and not to wherever SFTP happened to drop us.
	if !path.IsAbs(target) {
		target = path.Join(dir, target)
	}

	// The rule the whole browse side rests on: a link is followed only if it
	// lands inside the source's root. Browsing wants to be permissive, and a
	// symlink is how permissiveness escapes — /srv/media/everything → / is one
	// command, and without this it would turn a scoped source into a file
	// browser for the entire machine.
	//
	// This is what the listing *shows*; resolve is what actually refuses to
	// descend or open. Both, because a flag in a listing is a courtesy and the
	// browser is free to send back a path it was never shown.
	if !c.insideRoot(root, target) {
		e.Unreachable, e.Reason = true, "this link points outside the folder this source is scoped to"
		return e
	}

	// Inside the root, so it is worth knowing what it actually is.
	resolved, err := c.sftp.Stat(full)
	if err != nil {
		e.Unreachable, e.Reason = true, "this link points at something that is not there"
		return e
	}
	e.Dir = resolved.IsDir()
	e.Size = resolved.Size()
	return e
}

// within reports whether an absolute path is at or below root.
func within(root, full string) bool {
	root, full = CleanPath(root), CleanPath(full)
	return full == root || strings.HasPrefix(full, strings.TrimSuffix(root, "/")+"/")
}

// relativeTo expresses an absolute remote path as one relative to root, which
// is the only form that ever leaves this package: an API that handed out
// absolute server paths would be inviting the next caller to send one back.
func relativeTo(root, full string) string {
	root, full = CleanPath(root), CleanPath(full)
	if full == root {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(full, strings.TrimSuffix(root, "/")), "/")
}

// displayPath names a path in an error the way the person reading it thinks of
// it — relative to the source's root, since that is the only part they chose.
func displayPath(root, full string) string {
	if rel := relativeTo(root, full); rel != "" {
		return rel
	}
	return "the source's folder"
}

// OpenUnder opens one file under root for reading, and reports its size.
//
// The same boundary as ReadDir, through the same resolve — so a link out of the
// root is refused before the file is opened rather than after. That ordering is
// the point: Stat follows links, so a check made on the opened file would be a
// check on the target while the decision to open it had already been taken.
func (c *Client) OpenUnder(root, rel string) (*pkgsftp.File, os.FileInfo, error) {
	root = CleanPath(root)
	if root == "" {
		root = "/"
	}
	full, err := c.resolve(root, rel)
	if err != nil {
		return nil, nil, err
	}

	info, err := c.sftp.Stat(full)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a folder, not a file", displayPath(root, full))
	}
	if !info.Mode().IsRegular() {
		// A fifo, a socket, a device node. Reading one either blocks forever or
		// returns something that is not the file's contents.
		return nil, nil, fmt.Errorf("%s is not a regular file", displayPath(root, full))
	}

	f, err := c.sftp.Open(full)
	if err != nil {
		return nil, nil, err
	}
	return f, info, nil
}

// maxLinkHops bounds how many symlinks one path may be resolved through, so a
// pair of links pointing at each other is an error rather than a hang.
const maxLinkHops = 32

// resolve turns a caller-supplied relative path into a path on the server that
// is genuinely under root, and refuses it otherwise.
//
// Under is not enough on its own. It is lexical: it stops "../.." but it cannot
// stop a symlink, because the escape is not written in the path — `ln -s /
// everything` makes "everything" an ordinary-looking name that the server
// expands to the whole machine. Every component below the root therefore gets
// looked at, and a link is followed only if its target lands back inside.
//
// The walk is done here rather than left to the server's own canonicalization.
// SSH_FXP_REALPATH is specified loosely enough that servers differ on whether
// they resolve links at all, and a boundary that holds against some servers is
// not a boundary.
//
// What comes back is the resolved path, used to talk to the server. The
// *logical* path is what callers report to the browser, so following a link
// inside the root does not rewrite the breadcrumb out from under whoever
// clicked it.
func (c *Client) resolve(root, rel string) (string, error) {
	root = CleanPath(root)
	if root == "" {
		root = "/"
	}
	joined, err := Under(root, rel)
	if err != nil {
		return "", err
	}
	if joined == root {
		return root, nil
	}

	rest := strings.Split(strings.TrimPrefix(strings.TrimPrefix(joined, root), "/"), "/")
	current := root
	hops := 0

	for i, part := range rest {
		if part == "" {
			continue
		}
		current = path.Join(current, part)

		info, err := c.sftp.Lstat(current)
		if err != nil {
			// Nothing there to follow. Everything below a path that does not
			// exist is equally unreachable, so the rest is safe to hand back
			// unwalked and let the caller's own operation report the absence —
			// it says what was actually being attempted, and this cannot.
			return path.Join(append([]string{current}, rest[i+1:]...)...), nil
		}

		for info.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > maxLinkHops {
				return "", fmt.Errorf("%q leads through too many links to follow", rel)
			}
			target, err := c.sftp.ReadLink(current)
			if err != nil {
				return "", fmt.Errorf("cannot read the link %s: %w", displayPath(root, current), err)
			}
			if !path.IsAbs(target) {
				target = path.Join(path.Dir(current), target)
			}
			target = CleanPath(target)

			if !c.insideRoot(root, target) {
				return "", fmt.Errorf("%s is a link pointing outside the folder this source is scoped to",
					displayPath(root, current))
			}
			current = target

			if info, err = c.sftp.Lstat(current); err != nil {
				return "", fmt.Errorf("%s is a link pointing at something that is not there",
					displayPath(root, path.Join(root, rel)))
			}
		}
	}
	return current, nil
}

// insideRoot reports whether a resolved link target is still within the
// source's folder.
//
// Two forms of the root are accepted because both are the same folder: the one
// the source was configured with, and that one canonicalized. A root that is
// itself reached through a link — /srv/media where media is a link to
// /mnt/disk2/media — would otherwise reject its own contents, because links
// inside it point at the resolved form and the configured root is the
// unresolved one.
func (c *Client) insideRoot(root, target string) bool {
	if within(root, target) {
		return true
	}
	real, err := c.realRoot(root)
	if err != nil {
		return false
	}
	return within(real, target)
}

// realRoot is the source's folder as the server canonicalizes it, cached
// because it is asked for on every escaping-looking link and cannot change
// while a connection is open without the connection being wrong about a great
// deal more than this.
func (c *Client) realRoot(root string) (string, error) {
	root = CleanPath(root)

	c.rootsMu.Lock()
	cached, ok := c.roots[root]
	c.rootsMu.Unlock()
	if ok {
		return cached, nil
	}

	canonical, err := c.sftp.RealPath(root)
	if err != nil {
		return "", fmt.Errorf("cannot find %s on the server: %w", root, err)
	}
	canonical = CleanPath(canonical)

	c.rootsMu.Lock()
	if c.roots == nil {
		c.roots = map[string]string{}
	}
	c.roots[root] = canonical
	c.rootsMu.Unlock()
	return canonical, nil
}

// StatUnder describes one path under root without opening it, following a link
// only if it stays inside — the same boundary as ReadDir and OpenUnder.
//
// What it answers is "is this a file or a folder", which is what a caller
// expanding a selection has to know before it can decide whether to walk it.
func (c *Client) StatUnder(root, rel string) (os.FileInfo, error) {
	full, err := c.resolve(root, rel)
	if err != nil {
		return nil, err
	}
	info, err := c.sftp.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", displayPath(CleanPath(root), full), err)
	}
	return info, nil
}
