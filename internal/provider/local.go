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

func init() {
	Register(Spec{
		Kind:        KindLocal,
		Label:       "Local folder",
		Description: "A directory on this machine or a mounted network/removable drive. Useful as an offline third leg alongside two cloud accounts.",
		Order:       31,
		Fields: []FieldSpec{
			{
				Key:         "path",
				Label:       "Directory",
				Placeholder: "/mnt/backup/sand",
				Help:        "Created if it does not exist.",
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

// localProvider stores shards as files under a root directory.
type localProvider struct {
	base
	root string
}

// resolve maps an object key onto a path inside the root, refusing any key
// that would escape it.
func (p *localProvider) resolve(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash("/" + strings.TrimPrefix(key, "/")))
	full := filepath.Join(p.root, clean)
	if full != p.root && !strings.HasPrefix(full, p.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return full, nil
}

func (p *localProvider) Put(ctx context.Context, key string, data []byte) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	// Write to a temp file in the destination directory, then rename, so a
	// crash mid-write can never leave a half-written shard behind.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".sand-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing shard: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing shard: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("setting shard permissions: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("finalizing shard: %w", err)
	}
	return nil
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

func (p *localProvider) Delete(ctx context.Context, key string) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Prune the now-possibly-empty parent, ignoring failures.
	os.Remove(filepath.Dir(full))
	return nil
}

func (p *localProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
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

func (p *localProvider) Ping(ctx context.Context) error {
	if err := os.MkdirAll(p.root, 0700); err != nil {
		return fmt.Errorf("cannot use %s: %s%s", p.root, cause(err), hint(p.root, err))
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

// hint explains an unwritable directory in terms of the thing to go fix. The
// three ways a local folder usually fails — a sandboxed service, a folder
// owned by somebody else, a drive that is not mounted — are indistinguishable
// in the raw syscall error and have nothing in common as repairs. Returns a
// leading-space sentence, or "" when there is nothing useful to add.
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
		return " — the parent directory does not exist. If this is a removable or network" +
			" drive, it is probably not mounted."
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
