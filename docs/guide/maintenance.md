# Maintenance

Keeping a vault healthy without watching it: scheduled checks and repairs on a
folder, and the cleanups for objects the index and the clouds disagree about.
These are stray parts nothing points at, shards a disconnect mislaid, parts a
cloud has lost, old object versions on versioned buckets, and working files
left on the machine. Exact flags are in the [CLI reference](../reference/cli.md).

## A folder that looks after itself

A file missing one of its three shards reads back perfectly until the day a
second cloud is unavailable. Expired tokens, a bucket that stopped answering,
or a shard that never landed don't make anything look broken. So a folder can be
given a standing instruction to check every cloud and every shard on a schedule,
and to put back anything that is missing:

```bash
sand automation set /archive --daily 10:00 --action rebalance
```

Every morning at ten, every account is pinged, every file under `/archive` is
checked shard by shard, and anything short is rebuilt onto the clouds that
answered.

In the browser: open the folder, click **🗂** (*Organize and automate this
folder*) in the toolbar, then **Look after this folder**. The 🗂 button turns
amber when the last run found something. The dialog shows the last few runs
first and the settings underneath.

### Scope

- A policy covers its folder and everything under it, so `/` looks after the
  whole vault. Where policies are nested, the deeper one runs first and the
  outer one skips files already checked in that sweep.
- The policy moves with the folder when it is renamed, and is deleted with it.

### Settings

| Flag | Meaning |
|---|---|
| `--hourly` / `--daily HH:MM` / `--weekly day,HH:MM` | how often (exactly one; times are the server's local time) |
| `--task shards` (default) / `--task git` | check files' shards, or [mirrored git repositories](repositories.md) |
| `--action check` (default) | look and record; change nothing |
| `--action rebalance` | rebuild short files (shards task) |
| `--action pull` | fetch repositories whose upstream moved (git task) |
| `--narrow` | allow re-cutting a file with a smaller code when too few clouds answer |
| `--rebuild-limit SIZE` / `none` | largest file an unattended repair will rebuild |
| `--max-repairs N` | most files one run rebuilds (0 = no bound) |
| `--disabled` | store the policy without running it |

Editing a policy keeps its history and last-run time, so changing the hour
doesn't make the folder due immediately.

```bash
sand automation list               # every policy, next run, last result
sand automation run /archive       # now, without waiting
sand automation remove /archive    # policy and history
```

### What a repair does

A repair keeps the file's **code**, not its placement. A 4-of-6 file whose sixth
cloud died goes back out as 4-of-6 over six clouds that answer, with the same
storage cost, loss tolerance and collusion threshold.

If too few clouds answer to keep the code, the file is **left alone** and named
in the report. `--narrow` allows re-cutting it (4-of-6 on four clouds becomes
3-of-4), keeping the storage ratio as close as possible and always leaving a
spare. It is off by default because narrowing can only be undone with another
full rebuild.

A repair is always a **rebuild**. The missing shard has no source to copy from,
so the whole file is downloaded, cut again and written out, and it passes
through memory on the way. For that reason, unattended repairs skip files larger
than a ceiling. The default comes from the machine's sizing: a quarter of its
memory, between 1 GB and 16 GB. You can change it under **Vault settings › This
machine** (see [configuration](../reference/configuration.md)) or per policy:

```bash
sand automation set /films --daily 03:00 --action rebalance --rebuild-limit 8G
sand automation set /films --daily 03:00 --action rebalance --rebuild-limit none
```

Files over the ceiling are counted, named, and left for you to repair from the
browser. `--max-repairs` caps the number per run, and the remainder is picked up
by later runs, worst first.

### Reading the results

```bash
sand automation run /archive
#   20 Aug 20:01  412 checked, 409 whole, 3 short, 0 past repairing
#                 3 rebuilt (1.4 GB), 0 failed, 0 left for later
```

| State | Meaning |
|---|---|
| **whole** | every recorded shard was found where the index says |
| **short** | at least one was missing, but enough remain to rebuild |
| **past repairing** | not enough to rebuild; unreadable until the missing clouds come back |

The last eight runs are kept on the folder. Each run also writes one line to the
server log, including on quiet nights, so you can tell "working" from "never
ran":

```
automation /archive: 412 file(s) checked, every part where it should be
```

### Schedules run only while the vault is unlocked

`sand serve` runs schedules, and only while the vault is open, because a locked
vault has no index or keys. Being due is worked out from the last run, not from a
timer, so a slot missed overnight runs as soon as the vault is next unlocked. A
machine that was off for three days owes one sweep, not three.

A machine meant to run schedules unattended needs a long idle timeout (the
default is 30 minutes):

```bash
sand serve --idle-timeout 720h
```

This keeps the keys in memory, which is what the idle lock exists to prevent,
so weigh it against what the folder holds. A check-only policy on a machine you
unlock every day anyway costs you nothing.

## Disk health for local folders

On Linux, a root-run `sand smart-scan` reads SMART data from every physical disk
and writes a small report (`/var/lib/sand/smart.json`, or `$SAND_SMART_REPORT`).
The service reads that report during its cloud health check and warns on any
local-folder account whose disk reports trouble. The quickstart installs it as
an hourly `sand-smart` systemd timer. On other installs,
`scripts/enable-disk-health.sh` sets it up. It needs `smartmontools`. Disks
without SMART data (SD cards, most USB sticks) are reported as unsupported, not
healthy. See [deployment.md](deployment.md).

## Stray parts and mislaid shards

When the set of connected clouds changes, the browser asks each account what it
holds and subtracts what the index accounts for. The remainder falls into two
cases, and only one of them is rubbish.

**Vault settings › Stray parts** runs a fresh scan whenever you open it. It
reports parts nothing points at, mislaid shards, and working files on this
machine, and hands each to the panel that deals with it. The scan lists every
account, so it doesn't start until you open the panel.

### A shard a disconnect mislaid: reattach

Disconnecting a cloud **drops the index records that name it** but leaves the
objects where they are. Reconnecting the same storage gives it a **new internal
ID**, so those records never come back on their own. The file then reports a
missing spare while the spare is sitting on a connected cloud.

The fix moves no data. A part's object name comes from the file's archive ID and
shard number, so re-recording it is one index write:

```bash
sand vault reattach                  # which shards, which files
sand vault reattach --cloud Dropbox  # list one account only (faster)
sand vault reattach --yes            # record them
```

The browser offers this first, before the sweep. It only ever adds records. It
won't record a shard that the placement policy forbids (a second shard of one
file on the same account under `strict`), or a chunked shard that is missing
chunks. It does not check object contents. Run `sand check` afterwards to have
the accounts confirm the shards are really there.

