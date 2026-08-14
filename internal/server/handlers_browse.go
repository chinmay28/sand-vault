package server

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Walking this machine's folders, so the backends that take a path — a local
// folder, a Proton Drive sync folder — can be pointed at one instead of having
// it typed in from memory.
//
// What this browses is the filesystem SAND runs on, not the vault, so it is
// deliberately thin: it answers with folder names and nothing else, never a
// file, never its contents. It is behind a session for the same reason the
// rest of the API is, and an unlocked session could already aim an account at
// any folder on the machine by typing its path.

// maxBrowseFolders caps one listing. A folder with more subfolders than this
// is not one anybody is going to find a drive in by scrolling, and the picker
// says so rather than pretending the list is complete.
const maxBrowseFolders = 500

type browseFolder struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Hidden bool   `json:"hidden,omitempty"`
}

// browseRoot is somewhere worth starting from: home, and the roots removable
// disks and network shares get mounted under.
type browseRoot struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type browseResponse struct {
	// Requested is the path that was asked for; Path is the one actually
	// listed. They differ when the requested folder is not there — the field
	// is allowed to name a folder that connecting will create, and the picker
	// still has to open somewhere.
	Requested string `json:"requested"`
	Path      string `json:"path"`
	Parent    string `json:"parent,omitempty"`
	Exists    bool   `json:"exists"`

	// Separator is this machine's, not the browser's: the page joining a new
	// subfolder onto a Windows path has no other way to know.
	Separator string `json:"separator"`

	Folders   []browseFolder `json:"folders"`
	Roots     []browseRoot   `json:"roots"`
	Truncated bool           `json:"truncated"`

	// Error is why this folder could not be listed. A folder SAND cannot read
	// is a normal thing to walk into — the sandbox hides /home, a removable
	// drive belongs to somebody else — so it comes back as a listing that
	// failed rather than as a failed request: the picker still gets its
	// parent and its roots, and can go somewhere else.
	Error string `json:"error,omitempty"`
}

func (s *Server) handleSystemFolders(w http.ResponseWriter, r *http.Request) {
	requested := provider.ExpandHome(r.URL.Query().Get("path"))
	if requested == "" {
		requested = s.browseStart()
	}

	abs, err := filepath.Abs(requested)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	listed := nearestExistingDir(abs)
	parent := filepath.Dir(listed)
	if parent == listed {
		parent = ""
	}

	resp := browseResponse{
		Requested: abs,
		Path:      listed,
		Parent:    parent,
		Exists:    listed == abs,
		Separator: string(os.PathSeparator),
		Roots:     browseRoots(),
	}

	folders, truncated, err := readFolders(listed)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Folders, resp.Truncated = folders, truncated
	}
	writeJSON(w, http.StatusOK, resp)
}

// browseStart is where the picker opens when nothing has been typed yet: the
// first of these this process can actually read.
//
// Home is the obvious answer and the wrong one to insist on. Under the
// systemd unit both installers write, SAND runs as a user with no home of its
// own and `ProtectHome=yes` in force, so `$HOME` resolves to `/home` and the
// sandbox refuses to open it — which used to strand the picker on an empty
// folder it could not leave. The vault's own directory is always readable by
// definition, and the mount roots are where a drive worth scattering onto
// actually turns up.
func (s *Server) browseStart() string {
	candidates := make([]string, 0, 8)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, home)
	}
	candidates = append(candidates, s.vaultDir())
	candidates = append(candidates, provider.MountRoots()...)
	if wd, err := os.Getwd(); err == nil && wd != "" {
		candidates = append(candidates, wd)
	}
	candidates = append(candidates, string(os.PathSeparator))

	if start := firstReadableDir(candidates); start != "" {
		return start
	}
	return string(os.PathSeparator)
}

// vaultDir is the folder holding the vault file — /var/lib/sand under the
// service, ~/.sand on a desktop.
func (s *Server) vaultDir() string {
	path := s.VaultPath
	if path == "" {
		path = DefaultVaultPath()
	}
	return filepath.Dir(path)
}

func firstReadableDir(candidates []string) string {
	for _, dir := range candidates {
		if dir != "" && readable(dir) {
			return dir
		}
	}
	return ""
}

// readable reports whether this process can list a folder, which is not the
// same question as whether the folder is there: a sandbox, or a drive mounted
// by somebody else's desktop session, answers yes to one and no to the other.
// Only the first entry is read, so the check costs nothing on a huge folder.
func readable(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return true
}

// nearestExistingDir walks up from path until it reaches a folder that is
// really there. A path pointing at a drive that is not mounted, or at the
// subfolder connecting is about to create, still opens the picker somewhere
// useful — its nearest existing ancestor — rather than on an error.
func nearestExistingDir(path string) string {
	for {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func readFolders(dir string) ([]browseFolder, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}

	out := make([]browseFolder, 0, len(entries))
	for _, entry := range entries {
		if !isFolder(dir, entry) {
			continue
		}
		name := entry.Name()
		out = append(out, browseFolder{
			Name:   name,
			Path:   filepath.Join(dir, name),
			Hidden: strings.HasPrefix(name, "."),
		})
	}

	// Case-insensitively, which is the order a file manager shows and the one
	// somebody scanning for "Proton Drive" is reading in.
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})

	if len(out) > maxBrowseFolders {
		return out[:maxBrowseFolders], true, nil
	}
	return out, false, nil
}

// isFolder reports whether entry is a folder, following a symlink to find out.
// Sync clients and mount helpers hand out links constantly — ~/Proton Drive is
// often one — and skipping them would hide exactly the folders this exists to
// find. A dangling link stats as nothing and drops out.
func isFolder(dir string, entry fs.DirEntry) bool {
	if entry.IsDir() {
		return true
	}
	if entry.Type()&fs.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, entry.Name()))
	return err == nil && info.IsDir()
}

// browseRoots lists the places worth jumping to directly, skipping any this
// machine does not have and any this process cannot open. A shortcut that
// only ever answers "permission denied" — /home under the service's sandbox —
// is worse than no shortcut.
func browseRoots() []browseRoot {
	var out []browseRoot
	add := func(label, path string) {
		if path == "" {
			return
		}
		for _, existing := range out {
			if existing.Path == path {
				return
			}
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return
		}
		if !readable(path) {
			return
		}
		out = append(out, browseRoot{Label: label, Path: path})
	}

	if home, err := os.UserHomeDir(); err == nil {
		add("Home", home)
	}

	if runtime.GOOS == "windows" {
		// Drive letters, which is what "somewhere else on this machine" means
		// there. A: and B: are floppies nobody has and stat slowly.
		for letter := 'C'; letter <= 'Z'; letter++ {
			drive := string(letter) + `:\`
			add(drive, drive)
		}
		return out
	}

	add("/", "/")
	for _, root := range provider.MountRoots() {
		add(root, root)
	}
	return out
}
