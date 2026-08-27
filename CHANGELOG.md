# Changelog

Releases are `vYEAR.MONTH.PATCH` — a calendar version, where the patch number
is the repository's commit count, so `v2026.8.42` is the 42nd commit on the
2026.8 line. See [`internal/version/version.go`](./internal/version/version.go).
Releases before `v2026.8` used `vMAJOR.MINOR.PATCH` and are listed under their
original numbers.

Each section below is the body of the corresponding GitHub release. A heading
must name the tag exactly — a tag whose commit builds a different version is a
tag that shouldn't be published.

## Unreleased

### SAND now checks that your clouds are still answering, hourly

Every ping SAND made was one you started. Opening the accounts drawer pinged
them, **Test** pinged one, a folder's sweep pinged them all before checking
anything under it — and every one of those needed somebody sitting in front of
the app. The failure that matters is the one nobody is sitting in front of: a
refresh token revoked in March, an access key rotated by somebody else on the
team, a NAS that has been off since the power cut.

None of that makes anything look broken. Files still read, because a file needs
`k` of its `n` parts and the clouds still answering carry it — right up until a
second one goes and the file does not come back at all.

The server now asks them itself, once an hour, for as long as the vault is
unlocked. The foot of the connected clouds panel says what it found, in the
space beside the vault's own figures that was empty:

```
2671    580.4 GB                  ● 1 of 17 unhealthy
FILES   IN THE VAULT                 checked 12m ago
```

Pressing that line opens every cloud worst-first: what the unreachable ones
actually said, how long each has been failing — *not answering for 3 days* is a
different problem from *not answering* — and how quickly the healthy ones came
back, which is where a cloud on its way out shows up first. **Check now** asks
them all again, for when you have just fixed one.

**Vault settings → Cloud health** is the same panel, and where the schedule
lives: 15 minutes, hourly, 6 hours, daily, or off. Off is a real answer —
somebody metered by the request should be able to say so, and the panel then
stops claiming a freshness nothing is maintaining.

It is a ping and nothing more: one small request per cloud, no listing, no
download, no data moved. Whether the parts of a particular file are still where
the index says they went is the other question, and stays what it was — a
folder's standing instruction, opt-in per folder, because that one reads the
index and asks after parts by name.

Two things follow from where it runs. It only runs while the vault is unlocked,
since the credentials live in the encrypted index — but a slot that passes while
the vault is shut is not lost, because *due* is measured against the last check
rather than by a timer that has to have been running. And it never counts as
use: an hourly ping that renewed the idle timer would mean the vault never
auto-locked again.

Drawing the accounts panel pings every account, so it counts as a check — the
figure stays fresh while you are looking, and the scheduled one does not go out
and repeat what just happened. Something changing is logged once, rather than a
line an hour saying everything is fine:

```
cloud health: Elements (dial tcp 192.168.1.40:445: no route to host) — 1 of 17 not answering
cloud health: Elements answering again
```

On a headless machine — which is usually the one actually running the check —
`sand remote health` does the same from a terminal, with `--every 6h` and
`--off` for the schedule. `GET /api/providers/health` reads what the last check
found without contacting anybody, `POST /api/providers/health/check` runs one
now, and `POST /api/providers/health/schedule` is the setting.

### The connected clouds panel now starts folded away on a desktop too

On a phone the sidebar has always been a drawer: the file list is what you came
for, and the accounts are a place you visit. On a desktop the same panel was
simply always there, taking 286px off the file browser for the whole session
whether anybody was reading it or not — a status board for something that
changes when you connect a cloud and then does not change again.

It now starts folded away at every window size, and `☰` in the header brings it
out. That button is no longer a phone affordance: it is the switch for a pane
that is off by default, and pressing it again puts the panel back. On a desktop
the panel folds to nothing beside the file browser rather than sliding over it,
so bringing it out costs the browser its 286px and nothing else — no overlay, no
dimmed background, and the file list stays where it was. The `✕` in the panel's
own corner does the same thing from the inside.

Nothing about the panel's contents changed, and neither did the phone: below
860px it is still a drawer over the file list, because there is no room to stand
one pane next to the other. A folded panel is hidden rather than merely narrow,
so nothing in it can be reached by tab while it is shut.

### A folder's menu now says what the folder is holding

Opening `⋯` on a folder showed its name, the word *Folder*, and a wide empty
space where the useful part should have been. The list of things you could do to
the folder was there — move it, scatter it to other clouds, delete everything
inside it — with nothing to tell you how much *everything inside it* was.

That space now carries the figures the row cannot show:

```
Movies                                    5      28.1 MB          3
Folder                                  FILES     IN HERE     CLOUDS
42.2 MB across the clouds · 3 folders inside · newest Aug 23
```

**In here** is the `du -sh` reading — everything at or below the folder, however
deep it sits. **Across the clouds** is what that actually costs, which is the
bigger number: a file cut two-of-three is stored one and a half times over, and
that is the figure your accounts' free space is spent in. **Clouds** counts the
accounts holding a part of something under there, and names them if you hover
it. A folder holding a file that went out short of a part says so on the same
line.

It is worked out when the menu opens rather than carried on every row. Counting
one folder is a walk of the encrypted index and contacts no account, but a
listing of forty folders would have been forty walks for thirty-nine menus
nobody opened. If it cannot be fetched the menu opens exactly as it did before —
every choice still works, and this was a question nobody typed.

`GET /api/folders/stats?path=` is the same answer for anything else that wants
it.

### Stray parts now looks at the machine SAND is running on, too

**Vault settings → Stray parts** asked every cloud what it was holding that no
file in your vault needs. It never asked the one disk SAND writes to itself —
the folder your vault file lives in, `/var/lib/sand` for the service and
`~/.sand` on a desktop — and that is where the bigger number usually is.

An upload has to be written to that folder in full before any of it is sent:
every chunk carries the whole file's hash, and a stream will not give up its
hash until its last byte. SAND deletes that copy the moment the upload ends,
however it ends — but a process that is killed, or a machine that loses power,
never gets to that line. What is left is the whole file that was being
uploaded, at full size, in a folder nobody thinks to look in. Four interrupted
films is thirty gigabytes gone from a disk that was probably chosen for being
small, and until now nothing in SAND would ever have reclaimed it.

The scan now says so, beside what your clouds are holding, and **Tidy up** lists
the files with their sizes and when anything last wrote to them. Erasing them
frees room on the machine and changes nothing in the vault: these are SAND's own
scratch copies, not your files.

Two rules keep that promise. Only the temporary names SAND writes itself are
ever looked at — your vault file, your own notes, and anything a provider keeps
in the same folder are not listed and cannot be erased from here. And nothing is
offered until it has been left alone for an hour, because a file still being
written to is exactly what an upload running in another window looks like from
the outside; those are shown with the reason beside them instead. Anything this
SAND is writing right now is left out of the list entirely.

