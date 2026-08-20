package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/git"
)

// A repository in a vault is one file: a git bundle holding its whole history.
//
// That is the only shape that fits. SAND stores files and cuts each one into
// encrypted parts spread over several accounts, so a repository kept as a
// directory would be thousands of loose objects, each becoming several cloud
// objects of its own — and a fetch that repacked them would re-scatter most of
// that every week, for a handful of new commits. A bundle is one file. It goes
// through the ordinary upload path, gets the ordinary erasure code, and lands
// on the ordinary accounts, with nothing in this file needing to know how any
// of that works.
//
// It is also the shape that survives SAND. `git clone repo.bundle` is a
// complete repository — every branch, every tag, the right default branch —
// with no SAND involved and nothing to reimplement. An archive whose format
// needs the tool that wrote it is an archive with a deadline; this one can be
// read by any git for as long as there is git.
//
// # Why the weekly check is nearly free
//
// The expensive thing about keeping a mirror current is fetching it, and almost
// every week almost every repository has not moved. So the question is asked
// before the work is done: `git ls-remote` is one short conversation with the
// server, a few kilobytes of ref advertisement, no objects at all. Its answer
// is reduced to a digest and compared with the digest stored beside the bundle.
// Equal means done — nothing downloaded, nothing uploaded, nothing re-encrypted
// — and that is the path a hundred repositories take on an ordinary Sunday.
//
// Only a repository whose digest has changed costs anything, and even then not
// a full clone: the stored bundle is turned back into a mirror, origin is
// pointed at the real remote, and the fetch brings down the difference.
//
// # What is deliberately not done
//
// Nothing here is ever checked out, and no hook, filter or submodule of a
// fetched repository is ever run — see the note in internal/git. A repository
// is data on the way in and data on the way out.
//
// And an upstream that has stopped answering is not a reason to delete
// anything. A repository that 404s is the repository somebody is most glad to
// have kept, and an outage, a moved account and a revoked token all look
// exactly like a deletion from here. So the stored bundle stays, the run says
// the upstream is gone, and throwing it away stays a thing somebody does on
// purpose (GitPolicy.Prune).

// DefaultBundleLimit is how large a stored bundle an unattended refresh will
// re-fetch unless the policy says otherwise.
//
// The work behind a refresh is a git mirror on local disk plus a new bundle
// written beside it, so a repository costs roughly twice its bundle in
// temporary space before anything is uploaded. SAND is meant to run on a
// Raspberry Pi, and a scheduled job that decides at three in the morning to put
// eight gigabytes on an SD card is a machine that stops answering ssh. Past
// this, the run counts the repository, names it, and leaves it for somebody to
// refresh by hand where they can watch it.
const DefaultBundleLimit int64 = 2 << 30

// bundleSuffix is what a stored repository is named with. It is the extension
// git itself expects, which is the point: the file that comes out of a vault
// should be one `git clone` already knows what to do with.
const bundleSuffix = ".bundle"

// ErrNotTracked is returned for a file that is not a stored repository.
var ErrNotTracked = errors.New("this file is not a tracked repository")

