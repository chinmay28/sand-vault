# Placement

Where a file's shards go, and why that is a security decision: the placement
policy, the erasure scheme a file is cut with, which accounts are chosen, how
object keys are named, and the two kinds of move (changing which accounts hold
a file, and changing where it appears in the vault). For the user's view, see
[guide/storing-files.md](../guide/storing-files.md). The code itself is in
[data-pipeline.md](data-pipeline.md#the-erasure-code).

Any k shards of a file, plus the data key, rebuild it. So k shards on one
account means that account could, in principle, rebuild the file. Everything
below follows from keeping that from happening.

## Placement policy

Each vault has one policy (`sand vault policy`, `POST /api/vault/policy`),
stored in the clear section of the vault file (`internal/vault/placement.go`).

| | `strict` (default) | `redundant` |
|---|---|---|
| Rule | No account ever holds two shards of one file, at any width | Always store every shard, doubling up when fewer accounts than shards are chosen |
| Enough accounts | One shard per account | Same |
| Fewer than n, at least k | Only as many shards as accounts are stored. The file is rebuildable and confidential but has fewer spares (2 accounts on 2-of-3: no spare at all) | All n shards stored, some accounts holding two |
| Fewer than k | **Refused**, with an error naming the fix | Stored, but an account then holds enough to rebuild the file |
| Trade-off | Confidentiality first | Survives an account going dark with only one or two connected, at the cost of the one-account guarantee. The UI says so when you pick it |

`BuildPlan` deals shards one per account, rotating the starting account by a
per-file seed so that shard 1 doesn't pile onto the same provider for every file.

## Schemes, and how wide a file is spread

A file is cut with a k-of-n code (`archive.Scheme`, `internal/archive/scheme.go`).
Three numbers move with the choice, and they don't move together:

| Scheme | Storage (n/k) | Accounts that can go dark (n − k) | Accounts an attacker needs together (k) |
|---|---|---|---|
| `2-of-3` | 1.5× | 1 | 2 |
| `4-of-6` | 1.5× | 2 | 4 |
| `6-of-9` | 1.5× | 3 | 6 |
| `3-of-5` | 1.67× | 2 | 3 |
| `2-of-5` | 2.5× | 3 | 2 |
| `6-of-10` | 1.67× | 4 | 6 |
| `20-of-30` | 1.5× | 10 | 20 |

The last column is the one worth dwelling on. It is how many accounts an
attacker must hold *together* before their shards are enough to rebuild a file.
Lowering k to buy durability spends exactly that: 2-of-5 survives three losses,
but two accounts still rebuild the file, and each holds half the plaintext. It
buys durability with storage and secrecy, not storage alone.

### The default family: 2m of 3m

When nobody names a scheme, **the number of accounts chosen settles it**:
accounts come in groups of three, and two thirds of the shards rebuild the
file (`archive.SchemeFor`). Three accounts give 2-of-3, six give 4-of-6, nine
give 6-of-9, and so on. Every member of the family stores 1.5×, so **widening
costs accounts, not bytes**. Each group of three adds one more survivable failure
and two more providers an attacker must compromise together. Shards are
consecutive slices of the compressed stream, so one account plus the data key
yields 1/k of a file: a thirtieth at 20-of-30, where it was a half at 2-of-3.

A count that isn't a multiple of three names no default scheme and is refused
with a suggestion, unless a scheme is given. One or two accounts are the
exception: they take 2-of-3 with whatever shards fit (see the policy table).

### Naming a scheme

Any k-of-n with k ≥ 2 and n ≤ 255 can be named instead: `--scheme 3-of-5` on
`sand put`, `sand relocate` and `sand git`, a `scheme` field in the upload form
or relocation request, or a default (`sand vault defaults --scheme`). The limits:

- **k ≥ 2.** One data shard is a whole copy on every account, so any single
  account would hold the plaintext. That promise is not a setting.
- **n ≤ 255**, because the shard number is one byte in the header.
- **No more accounts than shards** (`checkSpread`). An account somebody chose
  would otherwise hold nothing, which silently ignores a choice rather than
  accepting a trade-off.
- **Fewer accounts than shards is allowed** and is the same shortfall a vault
  with two clouds has always had. How far it may go is the policy's call.

The one exception to k ≥ 2 is a **replica** on a single account: a whole copy
of the sealed file (`archive.WholeCopy`). It is the only placement that sets
aside the "no single account holds a file" rule, and it exists only because
somebody chose a second copy on one cloud and was told what that means
(`internal/vault/replica.go`, `sand replicate set`).

A vault holds files of different schemes at once. Widening doesn't rewrite what
is already stored, so each file records its own k and n in the index and, for an
offline restore, in every shard's header.

### What widening costs

Not storage, not memory, and not per-provider load. The chunk window bounds how
many chunks are in flight, so each provider sees at most that many requests
whatever n is, and rate limits are per provider. Bytes in flight are
`window × n × chunkSize/k`, and n/k is constant across the default family.

What does grow is the **total number of objects and requests**. A chunk becomes
n objects, so a 4 GB file at 16 MiB chunks is 256 × n objects: 768 at 2-of-3
and 7,680 at 20-of-30, spread over ten times as many accounts. Padding (up to
k − 1 bytes per chunk) is noise at any width.

### Changing a file's scheme is a rebuild, not a move

A 2-of-3 file's shards are halves of its compressed stream. A 4-of-6 file's are
quarters. They have nothing in common, so changing the width means gathering
the file, cutting it again, and writing it out: a whole download and a whole
upload, against 1/k of the file per shard for a move at the same width. Both the
CLI and the browser price it first and label it a rebuild, because on a large
folder the difference is two orders of magnitude.

## Which accounts, of the ones connected

The policy decides how many shards may share an account, and the scheme decides
how many shards there are. Neither decides *which* accounts a file uses. That is
decided in this order (`resolveAccounts`, `SelectAccounts`):

| The upload says | What happens |
|---|---|
| These accounts | Exactly those, and every one must be connected. Their count (or the named scheme) settles the code. Naming fewer than the scheme's width stores fewer shards and warns about the missing spare |
| Nothing, and the vault has default accounts | Exactly the default, on the same terms, with the default scheme where its width matches |
| Nothing, and there is no default | Three picked at random for this file alone |

The random pick is seeded from the file's archive ID (128 random bits minted for
it), so consecutive uploads land on different accounts and a vault with six
accounts fills all six, not the first three. The starting account within the
chosen set rotates by the same seed.

