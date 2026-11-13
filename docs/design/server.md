# The HTTP server

How `sand serve` is put together (`internal/server`): what is public and what
needs a session, partial success, the error model, long jobs, the WebDAV share,
stream tickets, and standalone mode. Every endpoint, with its parameters, is
listed in [reference/api.md](../reference/api.md). Exposure and binding are
covered in [security.md](../security.md).

The server is the only component that ever holds plaintext. It decodes an
upload before scattering it and rebuilds a file to answer a download. The
embedded web UI (`internal/server/dist`, built from `web/`) is served from the
same origin.

## Who may call what

| Surface | Credential | Examples |
|---|---|---|
| Public | none | `GET /api/health`, `GET /api/vault` (only whether a vault exists and is unlocked), `POST /api/vault/init`, `POST /api/vault/unlock`, `GET /api/providers/specs`, kit inspect and import, standalone `POST /api/archive` and `/api/restore` |
| Session | `HttpOnly`, `SameSite=Strict` cookie, set at unlock | everything else under `/api/` |
| Bearer tickets | a token in the path | `/stream/{token}/{name}`, `/zip/{token}/{name}` |
| OAuth callback | the flow's 256-bit `state` | `GET /api/providers/oauth/callback` |
| TV | a device token obtained by pairing | `/api/tv/*`, and the TV screen at `/tv/` (see [sand-tv.md](sand-tv.md)) |
| WebDAV | the vault (or sub vault) password over HTTP Basic | `/dav/`, `/sub/<name>/` when `--webdav` is on |

Every state-changing request passes `withSameOriginGuard`: a non-GET with an
`Origin` header from another host is refused `403 CROSS_ORIGIN`. Together with
the `SameSite=Strict` cookie, this stops a page on another site from driving the
API through your browser.

**Sessions and the idle lock.** The session cookie has no `Max-Age`, so it lives
until the browser closes, and the server's session store is the one clock on how
long a session lasts. The vault re-locks after `--idle-timeout` (default 30
minutes) with no activity. Background polling (the health panel, a paused
checksum job) uses `requireSessionQuietly`, which checks the session without
renewing it, so a tab left open doesn't keep the vault unlocked forever. Work
nobody is watching (a mounted share, a stream going out, a detached import, an
automation sweep) counts as activity through `noteExternalActivity`. Locking
voids every session, stream and zip ticket, and stops background jobs, because
their keys are about to leave memory.

## Partial success is a first-class result

Dropping twelve files on the browser and having one fail is normal, so an upload
returns a result per file rather than failing wholesale:

```json
{
  "stored": 2,
  "results": [
    { "name": "a.pdf", "ok": true,  "file": { … } },
    { "name": "b.png", "ok": true,  "file": { … },
      "warnings": ["stored 2 of 3 parts — the file is recoverable but has no spare copy"] },
    { "name": "c.mov", "ok": false, "error": "stored only 1 of 3 parts…",
      "stage": "scattering", "parts": { "stored": 1, "needed": 2, "total": 3 },
      "failed_parts": [ … ], "advice": "…" }
  ]
}
```

- `name` is the file's path inside an uploaded folder, not its leaf, so four
  `cover.jpg` files produce four distinguishable lines.
- A failure says where it happened (`stage`: placing, reading, scattering or
  recording), how far the scatter got, which accounts refused a shard, and what
  to do (`vault.UploadError`).
- A folder that can't be created is a result of its own. The files under it then
  fail one by one, which shows how much of the tree arrived.
- The status is `201` if anything was stored and `502` if nothing was.
- Quota crossings and a thumbnail that wouldn't store are warnings, not failures.

Deletes follow the same rule: an unreachable account produces a warning, and the
index entry is dropped regardless, so a dead provider can't pin a file in the
browser forever. Its leftover shard becomes a stray for the orphan scan.

### A choice bigger than one request

A folder isn't one request. Sending everything picked as one multipart body
caused three hidden problems:

- **Nothing was stored until all of it arrived**, because the handler can't start
  until `ParseMultipartForm` finishes.
