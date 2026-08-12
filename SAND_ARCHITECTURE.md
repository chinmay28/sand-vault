# SAND — Secure Archival Network Distribution

## Architecture Document v2.0

---

## 1. What SAND Is

SAND is a **file browser over storage you do not fully trust**.

You connect the cloud accounts you already have — a Google Drive, an S3 bucket,
a Nextcloud server, a folder on an external disk. From then on SAND behaves
like an ordinary file manager: folders, uploads, previews, downloads. What
happens underneath is not ordinary.

Every file you add is compressed, split into three parts, and encrypted. Each
part is pushed to a **different** account. Any two of the three rebuild the
original; **any one on its own is indistinguishable from noise**. When you open
a file, SAND fetches the parts back from wherever they live, reassembles them
in memory, and hands you the plaintext.

The point is that no single storage provider ever holds your data. They hold a
fragment that means nothing without a fragment held by someone else, plus a key
that never leaves your machine.

### 1.1 What changed from v1

v1 was a batch tool: feed it files, get three zip archives back, and carry them
to three places yourself. That mode still exists (§10), but it is no longer the
product.

| | v1 | v2 |
|---|---|---|
| Storage | You move zips by hand | SAND writes to connected accounts |
| Interface | Archive / Restore forms | A file browser with folders and previews |
| State | None — every run standalone | An encrypted index of what is stored where |
| Retrieval | Find the right zips, extract, restore | Click the file |
| Providers | — | Local disk, S3-compatible, WebDAV, Google Drive, Dropbox |
| Failure handling | Manual | Reads route around an account that is down |

---

## 2. System Shape

```
┌──────────────────────────────────────────────────────────────────────┐
│  Browser (React SPA, served from the binary)                         │
│  lock screen · account sidebar · file browser · preview              │
└───────────────────────────────┬──────────────────────────────────────┘
                                │ HTTP, loopback only
┌───────────────────────────────▼──────────────────────────────────────┐
│  internal/server — sessions, auto-lock, REST API, SPA hosting        │
├──────────────────────────────────────────────────────────────────────┤
│  internal/vault — THE CONTROL PLANE                                  │
│    · encrypted index (what exists, where each part went)             │
│    · connected-account credentials (encrypted at rest)               │
│    · placement policy, scatter/gather orchestration                  │
├───────────────────────────┬──────────────────────────────────────────┤
│  internal/archive         │  internal/provider                       │
│  compress → split → XOR   │  one tiny object-store interface,        │
│  → encrypt (and back)     │  five backends behind it                 │
└───────────────────────────┴──────────────┬───────────────────────────┘
                                           │
        ┌──────────────┬───────────────┬───┴──────────┬───────────────┐
        ▼              ▼               ▼              ▼               ▼
   Google Drive    S3 / R2 / B2     WebDAV        Dropbox      Local folder
      part 1          part 2         part 3           …              …
```

The layering rule that keeps this honest: **`internal/provider` never sees
plaintext.** By the time a byte reaches a backend it has already been
compressed, split, and sealed by `internal/archive`. A provider handles opaque
blobs and object keys, nothing else.

---

## 3. The Vault

The vault is a single JSON file (default `~/.sand/vault.sand`) in which
everything meaningful is encrypted. It is the only persistent state SAND has.

### 3.1 Layout

```json
{
  "version":   2,
  "kdf":       { "salt": "…", "time": 3, "memory": 65536, "threads": 4 },
  "check":     { "nonce": "…", "ciphertext": "…" },
  "data_key":  { "nonce": "…", "ciphertext": "…" },
  "providers": { "nonce": "…", "ciphertext": "…" },
  "manifest":  { "nonce": "…", "ciphertext": "…" },
  "policy":    "strict"
}
```

Only the KDF parameters and the policy are in the clear. Everything else is
sealed with AES-256-GCM under the **vault key**, derived from your password
with Argon2id.

| Section | Holds | Why it is encrypted |
|---|---|---|
| `check` | A fixed magic string | Opening it is what verifies the password |
| `data_key` | 32 random bytes | The key files are actually encrypted with |
| `providers` | Account configs **including credentials** | A stolen vault file must not yield cloud access |
| `manifest` | Filenames, folders, sizes, part placement | Filenames and folder structure are themselves sensitive |

