# Folder tools

`🗂` in the toolbar (**Organize and automate**) acts on the folder you are
standing in and everything under it. This page covers its entries: six that tidy
the tree, three that check whether files still come back right, and two for
snapshots. It also covers the figures a folder's own menu shows. The two
standing instructions at the bottom of the menu have pages of their own:

| Entry | Covered in |
|---|---|
| Flatten into this folder, File into folders by date, Remove empty folders, Remove files by type, Select files by type, Find duplicates | [Tidying a folder](#tidying-a-folder), [Finding duplicates](#finding-duplicates) |
| Find and fix corrupt files | [Finding corrupt files](#finding-corrupt-files-and-fixing-them) |
| Check files against their checksums, Calculate missing checksums | [Checksums](#checksums-and-the-ledger) |
| Take a snapshot, Restore a snapshot | [Snapshots](#snapshots-of-a-folder) |
| Look after this folder | [Maintenance](maintenance.md) |
| Repositories kept here | [Repositories](repositories.md) |

The `🗂` button shows the standing state without being opened: lit when the
folder has a maintenance policy, amber when the last run found something.

How these work inside is in [Index design](../design/index.md) and
[Cryptography](../design/cryptography.md).

## Tidying a folder

Each of these counts what it would do before it offers a button to do it, since
none of them acts on rows you picked one by one.

| Entry | What it does |
|---|---|
| **Flatten into this folder** | Every file below comes up to here. Name collisions are planned before anything moves: numbered the way a desktop file manager numbers them, or, with **Name each file after the folders it came from**, `2023/corfu/IMG_0001.jpg` becomes `2023 - corfu - IMG_0001.jpg`. **Show all N moves** lists every `old → new` with the renamed ones highlighted. **Remove the folders left behind** (on by default) drops the emptied folders afterwards. |
| **File into folders by date** | The reverse of a flatten, for a camera roll or ten years of statements: files go into `2026/January` (or just `2026`) by the date each was last modified, read in your local time. **This folder** files only what is loose here; **Everything under it** takes the lot and then offers to remove what the moves emptied. |
| **Remove empty folders** | Every folder under here that holds no file at any depth, deepest first. Each is removed on its own, never recursively, so a folder that turns out to hold something is refused rather than emptied. |
| **Remove files by type** | Pick the kinds (`.nfo`, `.txt`…, each with a count and size) and erase them, through the same confirmation the delete button uses. |
| **Select files by type** | Pick kinds and select them instead. Files deeper down join the selection as *"N from folders below"*, and every selection button acts on them. Selecting every `.srt` under a films folder and moving it to `/Subtitles` is two clicks and a folder pick. |
| **Find duplicates** | See [below](#finding-duplicates). |

**Filing by date is safe to press twice.** A file already in the folder its date
names is left where it is, so a second press has nothing to do, and a run that
stopped partway is finished by pressing it again. Files with no modified date are
counted and left alone. Destination names are planned first, so nothing lands on
an existing file.

**Nothing here touches a cloud account except deleting.** A file's parts are named
after the file, not its folder, so flattening four hundred films is an index
rewrite and as fast as a rename.

**A run that stalls tells you where.** Each tool reads the folder once
(`GET /api/folders/survey`, or `GET /api/folders/date-sort` for dates; both walk
the index and contact nothing). The browser then runs the moves one at a time
through the ordinary endpoints. A run that fails on the fortieth of two hundred
has moved thirty-nine and names the one that refused, and running the tool again
picks up exactly what is left.

## What a folder is holding

A folder's row cannot show its size, because the size lives in the levels below.
So the folder's `⋯` menu opens with the figures:

```
Movies                                    5      28.1 MB          3
Folder                                  FILES     IN HERE     CLOUDS
42.2 MB across the clouds · 3 folders inside · newest Aug 23
```

- **In here** is what `du -sh` would say: everything at or below the folder.
- **Across the clouds** is what that costs on your accounts, which is larger. A
  file cut two-of-three is stored one and a half times over.
- **Clouds** is how many accounts hold a part of something under here. Hover for
  their names.
- A folder with a file short a spare part says so on the same line.

The figures (`GET /api/folders/stats`) are fetched when the menu opens, not with
the listing. They walk the index and contact no account. If they cannot be
fetched, the menu opens without them.

## Finding duplicates

Copies of things never show in a listing, because they are never side by side.
**Find duplicates** asks four questions at once, at four degrees of certainty:

| Tab | Groups files that… | How sure |
|---|---|---|
| **Identical** | share a SHA-256 | Proof: one hash is one file |
| **Same size** | have the same number of bytes | Weakest. Each group says whether the hashes back it up. |
| **Similar names** | differ by a copy marker, a separator or a typo | Can be wrong |
| **Same film** | are one film under unrelated names (only offered where there are videos) | A resemblance: two encodes are different bytes |

**Similar names** reduces each name to what a copy would share:

- Copy marks come off the end, repeatedly: `(1)`, `[2]`, `- Copy`, `copy 2`, so
  `report (1) (2).pdf` matches `report.pdf`. A file actually called `copy.txt`
  keeps its name.
- Case and separators are ignored: `Vacation Photo.jpg`, `vacation-photo.jpg`
  and `VACATION_PHOTO.jpg` are one name.
- **Digits must match exactly.** `IMG_0001.jpg` and `IMG_0002.jpg` are two
  photos, not copies; letters may drift by an edit or two, digits may not.
- Extensions are never crossed, so a film is never grouped with its subtitles.
- A cluster that is alike in bulk (a naming scheme, where every name is one edit
  from a dozen others) is broken back into exact matches, and the dialog says so.

**Same film** uses what the vault already has and looks nothing up:

- Files matched against the film database (see [Films](films.md)) with the same
  film ID are one film. A matched file also answers to its stored title, so it
  can meet an unmatched copy.
- An unmatched file is read the way the lookup would read it: title and year off
  the name, with tags, dots and a leading *The* removed. A name with no year
  borrows one when the same title appears with exactly one year, so `Dune.mkv`
  beside `Dune.2021.mkv` is one film. If `Dune.1984.mkv` is there too, it is
  ambiguous and left out.
- A file that cannot name itself (`Blade Runner (1982)/title00.mkv`) borrows its
  folder's name, but only when the folder reads as a film.
- It crosses extensions (`.mkv` and `.mp4` of one film), but only between videos.

**Choosing what goes.** Each group starts with one copy spared: the shallowest
with the plainest name, or in a film group the **largest**, since that is usually
the better encode. Every row is its own tick. **Keep one** resets a group and
**Skip** drops it. A group with every copy ticked is allowed and called out,
because it is the one way this dialog could remove the last copy of something.
Ticks persist across the four tabs.

Then **Delete** sends the ticked files to the usual delete confirmation, or
**Select** selects them in the browser. Selecting is often the better answer: a
copy you are unsure of can be moved to another folder instead.

It reads `GET /api/folders/duplicates`, which walks the index in memory, decrypts
nothing and contacts no account. All four answers come from that one walk.

## Finding corrupt files, and fixing them

The health badge and a folder's scheduled sweep ask each cloud whether an object
exists, but neither downloads it, so neither can tell a part that is present from
one that is present and wrong. **Find and fix corrupt files** reads everything.

Tick the kinds to check (pictures, documents and video are ticked to begin with).
The dialog shows how many files and bytes that is. Then, for each file, the
server:

1. fetches **every** shard of every chunk from every cloud, not only the ones a
   download needs, and checks each is there and is the part it is recorded as;
2. rebuilds each chunk and checks the shards it did not need agree with those it
   did;
3. checks the whole file against the SHA-256 taken when it was stored;
4. checks the result as the kind of file it is: JPEG, PNG, GIF, WebP/AVI/WAV,
   PDF, MP4/MOV/HEIC/AVIF, MKV/WebM, ZIP-based formats (`.docx`, `.xlsx`, `.epub`…),
   TIFF/raw and BMP each have a structural check.

| Result | Meaning | Fix |
|---|---|---|
| **Reads, with bad parts** | Some shards are missing or damaged; the rest rebuild the file correctly | Rebuilds a fresh full set, then erases the bad parts |
| **Cannot be read** / **Does not match what was stored** | Too few good shards, or they rebuild to something else | From its replica, if it has one (see [Storing files](storing-files.md)) |
| **Was damaged before it was stored** | Every shard is right and the hash matches; the file was already broken when uploaded | None in the vault. Upload a good original over it. |
| **Could not be fully checked** | A cloud did not answer; what could be read is fine | Check again later |

Each finding names the part, the cloud holding it, and what is wrong. Checking
changes nothing. **Fix** is a separate step over exactly the files listed. The job
runs on the server, so you can close and reopen the dialog, and stop it to keep
the findings so far.

It downloads everything chosen, about 1.5× its size at the default 2-of-3 scheme,
so point it at a folder rather than a whole large vault. From a terminal, the
same check is:

```bash
sand check --deep <file-or-folder> [--fix]
```

## Checksums, and the ledger

Every file has a checksum: the SHA-256 of its contents. **`checksums.gz`** at the
root of the vault lists them all, one line per file:

```
$ zcat checksums.gz
/photos/2024/hike.jpg, 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
/taxes/2023, final.pdf, e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
```

That is the full path, a comma and a space, then the checksum. A comma in a name
needs no quoting, because the line splits at the last `, `. A newline, carriage
return or backslash in a name is written `\n`, `\r` or `\\`. The ledger is an
ordinary file in the vault on the default clouds, so you can download it and
check a file with `sha256sum` alone.

It is rewritten when the index has changed: every two minutes while unlocked by
default (`sand serve --checksums-every`; `0` writes it only when asked). It is
left alone when nothing changed. The name is reserved, so your own
`/checksums.gz` would be replaced. A sub vault has its own ledger at its own root.

Two entries in the `🗂` menu use the checksums:

| Entry | What it does |
|---|---|
| **Check files against their checksums** | Reads each file back as a download would and compares. The quick corruption check: about 1× the folder's size, against 1.5× for *Find and fix corrupt files*. It tells you *that* a file is wrong, not which part. |
| **Calculate missing checksums** | For files that have none, such as ones recovered after the index was lost or stored by a very old build. Reads them, records the checksum, and rewrites the ledger. On a normal vault it has nothing to do and says so. |

Both run in the background on the server; closing the dialog stops nothing. You
pick a speed before starting and can change it while it runs. **Pause** holds it
at the current file; **Stop** ends it and keeps what was done. Only one runs at a
time. A paused job does not keep an idle vault from locking, and locking ends it.
A job started inside a sub vault stays inside it, and locking that sub vault stops
the job there.

A file with no checksum also gets one the first time it is read start to finish,
by a download or a preview.

From a terminal:

```bash
sand checksums write              # bring checksums.gz up to date now
sand checksums verify /photos     # check files against their checksums
sand checksums compute /photos    # calculate the missing ones
sand checksums verify --limit 50  # read no faster than 50 MB/s
```

## Snapshots of a folder

**Take a snapshot** packs the folder and everything under it into one archive and
keeps it somewhere you choose. **Restore a snapshot** unpacks one into the folder
you are in.

A snapshot is:

- **Highly compressed:** one zstd stream over the whole tree at the library's best
  level, so what similar files share is stored once. Media that is already
  compressed will not shrink; documents and source trees will, a lot.
- **Encrypted under its own password:** Argon2id (the recovery kit's costs) makes
  the key, and the archive is sealed in AES-256-GCM frames. SAND Vault does not
  keep the password, so restoring asks for it again. A wrong password is reported
  before anything is unpacked.
- **Kept in one place:** one connected cloud, or a machine you have an SSH login
  on, in a folder you name there.
- **Split past a size you choose:**

  ```
  snapshot-20261111T192233Z-ab12cd34.sandsnap                one file
  snapshot-20261111T192233Z-ab12cd34.part001.sandsnap        the first of several
  snapshot-20261111T192233Z-ab12cd34.part002.sandsnap
  snapshot-20261111T192233Z-ab12cd34.part003.last.sandsnap   the last, which says so
  ```

  On a machine a part can be any size, or unlimited. On a cloud a part is held in
  memory as one object, so it is at most **256 MB** (64 MB by default).

The name records when the snapshot was taken and nothing else. A listing tells a
finished snapshot from an interrupted one by the part marked `.last`. An
incomplete one is shown as such and can be erased but not restored.

> [!IMPORTANT]
> **A snapshot is the one thing SAND Vault stores whole.** Every other file is cut
> into parts that mean nothing alone. A snapshot is the entire folder on a single
> account, protected only by its password. That is the point: it can be restored
> with its parts and its password and *no vault at all*. It is also the reason to
> give it a long password.

**What it holds:** every folder (empty ones too) and every file with its bytes,
modified time and checksum. **What it leaves out:** thumbnails, film details,
which clouds held the parts, and folder schedules.

**Restoring** puts the contents *in* the current folder: a snapshot of `/photos`
restored into `/photos` puts everything back where it was, and restored into
`/old` gives `/old/2019/…`. Each file arrives as an ordinary upload onto this
vault's clouds. A file already there with the same contents is skipped, so a
restore is safe to repeat, and repeating it is how a stopped one is resumed. A
file with the same name but different contents is left alone and listed, unless
**Replace files that differ** is ticked. Nothing is ever stored beside it under a
numbered name.

Both run in the background on the server, one at a time. Stopping a snapshot, or
hitting a file that cannot be read, erases what had been written of it, because a
snapshot missing a file is worse than none. Stopping a restore keeps the files
that had arrived.

From a terminal (the password is prompted for, or read from
`SAND_SNAPSHOT_PASSWORD`):

```bash
sand snapshot take /photos --cloud r2-cold --max-size 200M
sand snapshot take /photos --machine nas --dir backups/sand --max-size 4G
sand snapshot ls --machine nas --dir backups/sand
sand snapshot restore snapshot-20261111T192233Z-ab12cd34 /photos --machine nas --dir backups/sand
sand snapshot rm snapshot-20261111T192233Z-ab12cd34 --machine nas --dir backups/sand
```

`restore --replace` is the CLI's **Replace files that differ**. Every flag is in
the [CLI reference](../reference/cli.md).
