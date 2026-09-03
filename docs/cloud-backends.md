# Adding cloud backends

A running list of the storage SAND could hold parts on, what each one would
actually cost to add, and the recipe for adding it. Written down because the
question *"can it do X?"* has three very different answers depending on X, and
because the answer for most of the popular ones is already **yes, with a
five-line change**.

The order below is by cost, not by how well known the service is.

---

## What makes a candidate worth adding

A vault's guarantee is that no single account holds anything meaningful. That
guarantee is only as good as the *independence* of the accounts it is spread
across: three buckets at the same company, on the same card, behind the same
password reset, are one account wearing three hats.

So the thing a new backend buys is not a logo. It is a party that can go dark —
be suspended, be subpoenaed, raise its prices, lose a region — without the two
others noticing. Judge each candidate on that, and the picture changes: adding
**Cloudflare R2** as a preset next to Amazon S3 is worth more than adding a
sixth OAuth provider that most people would sign into with the same Google
account they already connected.

Second, and only second: reach. iCloud Drive is worth real work because a
great many people have it and have never opened a storage console in their
life.

---

## Tier 0 — a preset, no Go code

`s3.go` and `webdav.go` are generic. Anything speaking either protocol already
works today by typing an endpoint or a URL. A `Preset` just spares the typing:
five lines in the backend's `Specs` block, and the connect dialog grows a
button, because the dialog is rendered from the registry rather than written by
hand.

```go
{
    Key:    "storj",
    Label:  "Storj",
    Help:   "Create S3 credentials in the Storj console; the gateway is regional.",
    Values: map[string]string{"endpoint": "https://gateway.storjshare.io", "region": "us-1"},
},
```

**S3 API — worth presetting:** Google Cloud Storage (the XML interoperability
endpoint at `storage.googleapis.com` with HMAC keys — a hyperscaler already
supported and nowhere advertised), Storj, DigitalOcean Spaces, Scaleway, OVH,
IDrive e2, Hetzner Object Storage, Linode/Akamai, Filebase, Tigris, Vultr,
Contabo, Oracle OCI, Seagate Lyve.

**WebDAV — worth presetting:** Seafile, Yandex Disk, Infomaniak kDrive, Hetzner
Storage Box, Synology and QNAP NAS boxes, OpenDrive, Zoho WorkDrive.

A preset is for the services worth a button. Everything else the protocol
reaches belongs in the backend's `Covers` list, which is what the browser's
catalogue window is drawn from — one line per service with the shape of the
endpoint it wants. Adding a name there costs nothing in the connect dialog,
which is the point: the dialog stays fourteen entries long while the catalogue
answers *does it do Wasabi?*

Verify before adding either: endpoints move, and a preset that fills the form
with a dead hostname is worse than an empty field. Prefer a shape
(`https://s3.<region>.wasabisys.com`) over a hostname in a `Covers` hint — it
stays true when a provider adds a region. Check the region placeholder is
obviously a placeholder (`<account>`, not a real account ID) and that `Help`
says where the credentials are minted.

---

## Tier 1 — a folder some client already syncs — **done**

Applies to any service with a desktop client and no usable API: the parts are
encrypted before the client ever sees them, so *"a folder that syncs"* and *"an
account that answers HTTP"* are the same arrangement from the vault's point of
view.

All of it now lives in one table in `syncfolder.go` — **Proton Drive** (which
also has a tier-4 backend of its own, below, and that is the better of the two
wherever it can run), **MEGA**, **Jottacloud**, **Sync.com**, **Tresorit**,
**Icedrive** — where a service is a dozen lines: a kind, a label, a description, and a function
guessing where this machine's client put its folder. **iCloud Drive** stayed in
`icloud.go`, for the reason below. Adding another service means adding a row:

```go
{
    kind:        KindFilen,
    label:       "Filen",
    description: "Your Filen folder, kept in step by the Filen desktop app. …",
    docsURL:     "https://docs.filen.io/",
    order:       37,
    fieldLabel:  "Filen folder",
    fieldHelp:   "A folder inside the one Filen syncs. …",
    folders: func(home string) []string {
        return []string{filepath.Join(home, "Filen")}
    },
},
```