`sand vault sweep` reports and erases them alongside the cloud half, and
`--verbose` names each one.

### An import can be handed to the machine and left to it

Bringing files in used to mean keeping the page open. The import *was* the
request, so navigating away, letting a phone sleep, or closing the tab stopped
it — and stopped it with the browser's own unhelpful "load failed" as the only
explanation.

**Keep going if I close this page**, next to the Import button, hands the
transfer to the machine instead. It runs with nothing behind it, the dialog
picks the progress up again whenever you come back to that machine, and the
result waits there to be read. A running one can be stopped from the same
place, and stopping keeps every file that already landed — it is closer to a
pause than a cancel, since running the same import again skips those and
carries on.

It is opt-in, and stays that way. An import that keeps running after the page
is closed is something to have decided, not to discover. Up to four can be
going at once; past that they only slow each other down.

Two limits worth knowing. Restarting SAND stops a detached import: nothing
about a transfer in flight is written down, deliberately — re-running the
import is still the whole of the resume story. And locking the vault stops
one too, since the keys its chunks are being sealed with are exactly what
locking takes away. While one runs, the vault will not idle-lock out from
under it.

When a foreground import does lose its connection, the dialog now says what
happened and what to do about it, instead of passing on "load failed".

### An import you can watch, and an honest sentence about interrupting one

Bringing files in from a machine used to go quiet. The dialog said *Bringing
them in…* and then said nothing at all until the whole selection had landed —
which is fine for a folder of photographs and useless for the case people
actually reach for it: one very large file. An 18 GB film looks exactly like a
hung request for the hour it takes, and there was no way to tell the difference
from outside.

It now draws the file it is on: which file of how many, whether it is **coming
down from the machine** or **being split, encrypted and scattered** to the
clouds, how far through that half it is, how fast it is moving and roughly how
much longer that leaves, and what has already landed or been skipped. The two halves are named apart rather than added together, because
they are two passes over the same bytes and they are slow for different reasons
— one is the machine's upstream, the other is yours. The speed is per half for
the same reason, and a transfer that stalls stops claiming one rather than
holding the last good number.

This is a view of the request that is running, not a job queue growing quietly
in the corner. The server keeps it in memory for exactly as long as the import
request is open, the dialog asks for it once a second, and nothing is written
down: an import that finishes, fails or is cancelled simply stops being listed.
Re-running an import is still the whole of the resume story.

**And the sentence about interrupting one has been corrected.** It used to say
that if an import is interrupted, nothing is lost. That is true of files and
was never true of a file: every file that arrived *whole* is scattered and
committed, and re-running skips it — but a file cut off partway is not kept,
and the next run fetches it again from the first byte. On a selection of two
hundred files that distinction hardly shows. On a selection of one 18 GB film
it is the entire answer, and the old wording promised the opposite of what
happens. Both the dialog and the README now say which one it is.

### SAND makes the SSH key, so nobody has to paste the wrong half

Connecting a machine you have an SSH login on — as somewhere to keep parts, or
as somewhere to bring files in from — used to start with homework. Run
`ssh-keygen` somewhere else, work out which of the two files it wrote is the
private one, paste **that** one into a browser, and install the other on the
server. Three steps, each of them a chance to paste the wrong half, and the
wrong half is the interesting one to get wrong.

Both connect forms now open on **Generate a key pair** instead. SAND makes an
Ed25519 key, keeps the private half, and gives you one line to add to
`authorized_keys` on the server — with the command that appends it, for a box
you reach through a shell rather than a web console. The paste runs the other
way now, and the half that travels is the half that is meant to be handed out.

The private half is never sent to the browser at all, not even to be shown.
It is made on the machine SAND runs on, held there while you finish the form,
and written into the encrypted vault when the connection is stored. What the
form carries in the meantime is a handle standing in for it. That matters
because SAND is usually reached over plain HTTP at a LAN or tailnet address,
and an SSH private key is the one credential here that opens a shell rather
than a bucket.

**Your own key still works and is one word away.** *I have a key* puts the
paste box back exactly as it was — a key you already have for the box, or one
issued by a CA, is not something SAND can invent a replacement for.

`POST /api/ssh/keypair` is the endpoint, and it answers with the public half, a
fingerprint, and a handle that expires in half an hour. A form older than its
key says so and offers a new one, rather than failing with "this does not look
like a private key".

**Fixed while building it:** locking the vault dropped its live backends
without closing them. For every other kind of account that is the same thing —
they share one HTTP transport — but an SSH backend holds a socket and a session
on somebody else's machine, and letting go of it does not end either. A vault
locked and unlocked a few times left a trail of sessions on the far end until
sshd's own timeout noticed them. Disconnecting an account already closed
properly; now locking does too.

### How much more fits on each cloud

Every account card and every row of the upload picker said how much of the vault
that cloud is holding. None of them said how much more it would take — and
284.9 GB of parts is the same figure on a drive with four terabytes free and on
one with forty megabytes. Only one of those is somewhere to put the next file.

Room left is now named beside what is stored, everywhere the clouds are listed:
the sidebar, the *Stats* panel, the upload and default-clouds pickers, and the
`FREE` column of `sand remote list`. So is where the figure came from, because
the three sources are not the same kind of claim — the account's own live
reading, a capacity you typed against a count of a bucket, or a quota you set.
Where two of them have an opinion the room left is whichever leaves less: a
spent quota leaves nothing on a half-empty drive, and a full drive leaves
nothing whatever the quota says.

**A quota you set, for the accounts nothing else can answer for.** A capacity
says how big an account is; a quota says how much of it is SAND's to fill, which
is a different question and often the only one with an answer. Between a Drive's
own quota call, a filesystem's free blocks and a listed bucket there are still
accounts whose only known figure is what SAND itself wrote — and against a line
you draw, that figure becomes a fraction, a usage bar and a place in the picker's
ranking. It is offered on every account rather than only the silent ones: a cloud
reporting two terabytes free is still a cloud you might only want two hundred
gigabytes of parts on.

```bash
sand remote edit gdrive --quota '200 GB'
sand remote edit gdrive --quota none
```

or **Edit account → Quota** in the browser; `PATCH /api/providers/{id}` takes
`quota` as typed text alongside `capacity`.

**Crossing it warns rather than refuses**, and the reason is durability. The
parts of a file are placed together, so dropping the one part that would cross a
quota leaves the file on fewer clouds than the erasure code it was cut with
promises — a quiet loss of margin traded for a line nobody else can see. The
upload stores and says what it did. It says it once, by the file that crossed;
the four hundred behind it in the same batch do not repeat it, because being over
is a state rather than an event and the card, the panel and the `FREE` column all
carry it until the line is raised or files are moved off.

**Before the bytes move**, the upload dialog checks the share of the file each
chosen cloud is about to receive — about a kth of it under a k-of-n cut — against
the room that cloud has, and names the ones it will not fit on. Clouds that
cannot say are counted rather than passed: silence is not an all-clear.

