# Connecting accounts

Every file in Sand Vault is cut into encrypted parts, and each part goes to
one connected account. This page covers which backends exist, how to connect
them from the browser or the command line, naming and colouring them, telling
Sand Vault how much room they have, the hourly health ping, and repairing an
account whose credentials have stopped working.

Exact flags are in the [CLI reference](../reference/cli.md). How
the backends work inside is in [Providers](../design/providers.md). How to add
a new one is in [Adding cloud backends](../design/cloud-backends.md).

## Supported backends

| Kind | Works with | How you connect it |
|---|---|---|
| `gdrive` | Google Drive | Sign in |
| `onedrive` | OneDrive, personal or work | Sign in |
| `dropbox` | Dropbox | Sign in |
| `box` | Box | Sign in |
| `zoho` | Zoho WorkDrive, in the region the account lives in | Sign in |
| `s3` | Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO, any S3 API | Bucket, keys, and an endpoint for non-AWS |
| `webdav` | Nextcloud, ownCloud, pCloud, Koofr, Fastmail Files, anything behind `rclone serve webdav` | URL, username, app password |
| `drime` | Drime, through its API | An access token from the Drime web app |
| `filen` | Filen, through its end-to-end encrypted API | Email and password |
| `internxt` | Internxt Drive, through its end-to-end encrypted API | Email and password |
| `sftp` | Any machine with an SSH login: a VPS, a NAS, rsync.net, a Hetzner Storage Box | Host, username, and a key Sand Vault generates or your own |
| `protoncli` | Proton Drive, through Proton's own client | Sign in (a link you open anywhere) |
| `proton` | Proton Drive, through the folder its desktop app syncs | A path |
| `icloud` | iCloud Drive, through the folder macOS or iCloud for Windows syncs | A path |
| `mega` | MEGA, through the folder its desktop app syncs | A path |
| `jottacloud` | Jottacloud, through the folder its desktop app syncs | A path |
| `synccom` | Sync.com, through the folder its desktop app syncs | A path |
| `tresorit` | Tresorit, through a synced tresor or Tresorit Drive | A path |
| `icedrive` | Icedrive, through a synced folder or its mounted drive | A path |
| `local` | Any directory: an external disk, a NAS mount, a sync folder | A path |

Every backend is written against the standard library. SigV4 signing, OAuth
and Microsoft Graph's chunked uploads included, there are no cloud SDKs and no
CGO, so the artifact stays one static binary.

A service not named here still works if it speaks S3 or WebDAV: type its
endpoint or URL. In the browser, **+ Connect a cloud → Not here? See the full
list of services →** opens a searchable catalogue of every service each
backend reaches (Google Cloud Storage, Storj, Seafile, Yandex Disk and more),
with the endpoint each one wants. It is drawn from the same registry as the
connect form, so it cannot list something the code cannot do.

## Connecting an account

**In the browser:** press **+ Connect a cloud**, pick the service, and fill in
the form. It is generated from the backend's own spec, the same one
`sand remote kinds` prints. The account is pinged before it is saved, so
credentials the service refuses are never stored.

For the backends that take a path, the field has a **BROWSE…** button. It walks
the folders of *the machine Sand Vault runs on*, which is rarely the one you
are holding. It opens at the first folder it can read, with the usual mount
roots one tap away. The folder does not have to exist yet: name a new one
inside the folder you picked, and connecting creates it.

**From the command line:**

```bash
sand remote kinds                       # every backend, its settings, which are required or secret
sand remote add local  --name usb-drive --set path=/media/usb/sand
sand remote add s3     --name r2-cold \
    --set bucket=sand-shards \
    --set endpoint=https://<account>.r2.cloudflarestorage.com \
    --set access_key_id=… --set secret_access_key=…
sand remote list                        # NAME KIND STATUS PARTS STORED FREE COLOUR ID
sand remote test r2-cold
```

`--name` defaults to the backend's name. Two accounts may never share a name.
`sand remote` also answers to `sand account` and `sand cloud`.

