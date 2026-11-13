# The vault

This page covers the vault file itself and the machinery around it: its layout
and keys, how it locks and writes, the copy of the index each account carries,
how a lost vault is recovered, how stray and lost parts are found, how one vault
runs on several machines, sub vaults, and what a password change does. The code
is `internal/vault`. Index-level features (search, organizing, film details and
so on) are in [index.md](index.md); the user-facing steps are in the
[vault management](../guide/vault-management.md) and
[recovery](../guide/recovery.md) guides.

## The vault file

The vault is one JSON file, `~/.sand/vault.sand` by default (`--vault`, or
`/var/lib/sand` under the service; see
[configuration](../reference/configuration.md)). It is the only persistent state
Sand Vault keeps on the machine, apart from a sealed read-history sidecar
(`vault.sand.reads`). Everything meaningful in it is encrypted.

### Layout

```json
{
  "version":      4,
  "kdf":          { "salt": "…", "time": 3, "memory": 65536, "threads": 4 },
  "check":        { "nonce": "…", "ciphertext": "…" },
  "data_key":     { "nonce": "…", "ciphertext": "…" },
  "data_key_id":  "…",
  "retired_keys": [ { "id": "…", "key": { "nonce": "…", "ciphertext": "…" } } ],
  "providers":    { "nonce": "…", "ciphertext": "…" },
  "manifest":     { "nonce": "…", "ciphertext": "…" },
  "settings":     { "nonce": "…", "ciphertext": "…" },
  "policy":       "strict",
  "default_accounts": ["…"],
  "default_scheme":   "…",
  "manifest_backup_delay_seconds": 5,
  "health_check_minutes": 60,
  "sub_vaults":   [ { "id": "…", "kdf": { … }, "check": { … },
                      "data_key": { … }, "data_key_id": "…", "retired_keys": [ … ],
                      "section": { "nonce": "…", "ciphertext": "…" } } ]
}
```

Sealed sections are AES-256-GCM under the **vault key**, which is Argon2id of
the password (t=3, 64 MiB, 4 threads; see
[cryptography](cryptography.md)).

| Section | Holds | Why it is sealed |
|---|---|---|
| `check` | The constant `SAND-VAULT-OK` | Opening it is what verifies the password |
| `data_key` | 32 random bytes | The key file parts are actually encrypted under |
| `retired_keys` | Earlier data keys, while files are still on them | They open parts that have not been re-encrypted yet |
| `providers` | Account configs, **credentials included** | A stolen vault file must not yield cloud access |
| `manifest` | The index: files, folders, placement, thumbnails, film details, automations, repositories, the machine register | File names and folder structure are themselves sensitive |
| `settings` | Preferences that are secrets: the film database key, the assistant's model-server settings, retained recovery-kit codes, import/export sources (SFTP machines) | Credentials for other services; never copied into a manifest backup |
| `sub_vaults[].section` | One sub vault's whole index | Sealed under the sub vault's **own** password |

In the clear: the KDF parameters, the policy, key generation IDs, default
accounts and scheme (random account IDs that name nothing on their own), and
operational knobs (backup delay, health-check schedule, the last kit export's
date and account IDs). None of them says what is stored or where.

### Format versions

`StoreVersion` is 4 and the oldest readable is 2 (`store.go`). Every field added
since version 2 is additive: version 3 added sub vaults, version 4 added
replicas. Older vaults are read as they stand and upgraded on the first write.

The number still has to go up each time. An older build would parse a newer
file, drop the fields it does not know, and write it back without them, which
for sub vaults destroys the only copy of what they hold and for replicas turns
every replica object into a stray. Refusing an unknown version is what an older
build already does, so bumping the number is what makes the silent discard
impossible.

## Two keys, not one

File parts are encrypted under the random `data_key`, which is wrapped by the
password-derived vault key. That buys two things:

- **File encryption does not inherit a weak password.** Content is protected by
  256 random bits whatever was typed.
