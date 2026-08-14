package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// browse asks the folder picker's endpoint for one listing.
func (c *testClient) browse(path string) (*httptest.ResponseRecorder, browseResponse) {
	c.t.Helper()

	w := c.do(http.MethodGet, "/api/system/folders?path="+url.QueryEscape(path), nil, "")
	var out browseResponse
	if w.Body.Len() > 0 {
		json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w, out
}

func (r browseResponse) names() []string {
	out := make([]string, 0, len(r.Folders))
	for _, folder := range r.Folders {
		out = append(out, folder.Name)
	}
	return out
}

func TestBrowseFoldersListsOnlyFolders(t *testing.T) {
	c := newTestClient(t)
	c.setup("browse-password", 1)

	base := t.TempDir()
	for _, name := range []string{"photos", "Archive", ".hidden"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}

	w, resp := c.browse(base)
	if w.Code != http.StatusOK {
		t.Fatalf("browse: %d %s", w.Code, w.Body.String())
	}
	if resp.Path != base || !resp.Exists {
		t.Fatalf("expected to list %s, got %s (exists=%v)", base, resp.Path, resp.Exists)
	}
	if resp.Parent != filepath.Dir(base) {
		t.Fatalf("parent = %q, want %q", resp.Parent, filepath.Dir(base))
	}

	// Case-insensitively sorted, files left out, hidden folders included but
	// marked so the picker can fold them away.
	got := resp.names()
	want := []string{".hidden", "Archive", "photos"}
	if len(got) != len(want) {
		t.Fatalf("folders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("folders = %v, want %v", got, want)
		}
	}
	for _, folder := range resp.Folders {
		if folder.Hidden != (folder.Name == ".hidden") {
			t.Fatalf("%s: hidden = %v", folder.Name, folder.Hidden)
		}
		if folder.Path != filepath.Join(base, folder.Name) {
			t.Fatalf("%s: path = %q", folder.Name, folder.Path)
		}
	}
}

// A path is allowed to name a folder that connecting will create, so asking
// for one opens the picker at the nearest folder that is really there.
func TestBrowseFoldersFallsBackToNearestExistingFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("browse-password", 1)

	base := t.TempDir()
	missing := filepath.Join(base, "not-yet", "sand")

	w, resp := c.browse(missing)
	if w.Code != http.StatusOK {
		t.Fatalf("browse: %d %s", w.Code, w.Body.String())
	}
	if resp.Exists {
		t.Fatal("expected exists=false for a folder that is not there")
	}
	if resp.Path != base {
		t.Fatalf("listed %s, want %s", resp.Path, base)
	}
	if resp.Requested != missing {
		t.Fatalf("requested = %q, want %q", resp.Requested, missing)
	}
}

func TestBrowseFoldersExpandsHomeAndDefaultsToIt(t *testing.T) {
	c := newTestClient(t)
	c.setup("browse-password", 1)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	if !readable(home) {
		t.Skip("home is not readable by this process")
	}

	w, resp := c.browse("")
	if w.Code != http.StatusOK {
		t.Fatalf("browse: %d %s", w.Code, w.Body.String())
	}
	if resp.Path != home {
		t.Fatalf("empty path listed %s, want %s", resp.Path, home)
	}

	if _, tilde := c.browse("~"); tilde.Path != home {
		t.Fatalf("~ listed %s, want %s", tilde.Path, home)
	}

	// Home is always offered as somewhere to jump to.
	var found bool
	for _, root := range resp.Roots {
		if root.Path == home {
			found = true
		}
	}
	if !found {
		t.Fatalf("home missing from roots %v", resp.Roots)
	}
	if resp.Separator != string(os.PathSeparator) {
		t.Fatalf("separator = %q", resp.Separator)
	}
}

