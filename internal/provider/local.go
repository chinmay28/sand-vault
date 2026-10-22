package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The local folder backend keeps parts as plain files in one directory.
//
// Its contract with that directory, the account's root:
//
//   - Keys are flat filenames (vault.ShardKey), so every object is a file
//     directly inside the root, and the root is the only directory this
//     backend ever touches. Nothing is nested under it, and nothing is
//     pruned.
//   - The backend never creates the root, with one exception: Ping creates
//     it when it is missing, and only inside a parent that already exists.
//     A missing parent is the shape of an unmounted drive — udisks removes
//     /media/<user>/<Drive> on unmount — and creating the chain through it
//     would put the folder, and then the parts, on the wrong filesystem under
//     the mount point while reporting the account healthy.
//   - The backend never removes the root. A user's account folder on a
//     removable drive once vanished because Delete pruned "the parent of the
//     object", which with flat keys is the root itself; the parent belonged
//     to the desktop user, so the service could not even put it back.
//   - A missing root is an error from Put, Delete and List alike. Put must
//     not recreate it (that is the unmounted-drive case again), Delete must
//     not report a part erased from a folder that is not there, and List
//     must not answer "empty" for a folder it could not read — the orphan
//     scan treats "listed, nothing there" and "could not list" very
//     differently. Only "this one object is already gone, in a root that is
//     there" stays a successful Delete, because cleanup paths rely on
//     deletes being idempotent.

func init() {
	Register(Spec{
		Kind:        KindLocal,
		Label:       "Local folder",
		Description: "A directory on this machine or a mounted network/removable drive. Useful as an offline third leg alongside two cloud accounts.",
		// Last in the dialog: it is the one that names no service, and the
		// answer when none of the others fit.
		Order: 40,
		Covers: []Service{
			{Name: "An external or network drive", Hint: "wherever it is mounted"},
			{Name: "A NAS share", Hint: "an SMB or NFS mount on this machine"},
			{Name: "Any folder another tool keeps in step", Hint: "including a service SAND has no backend for"},
		},
		Fields: []FieldSpec{
			{
				Key:         "path",
				Label:       "Directory",
				Placeholder: "/mnt/backup/sand",
				Help:        "Created if it does not exist, inside a folder that does.",
				Required:    true,
				Directory:   true,
			},
		},
	}, newLocalProvider)
}

// newLocalProvider builds a directory-backed provider. Shared with the sync
// folder backends, which are the same thing pointed at a folder some desktop
// client keeps in step with a cloud account.
func newLocalProvider(cfg Config) (Provider, error) {
	root := ExpandHome(cfg.Option("path"))
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", root, err)
	}
	return &localProvider{base: base{cfg: cfg}, root: abs}, nil
}

