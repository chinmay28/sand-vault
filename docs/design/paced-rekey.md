# Pacing a password change

A password change re-encrypts every file, which on a large vault can saturate
every account for a day. This page covers how re-encryption is paced over days,
how the mixed state is shown while it lasts, how a move streams and survives a
cloud being down, and why a second password change is refused until the first
is done. The code is `internal/vault/pacer.go`, `rekeyplan.go`, `rekey.go` and
`rekeyfiles.go`, with the server loop in `internal/server/handlers_rekey.go`.

## The starting point

[vault.md](vault.md#changing-the-password) covers the mechanism. Changing the
password mints a **new data key**, rewrites the vault file under the new
password in one atomic write, and keeps the previous data key beside it,
**wrapped under the new password**, while any file is still on it. Each file then
moves on its own: gathered, re-encrypted, scattered, old parts erased.

So:

- **The old password opens nothing from the moment the change commits**, not the
  vault file and not the previous data key.
- **Every file stays readable with the new password throughout.**
- **What lingers is a set of files on the previous key.** Someone holding the
  previous password *and* an old copy of the vault file or a `manifest.sand` can
  read those files' parts until each one moves.

Pacing adds three things: re-encryption can run slowly (a daily byte budget,
chosen folders first); the mixed state is legible in the UI; and a second change
is refused while one is pending.

## Two keys, one password

The mixed state is not "two passwords". There is one password and two **key
generations**: the current one, used by new uploads and moved files, and the
previous one, which the files not yet moved are on. Both open with the current
password. The UI never asks for or mentions a second password; it models one
state, *on the previous key*, with a count, a size, a pace and an end date.

### Why the previous key is not a sub vault

The un-moved files could mechanically be put in a sub vault sealed under the old
password. It would be worse on every axis:

| | Sub vault under the old password | Files on the previous key (built) |
|---|---|---|
| Does the old password die? | No, it stays valid for the whole migration | Yes, when the change commits |
| Reading an un-moved file | Open the sub vault with the old password first | Click it |
| Where the files live | Out of their folders and back; paths change twice; WebDAV and automations lose them | Where they were; only `key_id` differs |
| Backups and recovery | Recovery needs both passwords | The backup already carries every generation the index names |
| Thumbnails, film details, folder art | Broken on the way out and back | Untouched |

What *is* borrowed from sub vaults is presentation: a **Previous key** entry at
the top of the vault, drawn in the sub-vault strip's style (dashed, above the
folders, not something a bulk delete can sweep up). It asks for no password and
opens the progress panel.

A sub vault's own password change gets the same one-change rule. Its
re-encryption stays a foreground pass (`sand sub passwd`, or the sub vault row's
finish button, `POST /api/subvaults/{id}/migrate`) rather than a paced plan,
because a sub vault can only be worked while it is open, which is when its owner
is present.

## The pace

### The meter

A plan says **so many bytes a day**. The meter is a token bucket counted in bytes
(`pacer.go`):

- The level rises continuously at the budget rate, never above the **burst**, an
  hour's worth of budget.
- A file may **start** whenever the level is not negative.
- The **whole file is charged** when it starts. A file bigger than the burst drives
  the level into debt, and nothing else starts until the debt has refilled. A
  40 GB film on 10 GB a day is four quiet days.

The average over any period longer than the biggest file is exactly the budget.
An hour's burst rather than a day's is deliberate: a day's would let a vault shut
for a week move a week of budget the moment it opened, the saturated day the plan
exists to prevent.

The meter is pure (no clock, I/O or lock), and its state (`level`, `level_at`) is
persisted in the plan. A file's charge is made under the lock that chooses it and
rides in the index write the move makes, so a restart cannot forget it. A file
that would not move is refunded.

### The plan

```json
"rekey": {
  "bytes_per_day": 10737418240,
  "full_speed": false,
  "folders": ["/Taxes", "/Photos/2024"],
  "only_folders": false,
  "paused": false,
  "keep_unlocked": false,
  "started_at": "2026-10-03T21:14:00Z",
  "files": 3310, "bytes": 1319413953331,
  "level": -2147483648, "level_at": "2026-10-19T08:00:00Z",
  "deferred": ["…"]
}
```

It lives in the main vault's manifest, because the folders it names are index,
and is dropped in the write that moves the last file off the previous key, the
same write that drops the key (`pruneRetiredKeysLocked`). `files` and `bytes` are
what was outstanding when the plan began.

- **Folders first.** Named folders are worked in order, smallest file first within
  each, then (unless `only_folders`) everything else, smallest first, because the
  count of files left is what the person watches. With `only_folders` set and the
  folders done, the plan is **idle** and says so.
- **Paused** holds the plan without forgetting it. A budget of zero is the same
  hold.
- **Full speed** is "finish now" as a plan: the meter is bypassed and files move
  back to back by the same loop, with the same order, deferred list and pause. The
  budget is kept as the pace to fall back to; choosing a pace clears `full_speed`
  and the banked level is still there. This is how the browser finishes now. The
  alternative, `POST /api/vault/migrate`, holds the request open until the last
  file moves, which on a phone means a screen that locks, a dropped connection and
  a "failed to fetch". So the browser sets the plan to full speed and polls, and
  the request answers at once. The password dialog's *all at once* is the same
  plan, started by the change.