- **The vault can hold several key generations at once.** Every file records
  the generation it was sealed under (`Entry.KeyID`). That is what makes a
  password change real ([below](#changing-the-password)), what lets a
  recovered vault move off an inherited key, and what lets a file move between
  the main vault and a sub vault without re-encrypting on the spot.

## Locking

Unlocking derives the vault key, opens every section and holds the keys in
memory. Locking zeroes them and drops everything derived from the files: the
decrypted index, cached thumbnails and cached chunks. A locked vault can list
nothing and fetch nothing, and a reader opened before the lock stops answering
rather than carrying on with a key it captured.

Operations that would be destroyed by losing the keys halfway (an import, a
rebuild) take a key hold (`holdKeys`); a `Lock` that arrives meanwhile is
remembered and applied when the hold is released.

The server locks the vault once every browser session has been idle past the
timeout (default 30 minutes, `sand serve --idle-timeout`). Background work that
must not be cut off (a paced re-encryption step, an automation sweep) counts as
activity for that timer while it runs. See [server](server.md).

## Atomic writes

The vault file is rewritten in full on every change: a temp file in the same
directory, `fsync`, then `rename`. An interrupted write cannot corrupt the index
that maps files to their parts, which is the one piece of state whose loss
would strand everything.

## The manifest backup

Parts are encrypted under the random data key, so losing the vault file used to
mean every part on every account was permanently opaque, password or not. So a
copy of the index travels with the data: each connected account holds
`manifest.sand`, rewritten whenever the index changes.

```json
{
  "magic":   "SAND-MANIFEST",
  "version": 1,
  "kdf":     { "salt": "…", "time": 3, "memory": 65536, "threads": 4 },
  "check":   { "nonce": "…", "ciphertext": "…" },
  "payload": { "nonce": "…", "ciphertext": "…" }
}
```

The envelope carries its **own** KDF parameters, because the vault is exactly
what its reader has lost. The password alone opens it.

### What it carries

The payload is a `Snapshot` (`backup.go`):

| Field | Holds |
|---|---|
| `data_key`, `keys` | The current data key and every retired generation the index still names, so a vault lost mid-migration recovers both halves |
| `manifest` | The file tree and which account holds which part under which key |
| `accounts` | Account ID, kind, name, when it was connected, and the machine a local account lives on. **Never credentials** |
| `policy` | The placement policy |
| `sub_vaults` | The sealed sub-vault records, verbatim |

Credentials are excluded on purpose: a copy sits in every account, so including
them would make one compromised account a master key to all the others. The
`settings` section (film database key, kit codes) is excluded for the same
reason. Film *details* do travel, because they are index.

### When it is pushed

A push is a round trip to every account, and writes come in bursts (a bulk
delete, a folder of photos). A provider asked to replace the same object a
hundred times a minute starts answering 429, so pushes are paced
(`backuppace.go`):

- The first change after a quiet spell is pushed at once.
- Changes within the gap wait for it and go out as one push.
- A change that lands while a push is out goes once that push has finished and
  the gap has passed again.

The gap is 5 s by default (`DefaultBackupDelay`), anything from 0 to an hour
(`MaxBackupDelay`), set from **Vault settings › Index backup**,
`sand vault backup --every`, or `POST /api/vault/backup/delay`. The backup can
be switched off entirely (`sand vault backup --disable`), which also means no
other machine can share the vault.

A command about to exit calls `AwaitBackupSync`, which starts any waiting push
at once, and `sand serve` does the same on an orderly stop for up to 30 s. A
vault locked with a push waiting pushes nothing; the next unlock brings the
copies up to date. The vault file itself is never behind: it is written before
any request is answered. Pacing only concerns the replicas.

### What it costs, and where it refuses

Every copy is a password away from the data key. One compromised account plus a
cracked password yields the tree, the placement map and whatever that
account's own shards contain. Rebuilding a whole file still needs shards from
other accounts, so the split remains a real second factor. Argon2id is what
stands between the envelope and a guessed password.

Two guards (`backup.go`):

- **Redundant policy with fewer than three accounts.** One account can then hold
  enough shards to rebuild a file, so adding the data key would leave the
  password as the only protection. The backup refuses to write and erases any
  copies it had written (`backupRefusalLocked`).
- **Another vault's backup.** An account already holding a `manifest.sand` this
  vault's key cannot open belongs to a different vault, and connecting an
  account to a second vault must not destroy the first vault's recovery data
  (`guardForeignBackup`). After a password change or a kit import the vault
  sets `BackupNeedsForce` and remembers the salts its own earlier copies were
  sealed with (`PreviousBackupSalts`), so it can replace its own stale copies
  without mistaking them for a stranger's. `sand vault backup --force` and
  `POST /api/vault/backup` with `force` claim an account explicitly.

## Changing the password

Re-wrapping the data key under a new password would leave every part encrypted
exactly as before, so anyone holding the old password and an old copy of the
vault file (or any `manifest.sand`) could still read them. So `sand vault passwd`
mints a **new** data key and rebuilds every file onto it: gather, decrypt under
the old key, re-encrypt, scatter, erase the old parts.

That cannot be atomic, so the vault carries both keys while it runs:

| On disk | Holds |
|---|---|
| `data_key` + `data_key_id` | The current generation; every new upload uses it |
| `retired_keys[]` | The generations files are still on, wrapped under the **new** password |
| `manifest.entries[].key_id` | Which generation each file answers to |

The change itself is one atomic write: new key minted, old key kept beside it,
every section re-sealed under the new password. From that moment the old
password opens nothing. Files then move one at a time, each committed on its
own, and a retired key is dropped from memory and file the moment no entry
names it. As a result:

- the password really is changed when the command returns, and every file stays
  readable throughout;
- an interrupted migration resumes (`sand vault migrate`) rather than restarts;
- a file whose part sits on an account the last health check found down is
  re-encrypted from the parts that answer if they are enough, and the part left
  behind is **recorded** as a stray under the previous key
  ([below](#parts-left-under-the-previous-key));
- thumbnails, sealed under the retiring key, are dropped and redrawn rather than
  re-encrypted.

The cost is bandwidth: everything is downloaded and uploaded again.
`--no-migrate` changes the password now and leaves the files for later, at the
price of the old password still reading the parts that have not moved. A change
can also carry a **plan** (so many bytes a day, these folders first) that the
server works through in the background. A second change is refused with
`ErrRekeyPending` while any file is on a key that vault retired. The pacing, the
plan, the progress UI and the one-change rule are designed in
[paced-rekey.md](paced-rekey.md).

## Recovery

### Routes back

In increasing order of what has survived:

| You have | Command | You get |
|---|---|---|
| `manifest.sand` + password | `sand manifest ls` | The file tree and placement map |
| Enough part files + `manifest.sand` + password | `sand restore --manifest` | A whole file, offline, no accounts |
| The accounts + password | `sand vault recover`, or the browser's prompt | The whole vault, files openable again |
| The accounts you were missing | `sand vault recover --resume` | The parts that were out of reach; no password needed |
| A recovered vault | `sand vault reclaim` | The same files on a key of your own, on clouds you pick |
| A recovery kit + its code | `sand vault kit import`, or the lock screen's kit door | The whole vault **with every cloud reconnected** |

**The offline route needs the manifest, not just the parts.** A shard is sealed
under the vault's random data key, and `manifest.sand` is the only thing outside
the vault file that carries it, so `sand restore` refuses chunked parts without
`--manifest` rather than prompting for a password that opens nothing. A chunked
file has one object per chunk per shard (`<archive>-cNNNNNNN-pN.sand`); the
restore reads each chunk's `k` off the parts it is given, and missing chunks are
an error naming the chunk count rather than a short file.

**`Vault.Recover`** runs against a fresh, empty vault with the accounts
reconnected. Reconnecting gives every account a new ID (a credential says
nothing about which account it was last time), so rather than trusting
remembered IDs it lists each account and re-points every shard record at
whichever account actually holds that object key. It adopts the old data key,
so new uploads join the existing files, and rewrites the backups under its own
password. It refuses a vault that already holds files, because adopting a key
would strand them. Sealed sub vaults' records cannot be re-pointed without their
passwords, so the translation is written into the main manifest as
`account_remap` and applied the first time each sub vault is opened.

### Prompting for it

Someone on a reinstalled machine with an empty file list has no reason to know
recovery exists. So `Vault.ScanForRecovery` asks each connected account two
questions that need no password: what do you hold, and does your
`manifest.sand` open under *this* vault's key? A readable envelope that this
vault cannot open was written by another vault, which is the signature of this
disaster, and the browser prompts on it. The scan only runs where it could
matter: an empty vault, or one with shard records pointing nowhere.

The prompt fires on the first account reconnected, which is by definition one
short, so it asks for the *next* cloud rather than a password it cannot use
yet. Connecting happens inside the dialog, each new account re-runs the scan,
and the next step follows from the result.

`RecoveryReport` leads on the shortfall: `Recoverable` against `Files`,
`RecoverableBytes` against `Bytes` (the bytes are usually the answer to "how
bad is this"), then the files that came up short and the accounts whose absence
caused it. An account whose return would change nothing is listed without
`Blocking`.

### Finishing a partial recovery

A partial recovery leaves a complete index, part of it out of reach:
`Stats.Unresolved` counts shard records naming accounts the vault is not
connected to, `Stats.Stranded` the files that cannot be opened because of them.
`Recover` cannot run twice (by then files depend on the key it adopted), so
`Vault.Reconcile` (`sand vault recover --resume`,
`POST /api/vault/recovery/resume`) does the small half: list the accounts,
re-point what now has somewhere to point, persist. A shard whose connected
account did not list it keeps its record; one failed listing should not throw
placement away.

### Reclaiming the inherited key

The recovered vault adopts the lost vault's data key because that is the only
thing that opens the parts already stored. Every old copy of `manifest.sand`
carries that key, including any taken off an account before the new vault
existed, so the files stay readable by whoever could read them before. The
vault records this (`InheritedKeyID` in the store, `Stats.InheritedKey`) and
keeps saying so.

`Vault.Reclaim` (`sand vault reclaim`, `POST /api/vault/reclaim`) closes it with
the same two moves a password change uses: `RotateDataKey` mints a fresh
generation and retires the adopted one (no password needed: the wrapping key is
not changing), then `MigrateFilesTo` rebuilds every file onto it. Because every
file is gathered and scattered anyway, it is also the cheap moment to change
placement, so it takes the accounts to land on (`--account`). The selection is
validated against the placement planner *before* the key rotates, so a
placement that cannot hold a file fails with the vault untouched. It is
interruptible on the same terms as a password change.

### The recovery kit

Every route above starts with the accounts reconnected by hand, because
`manifest.sand` cannot carry credentials. A **recovery kit** is the manifest
backup that never touches a cloud and can therefore carry them:
`sand vault kit export` writes a sealed zip and prints a 25-character code, and
importing it reconnects every account under the ID it already had, so the shard
records (sealed sub vaults' included) are correct on arrival. It then reads the
newer `manifest.sand` off the clouds, so an old kit still comes back current.
See [recovery-kit.md](recovery-kit.md).

## Strays: reconciling the index with the clouds

Every operation works outwards: the index says what exists and the accounts are
told. These scans work the other way, comparing what the accounts hold with
what the index accounts for. There are three ways they can disagree, plus two
housekeeping cases, and each has its own scan:

| Disagreement | Scan | Repair |
|---|---|---|
| An object no file owns | `ScanForOrphans` | `SweepOrphans` erases it |
| An object a file owns but has no record for | `ReattachShards` / `ScanForStrays` | Write the record back |
| A record the account does not back up | `ScanForLostParts` | `RestoreLostParts` remakes the object |
| Spools and temp files in the vault directory | `ScanForLeftovers` | `SweepLeftovers` |
| Old versions on a versioned bucket | `ScanForStaleVersions` | `SweepStaleVersions` |

### Orphans

Erasing a file deletes its parts from the accounts **connected at the time**.
Disconnect a cloud, delete files without it, and its share stays where it is,
unreadable and counting against quota. `Vault.ScanForOrphans` lists each
account and subtracts what the index owns.

**Ownership is by archive ID, not object key or account.** Every part key starts
with the archive's random 128-bit ID, so the owned set is the archive IDs the
vault still points at: from the main index, every open sub vault's index, and
the `Inventory` of each shut sub vault. So a chunked file's many objects are
recognised from one record per shard; a part whose account was reconnected is
not an orphan (its archive is still named, under an old account ID, which is
what `Reconcile` fixes); and a locked sub vault's files are not orphans either.

`SweepOrphans` erases what a scan reported, grouped by archive. It **re-runs the
scan itself** rather than trusting the caller's figures, because a file may have
been uploaded or a sub vault opened in between; a target that has stopped being
abandoned is skipped and reported. Progress is readable while it runs
(`GET /api/vault/orphans/erasing`). The CLI is `sand vault sweep`.

### Parts left under the previous key

One kind of abandoned part is deliberate. A re-encryption that finds one of a
file's accounts unreachable moves the file without it (if the answering parts
can rebuild it) and leaves the old part where it is: sealed under the retiring
key, named by nothing, readable by the previous password. It records that part
in the same write that stops naming it (`StalePart`, `staleparts.go`): account,
archive and shard, size, key generation, path and when. An erase that failed on
an answering account is recorded the same way. The record keeps no key alive;
it exists so the part is a stray with a name rather than a leak.

The orphan scan flags such objects `previous_key` with the file's path, and
totals them separately, because they are the reason to sweep today. Vault stats
and the re-encryption status count them and say whether each account is
answering now, so the browser re-runs the scan the moment a cloud holding
recorded parts comes back. The record is dropped by the sweep that erases the
part, by a complete listing that no longer shows it, or by disconnecting the
account (after which it is an ordinary orphan there).

### Where the sweep refuses

"No index points at it" stops meaning "abandoned" in four states, all named by
`orphanGuard`:

| State | Why it is different |
|---|---|
| The account carries a `manifest.sand` this vault cannot open | Another vault is using it |
| The account could not be listed | A part is abandoned only if every account agrees |
| The vault holds no archives and the accounts carry no index it wrote | This is the opening scene of a recovery; sweeping would erase what it needs. A vault that merely deleted its last file has its own backup on those accounts |
| The object is not named like a part | Only `<archive>-pN.sand` and `<archive>-cNNNNNNN-pN.sand` are counted; a shared folder keeps its own files |

The first two grey out one account with the reason; the third withholds the
sweep entirely. The sweep is also refused while another machine wrote the index
recently ([below](#when-the-folding-happens)). Nothing deletes without a second
call.

The app runs its own scan when the set of connected accounts changes and shows
a dismissible banner; **Vault settings › Stray parts** is the door that works
when the banner has gone, and runs a fresh scan when opened.

### Mislaid shards: reattaching

`RemoveProvider` drops shard records pointing at the disconnected account, so
the index keeps telling the truth, and leaves the objects alone. Reconnecting
mints a new ID, and `Reconcile` re-points records rather than inventing them,
so a record that was **dropped** has nothing to re-point and the file goes on
reporting degraded while the spare sits on a connected cloud.

`Vault.ReattachShards` (`sand vault reattach`) closes that. Parsing the shard
number out of the key as well as the archive ID puts every object in one
bucket:

| The index… | Then the object is |
|---|---|
| does not know the archive | an orphan |
| knows the archive and has a record for that shard | accounted for |
| knows the archive, has no record for that shard, and can be written | **mislaid**: reattach it |
| knows the archive but cannot be written (a thumbnail pack, a shut sub vault) | left alone |

Writing the record back moves nothing: object keys derive from archive ID and
shard number alone, so the bytes are already where a record would say. That is
why it carries none of the sweep's refusals; a file can only leave with more
shards than it had. It still declines four things that would make the index
claim something false: a shard number the file's scheme has no room for; a
chunked shard only partly present; under `strict`, a shard on an account that
already holds one of that file's; and a second copy of a shard already being
taken (what an interrupted relocation leaves). `entryForReattachLocked`
re-checks each row under the write lock, since the scan ran without it.

Disconnected accounts are also remembered (`Manifest.Departed`, `departed.go`)
with a small sample of the parts each held. A returning account found holding
one of those exact parts is that storage, so what was kept about the old ID
(read history today) can follow it to the new one.

**Asking one cloud.** Listing every account is the wrong cost when someone is
looking at thousands of short files and knows which cloud has the parts.
`Vault.ScanForStrays(ctx, providerID)` lists one account's parts folder against
the same owned set, and `ReattachOptions.ProviderID` aims the repair at it. It
carries nothing of the sweep: one listing cannot say what is abandoned. An ID no
connected account carries is `ErrNoAccount` rather than an empty answer. It is
reached from the short-files list ([index.md](index.md#short-files)),
`GET /api/vault/orphans/strays`, and `sand vault reattach --cloud`.

### Lost parts: records the account does not back up

The reverse question: a part the index records on an account that the account
does not list. A folder emptied by hand, a bucket restored from an old backup, a
sync client tidying up. Nothing else notices; the file reads back on its spares
until a second account fails. `Vault.ScanForLostParts` takes the index and
subtracts each account's listing; it downloads nothing.

`RestoreLostParts` puts each lost object back without rebuilding the file. Any
k shards rebuild any one shard (`archive.RemakeShard`), so the lost object alone
is cut again from k survivors and written back under its old name on the account
the index already names: k objects down and one up, no index write, nothing
erased or overwritten. An account that lists none of what it should is flagged
(`LostAccount.Emptied`), because "emptied" and "pointed at the wrong folder"
look identical and writing parts into the wrong folder helps nobody; that is why
the restore is a separate call. Not covered: the pre-chunking format, thumbnail
packs, replicas and shut sub vaults. CLI: `sand vault restore-parts [--yes]
[--cloud]`; HTTP: `/api/vault/lost-parts/*`.

### Working files in the vault directory

`Vault.ScanForLeftovers` asks the same question of the one disk Sand Vault
writes itself: the vault's directory. The answer is often the bigger one. An
upload spool is the whole file, in plaintext, because the whole-file SHA-256
must be known before the first chunk is sealed, so `UploadStream` spools the
stream beside the vault first. The spool is removed on every exit path, but a
killed process is not an exit path. The same applies in smaller ways to the
conversion spool, the atomic-write temporaries, read-history saves and the git
packing workspace.

A file is offered only if both hold:

| Test | Rules out |
|---|---|
| The name matches `leftoverPattern`: `.sand-(upload\|convert\|reads\|vault\|git)-` plus the digits `os.CreateTemp` appends | Anything Sand Vault did not write, including the vault file itself |
| Nothing has written it for an hour (`leftoverSettling`) and this process is not holding it (`Vault.spools`) | An upload still running, possibly from the CLI sharing the directory with the service |

A file inside the window is still shown, with the reason. `SweepLeftovers`
re-runs the scan and only acts on names its own scan produced. None of it
touches the index or needs a password. It rides along on the orphan scan
(`OrphanScan.Leftovers`) for the cost of one directory read; erasing is its own
`POST /api/vault/orphans/leftovers`.

### Old versions on versioned buckets

A bucket with versioning on (Backblaze B2 by default, S3 and MinIO when asked)
never overwrites or deletes: a `Put` adds a version and a `Delete` adds a marker,
and every version is billed until erased by version ID. A plain listing shows
none of it. Sand Vault makes two kinds: one `manifest.sand` version per index
change, and every deleted part still sitting under its marker.

`provider.Versioner` (`ListVersions`, `DeleteVersion`) is implemented by the S3
backend; on an unversioned bucket it degrades to the plain calls.
`Vault.ScanForStaleVersions` asks every account that implements it, and
`classifyVersions` (pure, no I/O) sorts the results:

| The version is | Treatment |
|---|---|
| The latest live version of any key | The object; never touched here |
| Under a key Sand Vault did not write | Someone else's; counted (`Other`), never touched |
| A superseded version or marker under `manifest.sand` | Stale |
| A superseded version under a part, or the history of a deleted part nothing points at | Stale |
| A marker on top of a part the index still points at | **Held**: the versions beneath are the only copies left |
| Anything on an account holding another vault's `manifest.sand` | **Held**, the whole account |

`SweepStaleVersions` (`sand vault prune`, `POST /api/vault/versions`) re-runs the
scan and erases **data first, markers last** per account: erasing a marker first
and then being interrupted would resurrect an object as an orphan, while the
other order leaves at worst a marker over nothing.

An account can opt in to a daily automatic prune (`provider.Config.AutoPrune`,
refused on a backend that is not a `Versioner`); the server's `pruneLoop` runs
`Vault.AutoPrune` every `AutoPruneInterval` (24 h) while the vault is open.
Daily because listing every version is billed per thousand entries. The last
outcome per account lives in memory. Stopping the history piling up is the
bucket's lifecycle setting, not Sand Vault's, and the command says so.

## One vault on several machines

The same vault can run on a desk and on a Raspberry Pi at once: same password,
same clouds, same tree, no server between them. Each machine keeps its own vault
file, and the `manifest.sand` copies on the accounts are the channel: a machine
reads the copies it can reach, folds each into its own index, and pushes the
result back. The code is `instance.go`, `reach.go`, `merge.go` and `sync.go`.

### Machine names and local accounts

Every machine opens vaults under a name: `SAND_INSTANCE`, or the hostname.
Accounts on a backend reached *through* the machine rather than over the network
(`provider.Spec.Local`: the local folder, the synced-folder family, iCloud,
Proton's client) record the machine that connected them (`Config.Host`). Another
machine does not try that path: the account is reported as living on that
machine rather than down, the health figure leaves it out
(`HealthReport.Elsewhere`), and its shards are marked `elsewhere` rather than
missing. An account from before machines had names has no `Host`, and the first
machine that finds the folder claims it.

The index keeps a register of machines that have written it
(`Manifest.Instances`): name, last write, and which accounts it can reach. From
that and the shard records a listing can say, with no network call, which files
this machine cannot rebuild and which machine can (`Listing.Unreachable`, one
`Reach` per file, counting the replica since a read falls back to it). The
browser greys such a row out and names the machine.

### Vector clocks and merging

`Manifest.Clock` has one counter per machine, bumped by every write that machine
makes. If one copy's clock dominates, it is taken whole. If both machines wrote
since they last agreed, the rows are combined (`mergeIndexes`):

- **A row on both sides**: decided in two halves. `IndexedAt` is stamped by the
  index write itself on exactly the rows whose content changed
  (`stampIndexLocked` compares per-row fingerprints, so no code path can forget);
  `PlacedAt` is stamped the same way from the placement alone (parts, replica,
  key, scheme). Where the file is filed follows the later `IndexedAt`; where
  its bytes are follows the later `PlacedAt` (`mergeRow`). A rename on one
  machine and a re-encryption on the other both survive.
- **A row on one side with a later tombstone on the other**: gone. Deletes record
  the ID in `Manifest.Tombstones` for 90 days; the deleting machine erased the
  parts. A row stamped after its tombstone came back (out to a sub vault and back,
  say) and stands. Explicit folders have their own tombstones.
- **A row on one side and no tombstone**: kept.
- **Two rows landing on one path**: both survive; the later takes the ` (2)` name,
  decided by stamp so both machines rename the same row.

Thumbnail packs follow their `UpdatedAt`; per-file and per-folder records (films,
repositories, automations, folder art) merge by key and drop with what they
describe. Sub-vault records merge by ID with inventories **unioned**: a sub
vault's section does not travel, so its inventory is the only account of what
it holds that reaches another machine, and a stale shorter one would make the
other machine's files look like orphans. A sub vault deleted on one machine is
tombstoned and released on the other.

Guards that keep the clocks honest:

- A copy with no clock was written by an older build; it is left alone, since it
  carries no tombstones and combining it would resurrect deletions.
- A copy ahead of this machine under *this machine's own* counter, or with the
  same clock and a different write time, came from another machine using the
  same name (two Pis both called `raspberrypi`). It is combined rather than
  taken, with a warning to set `SAND_INSTANCE`.
- A combine is pushed only when the freshest copy lacks something this machine
  holds, which stops two machines trading one index forever. An adopted copy is
  written locally and not counted as a write (`indexWrite`) for the same reason.

Disconnecting an account on one machine prunes no shard record another machine
still reaches. Nothing is lost in a race: what a machine wrote is in its own
vault file, and the next fold carries it again.

### When the folding happens

`SyncIndex` / `pullIndex` run:

- at every unlock;
- every 5 minutes once another machine is in the register (`PeerSyncInterval`),
  and hourly until one is (`LoneSyncInterval`);
- before every push once another machine is known, so what goes out includes
  what came in;
- before an orphan scan, which would otherwise read another machine's fresh
  uploads as orphans. The sweep is refused while another machine wrote the index
  within the settle window plus `MaxBackupDelay`, since its new files have parts
  on the accounts and no row in any copy until its push lands;
- on demand: `POST /api/vault/sync`, `sand vault sync`, the Machines dialog.

A machine new to the register announces itself with one write at its first pull,
so a read-only machine is still known. A copy sealed under a password this vault
does not have is counted and left alone. `sand vault instances` and
`GET /api/vault/instances` show the register.

### Joining a second machine

The copies are sealed under the vault key, so both machines need the same
password **and** KDF salt. Copying the vault file gives that. So does importing a
recovery kit as a sibling (`--second-instance`, or the checkbox in the import
dialog; `KitImportOptions.Sibling`): the kit's salt is kept, the password must be
the vault's own, and the copies on the accounts are folded in rather than
claimed. Changing the password on one machine changes the vault key; the others
then read every copy as foreign until re-joined from a fresh kit, and the pull
says so.

### What does not travel

- A sub vault's section: one made on machine A appears on B shut
  (`adoptSubVaultsLocked` takes the record), and what is done inside it stays on
  the machine that did it.
- Account credentials: a cloud connected on A is listed on B by its shard records
  and register entry ("readable on study-pc"), and connected there by hand if
  wanted.
- Anything, if the index backup is off: there are no copies to read, and the pull
  says so.

## Sub vaults

A sub vault is a vault inside the vault with its own password, for what should
not be readable by whoever holds the main password, which, once a vault is
mounted as a drive on an open laptop, is a wider set of people than it sounds.
Its section is sealed under Argon2id of its own password and nothing else.

Three properties are the design:

- **Its own namespace.** A sub vault's files live in a separate tree with its own
  root, so nothing collides with, or has to be checked against, paths that cannot
  be read while it is shut. In code this is `vault.Scope`: the zero value is the
  main vault, and path-addressed reads take one. Anything addressed by file ID
  takes none; IDs are unique across vaults and resolve against whatever is open,
  so reading, moving, deleting, streaming and thumbnailing are unchanged.
- **Its own data keys.** A sub vault has its own generations, using the same
  machinery as password changes, including deferred re-encryption.
- **Never on the main WebDAV share.** A `davfs.FileSystem` is fixed to one scope
  when built and never reads one from a request; the share at `/dav` is built
  over `MainScope`, so the guarantee holds by construction. A sub vault gets its
  own share at `/sub/<name>/`, opened by its own password (see
  [server](server.md)).

A sub vault can have its own default clouds and scheme (`subdefaults.go`),
stored in its own sealed index; with none it follows the vault's.

### What the main password can see

It sees that a sub vault exists, its label, when it was made, and an
**inventory** of the objects it owns (`SubVaultMeta.Inventory`: archive IDs, which
account each part is on and its size). No path, file name, per-file size, type or
content.

The inventory is the price of two things. A sub vault whose password is forgotten
can still be deleted, parts and all, instead of leaving unattributable objects
forever. And disconnecting an account can still refuse when it would strand
files, which would silently pass if a shut sub vault's parts could not be
counted. `SubVaultMeta.BorrowedKeys` similarly names other vaults' key
generations its files are still on, so the main vault does not retire one a shut
section still needs.

### Assignment

Moving a file or folder between vaults keeps its path: `/Papers/2019` arrives at
`/Papers/2019` in the sub vault, folders made as needed, so sending it back puts
it where it was (`POST /api/assign`, `sand sub assign`). The index move is one
instant write; nothing is uploaded, and entries keep their key generation until
the re-encryption behind the move catches up.

**No key crosses the boundary.** An earlier version copied the source's
generation into the destination so a moved file read immediately. That handed
the destination the key to everything *else* in the source, and since every
retired key is replicated to every account in the backup, main password plus any
one account was a complete break. Now nothing is adopted. A moved file reads
while both vaults are open (key lookup searches every open vault); once the
source is shut it reports `ErrSubVaultLocked` until it has been re-encrypted.
The file is only independent of its old vault once it is on the destination's
own key and the old parts are erased.

### Importing a vault found on an account

An account used before carries some vault's `manifest.sand`. `Recover` refuses
a vault that already holds files, but importing the found vault **as a sub vault**
dissolves that restriction: a snapshot is an index, a data key and a password
that opens it, which is what a sub vault is. Connecting an account reports only
whether its backup is ours; everything else is inside the envelope. The import
(`sand sub import <account>`, `POST /api/vaults/import`) asks for the old
password to open the snapshot and a new one for the sub vault; the snapshot's
data key is adopted as it stands. Parts are matched to accounts by listing, as
in a recovery. `sand sub scan` lists accounts holding another vault's index and
`sand sub forget` stops reporting one.

### Backup and recovery

A backup carries the sealed records verbatim, so a recovery restores the vault
with its sub vaults present and shut, each opened afterwards by its own
password. The account remap for their sealed shard records is applied on first
open (`account_remap`, [above](#routes-back)). A kit import needs no remap at
all, because it preserves account IDs.

A sub vault's password change works like the main vault's and refuses a second
change while one is pending; its re-encryption runs in the foreground
(`sand sub passwd`, or the row's finish button, `POST /api/subvaults/{id}/migrate`),
since a sub vault can only be worked while it is open.
