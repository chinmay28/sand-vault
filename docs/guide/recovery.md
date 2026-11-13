# Recovery

What to do when the vault file is gone: with a recovery kit, with only your
password and your accounts, or by hand from downloaded parts with no app
running. Also covers bringing in a vault you find on an account you reconnect,
and the risk the manifest backup carries. Internals:
[recovery-kit.md](../design/recovery-kit.md) and
[vault.md](../design/vault.md).

## What you need, and what survives

Your parts are encrypted under a random 256-bit **data key** that lives in the
vault file. Your password alone does nothing without it. So SAND Vault keeps two
kinds of backup:

| Backup | Where | Opens with | Carries |
|---|---|---|---|
| `manifest.sand` | every connected account, rewritten after index changes | your vault password | file tree, which account holds which part, the data key. **No account credentials.** |
| Recovery kit (`sand-recovery-kit-<date>.zip`) | wherever you put it; never a cloud | a 25-character recovery code (or your vault password, if you chose that) | everything above **plus every account's credentials**, colours, default clouds, film key, read history |

Which path to take:

| You have | Do this |
|---|---|
| A kit and its code | [Import the kit](#importing-a-kit). Every cloud reconnects on its own. |
| Your password and access to your accounts | [Recover from the accounts](#recovering-from-your-accounts). |
| A `manifest.sand`, some parts, your password, no network | [Rebuild a file by hand](#rebuilding-one-file-by-hand). |

## Recovery kit

### Making one

```bash
sand vault kit export                       # writes sand-recovery-kit-<date>.zip here
sand vault kit export --out /mnt/usb/kit.zip
sand vault kit export --use-vault-password  # seal under the vault password; no code
```

In the browser: **Vault settings › Recovery kit › Export a new kit**.

The **recovery code** is shown once, on its own. It is never inside the zip,
never in its filename, and never logged. The browser makes you tick *I have
written this down, or saved it somewhere other than beside the kit* before the
panel closes, and offers copy and **Save as a text file**. Keep the code and
the kit in different places, because together they are the vault.

- The vault keeps the code, so `sand vault kit code [kit-id]` (or **Show code**
  in settings) can show it again while the vault works. `--forget` drops the
  stored copy. That is tidying up, not revocation.
- `--use-vault-password` means one fewer secret, but the kit opens with
  **today's** password, not one you change to later.
- Export refuses to write the kit into a folder one of your local-folder
  accounts is connected to, because it would upload the credentials to that
  account. The browser warns about synced folders for the same reason.
- Settings shows how old the last kit is and how much has changed since.
  Export a fresh one after connecting a new account, because an old kit has no
  credentials for it.

### Testing one

```bash
sand vault kit inspect kit.zip   # which kit, when, code or password? No secret needed
sand vault kit verify kit.zip    # open it, ping every credential, compare with your accounts
```

Or use **Test this kit** in settings. Verify changes nothing. It reports how many
credentials still work, how many of the kit's files could be rebuilt today, files
added since, and accounts connected since. It asks for the code on purpose: the
failure it catches is the slip of paper that went missing, while your vault is
still alive and a new kit takes ten seconds.

### Importing a kit

On a fresh install, the lock screen has **I have a recovery kit** beside creating
a vault. Drop the zip in. The field asks for **Recovery code** or **Vault
password** depending on the kit. Typos in a code are caught by its check symbol
before any slow key derivation runs. From the CLI:

```bash
sand vault kit import kit.zip            # prompts for the code, then a new vault password
SAND_KIT_CODE=… SAND_PASSWORD=… sand vault kit import kit.zip   # unattended
```

Secrets come from a prompt, from stdin, or from these environment variables,
and never from an argument.

What happens:

1. Every account is reconnected **under the ID it had**, so every part record
   (including those inside sealed sub vaults) is already correct.
2. The kit's credentials are used to read the `manifest.sand` copies on the
   accounts, which are usually newer than the kit, and the newest index is
   adopted. `--skip-cloud-index` uses the kit's own index instead.
3. A report lists each account as connected or as needing attention. Fix each
   one with its button:

| Status | Meaning | Button |
|---|---|---|
| `needs_reauth` | sign-in expired or revoked | **SIGN IN AGAIN** |
| `needs_path` | the folder isn't at that path on this machine | **FIND IT** |
| `unreachable` | network, DNS, a bucket that's gone | retry later |
| `elsewhere` | a local disk on the machine the vault still runs on (second-instance import) | none needed |

An account that fails doesn't stop the import. The report can be reopened from
the accounts panel later.

Notes:

- The password you type is the recovered vault's **new** password. It doesn't
  have to be the old one. Exception: with `--second-instance` it must be the
  vault's own (see [vault-management.md](vault-management.md#opening-the-vault-on-several-machines)).
- If the vault password was changed after the kit was exported, the newer
  index copies on the accounts are under a key the kit doesn't have. Set
  `SAND_OLD_PASSWORD` (or type it when the browser asks) to open them.
  Skipping this costs only the files added between the export and the password
  change.
- Import refuses a vault that already holds files unless you pass `--replace`.
  Re-running an interrupted import is safe.

**If you lose control of a kit:** change the vault password (which rotates the
data key and re-encrypts every file, see
[vault-management.md](vault-management.md#changing-the-password)), let the
migration finish, and **rotate every account's credentials at the provider**.
SAND Vault can't do the last step for you.

## Recovering from your accounts

Without a kit, start a fresh vault (a new password is fine), reconnect the
accounts, and recover:

```bash
sand vault init
sand remote add gdrive --name work …     # reconnect each account
sand vault recover --dry-run             # what would come back
sand vault recover                       # asks for the lost vault's password
```

`--from ACCOUNT` picks which account's `manifest.sand` to read (the first
connected one by default). `--backup-password` or `SAND_BACKUP_PASSWORD` supplies
the lost vault's password. Reconnected accounts get new internal IDs, so recovery
asks each one what it holds and re-points the index at it. Recovery refuses to
run against a vault that already holds files. For that case, see
[Finding a vault on an account you reconnect](#finding-a-vault-on-an-account-you-reconnect).

### The browser does this without being asked

On a vault with no files, connecting an account makes the browser check whether
its `manifest.sand` was written by a different vault. If it was:

```
Sand files detected on your clouds — 412 parts (3.1 GB) and an encrypted
copy of a vault index this one did not write.        [Attempt recovery]
```

One cloud is never enough, because a file needs m of its n parts. So the dialog
asks you to **Connect another cloud** first. Each account you add is re-checked.
The second one brings up the password prompt (the **lost** vault's password),
and after the last one recovery runs on its own: it checks first, then commits
if the check is clean.

### What did not come back

Up to n − m missing clouds cost nothing. One more than that costs the file. The
report leads with the shortfall in files **and** bytes:

```
Recovered 18 of 23 file(s) in 6 folder(s) — 1.2 GB of 4.4 GB.
  2 file(s) came back with no spare part left.

Not recovered: 5 of 23 file(s), 3.2 GB of 4.4 GB.

Connect these accounts and run this again:
  onedrive-personal   onedrive   9 part(s) — 5 file(s) cannot be opened without it
  nas-backup          webdav     3 part(s) — spare parts only
```

Accounts that held only spares are listed but not blamed. In the browser,
**Connect a missing cloud** picks the recovery back up. Once the missing
accounts are connected:

```bash
sand vault recover --resume
```

`--resume` doesn't ask for a password. It re-points records at newly reachable
accounts. The browser shows the same banner with *Finish recovery*. Recovering a
second time is refused, because adopting the snapshot again would replace the
key the recovered files depend on.

### Making the recovered files yours

A recovery adopts the lost vault's data key, so the **old password** still
opens every part, including new uploads, through any old `manifest.sand`
anyone kept. `sand vault status` and a banner in the accounts panel keep
reminding you until you run:

```bash
sand vault reclaim                                      # onto the clouds they're on
sand vault reclaim --account work --account offsite --account nas
```

This mints a fresh key under your **current** password, rebuilds every file onto
it, and erases the old parts. Your password doesn't change. Since every file
moves anyway, `--account` is the cheap moment to choose new clouds. It costs a
download and an upload of the whole vault. It is safe to stop, and
`sand vault migrate` finishes it.

## Finding a vault on an account you reconnect

Every vault leaves its index on each account it used. Connecting such an account
to a different vault is detected:

```bash
sand sub scan                    # which accounts hold another vault's index
sand sub import old-dropbox      # bring it in as a sub vault
sand sub forget old-dropbox      # erase that index from the account
```

Use this when `sand vault recover` refuses because your vault already holds
files. The found vault lands **beside** yours as a sub vault instead of
replacing it. You are asked for two passwords: the old vault's, and the one the
sub vault will use from now on. The old data key is adopted, so nothing is
re-encrypted. Run `sand sub passwd <name>` afterwards to rotate it, which is what
finally makes the old password useless. `--name` sets the sub vault's name.
`--keep-backup` leaves the old index on the account, which also stops this
vault backing up there. Files on accounts you haven't connected come in anyway,
and are counted.

In the browser, found vaults are listed with **Import** and **🗑** (forget).
Leaving an unwanted one isn't neutral: the scan keeps reporting it, and this
vault won't write its own index to that account. Forgetting erases only that
account's copy of the index. The old vault's parts stay, and become ordinary
[stray parts](maintenance.md#a-part-no-file-wants-sweep) you can sweep.

## Rebuilding one file by hand

No vault, no accounts and no network are needed, but you do need the
`manifest.sand`: parts are sealed under the data key, and the manifest carries
it. Any account's copy will do.

1. Download `manifest.sand` from any account.
2. Read it:

   ```bash
   sand manifest ls manifest.sand --long
   ```
   ```
   Backup written 2026-08-13 05:20 — 2 file(s), 3 account(s), strict placement

   /finance/2026/ledger.csv                   263.8 KB
       part 1  cloud-a              7d4206c8…-p1.sand
       part 2  cloud-b              7d4206c8…-p2.sand
       part 3  cloud-c              7d4206c8…-p3.sand
   ```

3. Download m different parts of the file (two for 2-of-3) from different
   accounts.
4. Rebuild:

   ```bash
   sand restore --parts 7d4206c8…-p1.sand,7d4206c8…-p3.sand \
                --manifest manifest.sand --preserve-tree --output-dir ./rescued
   ```

- It prompts for the **vault password** of the vault that wrote the manifest.
  Use `SAND_PASSWORD` or stdin to run unattended. `--password` is refused
  together with `--manifest`.
- `--preserve-tree` recreates the file's folders under `--output-dir`.
- The number of parts needed is read from the parts themselves. Too few, and it
  says how many it wanted.
- **Chunked files** (anything stored since chunking, 16 MB chunks by default)
  are one object per chunk per part: `<id>-c0000000-p1.sand`,
  `<id>-c0000001-p1.sand`, and so on. The manifest lists chunk zero. Download
  every chunk for two parts and pass them all:

  ```bash
  sand restore --parts "$(ls 7d4206c8*-p1.sand 7d4206c8*-p3.sand | paste -sd,)" \
               --manifest manifest.sand --output-dir ./rescued
  ```

  Missing chunks are refused, and the error names how many the archive has.
- Without `--manifest`, `restore` says that the parts were written by a vault
  and need it. Parts made by `sand archive` (standalone mode) are sealed under
  the password typed at the time and have no manifest. `restore` tells the two
  apart on its own.

## The tradeoff

Every account holds a copy of `manifest.sand`, one password away from the data
key. If an account is compromised **and** your password is guessed, the
attacker gets your file tree, the placement map, and what that account's own
parts contain (about 1/m of each file, so half at 2-of-3). Rebuilding whole files
still takes m − 1 more accounts, so the m-of-n split remains a real second
factor.

The `redundant` policy with fewer than three accounts removes that factor,
because one account can hold enough parts to rebuild a file. In that case the
backup is refused and any existing copies are erased.

To have neither the backup nor its risk:

```bash
sand vault backup --disable    # erases every copy from every account
sand vault backup --enable     # turn it back on
```

Then back up `~/.sand/vault.sand` yourself, or keep a recovery kit. Without
either, losing the vault file loses everything. `sand vault backup --force`
overwrites a copy written by another vault or under an older password.
