# Pacing a Password Change

*A design for re-encrypting a vault slowly enough not to overwhelm the clouds,
showing the mixed state honestly while it lasts, and refusing a second change
until the first is done.*

---

## 1. What already exists, and what was asked for

§3.3 of the architecture document already settles the hard part. Changing the
password mints a **new data key**, rewrites the vault file under the new
password in one atomic write, and keeps the previous data key beside the new
one, **wrapped under the new password**, for as long as any file is still
sealed under it. Each file then moves across on its own: gathered, re-encrypted,
scattered again, the old parts erased. `sand vault migrate` finishes an
interrupted run.

Three consequences of that design matter here:

- **The old password opens nothing from the moment the change commits.** It
  does not open the vault file, and it does not unwrap the previous data key,
  because that key is now wrapped under the new password.
- **Every file stays readable with the new password throughout**, whichever
  key it is on.
- **What lingers is a set of files on the previous key.** Anyone who holds
  the previous password *and* an old copy of the vault file or of a
  `manifest.sand` backup can still read those files' parts until each one
  moves.

What was asked for is three things on top of that:

1. Re-encryption should be able to run **slowly** — a budget in gigabytes a
   day, or a choice of which folders to do — so a terabyte vault does not
   saturate every account for a day.
2. The vault is then in a **mixed state for a while**, and the UI has to make
   that legible. Could sub vaults carry it?
3. **A second password change should be refused** until the first has fully
   taken effect.

The first and third are built as described below. The second has an answer
that is shorter than the question, and it comes first because it shapes the
other two.

---

## 2. Two keys, one password

The brief frames the mixed state as "two passwords". It is not. There is one
password, and there are two **key generations**: the current one, which every
new upload and every moved file uses, and the previous one, which the files
that have not moved yet are still on. Both are opened by the one current
password. The UI therefore never asks for, mentions, or models a second
password. It models one state — *on the previous key* — and gives it a count,
a size, a pace and an end date.

### 2.1 Why the previous key is not a sub vault

A sub vault (§3.8) is an index sealed under its **own** Argon2id key, which
the main password cannot derive. The files still on the previous key could,
mechanically, be moved into a sub vault sealed under the old password. It
would be worse on every axis:

| | Sub vault under the old password | Files on the previous key (built) |
|---|---|---|
| Does the old password die? | No. It stays valid for as long as the migration takes, which is the opposite of changing it. | Yes, when the change commits. |
| Reading an un-moved file | Type the old password to open the sub vault first. | Click it. One password opens everything. |
| Where the files live | They move out of their folders into a vault and back. Paths change twice. WebDAV mounts and automations lose them meanwhile. | They stay where they are. Only `key_id` on the entry differs. |
| Backups and recovery | The backup carries sub vaults sealed; recovery needs both passwords. | The backup already carries every key generation the manifest names (§3.6). Nothing changes. |
| Thumbnails, film details, folder art | Keyed by path and scope; broken on the way out and again on the way back. | Untouched. |

What *is* borrowed from sub vaults is presentation. A **Previous key** entry
is drawn at the top of the vault in the sub vaults' chrome — dashed, listed
above the folders and not among them, not a thing a bulk delete can sweep up —
because to the eye it is the same kind of thing: a part of the vault that is
not a folder. It asks for no password and opens the progress panel. The
sub-vault panel's per-vault "still on an old key" counts, which already
existed, are the same figure.

A sub vault's **own** password change gets the same treatment as the main
vault's: the same refusal of a second change while one is pending. Its
re-encryption stays a foreground pass (`sand sub migrate`, the row's finish
button) rather than a paced plan, because a sub vault can only be worked while
it is open, which is when its owner is present.

---

## 3. The pace

### 3.1 The contract

A plan says **so many bytes a day**. The meter behind it is a token bucket
counted in bytes (`internal/vault/pacer.go`):

- The level rises continuously at the budget rate and never above the
  **burst**, which is an hour's worth of budget.
- A file may **start** whenever the level is not negative.
- The **whole file is charged** when it starts. A file bigger than the burst
  drives the level into debt, and nothing else starts until the debt has
  refilled. A 40 GB film on 10 GB a day is four quiet days.

So the average over any period longer than the biggest file is exactly the
budget, and the burst is as big as one file and no bigger. An hour's burst
rather than a day's is deliberate: a day's would let a vault that had been
shut for a week move a week of budget the moment it opened, which is the
saturated day the plan exists to prevent.

The meter is pure — no clock of its own, no I/O, no lock — so it is tested
without waiting, and its state (`Level`, `LevelAt`) is persisted beside the
plan. The charge for a file is made under the same lock that chooses it, before
the file moves, so it rides in the index write the move itself makes; a restart
cannot forget it. A file that would not move is refunded.

### 3.2 The plan

