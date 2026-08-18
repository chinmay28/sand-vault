package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// iCloud Drive publishes no API anyone outside Apple's own sandbox can use, so
// SAND takes the folder instead — the one the system keeps in step with the
// account, exactly as it does for Proton Drive. Parts are encrypted long
// before they are handed over, so an account that syncs a folder is the same
// arrangement as an account that answers HTTP: a place holding one fragment
// and able to do nothing with it.
//
// What makes this more than the local folder backend is eviction. When the Mac
// runs short of disk, or the account holder picks "Remove Download", macOS
// takes the file's contents away and leaves a few hundred bytes of placeholder
// named ".<name>.icloud" in its place. The shard is still in iCloud — it is
// just no longer on this machine, under a name the vault never wrote. Left
// alone, that reads as a missing part: Get fails, Stat says gone, and List
// reports a key nobody stored. Every override in this file exists to keep the
// vault seeing the keys it wrote, and to ask iCloud for the bytes back when
// one of them is actually needed.
func init() {
	Register(Spec{
		Kind:  KindICloud,
		Label: "iCloud Drive",
		Description: "Your iCloud Drive folder, kept in step by the Mac (or the iCloud for " +
			"Windows client) SAND runs on. Apple publishes no API, so SAND writes its parts " +
			"into the synced folder. A part the system has evicted to save disk is fetched " +
			"back on demand.",
		DocsURL: "https://support.apple.com/en-us/HT204025",
		Order:   30,
		Fields: []FieldSpec{
			{
				Key:         "path",
				Label:       "iCloud Drive folder",
				Placeholder: icloudDefaultPath(),
				Default:     icloudDefaultPath(),
				Help: "A folder inside iCloud Drive. SAND creates it if it does not exist and " +
					"only ever writes encrypted parts.",
				Required:  true,
				Directory: true,
			},
		},
	}, newICloudProvider)
}

const (
	// icloudStubSuffix, with a leading dot on the name, is what macOS renames
	// a file to once it has taken its contents away.
	icloudStubSuffix = ".icloud"

	// icloudPollInterval is how often a materializing shard is looked for.
	// A download that has already finished shows up on the first pass; this
	// only paces the wait for one that has not.
	icloudPollInterval = 250 * time.Millisecond

	// icloudDownloadTimeout bounds that wait when the caller's context does not
	// bound it first. Generous on purpose: the ceiling is there to turn a dead
	// sync daemon into an error message rather than to hurry a slow link, and a
	// part of a large file over a domestic uplink can legitimately take minutes.
	icloudDownloadTimeout = 10 * time.Minute
)

// icloudProvider is the local folder backend taught about evicted files.
type icloudProvider struct {
	*localProvider

	// containers are the directories on this machine that iCloud Drive
	// actually syncs. Empty on a platform with no client, where the checks
	// they drive are skipped rather than guessed at.
	containers []string

	// download asks the system to bring an evicted file back. A field so the
	// tests can stand in for a sync daemon they cannot run.
	download func(ctx context.Context, path string) error

	poll    time.Duration
	timeout time.Duration
}

func newICloudProvider(cfg Config) (Provider, error) {
	p, err := newLocalProvider(cfg)
	if err != nil {
		return nil, err
	}
	local, ok := p.(*localProvider)
	if !ok {
		return nil, fmt.Errorf("iCloud Drive: unexpected folder backend %T", p)
	}
	return &icloudProvider{
		localProvider: local,
		containers:    icloudContainers(),
		download:      requestICloudDownload,
		poll:          icloudPollInterval,
		timeout:       icloudDownloadTimeout,
	}, nil
}

// stubFor returns the placeholder path macOS leaves behind for a file.
func stubFor(full string) string {
	return filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+icloudStubSuffix)
}

