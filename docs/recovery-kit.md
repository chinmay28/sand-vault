# The Recovery Kit

*A design for one file that brings a vault back — clouds connected, tree
intact — onto a machine that has never seen it.*

---

## 1. What is already there, and what is missing

SAND already survives the loss of the vault file. Every connected account
carries `manifest.sand`, an envelope a password alone opens, and
`Vault.Recover` rebuilds the index from any one of them. §3.6 and §3.7 of the
architecture document describe it, and none of it is being replaced here.

But it recovers the *index*, and an index is not a vault. Walk through what
somebody actually does after a disk dies:

1. Install SAND on the new machine.
2. Create a vault.
3. **Connect Google Drive again.** Find the account. Sign in. Grant.
4. **Connect Dropbox again.** Same.
5. **Connect the S3 bucket again.** Find the access key. It was in a password
   manager that was on the dead machine.
6. **Connect the iCloud folder again.** The path is different on this machine
   — a different user name, or a different OS entirely.
7. Only now does the recovery prompt fire, and only now does the password do
   anything.

Steps 3–6 are the disaster. They are the part that takes an afternoon, the
part that fails when a bucket key is gone for good, and the part that a person
in the middle of a bad week is least equipped to do. The index was never the
hard problem — the credentials were, and `manifest.sand` deliberately does not
carry them: a copy of it sits on every account, so a credential inside it would
let one compromised account unlock all the others.

That refusal is correct, and it is exactly what leaves the gap. Here is
everything a vault holds, and where it currently survives:

| What | In `manifest.sand`? | Why |
|---|---|---|
| File tree, sizes, placement | yes | it is the manifest |
| Data key, retired generations | yes | without it the parts are noise |
| Thumbnails, film details, folder art | yes | index state, in the manifest |
| Sub-vault records | yes | carried sealed, opened by their own passwords |
| Which accounts existed (id, kind, name) | yes | `BackupAccount` |
| **Account credentials** | **no** | one compromised account would unlock the rest |
| **Account colour and declared capacity** | **no** | not in `BackupAccount` |
| **Default accounts, default scheme** | **no** | not in `Snapshot` |
| **Film database key** | **no** | `storeFile.Settings`, pointedly not replicated |
| **Read history** (`vault.sand.reads`) | **no** | a sidecar, sealed under the data key |
| **Account ids** in any usable form | **no** | reconnecting mints a fresh UUID |

Everything in the second half of that table is lost today, and the first row
of it is the one that costs the afternoon.

**The recovery kit is the manifest backup that never touches a cloud, and can
therefore carry the credentials.** That is the whole idea. One sentence,
one file, and the rest of this document is what follows from it.

---

## 2. The shape of the answer

> `sand vault kit export` produces `sand-recovery-kit-<date>.zip` and shows you
> a 25-character **recovery code**. On a fresh install, dropping that zip on the
> lock screen and typing that code gives you back the vault you had: every cloud
> connected, the tree exactly as it was, the films, the thumbnails, the sub
> vaults still shut, the read history still counting from where it stopped.
>
> The code is not in the zip. The zip is not the vault; the zip *and* the code
> are the vault.

Two properties make it work, and they are worth stating before the format:

**The kit preserves account ids.** `AddProvider` mints a `uuid.NewString()`
for every connection, which is why `Recover` has to list every account and
re-point every shard record by object key. A kit restores each account under
the id it already had, so the manifest's shard records are *already correct*
the moment the index is installed. Nothing needs remapping — including, and
this is the part that cannot be done any other way today, the shard records
inside sealed sub vaults, which is what `Manifest.AccountRemap` exists to
patch up after the fact.

**The kit is a starting point, not the answer.** A kit exported in March
describes March. If the disaster is in August, five months of uploads are
missing from it — but they are not missing from the accounts, because
`manifest.sand` is rewritten on every index change. So the import does not
stop at the kit: it uses the kit's credentials to reach the accounts, reads
the copies of `manifest.sand` sitting there, and takes whichever index is
newer. The kit gets you *connected*; the clouds tell you what you *have*.

That second property is what turns "restore a backup" into "recover from a
disaster", and it is why the kit carries the vault key as well as the data
key — see §4.3.

---

## 3. The zip

```
sand-recovery-kit-2026-08-19.zip
├── README.txt          human-readable, no secrets, for the day nothing else works
├── kit.sand            the sealed envelope — everything
├── manifest.sand       byte-identical to the copies on the accounts
└── fingerprint.txt     what this kit is, without opening it
```

**`kit.sand`** is the kit. Everything else in the zip is there for the days
when the app itself is not available.

**`manifest.sand`** is a straight copy of what `SyncManifestBackup` writes.
It costs nothing — the vault seals it anyway — and it means the two commands
that already work with no vault, no network and no accounts keep working from
the kit alone:

```
sand manifest ls  kit/manifest.sand          # the file tree, from a password
sand restore --manifest kit/manifest.sand \  # a whole file, from loose parts
     --parts a.p1.sand,a.p3.sand
```

Somebody who has the kit and two part files off an old external drive can
rebuild a file on a machine with no network, and that path must not depend on
anything new being implemented correctly.

**`fingerprint.txt`** is plain text and carries nothing secret:

```
SAND recovery kit
Kit id      3f2b9c1e-...
Created     2026-08-19T14:02:11Z
Built by    sand 0.9.4
Opened by   a 25-character recovery code (not in this file)
Accounts    4
Files       12,481 (2.31 TB)
Sub vaults  2
kit.sand    sha256:8a1f...  (1,904,332 bytes)
```