Everything shared sits around the table: the form is generated from it, the
default path prefers a folder that actually exists, and `Ping` refuses a folder
whose *parent* is missing — a folder no client syncs would accept every part
and upload none of them, which is the one failure mode of this whole tier and
is invisible until the day the parts are needed.

**MediaFire is deliberately not in the table.** It was on the original list,
but its desktop sync client was retired years ago, so there is no folder on any
machine for SAND to write into: a backend for it would be a menu entry that
cannot be made to work. If a tool appears that presents MediaFire as WebDAV,
that is the route, and it needs no code here at all.

The one thing to check per service, and the reason `icloud.go` is 300 lines
where a table row is a dozen: **does the client evict?** A client that keeps
every synced byte on disk needs nothing beyond the local backend, and so does a
virtual drive that fetches a file when something reads it — Tresorit Drive and
Icedrive's mount both behave that way, which is why they are rows rather than
files. What needs its own backend is a client that reclaims space by replacing
a file with a placeholder *under a different name* — iCloud Drive, OneDrive's
Files On-Demand, Dropbox Smart Sync — handing the vault a folder where a part
written in March is a stub in June under a name nobody stored. See "How iCloud
Drive handles eviction" below for the shape of the fix.

Still open: **Filen** (which also has an API worth a tier-3 look), **Degoo**,
and whatever else ships a client but no usable API. Check the service does not
speak WebDAV first — Infomaniak kDrive and Seafile both do, which makes them
tier-0 presets rather than rows here, and a preset needs no desktop app on the
machine at all.

---

## Tier 2 — a new credential form (~300–450 lines)

A backend of its own: a `Spec` with fields, a struct, six methods, its own
signing. No SDK — everything here is `net/http` and `crypto` out of the
standard library, which is what keeps the artifact one static CGO-free binary.

**Done: SFTP.** One backend covers rsync.net, BorgBase, Hetzner Storage Box,
every VPS and every NAS, and nothing has to be installed on the far end. It
came in cheaper than this page estimated — `golang.org/x/crypto` was already a
direct dependency, so `ssh` was a new import path rather than a new module.
See [`sftp.md`](sftp.md), which also covers the half that is designed and not
yet built: browsing a machine and importing files off it into a vault folder.

| Candidate | Why | Notes |
|---|---|---|
| **Azure Blob Storage** | The biggest real gap — the third hyperscaler, and the only one with no S3 face | SharedKey signing, same exercise as the SigV4 code in `s3.go` |
| **Backblaze B2 native** | — | Low value: the S3 preset already covers B2, and B2's own API reports no bucket size either — usage there is counted by listing and measured against a declared capacity (`UsageMeasurer`, `Config.Capacity`). B2's "keep all versions" default is handled through the S3 face too: `Versioner` lists and erases the versions a bucket keeps beneath what it shows (`sand vault prune`) |

---

## Tier 3 — a new sign-in (~350–500 lines)

`oauth.go` already carries the authorize URL, PKCE, the code exchange, refresh,
and rotation of refresh tokens that providers retire on use. A new OAuth
backend is one file: an `OAuthSpec` in the registration, and the six methods
against the provider's REST API.

- **pCloud** — a real API instead of the WebDAV preset, with quota reporting
- **Yandex Disk**, **Koofr**, **Egnyte**
- **Jottacloud** — already connectable as a synced folder; an API backend
  would drop the desktop-app requirement and report quota
- **Seafile** — token auth rather than OAuth, self-hosted
- **SharePoint / Graph document libraries** — mostly `onedrive.go` pointed at a
  different drive ID; opens up work accounts

When adding one, the parts that are easy to get wrong are already solved
elsewhere: copy `box.go` for a provider that rotates refresh tokens, and
`gdrive.go` for one where the scope must stay narrow enough that SAND can only
ever see what it created.

---

## Tier 4 — driving the service's own client — **done for Proton**