```json
"rekey": {
  "bytes_per_day": 10737418240,
  "folders": ["/Taxes", "/Photos/2024"],
  "only_folders": false,
  "paused": false,
  "keep_unlocked": false,
  "started_at": "2026-10-03T21:14:00Z",
  "files": 3310, "bytes": 1319413953331,
  "level": -2147483648, "level_at": "2026-10-19T08:00:00Z"
}
```

It lives in the main vault's manifest, because the folders it names are index,
and it is dropped in the write that moves the last file off the previous key —
the same write that drops the key (`pruneRetiredKeysLocked`). `files` and
`bytes` are what was outstanding when the plan began, so progress is a fraction
of something.

**Folders first.** The named folders are worked in order, smallest file first
within each, then — unless `only_folders` — everything else, smallest first.
Smallest first because the count of files on the previous key is what the
person is watching, and the budget is in bytes either way. With `only_folders`
set and the folders done, the plan is **idle**: files are pending, nothing is
allowed to move, and the status says so.

**Paused** holds the plan without forgetting it. A plan with a budget of zero
is the same hold under another name: nothing moves until a pace is set, and
`sand vault migrate` finishes the lot at any time.

**Keep unlocked.** The server moves files only while the vault is open, and
the idle timer closes it. By default the plan holds the timer off *during* a
move, so a file is never cut off, and otherwise leaves it alone — a plan must
not quietly turn auto-lock off for a month. `keep_unlocked` is the explicit,
warned opt-in that does.

### 3.3 One step at a time

`Vault.RekeyStep(ctx, now)` moves at most one file: pick the next the plan
allows, ask the meter, charge, move, or refund and remember why. The server
calls it on a twenty-second tick (`rekeyLoop`); `sand vault rekey --run` calls
it in a foreground loop for a machine with no server. A file that cannot be
read — an account offline, too few parts — is left alone for an hour, its
reason kept for the status, and the files behind it are not held up.

The paced step and the finish-now pass share one slot (`migrateMu`), so they
never gather and scatter the same file at once. The loser does not wait: the
pass returns `ErrMigrationBusy`, the step asks to be tried again shortly.

---

## 4. One change at a time

`ChangePassword` and `ChangeSubVaultPassword` refuse with `ErrRekeyPending`
while any file of that vault is on a key **that vault itself retired**. The
check is made from the index **as the vault file holds it**, after the old
password has unsealed it, so it holds on the lock screen as well as in an open
session; the open-vault check beforehand only spares the thumbnails from being
erased for a change that is about to be refused.

A file sealed under *another* vault's key — what an assignment between the
main vault and a sub vault leaves behind when its re-encryption is deferred —
is pending too, is marked and counted and moved by the plan like any other,
but is not a password change half done, and does not block the next one. The
key it needs is the other vault's, which the change neither holds nor touches.

Two reasons, and the second is the real one:

- A vault that can hold three generations at once is a vault whose owner has
  to reason about three. Two — the current key and the previous one — is what
  the file list can mark, what the status can report, and what a person can
  hold in their head.
- "The password is changed" is a promise with a tail, and the refusal is what
  makes the tail visible. A second change that went through would start a new
  tail before the first was done, and nothing on screen could say which files
  were readable by which old password.

The way out is the way through: finish now, or wait. There is no `--force`.
A file that can never move — its account gone for good — blocks a second
change until it is deleted or recovered from a backup, and the status names
it. That is the right pressure in the right place.

HTTP answers `409 REKEY_PENDING` with the counts in the message. The CLI
prints the counts and the two commands that clear them.

---

## 4.1 A move never holds the file

The first version of the step moved a file the way `MigrateFiles` always had:
`Fetch` the whole thing into one slice, scatter it from there. On a film the
size of the machine's memory that is the kernel killing the service mid-move,
under the unit's `MemoryMax`, and a plan that never gets past that file.

`rescatter` streams instead. A chunked file is read back through a sequential
`ChunkedReader`, which holds the chunk it is in and nothing else, feeding
`scatterStream` the way a spooled upload does; what is resident at any moment
is that chunk plus the upload window, the same hundred-odd megabytes an upload
costs, whatever the file weighs. A file still in the pre-chunking format has
no seams to read through, so it is rebuilt onto disk first, as a conversion is
(`rebuildLegacyToDisk`), and streamed from there.

Integrity holds the same way it did. Every chunk the reader hands over was
authenticated by its own seal; the bytes that go out are hashed as they pass
and checked against the hash on record before the index moves, and a mismatch
erases the new parts and leaves the file exactly as it was. The index switches
to the new parts in one write, after every one has landed, and the old parts
are erased only after that write — so a move cut off at any point, by a crash,
an upgrade or a kill, leaves the file readable on the key it was on, and the
plan takes up the next file when the vault is unlocked again. New parts a
cut-off move had already written are orphans the stray-parts scan finds.