### 3.2 Two keys, not one

File parts are **not** encrypted under your password. They are encrypted under
the random `data_key`, which is itself wrapped by the password-derived vault
key.

This buys two things:

- **Password changes are instant.** `sand vault passwd` re-wraps 32 bytes.
  Nothing is re-uploaded, because nothing a provider holds depended on your
  password.
- **Part encryption does not inherit a weak password.** The secret protecting
  file content is 256 bits of entropy regardless of what you typed.

### 3.3 Locking

Unlocking derives the vault key, decrypts every section, and holds the keys in
memory. Locking zeroes them and drops the decrypted index. A locked vault can
list nothing and fetch nothing — there is no cached view to fall back on.

The server re-locks automatically once every browser session has been idle past
the timeout (default 30 minutes, `--idle-timeout`).

### 3.4 Atomic writes

The vault file is rewritten in full on every change, via a temp file in the same
directory plus `fsync` and `rename`. An interrupted write cannot corrupt the
index that maps your files to their parts — the one piece of state whose loss
would strand everything.

---

## 4. The Data Pipeline

### 4.1 Storing a file

```
  bytes in
     │
     ├─► SHA-256 ─────────────────────────────► recorded for verification
     │
     ▼
 ┌────────┐   ┌───────────┐   ┌──────────┐   ┌─────────────┐
 │compress│──►│  split in │──►│   XOR    │──►│  encrypt    │
 │ (zstd) │   │  half     │   │  p1^p2   │   │ AES-256-GCM │
 └────────┘   └───────────┘   └──────────┘   └──────┬──────┘
                p1    p2          p3                │
                                                    ▼
                                        ┌───────────────────────┐
                                        │  placement policy     │
                                        │  decides which        │
                                        │  account gets which   │
                                        └───────────┬───────────┘
                                                    │ concurrent PUTs
                          ┌─────────────────────────┼─────────────────────┐
                          ▼                         ▼                     ▼
                     account A                 account B             account C
                  sand/<id>/p1.media       sand/<id>/p2.media    sand/<id>/p3.media
```

The three PUTs run in parallel. The upload commits if **at least two** parts
landed — that is the minimum that can still be rebuilt — and the entry is
flagged degraded if the third failed. If fewer than two succeed, the parts that
did land are deleted again rather than left as orphans on someone's account.

Only after storage succeeds is the entry added to the manifest and the vault
persisted. If that write fails, the parts are rolled back.

### 4.2 Opening a file

```
   click
     │
     ▼
 read placement from the manifest
     │
     ├──────────────┬──────────────┐   all three requested at once
     ▼              ▼              ▼
 account A      account B      account C
     │              │              ╳ offline
     └──────┬───────┘
            ▼  first two to arrive win; the rest are cancelled
   ┌─────────────┐   ┌──────────┐   ┌──────────┐   ┌────────┐
   │   decrypt   │──►│reconstruct│──►│decompress│──►│ verify │──► bytes out
   │ AES-256-GCM │   │   (XOR)   │   │  (zstd)  │   │SHA-256 │
   └─────────────┘   └──────────┘   └──────────┘   └────────┘
```

Reads are a **race, not a sequence**. All parts are requested simultaneously and
the fetch completes as soon as two have arrived, so a slow or unreachable
account costs nothing on the read path — it simply loses the race.

### 4.3 Reconstruction truth table

| p1 | p2 | p3 | Method |
|:--:|:--:|:--:|:---|
| ✓ | ✓ | — | concat(p1, p2) |
| ✓ | — | ✓ | p2 = p1 ⊕ p3, then concat |
| — | ✓ | ✓ | p1 = p2 ⊕ p3, then concat |
| ✓ | ✓ | ✓ | concat(p1, p2) — p3 unused |
| at most one | | | unrecoverable |

---

## 5. Placement Policy

Where the parts go is a **security decision**, because any two parts plus the
key rebuild the file. Two parts on one account means that account could, in
principle, rebuild it.

### 5.1 `strict` (default)

One part per account, never two on the same one.

- 3+ accounts → all three parts placed, each somewhere different. Full
  redundancy, and no single provider can reconstruct anything.
- 2 accounts → only parts 1 and 2 are stored. The file is recoverable and
  confidential, but there is no spare part.
