# Changelog

Releases are `vMAJOR.MINOR.PATCH`, where the patch number is the repository's
commit count — `v2.0.42` is the 42nd commit on the 2.0 line. See
[`internal/version/version.go`](./internal/version/version.go).

Each section below is the body of the corresponding GitHub release. A heading
must name the tag exactly — a tag whose commit builds a different version is a
tag that shouldn't be published.

## v2.0 — the multi-cloud file browser

The project is now **SAND Vault**, and it stops being a batch tool: it becomes
a file browser over storage you don't fully trust.

Before, you fed it files and got three zip archives back, then carried them to
three places yourself. Now you connect your cloud accounts once and it does the
carrying: every file you add is compressed, split into three encrypted parts,
and scattered across **separate** accounts. Opening a file gathers the parts
back and rebuilds it in memory. No single provider ever holds your data.

The old workflow is still there — `sand archive` and `sand restore` need no
vault and no accounts — and v1 parts still restore, though the part format has
moved on (see below).

Parts are named `.sand` rather than `.media`, and land as a flat
`<archive-id>-pN.sand` inside whatever folder or prefix an account is
configured with, instead of a nested `sand/<archive-id>/pN.media`. Every
backend already scopes SAND to a place of its own, so the extra folder only
buried each part two levels deeper — and going flat means Google Drive, which
has no paths at all, ends up with exactly the same part names as everywhere
else. Standalone mode follows the same scheme: `sand archive` writes
`sand-p1.zip`, `sand-p2.zip` and `sand-p3.zip`, holding one
`<filename>.pN.sand` per input file. Files already in a vault keep working —
each part's key is recorded in the manifest when it is written, so parts stored
under the old layout are still found where they are.

### The part format no longer says what it holds

Until now a part's header carried the original filename, the plaintext SHA-256
and the original size **in the clear**. A provider — or anyone who got hold of a
single part — could read the name of every file it had a part of, which is not
what the design claimed. Format version 2 moves all of it inside the AES-GCM
payload. What remains in the clear is only what a reader needs to derive the key
and open the part: magic, version, part number, the random archive ID, the
Argon2id parameters and the nonce.

Version 1 parts are still read, so nothing already stored has to be re-uploaded
— but they still carry their names, so re-upload anything whose *filename* is
itself sensitive. Nothing writes version 1 any more, which means a part written
today cannot be opened by a v1 binary.

### Losing the vault file is survivable

Parts are encrypted under a random key that existed in exactly one place: the
vault file. Lose it and your password bought you nothing — every part in every
account stayed permanently opaque.

Now every connected account carries `manifest.sand`: the file tree, the map of
which account holds which part, and the key those parts are encrypted under,
sealed under your vault password. It carries its own key-derivation parameters
rather than the vault's, because the vault is exactly what a reader in this
situation has lost, so **the password alone opens it**. It never contains the
credentials for any account — a copy sits in every account, and including them
would make one break-in a master key to the rest.

Three ways back:

- `sand manifest ls manifest.sand --long` prints the tree and every part's
  location, from the file alone.
- `sand restore --parts A,B --manifest manifest.sand` rebuilds a complete file
  offline — no vault, no accounts, no network.
- `sand vault recover` rebuilds the whole vault into a fresh one. Reconnected
  accounts get new internal ids, so it asks each account what it actually holds
  and re-points every shard record at whichever one answers.

The backup is on by default and rewritten whenever the index changes, in the
background so it never delays an upload. `sand vault backup --disable` erases
every copy.

**What it costs, plainly.** Every copy is one password away from the data key.
An attacker who compromises one account *and* guesses your password gets the
tree, the placement map, and — because each part is separately encrypted under
that key — whatever plaintext that account's own parts hold, which for a large
file is about half of it. Rebuilding a whole file still needs a second account,
so the two-of-three split is still a real second factor. The exception is the
redundant policy with fewer than three accounts, where one account can already
rebuild a file on its own; SAND refuses to write a backup there at all. An
account already holding a *different* vault's backup is also left alone, so
connecting an account to a second vault cannot destroy the first one's way
back.

### Changing your password now changes what protects your files

`sand vault passwd` used to re-wrap the data key and say, truthfully, that
nothing had to be re-uploaded. That was the wrong trade. The parts on your
accounts were still encrypted under the same key, so anyone holding the old
password and an old copy of the vault file — or of the `manifest.sand` sitting
on every connected account — could still read every one of them. The password
had changed; what it protected had not.

Now a password change mints a **new** data key and rebuilds every stored file
onto it: each file is gathered from its parts, re-encrypted, scattered again,
and the parts the old key opened are erased from the accounts.

