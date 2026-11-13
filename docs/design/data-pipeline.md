# The data pipeline

How SAND Vault turns a file into shards on several clouds and back: storing,
opening, reading at an offset, the erasure code, the part file format, and how
the pipeline sizes itself to the machine. The keys and integrity checks are in
[cryptography.md](cryptography.md). Which accounts the shards go to is in
[placement.md](placement.md).

## Storing a file

```
  bytes in
     │
     ├─► SHA-256 of the whole file ───────────────► recorded, and sealed into every part
     │
     ▼
  cut into fixed-size chunks (16 or 32 MiB, see "Sizing to the machine")
     │
     ▼  for each chunk, a bounded window of them at once
 ┌────────┐   ┌──────────────────┐   ┌───────────────┐
 │compress│──►│ erasure code:    │──►│ seal each     │
 │ (zstd) │   │ k data + n−k     │   │ shard with    │
 └────────┘   │ parity shards    │   │ AES-256-GCM   │
              └──────────────────┘   └──────┬────────┘
                                            │ concurrent PUTs, one shard per account
               ┌────────────────────────────┼────────────────────────┐
               ▼                            ▼                        ▼
          account A                    account B                account C
   <id>-c0000000-p1.sand        <id>-c0000000-p2.sand    <id>-c0000000-p3.sand
```

