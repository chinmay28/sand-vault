# SAND Vault — Secure Archival Network Distribution

## Architecture Document v2.0

---

## 1. What SAND Vault Is

SAND Vault is a **file browser over storage you do not fully trust**.

You connect the cloud accounts you already have — a Google Drive, an S3 bucket,
a Nextcloud server, a folder on an external disk. From then on SAND behaves
like an ordinary file manager: folders, uploads, previews, downloads. What
happens underneath is not ordinary.

Every file you add is compressed, cut into shards, and encrypted. Each shard is
pushed to a **different** account. Any two of the three rebuild the original;
**any one on its own is indistinguishable from noise**. When you open a file,
SAND fetches the shards back from wherever they live, reassembles them in
memory, and hands you the plaintext.

With more accounts connected the file is cut finer instead: six accounts hold
four-of-six, nine hold six-of-nine. Still one shard per account, so the sentence
above stays true however wide the spread — and gets stronger, because more
accounts have to be broken into together before what they hold is enough (§5.3).

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
  "version":     3,
  "kdf":         { "salt": "…", "time": 3, "memory": 65536, "threads": 4 },
  "check":       { "nonce": "…", "ciphertext": "…" },
  "data_key":    { "nonce": "…", "ciphertext": "…" },
  "data_key_id": "…",
  "providers":   { "nonce": "…", "ciphertext": "…" },
  "manifest":    { "nonce": "…", "ciphertext": "…" },
  "settings":    { "nonce": "…", "ciphertext": "…" },
  "policy":      "strict",
  "default_accounts": ["…", "…", "…"],
  "sub_vaults":  [ { "id": "…", "kdf": { … }, "check": { … },
                    "data_key": { … }, "data_key_id": "…",
                    "section": { "nonce": "…", "ciphertext": "…" } } ]
}
```

Version 3 reads a version 2 vault as it stands and upgrades it on the first
write; every version 3 field is additive, so a vault with no sub vaults is
byte-for-byte what version 2 always was. The number had to go up all the same:
a sub vault's section is the only copy of what it holds, and an older build
would parse the file, discard the field it had never heard of, and write it
back without it. Refusing an unknown version is what that build already does,
which is what makes the discarding impossible.

Only the KDF parameters, the policy, the key generation labels and the default
accounts are in the clear — those last are the random IDs of accounts whose
names, kinds and credentials all sit inside the encrypted providers section, so
they name nothing on their own. Everything else is sealed with AES-256-GCM
under the **vault key**, derived from your password with Argon2id.

| Section | Holds | Why it is encrypted |
|---|---|---|
| `check` | A fixed magic string | Opening it is what verifies the password |
| `data_key` | 32 random bytes | The key files are actually encrypted with |
| `retired_keys` | Earlier data keys, while a password change is still re-encrypting (§3.3) | They open parts that have not moved yet |
| `providers` | Account configs **including credentials** | A stolen vault file must not yield cloud access |
| `manifest` | Filenames, folders, sizes, part placement, film details (§3.10) | Filenames and folder structure are themselves sensitive |
| `settings` | The preferences that are secrets — today the film database key | It is a credential for a third-party service; absent entirely until one is set, and never copied into a manifest backup |
| `sub_vaults[].section` | One sub vault's whole index (§3.8) | Sealed under **its own** password — the vault key does not open it |

### 3.2 Two keys, not one

File parts are **not** encrypted under your password. They are encrypted under
the random `data_key`, which is itself wrapped by the password-derived vault
key.

This buys two things:

- **Part encryption does not inherit a weak password.** The secret protecting
  file content is 256 bits of entropy regardless of what you typed.
- **The vault can hold more than one of them at a time**, which is what makes
  changing a password something other than a lie. See §3.3.

### 3.3 Changing the password rotates the data key

Re-wrapping the data key under a new password would leave every part on every
account encrypted exactly as before. Anyone holding the old password *and* an
old copy of the vault file — or of the `manifest.sand` sitting on each connected
account — could still unwrap the same key and read the same parts. The password
would have changed; what it protects would not have.

So `sand vault passwd` generates a **new** data key and rebuilds every stored
file onto it. Each file is gathered from its parts, decrypted under the old key,
re-encrypted under the new one, scattered again, and the parts the old key opened
are erased.

That cannot be one atomic act — it is a download and an upload per file, across
accounts that may be slow or offline — so the vault carries the old key beside
the new one while it runs:

| On disk | Holds |
|---|---|
| `data_key` + `data_key_id` | The current generation. Every new upload uses it. |
| `retired_keys[]` | The generations files are still stored under, wrapped under the new password |
| `manifest.entries[].key_id` | Which generation each file's parts answer to |

The password change itself is one atomic write: new key minted, old key kept
beside it, every section re-sealed under the new password. From that moment the
old password opens nothing. Files then move one at a time, each committed on its
own, and a retired key is dropped — from memory and from the file — the moment
no entry names it, whether the last file on it was migrated or deleted.

What that buys:

- The password is genuinely changed the second the command returns.
- Every file stays readable throughout, on whichever key it is on.
- An interrupted migration resumes (`sand vault migrate`) rather than restarts.
- A file on an offline account holds up nothing but itself, and is reported.

A manifest backup written mid-migration carries **every** generation it needs,
so a vault lost halfway through still recovers both halves of its files.

**The cost is bandwidth.** Changing the password re-downloads and re-uploads
everything the vault holds. `--no-migrate` changes the password now and leaves
the files for later, at the price of the old password still being enough to read
the parts that have not moved.

### 3.4 Locking

Unlocking derives the vault key, decrypts every section, and holds the keys in
memory. Locking zeroes them and drops the decrypted index, the cached
thumbnails, and the cached chunks (§4.3) — everything derived from the files
themselves goes with the keys. A locked vault can list nothing and fetch
nothing: there is no cached view to fall back on, and a reader opened before the
lock stops answering rather than continuing from a key it captured.

The server re-locks automatically once every browser session has been idle past
the timeout (default 30 minutes, `--idle-timeout`).

### 3.5 Atomic writes

The vault file is rewritten in full on every change, via a temp file in the same
directory plus `fsync` and `rename`. An interrupted write cannot corrupt the
index that maps your files to their parts — the one piece of state whose loss
would strand everything.

### 3.6 The manifest backup

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
all the others. The film database key is excluded for the same reason and is
not part of the manifest at all — it lives in the vault file's own `settings`
section (§3.10). Film *details* do travel, because they are index: recovering
from a backup gives back every title and asks for the key again.

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

### 3.7 Recovery

Three routes back, in increasing order of how much has survived:

| You have | Command | You get |
|---|---|---|
| `manifest.sand` + password | `sand manifest ls` | The file tree and the placement map |
| Enough parts + `manifest.sand` + password | `sand restore --manifest` | The complete file, offline, no accounts |
| The accounts + password | `sand vault recover`, or the browser's own prompt | The whole vault, files openable again |
| The accounts you were missing | `sand vault recover --resume` | The parts that were out of reach, no password needed |
| A recovered vault | `sand vault reclaim` | The same files on a key of your own, on clouds you pick |

`sand vault recover` runs against a fresh vault with the accounts reconnected.
Reconnecting gives every account a new internal id, so rather than trusting the
remembered ids it asks each account what it actually holds and re-points every
shard record at whichever account answers with that key. Accounts that were not
reconnected show up as unreachable parts. The recovered vault adopts the old
data key, so new uploads join the existing files instead of starting a second
key, and it rewrites the backups under its own password.

**Nobody has to know the command exists.** The route above is only a route back
for someone who already suspects there is one, and the person in this situation
— a reinstalled machine, an empty vault, a file list that is blank because
everything they own is on accounts the vault has never heard of — has no reason
to suspect it. So `Vault.ScanForRecovery` asks each connected account two
questions that need no password: what are you holding, and does the
`manifest.sand` on you open under *this* vault's key? A readable envelope that
this vault cannot open was written by a different one, which is the signature of
exactly this disaster, and the browser prompts on it. The scan runs only where
it could matter — an empty vault, or one carrying shard records that point
nowhere — so it never sits on the path of a vault that is simply in use.

**And the prompt is an errand, not a form.** It fires on the first account
reconnected, which is by definition one short of `MinPartsToRestore`, so what it
asks for is the *next* cloud rather than a password it could not use yet.
Connecting happens inside the dialog, every account that lands re-runs the scan,
and the step that follows is chosen from what came back: still short, and it
asks again; enough, with the index already adopted, and it re-points; enough
with a password given, and it checks and then commits. Being handed the account
the dialog asked for is the assent — the alternative is a second button meaning
the same thing.

**The report leads on the shortfall.** `MinPartsToRestore` is two of three, so
one cloud you have not got back yet costs nothing and two costs you the file.
`RecoveryReport` therefore counts in pairs — `Recoverable` against `Files`,
`RecoverableBytes` against `Bytes`, because those two diverge and the bytes are
usually the answer to "how bad is this" — and then names what is left: the files
that came up short, and the accounts whose absence caused it. An account that
held only a third part is listed without `Blocking` set, since reconnecting it
changes nothing about what can be opened.

**Recovery ends with borrowed keys, and that is not where it should stop.** The
recovered vault adopts the lost vault's `data_key` because that key is the only
thing that opens the parts already on the accounts. It is derived from the *old*
password, and every copy of the old `manifest.sand` carries it — including any
taken off an account before the replacement vault existed, which no amount of
overwriting can reach. So the files are readable by whoever could read them
before the machine died, and the vault records that (`InheritedKeyID` in the
store, `Stats.InheritedKey` out of it) and keeps saying so.

`Vault.Reclaim` (`sand vault reclaim`, `POST /api/vault/reclaim`) closes it, and
is the third thing in this codebase built out of the same two moves §3.3
established. `RotateDataKey` mints a fresh generation and retires the adopted
one — no password needed, since the key that *wraps* the data key is the one
already held in memory and is not the one changing — and `MigrateFilesTo` then
rebuilds every file onto it and erases the parts the old key opened. Because
every file is gathered and scattered anyway, that migration is also the one
cheap moment to change placement, which is what the `accounts` argument is for:
the clouds a recovery lands on are the ones a machine that no longer exists
picked. The selection is validated against `BuildPlan` *before* the key rotates,
so a placement that cannot hold a file fails with the vault untouched.

Interruptible on the same terms as a password change: the rotation is one write,
each file commits on its own, and `sand vault migrate` finishes whatever is left.

**Finishing later is a separate operation.** A partial recovery leaves a
complete index, part of it out of reach: `Stats.Unresolved` counts the shard
records naming accounts the vault is not connected to, and `Stats.Stranded` the
files that cannot be opened because of them. `Recover` cannot be run twice — it
adopts a data key, and by then there are files depending on the one it adopted —
so `Vault.Reconcile` (`sand vault recover --resume`, `POST
/api/vault/recovery/resume`) does the small half instead: ask the accounts what
they hold, re-point what now has somewhere to point, persist. No password is
involved, because what was missing was never a secret. A shard whose account
*is* connected but did not appear in its listing keeps its record rather than
being dropped — one failed listing should not throw placement away; that is what
the health check is for.

### 3.8 Searching is a property of the open vault

Every store SAND writes to could answer "what do you hold?" with a list of
opaque part names and nothing else — filenames and folder structure live only
in the manifest, and the manifest is only ever readable while the vault is
unlocked, in memory. So search is not something delegated to a provider or
recovered from an index on disk: it is a scan of the decrypted manifest, and
locking the vault takes it away with everything else.

That also means it is cheap. The index is already in memory, a vault holds
thousands of entries rather than millions, and `Vault.Search` walks it under
the same read lock a listing takes:

- a bare query is a case-insensitive substring of the **name**; `*` and `?`
  make it a wildcard pattern; a query containing `/` is matched against the
  full **path** instead, anchored at a segment boundary
- folders are results too, including the ones that were never created
  explicitly and exist only because something was stored beneath them
- hits are ranked — exact name, then prefix, then substring, then by depth,
  so the answer nearest the top of the tree comes first — and the cap is
  applied after ranking, so truncating keeps the best matches rather than
  whichever the index happened to list first
- a file hit carries its whole index entry, so a result row draws its size,
  placement and redundancy without a second round trip

### 3.9 An account's name and colour are index state

`provider.Config` carries two fields the backend has no opinion about: `Name`,
the label an account answers to, and `Color`, the shade it wears in the browser
— the card's stripe, every part badge for a file it holds, and its row in the
cloud picker. `Vault.UpdateProvider` is the only writer of either, and it is the
one write against an account that never opens a connection to it: no credential
changes, no object is read or written, and no part moves.

Two properties are worth stating.

**A colour is validated, not trusted.** `provider.NormalizeColor` accepts
`#rgb`, `#rrggbb` and either without the `#`, and stores lower-case `#rrggbb`
— one length and one case, so nothing downstream compares two spellings of the
same colour. Anything else is refused, because the value ends up in a style the
browser paints. The empty string is a value rather than a failure: it means
*no choice*, and the browser goes back to assigning one from its palette,
claiming the colours accounts have chosen first so an automatic pick never
lands on one.