That is a download and an upload per file, which cannot be one atomic act, so
the vault holds more than one data key while it runs. The password change itself
is a single write — new key minted, old key kept beside it, every section
re-sealed under the new password — and each file then moves on its own, with the
manifest recording which key generation each one answers to. A retired key is
dropped the moment no file names it, whether the last file on it was migrated or
deleted.

So the password is genuinely changed the second the command returns, every file
stays readable the whole way through, and an interruption costs you nothing but
the files that had not moved yet:

- `sand vault migrate` picks up whatever is left — after an interruption, or
  after an account that was offline held its files back. Running it again when
  there is nothing to do is free.
- `sand vault status` and the accounts panel in the browser both say how many
  files are still on the old key, and the panel offers to finish the job.
- `sand vault passwd --no-migrate` changes the password now and defers the
  re-encryption. Until it runs, the old password and an old copy of the vault
  file still open whatever has not moved.

A manifest backup written while a migration is in flight carries every key
generation it describes, so a vault lost halfway through still recovers all of
its files, and `sand restore --manifest` picks the right key per file.

### Connected cloud accounts

Eight backends behind one small object-store interface: **Google Drive**,
**OneDrive**, **Dropbox**, **Box**, **S3-compatible** (Amazon S3, Cloudflare
R2, Backblaze B2, Wasabi, MinIO), **WebDAV** (Nextcloud, ownCloud, pCloud,
Koofr, Fastmail, or anything behind `rclone serve webdav`), **Proton Drive**
through the folder its desktop app syncs, and a plain **local folder**.

All of it is written on the standard library — SigV4 request signing, OAuth and
Microsoft Graph's chunked upload sessions included — so there are no cloud
SDKs, no CGO, and the deployable artifact is still one static binary. Each
backend declares the fields it needs, and the browser's connect form is
generated from that declaration, so adding a backend needs no frontend change.

### Connecting an account without leaving the app

The four OAuth backends are connected by signing in. Pick the provider, approve
the request on its own consent screen, and the account is connected: the code
is exchanged for tokens **on the server**, the connection names itself after
whoever signed in, and the credentials go straight into the encrypted vault.
The browser never handles a token, and nothing is copied by hand.

The redirect comes back as a cross-site navigation, which the `SameSite=Strict`
session cookie deliberately does not survive, so it is matched to the sign-in
that started it by an unguessable `state` parameter instead — bound to a flow
that lives in memory for fifteen minutes and is retired the moment it is spent.
Blocked popups fall back to taking over the tab and resuming on return, and a
redirect that cannot reach the server at all — a phone signing into a vault
bound to loopback — can be finished by pasting the URL the browser was left on.

SAND registers no OAuth apps of its own; a credential baked into a public
binary is not a secret. The first connection to a provider collects a client ID
from your own app registration, with a link to the console and the exact
redirect URI to paste into it, and every later account reuses it. Hand the
service `SAND_GOOGLE_CLIENT_ID`, `SAND_MICROSOFT_CLIENT_ID`,
`SAND_DROPBOX_APP_KEY` or `SAND_BOX_CLIENT_ID` (with secrets where the provider
demands one) and that step disappears entirely. `SAND_OAUTH_REDIRECT` pins the
redirect URI for instances behind a proxy.

Box and Microsoft retire a refresh token as it is spent. SAND writes the
replacement back into the vault as it goes, so an account connected once keeps
answering.

### The vault

A single encrypted file (`~/.sand/vault.sand`, or `/var/lib/sand/vault.sand`
under systemd) holding the index of what you've stored, where each part went,
and the credentials for every connected account. Filenames and folder structure
are themselves sensitive, so none of it is written in the clear.

File parts are encrypted under a random 256-bit data key that is itself wrapped
by your password. Changing your password re-wraps 32 bytes instead of
re-uploading everything, and part encryption doesn't inherit a weak password.

**Back this file up** — though it is no longer the only record: every connected
account carries an encrypted copy of the index, so a lost vault can be rebuilt
from any one of them with your password. See "Losing the vault file is
survivable" above.

### Placement is a security decision

Any two parts plus the key rebuild a file, so two parts on one account means
that account could rebuild it. The default **strict** policy never allows that,
and refuses to upload when only one account is connected. **Redundant** is
offered for people with fewer accounts and says plainly what it costs.

Uploads scatter concurrently and commit at two-of-three, rolling back the parts
that did land if fewer than two succeed. Reads race all three accounts and
finish on the first two to arrive, so an offline account costs nothing on the
read path.