// icloudKey maps a name as it appears on disk back to the key the vault wrote,
// reporting whether that name was a placeholder rather than the file itself.
func icloudKey(key string) (string, bool) {
	dir, base := "", key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		dir, base = key[:i+1], key[i+1:]
	}
	// ".icloud" on its own names nothing, so it is somebody else's file rather
	// than a placeholder — hence the length test as well as the two affixes.
	if !strings.HasPrefix(base, ".") || !strings.HasSuffix(base, icloudStubSuffix) ||
		len(base) <= 1+len(icloudStubSuffix) {
		return key, false
	}
	return dir + base[1:len(base)-len(icloudStubSuffix)], true
}

func (p *icloudProvider) Put(ctx context.Context, key string, data []byte) error {
	if err := p.localProvider.Put(ctx, key, data); err != nil {
		return err
	}
	// Overwriting an evicted shard leaves its placeholder sitting beside the
	// new file until iCloud gets round to noticing. Clearing it now keeps the
	// folder describing one shard per key. Best effort: the bytes are already
	// safely written, and List survives a leftover either way.
	if full, err := p.resolve(key); err == nil {
		os.Remove(stubFor(full))
	}
	return nil
}

func (p *icloudProvider) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := p.localProvider.Get(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		return data, err
	}

	full, resolveErr := p.resolve(key)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if _, statErr := os.Stat(stubFor(full)); statErr != nil {
		// No file and no placeholder: the shard really is gone.
		return nil, ErrNotFound
	}
	if err := p.materialize(ctx, full); err != nil {
		return nil, err
	}
	return p.localProvider.Get(ctx, key)
}

func (p *icloudProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := p.localProvider.Stat(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		return info, err
	}

	full, resolveErr := p.resolve(key)
	if resolveErr != nil {
		return ObjectInfo{}, resolveErr
	}
	if _, statErr := os.Stat(stubFor(full)); statErr != nil {
		return ObjectInfo{}, ErrNotFound
	}
	// The part is present — in iCloud, which is where it was put. Its size is
	// the one thing a placeholder cannot answer: it weighs a couple of hundred
	// bytes of metadata, and reporting that as the part's size would be a
	// worse answer than none. A health check sees the part, unweighed, and Get
	// still fetches it.
	return ObjectInfo{Key: key}, nil
}

func (p *icloudProvider) Delete(ctx context.Context, key string) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	// The placeholder goes first. A deleted shard that leaves one behind comes
	// back from the dead in List, as a key Get can never satisfy.
	if err := os.Remove(stubFor(full)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return p.localProvider.Delete(ctx, key)
}

func (p *icloudProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	// Ask for everything and filter here: the walk matches on the name as it
	// is on disk, and an evicted shard's name on disk is not its key. Costs
	// nothing — the walk covers the whole root whatever prefix it is given.
	objects, err := p.localProvider.List(ctx, "")
	if err != nil {
		return nil, err
	}

	at := make(map[string]int, len(objects))
	var out []ObjectInfo
	for _, obj := range objects {
		key, evicted := icloudKey(obj.Key)
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		obj.Key = key
		if evicted {
			obj.Size = 0
		}
		// A file can be on disk with a stale placeholder still beside it. That
		// is one shard, and the copy that is actually here is the one to
		// describe.
		if i, seen := at[key]; seen {
			if !evicted {
				out[i] = obj
			}
			continue
		}
		at[key] = len(out)
		out = append(out, obj)
	}
	return out, nil
}

func (p *icloudProvider) Ping(ctx context.Context) error {
	if err := p.checkContainer(); err != nil {
		return err
	}
	return p.localProvider.Ping(ctx)
}

