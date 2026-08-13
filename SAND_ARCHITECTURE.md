# SAND Vault — Secure Archival Network Distribution

## Architecture Document v2.0

---

## 1. What SAND Vault Is

SAND Vault is a **file browser over storage you do not fully trust**.

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
| Providers | — | Google Drive, OneDrive, Dropbox, Box, S3-compatible, WebDAV, Proton Drive, local disk |
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
│  → encrypt (and back)     │  eight backends behind it                │
└───────────────────────────┴──────────────┬───────────────────────────┘
                                           │
        ┌──────────────┬───────────────┬───┴──────────┬───────────────┐
        ▼              ▼               ▼              ▼               ▼
   Google Drive    S3 / R2 / B2     WebDAV      OneDrive / Box   Local folder
      part 1          part 2         part 3           …          Proton Drive
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

### 3.5 The manifest backup

Losing the vault file used to be unrecoverable, and not by a small margin: parts
are encrypted under the random `data_key`, which existed in exactly one place.
Password in hand, every part in every account was permanently opaque.

So a copy of the index travels with the data. Each connected account receives
`manifest.sand`, rewritten whenever the index changes:

```json
{
  "magic":   "SAND-MANIFEST",
  "version": 1,
  "kdf":     { "salt": "…", "time": 3, "memory": 65536, "threads": 4 },
  "check":   { "nonce": "…", "ciphertext": "…" },
  "payload": { "nonce": "…", "ciphertext": "…" }
}
```

It carries its **own** KDF parameters rather than referencing the vault's,
because the vault is precisely what the reader has lost. A password alone
derives the key and opens the payload:

| Field | Holds |
|---|---|
| `data_key` | The 32 random bytes every stored part is encrypted under |
| `manifest` | The file tree, and which account holds which part under which key |
| `accounts` | Account id, kind, name, and when it was connected — **never credentials** |
| `policy` | The placement policy in force |

Credentials are excluded deliberately. A copy of this file sits in every
account, so including them would make one compromised account a master key to
all the others.

**What it costs.** Every copy is a password away from the data key, so a single
compromised account plus a cracked password yields the tree, the placement map,
and — since each part is separately encrypted under that key — whatever the
breached account's own parts contain, which for a large file is about half of
it. Rebuilding a whole file still needs a second account, so the two-of-three
split remains a real second factor. Argon2id at 64 MB is what stands between the
envelope and a guessed password.

**The one configuration where that breaks down** is the redundant policy with
fewer than three accounts, where a single account can already hold enough parts
to rebuild a file on its own. Adding the data key there would leave the password
as the only protection, so the backup refuses to write, and erases any copies it
had already written.

The write is guarded in one more way: an account already holding a backup this
vault cannot open is left alone, because that is a *different* vault's recovery
data and connecting an account to a second vault must not destroy it.

### 3.6 Recovery

Three routes back, in increasing order of how much has survived:

| You have | Command | You get |
|---|---|---|
| `manifest.sand` + password | `sand manifest ls` | The file tree and the placement map |
| Enough parts + `manifest.sand` + password | `sand restore --manifest` | The complete file, offline, no accounts |
| The accounts + password | `sand vault recover` | The whole vault, files openable again |

