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
   each ①②③ is coloured by the account holding that part — a colour you pick
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
`SAND_RELEASE=v2026.8.42` pins a specific release instead of the latest. Releases
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

## Naming an account and choosing its colour

Every connected account wears a colour: the stripe down its card in the
sidebar, every part badge for a file it holds, and its row in the cloud picker
are all the same shade. That is what makes *which three clouds is this file on*
a question you answer by eye rather than by opening an inspector.

**Edit** on an account's card opens the menu for both fields — what it is
called, and which colour it wears. Twelve named colours are the shortlist;
**All shades** opens the whole palette — the same twelve hues in three shades
each, a hue per column — and a native picker takes any colour at all for a
cloud with a brand colour of its own. **Automatic** hands the choice back to the
browser. A swatch another account is already wearing is marked as such before
you pick it, and a chosen colour is claimed before the automatic ones are handed
out, so nothing else drifts onto it.

Every colour in the palette is light enough to carry the app's dark text and
dark enough to hold against the surface behind it, because a part badge is a
number drawn *on* the account's colour — a navy or a pastel would be a colour
you could pick and then not be able to read.

```bash
./sand remote edit r2-cold --name r2-archive       # rename it
./sand remote edit r2-cold --color '#38bdf8'       # or any hex colour
./sand remote edit r2-cold --color auto            # back to the browser's pick
```

Neither field reaches the cloud. Nothing is uploaded, downloaded or
re-encrypted, no credential is touched, and not one part moves — the account
answers to exactly what it did before. A rename does travel through the index:
every part records the name of the account holding it, which is what the file
list, the health read-out and a recovery from a `manifest.sand` all read, so
the new name lands on all of them in the same write.

Two accounts may not share a name, whether it is set at connect time or later.

---

## Placement Policy

Where parts go is a security decision: any two parts plus the key rebuild the
file, so two parts on one account means that account could rebuild it.

**`strict`** (default) — one part per account, never two on the same one.

- 3+ accounts → every shard placed separately. Full redundancy, and no single
  provider can reconstruct anything. Six or nine accounts cut the file finer
  instead of wider — still one shard per account, see below.
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

### A wider spread, when you have the clouds for it

How many clouds you choose settles the erasure code the file is cut with. One
rule: clouds go in groups of three, and two thirds of the shards rebuild the
file.

| Clouds | Scheme | Storage | Clouds that can go dark | Clouds needed to rebuild |
|---|---|---|---|---|
| 3 | `2-of-3` | 1.5× | 1 | 2 |
| 6 | `4-of-6` | 1.5× | 2 | 4 |
| 9 | `6-of-9` | 1.5× | 3 | 6 |
| 12 | `8-of-12` | 1.5× | 4 | 8 |
| 3m | `2m-of-3m` | 1.5× | m | 2m |

The table stops where patience does, not where the code does — every multiple of
three works, up to 255 clouds. Each group you add buys one more cloud that can
fail and two more that would have to collude, and costs nothing in storage:
every scheme in the family stores 1.5×, so **widening costs accounts, not
bytes**.

```bash
./sand vault defaults box s3 drive dropbox onedrive proton   # 4-of-6 from now on
./sand put taxes.pdf --accounts box,s3,drive,dropbox,onedrive,proton
```

Every cloud holds exactly one shard at every width, so the promise that a single
compromised provider yields noise is as true across thirty as across three — and
gets stronger, since one shard of twenty is a twentieth of the file rather than
a half.

Three is still what an upload takes when nobody says otherwise. Counts that are
not whole groups — 4, 5, 7, 8 — are refused rather than rounded, because there
is no code that uses them without leaving a cloud with no shard to hold.

**Changing an existing file's width rebuilds it.** A 2-of-3 file's shards are
halves and a 4-of-6 file's are quarters, so nothing carries across:

```bash
./sand relocate /taxes --accounts box,s3,drive,dropbox,onedrive,proton --dry-run
```

Moving a file between clouds *at the same width* is still the cheap case — only
the shards that have to move are copied, and nothing is decrypted. The dry run
says which of the two is about to happen.

## Moving something to another folder

The other kind of move, and the cheap one. Which folder a file is in is a field
in the encrypted index, and the objects its parts are stored as are named after
the file's random archive ID rather than after the folder — so moving a file
across the vault rewrites that one field and touches nothing on any account. A
4 GB film moves as fast as a note, and its parts stay on exactly the clouds they
were scattered to.

