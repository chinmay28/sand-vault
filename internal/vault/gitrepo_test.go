package vault

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The repositories these tests mirror are served over loopback HTTP rather than
// reached by path, and that is not incidental: ParseRemote deliberately refuses
// a local path, because a server that will mirror one on a schedule is a server
// that can be pointed at anything it can read. Serving the bare repository as
// static files — which is all "dumb HTTP" is — gives the tests a URL of exactly
// the kind a real one would have, with no exception carved into the rule.

// remoteRepo is a bare repository published over HTTP, that a test can add
// commits to.
type remoteRepo struct {
	t    *testing.T
	bare string
	URL  string
}

// newRemoteRepo publishes a repository with one commit and a tag.
func newRemoteRepo(t *testing.T) *remoteRepo {
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

	r := &remoteRepo{t: t, bare: bare}
	r.git(root, "init", "--quiet", "--bare", bare)

	work := filepath.Join(root, "work")
	r.git(root, "clone", "--quiet", bare, work)
	r.write(work, "README.md", "the first thing\n")
	r.git(work, "add", ".")
	r.git(work, "commit", "--quiet", "-m", "first")
	r.git(work, "tag", "v1")
	r.git(work, "push", "--quiet", "origin", "HEAD", "--tags")
	r.publish()

	srv := httptest.NewServer(http.FileServer(http.Dir(served)))
	t.Cleanup(srv.Close)
	r.URL = srv.URL + "/project.git"
	return r
}

// Commit adds one commit upstream, the way a project moving on looks from here.
func (r *remoteRepo) Commit(message string) {
	r.t.Helper()
	root := r.t.TempDir()
	work := filepath.Join(root, "w")
	r.git(root, "clone", "--quiet", r.bare, work)
	r.write(work, message+".txt", message+"\n")
	r.git(work, "add", ".")
	r.git(work, "commit", "--quiet", "-m", message)
	r.git(work, "push", "--quiet", "origin", "HEAD")
	r.publish()
}

// publish refreshes the files a dumb HTTP client reads to find out what is
// there. A real server does this in a hook; here it is done by hand.
func (r *remoteRepo) publish() {
	r.t.Helper()
	r.git(r.bare, "update-server-info")
}

func (r *remoteRepo) write(dir, name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *remoteRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// The claim the whole design rests on: what comes out of the vault is a
// repository, cloneable by a plain git with no SAND anywhere near it.
func TestTrackRepoStoresACloneableBundle(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	if repo.Name != "project.bundle" {
		t.Errorf("stored as %q, want project.bundle", repo.Name)
	}
	if repo.Digest == "" || repo.Refs == 0 {
		t.Errorf("nothing recorded about what the bundle holds: %+v", repo.GitSource)
	}

	// Take it back out and clone it, exactly as somebody would in ten years.
	dir := t.TempDir()
	bundle := filepath.Join(dir, "out.bundle")
	if err := v.exportFile(context.Background(), repo.ID, bundle); err != nil {
		t.Fatalf("exportFile: %v", err)
	}
	remote.git(dir, "clone", "--quiet", bundle, filepath.Join(dir, "restored"))

	restored := filepath.Join(dir, "restored")
	if tags := remote.git(restored, "tag"); !strings.Contains(tags, "v1") {
		t.Errorf("the restored clone has no tags: %q", tags)
	}
	if log := remote.git(restored, "log", "--oneline"); !strings.Contains(log, "first") {
		t.Errorf("the restored clone has no history: %q", log)
	}
}

// The efficiency claim, and the one worth a test of its own: a repository that
// has not moved costs one ref advertisement and nothing else. No fetch, no
// upload, no re-encryption — and the stored bundle is not touched, which is
// what FetchedAt staying put proves.
func TestRefreshDoesNothingWhenUpstreamHasNotMoved(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}

	after, moved, err := v.RefreshRepo(context.Background(), MainScope, repo.ID)
	if err != nil {
		t.Fatalf("RefreshRepo: %v", err)
	}
	if moved {
		t.Error("a repository that has not moved reported as updated")
	}
	if !after.FetchedAt.Equal(repo.FetchedAt) {
		t.Errorf("the bundle was rewritten for nothing: fetched at %s, was %s",
			after.FetchedAt, repo.FetchedAt)
	}
	if !after.CheckedAt.After(repo.CheckedAt) {
		t.Errorf("the check was not recorded: %s is not after %s", after.CheckedAt, repo.CheckedAt)
	}
	if after.Digest != repo.Digest {
		t.Errorf("digest moved without the repository moving")
	}
}