The default code is 2-of-3: each chunk becomes three shards, any two of which
rebuild it. Wider codes are covered in [placement.md](placement.md#schemes-and-how-wide-a-file-is-spread).

The rules that make the result trustworthy:

- **A shard is all or nothing across the file.** If a shard fails on any chunk,
  the chunks it did reach are erased and the shard is not recorded. A shard in
  the index therefore always means every object behind it exists, and delete,
  health checks and recovery all rely on that (`scatterChunked`,
  `internal/vault/chunked.go`).
- **The upload commits if at least k distinct shards landed.** k is the
  minimum that can still be rebuilt. A file that commits short of n carries a
  warning and shows up in the list of short files. Below k, everything written
  is erased rather than left as orphans.
- **The index is written last.** The entry is added only after storage
  succeeds. If the index write fails, the parts are rolled back.
- **In-flight parts are reserved.** From the moment an archive ID is minted
  until the commit, the ID is held in an in-flight registry, so a concurrent
  stray-parts sweep cannot mistake the new parts for abandoned ones
  (`internal/vault/inflight.go`).

### Two entry points

| Call | Used by | How it reads the input |
|---|---|---|
| `Upload` → `scatterChunked` | callers that already hold the bytes | from memory |
| `UploadStream` → `scatterStream` | WebDAV PUT, SFTP import, snapshot restore, conversion, re-encryption | spooled to a temporary file first |
| `UploadStreamAt` | the HTTP upload handler | reads the multipart temp file in place, so it is not spooled twice |

The stream is spooled because every part carries the whole-file SHA-256, so the
hash has to be known before the first chunk is sealed, and a stream only gives
it up after its last byte. The spool holds plaintext briefly. It lives in the
vault's own directory rather than the system temp directory, so it gets the
same protection as the vault file (and stays off a Raspberry Pi's tmpfs). It is
removed on every exit path. One left behind by a crash is a working file that
the maintenance scan reports
(see [guide/maintenance.md](../guide/maintenance.md)).

## Opening a file

```
 read placement from the index
     │
     ├──────────────┬──────────────┐   every shard of the chunk requested at once
     ▼              ▼              ▼
 account A      account B      account C
     │              │              ╳ offline
     └──────┬───────┘
            ▼  first k good shards win; the rest are cancelled
   ┌─────────────┐   ┌────────────┐   ┌──────────┐   ┌──────────────┐
   │ open shard  │──►│ reconstruct│──►│decompress│──►│ verify       │──► bytes out
   │ AES-256-GCM │   │ (RS / XOR) │   │  (zstd)  │   │ (whole file) │
   └─────────────┘   └────────────┘   └──────────┘   └──────────────┘
```

Reads are a **race, not a sequence**. All shards are requested at once and the
chunk is rebuilt as soon as k distinct, valid shards have arrived
(`gatherChunk`). A slow or unreachable account costs nothing on the read path:
it just loses the race.

Each shard is opened as it lands (`archive.OpenShard`) and must be the archive,
chunk and shard number the index asked for. A shard that fails is counted as a
failure and the race continues, so a file is unreadable only when fewer than k
of its shards are good. See
[cryptography.md](cryptography.md#a-shard-that-is-wrong-is-a-shard-that-is-lost).

The whole-file SHA-256 is checked when a file is read end to end. A partial read
relies on the per-shard GCM tags instead.

### The race is also a measurement

Every read times each account against the others. Nothing gets slower when one
cloud stops pulling its weight; the others carry it until the day two are
offline. So the result is kept (`internal/vault/readstats.go`): per account,
races entered, races won (an answer that arrived in time to be used), answers
too late to be used, downloads cancelled once enough had arrived, failures, and
response times. The reader records the winners as it picks them. The losers are
drained on their own goroutine, so a read never waits on accounts it no longer
needs.

| Figure | Source | Endpoint |
|---|---|---|
| Read race | `readstats.go` | `GET /api/reads?window=today\|month\|year\|all` |
| Transfers: bytes and objects in (`Put`) and out (`Get`), and failed calls, per account | `transferstats.go`, through `Vault.meteredPut` / `meteredGet` | `GET /api/transfers` |
| Throughput per cloud and direction, and the vault's own speed per file kind | `throughput.go` | `GET /api/throughput` (Mbps) |

All three are counted in daily buckets per account. A day is the smallest window
anyone asks about and sums into every larger one. Daily buckets are kept for
400 days beside an all-time total, so pruning thins the shape without changing
the sum. `POST /api/reads/forget` clears them.

Every object the vault moves goes through `meteredPut` / `meteredGet` (uploads,
the read race, moves between clouds, conversions, the index backup), so nothing
is counted twice or missed. A cancelled call or a missing object counts as
neither traffic nor failure. Nothing is counted while the vault is locked.

The buckets live in a sidecar, `<vault>.reads`, not in the vault file: counting
into the vault file would mean a write on every chunk of every stream to the
one file everything else depends on. The sidecar is sealed with AES-256-GCM
under `crypto.DerivePurposeKey(dataKey, "sand-read-history-v1")`, because which
clouds you read from, and when, is as private as the index. It is written at
most every 30 seconds while reads are happening, again on lock, and again before
a password change retires the key it was sealed under. A sidecar that will not
open is reported and started fresh rather than treated as damage.

The web UI draws these under **Statistics** in the accounts drawer: **Read
race**, **Transfers**, **Throughput** and **Storage breakdown**. The storage
breakdown is computed in the browser from the account list it already holds.

## Reading at an offset

`Vault.OpenReader` returns a `ChunkedReader`, an `io.ReaderAt`. Given an offset
it divides by the file's chunk size, gathers that one chunk by the race above,
and returns the bytes inside it.

It is `io.ReaderAt` rather than `io.ReadSeeker` on purpose. A FUSE mount is
handed an offset and a length; an `http.File`'s seek-then-read is a thin
wrapper over `ReaderAt` (`io.SectionReader`). Making the primitive the narrower
one keeps each filesystem layer an adapter. `OpenReadSeeker` wraps it for
`http.ServeContent` and WebDAV.

Behind it:

- **A bounded cache of decrypted chunks**, shared by every reader on the vault.
  A player scrubbing through a film asks for the same chunk repeatedly. The
  cache is sized in bytes, not chunks, because chunk size is per file. It holds
  plaintext, so locking the vault drops it. It is released rather than
  overwritten, because a reader may still be copying out of a chunk it was
  handed.
- **Single-flight per chunk**, because a player opens several connections at
  once and would otherwise gather the same chunk once per connection. (A known
  weakness: a second waiter inherits the first caller's cancellation. See
  [streaming-performance.md](streaming-performance.md).)
- **Read-ahead for sequential readers.** `OpenSequential`, used by exports,
  zips, snapshots and checksum jobs, and any reader whose next chunk follows its
  last, fetches the next `ReadAhead` chunks through the same single flight. A
  reader that seeks starts none, since the shared cache serves that case. A
  failed prefetch is forgotten, so the read that wanted the chunk fetches it
  normally and gets its own error. The sequential reader keeps its chunks out
  of the shared cache, so a one-pass copy does not evict what a player is using.

The reader holds no key. Every cache miss re-reads the vault's data key under
the lock and zeroes its copy afterwards, so a reader left open across a lock
stops working instead of carrying on with a captured key.

## Files stored before chunking

Files stored before chunking existed can't be read at an offset. Their parts are
halves of one AES-GCM-sealed blob, and GCM releases no plaintext until the tag
over all of it verifies. That is the reason chunking replaced the format.

So `OpenReader` refuses such a file with `ErrNeedsConversion` (HTTP `409
NEEDS_CONVERSION`) rather than quietly rebuilding the whole thing behind an
interface that promises cheap seeks. `Vault.Convert` (`internal/vault/convert.go`)
moves a file out of the old format:

| | |
|---|---|
| Only when asked | `sand vault convert`, or `POST /api/files/{id}/convert`. A read never triggers it. An earlier design converted on first read, which started a whole download and re-upload at the worst moment, took a 16 GB machine down repeatedly, and restarted on the next read because an interrupted conversion commits nothing. |
| Bounded | It reads the old format sequentially, the only way it can be read (`archive.DecodeLegacyTo`). The two halves are concatenated through an `io.MultiReader`, and the plaintext goes to a spool beside the vault and then into `scatterStream`. |
| In place | The index moves to the new parts in one write. The file stays readable throughout, and the old parts are erased only afterwards. |

`GET /api/conversions` lists what is left. `archive.DecodeBytes` remains for
small payloads already in memory: thumbnail packs and standalone restore.

## The erasure code

A chunk is cut into **k data shards and n − k parity shards, any k of which
rebuild it**. k and n are chosen per file (see [placement.md](placement.md)), and
every shard records them in its header.

The code is a systematic Cauchy Reed–Solomon code over GF(2⁸)
(`internal/splitter/erasure.go`). Three properties of that choice matter:

- **It is systematic.** The first k shards are the compressed chunk cut into k
  consecutive pieces. Only the parity is computed. A read that collects the data
  shards rebuilds by concatenating them, with no field arithmetic. And because a
  shard is a slice of the stream rather than a transform of all of it, one
  compromised account plus the key yields at most 1/k of the file.
- **The parity rows are Cauchy.** Every square submatrix of a Cauchy matrix is
  invertible, which is exactly the MDS guarantee that *any* k of the n shards
  rebuild the data, without the change of basis a Vandermonde construction
  needs. A decode picks the k rows it has, inverts that k×k matrix by
  Gauss–Jordan, and multiplies.
- **The field is GF(2⁸), not GF(2).** This is what lets the code widen. Before
  format version 4 a chunk was two halves and their XOR. XOR is addition in
  GF(2), where two symbols have only three non-zero combinations: A, B and A⊕B.
  Over GF(2⁸) every byte is a usable coefficient, and there is room for up to
  255 shards.

Formats 1 to 3 use the XOR construction and are still read
(`splitter.ReconstructXOR`). They are all 2-of-3, the only code they can express.

Before coding, the compressed chunk is padded up to a multiple of k. So a wide
code wastes up to k − 1 bytes per chunk, which is noise against a 16 MiB chunk.

### Reconstruction for 2-of-3

| s1 | s2 | s3 | Method |
|:--:|:--:|:--:|:---|
| ✓ | ✓ | any | concat(s1, s2), the systematic path, with no arithmetic |
| ✓ | — | ✓ | invert the 2×2 for rows {1,3}, then concat |
| — | ✓ | ✓ | invert the 2×2 for rows {2,3}, then concat |
| at most one | | | unrecoverable |

In general, **any k present means rebuildable and fewer than k means not**, with
no case analysis. That is the point of an MDS code. `erasure_test.go` checks every
k-subset where they can be enumerated, and a seeded sample plus the all-data and
parity-heavy edge subsets where C(n, k) is too large.

## Part file format

Each stored object is a self-describing `.sand` blob (`internal/sandfile`).
Everything needed to derive its key sits in the clear at the front. Everything
that describes the original file is inside the ciphertext. The whole cleartext
header is also the GCM associated data.

| Version | Written by | Key from | Unit |
|---|---|---|---|
| 1 | nothing (read only) | Argon2id over a password | whole file, metadata in the clear |
| 2 | standalone `sand archive`; folder thumbnail packs | Argon2id over a password | whole file |
| 3 | nothing now (read only) | HKDF over the vault's data key | one chunk, 2-of-3 XOR |
| 4 | every vault upload, conversion, re-encryption and recode | HKDF over the vault's data key | one chunk, k-of-n Reed–Solomon |

### Version 4: one shard of one chunk

```
Offset  Size   Field                          Cleartext header, also GCM
──────────────────────────────────────────────  associated data
0x00    4      Magic "SAND"
0x04    1      Version (4)
0x05    1      PartNumber (shard number, 1..n)
0x06    16     ArchiveID
0x16    4      ChunkIndex
0x1A    1      DataShards (k)
0x1B    1      TotalShards (n)
0x1C    12     AES-GCM nonce
──────────────────────────────────────────────
0x28    4      PayloadSize
0x2C    N      Ciphertext + 16-byte tag
                 └─ 32   OriginalHash (SHA-256 of the whole file)
                    8    OriginalSize   (the whole file)
                    8    CompressedSize (this chunk)
                    1    WasPadded      (this chunk)
                    4    ChunkCount
                    4    ChunkSize      (plaintext, fixed)
                    2    FilenameLength
                    var  Filename
                    var  this shard of this chunk
```

Version 3 is the same without the two shard-count bytes (nonce at 0x1A,
payload size at 0x26). k and n moved into the header in version 4 because a
reader has to know both before it can invert anything, and putting them in the
associated data means a shard can't be relabelled into a different scheme
without failing its tag.

Notes on the chunked layout:

- **No salt or Argon2 parameters.** The key comes from the vault's data key
  through HKDF ([cryptography.md](cryptography.md#chunk-keys-hkdf-sha256)). This
  is also why standalone mode still writes version 2: it has no vault and no
  data key, only a password.
- **`ChunkSize` is the plaintext length of every chunk but the last**, so the
  chunk holding an offset is the offset divided by it. It is recorded rather
  than inferred so a seek never has to consult an index.
- **The file description is repeated in every shard of every chunk.** That is
  ChunkCount × n copies of about a hundred bytes, which is nothing against a
  16 MiB chunk. In return, any k shards of any one chunk still say which file
  they came from, so an offline restore needs no index.
- **Costs.** Per-chunk compression gives up whatever zstd would have found
  across chunk boundaries (nothing for video, a little for large text), and the
  whole-file hash can't be checked on a partial read.

`Entry.Chunked` decides which read path a file takes. The whole-file and chunked
decoders refuse each other's input rather than guessing.

### Version 2: a whole file

```
0x00    4      Magic "SAND"
0x04    1      Version (2)
0x05    1      PartNumber (1..3)
0x06    16     ArchiveID
0x16    16     Argon2id salt
0x26    4      Argon2 time
0x2A    4      Argon2 memory (KiB)
0x2E    1      Argon2 threads
0x2F    12     AES-GCM nonce
0x3B    4      PayloadSize
0x3F    N      Ciphertext + tag: the same metadata minus ChunkCount/ChunkSize,
               then this part's share of the compressed file
```

Version 1 held the filename, hash and size in the clear header, so one account
could read the name of every file it held a part of. Version 2 moved all of it
inside the ciphertext. Version 1 parts still restore, and still reveal what they
always did, so re-upload anything whose name matters.

## Sizing to the machine

Everything that decides how much is in flight or in memory comes from one
place, `internal/tuning`: the chunk size, the upload window, the read-ahead,
the chunk cache, the compression level and threads, the windows a move or
delete runs at, the connection pool per cloud, and the two ceilings an
unattended job won't pass. The package knows nothing about vaults.

| Type | What it is |
|---|---|
| `Machine` | CPUs and memory, read from the cgroup and `/proc`, or given |
| `Tuning` | every knob |
| `Derive(Machine) Tuning` | the one formula |
| `Config` | what the operator chose: a profile, a custom machine, and any knob set by hand |
| `Resolve` | works a `Config` out on the detected machine and checks it fits |

**Profiles.** `auto` (the default) is the formula over the detected machine.
`pi5` (4 CPUs, 16 GB) and `workstation` (24 CPUs, 64 GB) are the formula over
fixed machines, so there are no tables to keep in agreement. `custom` is the
formula over a machine the operator describes.

**The formula** is monotone and clamped at both ends:

- The chunk size steps once: 16 MiB below 32 GB of memory, 32 MiB from there.
- The windows follow the CPU count and are capped where a consumer cloud starts
  rate-limiting, not where the machine runs out.
- The cache is a sixteenth of memory (128 MiB to 8 GiB). The unattended rebuild
  and bundle ceilings are a quarter of it.
- On a machine too small for the floors, `fit` shrinks the cache, then the
  read-ahead, then the upload window until the `Footprint` (one upload at full
  window, one sequential reader at full read-ahead, the cache full) is within
  half of memory. The other half is for the index, the connections and the
  runtime. `GOMEMLIMIT` is set to 85% of the same figure
  (`internal/server/memlimit.go`).

**What is stored is the choice, not the numbers**, in `machine.json` beside the
vault. It sits beside the vault rather than inside it because it is needed at
open, before there is a password, and because it belongs to the machine: a vault
restored onto a different box shouldn't bring a Pi's windows with it. Deriving
the numbers on every start means a machine that gains memory, or a later release
with a better formula, takes effect without a migration.

**Applying it.** Each layer takes only what it is handed:

- `Vault.SetTuning` stores the tuning behind an atomic pointer. Each operation
  reads the whole struct when it starts, so an upload in flight keeps its window,
  and the next one gets the new value. The chunk cache is resized in place.
- `compress.Configure` sets the level and threads for the next encoder. An
  encoder is built per chunk and closed after it. Keeping one was tried: an
  encoder holds tens of megabytes of history and block buffers between calls,
  times the upload window, which on a Pi raised the memory floor under every
  later read.
- `provider.ConfigureTransport` sizes the shared HTTP client's idle pool to the
  window. Go's default of two idle connections per host meant every request past
  the second in a window of twelve was a fresh TLS handshake.

None of this changes what is stored. A file records the chunk size it was cut
with, and a zstd stream names its own format.

`GET /api/machine` returns the resolution plus the vocabulary (every knob with
its range and hint, every profile with its label), so the settings page knows
nothing the server didn't tell it. `PUT` sends the whole choice and is refused,
not clamped, when it wouldn't fit. `DELETE` goes back to automatic.
`sand serve --profile` (or `SAND_PROFILE`) pins the profile from the unit file,
and the page then says so (`PROFILE_PINNED`). The knobs are listed in
[reference/configuration.md](../reference/configuration.md).