A folder is the same thing again: it is a path in the index and nothing more, so
moving one carries every file beneath it, at any depth, in a single write. There
is no moment where half the tree answers to its old name. The thumbnails and the
film-details setting come along too — both are filed under the folder rather than
inside it.

```bash
./sand mv /draft.txt /final/published.txt   # rename, and move it
./sand mv /draft.txt /final                 # into a folder, keeping its name
./sand mv /photos/2024 /archive/2024        # a folder, with everything in it
```

In the browser every row carries **→** for this, beside Download and Delete on a
file and beside **⇄** on a folder; it is also *Move to another folder* in the
row's menu, which every row now carries under **⋯**. Tick several rows and
**→ Folder** in the selection bar moves the lot — the button next to it,
**⇄ Clouds**, is the other move, the one that copies parts between accounts.

Either way it opens on the folder the thing is already in and walks the vault's
own tree from there, with a **+ New folder here** for a destination that does not
exist yet. Nothing that cannot happen is offered: a folder cannot be moved inside
itself, and anything already in the folder you are looking at says so rather than
being moved onto its own path. A name already taken in the destination is
refused, one item at a time, and the rest of a selection still moves.

### Renaming

The same operation with the folder left alone, and just as free: a name is a
field in the encrypted index, and a file's parts are named after the file's
random archive ID rather than after its name, so renaming rewrites that field and
touches no account. Renaming a folder carries everything beneath it in one write,
exactly as moving one does.

*Rename* sits in every row's **⋯** menu, and on the command line it is the same
`sand mv`:

```bash
./sand mv /draft.txt /published.txt      # a file
./sand mv /photos/2024 /photos/holiday   # a folder, with everything in it
```

The dialog opens with the name selected up to the extension, so typing replaces
the words and keeps the `.mkv`. A name is one segment — a `/` in it is a move,
and the field says so rather than quietly making folders — and a name already
taken in that folder is refused.

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

## Sub vaults

A sub vault is a vault inside your vault, with a password of its own. Your vault
password lists it and cannot open it.

That is worth having because "unlocked" is not a small state. A vault mounted as
a drive is a folder every process running as you can read, that a backup agent
will happily copy elsewhere, and that stays mounted long after you have walked
away from the machine. A sub vault is where the things go that should not be
reachable that way.

```bash
./sand sub new Taxes                    # asks for a password of its own
./sand sub ls                           # what exists, and how much each holds
./sand sub assign /Papers Taxes         # move a folder in, keeping its path
./sand ls --in Taxes /Papers            # work inside it
./sand put --in Taxes ./p60.pdf --path /Papers
./sand sub assign /Papers Taxes --out   # and back out again
```

In the browser it is **Vault settings → Sub vaults**. Tick *Show them at the top
of the vault* there and they appear alongside your folders, locked ones included
— click one and it asks for its password.

What holds:

- **Never on a WebDAV mount.** Not while locked, and not while unlocked either.
  There is no setting for this; the share only ever sees the main vault.
- **A password change on your vault does not touch it.** Its section is sealed
  under its own password, so it is carried across untouched.
- **Your vault password reveals the name and nothing else.** Not a path, not a
  filename, not a size, not a type. It does reveal an inventory of which
  accounts hold its parts and how big they are — which is what lets a forgotten
  sub vault still be erased from your clouds, and what stops disconnecting an
  account silently stranding files nobody can see.
- **There is no recovery for the password.** Not from your vault password, and
  not from a `manifest.sand` — the backups carry a sub vault sealed.

Moving something in is instant: the index changes and the parts stay where they
are. The files are then re-encrypted onto the destination's own key in the
background, and until that finishes they can only be read while the vault they
came from is open — no key is handed across, so a file assigned out of a sub
vault still needs that sub vault open until the re-encryption has moved it.

---

## Finding a vault on an account you reconnect

Every vault keeps a copy of its encrypted index on each account it uses. So an
account you used on a machine that has since died still carries one — and when
you connect that account to a new vault, SAND now says so.

```bash
./sand sub scan                    # which accounts hold a vault index, and whose
./sand sub import old-dropbox      # bring one in as a sub vault of this one
```

This is what to reach for when `sand vault recover` refuses because your vault
already holds files — `sand sub scan` asks the same question the recovery scan
does, and importing is what it can offer that recovering cannot. A backup carries an index, a data key and a password that
opens it — which is exactly what a sub vault is — so what was found lands beside
what you have rather than replacing it.