The kit id is there so that somebody holding three kits and two slips of paper
can tell which opens which — the code is stored against that id (§4.4), and
the export flow prints it beside the code.

Account *names* are not in it. "4 accounts" is what a person needs to know
whether they are holding the right kit; "Google Drive (personal), Dropbox
(work), s3://photos-cold-eu" is a map of somebody's life, and it belongs
inside the envelope with everything else.

**`README.txt`** says what the file is, what it can do, what opens it, and
that losing control of it is as bad as losing control of every cloud account
at once. It is written for a person who found a zip on a drive in a drawer in
2031 and has no idea what SAND is.

It is also the one place that states the arrangement plainly, because the
person reading it is by definition working out what they are holding: *this
archive is opened by a 25-character recovery code that was shown to you when
it was made. The code is deliberately not in here. Without it this file cannot
be opened by anyone, including you.*

The zip itself is not encrypted. Zip encryption is weak, non-portable and
would add a second secret for no gain — everything that matters is already
sealed. `README.txt` says so, so that a reader who can list the archive does
not conclude that their secrets are lying in the open.

---

## 4. `kit.sand`

### 4.1 The envelope

Deliberately the same shape as `Backup`, for the same reason it has that
shape: the KDF parameters have to travel with the ciphertext, because a reader
who has lost everything has nothing but the secret in their hand.

```go
// KitMagic identifies the envelope. A reader can tell a kit from a manifest
// backup before trying to open either.
const KitMagic = "SAND-KIT"
const KitVersion = 1

type KitEnvelope struct {
    Magic   string    `json:"magic"`
    Version int       `json:"version"`
    KitID   string    `json:"kit_id"`     // uuid, matches fingerprint.txt
    Created time.Time `json:"created_at"`
    KDF     kdfParams `json:"kdf"`
    Check   sealed    `json:"check"`      // seals "SAND-KIT-OK"
    Payload sealed    `json:"payload"`    // the Kit below

    // Secret says what opens this kit: "code" for a generated recovery code,
    // "password" for the vault password the exporter chose instead. It is in
    // the clear because it is not a secret and because an import that has to
    // guess asks the wrong question — "Recovery code" and "Vault password"
    // are different fields with different validation and different help text,
    // and the person typing into one of them is having a bad day. See §4.4.
    Secret string `json:"secret"`
}
```

`Check` gives the same distinction `OpenBackup` gives: a failed GCM tag on a
16-byte constant is a wrong code, a failure on the payload is a corrupt file. Those two need different words in front of a frightened user.

**The KDF is deliberately heavier than the vault's.** A vault password is
typed several times a day and Argon2id at 64 MB is the cost that buys. A kit
kit secret is typed once every few years, so the kit uses **t=8, m=512 MB,
p=4** — call it `crypto.KitArgon2Params()`. Roughly a second and a half on a
laptop, versus a hundred milliseconds; a rounding error at the only moment it
is ever paid, and a factor of forty against somebody grinding a stolen kit
offline. The parameters are in the envelope, so raising them later does not
strand an old kit.

### 4.2 The plaintext

```go
// Kit is what a recovery kit carries. It is a Snapshot with the things a
// manifest backup must not have: the credentials, the preferences, and the
// key that opens the backups sitting on the accounts.
type Kit struct {
    Version   int       `json:"version"`
    KitID     string    `json:"kit_id"`
    CreatedAt time.Time `json:"created_at"`
    AppVersion string   `json:"app_version"`

    // Snapshot is exactly the payload of a manifest backup, embedded whole.
    // Everything that already knows how to read one — Recover, restore
    // --manifest, manifest ls — reads a kit's without a line of change.
    Snapshot Snapshot `json:"snapshot"`

    // Accounts is the reason this file exists: the full provider.Config for
    // every connected account, credentials included, under the id it already
    // has. Snapshot.Accounts describes the same accounts without the means to
    // reach them; both are carried, and a mismatch is a corrupt kit.
    Accounts []provider.Config `json:"accounts"`

    // VaultKey is the key the lost vault's manifest backups are sealed under,
    // base64. See §4.3 — this is what lets an import read a newer index off
    // the accounts without the old password.
    VaultKey string `json:"vault_key"`

    // KDF is the lost vault's own parameters, kept so a restore under the same
    // password produces the same vault key rather than a re-derived one.
    KDF kdfParams `json:"kdf"`

    // Preferences are the store fields a manifest backup has no room for.
    DefaultAccounts []string `json:"default_accounts,omitempty"`
    DefaultScheme   string   `json:"default_scheme,omitempty"`
    MovieAPIKey     string   `json:"movie_api_key,omitempty"`
    ManifestBackupDisabled bool `json:"manifest_backup_disabled,omitempty"`

    // ReadHistory is the .reads sidecar's plaintext, carried decoded. It is
    // sealed under the data key on disk, and the data key is in Snapshot, so
    // carrying it sealed would have been the same secret twice.
    ReadHistory *readHistoryFile `json:"read_history,omitempty"`
}
```

Three things about that struct are choices rather than consequences:

**`Snapshot` is embedded, not restated.** The kit is a superset of a manifest
backup and says so structurally. `Vault.Recover`, `restoreWithManifest` and
`entryForParts` take a `*Snapshot` today; they take `kit.Snapshot` tomorrow,
unchanged. Sub-vault records, retired key generations, thumbnails, films,
folder art and the policy all come along because they are already in there.
Every future field added to `Snapshot` is in the kit for free, which is the
only version of this that stays true.

**Credentials are carried verbatim, not re-derived.** `provider.Config.Options`
holds OAuth refresh tokens, S3 keys, WebDAV passwords, and the folder paths
for the sync-folder backends. All of it, exactly as `storeFile.Providers`
holds it — plus `Color`, `Capacity`, `Name`, `AddedAt` and, critically, `ID`.

