# Cryptography and integrity

The keys SAND Vault uses, how each stored object is sealed, what catches
damage, and the slower checks built on top: the deep check, the checksums
ledger, and snapshots. The on-disk part layout is in
[data-pipeline.md](data-pipeline.md#part-file-format). How the vault file
itself is sealed, and what a password change does, is in [vault.md](vault.md).
The threat model is in [security.md](../security.md).

## Keys at a glance

```
 vault password ──Argon2id──► section key ──AES-GCM──► vault file sections
                                                         └─ wraps the data key (random, 256-bit)
 data key ──HKDF(salt = archive ID, info = label ‖ chunk index)──► chunk key  (v3/v4 parts)
 data key ──HKDF(purpose)──► purpose key  (read-history sidecar)
 hex(data key) ──Argon2id──► whole-file key  (v2 thumbnail packs)
 typed password ──Argon2id──► whole-file key  (standalone `sand archive`)
```

There are two keys, not one. Parts are encrypted under a random 256-bit data
key, which the password only wraps, so part encryption doesn't inherit a weak
password. A password change mints a **new** data key and re-encrypts every file
onto it. Re-wrapping the old key would leave every part on the accounts readable
to anyone holding the old password and an old copy of the vault file. Sub vaults
have their own password and their own data key.

## Key derivation: Argon2id

Every **password** is stretched with Argon2id (`internal/crypto/argon2.go`):

| | Vault, sub vault, standalone | Recovery kit, snapshots |
|---|---|---|
| Time | 3 passes | 8 passes |
| Memory | 64 MiB | 512 MiB |
| Threads | 4 | 4 |
| Salt | 16 random bytes, per vault / per file | 16 random bytes |
| Output | 32 bytes | 32 bytes |

The vault's parameters are what a password typed several times a day can
afford. A kit or snapshot is opened once in years, kept on storage somebody else
runs, and may be sealed under a password a person chose, so it pays eight times
the memory.

## Chunk keys: HKDF-SHA256

Argon2id makes guessing a low-entropy password expensive. It buys nothing for a
key nobody typed, and a chunked file would pay for it once per chunk, which
adds up to minutes of key derivation for one large video. So a chunk key is
derived (`crypto.DeriveChunkKey`):

```
chunk key = HKDF-SHA256(
    secret = the vault's data key,
    salt   = the 128-bit archive ID,
    info   = "sand-chunk-key-v3" ‖ chunk index (big-endian u32))
```

The archive ID separates files. The chunk index separates chunks, so one
chunk's key says nothing about its neighbours. The argument for HKDF is that the
input already has 256 bits of uniform entropy, so stretching it was never what
protected it. The same derivation serves version 3 and version 4 parts.

`crypto.DerivePurposeKey(dataKey, purpose)` is the same idea for a standing
purpose rather than a file. Its only user so far is the read-history sidecar
(`"sand-read-history-v1"`).

Folder thumbnail packs still use the whole-file format, keyed by Argon2id over
the hex of the data key (`shardPasswordFor`). That cost is why thumbnails are
packed a folder at a time (`internal/vault/thumbs.go`).

## Encryption: AES-256-GCM

- A 12-byte random nonce per part.
- The **whole cleartext header is the associated data.** That binds each
  ciphertext to its archive ID, shard number and, from version 4, its k and n.
  A shard can't be swapped for another file's, renumbered, or relabelled into a
  different scheme without the tag failing.
- In a chunked part the header also carries the **chunk index**, so the same
  binding stops a chunk being replayed at another offset of the file it really
  belongs to.
- A 16-byte tag.

## Integrity

| Layer | Mechanism | Catches |
|---|---|---|
| Per shard | GCM tag | Tampering, truncation, bit rot |
| Per shard | Cleartext header as associated data | Shard swapping, renumbering, relabelling the scheme |
| Per shard | Metadata sealed with the data | Edits to the name, hash or sizes |
| Per chunk | Chunk index in the associated data | A chunk replayed at another offset |
| Per chunk | Rebuilt length checked against the archive's chunking | A truncated or substituted chunk |
| Whole file | SHA-256 recorded at upload, checked after a full rebuild | Anything that slipped through |
| Vault | GCM tag on every section | A modified or truncated index |

The whole-file SHA-256 is the one guarantee a partial read can't give: reading
one chunk never touches the rest, so there is nothing to hash. A partial read
relies on the per-shard tag bound to the chunk index instead, which is what
makes serving an arbitrary byte range trustworthy.

## A shard that is wrong is a shard that is lost

The tag catches a bad shard. What the reader does next is the other half. The
read race opens each shard as it lands (`archive.OpenShard`) and holds it to
being the archive, chunk and shard the index asked for. One that fails is
recorded as a failure and the race goes on, so a file is unreadable only when
fewer than k of its shards are good. Before this, the first k arrivals were
decoded together, and a single damaged object on a fast account failed every
read of the file while every `Stat`-based health check called it whole.

A whole-file HTTP read asks for its first byte before writing any header. So a
file that can't be rebuilt comes back as an error that says which shards of
which chunk failed, not as a `200` with a declared length and no body.

## The deep check

`Vault.CheckFolder` / `CheckFile` (`internal/vault/verify.go`) is the one path
that reads all n shards. For each chunk it fetches every recorded shard and sorts
it:

| Verdict | Meaning |
|---|---|
| good | opens as what was written |
| `missing` | the account says the object is not there |
| `damaged` | there, but does not open as what was written |
| `unreachable` | no answer. Never read as missing, because a repair based on a rate limit would erase a part that still exists |
| `mismatched` | opens, but disagrees with the rebuild. The one fault no ordinary read can see |

It rebuilds from k good shards and recomputes all n from the result
(`archive.ShardData`) to find mismatches. It then holds the file to its length
and SHA-256, and holds the plaintext to `internal/filecheck`: a structural check
per format (JPEG, PNG, HEIC, PDF, ZIP-based documents, MP4/QuickTime, Matroska,
FLAC and others). The structural check reads a bounded amount and never decodes.

The verdict keeps those layers apart because the repairs differ:

- Bad shards under a matching hash are rebuilt from the good ones.
- An unreadable or mismatching file is rebuilt from its replica, if it has one.
- A matching hash over malformed contents is damage that arrived with the
  upload. The vault has no repair for it.

`RepairChecked` is a relocation onto the accounts the file is already on, with
the faulty shards treated as absent, or with the replica as the source (see
[placement.md](placement.md#changing-which-accounts-hold-a-file)). It refuses a
file whose archive ID changed since the check. The server runs check and repair
as background jobs (`jobWatch`, `internal/server/handlers_check.go`). The CLI
is `sand check --deep [--fix]`.

## Checksums and their ledger

A file's checksum is `Entry.Hash`. There is one record, and nothing adds a
second that could disagree with it (`internal/vault/checksums.go`).

**The ledger.** `checksums.gz` is that record written out, one `path, sha256`
line per file (`internal/checksums`). It is stored as an ordinary file at `/`
of each vault's tree through `Upload`, so it lands on the default clouds and
travels with the index. The encoding is deterministic (sorted, no timestamp in
the gzip header), so whether it would change is answered by hashing it against
the stored ledger's own `Hash`, and an unchanged ledger is never re-uploaded.
A server loop calls `WriteChecksumLedgers` on a tick (`--checksums-every`,
default 2 minutes) instead of on each index write, because writing the ledger is
an upload and an import of ten thousand files is ten thousand index writes. A
vault whose `UpdatedAt` hasn't moved since its last ledger is skipped.

**Verify and compute** are one loop over a folder's descendants, reading each
file through the sequential reader so nothing goes into the shared chunk cache.
Verify compares and writes nothing. Compute calls `RecordChecksum`, which sets a
hash only where none exists and only if the placement it read is still the
entry's, so a checksum taken at upload is never replaced. A whole-file HTTP read
of a file with no hash records one the same way, digesting only bytes that
arrive in order from offset zero.

**Pacing.** A `ChecksumControl` enforces a byte rate by sleeping out each read's
share. The rate can change while the job runs (debt owed at the old rate is
forgiven), and a pause is checked between reads and between files. Cancelling
returns the report so far, marked stopped. The server runs one such job at a
time (`handlers_checksums.go`), and a paused job doesn't count as activity for
the idle lock.

**A sub vault that shuts mid-job.** `scopeShut` is asked between files and after
any failure, in these jobs and in `CheckFolder` and `RepairChecked`. A shut
scope ends the job as stopped, with the reason in `StoppedBy` and what it had
done so far, never as a list of unreadable-file findings.

## Snapshots

A snapshot is one folder as one archive in one place, readable with its part
files and its password and no vault (`internal/snapshot`, with both ends in
`internal/vault/snapshot.go`). It deliberately shares none of the vault's format
or keys, because the case it exists for is the vault being gone.

The layers, outermost first:

```
part files   54-byte plaintext header, then a slice of the sealed stream
sealed       AES-256-GCM frames of up to 1 MiB, key = Argon2id(password, salt)
compressed   one zstd stream, SpeedBestCompression
archive      u32 length + JSON description, then a PAX tar of the tree
```

**The part header** is `SANDSNAP`, a version byte, the three Argon2id costs, a
16-byte salt, the snapshot's 16-byte ID and the part number. Every part carries
all of it, so a part found alone says what it belongs to, and a part from
another snapshot is refused by ID rather than by a cipher error. The costs come
from a file somebody else has held, so the reader bounds them (2 GiB of memory,
64 passes) before paying them. New snapshots use the kit's costs.

**The sealed stream** is framed because a snapshot can be larger than the
machine's memory. Each frame is a four-byte length and the ciphertext. The nonce
is the frame number, which is safe because the key is used for one snapshot,
derived from a fresh salt. The associated data is the snapshot ID plus one byte
saying whether the frame is the last (also the length's top bit). So reordered
frames, a frame from another snapshot, and a stream cut at a frame boundary all
fail to open. A failure on the first frame is reported as a wrong password;
on any later frame, as damage. A reader has verified the whole snapshot only
when it reaches EOF.

**The description** (path, counts, time) sits ahead of the compressed stream,
so trying a password costs one frame and no decompressor.

**The tar** carries folders, so empty ones survive, and files with their
modification time and, in a PAX record, the SHA-256 the index held. Paths are
relative to the snapshotted folder and are re-validated on restore.

**Parts** go through `snapshot.Store`. `Create` begins a part with no name, and
`Commit(name)` makes it visible whole or not at all. The name comes last because
only the end of the stream says which part is final. The final part is named
`.last`, which lets a listing tell complete from interrupted with no index:
`snapshot-<UTC stamp>-<8 hex of the ID>` alone (one part), `.partNNN`, or
`.partNNN.last`, with the `.sandsnap` extension. The name carries no folder or
file name.

There are two stores:

- **A connected account** (`accountSnapshots`) buffers a part and `Put`s it,
  because a backend takes an object as one slice. That is why a cloud part is
  capped at 256 MiB. Parts sit in the account's parts folder. Nothing else
  there ends in `.sandsnap`, and the orphan scan claims only keys matching its
  own strict pattern.
- **A machine** (`machineSnapshots`) streams a part over SFTP to a temporary
  name and renames it on commit (`sftp.Pending`), inside the source's root, so a
  part can be any size.

Both refuse to remove a name that isn't a part's.

**Taking a snapshot** reads the folder-zip plan (`PlanFolderZip`) through the
sequential reader into the writer. It is all or nothing: a failure or a stop
erases the parts written, because a snapshot missing a file is worse than none.

**Restoring** feeds each item to `UploadStream`, an ordinary upload onto the
restoring vault's clouds, after `heldAt` compares it with what is already at
its path. Same size and hash is skipped, which is all resuming takes. A
different file is kept and reported unless `Replace` is set, and is never
stored beside the original under a numbered name. One file failing doesn't stop
the rest, but damage in the stream does.

Both run as one background job (`handlers_snapshots.go`). The password travels
in the request body to the job and appears in no status, key or log. User-facing
steps are in [guide/folder-tools.md](../guide/folder-tools.md).
