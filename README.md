# SAND Vault — Secure Archival Network Distribution

**A file browser over storage you don't fully trust.**

Connect the cloud accounts you already have. SAND Vault compresses every file
you add, splits it into three parts, encrypts them, and puts each part on a
**different** account. Any two parts rebuild the original; any one on its own
is noise. Open a file in the browser and SAND fetches the parts back,
reassembles them in memory, and shows you the file.

No single provider ever holds your data — only a fragment that means nothing
without a fragment held by someone else, plus a key that never leaves your
machine.

Ships as a **single static Go binary** with a CLI and an embedded web UI.

```
┌ SAND VAULT ───────────────────────────────────────────────────────────┐
│ CONNECTED CLOUDS  │  ▣ / photos                        [+ Folder] [↑] │
│ ● drive-personal  ├───────────────────────────────────────────────────┤
│   gdrive · 41 pts │  NAME              SIZE      MODIFIED      PARTS  │
│ ● r2-cold         │  📁 2024                                          │
│   s3 · 41 pts     │  🖼 hike.jpg       4.2 MB    Aug 3 10:22   ①②③   │
│ ● nas-backup      │  📕 lease.pdf      880 KB    Jul 29 16:04  ①②③   │
│   webdav · 41 pts │  🎬 clip.mp4       88 MB     Jul 12 09:41  ①②③   │
└───────────────────┴───────────────────────────────────────────────────┘
   each ①②③ is coloured by the account holding that part
```

---

## Quick start on Linux (Ubuntu / Raspberry Pi)

Install SAND Vault as a hardened **systemd service** with one command:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh | sudo bash
```

(or, from a checkout: `sudo ./scripts/quickstart.sh`)

It installs Node 22 and Go if needed (both build-time only), creates a dedicated
`sand` system user, compiles the web client and the static server binary, and
runs it under systemd on `http://<host>:8123`, reachable from your network.