**`VaultKey` is 32 raw bytes of the derived vault key.** It is the one item
here that is not simply "a field the store already had", and §4.3 is why.

### 4.3 Why the kit carries the vault key

The kit is opened by a code that has nothing to do with the vault password
(§4.4). But the copies of `manifest.sand` on the accounts — the copies that are
*newer than the kit*, and are the only reason the August files come back from a
March kit — are sealed under the vault key, which comes from the vault
password.

So either the kit's secret is forced to equal the vault password, or the import
asks for the vault password on top of the code, or the kit carries the derived
key. The first is the option §4.4 exists to reject and the second asks a person
mid-disaster for the one secret most likely to be gone. The third is the only one that stays true after a
password change nobody remembered to re-export for, and it gives away nothing
new: the kit already carries the data key, which opens the file contents. The
vault key only opens an index the kit already contains a copy of.

The consequence is stated plainly in `README.txt` and in the export dialog:
**this file is the vault.** Not a description of it — the thing itself, minus
the bytes.

If the vault password was changed *after* the kit was made, the carried vault
key no longer opens the copies on the accounts. That is a recognised state,
not an error: §6.5.

### 4.4 The secret that opens it

**By default it is a code SAND generates, not a password the user chooses.**

A vault password is typed several times a day, which is what makes it
memorable. A kit secret is typed once in three years — the profile of a secret
people forget — and the event that destroys the machine is often the same
event that destroys the password manager. Worse, a kit sealed under "the vault
password" is sealed under *the vault password as it was on the day of the
export*, so a kit made in 2026 and a password changed in 2027 leave somebody
guessing which of their last four passwords was current in March.

A generated code fixes all three. It cannot be forgotten separately from the
password because it was never the password. It is pinned to the kit rather
than to a moment in a password history. And it has real entropy, which changes
what the KDF is for.

#### The code

```
K7QM4-9WXTB-2FGHN-5PRVD-8JC3S
```

- **Crockford base32** — `0123456789ABCDEFGHJKMNPQRSTVWXYZ`. `I`, `L`, `O` and
  `U` are absent: the first three because a code gets handwritten and read back
  months later by someone who did not write it, and `U` because an alphabet of
  32 symbols in five-character groups will eventually produce a word nobody
  wants printed in their safe. Decoding folds case and maps `I`/`l` → `1` and
  `O` → `0`, so a transcription that "corrects" them still opens the kit.
- **24 symbols of entropy = 120 bits**, drawn from `crypto/rand`. Grouped five
  and five with hyphens purely for the hand and the eye; the hyphens are not
  part of the value.
- **One check symbol**, the 25th, is the first five bits of
  `SHA-256(the 120 bits)`. Its only job is to tell *you have mistyped this*
  from *this is the wrong kit* — see below. It is not integrity; GCM is
  integrity.

Normalisation before anything else touches it: strip everything that is not an
alphanumeric, upper-case, fold `I`/`L`/`O`, then verify the check symbol. That
happens **before** the KDF runs, which is the point of having it: a typo is
caught in microseconds and answered with *"that code has a typo in it — check
the fourth group"*, instead of costing a second and a half of Argon2 and then
producing *"wrong code"*, which is the message that makes a person conclude
their backup is dead.

#### The KDF stays expensive anyway

120 bits does not need stretching. A single HKDF would be sound, and Argon2id
at 512 MB is, against a real code, security theatre with a cost.

It stays for two reasons. The opt-out below puts a human-chosen password
through the same envelope, and *that* case needs every bit of the 512 MB. And
one code path that cannot be configured wrong is worth more than 1.4 seconds
paid once every few years. `Secret` records which kind it was, for the prompt;
it does not select a weaker derivation.

#### Where the code lives

**Never in the zip, and never in its filename.** A code inside the artefact it
opens is not a secret, and a filename travels with the file through every copy,
sync and mail attachment it will ever see. The export flow shows it once, in a
panel the user must acknowledge, offers it as a separate small `.txt` to save
somewhere else, and says in one line what the arrangement means: *the kit and
the code in the same place are the vault in one place.*

**But it is not shown only once.** The generated code is stored in the vault's
own `Settings` section — sealed, beside `MovieAPIKey`, and pointedly *not* in
the manifest, so it is never replicated to an account. While the vault is
alive, **Settings → Recovery kit → Show code** displays it again.

That looks like it gives something away and does not. Reading it requires an
unlocked vault, and anybody with an unlocked vault can export a fresh kit with
a fresh code in ten seconds. What it buys is the ordinary case: a person who
still has their working machine, has mislaid the slip of paper, and would
otherwise be holding a zip that nothing on earth opens. The code is stored per
kit id, so a vault that has exported three kits can still say which code
belongs to which.

The one arrangement SAND cannot help with is the one that matters most —
machine gone, slip of paper gone. `verify` (§7) exists partly as the forcing
function for that: it asks for the code, so running the fire drill twice a year
is also a check that you can still find it.

#### The opt-out

**Use my vault password instead** is a checkbox on the export dialog and
`--use-vault-password` on the CLI. It is offered without discouragement,
because for somebody who keeps a long generated password in a password manager
that is itself backed up, it is a real answer and one fewer secret to file.

It sets `Secret: "password"`, stores nothing in `Settings`, and changes what
the import asks for. It does not change the KDF, the envelope or anything else
in this document. What the dialog says about it is one sentence: *the kit will
open with the password you are using today, not with whatever you change it to
later.*

---

## 5. Export

