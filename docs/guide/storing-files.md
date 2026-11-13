# Storing files

How SAND Vault stores and retrieves a file, which date a file keeps, how to
choose the clouds and the m-of-n code a file is cut with, what to do about files
that went out short, how to keep a second copy (replication), and how moving and
renaming work. For the mechanism, see
[data-pipeline.md](../design/data-pipeline.md) and
[placement.md](../design/placement.md); for exact flags, the
[CLI reference](../reference/cli.md).

## What happens to a file

Every file is cut with an **m-of-n** erasure code: n shards, any m of which
rebuild it. The default is 2-of-3.

```
store:    file → zstd → m data shards + (n − m) parity shards (Reed–Solomon)
               → AES-256-GCM per shard → one shard to each of n accounts, in parallel

retrieve: ask all n accounts at once → first m to arrive win, the rest are cancelled
               → decrypt → decode → decompress → verify SHA-256 → your file
```

- An upload commits once **at least m** shards have landed. If fewer than m
  land, the ones that did are deleted rather than left behind.
- Reads are a race, so a slow or offline account costs nothing as long as m
  others answer.
- The code is systematic: the first m shards are the compressed file itself, cut
  into slices. With all of them, rebuilding is a concatenation; with any other m,
  the missing slices are solved for. With fewer than m, the file cannot be read.
- Large files go up in chunks (16 MB by default), each cut and sealed on its own,
  so reads can start anywhere in a file.

After every change, each account also gets `manifest.sand`, an encrypted copy of
the index, so that losing the vault file is survivable. Writes are batched: by
default at most one every five seconds (**Vault settings › Index backup**, or
`sand vault backup --every 30s`). See [recovery.md](recovery.md).

## The date a file keeps

A file shows the modification time it had where it came from (the day the photo
was taken, not the day it was uploaded). The browser sorts by that date, the
WebDAV mount reports it, folder zips carry it, and it is written back onto a
machine you send the file to. Browser uploads, imports from a machine, and
`sand put` all keep it.

Older versions stamped every upload with the moment it landed. To fix files
stored that way, drop the same folder on the same place in the browser, or run
the same machine import again. Files already stored are not sent again. Any
that have the wrong date get it corrected in place, which is one index write
and touches no account. The banner says how many were corrected, and running it
again when the dates already match does nothing.

`sand put` of a name that already exists does not correct the date. It stores a
second copy named `name (2)`, or replaces the file with `--overwrite`.

## Placement policy

Any m shards plus the key rebuild a file, so m shards on one account would let
that account rebuild it. The policy decides whether that is allowed.

| Policy | Rule | With fewer accounts than shards |
|---|---|---|
| `strict` (default) | never two shards of a file on one account | 2 accounts: 2-of-3 is stored with two shards and no spare. 1 account: refused |
| `redundant` | always store every shard, doubling up if needed | survives an account going dark, but the account holding two shards can rebuild the file |

```bash
sand vault policy               # show
sand vault policy redundant     # change (applies to new uploads)
```