### Proton Drive without the desktop app

Proton Drive gains a second backend, `protoncli`, that talks to Proton rather
than to the folder its desktop app syncs. It drives `proton-drive`, the
command-line client Proton builds on its own Drive SDK.

Where the existing `proton` backend can run, this one is better in three ways.
It works on a machine with no desktop app, which is most servers — previously
that case needed `rclone serve webdav` in front of rclone's reverse-engineered
backend. Its upload confirms the part reached *Proton*: the folder backend's
`Put` returns as soon as the file is on local disk, so an account whose client
is signed out looked healthy while holding nothing. And it can measure what the
account holds, where a folder account can only measure the free space on your
disk.

The folder backend is unchanged and stays the default for a laptop that runs
the Proton app. Existing `proton` accounts keep working exactly as they did;
nothing migrates.

**Signing in is a link, not a redirect.** Proton's client prints one and waits,
so SAND shows it — copyable, because it can be followed on a phone or another
computer. That is how a headless box connects an account. From a terminal,
`sand remote proton login --name proton` does the same thing.

**The session is kept in the vault**, not in a file on disk. Proton's client
stores it in the OS secret store, which a systemd service with no keyring and
no home cannot reach; SAND writes it to a private 0600 file only for as long as
one command runs, and picks up Proton's rotations on the way back out. The
session holds the password that unlocks the account's key material, so this is
not the same as caching an access token.

**`scripts/quickstart.sh` builds the client**, with `CLI_APP_VERSION_NAME` set
so the install identifies itself honestly to Proton as a third-party client.
`INSTALL_PROTON=never` skips it; `PROTON_CLI_URL` takes a prebuilt binary
instead, which on a Raspberry Pi is the difference between seconds and twenty
minutes. A 32-bit board, a failed build or a failed download each warn and
leave the rest of the install alone — Proton can still be connected as a synced
folder. The unit gains `SAND_PROTON_STATE_DIR` under the data directory.

Proton's cryptographic model changes at the end of 2026 and clients
implementing only the old one stop interoperating. Because SAND drives Proton's
own binary rather than reimplementing its cryptography, that migration is
handled by updating the client — re-run the installer.

### A door to the stray parts, not just a notice about them

**Vault settings → Stray parts** opens what the tidy-up banners open. Until now
those two panels — the sweep for parts nothing points at, and the repair for
shards a disconnect mislaid — could only be reached from a banner the app raises
by itself when the set of connected clouds changes. That banner is dismissible
and only appears when there is something to say, so waving it away, or wanting
to check before the app volunteers anything, left nothing to click.

The new line runs its own scan when it is opened rather than reusing whatever
the app last saw, and reports both halves of what that scan finds. It shows no
figure until it is opened: the answer is a full listing of every account, which
is not something a settings menu should quietly start.

## v2026.8 — calendar versioning

The version is now `vYEAR.MONTH.PATCH`: the year and month the release line
opened, then the repository's commit count, exactly as before. `v2026.8.311` is
the 311th commit on the 2026.8 line.

Nothing about the software changed with this — it is a change to what the
numbers mean. The old `MAJOR.MINOR` claimed a compatibility promise the project
was not actually making at that granularity; a date says plainly how old a build
is, which is the question anyone reading a version here was asking anyway.
Breaking changes are called out in this file, which stays the thing to read
before upgrading.

The year and month remain hand-bumped source constants (`Year`/`Month` in
[`internal/version/version.go`](./internal/version/version.go)) rather than
being read from the build clock, so rebuilding an old tree still reports the
version it originally shipped. The month is not zero-padded — semver forbids a
leading zero, and every tag stays something a semver parser will accept.

Upgrading needs nothing. `SAND_INSTALL=release` asks GitHub for the latest
published release rather than comparing numbers itself, so it picks up
`v2026.8.x` the same way it picked up `v2.0.x`, and `SAND_RELEASE` still pins
whatever tag you give it.

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

### How full a drive is, and how much of that is SAND

An account card used to say "195 parts · 33.9 GB" and, for the clouds that
report a quota, "16.3 GB / 17.9 GB used". Neither number answers the question
somebody actually has in front of an upload, which is whether there is room —
and on a local folder there was no second line at all: a directory on a 5 TB
disk and a directory on a nearly full USB stick looked identical.

Local folders now report the drive they sit on, so every account has a capacity
line, and the line is drawn as a bar with the split in it: **what SAND put
there, what was already on the account, and what is free**. The distinction is
the point. A Drive is mostly photographs somebody put there by hand, and a disk
is shared with everything else on the machine; the parts SAND wrote are usually
the small part, and drawing them as the whole of a 5 TB disk described a
computer nobody has. Free space is what the filesystem says can actually be
written, not the subtraction — a reserve only root may spend, or a quota that
cuts an account down from the disk under it, is named as its own figure rather
than counted as room you have.

**Stats** on any account card opens the breakdown behind that bar:

- capacity, with SAND's share, everything else, free and reserved each named
  and measured rather than left to a colour
- what the parts belong to, by kind and by folder, weighed by what each thing
  left *on this account* rather than by its own size
- when they arrived, by month
- the heaviest files on it
- how many files could not be rebuilt without this account — the count the
  disconnect guard refuses on, said before you go looking for it

Everything in the panel is read off the index, so it costs one ping and no
listing: the vault already knows which parts it wrote where and what each one
weighs. The breakdowns come from the main vault alone. What a sub vault put on
an account counts towards the capacity figures and towards the last of those
counts — as it always has, from the inventory, so a locked sub vault never
makes an account look emptier than it is — but it is one line rather than a
list of its folders. A panel about a *cloud* is not where a second password's
contents should start appearing.

### A vault inside your vault

Some things should not be readable by whoever holds your vault password —
which, once a vault is mounted as a drive and left open on a laptop, is a
broader set of people than it sounds. So a vault can now hold **sub vaults**,
each sealed under a password of its own. Your vault password lists them and
opens none of them.

A sub vault has its own tree, so nothing collides with what is already stored,
and its own encryption keys, so a password change on your vault carries it
across untouched. Moving a folder into one keeps its path and is instant: the
index changes and not a byte travels between your clouds. The files are then
re-encrypted onto the destination's own key behind the move; until that
finishes, they can only be read while the vault they came from is open, because
no key is ever handed from one vault to the other.

They never appear on a WebDAV mount. Not while locked, and not while unlocked
either, and there is no setting for it: a mounted drive is a folder every
process running as you can read, and what goes in a sub vault is what should
not be reachable that way. In the browser they are behind *Show sub vaults*,
where a locked one is still listed — you are meant to see there is a place
called Taxes and be asked for a password, rather than have it be invisible
until you think to go looking.