```
sand vault kit export --out ~/sand-recovery-kit.zip
POST /api/vault/kit    →  application/zip
```

`Vault.ExportKit(ctx, opts ExportOptions, w io.Writer) (*KitFingerprint, error)`

where `ExportOptions` carries only `UseVaultPassword bool` and, when it is set,
the password to check against — so the ordinary call passes nothing and gets a
generated code back in the fingerprint.

0. Unless `UseVaultPassword`, mint the recovery code: 120 bits from
   `crypto/rand`, encoded Crockford, check symbol appended. It is returned to
   the caller and written into `Settings.KitCodes[kitID]`; it is not written
   into the zip, the filename, the log, or the fingerprint file.
1. Refuse if locked. The kit is built from the decrypted store.
2. Build `Kit`: `snapshotLocked()` for the snapshot, `v.providers` verbatim for
   the accounts, `v.vaultKey` for `VaultKey`, the store's preference fields,
   and `openReadHistory` for the sidecar.
3. Derive under `KitArgon2Params()` with a fresh 16-byte salt, from the
   normalised code or from the vault password, and set `Secret` to say which.
   Seal the check block, seal the payload, marshal the envelope.
4. Seal a `manifest.sand` from the same snapshot under the vault key — the
   same bytes `SyncManifestBackup` would write this second.
5. Compute the fingerprint over the finished `kit.sand`.
6. Stream the zip: `README.txt`, `kit.sand`, `manifest.sand`,
   `fingerprint.txt`. Deflate — the manifest is JSON and compresses four to
   one, which matters on a vault with a hundred thousand entries.
7. Record `LastKitExportAt` and `LastKitFileCount` in the store.
8. Zero the derived key and the copy of the vault key. Nothing is written to
   a temporary file at any point: the zip is assembled straight into the
   response writer, so a kit never exists on disk anywhere but where the user
   put it. The code itself is returned out-of-band — in the JSON of a
   companion request, never in the zip stream — for the reason in §4.4.

**One refusal.** Writing the kit into a folder that is the root of a connected
`local`, `icloud` or sync-folder account is refused outright. It is the exact
mistake the credential separation exists to prevent — the kit would be
uploaded to the cloud whose credentials it contains, by the sync client, in
the background, silently. The CLI checks the `--out` path against every
path-configured account; the browser cannot see where a download lands, so the
export dialog says it in one line instead.

**Staleness.** `GET /api/vault/kit` answers with what has changed since the
last export:

```json
{
  "exported_at": "2026-03-02T09:14:00Z",
  "kit_id": "3f2b9c1e-...",
  "secret": "code",
  "code_retained": true,
  "age_days": 170,
  "files_added": 312,
  "accounts_changed": true,
  "password_changed_since": false
}
```

`code_retained` says whether this vault can still show you the code for that
kit. It is false for a kit sealed under the vault password, which is a fact the
settings panel has to state rather than imply: for those, `password_changed_since`
being true means the kit opens under a password the user may no longer know,
and there is no **Show code** to fall back on.

`accounts_changed` and `password_changed_since` are the two that matter, and
they are the two the UI leads with, because a kit that is merely old still
recovers everything through §6.4 while a kit that predates an account or a
password change recovers less. A vault that has never exported one answers
`exported_at: null`, which is what the settings panel draws its nudge from.

---

## 6. Import

```
sand vault kit import ~/sand-recovery-kit.zip
POST /api/vault/kit/import   (multipart: file + secret + new_password?)
```

`Vault.ImportKit(ctx, kit *Kit, opts ImportOptions) (*KitImportReport, error)`

Unauthenticated at the HTTP layer, in the same way and for the same reason
`POST /api/vault/init` is: on a fresh install there is no session to have, and
what stands in front of it is possession of the file plus the secret that
opens it.

**What it asks for is read off the envelope, not guessed.** `inspect` parses
the header without any secret at all, so the import screen knows before the
user types whether this kit wants a recovery code or a vault password, and
labels the field accordingly. A code field normalises and check-sums as you
type — the last group completing turns the border green before anything is
submitted — and rejects a typo without a round trip. A password field does
neither, because there is nothing to check a password against until the KDF
has run.

### 6.1 The phases

**Phase 0 — read and refuse.** Parse the zip, parse the envelope, check the
magic and the version, derive, open the check block. Then: refuse to import
over a vault that holds files, unless `replace: true` was given explicitly.
`Recover` already refuses on exactly these terms and for exactly this reason —
adopting a data key when files depend on the current one destroys them.

**Phase 1 — mint the vault.** `Init` under the new password, which defaults to
the vault password the kit was sealed under, when it was sealed under one, and
otherwise must be chosen — a recovery code is not a password to run a vault on. Then adopt, in one write: the
data key and its id, every retired generation, the policy, the default
accounts and scheme, the film key, the manifest-backup preference, and the
sub-vault records verbatim.

If the new password differs from the old, the vault key changes and the copies
of `manifest.sand` on the accounts have to be rewritten — `BackupNeedsForce`
is set so that the guard against overwriting a foreign backup steps aside for
exactly this vault.

**Phase 2 — reconnect, keeping the ids.** A new `Vault.RestoreProvider(cfg)`,
which is `AddProvider` without the two lines that mint a fresh `ID` and
`AddedAt`, and without the name-collision check (a kit's accounts are
internally consistent by construction; two accounts of the same name in one
kit came from a vault that already had them).

Every account is pinged in parallel, and each lands in one of four states:

| State | Meaning | What the user does |
|---|---|---|
| `connected` | ping succeeded | nothing |
| `needs_reauth` | OAuth refresh token expired or revoked | one button: sign in again |
| `needs_path` | the configured folder does not exist here | one button: find this folder |
| `unreachable` | network, DNS, 5xx, bucket gone | retry later; `resume` picks it up |

