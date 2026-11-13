# Design choices

The decisions that shape Sand Vault, each with the reasoning behind it and the
alternatives that were turned down. Each entry is short and links to the design
page that has the mechanism. For how the pieces fit together, see
[architecture.md](architecture.md).

## Storage and placement

### Erasure coding, not replication
**Decision:** every chunk is cut into k data shards and n−k parity shards
(systematic Cauchy Reed–Solomon over GF(2⁸)), any k of which rebuild it, and
each shard goes to a different account. **Why:** one account holds a fragment
that means nothing alone, and storage is n/k (1.5× by default), not n×. The
code is systematic, so a read that gets the data shards just concatenates
them. **Rejected:** whole copies per account, which hand every provider the
file. `MinData = 2` refuses 1-of-n outright. Replicas exist, but only as an
opt-in second placement (`sand replicate`).
→ [design/data-pipeline.md](design/data-pipeline.md)

### A configurable k-of-n, defaulting to 2m-of-3m
**Decision:** the number of accounts chosen names the scheme: 3 → 2-of-3,
6 → 4-of-6, 3m → 2m-of-3m, up to 255 shards. `--scheme` picks any other
k-of-n. **Why:** holding k/n at 2/3 keeps storage at 1.5× at every width, so
widening costs accounts, not bytes, while both losses survived and accounts
needed to collude grow. Three is the default whatever is connected, because
widening should be a decision. **Rejected:** the original fixed 2-of-3 XOR
construction (GF(2) has no third independent combination, so it cannot widen;
formats 1–3 are still read), and counts that are not whole groups without an
explicit scheme.
→ [design/placement.md](design/placement.md)

### Placement is a security decision: `strict` by default
**Decision:** under `strict`, no account ever holds two shards of one file, and
one account is refused. `redundant` allows doubling up. **Why:** any k shards
plus the key rebuild the file, so k shards on one account is one provider able
to read it. **Rejected:** filling up whatever is connected. An explicit or
default account set is honoured, never quietly topped up with an account the
owner did not choose.
→ [design/placement.md](design/placement.md)

### Object keys leak nothing
**Decision:** a part is stored as `<random 128-bit archive id>-p<N>.sand`, or
`…-c<chunk>-p<N>.sand` when chunked, flat, with no directories. **Why:** an
account learns how many objects there are and their sizes, and nothing about
names, types or folders. Since part format v2 the filename, hash and size are
sealed inside the ciphertext. A key that names no location also makes a part
portable: moving it between accounts is a Get and a Put of the same bytes.
**Rejected:** nested `sand/<id>/` paths (no gain, and Google Drive has no
paths anyway), and the v1 cleartext header.
→ [design/placement.md](design/placement.md)

### Chunked parts, so a file can be read at an offset
**Decision:** files are cut into fixed chunks (16 MiB by default, 32 MiB on
machines with 32 GB or more), and the pipeline runs per chunk. **Why:** GCM
releases nothing until the tag over the whole blob verifies, so a whole-file
part cannot be seeked. Per-chunk parts let a player start a film from one
chunk. **Rejected:** smaller chunks, which multiply objects against APIs that
rate-limit per request. The costs accepted: a little lost compression across
chunk boundaries, and no whole-file hash on a partial read.
→ [design/data-pipeline.md](design/data-pipeline.md)

### Sizing comes from a formula, and the choice is stored, not the numbers
**Decision:** `internal/tuning` derives every window, cache and ceiling from
the machine (CPU, memory, cgroup). `machine.json` beside the vault records the
profile chosen, not the derived values. **Why:** a later release's better
formula applies on the next start, and a vault restored onto another machine
does not bring a Pi's limits with it. **Rejected:** three hand-kept tables, and
freezing derived numbers.
→ [design/data-pipeline.md](design/data-pipeline.md)

## The vault and its keys

### One encrypted vault file, plus a replicated manifest
**Decision:** all state is one JSON file whose sections are sealed under an
Argon2id-derived key. A copy of the index, `manifest.sand`, is pushed to every
connected account (paced, 5 seconds apart by default). **Why:** without it,
losing the vault file stranded everything, because the data key existed in one
place. The copy carries its own KDF parameters, since the vault is what the
reader has lost. **Rejected:** putting credentials in the copy, which would
make one compromised account a key to all the others. The backup also refuses
to write under `redundant` with fewer than three accounts, where it would leave
the password as the only protection.
→ [design/vault.md](design/vault.md)