**A rename travels through the manifest.** Every `Shard` records
`ProviderName` alongside `ProviderID` — it is what the file list, the health
read-out and `RecoverFromBackup` read, the last of which matches a backup's
accounts to the connected ones on kind and name. So the rename and the shards
it touches are one write under one lock: the config, the index, and the vault
file all move together or none of them do.

Names stay unique, case-insensitively, whether they are set by `AddProvider` or
changed here — two accounts answering to one name would make `--accounts a,b,c`
and every CLI lookup ambiguous.

### 3.10 Film details are index state too, and the one request that leaves

A video file's name is a poor description of the film in it, so a folder can be
told that its videos are films and matched against The Movie Database — the
service Plex and Jellyfin use. Three decisions define the shape of it.

**It is off until a folder asks, by name.** Every other request SAND makes goes
to an account the user connected. A lookup does not: it sends a title guessed
from a file name, this machine's address and the user's own API key to a third
party. So the switch is per folder, kept in `Manifest.MovieFolders` and
inherited downwards — a films folder is a library and libraries have folders
inside them — and `movieLookupLocked` walks up from a folder to find the
nearest setting. The server checks it against the index on every lookup path
rather than trusting the request, and turning it on performs no lookup at all:
the sweep is a separate call.

**What comes back is stored where its sensitivity says it belongs.** The text —
title, year, summary, runtime, genres, score, director, cast — is a few hundred
bytes per film and goes into `Manifest.Movies`, keyed by file ID, so it is
sealed with the rest of the index and travels in a manifest backup. Keying by
ID rather than path means a rename or a folder move carries it for free.