**A default is honoured, not completed.** Deciding which providers may hold a
file is the point of the vault. Quietly adding a fourth account because the
default named three and one went away would put data somewhere its owner chose
not to. What a narrower selection costs is reported in the upload's warnings
instead.

Disconnecting an account prunes it from the default (`pruneDefaults`). A default
scheme describes a width, so it is dropped if the remaining accounts no longer
match it. Without a scheme, what is left is trimmed back to a valid spread: a
default of six that loses one becomes a default of three, not of five. Fewer
than two left clears the default.

**Re-encryption refills instead.** After a password change a file goes back to
the accounts it was on, topped up from what is connected if one has gone, and
topped up to its own width: a 4-of-6 file that lost an account goes back onto
six, not down to three. Narrowing would be a re-encode, which a top-up has no
business deciding.

The quota, if one is set, is a warning rather than a limit: see
[providers.md](providers.md#how-full-an-account-is).

## Object keys leak nothing

```
<archive id>-c<7-digit chunk index>-p<N>.sand    chunked (every current upload)
<archive id>-p<N>.sand                            stored whole (old files, thumbnail packs)
```

`<archive id>` is 128 random bits in hex, and N is the shard number from 1 to n.
Someone with full access to one account learns how many objects you store and
how big each is, but nothing about names, types or folder structure.

The chunk index is zero-padded so that listing an account lexically returns a
file's chunks in order, which is what recovery walks. It gives away nothing new:
objects were already groupable by their shared archive ID, and a file's size was
already the sum of its parts.

The key is a flat name with no directory components (`ShardKey`,
`ChunkShardKey`). Every backend already scopes the vault to a place of its own: a
folder on Dropbox, Box and OneDrive, a prefix on S3 and WebDAV, the chosen
directory for a local or synced folder. Nesting a further directory inside that
only buried each part deeper. Staying flat also means Google Drive, which has no
paths, ends up with exactly the same part names as everywhere else.
`provider.ValidateKey` refuses an empty key, `.`, `..`, or anything with a
separator, so a bug upstream becomes an error rather than an emptied folder.

## Changing which accounts hold a file

Placement is decided at upload, but it is not the last word. An account gets
expensive, fills up, or stops being one you trust. `Vault.Relocate` moves a file,
or a folder and everything under it, onto a chosen set of accounts.
`Vault.PlanRelocation` prices the move from the index alone, without contacting
any account. (`sand relocate`, `POST /api/relocate`, with `--dry-run` /
`"preview": true`.)

**A part is portable because its name doesn't say where it lives.** The object
key comes from the archive ID and shard number only. So moving shard 3 from
Dropbox to Box is a `Get` and a `Put` of the same bytes under the same key: no
key is touched, nothing is decompressed or re-encoded, and the file keeps its
archive ID, hash, chunk layout and key generation. The vault has to be unlocked
only to know what to copy and to read the accounts' credentials.

**Only what has to move, moves.** Placement is a set, not a sequence. The
planner first lets every shard already on a kept account stay, then hands out
the rest:

| Currently on | Asked for | What happens |
|---|---|---|
| A, B, C | A, B, D | The shard on C moves to D. The others aren't read and their index rows aren't rewritten |
| A, B, C | C, A, B | Nothing. Order doesn't matter |
| A, B, C | D, E, F | All three move |
| A…F | A…E, G | One shard moves. Six to six is the same scheme |
| A, B, C | A…F | **Rebuilt.** 2-of-3 and 4-of-6 share no shards |
| A…F | A, B, C | **Rebuilt** the other way. The shards on D, E and F are erased |
| A, B, C | A, B | The shards on A and B stay. Under `strict` the third is erased (`Drop`); under `redundant` it doubles up, exactly as an upload to two accounts would |
| A, B (shard 3 never stored) | A, B, D | **Rebuilt** (`repair`). The missing shard exists nowhere to copy from |

Shards on an account that is no longer connected can't be copied. They are left
recorded where they are (`Stranded`), not silently dropped.

**A short file is always rebuilt.** An upload commits with as few as k shards,
so a cloud that wasn't answering at the time leaves the file permanently short.
The missing shard doesn't exist anywhere to be copied, so `planFileRelocation`
takes the rebuild branch whenever the file has fewer shards than its scheme,
even for the same code, and marks the row `repair`. It is the only way a missing
shard is ever put back, and the browser's list of short files sends each row
through it. A file below k can't be gathered at all: it fails on its own and the
rest of the relocation carries on.

**Rebuilding from the replica.** `RelocateFrom` takes the source each file is
read from: `ReadParts` (everything above) or `ReadReplica` (`--from replica`,
`"source": "replica"`). From the replica, nothing is kept or carried, because
the parts are what is not trusted. Every file in scope that has a replica is
rebuilt (`from_replica`) through the re-encryption path: gathered chunk by chunk
from the replica folders, cut again under the code the chosen accounts call for,
committed in one index write, and the old parts erased wherever they can still
be reached (anything that won't erase is recorded as a stray). The replica itself
isn't touched. A file with no usable replica is marked `no_replica`, left
exactly as it is, and counted separately. `either` is refused, because a rebuild
has to say which copy it believes. Replica rebuilds run in the re-encryption
slot, so one can't meet a paced password change on the same file.

A relocation can't produce a placement an upload couldn't, which is also why it
refuses a destination count that names no scheme.

**Copy, commit, erase, one file at a time.** Between the copy and the commit,
both accounts hold the shard and the index still names the old one, so reads
keep working. After the commit the index names an account that certainly has the
bytes, and only then is the original erased. That ordering means:

- An interruption leaves an unreferenced copy (litter) rather than a file
  missing a shard.
- A shard that won't copy is reported and left where it was. Running the
  relocation again moves exactly that shard.
- A commit is refused if it would leave a file below k shards.
- A file rewritten underneath the move (by a re-encryption, say) is spotted by
  its archive ID and left alone, and the copies made for it are erased.

A folder's thumbnail pack is moved at the end of a folder relocation, but it is
gathered and re-scattered rather than moved shard by shard, because it is small
and derived. A pack that won't move is only a warning; the browser can draw the
thumbnails again.

## Moving within the vault costs nothing

Changing where a file *appears* copies nothing. A file's folder is a field on its
index entry, and its object keys never mention the folder or the file name. So
`Vault.Move` rewrites one field and returns. A 4 GB film moves as fast as a note,
and its parts stay where they are.

**Renaming is the same call.** `Move(id, dir, name)` treats an empty argument as
"leave this half alone", so a rename is a move that keeps the folder. That gives
one entry point, one set of collision rules, and one place where `SanitizeName`
runs. `sand mv` reads its destination the same way.

`Vault.MoveFolder` is the same thing one level up, and covers renaming a folder.
A folder is a path in the index, so a move rewrites every entry beneath it, at
any depth, in a single index write with an in-memory rollback if the write fails.
A loop over `Move` would leave a window where half a tree answered to the old
name. Everything keyed by folder (thumbnail packs, film-lookup settings,
maintenance policies, folder pictures, replicated-folder settings) is re-keyed
in the same write and restored by the same rollback, with no network work.

What the vault refuses, whichever caller asked:

- The root can't be moved, and a folder can't move into itself or a descendant.
- The destination's parent must already exist (as for `Mkdir`), and the
  destination itself must not. A move never silently merges two trees.
- A name already taken in the destination folder is refused for that file alone.

`GET /api/folders` returns every folder in one answer for the browser's
destination picker, including folders that exist only because a file sits
beneath them. `POST /api/files/move` moves a batch in one index write.
Exact request shapes are in [reference/api.md](../reference/api.md).