### Two keys, and a password change rotates the data key
**Decision:** parts are sealed under a random 256-bit data key, which the
password-derived key wraps. Changing the password mints a new data key and
re-encrypts every file onto it, one file at a time. **Why:** part encryption
does not inherit a weak password, and re-wrapping alone would leave every part
readable by the old password plus any old manifest copy. **Rejected:** a
re-wrap only (now opt-in as `--no-migrate`, with that cost stated), and moving
un-migrated files into a sub vault under the old password, which keeps the old
password alive.
→ [design/vault.md](design/vault.md), [design/paced-rekey.md](design/paced-rekey.md)

### Per-chunk keys via HKDF, not Argon2id
**Decision:** a chunk key is HKDF-SHA256 over the data key, salted with the
archive ID, with `"sand-chunk-key-v3" ‖ chunk index` as info. **Why:** Argon2id
makes guessing a low-entropy password expensive and buys nothing against 256
random bits. Run per chunk, it would be minutes of key derivation per large
video. Argon2id stays where a person typed something: the vault key,
standalone mode, kits and snapshots. **Rejected:** stretching the hex of the
data key per part, which is what thumbnail packs were invented to amortize.
→ [design/cryptography.md](design/cryptography.md)

### A bad shard is a lost shard
**Decision:** each shard in the read race is opened and checked as it lands.
One that fails its GCM tag or header binding is dropped, and the race goes on.
**Why:** a file should be unreadable only when fewer than k shards are good.
**Rejected:** decoding the first k arrivals together, where one damaged object
on a fast account failed every read while stat-based checks said "whole".
→ [design/cryptography.md](design/cryptography.md)

### Sub vaults have their own password, and no key crosses the boundary
**Decision:** a sub vault's section is sealed under Argon2id of its own
password. The main password sees only its name, date and an inventory of
object IDs and sizes. It is never on the `/dav` share. **Why:** what goes in it
must not be readable by whoever holds the main password. The inventory is the
minimum needed to delete a forgotten sub vault's parts and to guard
disconnects. **Rejected:** copying the source key generation into the
destination on assignment, which handed over the key to everything else in
the source.
→ [design/vault.md](design/vault.md)

### Several machines share the vault through the manifest copies, with no server
**Decision:** each machine keeps its own vault file. The `manifest.sand` copies
are the channel: a vector clock per machine says whether one copy supersedes
another, and rows merge by `IndexedAt`/`PlacedAt` stamps, with 90-day
tombstones. **Why:** the copies already exist for recovery, and a coordination
server would be one more thing to trust and run. **Rejected:** last-writer-wins
on whole indexes, which lost a rename against a concurrent re-encryption.
Credentials and sub-vault contents deliberately do not travel. This syncs the
index. It is not a file-sync engine.
→ [design/vault.md](design/vault.md)

### The recovery kit carries what the cloud copies must not
**Decision:** `sand vault kit export` writes a sealed zip, opened by a
generated 25-character code, that carries the credentials and the vault key.
**Why:** every cloud-side route back leaves reconnecting each account by hand
as the real cost, and a code survives a forgotten or changed password.
**Rejected:** shipping `vault.sand` verbatim, which welds the kit to the vault
password, cannot be inspected, and makes the store format a kit format by
accident.
→ [design/recovery-kit.md](design/recovery-kit.md)

## Behaviour

### Partial success is a result, not an error
**Decision:** an upload answers per file (`stored` plus `results[]`, each with
`ok`, `warnings` or `error`). A delete drops the index entry even when an
account is unreachable, and says so. An upload commits on k of n shards and
flags the file as short. **Why:** one file of twelve failing is normal, and a
dead provider must not pin a file in the browser forever. **Rejected:**
all-or-nothing requests, and the single giant multipart body, which the
browser now sends as batches.
→ [design/server.md](design/server.md)

### Crossing a quota is a warning, not a wall
**Decision:** a quota (`Config.Quota`) is checked after the file is in the
index. The accounts it took past their line come back in `warnings`, and the
upload picker checks free space beforehand. **Why:** the parts of a file are
placed together, and dropping the one that would cross a line leaves the file
on fewer accounts than its code promises, a silent loss of durability for a
line no provider enforces. **Rejected:** refusing parts mid-scatter.
→ [design/providers.md](design/providers.md)