### A part no file wants: sweep

Deleting a file erases its shards from the accounts connected at that moment. An
account that was disconnected keeps its share, and when it is reconnected it
comes back as a new account that nothing links to those parts. They are
encrypted and harmless, but you are paying to store them.

When the browser finds any, it shows a dismissible notice:

> 41.6 MB across 12 parts on one of your clouds is holding storage no file in
> this vault points at any more. **Take a look**

The panel breaks this down per account and per abandoned archive, with
everything ticked. Untick anything you want to keep. Only size and location can
be shown, because the name was in the index entry that is gone.

```bash
sand vault sweep              # per account, plus working files on this disk
sand vault sweep --verbose    # every abandoned archive and working file
sand vault sweep --yes        # erase
```

Nothing is erased without `--yes`. The browser re-scans before deleting, and
skips anything that stopped being abandoned in the meantime.

**After a password change**, a file whose cloud was offline may have moved
without it. The part left on that cloud is still under the previous key. When
the cloud returns, the scan flags those parts and offers the sweep, which
finishes the password change:

> 2 parts (1.3 MB) on nas-attic are still under the previous key — left behind
> when the files were re-encrypted without that cloud. Cleaning them up
> finishes the password change. **Take a look**

### What the sweep refuses

"Nothing points at it" doesn't always mean "nobody wants it", so the sweep
refuses when:

- **The account carries an index this vault didn't write.** Another vault uses
  it. Those rows are greyed out with the reason.
- **An account didn't answer.** A part counts as abandoned only if every
  account agrees.
- **The vault holds no files and has never written an index to these
  accounts.** That's a reinstalled machine before recovery, and sweeping would
  erase what recovery needs (see [recovery.md](recovery.md)).