The poster is not text, and it is not kept there. It becomes the file's
**thumbnail**, which means it is compressed, split into three encrypted parts
and scattered by the same placement rules as every other picture in the vault
(§4.3's pack). Two things fall out of that: the browser still fetches every
pixel it draws from the local server, and a poster costs nothing new to store,
serve, move between clouds or delete.

The API key is the exception in the other direction. It is a credential for
somebody else's service, so it lives in a `settings` section of the vault file
sealed under the vault key — and deliberately *not* in the manifest, which is
replicated to every connected account (§3.6). A password change re-seals it
under the new key alongside the providers, because it is something the user set
rather than something derived. The browser puts it where that says it belongs:
with the vault's own settings, beside the password and the default clouds. A
folder's film dialog only reports whether a key exists, because one key serves
every folder and the folder switch is the only thing a folder owns.

**A sweep is resumable and idempotent.** Every film is committed the moment it
is matched, so a timeout, a rate limit or a lock costs the film in flight and
nothing else; a film that already has details is skipped without a request, and
one corrected by hand is skipped even by a full refresh. Posters are collected
per folder and written through `Vault.SetThumbs` once per folder rather than
once per film — a pack is a single stored object, so the naive loop would be
quadratic in the size of the folder.

That last point covers the one case where derived data cannot be re-derived. A
password change drops every thumbnail (§3.3), because they are sealed under the
retiring key and regenerating them is cheaper than re-encrypting them. A
photograph's thumbnail comes back the next time it is opened; a poster has no
local source, so `movie.Info` keeps the artwork's path at the database and a
re-sweep restores it for one image fetch each, with no search.

Name parsing lives in `internal/movie/name.go` and is the same guess every
media server makes: cut at the year if there is one, cut at the first encoding
tag if there is not, and fall back to the folder's name when the file's is a
disc track. It is wrong sometimes by construction, which is why `movie.Info`
records the query it was matched on, why a match can be corrected against a
candidate list, and why a corrected match is marked `Manual` and never
overwritten.

### 3.11 A folder's picture is a pointer, and only ever a chosen one

A folder can be drawn with a picture of something inside it, and the way that is
done is by not storing anything: `FolderArt` is the **ID of a file that already
has a thumbnail**, and the browser draws it through
`/api/files/{id}/thumb` — the same address that file's own row draws through.
There is no folder-cover object on any account, nothing to keep in step when the
picture changes, and nothing to lose.

**Nothing is picked automatically.** An earlier cut of this chose one for every
folder — the poster with the highest `fnv64a(folder ‖ file)`, so the pick was
stable and spread — and that was the wrong trade. Which film stands for a
trilogy is a matter of taste, so a guess has to be explained and undone rather
than simply made; and a picture appearing on a folder nobody asked about sits
badly in an app whose posture is that nothing happens to your files unless you
ask. So `Manifest.FolderArt` maps a folder path to a file ID, it is written only
by `SetFolderArt`, and a folder with no entry is a folder with no picture.

What `folderart.go` still computes per listing is one bit per folder: **is there
anything in here that could be chosen?** That is what decides whether the control
to choose appears at all, and it is why the listing's map distinguishes three
states — no entry (nothing picturable under the folder), an entry with no `id`
(nothing chosen yet), and an entry with one (this is what it wears). It is a
single walk of the index, offering every thumbnailed file to each of the wanted
folders that contains it, walking up from the file rather than down from the
folder so that a film three levels deep still counts for the library above it.

The stored half is the third map keyed by folder rather than by file — the other
two are the thumbnail packs (§4.3) and the film-lookup settings (§3.10) — so it
is rekeyed by `moveFolder` alongside them, and dropped when the folder or the
file it names goes away. A choice outliving its file would be a folder pointing
at nothing; instead the folder goes quietly back to its icon.

The one real cost is on the read side, and it is the packs': a picture lives in
the pack of the folder that holds it, so a parent whose folders have all been
given pictures gathers a pack per folder. The browser loads tiles lazily and a
gathered pack is held until the vault locks, which makes this the same cost as
opening those folders, paid where they are listed instead. Storing a copy of each
cover in the parent's own pack would trade that for an extra stored object per
folder and a second thing to invalidate; pointing at what is already there is the
cheaper bargain in both directions.

---

### 3.8 Sub vaults

A sub vault is a vault inside the vault, with a password of its own. It exists
for what should not be readable by whoever holds the main password — which,
once a vault is mounted as a drive and left open on a laptop, is a broader set
of people than it sounds.

It is sealed under Argon2id of its own password and nothing else. The main
password unwraps every other section of the file and does not unwrap this one.

Three properties follow, and they are the design:

**Its own namespace.** A sub vault's files are not hidden main-vault files at
hidden main-vault paths; they live in a separate tree with its own root.
Nothing can collide, and nothing has to be checked against paths that cannot be
read while the sub vault is shut. In code this is `vault.Scope`: the zero value
is the main vault, and the seven path-addressed reads take one. Everything
addressed by file ID takes none — an ID is unique across every vault, so the
vault resolves it against whatever is open, and reading, moving, deleting,
streaming and thumbnailing stay exactly the operations they were.

**Its own data keys.** A file records the key generation it was sealed under,
and a sub vault's generations belong to the sub vault. The machinery for "this
file is on a key the vault holds somewhere" already existed for password
changes, so it carries sub vaults with almost nothing added — including
deferring re-encryption, which is what makes assignment instant.

**Never on a WebDAV mount.** Not while locked, and not while unlocked either.
`internal/davfs` is pinned to `MainScope` by a constant, so the guarantee holds
by construction rather than by every call site remembering it. A mounted drive
is a folder every process running as you can read; what goes in a sub vault is
what should not be reachable that way.

#### What the main password can see

Deliberately bounded, and worth stating exactly. It sees that a sub vault
exists, what it is called, when it was made, and an **inventory** of the objects
it owns — archive IDs, which account each part sits on, and how big each is. It
sees no path, no filename, no per-file size, no type, and no content.

The inventory is the price of two things that would otherwise be impossible. A
sub vault whose password is forgotten can never be opened again, and without a
list of its objects they would sit on the accounts for good, unattributable and
undeletable. And disconnecting an account has to refuse when it would strand
files — a check that would silently pass if it could not count what a shut sub
vault has there.

#### Assignment

Moving a file or a folder between vaults keeps its path: `/Papers/2019` in the
main vault arrives at `/Papers/2019` in the sub vault, with the folders above it
made as it lands, so sending it back puts it where it was.

The index move is one write and it is instant. Nothing is uploaded or
downloaded, and the entries keep the key generation they were sealed under until
the re-encryption behind the move catches up.

**No key crosses the boundary.** An earlier version of this copied the source's
generation into the destination so a moved file would read immediately — which
handed the destination the key sealing everything *else* in the source, because
a vault's files share one active generation. Assigning one receipt out of a sub
vault therefore gave the main password the whole sub vault, and since
`snapshotLocked` replicates every retired key to every connected account
alongside the inventory naming the objects, the main password plus any one
account was a complete break of the guarantee above.

So nothing is adopted. A moved file is readable while both vaults are open,
because key lookup searches every open vault; once the source is shut it reports
`ErrSubVaultLocked` until the re-encryption has moved it. That is the honest
state, and it is why the pass matters rather than being a tidy-up: the file is
only fully independent of the vault it came from once it is on the destination's
own key and the old parts are erased.

#### Importing a vault found on an account

Every vault replicates its index to every account it uses, so an account that
has been used before carries a `manifest.sand`. Reconnecting an old cloud after
a machine died means looking straight at your own files — and `Recover` refuses
to run against a vault that already holds anything, because adopting a snapshot
replaces the data key and would strand everything already there.

Importing as a sub vault dissolves that restriction rather than working around
it: a snapshot carries an index, a data key and a password that opens it, which
is precisely what a sub vault is. The found vault lands beside yours instead of
replacing it.

Connecting an account probes it for a backup and reports only whether it is
ours — everything else is inside the envelope, which is right: an account
holding someone's backup should not be able to describe it to whoever connects
it next. The import asks for the old password to open the snapshot and a new one
for the sub vault; the second is free, because the snapshot's data key is
adopted as it stands and only the section wrapping is derived afresh. Parts are
matched to accounts by asking what object keys are really there, the way a
recovery does.

#### Backup and recovery

A backup carries the sealed records verbatim, so a recovery restores a vault
with its sub vaults present and shut, each opened afterwards by its own
password. A backup that dropped them would restore a vault missing exactly what
its owner had been most deliberate about protecting, and would do it silently.

One wrinkle: a recovery rewrites every shard record to point at the account that
really holds the part, since reconnecting an account gives it a fresh ID — and
it cannot do that inside a sealed section. So the translation is worked out from
the inventory, written into the main manifest as `account_remap`, and applied
the first time each sub vault is opened.

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
                                        │  the accounts chosen  │
                                        │  for this file, then  │
                                        │  the placement policy │
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

### 4.3 Reading at an offset

A chunked file (§7.1) can be read where it is wanted instead of from the start.
`Vault.OpenReader` returns a `ChunkedReader`, which is an `io.ReaderAt`: given
an offset it divides by the chunk size, gathers that one chunk by the race
above, and returns the bytes inside it.

`io.ReaderAt` rather than `io.ReadSeeker` on purpose. A FUSE mount is handed an
offset and a length directly; an `http.File`'s seek-then-read is a thin wrapper
over one, which `SectionReader` supplies. Making the primitive the narrower of
the two means a filesystem layer stays an adapter rather than a second
implementation of the same thing.

Two things sit behind it:

- **A bounded cache of decrypted chunks**, shared by every reader on the vault.
  A player scrubbing through a film asks for the same chunk repeatedly, and
  refetching it from two clouds each time would make seeking unusable. It is
  measured in bytes rather than chunks, because chunk size is per file. It
  holds plaintext, so locking the vault drops it (§3.4) — released rather than
  overwritten, since a reader may still be copying out of a chunk it was handed.
- **Single-flight per chunk**, because a player opens several connections at
  once and would otherwise gather the same chunk once per connection.

The reader holds no key. Every cache miss re-reads the vault's data key under
the lock and zeroes its copy afterwards, so a reader left open across a lock
stops being able to read rather than carrying on from a key it captured.

A file stored whole has no chunks to fetch individually, so `OpenReader` refuses
it rather than quietly rebuilding the whole thing behind an interface that
promises cheap seeks. That refusal is `ErrNeedsConversion`, and §4.5 is what
answers it.

### 4.5 One stored format, and a way out of the other

Everything SAND writes is chunked — `Upload` and `UploadStream` both go through
`scatterChunked`, and the only remaining caller of the whole-file writer is the
thumbnail pack. Everything that *serves* a file reads it chunked too, at an
offset, for a cost that does not grow with the file.

Files stored before chunking existed cannot join that. Their parts are halves of
one AES-GCM-sealed blob, and GCM will not release any plaintext before the tag
over all of it verifies — so the format cannot be read at an offset at all. This
is not an implementation shortcut; it is why chunking replaced it.

So the read path refuses them rather than rebuilding them whole, and `Convert`
(`internal/vault/convert.go`) is how they leave the old format:

| | |
|---|---|
| Asked for | Never triggered by a read. Converting is a download and a re-upload of the whole file; a read that started one on your behalf, at the worst moment, is how a 16 GB machine was taken down repeatedly — and an interrupted conversion commits nothing, so the next read started it again |
| Bounded | It reads the old format the one way that format supports — sequentially. Parts decrypt in place (`DecryptInPlace`), the halves concatenate through an `io.MultiReader` rather than a third buffer, and decompression streams into the chunked writer |
| Measured | 0.41 bytes of memory per byte of file, against about 3 for `DecodeBytes` |
| In place | The index moves onto the new parts in one write; the file is readable throughout, and the old parts are erased only after |

`DecodeBytes` stays for what it is good at — payloads already in hand and small:
thumbnail packs, and standalone restore.

### 4.4 The erasure code

A chunk is cut into **k data shards and n − k parity shards, any k of which
rebuild it**. Which k and n is per file (§5.3), and the shards say so in their
own headers.

The code is a systematic Cauchy Reed–Solomon over GF(2⁸)
(`internal/splitter/erasure.go`), and three properties of that choice are
load-bearing rather than incidental:

**It is systematic.** The first k shards are the compressed chunk cut into k
consecutive pieces; only the parity is computed. So a read that collects the
data shards rebuilds by concatenating them, with no field arithmetic at all —
the same fast path `concat(p1, p2)` always was. And a shard is a slice of the
stream rather than a transform of all of it, which bounds what one compromised
account plus the key yields at 1/k of the file.

**The parity rows are Cauchy.** Every square submatrix of a Cauchy matrix is
invertible, which is exactly the MDS guarantee — *any* k of the n shards invert
back to the data — and it holds without the change of basis a Vandermonde
construction needs. A decode picks the k rows it has, inverts that k×k matrix by
Gauss–Jordan, and multiplies.

**The field is GF(2⁸) rather than GF(2).** This is the whole reason the code can
widen at all. Until version 4 a chunk was two halves and their XOR, and XOR is
addition in GF(2), where two symbols have exactly three non-zero combinations:
A, B, A⊕B. There is no fourth. Over GF(2⁸) every byte is a usable coefficient
and there is room for 255 parity shards, so "two of three" becomes "k of n" with
no change of idea — only of field.

Format versions 1 to 3 are the XOR construction and are still read
(`splitter.ReconstructXOR`); they are all 2-of-3, which is the only code they
could express.

#### Reconstruction truth table

For the default 2-of-3 the table is what it always was:

| s1 | s2 | s3 | Method |
|:--:|:--:|:--:|:---|
| ✓ | ✓ | — | concat(s1, s2) — the systematic path, no arithmetic |
| ✓ | — | ✓ | invert the 2×2 for rows {1,3}, then concat |
| — | ✓ | ✓ | invert the 2×2 for rows {2,3}, then concat |
| ✓ | ✓ | ✓ | concat(s1, s2) — s3 unused |
| at most one | | | unrecoverable |

Generally: **any k present → rebuildable; fewer than k → not**, with no case
analysis, which is the point of an MDS code. `erasure_test.go` checks it across
the family — every k-subset where they can be enumerated, and a seeded sample
plus the two edge subsets (all-data, all-parity-heavy) at widths where C(n, k)
runs to more digits than the test file has bytes.

---

## 5. Placement Policy

Where the shards go is a **security decision**, because any k of them plus the
key rebuild the file. k shards on one account means that account could, in
principle, rebuild it.

### 5.1 `strict` (default)

One shard per account, never two on the same one — at every width.

- 3, 6 or 9 accounts → the matching scheme (§5.3), every shard somewhere
  different, and no single provider holding enough to reconstruct anything.
- 2 accounts → only shards 1 and 2 of the default scheme are stored. The file is
  recoverable and confidential, but there is no spare.
- 1 account → **refused**, with an error explaining why.

### 5.2 `redundant`

Always store the whole scheme, doubling up on an account when there are fewer
connected than it has shards.

Survives an account going dark even with one or two connected — at the cost of
the guarantee above, since a doubled-up account holds enough to rebuild. The
UI says so plainly when you pick it.

### 5.3 The scheme, and how wide a file is spread

How many accounts are chosen settles the erasure code the file is cut with.
There is one rule, **2m of 3m**: clouds come in groups of three, and two thirds
of the shards rebuild the file.

| Accounts | Scheme | Storage | Accounts that can go dark | Accounts needed to rebuild |
|---|---|---|---|---|
| 3 | `2-of-3` | 1.5× | 1 | 2 |
| 6 | `4-of-6` | 1.5× | 2 | 4 |
| 9 | `6-of-9` | 1.5× | 3 | 6 |
| 12 | `8-of-12` | 1.5× | 4 | 8 |
| 3m | `2m-of-3m` | 1.5× | m | 2m |

There is no list of blessed widths; the table stops where patience does. Adding
a group of three clouds always does the same thing: one more failure survived,
two more providers needed to collude, no extra bytes.

The constant ratio is the point. Storage is n/k of the file, and holding k/n at
2/3 makes that 1.5× at every width, so **widening costs accounts rather than
bytes**. A vault that grows from three clouds to thirty does not store one extra
byte of anything.

What it buys shows up in both of the last two columns, and the right-hand one is
the one worth dwelling on. It is how many accounts an attacker must hold
*together* before the shards they have are enough to reassemble a file. It rises
linearly with the width, so a wider vault is not merely more durable — it is
harder to collude against. Shards are also consecutive slices of the compressed
stream, so one compromised account plus the data key yields 1/k of a file: a
thirtieth at 20-of-30 where it was a half at 2-of-3.

Every account holds exactly one shard at every width, which is what keeps §5's
promise intact as the spread grows. `BuildPlan` deals shards one per account,
and the strict policy's capacity is one whatever n is.

**The ceiling is 255 accounts** (85 groups), because a shard's number is one byte
in its header. Counts that are not whole groups are **refused** — there is no
code that uses them without leaving a cloud with no shard of its own to hold.
The rule is enforced identically for an upload's choice, a stored default and a
relocation's destination (`vault.SchemeFor`), so no path can produce a placement
another path would reject.

**Three is what an upload takes when nobody says otherwise**, whatever is
connected. Widening is a decision about how many providers you want in play, and
a vault does not drift into it by having accounts available.

A vault holds files of different schemes at once. Widening does not rewrite what
is already stored, so each file records its own k and n — in the index and, so
that an offline restore needs no index, in every shard's header (§5.5).

#### What widening actually costs

Not storage, and not memory or per-provider load either — but it is not free.

Concurrency scales the right way by construction. The chunk window bounds how
many chunks are in flight, so each *provider* sees at most that many requests
whatever n is, and rate limits are per provider. Bytes in flight are
`window × n × chunkSize/k`, and n/k is 1.5 at every scheme, so a 30-cloud vault
holds exactly as much in memory as a 3-cloud one.

What does grow is **object count and request count in total**: a chunk becomes n
objects, so a 4 GB file at the default chunk size is 256 × n of them. At 2-of-3
that is 768; at 20-of-30 it is 7,680, spread over ten times as many accounts.

And **padding**, which is charged per chunk: the compressed bytes are padded up
to a multiple of k, so a wide scheme wastes up to k−1 bytes per chunk. Against a
16 MiB chunk that is noise at any width the format allows. It only bites on
inputs small relative to k, which is why the coder's overhead test scales its
tolerance with k rather than asserting a flat 1.5×.

#### Changing a file's scheme is not a move

A 2-of-3 file's shards are halves of its compressed stream; a 4-of-6 file's are
quarters. They have nothing in common, so there is no blob to carry across and
no index rewrite that would do. Changing the width of an existing file means
gathering it, cutting it again and writing it out — the same operation a
password change performs, pointed at different accounts (`Vault.recodeFile`).

That is a whole download and a whole upload per file, against 1/k of a file per
shard for a move at the same width. Both the CLI and the browser price it before
it runs and label it as a rebuild rather than a move, because the difference is
two orders of magnitude on a large folder and not something to discover
afterwards.

### 5.4 Which accounts, of the ones connected

Policy decides how many shards may share an account, and §5.3 decides how many
shards there are. Neither decides *which* accounts a file uses, which matters as
soon as more are connected than the scheme has shards to place.

That choice is made in this order:

| The upload says | What happens |
|---|---|
| These accounts | Exactly those, and every one must be connected. How many are named settles the scheme; naming two stores two shards and warns about the missing spare. |
| Nothing, and the vault has default accounts | Exactly the default, on the same terms |
| Nothing, and there is no default | Three picked at random for this file alone |

The random pick is seeded from the file's archive ID — 128 random bits minted
for it — so consecutive uploads land on different accounts and a vault with
six accounts fills all six instead of the first three. Re-encrypting a file
after a password change is not one of these cases: it goes back to the accounts
it was already on, topped up if one has since been disconnected.

The default is honoured rather than completed for the same reason the choice
exists at all. Deciding which providers may hold a file is the point of SAND;
quietly adding a fourth because the default named three and one went away would
put data somewhere its owner deliberately did not choose. What a narrower
selection costs — one part fewer, no spare — is said in the upload's warnings
instead. Disconnecting an account prunes it from the default, trims what is left
back to whole groups (a default of six that loses one becomes a default of
three, not of five), and clears it outright if that would leave fewer than two.

Re-encryption is the one case that refills rather than honours, and it refills
to a scheme width: a 4-of-6 file that has lost one account goes back onto six,
not down to three — narrowing it would be a re-encode, which is not something a
top-up gets to decide.

Within the chosen accounts the starting one rotates per file, seeded the same
way. Without this every part 1 would pile onto the same account.

### 5.5 Object keys leak nothing

```
<128-bit random archive id>-p<N>.sand              stored whole
<128-bit random archive id>-c<index>-p<N>.sand     stored in chunks (§7.1)
```

N is the shard number, 1 to n. It runs to 3 for a 2-of-3 file and to 9 for a
6-of-9 one, which is the only thing about the key that a wider scheme changes.

Derived only from a random ID. Someone with full access to one account learns
how many objects you store and how big each part is — nothing about names,
types, or folder structure.

A chunked file's objects carry a zero-padded chunk index, so listing an account
lexically returns them in order — which is what the recovery path of §3.7 walks.
The index gives nothing away that the flat form did not: objects were already
groupable by their shared archive ID, and a file's size was already readable by
adding up its parts. A chunk count is the same fact reached by counting.

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

### 5.6 Changing which accounts hold something already stored

Placement is decided when a file is uploaded, and it is not the last word: a
cloud account gets expensive, or fills up, or stops being one you want holding
your things. `Vault.Relocate` moves a file — or a folder and everything under it
— onto a chosen set of accounts, and `Vault.PlanRelocation` answers what that
would cost without contacting any of them.

**A part is portable because its name does not mention where it lives.** The
object key above is derived from the file's random archive ID and the part
number alone. So moving part 3 from Dropbox to Box is a `Get` and a `Put` of the
same bytes under the same key: no data key is touched, nothing is decompressed
or re-encoded, and the file comes out the other side with the same archive ID,
the same plaintext hash, the same chunk layout and the same key generation. The
vault has to be unlocked to know what to copy and to read the accounts'
credentials, and that is all the unlock is for.

**Only what has to move moves.** Placement is a set of accounts, not a sequence,
so the planner starts by letting every part already sitting on an account that is
being kept claim it, and only then hands out what is left:

| Currently on | Asked for | What happens |
|---|---|---|
| A, B, C | A, B, D | Shard on C moves to D. The other two are not read, and their index rows are not rewritten. |
| A, B, C | C, A, B | Nothing. A different order is the same answer. |
| A, B, C | D, E, F | All three move — the expensive case among the moves. |
| A…F | A…E, G | One shard moves. Six clouds to six clouds is the same scheme, so it is still a copy. |
| A, B, C | A…F | **Rebuilt.** 2-of-3 to 4-of-6 shares no shards, so the file is gathered, cut again and written out (§5.3). |
| A…F | A, B, C | **Rebuilt**, the other way, and the shards on D, E and F are erased. |
| A, B, C | A, B | Shards on A and B stay; the third is erased, because under `strict` no account may hold two shards of one file. |

The two rebuild rows are the ones worth saying out loud, and both the CLI and
the browser do before anything happens — a rebuild moves the whole file where a
move at the same width moves 1/k of it per relocated shard. `--dry-run` and the
browser's estimate both label it as a rebuild and price it separately from the
moves.

The last row is the other one to say out loud: narrowing below a scheme's width
costs a file its spare. The redundant policy has room for all three shards on
two accounts and doubles up instead, exactly as an upload to those accounts
would — a relocation is not allowed to produce a placement that could not have
been uploaded, which is also why it refuses a destination count that names no
scheme (§5.3).

**Copy, commit, erase, one file at a time.** Between the copy and the commit both
accounts hold the part and the index still names the old one, so a read during
the move works; after the commit the index names an account that certainly has
the bytes. Only then is the original erased. What that ordering buys:

- An interruption leaves an unreferenced copy — litter — rather than a file
  missing a part.
- A part that will not copy is reported and left exactly where it was. The file
  is still whole, just not yet all in the right place, and running the
  relocation again moves precisely that part.
- A commit is refused if it would drop a file below the k shards it takes to
  rebuild it, which the plan cannot ask for on its own but a failed copy can
  bring about.
- A file rewritten underneath the move — by a password change's re-encryption,
  say — is spotted by its archive ID and left alone, and the copies made for it
  are erased.

A folder's thumbnail pack (§4.3's cousin, stored one archive per folder) is
carried along at the end of a folder relocation. It does not move the same way:
a pack is small and derived, so it is gathered and re-scattered onto the chosen
accounts rather than growing a second copy of the placement machinery. A pack
that will not move is a warning — it is a picture, and the browser can draw
another.

