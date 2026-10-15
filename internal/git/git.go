// Package git is the small part of git SAND actually needs: ask a remote what
// its refs are, keep a bare mirror of one, and turn that mirror into a single
// file.
//
// It shells out to the git on the machine rather than speaking the protocol
// itself, and that is a deliberate trade rather than a shortcut. The hard part
// of talking to somebody's repositories is not the wire format, it is the
// credentials — an SSH key with a passphrase held in an agent, a credential
// helper on the keychain, a token in ~/.gitconfig, a host alias in
// ~/.ssh/config, a corporate CA. All of that is configuration the user has
// already done for the git they use every day, and a second implementation
// inside SAND would either reimplement it badly or ignore it and be unable to
// reach a single private repository. Borrowing the real git means a repository
// SAND can see is exactly a repository the user can see from the same machine,
// which is a rule that needs no documentation.
//
// The cost is that git becomes a runtime dependency for this one feature, and
// the cost is paid honestly: Available reports whether it is there, and a
// policy that needs it says so plainly instead of failing at three in the
// morning.
//
// # What is hardened here, and why
//
// A remote URL is a string the user typed, and a git URL is not an inert
// address. The `ext::` transport runs its argument as a shell command, so
// `ext::sh -c …` in a repository URL is arbitrary code execution as whoever
// runs the server; a URL beginning with a dash is an argument rather than an
// address. Both are shut off twice over — once by ParseRemote, and once by the
// -c flags carried by every invocation — because the second line of defence is
// what survives somebody adding a call site later and forgetting the first.
// ParseRemote is not a check a call site can skip, either: it is the only way
// to make the Remote that every function reaching the network insists on.
//
// Nothing here ever runs a hook, a filter or a submodule fetch. A bundle is
// data, a mirror is refs and objects, and neither is allowed to become a
// program.
package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ErrUnavailable is returned when there is no git on this machine to borrow.
var ErrUnavailable = errors.New("no git found on this machine")

// ErrNotARepository is returned when a directory that should hold a mirror does
// not hold one — which is how a half-finished clone from an interrupted run is
// told from a good one, so it can be thrown away and made again.
var ErrNotARepository = errors.New("not a git repository")

// hardening is the set of config every invocation carries.
//
// These are -c flags rather than anything written into a config file because
// they must apply to the clone that creates a repository as well as to the
// commands run inside it afterwards, and because a repository fetched from
// elsewhere must not be able to talk this build out of them: a value passed on
// the command line beats the same key in any file the repository carries.
var hardening = []string{
	// The whole reason a repository URL has to be treated as hostile. ext::
	// hands its argument to a shell.
	"-c", "protocol.ext.allow=never",

	// A fetch that decided to repack a four-gigabyte repository in the middle
	// of a scheduled sweep would be a surprise nobody asked for.
	"-c", "gc.auto=0",

	// Submodules are somebody else's URLs, arriving from inside the repository
	// rather than from the person who asked for it. SAND stores what it was
	// pointed at and nothing that repository points at in turn.
	"-c", "fetch.recurseSubmodules=no",
	"-c", "submodule.recurse=false",

	// A graphical password prompt from a background sweep would be a dialog
	// nobody is sitting in front of. Together with GIT_TERMINAL_PROMPT=0 this
	// makes a repository SAND cannot authenticate to fail quickly and say so,
	// rather than hang until the context gives out.
	"-c", "core.askPass=",
}

// lookup finds git once and remembers the answer. The path is resolved rather
// than the name shelled out to, so that what runs is settled at the moment the
// question is asked.
var lookup = sync.OnceValues(func() (string, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return "", ErrUnavailable
	}
	return path, nil
})

// Available reports whether there is a git to borrow. It is what lets the
// automation panel grey out a task this machine cannot carry out, and what
// lets a policy say why rather than only that it failed.
func Available() bool {
	_, err := lookup()
	return err == nil
}

