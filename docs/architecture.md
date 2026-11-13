# Architecture

This page is the map: what Sand Harbor and SAND Vault are, how the pieces fit,
the main flows in brief, every package and what it owns, the locking rules,
operational notes, and what is not built. Each section links to the design page
that covers it in depth.

## Sand Harbor, SAND Vault and SAND TV

**Sand Harbor** is the project as a whole. It has two components today:

- **SAND Vault**: a file browser over storage you do not fully trust. It is the
  `sand` binary: CLI, HTTP server, embedded web UI and WebDAV share.
- **SAND TV**: a Fire TV app (`apps/firetv`) that plays one vault folder of films.

You connect the cloud accounts you already have (Google Drive, an S3 bucket, a
WebDAV server, a folder on an external disk, and so on), and from then on SAND
Vault behaves like a file manager: folders, uploads, previews, downloads.
Underneath, every file is cut into chunks; each chunk is compressed,
erasure-coded into shards, and each shard is encrypted and sent to a
**different** account. With three accounts any two shards rebuild a chunk; with
more, the code widens (four-of-six, six-of-nine) while keeping one shard per
account. No single provider holds anything usable: a shard on its own is
indistinguishable from noise, and the key never leaves your machine.

What makes it a vault rather than a backup tool is the **index**: an encrypted
record of every file, folder and where each shard went. It lives in one file on
your machine (`vault.sand`), and a copy sealed under your password is kept on
every account so the vault can be recovered if the machine is lost.

## System shape

```
 ┌──────────────┐   ┌───────────────────────┐   ┌───────────────────────────┐
 │  sand CLI    │   │  Web UI (React SPA,   │   │  SAND TV (Fire TV app:    │
 │  cmd/sand    │   │  embedded, web/)      │   │  WebView on /tv/ + player)│
 └──────┬───────┘   └───────────┬───────────┘   └────────────┬──────────────┘
        │ in-process            │ HTTP /api, WebDAV /dav      │ HTTP /api/tv,
        │                       │                             │ /stream, UDP discovery
        │           ┌───────────▼─────────────────────────────▼──────────────┐
        │           │  internal/server: sessions, auto-lock, REST API,       │
        │           │  stream tickets, TV pairing, background loops          │
        │           │  internal/davfs: WebDAV over one vault scope           │
        │           └───────────┬────────────────────────────────────────────┘
        └───────────────────────┤
                    ┌───────────▼────────────────────────────────────────────┐
                    │  internal/vault: THE CONTROL PLANE                     │
                    │  encrypted index · keys · account configs · placement  │
                    │  scatter/gather · recovery · sync between machines     │
                    └───────────┬────────────────────────────────────────────┘
                    ┌───────────▼────────────────────────────────────────────┐
                    │  Pipeline: internal/archive                            │
                    │  compress (zstd) → erasure code (splitter) →           │
                    │  AES-256-GCM per shard (crypto) → part file (sandfile) │
                    └───────────┬────────────────────────────────────────────┘
                    ┌───────────▼────────────────────────────────────────────┐
                    │  internal/provider: put / get / delete / list,         │
                    │  one interface, ~20 backends                           │
                    └──┬─────────┬──────────┬──────────┬──────────┬──────────┘
                       ▼         ▼          ▼          ▼          ▼
                  Google Drive  S3/B2/R2  WebDAV    OneDrive   local folder,
                    shard 1     shard 2   shard 3   Dropbox…   iCloud, Proton…
```

Two layering rules keep this honest:

- **`internal/provider` never sees plaintext.** By the time a byte reaches a
  backend it has been compressed, coded and sealed. A provider handles opaque
  blobs and object keys.
- **The server is the only place plaintext exists**, and only transiently: it
  decodes an upload before scattering it and rebuilds what a download or stream
  asks for. Nothing readable is written to an account.

## Main flows

**Unlock.** The password derives the vault key (Argon2id), which opens the vault
file's sections: the data key, account configs, the index and settings. Locking
zeroes the keys and drops every cache. The server locks after every session has
been idle for 30 minutes. See [vault](design/vault.md).