- **The body was spooled whole first.** `ParseMultipartForm` keeps 32 MiB in
  memory and the rest in a temp file, so the server needed room for the entire
  choice in `TMPDIR`. On a Raspberry Pi with a tmpfs, that meant an upload failing
  for want of space it never said it needed.
- **The tab made every thumbnail first.** Thumbnails are made in the browser,
  where the plaintext is, and a decoded photo is its pixels. A folder of phone
  photos decoded at once was gigabytes of bitmaps, which killed the tab.

So the client cuts a choice into batches, bounded in bytes and file count and
well inside the request ceiling, and sends them one after another
(`batchPicks`, `web/src/upload.js`). Each batch makes its own thumbnails just
before it leaves. The server is unchanged: a batch is an ordinary upload, and
the caller accumulates the per-file results. A failed batch is reported per file
and the rest still go. Empty folders in the tree (`dirs`) ride with the first
batch, because they must exist before anything under them.

Before sending, `POST /api/files/precheck` asks which files are already stored
at the same name and size, so the browser can skip them, and which of those
carry a different modification time, which `POST /api/files/retime` corrects
in the index.

A single request over `MaxUploadSize` (2 GiB) is answered `413 TOO_LARGE`, naming
the limit, rather than the `PARSE_ERROR` `http.MaxBytesReader` would produce. The
server reads the multipart temp file in place (`UploadStreamAt`) rather than
spooling it a second time.

## Error model

Every error is JSON with a human-readable message and a stable code the
frontend switches on:

```json
{ "error": "human-readable explanation", "code": "LOCKED" }
```

`vaultErrorResponse` maps vault errors to status and code in one place:

| Code | Status | Meaning |
|---|---|---|
| `LOCKED` | 401 | No session, or the vault is locked |
| `WRONG_PASSWORD` | 401 | The password didn't open the vault |
| `SUB_VAULT_LOCKED` | 401 | The vault is open but this sub vault isn't. The browser asks for one more password rather than returning to the lock screen |
| `NO_VAULT` | 404 | No vault has been created |
| `NOT_FOUND` | 404 | No such file, folder, account or sub vault |
| `NEEDS_CONVERSION` | 409 | The file is in the pre-chunking format. The browser offers to convert it |
| `REKEY_PENDING`, `MIGRATION_BUSY`, `AUTOMATION_BUSY`, `*_BUSY` | 409 | The vault's state, not the request, has to change first. One job of a kind runs at a time |
| `CROSS_ORIGIN` | 403 | A write from another origin |
| `TOO_LARGE` | 413 | One request over the upload ceiling |
| `BAD_REQUEST`, `BAD_PATH`, `BAD_SCHEME`, `PARSE_ERROR`, `MISSING_FILE` | 400 | The request itself is malformed |
| `VAULT_ERROR` | 400 | Any other vault refusal, with its message |

Feature-specific codes (film lookup, kits, snapshots, TV pairing, OAuth,
replication) are listed with their endpoints in
[reference/api.md](../reference/api.md).

## Long work runs beside the request

A page navigated away from cancels the request it was waiting on, which used to
mean minutes of work thrown away, or a repair that never happened. Long work
therefore runs on a context of its own, and the page polls for progress:

- **`jobWatch`** (`job_watch.go`) runs one job of a kind at a time and keeps
  what it found until the next run. It is used by the deep check and repair,
  snapshots, the stray-parts scan and others. A job is in memory only and is
  dropped on lock.
- **Watches for transfers** (`transfer_watch.go`, `relocate_watch.go`,
  `erase_runs.go`). An SFTP import or export, a relocation or a large delete may
  be *detached*: it answers `202` with a run to follow, keeps the vault awake
  while it moves bytes, can be stopped, and holds its result for 30 minutes. A
  restart loses them, and re-running is how they resume.

Requests that stay synchronous carry per-kind deadlines sized to the work
(an upload 6 hours, a relocation 6 hours, a health check 90 seconds).

## The WebDAV share

`sand serve --webdav` mounts the vault at `/dav` (`--webdav-path`) as a WebDAV
filesystem, so a file manager or player can open it as a drive. It is off by
default: it is a second way in, and it carries the password on every request.