## 4.2 Leaving a file for last

A file can be deferred: the one being moved right now, or any pending one.
`DeferRekeyFile` writes it into the plan's `deferred` list — in the plan, so
the choice survives a restart — and, if it is the file being moved, pulls that
move's cancel. The move stops where it is; the new parts it had written are
erased; the file stays on the previous key, readable; nothing is charged to
the meter. Deferred files are moved after everything else, by the paced step
and by a finish-now pass alike, and `UndeferRekeyFile` puts one back in its
turn. Every move goes through `moveTracked`, which is what the status reads
for the file in flight, how far through it is, and the cancel the skip pulls.

## 5. Surfaces

### HTTP

| Call | Does |
|---|---|
| `POST /api/vault/password` with `plan` | Changes the password now and leaves the files to the plan. `plan` is a `RekeySettings`; giving one means not migrating now, whatever `migrate` says. |
| `GET /api/vault/rekey` | `RekeyStatus`: pending files and bytes, totals the plan began over, the plan, when the password changed, next-file and ETA seconds, idle, the per-folder breakdown (queued folders first in their order, then every top-level folder with files left), what is moving now, and the recent failures. |
| `PUT /api/vault/rekey` | Sets the plan from a whole `RekeySettings`. Starts one if none exists; refused when nothing is on the previous key. |
| `POST /api/vault/migrate` | Unchanged: finish now. `409 MIGRATION_BUSY` while a step holds the slot. |
| `POST /api/vault/rekey/skip` | `{id, undo}`: leave a file for last (the one being moved when `id` is empty, stopping that move), or put it back in its turn. Answers with the status. |
| `GET /api/vault` | `stats.key_id` names the current generation; `sub_vaults[].key_id` likewise while open. |
| `GET /api/files` | `previous_key` maps each subfolder to how many files under it are still on the previous key. Each file already carries `key_id`. |

### CLI

```
sand vault passwd --per-day 10G --folder /Taxes --folder /Photos/2024 [--only-folders] [--keep-unlocked]
sand vault rekey                      # where it stands
sand vault rekey --per-day 25G --all  # faster, forget the folder order
sand vault rekey --pause | --resume
sand vault rekey --skip /Films/Heat.1995.mkv   # leave it for last; '.' is the file moving now
sand vault rekey --unskip /Films/Heat.1995.mkv
sand vault rekey --run                # the plan, in the foreground, until done
sand vault status                     # names the pace and the time left
sand vault migrate                    # finish now, as before
```

### Browser

See the renderings. In short: the password dialog asks *all at once* or *so
much a day* with a folder queue; a dialog opened while a change is pending
shows that change's progress and the two ways out instead of a form; a
progress panel carries pace, folders, pause, finish-now and the recent
failures; the accounts drawer carries a card; the top of the vault carries the
*Previous key* entry; a file row on the previous key wears a quiet dashed
`prev key` pill beside its parts, and a folder row the count under it.

---

## 6. Failure modes

| What happens | What the design does |
|---|---|
| Server restarts mid-plan | The meter's level and clock are in the plan; the plan resumes at the pace it left, not with a fresh burst. |
| Vault is locked | Nothing moves. The step returns `ErrLocked` and the loop waits for the next unlock. |
| A cloud is offline | Its files fail, are refunded, listed with the reason, and retried after an hour. Others move. |
| A file is deleted while chosen | `migrateFile` finds no entry and writes nothing; the charge is refunded. |
| Finish-now runs while the loop ticks | One slot. The loop's step comes back asking to be tried again shortly. |
| The budget is raised | The loop trusts a wait for at most five minutes, so the new pace is felt within that. |
| `only_folders` and the folders are done | The plan is idle, says so, and the rest waits for `--all` or a wider plan. |
| Nothing pending, plan requested | Refused: a plan needs something to be a plan about. |
| Clock goes backwards | The meter earns nothing for the lost interval and does not pay it twice. |

---

## 7. Tests

`internal/vault/pacer_test.go` covers the meter alone: the starting burst, a
file bigger than the burst paying itself off, the thirty-day average landing
within one file of the budget, a week idle banking no more than the burst, a
backwards clock, refunds, ETA, unmetered.

`internal/vault/rekeyplan_test.go` covers the vault: a second change refused
open and locked and allowed once finished; the same for a sub vault; a planned
change moving files in folder order then smallest first and dropping the plan
and the key with the last file; a step waiting on the meter and the debt
surviving a reopen; a plan going idle at its folders and widening; pause and a
zero budget; folder validation and the nothing-pending refusal; an unreadable
file left alone for an hour and then moved; one pass at a time; `Stats.KeyID`.

`internal/server/rekey_api_test.go` covers the HTTP surface end to end,
including the `409`, the plan adjusted in flight, one server-side step moving
the queued folder's file first, and finish-now clearing the plan.