- **Another machine opening this vault wrote the index recently.** It may be
  uploading right now. The sweep waits until every machine has been quiet for
  the settling window plus the longest push delay (75 minutes by default). See
  [vault-management.md](vault-management.md#opening-the-vault-on-several-machines).
- **The object isn't named the way SAND Vault names parts**
  (`<archive-id>-pN.sand` or `<archive-id>-cNNNNNNN-pN.sand`). Other files in a
  shared folder are never counted.

Parts on a reconnected account whose records still exist (under the old account
ID) are not abandoned: finishing a recovery re-points them. Shards whose records
were dropped are sent to `reattach`, not to the sweep.

## Parts a cloud has lost

This is the reverse case: the index records a part that the cloud no longer
holds. It happens when a folder is emptied by mistake, a bucket is restored from
an old backup, or a sync client "tidies up". The file still shows full badges
and reads from its spares until a second cloud fails.

The same listing finds these, and a banner over the file list offers to put
them back. **Vault settings › Lost parts** checks on demand:

```bash
sand vault restore-parts                    # what is missing, per cloud
sand vault restore-parts --verbose          # every part, and why any can't be remade
sand vault restore-parts --yes              # remake and write back
sand vault restore-parts --yes --cloud Dropbox
```

This is **not a rebuild of the file**. Only the lost object is recomputed, from
m of the surviving shards, and written to the same cloud under the same name:

- Only the shards a read needs are downloaded, and only the lost object is
  uploaded, chunk by chunk.
- The index is not written.
- Nothing is erased or overwritten. Each object is checked by name before it is
  written, and files moved, re-encrypted or deleted since the scan are skipped.

Two shards of a long file that each lost different chunks can repair each
other. A chunk with fewer than m shards left can't be remade this way. Use the
file's [replica](storing-files.md#rebuilding-the-shards-from-the-replica) if it
has one.

A cloud that holds **none** of what the index says it should is marked *holds
none of them*. It may have been emptied, or the account may point at the wrong
folder. If the folder moved, fix the account (see
[accounts.md](accounts.md)) rather than writing parts into the wrong place.
This is why nothing is restored unasked.

Not covered: files in the pre-chunking format (convert them with
`sand vault convert`), the files of a locked sub vault (unlock it first),
thumbnails and replicas (remade by their own means), clouds that don't answer,
and disks that belong to another machine.

## Old versions on versioned buckets

A versioned bucket never overwrites or deletes. It keeps old versions under the
new one and puts a delete marker on top. **Backblaze B2 does this by default**,
and S3 and MinIO do when asked. SAND Vault rewrites `manifest.sand` on every
index change and deletes parts, so such a bucket fills with history that no
plain listing shows. Your usage bar may say 1 GB while the provider bills 10.

```bash
sand vault prune                          # what each bucket stores beneath what it shows
sand vault prune --verbose                # every object with old versions, and holds
sand vault prune --yes                    # erase them
sand vault prune --yes --account r2-cold  # one account
```

Prune only touches SAND Vault's own objects (the index backup and parts), and
never their current versions. Anything else in the bucket keeps its history, and
is counted. It also holds back a part the index still points at whose current
version is a delete marker. That part was deleted outside SAND Vault, and the
versions under the marker are the only copies left.

**Automatic:** use *Edit account › Erase old versions daily*, or
`sand remote edit b2-cold --auto-prune on`. This runs the same prune once a day
while `sand serve` has the vault open. The account's stats panel shows the last
run. It is only offered on accounts that can keep versions.

Pruning doesn't stop new history from building up. To stop it at the source, set
the bucket's lifecycle to keep only the latest version. On B2 that is *Keep only
the last version of the file*. On S3, use a rule that expires noncurrent versions
and removes expired delete markers.

## Working files left on the machine

An upload is spooled in full to the vault's own folder before it is sent
(`/var/lib/sand` for the service, `~/.sand` on a desktop). Each chunk carries the
whole file's hash, which isn't known until the last byte. The spool is deleted
when the upload ends, but a killed process or a power cut leaves it behind at
full size.

> 31.6 GB of working files sit in this vault's own folder on this machine.
> **Tidy up**

Erasing these changes nothing in the vault. Only names SAND Vault writes itself
are considered. A file is offered only after it has been untouched for an hour,
because a spool still being written looks like a running upload. Files the
current process is writing are never listed. `sand vault sweep` reports and
erases them too.