// ExpandHome resolves a leading ~ so a path copied out of documentation works
// as typed.
func ExpandHome(path string) string {
	path = strings.TrimSpace(path)
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// localProvider stores shards as files under a root directory. See the
// contract at the top of the file for what it does and does not do to that
// directory.
type localProvider struct {
	base
	root string
}

// resolve maps an object key onto a path inside the root, refusing any key
// that would escape it and any key that names the root itself. The second
// refusal matters as much as the first: "", "." and "/" all clean to the
// root, and a Delete of the root is the account folder gone.
func (p *localProvider) resolve(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash("/" + strings.TrimPrefix(key, "/")))
	full := filepath.Join(p.root, clean)
	if full == p.root {
		return "", fmt.Errorf("invalid key %q: it names the account folder itself, not an object in it", key)
	}
	if !strings.HasPrefix(full, p.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return full, nil
}

// checkRoot is the "is the folder there" question every operation but Ping
// asks before touching anything, so that a folder that is gone — the drive
// unmounted, the directory deleted — is an error with the repair in it rather
// than a Put that quietly recreates the folder on the wrong disk, a Delete
// that reports success, or a List that reads as empty.
func (p *localProvider) checkRoot() error {
	info, err := os.Stat(p.root)
	if err != nil {
		return fmt.Errorf("cannot use %s: %s%s", p.root, cause(err), hint(p.root, err))
	}
	if !info.IsDir() {
		return fmt.Errorf("cannot use %s: it is a file, not a directory", p.root)
	}
	return nil
}

// Put stores a part as a file in the root. The root has to be there already:
// Ping is what creates it, and a Put that finds it missing says so instead.
func (p *localProvider) Put(ctx context.Context, key string, data []byte) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := p.checkRoot(); err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if dir != p.root {
		// Keys are flat, so this is never taken by the vault. It is here so
		// that a key with a slash in it still lands somewhere sensible —
		// inside the root, which checkRoot has just proved is there.
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("creating directory: %w", err)
		}
	}

	// Write to a temp file in the destination directory, then rename, so a
	// crash mid-write can never leave a half-written shard behind. The rename
	// protects the name; the fsync before it protects the bytes. Without it a
	// power cut after the rename can leave a zero-length file under a name
	// the index trusts, which is worse than no file at all.
	//
	// CreateTemp opens the file 0600, which is the mode a part should have;
	// it is not chmod'ed afterwards because exFAT and vfat drives — the
	// common formats for a removable disk — refuse chmod with EPERM, and
	// that used to fail every Put on such a drive while deletes went through.
	tmp, err := os.CreateTemp(dir, ".sand-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing shard: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("syncing shard: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing shard: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("finalizing shard: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir flushes a directory so a rename into it survives a power cut. Best
// effort: vfat and exFAT, and some network filesystems, refuse to fsync a
// directory, and Windows has no such call at all. On those the data is
// already synced and the name is as durable as the filesystem makes it.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

func (p *localProvider) Get(ctx context.Context, key string) ([]byte, error) {
	full, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (p *localProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	full, err := p.resolve(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(full)
	if os.IsNotExist(err) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: info.Size()}, nil
}

// Delete removes one part. A part that is already gone is not an error — but
// only when the root is there to be checked. With the drive unmounted every
// path is "already gone", and reporting that as success would have callers
// believe parts were erased that are still on the disk.
//
// The root is left alone whatever happens: an older version pruned the
// object's parent directory, which with flat keys is the root, and that
// removed a user's whole account folder when its last part went.
func (p *localProvider) Delete(ctx context.Context, key string) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	err = os.Remove(full)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The object is not there. Whether that means "already deleted" or "the
	// whole folder is gone" is a question for the root.
	return p.checkRoot()
}

// List walks the root. A root that is not there is an error, not an empty
// listing: an account that answers "nothing here" when its drive is unplugged
// would have every one of its parts read as abandoned.
func (p *localProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := p.checkRoot(); err != nil {
		return nil, err
	}
	var out []ObjectInfo
	err := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == p.root {
				// The root went between the check and the walk; the same
				// answer as if it had never been there.
				if rootErr := p.checkRoot(); rootErr != nil {
					return rootErr
				}
				return err
			}
			if os.IsNotExist(err) {
				// An entry that vanished mid-walk is a part deleted under us,
				// which is not this listing's problem.
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(p.root, path)
		if relErr != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Ping proves the folder is there and writable, creating it if it is not —
// but only the folder itself, inside a parent that already exists. It is the
// one place this backend creates anything other than a part: connecting an
// account, and every health check after, goes through here, and that is where
// "the folder is missing" should be answered, not in the middle of a write.
//
// The parent is required rather than created because a missing parent is
// what an unmounted drive looks like. MkdirAll would build the path through
// the empty mount point, on the system disk, and the account would then
// accept parts onto the wrong filesystem while reporting itself healthy.
func (p *localProvider) Ping(ctx context.Context) error {
	parent := filepath.Dir(p.root)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("cannot use %s: %s%s", p.root, cause(err), hint(p.root, err))
	}
	if !info.IsDir() {
		return fmt.Errorf("cannot use %s: %s is a file, not a directory", p.root, parent)
	}
	if err := os.Mkdir(p.root, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cannot create %s: %s%s", p.root, cause(err), hint(p.root, err))
	}
	if err := p.checkRoot(); err != nil {
		return err
	}
	probe := filepath.Join(p.root, ".sand-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		return fmt.Errorf("%s is not writable: %s%s", p.root, cause(err), hint(p.root, err))
	}
	return os.Remove(probe)
}

// errNoDiskUsage is what a platform with no way to ask returns. Not surfaced
// to anyone: a Usage that fails is a Usage the account card simply does not
// draw, the same as one from a backend that reports no quota at all.
var errNoDiskUsage = errors.New("disk usage unavailable on this platform")

// Usage reports the drive the folder sits on: how big it is, how much of it is
// spoken for by everything on it, and how much a part could still be written
// into.
//
// The folder itself is not measured — what SAND put here is already known from
// the index, and walking a drive to weigh it again would cost a full traversal
// on every refresh of the sidebar. What the index cannot know is the other
// 800 GB on the disk, and that is what this asks for.
//
// A folder that is not there yet is answered for by the nearest parent that
// is: the drive is the same drive either way, and Ping creates the folder on
// the next refresh anyway.
func (p *localProvider) Usage(ctx context.Context) (Usage, error) {
	dir := p.root
	for {
		usage, err := diskUsage(dir)
		if err == nil {
			return usage, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Usage{}, err
		}
		dir = parent
	}
}

// cause strips the syscall wrapping off a filesystem error, leaving the part
// that says what actually went wrong. The path is already in the sentence.
func cause(err error) string {
	var perr *fs.PathError
	if errors.As(err, &perr) {
		return perr.Err.Error()
	}
	return err.Error()
}

// hint explains an unusable directory in terms of the thing to go fix. The
// ways a local folder usually fails — a sandboxed service, a folder owned by
// somebody else, a drive that is not mounted, a folder that has gone from a
// drive that is — are indistinguishable in the raw syscall error and have
// nothing in common as repairs. Returns a leading-space sentence, or "" when
// there is nothing useful to add.
func hint(root string, err error) string {
	switch {
	case errors.Is(err, syscall.EROFS):
		if sandboxed() {
			if underMountRoot(root) {
				// The unit both installers write grants these roots, so a unit
				// that does not is one an older installer wrote. Upgrading is
				// the repair, and it fixes every other drive at the same time.
				return " — the sand service runs under systemd with ProtectSystem=strict," +
					" and this path is under " + strings.Join(mountRoots, ", ") +
					", which the current unit grants but an older one does not." +
					" Re-run the installer to refresh the unit, or grant this path alone:" +
					" sudo scripts/allow-local-path.sh " + root + ", then reconnect."
			}
			return " — the sand service runs under systemd with ProtectSystem=strict," +
				" which makes every path outside its data directory and the usual mount" +
				" roots (" + strings.Join(mountRoots, ", ") + ") read-only to it." +
				" Grant it this one: sudo scripts/allow-local-path.sh " + root +
				" (or add ReadWritePaths= to the unit) and reconnect."
		}
		if mountedReadOnly(root) {
			return " — the filesystem holding it is mounted read-only." +
				" Remount it read-write; on an NTFS drive that usually means clearing" +
				" the dirty bit left by Windows fast startup or hibernation."
		}
		return " — remount the filesystem holding it read-write, or, if SAND runs as a" +
			" sandboxed service, grant the service write access to this path."
	case errors.Is(err, fs.ErrPermission):
		return " — SAND runs as " + whoami() + ", which has no write permission there." +
			" A removable drive mounted by a desktop session belongs to that desktop user" +
			" and is typically unreadable to anyone else: chown the folder to the service" +
			" user, or mount the drive with permissions it has."
	case errors.Is(err, fs.ErrNotExist):
		// Which directory is missing decides the sentence. A missing parent is
		// the shape of an unmounted drive, and nothing SAND does will help
		// until it is mounted. A missing folder inside a parent that is there
		// is either a drive mounted somewhere else or a folder somebody
		// deleted, and the next health check puts it back — empty.
		parent := filepath.Dir(root)
		if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
			return " — " + parent + " does not exist. If this is a removable or network" +
				" drive, it is probably not mounted."
		}
		return " — the folder is gone, though " + parent + " is still there. If this is a" +
			" removable or network drive, it is probably not mounted or was mounted" +
			" somewhere else; otherwise the folder was deleted or moved. Once the drive" +
			" is back, the next health check recreates the folder."
	case errors.Is(err, syscall.ENOSPC):
		return " — the filesystem is full."
	case errors.Is(err, syscall.ENOTDIR):
		return " — part of that path is a file, not a directory."
	}
	return ""
}

// mountRoots are the directories removable disks and network shares get
// mounted under, which the systemd unit the installers write grants outright
// (see scripts/quickstart.sh). A local folder under one of these needs no
// per-path grant on a current install.
var mountRoots = []string{"/media", "/run/media", "/mnt", "/srv"}

// MountRoots returns those directories, for the folder picker: they are the
// places a drive worth scattering shards onto turns up, and the shortcuts it
// offers alongside home.
func MountRoots() []string {
	return append([]string(nil), mountRoots...)
}

// underMountRoot reports whether path is one of the mount roots or sits inside
// one. Lexical, like the ReadWritePaths= match it mirrors.
func underMountRoot(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range mountRoots {
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return true
		}
	}
	return false
}

// sandboxed reports whether this process was started by systemd, which is the
// deployment where ProtectSystem=strict turns an ordinary directory read-only
// without anything about the drive itself being wrong.
func sandboxed() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

// mountedReadOnly reports whether the mount that path falls under carries the
// ro option. Reads /proc/self/mounts, so it answers only on Linux — elsewhere
// it says no and the caller falls back to the generic wording.
func mountedReadOnly(path string) bool {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	return mountedReadOnlyIn(string(data), path)
}

// mountedReadOnlyIn is mountedReadOnly against an already-read mount table.
func mountedReadOnlyIn(table, path string) bool {
	best, readOnly := "", false
	for _, line := range strings.Split(table, "\n") {
		// device mountpoint fstype options dump pass
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		point := unescapeMount(fields[1])
		if point != "/" && !strings.HasPrefix(path, point+"/") && path != point {
			continue
		}
		if len(point) < len(best) {
			continue
		}
		// Later entries win at equal length: a mount point can be mounted over,
		// and /proc/self/mounts lists the effective one last.
		best = point
		readOnly = false
		for _, opt := range strings.Split(fields[3], ",") {
			if opt == "ro" {
				readOnly = true
			}
		}
	}
	return readOnly
}

// unescapeMount decodes the octal escapes /proc/self/mounts uses for spaces
// and other awkward characters in a mount point.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

// whoami names the account SAND is running as, for a permission error that is
// really a question of which user the service is.
func whoami() string {
	uid := os.Getuid()
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return fmt.Sprintf("user %s (uid %d)", u.Username, uid)
	}
	return fmt.Sprintf("uid %d", uid)
}