### 5.6 Moving something within the vault costs nothing

The other move, and the one to reach for first. §5.5 changes which accounts hold
a file's parts and copies bytes to do it. Changing where a file *appears* copies
nothing at all: a file's folder is a field on its index entry, and by §5.4 the
objects its parts live in are named after its random archive ID and the part
number — never after the folder, and never after the file's name either. So
`Vault.Move` rewrites one field and returns. A 4 GB film moves as fast as a note,
and its parts stay exactly where they are, on the same accounts, under the same
key generation.

**Renaming is the same call with the other argument.** `Move(id, dir, name)`
takes both and treats an empty one as "leave this half alone", so a rename is a
move that keeps the folder and a move is a rename that keeps the name — which is
why there is one entry point, one set of collision rules and one place where
`SanitizeName` runs. `sand mv` reads its destination the same way.

`Vault.MoveFolder` is the same fact one level up, and covers renaming a folder
for the same reason: a rename is a move to a new name beside itself. A folder is
a path in the index, so either one rewrites every entry beneath it — at any
depth — in a single index write, with an in-memory rollback if that write fails. A loop over `Move`
would have left a window where half a tree answered to its old name and half to
its new one. Two maps keyed by folder come along in the same write: the
thumbnail packs (§4.3) and the film-lookup settings (§3.10). Neither carries its
folder's name inside it, so both travel for the price of rekeying a map, with no
network work at all — which is exactly what a folder relocation cannot do, and
why that one pays a gather and a scatter instead.

