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

Verify before adding one: endpoints move, and a preset that fills the form with
a dead hostname is worse than an empty field. Check the region placeholder is
obviously a placeholder (`<account>`, not a real account ID) and that `Help`
says where the credentials are minted.

---

## Tier 1 — a folder some client already syncs (~70 lines)

`proton.go` is a `Spec` and a default-path guess, handing the work to
`newLocalProvider`. The whole backend is a form field. It applies to any
service with a desktop client and no usable API: the parts are encrypted before
the client ever sees them, so *"a folder that syncs"* and *"an account that
answers HTTP"* are the same arrangement from the vault's point of view.

Shipped this way: **Proton Drive**, **iCloud Drive**.

Still open: **Mega**, **Jottacloud**, **Sync.com**, **Tresorit**, **Icedrive**,
**MediaFire**. Each is a path guesser and a description.

The one thing to check per service, and the reason `icloud.go` is 300 lines and
`proton.go` is 70: **does the client evict?** A client that keeps every synced
byte on disk needs nothing beyond the local backend. A client that reclaims
space by replacing a file with a placeholder — iCloud Drive, OneDrive's Files
On-Demand, Dropbox Smart Sync — hands the vault a folder where a part written
in March is a stub in June, under a name nobody stored. See "How iCloud Drive
handles eviction" below for the shape of the fix.

---

## Tier 2 — a new credential form (~300–450 lines)

A backend of its own: a `Spec` with fields, a struct, six methods, its own
signing. No SDK — everything here is `net/http` and `crypto` out of the
standard library, which is what keeps the artifact one static CGO-free binary.

| Candidate | Why | Notes |
|---|---|---|
| **Azure Blob Storage** | The biggest real gap — the third hyperscaler, and the only one with no S3 face | SharedKey signing, same exercise as the SigV4 code in `s3.go` |
| **SFTP** | One backend covers rsync.net, BorgBase, Hetzner Storage Box, every VPS and every NAS | Needs `golang.org/x/crypto/ssh` — the only new dependency on this page, and still no CGO |
| **Backblaze B2 native** | — | Low value: the S3 preset already covers B2 |

---

## Tier 3 — a new sign-in (~350–500 lines)

`oauth.go` already carries the authorize URL, PKCE, the code exchange, refresh,
and rotation of refresh tokens that providers retire on use. A new OAuth
backend is one file: an `OAuthSpec` in the registration, and the six methods
against the provider's REST API.

- **pCloud** — a real API instead of the WebDAV preset, with quota reporting
- **Yandex Disk**, **Jottacloud**, **Koofr**, **Egnyte**
- **Seafile** — token auth rather than OAuth, self-hosted
- **SharePoint / Graph document libraries** — mostly `onedrive.go` pointed at a
  different drive ID; opens up work accounts

When adding one, the parts that are easy to get wrong are already solved
elsewhere: copy `box.go` for a provider that rotates refresh tokens, and
`gdrive.go` for one where the scope must stay narrow enough that SAND can only
ever see what it created.

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
   `Register(Spec{…}, newXProvider)`. The `Spec` is the connect form: each
   `FieldSpec` becomes an input, `Secret` fields are redacted on the way to the
   browser, `Directory` fields get the folder picker, `Advanced` fields hide
   behind a disclosure, and `Presets` become buttons.
3. **The six methods** — `Put`, `Get`, `Stat`, `Delete`, `List`, `Ping`.
   `Get`/`Stat` return `ErrNotFound` for a missing object; `Delete` is
   idempotent. Implement `UsageReporter` where the service reports a quota,
   `Identifier` so a freshly connected account can name itself, and
   `CredentialRotator` if credentials change as they are spent.
4. **Order** — 10s for sign-in backends, 20s for credentials, 30s for the ones
   that take a path. Ties break by label.
5. **Tests** in `internal/provider/`. The HTTP backends are tested against an
   `httptest` server (`cloudapi_test.go`); the folder ones against `t.TempDir()`.
6. **An icon** in `web/src/theme.js` (`KIND_ICONS`) — one character. Missing
   kinds fall back to `☁`, so this is cosmetic, but the committed bundle in
   `internal/server/dist` has to be rebuilt (`make build-web`) for it to show.
7. **The table** in `README.md` under *Supported Backends*.

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