**A failure here is never fatal to the import.** The account is restored into
the vault with its id and its credentials intact and its state recorded; the
tree still comes back, and the files on that one account are marked short
exactly as `RecoveryReport` marks them. This matters more than any other line
in this document: a year-old OAuth token is the *expected* case, not the edge
case, and an import that refused to finish because Dropbox wanted a fresh
sign-in would be useless precisely when it is needed.

Both repairs preserve the account id, which is what keeps the manifest correct
across them:

- **Re-auth** runs the existing OAuth flow (`POST /api/providers/oauth/start`)
  and writes the new tokens into the *existing* config rather than adding an
  account.
- **Re-point** is a new `PATCH /api/providers/{id}` field for the path option,
  guarded by a probe: the folder must contain at least one `.sand` object the
  index expects to find there, or the user is warned they have picked the
  wrong folder. That probe is worth the code — "find this folder" on a new
  machine with a differently-named home directory is where a person picks
  their *Downloads* folder and then wonders why nothing came back.

**Phase 3 — install the index.** The kit's manifest, as it stands.

At this point the tree is back and the app is usable. Everything after this is
about being *current* rather than being *usable*, and each phase reports
rather than fails.

**Phase 4 — take the newer index off the clouds.** For every account that
reached `connected`, download `manifest.sand` and open it with the kit's
`VaultKey`. Compare `CreatedAt` against the kit's. If any is newer:

- Adopt its manifest in place of the kit's.
- Union its key generations with the kit's, in case a password change happened
  between the export and the disaster.
- Note in the report which account the index came from and how much newer it
  was, because "your kit was 170 days old and the index came off Dropbox,
  dated four days before the crash" is the sentence that tells somebody they
  have actually got everything back.

Where a cloud copy does not open under the carried vault key, §6.5.

**Phase 5 — discover.** Run `locateShards` over every reachable account: list
each one, map object key to account. Because the ids were preserved, the
overwhelmingly common result is that the index is already right, and this pass
is a verification rather than a repair. It is still run, and it still fixes
what it finds:

- A shard record naming account A whose object is actually on account B is
  re-pointed. (Relocations that happened after the kit was exported land here,
  as does a folder that was moved between accounts by hand.)
- A record whose object is on no reachable account is left alone and counted
  unreachable — not dropped. One failed listing must never throw placement
  away; that is what the health check is for.
- An object on an account that the index does not name is an **orphan**, and
  is counted and reported rather than deleted. Orphans mean the index is older
  than the storage, and the honest thing to do with somebody's data that the
  index has forgotten is to say so.

**Phase 6 — the sidecar.** Write `vault.sand.reads` from `Kit.ReadHistory`,
sealed under the adopted data key. The read counters resume from where they
stopped rather than from zero, which is a small thing and exactly the kind of
small thing whose absence makes a restored machine feel like somebody else's.

**Phase 7 — push the index back.** `SyncManifestBackup(ctx, force=true)`, so
every account carries the current index under the current password, and so the
foreign-backup guard stops being armed against this vault. Same closing move
as `handleRecoveryRun`.

### 6.2 What comes back

```go
type KitImportReport struct {
    KitID      string    `json:"kit_id"`
    KitCreated time.Time `json:"kit_created_at"`

    // IndexSource says where the installed index came from: "kit", or the id
    // of the account whose manifest.sand was newer. IndexAt is its date.
    IndexSource string    `json:"index_source"`
    IndexAt     time.Time `json:"index_at"`

    Accounts []KitAccountResult `json:"accounts"`

    // The same pairs RecoveryReport counts in, for the same reason: two of
    // three parts rebuild a file, so one account short costs nothing and two
    // costs the file, and the bytes are the answer to "how bad is this".
    Files            int   `json:"files"`
    Recoverable      int   `json:"recoverable"`
    Bytes            int64 `json:"bytes"`
    RecoverableBytes int64 `json:"recoverable_bytes"`

    Missing  []MissingFile    `json:"missing,omitempty"`
    Blocking []MissingAccount `json:"blocking,omitempty"`

    Repointed int   `json:"repointed"`
    Orphans   int   `json:"orphans"`
    OrphanBytes int64 `json:"orphan_bytes"`

    SubVaults int      `json:"sub_vaults"`   // present and shut
    Warnings  []string `json:"warnings,omitempty"`
}
```

`MissingFile` and `MissingAccount` are the existing types. The report leads on
the shortfall, the same way `RecoveryReport` does, and for the same reason: a
list of what worked is not what the person reading it needs.

### 6.3 Sub vaults

Carried verbatim in `Snapshot.SubVaults`, restored verbatim, and shut. Each
opens afterwards with its own password, exactly as before.

The kit does something for sub vaults that no other recovery route can. A
recovery from `manifest.sand` cannot re-point the shard records *inside* a
sealed section — it does not hold the password — so it leaves
`Manifest.AccountRemap` behind and patches each sub vault the first time it is
opened. A kit import preserves the account ids, so **the records inside a
sealed sub vault are correct without ever being touched**. No remap is
written, and a sub vault that is never opened again is nonetheless intact.

### 6.4 The old kit

The case worth walking through, because it is the case:

> Kit exported in March. Machine dies in August. 312 files added in between,
> and one new Backblaze account connected in June.

