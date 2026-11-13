# HTTP API reference

Every route `sand serve` registers, taken from the route table in
`internal/server/server.go` (`Server.Handler`) and the handlers behind it. The
web UI in `web/src` and Sand TV are both clients of this API; nothing here is
private to them, but it is not a versioned public API either, and it changes
with the binary.

Design and rationale (partial success, the error model, tickets, WebDAV,
standalone mode): [server design](../design/server.md). Sand TV's side:
[Sand TV design](../design/sand-tv.md). Exposure and TLS: [security](../security.md).

**Contents**

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Errors](#errors)
- [Long-running work](#long-running-work)
- [Summary](#summary)
- Areas: [vault](#vault) · [password change and re-key](#password-change-and-re-key) ·
  [recovery](#recovery) · [recovery kit](#recovery-kit) · [housekeeping](#housekeeping) ·
  [accounts](#accounts) · [sign-in flows](#sign-in-flows) · [sub vaults](#sub-vaults) ·
  [files](#files) · [reading and streaming](#reading-and-streaming) · [folders](#folders) ·
  [moving and deleting](#moving-and-deleting) · [checks and checksums](#checks-and-checksums) ·
  [snapshots](#snapshots) · [replication](#replication) · [automation](#automation) ·
  [repositories](#repositories) · [film details](#film-details) · [assistant](#assistant) ·
  [machines (SFTP sources)](#machines-sftp-sources) · [statistics](#statistics) ·
  [this machine](#this-machine) · [Sand TV](#sand-tv) · [WebDAV](#webdav) ·
  [standalone mode](#standalone-mode) · [frontend](#frontend)

## Conventions

- Default address `http://<host>:8123`. All paths below are absolute.
- Request bodies are JSON unless marked *multipart*. A JSON body is capped at
  1 MiB and **unknown fields are rejected** (`400 BAD_REQUEST`).
- Responses are JSON (`Content-Type: application/json`) unless the endpoint
  serves a file, an image, a zip or an HTML page.
- Times are RFC 3339 strings. Sizes are bytes. Field names are `snake_case`; a
  field marked `?` below is omitted when empty.
- `{id}` is a file's entry ID unless the section says otherwise; `{path}` query
  values are vault paths such as `/photos/2024`.
- **Scope.** The vault file can hold sub vaults. Endpoints that work on files
  take the sub vault ID as `?vault=<id>` (GET/DELETE) or `"vault": "<id>"` in the
  body; empty or absent means the main vault. A sub vault must be unlocked first
  (`POST /api/subvaults/{id}/unlock`), or the call fails with `SUB_VAULT_LOCKED`.
- Every `GET` route answers `HEAD` too. A request whose method has no route
  falls through to the catch-all and answers `404 NOT_FOUND` ("no such
  endpoint"), not 405.

## Authentication

Five credentials, each for its own kind of client.

| Credential | Used by | How it is presented | Where it comes from |
|---|---|---|---|
| Session cookie `sand_session` | the browser | cookie | `POST /api/vault/unlock`, `POST /api/vault/init`, `POST /api/vault/kit/import` |
| Stream / zip ticket | a media player, a download manager | in the URL path | `POST /api/files/{id}/stream`, `POST /api/folders/zip`, `POST /api/tv/items/{id}/play` |
| TV device token | Sand TV | `Authorization: Bearer <token>` | `POST /api/tv/pair` with a code from `POST /api/tv/codes` |
| Vault (or sub vault) password | WebDAV clients | HTTP Basic, any username | the password itself |
| OAuth `state` | the provider's redirect | query string | `POST /api/providers/oauth/start` |

**Sessions.** The cookie is `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure`
when the request came over TLS (or with `X-Forwarded-Proto: https`), and has no
`Max-Age`: the server's session store is the only clock. A session lapses after
`--idle-timeout` (default 30 minutes) without use, but never while one of its
requests is still running. When every session has lapsed and nothing else is
active (a stream, a WebDAV request, a transfer, a scan), the vault locks itself.
Locking (by `POST /api/vault/lock` or the idle timer) is global: it zeroes the
keys, ends every session, voids every stream and zip ticket, and stops running
jobs.

Session-protected routes answer `401 LOCKED` when the cookie is missing or
lapsed, or the vault has been locked under it. `GET /api/providers/health` is
checked the same way but does not count as use, so a tab left open does not keep
the vault unlocked.

**Cross-site protection.** There is no CSRF token. Every request that is not
`GET`, `HEAD` or `OPTIONS` and carries an `Origin` header whose host differs from
the request's `Host` is refused with `403 CROSS_ORIGIN` (ports are ignored; an
absent or `null` Origin passes). Together with the `SameSite=Strict` cookie this
stops another site from driving the API through your browser. The guard wraps
the whole server, WebDAV included.

**Exposure.** `/api/vault/unlock` is reachable unauthenticated by anyone who can
reach the port, and on plain HTTP the password and decrypted files cross the
network in the clear. The server warns on every non-loopback bind. See
[security](../security.md).

## Errors

Every JSON error has the same envelope:

```json
{ "error": "human-readable explanation", "code": "LOCKED" }
```

Switch on `code`; the message is for people and may change. The codes, with the
status each is sent with:

| Code | Status | Meaning |
|---|---|---|
| `BAD_REQUEST` | 400 | Malformed body, unknown field, missing or invalid parameter |
| `VAULT_ERROR` | 400 / 500 | Any other refusal from the vault (400), or the vault file could not be opened (500) |
| `LOCKED` | 401 | No valid session, or the vault is locked |
| `WRONG_PASSWORD` | 401 | Wrong vault password (also a wrong kit password) |
| `SUB_VAULT_LOCKED` | 401 | The vault is open but the sub vault named is not: ask for its password |
| `NOT_THE_VAULTS_PASSWORD` | 400 | A second-instance kit import given a password that is not the vault's |
| `NO_VAULT` | 404 | No vault has been created yet |
| `NOT_FOUND` | 404 | No such file, folder, account, sub vault, run, code or endpoint |
| `CROSS_ORIGIN` | 403 | Refused by the same-origin guard |
| `NEEDS_CONVERSION` | 409 | The file is in the pre-chunking format; convert it first |
| `NO_REPLICA` | 404 | A read or drop asked for a replica the file does not have |
| `NO_AUTOMATION` | 404 | The folder has no policy |
| `AUTOMATION_BUSY` | 409 | An automation run is already going |
| `REKEY_PENDING` | 409 | Files are still on the previous key; a second password change must wait |
| `MIGRATION_BUSY` | 409 | A re-encryption is already running |
| `DELETE_BUSY`, `RELOCATE_BUSY`, `TRANSFER_BUSY` | 409 | That kind of run could not be started alongside the one going |
| `REATTACH_BUSY`, `SWEEP_BUSY`, `RESTORE_BUSY`, `MOVIE_SCAN_BUSY` | 409 | A job of that kind is running for a different scope |
| `NO_CHECK` | 409 | A repair was asked for with no finished deep check to repair from |
| `PARSE_ERROR` | 400 | A multipart body could not be read |
| `TOO_LARGE` | 413 | An upload request over the 2 GiB ceiling |
| `MISSING_FILE` | 400 | A multipart upload with no file in it |
| `BAD_PATH` | 400 | An upload's folder path is not a plain name, or climbs out of its folder |
| `BAD_SCHEME` | 400 | An erasure code that cannot be parsed or does not fit the accounts |
| `BAD_SOURCE` | 400 | A relocation `source` other than parts or replica |
| `UNREADABLE` | 502 | A file could not be read back from the accounts |
| `NO_THUMBNAIL` | 404 | The file or folder has no picture |
| `BAD_THUMBNAIL` | 400 | An uploaded thumbnail was refused |
| `NO_TICKET` | 404 | The stream link has expired, was voided by a lock, or its TV code was revoked |
| `NO_BACKUP` | 404 | The account named for recovery holds no backup |
| `NO_KIT_CODE` | 404 | The vault holds no code for that kit |
| `NOT_A_KIT`, `KIT_DAMAGED`, `KIT_CODE_TYPO`, `KIT_CODE_LENGTH`, `KIT_SECRET_WRONG` | 400 | Recovery kit problems: not a kit, damaged, a code with a typo or wrong length, wrong secret |
| `SNAPSHOT_PASSWORD` | 400 | Wrong snapshot password |
| `SNAPSHOT_INCOMPLETE`, `SNAPSHOT_DAMAGED` | 409 | A snapshot is missing parts, or will not open |
| `KEY_EXPIRED` | 400 | A generated SSH key handle has aged out; generate another |
| `KEYGEN_FAILED` | 500 | Key generation failed |
| `OAUTH_NO_CLIENT` | 400 | No OAuth app credentials were given or configured |
| `OAUTH_DENIED`, `OAUTH_STATE`, `OAUTH_FAILED` | 400 | The provider refused, the pasted URL is for another sign-in, or the exchange failed |
| `OAUTH_PENDING` | 409 | The sign-in has not finished yet |
| `PROTON_NO_CLIENT` | 400 | The `proton-drive` client is not installed |
| `NO_MOVIE_KEY` | 412 | No film database key is stored |
| `BAD_MOVIE_KEY` | 502 | The film database refused the key |
| `MOVIE_LOOKUP_OFF` | 403 | Film lookup is not switched on for that folder |
| `NO_ASSISTANT` | 412 | No assistant model server is configured |
| `NO_SUCH_MODEL`, `NO_TOOL_SUPPORT`, `ASSISTANT_LOOPED`, `ASSISTANT_FAILED` | 502 | The model server's failures |
| `PROFILE_PINNED` | 409 | The machine profile is pinned by `--profile` / `SAND_PROFILE` |
| `OUT_OF_RANGE` | 400 | A tuning override outside its knob's range |
| `RESOLVE_FAILED`, `SAVE_FAILED` | 500 | Machine sizing could not be worked out or saved |
| `TV_UNPAIRED` | 401 | Missing, expired or revoked TV token: pair again |
| `BAD_CODE` | 401 | Wrong TV pairing code |
| `THROTTLED` | 429 | Too many wrong pairing codes; see `Retry-After` |
| `TOO_MANY_CODES`, `TOO_MANY_DEVICES` | 409 | 32 TV codes live already / this code has paired 8 TVs |
| `INTERNAL_ERROR`, `INTERNAL` | 500 | Could not mint a token, start a session, or similar |
| `MISSING_PASSWORD`, `DUPLICATE_NAME`, `READ_ERROR`, `ARCHIVE_ERROR`, `INVALID_PARTS`, `RESTORE_ERROR`, `MISMATCHED_PARTS`, `CORRUPT_FILE` | 400 / 500 | [Standalone mode](#standalone-mode) |

Pages that a person opens directly rather than a script (an expired zip link, the
OAuth callback) answer with a small HTML page instead of JSON.

### Partial success

Batch operations report per item instead of failing whole. The rule throughout:
an unreachable account produces a `warnings` entry, not an error, and the
operation completes as far as it can.

- **Upload** (`POST /api/files`) returns one result per file, `201` when at
  least one stored and `502` when none did, with the same body either way. See
  [upload](#post-apifiles).
- **Delete** of a file, a selection or a folder drops the index entries even when
  some parts could not be erased, and lists those in `warnings`.
- **Moves** of a batch report `moved`, `missing` and `refused` separately.
- **Relocation, imports, exports, repairs and restores** report counts of what
  succeeded, failed and was deferred, plus `warnings`.

## Long-running work

Three patterns, chosen by how long the work runs.

**Held request.** The request stays open until the work is done (deletes of one
file, a deep check of one file, git track, imports without `detach`). Each has
its own server-side deadline, longer than the default.

**Job** (deep check, repair, checksums, snapshots, lost-parts scan and restore,
stray scan, reattach, sweep, film scan). One job of each kind at a time.
`POST` starts it, `GET` polls it, `POST …/stop` (or `DELETE`) stops it. The
start answers:

| Status | Meaning |
|---|---|
| `202 Accepted` | A new job started |
| `200 OK` | Joined the job already running for the same scope |
| `409 Conflict` | A job of this kind is running for a different scope (body is still the status) |

Every job status has this shape:

```json
{
  "id": "…", "running": true, "scope": "…",
  "started_at": "…", "finished_at": "…",
  "progress": { "…": "kind-specific" },
  "result": { "…": "the finished report" },
  "error": "…"
}
```

Jobs keep their last result until the next one starts, and are stopped when the
vault locks.

**Run** (detached delete, relocation, machine import and export). Pass
`"detach": true` and the call answers `202 {"run": {…}}` at once; the runs list
is then polled, and `DELETE …/runs/{run}` stops a running run or dismisses a
finished one. A run object:

```json
{
  "id": "…", "label": "…", "detached": true, "waiting": false,
  "started_at": "…", "finished_at": "…",
  "at": { "…": "progress" }, "rate": 0,
  "done": false, "report": { }, "error": "", "cancelled": false
}
```

(Transfers carry `kind`, `source`, `dest`, `vault?` and `summary` in place of
`label` and `report`.)

---

## Summary

Auth column: **public** needs nothing; **session** needs the cookie and an
unlocked vault; **quiet** is a session check that does not count as use;
**ticket**, **TV** and **Basic** are described [above](#authentication).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/health` | public | Liveness and version |
| GET | `/api/vault` | public | Initialized? unlocked? (details only to a session) |
| POST | `/api/vault/init` | public | Create the vault, start a session |
| POST | `/api/vault/unlock` | public | Unlock, start a session |
| POST | `/api/vault/lock` | session | Lock, end every session |
| POST | `/api/vault/policy` | session | Set placement policy |
| POST | `/api/vault/defaults` | session | Default accounts and scheme for uploads |
| POST | `/api/vault/backup` | session | Manifest backup on/off, force a write |
| POST | `/api/vault/backup/delay` | session | Least time between backup pushes |
| GET | `/api/vault/instances` | session | Machines opening this vault |
| POST | `/api/vault/sync` | session | Pull other machines' index changes |
| POST | `/api/vault/password` | session | Change password, rotate key |
| POST | `/api/vault/migrate` | session | Finish re-encryption now |
| GET · PUT | `/api/vault/rekey` | session | Re-key status · plan |
| POST | `/api/vault/rekey/skip` | session | Leave a file for last, or undo |
| GET | `/api/vault/rekey/files` | session | Files still on the previous key |
| DELETE | `/api/vault/rekey/files/{id}` | session | Delete a file that will not move |
| GET · POST | `/api/vault/recovery` | session | Scan for a recoverable backup · recover |
| POST | `/api/vault/recovery/resume` | session | Re-point at reconnected accounts |
| POST | `/api/vault/reclaim` | session | Rebuild recovered files onto this vault's key |
| POST | `/api/vault/kit/inspect` | public | Read a kit's header |
| POST | `/api/vault/kit/import` | public | Create the vault from a kit |
| GET · POST | `/api/vault/kit` | session | Kit status · export a kit |
| POST | `/api/vault/kit/verify` | session | Test a kit against the accounts |
| GET · DELETE | `/api/vault/kit/code/{id}` | session | Read · forget a kit's code |
| GET · POST | `/api/vault/orphans` | session | Unreferenced parts · erase them |
| GET | `/api/vault/orphans/erasing` | session | Progress of that erase |
| POST | `/api/vault/orphans/reattach` | session | Record mislaid shards (held) |
| GET | `/api/vault/orphans/strays` | session | Mislaid shards on one or all accounts |
| POST · GET | `/api/vault/orphans/scan` | session | Stray scan job |
| POST · GET | `/api/vault/orphans/reattaching` | session | Reattach job |
| POST · GET | `/api/vault/orphans/sweeping` | session | Sweep job |
| POST | `/api/vault/orphans/leftovers` | session | Erase working files left on disk |
| POST · GET | `/api/vault/lost-parts/scan` | session | Lost-parts scan job |
| POST · GET | `/api/vault/lost-parts/restore` | session | Lost-parts restore job |
| POST | `/api/vault/lost-parts/restore/stop` | session | Stop it |
| GET · POST | `/api/vault/versions` | session | Old bucket versions · erase them |
| GET | `/api/vault/versions/erasing` | session | Progress of that erase |
| GET | `/api/providers/specs` | public | Backend descriptions |
| GET · POST | `/api/providers` | session | List · connect accounts |
| PATCH · DELETE | `/api/providers/{id}` | session | Edit · disconnect an account |
| POST | `/api/providers/{id}/test` | session | Ping one account |
| GET | `/api/providers/stats/{id}` | session | One account taken apart |
| POST | `/api/providers/{id}/measure` | session | Count a bucket by listing it |
| GET | `/api/providers/health` | quiet | Last health check |
| POST | `/api/providers/health/check` | session | Ping every account now |
| POST | `/api/providers/health/schedule` | session | Health check schedule |
| POST | `/api/providers/oauth/start` | session | Begin a sign-in |
| GET | `/api/providers/oauth/callback` | state | Provider redirect target |
| GET | `/api/providers/oauth/{id}` | session | Sign-in status |
| POST | `/api/providers/oauth/exchange` | session | Finish from a pasted redirect URL |
| POST | `/api/providers/oauth/complete` | session | Turn a sign-in into an account |
| POST | `/api/providers/oauth/reauthorize` | session | Spend a sign-in on an existing account |
| POST | `/api/providers/proton/signin` | session | Begin a Proton client sign-in |
| GET | `/api/system/folders` | session | Folders on the server machine |
| POST | `/api/ssh/keypair` | session | Generate an SSH key pair |
| GET · POST | `/api/subvaults` | session | List · create sub vaults |
| PATCH · DELETE | `/api/subvaults/{id}` | session | Rename · delete |
| POST | `/api/subvaults/{id}/unlock` · `/lock` | session | Open · shut |
| POST | `/api/subvaults/{id}/password` · `/migrate` | session | Change password · finish re-key |
| GET · POST | `/api/subvaults/{id}/defaults` | session | Sub vault upload defaults |
| POST | `/api/assign` | session | Move between vaults |
| POST | `/api/vaults/import` | session | Import a found vault as a sub vault |
| DELETE | `/api/vaults/found/{provider}` | session | Erase a found vault's index |
| GET · POST | `/api/files` | session | List a folder · upload |
| GET | `/api/search` | session | Search by name |
| POST | `/api/files/precheck` · `/retime` | session | Before upload: what is already here · fix times |
| GET · DELETE | `/api/files/{id}` | session | Metadata · delete |
| GET | `/api/files/{id}/content` | session | The file's bytes, ranged |
| POST | `/api/files/{id}/stream` | session | Mint a stream link |
| GET | `/stream/{token}`, `/stream/{token}/{name}` | ticket | Play a stream link |
| GET · PUT | `/api/files/{id}/thumb` | session | Thumbnail |
| GET | `/api/files/{id}/health` | session | Per-part reachability |
| POST | `/api/files/{id}/move` | session | Rename or move a file |
| POST | `/api/files/move` | session | Move a batch |
| POST | `/api/files/delete` | session | Delete a batch of files |
| GET · DELETE | `/api/files/erasing` | session | That batch's progress · stop |
| POST | `/api/delete` | session | Delete a selection as a run |
| GET | `/api/delete/runs` | session | Delete runs |
| DELETE | `/api/delete/runs/{run}` | session | Stop or dismiss one |
| GET | `/api/degraded` | session | Files missing a part |
| GET | `/api/conversions` | session | Files in the old format |
| POST | `/api/files/{id}/convert` | session | Convert one |
| DELETE | `/api/files/{id}/replica` | session | Drop a file's replica |
| GET · POST · DELETE | `/api/folders` | session | Folder tree · create · delete |
| GET · DELETE | `/api/folders/erasing` | session | Folder delete progress · stop |
| POST | `/api/folders/move` | session | Move a folder |
| GET · POST | `/api/folders/art` | session | Folder picture |
| GET | `/api/folders/survey` · `/date-sort` · `/stats` · `/duplicates` | session | Read-only folder analyses |
| POST | `/api/folders/zip` | session | Mint a folder zip link |
| GET | `/zip/{token}/{name}` | ticket | Download the zip |
| POST | `/api/relocate` | session | Move onto other accounts |
| GET | `/api/relocate/runs` | session | Relocation runs |
| DELETE | `/api/relocate/runs/{run}` | session | Stop or dismiss one |
| POST | `/api/files/{id}/check` | session | Deep check one file |
| GET | `/api/check/survey` | session | What a folder check would read |
| POST · GET | `/api/check` | session | Deep check job |
| POST | `/api/check/stop` | session | Stop it |
| POST · GET | `/api/check/repair` | session | Repair job |
| POST | `/api/check/repair/stop` | session | Stop it |
| GET | `/api/checksums/survey` | session | Checksum coverage |
| POST · GET | `/api/checksums` | session | Verify / compute job |
| POST | `/api/checksums/pause` · `/resume` · `/rate` · `/stop` | session | Control it |
| POST | `/api/checksums/ledger` | session | Write `/checksums.gz` now |
| GET | `/api/snapshots/plan` · `/stored` | session | Snapshot size · list |
| POST | `/api/snapshots/inspect` · `/delete` | session | Open · erase one |
| POST · GET | `/api/snapshots` | session | Take job · status |
| POST | `/api/snapshots/restore` · `/stop` | session | Restore job · stop |
| GET · PUT | `/api/vault/replication` | session | Replication status · policy |
| POST | `/api/replicate` | session | Replicate, exclude or reset a path |
| GET · POST · DELETE | `/api/automation` | session | Folder policies |
| POST | `/api/automation/run` | session | Run a policy now |
| GET | `/api/git` | session | Stored repositories |
| POST | `/api/git/track` | session | Track a repository |
| POST | `/api/git/{id}/refresh` · `/rebuild` | session | Refresh · rebuild |
| DELETE | `/api/git/{id}` | session | Untrack |
| GET | `/api/movies` | session | Film details settings |
| POST | `/api/movies/key` · `/lookup` · `/scan` | session | Key · per-folder switch · sweep (held) |
| POST · GET · DELETE | `/api/movies/scanning` | session | Sweep job |
| GET · POST · DELETE | `/api/files/{id}/movie` | session | A file's film |
| GET | `/api/files/{id}/movie/candidates` | session | Search the film database |
| GET | `/api/assistant` | session | Assistant settings |
| POST | `/api/assistant/settings` · `/ask` | session | Configure · ask |
| GET · POST | `/api/remote` | session | Machines · connect one |
| PATCH · DELETE | `/api/remote/{id}` | session | Edit · remove |
| GET | `/api/remote/{id}/files` | session | Browse a machine |
| POST · GET | `/api/remote/{id}/import` | session | Import from it · progress |
| DELETE | `/api/remote/{id}/import/{run}` | session | Stop or dismiss |
| POST · GET | `/api/remote/{id}/export` | session | Export to it · progress |
| DELETE | `/api/remote/{id}/export/{run}` | session | Stop or dismiss |
| GET | `/api/reads` · `/api/transfers` · `/api/throughput` | session | Per-account statistics |
| POST | `/api/reads/forget` | session | Erase read history |
| GET | `/api/reads/watch/{token}` | session | Progress of one read |
| GET · PUT · DELETE | `/api/machine` | session | Machine sizing |
| GET · POST | `/api/tv/codes` | session | TV codes · mint one |
| DELETE | `/api/tv/codes/{code}` | session | Revoke a code |
| POST | `/api/tv/pair` | public | Trade a code for a token |
| GET | `/api/tv/session` | TV | Pairing and lock state |
| POST | `/api/tv/logout` | TV | Unpair |
| GET | `/api/tv/library` | TV | Folders and videos |
| GET | `/api/tv/items/{id}` · `/poster` | TV | One video · its poster |
| POST | `/api/tv/items/{id}/play` | TV | Stream link and subtitles |
| GET | `/api/tv/folders/art` | TV | A folder's picture |
| GET | `/tv`, `/tv/…` | public | The TV app's page |
| * | `/dav/…` | Basic | WebDAV share (with `--webdav`) |
| * | `/sub/<name>/…` | Basic | A sub vault's WebDAV share |
| POST | `/api/archive` · `/api/restore` | public | Standalone mode |
| GET | `/…` | public | The embedded web UI |

---

## Vault

#### GET /api/health

Public. `{"status": "ok", "version": "v2026.11.0"}`.

#### GET /api/vault

Public. Without a valid session, only `initialized`, `unlocked: false`, `path`
and `policy` are meaningful. With one, the full status:

| Field | Meaning |
|---|---|
| `initialized`, `unlocked` | Lifecycle |
| `path`, `policy` | Vault file and placement policy (`strict` / `redundant`) |
| `stats?` | `files`, `folders`, `bytes`, `stored_bytes`, `degraded`, `accounts`, `policy`, `default_accounts`, `default_scheme?`, `pending_migration`, `stale_parts?`, `stale_bytes?`, `unresolved`, `stranded`, `inherited_key`, `key_id`, `replication?` |
| `webdav?` | `{path, sub_vaults?: {id: path}}` when served with `--webdav` |
| `sub_vaults?` | Each `{id, label, created_at, unlocked, files, stored_bytes, pending_migration?, key_id?, defaults?}` |
| `manifest_backup`, `manifest_backup_delay_seconds` | Index backup on/off and its delay |
| `instance`, `peers` | This machine's name; how many other machines open the vault |

#### POST /api/vault/init

Public. `{"password": "…", "policy": "strict"}` (`policy` defaults to `strict`).
Creates the vault, sets the session cookie, answers `201` with the full status.
Errors: `VAULT_ERROR` (already exists).

#### POST /api/vault/unlock

Public. `{"password": "…"}`. Unlocks, pulls other machines' index changes in the
background, sets the session cookie, answers `200` with the full status. Errors:
`401 WRONG_PASSWORD`, `404 NO_VAULT`.

#### POST /api/vault/lock

Locks the vault for everyone (see [sessions](#authentication)), clears the
cookie, answers `{initialized: true, path}`.

#### POST /api/vault/policy

`{"policy": "strict" | "redundant"}` → `{"policy": …}`.

#### POST /api/vault/defaults

`{"accounts": ["id", …], "scheme?": "3-of-5"}`; an empty list clears the default.
→ `{"default_accounts": […], "default_scheme": "…"}`. `400 BAD_SCHEME` when the
scheme does not fit.

#### POST /api/vault/backup

`{"enabled": true, "force": false}`. Turning it off erases the copies on the
accounts; `force` overwrites a backup written by another vault or an older
password. → `{"manifest_backup", "claimed", "warnings"}`.

#### POST /api/vault/backup/delay

`{"seconds": 5}` (0 to 3600). → `{"manifest_backup", "manifest_backup_delay_seconds"}`.

#### GET /api/vault/instances

→ `{"instance": "this-host", "instances": [{name, last_seen, accounts, here}], "last_sync": "…" | null}`.

#### POST /api/vault/sync

Pulls now. → `{instance, asked, answered, foreign?, outcome, added, removed,
updated, renamed?, instances, warnings?, synced_at}`.

## Password change and re-key

#### POST /api/vault/password

```json
{ "old_password": "…", "new_password": "…", "migrate": true,
  "plan": { "bytes_per_day": 10737418240, "full_speed": false, "folders": ["/Taxes"],
            "only_folders": false, "paused": false, "keep_unlocked": false } }
```

Changes the password and rotates the data key. With `migrate` true (the default)
and no `plan`, re-encrypts everything before answering; `"migrate": false`
defers it; a `plan` paces it in the background. → `{pending, migrated,
remaining, bytes, warnings?}`. `409 REKEY_PENDING` while files from a previous
change remain. Design: [paced re-key](../design/paced-rekey.md).

#### POST /api/vault/migrate

Moves everything still on an old key now (held). → the same migration report.
`409 MIGRATION_BUSY` when one is running.

#### GET /api/vault/rekey · PUT /api/vault/rekey

`GET` → the re-key status: `pending`, `pending_bytes`, `total`, `total_bytes`,
`plan?` (the settings plus `started_at`, `files`, `bytes`, `level?`,
`deferred?`), `since`, `next_in_seconds?`, `eta_seconds?`, `idle`, `folders`
(`{path, files, bytes, queued}`), `running`, `current?`, `current_id?`,
`current_done?`, `current_size?`, `deferred?`, `held?`, `held_on?`,
`retrying?`, `retry_in_seconds?`, `stale_replicas?`, `stale_parts?`,
`warnings?`.

`PUT` with the `plan` object above replaces the plan and answers the status.

#### POST /api/vault/rekey/skip

`{"id": "<file id>", "undo": false}` leaves a file for last (or puts it back).
→ status.

#### GET /api/vault/rekey/files

`?offset=&limit=&folder=` (limit default 50, at most 500). → `{files: [{id, path,
dir, name, bytes, state, held_on?, reason?, retry_at}], offset, limit, folder,
total, bytes}`.

#### DELETE /api/vault/rekey/files/{id}

Deletes a file that will not move. → `{status: "deleted", path, warnings}`.

## Recovery

Guide: [recovery](../guide/recovery.md).

#### GET /api/vault/recovery

Scans the connected accounts for a backup this vault could adopt. →
`{vault_empty, available, resumable, unresolved, stranded, parts, bytes,
sources: [{provider_id, name, kind, backup, foreign, parts, bytes, error?}],
warnings?}`.

#### POST /api/vault/recovery

`{"provider_id": "…", "password": "<the lost vault's>", "dry_run": false}`. →
`{report, source, backup_at}`. `404 NO_BACKUP`, `401 WRONG_PASSWORD`.

#### POST /api/vault/recovery/resume

`{"dry_run": false}`; no password. → `{report}`.

#### POST /api/vault/reclaim

`{"accounts": ["id", …]}` (empty: each file stays on its accounts). Fresh key,
every file rebuilt (held). → `{key_id, accounts?, pending, migrated, remaining,
bytes, warnings?}`.

## Recovery kit

Design: [recovery kit](../design/recovery-kit.md).

#### POST /api/vault/kit/inspect

Public, *multipart*: `kit` (the zip). → `{kit_id, created_at, secret}` where
`secret` says whether it wants a code or a password. `400 NOT_A_KIT`.

#### POST /api/vault/kit/import

Public, *multipart*: `kit`, `secret` (the code or the old vault password),
`password` (the new vault's), and optional `replace`, `old_password`,
`skip_cloud_index`, `sibling` (`true`/`1`; `sibling` is the second-instance
join). Creates the vault, starts a session. → `{report, status}`. Errors:
`KIT_CODE_TYPO`, `KIT_CODE_LENGTH`, `KIT_DAMAGED`, `KIT_SECRET_WRONG`,
`WRONG_PASSWORD`, `NOT_THE_VAULTS_PASSWORD`.

#### GET /api/vault/kit

→ `{exported_at, kit_id?, secret?, code_retained, age_days, files, files_added,
accounts, exported, accounts_changed, password_changed_since}`.

#### POST /api/vault/kit

`{"use_vault_password": false, "password": ""}` (empty body allowed). Answers the
zip itself (`application/zip`, `Content-Disposition: attachment;
filename="sand-recovery-kit-<date>.zip"`, `Cache-Control: no-store`), with
headers `X-Sand-Kit-Id`, `X-Sand-Kit-Secret`, `X-Sand-Kit-Sha256` and, for a
code-sealed kit, `X-Sand-Kit-Code`. The code is in no other place in the
response.

#### POST /api/vault/kit/verify

*Multipart*: `kit`, `secret`. → `{kit_id, kit_created_at, secret, age_days,
accounts: [{id, name, kind, status, detail?}], working, unusable, listed,
not_listed, kit_files, kit_bytes, recoverable, recoverable_bytes, vault_files,
added_since, accounts_added?, stale_index, warnings?}`.

#### GET /api/vault/kit/code/{id} · DELETE

`GET` → `{"code": "…"}` (`no-store`), `404 NO_KIT_CODE`. `DELETE` →
`{"forgotten": true}`.

## Housekeeping

Comparing what the accounts hold with the index. Guide:
[maintenance](../guide/maintenance.md).

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/vault/orphans` | | Orphan scan: totals (`objects`, `bytes`, `archives`, `deletable`, `deletable_bytes`, `previous_key*`), `strays` (mislaid shards with `have`/`want`/`reattachable`), `reattachable`, `accounts` (per account: `orphans`, `strays`, `backup`, `foreign`, `error?`), `items` (per archive: `provider_id`, `archive_id`, `objects`, `bytes`, `deletable`, `reason?`, `path?`), `lost?`, `leftovers?` (working files on disk), `blocked?`, `warnings?` |
| `POST /api/vault/orphans` | `{targets: [{provider_id, archive_id}], dry_run}` (empty targets: all). Re-scans first | `{archives, deleted, bytes, skipped?, warnings?}` |
| `GET /api/vault/orphans/erasing` | | `{running, done, total}` |
| `POST /api/vault/orphans/reattach` | `{provider_id?, dry_run}` (held) | `{shards, files, bytes, restored?, skipped?, merged?, warnings?}` |
| `GET /api/vault/orphans/strays` | `?provider=` (optional) | `{accounts, strays, strays_truncated?, reattachable, stray_files, stray_bytes, warnings?}`; unknown provider `404` |
| `POST · GET /api/vault/orphans/scan` | | Job; result is the orphan scan; progress `{stage, roots, listed, waiting?}` |
| `POST · GET /api/vault/orphans/reattaching` | `{provider_id?}` | Job; result is the reattach report |
| `POST · GET /api/vault/orphans/sweeping` | `{targets}` | Job; progress `{done, total}`; result is the sweep report |
| `POST /api/vault/orphans/leftovers` | `{names: […], dry_run}` (empty: all) | `{deleted, bytes, skipped?, warnings?}` |
| `POST · GET /api/vault/lost-parts/scan` | | Job; result `{found, parts, files, objects, bytes, restorable, on?, items: [{provider_id, file_id, path?, part, objects, of, bytes, restorable, reason?}], accounts: [{…, recorded, lost, emptied?}], warnings?}` |
| `POST · GET /api/vault/lost-parts/restore` | `{provider_ids: […]}` | Job; result `{parts, files, objects, bytes, already_there?, withheld, failed, failures?, on?, stopped?, warnings?}` |
| `POST /api/vault/lost-parts/restore/stop` | | Job status |
| `GET /api/vault/versions` | | `{found, versioned, stale, stale_bytes, markers, deletable, deletable_bytes, accounts: [{…, versioned, auto_prune?, last_prune?}], items: [{provider_id, key, what, versions, markers, bytes, deletable, reason?}], warnings?}` |
| `POST /api/vault/versions` | `{accounts: [ids], dry_run}`. Re-scans first | `{deleted, markers, bytes, accounts?: {id: {deleted, markers, bytes, error?}}, skipped?, warnings?}` |
| `GET /api/vault/versions/erasing` | | `{running, done, total}` |

## Accounts

Design: [providers](../design/providers.md).

#### GET /api/providers/specs

Public. → `{"specs": [{kind, label, description, docs_url?, fields: [{key,
label, placeholder?, help?, secret?, required?, default?, directory?, advanced?,
multiline?, ssh_key?}], presets?, covers?, oauth?, sign_in_link?, local?}]}`.
`oauth.configured` says whether app credentials come from the environment.

#### GET /api/providers

→ `{"providers": [...]}`; each account is its config with secrets redacted
(`id, kind, name, options, added_at, host?, color?, replica_folder?, capacity?,
quota?, auto_prune?`) plus `online`, `error?`, `elsewhere?` (another machine's
local folder), `shards`, `stored`, `usage` (`used, total, free?, measured?,
measured_at, declared?`), `measurable?` and `space` (`free, used, total,
source?: reported | declared | quota, over?`). Pings each account (30 s
deadline).

#### POST /api/providers

`{"kind": "s3", "name": "r2", "options": {"bucket": "…", …}}`. Pinged before it
is saved. → `201 {"provider": {…}}`. `400 KEY_EXPIRED` for a stale generated-key
handle.

#### PATCH /api/providers/{id}

Any of `name`, `color` (`""` for automatic), `capacity`, `quota` (text such as
`"10 GB"`, `""` to clear), `auto_prune` (bool; refused on a backend without
versions), `options`. Only `options` reaches the backend: the account is
connected with the new settings before they are stored, and a secret sent back as
the redaction placeholder is left as it was. → `{"provider": {…}}`.

#### DELETE /api/providers/{id}

`?force=1` to disconnect even if it makes files unrecoverable. → `{"status":
"removed"}`.

#### POST /api/providers/{id}/test

→ `{"online": true}` or `{"online": false, "error": "…"}` (always 200).

#### GET /api/providers/stats/{id}

(The ID trails `stats` because `{id}/stats` would collide with
`/api/providers/oauth/{id}`.) → `{"stats": {…status, vault_shards,
vault_stored, files, sole, kinds, folders, largest, months, sub_vaults}}`.

#### POST /api/providers/{id}/measure

Lists the bucket end to end (10 minute deadline). → `{"usage": {…}}`.

#### GET /api/providers/health

Quiet session check; reads memory, contacts nothing. → `{"health": {clouds:
[{id, name, kind, checked, healthy, error?, checked_at, elsewhere?, took_ms?,
failing_since, disk?, disk_warning?}], accounts, healthy, unhealthy, unchecked,
warnings?, elsewhere?, checked_at, schedule: {enabled, interval_minutes,
smart_enabled}, next_check_at}}`.

#### POST /api/providers/health/check

Pings every account now. → `{"health": …}`.

#### POST /api/providers/health/schedule

`{"enabled": true, "interval_minutes": 60, "smart_enabled": true}`.
`interval_minutes` 0 keeps the current interval; otherwise 5 minutes to 7 days.
An omitted `enabled` or `smart_enabled` keeps its value. → `{"schedule"}`, plus
`health` when the change triggered a check.

#### GET /api/system/folders

`?path=` (with `~` expansion; default a sensible start). Folders on the server
machine, for the Local folder picker. → `{requested, path, parent?, exists,
separator, folders: [{name, path, hidden?}], roots: [{label, path}], truncated,
error?}`. When the path does not exist, the nearest existing parent is listed.

#### POST /api/ssh/keypair

`{"comment?": "…"}`. → `201 {handle, public_key, fingerprint}`. The private key
stays on the server; pass `handle` as the private-key field of an SFTP account
or machine, and it is swapped in when that request arrives. Handles age out
(`KEY_EXPIRED`).

## Sign-in flows

For the backends with a consent screen (Google Drive, OneDrive, Dropbox, Box,
Zoho WorkDrive) and Proton's client.

```
POST /api/providers/oauth/start ──► {flow_id, auth_url}
        browser → auth_url → provider → GET /api/providers/oauth/callback?state=&code=
        (or paste the final URL → POST /api/providers/oauth/exchange)
GET /api/providers/oauth/{flow_id} ──► status: pending | ready | error
POST /api/providers/oauth/complete      (new account)
POST /api/providers/oauth/reauthorize   (existing account, same ID and parts)
```

#### POST /api/providers/oauth/start

`{"kind": "gdrive", "client_id?", "client_secret?", "redirect_uri?", "region?",
"provider_id?"}`. App credentials default to the `SAND_*_CLIENT_ID` environment
variables; `provider_id` starts a re-authorization of an existing account. The
redirect URI is `<origin>/api/providers/oauth/callback` unless
`SAND_OAUTH_REDIRECT` pins it. → `{flow_id, auth_url, redirect_uri,
expires_in: 900}`. `400 OAUTH_NO_CLIENT`.

#### GET /api/providers/oauth/callback

Authenticated by the flow's `state` (the cookie does not survive the cross-site
redirect). Exchanges `code`, records the result on the flow, and answers an HTML
page telling the user to return to the app.

#### POST /api/providers/oauth/exchange

`{"flow_id", "url"}`: the URL the browser was left on, for when the redirect
could not reach the server. Errors: `OAUTH_DENIED`, `OAUTH_STATE`,
`OAUTH_FAILED`. → the flow status.

#### GET /api/providers/oauth/{id}

→ `{flow_id, kind, status, account, provider_id?, sign_in_url?, error?,
suggested_name?}` (`suggested_name` once `ready`).

#### POST /api/providers/oauth/complete

`{"flow_id", "name", "options"}`. → `201 {"provider"}`. `409 OAUTH_PENDING`.

#### POST /api/providers/oauth/reauthorize

`{"flow_id", "provider_id"}`. New credentials for the same account. →
`{"provider"}`.

#### POST /api/providers/proton/signin

`{"options": {"folder": "…", …}, "provider_id?": "…"}`. Starts the
`proton-drive` client in the background and answers at once with `{flow_id,
expires_in}`; poll `GET /api/providers/oauth/{flow_id}`, whose `sign_in_url` is
the link to open, then complete or reauthorize as above. `400
PROTON_NO_CLIENT`.

## Sub vaults

Guide: [vault management](../guide/vault-management.md).

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/subvaults` | | `{sub_vaults: [SubVaultInfo]}` |
| `POST /api/subvaults` | `{label, password}` | `201` SubVaultInfo |
| `POST /api/subvaults/{id}/unlock` | `{password}` | `{id, unlocked: true}`; `401 WRONG_PASSWORD` |
| `POST /api/subvaults/{id}/lock` | | `{id, unlocked: false}`; drops every stream link (all vaults') |
| `PATCH /api/subvaults/{id}` | `{label}` | `{id, label}` (no sub vault password needed) |
| `POST /api/subvaults/{id}/password` | `{password, new_password, migrate?}` | migration report |
| `POST /api/subvaults/{id}/migrate` | | migration report |
| `GET /api/subvaults/{id}/defaults` | | `{default_accounts, default_scheme?, inherited}` |
| `POST /api/subvaults/{id}/defaults` | `{accounts, scheme?}` | the same |
| `DELETE /api/subvaults/{id}` | `?force=1` when locked | `{status: "deleted", warnings}` |
| `POST /api/assign` | `{from, to, target, migrate?}`: `from`/`to` are sub vault IDs (`""` = main), `target` a file ID, file path or folder path | `{files, folders, renamed, migration?, warnings?}` |
| `POST /api/vaults/import` | `{provider, backup_password, password, label, adopt_backup?}` | `{sub_vault, files, folders, relocated, unreachable, recoverable, warnings?}` |
| `DELETE /api/vaults/found/{provider}` | | `{account, claimed}` |

SubVaultInfo is `{id, label, created_at, unlocked, files, stored_bytes,
pending_migration?, key_id?, defaults?}`.

## Files

A file entry, as listings and metadata return it:

```json
{
  "id": "…", "dir": "/photos", "name": "a.jpg", "size": 123, "hash": "sha256 hex",
  "mime": "image/jpeg", "archive_id": "…", "created_at": "…", "modified_at": "…",
  "shards": [{ "part": 1, "provider_id": "…", "provider_name": "…", "provider_kind": "s3", "key": "…", "size": 456 }],
  "key_id": "…", "chunk_size": 0, "chunk_count": 0, "data_shards": 2, "total_shards": 3,
  "replica": null, "replicate": null, "indexed_at": "…", "placed_at": "…"
}
```

#### GET /api/files

`?path=/&vault=`. → `{path, parent, folders: [names], files: [entry], thumbs:
[ids with a thumbnail], movies?, movie_lookup, automation?, folder_art?,
repos?, tracked_repos?, previous_key?, replicate, replicated_folders?,
unreachable?}`.

#### GET /api/search

`?q=&path=&type=all|file|folder&limit=&vault=` (limit default 200). → `{query,
scope, hits: [{type, path, dir, name, file?}], matched, truncated, thumbs,
movies?, tracked_repos?, folder_art?}`. `400` for an empty query.

#### POST /api/files

Upload, *multipart*. One request is capped at 2 GiB (`413 TOO_LARGE`); the
browser cuts a large choice into batches.

| Field | Meaning |
|---|---|
| `files[]` | The files (repeatable) |
| `path` | Destination folder (default `/`) |
| `vault` (query) | Sub vault |
| `overwrite` | `true`/`1` to replace a file of the same name |
| `accounts` or `accounts[]` | Accounts to spread over, repeated or comma-separated |
| `scheme` | `k-of-n` |
| `rel-N` | The path file N had inside an uploaded folder |
| `mod-N` | File N's modification time, ms since the epoch |
| `thumb-N` | A thumbnail JPEG for file N |
| `dirs` | Folders of an uploaded tree that hold no file |

Folder paths from `rel-N` and `dirs` are rebuilt under `path` and checked like
typed names (`400 BAD_PATH`). The answer is `201` if at least one file stored and
`502` if none did, with per-file results either way:

```json
{
  "stored": 1,
  "results": [
    { "name": "a.pdf", "ok": true, "file": { "…": "…" } },
    { "name": "2024/b.mov", "ok": false,
      "error": "stored only 1 of 3 shards, need at least 2: …",
      "stage": "scattering",
      "parts": { "stored": 1, "needed": 2, "total": 3 },
      "failed_parts": [ { "part": 2, "account_id": "…", "account": "Drive", "kind": "gdrive", "reason": "…" } ],
      "advice": "Nothing of the file was kept: …" }
  ]
}
```

`name` is the path inside the uploaded folder, so four `cover.jpg` are told
apart. `stage` is `placing`, `reading`, `scattering` or `recording`. A folder
that could not be made is a result line of its own. `warnings` on an OK result
note things like a missing spare part or a thumbnail that did not store.

#### POST /api/files/precheck

`{"path", "vault?", "files": [{"name", "rel?", "size", "mod?"}]}`. Index only.
→ `{"existing": [...], "retime": [...]}`: which files are already stored at
their destination with the same name and size, and which of those carry a
different modification time.

#### POST /api/files/retime

Same body. Corrects stored modification times (name and size re-checked). →
`{"retimed": n}`.

#### GET /api/files/{id}

→ `{"file": entry, "path": "/full/path"}`.

#### GET /api/files/{id}/health

Per-part reachability without downloading. → `{id, path, recoverable, shards:
[{…shard, present, observed_size, error?, elsewhere?}], scheme, needed, spare,
chunks_sampled?, chunk_count?, replica?}`.

#### GET /api/files/{id}/thumb · PUT

`GET` → `image/jpeg`, or `404 NO_THUMBNAIL`. `PUT` with the JPEG as the body
stores one → `201 {"status": "stored"}`; `400 BAD_THUMBNAIL`.

#### GET /api/degraded

`?offset=&limit=` (default 25, at most 200). Files missing at least one part,
worst first, from the index alone. → `{files: [{id, path, dir, name, size, mime,
shards, data_shards, total_shards, stored, missing, spare, readable, replica?,
tracked?, modified_at}], total, offset, limit, bytes, unreadable}`. A missing part
is put back with `POST /api/relocate`.

#### GET /api/conversions · POST /api/files/{id}/convert

`GET` → `{files: [{id, path, size}], bytes}`: files still in the pre-chunking
format. `POST` converts one (held) → `{path, size, chunk_count, warnings?}`.

## Reading and streaming

#### GET /api/files/{id}/content

The file's bytes through `http.ServeContent`, so `Range`, `If-None-Match` and
`HEAD` work; a range costs only the chunks it covers.

| Parameter | Meaning |
|---|---|
| `from` | `parts` (default), `replica` or `either` |
| `download` | `1`/`true` for `Content-Disposition: attachment` |
| `watch` | A client-chosen token; progress is then readable at `GET /api/reads/watch/{token}` |

Headers: the stored MIME type, `X-Content-Type-Options: nosniff`,
`Content-Security-Policy: default-src 'none'; sandbox`, `Cache-Control: private,
no-store`, `ETag` of the file hash. HTML, SVG, XML and JavaScript are always
served as attachments. Errors: `409 NEEDS_CONVERSION`, `404 NO_REPLICA`, `502
UNREADABLE`. A read that receives nothing from the accounts for 30 minutes is
abandoned.

#### POST /api/files/{id}/stream

`?from=parts|replica|either`. Mints a stream ticket for one file. →
`201 {url: "/stream/<token>/<name>", name, mime?, expires_at, expires_in}`. The
URL is relative; the client resolves it against the origin it reached the
server on.

#### GET /stream/{token}/{name}

No session: the token is the credential. `{name}` is cosmetic (players choose a
demuxer from the extension); `/stream/{token}` works too. Served like
`/content`, ranged. Ticket rules:

| | |
|---|---|
| Scope | One file, one placement (parts or replica), fixed at minting |
| Lifetime | 12 hours, slid forward on every request |
| Storage | Memory only; voided when the vault locks or the server restarts |
| TV links | Also die when their TV code is revoked, mid-film included |
| Keep-alive | Counts as vault activity while bytes are flowing; a paused player does not |

Errors: `404 NO_TICKET`, `401 LOCKED`.

#### GET /api/reads/watch/{token}

→ `{"read": {phase, chunk, chunks, needed, have, accounts: [{provider_id, name,
kind, part, state, took_ms?, error?}], sent, size, error?, updated}}`; `404` once
it is gone.

## Folders

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/folders` | `?vault=` | `{folders: [paths]}`, root first |
| `POST /api/folders` | `{path}` or `{paths: […]}`, `vault?` | `201 {path}` or `201 {paths}` |
| `DELETE /api/folders` | `?path=&recursive=1&vault=` (held) | `{status: "deleted" \| "stopped", warnings}` |
| `GET /api/folders/erasing` | `?path=&vault=` | `{running, done, total}` |
| `DELETE /api/folders/erasing` | `?path=&vault=` | `{stopped}` |
| `POST /api/folders/move` | `{from, to, vault?}` | `{from, path}`; index only |
| `GET /api/folders/art` | `?path=&vault=` | `{path, candidates: […], truncated, art?: {id?, film?}}` |
| `POST /api/folders/art` | `{path, id, vault?}` (`id: ""` clears) | same as GET |
| `GET /api/folders/survey` | `?path=&vault=` | `{path, files: [{id, name, dir, size, ext, depth}], folders: [{path, name, depth, files, total, bytes}]}` |
| `GET /api/folders/date-sort` | `?path=&grain=month\|year&deep=1&offset=<minutes east of UTC>&vault=` | `{path, grain, deep, offset, folders: [{path, label, year, month, files, bytes, exists}], moves: [{id, name, dir, size, to, as, modified}], bytes, settled, undated, emptied}` |
| `GET /api/folders/stats` | `?path=&vault=` | `{path, files, bytes, folders, stored_bytes, clouds, degraded, newest?}` |
| `GET /api/folders/duplicates` | `?path=&vault=` | `{path, scanned, videos, content, size, name, film}`; each set `{groups: [{key, files, bytes, waste, certain, title?}], files, extra, waste, partial, crowded}` |
| `POST /api/folders/zip` | `{path, vault?}` | `201 {url: "/zip/<token>/<name>.zip", name, files, folders, bytes, expires_at, expires_in}`; `409 NEEDS_CONVERSION` if anything under it is in the old format |

The survey, date-sort and duplicates endpoints are read-only planners: the
browser carries out their plans one item at a time over the move, delete and
folder endpoints.

#### GET /zip/{token}/{name}

No session; the ticket is the credential (12 hours of disuse, voided by a lock).
Streams the folder as a stored (not deflated) zip, gathering each file as it is
written, so its length is not known up front. An expired link answers an HTML
page saying so.

## Moving and deleting

#### POST /api/files/{id}/move

`{"dir": "/to", "name": "new.txt"}`. Index only. → `{"file", "path"}`.

#### POST /api/files/move

`{"moves": [{"id", "dir", "name"}, …]}` in one index write. → `{moved, missing,
refused}`.

#### DELETE /api/files/{id}

Erases every part, holding the request until done (5 minute deadline). →
`{"status": "deleted", "warnings": […]}`.

#### POST /api/files/delete · GET /api/files/erasing · DELETE

`{"ids": […], "batch": "<client token>"}` deletes a set in one index write →
`{status: "deleted" | "stopped", deleted, missing, warnings}`. Progress at
`GET /api/files/erasing?batch=` → `{running, done, total}`; `DELETE
/api/files/erasing?batch=` stops it → `{stopped}`.

#### POST /api/delete

A selection as one run: `{"targets": [{"id"} | {"path", "vault?"}], "detach":
true}`, or a single `id` / `path` (+ `vault`). With `detach` it answers `202
{"run"}` and continues with no request behind it (the browser always detaches);
otherwise it holds and answers `{status, deleted, folders, missing, warnings}`.
`404` when a single target does not exist.

#### GET /api/delete/runs · DELETE /api/delete/runs/{run}

`{"runs": [run]}` (waiting, running and recently finished). `DELETE` stops a run
between files or dismisses a finished one → `{"runs"}`.

#### POST /api/relocate

```json
{ "id": "…" , "path": "/photos", "targets": [{ "id": "…" }, { "path": "…", "vault": "…" }],
  "accounts": ["a", "b", "c"], "scheme": "3-of-5", "source": "replica",
  "preview": false, "vault": "", "detach": false }
```

Name the scope by `id`, `path` or `targets`. Moves only what has to move; a
different account count or `scheme` re-cuts the file; a file short a part is
rebuilt. `"source": "replica"` rebuilds every file in scope that has a replica
from it. `"preview": true` answers the plan without moving anything: `{vault?,
path, folder, accounts, source?, files: [{id, path, size, moves?, stay?, drop?,
stranded?, recode?, from_scheme?, to_scheme?, repair?, missing?, from_replica?,
no_replica?, bytes?}], truncated?, total, unchanged, moves, drops, bytes,
no_replica?, recoded, recode_bytes, outgoing?, incoming?, warnings?}`. Otherwise
`202 {"run"}` with `detach`, or held → `{path, folder, accounts, source?, total,
unchanged, relocated, partial, failed, parts_moved, parts_dropped, recoded,
no_replica?, deferred?, bytes, warnings?}`. Errors: `BAD_SCHEME`,
`BAD_SOURCE`, `409 RELOCATE_BUSY`.

#### GET /api/relocate/runs · DELETE /api/relocate/runs/{run}

As for delete runs.

## Checks and checksums

#### POST /api/files/{id}/check

Deep-checks one file (held): every shard of every chunk fetched and opened, the
hash, the contents. → `{id, vault?, path, name, size, kind, state, summary,
advice?, scheme, needed, total, good, faults?: [{part, provider_id,
provider_name, kind, chunks, first_chunk, detail?}], rebuilt, hash_ok, content?:
{format?, status, problems?, note?}, has_replica?, fix?}`.

#### GET /api/check/survey

`?path=&vault=` → `{path, kinds: [{kind, files, bytes}], files, bytes}`.

#### POST /api/check · GET · POST /api/check/stop

Job. `{"path", "vault?", "kinds?": […]}`. Progress `{path, file, files, bytes,
total, flagged}`; result `{vault?, path, kinds?, files, checked, bytes, sound,
repairable, unreadable, altered, damaged_at_source, incomplete, skipped,
fixable, flagged: [file check], truncated?, stopped?, stopped_by?, offline?}`.

#### POST /api/check/repair · GET · POST /api/check/repair/stop

Job. `{"ids?": […]}` (empty: every file the last check offered to fix). `409
NO_CHECK` without a finished check. Result `{repaired, failed, deferred, files:
[{id, path, fix, repaired, deferred?, reason?, warnings?}], stopped?}`.

#### GET /api/checksums/survey

`?path=&vault=` → `{path, files, bytes, with, with_bytes, without,
without_bytes, ledger: {path, stored, id?, size?, written_at?, entries,
written?}}`.

#### POST /api/checksums · GET

Job. `{"kind": "verify" | "compute", "path", "vault?", "rate?": bytes_per_s}`.
The status adds `paused` and `rate`. Result `{kind, path, files, checked, bytes,
matched, mismatched, computed, unreadable, skipped, no_checksum, findings: [{id,
path, size, state, detail, expected?, got?}], truncated?, stopped?, ledger?,
ledger_error?}`.

#### POST /api/checksums/pause · /resume · /rate · /stop

Control the running job; `/rate` takes `{"rate": bytes_per_s}` (0 unlimited).
Each answers the status.

#### POST /api/checksums/ledger

Writes `/checksums.gz` now. → the ledger object.

## Snapshots

A `target` is `{"account": "<id>"}` or `{"machine": "<id>", "dir?": "…"}`. Guide:
[folder tools](../guide/folder-tools.md).

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/snapshots/plan` | `?path=&vault=` | `{path, files, folders, bytes, unconverted?, min_part_size, cloud_max_part_size (256 MiB), cloud_default_part_size (64 MiB)}` |
| `GET /api/snapshots/stored` | `?account=` or `?machine=&dir=` | `{snapshots: […]}` read from the names there |
| `POST /api/snapshots/inspect` | `{target, name, password}` | `{version, id, path, name, taken_at, files, folders, bytes, tool?}`; `400 SNAPSHOT_PASSWORD` |
| `POST /api/snapshots/delete` | `{target, name}` | `{deleted: name}` |
| `POST /api/snapshots` | `{path, vault?, target, password, part_size?}` | Job (take) |
| `POST /api/snapshots/restore` | `{path, vault?, target, name, password, replace?}` | Job (restore) |
| `GET /api/snapshots` | | Job status. Progress `{kind, stage, file, files, path?, done, bytes, stored?, parts?, restored?, skipped?, kept?, failed?}`; result `{kind, path, where, name, parts?, files, folders, bytes, stored, part_size?, restored?, skipped?, kept?, failed?, results?, omitted?, stopped?, of?}` |
| `POST /api/snapshots/stop` | | Stopping a take erases what was written; a restore keeps what arrived |

## Replication

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/vault/replication` | | `{policy?: {accounts, data_shards, total_shards, bytes_per_day?, paused?, level?, level_at}, folders?: {path: setting}, replicated, pending, stale, pending_bytes, replica_bytes, current?, current_done?, current_size?, warnings?, left_alone?, eta_seconds?}` |
| `PUT /api/vault/replication` | `{accounts: [{id, replica_folder}], scheme, bytes_per_day, paused}` (empty `accounts` clears) | the status |
| `POST /api/replicate` | `{path, enabled}`: `true` replicate, `false` exclude, `null` follow the parent | the status |
| `DELETE /api/files/{id}/replica` | | `{warnings}`; `404 NO_REPLICA` |

## Automation

Guide: [maintenance](../guide/maintenance.md).

#### GET /api/automation

Without `path`: `{automations: [{vault?, folder, …policy, next_run_at?,
trouble?}], running, folder}`. With `?path=&vault=`: `{"automation": policy |
null}` (null, not 404, when there is none). A policy is `{enabled, cadence,
at?, weekday?, task?, action, shards?: {narrow?, max_repairs?, rebuild_limit?},
git?: {max_repos?, size_limit?, prune?}, created_at, updated_at?, last_run_at?,
history?: [run]}`.

#### POST /api/automation

```json
{ "vault": "", "path": "/archive", "enabled": true,
  "cadence": "hourly" | "daily" | "weekly", "at": "10:00", "weekday": 0,
  "task": "shards" | "git", "action": "check" | "rebalance" | "pull",
  "narrow": false, "max_repairs": 0, "rebuild_limit": 0,
  "max_repos": 0, "size_limit": 0, "prune": false }
```

`weekday` 0 is Sunday. `rebuild_limit` / `size_limit` are bytes: 0 is the
default (1 GiB / 2 GiB), negative is no ceiling. Creating and editing are the same
call; an edit keeps history and last-run time. → `{"automation"}`.

#### DELETE /api/automation

`?path=&vault=`. → `{"removed": path}`.

#### POST /api/automation/run

`{"vault?", "path"}`. Runs now, due or not (relocation's long deadline). →
`{"run": {folder, vault?, trigger, task, action, started_at, finished_at,
accounts, offline?, shards?: {files, checked, whole, short, at_risk, repaired,
parts_written, bytes, failed, deferred}, git?: {repos, checked, current, updated,
gone?, pruned?, commits?, failed, deferred, bytes}, warnings?, error?}}`. `409
AUTOMATION_BUSY`.

## Repositories

Guide: [repositories](../guide/repositories.md). `{id}` is the bundle's file ID.

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/git` | `?path=&vault=` | `{repos, folder, available}` (`available`: is git installed) |
| `POST /api/git/track` | `{vault?, path, url, accounts?, scheme?}` (long deadline) | `{repo, warnings}` |
| `POST /api/git/{id}/refresh` | `?vault=` | `{repo, updated}`; the repo carries its **new** ID when it moved |
| `POST /api/git/{id}/rebuild` | `{vault?, accounts, scheme?}` (`accounts` required) | `{repo, warnings}`, new ID |
| `DELETE /api/git/{id}` | `?vault=` | `{untracked: id}`; the bundle stays |

## Film details

Guide: [films](../guide/films.md). The only feature that talks to a third party
(TMDB), so it is switched on per folder.

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/movies` | | `{has_key, folders}` |
| `POST /api/movies/key` | `{key}` (checked first; `""` clears) | `{has_key}`; `502 BAD_MOVIE_KEY` |
| `POST /api/movies/lookup` | `{path, enabled}`, `?vault=` | `{path, lookup: {enabled, source?}}` |
| `POST /api/movies/scan` | `{path, refresh}`, `?vault=` (held) | `{path, considered, matched, skipped, artwork, unmatched?, warnings?, stopped?}` |
| `POST /api/movies/scanning` | same | Job (202 new, 200 joined, `409 MOVIE_SCAN_BUSY`); progress `{path, vault?, done, total, matched, current?}` |
| `GET · DELETE /api/movies/scanning` | | Status · stop (what matched is kept) |
| `GET /api/files/{id}/movie` | | `{movie, guess, lookup}` |
| `POST /api/files/{id}/movie` | `{query?, year?, tmdb_id?}` | `{movie, guess, artwork, warnings}` (or `{movie: null, guess}` when nothing matched) |
| `DELETE /api/files/{id}/movie` | | `{status: "forgotten"}`; the poster stays as the thumbnail |
| `GET /api/files/{id}/movie/candidates` | `?q=` | `{guess, candidates}` |

Errors: `412 NO_MOVIE_KEY`, `403 MOVIE_LOOKUP_OFF`. A film is `{tmdb_id,
imdb_id?, title, original_title?, year?, released?, tagline?, overview?,
runtime?, genres?, rating?, votes?, directors?, cast?, poster_path?, query?,
file?, manual?, matched_at}`.

## Assistant

Sandy, the archivist assistant, runs on a model server you point it at. Guide:
[Sandy](../guide/sandy.md).

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/assistant` | | `{configured, url, model, has_key, context_reported, context_tokens, context_window}` |
| `POST /api/assistant/settings` | `{url, model, key?, context_tokens?}` (`url: ""` clears; `key` absent keeps, `""` clears) | same as GET |
| `POST /api/assistant/ask` | `{messages: [{role, content}]}`, last one the question; `?vault=` | `{text, steps?: [{tool, arguments?, error?}], context?: {tokens, window?}}` |

Errors: `412 NO_ASSISTANT`, `502 NO_SUCH_MODEL`, `NO_TOOL_SUPPORT`,
`ASSISTANT_LOOPED`, `ASSISTANT_FAILED`.

## Machines (SFTP sources)

Machines you have an SSH login on, to import your own files from, export files
to, and keep snapshots on. Not accounts: they never hold shards.

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/remote` | | `{sources: [{id, name, host, port?, user, root, host_key?, added_at}]}` |
| `POST /api/remote` | `{name, host, port?, user, root, private_key?, passphrase?, password?, host_key?, relearn_host_key?}` (`private_key` may be a `/api/ssh/keypair` handle) | `201 {source}`; `KEY_EXPIRED` |
| `PATCH /api/remote/{id}` | same | `{source}` |
| `DELETE /api/remote/{id}` | | `{removed: true}` |
| `GET /api/remote/{id}/files` | `?path=` | `{path, parent, at_root?, entries: [{name, size, dir?, modified, symlink?, unreachable?, reason?}], truncated?}` |
| `POST /api/remote/{id}/import` | `{vault?, paths, dest, accounts?, scheme?, overwrite?, detach?}` | `202 {run}` with `detach`; else `201` (something imported) or `200` `{results: [{path, dest, ok?, skipped?, retimed?, reason?, error?, warnings?}], imported, skipped, failed, retimed?, omitted?}` |
| `GET /api/remote/{id}/import` | | `{imports: [run]}` |
| `DELETE /api/remote/{id}/import/{run}` | | `{imports}` |
| `POST /api/remote/{id}/export` | `{vault?, paths, dest, overwrite?, detach?}` | as import, with `{results: [{path, dest, size, ok?, skipped?, reason?, error?}], exported, skipped, failed, bytes, omitted?}` |
| `GET /api/remote/{id}/export` · `DELETE …/export/{run}` | | `{exports}` |

`409 TRANSFER_BUSY` when a transfer cannot start. Exports write files in the
clear on the machine.

## Statistics

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/reads` | `?window=today\|month\|year\|all` (default `today`) | `{reads: [{provider_id, name, kind, connected, fetches, wins, late, aborted, failures, bytes, average_ms, fastest_ms, slowest_ms, last_error?, last_error_at, last_answer_at, trend?}]}`: which account wins the race each read runs |
| `POST /api/reads/forget` | `?window=` | `{reads}` after erasing the history |
| `GET /api/transfers` | `?window=` | `{transfers}`: per-account up/down speeds |
| `GET /api/throughput` | `?window=` | `{throughput}`: per-kind open/store speeds |

## This machine

How Sand Vault is sized to the machine it runs on; kept in `machine.json` beside
the vault. Design: [data pipeline](../design/data-pipeline.md); knobs:
[configuration](configuration.md).

| Endpoint | Request | Response |
|---|---|---|
| `GET /api/machine` | | `{profile, machine, detected, defaults, overrides, effective, pinned, footprint, knobs: [{name, label, hint, unit, min, max, step?, restart?}], profiles: [{id, label, machine?}]}` |
| `PUT /api/machine` | `{profile: auto\|pi5\|workstation\|custom, machine?: {cpus, memory_bytes}, overrides?: {knob: value}}` | the view; applied at once. `409 PROFILE_PINNED`, `400 OUT_OF_RANGE` |
| `DELETE /api/machine` | | Back to automatic, no overrides |

`defaults` and `effective` carry `chunk_size, upload_window, read_ahead,
chunk_cache_bytes, compress_level, compress_threads, transfer_window,
erase_window, conns_per_host, rebuild_limit, bundle_limit`.

## Sand TV

One folder of films opened with a four-character code. Guide: [Sand
TV](../guide/sand-tv.md); design: [Sand TV](../design/sand-tv.md).

### Owner side (session)

#### POST /api/tv/codes

`{"path": "/Films", "vault?": "", "label?": "", "minutes?": 240, "forever?":
false}`. `minutes` defaults to 240 and may be 5 minutes to 365 days; `forever:
true` (with `minutes` 0) lasts until revoked or the server restarts. Label
defaults to the folder name (80 characters at most). → `201 {code, vault?, path,
label, created_at, expires_at, forever?, devices}`. Errors: `404 NOT_FOUND`
(folder), `409 TOO_MANY_CODES` (32 live).

#### GET /api/tv/codes

→ `{codes: [grant], addresses: ["http://192.168.1.10:8123", …], discovery_port?,
lan}`. `lan` is false on a loopback bind, where no TV can connect.

#### DELETE /api/tv/codes/{code}

Revokes the code, unpairs every TV that used it, and stops any film playing
under it. → `{"status": "revoked"}`.

### TV side

Pairing is public; everything after it sends `Authorization: Bearer <token>`
and answers `Cache-Control: private, no-store`. A token reaches only its grant's
folder, the videos in it and the folders below it that hold videos; every path
is relative to that folder, and a file ID from elsewhere is `404`. A grant
survives a lock of the vault (reads then fail `401 LOCKED`).

#### POST /api/tv/pair

`{"code": "ABCD"}`. → `201 {token, label, expires_at}` (or `forever: true` and
no `expires_at`). The token is 256 bits; the server keeps only its SHA-256.
Errors: `401 BAD_CODE`, `409 TOO_MANY_DEVICES` (8 per code), `429 THROTTLED`
with `Retry-After` after 5 wrong codes from one address or 30 from anywhere in
10 minutes (the right code is refused too while throttled; the client address is
the connection's, not `X-Forwarded-For`).

#### GET /api/tv/session

→ `{label, expires_at, forever?, locked}`. Does not count as vault use.

#### POST /api/tv/logout

Unpairs this TV. → `{"status": "unpaired"}`.

#### GET /api/tv/library

`?path=/sub&all=1`. → `{label, path, parent?, folders: [{name, path, count,
art?}], items: [{id, name, folder, size, modified_at, title?, year?, rating?,
poster}]}`. `all=1` lists every video at or below the folder, flat.

#### GET /api/tv/items/{id}

→ the item plus `mime?` and `movie?` (film details).

#### GET /api/tv/items/{id}/poster · GET /api/tv/folders/art?path=

`image/jpeg`, or `404 NO_THUMBNAIL`.

#### POST /api/tv/items/{id}/play

→ `201 {url: "/stream/…", name, mime?, expires_at, expires_in, subtitles: [{url,
label, language?, mime}]}`: a stream link bound to the code, and links for the
sidecar subtitle files beside the video.

Errors throughout: `401 TV_UNPAIRED` (pair again), `401 LOCKED` (unlock the
vault).

#### GET /tv, /tv/…

The TV app's page (`/tv` redirects to `/tv/`), embedded from
`internal/server/tvui`, with a strict `Content-Security-Policy`. The Fire TV app
is a WebView around it; any browser on a TV can open it too.

#### UDP discovery

Not HTTP, but part of the same surface: `sand serve` listens on UDP 8124
(`--tv-discovery-port`) for the exact datagram `SAND-TV-DISCOVER/1` and answers
`{"service": "sand-vault", "name": "<hostname>", "port": 8123, "version":
"…"}`. It answers only queries from private, loopback, link-local or CGNAT
addresses, is not started on a loopback bind, and authorizes nothing.

## WebDAV

With `sand serve --webdav`, the vault is a WebDAV share at `/dav/`
(`--webdav-path`), and each sub vault has its own share at `/sub/<name>/`
(`--webdav-sub-path`), `<name>` being its label (case ignored) or ID. The two
prefixes may not nest.

- **Auth:** HTTP Basic, realm `SAND Vault`; the username is ignored and the
  password is the vault's (or that sub vault's). A verified password is
  remembered for a minute under a per-process HMAC, so a playing film does not
  pay Argon2id per request. A correct password against a locked vault unlocks
  it. A sub vault share answers `503` while the vault around it is locked, and an
  unknown sub vault name looks like a wrong password. `/dav` never shows sub
  vaults.
- **Methods:** `GET` (ranged) and `HEAD`, `PUT` (streamed in; replaces),
  `PROPFIND`, `MKCOL` (one level), `DELETE` (recursive for a folder), `MOVE` and
  `COPY`, `LOCK`/`UNLOCK` (in-memory locks). A `PUT` with `Content-Range` is
  refused with `400`.
- Requests count as vault activity, and a request still in flight holds off the
  auto-lock.
- On plain HTTP the password is sent with every request; use TLS.

Guide: [streaming](../guide/streaming.md); design: [server](../design/server.md).

## Standalone mode

Public, no vault involved: the browser equivalent of `sand archive` and `sand
restore`.

#### POST /api/archive

*Multipart*: `password`, `files[]` (up to 500 MiB in memory). Answers
`application/zip` named `sand-archives.zip`, holding the three part zips.
Errors: `MISSING_PASSWORD`, `MISSING_FILE`, `DUPLICATE_NAME` (two inputs with
one name), `READ_ERROR`, `ARCHIVE_ERROR`.

#### POST /api/restore

*Multipart*: `password`, `parts[]` (two or three `.sand` parts). Answers the
restored file as `application/octet-stream` with its name in
`Content-Disposition`. Errors: `MISSING_PASSWORD`, `INVALID_PARTS`,
`READ_ERROR`, `WRONG_PASSWORD`, `MISMATCHED_PARTS`, `CORRUPT_FILE`,
`RESTORE_ERROR`.

## Frontend

Any other path serves the embedded web UI. A path with no file extension
renders the app (`index.html`), for client-side routing. Files under `/assets/`
are fingerprinted and sent `Cache-Control: public, max-age=31536000, immutable`;
everything else is `no-cache` with a content `ETag`. An unknown `/api/…` path is
`404 NOT_FOUND`, never the app.