func TestRefreshPicksUpANewCommit(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	remote.Commit("second")

	after, moved, err := v.RefreshRepo(context.Background(), MainScope, repo.ID)
	if err != nil {
		t.Fatalf("RefreshRepo: %v", err)
	}
	if !moved {
		t.Fatal("a repository that gained a commit reported as unchanged")
	}
	if after.Digest == repo.Digest {
		t.Error("the digest did not move with the repository")
	}
	if after.Commits <= repo.Commits {
		t.Errorf("commits = %d, want more than the %d stored before", after.Commits, repo.Commits)
	}

	// And the commit is really in the stored bundle, not just in the record.
	dir := t.TempDir()
	bundle := filepath.Join(dir, "out.bundle")
	if err := v.exportFile(context.Background(), after.ID, bundle); err != nil {
		t.Fatalf("exportFile: %v", err)
	}
	remote.git(dir, "clone", "--quiet", bundle, filepath.Join(dir, "restored"))
	if log := remote.git(filepath.Join(dir, "restored"), "log", "--oneline"); !strings.Contains(log, "second") {
		t.Errorf("the new commit is not in the stored bundle: %q", log)
	}
}

// A refresh replaces the repository rather than filing a second copy beside it.
// A folder that grew project.bundle, project (2).bundle every time somebody
// pushed would be a folder nobody could use.
func TestRefreshOverwritesRatherThanAccumulating(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	if _, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{}); err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	remote.Commit("second")

	repos, err := v.TrackedRepos(MainScope, "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.RefreshRepo(context.Background(), MainScope, repos[0].ID); err != nil {
		t.Fatalf("RefreshRepo: %v", err)
	}

	after, err := v.TrackedRepos(MainScope, "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("after a refresh there are %d repositories, want 1: %+v", len(after), after)
	}
	if after[0].Name != "project.bundle" {
		t.Errorf("the refreshed repository is called %q", after[0].Name)
	}
}

// The scheduled sweep, end to end: nothing has moved, so every repository is
// reported current and nothing is fetched.
func TestGitSweepReportsEverythingCurrent(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	if _, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{}); err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}

	policy := dailyAt("04:00", ActionPull)
	policy.Task = TaskGit
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Git == nil {
		t.Fatalf("a git policy produced no git result: %+v", run)
	}
	if run.Git.Repos != 1 || run.Git.Current != 1 || run.Git.Updated != 0 {
		t.Errorf("git result = %+v, want one repository, current", run.Git)
	}
	if !run.Clean() {
		t.Errorf("a sweep that found everything current is not clean: %+v", run)
	}
	// The storage counters have no business being filled in by a git policy.
	if run.Shards != nil {
		t.Errorf("a git policy produced a shard result: %+v", run.Shards)
	}
}

func TestGitSweepFetchesWhatMoved(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	if _, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{}); err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	remote.Commit("second")

	policy := dailyAt("04:00", ActionPull)
	policy.Task = TaskGit
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Git.Updated != 1 {
		t.Fatalf("git result = %+v, want the one repository updated", run.Git)
	}
	if run.Git.Commits < 1 {
		t.Errorf("commits = %d, want the one that arrived", run.Git.Commits)
	}
	if run.Git.Failed != 0 || run.Git.Deferred != 0 {
		t.Errorf("git result = %+v, want nothing failed or deferred", run.Git)
	}

	// Running it again finds nothing left to do, which is what "up to date"
	// means and what makes the schedule cheap from the second week on.
	again, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if again.Git.Current != 1 || again.Git.Updated != 0 {
		t.Errorf("second sweep = %+v, want everything current", again.Git)
	}
}