What your vault password does see is the name, and an inventory of which
accounts hold the parts and how big they are. That boundary is deliberate: it
is what lets a sub vault whose password you have forgotten still be erased from
your clouds rather than leaving its parts there for good, and what stops
disconnecting an account silently stranding files nothing in the process can
see. It sees no path, no filename, no size and no type.

There is no recovery for a sub vault's password — not from your vault password,
and not from a `manifest.sand`. The backups carry a sub vault sealed, so a
recovery brings it back shut and its own password opens it afterwards.

```
sand sub new Taxes
sand sub assign /Papers Taxes
sand ls --in Taxes /Papers
```

### A cloud you reconnect can tell you it already holds a vault

Every vault keeps a copy of its encrypted index on each account it uses, so an
account from a machine that has since died still carries one. Connecting it to
a new vault now says so, and offers to bring what is there in as a sub vault.

That is the case `sand vault recover` could never handle: recovery replaces the
vault's data key, so it refuses to run against a vault that already holds
anything. A backup carries an index, a data key and a password that opens it —
which is exactly what a sub vault is — so what was found lands beside what you
have rather than replacing it, and the restriction simply does not arise.

You give the old vault's password to open its index and choose a new one for
the sub vault. The second costs nothing: the old data key is adopted as it
stands, so no file is re-encrypted by the import. Rotating that key afterwards
is what finally makes the old password useless, and the app offers it.

Only whether a backup is yours can be told without a password. When it was
written, what it holds and how big it is are all inside the envelope — an
account holding someone's backup should not be able to describe it to whoever
connects it next.

### Index backup has a switch now

The one setting that decides whether a lost machine is recoverable could only be
reached from the command line. It joins the rest of them in vault settings,
alongside a line for your sub vaults.

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

### The browser notices there is a vault to recover, and says so

Recovery used to be something you had to know existed. Someone whose machine
died would reinstall SAND, make a fresh vault, reconnect their clouds, see an
empty file list, and have no reason to suspect that everything they owned was
sitting on those accounts waiting to be claimed — `sand vault recover` only
helps the people who already know to type it.

So the app looks. On a vault holding no files, connecting an account asks it two
questions no password is needed for: what are you holding, and is that
`manifest.sand` one this vault wrote? An index backup written by a *different*
vault is the signature of the disaster this feature is for, and it opens the
prompt:

> **Sand files detected** — 412 parts (3.1 GB) and an encrypted copy of a vault
> index this one did not write.

And then it walks the rest of the way, because that prompt fires on the *first*
cloud you reconnect and one cloud is never enough: a file is rebuilt from two of
its three parts. So the dialog does not open on a password box it cannot use
yet. It asks for the next cloud, connects it without leaving the dialog, and
re-checks by itself when it lands — the second account turns it into the
password prompt, and the last one is taken as the answer to the question it
asked, running the recovery rather than making you press the same button again.
It still checks before it commits.

The password it asks for is the one belonging to the vault that is gone, not
this one's — a distinction the dialog makes rather than leaving you to discover.

**And it says what did not come back.** A recovery is only as complete as the
accounts you managed to reconnect: a file is rebuilt from any two of its three
parts, so one cloud you have not got back yet costs nothing, and two costs you
the file. The report now counts both halves — files *and* bytes, because those
diverge and the bytes are usually the answer to "how bad is it" — and then names
the shortfall:

```
Recovered 18 of 23 file(s) in 6 folder(s) — 1.2 GB of 4.4 GB.
  2 file(s) came back with no spare part left.

Not recovered: 5 of 23 file(s), 3.2 GB of 4.4 GB.

Connect these accounts, then 'sand vault recover --resume':
  onedrive-personal         onedrive   9 part(s) — 5 file(s) cannot be opened without it
  nas-backup                webdav     3 part(s) — spare parts only
```

The same report renders in the browser, and the same distinction survives: an
account holding only spare copies is listed but not blamed, because reconnecting
it changes nothing about what you can open. Files that came back openable but
without a spare are called out too — they read fine and they have no redundancy
left.

### …and finishing it when the last cloud turns up

Telling you which accounts to connect is only worth saying if connecting them
finishes the job, and it did not: running `sand vault recover` a second time was
refused, because by then the vault held the files the first pass brought back
and adopting the snapshot again would have replaced the very key they depend on.
The advice was a dead end.

`sand vault recover --resume` is the way through. It is a much smaller operation
than a recovery — the index is already here and so is the key; what was missing
was a reachable copy of the parts — so it asks every account what it holds,
re-points the records that now have somewhere to point, and **asks for no
password at all**. The browser offers the same thing as *Finish recovery*, and
`sand vault status` now counts what is waiting on it: `unresolved` shard records
naming accounts the vault is not connected to, and the `stranded` files that
cannot be opened because of them.

Three endpoints carry all of it: `GET /api/vault/recovery` for the scan,
`POST /api/vault/recovery` for the dry run and the rebuild, and
`POST /api/vault/recovery/resume` to finish one later. All need a session.

### …and taking the recovered files off the dead vault's key

A recovery adopts the lost vault's data key. It has to — that key is the only
thing that opens the parts already sitting on your accounts — and it means the
job is not finished when the files come back. The key is derived from the *old*
password, and every copy of the old `manifest.sand` hands it over, including any
taken off an account before the replacement vault existed. Recovery replaces the
copies it can reach; it cannot replace the ones it cannot. So the files stay
readable by whoever could read them before the machine died, and nothing said so.

Now the vault says so and keeps saying so — in `sand vault status`, and as a
standing banner in the accounts panel — until:

```bash
sand vault reclaim --account work --account offsite --account nas
```

A fresh data key sealed under your **current** password, every file rebuilt onto
it, and the parts the old key opened erased. Your password does not change; only
what it protects does. And since every file is gathered and scattered anyway,
that is the one cheap moment to say where they should live: `--account` moves
them off the clouds a machine you no longer have picked. The browser offers the
same thing with the cloud picker in it.

It costs a download and an upload of the whole vault, which is why it is offered
rather than done as part of the recovery — a recovery has to work with the
network you have, and this can wait for the one you want. Files stay readable
throughout, an interrupted run resumes with `sand vault migrate`, and a
selection that could not hold a file is refused before the key rotates rather
than halfway through.

### A CLI upload no longer outruns its own backup

`sand put` returned the moment the parts were on the accounts, and the push of
the encrypted index runs on its own goroutine so that an upload never waits on
network round-trips it does not need. The process then exited — and locking the
vault on the way out aborted the push outright, since it needs the keys that
just went away.

So the copies on the accounts described a vault one file behind, and the file
missing from a recovery was always the one stored last. Every CLI command now
lets that push settle before it locks. A command with nothing to push returns
immediately; one that finds a push left over by an earlier command finishes it.

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
- The browser can do all of this too: **Change vault password** sits at the
  foot of the accounts panel, says how much it is about to re-encrypt before
  you commit to it, and reports what moved.
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

### A folder is picked, not typed out