On the systemd service, a Local folder outside `/media`, `/run/media`, `/mnt`
and `/srv` must be granted first. See
[Local folders under systemd](deployment.md#local-folders-under-systemd).

### Signing in to Google Drive, OneDrive, Dropbox, Box and Zoho

Press **+ Connect a cloud**, pick the provider and approve on its consent
screen. Zoho also asks which region the account lives in. The server
exchanges the code for tokens itself, names the account after whoever signed
in, and stores the tokens in the encrypted vault. The browser never sees a
token.

**Sand Harbor ships no registered OAuth apps.** There is no central service to
register them against, and a client secret baked into a public binary is not
a secret. So the first connection to a provider asks for a client ID from
*your own* app registration. The dialog links to the provider's console, says
what to create, and shows the exact redirect URI to paste. Later accounts on
that provider reuse it. To skip the step, give the service the credentials
once:

```bash
SAND_GOOGLE_CLIENT_ID=…      SAND_GOOGLE_CLIENT_SECRET=…
SAND_MICROSOFT_CLIENT_ID=…   SAND_MICROSOFT_CLIENT_SECRET=…   # secret optional (public client)
SAND_DROPBOX_APP_KEY=…       SAND_DROPBOX_APP_SECRET=…
SAND_BOX_CLIENT_ID=…         SAND_BOX_CLIENT_SECRET=…
SAND_ZOHO_CLIENT_ID=…        SAND_ZOHO_CLIENT_SECRET=…
```

Under systemd, put these in a drop-in (`systemctl edit sand`, then
`Environment=…` lines) so upgrades keep them.

**The redirect URI must be one the provider accepts.** It defaults to the
address you are using, `http://<host>:8123/api/providers/oauth/callback`, and
providers are picky about plain HTTP on anything but loopback. Behind a proxy,
or to use one fixed URI, pin it:

```bash
SAND_OAUTH_REDIRECT=https://vault.example.com/api/providers/oauth/callback
```

If the redirect cannot reach the server at all, say you are signing in from a
phone against a server bound to `127.0.0.1`, the dialog takes the URL the
browser was left on and finishes the exchange from that.

A sign-in in progress lives only in memory, for 15 minutes. The redirect is
matched to it by an unguessable `state` parameter, because the session cookie
is `SameSite=Strict` and deliberately does not survive the cross-site
navigation. Box and Microsoft retire a refresh token when it is used, and the
replacement is written back into the vault each time.

The CLI takes the same credentials directly. This is also how to move an
account between machines:

```bash
sand remote add onedrive --name onedrive-personal \
    --set client_id=… --set refresh_token=… --set folder=sand
```

### SSH / SFTP

Nothing has to be installed on the far end: if you can `sftp` to a box, it can
hold parts. Name a folder for the parts, then pick **SAND MAKES ONE** and
**Generate a key pair**. You get an Ed25519 key and one line to add to the
account's `authorized_keys`, with the command that appends it. The private
half is never shown in the browser and never leaves the server; it goes
straight into the encrypted vault. **I HAVE A KEY** takes a pasted OpenSSH
private key (and its passphrase, if any) instead. A password works too, for a
box whose console will not take a key.

The host key is always checked. The first connection learns the server's
fingerprint, and every later one must match it. A different key is refused
with both fingerprints shown, because a rebuilt VPS and an impostor look the
same from here. If you rebuilt it, clear the stored fingerprint and reconnect.
To have even the first connection checked, run
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the server and paste the
`SHA256:…` part into the form's advanced settings. Details are in
[SFTP](../design/sftp.md).

A VPS is **one** account however many folders you point at it. Two parts of a
file on one machine is one machine that can rebuild the file.

### Synced-folder services

Proton Drive (the `proton` kind), MEGA, Jottacloud, Sync.com, Tresorit,
Icedrive and iCloud Drive publish no API a third party can use. What they do
have is a desktop app that keeps a folder in step with the account. Sand Vault
writes its parts, already encrypted, into that folder, and the app carries
them up. The app must be installed and signed in on the machine Sand Vault
runs on.

- Connecting a folder whose *parent* does not exist is refused rather than
  created. A folder no app syncs would accept every part and upload none.
- The upload "succeeds" once the file is on local disk. If the app is signed
  out, the account looks healthy while holding nothing.
- **iCloud** may evict a part's contents to save disk. Sand Vault asks iCloud
  for it and waits, so a read can take a moment on a Mac short of space, and a
  health check reports an evicted part as present but unweighed. The folder
  must be inside iCloud Drive.
- On a headless box, several of these have an rclone backend you can put
  behind `rclone serve webdav` and connect as `webdav`.

### Proton Drive

The `protoncli` backend is the better Proton route wherever it can run. It
drives `proton-drive`, the client Proton builds on
[its own Drive SDK](https://github.com/ProtonDriveApps/sdk). It works with no
desktop app. Its upload confirms the part reached *Proton*, not just a local
folder. And it can report what the account holds.

It needs that client on `PATH`, or named with `--set binary=…`. The
[Linux quick start](installation.md#protons-client) builds or downloads it.
Without it, the backend still appears in the connect dialog and says what is
missing when you try to use it.

Signing in is a link, not a redirect: the client prints one and waits. **Open
it on any device**, a phone included, which is how a headless box connects.
No password reaches Sand Vault.

```bash
sand remote proton login --name proton --folder /my-files/sand   # a new account
sand remote proton login proton                                  # sign an existing one back in
```

Proton keeps its session in the OS secret store, which a system service has no
access to. Sand Vault therefore keeps the session in the vault and writes it
to a private per-account file under `SAND_PROTON_STATE_DIR` only while a
command runs. The quick-start unit sets that variable to `/var/lib/sand/proton`.
A unit without it falls back to the user cache directory, which the systemd
sandbox leaves read-only, so the client cannot run. Add the variable in a
drop-in if you installed some other way.

Proton's cryptographic model changes at the end of 2026. Because Sand Vault
runs Proton's own binary, an updated client handles the migration.

### Filen and Internxt

Both are end-to-end encrypted, and Sand Vault speaks their APIs directly. It
signs in with the account's email and password and derives the account's keys
on this machine: PBKDF2 or Argon2 for Filen, a recovery phrase unlocked from
the password for Internxt. Each part is then encrypted the way their own
clients do before it leaves. The password is used once. What the vault keeps
is the signed-in session and the derived keys, so the account works headless
with no slow derivation on each start. An account with two-factor
authentication needs its current code for that first sign-in only.

## Moving your own files to a machine

This is a different feature from an SFTP account. **⇅ Machine** in a folder
connects a machine you have an SSH login on, to move *your own files*:

- **BRING IN** browses the machine and imports what you pick into the folder.
  The files are compressed, split, encrypted and scattered like any upload, to
  the vault's default accounts (**Change** picks others for this import only).
- **SEND OUT** writes vault files to a folder on the machine **whole and in the
  clear**, under their own names. A folder's menu has **Send to a machine** for
  the common case.

A machine entry and an account are kept apart even for the same box: an
account is where encrypted parts go, a machine is where your files go, and
never the other way round. Each has its own key.

Bytes go between the machine, the server and your accounts, never through the
browser, and only the piece in flight is held in memory. Files keep their
dates in both directions. Nothing outside the folder the machine is scoped to
can be read or written, including through a symlink. Nothing is deleted on
either side.

If a transfer is interrupted, every file that arrived whole is kept. Run the
same transfer again and it skips those. A file cut off partway starts again
from its first byte. Sending out never overwrites an existing file unless
**Replace files already there** is ticked.

**Keep going if I close this page** (on by default) runs the import on the
server with no browser attached. Reopen the machine to see progress or stop
it. A restart of the server ends it; run the import again to carry on.

## Naming an account and choosing its colour

Every account wears a colour: the stripe down its card, every part badge for a
file it holds, and its row in the upload picker. That is what lets you see at
a glance which clouds a file is on.

**Edit** on the account's card opens a dialog. Its first tab is the name and
the colour: twelve named colours, **All shades** for the full palette (the same
hues in three shades each), a free picker, or **Automatic** to let the browser
choose. A swatch another account already wears is marked. Every colour is
light enough to carry the dark part-number text drawn on it.

```bash
sand remote edit r2-cold --name r2-archive
sand remote edit r2-cold --color '#38bdf8'
sand remote edit r2-cold --color auto
```

None of this reaches the cloud. Nothing is uploaded or re-encrypted, and no
part moves. A rename is written through the index, so the file list, health
panel and any recovery from `manifest.sand` all show the new name.

## How much room is left

Each account card, and each cloud in the upload picker, shows how much of the
vault it holds and how much room is left, with where that figure came from:

| Source | Where the number comes from |
|---|---|
| **reported** | The account itself: a drive's quota, a filesystem's free blocks. Live |
| **declared** | A capacity you typed, minus what `sand remote measure` counted there. For buckets, since S3 has no quota call |
| **quota** | A line *you* drew, minus what the index says Sand Vault put there. Works on any backend |

With two sources, the room left is whichever leaves less.

```bash
sand remote edit b2-cold --capacity '10 GB'   # how big the bucket is
sand remote edit b2-cold --capacity none
sand remote measure b2-cold                   # list it and count; costs one request per thousand objects
sand remote edit gdrive --quota '200 GB'      # how much Sand Vault may fill
sand remote edit gdrive --quota none
```

`measure` (**Count it** in the account's Stats panel) is never run on a
schedule, because some providers bill for listing. Its figure lasts until the
vault closes.

A quota is about *your share*, not the account's size, so it is offered on
every account. With one set:

- the picker ranks clouds by room, and checks *per cloud* that the share each
  will receive fits (a 2-of-3 file puts about half its size on each);
- an account that reports nothing gets a usage bar;
- an upload that crosses the quota **still stores and warns**. The parts of a
  file are placed together, and dropping the one that crosses the line would
  leave the file less durable than its code promises. The warning appears once,
  on the file that crossed. The card, the Stats panel and the `FREE` column of
  `sand remote list` keep showing "over" until you raise the quota or move
  files off.

A versioned bucket, such as Backblaze B2 with "keep all versions", holds old
copies you cannot see. `sand remote edit <name> --auto-prune on` erases them
daily while the server runs. See [Maintenance](maintenance.md).

## Are the clouds still there?

A file reads fine with one cloud gone, because it needs only `k` of its `n`
parts. So a revoked refresh token, a rotated key or a NAS that never came back
after a power cut breaks nothing visible, right up until a second one goes.
For that reason the server pings every account itself, **hourly by default,
while the vault is unlocked**. The foot of the accounts panel shows the
result:

```
2671    580.4 GB                  ● 1 of 17 unhealthy
FILES   IN THE VAULT                 checked 12m ago
```

Press it for **Cloud health**: every cloud worst-first, what the failing ones
said, and how long each has been failing. **Check now** re-checks after a fix.

A check is one small request per cloud. Nothing is listed or downloaded, so it
costs nothing at providers that bill per listing. Whether a particular file's
parts are still where the index says is a different question; a folder's
standing checks answer it ([Maintenance](maintenance.md)).

Change the interval (15 minutes, hourly, 6 hours, daily, or off) under **Vault
settings → Cloud health**, or:

```bash
sand remote health              # check now and print the result
sand remote health --every 6h
sand remote health --off
```

Off is a real setting, for a metered connection or a laptop that sleeps most
of the day. Accounts are still pinged whenever they are listed.

- **Only while unlocked.** The credentials are in the encrypted index. A slot
  missed while locked is not lost: the next unlock checks at once if one is
  due. A machine meant to keep the schedule wants a long `--idle-timeout`.
- **A check never counts as activity**, so it never stops the vault from
  auto-locking.
- **Results stay in memory.** They are not written to the vault, and they go
  when it locks. A change of state is logged once:

  ```
  cloud health: Elements (dial tcp 192.168.1.40:445: no route to host) — 1 of 17 not answering
  cloud health: Elements answering again
  ```

- **Disk health** in the same panel adds SMART warnings for Local folder
  accounts on a failing disk. See [Disk health](deployment.md#disk-health-smart).

`GET /api/providers/health` returns the same data from memory, and
`POST /api/providers/health/check` and `/health/schedule` run a check and set
the schedule ([API](../reference/api.md)).

## Repairing an account that has stopped answering

When credentials fail (a rotated key, an expired refresh token, a deleted
OAuth client, revoked consent), the card goes red and says why. **Disconnect**
is the wrong fix: it forgets every part the account holds. Use **Edit → How it
connects** instead:

- **Sign in again** (OAuth clouds) runs the consent screen again and replaces
  the stored tokens. The OAuth app the account was connected through is reused,
  client secret included, read from the vault and never sent to the browser.
  If that app is gone, enter a new client ID here.
- **The settings themselves**: the connect form, filled in from the current
  connection. Secret fields start empty and read *unchanged*. Leave one empty
  to keep the stored secret; type into it to replace it.

Whatever you change is connected with and pinged before it is saved. Settings
the provider rejects are refused, and the account keeps what it had. Its ID,
name, colour, parts and index rows all survive.

```bash
sand remote edit s3-cold --set secret_access_key=…    # a rotated key
sand remote edit gdrive  --set refresh_token=1//0g…   # a re-pasted token
sand remote edit s3-cold --set endpoint=https://…     # a bucket that moved
sand remote edit s3-cold --set prefix=                # back to the default
sand remote proton login proton                       # a signed-out Proton account
```

> **Re-pointing is not repairing.** Changing the bucket, folder, prefix or path
> does not carry existing parts across, and Sand Vault will look for them in
> the new place and not find them. Move the parts yourself first, or run
> `sand check --all` afterwards and let the missing parts be rebuilt
> ([Maintenance](maintenance.md)).

## Disconnecting

```bash
sand remote remove r2-cold            # refused if it would leave files unrecoverable
sand remote remove r2-cold --force
```

Move files off an account before disconnecting it
([Storing files](storing-files.md)). Once its parts are forgotten, any file
that needed them is down one part, or gone.