You are asked for two passwords: the old vault's, to open its index, and the one
the sub vault will answer to from now on. The second costs nothing, because the
old data key is adopted as it stands and no file is re-encrypted by the import
itself. Run `sand sub passwd` afterwards to rotate that key too, which is what
finally makes the old password useless.

Files whose parts sit on accounts you have not connected come in anyway and are
counted; connect those accounts and the files come back with them.

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

### The browser offers this without being asked

You should not have to know the command exists. On a vault holding no files —
which is what a reinstalled machine has — connecting an account makes SAND ask
it what it is holding, and whether the `manifest.sand` there is one this vault
wrote. A backup written by a *different* vault means exactly one thing, and the
app says so:

```
⚠ Sand files detected on your clouds — 412 parts (3.1 GB) and an
  encrypted copy of a vault index this one did not write.   [Attempt recovery]
```

That fires on the *first* cloud you reconnect, which is never enough on its own:
a file is rebuilt from two of its three parts, and one account holds one of
them. So the dialog does not open on a password box it cannot use yet — it asks
for the next cloud, and connects it for you:

> A file was split into 3 parts across 3 clouds and is rebuilt from any 2 of
> them, so one cloud on its own carries no whole file. Connect the next cloud
> that held parts of this vault — as many as you can — and the recovery starts
> on its own.
>
>                                             `Not now`  **`+ Connect another cloud`**

Every account that lands is re-checked without being asked. The second one turns
the dialog into the password prompt; the last one is taken as your answer, and
the recovery runs itself: check first, and commit if the check comes back whole.
The password is the one belonging to the vault you lost, which need not be the
password of the vault you are recovering into.

### What did not come back

A file is rebuilt from any two of its three parts. One cloud you have not
reconnected yet costs you nothing; two costs you the file. So the report ends on
the shortfall rather than the total, in files **and** in bytes — those diverge,
and the bytes are usually the answer to "how bad is this":

```
Recovered 18 of 23 file(s) in 6 folder(s) — 1.2 GB of 4.4 GB.
  2 file(s) came back with no spare part left.

Not recovered: 5 of 23 file(s), 3.2 GB of 4.4 GB.

Connect these accounts and run this again:
  onedrive-personal         onedrive   9 part(s) — 5 file(s) cannot be opened without it
  nas-backup                webdav     3 part(s) — spare parts only

Files still missing:
  /finance/2026/ledger.csv                 263.8 KB  1 of 2 part(s) found
  …
```

An account that only held spare copies is listed but not blamed — reconnecting
it changes nothing about what you can open. In the browser this list comes with
a **Connect a missing cloud** button, and connecting one from it picks the
recovery straight back up.

Once the missing clouds turn up, finish the job:

```bash
sand remote add onedrive --name onedrive-personal …
sand vault recover --resume
```

`--resume` is a different operation from the recovery itself, and a much
smaller one. The index is already here and so is the key; what was missing was a
reachable copy of the parts. So it asks every account what it holds, re-points
the records that now have somewhere to point, and asks for no password at all.
Recovering a second time is refused — adopting the snapshot again would replace
the data key the files it already brought back depend on.

In the browser this is the same banner and the same dialog, saying *Finish
recovery* instead of *Attempt recovery*.

### Making the recovered files yours

A recovery adopts the lost vault's data key, because that key is the only thing
that opens the parts already sitting on your accounts. It gets your files back,
and it leaves something behind: those parts are still encrypted under the old
key, which the **old password** still derives. Every copy of the old
`manifest.sand` hands that key over — including any taken off an account before
this vault existed, which no amount of overwriting can reach.

So the vault says so, and keeps saying so, until you finish the job:

```
Inherited key:    still the one a recovery adopted, so the lost vault's password
                  opens every part — new uploads included. Run 'sand vault reclaim'
```

Note the *new uploads included*: the adopted key is the vault's active key, so
anything stored after the recovery is sealed under it too. Waiting widens what
the old password reaches rather than holding it still.

```bash
sand vault reclaim                                  # onto the clouds they are on
sand vault reclaim --account work --account offsite --account nas
```

A fresh data key is sealed under your **current** password, every file is
rebuilt onto it, and the parts the old key opened are erased. Your password does
not change. Since every file is gathered and scattered anyway, `--account` is
the cheap moment to say where they should live — the clouds a recovery lands on
are the ones a machine you no longer have picked.