The two backends that take a path — a local folder, and Proton Drive through
the folder its desktop app syncs — used to ask for it in full, spelled
correctly, from memory. That path belongs to the machine SAND is running on,
which is very often not the machine you are typing on: the vault gets driven
from a phone, and the phone has no idea what is mounted on the NAS.

Both fields now come with **Browse…**. It walks the server's own folders,
starting from home and the roots a drive appears under — `/media`,
`/run/media`, `/mnt`, `/srv`, or the drive letters on Windows — with a
breadcrumb back up, hidden folders behind a toggle, and symlinked folders
followed, since a synced cloud folder is usually one. The field stays a text
box, so a path you already have still pastes in.

Where it opens is the first of those SAND can actually read, which under the
service is not home: it runs as a user with no home of its own and a unit that
sets `ProtectHome=yes`, so `$HOME` resolves to `/home` and the sandbox refuses
to open it. That case falls through to the vault's own directory, and an
unreadable folder is dropped from the shortcuts rather than offered as one that
only ever fails. Walking into a folder SAND cannot read is a listing that says
why — with its parent and the shortcuts still there to leave by — rather than a
dead end.

A folder that does not exist yet is still a valid answer, because connecting
creates it. Type a path for one and the picker opens at the nearest folder
that *is* there, keeping the rest as a name to create inside it — so what you
typed comes back unchanged if you just confirm.

What is behind it, `GET /api/system/folders`, is deliberately the smallest
thing that could work: it answers with folder names, never a file and never
its contents, and only to a session that has unlocked the vault — a session
that could already point an account at any folder on the machine by typing its
path.

### A folder of photos looks like one

The list used to show the same `🖼` against every picture, so a folder of
photos was a column of identical icons and a filename you had to read. Images
and PDFs now carry a thumbnail: the picture itself, and for a PDF the first
page, which is how anyone recognizes a document long before they read its name.

The obvious way to do this would be ruinous. There is nothing small to fetch —
drawing one row would mean gathering two parts of a 4 MB photo from two
accounts and rebuilding the whole file, for a 52-pixel square. So the picture
is made **once, when the file is uploaded**, and stored like everything else:
compressed, split into three encrypted parts under the vault's data key, and
scattered across the same accounts by the same placement rules.

They are stored a folder at a time rather than a file at a time, and the reason
is Argon2id. Sealing an archive derives a key once — 64 MB, three iterations —
whatever the archive weighs, so a 9 KB thumbnail would cost exactly as much as
a 4 GB video, on every write and every read. One pack per folder pays that once
for the whole listing: opening a folder gathers a few hundred kilobytes, and
every row draws from it.

The pictures are made in the browser, because that is the only place they can
be made at all. A static Go binary cannot rasterize a PDF page without a C
library, cannot decode the HEIC most phone photos arrive as, and would ignore
the EXIF orientation that leaves a third of them on their side. A tab can do
all three. What it sends is decoded and re-encoded server-side before it is
stored, so a thumbnail is always the vault's own JPEG at a known size rather
than whatever bytes a caller supplied.

Nothing about them is precious. A thumbnail is derived from a file that is
still stored, so anything that goes wrong ends at the icon the list has always
shown: a format that cannot be decoded, an account that has gone quiet, a file
uploaded before this existed. Deleting a file drops its picture, moving one
carries it, and changing your password erases them rather than paying to
re-encrypt them — they come back as files are opened.

Opening a picture that has no thumbnail stores one on the way past. The file
has just been rebuilt and decoded on screen, so taking a copy of it costs a
canvas and nothing else.

### Every account has a colour of its own

A file's three part badges have always been coloured by the account holding
each part, but the colour came out of a hash of the account's id — so two of
your accounts could land on the same one, and a row of badges would then claim
a file was on two clouds when it was on three. Colours are now handed out
against the whole account list instead: each account still starts at the colour
its id hashes to, so a colour stays put as other accounts come and go, but
anything that would collide takes the next free colour. No two connected
accounts share one until you have more accounts than the palette has colours.

The other half of the match is now drawn where it can be read. Each account's
card in the sidebar carries its colour as a swatch beside the name, not only as
the stripe down its edge, and the per-part health read-out carries the same
swatch on each row. Which three clouds a file is on is a question you answer by
looking from the badges to the sidebar.

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

### Which clouds a file goes to is yours to choose

Policy decides how many parts may share an account. Connect a fourth account
and something also has to decide *which* three a file uses — three parts cannot
go to five places, and until now that was decided by the order the accounts
happened to be connected in.

Now every upload chooses. The browser asks before a single byte leaves the
machine: the dialog names the clouds the files are about to be scattered over,
each of which can be swapped for another account, and says what a narrower
choice costs before you make it rather than after.

What that dialog opens on is the vault's **default clouds**, set from the
accounts panel, from the upload dialog itself, or with
`sand vault defaults usb-drive r2-cold nextcloud`. With no default set — which
is how every existing vault starts — each file gets three clouds picked at
random, seeded from its own archive ID. That is what finally makes a fifth
account worth connecting: uploads spread over everything you have joined
instead of piling onto the first three.

A selection is followed exactly rather than quietly completed. Choosing two
clouds stores two parts and warns that the file has no spare, instead of
putting the third somewhere you deliberately left out — deciding which
providers may hold your data is the entire point of SAND. Disconnecting an
account drops it from the default, and re-encrypting after a password change
puts a file back on the accounts it was already on.

On the command line it is `sand put report.pdf --accounts usb-drive,nextcloud`
for one upload and `sand vault defaults` for the standing answer; over HTTP,
an `accounts` field on the upload and `POST /api/vault/defaults`.

### A file can be spread over six or nine clouds, not just three

Three clouds, one part each, was the widest a file could go — and it was not a
choice so much as a consequence. The split was two halves and their XOR, which
is addition in GF(2), and over that field two symbols have exactly three
non-zero combinations. There was no fourth shard to make.

The coder now works in GF(2⁸), where every byte is a usable coefficient, so the
same idea generalises to **k of n**. How many clouds you pick chooses the code —
one rule, clouds in groups of three, two thirds of the shards rebuilding:

| Clouds | Scheme | Storage | Clouds that can go dark | Clouds needed to rebuild |
|---|---|---|---|---|
| 3 | `2-of-3` | 1.5× | 1 | 2 |
| 6 | `4-of-6` | 1.5× | 2 | 4 |
| 9 | `6-of-9` | 1.5× | 3 | 6 |
| 12 | `8-of-12` | 1.5× | 4 | 8 |
| 3m | `2m-of-3m` | 1.5× | m | 2m |

The table stops where patience does, not where the code does: every multiple of
three works, to a ceiling of 255 clouds set by the one byte a shard number
occupies. Each group you add buys one more cloud that can fail and two more that
would have to collude.

