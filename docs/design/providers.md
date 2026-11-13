# Providers

The storage layer under the vault (`internal/provider`): the interface every
backend implements, the backends there are, the self-describing specs the
connect form is generated from, browser sign-in, how full an account is, and
the background check that each account is still answering. Connecting accounts
as a user is in [guide/accounts.md](../guide/accounts.md). Adding a backend is
in [cloud-backends.md](cloud-backends.md).

## The interface

```go
type Provider interface {
    Config() Config
    Put(ctx, key string, data []byte) error     // overwrites
    Get(ctx, key string) ([]byte, error)        // ErrNotFound if absent
    Stat(ctx, key string) (ObjectInfo, error)   // presence and size, no download
    Delete(ctx, key string) error               // idempotent
    List(ctx, prefix string) ([]ObjectInfo, error)
    Ping(ctx) error                             // credentials + reachability
}
```

The contract is stricter than it reads, because of what the vault does with
the answers. `ErrNotFound` means the object is **definitely** not there:
health checks and automated repair act on it by rebuilding. `Delete` returns
nil only for an object that is gone, never for a refusal. `List` is complete or
it fails, because the orphan scan deletes what the index doesn't name and
recovery rebuilds the index from listings. The full rules are in
[cloud-backends.md](cloud-backends.md#the-recipe).

Optional capabilities, which the layers above check for rather than assume:

| Interface | For backends that | Examples |
|---|---|---|
| `UsageReporter` | can report quota cheaply. It is called on every ping | most backends: Drive, OneDrive, Dropbox, Box, Drime, Filen, Internxt, SFTP (`statvfs@openssh.com`), local folders (the drive they sit on). S3, Zoho and the Proton CLI answer it with their last count |
| `UsageMeasurer` | can only be counted, by listing | S3, the Proton CLI, Zoho WorkDrive |
| `Versioner` | keep every version of an object, so erasing needs a version ID | S3 buckets with versioning, B2's "keep all versions" (`sand vault prune`) |
| `PingBudgeter` | need longer than the 20-second default to answer a ping | the Proton CLI (2 minutes) |
| `Identifier` | can name the signed-in account, so a new account labels itself | the OAuth backends, Filen, Internxt, the Proton CLI |
| `CredentialRotator` | change stored credentials as they are used | the OAuth backends (Box and Microsoft rotate refresh tokens), Filen and Internxt (session), the Proton CLI (rotated session), SFTP (learned host key) |
| `io.Closer` | hold a connection open | SFTP. The vault closes it on lock, disconnect and cache reset |

Each account can be opened in one of two roles (`provider/replica.go`):
**parts**, at the folder its config names, and **replicas**, at its
`ReplicaFolder`. Only the folder tells a replica object from a part.

## Backends

| Kind | Covers | Transport |
|---|---|---|
| `local` | Any directory: external disk, NAS mount | Filesystem, atomic writes, every call under a deadline |
| `s3` | Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO, Google Cloud Storage (HMAC), and more | SigV4, hand-rolled on the standard library |
| `webdav` | Nextcloud, ownCloud, pCloud, Koofr, Fastmail, rclone, and more | PUT/GET/HEAD/DELETE/MKCOL/PROPFIND |
| `sftp` | Any machine with an SSH login: VPS, NAS, Hetzner Storage Box, rsync.net | SSH via `golang.org/x/crypto/ssh` + `github.com/pkg/sftp` (see [sftp.md](sftp.md)) |
| `gdrive` | Google Drive | Drive v3 REST, OAuth with the `drive.file` scope |
| `onedrive` | OneDrive, personal or work | Microsoft Graph, chunked upload sessions |
| `dropbox` | Dropbox | API v2, OAuth refresh grant |
| `box` | Box | API 2.0, OAuth with rotating refresh tokens |
| `zoho` | Zoho WorkDrive | WorkDrive API v1, OAuth sent to the account's region |
| `drime` | Drime | REST API v1 with a pasted token, S3-style multipart past 5 MiB |
| `filen` | Filen | Its end-to-end encrypted client protocol, reimplemented on the standard library |
| `internxt` | Internxt Drive | Same: BIP-39 seed from the recovery phrase, per-file AES-CTR keys |
| `protoncli` | Proton Drive, via Proton's own `proton-drive` CLI | One process per operation. Session held in the vault |
| `icloud` | iCloud Drive, via the folder macOS or iCloud for Windows syncs | Filesystem, plus `brctl` for an evicted part |
| `proton`, `mega`, `jottacloud`, `synccom`, `tresorit`, `icedrive` | The folder each service's desktop app syncs | Filesystem (the `local` backend) |

**No cloud SDKs.** Everything is built on `net/http` and the standard library,
plus a few `golang.org/x` modules and `github.com/pkg/sftp`. `CGO_ENABLED=0`
still produces a fully static binary. See `go.mod` for the full list.

**Synced folders.** Some services publish no API a third party can use. Their
backend is the local one pointed at the folder the desktop client keeps in step.
That doesn't weaken the threat model, because parts are encrypted long before
the client sees them. But such an account is only as live as the machine running
the client, and its `Ping` refuses a folder whose parent doesn't exist, since a
folder no client syncs would accept every part and upload none. All six live in
one table in `syncfolder.go`. iCloud Drive has its own file because macOS evicts
synced files to placeholders (see
[cloud-backends.md](cloud-backends.md#how-icloud-drive-handles-eviction)).

**Proton Drive has two backends.** The synced folder works on a laptop. On a
headless server nothing runs the desktop app, and the folder backend can't tell:
its `Put` returns as soon as the file is on local disk. `protoncli` drives
Proton's own CLI instead, confirming each part reached Proton
(`internal/provider/protoncli.go`, and
[cloud-backends.md](cloud-backends.md#tier-4--driving-the-services-own-client--done-for-proton)).

**Local accounts belong to one machine.** A spec marked `Local` (local folders,
synced folders, iCloud, the Proton CLI) is reached through the machine the vault
runs on. `Config.Host` records which machine connected it. Another machine
opening the same vault treats such an account as living elsewhere: it doesn't
ping it, and doesn't count its parts as readable.

### Local folders run every call under a deadline

A read or write on a filesystem that has stopped answering (a failing drive, a
pulled USB disk, an NFS or SMB mount whose server is gone) doesn't fail. It
doesn't return, because the kernel holds the thread until the I/O completes,
and nothing in `os` honours a context. So `local.go` makes each call on its own
goroutine (`onDisk`) and waits for the answer or the deadline. Past the deadline
the caller gets `ErrDiskNotAnswering`, naming the folder and what to do. The
goroutine stays parked until the kernel releases it, but it is one goroutine
rather than the whole server. Without this, one such account stalled the health
sweep, every transfer that touched it, and finally process exit. A cancellation
passes through as itself, because "not answering" is a finding about the disk
and a user who pressed stop hasn't found anything. Synced-folder and iCloud
accounts share these methods.

**Disk health.** The service runs with `PrivateDevices=true`, so it can't read
SMART counters itself. A root-run one-shot, `sand smart-scan` (installed as a
systemd timer by the quick-start script or `scripts/enable-disk-health.sh`),
writes a report to the data directory. `disksmart.go` joins it to each local
account's folder through the mount table, so the health panel can show
"32 sectors pending reallocation" next to the account sitting on them.

## Self-describing configuration

Each backend registers a `Spec` in its `init`:

```go
Register(Spec{
    Kind:  KindS3,
    Label: "S3-compatible storage",
    Fields: []FieldSpec{
        {Key: "bucket", Label: "Bucket", Required: true},
        {Key: "secret_access_key", Label: "Secret access key", Secret: true, Required: true},
        …
    },
    Presets: []Preset{…},   // one-click pre-fills for well-known services
    Covers:  []Service{…},  // the catalogue of services this backend reaches
}, newS3Provider)
```

`GET /api/providers/specs` serves these, and both the web UI's connect form and
`sand remote kinds` / `sand remote add` are generated from them. **Adding a
backend needs no frontend change.** The field flags drive the form:

| Flag | Effect |
|---|---|
| `Secret` | Redacted everywhere a config is returned |
| `Required` | Checked by `provider.New` before the factory runs |
| `Directory` | Gets the folder picker |
| `Multiline` | Gets a textarea (an `<input>` silently drops the line breaks from a pasted PEM key) |
| `SSHKey` | Offers to generate a key pair, keeping the private half on the server |
| `Advanced` | Hidden behind a disclosure, for fields a sign-in fills in |

`Order` sorts the connect dialog: sign-in backends in the 10s, credential forms
in the 20s, path-based and synced-folder backends in the 30s, the local folder
at 40.

### The folder picker

`GET /api/system/folders?path=` returns the subfolders of one folder, its
parent, and the roots worth jumping to (home, `/media`, `/run/media`, `/mnt`,
`/srv`, or the drive letters on Windows). Symlinked folders are followed,
because a synced cloud folder is often one.

The endpoint browses the machine the vault runs on, not the vault, so it is
deliberately narrow: folder names only, never files or contents, behind the
same session that could already point an account anywhere by typing a path. A
path that doesn't exist yet is a legitimate answer, since connecting creates it,
so the listing climbs to the nearest existing ancestor and reports both. The
server tells the browser its path separator, because the phone in your hand and
the machine holding the folder are often different kinds of computer.

Readability, not existence, decides where the picker opens. Under the service
unit `$HOME` is `/home` and `ProtectHome=yes` denies it, so the picker opens at
the first candidate that can be listed (home, then the vault's directory, then
the mount roots) and leaves out shortcuts that can't be opened. A listing that
fails is still a `200` carrying the parent, the shortcuts and the reason in
`error`, so a picker that lands on an unreadable folder can still leave it.

### Google Drive has no paths

Drive isn't path-addressed, so the object key is written into the file's
`appProperties` (`sandKey`) and looked up by query, with a per-provider ID cache
so repeated reads skip the lookup. The `drive.file` scope means the vault can
only ever see files it created.

## Signing in instead of pasting tokens

A backend that speaks OAuth adds an `OAuthSpec` to its registration: the
authorize and token endpoints, scopes, whether to use PKCE, where the tokens are
written, and the environment variables that can supply app credentials
(`SAND_GOOGLE_CLIENT_ID`, `SAND_MICROSOFT_CLIENT_ID`, `SAND_DROPBOX_APP_KEY`,
`SAND_BOX_CLIENT_ID`, `SAND_ZOHO_CLIENT_ID` and their secrets). A new OAuth
backend is one struct literal plus the six methods (`internal/provider/oauth.go`).

```
browser                    server                        provider
   │  POST oauth/start        │                              │
   │─────────────────────────►│  mint state + PKCE verifier  │
   │  ◄── auth_url, flow_id ──│  held in memory, 15 min      │
   │                          │                              │
   │  window.open(auth_url) ─────────────────────────────────►│
   │                          │  GET oauth/callback?code&state (no cookie)
   │                          │◄─────────────────────────────│
   │                          │  exchange code ─────────────►│
   │                          │  ask the account its name ──►│
   │  ◄── poll oauth/{flow} ──│  tokens held against the flow│
   │  POST oauth/complete ───►│  AddProvider → encrypted vault
```

- **The callback authenticates on `state` alone.** The redirect is a cross-site
  navigation, which the `SameSite=Strict` session cookie deliberately doesn't
  survive. The 256-bit state is single-use, so a replayed redirect exchanges
  nothing. Status and complete, the calls that turn a flow into an account, are
  bound to the session that started it.
- **Tokens never reach the browser.** Status reports progress and which account
  signed in. Credentials go from the token endpoint straight into the vault.
- **When the redirect can't come back** (the server isn't reachable at the
  registered redirect URL), `POST /api/providers/oauth/exchange` finishes the
  flow from a pasted redirect URL. `SAND_OAUTH_REDIRECT` overrides the redirect.
- **Reauthorizing** (`POST /api/providers/oauth/reauthorize`) spends a finished
  sign-in on an existing account: new credentials, same ID, same parts. It
  answers a revoked consent without disconnecting.
- **Regions.** Zoho runs a separate deployment per region, so its spec carries
  `RegionField` and `Regions`. The browser offers the list, `ForRegion` fills the
  hostnames, and only a listed region is accepted, because the value is spliced
  into the host the client secret is posted to.
- **Rotation.** For Box and Microsoft, which retire a refresh token as it is
  spent, the vault installs a `CredentialRotator` sink on every live provider. A
  rotated token is written back asynchronously, because the refresh may happen
  inside a call that already holds the vault lock.

Proton's CLI can't redirect anywhere: it prints a link and blocks. Its spec
declares a `SignInLink` instead (`POST /api/providers/proton/signin`). The
browser polls the same flow store, and the link can be followed on another
device, which is how a machine with no browser connects an account.

## How full an account is

`Usage` carries `used`, `total`, and `free` where the backend measured it.
`free` exists for local folders: a filesystem keeps a reserve only root may
spend, and a quota can cut an account's share, so what can actually be written
isn't `total − used`. `Usage.Remaining()` prefers the measured figure.

Two flags say where the figures came from. `Measured` marks a `used` that was
counted rather than reported. `Declared` marks a `total` that was typed rather
than reported. `Usage.UsedKnown()` separates an empty account from one nobody
has looked at, which both read as zero.

- **Buckets.** S3 has no quota call. `UsageMeasurer.MeasureUsage` counts the
  whole bucket by listing it (not just the vault's prefix, since everything else
  is the part the index can't supply). That costs a request per thousand objects
  and is billed at some providers, so nothing runs it on a timer: only
  `POST /api/providers/{id}/measure` and `sand remote measure` do. The backend
  then keeps the count for its cheap `Usage` on later pings.
- **Declared capacity.** `Config.Capacity` is the denominator, typed by whoever
  pays: the cap in the provider's console, or how much this vault may fill. It
  is set through the same `PATCH` that renames an account (parsed by
  `provider.ParseSize`), never reaches the backend, and enforces nothing.
  `withDeclaredCapacity` applies it only once something has been counted, and
  never over a backend's own reported quota.
- **Local folders** answer from `statfs` (`GetDiskFreeSpaceEx` on Windows) for
  the drive they sit on, climbing to the nearest existing parent. The drive, not
  the folder, is deliberate: what the vault put there is already known from the
  index, and the figure the index can't supply is everything else on the disk.
  Nothing walks the tree.
- **A quota** (`Config.Quota`) is a line someone draws through an account: how
  much of it the vault may fill, as opposed to how big it is. It covers the
  backends that can neither report nor be listed, and an account may carry both.

`spaceFor(usage, quota, stored)` (`internal/vault/space.go`) folds all of this
into one `Space`: `free`, `used`, `total`, and the `source` that answered
(`reported`, `declared`, `quota`, or empty when nobody can say). Where two
sources have an opinion, whichever leaves less room wins. `Over` is how far past
its quota an account is, and only a quota produces it. `Space` rides on every
provider status, so the sidebar, the upload picker, the stats panel and
`sand remote list` all read the same figure.

**A quota is a warning, not a wall.** `commitUpload` calls `quotaWarnings` once
the file is safely in the index and reports the accounts this file took past
their line, per file in `results[].warnings`. Refusing a shard mid-scatter is
the one thing that must not happen: the shards of a file are placed together,
and dropping one leaves the file on fewer accounts than its code promises, a
quiet loss of durability for a line nobody else can see. Crossing is reported
once, by the file that crossed; being over is a state the account card carries
until resolved. Before any of this, the upload picker checks each chosen cloud's
`space.free` against the share it is about to take (about 1/k of the file),
where changing the choice is still free.

**`ProviderStats`** (`GET /api/providers/stats/{id}`) takes one account apart,
reading only the index, so the panel costs one ping:

| | |
|---|---|
| capacity | `Usage` against what the vault stored: its parts, everything else on the account, free, reserved. Or, where nothing is reported, stored against the quota |
| `Space` | how much more fits, and which source said so |
| `Files` / `Sole` | files with a shard here, and those that couldn't be rebuilt without it (counted the way the disconnect guard counts) |
| `Kinds` / `Folders` / `Largest` / `Months` | what the shards belong to, weighed by what each file left *here* |
| `SubVaults` | one line: parts and bytes |

The breakdowns come from the main vault's index alone. A sub vault reveals only
its name and weight to the main password, and an open one would otherwise start
naming its folders in a panel about an account. Its parts still count toward
capacity and `Sole`, so a locked sub vault doesn't make an account look emptier.

## Whether the account is still there

The failure nobody notices is the one with nobody looking: a revoked refresh
token, a rotated access key, a NAS off since a power cut. Nothing in the vault
looks wrong, because a file reads from any k of its n shards and the accounts
still answering carry it, until the second one goes.

`Vault.CheckClouds` is the sweep: one `Ping` per account, all at once, 20
seconds each (`healthPingTimeout`), and nothing else. No listing, usage call or
transfer. Whether particular shards are still there is the job of the folder
maintenance sweep; this only asks whether each account answers.

- **Results live in memory** (`Vault.healthSeen`) and are dropped on lock. A
  ping result expires within the hour, so sealing it into the vault file would
  be a disk write for a fact with an hour to live. `CloudHealth` builds the
  report against the accounts as they are *now*: one connected since the check
  is `Checked: false`, and a disconnected one stops counting immediately.
  `FailingSince` carries across checks, because "unreachable for three days" is
  different news from "unreachable".
- **Every ping feeds it.** The accounts listing files its results through
  `recordSweep`, which also resets the clock for the next scheduled check,
  because it is a ping of every account. The Test button files one result
  through `rememberOne` and leaves the clock alone.
- **The listing doesn't wait the full twenty seconds; the ping does.** The
  listing answers after `statusWait` (8 seconds) with whoever has replied, and
  reports the rest as still being waited on, not as unreachable. One silent
  cloud no longer sets the pace of the whole drawer. The remaining pings run on,
  detached from the request, and the sweep is recorded when the last answers
  (and not at all if the vault locked meanwhile).
- **A backend that needs longer says so.** `provider.PingBudgeter` declares its
  own deadline. Proton's CLI restores a session and decrypts its way down a path
  on every command, which on a small box passes twenty seconds with nothing
  wrong. `vault.pingBudget` takes the longer figure, and every ping the vault
  starts goes through it.

**The schedule** is `HealthCheckMinutes` / `HealthCheckOff` in the vault file's
clear section, beside the placement policy. It is stored negated so that a vault
written before the feature existed reads as switched on. The default is hourly,
bounded to between 5 minutes and 7 days: below that a cloud's occasional missed
answer makes the panel flicker, and above it "checked recently" stops meaning
anything. Configure it with `POST /api/providers/health/schedule` or `sand
remote health`.

`Server.healthLoop` ticks once a minute and contacts nothing unless
`HealthCheckDue` says so. It never counts as activity for the idle lock: an
hourly ping that renewed the idle timer would mean no machine running SAND Vault
ever auto-locked again. It logs only changes, one line when an account stops
answering and one when it comes back, because a line an hour saying all is well
would bury the one that matters. `GET /api/providers/health` reads the result
from memory without contacting anyone, which is why the accounts panel can poll
it.