What is refused, and by the vault rather than by whichever caller asked:

- The root cannot be moved, and a folder cannot be moved inside itself or into
  one of its own descendants — the rewrite would walk into the path it was
  rewriting.
- The destination's parent has to exist already, the same as it does for
  `Mkdir`, and the destination itself must not: a move never silently merges two
  trees.
- A name already taken in the destination folder is refused for that file alone.

The browser reads `GET /api/folders` to draw the tree it picks a destination
from — every folder in one answer, since `GET /api/files` walks a level at a time
and choosing where something goes is rarely a question about the folder already
open. It lists a folder whether it was created outright or exists only because a
file sits beneath it: both are folders you can walk into, so both are folders you
can move into.

---

## 6. Cryptography

### 6.1 Key derivation — Argon2id

Every **password** is stretched with Argon2id, and the parameters are unchanged
from v1:

| Parameter | Value |
|---|---|
| Time | 3 iterations |
| Memory | 64 MB |
| Threads | 4 |
| Salt | 16 random bytes, unique per file and per vault |
| Output | 32 bytes (AES-256) |

That covers the two places a password is actually involved: the vault key
(§3.1), and standalone mode, which has no vault and derives a file's key
straight from what the user typed.

### 6.2 Chunk keys — HKDF-SHA256

Argon2id is what makes guessing a low-entropy password expensive. It buys
nothing against a key nobody typed, and until the chunked format arrived it was
being run on one: `shardPasswordFor` is the hex of the vault's random 256-bit
data key, and every part paid an Argon2id pass to stretch it. The cost was real
enough that thumbnails are packed a folder at a time purely to amortize it
(`internal/vault/thumbs.go`).