### A repair is always a rebuild
**Decision:** a file short of shards is gathered from what can be read, cut
again, and written out as a full set (`migrateFile`), whether by hand, from the
short-files list, or by a folder's schedule. **Why:** forced, not chosen: the
cheap relocation path copies a shard from where it is, and a missing shard is
nowhere. Unattended repair is bounded (`RebuildLimit`, `MaxRepairs`) and never
narrows a scheme unless told to (`Narrow`). **Rejected:** re-deriving one
missing part in place, which v2 listed as "not built" and the relocation
planner made unnecessary.
→ [design/index.md](design/index.md), [design/placement.md](design/placement.md)

### Sweeps re-scan before they erase
**Decision:** orphan, leftover and old-version sweeps run their own scan again
before deleting, and refuse whole classes of account: one carrying another
vault's manifest, one that could not be listed, an empty vault on accounts
with no index it wrote. **Why:** between being shown a figure and agreeing, an
upload, a reconnect or another machine can change the answer. "No index points
at it" is not "abandoned" in those states. **Rejected:** trusting the caller's
list.
→ [design/vault.md](design/vault.md)

### Organizing is planned once, then run as ordinary per-item calls
**Decision:** flatten, date sort, purge and duplicates read one survey from the
server, then the browser runs the existing move, mkdir and delete endpoints one
item at a time. **Why:** a run that stalls at item 40 of 200 has done 39 and
can name the one that refused, and folder removal stays non-recursive, so a
tidy can never take a file with it. **Rejected:** server-side bulk endpoints
that could half-succeed with no way to say which half.
→ [design/index.md](design/index.md)

### A folder's picture is a pointer, and only ever a chosen one
**Decision:** `Manifest.FolderArt` maps a folder to the ID of a file that
already has a thumbnail, written only by `SetFolderArt`. **Why:** nothing new
is stored on any account, there is nothing to keep in step, and a picture
that loses its file just disappears. **Rejected:** an automatic pick per folder
(a hash-chosen poster), because which film stands for a trilogy is taste, and
nothing should happen to your files unasked. Also rejected: a copy of each
cover in the parent's thumbnail pack.
→ [design/index.md](design/index.md)

### Film lookup is off until a folder asks
**Decision:** matching against The Movie Database is switched on per folder
and inherited downwards. Text goes into the encrypted index, the poster
becomes an ordinary scattered thumbnail, and the API key lives in the vault's
`settings` section, never in the replicated manifest. **Why:** `internal/movie`
is the only code that talks to a third party, and a lookup discloses a guessed
title and the machine's address. **Rejected:** library-wide matching by
default.
→ [design/index.md](design/index.md)

### Git repositories are stored as bundles, using the machine's git
**Decision:** a repository is one `git bundle` file. A weekly `ls-remote`
digest decides whether to fetch at all. `internal/git` shells out to the
installed git. **Why:** loose objects would scatter thousands of objects and
re-scatter on every repack, and a bundle is `git clone`-able without Sand
Vault. The machine's git already has the SSH agent, credential helpers and
host aliases. **Rejected:** a built-in git implementation, which could reach no
private repository. URLs are allow-listed and `ext::` is disabled, because a
URL is hostile input.
→ [design/index.md](design/index.md)

## Surfaces

### One static binary with the web client embedded
**Decision:** `sand` is CLI, server, web UI and WebDAV in one `CGO_ENABLED=0`
binary. The built React client is embedded with `go:embed` and committed, and
every backend is written on the standard library. **Why:** it runs on a
Raspberry Pi or a desktop with one file to copy, and builds from a bare
checkout with no Node. The UI makes zero third-party requests, so fonts are
embedded at build time. **Rejected:** cloud SDKs (the dependency footprint
stays at a handful of modules) and a separately deployed front end.
→ [design/server.md](design/server.md), [design/providers.md](design/providers.md)

### Self-describing backends
**Decision:** each backend registers a `Spec` (fields, labels, `secret` flags,
optional `OAuthSpec`). The connect form is generated from
`/api/providers/specs`. **Why:** adding a backend needs no frontend change, and
the `secret` flag drives redaction everywhere. Edits merge a redacted
placeholder as "unchanged", so the browser never holds a real secret.
**Rejected:** a hand-built form per provider, and pasting refresh tokens (v2
shipped a browser OAuth sign-in instead).
→ [design/providers.md](design/providers.md), [design/cloud-backends.md](design/cloud-backends.md)