`internal/davfs` is an adapter and nothing more. Where a file may be stored, how
it is split and what encrypts it are all decided in `internal/vault`.

| WebDAV | Vault |
|---|---|
| `GET` with `Range` | `OpenReadSeeker`, so a seek fetches only the chunks the range covers |
| `PUT` | `UploadStream`, piped in as it arrives, overwriting a file already at that path |
| `PROPFIND` | `List` |
| `MKCOL` | `Mkdir`, one level only: the parent must exist |
| `DELETE` | `Delete`, or `Rmdir` recursively for a folder |
| `MOVE` | `Move`, or `MoveFolder` for a collection. An index change; the parts don't travel |

**Overwrites never erase first.** The WebDAV library implements `Overwrite: T`
as "delete the destination, then move or copy". For the vault a delete erases
shards from every account with no undo, so if the move then failed or a copy's
body came up short, the replaced file would simply be gone. `davfs` moves the
old file aside under a temporary name instead (one index write) and only erases
it once the replacement has fully landed, putting it back otherwise. A `PUT`
whose body is shorter than its `Content-Length` fails rather than committing a
truncated file.

**Appending** stores the file again with the new bytes on the end, since the
vault stores whole files. The stored file is streamed back into the same upload,
so the cost is bandwidth, not memory. WebDAV has no append verb, so this is only
reachable through the filesystem interface. `PUT` always truncates.

**Authentication** is the vault password over HTTP Basic. The username isn't
checked: a vault has one owner, and a name would invite treating it as a second
secret.

- **Verified credentials are remembered for a minute** (`DefaultVerifyTTL`),
  keyed by an HMAC of the credential under a per-process key. Basic auth sends
  the password on every request and a playing film sends hundreds; verifying
  each one is a 64 MiB Argon2id pass, a denial of service aimed at yourself. The
  HMAC keeps the map from being a list of passwords, and the per-process key
  makes it useless anywhere else. A remembered credential still has to meet an
  unlocked vault, so locking takes access away.
- **A correct password against a locked vault unlocks it.** A mount outlives the
  idle timeout, and the alternative is a share that goes dead until someone
  opens a browser. It is the same surface `/api/vault/unlock` already presents.
  Requests to the share count as activity, so the auto-lock doesn't fire halfway
  through a film.
- **On a plain listener the password crosses the network in the clear on every
  request.** That is why the share is opt-in, and why the server warns at start.
  Put TLS in front of it (`scripts/nginx-sand.conf`, or Tailscale Serve).

**Sub vault shares.** Each sub vault has its own share at `/sub/<name>/`
(`--webdav-sub-path`), served by `davfs.SubVaultShares` whenever the main share
is on. The address names the sub vault by label (case-insensitive) or ID,
resolved on every request. Behind it is a filesystem built with
`NewScoped(v, Scope(id))`, which presents that sub vault and nothing else. No
request can ask a filesystem for a different scope, so `/dav` never shows a sub
vault, and a `MOVE` to another share fails the library's prefix check. The two
trees sit side by side rather than nested, because a sub vault called `Films`
under `/dav/` would shadow a folder of that name, and the server refuses to
start with one prefix inside the other.

- Its password is the sub vault's own, verified with the same minute-long HMAC
  memory. A correct password against a shut sub vault opens it.
- It can't unlock the main vault, whose password seals the accounts the sub
  vault's parts are on. While the vault is locked, the sub share answers `503`
  rather than `401`, so a client doesn't loop on password prompts.
- A name that is no sub vault's is answered exactly like a wrong password, so
  which sub vaults exist can't be read off status codes. `/sub/` itself lists
  nothing.
- One `webdav.Handler` is built per address, because the library writes its
  prefix into every `href`. All addresses of one sub vault share one
  `LockSystem`, so a file locked under the label is locked under the ID too. A
  cached endpoint remembers which sub vault it was built for, so two sub vaults
  renamed past each other are noticed and rebuilt.

User steps for mounting are in [guide/streaming.md](../guide/streaming.md).

## Stream tickets