- 1 account → **refused**, with an error explaining why.

### 5.2 `redundant`

Always store all three parts, doubling up when there are fewer than three
accounts.

Survives an account going dark even with one or two connected — at the cost of
the guarantee above, since a doubled-up account holds enough to rebuild. The
UI says so plainly when you pick it.

### 5.3 Spreading load

The starting account rotates per file, seeded from the file's random archive ID.
Without this every part 1 would pile onto the first-connected account.

### 5.4 Object keys leak nothing

```
sand/<128-bit random archive id>/p<N>.media
```

Derived only from a random ID. Someone with full access to one account learns
how many objects you store and how big each part is — nothing about names,
types, or folder structure.

---

## 6. Cryptography

Unchanged from v1, and deliberately so.

### 6.1 Key derivation — Argon2id

| Parameter | Value |
|---|---|
| Time | 3 iterations |
| Memory | 64 MB |
| Threads | 4 |
| Salt | 16 random bytes, unique per file and per vault |
| Output | 32 bytes (AES-256) |

### 6.2 Encryption — AES-256-GCM

- 12-byte random nonce, unique per part
- The serialized media header is passed as **associated data**, binding each
  ciphertext to its own part number and archive ID — a part cannot be swapped
  for another file's part without the tag failing
- 16-byte authentication tag

### 6.3 Integrity

| Layer | Mechanism | Catches |
|---|---|---|
| Per part | GCM tag | Tampering, truncation, bit rot |
| Per part | Header as associated data | Part swapping, metadata edits |
| Whole file | SHA-256 recorded at upload, checked after rebuild | Any corruption that slipped through |
| Vault | GCM tag on every section | A modified or truncated index |

---

## 7. Media File Format

Each stored object is a self-describing `.media` blob. Unchanged from v1, which
means **parts written by v2 can still be restored by the standalone `sand
restore` command** given the right secret.

```
Offset  Size   Field
──────────────────────────────────────────────────────────
0x00    4      Magic "SAND"
0x04    1      Version
0x05    1      PartNumber (1..3)
0x06    16     ArchiveID
0x16    32     OriginalHash (SHA-256)
0x36    8      OriginalSize
0x3E    8      CompressedSize
0x46    1      WasPadded
0x47    2      FilenameLength
0x49    var    Filename
var     16     Argon2id salt
var     4      Argon2 time
var     4      Argon2 memory (KB)
var     1      Argon2 threads
var     12     AES-GCM nonce
var     4      PayloadSize
var     N      Ciphertext + 16-byte tag
```

Every part carries the full metadata, so any two are self-sufficient.

---

## 8. Provider Layer

### 8.1 The interface

```go
type Provider interface {
    Config() Config
    Put(ctx, key string, data []byte) error
    Get(ctx, key string) ([]byte, error)
    Stat(ctx, key string) (ObjectInfo, error)   // health checks, no download
    Delete(ctx, key string) error
    List(ctx, prefix string) ([]ObjectInfo, error)
    Ping(ctx) error                              // credentials + reachability
}
```

Optional: `UsageReporter` for backends that can report quota.

### 8.2 Backends

| Kind | Covers | Transport |
|---|---|---|
| `local` | Any directory: external disk, NAS mount, sync folder | Filesystem, atomic writes |
| `s3` | Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO | SigV4, hand-rolled on the stdlib |
| `webdav` | Nextcloud, ownCloud, Box, Koofr, Fastmail | PUT/GET/HEAD/DELETE/MKCOL/PROPFIND |
| `gdrive` | Google Drive | Drive v3 REST, OAuth refresh grant |
| `dropbox` | Dropbox | API v2, OAuth refresh grant |

Everything is built on `net/http` and the standard library. No cloud SDKs — the
dependency footprint stays at five modules and `CGO_ENABLED=0` still yields a
fully static binary.

### 8.3 Self-describing configuration

Each backend registers a `Spec` listing the fields it needs, with labels, help
text and a `secret` flag:

```go
Register(Spec{
    Kind:  KindS3,
    Label: "S3-compatible storage",
    Fields: []FieldSpec{
        {Key: "bucket", Label: "Bucket", Required: true},
        {Key: "secret_access_key", Label: "Secret access key", Secret: true, Required: true},
        …
    },
}, newS3Provider)
```