Every scheme stores 1.5×, so **widening costs accounts rather than bytes** —
thirty clouds is not a byte more than three. What it buys is on both axes SAND
cares about. More clouds can fail. And more of them have to be broken into
together before what they hold is enough to rebuild anything: `6-of-9` means six
of your providers colluding, where every previous version of SAND meant two.

It also shrinks what one compromised account plus your password yields. Shards
are consecutive slices of the compressed stream, so an account holds 1/k of a
file — a sixth at `6-of-9`, against the half it held at `2-of-3`.

Every account still holds exactly one shard, at every width. The promise that a
single provider's copy is noise is not weakened by spreading further; it is
strengthened.

Three remains what an upload takes when nobody says otherwise, whatever is
connected. Set it per upload or save it as the default:

```bash
sand vault defaults box s3 drive dropbox onedrive proton
sand put taxes.pdf --accounts box,s3,drive,dropbox,onedrive,proton
```

Counts that are not whole groups — 4, 5, 7, 8 — are refused rather than rounded
down, in the picker and on the command line alike. There is no code that uses
them without leaving a cloud with no shard of its own to hold.

Widening does not cost memory or per-provider load either. The chunk window
bounds requests per provider whatever the width, and the bytes in flight are
`window × n × chunkSize/k` — with n/k fixed at 1.5, a thirty-cloud vault holds
exactly as much as a three-cloud one. What does grow is the total object count:
a chunk becomes n objects, so a 4 GB file is 768 of them at `2-of-3` and 7,680
at `20-of-30`, spread over ten times as many accounts.

**Changing an existing file's width rebuilds it.** A `2-of-3` file's shards are
halves of its compressed stream and a `4-of-6` file's are quarters, so nothing
carries across: the file is gathered, cut again and written out. That is a whole
download and upload, against 1/k of a file per shard for a move at the same
width, and both `--dry-run` and the browser's estimate label it as a rebuild and
price it separately. Moving a file between clouds without changing the count is
still the cheap case it always was.

Everything already stored keeps working untouched. Parts written under formats 1
to 3 are all `2-of-3` and are still read; new writes use format 4, whose header
carries k and n so that a shard says which code it belongs to — and so that an
offline restore from part files and a manifest backup needs no index to work it
out. `sand convert` moves an old file onto the new format when you ask it to.

### Moving what is already stored to different clouds

Choosing where a file goes was a decision you made once, at upload, and could
not revisit. Now a file — or a whole folder — can be picked up and set down on
other accounts: `sand relocate /photos --accounts box,r2-cold,nextcloud`, the
**⇄ Move to other clouds** button in the browser's parts inspector, or
`POST /api/relocate`.

**Only the parts that have to move move.** Placement is a set of accounts, not a
sequence, so anything already sitting on a cloud you kept stays exactly where it
is — swapping one of three copies one part rather than rewriting the file, and
naming the same three in a different order does nothing at all.

What does move is carried across as the encrypted blob it already is. A part's
object name is derived from the file's random archive ID and the part number,
never from the account holding it, so moving one is a download and an upload of
the same bytes under the same name. Nothing is decrypted on the way, no data key
is touched, and the file keeps its identity, its plaintext hash and its chunk
layout. That is the difference between this and re-encrypting after a password
change, which has to rebuild every file it touches.

The order is copy, then commit, then erase, one file at a time. So an
interrupted move — a cloud that stops answering, a laptop that sleeps — costs
the file in flight and nothing else, leaves every file readable, and is finished
by running it again. A part that will not copy is reported and left where it
was.

Two things it says out loud before doing them. Sending a three-part file to two
clouds under the `strict` policy erases the spare, because no account may hold
two parts of one file — a relocation is not allowed to produce a placement an
upload could not have produced. And `--dry-run`, or the browser dialog as you
pick, prices the whole thing first: how many parts travel, how many bytes, and
how much of it is already where you are sending it. That answer comes out of the
encrypted index alone, without contacting a single account.

A folder's stored thumbnails come along too, so moving a folder off an account
really does move it off.

### Moving something to another folder

The other move, and until now the missing one: the browser could send a file to
different clouds but not to a different folder. Tidying a vault meant downloading
a file, deleting it and uploading it again — a round trip of every byte to change
one word.

Now every row carries **→** for it, beside Download and Delete on a file and
beside **⇄** on a folder, with *Move to another folder* in the row's menu on a
phone or a tile. Tick several rows and **→ Folder** in the selection bar moves
the lot; **⇄ Clouds** beside it is still the other move, renamed so the two are
told apart by what they move something onto. The dialog opens on the folder you
are already in, walks the vault's own tree, and has a **+ New folder here** for
a destination that does not exist yet. On the command line `sand mv` now takes a
folder as well as a file, and over HTTP it is `POST /api/files/{id}/move` for a
file and a new `POST /api/folders/move` for a folder.

**Nothing is transferred.** Which folder something is in is a field in the
encrypted index, and a part's object name is derived from the file's random
archive ID rather than from the folder it sits in — so this rewrites that field
and contacts no account at all. A 4 GB film moves as fast as a note, and its
parts stay on the same clouds under the same key. A folder is a path in the
index in exactly the same way, so moving one carries every file beneath it, at
any depth, in a single index write: there is no moment where half a tree answers
to its old name. Its thumbnails and its film-details setting travel with it,
since both are filed under the folder rather than inside it.

What cannot happen is said rather than attempted. A folder cannot be moved
inside itself and is not offered as a destination; anything already in the folder
you are looking at says so instead of being moved onto its own path; and a name
already taken in the destination is refused for that one file, with the rest of
a selection still moving.

### Naming an account, and choosing its colour

A connected account is yours to label. **Edit** on its card in the sidebar opens
a dialog for what it is called and the colour it wears — the same colour on the
card, on every part badge in the file list, and in the cloud picker, which is
what makes "which three clouds is this file on" a question you answer by eye.
Twelve named colours are the shortlist, **All shades** opens the whole palette —
the same twelve hues in three shades each, a hue per column, so "the same blue
but deeper" is a move downwards rather than a hunt — a native picker takes any
colour at all, and **Automatic** hands the choice back: the browser then picks
one and keeps it stable as other accounts come and go. Every colour in the
palette is light enough to carry the app's dark text and dark enough to hold
against the surface behind it, since a part badge is a number drawn *on* the
account's colour.

A chosen colour is claimed before the automatic ones are handed out, so nothing
else drifts onto it — your Google Drive can be the blue one because that is what
it is to you.

Neither field reaches the cloud. Nothing is uploaded, downloaded or
re-encrypted, no credential is touched, and not one part moves. A rename does
travel through the index, though: every part records the name of the account
holding it, which is what the file list, the health read-out and a recovery from
a manifest backup all read, so the new name lands on all of them in the same
write.

On the command line it is `sand remote edit r2-cold --name r2-archive
--color '#38bdf8'` (and `--color auto` to hand it back), with the colour shown
in `sand remote list`; over HTTP, `PATCH /api/providers/{id}` with either field.