// Version is what `git --version` says, for the diagnostics panel.
func Version(ctx context.Context) (string, error) {
	out, err := run(ctx, "", "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Remote is a repository URL that has been checked.
//
// It is a type rather than a string so that the check cannot be skipped. Every
// function here that reaches the network takes one, and the only way to make
// one from outside this package is ParseRemote — so a call site added later
// cannot forget to validate, because there is nothing for it to pass.
type Remote struct{ url string }

// String is the URL as given. Safe to show and to store: it is what the user
// typed, minus the shapes ParseRemote refuses.
func (r Remote) String() string { return r.url }

// Empty reports whether this is the zero Remote, which names nothing.
func (r Remote) Empty() bool { return r.url == "" }

// Ref is one ref as a remote advertises it.
type Ref struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// Same reports whether two sets of refs describe the same repository state.
//
// This is the comparison the whole schedule turns on: the refs stored beside a
// bundle against the refs a remote advertises now. Order is ignored because
// nothing guarantees it — ls-remote and show-ref sort differently, and a
// repository has not changed because git listed its tags in another order.
//
// A ref that moved, one that appeared and one that was deleted upstream all
// come out as "not the same", which is right: each of the three is a reason to
// fetch. What the fetch does about the deleted one is keep it, not prune it —
// see Fetch — but the mirror still has to stop advertising it under its old
// name, or the comparison would never come out equal again.
func Same(a, b []Ref) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]string, len(a))
	for _, r := range a {
		seen[r.Name] = r.Hash
	}
	for _, r := range b {
		if hash, ok := seen[r.Name]; !ok || hash != r.Hash {
			return false
		}
	}
	return true
}