`sand vault recover` runs against a fresh vault with the accounts reconnected.
Reconnecting gives every account a new internal id, so rather than trusting the
remembered ids it asks each account what it actually holds and re-points every
shard record at whichever account answers with that key. Accounts that were not
reconnected show up as unreachable parts. The recovered vault adopts the old
data key, so new uploads join the existing files instead of starting a second
key, and it rewrites the backups under its own password.

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
                  <id>-p1.sand             <id>-p2.sand          <id>-p3.sand
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
<128-bit random archive id>-p<N>.sand
```

Derived only from a random ID. Someone with full access to one account learns
how many objects you store and how big each part is — nothing about names,
types, or folder structure.

Until format version 2 that last sentence was not true: the part header carried
the original filename, its plaintext SHA-256 and its size in the clear, so a
single account could read the name of every file it held a part of. Version 2
moved all of it inside the ciphertext (§7). Version 1 parts are still readable,
and still say what they always said — re-upload anything whose name matters.

The key is a flat filename with no directory components. Every backend already
scopes SAND to somewhere of its own — a folder on Dropbox, Box and OneDrive, a
prefix on S3 and WebDAV, the chosen directory for a local or sync folder — so
nesting a further `sand/<id>/` inside that only buried each part two levels
deeper for no gain. Staying flat also makes Google Drive, which has no paths
and stores each part as a plain file, land on exactly the same part names as
everywhere else.

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
- The cleartext part header is passed as **associated data**, binding each
  ciphertext to its own part number and archive ID — a part cannot be swapped
  for another file's part, or re-labelled as a different part number, without
  the tag failing
- 16-byte authentication tag

### 6.3 Integrity

| Layer | Mechanism | Catches |
|---|---|---|
| Per part | GCM tag | Tampering, truncation, bit rot |
| Per part | Cleartext header as associated data | Part swapping, re-labelling |
| Per part | Metadata sealed with the data | Edits to the name, hash or sizes |
| Whole file | SHA-256 recorded at upload, checked after rebuild | Any corruption that slipped through |
| Vault | GCM tag on every section | A modified or truncated index |

---

## 7. Part File Format

Each stored object is a self-describing `.sand` blob: everything needed to
derive its key sits in the clear at the front, and everything that would
describe the file it came from sits inside the ciphertext.

```
Offset  Size   Field                          Cleartext header, also GCM
──────────────────────────────────────────────  associated data
0x00    4      Magic "SAND"
0x04    1      Version (2)
0x05    1      PartNumber (1..3)
0x06    16     ArchiveID
0x16    16     Argon2id salt
0x26    4      Argon2 time
0x2A    4      Argon2 memory (KB)
0x2E    1      Argon2 threads
0x2F    12     AES-GCM nonce
──────────────────────────────────────────────
0x3B    4      PayloadSize
0x3F    N      Ciphertext + 16-byte tag
                 └─ 32   OriginalHash (SHA-256)   sealed metadata
                    8    OriginalSize
                    8    CompressedSize
                    1    WasPadded
                    2    FilenameLength
                    var  Filename
                    var  this part's share of the compressed stream
```

Every part carries the full metadata, so any two are self-sufficient — and a
part on its own tells an observer only that it is a SAND part, which archive it
belongs to, and how big it is.

**Version 1**, whose header held the metadata in the clear, is still read: parts
written by older builds restore unchanged. Nothing writes version 1 any more, so
the standalone `sand restore` from a v1 binary can no longer open parts written
today — restore with a current build instead.

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

Optional, and each one is a capability the layers above check for rather than
assume: `UsageReporter` for backends that can report quota, `Identifier` for
backends that can name the account they are pointed at, and
`CredentialRotator` for backends whose stored credentials change as they are
used.

### 8.2 Backends

| Kind | Covers | Transport |
|---|---|---|
| `local` | Any directory: external disk, NAS mount, sync folder | Filesystem, atomic writes |
| `s3` | Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO | SigV4, hand-rolled on the stdlib |
| `webdav` | Nextcloud, ownCloud, pCloud, Koofr, Fastmail, rclone | PUT/GET/HEAD/DELETE/MKCOL/PROPFIND |
| `gdrive` | Google Drive | Drive v3 REST, OAuth refresh grant |
| `onedrive` | OneDrive, personal or work | Microsoft Graph, chunked upload sessions |
| `dropbox` | Dropbox | API v2, OAuth refresh grant |
| `box` | Box | API 2.0, OAuth with rotating refresh tokens |
| `proton` | Proton Drive, via the folder its desktop app syncs | Filesystem, atomic writes |

Everything is built on `net/http` and the standard library. No cloud SDKs — the
dependency footprint stays at five modules and `CGO_ENABLED=0` still yields a
fully static binary.

Proton Drive is the odd one out: it publishes no API, so the backend is the
local one pointed at the folder Proton's desktop client syncs. That is not a
compromise of the threat model — parts are encrypted long before the sync
client sees them — but it does mean the account is only as live as the machine
running the client.

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

### 8.4 Signing in instead of pasting tokens

A backend that speaks OAuth adds an `OAuthSpec` to its registration: the
authorize and token endpoints, the scopes, whether to use PKCE, and which
option keys the exchanged tokens are written to. That is the whole of what the
server needs to run a sign-in, so a new OAuth backend costs one struct literal.

```
browser                    server                        provider
   │  POST oauth/start        │                              │
   │─────────────────────────►│  mint state + PKCE verifier  │
   │  ◄── auth_url, flow_id ──│  hold them in memory (15m)   │
   │                          │                              │
   │  window.open(auth_url) ─────────────────────────────────►│
   │                          │  GET oauth/callback?code&state (no cookie)
   │                          │◄─────────────────────────────│
   │                          │  exchange code ─────────────►│
   │                          │  ask the account its name ──►│
   │  ◄── poll oauth/{flow} ──│  tokens held against the flow│
   │  POST oauth/complete ───►│  AddProvider → encrypted vault