`GET /api/providers/specs` serves these, and the web UI generates its connect
form from them. **Adding a backend requires no frontend change**, and the
`secret` flag is what drives redaction everywhere a config is returned.

### 8.4 Google Drive has no paths

Drive is not a path-addressed store, so the SAND object key is written into the
file's `appProperties` and looked up by query, with a per-provider ID cache so
repeated reads skip the lookup.

---

## 9. HTTP API

Everything under `/api/files`, `/api/folders`, `/api/providers` and the
vault-mutating endpoints requires a session. `GET /api/vault` is public but
reveals only whether a vault exists.

### 9.1 Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness |
| GET | `/api/vault` | Initialized? Unlocked? Stats |
| POST | `/api/vault/init` | Create the vault, start a session |
| POST | `/api/vault/unlock` | Unlock, start a session |
| POST | `/api/vault/lock` | Zero the keys, end every session |
| POST | `/api/vault/password` | Re-wrap under a new password |
| POST | `/api/vault/policy` | Change placement policy |
| GET | `/api/providers/specs` | Backend descriptions for the connect form |
| GET | `/api/providers` | Connected accounts: online, parts held, quota |
| POST | `/api/providers` | Connect an account (pings before saving) |
| POST | `/api/providers/{id}/test` | Re-check one account |
| DELETE | `/api/providers/{id}` | Disconnect (`?force=1` to override the guard) |
| GET | `/api/files?path=` | List a folder |
| POST | `/api/files` | Upload (multipart `files[]`, `path`, `overwrite`) |
| GET | `/api/files/{id}` | Metadata including part placement |
| GET | `/api/files/{id}/content` | **Rebuild and stream** (`?download=1` to save) |
| GET | `/api/files/{id}/health` | Per-part reachability, without downloading |
| POST | `/api/files/{id}/move` | Rename or move (index only) |
| DELETE | `/api/files/{id}` | Erase every part, drop the entry |
| POST | `/api/folders` | Create a folder |
| DELETE | `/api/folders?path=&recursive=` | Delete a folder |
| POST | `/api/archive` | Standalone mode (§10) |
| POST | `/api/restore` | Standalone mode (§10) |

### 9.2 Partial success is a first-class result

Upload returns a per-file result rather than failing wholesale, because
dropping twelve files onto the browser and having one fail is normal:

```json
{
  "stored": 2,
  "results": [
    { "name": "a.pdf", "ok": true,  "file": { … } },
    { "name": "b.png", "ok": true,  "file": { … },
      "warnings": ["stored 2 of 3 parts — the file is recoverable but has no spare copy"] },
    { "name": "c.mov", "ok": false, "error": "stored only 1 of 3 parts…" }
  ]
}
```

Deletes behave the same way: an unreachable account produces a warning, and the
index entry is dropped regardless, so a dead provider cannot pin a file in the
browser forever.

### 9.3 Error codes

```json
{ "error": "human-readable explanation", "code": "LOCKED" }
```

`LOCKED · WRONG_PASSWORD · NO_VAULT · NOT_FOUND · CROSS_ORIGIN · VAULT_ERROR ·
PARSE_ERROR · MISSING_FILE`

---

## 10. Standalone Mode

The v1 workflow, kept because it needs no vault, no accounts and no state —
useful for a one-off and for moving data onto a machine that has never seen
SAND before.

```
sand archive report.pdf photos.zip --output-dir ./out
  → media1.zip  media2.zip  media3.zip     (put each somewhere different)

sand restore --parts a.media1,a.media3 --output-dir .
  → the original file
```

The same endpoints back the API (`POST /api/archive`, `POST /api/restore`).

---

## 11. Security

### 11.1 Threat model