A chunked file would pay that cost once per chunk — minutes of pure key
derivation for one large video. So a chunk key is derived instead:

```
chunk key = HKDF-SHA256(
    secret = the vault's data key,
    salt   = the 128-bit archive ID,
    info   = "sand-chunk-key-v3" ‖ chunk index)
```

The archive ID as salt separates files; the index in the info string separates
chunks, so recovering one chunk's key says nothing about its neighbours. The
security argument is that the input already has 256 bits of uniform entropy —
stretching it was never what protected it.

### 6.3 Encryption — AES-256-GCM

- 12-byte random nonce, unique per part
- The cleartext part header is passed as **associated data**, binding each
  ciphertext to its own part number and archive ID — a part cannot be swapped
  for another file's part, or re-labelled as a different part number, without
  the tag failing
- In a chunked part the header also carries the **chunk index**, so the same
  binding stops a chunk being replayed at a different offset of the file it
  genuinely belongs to
- 16-byte authentication tag

### 6.4 Integrity

| Layer | Mechanism | Catches |
|---|---|---|
| Per part | GCM tag | Tampering, truncation, bit rot |
| Per part | Cleartext header as associated data | Part swapping, re-labelling |
| Per part | Metadata sealed with the data | Edits to the name, hash or sizes |
| Per chunk | Chunk index in the associated data | A chunk replayed at another offset |
| Per chunk | Rebuilt length checked against the archive's chunking | A truncated or substituted chunk |
| Whole file | SHA-256 recorded at upload, checked after rebuild | Any corruption that slipped through |
| Vault | GCM tag on every section | A modified or truncated index |

The whole-file SHA-256 is the one guarantee a chunked read cannot give: opening
the chunk under an offset never touches the rest of the file, so there is
nothing to hash. Reading a file end to end still verifies it. What a partial
read gets instead is the per-part GCM tag bound to that chunk's index, which is
what makes serving an arbitrary byte range honest rather than hopeful.

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

### 7.1 Version 3 — one chunk per object

A version 2 object is a whole file, which is why reading one byte of it means
fetching all of it. Version 3 cuts the file into fixed-size chunks first and
runs the pipeline of §4.1 over each chunk on its own, so the chunk covering an
offset opens without the rest of the file existing.

```
Offset  Size   Field                          Cleartext header, also GCM
──────────────────────────────────────────────  associated data
0x00    4      Magic "SAND"
0x04    1      Version (3)
0x05    1      PartNumber (1..3)
0x06    16     ArchiveID
0x16    4      ChunkIndex
0x1A    12     AES-GCM nonce
──────────────────────────────────────────────
0x26    4      PayloadSize
0x2A    N      Ciphertext + 16-byte tag
                 └─ 32   OriginalHash (SHA-256, the whole file)
                    8    OriginalSize   (the whole file)
                    8    CompressedSize (this chunk)
                    1    WasPadded      (this chunk)
                    4    ChunkCount
                    4    ChunkSize      (plaintext, fixed)
                    2    FilenameLength
                    var  Filename
                    var  this part's share of this chunk
```

The header loses the salt and the Argon2 parameters and gains the chunk index,
because the two answer to different key management: version 2 stretches a
password, version 3 derives from the vault's data key (§6.2). That is also why
**standalone mode keeps writing version 2** — it has no vault and no data key,
so a password is genuinely all it has.

The vault writes version 3 for every upload, and re-encrypting a file after a
password change writes it too, so a vault filled before chunking existed
converts as `sand vault passwd` works through it. Files still stored whole are
read exactly as they were; `Entry.Chunked` is what decides which path a read
takes. A whole-file part and a chunked one are told apart by their version
alone, which is why the two decoders refuse each other's input rather than
guessing.

One consequence worth stating: a part is written to **every** chunk of a file or
erased from all of them. A part that fails partway through an upload has the
chunks it did manage erased, so that a shard in the index always means the
objects behind it are really there — which is what delete, health and recovery
all read it as.

`ChunkSize` is the **plaintext** length of every chunk but the last, which makes
the chunk holding an offset that offset divided by it. Recording it rather than
inferring it is the point: a seek must not have to read an index to find out
where to look.

The archive description is repeated in every part of every chunk. That is
`ChunkCount × 3` copies of about a hundred bytes — against a 16 MiB chunk,
nothing — and it keeps the property the format had before: any two parts of any
one chunk still say which file they came from.

Two costs are worth naming. Per-chunk compression gives up whatever ratio a
zstd stream would have found across a chunk boundary, which for the video this
exists to serve is nothing and for a large text file is a little. And the
whole-file hash can no longer be checked on a partial read — see §6.4.

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
assume: `UsageReporter` for backends that can report quota — including the
local folder, which reports the drive it sits on (§8.7) — `Identifier` for
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

### 8.5 A path field is picked, not typed

A field can declare itself a folder on this machine — `Directory: true` — and
the generated form puts a picker on it. `GET /api/system/folders?path=` answers
with the subfolders of one folder, its parent, and the roots worth jumping to
(home, and `/media`, `/run/media`, `/mnt`, `/srv`, or the drive letters on
Windows). Symlinked folders are followed, because a synced cloud folder is
frequently one.

The endpoint browses the filesystem SAND runs on, not the vault, so it is kept
narrow on purpose: folder names only, never a file and never its contents,
behind the same session as everything else — a session that could already point
an account anywhere by typing the path. A path naming a folder that is not
there yet is a legitimate answer, since connecting creates it, so the listing
climbs to the nearest existing ancestor and reports both what was asked for and
what it could show. The browser is told the server's path separator rather than
guessing it: the phone in your hand and the machine holding the folder are
often not the same kind of computer.

Readability, not existence, decides where the picker opens and what it offers.
Under the service `$HOME` is `/home` and `ProtectHome=yes` denies it, so the
opening folder is the first candidate that can actually be listed — home, then
the vault's own directory, then the mount roots — and a root that cannot be
opened is left out of the shortcuts instead of offered as one that always
fails. A listing that fails is still a `200` carrying the folder's parent and
those shortcuts, with the reason in `error`: an unreadable folder is a normal
thing to walk into, and a picker that cannot leave the folder it landed on is
worse than one that explains itself.

### 8.6 Google Drive has no paths

Drive is not a path-addressed store, so the SAND object key is written into the
file's `appProperties` and looked up by query, with a per-provider ID cache so
repeated reads skip the lookup.

### 8.7 How full an account is

`Usage` carries three figures — `used`, `total`, and `free` where the backend
measured it rather than leaving it to be subtracted. The third exists for local
folders: a filesystem keeps a reserve only root may spend, and a quota can cut
an account's share down from the disk it sits on, so what can actually be
written is not `total - used`. `Usage.Remaining()` prefers the measured figure
and falls back to the subtraction.

A local folder answers from `statfs` (`GetDiskFreeSpaceEx` on Windows) against
the drive it sits on, climbing to the nearest existing parent for a folder that
has not been created yet. Deliberately the drive and not the folder: what SAND
put there is already known from the index, and the number the index cannot
supply is the 800 GB of other things on the same disk. Nothing walks the tree.

`ProviderStats` is that account taken apart, and every figure in it is read off
the index — no listing, no traversal, so the panel costs one ping:

| | |
|---|---|
| capacity | `Usage` against `Stored`: SAND's parts, everything else on the account, what is free, what is reserved |
| `Files` / `Sole` | files with a part here, and those that could not be rebuilt without it — counted against the accounts that would survive the disconnect, exactly as the guard in §3.9 counts |
| `Kinds` / `Folders` / `Largest` / `Months` | what the parts belong to, weighed by what each thing left *here* rather than by its own size |
| `SubVaults` | one line: parts and bytes, from the inventory |