### Driving Proton's own client
**Decision:** the `proton` backend drives Proton's `proton-drive` CLI
(`upload`, `download`, `info`, `list`, `delete`, with `--json`), and the
session is kept encrypted in the vault, handed over as a 0600 file for one
command at a time. **Why:** Proton publishes no Go SDK, its SDK leaves out
authentication, and its cryptography changes at the end of 2026. Driving its
binary leaves that migration to Proton. A synced folder cannot tell a
signed-out client from a working one. **Rejected:** reimplementing the API,
and keeping the session in a keyring (none under systemd) or in `pass`.
→ [design/cloud-backends.md](design/cloud-backends.md), [design/providers.md](design/providers.md)

### SFTP over custom agents
**Decision:** a machine you have a login on (a VPS, a NAS, a storage box) is
reached over SFTP, both as a shard backend and as a source of your own files,
kept as two separate account entries. **Why:** it needs nothing installed on
the far end, so it reaches boxes where you only have a login. It added one
small module (`pkg/sftp`) and no CGO. Host keys are trust-on-first-use, or
pinned from a pasted fingerprint. **Rejected:** a daemon on the far end (MinIO
or Garage speaking S3, which still works with no code), and shelling out to
`ssh`/`sftp(1)`, which has no machine-readable output and, under systemd, no
`known_hosts`.
→ [design/sftp.md](design/sftp.md)

### WebDAV is opt-in, with a cached password check
**Decision:** `sand serve --webdav` exposes the main vault at `/dav` (and each
sub vault at `/sub/<name>/` under its own password), authenticated by the
vault password over HTTP Basic. A verified credential is remembered for a
minute under a per-process HMAC. **Why:** a player sends hundreds of requests
per film, and a 64 MB Argon2id pass on each is a self-inflicted DoS. **Rejected:**
on by default (the password crosses the wire on every request, so it needs
TLS), and a username that would pretend to be a second secret.
→ [design/server.md](design/server.md)

### Stream tickets for players that are not the browser
**Decision:** a session mints a 32-byte bearer token good for one file,
`/stream/{token}/{name}`, held in memory, slid forward on use, and voided on
lock. **Why:** VLC cannot carry a `SameSite=Strict`, `HttpOnly` cookie, and the
vault password does not belong in a URL. **Rejected:** persisting tickets,
which would outlive the unlocked vault that minted them.
→ [design/server.md](design/server.md)

### A TV gets one folder, through a four-character code
**Decision:** Sand TV pairs with a short code traded once for a 256-bit device
token, scoped to one folder (`internal/tvaccess`). Pairing is rate-limited (5
wrong codes per address or 30 overall in ten minutes). The app is a WebView
around the server's `/tv/` page plus a native Media3 player. **Why:** a remote
cannot type a password, and a TV should not hold one. Screens that live on the
server update with the server, not with an APK. **Rejected:** loading the main
app on the TV.
→ [design/sand-tv.md](design/sand-tv.md)

### Snapshots are a separate format with separate keys
**Decision:** a snapshot is tar → zstd → AES-GCM frames under Argon2id of its
own password, cut into self-describing parts. It is readable with no vault.
**Why:** the case it exists for is the vault being gone, so it shares none of
the vault's keys. **Rejected:** reusing vault parts and keys.
→ [design/cryptography.md](design/cryptography.md)

## Revisited: what v2 listed as "not built"

The v2 architecture document ended with a "Not Built" list. The code has moved
past most of it:

| Item | Status now |
|---|---|
| Configurable N-of-M via Reed–Solomon | **Built.** Cauchy Reed–Solomon over GF(2⁸), 2m-of-3m by default, any k-of-n with `--scheme` |
| A browser OAuth flow | **Built.** `OAuthSpec` and `/api/providers/oauth/*` |
| Repair of a missing part | **Built, as a rebuild** (see above) |
| HTTP uploads and reads are whole-file | **No longer true.** Uploads go through `UploadStreamAt`, and `/content` reads through `OpenReadSeeker` at an offset |
| Sync / conflict resolution | The index merges across machines. Sand Vault is still a store, not a file-sync engine |
| A FUSE mount | Not built. WebDAV covers file managers and players |
| Multi-user access | Not built. A vault has one owner by design |