Import: four accounts come back from the kit and connect. The Backblaze
account is not in the kit at all. Phase 4 downloads `manifest.sand` from
Google Drive, dated two days before the crash, and adopts it — so all 312 new
files are in the tree, including the ones whose parts are on Backblaze. Phase
5 lists the four accounts it has and re-points what it can. The report says:
12,793 files, 12,481 recoverable, and **one account blocking**, named
`Backblaze (photos)`, kind `s3` — because the newer manifest names it even
though the kit never did.

The user connects Backblaze by hand — the one credential they have to find,
instead of five — and `POST /api/vault/recovery/resume` re-points the rest.
`Reconcile` already does precisely this job and needs no changes.

That is the honest ceiling of an old kit, and it is a good one: **a kit is
never worse than no kit, and the only accounts it cannot save you are the ones
it never knew about.**

### 6.5 The kit that predates a password change

**The kit itself still opens.** This is the case a generated code exists for:
the code is not the vault password and a password change does not touch it, so
the credentials, the tree and every file the kit describes come back exactly as
they would have. Under the §4.4 opt-out the same event is far worse — the kit
opens under a password that was retired eighteen months ago and the user is
guessing.

What is affected is only phase 4. The carried `VaultKey` no longer opens the
copies on the accounts. Phase 4
detects it — the check block in each `manifest.sand` fails, uniformly, on
every account — and distinguishes it from corruption by its uniformity.

The import does not fail. It reports:

> The index on your accounts was sealed after this kit was made — your vault
> password changed on **12 June**. Type the password you were using when the
> machine died and the newer index comes back too; skip, and you get the
> {date} index from the kit.

Skipping is a real option and is offered as one — and it is the *default* on a
code-sealed kit, because the person in front of this dialog has already proved
they hold the code and is now being asked for a second, older secret they may
never have had. The kit's index is older but complete for what it describes,
and every account is already connected, which is nearly all of the value. What
skipping costs is named exactly: the files added between the export and the
password change, which the report counts.

### 6.6 Rejected: shipping the vault file verbatim

The obvious design is a zip containing `vault.sand` and `vault.sand.reads`,
byte for byte. The store file is already sealed under the vault password and
already contains the credentials, the manifest, the sub vaults, the settings
and the defaults. Import would be `cp`. Zero new crypto.

It was rejected for three reasons, in order of weight:

1. **It welds the kit's secret to the vault password**, which forecloses §4.4
   entirely: there is no room for a generated code, so the kit can only ever
   open under a password the user chose, and a password change quietly leaves
   the kit answering to one they have stopped using.
2. **It cannot be inspected.** `fingerprint.txt` and
   `POST /api/vault/kit/inspect` both need structure the store file does not
   expose without full decryption, and "what is in this zip" is a question
   people ask of a backup constantly.
3. **It is a format by accident.** `StoreVersion` is an on-disk format that
   moves for reasons that have nothing to do with recovery, and `minStoreVersion`
   would become a constraint on how far back a kit can be read. An explicit
   `KitVersion` with an embedded `Snapshot` is one number that means one thing.

---

## 7. The fire drill

```
sand vault kit verify ~/sand-recovery-kit.zip
POST /api/vault/kit/verify
```

**An untested backup is a rumour.** `verify` opens a kit, and then, changing
nothing anywhere:

- builds a live provider from each carried config and pings it, so every
  credential in the kit is proven to still work;
- lists each account and checks the carried index against what is really
  there, reporting the same `Recoverable`-against-`Files` pair a real import
  would;
- reports the drift between the kit's index and the current one — how many
  files have been added since, how many of them a kit-only recovery would
  miss if every account were also unreachable.

It runs against the vault the user still has, and it answers the question the
export dialog cannot: *if I needed this today, what would I get back?*

**It asks for the code, and that is half of why it exists.** The drill could
open the kit from the live vault's own key material and skip the prompt
entirely. It does not, because the failure this design cannot otherwise catch
is the slip of paper that went missing in a house move two years ago, and the
only way to catch it is to make somebody find the paper. A drill that passes
proves the credentials still work *and* that the secret is still in the
world — and the day it fails on the second, the vault is still alive and a
fresh kit is ten seconds away.

For a kit whose code is still retained (§4.4) the drill can offer **use the
stored code** as a second button, which turns it back into a one-click check
of the credentials alone. It is the weaker drill and is labelled as one.

This is the single highest-value item in the design after the import itself.
It is the difference between a kit somebody made once and a kit somebody
trusts, and it costs one ping and one listing per account.

---

## 8. Security

**One file, everything.** The kit holds every credential, the data key, the
vault key and the whole index. Somebody who has the kit and the code has the
vault. This is not a weakening of the model — the same person with the vault
file and the vault password has exactly the same thing — but it is a
*concentration* of it, in an artefact designed to be copied and stored
elsewhere, and the design handles it by saying so loudly rather than by
pretending otherwise.

**Somebody who has the kit alone has nothing.** 120 bits, uniformly random,
behind Argon2id at 512 MB. There is no dictionary, no reuse from another
breach, no birthday, no shoulder to surf: the arithmetic is not close enough
to be worth writing out. This is the real reason to prefer a code over a
password, and it is worth being blunt about which way round the benefit runs —
the code is not chosen to make the kit harder to crack, since a stolen kit was
never the likely loss. It is chosen because it cannot be forgotten *alongside*
the password, and the security is a side effect of fixing that.

**The two failure directions are not symmetric, and the design is not neutral
between them.** Losing the code loses everything the kit could have restored;
someone else finding the code, without also finding the kit, gains nothing at
all. So every choice above leans toward retrievability: the code is stored in
the live vault, `verify` drags it into daylight twice a year, and the export
panel nags about a kit whose code was never confirmed as written down. The
lean is deliberate and it is the right one — but it does mean an unlocked
vault is a path to the code, which is stated here rather than left to be
discovered.