// checkContainer refuses a folder that iCloud Drive does not sync. A writable
// directory is not the test here: ~/Documents is writable and would take every
// shard SAND gave it, up to the day the Mac died with all of them still on it.
// Skipped where this machine has no iCloud client at all, since there is then
// nothing to compare against and the backend is just a folder.
func (p *icloudProvider) checkContainer() error {
	if len(p.containers) == 0 {
		return nil
	}
	// Both sides get resolved. A Mac's home directory is reached through a
	// firmlink, so /Users/me/… and /System/Volumes/Data/Users/me/… are the
	// same folder, and comparing one spelling against the other would refuse a
	// folder that is squarely inside iCloud Drive.
	resolved := make([]string, len(p.containers))
	for i, container := range p.containers {
		resolved[i] = resolveSymlinks(container)
	}
	if underAny(resolved, resolveSymlinks(p.root)) {
		return nil
	}
	if missing := missingDirs(p.containers); len(missing) == len(p.containers) {
		return fmt.Errorf("iCloud Drive is not set up on this machine — %s appears once iCloud "+
			"Drive is turned on (System Settings → your name → iCloud → iCloud Drive)",
			p.containers[0])
	}
	return fmt.Errorf("%s is not inside iCloud Drive, so nothing written there would ever leave "+
		"this machine — pick a folder under %s, or connect it as a local folder instead",
		p.root, strings.Join(p.containers, " or "))
}

// materialize asks iCloud for an evicted file and waits for it to land.
func (p *icloudProvider) materialize(ctx context.Context, full string) error {
	if err := p.download(ctx, full); err != nil {
		return err
	}

	deadline := time.Now().Add(p.timeout)
	for {
		if _, err := os.Stat(full); err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("iCloud has this part but has not handed it back after %s — it was "+
				"evicted from this machine to save disk, and the download has not finished. Check "+
				"that iCloud Drive is signed in and online, then try again", p.timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.poll):
		}
	}
}

// requestICloudDownload asks the system to bring back a file whose contents
// have been evicted. On a Mac that is brctl, the command line onto the same
// daemon Finder's download arrow talks to. Everywhere else there is nothing to
// ask: iCloud for Windows hydrates a placeholder on open, so a Windows machine
// never reaches this path, and no other platform has a client at all.
func requestICloudDownload(ctx context.Context, path string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("%s is an iCloud placeholder and this is not a Mac — only macOS can ask "+
			"iCloud to download one. Open the folder on the machine that syncs it, or connect an "+
			"account SAND can talk to directly", filepath.Base(path))
	}

	out, err := exec.CommandContext(ctx, "brctl", "download", path).CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("this Mac has no brctl, which is how a file evicted from iCloud Drive is "+
			"asked for — download %s in Finder and try again", filepath.Base(path))
	}
	if detail := strings.TrimSpace(string(out)); detail != "" {
		return fmt.Errorf("asking iCloud for %s: %s", filepath.Base(path), detail)
	}
	return fmt.Errorf("asking iCloud for %s: %w", filepath.Base(path), err)
}

// icloudContainers returns the directories iCloud Drive syncs on this machine.
// macOS keeps everything under one path that cannot be moved; iCloud for
// Windows offers two spellings depending on the client's vintage.
func icloudContainers() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Mobile Documents")}
	case "windows":
		return []string{
			filepath.Join(home, "iCloudDrive"),
			filepath.Join(home, "iCloud Drive"),
		}
	default:
		return nil
	}
}

// icloudDefaultPath pre-fills the folder field with the place a part should
// go, so the common setup is a button rather than a path typed from memory.
func icloudDefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")}
	case "windows":
		candidates = []string{
			filepath.Join(home, "iCloudDrive"),
			filepath.Join(home, "iCloud Drive"),
		}
	default:
		// No client on this platform. The field still has to say something,
		// and the Mac path is the one that explains what is being asked for.
		candidates = []string{filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")}
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Join(candidate, "sand")
		}
	}
	return filepath.Join(candidates[0], "sand")
}

// resolveSymlinks follows a path to what it really is, so a folder reached
// through a link into iCloud Drive is recognised as being in it. A folder that
// does not exist yet is the normal case here — it is about to be created — so
// the walk climbs to the deepest ancestor that does exist, resolves that, and
// puts the rest back on the end.
func resolveSymlinks(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// underAny reports whether path is one of the roots or sits inside one.
func underAny(roots []string, path string) bool {
	clean := filepath.Clean(path)
	for _, root := range roots {
		root = filepath.Clean(root)
		if clean == root || strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// missingDirs returns the roots that are not directories on this machine.
func missingDirs(roots []string) []string {
	var out []string
	for _, root := range roots {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			out = append(out, root)
		}
	}
	return out
}
