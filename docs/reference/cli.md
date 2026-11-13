# CLI reference

Every command of the `sand` binary, the CLI and server of Sand Vault, taken from
the cobra tree in `cmd/sand` and checked against `sand <command> --help`. The
binary prints the same help: `sand <command> --help` is the authority when the
two disagree.

For how the pieces fit together see [architecture](../architecture.md); for
environment variables, file locations and tuning knobs in one place see
[configuration](configuration.md).

**Contents**

- [Global flags](#global-flags)
- [Vault](#vault)
- [Sub vaults](#sub-vaults)
- [Converting old files](#converting-old-files)
- [Recovery](#recovery)
- [Housekeeping on the accounts](#housekeeping-on-the-accounts)
- [Accounts](#accounts)
- [Files](#files)
- [Replication](#replication)
- [Snapshots](#snapshots)
- [Automation](#automation)
- [Repositories](#repositories)
- [Server](#server)
- [Disk health](#disk-health)
- [Standalone mode](#standalone-mode)
- [Version](#version)
- [Passwords in scripts](#passwords-in-scripts)
- [Exit status](#exit-status)

## Global flags

These are persistent flags: every command accepts them.

| Flag | Default | Meaning |
|---|---|---|
| `--vault <path>` | `$SAND_VAULT`, else `~/.sand/vault.sand` | The vault file to open |
| `--in <name-or-id>` | main vault | Work inside a sub vault. Asks for its password, or reads `SAND_SUB_PASSWORD` |
| `-h`, `--help` | | Help for the command |
| `-v`, `--version` | | Root command only: prints `sand version <version>` |

`--in` matches a sub vault by ID or by name, ignoring case. It applies to the
ordinary commands (`ls`, `find`, `put`, `get`, `mv`, `rm`, `relocate`,
`check`, `vault defaults`, `snapshot`, `git`, `automation` and so on):

```bash
sand ls --in Taxes /
sand put --in Taxes ./p60.pdf --path /Papers
sand get --in Taxes /Papers/p60.pdf -o ./
```

`sand remote` has the aliases `sand account` and `sand cloud`; `sand sub` has the
alias `sand subvault`.

Paths inside the vault are absolute (`/photos/2024`). Where a command takes
`<path-or-id>`, it also accepts a file's entry ID, which `sand ls -l` and
`sand find -l` print.

---

## Vault

Creating the vault and the settings that apply to all of it. Design: [vault
internals](../design/vault.md).

### sand vault init

```
sand vault init [--policy strict|redundant]
```

Creates the encrypted index at the `--vault` path. Asks for a new password
twice (once when it comes from `SAND_PASSWORD` or a pipe). The password
protects the index and the account credentials; file contents are encrypted
under a random data key held inside the index.

| Flag | Default | Meaning |
|---|---|---|
| `--policy` | `strict` | `strict`: never two parts of a file on one account. `redundant`: always store every part, doubling up on accounts when there are too few |

```bash
sand vault init
sand remote add local --name usb --set path=/mnt/usb/sand   # then connect accounts
```

### sand vault status

```
sand vault status
```

Prints the vault path, placement policy, number of accounts, files and folders,
original size and stored size.

### sand vault passwd

```
sand vault passwd [--no-migrate] [--per-day SIZE | --full-speed] [--folder DIR]... [--only-folders] [--keep-unlocked]
```

Changes the password now and rotates the data key. Every file is then rebuilt
onto the new key: gathered from its parts, re-encrypted, scattered again, and
the old parts erased. Files stay readable throughout. Without a pace flag this
runs in the foreground until done; an interruption is finished by
[`sand vault migrate`](#sand-vault-migrate). A second password change is
refused until every file has moved.

| Flag | Meaning |
|---|---|
| `--no-migrate` | Change the password and leave the files on the old key for `sand vault migrate` |
| `--per-day SIZE` | Re-encrypt this much a day (`10G`, `500M`) while `sand serve` has the vault open, or under `sand vault rekey --run` |
| `--full-speed` | Re-encrypt in the background as fast as the clouds allow, under `sand serve` or `sand vault rekey --run` |
| `--folder DIR` | Re-encrypt this folder first. Repeatable, in order. Needs `--per-day` or `--full-speed` |
| `--only-folders` | Stop after the `--folder` folders and leave the rest on the previous key for now |
| `--keep-unlocked` | Have `sand serve` hold its idle timer off while files are still on the previous key |

```bash
sand vault passwd --per-day 10G --folder /Taxes
```

The command asks for the current password, then the new one. Both prompts read
`SAND_PASSWORD` when it is set, so a script should pipe the two lines on stdin
instead (see [passwords in scripts](#passwords-in-scripts)). Design:
[paced re-key](../design/paced-rekey.md).

### sand vault migrate

```
sand vault migrate
```

Finishes the re-encryption a password change started: after an interruption,
after `--no-migrate`, or after an account was offline at the time. It moves
whatever is still on the old key and erases the parts left behind. Files that
have already moved are skipped, so running it again costs nothing.

### sand vault rekey

```
sand vault rekey [--list [--under DIR]] [--per-day SIZE | --full-speed] [--folder DIR]... [--all]
                 [--only-folders] [--keep-unlocked] [--pause | --resume] [--skip PATH | --unskip PATH] [--run]
```

With no flags, shows where a paced re-encryption stands. With flags, changes its
pace, order and holds. Until a file has moved it can be read by anyone holding
the previous password and an old copy of the vault file or of a manifest
backup; that is what the figures count down.

| Flag | Default | Meaning |
|---|---|---|
| `--list` | | List the files still on the previous key, in plan order, with where each stands |
| `--under DIR` | `/` | With `--list`, only the files in this folder |
| `--per-day SIZE` | | Re-encrypt this much a day |
| `--full-speed` | | As fast as the clouds take it, in the background |
| `--folder DIR` | | Take this folder first (repeatable, in order); needs `--per-day` or `--full-speed` |
| `--all` | | Forget the `--folder` order and take everything in turn |
| `--only-folders` | | Stop after the `--folder` folders |
| `--keep-unlocked` | | Hold `sand serve`'s idle timer off while files remain |
| `--pause` / `--resume` | | Hold the plan without forgetting it / take it up again |
| `--skip PATH` | | Leave this file for last; `.` is the file being moved now |
| `--unskip PATH` | | Put a skipped file back in its turn |
| `--run` | | Run the plan in the foreground until everything has moved, or Ctrl-C |

```bash
sand vault rekey                      # where it stands
sand vault rekey --per-day 50G        # speed it up
sand vault rekey --run                # on a machine with no server running
```

### sand vault policy

```
sand vault policy [strict|redundant]
```

Shows the placement policy, or sets it.

### sand vault defaults

```
sand vault defaults [account]... [--scheme K-of-N] [--clear]
```

Shows or sets the accounts uploads use when they do not name their own, and the
erasure code they are cut with. `sand put --accounts` and `--scheme` override it
per upload. With no default set, each file picks three accounts at random.

The number of accounts named chooses the code: clouds go in groups of three,
and two thirds of the shards rebuild the file (3 accounts give 2-of-3, 6 give
4-of-6, 3m give 2m-of-3m). Every one of these stores 1.5x the file. `--scheme`
picks another code as wide as the accounts named:

| Accounts | Scheme | Stored | Can go dark | Must collude to read |
|---|---|---|---|---|
| 3 | 2-of-3 (default) | 1.5x | 1 | 2 |
| 5 | `--scheme 3-of-5` | 1.67x | 2 | 3 |
| 5 | `--scheme 2-of-5` | 2.5x | 3 | 2 |
| 10 | `--scheme 6-of-10` | 1.67x | 4 | 6 |

| Flag | Meaning |
|---|---|
| `--scheme K-of-N` | The code to cut uploads with (default: 2m-of-3m from the count) |
| `--clear` | Forget the default; pick accounts per upload. With `--in`, the sub vault follows the vault's default again |

```bash
sand vault defaults box s3 drive
sand vault defaults --in Taxes usb-drive r2-cold nextcloud
sand vault defaults --in Taxes --clear
```

Design: [placement](../design/placement.md).

### sand vault backup

```
sand vault backup [--enable | --disable] [--every DURATION] [--force]
```

Writes the encrypted index to every connected account as `manifest.sand`. The
copy is sealed under the vault password with its own KDF parameters, so the
password alone opens it. It holds the file tree, the part map and the data key,
and never any account credentials. It is written automatically after the index
changes; this command forces a write or changes the settings.

| Flag | Default | Meaning |
|---|---|---|
| `--enable` | | Turn the manifest backup on |
| `--disable` | | Turn it off and erase the copies already on the accounts |
| `--every DURATION` | `5s` | Least time between automatic writes after a change (whole seconds, `0s` to `1h`; `0s` writes at once) |
| `--force` | | Overwrite a backup written by a different vault or an older password |

### sand vault sync

```
sand vault sync
```

Reads the copies of the index on the connected accounts and folds in what other
machines opening the same vault have written. `sand serve` does this on its own
(at unlock, every few minutes once another machine has been seen, and before
every push); this is the same pull for a machine with no server running.
Design: [multi-machine](../design/vault.md).

### sand vault instances

```
sand vault instances
```

Lists every machine that has written this vault's index, this one first, with
when it last did and how many connected accounts it can reach. Read from the
local index; `sand vault sync` brings it up to date. A machine is named by its
hostname, or `SAND_INSTANCE` when set.

---

## Sub vaults

A sub vault is sealed under a password of its own; the main password lists it
and cannot open it. It never appears on the vault's WebDAV share. Use `--in` on
the ordinary commands to work inside one. Guide: [vault
management](../guide/vault-management.md).

### sand sub ls

```
sand sub ls
```

Lists the sub vaults.

### sand sub new

```
sand sub new <name>
```

Makes a sub vault with a password of its own (asked twice, or
`SAND_SUB_PASSWORD`). There is no recovery for that password.

### sand sub passwd

```
sand sub passwd <name> [--no-migrate]
```

Changes the sub vault's password and rotates its key: every file in it is
rebuilt onto the new key, readable throughout. Reads the current password from
`SAND_SUB_PASSWORD` and the new one from `SAND_NEW_SUB_PASSWORD`.

| Flag | Meaning |
|---|---|
| `--no-migrate` | Change the password now and leave the files on the old key |

### sand sub assign

```
sand sub assign <path> <name> [--out]
```

Moves a file or folder from the main vault into a sub vault, or with `--out`
back again. The path is kept (`/Papers/2019` lands at `/Papers/2019`). Only the
index changes; the command then waits while the files are re-encrypted onto the
destination's key, because until then the source vault can still read them.

### sand sub rm

```
sand sub rm <name> [--force]
```

Deletes a sub vault and erases its parts from the accounts. A locked sub vault
needs `--force`: its contents cannot be listed, but the vault keeps an
inventory of the objects each sub vault owns, so they can still be erased.

### sand sub scan

```
sand sub scan
```

Asks each connected account what it holds and reports which carry an index this
vault cannot open: typically your own vault from a dead machine, on a cloud you
reconnected.

### sand sub import

```
sand sub import <account> [--name NAME] [--keep-backup]
```

Imports the vault whose index is on that account as a sub vault of this one. Use
it when `sand vault recover` refuses because this vault already holds files.
Asks for the old vault's password (`SAND_BACKUP_PASSWORD`) and a password for
the new sub vault (`SAND_SUB_PASSWORD`). The old data key is adopted, so nothing
is re-encrypted; run `sand sub passwd` afterwards to rotate it.

| Flag | Default | Meaning |
|---|---|---|
| `--name` | the account's name | What to call the sub vault |
| `--keep-backup` | | Leave the old index on the account (this vault then will not back up to it) |

### sand sub forget

```
sand sub forget <account>
```

Deletes another vault's index from one account. The old vault's parts stay, but
stop being another vault's, so `sand vault sweep` then offers them. Each account
carries its own copy; this vault's own index is never deleted.

---

## Converting old files

### sand vault convert

```
sand vault convert [path]... [--all]
```

Files stored before chunked storage are one sealed blob with no seams: reading a
byte in the middle means rebuilding the whole file, so nothing streams them and
the browser and WebDAV share refuse them (`NEEDS_CONVERSION`). Converting reads
each file once and stores it again in chunks, then erases the old parts. It
costs a download and an upload of the whole file; the file stays readable and an
interrupted conversion leaves it as it was.

```bash
sand vault convert                  # report what is still in the old format
sand vault convert --all            # convert all of it
sand vault convert /Videos/a.mkv    # or just these
```

---

## Recovery

Getting a vault back after the machine holding it is gone. Guide:
[recovery](../guide/recovery.md); design: [recovery kit](../design/recovery-kit.md),
[vault internals](../design/vault.md).

### sand vault recover

```
sand vault recover [--from ACCOUNT] [--backup-password PW] [--dry-run]
sand vault recover --resume [--dry-run]
```

Rebuilds this vault's index from the `manifest.sand` on a connected account.
Start from a fresh vault, connect at least the accounts holding your parts, then
run it. Reconnected accounts have new internal IDs, so each one is asked which
parts it holds and the index is re-pointed at whichever answers. Accounts not yet
reconnected show up as unreachable parts; connect them later and run
`--resume`, which needs no password.

| Flag | Default | Meaning |
|---|---|---|
| `--from` | first connected account | Account to read the backup from |
| `--backup-password` | prompt, or `SAND_BACKUP_PASSWORD` | Password of the vault being recovered |
| `--dry-run` | | Report what would be recovered and change nothing |
| `--resume` | | Re-point the index at accounts connected since |

```bash
sand vault init
sand remote add s3 --name r2 --set bucket=… --set access_key_id=… --set secret_access_key=…
SAND_BACKUP_PASSWORD='old password' sand vault recover --from r2
```

### sand vault reclaim

```
sand vault reclaim [--account NAME]...
```

Takes recovered files off the lost vault's data key. A fresh key is sealed
under your current password, every file is rebuilt onto it and the old parts
are erased; the password does not change. `--account` names the clouds to put
the files on (repeatable); left out, each file stays on the accounts it is on.
Costs a download and an upload of the whole vault; `sand vault migrate`
finishes an interrupted run.

### sand vault kit

A recovery kit is a sealed zip holding the index **and the account
credentials**, which never goes to a cloud. Importing one reconnects every
account under its old ID. Keep the kit and its recovery code apart.

#### sand vault kit export

```
sand vault kit export [--out FILE] [--use-vault-password]
```

Builds a kit and prints the recovery code that opens it. The code is not written
into the archive, its filename, or any log; the vault keeps it so `sand vault
kit code` can show it again.

| Flag | Default | Meaning |
|---|---|---|
| `--out` | `sand-recovery-kit-<date>.zip` | Where to write the kit |
| `--use-vault-password` | | Seal under the current vault password instead of a generated code. The kit then opens with today's password, not a later one |

#### sand vault kit code

```
sand vault kit code [kit-id] [--forget]
```

Shows the code a kit this vault exported was sealed under (the last kit when no
ID is given). `--forget` drops the code the vault is holding.

#### sand vault kit inspect

```
sand vault kit inspect <kit.zip>
```

Reads the kit's clear-text header: which kit it is, when it was made, and
whether it wants a recovery code or a vault password. Needs no secret.

#### sand vault kit verify

```
sand vault kit verify <kit.zip>
```

Opens a kit (asks for its code, or `SAND_KIT_CODE`), pings every credential in
it, compares its index with what the accounts hold, and reports what you would
get back today. Writes nothing.

#### sand vault kit import

```
sand vault kit import <kit.zip> [--replace] [--second-instance] [--skip-cloud-index]
```

Creates a vault from a kit: accounts reconnected under their old IDs, the file
tree as it was, sub vaults present and still locked. Asks for the kit's code
(`SAND_KIT_CODE`) and the password the new vault will use (`SAND_PASSWORD`). If
the password was changed after the kit was exported, put the old one in
`SAND_OLD_PASSWORD` so the newer index copies on the accounts can be opened. An
account that will not connect is recorded and reported; the rest carries on.

| Flag | Meaning |
|---|---|
| `--replace` | Import over a vault that already holds files, replacing them |
| `--second-instance` | Join the vault the kit came from, which keeps running there. The password must be that vault's own |
| `--skip-cloud-index` | Use the kit's own index rather than a newer copy from the accounts |

### sand manifest ls

```
sand manifest ls <manifest.sand> [--long]
```

Prints the file tree recorded in a manifest backup taken from an account. Needs
only the file and the password that wrote it (`SAND_PASSWORD`), so it works on a
machine that has never seen the vault. `--long` also shows which account holds
each part and under what object key.

### sand restore --manifest

Rebuilding one file by hand from loose parts and a manifest is
[`sand restore`](#sand-restore) with `--manifest`; see the standalone section.

---

## Housekeeping on the accounts

Commands that compare what the accounts hold with what the index records. Each
lists first and changes nothing without `--yes`. Guide:
[maintenance](../guide/maintenance.md).

### sand vault sweep

```
sand vault sweep [--verbose] [--yes]
```

Finds parts on the accounts that no file points at (usually from a delete made
while an account was disconnected), and working files an interrupted upload left
in the vault's own directory (only names Sand Vault writes, untouched for an
hour). Refuses to sweep an account carrying another vault's index, an account
that does not answer, or a vault that looks like it is waiting to be recovered.

| Flag | Meaning |
|---|---|
| `--verbose` | List every abandoned archive, not just totals |
| `--yes` | Erase what is found |

### sand vault reattach

```
sand vault reattach [--cloud ACCOUNT] [--yes]
```

Finds shards of files you still have that the index has no record of (which a
disconnect followed by a reconnect leaves behind) and records them again. No
bytes move: a part's object key derives from the archive ID and shard number.
Purely additive. `--cloud` lists one account instead of all. Afterwards run
`sand check --all` to confirm the parts are really there.

### sand vault restore-parts

```
sand vault restore-parts [--cloud ACCOUNT]... [--verbose] [--yes]
```

Finds parts the index records on an account that the account does not list
(lost), and with `--yes` cuts each one again from the file's other parts and
writes it back under its old name. Costs the parts a read needs plus the lost
object; the index is not written. An account that holds none of what it should
is flagged, since an emptied account and one pointed at the wrong folder look
the same. Does not cover pre-chunking files, locked sub vaults, thumbnails or
replicas.

| Flag | Meaning |
|---|---|
| `--cloud` | Put parts back on this account only (repeatable). Every account is still listed |
| `--verbose` | List every lost part |
| `--yes` | Remake the lost parts |

### sand vault prune

```
sand vault prune [--account ACCOUNT]... [--verbose] [--yes]
```

For buckets with versioning on (Backblaze B2 by default): lists the old
versions and delete markers of Sand Vault's own objects (index backups and
parts) and with `--yes` erases them. The current version of every object stays.
A part the index still wants whose current version is a delete marker is held
back. Other objects in the bucket are counted, not touched. To stop the build-up,
set the bucket's lifecycle to keep only the latest version, or switch on
`sand remote edit <account> --auto-prune on`.

| Flag | Meaning |
|---|---|
| `--account` | Only erase from this account (repeatable; the listing covers every account) |
| `--verbose` | List every object with stale versions |
| `--yes` | Erase what is found |

---

## Accounts

Connecting and managing the cloud accounts parts are stored on. Guide:
[accounts](../guide/accounts.md); design: [providers](../design/providers.md).

### sand remote kinds

```
sand remote kinds
```

Lists every backend and the `--set` keys it takes, with which are required or
secret. The kinds are `gdrive`, `onedrive`, `dropbox`, `box`, `protoncli`,
`zoho`, `s3`, `webdav`, `drime`, `filen`, `internxt`, `sftp`, `proton`, `mega`,
`jottacloud`, `synccom`, `tresorit`, `icedrive` and `local`. For the
sign-in backends it notes the `SAND_*_CLIENT_ID` variable that saves
registering an app.

### sand remote add

```
sand remote add <kind> [--name NAME] [--set key=value]...
```

Connects an account. It is pinged before it is saved.

| Flag | Default | Meaning |
|---|---|---|
| `--name` | the backend name | A label for the account |
| `--set key=value` | | A backend setting (repeatable); keys from `sand remote kinds` |

```bash
sand remote add local --name offsite --set path=/mnt/backup/sand
sand remote add s3 --name r2 --set bucket=shards --set access_key_id=… \
    --set secret_access_key=… --set endpoint=https://<acct>.r2.cloudflarestorage.com
```

Accounts that sign in through a consent screen (Google Drive, OneDrive, Dropbox,
Box, Zoho WorkDrive) are easiest to connect from the browser under `sand serve`.

### sand remote list

```
sand remote list        # alias: sand remote ls
```

Shows each account's name, kind, status, parts held, bytes stored, room left
(`FREE`), colour and ID.

### sand remote edit

```
sand remote edit <name-or-id> [--name NAME] [--color HEX|auto] [--capacity SIZE|none]
                 [--quota SIZE|none] [--auto-prune on|off] [--set key=value]...
# aliases: sand remote rename, sand remote set
```

| Flag | Meaning |
|---|---|
| `--name` | New label |
| `--color` | Hex colour such as `'#38bdf8'`, or `auto` to let the browser pick |
| `--capacity` | How big the account is, for backends that report no quota (`'10 GB'`, or `none`) |
| `--quota` | How much of the account Sand Vault may fill before it warns (`'200 GB'`, or `none`). Nothing is refused for crossing it |
| `--auto-prune` | `on` to erase old versions once a day while `sand serve` runs, `off` to stop; see [`sand vault prune`](#sand-vault-prune) |
| `--set key=value` | Change a connection setting (repeatable). The account is connected with the result before it is stored, so credentials the provider rejects are refused |

Only `--set` reaches the backend; the others change the index only. Changing a
bucket, prefix, folder or path does not move the parts already there: run
`sand check --all` afterwards.

```bash
sand remote edit r2-cold --name r2-archive --color '#38bdf8'
sand remote edit b2-cold --capacity '10 GB'
sand remote edit gdrive-photos --quota '200 GB'
sand remote edit s3-cold --set secret_access_key=...
```

### sand remote test

```
sand remote test <name-or-id>
```

Checks that one account is reachable.

### sand remote health

```
sand remote health [--every DURATION | --off]
```

With no flags, pings every account now and prints which answered. The server
also checks on its own, hourly by default, while the vault is unlocked.

| Flag | Meaning |
|---|---|
| `--every` | How often to check from now on (`15m`, `1h`, `24h`; 5 minutes to 7 days) |
| `--off` | Stop checking on a schedule |

### sand remote measure

```
sand remote measure <name-or-id>        # alias: sand remote count
```

Counts what is on an account by listing it, for backends that report no total
(buckets). Costs one request per thousand objects, which some providers bill, so
nothing runs it on a schedule. The figure is kept until the vault closes; pair it
with `--capacity`.

### sand remote proton login

```
sand remote proton login [name-or-id] [--name NAME] [--folder PATH] [--binary PATH]
```

Signs in to Proton Drive through Proton's `proton-drive` client. The client
prints a link; open it in a browser on any machine to finish signing in. With no
argument this connects a new account; with one, it signs an existing account
back in.

| Flag | Default | Meaning |
|---|---|---|
| `--name` | | Label for a newly connected account |
| `--folder` | `/my-files/sand` | Where in Proton Drive the parts go |
| `--binary` | found on `PATH` | Path to the `proton-drive` client |

### sand remote remove

```
sand remote remove <name-or-id> [--force]        # alias: sand remote rm
```

Disconnects an account. The parts on it are left alone. Refused when it would
make files unrecoverable, unless `--force`.

---

## Files

Storing, finding and moving files. Guide: [storing files](../guide/storing-files.md).

### sand ls

```
sand ls [path] [-l]
```

Lists a folder (default `/`). Each file shows size, modified time, scheme and
which account holds each shard; `-l` adds entry IDs.

```
photos/
report.txt  6 B  2026-11-13 02:21  2-of-3 s1:l1 s2:l3 s3:l2
```

### sand find

```
sand find <query> [--path DIR] [--type all|file|folder] [--limit N] [-l]
```

Searches the index by name. A bare word matches any name containing it,
ignoring case; `*` and `?` are wildcards; a query containing `/` is matched
against the whole path. Folders are results too. The index is only readable
with the vault open, so this is the only way to search.

| Flag | Default | Meaning |
|---|---|---|
| `--path` | `/` | Only search inside this folder |
| `--type` | `all` | `all`, `file` or `folder` |
| `--limit` | `200` | Most matches to print |
| `-l`, `--long` | | Also show entry IDs |

```bash
sand find receipt
sand find '*.jpg'
sand find photos/2024
```

### sand put

```
sand put <path>... [--path DIR] [--overwrite] [--accounts a,b,c] [--scheme K-of-N]
```

Uploads files or whole folders. A folder lands under a folder of the same name
inside `--path`, keeping its shape; each file is still compressed, split and
scattered on its own.

| Flag | Default | Meaning |
|---|---|---|
| `--path` | `/` | Destination folder in the vault |
| `--overwrite` | | Replace an existing file with the same name |
| `--accounts` | vault default, else three at random | Accounts to scatter over, by name or ID |
| `--scheme` | 2m-of-3m from the account count | The code to cut with, such as `3-of-5` |

```bash
sand put ./photos --path /archive
sand put report.pdf --accounts box,s3,drive,dropbox,onedrive --scheme 3-of-5
```

### sand get

```
sand get <path-or-id> [-o OUT] [--force] [--from parts|replica|either]
```

Downloads and decrypts a file.

| Flag | Default | Meaning |
|---|---|---|
| `-o`, `--output` | current directory, original name | Output file or directory |
| `--force` | | Replace a local file that is already there |
| `--from` | `parts` | Read from the parts, the replica, or either |

### sand mkdir

```
sand mkdir <path>
```

Creates a folder.

### sand mv

```
sand mv <path-or-id> <new-path>
```

Renames or moves a file, or a folder and everything under it. Only the index
changes; parts stay where they are.

```bash
sand mv /draft.txt /final/published.txt   # rename and move
sand mv /draft.txt /final                 # into a folder, keeping the name
sand mv /photos/2024 /archive/2024        # a folder, with everything in it
```

### sand relocate

```
sand relocate <path-or-id> --accounts a,b,c [--scheme K-of-N] [--from replica] [--dry-run]
```

Moves a file, or a folder and everything under it, onto the accounts named. Only
what has to move moves: a part already on a chosen account stays, and what
travels is copied as the encrypted blob it already is. Naming a different
*number* of accounts (or `--scheme`) changes the code, so the file is gathered
and rewritten. Each file commits on its own, so the command is safe to interrupt
and repeat. A file short a part is rebuilt rather than carried, which also puts
missing parts back.

`--from replica` rebuilds every file in scope that has a replica from that
replica, onto the accounts named, and erases the old parts where reachable. The
replica is not touched; files without one are counted and left.

| Flag | Meaning |
|---|---|
| `--accounts` | The accounts to move onto, by name or ID |
| `--scheme` | The code to end up cut with (default 2m-of-3m from the count) |
| `--from` | `replica` to rebuild from replicas (default: the parts, a move) |
| `--dry-run` | Say what would move, and move nothing |

```bash
sand relocate /photos --accounts box,s3,drive
sand relocate /notes.txt --accounts box,s3 --dry-run
sand relocate /photos --accounts box,s3,drive --from replica
```

Design: [placement](../design/placement.md).

### sand rm

```
sand rm <path-or-id> [-r]
```

Deletes a file and erases its parts from every account, or a folder with
`-r`/`--recursive`. An unreachable account produces a warning and the entry is
dropped anyway (the leftover parts are `sand vault sweep` business).

### sand check

```
sand check [path-or-id] [--all]
sand check --deep <file-or-folder> [--fix]
```

Without `--deep`, asks each account whether every part of the file is still
there (no download). With `--deep`, downloads and verifies every shard of every
chunk, the file's hash, and its contents as a file of its kind; `--fix` then
repairs what can be repaired.

| Flag | Meaning |
|---|---|
| `--all` | Check every file in the vault |
| `--deep` | Download and verify a file or a folder |
| `--fix` | With `--deep`, repair the files that can be repaired |

Exits non-zero when anything needs attention, which makes `sand check --all` a
reasonable cron job. Design: [cryptography](../design/cryptography.md).

### sand checksums

`/checksums.gz` at the vault root lists every file's SHA-256 (`path, checksum`
per line), stored on the vault's default clouds. `sand serve` keeps it up to
date (see `--checksums-every`). Guide: [folder tools](../guide/folder-tools.md).

#### sand checksums write

```
sand checksums write
```

Brings `/checksums.gz` up to date with the index now.

#### sand checksums verify

```
sand checksums verify [folder] [--limit MB_PER_S]
```

Reads every file under a folder (default `/`) and holds it to its checksum.
`--limit` caps the read rate in MB a second (`0`, the default, is unlimited).

#### sand checksums compute

```
sand checksums compute [folder] [--limit MB_PER_S]
```

Reads the files under a folder that have no checksum and records one.

---

## Replication

A replica is a second, independent placement of a file, written into a
*replica folder* beside the parts folder on clouds you choose. With one cloud a
replica is the whole sealed file; with several it is cut k-of-n. Replicas are
made by `sand serve` while the vault is open, at the `--per-day` pace, or by
`sand replicate run`. `sand get --from replica` and `sand relocate --from
replica` read them. Guide: [storing files](../guide/storing-files.md).

### sand replicate set

```
sand replicate set --cloud NAME=FOLDER... [--scheme K-of-N|whole] [--per-day SIZE]
sand replicate set --pause | --resume | --clear
```

| Flag | Meaning |
|---|---|
| `--cloud name=folder` | A connected account and the replica folder on it (repeatable). The folder must sit beside the parts folder, not inside it; an account whose parts are at its root cannot take replicas |
| `--scheme` | How a replica is cut: k-of-n as wide as the clouds, or `whole` for one cloud |
| `--per-day SIZE` | Copy at most this much a day (`10G`, `500M`; `0` for no limit) |
| `--pause` | Stop making replicas, keeping the policy. Replicas still on a retired key are erased |
| `--resume` | Start again after `--pause` |
| `--clear` | Forget the policy. Replicas on the current key stay; ones on a retired key are erased |

```bash
sand replicate set --cloud r2-cold=sand-replica --cloud nas=/mnt/nas/replica \
    --cloud drive="Vault replicas" --scheme 2-of-3 --per-day 10G
```

### sand replicate add

```
sand replicate add <path>...
```

Replicates a folder, or one file, and everything under it.

### sand replicate exclude

```
sand replicate exclude <path>...
```

Keeps a folder or file out of replication whatever its parent says.

### sand replicate remove

```
sand replicate remove <path>...
```

Puts a folder or file back to following its parent. Replicas already made stay.

### sand replicate drop

```
sand replicate drop <path-or-id>
```

Erases one file's replica now; its parts are untouched.

### sand replicate status

```
sand replicate status
```

Shows the policy and how far the copies have got.

### sand replicate run

```
sand replicate run
```

Makes the wanted replicas in the foreground, keeping to `--per-day`, for a
machine that does not run the server. Ctrl-C stops between files.

---

## Snapshots

A snapshot packs everything under a folder into one archive, compressed with
zstd, sealed under a password of its own, and kept on one connected cloud
(`--cloud`) or on a machine you have an SSH login on (`--machine`, with an
optional `--dir` under its root). Over `--max-size` it is cut into part files.
The password is asked for (twice when taking) or read from
`SAND_SNAPSHOT_PASSWORD`. Machines are connected in the web UI ("Connect a
machine"; API: [`/api/remote`](api.md#machines-sftp-sources)); there is no CLI
command for them. Guide: [folder tools](../guide/folder-tools.md).

Every snapshot subcommand takes the same place flags:

| Flag | Meaning |
|---|---|
| `--cloud ACCOUNT` | Keep it on this connected account (name or ID) |
| `--machine NAME` | Keep it on this machine (name or ID) |
| `--dir DIR` | The folder on the machine, under its root |

### sand snapshot take

```
sand snapshot take [folder] (--cloud ACCOUNT | --machine NAME [--dir DIR]) [--max-size SIZE]
```

Packs a folder (default `/`) into a snapshot. `--max-size` is the most one part
file may be (`2G`, `500M`); the default is no limit on a machine and 64 MB on a
cloud, where 256 MB is the most allowed. Ctrl-C stops it and erases what had been
written.

### sand snapshot ls

```
sand snapshot ls (--cloud ACCOUNT | --machine NAME [--dir DIR])
```

Lists the snapshots a place is keeping.

### sand snapshot restore

```
sand snapshot restore <snapshot> [folder] (--cloud … | --machine … [--dir …]) [--replace]
```

Unpacks a snapshot into a folder (default `/`). `--replace` writes the
snapshot's file over a different file already under its name. Ctrl-C keeps what
had arrived; running it again carries on.

### sand snapshot rm

```
sand snapshot rm <snapshot> (--cloud … | --machine … [--dir …])
```

Erases a snapshot from where it is kept.

---

## Automation

Standing instructions for a folder and everything under it: check it on a
schedule, and optionally repair it. The schedules are kept by `sand serve` while
the vault is unlocked (give a machine meant to keep them a long
`--idle-timeout`); a slot missed while locked comes due at the next unlock.
Guide: [maintenance](../guide/maintenance.md).

### sand automation set

```
sand automation set <folder> (--hourly | --daily HH:MM | --weekly DAY,HH:MM)
    [--task shards|git] [--action check|rebalance|pull] [--disabled]
    [--narrow] [--max-repairs N] [--rebuild-limit SIZE|none]     # shards
    [--max-repos N] [--size-limit SIZE|none] [--prune]           # git
```

Creating and editing are the same command; an edit keeps the policy's history
and last-run time, so changing the hour does not make it due at once. Times are
local to the machine running the server.

| Flag | Default | Meaning |
|---|---|---|
| `--hourly` / `--daily HH:MM` / `--weekly day,HH:MM` | | Exactly one says how often |
| `--task` | `shards` | `shards`: every cloud asked whether it is there and every part checked against the index. `git`: every stored repository asked whether its upstream moved |
| `--action` | `check` | `check` only records what it found. `rebalance` (shards) rebuilds what came back short onto the clouds that answered. `pull` (git) fetches repositories that moved |
| `--disabled` | | Store the policy but do not run it on schedule |
| `--narrow` | off | Allow a repair to cut a file with a smaller code when too few clouds answer for its own. Cannot be undone without another full rebuild |
| `--max-repairs N` | `0` (no bound) | How many files one run may rebuild; the rest wait for the next run, worst first |
| `--rebuild-limit SIZE` | `1G` | Largest file an unattended repair will rebuild (a rebuild holds the whole file in memory); `none` for no ceiling |
| `--max-repos N` | `0` (no bound) | How many repositories one run may fetch |
| `--size-limit SIZE` | `2G` | Largest stored bundle an unattended refresh will fetch; `none` for no ceiling |
| `--prune` | off | Delete a stored repository whose upstream has gone. Off because an outage and a deletion look the same |

```bash
sand automation set /archive --daily 10:00 --action rebalance
sand automation set /films --weekly sun,03:00 --action check
sand automation set /taxes --daily 02:00 --action rebalance --rebuild-limit 8G
sand automation set /code --weekly sun,04:00 --task git --action pull
```

### sand automation list

```
sand automation list
```

Shows every folder with a policy, its schedule, next run and what the last run
found.

### sand automation run

```
sand automation run <folder>
```

Carries out a folder's policy now, due or not, on or off. Stamps the last-run
time as a scheduled run does. Exits non-zero when a file is past repairing or a
repository could not be brought up to date.

### sand automation remove

```
sand automation remove <folder>        # alias: sand automation rm
```

Takes the policy off a folder, history and all.

---

## Repositories

A git repository is stored as a single git bundle (all history, branches and
tags) that `git clone` reads directly. Sand Vault uses the `git` on the machine,
so it reaches exactly the repositories you can (your SSH keys, credential
helper, `~/.ssh/config`). Guide: [repositories](../guide/repositories.md).

### sand git track

```
sand git track <url> [--into DIR] [--accounts a,b,c] [--scheme K-of-N]
```

Mirrors a repository and stores it as `<name>.bundle` in `--into` (default `/`),
remembering its upstream. The first copy is the whole history; later refreshes
cost only the difference.

```bash
sand git track https://github.com/chinmay28/sand-vault.git --into /code
sand git track git@github.com:me/private.git --into /code --accounts a,b,c
```

### sand git list

```
sand git list [folder]
```

Shows the repositories stored under a folder and when each was last fetched.

### sand git refresh

```
sand git refresh <path>
sand git refresh <folder> --all
```

Asks the upstream whether it has moved (a few kilobytes of ref advertisement)
and fetches only if it has. `--all` treats the path as a folder and refreshes
every repository under it.

### sand git rebuild

```
sand git rebuild <path> --accounts a,b,c [--scheme K-of-N]
```

Mirrors a tracked repository again from its upstream and stores it over the
existing bundle on the accounts named, whatever state the stored bundle is in.
This is the way back when its parts cannot be read. It costs a full clone.
`--accounts` is required.

### sand git untrack

```
sand git untrack <path>
```

Stops following the upstream. The bundle stays and is still cloneable.

---

## Server

### sand serve

```
sand serve [--port 8123] [--bind 0.0.0.0] [--idle-timeout 30m] [--profile auto|pi5|workstation|custom]
           [--webdav] [--webdav-path /dav] [--webdav-sub-path /sub]
           [--tv-discovery-port 8124] [--checksums-every 2m]
```

Starts the web server and file browser. The vault starts locked and is unlocked
from the browser (or by a WebDAV client presenting the password). While unlocked
the server also runs the background work: automation schedules, health checks,
paced re-keying, replication, the checksum ledger, auto-prune and multi-machine
sync.

| Flag | Default | Meaning |
|---|---|---|
| `--port` | `8123` | TCP port |
| `--bind` | `0.0.0.0` | Address to bind. Any non-loopback bind logs a warning: passwords and decrypted files cross the network in the clear unless TLS is in front |
| `--idle-timeout` | `30m` | Re-lock the vault after this much inactivity |
| `--profile` | `$SAND_PROFILE`, else `machine.json` beside the vault | Size to a machine: `auto`, `pi5`, `workstation` or `custom`. Pins it; the browser's Settings cannot change it |
| `--webdav` | off | Also serve the vault as a WebDAV share |
| `--webdav-path` | `/dav` | URL path of the vault's share |
| `--webdav-sub-path` | `/sub` | URL path under which each sub vault has its own share, `<path>/<name>/`, behind its own password |
| `--tv-discovery-port` | `8124` | UDP port Sand TV broadcasts to when looking for the server; `0` turns discovery off. Never served on a loopback bind |
| `--checksums-every` | `2m` | How often `/checksums.gz` is brought up to date when the index has changed; `0` writes it only when asked |

At start the server reads the memory limit it runs under (a systemd
`MemoryMax`, or the machine's memory) and sets `GOMEMLIMIT` below it. Set
`GOMEMLIMIT` yourself to override. An orderly stop (Ctrl-C, SIGTERM) waits up
to 30 seconds for the index backup to finish writing.

```bash
sand serve --bind 127.0.0.1                    # local only
sand serve --webdav --idle-timeout 12h         # a home server with a mounted share
```

HTTP endpoints: [API reference](api.md). Deployment: [deployment
guide](../guide/deployment.md). Design: [server](../design/server.md).

---

## Disk health

### sand smart-scan

```
sand smart-scan [--out PATH] [--smartctl PATH]
```

Asks `smartctl` about every physical disk and writes a JSON report. The server
reads it during its health check and warns on any Local folder account whose
disk reports trouble (pending sectors, failed self-tests, tripped thresholds).
Disks with no SMART data (SD cards, most USB sticks) are recorded as unsupported,
not healthy. Needs root and smartmontools. Normally run by the `sand-smart`
systemd timer the quickstart installs (`scripts/enable-disk-health.sh` adds it
to other installs).

| Flag | Default | Meaning |
|---|---|---|
| `--out` | `/var/lib/sand/smart.json` | Where to write the report; the server reads this path, or `$SAND_SMART_REPORT` |
| `--smartctl` | `smartctl` | The smartctl binary to run |

---

## Standalone mode

Needs no vault and no accounts: three zip files you move around by hand.

### sand archive

```
sand archive <file>... [--output-dir DIR] [--password PW]
```

Each input file is compressed, split in two, given a third XOR parity part, and
encrypted under the password. Writes `sand-p1.zip`, `sand-p2.zip` and
`sand-p3.zip`, each holding one part (`<name>.p1.sand` and so on) per input
file. Any two zips restore everything.

| Flag | Default | Meaning |
|---|---|---|
| `--output-dir` | `.` | Where to write the zips |
| `--password` | prompt (or `SAND_PASSWORD`) | Encryption password |

### sand restore

```
sand restore --parts FILE1,FILE2[,FILE3] [--output-dir DIR] [--password PW] [--manifest manifest.sand [--preserve-tree]]
```

Rebuilds a file from any two of its parts. Parts from `sand archive` need only
the password they were written with. Parts pulled from a vault's cloud accounts
are encrypted under the vault's data key, so pass `--manifest` with a
`manifest.sand` from any of the accounts; the vault password opens it. Enough
parts, the manifest and the password rebuild a file with no vault and no
network.

| Flag | Default | Meaning |
|---|---|---|
| `--parts` | | Comma-separated `.sand` part files: two of a whole file's three, or two of every chunk's |
| `--output-dir` | `.` | Where to write the restored file |
| `--password` | prompt (or `SAND_PASSWORD`) | Decryption password |
| `--manifest` | | A `manifest.sand` holding the key the parts were stored under |
| `--preserve-tree` | | With `--manifest`, recreate the file's folders inside the output directory |

```bash
sand archive report.pdf photos.zip --output-dir ./out
# → out/sand-p1.zip  out/sand-p2.zip  out/sand-p3.zip  (store each somewhere different)

unzip out/sand-p1.zip && unzip out/sand-p3.zip
sand restore --parts report.pdf.p1.sand,report.pdf.p3.sand --output-dir .
```

Rebuilding a vault file by hand: [recovery guide](../guide/recovery.md).

---

## Version

### sand version

```
sand version
```

Prints one line, `SAND Vault <version>` (for example `SAND Vault v2026.11.0`),
and exits. The quickstart installer parses it, so it stays a single line.
`sand --version` prints `sand version <version>`.

---

## Passwords in scripts

Every prompt reads without echo from a terminal. For unattended use, each
password has an environment variable; when it is unset and stdin is not a
terminal, the password is read as one line from stdin. stdin is buffered once
per process, so a command needing several passwords can take them as successive
lines.

| Variable | Read by |
|---|---|
| `SAND_PASSWORD` | The vault password, for every command that opens the vault. Also the new password for `vault init` and `vault kit import`, the password for `archive`, `restore` and `manifest ls`, and the sealing password for `vault kit export --use-vault-password` |
| `SAND_SUB_PASSWORD` | A sub vault's password: `--in`, `sub new`, `sub passwd` (current), `sub import` (new) |
| `SAND_NEW_SUB_PASSWORD` | The new password for `sub passwd` |
| `SAND_BACKUP_PASSWORD` | The lost vault's password: `vault recover`, `sub import` |
| `SAND_OLD_PASSWORD` | `vault kit import`: the lost vault's password, tried when the kit's key is refused |
| `SAND_KIT_CODE` | The recovery code: `vault kit import`, `vault kit verify` |
| `SAND_SNAPSHOT_PASSWORD` | A snapshot's password: `snapshot take`, `restore`, `ls`, `rm` |

```bash
export SAND_PASSWORD='…'
sand check --all

# Two passwords down one pipe: current, then new.
printf '%s\n%s\n' "$OLD" "$NEW" | env -u SAND_PASSWORD sand vault passwd
```

`sand vault passwd` reads both its current and its new password through
`SAND_PASSWORD`, so with that variable set the "new" password is the current one.
Pipe the two lines with the variable unset, as above.

Other environment variables the binary reads:

| Variable | Effect |
|---|---|
| `SAND_VAULT` | Default vault path (overridden by `--vault`) |
| `SAND_PROFILE` | Default for `sand serve --profile` |
| `SAND_INSTANCE` | The name this machine opens the vault under (default: hostname) |
| `SAND_OAUTH_REDIRECT` | Pins the OAuth redirect URI, for a server behind a reverse proxy |
| `SAND_GOOGLE_CLIENT_ID` / `_SECRET`, `SAND_MICROSOFT_CLIENT_ID` / `_SECRET`, `SAND_DROPBOX_APP_KEY` / `_SECRET`, `SAND_BOX_CLIENT_ID` / `_SECRET`, `SAND_ZOHO_CLIENT_ID` / `_SECRET` | App credentials for the sign-in backends, so each connect need not register an app |
| `SAND_PROTON_STATE_DIR` | Root directory for the Proton client's per-account state |
| `SAND_SMART_REPORT` | Path of the SMART report the server reads |
| `GOMEMLIMIT` | Overrides the memory limit `sand serve` sets for itself |

Full list with defaults: [configuration](configuration.md).

## Exit status

Every command exits `0` on success and `1` on any error, printing `error: …` to
stderr. Commands that find problems also exit `1`: `sand check` when any file
needs attention, `sand automation run` when a file is past repairing or a
repository could not be updated, and `sand relocate` when a file did not fully
move.