The breakdowns come from the main vault's index alone. A sub vault reveals its
name and its weight to the main password and nothing else about what is inside
it (§3.8), and an open one would otherwise start naming its folders in a panel
opened from the drawer — about an account, with nothing to say which vault a
row came from. Its parts still count towards the capacity figures and towards
`Sole`, from the same inventory `ProviderStatuses` uses, so a locked sub vault
does not make an account look emptier than it is.

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
| POST | `/api/vault/password` | New password, new data key, everything re-encrypted onto it (`"migrate": false` to defer) |
| POST | `/api/vault/migrate` | Finish a re-encryption that was deferred or interrupted |
| POST | `/api/vault/policy` | Change placement policy |
| POST | `/api/vault/defaults` | Set the accounts uploads use by default — any multiple of 3, which chooses the scheme (§5.3); empty list = pick per file |
| POST | `/api/vault/backup` | Turn replication of the index to the accounts on or off (off erases the copies out there) |
| GET | `/api/vault/recovery` | Is a connected account carrying a vault this one could recover? (§3.7) |
| POST | `/api/vault/recovery` | Adopt that index (`password` — the *lost* vault's, `provider_id`, `dry_run`) |
| POST | `/api/vault/recovery/resume` | Re-point the index at accounts reconnected since (`dry_run`; no password) |
| POST | `/api/vault/reclaim` | Fresh data key under this password, every file rebuilt onto it, onto `accounts` (§3.7) |
| GET | `/api/subvaults` | The vaults inside this one, and whether each is open (§3.8) |
| POST | `/api/subvaults` | Make one, sealed under a password of its own |
| POST | `/api/subvaults/{id}/unlock` | Open one — a second password on top of the session, never a way around it |
| POST | `/api/subvaults/{id}/lock` | Shut it; drops the chunk cache and every stream link |
| PATCH | `/api/subvaults/{id}` | Rename it (metadata the main vault keeps, so its own password is not needed) |
| POST | `/api/subvaults/{id}/password` | New password, new data key, its files re-encrypted onto it |
| POST | `/api/subvaults/{id}/migrate` | Finish a re-encryption inside one |
| DELETE | `/api/subvaults/{id}` | Delete it and erase its parts (`?force=1` when it is locked and cannot be listed) |
| POST | `/api/assign` | Move a file or folder between vaults (`from`, `to`, `target`) |
| POST | `/api/vaults/import` | Bring a vault the recovery scan found in as a sub vault, rather than recovering over this one |
| GET | `/api/providers/specs` | Backend descriptions for the connect form |
| GET | `/api/providers` | Connected accounts: online, parts held, quota |
| GET | `/api/providers/stats/{id}` | One account taken apart: quota against SAND's own share, and what the load is made of (§8.7). The id trails `stats` because `{id}/stats` collides with the OAuth status route below |
| POST | `/api/providers` | Connect an account (pings before saving) |
| POST | `/api/providers/oauth/start` | Begin a sign-in; returns the consent URL |
| GET | `/api/providers/oauth/callback` | Where the provider sends the browser back (public; matched on `state`) |
| GET | `/api/providers/oauth/{id}` | How far along a sign-in is |
| POST | `/api/providers/oauth/exchange` | Finish a sign-in from a pasted redirect URL |
| POST | `/api/providers/oauth/complete` | Turn a finished sign-in into an account |
| POST | `/api/providers/{id}/test` | Re-check one account |
| PATCH | `/api/providers/{id}` | Rename it / set its colour — index only, the backend is never contacted (§3.9) |
| DELETE | `/api/providers/{id}` | Disconnect (`?force=1` to override the guard) |
| GET | `/api/files?path=` | List a folder (`&vault=` for a sub vault; absent is the main one) |
| GET | `/api/search?q=` | Find files and folders by name (`&path=` scopes to a subtree, `&vault=`, `&type=file\|folder`, `&limit=`) |
| POST | `/api/files` | Upload (multipart `files[]`, `path`, `overwrite`, `accounts`) |
| GET | `/api/files/{id}` | Metadata including part placement |
| GET | `/api/files/{id}/content` | **Serve at an offset** through `ChunkedReader` (`?download=1` to save) |
| GET | `/api/conversions` | Files still in the pre-chunking format |
| POST | `/api/files/{id}/convert` | Move one out of it (§4.5) |
| POST | `/api/files/{id}/stream` | Mint a stream ticket for one file (§9.5) |
| GET | `/stream/{token}/{name}` | Play it — public, ranged; the token is the credential |
| GET | `/api/files/{id}/health` | Per-part reachability, without downloading |
| POST | `/api/files/{id}/move` | Rename or move into another folder (index only, §5.6) |
| DELETE | `/api/files/{id}` | Erase every part, drop the entry |
| GET | `/api/folders` | Every folder in the vault, root first — the whole tree in one answer, for a destination picker |
| GET | `/api/folders/art?path=` | Which file's thumbnail a folder is drawn with, if any, and every file under it that could be (§3.11) |
| POST | `/api/folders/art` | Give it a picture (`path`, `id`), or take it away again with an empty `id` |
| POST | `/api/folders` | Create a folder |
| POST | `/api/folders/move` | Move a folder `from` one path `to` another, with everything under it (§5.6) |
| DELETE | `/api/folders?path=&recursive=` | Delete a folder |
| POST | `/api/relocate` | Move a file (`id`) or a folder (`path`) onto other `accounts` (§5.6); a different *count* of accounts changes the scheme and rebuilds the file; `"preview": true` prices it out of the index and moves nothing |
| GET | `/api/movies` | Whether a film database key is stored, and which folders are opted in (§3.10) |
| POST | `/api/movies/key` | Store the key — checked against the database before it is kept; `""` clears it |
| POST | `/api/movies/lookup` | Turn film lookup on or off for a `path` and everything beneath it |
| POST | `/api/movies/scan` | Match every unmatched video under a `path` (`"refresh": true` redoes the matched ones) |
| GET | `/api/files/{id}/movie` | The stored film, plus what the file name reads as |
| POST | `/api/files/{id}/movie` | Look one up: on the name, on a `query`, or on a `tmdb_id` chosen by hand |
| DELETE | `/api/files/{id}/movie` | Forget the film; the poster stays as the file's thumbnail |
| GET | `/api/files/{id}/movie/candidates?q=` | Search the database, storing nothing, to correct a match |
| GET | `/api/system/folders?path=` | Folders on the machine SAND runs on, for the folder picker (§8.5) |
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
PARSE_ERROR · MISSING_FILE · NEEDS_CONVERSION · NO_MOVIE_KEY · BAD_MOVIE_KEY ·
MOVIE_LOOKUP_OFF`

### 9.4 The WebDAV share

`sand serve --webdav` mounts the same vault at `/dav` as a WebDAV filesystem, so
a file manager or a player can open it as a drive instead of driving the API.
It is off by default: it is a second way in, and one that carries the password
far more often than the browser does.

`internal/davfs` is an adapter over five methods and nothing more. Reads go
through `ChunkedReader` (§4.3), so a player seeking into a film fetches the
chunk it lands in; writes go through `UploadStream`, so a PUT is piped into the
vault as it arrives rather than collected first. Where a file may be stored, how
it is split and what encrypts it are all still decided in `internal/vault`.

| WebDAV | Vault |
|---|---|
| `GET` with `Range` | `OpenReadSeeker` → seek → the chunks that range covers |
| `PUT` | `UploadStream`, overwriting a file already at that path |
| `PROPFIND` | `List` |
| `MKCOL` | `Mkdir`, and only one level: the parent must exist |
| `DELETE` | `Delete`, or `Rmdir` recursively for a folder |
| `MOVE` | `Move`, or `MoveFolder` for a collection — an index change, the parts do not travel |

**Authentication is the vault password over HTTP Basic**, and the username is
not checked: a vault has one owner (§15), so a name would distinguish nothing
and pretending otherwise invites treating it as a second secret.

Basic auth is stateless, so the password arrives on *every* request and a
playing film sends hundreds. Verifying each one properly is a 64 MB Argon2id
pass — a denial of service aimed at oneself — so a verified credential is
remembered for a minute, keyed by an HMAC of it under a key minted for this
process. The HMAC is what keeps the map from being a list of passwords; being
per-process makes the remembered form useless anywhere else. A remembered
credential still has to meet an unlocked vault, so locking takes access away
rather than being papered over by the cache.

A correct password against a *locked* vault unlocks it. A mount outlives the
idle timeout, and the alternative is a share that goes dead until someone opens
a browser. It is the same surface `/api/vault/unlock` already presents to
anyone who can reach the port. Requests to the share also count as use, so the
auto-lock does not fire halfway through a film.

**Renaming a folder** goes through `Vault.MoveFolder`, the same call the
browser's own move reaches (§5.6): every entry beneath it is rewritten in one
index write, so there is never a window where half a tree answers to its old
name, and a failure to persist rolls the whole rewrite back.

**Appending** stores the file again with the new bytes on the end, because the
vault stores whole files. What it does not do is hold either half in memory —
the stored file is read back as a stream and the new bytes follow it into the
same streaming upload, so the cost is bandwidth rather than RAM. WebDAV has no
append verb, so this is reached through the filesystem interface rather than
over the wire; `PUT` always truncates, and `O_TRUNC` beside `O_APPEND` truncates
too, as `os.OpenFile` does.

**On a plain listener the password crosses the network in the clear on every
request.** That is worse than the browser, which sends it once at sign-in, and
is why the share is opt-in and why `Start` says so. Put TLS in front of it —
`scripts/nginx-sand.conf`, or Tailscale Serve.

### 9.5 Stream tickets

Playing a film in VLC means giving VLC an address, and VLC has none of what
authenticates the app. The session cookie is `HttpOnly` and `SameSite=Strict`,
so it is neither readable by the page nor carried by another program; the share
is authenticated by the vault password itself, which is not a thing to put in a
URL and on a clipboard. Both are the wrong shape for *play this one file over
there*.

So a ticket: 32 random bytes standing for one file, minted by a session that has
already unlocked the vault, and good for that file and nothing else. It is
plainly a bearer credential — anyone holding the link can play that file — which
is what every property of it follows from.

| | |
|---|---|
| Scope | One file. Not the folder, not the index, not the vault |
| Lifetime | `DefaultStreamTTL`, 12 hours, slid forward on each request so a link in use never expires mid-film |
| Storage | Memory only — a link that outlived the process would outlive the unlocked vault it was minted from |
| Locking | `clear()` on lock and on auto-lock: the links are voided, not left to fail one request at a time |
| Reads | `OpenReadSeeker` (§4.3) behind `http.ServeContent`, so a seek costs the chunks that range covers |
| Keep-alive | Counts as external activity, so the auto-lock cannot fire mid-film — the hole a mounted share has |

The path ends in the file's own name (`/stream/{token}/film.mkv`) because a
player picks its demuxer off the extension before a byte arrives; the token
before it is what decides which file is served, so renaming that segment changes
nothing. The response is what `/content` gives a browser — the stored type,
`no-store`, `nosniff`, and anything that could execute in this origin forced to
a download.

The address is assembled in the browser from the origin it reached the server
on, not from anything the server reports about itself: a name the server guessed
would resolve nowhere else, and behind Tailscale Serve or a reverse proxy the
right answer is a host the server has never heard of.

Reaching VLC from the page is then three answers, because there is no fourth:
`vlc-x-callback://` on iOS (VLC's documented entry point), an `intent:` naming
`org.videolan.vlc` on Android (a bare `vlc://` drops the scheme and assumes
http, which would fetch an https share in the clear), and on a desktop a
two-line `.m3u`, which VLC registers itself for wherever it installs. Nothing
reports whether a scheme was handled, so the page watches for its own
`visibilitychange` instead and offers the address when none came.

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
| **One account compromised *and* the password guessed** | The manifest opens: tree, placement map, data key, and roughly half of each large file that account holds a part of. A whole file still needs a second account. See §3.6. |
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
| **A film database learning your library** | Off until a folder is opted in by name; one lookup per film, carrying a title guessed from the file name and nothing else about the file; the answer is stored so it is never asked twice (§3.10) |
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
- **A film database knowing what it was asked.** Sealing the answer into the
  index is not the same as the question never having been asked: TMDB sees a
  title guessed from a file name, and the machine's address, once per film, for
  the folders that were opted in (§3.10). Nothing about the file goes with it,
  and nothing goes at all for a folder that never asked — but the request is a
  real disclosure and no amount of encryption at rest changes that.

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
├── cmd/sand/                  # CLI: serve, vault, sub, remote,
│                              #   ls/find/put/get/rm/relocate (all take --in),
│                              #   archive/restore, manifest ls,
│                              #   vault backup/recover
├── internal/
│   ├── archive/               # encode.go — the in-memory pipeline both modes use
│   ├── compress/              # zstd
│   ├── crypto/                # Argon2id + AES-256-GCM
│   ├── splitter/              # split, XOR, reconstruct
│   ├── sandfile/              # binary .sand part format
│   ├── provider/              # provider.go, local, s3, webdav, gdrive, dropbox
│   ├── movie/                 # file name → film, and the film database client;
│   │                          #   the only package that speaks to a third party (§3.10)
│   ├── vault/                 # store (encrypted file), manifest, placement, transfer,
│   │                          #   relocate (moving parts between accounts), rekey,
│   │                          #   backup (writing the index out), recovery (reading it
│   │                          #   back), reclaim (onto a key of your own),
│   │                          #   subvault (vaults inside the vault), discover (import)
│   └── server/                # sessions, handlers, embedded SPA
├── web/src/                   # React file browser
│   ├── api.js  theme.js  App.jsx
│   ├── navigation.js          # the trail walked — vault and path per step
│   ├── view.js                # list or grid, sort key and direction (persisted)
│   └── components/            # LockScreen, AccountsPanel, FileBrowser, Toolbar,
│                              #   FileEntry (rows + tiles), BulkActions,
│                              #   MoveToFolder, Rename, FolderArt, PreviewModal,
│                              #   SubVaults, ImportVault,
│                              #   PdfPreview (pdf.js), FilmDetails, ui
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
- The chunk cache, the per-chunk single-flight and the background rechunk queue
  each carry their own leaf lock, on the same terms: taken after `mu` and never
  around a call that takes it. The rechunk queue follows the manifest backup
  syncer exactly — scheduling happens while `mu` is held, the work runs on its
  own goroutine once it is released, and `AwaitRechunk` is how a caller waits
  for it to settle.
- Re-checks after re-acquiring: the vault may have been locked mid-transfer, in
  which case the freshly written parts are rolled back.

---

## 14. Operational Notes

- `sand check --all` stats every part of every file and exits non-zero if
  anything is degraded or unrecoverable — suitable for a cron job.
- Disconnecting an account is refused when it would leave any file with fewer
  than two reachable parts, unless forced. Either way the shard records
  pointing at that account are pruned so the index keeps telling the truth.
- `Upload` and `Fetch` hold a file entirely in memory. `UploadStream` and
  `OpenReader` (§4.3) do not — they are bounded by the chunk window — but the
  HTTP API still goes through the older pair, so the browser is still the
  whole-file path (§15).
- Reading a file that is still stored whole converts it to chunks afterwards,
  in the background, one file at a time. It costs a download and an upload of
  whatever gets read, so `SetRechunkOnRead(false)` turns it off on a metered
  connection.

---

## 15. Not Built

- **A FUSE mount.** WebDAV (§9.4) covers a file manager and a player; a media
  server like Jellyfin wants a real path rather than a URL, and that needs a
  filesystem. `ChunkedReader` (§4.3) is the primitive it would sit on, the same
  one the share already uses, so it is a binding rather than a second
  implementation. A library scan would also need throttling: it reads the head
  of every file, which is a burst of requests against APIs that rate-limit.
- **Whole-file `Upload` and `Fetch` are still whole-file.** `UploadStream` and
  `ChunkedReader` bound their memory by the chunk window, but the older pair
  still take and return a complete `[]byte`, and the HTTP API still uses them.
  A file larger than RAM is storable and readable through the streaming pair
  only.
- **Repair** — re-uploading a missing part from the two that survive, rather
  than re-uploading the whole file
- **Configurable N-of-M** via Reed–Solomon instead of fixed 2-of-3
- **A browser OAuth flow** — Drive and Dropbox currently take a refresh token
  you obtain yourself
- **Multi-user access**; the vault is single-owner by design
- **Sync / conflict resolution**; SAND is a store, not a sync engine