It costs a download and an upload of the whole vault, which is why it is offered
rather than done: a recovery has to work with the network you have, and this can
wait for the one you want. Files stay readable throughout, and stopping is safe
— whatever moved stays moved, and `sand vault migrate` finishes the rest.

In the browser this is a standing banner in the accounts panel, and a dialog
with the cloud picker in it.

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
sand vault recover --resume                   Finish one, once the rest of the clouds are back
sand vault reclaim [--account NAME]...        Re-encrypt recovered files under your own key
```

### Sub vaults

```
sand sub ls                                   The vaults inside your vault
sand sub new <name>                           Make one, with a password of its own
sand sub passwd <name> [--no-migrate]         Change its password, re-encrypt its files
sand sub assign <path> <name> [--out]         Move a file or folder in, or back out
sand sub rm <name> [--force]                  Delete it and erase its parts
sand sub scan                                 Look for other vaults on your accounts
sand sub import <account> [--name N]          Bring one in as a sub vault
```

Every ordinary command takes `--in <name>` to work inside one:

```
sand ls --in Taxes /Papers
sand put --in Taxes ./p60.pdf --path /Papers
sand get --in Taxes /Papers/p60.pdf -o ./
```

For scripts, `SAND_SUB_PASSWORD` is to a sub vault what `SAND_PASSWORD` is to
the vault; `sand sub passwd` also reads `SAND_NEW_SUB_PASSWORD`, and
`sand sub import` reads `SAND_BACKUP_PASSWORD` for the vault being imported.

### Converting old files

```
sand vault convert                 # what is still in the old format
sand vault convert --all           # move all of it
sand vault convert /Videos/a.mkv   # or just these
```

Files stored before SAND had chunked storage are one sealed blob with no seams,
so reading a byte in the middle means rebuilding all of it. Nothing streams such
a file for that reason — the browser and the share both refuse it and offer to
convert instead. Converting reads it once and stores it again in chunks, after
which it seeks like anything else. It costs a download and an upload of the whole
file, and erases the old parts once the new ones are committed.

### Recovery

```
sand manifest ls <manifest.sand> [--long]     Print the tree a backup records
sand restore --parts A,B --manifest M         Rebuild a file offline from loose parts
```

### Accounts

```
sand remote kinds                             List backends and their settings
sand remote add <kind> --name N --set k=v …   Connect an account (pings first)
sand remote list                              Status, parts held, bytes stored, room left, colour
sand remote edit <name-or-id> [--name N] [--color '#38bdf8'|auto]
                                              Rename it, or change the colour it wears
sand remote test <name-or-id>                 Re-check reachability
sand remote remove <name-or-id> [--force]     Disconnect
```

`sand remote edit` changes what an account is called and the colour it wears in
the browser. Neither reaches the cloud: no credential is touched and not one
part moves. See [Naming an account and choosing its
colour](#naming-an-account-and-choosing-its-colour).

### Files

```
sand ls [path] [-l]                24 B  Aug 12 09:14  p1:acct-a p2:acct-b p3:acct-c
sand find <query> [--path /dir] [--type file|folder] [--limit N] [-l]
sand put <file>... [--path /dir] [--overwrite] [--accounts a,b,c]
sand get <path-or-id> [-o out]     Rebuild and decrypt
sand mkdir <path>
sand mv <path> <new-path>          A file or a folder; index only, parts never move
sand relocate <path> --accounts a,b,c [--dry-run]
                                   Move a file or folder onto other clouds
sand rm <path> [-r]                Erases every part from every account
sand check [path] [--all]          Verify parts are still there; non-zero if not
```

`sand relocate` moves only the parts that have to move: anything already on one
of the accounts you named stays where it is, and what travels is copied across
still encrypted rather than rebuilt. A folder takes everything under it. See
[Moving something to different clouds](#moving-something-to-different-clouds).

`sand check --all` exits non-zero when anything is degraded or unrecoverable,
which makes it a reasonable cron job.

Film details have no CLI of their own. Matching a folder is a thing you look at
while it happens — the posters, the misses, the match you have to correct by
hand — so it lives in the browser, and the CLI stays the tool for moving files
about. See [Film details](#film-details).

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
           [--webdav] [--webdav-path /dav]
```

On start it reads whatever memory limit it is running under — the systemd unit's
`MemoryMax`, or the machine's own — and sets `GOMEMLIMIT` below it, so the
collector works against a ceiling instead of growing towards the machine's. Set
`GOMEMLIMIT` yourself to override.

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
  and how full it is: a bar splitting what SAND put there from whatever else
  already lives on the account, and from the room that is left. Local folders
  report the drive they sit on, so a disk shared with the rest of the machine
  says how much of itself is actually free