**Or skip the build entirely** and install the prebuilt binary from the latest
[release](https://github.com/chinmay28/sand-vault/releases) — no Node, no Go, no
source tree, seconds instead of minutes on a Raspberry Pi:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh \
  | sudo SAND_INSTALL=release bash
```

The download's checksum is verified before anything is swapped in, and
`SAND_RELEASE=v2.0.42` pins a specific release instead of the latest. Releases
publish **`linux/amd64`** and **`linux/arm64`**; anything else builds from
source (the default), which works everywhere. Both modes install the same thing
— one static binary with the web client embedded, under the same unit and the
same data directory — so you can switch between them by re-running with a
different `SAND_INSTALL`.

**Re-run it any time to upgrade — installs and upgrades are non-disruptive and
never lose data:**

- The vault lives at a stable path **outside** the source tree
  (`/var/lib/sand/`), so rebuilding or pulling can't clobber it.
- Each upgrade quiesces the service, **snapshots the vault** to a timestamped
  backup, then swaps code in. The new build compiles while the old version
  keeps serving, so a failed build leaves the running app untouched.
- After restart it polls `/api/health`; if the new version is unhealthy it
  **rolls back** — to the previous commit when it built from source, to the
  previous binary when it installed a release — and **restores the pre-upgrade
  vault snapshot**.

Override defaults with env vars (`PORT`, `HOST`, `SAND_WEBDAV`, `SAND_INSTALL`,
`SAND_REF`, `SAND_RELEASE`, `SAND_DATA_DIR`, `SAND_PREFIX`, `SAND_USER`,
`SAND_LOCAL_PATHS`, …). Manage it with `systemctl status sand` and
`journalctl -u sand -f`.

`PORT`, `HOST` and `SAND_WEBDAV` are **remembered**: on an upgrade, leaving one
unset keeps whatever the service is already running with rather than resetting
it, so re-running the script to pick up a new version can't quietly move a
loopback-only install back onto every interface.

> **`HOST` defaults to `0.0.0.0`** — the service is reachable from your network
> as soon as it is installed. Know what that exposes: this server is the one
> component that ever holds plaintext, it takes your vault password over the
> wire, and `/api/vault/unlock` answers anyone who can reach the port. On plain
> HTTP all of it is in the clear. Put TLS in front (Tailscale Serve, or
> `scripts/nginx-sand.conf`), or set `HOST=127.0.0.1` to keep it on loopback.

---

## Quick start from source

```bash
# 1. Build (requires Go 1.25+ and Node.js 18+)
make build

# 2. Create your vault — this password protects the file index and your
#    cloud credentials. There is no recovery if you lose it.
./sand vault init

# 3. Connect at least two accounts (three for full redundancy)
./sand remote add local  --name usb-drive --set path=/media/usb/sand
./sand remote add s3     --name r2-cold \
    --set bucket=sand-shards \
    --set endpoint=https://<account>.r2.cloudflarestorage.com \
    --set access_key_id=… --set secret_access_key=…
./sand remote add webdav --name nextcloud \
    --set url=https://cloud.example.com/remote.php/dav/files/alice \
    --set username=alice --set password=…

# 4. Store and retrieve
./sand put ~/Documents/passport.pdf          # add --accounts to pick the clouds
./sand ls
#   passport.pdf  2.1 MB  Aug 12 09:14  p1:usb-drive p2:r2-cold p3:nextcloud
./sand get /passport.pdf -o ./restored.pdf

# 5. Or use the browser
./sand serve --port 8123
# → http://127.0.0.1:8123
```

Run `./sand remote kinds` to see every backend and the settings it needs.

Google Drive, OneDrive, Dropbox and Box are quicker from the browser: **+
Connect a cloud** signs you in on the provider's own page and stores the tokens
itself — see [Connecting an Account by Signing In](#connecting-an-account-by-signing-in).

---

## How It Works

### Storing

```
file → zstd compress → split in half (p1, p2) → p3 = p1 XOR p2
     → encrypt each part with AES-256-GCM (Argon2id-derived key)
     → PUT each part to a different account, in parallel
```

An upload commits once **at least two** parts have landed — the minimum that can
still be rebuilt. If fewer than two succeed, the ones that did land are deleted
rather than left as orphans.

Each account also receives `manifest.sand`, an encrypted copy of the index, so
that losing your vault file is survivable. See
[Losing the vault file](#losing-the-vault-file).

### Retrieving

```
request all three parts at once → the first two to arrive win, the rest cancelled
     → decrypt → XOR-reconstruct → decompress → verify SHA-256 → your file
```

Reads are a race, so a slow or offline account costs you nothing — it just loses.

### Reconstruction

| p1 | p2 | p3 | Method |
|:--:|:--:|:--:|:---|
| ✓ | ✓ | — | concat(p1, p2) |
| ✓ | — | ✓ | p2 = p1 ⊕ p3, then concat |
| — | ✓ | ✓ | p1 = p2 ⊕ p3, then concat |
| at most one | | | unrecoverable |

---

## Supported Backends

| Kind | Works with | How you connect it |
|---|---|---|
| `gdrive` | Google Drive | **Sign in** |
| `onedrive` | OneDrive, personal or work | **Sign in** |
| `dropbox` | Dropbox | **Sign in** |
| `box` | Box | **Sign in** |
| `s3` | Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO | Bucket, keys, endpoint for non-AWS |
| `webdav` | Nextcloud, ownCloud, pCloud, Koofr, Fastmail, anything behind `rclone serve webdav` | URL, username, app password |
| `proton` | Proton Drive, through the folder its desktop app syncs | A path |
| `local` | Any directory — external disk, NAS mount, sync folder | A path |

All of them are built on the standard library — SigV4 signing, OAuth and
Microsoft Graph's chunked uploads included — so there are no cloud SDKs, no
CGO, and the artifact is still one static binary.

> **Local folder** on the systemd service is sandboxed: the unit sets
> `ProtectSystem=strict` and grants write access to `/var/lib/sand` plus the
> mount roots a drive normally appears under — `/media`, `/run/media`, `/mnt`,
> `/srv` — so an external disk or a NAS mount connects as-is. A vault folder
> outside those is read-only to the service and fails with *"…is not writable:
> read-only file system"*; grant it once —
> `sudo ./scripts/allow-local-path.sh /data/SANDVault` — and reconnect. See
> [Local folders on the systemd
> service](#local-folders-on-the-systemd-service).

> **The two backends that take a path** — `local` and `proton` — are pointed at
> one rather than told it: the field has a **Browse…** button that walks the
> folders of the machine SAND is running on, opening at the first folder it can
> actually read — home, or the vault's own directory when the service's sandbox
> denies `/home` — with the mount roots a drive turns up under one tap away.
> That machine is rarely the one you are holding,
> which is the whole problem with typing the path from memory on a phone. The
> folder does not have to exist yet — name a new one inside the folder you
> picked, and connecting creates it.

> **Proton Drive** publishes no API. SAND writes its parts into the folder the
> Proton Drive desktop app syncs, which is the same arrangement as any other
> account: the parts are encrypted before Proton ever sees them. On a headless
> box, run rclone's Proton Drive backend behind `rclone serve webdav` and
> connect that as `webdav`.

---

## Connecting an Account by Signing In

Google Drive, OneDrive, Dropbox and Box are connected from inside the app:
press **+ Connect a cloud**, pick the provider, and approve the request on its
own consent screen. SAND exchanges the code for tokens **on the server**, names
the account after whoever signed in, and stores the credentials in the
encrypted vault. The browser never touches a token, and nothing is pasted by
hand.

Two things are worth knowing about how this works on a self-hosted app.

**SAND ships no registered OAuth apps.** There is no SAND cloud to register
them against, and an app credential baked into a public binary is not a secret.
So the first connection to a provider asks for a client ID from your own app
registration — the dialog links straight to the console, tells you what to
create, and shows you the exact redirect URI to paste into it. Every later
account on that provider reuses it.

To skip that step entirely, hand the app credentials to the service once and
every connection becomes a single button:

```bash
SAND_GOOGLE_CLIENT_ID=…      SAND_GOOGLE_CLIENT_SECRET=…
SAND_MICROSOFT_CLIENT_ID=…   SAND_MICROSOFT_CLIENT_SECRET=…   # secret optional
SAND_DROPBOX_APP_KEY=…       SAND_DROPBOX_APP_SECRET=…
SAND_BOX_CLIENT_ID=…         SAND_BOX_CLIENT_SECRET=…
```

**The redirect URI has to be one the provider will accept.** It defaults to the
address you are using the app from — `http://<host>:8123/api/providers/oauth/callback`
— and providers are picky about non-loopback plain HTTP. Register the URI the
dialog shows you, and if this instance sits behind a proxy or has to use one
fixed URI, pin it:

```bash
SAND_OAUTH_REDIRECT=https://sand.example.com/api/providers/oauth/callback
```

If a redirect cannot reach the server at all — signing in from a phone against
a vault bound to `127.0.0.1`, say — the dialog takes the URL the browser was
left on and finishes the exchange from there.

The CLI still takes credentials directly, which is also the way to move an
account between machines:

```bash
./sand remote add onedrive --name onedrive-personal \
    --set client_id=… --set refresh_token=… --set folder=sand
```

> Sign-in state lives only in memory and lasts 15 minutes. The redirect is
> matched to it by an unguessable `state` parameter, because the session cookie
> is `SameSite=Strict` and deliberately does not survive a cross-site
> navigation. Box and Microsoft retire a refresh token as it is spent; SAND
> writes the replacement back into the vault as it goes.

---

## Placement Policy

Where parts go is a security decision: any two parts plus the key rebuild the
file, so two parts on one account means that account could rebuild it.

**`strict`** (default) — one part per account, never two on the same one.

- 3+ accounts → all three parts placed separately. Full redundancy, and no
  single provider can reconstruct anything.
- 2 accounts → parts 1 and 2 only. Recoverable and confidential, but no spare.
- 1 account → refused.

**`redundant`** — always store all three, doubling up when there are fewer than
three accounts. Survives an account going dark, but the doubled-up account holds
enough to rebuild.

```bash
./sand vault policy              # show
./sand vault policy redundant    # change (applies to new uploads)
```

### Which clouds a file goes to

Policy says how many parts may share an account. With more than three accounts
connected, something also has to say *which* three a file uses — three parts
cannot go to five places.

Every upload can choose for itself, and the browser asks before a single byte
leaves the machine: the dialog opens on the clouds the file is about to be
scattered over, and any of them can be swapped for another account.

A vault-wide default sets what that dialog opens on. With no default set, each
file gets three clouds picked at random, which is what spreads a vault evenly
over more than three accounts instead of filling the first three.

```bash
./sand vault defaults                      # show
./sand vault defaults usb-drive r2-cold nextcloud   # set
./sand vault defaults --clear              # back to picking per upload

./sand put report.pdf --accounts usb-drive,nextcloud   # this file only
```

A selection is followed exactly rather than topped up: naming two clouds stores
two parts and says that the file has no spare, instead of quietly putting the
third somewhere you did not choose. Disconnecting an account drops it from the
default.

---

## Changing the password

```bash
./sand vault passwd
```

This does more than it sounds like. The parts on your accounts are encrypted
under a random key kept inside the vault file, not under your password, so
re-wrapping that key under a new password would leave every part exactly as
readable as before to anyone holding the old password and an old copy of the
vault file — or of the `manifest.sand` on any connected account.

So changing the password mints a **new** random key and rebuilds every stored
file onto it: each file is gathered from its parts, re-encrypted, scattered
again, and the parts the old key opened are erased.

That is a download and an upload per file, so on a full vault it takes a while
and it costs bandwidth. It is safe to interrupt:

- The password itself changes in one atomic write, before any file moves.
- Files stay readable throughout — the vault keeps the old key for whatever has
  not moved yet, and drops it the moment nothing needs it.
- `./sand vault migrate` picks up whatever is left, including files whose
  account was offline at the time. `./sand vault status` says how many are.

`./sand vault passwd --no-migrate` changes the password now and leaves the files
for later. Until the migration runs, the old password plus an old copy of the
vault file still opens the parts that have not moved.

---

## Losing the vault file

Parts are encrypted under a random 256-bit key that lives inside your vault
file. Lose that file and your password buys you nothing: every part in every
account is permanently opaque.

So SAND keeps a copy of the index with the data. Every connected account gets
`manifest.sand` — the file tree, the map of which account holds which part, and
the key those parts are encrypted under — encrypted under your vault password
and carrying its own key-derivation parameters, so **your password alone opens
it**. It is rewritten whenever the index changes. It never contains the
credentials for any account.

There are three ways back, depending on what survived:

**You have a `manifest.sand` and your password** — read the tree, and see where
every part went:

```bash
sand manifest ls manifest.sand --long
```
```
Backup written 2026-08-13 05:20 — 2 file(s), 3 account(s), strict placement

/finance/2026/ledger.csv                   263.8 KB
    part 1  cloud-a              7d4206c8…-p1.sand
    part 2  cloud-b              7d4206c8…-p2.sand
    part 3  cloud-c              7d4206c8…-p3.sand
```

**You also have two of a file's parts** — rebuild it with no vault, no accounts
and no network:

```bash
sand restore --parts 7d4206c8…-p1.sand,7d4206c8…-p3.sand \
             --manifest manifest.sand --preserve-tree --output-dir ./rescued
```

**Your accounts are still reachable** — rebuild the whole vault. Start a fresh
vault (a new password is fine), reconnect the accounts, and recover:

```bash
sand vault init
sand remote add gdrive --name work …          # reconnect each account
sand vault recover --dry-run                  # see what would come back
sand vault recover
```

Reconnecting an account gives it a new internal id, so recovery asks each
account what it actually holds and re-points the index at whichever one answers.
Accounts you have not reconnected are reported as unreachable parts — connect
them and run it again.

### The tradeoff, stated plainly

A copy of this file sits in every account, and every copy is one password away
from the data key. If an account is compromised **and** your password is
guessed, the attacker gets your file tree, the placement map, and — because each
part is separately encrypted under that key — whatever plaintext that account's
own parts hold, which for a large file is roughly half of it. Rebuilding a whole
file still requires breaking into a second account, so the two-of-three split
remains a genuine second factor.

One configuration removes that factor: the `redundant` policy with fewer than
three accounts, where a single account can already hold enough parts to rebuild
a file. SAND refuses to write a backup there, and erases any copies it had
already written.

If you would rather have neither the backup nor its risk:

```bash
sand vault backup --disable    # erases every copy from every account
```

Then back up `~/.sand/vault.sand` yourself, and understand that losing it loses
everything.

---

## CLI Reference

### Vault

```
sand vault init [--policy strict|redundant]   Create the vault
sand vault status                             What's stored, where, how much
sand vault passwd [--no-migrate]              Change password, re-encrypt everything
sand vault migrate                            Finish a deferred or interrupted re-encryption
sand vault policy [strict|redundant]          Show or set placement policy
sand vault defaults [account]... [--clear]    Show or set the clouds uploads go to
sand vault backup [--disable|--enable]        Write the encrypted index to every account
sand vault recover [--from ACCOUNT]           Rebuild a lost vault from an account's copy
```

### Recovery

```
sand manifest ls <manifest.sand> [--long]     Print the tree a backup records
sand restore --parts A,B --manifest M         Rebuild a file offline from loose parts
```

### Accounts

```
sand remote kinds                             List backends and their settings
sand remote add <kind> --name N --set k=v …   Connect an account (pings first)
sand remote list                              Status, parts held, bytes stored
sand remote test <name-or-id>                 Re-check reachability
sand remote remove <name-or-id> [--force]     Disconnect
```

### Files

```
sand ls [path] [-l]                24 B  Aug 12 09:14  p1:acct-a p2:acct-b p3:acct-c
sand find <query> [--path /dir] [--type file|folder] [--limit N] [-l]
sand put <file>... [--path /dir] [--overwrite] [--accounts a,b,c]
sand get <path-or-id> [-o out]     Rebuild and decrypt
sand mkdir <path>
sand mv <path> <new-path>          Index only — parts never move
sand rm <path> [-r]                Erases every part from every account
sand check [path] [--all]          Verify parts are still there; non-zero if not
```

`sand check --all` exits non-zero when anything is degraded or unrecoverable,
which makes it a reasonable cron job.

`sand find` searches the file index by name: a bare word matches any name
containing it, ignoring case; `*` and `?` are wildcards (`sand find '*.jpg'`);
and a query with a `/` in it is matched against the whole path
(`sand find photos/2024`). Folders are results too. The index only exists
inside the open vault, so this is the only way to search at all — no connected
account can be asked what it is holding.

```
sand find receipt
/receipts/
/receipts/coffee.pdf   18 KB  Aug 12 09:14  p1:acct-a p2:acct-b p3:acct-c
```

### Server

```
sand serve [--port 8123] [--bind 0.0.0.0] [--idle-timeout 30m] [--vault PATH]
           [--webdav] [--webdav-path /dav] [--rechunk-on-read]
```

`--rechunk-on-read` (on by default) converts files stored before chunked storage
existed after they are read. Such a file has to be rebuilt in full on every read;
converting it once ends that. Turning it off costs nothing but leaves those files
as they are — worth doing on a metered connection, where the conversion's
download and re-upload are real money.

### Standalone mode (no vault, no accounts)

The original SAND workflow, kept because it needs no state at all:

```bash
sand archive report.pdf photos.zip --output-dir ./out
# → sand-p1.zip  sand-p2.zip  sand-p3.zip   (store each somewhere different)

sand restore --parts report.pdf.p1.sand,report.pdf.p3.sand --output-dir .
# → report.pdf, byte-identical
```

### Passwords in scripts

Every command prompts without echo. Set `SAND_PASSWORD` to run unattended, or
pipe the password on stdin.

---

## Web UI

`sand serve` puts a file browser at `http://<host>:8123`:

- **Lock screen** — nothing can be listed or fetched until the vault is open
- **Sidebar** — every connected account, live status, how many parts it holds,
  quota where the provider reports it
- **Connect dialog** — generated from each backend's own field spec, so new
  backends appear without frontend changes
- **Browser** — folders, breadcrumbs, drag-and-drop upload with progress
- **Search** — a box in the toolbar finds a file or folder anywhere in the
  vault, each hit shown with the folder it lives in; searching inside a folder
  looks there first and offers to widen to the whole vault
- **Part badges** — `①②③` coloured per account; click for a live per-part
  health read-out
- **Thumbnails** — images and PDFs show a picture rather than an icon, the PDF's
  being its first page. Made in the browser when the file is uploaded, then
  stored the way everything else is: split into three encrypted parts across
  your accounts, one small pack per folder. Anything without one keeps its icon
- **Preview** — images, video, audio, PDF and text render inline, rebuilt on
  demand; anything else downloads
- **Stream in VLC** — `▶` on a video or audio row opens it in VLC and starts
  playing, and shows the address with a copy button either way
- **Change password** — at the foot of the sidebar; re-encrypts every stored
  file onto the new key, and offers to finish the job later if you defer it or
  an account was unreachable
- **Auto-lock** — the vault re-locks after the idle timeout

The UI loads no external fonts, scripts or styles. Opening your vault makes zero
third-party requests.

Asset filenames carry a content hash and are cached for a year; `index.html`,
which names the current bundle, revalidates on every load against an ETag. An
upgrade therefore reaches a browser the next time it is opened, without the page
paying for a re-download when nothing has changed.

### On a phone

A vault you reach over Tailscale or a reverse proxy gets opened from a phone as
often as from a desk, so the layout folds rather than shrinks. Under 860px wide
the sidebar becomes a drawer behind `☰`, the file table drops its columns for
stacked rows — the thumbnail or icon down the left, the name beside it, and
size, date and part badges underneath — and the toolbar gives the breadcrumb
trail a row of its own. Heights are
measured against the visible viewport, so a phone's collapsing address bar never
hides the last row.

Every control is a target a fingertip can actually hit: 44px, the smallest size
Apple and Google both publish, and a glyph-only button gets that in width as
well as height. Where a pointer can pick between a download and a delete a few
pixels apart, a fingertip cannot — so on a phone each row carries a single `⋯`
instead, and the choices open in a sheet at the bottom of the screen with their
names spelled out and the destructive one set apart at the end. Deleting then
asks once more in a dialog of the app's own, rather than the browser's
`confirm()`, whose two buttons land side by side at the top of the screen.

The part badges are a read-out there rather than a third thing to hit: the menu
opens the same inspector by name. Sizes are set by the layout itself rather than
left to a `pointer: coarse` rule, so a narrow window behaves the way a phone
does — and a phone never inherits a target some inline style quietly shrank.

**Add to Home Screen** puts the vault on the home screen under the SAND mark
rather than a screenshot of the page, and — on iOS always, on Android wherever
the browser honours the manifest — opens it without browser chrome. The mark is
served from the binary in every form the two platforms ask for — iOS reads
`apple-touch-icon.png`, Android reads `manifest.json` and its 192/512px icons,
including a maskable one so an adaptive launcher crops the background and not
the logo. All of it is drawn from a single [`icon.svg`](./web/public/icon.svg)
by [`scripts/make-icons.mjs`](./scripts/make-icons.mjs); change the SVG, run
`make icons`, and commit the PNGs that fall out.

The shortcut still opens a locked vault — it is a link to your server, not a
copy of anything. Nothing is cached offline, so a stolen phone with the icon on
its home screen gets the password prompt like any other browser would.

---

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness |
| GET | `/api/vault` | Initialized? Unlocked? Stats |
| POST | `/api/vault/init` · `/unlock` · `/lock` | Vault lifecycle |
| POST | `/api/vault/password` · `/policy` | Change password (re-encrypts every file) / placement |
| POST | `/api/vault/defaults` | Set the accounts uploads use by default (empty = pick per file) |
| POST | `/api/vault/migrate` | Finish a deferred or interrupted re-encryption |
| GET | `/api/providers/specs` | Backend descriptions for the connect form |
| GET · POST | `/api/providers` | List / connect accounts |
| POST | `/api/providers/{id}/test` | Re-check an account |
| DELETE | `/api/providers/{id}` | Disconnect (`?force=1`) |
| GET | `/api/files?path=` | List a folder |
| GET | `/api/search?q=` | Find files and folders by name (`&path=` to scope, `&type=file\|folder`, `&limit=`) |
| POST | `/api/files` | Upload (`files[]`, `path`, `overwrite`, `thumb-N` per file) |
| GET | `/api/files/{id}/content` | Rebuild and stream (`?download=1`) |
| POST | `/api/files/{id}/stream` | Mint a link a media player can open — see below |
| GET | `/stream/{token}/{name}` | Play that link (no session; the token is the credential) |
| GET · PUT | `/api/files/{id}/thumb` | The stored thumbnail; PUT stores one for a file that has none |
| GET | `/api/files/{id}/health` | Per-part reachability |
| POST | `/api/files/{id}/move` | Rename / move |
| DELETE | `/api/files/{id}` | Erase every part |
| POST · DELETE | `/api/folders` | Create / delete folders |
| GET | `/api/system/folders?path=` | Folders on this machine, for the folder picker |
| POST | `/api/archive` · `/api/restore` | Standalone mode |

Uploads report per-file results, so one failure in a batch doesn't sink the rest:

```json
{
  "stored": 1,
  "results": [
    { "name": "a.pdf", "ok": true, "file": { "…": "…" } },
    { "name": "b.mov", "ok": false, "error": "stored only 1 of 3 parts…" }
  ]
}
```

Sessions use a `SameSite=Strict` cookie and every write is `Origin`-checked, so
another site in your browser can't drive the local API.

---

## Stream a file to VLC

A row's `▶` — or **Stream in VLC** in the phone's row menu — opens the file in
VLC and starts playing it. Nothing is typed and nothing is transcribed.

The browser can't hand VLC a cookie, so the address is what carries the
authority: the server mints a link standing for that **one file**, and the
player follows it with no password and no session. That is also the thing to
know before sharing one — anyone holding the link can play that file, so it
expires on its own, and locking the vault voids every link already handed out.

| | |
|---|---|
| Lifetime | 12 hours, and the clock restarts each time the link is used, so one in use never expires mid-film |
| Locking the vault | Voids every outstanding link immediately |
| Reach | One file. Not the folder, not the index, not the vault |

The dialog shows the address whatever happens, with a copy button — that is the
answer to "how do I get the path", and the fallback when a handoff finds no VLC
installed. How the handoff is made depends on where you are:

| | |
|---|---|
| iOS / iPadOS | `vlc-x-callback://` — VLC's own documented entry point |
| Android | An `intent:` naming `org.videolan.vlc`, so the scheme survives the trip and https stays https |
| Desktop | A two-line `.m3u`; VLC registers itself for playlists on every desktop it installs on |

Seeking works the way it does over the share: a player asking for the middle of
a film fetches the chunks covering that stretch, not the file. The link counts
as use of the vault, so the auto-lock won't fire halfway through.

Any player that takes a URL works — the address is an ordinary HTTP one that
answers `Range` requests. VLC is what the button reaches for because it is the
one with a documented way in on every platform.

> [!NOTE]
> The link inherits the origin you reached the app on, so behind Tailscale
> Serve or a reverse proxy it is the `https://` address without being told
> about it. Over plain HTTP it is a bearer token in the clear, like everything
> else SAND sends there — put TLS in front of it.

---

## Mount it as a drive

`sand serve --webdav` also serves the vault as a WebDAV share, so a file manager
or a player can open it as a drive instead of going through the browser. It's
**off by default** — see the warning below.

```bash
sand serve --webdav              # share at http://<host>:8123/dav/
sand serve --webdav --webdav-path /share
```

On the systemd service, turn it on with `SAND_WEBDAV=1` — either at install
time or by re-running the quickstart, which keeps every other setting as it
was:

```bash
curl -fsSL .../quickstart.sh | sudo SAND_WEBDAV=1 bash
```

Once it is on, the web UI's sidebar grows a **Mount as a drive** button with the
address, a copy button, and where to paste it. The address it shows is the one
you reached the app on, so behind Tailscale Serve or a reverse proxy it offers
the `https://` address without being told about it.

Mount it with **any username** and your **vault password**:

| | |
|---|---|
| macOS | Finder → Go → Connect to Server → `https://host:8123/dav/` |
| Linux | `gio mount dav://host:8123/dav/`, or Files → Other Locations |
| Windows | Map network drive → `https://host:8123/dav/` |
| VLC | Open Network Stream → `https://host:8123/dav/film.mkv` |
| iOS / tvOS | VLC or Infuse → Add Network Share → WebDAV |

Seeking works properly: a player asking for the middle of a film fetches the
chunks that range covers, not the whole file. Copying files in and out streams
rather than buffering, so size is bounded by disk, not memory.

Renaming and moving cost nothing — a file records which folder it is in, so the
index changes and the stored parts never move. That holds for a whole folder
too: renaming one carries everything beneath it, thumbnails included, in a
single write.

> [!WARNING]
> WebDAV authenticates with HTTP Basic, which sends your vault password on
> **every request** rather than once at sign-in. On plain HTTP that is far more
> exposure than the web UI, which is why the share is opt-in. Put TLS in front
> of it — Tailscale Serve, or `scripts/nginx-sand.conf` — and use `https://` in
> the URLs above. Windows refuses Basic auth over plain HTTP by default anyway,
> and caps files at 50 MB until `FileSizeLimitInBytes` is raised.

A correct password against a locked vault unlocks it, which is what keeps a
mount alive past the idle timeout instead of going dead until you open a
browser. Requests to the share count as activity, so the auto-lock won't fire
halfway through a film.

To play *one* file in a player you don't need the share at all — see [Stream a
file to VLC](#stream-a-file-to-vlc), which works on a plain `sand serve` and
hands over a link rather than your password. The share is for browsing the whole
vault from outside the browser.

Jellyfin and Plex want a real filesystem rather than a URL, so they need a FUSE
mount — not built yet.

---

## Security

| Threat | Mitigation |
|---|---|
| One cloud account compromised | Attacker holds one encrypted part and a manifest they cannot open — useless. One part per file is guaranteed by `strict` placement. |
| One account compromised **and** your password guessed | The manifest opens: the tree, the placement map, and about half of each large file that account holds a part of. A whole file still needs a second account. |
| Two accounts compromised | Still needs the key — the vault file, or a manifest backup *and* your password |
| Vault file stolen | Every section AES-256-GCM sealed under an Argon2id key — yields neither credentials nor filenames |
| Provider tampers with a part | GCM tag fails; the other two rebuild the file |
| Part swapping between files | Cleartext header bound as GCM associated data |
| A provider reading your filenames | Names, hashes and sizes are sealed inside each part, not in its header |
| Silent bit rot | Whole-file SHA-256 verified on every rebuild |
| An account disappears | Any two parts suffice; `sand check --all` finds damage early |
| Another site in your browser | `SameSite=Strict` + `Origin` checks |
| Stored HTML/SVG executing in the app | Forced to `attachment`, `nosniff`, restrictive CSP |
| Plaintext cached by a proxy | `Cache-Control: private, no-store` |
| Walking away from the machine | Idle timeout re-locks the vault |
| Metadata leaking to a provider | Object keys derived only from a random ID |

**Two keys, not one.** File parts are encrypted under a random 256-bit data key,
which is itself wrapped by your password — so part encryption doesn't inherit a
weak password. Changing your password mints a **new** data key and re-encrypts
every stored file onto it, because merely re-wrapping the old one would leave
every part on your accounts readable to whoever has the old password and an old
copy of the vault file.

### What SAND does not protect against

- **A compromised machine.** SAND sees plaintext and holds keys while unlocked.
- **Weak passwords.** Argon2id slows a guess; it doesn't fix `hunter2`.
- **Two providers *and* your password** — that's the recovery path, and also the
  attack. Use genuinely independent accounts; two buckets in one AWS account is
  not distribution.
- **Losing your password.** There is no recovery — not from the vault, and not
  from a manifest backup.
- **The manifest backup itself.** Replicating the index is what makes a lost
  vault survivable, and it is also a new thing to steal. `sand vault backup
  --disable` erases every copy — and puts you back to losing everything if you
  lose the vault file.

⚠️ **`--bind` off loopback** sends your password and rebuilt plaintext over the
network in the clear. The server warns you. Put TLS in front of it
(`scripts/nginx-sand.conf`).

---

## Building from Source

| Tool | Minimum | Install |
|---|---|---|
| Go | 1.22 | https://go.dev/dl/ |
| Node.js | 18 | https://nodejs.org |

```bash
make build        # frontend + binary
make build-web    # frontend only → internal/server/dist/
make build-go     # binary only
make version      # print the version this tree would build as
make release      # cross-compile all platforms → dist/
```

Output is `sand` on Linux/macOS, `sand.exe` on Windows.

### Versioning

`vMAJOR.MINOR.PATCH`, where **the patch number is the repository's commit
count** — every commit is a patch release, so `v2.0.311` is the 311th commit on
the 2.0 line.

- `MAJOR`/`MINOR` are source constants in
  [`internal/version/version.go`](./internal/version/version.go). Bump them by
  hand.
- `PATCH` only exists at build time, so it is stamped in: `-ldflags -X` for the
  Go binary, Vite's `define` for the web bundle. Both read
  [`scripts/version.mjs`](./scripts/version.mjs), so the header, `sand version`
  and `/api/health` can never disagree.

A patch of `0` means an unstamped build — no git, or a **shallow clone**, which
`version.mjs` detects and refuses to guess around rather than shipping a build
that quietly calls itself `v2.0.1`. Anything building a release needs the full
commit graph (`fetch-depth: 0`, or `--filter=blob:none` rather than
`--depth 1`).

---

## Testing

```bash
make test          # Go unit tests
make test-cover    # …with an HTML coverage report
make test-e2e      # Python e2e: CLI, HTTP API, vault flow, browser
make test-all      # everything
```

The e2e suite stands up throwaway "cloud accounts" as local-folder providers, so
it exercises the real scatter/gather path — including reading a file back with an
account deliberately taken offline — without needing credentials in CI. It also
walks every fake account asserting no plaintext or filename ever appears there.

If Playwright can't launch its bundled Chromium, point it at one you have:

```bash
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome make test-e2e
```

---

## Deployment

### Linux — systemd

Use [`scripts/quickstart.sh`](#quick-start-on-linux-ubuntu--raspberry-pi): it
fetches or builds, installs, upgrades in place, and rolls back a bad upgrade.

If you already have a binary and only want it running,
`scripts/deploy-linux.sh` is the small path:

```bash
make build
sudo ./scripts/deploy-linux.sh ./sand 8123 127.0.0.1
systemctl status sand && journalctl -u sand -f
```

Both write the same unit and use the same data directory
(`/var/lib/sand`), so they can be used interchangeably on one host.

### Memory, on a small machine

The unit caps the service:

```ini
MemoryMax=80%
MemorySwapMax=0
```

A percentage rather than a number, so it tracks whatever it lands on — 800 MB on
a 1 GB Pi, 12.8 GB on a 16 GB server, no editing. The swap line is the half that
keeps the machine responsive: a limit met by swapping to an SD card is exactly
the unresponsiveness the limit is there to prevent. Under both, a runaway kills
SAND and `Restart=on-failure` brings it back, instead of taking the box —
including ssh — down with it.

Raise it if you have a reason to:

```bash
sudo systemctl edit sand    # [Service] / MemoryMax=90%
```

Files stored before chunked storage existed are the ones that used to need the
headroom: they cannot be read in pieces, so any read rebuilds all of them. They
are now rebuilt onto disk rather than into memory, and converted to chunks after
the first read, so it is a one-off per old file. If you would rather not have
that conversion happen — it costs a download and a re-upload of whatever is read
— turn it off:

```bash
sand serve --rechunk-on-read=false     # or SAND_RECHUNK=0 for the quickstart
```

### Local folders on the systemd service

The unit is hardened with `ProtectSystem=strict`: the filesystem is read-only
to the service apart from the paths its `ReadWritePaths=` lines name. That is
deliberate — the service holds your cloud credentials — but a **Local folder**
account has to be able to write somewhere, so the unit grants the data
directory *and* the roots removable disks and network shares are mounted under:

```ini
ReadWritePaths=/var/lib/sand
ReadWritePaths=-"/media"
ReadWritePaths=-"/run/media"
ReadWritePaths=-"/mnt"
ReadWritePaths=-"/srv"
```

A drive mounted where your desktop or `/etc/fstab` normally puts it therefore
connects with no extra step, while `/etc`, `/usr`, `/home` and the rest stay
read-only to the service. The leading `-` means a root that does not exist on
this host — or a drive that is not plugged in at boot — does not stop the
service from starting. Set `SAND_MOUNT_ROOTS` at install time to change the
list (colon-separated; empty grants nothing but the data directory).

A vault folder outside those roots — `/data/SANDVault`, say — is still
read-only to the service, and connecting it says so:

```
could not connect to Local folder: /data/SANDVault is not writable:
read-only file system — the sand service runs under systemd with
ProtectSystem=strict, which makes every path outside its data directory
and the usual mount roots (/media, /run/media, /mnt, /srv) read-only to it.
```

Nothing is wrong with the drive; the service simply has no write access to it.
Grant the path and reconnect:

```bash
sudo ./scripts/allow-local-path.sh /data/SANDVault
sudo ./scripts/allow-local-path.sh --list      # what the unit and drop-in grant
sudo ./scripts/allow-local-path.sh --remove /data/SANDVault
```

It writes `/etc/systemd/system/sand.service.d/10-local-paths.conf`, which
neither installer touches, so grants survive upgrades. Naming a path the unit
already covers is a no-op it tells you about rather than a redundant entry. To
set paths up at install time instead, pass a colon-separated list:

```bash
curl -fsSL .../quickstart.sh | sudo SAND_LOCAL_PATHS=/data/SANDVault bash
```

Upgrading an install made before the mount roots were granted rewrites the unit
and picks them up; until then, a drive under `/media` needs the same one-path
grant, and the connect error says as much.

Two things the sandbox has no say in, and which the script warns about:

- **Ownership.** The service runs as the `sand` user. A drive mounted by your
  desktop session belongs to your login and is typically `0700`, so `sand`
  cannot write to it whatever the unit says. Either
  `sudo chown -R sand:sand /media/you/Disk/SANDVault`, or mount the drive with
  options that let it write (`uid=`, `gid=`, `umask=` on NTFS/exFAT).
- **A read-only mount.** If the drive itself is mounted `ro` — the usual cause
  on NTFS is a dirty bit left by Windows fast startup or hibernation — remount
  it read-write first. The connect error says so rather than blaming systemd:
  SAND checks `/proc/self/mounts` before choosing its wording.

An external drive plugged in *after* the service starts is picked up
automatically. If a granted path stops working after a re-plug,
`sudo systemctl restart sand` re-establishes it.

Running `sand serve` directly from a shell has none of this — it writes
wherever your user can.

### Windows — NSSM

```powershell
winget install nssm
make build
.\scripts\deploy-windows.ps1 -Binary .\sand.exe -Port 8123 -Bind 127.0.0.1
Get-Service sand
```

### Reverse proxy (nginx + TLS)

```bash
sudo cp scripts/nginx-sand.conf /etc/nginx/sites-available/sand
sudo ln -s /etc/nginx/sites-available/sand /etc/nginx/sites-enabled/
sudo certbot --nginx -d sand.example.com
sudo nginx -t && sudo systemctl reload nginx
```

### Release builds

```bash
./scripts/build-release.sh 2.0.0     # dist/ with all platforms + SHA256SUMS
```

---

## Project Structure

```
sand/
├── cmd/sand/                    # CLI: serve, vault, remote, ls/find/put/get/rm,
│                                #   archive/restore, manifest ls,
│                                #   vault backup/recover
├── internal/
│   ├── archive/                 # encode.go — the in-memory pipeline both modes share
│   ├── crypto/                  # Argon2id + AES-256-GCM
│   ├── compress/                # zstd
│   ├── splitter/                # split, XOR, reconstruct
│   ├── sandfile/                # binary .sand part format
│   ├── provider/                # local, s3 (SigV4), webdav, gdrive, dropbox,
│   │                            #   onedrive, box, proton + the OAuth sign-in flow
│   ├── vault/                   # encrypted store, manifest, placement, scatter/gather,
│   │                            #   the replicated manifest backup and recovery
│   └── server/                  # sessions, OAuth flows, handlers, embedded SPA
├── web/src/                     # React file browser
│   ├── api.js  theme.js  App.jsx
│   └── components/              # LockScreen, AccountsPanel, ConnectCloud,
│                                #   FileBrowser, PreviewModal, StreamLink, ui
│   ├── public/                  # app icon, home-screen icons + manifest,
│   │                            #   developer badge
│   └── build-version.js         # feeds the version into the bundle
├── internal/version/            # MAJOR/MINOR; PATCH stamped at link time
├── tests/                       # pytest e2e: CLI, API, vault flow, browser
├── scripts/
│   ├── quickstart.sh            # one-command systemd install / upgrade / rollback
│   ├── version.mjs              # the one place the version is assembled
│   ├── make-icons.mjs           # redraws the home-screen PNGs from icon.svg
│   ├── build-release.sh         # cross-compile all platforms
│   ├── deploy-linux.sh          # install an already-built binary
│   ├── allow-local-path.sh      # let the sandboxed service write to a local disk
│   └── nginx-sand.conf          # reverse-proxy template
├── CHANGELOG.md                 # release notes, one section per tag
├── SAND_ARCHITECTURE.md         # the full design document
├── CONTRIBUTING.md              # how to contribute + the DCO sign-off
├── CLA.md                       # contributor license agreement
├── LICENSE                      # AGPL-3.0-only
└── Makefile
```

---

## Development

```bash
# Terminal 1 — Go server
make build-go && ./sand serve --port 8123

# Terminal 2 — hot-reload frontend (proxies /api/* to :8123)
cd web && npm run dev     # → http://localhost:5173
```

Use `--vault /tmp/dev-vault.sand` (or `SAND_VAULT`) while developing so you never
touch your real vault.

---

## Where Things Live

| Path | What |
|---|---|
| `~/.sand/vault.sand` | Encrypted index + cloud credentials. Back this up — though since every account also carries a `manifest.sand`, losing it is recoverable with your password. |
| `<archive-id>-pN.sand` | How parts appear on each account, inside whatever folder or prefix that account is configured with. The ID is random and reveals nothing. |
| `manifest.sand` | An encrypted copy of the index, on every account. Opens with your vault password alone. |

Override the vault location with `--vault` or `SAND_VAULT`.

---

## License

SAND is free software licensed under the **GNU Affero General Public License
v3.0** (`AGPL-3.0-only`). See [LICENSE](./LICENSE) for the full text.

The AGPL is a strong copyleft license: anyone who distributes SAND — or **runs a
modified version as a network service** — must make the complete corresponding
source available under the same license. Copyright in the project is held by
Chinmay Manjunath, who may also offer SAND under separate commercial terms.

> **Note for operators (AGPL §13):** if you run a modified SAND server that
> other people interact with over a network, you must offer those users the
> corresponding source of your modified version. Running an unmodified build
> for yourself — the normal case — carries no such obligation.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](./CONTRIBUTING.md). By
contributing you agree to the [Contributor License Agreement](./CLA.md), which
lets the project be offered under both the AGPL and possible future commercial
terms. Sign off your commits with `git commit -s`.
