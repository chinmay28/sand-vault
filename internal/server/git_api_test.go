package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The repositories these tests mirror are served over loopback HTTP rather than
// reached by path, because the vault refuses a local path on purpose: a server
// that will mirror one on a schedule is a server that can be pointed at
// anything it can read. Serving a bare repository as static files — which is
// all "dumb HTTP" is — gives the tests a URL of the kind a real one has.

// servedRepo is a bare repository published over HTTP for a test to mirror.
type servedRepo struct {
	t    *testing.T
	bare string
	URL  string
}

func newServedRepo(t *testing.T) *servedRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}

	root := t.TempDir()
	served := filepath.Join(root, "served")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(served, "project.git")

	r := &servedRepo{t: t, bare: bare}
	r.git(root, "init", "--quiet", "--bare", bare)

	work := filepath.Join(root, "work")
	r.git(root, "clone", "--quiet", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.git(work, "add", ".")
	r.git(work, "commit", "--quiet", "-m", "first")
	r.git(work, "push", "--quiet", "origin", "HEAD")
	r.git(bare, "update-server-info")

	srv := httptest.NewServer(http.FileServer(http.Dir(served)))
	t.Cleanup(srv.Close)
	r.URL = srv.URL + "/project.git"
	return r
}

// Commit moves the upstream on by one.
func (r *servedRepo) Commit(name string) {
	r.t.Helper()
	root := r.t.TempDir()
	work := filepath.Join(root, "w")
	r.git(root, "clone", "--quiet", r.bare, work)
	if err := os.WriteFile(filepath.Join(work, name+".txt"), []byte(name), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git(work, "add", ".")
	r.git(work, "commit", "--quiet", "-m", name)
	r.git(work, "push", "--quiet", "origin", "HEAD")
	r.git(r.bare, "update-server-info")
}

func (r *servedRepo) git(dir string, args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// track stores a repository through the endpoint the browser uses.
func (c *testClient) track(url, path string) map[string]any {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/git/track", map[string]any{
		"url": url, "path": path,
	})
	if w.Code != http.StatusOK {
		c.t.Fatalf("POST /api/git/track: %d %s", w.Code, w.Body.String())
	}
	repo, ok := body["repo"].(map[string]any)
	if !ok {
		c.t.Fatalf("no repo in %v", body)
	}
	return repo
}

func TestGitTrackStoresARepositoryAndListsIt(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	remote := newServedRepo(t)

	repo := c.track(remote.URL, "/")
	if repo["name"] != "project.bundle" {
		t.Errorf("stored as %v, want project.bundle", repo["name"])
	}
	if repo["url"] != remote.URL {
		t.Errorf("url = %v, want %s", repo["url"], remote.URL)
	}

	_, body := c.json(http.MethodGet, "/api/git?path=/", nil)
	repos, _ := body["repos"].([]any)
	if len(repos) != 1 {
		t.Fatalf("listing has %d repositories, want 1: %v", len(repos), body)
	}
	if available, _ := body["available"].(bool); !available {
		t.Error("the listing says this machine has no git, but one was just used")
	}

	// And the folder listing carries the count, which is what lights the button
	// without a request per folder.
	_, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	if got := number(t, body, "repos"); got != 1 {
		t.Errorf("listing repos = %d, want 1", got)
	}
}

func TestGitRefreshReportsWhetherAnythingMoved(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	remote := newServedRepo(t)

	repo := c.track(remote.URL, "/")
	id := repo["id"].(string)

	// Nothing has moved: the cheap path, and it says so.
	w, body := c.json(http.MethodPost, "/api/git/"+id+"/refresh", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	if updated, _ := body["updated"].(bool); updated {
		t.Error("a repository that has not moved reported as updated")
	}

	remote.Commit("second")

	w, body = c.json(http.MethodPost, "/api/git/"+id+"/refresh", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	if updated, _ := body["updated"].(bool); !updated {
		t.Fatalf("a repository that gained a commit reported as unchanged: %v", body)
	}
	// The refresh stores a new file over the old one, so the answer has to carry
	// the identity the caller should hold from now on.
	after := body["repo"].(map[string]any)
	if after["id"] == nil || after["id"] == "" {
		t.Errorf("refresh gave back no id: %v", after)
	}
}

// A git policy's run reports in the counters of the job that produced it.
func TestGitAutomationRunReportsRepositories(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	remote := newServedRepo(t)
	c.track(remote.URL, "/")
	remote.Commit("second")

	c.setAutomation(map[string]any{
		"path": "/", "enabled": true, "cadence": "weekly", "at": "04:00",
		"weekday": 0, "task": "git", "action": "pull",
	})

	w, body := c.json(http.MethodPost, "/api/automation/run", map[string]any{"path": "/"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/automation/run: %d %s", w.Code, w.Body.String())
	}
	run := body["run"].(map[string]any)
	if run["task"] != "git" {
		t.Errorf("run task = %v, want git", run["task"])
	}

	result, ok := run["git"].(map[string]any)
	if !ok {
		t.Fatalf("a git policy produced no git result: %v", run)
	}
	if got := number(t, result, "updated"); got != 1 {
		t.Errorf("updated = %d, want the one repository fetched: %v", got, result)
	}
	if _, ok := run["shards"]; ok {
		t.Errorf("a git policy also produced a shard result: %v", run["shards"])
	}
}

func TestGitTrackRefusesAUrlThatWouldRunACommand(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	for _, url := range []string{
		`ext::sh -c "touch /tmp/pwned"`,
		"/etc/passwd",
		"file:///etc",
	} {
		w, _ := c.json(http.MethodPost, "/api/git/track", map[string]any{"url": url, "path": "/"})
		if w.Code == http.StatusOK {
			t.Errorf("POST /api/git/track accepted %q", url)
		}
	}
}

func TestGitUntrackKeepsTheFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	remote := newServedRepo(t)

	repo := c.track(remote.URL, "/")
	id := repo["id"].(string)

	w, _ := c.json(http.MethodDelete, "/api/git/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /api/git/{id}: %d %s", w.Code, w.Body.String())
	}

	_, body := c.json(http.MethodGet, "/api/git?path=/", nil)
	if repos, _ := body["repos"].([]any); len(repos) != 0 {
		t.Errorf("still tracked after untracking: %v", repos)
	}

	// The bundle itself is a complete repository and a perfectly good file, so
	// it stays.
	_, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Errorf("untracking removed the file: %v", files)
	}
}