Playing a film in VLC means giving VLC an address, and VLC has none of what
authenticates the app. The session cookie is `HttpOnly` and `SameSite=Strict`,
so the page can't read it and no other program carries it. The WebDAV share is
authenticated by the vault password, which doesn't belong in a URL or on a
clipboard.

So a **ticket**: 32 random bytes standing for one file, minted by a session that
has already unlocked the vault (`POST /api/files/{id}/stream`). It is a bearer
credential, so anyone holding the link can play that file. Every property
follows from that:

| | |
|---|---|
| Scope | One file. Not the folder, the index or the vault |
| Lifetime | `DefaultStreamTTL`, 12 hours, slid forward on each request, so a link in use never expires mid-film |
| Storage | Memory only. A link that outlived the process would outlive the unlocked vault it was minted from |
| Locking | Cleared on lock and auto-lock, so links are voided at once, not left to fail one request at a time |
| Reads | `OpenReadSeeker` behind `http.ServeContent`, so a seek costs only the chunks it covers. `HEAD` is answered too, because players ask for length and range support first |
| Keep-alive | Counts as activity while bytes are going out, noted about once a second. A paused player takes nothing and isn't activity, so a TV left on pause doesn't hold the vault open |
| Time limit | None on the request. A film is one request lasting the length of the film. The limit is on the vault going quiet: given nothing for `streamStallLimit` (30 minutes) at a stretch, the stream is given up. Waiting on the player doesn't count |
| TV codes | A link minted under a TV code is checked against the code as the film goes out, so revoking the code stops a film already playing |

The last three rows live in one place, `streamWriter`
(`internal/server/stream_serve.go`), because every byte of the body passes
through its `Write`: it is the one thing that knows whether the handler is
waiting on the player or has delivered something. Its deadline has to move, and
a context's can't, so it cancels with a cause wrapping
`context.DeadlineExceeded`. A local-folder account reads that cause to tell a
disk that has stopped answering from a caller that has stopped waiting. The
browser's own `/content` reads share the same time limit and nothing else,
because their session is held for as long as the request runs.

The path ends in the file's own name (`/stream/{token}/film.mkv`), because a
player picks its demuxer from the extension before a byte arrives. The token
decides which file is served, so renaming the last segment changes nothing. The
response matches what `/content` gives a browser: the stored type, `no-store`,
`nosniff`, a sandboxing CSP, and anything that could execute in this origin
forced to a download.

The address is assembled in the browser from the origin it reached the server
on, not from anything the server reports, because behind Tailscale Serve or a
reverse proxy the right host is one the server has never heard of. Reaching VLC
from the page then has three answers (`web/src/stream.js`):
`vlc-x-callback://` on iOS, an `intent:` URL naming `org.videolan.vlc` on
Android (a bare `vlc://` assumes http and would fetch an https share in the
clear), and a two-line `.m3u` on a desktop. Nothing reports whether a scheme was
handled, so the page watches its own `visibilitychange` and offers the address
when nothing happened.

**Folder zips** use the same ticket store with a different payload
(`POST /api/folders/zip`, then `GET /zip/{token}/{name}`), because a folder's zip
is too big to come back through a `fetch` the page buffers and has to be an
address the browser saves from directly.

## Standalone mode

The original workflow, kept because it needs no vault, no accounts and no state.
It is useful for a one-off, or for moving data to a machine that has never seen
SAND Vault.

```bash
sand archive report.pdf photos.zip --output-dir ./out
#  → sand-p1.zip  sand-p2.zip  sand-p3.zip      (put each somewhere different)

sand restore --parts a.p1.sand,a.p3.sand --output-dir .
#  → the original file
```

Each input is compressed, split 2-of-3 and sealed in the version 2 whole-file
format, under a key Argon2id derives from the password you type. Any two of the
three zips restore everything. `POST /api/archive` and `POST /api/restore` do the
same over HTTP, without a session.

`sand restore` also reads parts pulled from a vault's accounts: those are chunked
and keyed by the vault's data key, so it needs `--manifest` pointing at a
`manifest.sand` backup from one of the accounts, which the vault password opens.
See [guide/recovery.md](../guide/recovery.md).