The policy is a vault-wide setting, and `sand vault init --policy` sets it at
creation. Under `redundant` with fewer than three accounts, the index backup is
refused (see [The tradeoff](recovery.md#the-tradeoff)).

## Which clouds a file goes to

When more accounts are connected than a file has shards, something picks which
ones it uses.

- **In the browser**, the upload dialog opens on the clouds the file is about
  to go to, and you can swap any of them. Each row shows how much that cloud
  already holds and how much more fits, and the dialog warns about any cloud
  that will not have room for its share (about 1/m of the file). See
  [accounts.md](accounts.md) for capacity and quotas.
- **A vault-wide default** sets what the dialog opens on. With none set, each
  file gets three clouds chosen at random, which spreads a vault evenly over
  more than three accounts.

```bash
sand vault defaults                                  # show
sand vault defaults usb-drive r2-cold nextcloud      # set
sand vault defaults --clear                          # back to random per upload
sand put report.pdf --accounts usb-drive,nextcloud   # this upload only
```

A selection is followed exactly. If you name two clouds, two shards are stored
and the file is reported as having no spare, rather than putting the third
somewhere you did not choose. Disconnecting an account removes it from the
default. A [sub vault](vault-management.md#sub-vaults) follows the vault's
default until it is given its own.

## Choosing m of n

Any m from 2 to n, and any n up to 255, is a valid code. The choice trades off
three things:

| Scheme | Storage (n/m) | Clouds that can go dark (n − m) | Clouds that must collude (m) |
|---|---|---|---|
| `2-of-3` | 1.50× | 1 | 2 |
| `3-of-5` | 1.67× | 2 | 3 |
| `2-of-5` | 2.50× | 3 | 2 |
| `4-of-6` | 1.50× | 2 | 4 |
| `6-of-9` | 1.50× | 3 | 6 |
| `6-of-10` | 1.67× | 4 | 6 |
| `20-of-30` | 1.50× | 10 | 20 |

The last column is how many accounts an attacker must hold together to rebuild
a file. Lowering m buys durability with that, not only with storage: 2-of-5
survives three losses, but any two accounts still rebuild the file, and each
holds half of it.

Rules:

- **m is never less than 2.** One shard per rebuild would be a whole copy on
  every account.
- **n is the number of clouds you pick.** Each cloud holds exactly one shard, at
  any width.
- **A count of clouds names a default code** from the 2m-of-3m family: 3 clouds
  is 2-of-3, 6 is 4-of-6, 9 is 6-of-9, which keeps storage at 1.5×. One or two
  clouds get 2-of-3, stored short. **Any other count (4, 5, 7, …) needs an
  explicit scheme**, or the upload is refused with a suggestion.
- More shards than clouds stores what fits and reports the file as short a
  spare. More clouds than shards is refused, so no cloud you picked ends up
  holding nothing.
- With nothing chosen, an upload is 2-of-3 on three clouds, however many are
  connected.

```bash
sand vault defaults box s3 drive dropbox onedrive proton              # 4-of-6 from now on
sand vault defaults box s3 drive dropbox onedrive --scheme 3-of-5
sand put taxes.pdf --accounts box,s3,drive,dropbox,onedrive --scheme 2-of-5
```

In the browser, the upload dialog's **Rebuild from _m_ of _n_ clouds** picker
shows what each choice stores, survives and needs to collude.

### Changing an existing file's code or clouds

`sand relocate` (**Move to other clouds**, or **⇄ Clouds** in the selection
bar) moves files onto the accounts you name:

- **Same width:** only the shards that have to move are copied, as the
  encrypted objects they already are. Nothing is decrypted.
- **Different width or scheme:** no shard carries over (2-of-3 shards are halves,
  4-of-6 shards are quarters), so each file is downloaded, cut again and
  uploaded whole.

```bash
sand relocate /taxes --accounts box,s3,drive,dropbox,onedrive,proton --dry-run
sand relocate /taxes --accounts box,s3,drive,dropbox,onedrive --scheme 3-of-5
```

`--dry-run` says which of the two will happen and what it costs. Each file
commits on its own, so a relocate is safe to interrupt and to repeat.

## Files that went out short

If a cloud is not answering while a file is being stored, the upload does not
fail. Two shards of three go out and the file is readable, but it has no spare,
and nothing goes back later to finish it.

The accounts panel counts these files at the bottom, for example
`4 files missing a spare part`. Click the line to open **Files missing a part**,
with the worst first. Each row shows the shards it has, coloured by account,
and an outlined square for each missing one. **⇄ Clouds** on a row opens the
cloud picker so you can swap the failed cloud for one that answers.

Putting a shard back is a rebuild, not a copy, because the missing shard is not
on any account to copy from. The estimate says `1 file to rebuild` and why:

```bash
sand relocate /holiday.mov --accounts box,s3,drive --dry-run
#   /holiday.mov is missing 1 of its 3 shards, so it has to be rebuilt rather
#   than moved — ...
```

A file with fewer than m shards recorded is listed in red with no repair
offered, unless it has a [replica](#replication), in which case the row offers
**↻ Replica**. `sand check` and a file's **Where the shards live** report the
same thing for one file.

To repair short files on a schedule, see
[maintenance.md](maintenance.md#a-folder-that-looks-after-itself). If a shard
went missing because an account was disconnected and reconnected, it may only
need re-recording, which costs nothing: see
[maintenance.md](maintenance.md#a-shard-a-disconnect-mislaid-reattach).

## Replication

Replication adds a second, independent placement for the folders you choose.
The same bytes are cut again under their own archive ID and the current key,
and written into a **replica folder** on each replication cloud. The original
shards are left as they are.

Replication clouds are accounts you have already connected. The replica folder
sits beside the folder that account's shards are in, never inside it, and it
is the only thing that tells replica objects apart from shards. An account that
keeps its shards at its root can't hold replicas until its shards have their own
folder.

- **One replication cloud** holds the **whole sealed file**. This is the only
  arrangement where one account holds a whole file. It is still encrypted, but
  that account plus the key is enough. The settings dialog says so.
- **Several replication clouds** cut the replica k-of-n like any other file, or
  `--scheme whole` for a whole copy on each.

```bash
sand replicate set --cloud r2-cold=sand-replica --cloud nas=/mnt/nas/replica \
    --cloud drive="Vault replicas" --scheme 2-of-3 --per-day 10G
sand replicate add /photos           # this folder and everything under it
sand replicate exclude /photos/raw   # keep a folder out, whatever its parent says
sand replicate remove /photos/raw    # back to following its parent
sand replicate status
sand replicate run                   # in the foreground; 'sand serve' does it on its own
sand replicate drop /photos/hike.jpg # erase one file's replica
sand replicate set --pause | --resume | --clear
sand get --from replica /photos/hike.jpg
```

In the browser:

- **Vault settings › Replication** sets the clouds, the replica folder on each,
  the code and a daily budget.
- A folder's **⋯** menu has **Replicate this folder**, and a file's has
  **Replicate this file** and **Drop the replica**. Each shows the cost before
  anything moves.
- Replicas are made while the vault is open, smallest file first, at the pace
  you set. A replica is made again when the file's bytes change, when the key
  changes after a password change, or when the policy changes.
- Deleting a file erases its replica too. `--clear` keeps replicas on the
  current key and erases the ones on a retired key.

A replicated file shows a second, outlined group of badges after its shards.
**Where the shards live** checks each replica shard and says whether the replica
alone can rebuild the file. When you open, stream or download a file, **Read
from** lets you choose the shards, the replica, or **either** (the shards, with
the replica filling in any chunk they cannot give). A stream link made for the
replica keeps reading the replica.

### Rebuilding the shards from the replica

Reading from the replica keeps a file reachable, but it does not repair the
file. To rebuild the shards themselves, use **Move to other clouds** on a file,
folder or selection, set **Read from** to *the replica*, and tick the target
clouds. Each file that has a replica is read from it and written out as a fresh
set of shards. The old shards are erased wherever they can still be reached,
and the replica itself is not touched. The estimate counts the files in scope
that have no replica; those are left as they are.

```bash
sand relocate /photos --accounts box,s3,drive --from replica --dry-run
sand relocate /photos --accounts box,s3,drive --from replica
```

## Moving to another folder

The folder a file is in is a field in the encrypted index, and its shards are
named after the file's random archive ID. Moving a file therefore rewrites one
field and touches no account. A 4 GB film moves as fast as a note. A folder is
only a path in the index, so moving one carries everything under it in a single
write, including the thumbnails and film-details settings filed under it.

```bash
sand mv /draft.txt /final/published.txt   # move and rename
sand mv /draft.txt /final                 # into a folder, keeping its name
sand mv /photos/2024 /archive/2024        # a folder and everything in it
```

In the browser, use **→** on a row, **Move to another folder** in its **⋯**
menu, or **→ Folder** in the selection bar for several items. (**⇄ Clouds**
next to it is the other kind of move, which moves shards between accounts.)
The picker opens on the item's current folder and has **+ New folder here**.
A folder can't be moved inside itself. A name that is already taken in the
destination is refused one item at a time, and the rest of the selection still
moves.

## Renaming

Renaming is also an index write and touches no account. Use **Rename** in a
row's **⋯** menu, or `sand mv`:

```bash
sand mv /draft.txt /published.txt
sand mv /photos/2024 /photos/holiday
```

The dialog selects the name up to the extension, so typing keeps the `.mkv`. A
name is a single path segment: a `/` would make it a move, and the field says
so. A name that is already taken in the folder is refused.