- **Keep unlocked.** The server moves files only while the vault is open. By
  default the plan holds the idle timer off *during* a move and otherwise leaves
  it alone, so a plan never quietly disables auto-lock for a month.
  `keep_unlocked` is the explicit, warned opt-in that does.

### One step at a time

`Vault.RekeyStep(ctx, now)` moves at most one file: pick the next the plan allows,
ask the meter, charge, move, or refund and remember why. The server calls it from
`rekeyLoop`; `sand vault rekey --run` loops it in the foreground for a machine
with no server.

- A file that cannot be read (too few parts) is left alone for an hour with its
  reason, and the files behind it are not held up.
- A file with a part on an account the health check found down, whose other parts
  are too few to rebuild it, waits for that account, counted and named in the
  status. When the others are enough, it moves without it
  ([below](#a-cloud-that-is-not-answering)).

The loop checks every 20 s whether anything is due, and after a move takes the
next file straight away (`rekeyRound`): the meter spaces files out, and full speed
has no meter. A plan set or changed over HTTP wakes the loop at once. A wait is
trusted for at most five minutes, so a raised budget is felt within that. Each
move counts as use of the vault for the idle timer.

The paced step and the finish-now pass share one slot (`migrateMu`, `TryLock`), so
they never move the same file at once. The pass returns `ErrMigrationBusy`
(HTTP `409 MIGRATION_BUSY`); the step asks to be retried shortly.

## A move never holds the file

A move streams (`rescatter`). A chunked file is read through a sequential
`ChunkedReader` holding only the chunk it is in and fed to `scatterStream` as a
spooled upload is, so what is resident is that chunk plus the upload window
whatever the file weighs. A file still in the pre-chunking format is rebuilt to
disk first (`rebuildLegacyToDisk`) and streamed from there. (The first version
fetched the whole file into memory, which on a film the size of the machine's
memory is the service killed under its `MemoryMax`.)

Integrity holds: every chunk was authenticated by its own seal, the outgoing
bytes are hashed and checked against the recorded hash before the index moves,
and a mismatch erases the new parts and leaves the file as it was. The index
switches to the new parts in one write after all have landed, and old parts are
erased only after that write. A move cut off at any point (crash, upgrade, kill)
leaves the file readable on its old key; new parts it had written are orphans the
stray-parts scan finds.

## Leaving a file for last

Any pending file, including the one moving now, can be deferred.
`DeferRekeyFile` adds it to the plan's `deferred` list (so the choice survives a
restart) and, if it is the file in flight, cancels that move: the move stops, its
new parts are erased, the file stays readable on the previous key, nothing is
charged. Deferred files move after everything else, by the paced step and
finish-now alike; `UndeferRekeyFile` puts one back. Every move goes through
`moveTracked`, which is what the status reads for the file in flight, its
progress, and the cancel a skip pulls.

## A cloud that is not answering

Re-encryption once refused to commit a file that came out a part short, so a file
with a part on a cloud that was down waited for that cloud. Right for a cloud that
dropped one write; wrong for a NAS switched off for a month, which kept the
previous key and password alive for that month.

Now the health check's verdict is used. A file with a part on an account the last
check found unreachable is re-encrypted from the answering accounts' parts, if
those can rebuild it, and written back to them (topped up from any other connected
account the spread allows, or a part short where there is none). The part on the
silent account is neither read nor written nor erased. It is **recorded**
(`manifest.stale_parts`) as a part under the previous key on that account, with
its file and when, in the same write that moves the file. A part an erase could
not remove on an answering account is recorded the same way.

The record makes the part a stray rather than a leak. The rekey status and vault
stats count recorded parts and name their accounts; the stray-parts scan flags each
as *under the previous key* with its file's path; and the browser re-runs that scan
when a cloud holding recorded parts comes back. Sweeping them is what finishes the
password change. See [vault.md](vault.md#parts-left-under-the-previous-key).

A file that cannot be rebuilt without the silent account is still held. A write
that fails on an account the health check found *up* is still a refusal: a dropped
write, not an absent cloud.

`MigrateFiles` (finish-now) runs its own health check first when the last one is
older than five minutes (`MinHealthInterval`), so `sand vault migrate` on a machine
with no server knows which accounts are down. `sand vault rekey --run` checks on
the vault's schedule, as the server does.

## One change at a time

`ChangePassword` and `ChangeSubVaultPassword` refuse with `ErrRekeyPending` while
any file of that vault is on a key **that vault itself retired**. The check reads
the index as the vault file holds it, after the old password has unsealed it, so it
holds on the lock screen as well as in an open session.

A file sealed under *another* vault's key (left by an assignment between the main
vault and a sub vault) is pending too, and is moved by the plan like any other,
but it is not a password change half done and does not block the next one.

Why refuse:

- A vault holding three generations is one whose owner has to reason about three.
  Two (current and previous) is what the file list can mark and a person can hold.
- "The password is changed" is a promise with a tail, and the refusal keeps the
  tail visible. A second change mid-tail would leave nothing on screen able to say
  which files were readable by which old password.

The way out is through: finish now, or wait. There is no `--force`. A file that
can never move (its account gone for good) blocks the next change until it is
deleted or recovered, and the status names it. HTTP answers
`409 REKEY_PENDING` with the counts; the CLI prints them and the commands that
clear them.

## Surfaces

### HTTP

| Call | Does |
|---|---|
| `POST /api/vault/password` with `plan` | Changes the password now and leaves the files to the plan (a `RekeySettings`); a plan means not migrating now |
| `GET /api/vault/rekey` | `RekeyStatus`: pending files and bytes, the plan and its starting totals, when the password changed, next-file and ETA seconds, idle, per-folder breakdown, the file moving now, files held for a silent cloud (`held`, `held_on`), recorded `stale_parts` per account with whether each answers now, recent failures, and `retrying` / `retry_in_seconds` |
| `PUT /api/vault/rekey` | Sets the plan from a whole `RekeySettings`; refused when nothing is on the previous key. With `full_speed`, finishes now in the background and answers at once |
| `POST /api/vault/migrate` | Finish now in the foreground, holding the request open; `409 MIGRATION_BUSY` while a step holds the slot. Used by the CLI, not the browser |
| `POST /api/vault/rekey/skip` | `{id, undo}`: defer a file (empty `id` is the one moving now) or undo; answers with the status |
| `GET /api/vault/rekey/files` | `RekeyPendingPage`: pending files in plan order, each with `state` (`queued`, `moving`, `deferred`, `held` + `held_on`, `retrying` + `reason` and `retry_at`, or `outside` the plan's folders). `?folder`, `?offset`, `?limit` (50 default, 500 max); carries the folder's `total` and `bytes`; 404 for a missing folder |
| `DELETE /api/vault/rekey/files/{id}` | Erases a pending file instead of moving it. If it is moving, the move is stopped first (as a skip, nothing charged). Answers like `DELETE /api/files/{id}`; 400 for a file already on the current key |
| `GET /api/vault` | `stats.key_id` (current generation; `sub_vaults[].key_id` while open), `stats.stale_parts`, `stats.stale_bytes` |
| `GET /api/files` | `previous_key` maps each subfolder to its count of files on the previous key; each file carries `key_id` |

### CLI

```bash
sand vault passwd --per-day 10G --folder /Taxes --folder /Photos/2024 [--only-folders] [--keep-unlocked]
sand vault passwd --full-speed        # change now, finish in the background
sand vault passwd --no-migrate        # change now, move nothing yet
sand vault rekey                      # where it stands
sand vault rekey --per-day 25G --all  # faster, and forget the folder order
sand vault rekey --full-speed         # finish now, in the background under 'sand serve'
sand vault rekey --pause | --resume
sand vault rekey --skip /Films/Heat.1995.mkv    # leave it for last; '.' is the file moving now
sand vault rekey --unskip /Films/Heat.1995.mkv
sand vault rekey --list [--under /Archives]     # pending files in plan order, and what holds each
sand vault rekey --run                # run the plan in the foreground until done
sand vault status                     # includes the pace and time left
sand vault migrate                    # finish now, in the foreground
```

### Browser

- The password dialog offers *all at once* or *so much a day* with a folder queue.
  Opened while a change is pending, it shows that change's progress and the two
  ways out instead of a form.
- The progress panel (`Rekey.jsx`) carries pace, folders, pause, finish-now and
  recent failures, and polls faster while finishing. The accounts drawer has a
  card; the top of the vault has the *Previous key* entry; a file row on the
  previous key wears a dashed `prev key` pill, and a folder row its count.
- **The counts open.** *Files left* and each folder's count list the files behind
  them a page at a time (`RekeyFiles.jsx`), in plan order, each with where it
  stands and skip, un-skip and delete controls. "7 files would not move · tried
  again in 40 min" says which seven, and a file that will never move (too few
  parts, say) can be deleted from its row after confirmation.
- The pace field commits when typing pauses, on Enter or on blur, and is never
  disabled while focused: saving on every keystroke used to disable the field and,
  on a phone, dismiss the keyboard after every digit.

## Failure modes

| What happens | What the design does |
|---|---|
| Server restarts mid-plan | Level and clock are in the plan; it resumes at its pace, not with a fresh burst |
| Vault is locked | Nothing moves; the step returns `ErrLocked` until the next unlock |
| A cloud is offline | Files rebuildable without it move, leaving a recorded stray; others are held, named, and moved when the cloud is back |
| A file is deleted while chosen | The move finds no entry and writes nothing; the charge is refunded |
| Finish-now runs while the loop ticks | One slot; the step retries shortly |
| The browser that asked for full speed goes away | Nothing; the plan is the server's |
| The budget is raised | Felt within five minutes |
| `only_folders` and the folders are done | Idle, says so; the rest waits for `--all` or a wider plan |
| Plan requested with nothing pending | Refused |
| Clock goes backwards | The meter earns nothing for the lost interval and does not pay it twice |