### Finding a file again

A vault deep enough to be worth having is one you cannot click through, so the
index is searchable — by name, or by path for a query with a `/` in it, with
`*` and `?` as wildcards. Folders are results too, including the ones that
exist only because something was stored inside them, and each hit says which
folder it was found in.

It is `sand find receipt` on the command line, `GET /api/search?q=` over HTTP,
and a box in the browser's toolbar that searches as you type. Searching from
inside a folder looks there first and offers to widen to the whole vault.

None of it asks a cloud account anything. Filenames and folder structure live
only in the encrypted index, so searching is something an **open** vault can
do and nothing else can — which is also why it is instant: the index is
already in memory.

### Walking the tree like a file manager

The browser knew where you were and nothing about how you got there. The
breadcrumb trail only ever pointed up the branch it was showing, so returning to
a folder you had just left meant finding it again — and in a vault deep enough
to be worth having, that is the whole afternoon.

**Back, Forward and Up** now lead the toolbar, on a phone as well as at a desk.
The current folder is a trail of the ones walked through rather than a single
value, so the arrows step along it the way they do everywhere else; `Alt+←`,
`Alt+→` and `Alt+↑` do the same from the keyboard. The trail lives in memory and
is dropped when the vault locks — folder names are the index, and locking puts
the index away.

**A grid** stands beside the list, because a folder of photographs or films is a
folder whose file names say nothing at all: the stored thumbnail was the only
part of the row anyone was reading, so it becomes the tile. **Sorting** is by
name, size, date or kind, each column opening the way round it is normally
wanted — largest first for size, newest first for date — and reversing when
chosen again. Folders lead whichever column is picked, since a folder in the
index carries a name and nothing else to sort on. Both are preferences and are
remembered, in three words in the browser's own storage and nothing about the
vault.

**On a phone that toolbar becomes a heading.** The desk's row of arrows, trail
and view icons does not survive a 390px screen — half of it is empty and the
trail takes a line of its own to draw a slash — so rather than squeezing that
row, the phone puts something in it worth the space: the folder's name, and
under it the one line only this app can write. A file here is three encrypted
parts on three separate accounts, so `3 files · 42.5 KB · 3 clouds` is the vault
saying what this folder holds and how far it is spread, counted off the index
that already arrived rather than asked of anyone. Uploading sits beside the
name, since it is what people came to do; search, the view, the order, selecting
and a new folder sit on a hairline strip below it, each at a full target.
Tapping the name drops the trail as a list — readable four folders deep, where
crumbs are not, and fewer taps than walking them. The desk's layout is
unchanged.

**Rows can be picked**, singly, in a run with `Shift`, or the lot with the tick
above the columns or `Ctrl+A`. What is picked can be downloaded, moved onto
other clouds, or deleted, in one go. Moving a selection prices the whole thing
as one number — every estimate still comes out of the encrypted index without
contacting an account — and then carries the files one at a time, so a cloud
that stops answering costs the file in flight and nothing else. Deleting counts
folders and files separately before it asks, because a folder takes everything
inside it.

### The browser

Lock screen, a sidebar of connected accounts with live status and how much each
is holding, navigation controls and breadcrumbs — a folder heading on a phone,
saying what it holds and over how many clouds — search, list or grid, sorting,
selection with bulk actions, drag-and-drop upload with progress, part badges in
each account's own colour, a per-part health read-out, and inline preview for
images, video, audio, PDF and text — each one rebuilt on demand.

The wordmark is set in two hands: SAND in the tracked-out monospace it has
always had, and *Vault* written beside it in a monoline hand.

