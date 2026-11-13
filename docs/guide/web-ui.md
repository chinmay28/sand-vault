# The web UI

`sand serve` puts a file browser at `http://<host>:8123`. This page is a tour of
it: the sidebar of accounts, the folder toolbar, what a row can do, the
previews, and how the layout changes on a phone. Each feature that has a page of
its own is linked from here.

Nothing can be listed or fetched until the vault is unlocked: the first screen
is the lock screen, which also creates a new vault or recovers one (see
[Recovery](recovery.md)).

```
┌─ header ───────────────────────────────────────────────────────────────┐
│ ☰  SAND Vault                         ✦ Sandy   ⟳ Refresh   🔒 Lock vault│
├─ sidebar (folded) ─┬─ toolbar ─────────────────────────────────────────┤
│ Connected clouds   │ ◀ ▶ ↑  / films / 2024   🎬 🗂  ▦ ⇅ ✓              │
│  account cards     │ ⌕ search…        + Folder  ⇅ Machine  ↑ Upload    │
│  ● 1 of 17 unhealthy├─ listing ───────────────────────────────────────┤
│ ⚙️ Vault settings   │ name            size   modified   ①②③   🎬▶↓→✕⋯ │
│ 📊 Statistics       │ …                                                │
└────────────────────┴──────────────────────────────────────────────────┘
```

## Header

| Control | What it does |
|---|---|
| `☰` | Brings out the sidebar of connected clouds, and folds it again |
| `✦ Sandy` | Opens Sandy, the archivist assistant; see [Sandy](sandy.md) |
| `⟳ Refresh` | Reloads the listing and the accounts |
| `🔒 Lock vault` | Locks now, without waiting for the idle timeout |

## The sidebar: your accounts

The sidebar starts folded at every window size, because the file browser is
what the app is for. `☰` brings it out; `☰` again, or `✕` on the panel, folds it.

- **Account cards.** Every connected account with its live status, how many
  parts it holds, and a usage bar split into Sand Vault's parts, everything
  else on the account, and the room left. A local folder reports the drive it
  sits on. Room left comes from the account itself, from a capacity you
  declared, or from a quota you set; see [Accounts](accounts.md).
- **Stats** on a card opens that account's breakdown: capacity against Sand
  Vault's share, what its parts belong to by kind and by folder, the month they
  arrived, the heaviest files, old versions, and how many files could not be
  rebuilt without this account.
- **Edit** on a card has two tabs. *How it looks* sets the name, the colour the
  account wears everywhere, and its capacity and quota, none of which touch its
  credentials or its parts. *How it connects* takes rotated keys, a moved
  bucket, or a trip back through the provider's sign-in, and repairs an account
  that has gone red without disconnecting it.
- **+ Connect a cloud.** The form is generated from each backend's own field
  spec, so a new backend appears without frontend changes.
- **Cloud health**, at the foot: a line such as `● 1 of 17 unhealthy` and when
  the accounts were last asked. Pressing it lists every account worst-first,
  with what the failing ones said and for how long, a **Check now**, and the
  schedule (`15 min`, `Hourly`, `6 hours`, `Daily`, `Off`). It is a ping and
  nothing else. See [Accounts](accounts.md).

Two tiles sit under the cards:

- **⚙️ Vault settings**: everything that belongs to the vault rather than to an
  account or a folder. Its rows are Default clouds, Replication, Cloud health,
  Index backup, Machines, Password, Recovery kit, Sub vaults, Film key, Sandy,
  This machine, Mount as a drive (only when the share is on), Stray parts and
  Lost parts. The pages that cover them:
  [Storing files](storing-files.md), [Maintenance](maintenance.md),
  [Vault management](vault-management.md), [Recovery](recovery.md),
  [Films](films.md), [Streaming](streaming.md#mount-it-as-a-drive),
  [Configuration](../reference/configuration.md).
- **📊 Statistics**: four panels.

### Statistics

| Panel | What it shows |
|---|---|
| **Read race** | Opening a file is a race between the accounts holding its shards, and the losers are never mentioned anywhere else. This shows each account's share of the shards actually used, how long its answers take (average, with the quickest-to-slowest spread), and what became of every shard it was asked for: won, too late, cut off, or failed. A sparkline per account shows whether its share is drifting. |
| **Transfers** | Bytes sent in to each cloud and fetched out of it, whatever moved them (uploads, reads, moves, conversions, the index backup), with a traffic chart and a row per cloud with objects moved and failed calls. |
| **Throughput** | Speeds in Mbps: opening and storing files, the same again by kind of file, and each cloud's own upload and download speed. Every figure is total bytes over total time, so a thousand small files cannot outvote one large one. |
| **Storage breakdown** | A pie of the capacity of every cloud that knows its size, split into Sand Vault's parts, other files, free space and space out of reach, with how many bytes are stored per byte in the vault. Drawn from figures the sidebar already has, so it asks nothing of any cloud. |

Read race, Transfers and Throughput have the same four tabs: **Today**,
**Month**, **Year** and **All time**. The figures are kept by the day in a file
beside the vault, sealed under a key derived from the vault's, so they survive a
restart and can be read only while the vault is open. **Forget history** erases
them.

## The folder toolbar

| Control | What it does |
|---|---|
| `◀` `▶` `↑` | Back, Forward and Up through the folders you have walked. Also `Alt+←`, `Alt+→`, `Alt+↑`. The trail is held in memory and goes when the vault locks. |
| Breadcrumbs | Jump to any folder above this one |
| `🎬` | Film details for this folder and everything under it; lit when they are on. See [Films](films.md). |
| `🗂` | **Organize and automate**: tidying, duplicates, corruption checks, checksums, snapshots, and the folder's standing instructions. It lights when the folder has a maintenance policy and turns amber when the last run found something. See [Folder tools](folder-tools.md). |
| `▦` / `▤` | Grid or list |
| `⇅` | Sort by name, size, date or kind. Each opens in the direction it is usually wanted and reverses when chosen again; folders always lead. The view and the sort are remembered. |
| `✓` | Selection mode |
| Search | Finds a file or folder anywhere in the vault, each hit shown with its folder. Searching inside a folder looks there first and offers to widen to the whole vault. |
| `+ Folder` | New folder here |
| `⇅ Machine` | Bring files in from a machine you have an SSH login on, or send files to one |
| `↑ Upload` | Upload files, or a whole folder (from the picker or by dropping it). Everything under a folder goes up keeping its shape, in batches, so files appear as they land and a file that fails takes down only its own batch. |

**List or grid.** The grid is built round each file's stored thumbnail, for the
folder of photographs whose names say nothing. Tiles are square, or two-by-three
posters in a folder with film details on.

### Selecting

Tick rows one at a time, a run with `Shift`, or everything with `Ctrl+A`
(`⌘A`). The selection bar then offers:

| Button | Effect |
|---|---|
| `↓ Download` | The selected files, rebuilt and decrypted |
| `→ Folder` | Move into another folder. Only the index changes. |
| `⇄ Clouds` | Move the parts onto other accounts, priced as one number before it starts |
| `🔒 Into a sub vault` / `🔒 Take out` | Only when a sub vault is open; see [Vault management](vault-management.md) |
| `✕ Delete` | Erase every part, everywhere |

*Select files by type* in the `🗂` menu can add files from folders below; the bar
then says *"N from folders below"* and every button acts on them too.

## Rows

Each row shows its name, size, modified date and **part badges** (`①②③`, coloured
per account). Clicking the badges opens a live per-part health read-out, with
the file's replica checked beside it when it has one.

A file row carries `🎬` (in a films folder), `▶` (video and audio), `↓`, `→`
and `✕`. A folder row carries `🖼` (when there is a picture to pick), `→`, `⇄`
and `✕`. Every row also has `⋯`, which opens the full menu, the same sheet a
phone gets.

**A file's menu:**

| Item | Notes |
|---|---|
| Open | Rebuild and preview it |
| Stream in VLC | Video and audio; see [Streaming](streaming.md#stream-a-file-to-vlc) |
| Copy the address | A stream link any player or app can open |
| Download | The rebuilt, decrypted file |
| Send to a machine | Copy it, in the clear, onto a machine you have an SSH login on |
| Film details / Look up the film | Only in a folder with film details on |
| Where the shards live | The per-part read-out |
| Replicate this file / Stop replicating / Drop the replica | See [Storing files](storing-files.md) |
| Rename, Move to another folder | Only the index changes |
| Move to other clouds | Only the parts that have to move are copied |
| Rebuild from upstream | Only on a kept git repository; see [Repositories](repositories.md) |
| Move into / out of a sub vault | Only when one is open |
| Delete | Erases every part, everywhere |

A file stored in the old whole-file format shows **Convert to chunks** in place of
Open; it cannot be opened or streamed until converted.

**A folder's menu** leads with what the folder holds: files and bytes at every
depth, how many clouds the parts went to, what they weigh across the accounts,
how many folders are inside, and when the newest changed (see
[Folder tools](folder-tools.md#what-a-folder-is-holding)). Then:

| Item | Notes |
|---|---|
| Open folder | |
| Download as zip | The whole folder as one archive, built as it downloads |
| Watch on TV | A four-character code for a TV; see [Sand TV](sand-tv.md) |
| Send to a machine | The folder, whole and in the clear, onto an SSH machine |
| Replicate this folder / Stop replicating | See [Storing files](storing-files.md) |
| Folder picture | See [Films](films.md#folder-pictures) |
| Rename, Move to another folder, Move to other clouds | Everything inside comes along |
| Move into / out of a sub vault | Only when one is open |
| Delete folder | Erases everything inside it, everywhere |

**Download as zip** gathers each file from the clouds and writes it straight into
the download, so a folder far bigger than the server's memory still leaves it. The
archive is stored rather than compressed, since the files already were. The
dialog also shows the archive's address, which carries its own credential and is
good for twelve hours (or until the vault locks), for a download manager or
`curl -O` on another machine.

## Previews

- **Images, video, audio, PDF and text** open inline, rebuilt on demand.
  Anything else downloads. Pictures open full-screen from the preview.
- **A matched film** opens on its poster and summary, with **▶ Play** to play it
  in the page and **▶ Stream in VLC** beside it.
- **PDFs** are drawn by the app itself rather than the browser's viewer, so a
  phone shows the document instead of the blank frame iOS Safari makes of one.
  One page at a time (arrow keys or `‹ ›`), with zoom. Only the pages you look at
  are fetched.
- **HEIC** opens in every browser: where the browser cannot decode it, the page
  does (libheif compiled to WebAssembly, the `heic-to` package), loaded from the
  vault's own origin only the first time it is needed. The stored file and its
  download stay the original HEIC.
- **A picture that will not draw** gets checked: the preview checks that one file
  and says whether it is intact or what is wrong with it.

**Thumbnails.** Images and PDFs get a picture instead of an icon (a PDF's is its
first page). It is made in the browser at upload and stored like any other data,
encrypted and split across your accounts, one small pack per folder. A video gets
one from a frame while you watch it, since nothing can picture a film at upload
time without decoding it. A film in a films folder gets its poster.

## Auto-lock

The vault re-locks after the idle timeout (`--idle-timeout`). A request that is
still running, such as an evening-long folder upload, a stream, or a zip
download, counts as use for as long as it runs. The poll a tab makes every
minute does not, so a tab left open does not keep the vault unlocked. While an
upload is going, the page asks the system to keep the screen on.

## What the page loads

The UI loads no external fonts, scripts or styles, so opening your vault makes no
third-party requests. That includes a folder with film details on: the posters
and summaries are stored in the vault and served by your own server. The only
request that leaves is the film lookup itself, made by the server, for a folder
you turned on, when you ask.

The wordmark's *Vault* is the one typeface that is not the system's: a 3.4 KB,
five-glyph subset of Caveat (SIL Open Font License), embedded in the page as a
`data:` URI. If you drop in a licensed copy, **Nefelibata Script** is used ahead
of it. See [`web/fonts/README.md`](../../web/fonts/README.md).

Asset filenames carry a content hash and are cached for a year. `index.html`
revalidates on every load against an ETag, so an upgrade reaches a browser the
next time it opens the page, and costs nothing when nothing changed.

## On a phone

A vault you reach over Tailscale or a reverse proxy is opened from a phone as
often as from a desk, so under **860px** wide the layout changes shape rather
than shrinking:

- **The sidebar** becomes a drawer over the file list.
- **The toolbar** becomes a heading. `◀` Back, then the folder's name with a
  summary line (`23 files · 1.4 GB · 3 clouds`), then `⇅` (machines) and
  `↑ Upload`. Tapping the name drops the folder trail as a list, with Forward
  when there is somewhere to go. Under the heading is a strip with search, view,
  sort, select, `🎬`, `🗂` and new folder. Search takes over the strip and keeps
  the folder named above it.
- **Rows** drop their columns for stacked rows: thumbnail or icon on the left,
  the name, and size, date and part badges beneath. Each row has a single `⋯`.
  Its choices open in a bottom sheet with their names spelled out and the
  destructive one set apart at the end. Sorting uses the same sheet.
- **Deleting** asks again in the app's own dialog rather than the browser's
  `confirm()`.
- **Every control** is at least 44px, the smallest target Apple and Google both
  publish. Sizes come from the layout rather than a `pointer: coarse` rule, so a
  narrow desktop window behaves the same way.
- Heights are measured against the visible viewport, so a collapsing address bar
  never hides the last row.

**Add to Home Screen** installs the vault under its own icon (named "SAND Vault"
in `manifest.json`). It opens without browser chrome on iOS, and on Android
wherever the browser honours the manifest. The server serves
`apple-touch-icon.png`, `manifest.json` and 192/512px icons, including a
maskable one. All of them are drawn from
[`web/public/icon.svg`](../../web/public/icon.svg) by
[`scripts/make-icons.mjs`](../../scripts/make-icons.mjs): change the SVG, run
`make icons`, and commit the PNGs.

The shortcut is a link to your server, not a copy of anything. Nothing is cached
offline, so a stolen phone with the icon on it gets the password prompt.
