# Keeping git repositories

A vault folder can hold copies of git repositories and keep them up to date with
their upstreams. Each repository is stored as **one file**, a git bundle holding
its whole history (every branch and every tag). It is encrypted, cut into parts
and spread over your clouds like any other file. How the index records it is in
[index.md](../design/index.md).

## Tracking a repository

```bash
sand git track https://github.com/chinmay28/sand-vault.git --into /code
# Mirroring https://github.com/chinmay28/sand-vault.git — the first copy is the whole history.
# Stored /code/sand-vault.bundle (14.2 MB, 47 refs, 1841 commits) in 12s.

sand git track git@github.com:me/private.git --into /code --accounts a,b,c --scheme 2-of-3
```

The bundle is named after the repository. In the browser, open the folder, click
**🗂**, then **Repositories kept here**.

Getting it back needs nothing from SAND Vault except the download:

```bash
sand get /code/sand-vault.bundle -o ./recovered.bundle
git clone ./recovered.bundle
```

That is the point of the format. An archive that needs the tool that wrote it
has a deadline.

## Keeping it current

```bash
sand git list /code                       # what is stored under a folder
sand git refresh /code/sand-vault.bundle  # one repository
sand git refresh /code --all              # everything under a folder
sand git untrack /code/sand-vault.bundle  # stop following; the bundle stays
```

A refresh first asks the upstream whether anything has moved. That is one short
exchange of a few kilobytes of refs, with no history. It fetches only when the
answer changed, and then only the difference, because the stored bundle becomes
the local mirror before the fetch.

To run it on a schedule, use a [folder policy](maintenance.md#a-folder-that-looks-after-itself)
with the git task:

```bash
sand automation set /code --weekly sun,04:00 --task git --action pull
sand automation run /code
#   20 Aug 21:27  9 checked, 8 up to date, 1 updated
#                 11 new commit(s), 4.2 MB stored
```

| Flag | Effect |
|---|---|
| `--action check` | ask upstreams, fetch nothing |
| `--action pull` | fetch repositories that moved |
| `--max-repos N` | most repositories fetched per run (every one is still asked) |
| `--size-limit SIZE` / `none` | leave bundles larger than this for a manual refresh; the default comes from the machine's sizing (a quarter of memory, 2–16 GB) |
| `--prune` | delete a stored repository whose upstream has gone (off by default) |

## It uses the git on the machine

SAND Vault runs the machine's own `git`, so private repositories work with your
SSH keys, credential helper, `~/.ssh/config` aliases and corporate CA. **A
repository SAND Vault can reach is one you can reach from the same machine.**
This feature needs git installed. A `--action pull` policy is refused when it is
set on a machine without git, rather than failing at 4 a.m.

Deliberate limits:

- It never checks anything out, never runs hooks or filters, and never follows
  submodules. A submodule is somebody else's URL arriving from inside the
  repository.
- URLs are treated as hostile input, because git's `ext::` transport runs its
  argument as a command. Only `https://`, `http://`, `ssh://` and
  `git@host:path` are accepted. Local paths and `file://` are refused.

## When the upstream disappears

If an upstream stops answering (taken down, renamed, made private, or just a DNS
outage), the stored copy is **kept** and the run reports the upstream as gone.
From here, all of those look the same, and only one of them is a reason to throw
away what may be the last copy. Deletion only happens with `--prune`.

## When the stored copy disappears

If the stored bundle can't be read (too few of its clouds answer) but the
upstream is still there, **Rebuild from upstream** mirrors the repository again
from scratch. It stores the result over the file as a fresh set of shards on the
clouds you pick, then erases any old shards still reachable. The stored shards
are never read, so this works whatever state they are in.

You'll find it in a repository file's **⋯** menu, under **Repositories kept
here**, and on its row in the **Files missing a part** list.

```bash
sand git rebuild /code/sand-vault.bundle --accounts box,s3,drive
# Mirroring https://github.com/chinmay28/sand-vault.git afresh — the whole history comes down, and the stored copy is not read.
# Stored /code/sand-vault.bundle afresh (14.2 MB, 47 refs, 1841 commits) in 14s.
```

`--accounts` is required. `--scheme` defaults to the code that number of
accounts names. A rebuild costs as much as the first copy did, so for a bundle
that still reads and only needs new commits, refresh is cheaper.