That hand is **3.4 KB and in this repository** — five glyphs, `V a u l t`,
subset from [Caveat](https://fonts.google.com/specimen/Caveat) by Pablo
Impallari under the SIL Open Font License and pinned to one weight, travelling
with its `OFL.txt`. Everyone sees the same mark, including a Linux browser with
no handwriting face installed, which is what the old system-font stack could not
promise — and Caveat's large x-height keeps it legible at the 17px the header
sets it in.

It is never linked. The build reads whatever face is in `web/fonts/` and embeds
it in the page as a `data:` URI, so it is one request fewer than a system font
rather than one more, and it cannot become a call to somebody's CDN however the
app is deployed. **Nefelibata Script** — a *nefelibata* is a cloud-walker, which
is a fair description of a vault living on other people's clouds — is asked for
first and picked up the same way if a licensed copy is dropped in; the
repository cannot carry one. Behind both, the platform's own handwriting faces
still stand. See [`web/fonts/README.md`](./web/fonts/README.md).

It loads no external fonts, scripts or styles: opening your vault makes zero
third-party requests.

Hashed assets are cached for a year and `index.html` revalidates against an
ETag, so an upgrade actually reaches a browser that has opened the vault before.

The layout folds on a phone rather than shrinking: under 860px the accounts
sidebar becomes a drawer, the file table's columns give way to stacked rows, and
controls take touch-sized targets — 44px, in width as well as height. Heights
track the visible viewport, so a collapsing address bar never sits over the last
row of files.

A file row's actions are a single `⋯` on a phone rather than a download and a
delete a few pixels apart: the choices open in a bottom sheet, named rather than
drawn, with the destructive one set apart at the end. Deleting confirms in a
dialog of the app's own instead of the browser's `confirm()`. The part badges
become a read-out, since the same inspector is in the menu by name.

Added to a home screen it goes on as an app: the SAND mark instead of a
screenshot of the page, and a launch without browser chrome. The mark ships in
every form the two platforms ask for — `apple-touch-icon` for iOS, a web app
manifest with 192px, 512px and maskable icons for Android — all rendered from
the one `icon.svg` by `scripts/make-icons.mjs`, and all served from the binary,
like everything else the browser loads.

Launching without browser chrome also means launching without a back button, so
downloading a file no longer navigates the window to it. Sending a home-screen
app at a file the phone cannot render inline — an epub, a zip — left it showing
a bare document icon on a black screen with nothing to press, and force-quitting
the app was the only way out. The bytes are now fetched in the background and
handed to the browser under the file's own name, and the page you started from
never moves.

### A PDF opens in the app rather than in the browser's viewer

Previewing a PDF used to mean framing it and hoping. On a desktop the browser's
own viewer took over; on a phone it produced a blank box or one unscrollable
page, so the app stopped trying and offered an apology and a download button
instead of the document.

It now draws the pages itself, with the pdf.js that was already in the bundle
making thumbnails. A phone and a desktop show the same document with the same
controls: one page at a time, `‹ ›` or the arrow keys to turn, and a zoom for a
dense page on a small screen. The page is fitted to the width it has and drawn
at the screen's real pixels, so it is as sharp as the screen allows.

Only the page you are looking at is drawn, and only the bytes that page needs
are fetched — the content endpoint answers ranges out of the chunks they fall
in, so opening a 300-page scan does not gather 300 pages off your accounts
first. Opening one whose thumbnail was never made — anything put in from the
command line — quietly stores the first page as its picture, since it has just
been drawn and costs nothing to keep.

Still no third-party requests: the renderer, its worker and every byte it reads
come from the binary and the vault.

### Films look like films

A folder of films is a folder whose file names say nothing.
`The.Thing.1982.REMASTERED.1080p.BluRay.x265-RARBG.mkv` is a fine thing to
store and a terrible thing to read, and until now a folder of forty of them was
a column of `🎬` and forty strings to squint at. A folder can now be told its
videos are films, and then they get what Plex and Jellyfin would give them: the
poster, the summary, the score, the runtime, the genres, the director and the
top of the cast.

The `🎬` button in the toolbar turns it on for the folder you are standing in
and everything under it — a films folder is a library, and libraries have
folders inside them. Sweeping it reads each video's name for a title and a
year, looks that up, and stores what comes back. From then on the grid is a
wall of posters captioned with the films' names rather than the files', a row
draws the poster where its icon was, and `🎬` on the row — or the strip above a
video you are watching — opens the rest.

Opening a film shows the film. A video element that has not been played is a
black rectangle with a triangle on it — iOS draws nothing else without a
`poster`, and nothing can make a poster of a video at upload time without
decoding it. On a phone that was half the screen saying nothing, in front of a
film whose artwork, summary and cast were already in the vault. The preview now
opens on those and hands over to the player when you press **Play here**.

Videos get a thumbnail of their own out of the same problem. There is no cheap
frame to grab at upload time, but there is one on screen the moment you watch
something — so the picture is taken from the frame you are already looking at,
the way the app has always backfilled a photograph's thumbnail when you open
one. A film's poster is never overwritten by it; only a video with no picture
at all gets one.

The grid changes shape for it. Tiles are square everywhere else, because a
folder of photographs holds both orientations and crops to a square about
equally badly; a poster is two-by-three, and squaring one cuts the title off
its foot. So a folder that has asked for films gets poster-shaped tiles — the
whole grid rather than the matched tiles alone, since one shape per view is
what keeps the rows level and the folders in step with the films beside them.

**It is off until a folder asks for it, and that is the whole design.**
Everything else in SAND stays between your machine and the accounts you
connected. A lookup does not: it sends a title guessed from a file name, this
machine's address and your own API key to The Movie Database. So it is a switch
per folder rather than a setting, it records which folder it was made on, and
turning it on sends nothing at all — the sweep is a second button. Nothing
about the file itself ever leaves: not its contents, not its size, not its
hash, not which clouds its parts are on. And each film is looked up once —
after that the answer is in your vault, and opening the folder contacts nobody.

The key is yours, free, from themoviedb.org; the v3 key and the v4 read token
both work. It lives in the vault file sealed under your password, and
pointedly **not** in the manifest — that is replicated to every connected
account, and a credential for somebody else's service has no business being
copied onto three clouds.

What comes back is stored the way everything else in the vault is. The text
goes into the encrypted index. The poster becomes the file's thumbnail, which
means it is compressed, split into three encrypted parts and scattered across
your accounts like any other picture — and it is why the browser still fetches
from nowhere but your own server, even in a folder with this on.

Names are read the way every media server reads them: cut at the year if there
is one, cut at the first `1080p`/`BluRay`/`x265` if there is not, and fall back
to the folder's name when the file's says nothing, so
`Blade Runner (1982)/title00.mkv` is matched from its folder. Where two films
share a name, the year in the file name decides which — `The Thing (1982)` is
not the 2011 one. It still guesses wrong sometimes, so the details view always
says what it searched for, **Fix the match** picks the right film out of a
list, and a film chosen by hand survives every later sweep.

A sweep is one search, one record and one poster per film, so a large folder
takes a while — but it is resumable, and nothing already matched is looked up
twice. Posters are written one pack per folder rather than one per film, which
is the difference between two hundred films costing two hundred uploads and
costing one. Changing your password still erases every thumbnail, posters
included; the film's record keeps the artwork's address, so sweeping again
brings them back for one image fetch each and no searching.

Details and artwork come from The Movie Database. SAND uses the TMDB API but is
not endorsed or certified by TMDB.

### Renaming, and a menu on every row

A file or a folder can be renamed from the browser. It was on the command line
all along (`sand mv`) and reachable over WebDAV, and nowhere in the app.

*Rename* sits in any row's menu. The dialog opens with the name selected up to
the extension, the way every file manager does, so typing replaces the words and
keeps the `.mkv`; a `/` in the name is a move rather than a name and the field
says so instead of quietly making folders; and a name already taken in that
folder is refused. Renaming a folder carries everything inside it in one index
write, so there is never a moment where half a tree answers to its old name.

Nothing is transferred, for the same reason moving between folders transfers
nothing: a name is a field in the encrypted index, and a file's parts are named
after its random archive ID rather than after its name.

Getting there is the other half of this. A desktop row's controls were the three
or four things worth a click, and renaming would have been the fifth — so every
row now ends in `⋯`, opening the same sheet a phone has always had. That is
where Rename lives, and with it the things a desk could only reach sideways
before: the parts inspector, moving to other clouds, copying a file's address,
looking up a film. The row grows by one button once, rather than by one button
per feature.

### A folder can wear a picture of what is inside it

The films got posters and the folders holding them did not, so a library was a
row of identical `📁` — exactly the problem the files themselves had before. A
folder can now be given a picture of something inside it: a film's poster, or
the thumbnail of a photograph or a PDF. It reaches as deep as it needs to, so a
library whose films sit one folder each can wear one from two levels down.

**Nothing is picked for you.** A folder keeps its icon until you say otherwise:
which film stands for a trilogy is a matter of taste, and a picture appearing on
a folder you never asked about is the wrong kind of surprise. `🖼` on the
folder's row, or **Folder picture** in its menu, shows everything inside it that
has a picture and lets you pick one; **Use no picture** takes it away again.
Both controls appear only where there is something to choose from, so a folder
of text files is exactly as it was.

**Nothing is stored to do it either.** The folder points at a file that already
has a thumbnail and draws that file's own picture, through the same address its
row draws through — no cover object on any account, nothing to keep in step,
nothing to lose. Your choice is remembered by file rather than by name or place,
so renaming that file, moving it deeper, or moving the whole folder somewhere
else all leave it standing; deleting the file puts the folder back to its icon.

Thumbnails are stored a pack per folder, so a parent whose folders have all been
given pictures gathers a pack per folder the first time it is drawn. Only the
tiles on screen fetch anything and each pack is gathered once, which makes it the
same cost as opening those folders — paid where they are listed instead.

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
- New CLI: `vault`, `remote`, `ls`, `find`, `put`, `get`, `mkdir`, `mv`, `rm`,
  `check`. `sand check --all` exits non-zero on a degraded or unrecoverable
  file, which makes it a reasonable cron job.
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