**The refusals, and the one that is deliberately absent.** The manifest backup
refuses to be written under redundant placement with fewer than three
accounts, because a single account would then hold enough parts to rebuild a
file *and* the key to do it. That refusal is about a file the vault puts on
somebody else's server. A kit goes where the user puts it, so the reasoning
does not carry across and `backupRefusalLocked` is not consulted. What *is*
enforced is §5's refusal to write a kit into a synced folder, which is the
same hazard arriving by a different road.

**Handling.** The export path never touches a temporary file. The derived key,
the vault key copy and the marshalled plaintext are zeroed. The code and the
password are never logged, never in a URL, and never a GET parameter — which is
why export is a POST that returns a body. The code is never inside the zip,
never in its filename, and never in `fingerprint.txt`.

**A kit you have lost control of** is the one case with no automatic remedy,
and the docs say what to do: change the vault password (which rotates the data
key), run `sand vault migrate` to re-encrypt every file onto the new
generation, and rotate the credentials of every account at the provider. The
old kit then opens a vault whose key opens nothing that is still stored. Note
the order — the credentials are the part SAND cannot rotate for you.

Destroying the code is *not* a substitute for any of that. It is a value the
vault has been holding for you, and the person who took the kit may have taken
it from a screenshot, a note app or the `.txt` beside the zip. Deleting the
retained copy is offered as tidying up after the rotation, never instead of
it, and the UI does not present it as revocation.

---

## 9. Surfaces

### HTTP

| Route | Session | Purpose |
|---|---|---|
| `GET /api/vault/kit` | yes | staleness: last export, drift since |
| `POST /api/vault/kit` | yes | build and stream the zip |
| `GET /api/vault/kit/code/{kit_id}` | yes | show the retained code again (§4.4) |
| `POST /api/vault/kit/verify` | yes | the fire drill |
| `POST /api/vault/kit/inspect` | no | what is in this zip; changes nothing |
| `POST /api/vault/kit/import` | no | the import; starts a session on success |

`inspect` and `import` sit outside the session for the same reason
`/api/vault/init` does: on the machine where they matter there is no vault to
have a session with. `import` refuses against a vault holding files unless
told otherwise, which is the guard that matters.

### CLI

```
sand vault kit export  [--out FILE] [--use-vault-password]
sand vault kit inspect FILE
sand vault kit verify  FILE [--code-file FILE]
sand vault kit import  FILE [--vault PATH] [--replace] [--code-file FILE]
sand vault kit code    [KIT_ID]
```

`export` prints the code to the terminal and to nothing else, on its own, with
the kit id beside it — so `sand vault kit export --out /mnt/usb/kit.zip` in a
script still puts the one thing that must not be in the file in front of a
human. `--use-vault-password` seals under the vault password instead and prints
nothing.

`inspect` and `import` are the only vault commands that work with no vault at
all — `import` creates one, and `inspect` also reports whether the kit wants a
code or a password, which is what lets `import` prompt with the right word.
`sand vault kit code` reads a retained code back out of a live vault.

Codes and passwords come from a prompt or a file, never from an argument, so
they stay out of shell history and out of `ps`. A `--code-file` is read,
normalised and check-summed before the KDF runs, so a mangled file fails
instantly and legibly.

### Browser

**Export** lives in `VaultSettings.jsx`, under a heading of its own: the
staleness line ("Exported 170 days ago · 312 files added since"), the button,
one checkbox for *use my vault password instead*, and the sentence about not
saving the kit into a synced folder. Afterwards, a **Test this kit** link
running `verify`, and — for a kit whose code is retained — **Show code**.

The code panel is the one screen in SAND that must not be skimmed past, so it
is the one screen that behaves like a modal with a job: the code in the
project's mono face, big, grouped, with a copy button and a **Save as a text
file** button; the kit id beside it; one line saying it is not inside the zip
and cannot be recovered from it; and a checkbox — *I have written this down or
saved it somewhere else* — that the dismiss button waits on. Not because a
checkbox proves anything, but because the alternative is a panel people close
by reflex, and this is the reflex that costs them the vault.

It is a deliberately soft gate: **Show code** in settings is right there
underneath, and the panel says so. A user who clicks the checkbox without
reading has lost nothing, because the vault still holds the code. The gate
exists for the person who would otherwise never have registered that a second
artefact exists at all.

**Import** is a third door on `LockScreen.jsx`, beside "create a vault", shown
only when no vault exists: *I have a recovery kit*. Drop zone or file picker;
then `inspect` runs and the field that appears is labelled **Recovery code**
or **Vault password** according to `Secret`, with the code field grouping,
folding and check-summing as it is typed. Then a live progress list built from
the phases — one row per account as it connects, then the index, then the
tally. The report screen is where the four account states become buttons, and
it is reachable again from the accounts panel afterwards, because "sign in to
Dropbox again" is not something to lose by closing a dialog.

The existing `RecoverVault.jsx` prompt is untouched and stays the answer for
somebody with no kit. It gains one line — *Have a recovery kit? Import it
instead* — and the kit import gains its mirror, so neither route is a dead end
for somebody who took the other one.

---

## 10. Failure modes

