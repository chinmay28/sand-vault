# Managing the vault

Three things done to the vault as a whole: changing its password (and with it
the key your files are encrypted under), keeping sub vaults with passwords of
their own, and opening one vault on more than one machine. Internals are in
[vault.md](../design/vault.md) and [paced-rekey.md](../design/paced-rekey.md).

## Changing the password

```bash
sand vault passwd
```

Your files are not encrypted under your password. They are encrypted under a
random data key kept inside the vault file. Re-wrapping that key under a new
password would leave every part readable to anyone with the old password and an
old copy of the vault file, or of the `manifest.sand` on any account. So a
password change **mints a new data key and re-encrypts every file onto it**:
each file is gathered, re-encrypted, scattered again, and its old parts are
erased.

That is a download and an upload per file. It is safe to interrupt:

- The password itself changes in one atomic write, before any file moves.
- Files stay readable throughout. The previous key is kept until nothing needs
  it.
- `sand vault migrate` finishes whatever is left, including files whose cloud
  was offline. `sand vault status` says how many remain.

`--no-migrate` changes the password and leaves the files for later. Until they
move, the old password plus an old vault file still opens their parts.

### Pacing it on a large vault

Re-encrypting everything at once means a day of saturated upload on every
account, and clouds with a daily transfer cap will lock you out. Spread it out
instead:

```bash
sand vault passwd --per-day 10G --folder /Taxes --folder /Photos/2024
```

The password changes now. Files then move at 10 GB a day, the named folders
first and in order, smallest file first. This happens while the vault is open
under `sand serve`, or in the foreground with `sand vault rekey --run`.

| Flag (on `passwd` or `rekey`) | Effect |
|---|---|
| `--per-day SIZE` | daily budget (`10G`, `500M`) |
| `--full-speed` | as fast as the clouds take it, in the background |
| `--folder PATH` | do this folder first (repeatable, in order) |
| `--only-folders` | stop after the named folders |
| `--keep-unlocked` | `sand serve` holds its idle timer off while files are pending |

```bash
sand vault rekey                              # where it stands, pace, time left
sand vault rekey --per-day 25G --all          # faster; drop the folder order
sand vault rekey --pause | --resume
sand vault rekey --skip /Films/Heat.1995.mkv  # leave for last; '.' = the file moving now
sand vault rekey --unskip /Films/Heat.1995.mkv
sand vault rekey --list --under /Archives     # pending files in plan order, and what holds each
sand vault migrate                            # finish everything now, in the foreground
```

In the browser, the password dialog (**Vault settings › Password**) lets you
choose between doing everything at once and a daily pace with a queue of folders
to do first. While a change is in progress:

- The accounts panel shows a card, and a **Previous key** entry appears at the
  top of the vault.
- A file still on the old key shows a small dashed `prev key` marker beside its
  parts, and a folder row shows `N on prev key`.
- The progress panel shows pace, folders, pause and finish-now. Its counts are
  buttons that list the files behind them, each with its state: moving, queued,
  left for last, waiting for a cloud, or failed with the reason and the retry
  time. A row can be skipped, un-skipped or deleted. Delete is for a file that
  will never move, such as one with too few parts.