// GitSource is what makes a stored bundle a mirror rather than a file somebody
// happened to upload.
//
// It lives in the manifest keyed by file ID, beside the film details and for
// the same reasons: it is index rather than content, it is encrypted at rest,
// it travels with a rename, and it goes when the file goes. Which repositories
// somebody keeps is as revealing as which films they do.
type GitSource struct {
	// URL is the upstream, as the user gave it and as ParseRemote accepted it.
	URL string `json:"url"`

	// Digest is git.Digest over the refs the stored bundle holds. It is the
	// whole comparison: what the upstream advertises now, reduced the same way,
	// either equals this or does not.
	Digest string `json:"digest"`

	// Refs is how many refs that digest covers and Head the default branch, so
	// that a listing can say "47 refs, main" without the bundle being fetched
	// or opened.
	Refs int    `json:"refs"`
	Head string `json:"head,omitempty"`

	// Commits is how many the bundle holds across every ref, which is what lets
	// a refresh report that a week brought eleven of them.
	Commits int `json:"commits,omitempty"`

	// FetchedAt is when the stored bundle was last actually made, and CheckedAt
	// when the upstream was last asked. They differ by design and the gap is
	// the good news: checked every Sunday, fetched in March, means a repository
	// that has not moved since March and cost nothing to know that about.
	FetchedAt time.Time `json:"fetched_at"`
	CheckedAt time.Time `json:"checked_at,omitempty"`

	// Gone is set when the upstream stopped answering, with the reason, so that
	// a listing can mark it without the run's history being read. Cleared the
	// moment it answers again.
	Gone   bool   `json:"gone,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// TrackedRepo is one stored repository as anything outside this package sees
// it: where the file is, and what is known about the repository inside it.
type TrackedRepo struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Dir  string `json:"dir"`
	Name string `json:"name"`
	Size int64  `json:"size"`

	GitSource
}

// RepoName turns a repository URL into the file name its bundle is stored
// under: the last path segment, without a .git suffix, plus .bundle.
//
// It is a name rather than an ID because the file is meant to be recognisable
// in a folder listing — somebody looking at /code should see sand-vault.bundle,
// not a UUID. Collisions are the vault's ordinary business: two repositories
// with the same last segment produce the same name, and the upload path files
// the second one beside the first under a numbered name exactly as it would for
// two photographs with the same name.
func RepoName(url string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(url), "/"), ".git")

	// Take whatever follows the last separator of either kind, which covers
	// https://host/owner/repo and git@host:owner/repo alike.
	if i := strings.LastIndexAny(trimmed, "/:"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if name, err := SanitizeName(trimmed + bundleSuffix); err == nil {
		return name
	}
	return "repository" + bundleSuffix
}

// TrackRepo mirrors a repository and stores it as a bundle in dir.
//
// This is the one operation that puts a repository into a vault, and it is
// deliberately the same shape as an upload: it produces a file in a folder,
// with an ordinary name, an ordinary erasure code and ordinary parts on
// ordinary accounts. What makes it a tracked repository rather than a file is
// the GitSource recorded beside it afterwards.
func (v *Vault) TrackRepo(ctx context.Context, scope Scope, dir, url string, opts UploadOptions) (*TrackedRepo, []string, error) {
	remote, err := git.ParseRemote(url)
	if err != nil {
		return nil, nil, err
	}
	if !git.Available() {
		return nil, nil, fmt.Errorf(
			"%w — SAND borrows the git already on the machine rather than carrying its own",
			git.ErrUnavailable)
	}

	work, cleanup, err := v.gitWorkspace()
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	mirror := filepath.Join(work, "mirror.git")
	if err := git.Mirror(ctx, remote, mirror); err != nil {
		return nil, nil, err
	}

	source, bundle, err := bundleMirror(ctx, work, mirror, remote)
	if err != nil {
		return nil, nil, err
	}

	entry, warnings, err := v.uploadBundle(ctx, scope, dir, RepoName(url), bundle, opts)
	if err != nil {
		return nil, warnings, err
	}
	if err := v.setGitSource(scope, entry.ID, source); err != nil {
		return nil, warnings, err
	}
	return &TrackedRepo{
		ID: entry.ID, Path: entry.Path(), Dir: entry.Dir, Name: entry.Name,
		Size: entry.Size, GitSource: *source,
	}, warnings, nil
}

// RefreshRepo brings one stored repository up to date with its upstream, and
// reports whether anything actually moved.
//
// It is the unit the scheduled sweep is made of, and it is also the button in
// the browser, so the two cannot drift apart.
func (v *Vault) RefreshRepo(ctx context.Context, scope Scope, id string) (*TrackedRepo, bool, error) {
	repo, err := v.TrackedRepo(scope, id)
	if err != nil {
		return nil, false, err
	}
	return v.refreshRepo(ctx, scope, repo, nil, nil)
}

// refreshRepo is the work: ask, and fetch only if the answer has changed.
//
// It hands back the repository as it now stands rather than only whether it
// moved, because storing a new bundle over the old one gives the file a new ID
// — an overwrite is a new entry, not an edited one — and a caller left holding
// the ID it started with would be holding one that names nothing.
//
// advertised, when given, is what the upstream has already been heard to say,
// so the sweep — which has to ask before it can decide whether the bounds apply
// — does not make this ask a second time. Nil means go and ask.
//
// counts, when given, has the new-commit tally added to it — the sweep wants it
// summed across every repository and a single refresh does not.
func (v *Vault) refreshRepo(ctx context.Context, scope Scope, repo *TrackedRepo, advertised []git.Ref, counts *int) (*TrackedRepo, bool, error) {
	remote, err := git.ParseRemote(repo.URL)
	if err != nil {
		return nil, false, err
	}

	// The cheap half, and on an ordinary week the only half.
	if advertised == nil {
		if advertised, err = git.LsRemote(ctx, remote); err != nil {
			// The upstream did not answer. That is worth recording and is never
			// by itself worth deleting anything over — see the note at the top.
			source := repo.GitSource
			source.CheckedAt = time.Now().UTC()
			source.Gone = true
			source.Reason = err.Error()
			if setErr := v.setGitSource(scope, repo.ID, &source); setErr != nil {
				return nil, false, setErr
			}
			return nil, false, err
		}
	}

	if git.Digest(advertised) == repo.Digest {
		// Nothing moved. Stamp the check and stop: no clone, no fetch, no
		// upload, no re-encryption, nothing over the wire but the ref
		// advertisement that got us here.
		source := repo.GitSource
		source.CheckedAt = time.Now().UTC()
		source.Gone, source.Reason = false, ""
		if err := v.setGitSource(scope, repo.ID, &source); err != nil {
			return nil, false, err
		}
		unchanged := *repo
		unchanged.GitSource = source
		return &unchanged, false, nil
	}

	work, cleanup, err := v.gitWorkspace()
	if err != nil {
		return nil, false, err
	}
	defer cleanup()

	// The stored bundle becomes the local mirror, so the fetch brings down the
	// difference rather than the history.
	stored := filepath.Join(work, "stored.bundle")
	if err := v.exportFile(ctx, repo.ID, stored); err != nil {
		return nil, false, fmt.Errorf("reading the stored bundle back: %w", err)
	}

	mirror := filepath.Join(work, "mirror.git")
	if err := git.FromBundle(ctx, stored, mirror, remote); err != nil {
		return nil, false, err
	}
	before, _ := git.CountCommits(ctx, mirror)

	if err := os.Remove(stored); err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	if err := git.Fetch(ctx, mirror); err != nil {
		return nil, false, err
	}

	source, bundle, err := bundleMirror(ctx, work, mirror, remote)
	if err != nil {
		return nil, false, err
	}
	if after, _ := git.CountCommits(ctx, mirror); after > before && counts != nil {
		*counts += after - before
	}

	// Overwrite rather than store beside: this is the same repository, and a
	// folder that accumulated sand-vault.bundle, sand-vault (2).bundle every
	// time somebody pushed would be a folder nobody could use.
	entry, _, err := v.uploadBundle(ctx, scope, repo.Dir, repo.Name, bundle,
		UploadOptions{Overwrite: true})
	if err != nil {
		return nil, false, err
	}

	// An overwrite stores a new entry over the old one rather than editing it,
	// so the file has a new ID. The record follows the file, and the old one is
	// dropped rather than left pointing at something that is no longer there.
	if entry.ID != repo.ID {
		if err := v.setGitSource(scope, repo.ID, nil); err != nil {
			return nil, false, err
		}
	}
	if err := v.setGitSource(scope, entry.ID, source); err != nil {
		return nil, false, err
	}
	return &TrackedRepo{
		ID: entry.ID, Path: entry.Path(), Dir: entry.Dir, Name: entry.Name,
		Size: entry.Size, GitSource: *source,
	}, true, nil
}

// bundleMirror writes a bundle of a mirror and describes what went into it.
func bundleMirror(ctx context.Context, work, mirror string, remote git.Remote) (*GitSource, string, error) {
	refs, err := git.Refs(ctx, mirror)
	if err != nil {
		return nil, "", err
	}
	head, _ := git.Head(ctx, mirror)
	commits, _ := git.CountCommits(ctx, mirror)

	bundle := filepath.Join(work, "out.bundle")
	if err := git.Bundle(ctx, mirror, bundle); err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	return &GitSource{
		URL:       remote.String(),
		Digest:    git.Digest(refs),
		Refs:      len(refs),
		Head:      head,
		Commits:   commits,
		FetchedAt: now,
		CheckedAt: now,
	}, bundle, nil
}

// uploadBundle puts a bundle into the vault through the ordinary upload path.
func (v *Vault) uploadBundle(ctx context.Context, scope Scope, dir, name, bundle string, opts UploadOptions) (*Entry, []string, error) {
	f, err := os.Open(bundle)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	return v.UploadStreamAt(ctx, scope, dir, name, f, info.Size(), opts)
}

// exportFile writes a stored file out to a path on local disk.
//
// It streams chunk by chunk rather than gathering the file first, which for a
// two-gigabyte bundle is the difference between a mirror SAND can refresh on a
// Raspberry Pi and one it cannot. A file in the pre-chunking format has no
// seams to stream along and is read whole — those are old and small, and the
// alternative is refusing to refresh a repository stored before chunking
// existed.
func (v *Vault) exportFile(ctx context.Context, id, path string) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	src, _, err := v.OpenReadSeeker(ctx, id)
	switch {
	case errors.Is(err, ErrNeedsConversion):
		data, _, fetchErr := v.Fetch(ctx, id)
		if fetchErr != nil {
			return fetchErr
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := io.Copy(out, src); err != nil {
			return err
		}
	}
	return out.Sync()
}

// gitWorkspace makes a private directory for a mirror and a bundle to be built
// in, next to the vault file rather than in the system temp directory — the
// same choice the upload spool makes, and for the same reason: it inherits
// whatever protects the vault, and it is on the disk the user chose rather than
// on whichever one /tmp happens to be.
func (v *Vault) gitWorkspace() (string, func(), error) {
	v.mu.RLock()
	base := filepath.Dir(v.path)
	v.mu.RUnlock()

	dir, err := os.MkdirTemp(base, ".sand-git-*")
	if err != nil {
		return "", nil, fmt.Errorf("making room to work on the repository: %w", err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// ---------------------------------------------------------------------------
// The index side
// ---------------------------------------------------------------------------

// setGitSource records, replaces or (with nil) drops what is known about one
// stored repository.
func (v *Vault) setGitSource(scope Scope, id string, source *GitSource) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return err
	}

	previous, had := m.Repos[id]
	switch {
	case source == nil:
		if !had {
			return nil
		}
		delete(m.Repos, id)
		if len(m.Repos) == 0 {
			m.Repos = nil
		}
	default:
		if m.Repos == nil {
			m.Repos = map[string]*GitSource{}
		}
		m.Repos[id] = source
	}

	if err := v.persistLocked(); err != nil {
		// Put the index back the way the file on disk still has it.
		if had {
			if m.Repos == nil {
				m.Repos = map[string]*GitSource{}
			}
			m.Repos[id] = previous
		} else {
			delete(m.Repos, id)
			if len(m.Repos) == 0 {
				m.Repos = nil
			}
		}
		return err
	}
	return nil
}

// TrackedRepo returns one stored repository, or ErrNotTracked for a file that
// is only a file.
func (v *Vault) TrackedRepo(scope Scope, id string) (*TrackedRepo, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}

	entry := m.ByID(id)
	if entry == nil {
		return nil, fmt.Errorf("no such file: %s", id)
	}
	source := m.Repos[id]
	if source == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotTracked, entry.Path())
	}
	return &TrackedRepo{
		ID: entry.ID, Path: entry.Path(), Dir: entry.Dir, Name: entry.Name,
		Size: entry.Size, GitSource: *source,
	}, nil
}

// TrackedRepos lists the repositories stored under a folder and everything
// below it, by path.
func (v *Vault) TrackedRepos(scope Scope, dir string) ([]TrackedRepo, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	return reposUnder(m, CleanDir(dir)), nil
}

// reposUnder collects the tracked repositories below a folder. The caller must
// hold at least the read lock.
func reposUnder(m *Manifest, dir string) []TrackedRepo {
	if len(m.Repos) == 0 {
		return nil
	}
	out := make([]TrackedRepo, 0, len(m.Repos))
	for _, e := range m.Descendants(dir) {
		source := m.Repos[e.ID]
		if source == nil {
			continue
		}
		out = append(out, TrackedRepo{
			ID: e.ID, Path: e.Path(), Dir: e.Dir, Name: e.Name,
			Size: e.Size, GitSource: *source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// countReposUnder is how many tracked repositories are below a folder.
//
// Separate from reposUnder because it is on a different path: every folder
// listing asks for it, only to decide whether to light a button, and building
// the rows to throw them away would be an allocation per listing. The early
// return is what makes it free for the vaults that keep no repositories, which
// is nearly all of them. The caller must hold at least the read lock.
func countReposUnder(m *Manifest, dir string) int {
	if len(m.Repos) == 0 {
		return 0
	}
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}

	var n int
	for _, e := range m.Entries {
		if e.Dir != dir && !strings.HasPrefix(e.Dir, prefix) {
			continue
		}
		if m.Repos[e.ID] != nil {
			n++
		}
	}
	return n
}

// UntrackRepo forgets that a file is a mirror, leaving the file alone.
//
// The bundle stays because it is a perfectly good file: a complete repository
// somebody can still clone. What stops is SAND asking its upstream anything.
func (v *Vault) UntrackRepo(scope Scope, id string) error {
	if _, err := v.TrackedRepo(scope, id); err != nil {
		return err
	}
	return v.setGitSource(scope, id, nil)
}

// forgetRepos drops the records of a set of files. It is what deleting one — or
// a folder of them — calls, and it does not persist: the caller is already
// writing the index in the same breath. Exactly forgetMovies, next door.
func (m *Manifest) forgetRepos(ids ...string) {
	if len(m.Repos) == 0 {
		return
	}
	for _, id := range ids {
		delete(m.Repos, id)
	}
	if len(m.Repos) == 0 {
		m.Repos = nil
	}
}

// ---------------------------------------------------------------------------
// The scheduled sweep
// ---------------------------------------------------------------------------

// sweepGit is the mirror task: every tracked repository under the folder asked
// whether its upstream has moved, and the ones that have brought up to date.
//
// The asking is not bounded and the fetching is. Asking is a few kilobytes per
// repository and the whole point of the design; fetching is the expensive half,
// and a policy that says "at most five a night" means five fetches, not five
// questions.
func (v *Vault) sweepGit(ctx context.Context, scope Scope, policy *Automation, entries []*Entry, run *AutomationRun, seen map[string]bool) {
	res := &GitResult{}
	run.Git = res

	v.mu.RLock()
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		run.Error = err.Error()
		return
	}
	byID := make(map[string]*GitSource, len(m.Repos))
	for id, source := range m.Repos {
		byID[id] = source
	}
	v.mu.RUnlock()

	repos := make([]TrackedRepo, 0, len(entries))
	for _, e := range entries {
		source := byID[e.ID]
		if source == nil {
			continue
		}
		repos = append(repos, TrackedRepo{
			ID: e.ID, Path: e.Path(), Dir: e.Dir, Name: e.Name,
			Size: e.Size, GitSource: *source,
		})
	}
	res.Repos = len(repos)

	if !git.Available() {
		run.Error = "there is no git on this machine for SAND to borrow, so no repository could be asked about"
		return
	}
	if len(repos) == 0 {
		return
	}

	cfg := policy.git()
	ceiling := policy.bundleCeiling()

	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			run.Error = fmt.Sprintf("stopped part way: %v", err)
			return
		}
		// A repository an inner folder's own policy already dealt with this
		// tick is not asked about twice.
		if seen != nil {
			if seen[repo.ID] {
				continue
			}
			seen[repo.ID] = true
		}
		res.Checked++

		v.sweepOneRepo(ctx, scope, policy, cfg, ceiling, repo, res, run)
	}
}

// sweepOneRepo is one repository's turn, split out so the loop above reads as
// the schedule it is.
func (v *Vault) sweepOneRepo(
	ctx context.Context,
	scope Scope,
	policy *Automation,
	cfg GitPolicy,
	ceiling int64,
	repo TrackedRepo,
	res *GitResult,
	run *AutomationRun,
) {
	remote, err := git.ParseRemote(repo.URL)
	if err != nil {
		res.Failed++
		run.Warnings = append(run.Warnings, fmt.Sprintf("%s: %v", repo.Path, err))
		return
	}

	advertised, err := git.LsRemote(ctx, remote)
	if err != nil {
		res.Gone++
		run.Warnings = append(run.Warnings, fmt.Sprintf(
			"%s: %s did not answer — %v", repo.Path, repo.URL, err))

		source := repo.GitSource
		source.CheckedAt = time.Now().UTC()
		source.Gone, source.Reason = true, err.Error()
		if setErr := v.setGitSource(scope, repo.ID, &source); setErr != nil {
			run.Error = setErr.Error()
			return
		}
		v.pruneGone(ctx, scope, cfg, repo, res, run)
		return
	}

	if git.Digest(advertised) == repo.Digest {
		res.Current++
		source := repo.GitSource
		source.CheckedAt = time.Now().UTC()
		source.Gone, source.Reason = false, ""
		if err := v.setGitSource(scope, repo.ID, &source); err != nil {
			run.Error = err.Error()
		}
		return
	}

	// It has moved. Everything from here costs real bandwidth, so the bounds
	// apply.
	if policy.Action != ActionPull {
		res.Deferred++
		run.Warnings = append(run.Warnings, fmt.Sprintf(
			"%s has moved upstream — this policy only looks, so nothing was fetched", repo.Path))
		return
	}
	if cfg.MaxRepos > 0 && res.Updated+res.Failed >= cfg.MaxRepos {
		res.Deferred++
		return
	}
	if ceiling > 0 && repo.Size > ceiling {
		res.Deferred++
		run.Warnings = append(run.Warnings, fmt.Sprintf(
			"%s is %s, past this policy's %s ceiling — a refresh puts a mirror and a new "+
				"bundle on local disk, so this one is left for somebody to start by hand",
			repo.Path, formatSize(repo.Size), formatSize(ceiling)))
		return
	}

	// The refs are handed on rather than asked for again: this function had to
	// ask before it could tell whether the bounds above applied at all.
	updated, moved, err := v.refreshRepo(ctx, scope, &repo, advertised, &res.Commits)
	switch {
	case err != nil:
		res.Failed++
		run.Warnings = append(run.Warnings,
			fmt.Sprintf("%s could not be brought up to date: %v", repo.Path, err))
		if errors.Is(err, ErrLocked) {
			run.Error = "the vault was locked before the sweep finished"
		}
	case moved:
		res.Updated++
		res.Bytes += updated.Size
	default:
		res.Current++
	}
}

// pruneGone deletes a stored bundle whose upstream has gone, but only when the
// policy was explicitly told it may.
//
// The default is to keep it, and that is not timidity. Every reason an upstream
// stops answering looks identical from here — deleted, renamed, made private, a
// token expired, a network that is down, a DNS server having a bad afternoon —
// and only one of them is a reason to throw away what may be the last copy of
// somebody's repository.
func (v *Vault) pruneGone(ctx context.Context, scope Scope, cfg GitPolicy, repo TrackedRepo, res *GitResult, run *AutomationRun) {
	if !cfg.Prune {
		return
	}
	warnings, err := v.Delete(ctx, repo.ID)
	run.Warnings = append(run.Warnings, warnings...)
	if err != nil {
		res.Failed++
		run.Warnings = append(run.Warnings,
			fmt.Sprintf("%s could not be deleted: %v", repo.Path, err))
		return
	}
	res.Pruned++
	run.Warnings = append(run.Warnings, fmt.Sprintf(
		"%s was deleted: its upstream has gone and this policy prunes", repo.Path))
}