// The service runs as a user with no home of its own, under a unit that sets
// ProtectHome=yes: $HOME resolves to /home and the sandbox refuses to open it.
// Opening the picker there strands it on a folder it cannot read and cannot
// leave, so an unreadable home is skipped rather than insisted on.
func TestBrowseStartSkipsAnUnreadableHome(t *testing.T) {
	blocked := unreadableDir(t)

	base := t.TempDir()
	vault := filepath.Join(base, "data", "vault.sand")
	if err := os.MkdirAll(filepath.Dir(vault), 0700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", blocked)
	// Windows and plan9 read other variables; the rest of the check is
	// platform-independent, so only assert where HOME is what os reads.
	if home, err := os.UserHomeDir(); err != nil || home != blocked {
		t.Skip("home directory is not taken from $HOME on this platform")
	}

	s := &Server{VaultPath: vault}
	if start := s.browseStart(); start != filepath.Dir(vault) {
		t.Fatalf("started at %s, want the vault's own folder %s", start, filepath.Dir(vault))
	}
}

// The same folder, walked into deliberately: the listing fails, but the reply
// still carries the parent and the roots, so the picker can go somewhere else
// instead of dead-ending on an empty list.
func TestBrowseFoldersReportsAnUnreadableFolderWithoutStranding(t *testing.T) {
	blocked := unreadableDir(t)

	c := newTestClient(t)
	c.setup("browse-password", 1)

	w, resp := c.browse(blocked)
	if w.Code != http.StatusOK {
		t.Fatalf("browse: %d %s", w.Code, w.Body.String())
	}
	if resp.Error == "" {
		t.Fatal("expected the listing to say why it failed")
	}
	if resp.Path != blocked || resp.Parent != filepath.Dir(blocked) {
		t.Fatalf("path = %q, parent = %q", resp.Path, resp.Parent)
	}
	if len(resp.Roots) == 0 {
		t.Fatal("no roots to escape to")
	}
}

// A shortcut that can only ever answer "permission denied" is worse than no
// shortcut, so an unreadable root is left out of the list.
func TestBrowseRootsSkipUnreadableFolders(t *testing.T) {
	blocked := unreadableDir(t)

	t.Setenv("HOME", blocked)
	if home, err := os.UserHomeDir(); err != nil || home != blocked {
		t.Skip("home directory is not taken from $HOME on this platform")
	}

	for _, root := range browseRoots() {
		if root.Path == blocked {
			t.Fatalf("unreadable folder offered as a root: %v", root)
		}
	}
}

// unreadableDir makes a folder this process cannot list, skipping the test
// where that is not possible — root ignores the mode bits entirely.
func unreadableDir(t *testing.T) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("directory modes do not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, which reads a folder whatever its mode says")
	}

	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	return dir
}

// A folder symlinked from elsewhere — how sync clients and mount helpers hand
// out folders — has to show up, or the picker hides what it exists to find.
func TestBrowseFoldersFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}

	c := newTestClient(t)
	c.setup("browse-password", 1)

	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "gone"), filepath.Join(base, "dangling")); err != nil {
		t.Fatal(err)
	}

	_, resp := c.browse(base)
	got := resp.names()
	if len(got) != 1 || got[0] != "linked" {
		t.Fatalf("folders = %v, want [linked]", got)
	}
}

// The endpoint sits behind the session like the rest of the API: a locked
// vault does not get to enumerate the machine's folders.
func TestBrowseFoldersRequiresSession(t *testing.T) {
	c := newTestClient(t)
	c.setup("browse-password", 1)

	w, _ := c.json(http.MethodPost, "/api/vault/lock", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}

	if w, _ := c.browse(t.TempDir()); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after locking, got %d", w.Code)
	}
}

func TestBrowseFoldersTruncatesHugeListings(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < maxBrowseFolders+5; i++ {
		if err := os.Mkdir(filepath.Join(base, "d"+string(rune('a'+i%26))+string(rune('a'+i/26))), 0700); err != nil {
			t.Fatal(err)
		}
	}

	folders, truncated, err := readFolders(base)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("expected the listing to be marked truncated")
	}
	if len(folders) != maxBrowseFolders {
		t.Fatalf("got %d folders, want %d", len(folders), maxBrowseFolders)
	}
}