// Digest reduces a set of refs to one short string that changes whenever any of
// them does.
//
// Stored instead of the refs themselves, and that is a size decision rather
// than a tidiness one. A repository with four thousand tags has four thousand
// refs; keeping them all would put a quarter of a megabyte per repository into
// the index, which is encrypted, held in memory, written on every change and
// replicated to every connected account as a backup. The digest is sixteen
// bytes and answers the only question the schedule asks — "is this still what
// I stored?" — exactly as well.
//
// Sorted before hashing, because neither ls-remote nor show-ref promises an
// order and a repository has not changed because git listed its tags
// differently.
func Digest(refs []Ref) string {
	lines := make([]string, 0, len(refs))
	for _, r := range refs {
		lines = append(lines, r.Name+" "+r.Hash)
	}
	sort.Strings(lines)

	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// CountCommits is how many commits a mirror holds across every ref.
//
// Taken before and after a fetch, the difference is how much a project actually
// moved in a week — which is the one number in a run's report that says
// something about the work rather than about the transfer.
func CountCommits(ctx context.Context, dir string) (int, error) {
	if err := checkRepo(dir); err != nil {
		return 0, err
	}
	out, err := run(ctx, dir, "rev-list", "--count", "--all")
	if err != nil {
		// An empty repository has nothing to count and says so by failing.
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// ParseRemote checks a repository URL and returns it in the form the rest of
// this package will use.
//
// It is allow-list rather than deny-list. The transports that reach somebody
// else's server are https, http, ssh and the scp-like form git@host:path, and
// those are the four this understands; everything else — ext::, file://, a bare
// local path, a transport a future git invents — is refused by not being on the
// list. That is the only shape of check that is still right after git grows a
// feature nobody here has heard of.
//
// A local path is refused rather than merely unsupported, and deliberately: a
// server that will mirror a path off its own disk on a schedule is a server
// that can be pointed at anything it can read.
func ParseRemote(raw string) (Remote, error) {
	url := strings.TrimSpace(raw)
	if url == "" {
		return Remote{}, errors.New("give the repository's URL")
	}
	if strings.ContainsAny(url, "\x00\n\r") {
		return Remote{}, errors.New("a repository URL cannot contain a line break")
	}
	// A leading dash makes the URL an option wherever it lands, however many
	// layers of quoting it passes through on the way.
	if strings.HasPrefix(url, "-") {
		return Remote{}, fmt.Errorf("a repository URL cannot begin with a dash: %s", url)
	}

	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return Remote{url}, nil
	case strings.HasPrefix(lower, "ssh://"):
		return Remote{url}, nil
	}

	// Anything still holding "::" is a remote helper — git reads <name>::<rest>
	// as "run git-remote-<name>" — and this is where that is refused, before the
	// scp-like form below gets a chance to accept it.
	//
	// The order is the whole point and was wrong once. "ext::sh -c …" is the
	// famous one, but the check cannot be for ext alone: git dispatches on
	// whatever precedes the "::", so the rule is that a helper URL is not a
	// transport SAND speaks, whichever helper it names. Reaching this after the
	// scheme prefixes above is deliberate — an ssh:// or https:// URL has
	// already been accepted by then, so an IPv6 literal like ssh://git@[::1]/r
	// is unaffected. The scp-like form with a bare IPv6 literal is refused, and
	// git barely supports that spelling either; ssh:// is the way to write it.
	if i := strings.Index(url, "::"); i > 0 {
		return Remote{}, fmt.Errorf(
			"%s names git's %q remote helper, which runs a command rather than fetching a "+
				"URL, and SAND will not do that on a schedule — write an https:// or ssh:// "+
				"address instead", url, url[:i])
	}

	// The scp-like form, user@host:path. Told from a Windows drive letter and
	// from a URL with a port by requiring something before the colon that
	// contains a dot or an at sign, which every hostname reachable over the
	// network does.
	if host, path, ok := strings.Cut(url, ":"); ok && path != "" && !strings.Contains(host, "/") {
		if strings.Contains(host, "@") || strings.Contains(host, ".") {
			return Remote{url}, nil
		}
	}
	return Remote{}, fmt.Errorf(
		"%s is not a repository URL SAND can fetch — give an https:// or ssh:// address, "+
			"or the git@host:path form", url)
}

// LsRemote asks a remote what it is holding, without downloading any of it.
//
// This is the whole reason a weekly policy is cheap. A repository that has not
// moved is answered by one short conversation with the server — a few kilobytes
// of ref advertisement — and the expensive half never starts. Only a repository
// whose refs no longer match what was stored is worth spending a fetch on.
//
// Two kinds of entry are dropped, and both would otherwise break the
// comparison this exists to feed. The peeled `^{}` entries say what an
// annotated tag points at rather than naming a ref of their own. And HEAD,
// which a remote advertises and a local repository does not report, is a symref
// to a branch that is already in the list — keeping it would make every stored
// repository differ from its own upstream for ever, and turn the cheap weekly
// check into a full fetch every time.
//
// Anything an upstream advertises under refs/sand/ is dropped as well. That
// namespace is SAND's own in a mirror (see KeptPrefix), Refs leaves it out for
// the same reason, and the two have to agree on what they are comparing.
func LsRemote(ctx context.Context, remote Remote) ([]Ref, error) {
	return lsRemote(ctx, remote.url)
}

// lsRemote is LsRemote for a URL already known to be one — the one a mirror's
// origin was set to by Mirror or FromBundle.
func lsRemote(ctx context.Context, url string) ([]Ref, error) {
	out, err := run(ctx, "", "ls-remote", "--quiet", "--", url)
	if err != nil {
		return nil, err
	}

	var refs []Ref
	for _, line := range strings.Split(out, "\n") {
		hash, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || hash == "" || name == "" {
			continue
		}
		if strings.HasSuffix(name, "^{}") || name == "HEAD" || strings.HasPrefix(name, ownPrefix) {
			continue
		}
		refs = append(refs, Ref{Name: name, Hash: hash})
	}
	return refs, nil
}

// Mirror makes a bare mirror of a remote in dir, which must not exist yet.
//
// A mirror rather than a working clone because nothing here ever wants a
// checkout: the point is to hold every ref the remote has, exactly as it has
// them, so that what comes out the other end is the repository rather than one
// branch of it. It is also the only clone that keeps refs SAND was not told to
// care about — every tag, every branch — which is what makes the stored copy an
// archive rather than a snapshot of main.
func Mirror(ctx context.Context, remote Remote, dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	_, err := run(ctx, "", "clone", "--mirror", "--quiet", "--", remote.url, dir)
	return err
}

// ownPrefix is the namespace a mirror keeps for itself, which no fetch writes
// to and no comparison with the upstream looks at.
const ownPrefix = "refs/sand/"

// KeptPrefix is where a mirror holds what its upstream took away.
//
// A ref the upstream deleted or rewrote is held as
// refs/sand/kept/<name>@<hash>, where <name> is the ref without its refs/ and
// <hash> the first twelve characters of what it pointed at — so
// refs/sand/kept/heads/feature@3f9a1c2b7d0e is the branch feature as it stood
// before somebody removed it, and refs/sand/kept/tags/v1@… a tag that has
// since been deleted or moved.
//
// They are refs rather than a file on the side because a bundle is made of
// refs: whatever a ref reaches is in the bundle and whatever none does is not,
// so holding a ref is the whole mechanism by which the history it reaches
// survives. They are in a namespace of their own so that a plain `git clone`
// of the bundle is the repository as the upstream has it, with nothing
// unexpected checked out; the kept history is there for the asking:
//
//	git clone repo.bundle project
//	cd project && git fetch origin 'refs/sand/kept/*:refs/sand/kept/*'
//	git log refs/sand/kept/heads/feature@3f9a1c2b7d0e
const KeptPrefix = ownPrefix + "kept/"

// Fetch brings a mirror up to date with its upstream without letting the
// upstream take anything away from it.
//
// A plain mirror fetch makes the mirror a copy of the upstream as it stands,
// and that is the wrong contract for a backup. A branch deleted upstream would
// be deleted here; a branch force-pushed would drop the commits it used to
// reach; a repository wiped and started again would wipe its own copy — in
// every case the stored bundle loses exactly the thing somebody later wants it
// for. So this fetch keeps the mirror matching the upstream ref for ref, which
// is what keeps the cheap comparison the schedule turns on cheap, and moves
// what the upstream dropped or rewrote under KeptPrefix instead of discarding
// it:
//
//   - A ref the upstream no longer advertises is moved there before the fetch
//     — before rather than after, because a branch renamed from foo to foo/bar
//     cannot be fetched while a ref named foo is in the way — and is only then
//     deleted under its old name.
//   - A ref the fetch moved to a commit that does not descend from the old one
//     (a force-push, a rewritten history) has its old tip kept there.
//   - Nothing under refs/sand/ is changed by the fetch itself, whatever the
//     upstream advertises.
//   - HEAD follows a renamed default branch, so that a clone of the bundle
//     still checks something out.
//
// Not everything moved there stays. A branch whose last commit is still
// reachable from a ref the upstream kept — the usual case, a feature branch
// merged and then deleted — is let go once the fetch is in, since nothing it
// reaches would be lost; keeping every one of those would turn the copy into
// a museum of every branch the project ever had. A tag is kept whatever it
// reaches, because a tag is a name that matters on its own: that v1.0 was once
// this commit is a fact about the repository, and not one the commit being
// there restores. And a kept ref superseded by a later one for the same name —
// one whose tip is an ancestor of the new one — is replaced by it, so a branch
// that is deleted, remade and deleted again holds one entry and not a trail.
//
// advertised is what the upstream has already been heard to say, so a caller
// that asked once need not ask twice; nil means ask now. What was newly kept
// is returned under its new names. On an error part way the mirror may have
// been left mid-move; a caller that wants it whole again makes it from the
// bundle afresh, which is what the vault does anyway.
func Fetch(ctx context.Context, dir string, advertised []Ref) ([]Ref, error) {
	if err := checkRepo(dir); err != nil {
		return nil, err
	}
	if advertised == nil {
		url, err := run(ctx, dir, "remote", "get-url", "origin")
		if err != nil {
			return nil, err
		}
		if advertised, err = lsRemote(ctx, strings.TrimSpace(url)); err != nil {
			return nil, err
		}
	}
	upstream := make(map[string]bool, len(advertised))
	for _, r := range advertised {
		upstream[r.Name] = true
	}

	before, err := Refs(ctx, dir)
	if err != nil {
		return nil, err
	}

	// A ref the upstream has dropped, and where it is held now. held is ""
	// for one that still has to be decided after the fetch, and created says
	// whether the holding ref was made for it or was already there.
	type dropped struct {
		ref     Ref
		held    string
		created bool
	}
	var pending []dropped

	// Out of the way first, kept before deleted, so that there is no moment at
	// which the mirror reaches less than it did.
	for _, r := range before {
		if upstream[r.Name] {
			continue
		}
		held, created, err := keep(ctx, dir, r)
		if err != nil {
			return nil, err
		}
		if _, err := run(ctx, dir, "update-ref", "-d", r.Name, r.Hash); err != nil {
			return nil, err
		}
		pending = append(pending, dropped{ref: r, held: held, created: created})
	}

	// What is SAND's own must come out of the fetch as it went in.
	own, err := refsUnder(ctx, dir, ownPrefix)
	if err != nil {
		return nil, err
	}
	// Forced, as a mirror's own refspec is: a branch rewritten upstream is
	// taken, and its old tip is dealt with below.
	if _, err := run(ctx, dir, "fetch", "--quiet", "origin", "+refs/*:refs/*"); err != nil {
		return nil, err
	}
	if err := restoreOwn(ctx, dir, own); err != nil {
		return nil, err
	}

	after, err := Refs(ctx, dir)
	if err != nil {
		return nil, err
	}
	now := make(map[string]string, len(after))
	for _, r := range after {
		now[r.Name] = r.Hash
	}
	if err := followHead(ctx, dir, now); err != nil {
		return nil, err
	}
	for _, r := range before {
		if !upstream[r.Name] {
			continue
		}
		if hash, ok := now[r.Name]; !ok || hash != r.Hash {
			pending = append(pending, dropped{ref: r})
		}
	}

	// With the fetch in, it is known what still reaches what, and the decision
	// can be made once and for the right reason.
	var kept []Ref
	for _, d := range pending {
		wanted := strings.HasPrefix(d.ref.Name, "refs/tags/")
		if !wanted {
			wanted = !reachable(ctx, dir, d.ref.Hash)
		}
		switch {
		case wanted && d.held == "":
			held, created, err := keep(ctx, dir, d.ref)
			if err != nil {
				return nil, err
			}
			if created {
				kept = append(kept, Ref{Name: held, Hash: d.ref.Hash})
			}
		case wanted:
			if d.created {
				kept = append(kept, Ref{Name: d.held, Hash: d.ref.Hash})
			}
		case d.created:
			// Moved aside before the fetch, and the fetch brought a ref that
			// reaches everything it did.
			if _, err := run(ctx, dir, "update-ref", "-d", d.held, d.ref.Hash); err != nil {
				return nil, err
			}
		}
	}
	return kept, nil
}

// followHead moves a mirror's HEAD when the branch it named has gone — master
// renamed to main, usually. A fetch does not touch HEAD, and a bundle whose
// HEAD names a branch it does not hold clones with nothing checked out. The
// upstream is asked where its own HEAD points, which is the one extra round
// trip and only on a rename, and followed when the answer is a branch the
// mirror now has. Not being able to follow is not a failed fetch.
func followHead(ctx context.Context, dir string, have map[string]string) error {
	target, err := run(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return nil
	}
	if _, ok := have[strings.TrimSpace(target)]; ok {
		return nil
	}
	out, err := run(ctx, dir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, "ref: ")
		if !ok {
			continue
		}
		ref, _, _ := strings.Cut(rest, "\t")
		if _, ok := have[ref]; ok {
			_, err := run(ctx, dir, "symbolic-ref", "HEAD", ref)
			return err
		}
	}
	return nil
}

// keep holds a ref the upstream dropped or rewrote under KeptPrefix and says
// where. created is false when a kept ref for the same name already reaches
// this hash, in which case nothing is written; a kept ref for the same name
// that this hash reaches is superseded and replaced.
func keep(ctx context.Context, dir string, r Ref) (held string, created bool, err error) {
	family := KeptPrefix + strings.TrimPrefix(r.Name, "refs/") + "@"
	held = family + shortHash(r.Hash)

	// Both questions are asked of git rather than answered from a list because
	// they are about ancestry, not names. Either failing is not fatal: a ref
	// pointing at something that is not a commit — a tag on a tree — cannot be
	// placed in a history, and the safe answer is to keep it.
	if covering, err := forEachRef(ctx, dir, KeptPrefix, "--contains", r.Hash); err == nil {
		for _, name := range covering {
			if strings.HasPrefix(name, family) {
				return name, false, nil
			}
		}
	}
	if superseded, err := forEachRef(ctx, dir, KeptPrefix, "--merged", r.Hash); err == nil {
		for _, name := range superseded {
			if strings.HasPrefix(name, family) && name != held {
				if _, err := run(ctx, dir, "update-ref", "-d", name); err != nil {
					return "", false, err
				}
			}
		}
	}
	if _, err := run(ctx, dir, "update-ref", held, r.Hash); err != nil {
		return "", false, err
	}
	return held, true, nil
}

// reachable reports whether a commit is reachable from some ref the upstream
// still has — one outside SAND's own namespace. A hash git cannot place in a
// history counts as unreachable, which errs towards keeping.
func reachable(ctx context.Context, dir, hash string) bool {
	names, err := forEachRef(ctx, dir, "refs/", "--contains", hash)
	if err != nil {
		return false
	}
	for _, name := range names {
		if !strings.HasPrefix(name, ownPrefix) {
			return true
		}
	}
	return false
}

// restoreOwn puts refs/sand/ back the way it was before a fetch: anything the
// fetch changed is changed back, anything it added is removed.
func restoreOwn(ctx context.Context, dir string, own []Ref) error {
	now, err := refsUnder(ctx, dir, ownPrefix)
	if err != nil {
		return err
	}
	was := make(map[string]string, len(own))
	for _, r := range own {
		was[r.Name] = r.Hash
	}
	seen := make(map[string]bool, len(now))
	for _, r := range now {
		seen[r.Name] = true
		switch hash, ok := was[r.Name]; {
		case !ok:
			if _, err := run(ctx, dir, "update-ref", "-d", r.Name); err != nil {
				return err
			}
		case hash != r.Hash:
			if _, err := run(ctx, dir, "update-ref", r.Name, hash); err != nil {
				return err
			}
		}
	}
	for _, r := range own {
		if !seen[r.Name] {
			if _, err := run(ctx, dir, "update-ref", r.Name, r.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}

// shortHash is enough of a hash to name a kept ref by: long enough that two
// tips of the same branch will not share it, short enough to read.
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// Refs lists what a mirror is holding of its upstream, in the same shape
// LsRemote returns, so that what is stored and what is upstream can be
// compared directly. What the mirror keeps for itself under refs/sand/ is
// left out — see Kept for that.
func Refs(ctx context.Context, dir string) ([]Ref, error) {
	if err := checkRepo(dir); err != nil {
		return nil, err
	}
	all, err := refsUnder(ctx, dir, "refs/")
	if err != nil {
		return nil, err
	}
	refs := all[:0]
	for _, r := range all {
		if !strings.HasPrefix(r.Name, ownPrefix) {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	return refs, nil
}

// Kept lists what a mirror holds that its upstream no longer does, under the
// names KeptPrefix describes.
func Kept(ctx context.Context, dir string) ([]Ref, error) {
	if err := checkRepo(dir); err != nil {
		return nil, err
	}
	return refsUnder(ctx, dir, KeptPrefix)
}

// refsUnder lists the refs below a prefix. A repository with none — a freshly
// mirrored empty one — is an answer, not a failure.
func refsUnder(ctx context.Context, dir, prefix string) ([]Ref, error) {
	out, err := run(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, line := range strings.Split(out, "\n") {
		hash, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || hash == "" || name == "" {
			continue
		}
		refs = append(refs, Ref{Name: name, Hash: hash})
	}
	return refs, nil
}

// forEachRef names the refs below a prefix that satisfy one of for-each-ref's
// ancestry filters, --contains or --merged, for a hash.
func forEachRef(ctx context.Context, dir, prefix, filter, hash string) ([]string, error) {
	out, err := run(ctx, dir, "for-each-ref", "--format=%(refname)", filter, hash, prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// Head is the branch a mirror's HEAD points at, "" when it points at nothing —
// which is what an empty repository looks like. Stored alongside the bundle so
// that a person restoring one can be told which branch to expect.
func Head(ctx context.Context, dir string) (string, error) {
	if err := checkRepo(dir); err != nil {
		return "", err
	}
	out, err := run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// Bundle writes every ref in a mirror to a single file at out.
//
// A bundle is the reason a repository fits SAND at all. The vault stores files
// and scatters each one over several accounts as encrypted parts; a repository
// stored as a directory would be thousands of tiny objects, each one becoming
// several cloud objects of its own, and a fetch that rewrote the pack files
// would re-scatter most of them every week. A bundle is one file, it holds the
// entire history, and `git clone repo.bundle` turns it back into a repository
// with no SAND involved — which is the property worth having in an archive
// nobody may be able to ask about in ten years.
func Bundle(ctx context.Context, dir, out string) error {
	if err := checkRepo(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	// --all names every ref rather than the current branch, which is the
	// difference between archiving a repository and archiving a checkout.
	if _, err := run(ctx, dir, "bundle", "create", "--quiet", out, "--all"); err != nil {
		return err
	}
	// A bundle git will not read back is not a backup, and the check costs one
	// pass over a file that was just written.
	return Verify(ctx, out)
}

// Verify reports whether a bundle is one git will read back.
//
// It is run on the way in rather than only on the way out. A bundle is verified
// the moment it is made, before it is encrypted, cut into parts and spread over
// several clouds, because that is the last moment at which making it again is
// cheap.
func Verify(ctx context.Context, bundle string) error {
	// verify wants somewhere to be, and refuses to run outside a repository on
	// some versions; a bare temporary one costs nothing and settles it.
	dir, err := os.MkdirTemp("", "sand-bundle-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := run(ctx, "", "init", "--quiet", "--bare", dir); err != nil {
		return err
	}

	abs, err := filepath.Abs(bundle)
	if err != nil {
		return err
	}
	if _, err := run(ctx, dir, "bundle", "verify", "--quiet", abs); err != nil {
		return fmt.Errorf("the bundle just written is not one git will read back: %w", err)
	}
	return nil
}

// FromBundle turns a bundle back into a bare mirror in dir, so that the next
// fetch can be incremental.
//
// This is what makes the second week cheap. Without it, a repository whose refs
// have moved would have to be cloned from scratch every time — the whole
// history down the wire for one new commit. With it, the stored bundle becomes
// the local copy, origin is pointed back at the real remote, and the fetch
// brings down the difference.
func FromBundle(ctx context.Context, bundle, dir string, remote Remote) error {
	abs, err := filepath.Abs(bundle)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if _, err := run(ctx, "", "clone", "--mirror", "--quiet", "--", abs, dir); err != nil {
		return err
	}
	// The clone left origin pointing at a file that is about to be deleted.
	_, err = run(ctx, dir, "remote", "set-url", "origin", "--", remote.url)
	return err
}

// checkRepo reports whether dir holds a git repository, so that a directory
// left behind by an interrupted run is told from a usable mirror.
func checkRepo(dir string) error {
	if dir == "" {
		return ErrNotARepository
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return fmt.Errorf("%w: %s", ErrNotARepository, dir)
	}
	return nil
}

// run invokes git in dir, with the hardening flags and an environment that
// cannot stop to ask anybody anything.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	bin, err := lookup()
	if err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, bin, append(append([]string(nil), hardening...), args...)...)
	cmd.Dir = dir
	cmd.Env = environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), fmt.Errorf("git %s: %w", args[0], ctxErr)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// git is wordy on failure and the last line is usually the reason;
		// the whole of it in a run's warning list would bury everything else.
		return stdout.String(), fmt.Errorf("git %s: %s", args[0], lastLine(msg))
	}
	return stdout.String(), nil
}

// environ is the caller's environment with the interactive doors shut.
//
// It is the caller's environment rather than a clean one on purpose: SSH_AUTH_SOCK,
// HOME, GIT_CONFIG_GLOBAL and the rest are how the user's own credentials reach
// this, and stripping them would leave a git that can fetch nothing private.
// What is added only ever removes a way for git to stop and wait for somebody.
func environ() []string {
	env := append([]string(nil), os.Environ()...)

	// Never prompt on the terminal for a username or password. A background
	// sweep has no terminal, and a git that asks will sit there until the
	// context expires rather than failing with something a person can read.
	env = append(env, "GIT_TERMINAL_PROMPT=0")

	// The same for ssh, which does its own asking — for a key passphrase, or to
	// confirm an unknown host. BatchMode turns both into an immediate failure
	// while still using an agent that already holds the key. An explicit
	// GIT_SSH_COMMAND wins: somebody who has set one has said how they want ssh
	// run, and this is not the place to overrule them.
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// lastLine is the final non-empty line of git's complaint.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return s
}
