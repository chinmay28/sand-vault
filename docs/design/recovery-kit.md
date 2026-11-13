# The recovery kit

A recovery kit is one file that brings a vault back onto a machine that has
never seen it: every cloud reconnected, the tree intact. This page covers why it
exists, the zip and the sealed envelope inside it, the recovery code, export,
import, the fire drill (`verify`), and the security trade. The code is
`internal/vault/kit.go`, `kitcode.go`, `kitzip.go`, `kitimport.go` and
`kitverify.go`. Using one is described in the [recovery guide](../guide/recovery.md).

## Why it exists

SAND Vault already survives the loss of the vault file: every account carries
`manifest.sand`, which a password alone opens, and `Vault.Recover` rebuilds the
index from it ([vault.md](vault.md#recovery)). But that recovers the *index*.
After a disk dies, the slow part is reconnecting every account by hand: finding
the S3 key that was in a password manager on the dead machine, signing back in
to Drive and Dropbox, re-pointing a synced folder whose path differs on the new
machine. Only then does the password do anything.

`manifest.sand` deliberately does not carry credentials: a copy sits on every
account, so a credential inside it would let one compromised account unlock the
others. That refusal is right, and it is what leaves the gap:

| What | In `manifest.sand`? | In a kit? |
|---|---|---|
| File tree, placement, data key and retired generations | yes | yes |
| Thumbnails, film details, folder art, sealed sub-vault records | yes | yes |
| Which accounts existed (ID, kind, name) | yes | yes |
| **Account credentials**, colour, capacity, full config | no | yes |
| Default accounts and scheme, backup preferences | no | yes |
| Film database key | no | yes |
| Read history (`vault.sand.reads`) | no | yes |
| The vault key that opens the copies on the accounts | no | yes |

**The recovery kit is the manifest backup that never touches a cloud, and can
therefore carry the credentials.** Everything else follows from that.

Two properties make it work:

- **Account IDs are preserved.** `AddProvider` mints a new UUID per connection,
  which is why `Recover` has to list every account and re-point every shard
  record. A kit restores each account under the ID it already had
  (`Vault.RestoreProvider`), so the shard records are correct on arrival,
  including those inside sealed sub vaults, which no other route can fix without
  their passwords.
- **The kit is a starting point.** A kit made in March describes March, but the
  `manifest.sand` copies on the accounts are rewritten on every change. So the
  import uses the kit's credentials to reach the accounts, reads those copies and
  takes the newer index. The kit gets you connected; the clouds tell you what you
  have. That is why the kit carries the vault key as well as the data key.

## The zip

```
sand-recovery-kit-2026-08-19.zip
├── README.txt        for a person, no secrets
├── kit.sand          the sealed envelope: everything
├── manifest.sand     byte-identical to what the accounts carry
└── fingerprint.txt   what this kit is, without opening it
```

- **`kit.sand`** is the kit. The rest is for days when the app is not available.
  An import also accepts a bare `kit.sand`.
- **`manifest.sand`** is sealed from the same snapshot under the vault key, so the
  commands that need no vault, network or accounts keep working from the kit
  alone:

  ```bash
  sand manifest ls kit/manifest.sand
  sand restore --manifest kit/manifest.sand --parts a-p1.sand,a-p3.sand
  ```

- **`fingerprint.txt`** carries nothing secret:

  ```
  SAND recovery kit

  Kit id      3f2b9c1e-...
  Created     2026-08-19T14:02:11Z
  Built by    sand v2026.8.311
  Opened by   a 25-character recovery code (not in this file)
  Accounts    4
  Files       12481 (2.1 TB)
  Sub vaults  2
  kit.sand    sha256:8a1f...  (1904332 bytes)
  ```

  The kit ID tells someone holding three kits and two slips of paper which opens
  which. Account *names* are not in it: "4 accounts" identifies the kit, a list of
  services is a map of somebody's life.
- **`README.txt`** says what the file is, what opens it, that the code is
  deliberately not inside, and that losing control of the zip is as bad as losing
  every cloud account at once. It is written for someone who finds a zip in a
  drawer in 2031.

The zip itself is not encrypted: zip encryption is weak and non-portable, and
everything that matters is already sealed. The README says so.

## `kit.sand`

### The envelope

The same shape as a manifest backup, for the same reason: the KDF parameters
travel with the ciphertext because the reader has nothing but the secret.

```go
const KitMagic = "SAND-KIT"
const KitVersion = 1

type KitEnvelope struct {
    Magic     string    `json:"magic"`
    Version   int       `json:"version"`
    KitID     string    `json:"kit_id"`
    CreatedAt time.Time `json:"created_at"`
    Secret    string    `json:"secret"`  // "code" or "password", in the clear
    KDF       kdfParams `json:"kdf"`
    Check     sealed    `json:"check"`   // seals "SAND-KIT-OK"
    Payload   sealed    `json:"payload"` // the Kit
}
```

`Secret` is in the clear so the import can label its field ("Recovery code" and
"Vault password" have different validation and help text) without guessing.
`Check` separates a wrong secret (the check fails) from a damaged file (the check
opens, the payload does not); those need different words in front of a
frightened user.

**The KDF is heavier than the vault's:** Argon2id t=8, 512 MiB, 4 threads
(`crypto.KitArgon2Params`), against the vault's 64 MiB. A vault password is typed
several times a day; a kit is opened once in several years, so a second and a
half is nothing at the only moment it is paid. Against a generated code it is
belt and braces (120 random bits need no stretching); it is there for kits sealed
under a chosen password, and so there is one code path that cannot be configured
wrong. The parameters are in the envelope, so raising them later strands no kit.

### The plaintext

```go
type Kit struct {
    Version, KitID, CreatedAt, AppVersion
    Snapshot  Snapshot          // exactly a manifest backup's payload, embedded
    Accounts  []provider.Config // full configs, credentials included, original IDs
    VaultKey  string            // base64 of the lost vault's vault key
    KDF       kdfParams         // the lost vault's parameters, as a record
    DefaultAccounts, DefaultScheme, MovieAPIKey,
    ManifestBackupDisabled, ManifestBackupDelaySeconds
    ReadHistory *readHistoryFile // the .reads sidecar, decoded
}
```

- **`Snapshot` is embedded, not restated.** Everything that reads a manifest
  backup reads a kit's, and every field added to `Snapshot` later is in the kit
  for free.
- **Credentials are carried verbatim**: OAuth refresh tokens, S3 keys, WebDAV
  passwords, sync-folder paths, plus name, colour, capacity, `AddedAt` and `ID`.
- **`ReadHistory` is carried decoded**: on disk it is sealed under the data key,
  which the snapshot already carries.
- **`KDF` is not reused.** The recovered vault gets a fresh salt, so choosing the
  lost vault's password again does not reproduce its vault key
  ([below](#a-kit-older-than-a-password-change)).

### Why it carries the vault key

The kit is opened by a code unrelated to the vault password, but the newer
`manifest.sand` copies on the accounts are sealed under the vault key. The
options were: force the kit secret to equal the vault password, ask for the
vault password on top of the code, or carry the derived key. The first defeats
the point of a code; the second asks someone mid-disaster for the secret most
likely to be gone. The third gives nothing new away: the kit already carries the
data key, which opens the files; the vault key only opens an index the kit
already has a copy of. The consequence is stated in the README and the export
dialog: **this file is the vault**, minus the bytes.

## The secret that opens it

**By default it is a code SAND Vault generates, not a password the user picks.**
A kit secret is typed once in years, which is the profile of a secret people
forget, and the event that destroys the machine often destroys the password
manager too. A kit sealed under "the vault password" is sealed under the password
*as it was that day*, which leaves somebody guessing which of their last four was
current in March. A generated code cannot be forgotten alongside the password,
is pinned to the kit rather than to a point in a password history, and has real
entropy.

### The code

```
K7QM4-9WXTB-2FGHN-5PRVD-8JC3S
```

- **Crockford base32** (`0123456789ABCDEFGHJKMNPQRSTVWXYZ`). No `I`, `L`, `O`
  (confusable when handwritten) or `U` (avoids accidental words). Decoding folds
  case and maps `I`/`L` to `1` and `O` to `0`, so a "corrected" transcription
  still opens.
- **24 symbols of entropy (120 bits)** from `crypto/rand`, grouped in fives; the
  hyphens are not part of the value.
- **One check symbol**, the 25th: five bits of SHA-256 of the 120 bits. It tells
  *you mistyped this* from *this is the wrong kit*. It is not integrity (GCM is).
  It says *that* there is a typo, never *where*; a character outside the alphabet
  is the one case that is localised and named.

`NormalizeKitCode` strips non-alphanumerics, upper-cases, folds, and verifies the
check symbol **before** the KDF runs, so a typo is answered in microseconds with
"that code has a typo in it" instead of 1.5 s of Argon2 followed by "wrong code",
which is the message that makes someone conclude their backup is dead. The
browser runs the same check in the field (`ImportKit.jsx`), so the button never
enables on a code that cannot be right. It is the one piece deliberately
implemented twice: a typo must never cost a round trip.

### Where the code lives

**Never in the zip, its filename, `fingerprint.txt` or any log.** A filename
travels with the file through every copy and attachment. The export shows it
once, in a panel that must be acknowledged, with a copy button and **Save as a
text file**, and one line on what the arrangement means: the kit and the code in
one place are the vault in one place.

**But it is not shown only once.** The code is kept in the vault's sealed
`settings` section (`KitCodes`, by kit ID), never in the manifest, so it never
reaches an account. **Vault settings › Recovery kit › Show code** and
`sand vault kit code [kit-id]` read it back. That gives nothing away: reading it
needs an unlocked vault, and anyone with one can export a fresh kit in seconds.
It covers the common case of a person with a working machine and a mislaid slip
of paper. `sand vault kit code --forget` (and `DELETE /api/vault/kit/code/{id}`)
drops it, as tidying up, never as revocation ([security](#security)).

The case SAND Vault cannot help is machine gone and paper gone. `verify` asks for
the code partly so the drill also proves the code can still be found.

### The opt-out

**Use my vault password instead** (`--use-vault-password`) is offered without
discouragement: for someone whose long generated password lives in a backed-up
password manager it is a real answer. It sets `Secret: "password"`, retains
nothing in settings, and checks the password before sealing so a kit is never
sealed under a typo. It changes nothing else. The dialog says the kit will open
with today's password, not whatever it is changed to later.

## Export

```bash
sand vault kit export [--out FILE] [--use-vault-password]
```

`POST /api/vault/kit` returns `application/zip`; the code comes back in the
`X-Sand-Kit-Code` response header (with `X-Sand-Kit-Id`, `-Secret`, `-Sha256`),
since the body is the archive. That is the same channel, not a second one:
anything that can read the headers can read the body, which is every credential.
It is a POST so the opt-out password and the code never sit in a URL; the
response is `no-store`.

`Vault.ExportKit(opts, w)`:

1. Mint the code (unless the opt-out), or verify the vault password.
2. Build the `Kit` from the unlocked vault: `snapshotLocked()`, the account
   configs verbatim, the vault key, the preference fields, the read history.
3. Seal under `KitArgon2Params` with a fresh salt.
4. Seal a `manifest.sand` from the same snapshot under the vault key.
5. Fingerprint the finished `kit.sand`.
6. Write the zip (deflate; the manifest is JSON and compresses well).
7. Record the export in the store (`LastKitExportAt`, `LastKitID`,
   `LastKitSecret`, `LastKitFileCount`, `LastKitAccounts`) and the code in
   `KitCodes`.

Nothing is written to a temporary file. The CLI writes straight to the
destination (created exclusively, mode 0600, removed on failure); the server
builds the zip in memory so a failure is a readable error rather than a truncated
download with a 200. Derived keys are zeroed. The CLI prints the code on its own
at the end, so a script that redirects the rest still puts it in front of a
human.

**One refusal.** Writing the kit inside a folder that one of the vault's own
local or synced-folder accounts points at is refused (`CheckKitDestination`,
`ErrKitInSyncedFolder`): the sync client would upload the kit to the very cloud
whose credentials it carries. Only backends whose path is on this machine are
checked. The browser cannot see where a download lands, so the dialog says it
instead.

**Staleness.** `GET /api/vault/kit` (`KitStatus`) reports `exported_at`,
`kit_id`, `secret`, `code_retained`, `age_days`, `files`, `files_added`,
`accounts`, `accounts_changed` and `password_changed_since`. The last two lead
the panel: a kit that is merely old still recovers everything through the newer
index on the clouds, but one that predates an account or a password change
recovers less. `accounts_changed` compares account IDs, not counts, because
swapping one account for another leaves the count alone while the kit loses an
account. A vault that never exported has `exported: false`, which drives the
nudge.

## Import

```bash
sand vault kit import <kit.zip> [--replace] [--skip-cloud-index] [--second-instance]
```

`POST /api/vault/kit/import` is multipart (`file`, `secret`, `password`, optional
`old_password`, `replace`, `skip_cloud_index`, `sibling`) and is outside the
session, like `/api/vault/init`: on a fresh install there is no session, and what
stands in front of it is the file plus its secret. A successful import starts a
session.

`POST /api/vault/kit/inspect` (and `sand vault kit inspect`) reads the envelope
with no secret, so the import screen knows whether to ask for a code or a
password before anything is typed.

`Vault.ImportKit` runs seven phases. The first three (a vault, accounts, tree)
are the whole of the user's ask and stand alone; the rest are about being
*current*, and each reports rather than fails. Backup pushes are held for the
whole import and a key hold keeps the server's idle lock from closing the vault
between phases.

| Phase | Does |
|---|---|
| 0 | Refuse to import over a vault holding files unless `replace` (the same rule as `Recover`: adopting a key strands what depends on the current one) |
| 1 | Mint the vault under the chosen password with a fresh salt; adopt the data key and every retired generation, policy, defaults, film key, backup preferences and sub-vault records in one write. Set `BackupNeedsForce` so phase 7 may replace the lost vault's copies |
| 2 | Reconnect every account with `RestoreProvider` (`AddProvider` keeping `ID` and `AddedAt`), pinged in parallel |
| 3–4 | Unless `--skip-cloud-index`, open each reachable account's `manifest.sand` with the carried vault key and adopt the newest if it is newer than the kit, recording which account it came from; then install the index |
| 5 | List every reachable account (`locateShards`): re-point records whose object is elsewhere, count unreachable records without dropping them, count orphans without deleting them |
| 6 | Restore the read-history sidecar under the adopted data key |
| 7 | Push the index to every account, forced, so the copies are under the new password and the foreign-backup guard stops firing against this vault |

The password for the recovered vault need not be the lost vault's: nothing in the
import is derived from it.

### Accounts that do not come back

**A failure is never fatal to the import.** A year-old OAuth token is the
expected case. The account is restored with its ID and credentials, its state is
recorded, the tree still comes back, and its files are counted short.

| Status | Meaning | `repair` |
|---|---|---|
| `connected` | Answered | (none) |
| `needs_reauth` | Credentials rejected: revoked consent, expired token, rotated key | `sign_in` for OAuth backends, `settings` otherwise |
| `needs_path` | The configured folder is not on this machine | `path` (with `path_option` naming the setting) |
| `unreachable` | Network, DNS, a gone bucket | `retry` |
| `elsewhere` | A local account on the machine the kit came from (sibling import) | (none) |

The status says what is wrong; `repair` says which door to open, since a revoked
Dropbox consent and a rotated S3 key both fail auth and share no repair steps.
`repairFor` asks the backend (does its spec have OAuth?).

The repair is the accounts panel's own edit dialog, opened over the report. It
reuses the OAuth app the kit carried, so signing back in is one button, and it
pings before writing, so rejected credentials are refused. A **folder** is the
weak case: local and synced-folder backends create the directory they are pointed
at, so a wrong folder succeeds and comes back empty. So when the dialog closes,
the row re-tests the account and runs `Reconcile`, and the report is redrawn from
that tally; a repair that found nothing leaves the figures unchanged rather than
claiming an improvement. Each repair keeps the account ID, and ends by claiming
the account with a forced push (`POST /api/vault/backup` with `force`), since an
account that comes back after phase 7 still holds the dead vault's index.

### The report

`KitImportReport` leads on the shortfall, like `RecoveryReport`: `files` against
`recoverable`, `bytes` against `recoverable_bytes`, `lost`, `missing` files and
`blocking` accounts, then `repointed`, `orphans`, `sub_vaults`, where the index
came from (`index_source`, `index_source_name`, `index_at`), `password_changed`
and warnings.

**Sub vaults** come back verbatim and shut, each opened by its own password.
Because account IDs are preserved, the shard records inside a sealed sub vault are
already correct: no `account_remap` is written, and a sub vault never opened again
is still intact.

### An old kit

> Kit exported in March. Machine dies in August. 312 files added since, and a
> Backblaze account connected in June.

Four accounts come back from the kit. Phase 4 adopts the `manifest.sand` from
Drive dated two days before the crash, so all 312 files are in the tree. The
report names one blocking account, `Backblaze (photos)`, because the newer index
names it although the kit never did. The user connects that one account by hand
and `POST /api/vault/recovery/resume` (`sand vault recover --resume`) re-points
the rest. **A kit is never worse than no kit, and the only accounts it cannot
save you are the ones it never knew about.**

### A kit older than a password change

The kit still opens: the code is not the vault password. Only phase 4 is
affected: the carried vault key no longer opens the copies on the accounts. That
shows up uniformly on every account, which distinguishes it from damage, and the
report sets `password_changed` and installs the kit's own index. (Another vault
having claimed these accounts looks identical, so the report describes the
observation rather than asserting the cause.)

**Only the old password closes the gap.** Typing it as the *new* password changes
nothing, because the recovered vault has a fresh salt. It has to be given
separately (`old_password`, or `SAND_OLD_PASSWORD` on the CLI), and it is tried
against every reachable account when the carried key is refused
(`cloudIndexWithPassword`). A password that opens nothing is a warning. Since
phase 4 runs after the vault is written, it is asked up front and quietly: the
browser folds the field behind "did your vault password change after this kit
was made?". Skipping is a real option and the default; it costs only the files
added between the export and the password change, and the report counts them.
Nothing should suggest the old password is useless: it is the only key to the
newest index in existence.

### Joining a running vault

`--second-instance` (`sibling`) imports the kit as a second machine on a vault
that keeps running where it was exported, rather than as a replacement. The
password must be the vault's own (`ErrNotTheVaultsPassword` otherwise), the kit's
salt is kept so both machines derive the same vault key, the copies on the
accounts are folded in rather than claimed, and the other machine's local
accounts are marked `elsewhere`. See
[vault.md](vault.md#joining-a-second-machine).

### Rejected: shipping the vault file verbatim

A zip of `vault.sand` and its sidecar would be import-by-copy with no new crypto.
Rejected because it welds the kit's secret to the vault password (no room for a
code, and a password change leaves the kit answering to an old one); it cannot be
inspected without full decryption; and it would make the store format, which
moves for unrelated reasons, a constraint on how old a kit can be read. An
explicit `KitVersion` with an embedded `Snapshot` is one number meaning one thing.

## The fire drill

```bash
sand vault kit verify <kit.zip>
```

`POST /api/vault/kit/verify`, **Test this kit** in settings. **An untested backup
is a rumour.** `Vault.VerifyKit` opens a kit against the vault you still have and,
writing nothing anywhere:

- builds a live provider from each carried config and pings it (`working`,
  `unusable`);
- lists each account and checks the kit's index against what is there
  (`recoverable` of `kit_files`);
- reports drift: files added since (`added_since`) and accounts connected after
  the kit (`accounts_added`), which it holds no credentials for.

**It asks for the code on purpose.** It could open the kit from the live vault's
key material and skip the prompt, but the failure nothing else catches is the slip
of paper lost in a house move. A drill that passes proves the credentials work
*and* the secret still exists; the day it fails on the second, the vault is still
alive and a fresh kit is seconds away. The CLI notes that its figures are a floor:
in a real recovery the newer index on the clouds closes most of the gap.

## Security

**One file, everything.** The kit holds every credential, the data key, the vault
key and the whole index. That is not a weakening of the model (the vault file
plus its password is the same thing), but it is a concentration of it in an
artefact meant to be copied elsewhere, and the design says so loudly.

**The kit alone gives nothing.** 120 uniform bits behind Argon2id at 512 MiB. The
code is not chosen to make a stolen kit harder to crack (that was never the likely
loss); it is chosen because it cannot be forgotten alongside the password.

**The failure directions are not symmetric, and the design leans one way.** Losing
the code loses everything; someone finding the code without the kit gains nothing.
So everything leans toward retrievability: the code is retained in the live vault,
`verify` drags it into daylight, the export panel's dismiss waits on *I have
written this down*. The consequence, stated here: an unlocked vault is a path to
the code.

**Refusals.** The manifest backup refuses redundant placement on fewer than three
accounts, because one account would hold enough shards *and* the key. A kit goes
where the user puts it, so that rule is not consulted; the synced-folder refusal is
the same hazard by another road.

**Handling.** Keys are zeroed after use; the code and passwords never appear in a
URL, a GET parameter or a log; on the CLI they come from a prompt, piped stdin, or
`SAND_KIT_CODE` / `SAND_PASSWORD` / `SAND_OLD_PASSWORD`, never an argument, so they
stay out of shell history and `ps`.

**A kit you have lost control of** has no automatic remedy. Change the vault
password (which rotates the data key), let the re-encryption finish
(`sand vault migrate`), and rotate every account's credentials at the provider;
the old kit then opens a key that opens nothing still stored. The credentials are
the part SAND Vault cannot rotate for you. Forgetting the retained code is not a
substitute: whoever took the kit may have the code from a screenshot or the `.txt`
beside it.

## Surfaces

### HTTP

| Route | Session | Purpose |
|---|---|---|
| `GET /api/vault/kit` | yes | Staleness |
| `POST /api/vault/kit` | yes | Build and return the zip; code in `X-Sand-Kit-Code` |
| `GET /api/vault/kit/code/{id}` | yes | Show a retained code |
| `DELETE /api/vault/kit/code/{id}` | yes | Forget a retained code |
| `POST /api/vault/kit/verify` | yes | The fire drill |
| `POST /api/vault/kit/inspect` | no | Read the envelope; changes nothing |
| `POST /api/vault/kit/import` | no | The import; starts a session |

Repairs from the report use the existing OAuth routes under
`/api/providers/oauth/`, `PATCH /api/providers/{id}`, and
`POST /api/vault/backup` with `force`.

### CLI

```bash
sand vault kit export  [--out FILE] [--use-vault-password]
sand vault kit inspect <kit.zip>
sand vault kit verify  <kit.zip>
sand vault kit import  <kit.zip> [--replace] [--skip-cloud-index] [--second-instance]
sand vault kit code    [kit-id] [--forget]
```

`inspect` and `import` need no existing vault (`import` creates one at `--vault`).
The code is read from `SAND_KIT_CODE`, piped stdin or a prompt, and is normalised
and check-summed before the KDF runs. See the [CLI reference](../reference/cli.md).

### Browser

- **Export** is the *Recovery kit* entry in **Vault settings** (`RecoveryKit.jsx`):
  the staleness line, the button, the opt-out checkbox, the line about synced
  folders, then **Test this kit** and, for a retained code, **Show code**. The code
  panel shows the code large and grouped with copy and **Save as a text file**,
  the kit ID, and a checkbox the dismiss button waits on. It is a deliberately soft
  gate (Show code is right underneath); it exists for the person who would never
  otherwise register that a second artefact exists.
- **Import** is the lock screen's third door, *I have a recovery kit*
  (`ImportKit.jsx`), shown when no vault exists: pick the file, `inspect` labels
  the field **Recovery code** or **Vault password**, the code field groups, folds
  and check-sums as typed, then a progress list per phase and the report, whose
  account rows become repair buttons. A checkbox imports as a second machine.
  `RecoverVault.jsx` remains the route for someone with no kit.

## Failure modes

| What went wrong | What happens |
|---|---|
| Code with a typo | Caught by the check symbol before the KDF: "that code has a typo in it" |
| A character outside the alphabet | Named, with the character quoted |
| Code for a different kit | Check symbol passes, check block fails: the code does not open this kit |
| Code lost, vault alive | **Show code** / `sand vault kit code` |
| Code lost, vault gone | Nothing opens the kit; the one unrecoverable state, and why `verify` asks for the code |
| Wrong vault password (opt-out kits) | `ErrWrongPassword` from the check block, before anything is touched |
| Truncated or edited `kit.sand` | `ErrKitDamaged`: the check passed, the payload did not |
| Kit from a newer build | Refused on `KitVersion`, naming the version |
| Vault already holds files | Refused unless `replace` |
| An account's credentials expired | `needs_reauth`; import completes; one button fixes it |
| A folder path is not on this machine | `needs_path`; import completes; the edit dialog re-points it |
| An account gone for good, or unknown to the kit | Its files counted short, the account in `blocking`; connect it, then resume |
| Kit older than the accounts | Newer index adopted off a cloud |
| Vault password changed after export | `password_changed`; supply the old password, or accept the kit's index |
| Objects on an account the index forgot | Counted as orphans, never deleted |