A file whose cloud isn't answering moves without it when the other clouds can
rebuild it. The part left behind under the previous key is recorded and offered
for [sweeping](maintenance.md#a-part-no-file-wants-sweep) when that cloud comes
back. A file that can't be rebuilt without that cloud waits for it.

**One change at a time.** While any file is still on the previous key, a second
password change is refused everywhere (browser, lock screen and CLI).

On a vault opened by [several machines](#opening-the-vault-on-several-machines),
the other machines have to rejoin from a fresh recovery kit after a password
change.

## Sub vaults

A sub vault is a vault inside your vault, with its own password. Your vault
password lists it but can't open it. Use one for things that shouldn't be
readable just because the main vault is unlocked: a WebDAV-mounted vault can be
read by every process running as you, copied by backup agents, and stays mounted
after you walk away.

```bash
sand sub new Taxes                      # prompts for its own password
sand sub ls                             # what exists, how much each holds
sand sub assign /Papers Taxes           # move a folder in, keeping its path
sand ls --in Taxes /Papers              # any command, inside it (--in takes a name or ID)
sand put --in Taxes ./p60.pdf --path /Papers
sand sub assign /Papers Taxes --out     # move it back out
sand sub passwd Taxes                   # new password and key; re-encrypts its files
sand sub rm Taxes                       # delete it and erase its files (--force if locked)
```

In the browser, use **Vault settings › Sub vaults**. Ticking a row shows that
sub vault above your folders at the root, locked or not. Click one to unlock it.
*Show them at the top of the vault, locked ones included* sets this for all of
them and for new ones. The setting is only a display preference in that browser
and doesn't change how sealed anything is. An open sub vault's row has **Open**,
**Lock**, **Clouds** (its default clouds), **🔑** (password), **💾** (mount)
and **🗑** (delete).

What holds:

- **A sub vault never appears on the vault's WebDAV mount**, locked or
  unlocked. It can have its own drive address, opened only by its own password.
  See [streaming.md](streaming.md).
- **A main-vault password change doesn't touch it.** It is sealed under its own
  password.
- **Your vault password reveals its name and nothing else** inside it: no paths,
  names, sizes or types. It does reveal which accounts hold its parts and how
  much, which is what lets a forgotten sub vault still be erased, and stops a
  disconnect from silently stranding its files.
- **There is no recovery for its password**, from the vault password or from a
  `manifest.sand`.

### Its own default clouds

Until it is given default clouds of its own, a sub vault follows the vault's.
Once set, uploads into it that don't choose clouds go there. The choice is
sealed with the sub vault, so it can only be seen or changed while the sub vault
is open. In the browser, use **Clouds** on its row, or *Make this the default
for Taxes* in the upload dialog while inside it.

```bash
sand vault defaults --in Taxes                       # its own, or "follows the vault's"
sand vault defaults --in Taxes d1 d2 d3 --scheme 2-of-3
sand vault defaults --in Taxes --clear
```

An account that was disconnected while the sub vault was shut is dropped from
its default the next time it is opened. If too few clouds remain, it goes back
to following the vault's default.

### Moving files in and out

Assigning is instant: the index changes and the parts stay put. The files are
then re-encrypted onto the destination's key in the background. Until that
finishes, they can only be read while the vault they came from is open. No key
is handed across.

## Opening the vault on several machines

One vault can run on a desktop and on a Raspberry Pi at once, with the same
password, clouds and tree. Each machine keeps its own vault file. Changes travel
through the copies of the index every cloud already holds. A machine reads the
copies it can reach, merges them into its own index, and writes the result back.
It does this at every unlock, every five minutes once it has seen another
machine, before every push, and on demand:

```bash
sand vault sync         # pull now (Vault settings › Machines › Pull now)
sand vault instances    # the machines that open this vault
```

**Joining a second machine:** on the first machine, export a
[recovery kit](recovery.md#recovery-kit). On the second, import it with *The
vault is still running on another machine — open it here too* ticked (CLI:
`sand vault kit import --second-instance`), under the vault's **own** password.
Copying the vault file across also works. The password must match because the
index copies are sealed under the key it derives. The second machine appears
under *Machines* on the first after its first pull.

**Local folders belong to their machine.** On the other machine, the account
card says **on study-pc** instead of going red, health figures leave it out, and
files whose parts are on it are **greyed out**, naming the machine that can read
them. Files with enough parts on clouds open from either machine.

**Concurrent writes are merged:**

- A file stored on one machine appears on the other.
- A file deleted on one machine disappears from the other, even if it was edited
  there since.
- The same entry changed on both keeps the later change.
- Two files of the same name uploaded to one folder are both kept, and the later
  one becomes `notes (2).txt`.

Nothing is lost to the race, because it is in that machine's vault file until
the next pull carries it over.

**What doesn't travel:**

- Account credentials. A cloud connected on one machine has to be connected by
  hand on the other.
- The inside of a sub vault. It appears shut on the other machine and opens with
  its own password. Deleting a sub vault on one machine deletes it everywhere,
  and files stored into it elsewhere since your last pull aren't counted by that
  delete.

**Cautions:**

- **Sweeps wait.** A file mid-upload on another machine has parts on the clouds
  but no index row yet. The stray-parts scan pulls first, and the sweep is
  refused until every other machine has been quiet for 75 minutes.
- **Machines are told apart by hostname.** Two machines with the same name (two
  Pis called `raspberrypi`) are warned about and have their indexes combined.
  Set `SAND_INSTANCE` on one of them.
- **Renaming a machine** makes its own disks look like another machine's. Open
  the account, then *Edit*, then *Save*, to claim the disk back.