The tier that exists because tier 1 has a hole in it. A synced folder is only a
backend where somebody has installed a desktop app, and the machine SAND is
usually installed on is a server with no desktop at all. Worse, the folder
backend cannot tell the difference: its `Put` returns as soon as the file is on
local disk, so an account whose client is signed out or never installed looks
perfectly healthy while holding nothing.

Where a service ships a *command-line* client, that hole closes. `protoncli.go`
drives `proton-drive`, the CLI Proton builds on
[its own Drive SDK](https://github.com/ProtonDriveApps/sdk): `filesystem
upload` for `Put`, `download` for `Get`, `info` for `Stat`, `list` for `List`,
`delete` for `Delete`, all with `--json`. It is roughly 500 lines and no
cryptography.

**Why not speak the API.** Proton publishes no Go SDK — the native ones are
TypeScript and C#, and the Kotlin and Swift bindings wrap the C# one. The SDK
excludes authentication, session management and the address provider outright,
so an implementation would carry Proton's login as well as its API. And
Proton's cryptographic model changes at the end of 2026, after which clients
implementing only the old one stop interoperating. Driving Proton's binary puts
that migration on Proton.

**Where the session lives, and why it is not simply on disk.** Proton's client
keeps its session in the OS secret store. A systemd service with no session
bus, no keyring and no home cannot reach one. The client's other options are
`pass`, which needs a GPG key that would sit unlocked beside the thing it
protects, and a plaintext file, which Proton labels for testing only — fairly,
since the session holds the password that unlocks the account's key material
and not merely an access token.

So SAND keeps it where it keeps every other cloud credential: in the vault,
encrypted under the vault password. The plaintext file is the *handover*
between the two and not a home — written 0600 immediately before a command,
removed immediately after, in a directory only the service user can read:

```
vault ──decrypt──▶ $SAND_PROTON_STATE_DIR/<account>/auth-session.json
                     proton-drive filesystem upload …
vault ◀──encrypt── (rotated session read back)
                     file removed
```

Reading it back matters as much as writing it. Proton rotates the session as it
is used, exactly as Box and Microsoft rotate a refresh token, and the existing
`CredentialRotator` sink is what carries the new one to the vault. A rotation
that is dropped leaves the account working until the token it still holds
expires, and then signed out for good — the kind of failure that arrives weeks
after the mistake.

**The rest of the shape**, for anyone adding a second one of these:

- *One invocation at a time per account.* The client keeps SQLite caches in its
  state directory and rewrites the session there; two copies race on both. The
  parallelism that matters is across accounts, and that is untouched.
- *A directory per account.* Two accounts sharing one would each sign the other
  out. An account has no ID until it is stored, so a sign-in that has not
  produced an account yet uses a temporary directory.
- *`UsageMeasurer`, not `UsageReporter`.* The client has no quota command, so
  the only honest figure is the sum of a listing — the same position a bucket
  is in, and taken when somebody opens the panel rather than on every ping.
- *Parts stage under the state directory, not `/tmp`.* The unit sets
  `PrivateTmp`, which makes `/tmp` memory; a 16 MiB chunk passing through it
  would spend a memory ceiling on a file being handed straight on.
- *A missing binary is a `Ping` failure, not a hidden backend.* It names the
  fix and the synced-folder alternative. A connect list that varies by machine
  is worse to document than one entry that fails with a sentence.
- *Signing in is a link, not a redirect.* The client prints one and blocks;
  there is nothing to catch. `SignInLink` on the spec declares that shape, and
  the browser polls the same flow store OAuth uses. The link being *copyable*
  is the point — it can be followed on a phone, which is how a machine with no
  browser connects an account.

**Candidates.** Any service with a real CLI and no usable API. The test is
whether the client can be driven non-interactively, put its session somewhere
readable, and report a missing path distinguishably from a broken account —
without which `ErrNotFound` cannot be told from a failure, and the vault
repairs parts that were never lost.

---

## Deliberately not on the list

- **Mega's native API** — a bespoke crypto stack, no OAuth, and no way to do it
  in the standard library. The sync folder (tier 1) or `rclone serve webdav` is
  the answer.
- **iCloud's private protocol** — the folder is the supported route; anything
  else is an unofficial reimplementation of an authentication flow Apple
  changes at will.
- **Telegram, Discord, or any chat service as a disk** — against their terms,
  and a vault that depends on a ToS violation is not a vault.
- **A Git forge as a disk** — same objection, plus every part would be
  preserved forever in history whether or not the vault deleted it.

---

## The recipe

Everything the UI needs comes from the registry, so the frontend is not part of
adding a backend. In order:

1. **Kind constant** in `internal/provider/provider.go`.
2. **A file** `internal/provider/<kind>.go` with an `init` that calls
   `Register(Spec{…}, newXProvider)` — or, for a service that is only a synced
   folder, a row in the `syncFolders` table in `syncfolder.go`, which does the
   registering for you. The `Spec` is the connect form: each
   `FieldSpec` becomes an input, `Secret` fields are redacted on the way to the
   browser, `Directory` fields get the folder picker, `Multiline` fields get a
   textarea (needed for anything with newlines in it — an `<input>` drops them
   silently), `Advanced` fields hide behind a disclosure, and `Presets` become
   buttons.
3. **The six methods** — `Put`, `Get`, `Stat`, `Delete`, `List`, `Ping`.
   `Get`/`Stat` return `ErrNotFound` for a missing object; `Delete` is
   idempotent. Implement `UsageReporter` where the service reports a quota —
   cheaply, since it is on the ping path — `UsageMeasurer` where it can only be
   counted, as an S3 bucket can, `Identifier` so a freshly connected account can
   name itself, and `CredentialRotator` if credentials change as they are
   spent.
4. **Order** — 10s for sign-in backends, 20s for credentials, 30s for the ones
   that take a path. Ties break by label.
5. **Tests** in `internal/provider/`. The HTTP backends are tested against an
   `httptest` server (`cloudapi_test.go`); the folder ones against `t.TempDir()`.
6. **An icon** in `web/src/theme.js` (`KIND_ICONS`) — one character. Missing
   kinds fall back to `☁`, so this is cosmetic, but the committed bundle in
   `internal/server/dist` has to be rebuilt (`make build-web`) for it to show.
7. **`Covers`** on the spec if the backend reaches services its label does not
   name — that is the browser's catalogue window, and the only place the full
   list lives.
8. **The table** in `README.md` under *Supported Backends*.

---

## How iCloud Drive handles eviction

Worth reading before doing another tier-1 backend for a client that evicts,
since it is the only interesting part of that work.

macOS reclaims disk by taking a synced file's contents away and leaving a few
hundred bytes named `.<name>.icloud` where the file was. The part is still in
iCloud; it is just no longer on this machine, under a name the vault never
wrote. Left alone that reads as data loss — `Get` fails, `Stat` says gone, and
`List` reports a key nobody stored.

`icloud.go` embeds the local backend and overrides the five methods that touch
a name on disk:

- **`Get`** falls through to the placeholder, asks the sync daemon for the file
  (`brctl download`), and waits for it to land, bounded by the caller's context
  or ten minutes.
- **`Stat`** reports an evicted part as present with no size. The placeholder's
  own size is not the part's, and quoting it would be a worse answer than none.
- **`List`** maps placeholder names back to keys, and prefers the copy on disk
  when both exist for a moment after a download.
- **`Put`** and **`Delete`** clear the placeholder, so one key never describes
  two files and a deleted part cannot come back as a key `Get` can never
  satisfy.
- **`Ping`** refuses a folder outside iCloud Drive. Writability is not the test:
  `~/Documents` would accept every part SAND gave it and keep them all on the
  one machine that can die.

The equivalents elsewhere, if these get built: OneDrive on Windows and Dropbox
Smart Sync both hydrate on `open()`, so the read path needs nothing — but the
placeholder is still a reparse point with a nonsense size, so `Stat` and `List`
need the same care.