// A check-only policy says what it found and fetches nothing, for either task.
func TestGitCheckOnlyPolicyFetchesNothing(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	remote.Commit("second")

	policy := dailyAt("04:00", ActionCheck)
	policy.Task = TaskGit
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Git.Updated != 0 {
		t.Errorf("a check-only policy fetched something: %+v", run.Git)
	}
	if run.Git.Deferred != 1 {
		t.Errorf("git result = %+v, want the moved repository counted as left alone", run.Git)
	}

	after, err := v.TrackedRepo(MainScope, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Digest != repo.Digest {
		t.Error("a check-only policy rewrote what it stored")
	}
}

// An upstream that has stopped answering is never by itself a reason to delete
// anything: an outage, a rename and a deletion look identical from here, and
// the stored copy may be the last one in the world.
func TestUpstreamGoneKeepsTheStoredCopy(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}

	// The server goes away, which is what every kind of "gone" looks like.
	remote.URL = "http://127.0.0.1:1/project.git"
	if err := v.setGitSource(MainScope, repo.ID, &GitSource{
		URL: remote.URL, Digest: repo.Digest, Refs: repo.Refs, FetchedAt: repo.FetchedAt,
	}); err != nil {
		t.Fatal(err)
	}

	policy := dailyAt("04:00", ActionPull)
	policy.Task = TaskGit
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Git.Gone != 1 {
		t.Errorf("git result = %+v, want the unreachable upstream counted", run.Git)
	}
	if run.Git.Pruned != 0 {
		t.Fatalf("a policy that was not told to prune deleted something: %+v", run.Git)
	}
	if run.Clean() {
		t.Error("a sweep whose upstream has vanished reported as clean")
	}

	// The file is still there, which is the whole point.
	after, err := v.TrackedRepo(MainScope, repo.ID)
	if err != nil {
		t.Fatalf("the stored repository was lost: %v", err)
	}
	if !after.Gone || after.Reason == "" {
		t.Errorf("the record does not say the upstream has gone: %+v", after.GitSource)
	}
}

func TestPruneDeletesOnlyWhenAskedTo(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	if err := v.setGitSource(MainScope, repo.ID, &GitSource{
		URL: "http://127.0.0.1:1/project.git", Digest: repo.Digest, FetchedAt: repo.FetchedAt,
	}); err != nil {
		t.Fatal(err)
	}

	policy := dailyAt("04:00", ActionPull)
	policy.Task = TaskGit
	policy.Git = &GitPolicy{Prune: true}
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Git.Pruned != 1 {
		t.Fatalf("git result = %+v, want the gone repository pruned", run.Git)
	}
	if _, err := v.TrackedRepo(MainScope, repo.ID); err == nil {
		t.Error("the pruned repository is still in the index")
	}
}

// A bound on the fetching is not a bound on the asking: every repository is
// still asked whether it has moved, because that is the cheap half.
func TestMaxReposBoundsTheFetchingNotTheAsking(t *testing.T) {
	v, _ := newTestVault(t, 3)
	first, second := newRemoteRepo(t), newRemoteRepo(t)

	for _, remote := range []*remoteRepo{first, second} {
		if _, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{}); err != nil {
			t.Fatalf("TrackRepo: %v", err)
		}
		remote.Commit("second")
	}

	policy := dailyAt("04:00", ActionPull)
	policy.Task = TaskGit
	policy.Git = &GitPolicy{MaxRepos: 1}
	if _, err := v.SetAutomation(MainScope, "/", policy); err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}

	run, err := v.RunAutomation(context.Background(), MainScope, "/")
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if run.Git.Checked != 2 {
		t.Errorf("checked = %d, want both asked — the bound is on fetching", run.Git.Checked)
	}
	if run.Git.Updated != 1 || run.Git.Deferred != 1 {
		t.Errorf("git result = %+v, want one fetched and one left for next time", run.Git)
	}
}