**Store.** An upload is spooled beside the vault (the whole-file SHA-256 must be
known before the first chunk is sealed), then cut into chunks. Each chunk is
compressed, coded k-of-n, and each shard sealed under a key derived from the data
key, archive ID and chunk index, then written to its account. A bounded window of
chunks is in flight at once. The index entry is committed once enough shards
have landed to rebuild the file; a missing spare is recorded, not fatal. See
[data pipeline](design/data-pipeline.md), [placement](design/placement.md) and
[cryptography](design/cryptography.md).

**Open and stream.** A read fetches, for each chunk it needs, shards from every
account holding one and uses the first k to answer (the race routes around a slow
or dead account), then decrypts, decodes and decompresses. Seeking fetches only
the chunk it lands in, and decrypted chunks are cached. Players and TVs get
time-limited stream tickets (`/stream/{token}`) instead of the session. See
[data pipeline](design/data-pipeline.md),
[streaming performance](design/streaming-performance.md) and
[server](design/server.md).

**Back up the index.** Every index change schedules a paced push of
`manifest.sand` to every account. See
[vault: the manifest backup](design/vault.md#the-manifest-backup).

**Recover.** On a fresh install, reconnect accounts and the vault finds the
backup and rebuilds the index, or import a recovery kit that reconnects every
account itself. See [vault: recovery](design/vault.md#recovery) and
[recovery kit](design/recovery-kit.md).

**Several machines.** Machines opening one vault exchange index changes through
the same `manifest.sand` copies, merged with vector clocks. See
[vault: one vault on several machines](design/vault.md#one-vault-on-several-machines).

**Watch on a TV.** The owner mints a short pairing code for one folder; SAND TV
finds the server by UDP broadcast, trades the code for a device token scoped to
that folder, and plays through stream tickets. See [SAND TV](design/sand-tv.md).

**Standalone mode.** `sand archive` / `sand restore` (and `POST /api/archive`,
`POST /api/restore`) are the original batch tool: turn files into part files on
disk and back, with no vault or accounts. Kept because a part file plus the
right secret must always be readable without the app that wrote it. See
[data pipeline](design/data-pipeline.md).

## Package map

Verified against each package's doc comment.

| Package | Owns |
|---|---|
| `cmd/sand` | The cobra CLI: `serve`, `vault …` (init, passwd, rekey, migrate, backup, recover, reclaim, sweep, reattach, restore-parts, prune, kit, sync, instances…), `sub`, `remote` (accounts), file commands (`ls`, `find`, `put`, `get`, `rm`, `mkdir`, `mv`, `relocate`, `check`), `replicate`, `automation`, `git`, `checksums`, `snapshot`, `smart-scan`, and standalone `archive`/`restore`. See [CLI reference](reference/cli.md) |
| `internal/vault` | The control plane: the vault file and its keys, the encrypted index, account configs, placement, scatter/gather, readers and streams, relocation, replicas, password change and paced re-encryption, manifest backup, recovery, recovery kits, sub vaults, stray/lost-part scans, multi-machine sync, automation, git repositories, film details, search and the organizer reads |
| `internal/server` | HTTP: sessions and auto-lock, the REST API, embedded SPA (`dist/`) and TV page (`tvui/`, served at `/tv/`), stream tickets, OAuth flows, progress watchers for long jobs, and the background loops. Binds `0.0.0.0:8123` by default |
| `internal/davfs` | Presents one vault scope as a WebDAV filesystem (`/dav` for the main vault, `/sub/<name>/` per sub vault). An adapter over the vault's chunked reader and streaming upload |
| `internal/archive` | The pipeline in both directions: chunk planning, per-chunk encode (compress, code, seal), shard opening and assembly, remaking one lost shard, schemes, the legacy whole-file format, and standalone archive/restore |
| `internal/splitter` | The erasure coder: k data shards of n over GF(2^8), any k rebuild; plus the original split-and-XOR two-of-three kept for reading old parts |
| `internal/compress` | zstd, applied to every chunk before it is coded |
| `internal/crypto` | Argon2id (vault and recovery-kit parameters), HKDF-SHA256 chunk keys, AES-256-GCM |
| `internal/sandfile` | The on-disk format of one encrypted part: a clear header with what is needed to derive its key, everything else inside the ciphertext |
| `internal/provider` | The object-store interface (put, get, delete, list) and every backend: local folder, S3-compatible, WebDAV, SFTP, Google Drive, Dropbox, OneDrive, Box, Zoho WorkDrive, Drime, Filen, Internxt, iCloud Drive, Proton Drive, and synced-folder backends (MEGA, Jottacloud, Sync.com, Tresorit, Icedrive). Specs, OAuth, quotas, health pings, disk SMART. `s3test` is an in-memory versioned S3 for tests. See [providers](design/providers.md) and [adding a backend](design/cloud-backends.md) |
| `internal/sftp` | SSH/SFTP spoken natively (not via the system `ssh`), for SFTP accounts and for import/export sources; key generation and host keys. `sftptest` is an in-process sshd. See [SFTP](design/sftp.md) |
| `internal/git` | The small part of git needed: `ls-remote`, bare mirrors, bundles. Shells out to the machine's git for its credentials; URLs are allow-listed. See [index: repositories](design/index.md#repositories-kept-as-bundles) |
| `internal/movie` | File name to film guess, and the TMDb client. Reached only for folders opted in by name. See [index: film details](design/index.md#film-details) |
| `internal/assistant` | Sandy: a chat model in front of read-only tools over the index, speaking the OpenAI-compatible chat-completions protocol to a model server the user configures. See [Sandy](guide/sandy.md) |
| `internal/thumb` | Normalises browser-made preview images into re-encoded JPEGs of a known size |
| `internal/checksums` | The `checksums.gz` ledger format: one `path, sha256` line per file |
| `internal/filecheck` | Structural checks that a file is a well-formed JPEG, PDF, MP4 and so on, for the deep check. See [cryptography](design/cryptography.md) |
| `internal/snapshot` | A folder packed into one sealed, split archive under its own password, readable without the vault. See [cryptography](design/cryptography.md) |
| `internal/tuning` | Sizes chunk, window and cache knobs to the machine (Raspberry Pi to workstation), with operator overrides. See [data pipeline](design/data-pipeline.md) |
| `internal/tvaccess` | Pairing codes and device tokens that give a TV one folder for a time. See [SAND TV](design/sand-tv.md) |
| `internal/tvdiscover` | UDP discovery (port 8124) so a TV finds the server without typing an address |
| `internal/version` | The calendar version `vYEAR.MONTH.PATCH`, derived from git. See [developer guide](developer-guide.md) |
| `web/` | The React SPA (Vite), built into `internal/server/dist` and embedded in the binary. See [web UI guide](guide/web-ui.md) |
| `apps/firetv` | SAND TV: a Kotlin Android TV app, a WebView frame around the server's `/tv/` page plus a native player, discovery and playback stats. See [SAND TV](design/sand-tv.md) |

`tests/` holds the pytest end-to-end suites (CLI, API, vault flow, browser) and
`scripts/` the quickstart, deploy and release scripts; both are covered in the
[developer guide](developer-guide.md).

## Concurrency and locking

`Vault` is safe for concurrent use under one discipline:

- **`mu` (RWMutex)** guards the keys, the index and the account list.
- **No network I/O under `mu`.** An operation snapshots what it needs, releases
  the lock, does its I/O, then re-takes the lock to commit. Browsing stays
  responsive during a large upload. The commit re-checks the world: if the vault
  was locked or the scope closed meanwhile, the freshly written parts are erased
  and nothing is recorded. Archives being written but not yet in the index are
  tracked (`inflight.go`) so a stray-parts scan does not mistake them for orphans.
- **Leaf locks**, each taken after `mu` (or alone) and never around a call that
  takes `mu`: `liveMu` (constructed providers), `healthMu` (last ping results,
  written from ping paths that must never wait on the index), `thumbMu`, the chunk
  cache and per-chunk single-flight, `spoolMu` (temp files in flight), `keyUseMu`
  (data-key generations in use by transfers), `readsMu`, `pruneMu`, `syncMu`,
  `backupMu`, `rekeyMu`, `replicateMu`, `ledgerMu`. `thumbLoad` is the exception,
  taken before `mu`.
- **One-at-a-time slots**, taken with `TryLock` and never waited on: `migrateMu`
  (a re-encryption pass), `replicateSlot` (making a replica), and the automation
  runner (`autoMu`). The loser returns a busy error (`ErrMigrationBusy`,
  `ErrAutomationBusy`) or retries later.
- **Deferred work runs off the lock.** The manifest backup is *scheduled* while
  `mu` is held and *pushed* on its own goroutine once it is released.
- **Key holds.** Work that would be destroyed by losing the keys halfway takes a
  hold; a `Lock` arriving meanwhile waits for it.
- **Tuning** is an atomic pointer, so the settings page changes it under a
  running vault without taking `mu`.

The server's background loops (`autoLockLoop`, `automationLoop`, `healthLoop`,
`pruneLoop`, `checksumLedgerLoop`, `rekeyLoop`, `replicationLoop`, `syncLoop`,
and `discoveryLoop` on a bind a TV can reach) all do nothing while the vault is
locked.

## Operational notes

- **Binding.** `sand serve` binds all interfaces on port 8123 by default and
  warns on every non-loopback bind. The password and decrypted files cross the
  wire, so put TLS in front of it or use `--bind 127.0.0.1`. See
  [security](security.md) and [deployment](guide/deployment.md).
- **Health.** Connected accounts are pinged hourly while unlocked (5 minutes to
  7 days, `sand remote health --every`, `--off`), and a cloud that stops or starts
  answering is one log line. `sand check --all` stats every part of every file and
  exits non-zero if anything needs attention, which suits a cron job; `--deep`
  reads and verifies content, `--fix` repairs.
- **Disconnecting** an account is refused when it would leave any file
  unrecoverable, unless `--force`. Either way its shard records are pruned so the
  index keeps telling the truth; the objects are left on the account (see
  [vault: strays](design/vault.md#strays-reconciling-the-index-with-the-clouds)).
- **Memory.** Uploads and reads are bounded by the chunk window, not file size,
  including over HTTP. `GOMEMLIMIT` is set from the cgroup limit (systemd
  `MemoryMax`) or the machine's memory. Unattended repairs that rebuild a whole
  file are capped by `RebuildLimit`. Knobs are sized by `internal/tuning`; see
  [configuration](reference/configuration.md).
- **Shutdown.** An orderly stop flushes a pending manifest push for up to 30 s.
- **Idle lock vs. background work.** Automation, paced re-encryption and
  replication run only while unlocked. A running step holds the idle timer off;
  keeping the vault open overnight is an explicit choice (`--idle-timeout`, or the
  paced re-key's `keep_unlocked`).
- **Machine names.** Two machines sharing a vault under the same hostname confuse
  the merge; set `SAND_INSTANCE` on one of them.

## Not built

- **A FUSE mount.** WebDAV covers file managers and players; a media server like
  Jellyfin or Plex wants a real path. The chunked reader is the primitive it
  would sit on, and a library scan would need throttling against rate-limited
  APIs.
- **Multi-user access.** A vault is single-owner. The only delegated access is a
  TV's time-limited token for one folder.
- **File sync.** SAND Vault is a store, not a sync engine. Several machines share
  one index, but nothing keeps a folder on disk in step with a vault folder.
- **SAND Gallery and SAND Pod** are possible future Sand Harbor components; neither
  exists yet.

## Design pages

| Page | Covers |
|---|---|
| [design/vault.md](design/vault.md) | Vault file, keys, locking, manifest backup, recovery, strays, multi-machine, sub vaults, password change |
| [design/index.md](design/index.md) | Search, account names and settings, film details, folder pictures, organizer, short files, automation, git |
| [design/data-pipeline.md](design/data-pipeline.md) | Store, open and offset reads; erasure code; part format; sizing |
| [design/cryptography.md](design/cryptography.md) | KDF, chunk keys, AES-GCM, integrity, deep check, checksums, snapshots |
| [design/placement.md](design/placement.md) | Policies, schemes, account choice, object keys, re-placement, moves |
| [design/providers.md](design/providers.md) | Provider interface, backends, specs, OAuth, quota, health |
| [design/server.md](design/server.md) | Partial success, errors, WebDAV, stream tickets, standalone mode |
| [design/sand-tv.md](design/sand-tv.md) | Pairing, discovery, the TV player |
| [design/recovery-kit.md](design/recovery-kit.md) | The recovery kit |
| [design/paced-rekey.md](design/paced-rekey.md) | Pacing a password change |
| [design/sftp.md](design/sftp.md) | SFTP accounts and import/export sources |
| [design/streaming-performance.md](design/streaming-performance.md) | Streaming performance |
| [design/cloud-backends.md](design/cloud-backends.md) | Adding a backend |

The reasoning behind the big decisions is collected in
[design choices](design-choices.md); the threat model in [security](security.md).