### The browser

Lock screen, a sidebar of connected accounts with live status and how much each
is holding, breadcrumbs, drag-and-drop upload with progress, part badges
coloured per account, a per-part health read-out, and inline preview for
images, video, audio, PDF and text — each one rebuilt on demand.

It loads no external fonts, scripts or styles: opening your vault makes zero
third-party requests.

Hashed assets are cached for a year and `index.html` revalidates against an
ETag, so an upgrade actually reaches a browser that has opened the vault before.

The layout folds on a phone rather than shrinking: under 860px the accounts
sidebar becomes a drawer, the file table's columns give way to stacked rows, and
controls take touch-sized targets. Heights track the visible viewport, so a
collapsing address bar never sits over the last row of files.

Added to a home screen it goes on as an app: the SAND mark instead of a
screenshot of the page, and a launch without browser chrome. The mark ships in
every form the two platforms ask for — `apple-touch-icon` for iOS, a web app
manifest with 192px, 512px and maskable icons for Android — all rendered from
the one `icon.svg` by `scripts/make-icons.mjs`, and all served from the binary,
like everything else the browser loads.

### Quick start

One command installs SAND as a hardened systemd service, building from source
or installing a prebuilt binary:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh | sudo bash
```

Re-run it to upgrade. It snapshots the vault before swapping code in, builds
the new version while the old one keeps serving, and rolls back — code *and*
vault — if the new one fails its health check.

### Local folders under the sandbox

The unit's `ProtectSystem=strict` makes everything outside its granted paths
read-only to the service, so connecting a **Local folder** on an external disk
failed with a bare `read-only file system` — a true statement about a drive
that was mounted read-write and perfectly healthy.

The unit now grants the roots removable disks and network shares are mounted
under — `/media`, `/run/media`, `/mnt`, `/srv`, each with the `-` prefix that
lets the service start when the drive is unplugged — so a disk in the usual
place connects with no extra step, while `/etc`, `/usr`, `/home` and the rest
stay read-only. `SAND_MOUNT_ROOTS` changes the list at install time.

For a vault folder outside those roots, the three ways a local folder actually
fails now say which one it is, and what to do about it: a sandboxed service
(`INVOCATION_ID` is set), a folder owned by another user, or a genuinely
read-only mount (checked against `/proc/self/mounts` rather than guessed).
`scripts/allow-local-path.sh` grants a path to the service through a drop-in
that upgrades do not touch, skips paths the unit already covers, warns when
ownership or the mount would defeat the grant anyway, and `SAND_LOCAL_PATHS`
does the same at install time in both installers.

### Versioning

The patch number is now the repository's commit count, assembled in one place
([`scripts/version.mjs`](./scripts/version.mjs)) and stamped into both the Go
binary and the web bundle, so the header, `sand version` and `/api/health` can
never disagree. A patch of `0` marks an unstamped build — including one made
from a shallow clone, which is detected rather than guessed around.

### Reachable by default

`--bind` and the installers' `HOST` default to `0.0.0.0`, so a fresh install is
reachable from your network without a second step.

Understand what that exposes. This server is the one component that ever holds
plaintext: it takes your vault password over the wire and sends rebuilt,
decrypted files back, and `/api/vault/unlock` answers anyone who can reach the
port, with no rate limiting behind it. On plain HTTP all of that is in the
clear. Put TLS in front of it — Tailscale Serve, or `scripts/nginx-sand.conf` —
or set `HOST=127.0.0.1` to keep it on loopback. The server warns on every
non-loopback bind.

### Also

- `cmd/sand` is present. The Makefile and the e2e suite both referenced it, but
  it had never been committed, so the tree did not build.
- New CLI: `vault`, `remote`, `ls`, `put`, `get`, `mkdir`, `mv`, `rm`, `check`.
  `sand check --all` exits non-zero on a degraded or unrecoverable file, which
  makes it a reasonable cron job.
- `scripts/deploy-linux.sh` writes a unit the service can actually run — it
  previously combined `ProtectHome=yes` with no vault path, so the server had
  nowhere to write.
- Sessions with idle auto-lock, `SameSite=Strict` cookies and `Origin` checks
  on every write; stored HTML and SVG are served as downloads rather than
  rendered in the app's origin.

## v1.0 — split, encrypt, distribute

The original: compress with zstd, split in two, generate a third XOR redundancy
part, encrypt all three with AES-256-GCM under an Argon2id-derived key, and
write three zip archives. Any two of the three reconstruct the original. CLI
and an embedded web GUI, shipped as one static binary.