func TestGitPolicyRefusesAnActionTheTaskCannotDo(t *testing.T) {
	v, _ := newTestVault(t, 3)

	policy := dailyAt("04:00", ActionRebalance)
	policy.Task = TaskGit
	if _, err := v.SetAutomation(MainScope, "/", policy); err == nil {
		t.Fatal("a git policy accepted an action belonging to the storage task")
	}

	shards := dailyAt("04:00", ActionPull)
	if _, err := v.SetAutomation(MainScope, "/", shards); err == nil {
		t.Fatal("a storage policy accepted an action belonging to the git task")
	}
}

// Switching a policy from one task to the other must not leave the settings of
// the job it no longer does lying around to surprise somebody who switches back.
func TestSwitchingTaskDropsTheOtherTasksSettings(t *testing.T) {
	v, _ := newTestVault(t, 3)

	shards := dailyAt("04:00", ActionRebalance)
	shards.Shards = &ShardPolicy{MaxRepairs: 5, Narrow: true}
	shards.Git = &GitPolicy{Prune: true}
	stored, err := v.SetAutomation(MainScope, "/", shards)
	if err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	if stored.Git != nil {
		t.Errorf("a storage policy kept the git settings: %+v", stored.Git)
	}
	if stored.Shards == nil || stored.Shards.MaxRepairs != 5 {
		t.Errorf("a storage policy lost its own settings: %+v", stored.Shards)
	}

	toGit := dailyAt("04:00", ActionPull)
	toGit.Task = TaskGit
	toGit.Git = &GitPolicy{MaxRepos: 2}
	stored, err = v.SetAutomation(MainScope, "/", toGit)
	if err != nil {
		t.Fatalf("SetAutomation: %v", err)
	}
	if stored.Shards != nil {
		t.Errorf("a git policy kept the storage settings: %+v", stored.Shards)
	}
	if stored.Git == nil || stored.Git.MaxRepos != 2 {
		t.Errorf("a git policy lost its own settings: %+v", stored.Git)
	}
}

// Deleting the file has to forget it was a repository, or the index keeps a
// record pointing at nothing.
func TestDeletingABundleForgetsItIsARepository(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	if _, err := v.Delete(context.Background(), repo.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	v.mu.RLock()
	left := len(v.manifest.Repos)
	v.mu.RUnlock()
	if left != 0 {
		t.Errorf("%d repository record(s) left behind by the delete", left)
	}
}

// Untracking stops SAND asking the upstream anything and leaves the file alone:
// a bundle is a perfectly good file, and a complete repository besides.
func TestUntrackKeepsTheFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	remote := newRemoteRepo(t)

	repo, _, err := v.TrackRepo(context.Background(), MainScope, "/", remote.URL, UploadOptions{})
	if err != nil {
		t.Fatalf("TrackRepo: %v", err)
	}
	if err := v.UntrackRepo(MainScope, repo.ID); err != nil {
		t.Fatalf("UntrackRepo: %v", err)
	}

	if _, err := v.TrackedRepo(MainScope, repo.ID); err == nil {
		t.Error("still tracked after being untracked")
	}
	v.mu.RLock()
	entry := v.manifest.ByID(repo.ID)
	v.mu.RUnlock()
	if entry == nil {
		t.Error("untracking deleted the file")
	}
}

func TestTrackRepoRefusesAUrlThatWouldRunACommand(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if _, _, err := v.TrackRepo(context.Background(), MainScope, "/",
		`ext::sh -c "touch /tmp/pwned"`, UploadOptions{}); err == nil {
		t.Fatal("TrackRepo accepted a URL that runs a command")
	}
	if _, _, err := v.TrackRepo(context.Background(), MainScope, "/",
		"/etc", UploadOptions{}); err == nil {
		t.Fatal("TrackRepo accepted a local path")
	}
}

func TestRepoNameReadsTheLastSegment(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/chinmay28/sand-vault.git": "sand-vault.bundle",
		"https://github.com/chinmay28/sand-vault":     "sand-vault.bundle",
		"https://example.com/deep/path/thing.git/":    "thing.bundle",
		"git@github.com:chinmay28/sand-vault.git":     "sand-vault.bundle",
		"ssh://git@host/~user/repo.git":               "repo.bundle",
	} {
		if got := RepoName(url); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", url, got, want)
		}
	}
}