| Threat | Mitigation |
|---|---|
| **One cloud account compromised** | Attacker holds one part: ciphertext derived from half the compressed bytes. Useless. Under `strict` this is guaranteed by placement. |
| **Two accounts compromised** | Attacker holds enough parts but not the key. Still needs the vault file *and* the password. |
| **Vault file stolen** | Every section is AES-256-GCM sealed under an Argon2id key. Yields neither cloud credentials nor filenames. |
| **Provider tampers with a part** | GCM tag fails; the other two parts still rebuild the file |
| **Part swapping between files** | Header bound as associated data |
| **Silent bit rot** | Whole-file SHA-256 verified on every rebuild |
| **Account goes away** | Any two parts suffice; `sand check --all` finds damage before you need the file |
| **Brute force** | Argon2id, 64 MB, 3 passes |
| **Another site in your browser** | `SameSite=Strict` cookie plus an `Origin` check on every write |
| **Stored HTML/SVG executing in the app** | Risky types forced to `attachment`, `X-Content-Type-Options: nosniff`, restrictive CSP |
| **Plaintext cached by a proxy** | `Cache-Control: private, no-store` on rebuilt content |
| **Walking away from the machine** | Idle timeout re-locks the vault |
| **Metadata leaking to a provider** | Object keys derived only from a random ID |
| **Third-party tracking** | The UI loads no external fonts, scripts or styles — opening the vault makes zero third-party requests |

### 11.2 What SAND does not protect against

- **A compromised endpoint.** The machine running SAND sees plaintext and holds
  the keys while unlocked. A keylogger or malware on it defeats everything here.
- **Weak passwords.** Argon2id slows a guess; it does not fix `hunter2`.
- **Two providers *and* your password.** That is the documented recovery path;
  it is also the attack. SAND's model assumes the accounts are independent —
  don't use two buckets in the same AWS account and call it distribution.
- **Losing the password.** There is no recovery. The index cannot be decrypted,
  and the parts scattered across your accounts stay meaningless.
- **Traffic analysis.** A provider sees when you upload and how large each part
  is.

### 11.3 Binding to a non-loopback address

`--bind` accepts anything, but off loopback the password and rebuilt plaintext
cross the network unencrypted. The server logs a warning; put TLS in front of it
(`scripts/nginx-sand.conf`).

---

## 12. Project Layout

```
sand/
├── cmd/sand/                  # CLI: serve, vault, remote, ls/put/get/rm, archive/restore
├── internal/
│   ├── archive/               # encode.go — the in-memory pipeline both modes use
│   ├── compress/              # zstd
│   ├── crypto/                # Argon2id + AES-256-GCM
│   ├── splitter/              # split, XOR, reconstruct
│   ├── mediafile/             # binary part format
│   ├── provider/              # provider.go, local, s3, webdav, gdrive, dropbox
│   ├── vault/                 # store (encrypted file), manifest, placement, transfer
│   └── server/                # sessions, handlers, embedded SPA
├── web/src/                   # React file browser
│   ├── api.js  theme.js  App.jsx
│   └── components/            # LockScreen, AccountsPanel, FileBrowser, PreviewModal, ui
└── tests/                     # pytest e2e: CLI, API, vault flow, browser
```

---

## 13. Concurrency and Locking

`Vault` is safe for concurrent use, with a deliberate discipline:

- `mu` (RWMutex) guards the keys, the manifest and the account list.
- **Network calls never happen while `mu` is held.** Each operation snapshots
  what it needs, releases the lock, does its I/O, then re-takes the lock to
  commit. Browsing stays responsive while a large upload is in flight.
- `liveMu` is a separate leaf lock over the cache of constructed providers, so
  warming that cache can never deadlock against `mu`.
- Re-checks after re-acquiring: the vault may have been locked mid-transfer, in
  which case the freshly written parts are rolled back.

---

## 14. Operational Notes

- `sand check --all` stats every part of every file and exits non-zero if
  anything is degraded or unrecoverable — suitable for a cron job.
- Disconnecting an account is refused when it would leave any file with fewer
  than two reachable parts, unless forced. Either way the shard records
  pointing at that account are pruned so the index keeps telling the truth.
- A file is held entirely in memory during upload and rebuild. Streaming and
  chunked processing for files larger than RAM is the main thing still on the
  list (§15).

---

## 15. Not Built

- **Streaming / chunked processing** for files larger than available RAM
- **Repair** — re-uploading a missing part from the two that survive, rather
  than re-uploading the whole file
- **Configurable N-of-M** via Reed–Solomon instead of fixed 2-of-3
- **A browser OAuth flow** — Drive and Dropbox currently take a refresh token
  you obtain yourself
- **Multi-user access**; the vault is single-owner by design
- **Sync / conflict resolution**; SAND is a store, not a sync engine