```

The redirect arrives as a cross-site navigation, which the `SameSite=Strict`
session cookie deliberately does not survive. So the callback authenticates on
the 256-bit `state` alone, and the calls that turn a finished flow into an
account — status and complete — are the ones bound to the session that started
it. A state is retired as soon as it is spent, so a replayed redirect exchanges
nothing.

Tokens never reach the browser. The flow's status reports only how far along it
is and which account signed in; the credentials go from the token endpoint into
the vault without passing through the page.

`CredentialRotator` closes the loop for Box and Microsoft, which retire a
refresh token as it is spent: the vault installs a sink on every live provider
it builds, and a rotated token is written back — asynchronously, because the
refresh may well be happening inside a call that already holds the vault lock.

### 8.5 Google Drive has no paths

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
| POST | `/api/providers/oauth/start` | Begin a sign-in; returns the consent URL |
| GET | `/api/providers/oauth/callback` | Where the provider sends the browser back (public; matched on `state`) |
| GET | `/api/providers/oauth/{id}` | How far along a sign-in is |
| POST | `/api/providers/oauth/exchange` | Finish a sign-in from a pasted redirect URL |
| POST | `/api/providers/oauth/complete` | Turn a finished sign-in into an account |
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
  → sand-p1.zip  sand-p2.zip  sand-p3.zip     (put each somewhere different)

sand restore --parts a.p1.sand,a.p3.sand --output-dir .
  → the original file
```

The same endpoints back the API (`POST /api/archive`, `POST /api/restore`).

---

## 11. Security

### 11.1 Threat model

| Threat | Mitigation |
|---|---|
| **One cloud account compromised** | Attacker holds one part: ciphertext derived from half the compressed bytes, plus a `manifest.sand` they cannot open. Under `strict` one part per file is guaranteed by placement. |
| **One account compromised *and* the password guessed** | The manifest opens: tree, placement map, data key, and roughly half of each large file that account holds a part of. A whole file still needs a second account. See §3.5. |
| **Two accounts compromised** | Attacker holds enough parts but not the key — which needs the vault file, or a manifest backup *and* the password. |
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
| **Metadata leaking to a provider** | Object keys derived only from a random ID; filenames, hashes and sizes sealed inside the part since format v2 |
| **Third-party tracking** | The UI loads no external fonts, scripts or styles — opening the vault makes zero third-party requests |

### 11.2 What SAND does not protect against

- **A compromised endpoint.** The machine running SAND sees plaintext and holds
  the keys while unlocked. A keylogger or malware on it defeats everything here.
- **Weak passwords.** Argon2id slows a guess; it does not fix `hunter2`.
- **Two providers *and* your password.** That is the documented recovery path;
  it is also the attack. SAND's model assumes the accounts are independent —
  don't use two buckets in the same AWS account and call it distribution.
- **Losing the password.** There is no recovery. Neither the vault nor any
  manifest backup can be decrypted, and the parts scattered across your accounts
  stay meaningless.
- **A manifest backup plus a guessed password.** Replicating the index is what
  makes a lost vault survivable, and it is also an attack surface that did not
  exist before. `sand vault backup --disable` erases every copy and takes the
  vault back to "the vault file is the only way in" — along with everything that
  implies if you lose it.
- **Traffic analysis.** A provider sees when you upload and how large each part
  is.

### 11.3 Binding

`--bind` defaults to `0.0.0.0`, so an install is reachable from the network
without a second step. That is a deliberate usability choice with a real cost:
off loopback the vault password and every rebuilt file cross the network
unencrypted, and `/api/vault/unlock` is reachable unauthenticated with no rate
limiting — only Argon2id's cost stands between an attacker on your network and
a guessing loop. The server logs a warning on every non-loopback bind. Put TLS
in front of it (`scripts/nginx-sand.conf`, or Tailscale Serve), or set
`--bind 127.0.0.1` to close it.

---

## 12. Project Layout

```
sand/
├── cmd/sand/                  # CLI: serve, vault, remote, ls/put/get/rm, archive/restore,
│                              #   manifest ls, vault backup/recover
├── internal/
│   ├── archive/               # encode.go — the in-memory pipeline both modes use
│   ├── compress/              # zstd
│   ├── crypto/                # Argon2id + AES-256-GCM
│   ├── splitter/              # split, XOR, reconstruct
│   ├── sandfile/              # binary .sand part format
│   ├── provider/              # provider.go, local, s3, webdav, gdrive, dropbox
│   ├── vault/                 # store (encrypted file), manifest, placement, transfer, backup
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