- **Account stats** — *Stats* on any account card opens the breakdown behind
  that bar: capacity against SAND's own share, what the parts belong to by kind
  and by folder, the month they arrived in, the heaviest files, and how many
  files could not be rebuilt without this account
- **Connect dialog** — generated from each backend's own field spec, so new
  backends appear without frontend changes
- **Browser** — folders, breadcrumbs, drag-and-drop upload with progress
- **Navigation** — Back, Forward and Up lead the toolbar and step along the
  trail of folders you have walked through, as does `Alt+←` / `Alt+→` / `Alt+↑`.
  The trail is held in memory only and goes when the vault locks
- **List or grid** — rows with columns, or tiles built round the stored
  thumbnail, for the folder of photographs whose file names say nothing. Tiles
  are square, or a poster's two-by-three in a folder that has asked for film
  details
- **Sorting** — by name, size, date or kind, each opening the way round it is
  normally wanted and reversing when chosen again; folders always lead. The view
  and the sort are remembered between visits
- **Selection** — tick rows singly, in a run with `Shift`, or all of them with
  `Ctrl+A`, then download, move into another folder, move to other clouds, or
  delete the lot. A move onto other clouds prices the whole selection as one
  number before it starts
- **Folder pictures** — a folder can be given a picture of something inside it, a
  film's poster or any other thumbnail, reaching as deep as it needs to. Never
  chosen for you, nothing is stored for it, and `🖼` on the row picks or removes
  it — see [A folder can wear one too](#a-folder-can-wear-one-too)
- **Move between folders** — `→` on any row, or *Move to another folder* in its
  menu, opens the vault's own folder tree; nothing is uploaded or downloaded and
  the parts stay on the clouds they are on — see [Moving something to another
  folder](#moving-something-to-another-folder)
- **Rename** — *Rename* in any row's menu, for a file or a folder; the name is
  index, so nothing is transferred — see [Renaming](#renaming)
- **Row menu** — `⋯` on every row opens the same sheet a phone gets, so a desk
  reaches everything a file can do without the row growing a control per feature
- **Search** — a box in the toolbar finds a file or folder anywhere in the
  vault, each hit shown with the folder it lives in; searching inside a folder
  looks there first and offers to widen to the whole vault
- **Part badges** — `①②③` coloured per account; click for a live per-part
  health read-out
- **Edit an account** — rename a cloud, or pick the colour it wears everywhere
  in the app; neither touches its credentials or the parts on it
- **Thumbnails** — images and PDFs show a picture rather than an icon, the PDF's
  being its first page. Made in the browser when the file is uploaded, then
  stored the way everything else is: split into three encrypted parts across
  your accounts, one small pack per folder. A video gets one too, taken from a
  frame while you watch it — nothing can make a picture of a film at upload time
  without decoding the film. Anything without one keeps its icon
- **Film details** — a folder can be told its videos are films, and then they
  get the poster, the summary, the cast and the score, the way Plex or Jellyfin
  would show them. Off everywhere until a folder asks for it, because it is the
  one thing in SAND that talks to anyone but your own accounts — see
  [Film details](#film-details)
- **Preview** — images, video, audio, PDF and text render inline, rebuilt on
  demand; anything else downloads. A matched film opens on its poster and
  summary rather than an unplayed black rectangle
- **PDF viewer** — pages are drawn by the app itself rather than handed to the
  browser's viewer, so a phone shows the document instead of the blank frame
  iOS Safari makes of one. Page at a time, arrow keys or `‹ ›`, and a zoom for
  reading a dense page on a small screen; only the pages you look at are
  fetched
- **Stream in VLC** — `▶` on a video or audio row opens it in VLC and starts
  playing, and shows the address with a copy button either way
- **Change password** — under **Vault settings** at the foot of the sidebar,
  with the rest of what the vault itself is set to; re-encrypts every stored
  file onto the new key, and offers to finish the job later if you defer it or
  an account was unreachable
- **Auto-lock** — the vault re-locks after the idle timeout

The UI loads no external fonts, scripts or styles. Opening your vault makes zero
third-party requests — including in a folder with film details on, since the
poster and the summary are stored in the vault and served by your own server
like everything else. The one request that does leave is the lookup itself, made
by the server, only for a folder you turned on, and only when you ask for it.

The wordmark is the one piece of typography that is not the system's own:
*Vault* is written in a monoline hand that ships in the repository at **3.4
KB** — five glyphs and nothing else, subset from
[Caveat](https://fonts.google.com/specimen/Caveat) under the SIL Open Font
License (see [`web/fonts/`](./web/fonts/README.md)). It is never linked:
the build embeds it in the page as a `data:` URI, so it costs one request fewer
than a system font rather than one more. **Nefelibata Script** — a *nefelibata*
is a cloud-walker — is asked for ahead of it and embedded the same way if you
drop a licensed copy in, and the platform's own handwriting faces stand behind
both.

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
trail a row of its own, under the row carrying Back, Forward, Up and the view
controls. Sorting opens in the same bottom sheet a row's menu does. Heights are
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

## Film details

A folder of films is a folder whose file names say nothing.
`The.Thing.1982.REMASTERED.1080p.BluRay.x265-RARBG.mkv` is a perfectly good
thing to store and a terrible thing to read. Plex and Jellyfin solve that by
looking each file up and showing you the poster, and SAND can now do the same —
for the folders you say so.

Turn it on with the `🎬` button in the toolbar, which acts on the folder you are
standing in and everything underneath it. Then **Look up new films** sweeps it:
each video's name is read for a title and a year, that is searched for, and what
comes back is stored — the summary, the score, the runtime, the genres, the
director and the top of the cast in the vault's encrypted index, and the poster
as the file's thumbnail.

From then on the grid is a wall of posters captioned with the films' names
rather than the files', a row shows the poster where its icon was, and `🎬` on
the row — or **Film details** in the phone's row menu, or the strip above a
video you are watching — opens everything that was found.

Opening one shows the film rather than a player. A video element that has not
been played is a black rectangle with a triangle on it — on a phone that is half
the screen saying nothing, in front of a film whose poster, summary and cast the
vault is already holding. So the preview opens on those, and the player takes
over when you press **Play here**. Everything else about the dialog is where it
was: stream it to VLC, or download the rebuilt file.

The tiles take a poster's shape rather than a photograph's in a folder that has
asked for films, since a square crop of a two-by-three poster eats the title
band at its foot. It is the whole grid that changes rather than the matched
tiles alone: one shape per view keeps the rows level and the folders in step
with the films beside them. Everywhere else the tiles stay square.

### It is off until you turn it on, and here is why

Everything else in SAND stays between this machine and the accounts you
connected. This does not. Looking a film up sends a title guessed from a file
name, plus this machine's address and your own API key, to
[The Movie Database](https://www.themoviedb.org) — the same service Plex and
Jellyfin use.

So it is a per-folder switch rather than a setting, it names the folder it was
made on, and turning it on **sends nothing**: the sweep is a second button you
have to press. Nothing about the file itself ever goes — not its contents, not
its size, not its hash, not which clouds its parts are on. And a film is looked
up once: after that the answer is in your vault, and opening the folder again
contacts nobody.

You need a free key of your own, from **themoviedb.org → Settings → API**.
Either the v3 key or the v4 read access token works. Paste it into **Vault
settings → Film key**, at the foot of the accounts panel (behind `☰` on a
phone) — it sits with the password and the default clouds because it is one key
the whole vault shares, not a property of any one folder. Until there is one, a
folder's `🎬` dialog says so and cannot be turned on. It is stored in the vault
file, sealed under your password like your cloud credentials — and deliberately
*not* in the manifest, which is replicated to every connected account, because a
credential for someone else's service has no business being copied onto three
clouds.

### When it guesses wrong

It will, sometimes. Names are read the way every media server reads them: cut at
the year if there is one, cut at the first `1080p`/`BluRay`/`x265` if there is
not, and fall back to the folder's name when the file's says nothing —
`Blade Runner (1982)/title00.mkv` is matched from the folder. Where two films
share a name, the year in the file name decides.

Open the file's details and **Fix the match**: search by title, and pick the
right film from the list. A film chosen by hand is marked as such, and a later
sweep — even **Look up everything again** — leaves it alone. **Forget** drops the
details for a file entirely.

### What it costs, and what it survives

A sweep is one search, one record and one poster per film, so a large folder
takes a while — but it is safe to stop and run again, since every film is stored
the moment it is matched and nothing already matched is looked up twice. Posters
are written one pack per folder rather than one per film, which is the
difference between a folder of two hundred films costing two hundred uploads and
costing one.

Changing your vault password drops every stored thumbnail — they are sealed
under the key being retired, and regenerating derived data beats re-encrypting
it. A photograph's thumbnail comes back the next time you open it; a poster has
no such source, so the film's record keeps the artwork's address. Sweep the
folder again afterwards and the posters come back for one image fetch each, with
no searching at all.

Turning the switch back off stops any further lookups and leaves what was
already found exactly where it is.

Details and artwork come from The Movie Database. SAND uses the TMDB API but is
not endorsed or certified by TMDB.

### A folder can wear one too

A folder of films is otherwise a row of identical `📁` — the same problem its
files had before they had posters. So a folder can borrow a picture from what is
inside it: a film's poster, or the thumbnail of a photograph or a PDF. It reaches
as deep as it needs to, so a library whose films sit in a folder each can wear
one from two levels down.

**Nothing is picked for you.** A folder keeps its icon until you say otherwise —
which film stands for a trilogy is a matter of taste, and a picture appearing on
a folder you never asked about is the wrong kind of surprise. **🖼 on the
folder's row**, or *Folder picture* in its menu, shows everything inside it that
has a picture and lets you pick one; **Use no picture** takes it away again. Both
controls appear only where there is something to choose from.

**Nothing is stored to do it either.** The folder points at a file that already
has a thumbnail and draws that file's own picture, through the same address its
row draws through. A folder's picture therefore costs no upload, no extra object
on any account, and nothing at all to change or to remove — and it is not artwork
you can lose, because it is not a copy of anything.

Your choice is remembered by file, not by name or place, so renaming that file,
moving it deeper, or moving the whole folder somewhere else all leave it
standing. Deleting the file puts the folder back to its icon.

One cost worth knowing: thumbnails are stored one pack per folder, so a parent
whose folders have all been given pictures gathers a pack per folder the first
time it is drawn. Only the tiles actually on screen fetch anything, and each pack
is gathered once and kept until the vault locks — it is the same cost as opening
those folders, paid where they are listed instead.

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
| GET | `/api/vault/recovery` | Is a connected account carrying a vault this one could recover? |
| POST | `/api/vault/recovery` | Rebuild the index from that copy (`password`, `provider_id`, `dry_run`) |
| POST | `/api/vault/recovery/resume` | Re-point the index at accounts reconnected since (`dry_run`; no password) |
| POST | `/api/vault/reclaim` | Re-encrypt recovered files under this vault's own key, onto `accounts` |
| GET | `/api/providers/specs` | Backend descriptions for the connect form |
| GET · POST | `/api/providers` | List / connect accounts |
| POST | `/api/providers/{id}/test` | Re-check an account |
| PATCH | `/api/providers/{id}` | Rename it / set its colour (`name`, `color`; `""` for automatic) |
| DELETE | `/api/providers/{id}` | Disconnect (`?force=1`) |
| GET | `/api/files?path=` | List a folder |
| GET | `/api/search?q=` | Find files and folders by name (`&path=` to scope, `&type=file\|folder`, `&limit=`) |
| POST | `/api/files` | Upload (`files[]`, `path`, `overwrite`, `thumb-N` per file) |
| GET | `/api/files/{id}/content` | Serve at an offset — a range costs the chunks it covers, not the file (`?download=1`) |
| GET | `/api/conversions` | Files still in the pre-chunking format |
| POST | `/api/files/{id}/convert` | Move one out of it |
| POST | `/api/files/{id}/stream` | Mint a link a media player can open — see below |
| GET | `/stream/{token}/{name}` | Play that link (no session; the token is the credential) |
| GET · PUT | `/api/files/{id}/thumb` | The stored thumbnail; PUT stores one for a file that has none |
| GET | `/api/movies` | Whether a film database key is stored, and which folders are opted in |
| POST | `/api/movies/key` | Store the key (checked against the database first); `""` clears it |
| POST | `/api/movies/lookup` | Turn film details on or off for a `path` and everything under it |
| POST | `/api/movies/scan` | Look up every unmatched video under a `path` (`refresh` to redo the matched ones) |
| GET · POST · DELETE | `/api/files/{id}/movie` | What film this is / look it up (`query`, `year`, `tmdb_id`) / forget it |
| GET | `/api/files/{id}/movie/candidates?q=` | Search the database without storing anything, to correct a match |
| GET | `/api/files/{id}/health` | Per-part reachability |
| POST | `/api/files/{id}/move` | Rename / move into another folder |
| DELETE | `/api/files/{id}` | Erase every part |
| GET | `/api/folders` | Every folder in the vault, for a destination picker |
| GET · POST | `/api/folders/art?path=` | Which file's thumbnail a folder is drawn with, and what else it could be (`id` to pick one, `""` for none) |
| POST · DELETE | `/api/folders` | Create / delete folders |
| POST | `/api/folders/move` | Move a folder, and everything under it, `from` one path `to` another |
| POST | `/api/relocate` | Move a file (`id`) or folder (`path`) onto other `accounts`; `preview` prices it without moving anything |
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

Once it is on, **Vault settings** in the web UI's sidebar grows a **Mount as a
drive** line with the address, a copy button, and where to paste it. The address it shows is the one
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
| The film database learning what you have | Off until a folder is opted in by name; sends a title guessed from a file name and nothing else, once per film, and never the file, its size or where its parts live |

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
  lose the vault file. Note what it carries once you turn film details on: the
  backup *is* the index, so the titles are in it. They add nothing an attacker
  who could already read `The.Thing.1982…mkv` from the same blob did not have.
- **The film database knowing what you asked it.** Storing the answer in the
  vault is not the same as the question never having been asked. TMDB sees a
  title and your address, once per film, for the folders you turned on. That is
  the whole reason it is a switch — see [Film details](#film-details).

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

`vYEAR.MONTH.PATCH` — a calendar version, where **the patch number is the
repository's commit count** — so `v2026.8.311` is the 311th commit on the 2026.8
line. There is no semantic major/minor: the leading numbers say *when* a release
line opened, not what it promises about compatibility. Breaking changes are
called out in [`CHANGELOG.md`](./CHANGELOG.md), which is the thing to read
before upgrading.

- `YEAR`/`MONTH` are source constants in
  [`internal/version/version.go`](./internal/version/version.go). Bump them by
  hand when a release line opens — they are deliberately not read from the build
  clock, so rebuilding an old tree still reports what it originally shipped.
- The month is not zero-padded (`v2026.8.311`, not `v2026.08.311`): semver
  forbids a leading zero, and an unpadded month keeps every tag something a
  semver parser will accept.
- `PATCH` only exists at build time, so it is stamped in: `-ldflags -X` for the
  Go binary, Vite's `define` for the web bundle. Both read
  [`scripts/version.mjs`](./scripts/version.mjs), so the header, `sand version`
  and `/api/health` can never disagree.

A patch of `0` means an unstamped build — no git, or a **shallow clone**, which
`version.mjs` detects and refuses to guess around rather than shipping a build
that quietly calls itself `v2026.8.1`. Anything building a release needs the
full commit graph (`fetch-depth: 0`, or `--filter=blob:none` rather than
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
│   ├── movie/                   # file name → film, and the film database client —
│   │                            #   the only package that talks to a third party
│   ├── vault/                   # encrypted store, manifest, placement, scatter/gather,
│   │                            #   the replicated manifest backup and recovery
│   └── server/                  # sessions, OAuth flows, handlers, embedded SPA
├── web/src/                     # React file browser
│   ├── api.js  theme.js  App.jsx
│   ├── navigation.js  view.js   # the trail of folders walked; view + sort prefs
│   └── components/              # LockScreen, AccountsPanel, ConnectCloud,
│                                #   FileBrowser, Toolbar, FileEntry, BulkActions,
│                                #   MoveToFolder, Rename, FolderArt, PreviewModal,
│                                #   PdfPreview, FilmDetails,
│                                #   StreamLink, RecoverVault, ReclaimVault, ui
│   ├── public/                  # app icon, home-screen icons + manifest,
│   │                            #   developer badge
│   └── build-version.js         # feeds the version into the bundle
├── internal/version/            # YEAR/MONTH; PATCH stamped at link time
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
| `~/.sand/vault.sand` | Encrypted index + cloud credentials, and the film database key if you set one. Back this up — though since every account also carries a `manifest.sand`, losing it is recoverable with your password. |
| `<archive-id>-pN.sand` | How parts appear on each account, inside whatever folder or prefix that account is configured with. The ID is random and reveals nothing. |
| `manifest.sand` | An encrypted copy of the index, on every account. Opens with your vault password alone. It carries the film details, since those are part of the index — but never the film database key, which stays in the vault file alone. So recovering from a backup restores your films and asks for the key again. |

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