| What went wrong | What happens |
|---|---|
| Code with a typo | caught by the check symbol before the KDF runs — "that code has a typo in it", not "wrong code" |
| Code for a different kit | check symbol passes, GCM check block fails — "this code does not open this kit"; `fingerprint.txt` names which kit it is |
| Code lost, vault alive | **Show code** / `sand vault kit code` reads it back out of the vault (§4.4) |
| Code lost, vault gone | nothing opens the kit. The one unrecoverable state in this design, and the reason `verify` asks for the code |
| Wrong vault password (opt-out kits) | `ErrWrongPassword`, from the check block, before anything is touched |
| Truncated or edited `kit.sand` | "this kit is damaged" — a GCM failure on the payload after the check passed |
| Kit from a newer SAND | refuses on `KitVersion`, naming the version that wrote it |
| Vault already holds files | refuses unless `replace` — `Recover`'s existing rule |
| One account's OAuth expired | `needs_reauth`; import completes; one button fixes it |
| A folder path does not exist here | `needs_path`; import completes; picker fixes it, probe confirms it |
| An account is gone for good | its files counted short, the account named in `Blocking` |
| Kit older than the accounts | newer index adopted off a cloud; §6.4 |
| Vault password changed after export | offer to type the old one; skipping still works; §6.5 |
| An account exists that the kit never knew | named in `Blocking`; connect it, then `recovery/resume` |
| Objects on an account the index forgot | counted as orphans, reported, never deleted |
| Import interrupted halfway | the vault file is written once per phase; re-running is safe — phase 0 sees the files and asks for `replace` |

---

## 11. Building it

| Order | File | What |
|---|---|---|
| 1 | `internal/crypto/argon2.go` | `KitArgon2Params()` |
| 1b | `internal/vault/kitcode.go` | Crockford encode/decode, check symbol, `NewKitCode`, `NormalizeKitCode` |
| 2 | `internal/vault/kit.go` | `Kit`, `KitEnvelope`, `SealKit`, `OpenKit`, `ExportKit` |
| 3 | `internal/vault/kitzip.go` | zip assembly, `README.txt`, `fingerprint.txt` |
| 4 | `internal/vault/vault.go` | `RestoreProvider` — `AddProvider` keeping id and `AddedAt` |
| 5 | `internal/vault/kitimport.go` | `ImportKit`, the seven phases, `KitImportReport` |
| 6 | `internal/vault/kitverify.go` | `VerifyKit` |
| 7 | `internal/vault/store.go` | `LastKitExportAt`, `LastKitFileCount`, `Settings.KitCodes` |
| 8 | `internal/server/handlers_kit.go` | the five routes |
| 9 | `cmd/sand/kit.go` | the four commands |
| 10 | `web/src/components/RecoveryKit.jsx` | export panel, staleness, verify, the code panel and **Show code** |
| 11 | `web/src/components/ImportKit.jsx` | the third door, the code field, the report |
| 12 | `tests/`, `internal/vault/kit_test.go` | §12 |

`kitcode.go` is first because it is forty lines with no dependencies and the
export signature is shaped around it; getting the alphabet and the check symbol
settled before anything seals against them avoids a format change after kits
exist in the world. Phases 1–3 of the import (mint, reconnect, install) are the
deliverable that stands alone: a kit that connects the clouds and restores the tree is the
whole of the user's ask. Phases 4–5 (the newer index, the discovery pass) are
what make an *old* kit as good as a fresh one, and are worth doing second
rather than never. `verify` is worth doing third and before any polish,
because it is what makes anyone believe the first two.

---

## 12. Tests

The round trip is the test that matters, and it can be written entirely
against `local` providers in a temp dir:

- **Round trip.** Vault with three local accounts, a nested tree, a chunked
  file, a folder with film details and a sub vault. Export. Delete the vault
  file and the sidecar. Import into a fresh path. Assert: identical manifest,
  identical account ids, every file reads back byte-for-byte, read history
  preserved, sub vault shut and then openable with its own password.
- **Ids are preserved**, and therefore no `AccountRemap` is written and the
  sub vault's shard records are correct untouched.
- **Old kit, newer cloud.** Export, add files, export nothing, import — the
  newer `manifest.sand` is adopted and `IndexSource` names the account.
- **Missing account.** Export, import with one account's root removed —
  completes, files short, account in `Blocking`, and connecting it plus
  `Reconcile` finishes the job.
- **Moved path.** Import with an account's directory moved — `needs_path`, and
  the re-point plus a discovery pass recovers it.
- **The code, on its own.** Round-trip every value through encode/decode;
  reject a bad check symbol; accept `i`/`l`/`o` folded to `1`/`1`/`0`, lower
  case, missing hyphens, stray spaces; confirm the alphabet contains no `I`,
  `L`, `O` or `U`; confirm 10,000 generated codes are distinct and drawn from
  `crypto/rand`. Table-driven and cheap — this is the piece a person's whole
  vault hangs off, and it is the piece most likely to be quietly wrong.
- **A single-character typo in every position** of a valid code is rejected by
  the check symbol, as is every adjacent transposition. Both fail *before* the
  KDF — assert that too, by timing or by a counter, since the fast rejection is
  the feature.
- **Code retention.** Export, read the code back via `sand vault kit code`,
  import with it. Then assert the code is absent from every byte of the zip, of
  the filename, and of `fingerprint.txt`.
- **The opt-out.** A kit sealed with `--use-vault-password` sets
  `Secret: "password"`, retains nothing in `Settings`, and opens with the
  password rather than a code.
- **Wrong code for the right kit**, **wrong vault password**, **truncated
  kit**, **future `KitVersion`**: four distinct errors with four distinct
  messages.
- **Import over a live vault** refuses without `replace`.
- **Password changed after export**: cloud manifests do not open, the old
  password is offered, skipping still yields the kit's index.
- **Export into a synced folder** is refused.
- **No plaintext leaks**: grep the finished zip for a known credential string,
  a known filename, a known account name and the recovery code. Only
  `fingerprint.txt`, `README.txt` and the envelope's headers may be readable,
  and none of them may contain any of the four.
