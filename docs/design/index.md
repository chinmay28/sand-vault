# Index-level features

Everything on this page is a property of the decrypted index (`Manifest`, in
`internal/vault`): search, how accounts are named and edited, film details,
folder pictures, the organizer, the list of files that went out short, folder
automation, and git repositories. Most of it contacts no account at all. For the
vault file, keys, backup and recovery underneath, see [vault.md](vault.md).

Two rules recur:

- **What describes files is index, and lives in the manifest.** It is sealed with
  the rest of the index, travels in every `manifest.sand` backup, merges between
  machines, and is carried by a rename or a folder move. Per-folder records
  (`MovieFolders`, `FolderArt`, `Automations`, the thumbnail packs) are rekeyed
  by `moveFolder` and dropped when their folder goes.
- **What is a credential for someone else lives in the `settings` section**, sealed
  in the vault file and never replicated to an account.

## Search

Every store could answer "what do you hold?" only with opaque part names: names
and folders exist only in the manifest, and the manifest is only readable while
the vault is unlocked. So search is a scan of the decrypted index in memory, and
locking takes it away with everything else. It is also cheap: a vault holds
thousands of entries, not millions, and `Vault.Search` (`search.go`) walks them
under the same read lock a listing takes.

- A bare query is a case-insensitive substring of the **name**; `*` and `?` make
  it a wildcard; a query containing `/` matches the full **path**, anchored at a
  segment boundary.
- Folders are results too, including implicit ones that exist only because
  something is stored beneath them.
- Hits are ranked exact name, then prefix, then substring, then shallower first,
  and the cap is applied *after* ranking, so truncation keeps the best matches.
- A file hit carries its whole entry, so a result row draws size, placement and
  redundancy without a second round trip.

`GET /api/search`; `sand find <query> [--type all|file|folder]`.

## An account's name and colour

`provider.Config` carries two fields the backend has no opinion about: `Name`,
the label it answers to, and `Color`, the shade it wears in the browser (card
stripe, part badges, cloud picker). `Vault.UpdateProvider` is the only writer,
and editing them opens no connection to the account.

- **A colour is validated, not trusted.** `provider.NormalizeColor` accepts
  `#rgb`, `#rrggbb` and either without `#`, and stores lower-case `#rrggbb`, so
  nothing downstream compares two spellings. Anything else is refused, because the
  value ends up in a style the browser paints. Empty means *no choice*: the browser
  assigns one from its palette, skipping colours other accounts have chosen.
- **A rename travels through the manifest.** Every `Shard` records `ProviderName`
  beside `ProviderID`; the file list, health read-out and recovery reports read
  it. So the config, the shard records and the vault file change in one write
  under one lock.
- **Names are unique, case-insensitively**, whether set on connect or edited, since
  `--accounts a,b,c` and every CLI lookup resolve by name.

## Account settings: the edit that does reach the account

`ProviderEdit.Options` is what the account connects with (keys, secrets, the
bucket or folder), so an edit is checked against the backend before it is
stored: the merged config is built into a live provider and pinged, and only a
successful ping reaches `persistLocked`. The cached live provider is then
replaced. This is what repairs an account whose credentials expired. The only
alternative was disconnect and reconnect, which mints a new ID and orphans every
shard record naming the old one.

- **A secret is merged, not replaced.** `Config.Redacted` substitutes
  `provider.RedactedSecret` for every secret on its way to the API, so the browser
  never holds a real one. `provider.MergeOptions` reads the placeholder as
  *unchanged*; anything else, including the empty string, is taken literally.
  Undeclared keys are refused, and required fields left blank are refused before
  anything is built.
- **Signing back in is the same write.** `POST /api/providers/oauth/reauthorize`
  spends a finished sign-in on an existing account, ending in `UpdateProvider`
  rather than `AddProvider`. A flow started for a `provider_id` can only be spent
  on that account, and its kind must match, so a Dropbox sign-in cannot overwrite
  a Google Drive account. The OAuth app credentials come from the account's own
  config (`Vault.ProviderSignIn`), so the client secret never leaves the vault.
- **Settings are not a move.** Pointing an account at another bucket or folder
  relocates nothing; its shard records now point at objects that are not there,
  which the health check reports.

`PATCH /api/providers/{id}`; `sand remote edit`. Provider specs and OAuth are in
[providers.md](providers.md).

## Film details

A video's file name is a poor description of the film in it, so a folder can be
told its videos are films and matched against The Movie Database (TMDb). The
code is `internal/movie` (name parsing and the TMDb client) and
`internal/vault/movies.go`.

**Off until a folder asks, by name.** Every other request SAND Vault makes goes to
an account the user connected. A lookup sends a title guessed from a file name,
this machine's address and the user's own API key to a third party. So the
switch is per folder, kept in `Manifest.MovieFolders` and inherited downwards (a
library has folders inside it); `movieLookupLocked` walks up to the nearest
setting. The server checks it against the index on every lookup rather than
trusting the request, and turning it on looks nothing up: the sweep is a separate
call (`POST /api/movies/lookup` sets the switch, `POST /api/movies/scan` sweeps).

**Each piece stored where its sensitivity says.**

| What | Where | Why |
|---|---|---|
| Title, year, summary, runtime, genres, score, director, cast | `Manifest.Movies`, keyed by file ID | Index: sealed, backed up, carried by renames for free |
| Poster | The file's **thumbnail**, in its folder's thumbnail pack | Encrypted, split and placed like every other picture; the browser still fetches every pixel from the local server |
| API key | The vault file's `settings` section | A credential for someone else's service; never in the manifest that is replicated to every account |

The browser puts the key with the vault's own settings; a folder's film dialog
only reports whether one is set.

**A sweep is resumable and idempotent.** Each film is committed when matched, so a
timeout or rate limit costs the film in flight. A film that already has details
is skipped without a request, and one corrected by hand (`Manual`) is never
overwritten, even by a refresh. Posters are written once per folder through
`Vault.SetThumbs`, because a pack is a single stored object and writing it per
film would be quadratic in the folder size. A password change drops every
thumbnail; a poster has no local source, so `movie.Info` keeps the artwork's path
at TMDb and a re-sweep restores it with one image fetch and no search.

**Name parsing is a guess by construction** (`movie/name.go`, the same one media
servers make): cut at the year if there is one, else at the first encoding tag,
and fall back to the folder name for a disc track. So `movie.Info` records the
query it matched on, a match can be corrected from a candidate list
(`GET /api/files/{id}/movie/candidates`), and a corrected match is marked
`Manual`.

## Folder pictures

A folder can wear a picture of something inside it, and it is done by storing
nothing: `Manifest.FolderArt` maps a folder path to the **ID of a file that
already has a thumbnail**, and the browser draws it through
`/api/files/{id}/thumb`, the address that file's own row uses. There is no
cover object on any account and nothing to keep in step.

**Nothing is picked automatically.** An earlier version chose one per folder (a
stable hash pick among posters). It was the wrong trade: which film stands for a
trilogy is taste, and a picture appearing on a folder nobody asked about sits
badly in an app whose posture is that nothing happens unless asked. So
`FolderArt` is written only by `SetFolderArt` (`POST /api/folders/art`), and no
entry means no picture.

What `folderart.go` computes per listing is one bit per folder: is there
anything under it that could be chosen? The listing's map has three states: no
entry (nothing picturable), an entry without an `id` (nothing chosen yet), and an
entry with one. It is one walk of the index, walking up from each thumbnailed file
so a film three levels down still counts for the library above.

The choice is dropped when the folder or the file goes, so the folder goes back
to its icon rather than pointing at nothing. The cost is on the read side: a
picture lives in the pack of the folder holding it, so a parent whose children
all have pictures gathers a pack per child. Tiles load lazily and gathered packs
are held until lock, so this is the cost of opening those folders, paid where
they are listed. Copying covers into the parent's pack would add an object per
folder and a second thing to invalidate.

## Organizing a folder

The organizer (the `🗂` button on a folder) does six things to a folder and
everything under it: flatten it, file it into `2026/January` by modified date,
remove empty folders, erase every file of a kind, select every file of a kind,
and find duplicates. All six have the same shape: **one read to plan from, then
the per-item endpoints the app already had.**

The common read is `Vault.Survey` (`organize.go`, `GET /api/folders/survey`): one
walk returning every file at or below the folder (ID, name, folder, size,
lower-cased extension, depth) and every folder below it with what it holds
directly, what it holds at any depth, and the bytes. Two counts because a folder
holding nothing directly can still have something below it; a folder is safe to
remove exactly when the deep count is zero. Folders never used are included
(`Manifest.AllFolders`), since those are what the empty-folder tool looks for.

**No organizer endpoint writes.** A flatten is a `POST /api/files/{id}/move` per
file then a `DELETE /api/folders` per emptied folder; a date sort adds a
`POST /api/folders` per date folder first; a purge is a `DELETE /api/files/{id}`
per match. The loop runs **in the browser, one item at a time**: a run that stalls
on the fortieth of two hundred has moved thirty-nine things and can name the one
that refused. A single server-side "flatten" call could half-succeed with no way
to say which half, and could not show progress.

Two invariants make that composition safe without the server knowing an
organizer exists:

- **Names are planned before anything moves.** A folder per camera means
  `IMG_0001.jpg` in each, so the browser builds the whole target-name set first,
  against the destination and against each other, and numbers or prefixes the
  collisions. It joins prefixes with ` - `, because `SanitizeName` keeps only the
  leaf of anything containing a slash. The plan can be shown before it runs
  (`source path → new name`, redrawn per naming choice); past a few hundred rows
  it shows the first and counts the rest.
- **Folder removal stays non-recursive.** Empty folders are removed deepest first
  with `recursive=0`, so the server's own refusal (`"%s is not empty"`) guarantees
  a tidy never takes a file with it. The browser's survey is milliseconds old; the
  server's view is current and decides.

Flattening, date filing and pruning move no bytes: a file records its folder,
and its parts are named after the archive, not the path, so all three are index
rewrites. Moving does not restamp the modified time, for the same reason
converting, re-encrypting and relocating do not; a date sort that restamped what
it moved would destroy the dates it had just read. Every delete-shaped tool ends
in the ordinary delete confirmation.

### Filing by date

The one tool whose plan is not a rearrangement of the survey:
`Vault.DateSort` (`datesort.go`, `GET /api/folders/date-sort`) needs each file's
modified time, a calendar and a simulation of the tree afterwards, and returns the
finished plan: folders to make, which file goes where under what name, and which
folders the moves leave empty. The browser runs it as a flatten runs.

- **A file already in the folder its date names is *settled*, not moved.** The
  tool is idempotent, and a stalled run is finished by running it again.
- **Names are never freed by the moves that vacate them.** Every landing name is
  checked against the destination now and everything else arriving, and a name
  being moved out of stays taken. The plan holds in any order, including the order
  a stalled run left. The price is an occasional needless `(2)`.
- **The folder is named in the viewer's clock.** Times are stored in UTC and shown
  locally, so the request carries `offset` (the browser's minutes from UTC). A
  fixed offset is not a time zone, so a summer file planned from a winter browser
  can land an hour out near a month's end; the alternative was shipping `tzdata` in
  a static binary.

Month folders use English names (`2026/January`) so they mean the same folder on a
machine set to another language. A file with no modified time is counted and left
where it is.

### Duplicates

`Vault.Duplicates` (`duplicates.go`, `GET /api/folders/duplicates`) is a separate
read because it needs every file's SHA-256, which no other tool wants. It answers
three questions at once, weakest to strongest, because switching between them is
the whole use of the dialog:

- **By content**: files sharing a SHA-256. The vault hashes every file it stores,
  so this is not a heuristic. A file with no recorded hash is left out rather than
  grouped with others that have none.
- **By size**: same length, empty files excluded. Each group says whether the
  hashes agree.
- **By name**: copy markers stripped repeatedly (`report (1) (2).pdf`), case and
  separators dropped, then the name split into **digit runs** and **letters**.
  Digits must match exactly; letters may differ by a bounded edit distance (one
  edit from five letters, two from sixteen, none below). The split is what keeps
  `IMG_0001.jpg` and `IMG_0002.jpg` apart. The extension is part of the key, so a
  film never groups with its subtitles.

Only names sharing an extension and digit signature are compared, sorted by
length so the walk stops when a candidate is too long; a hard comparison budget
catches the rest and the answer says `partial`. Because similarity chains, a run
of near-matches joining more than a handful of distinct names is treated as a
naming scheme and split back into exact matches, rather than coming back as one
group of nine thousand. Groups are sorted by what clearing them would free, with
the suggested keeper first (shallowest, then plainest name, then sort order).
Nothing here deletes: ticked files go through the ordinary delete, or to the
selection bar.

### What a folder holds

A folder's size is the sum of everything below it, so the listing cannot show it.
`Vault.FolderStats` (`folderstats.go`, `GET /api/folders/stats`), asked when a
folder's menu opens, returns files and bytes at or below it, sub-folder count,
what the parts weigh across accounts, which accounts, how many files are short a
shard, and the newest change.

Two weights because they answer different questions: what is *in* the folder (a
`du -sh`) and what it *costs*, widened by the erasure code, which is what account
quota is spent in. The clouds are named from the shard records, so a folder whose
parts sit on a since-disconnected account still says so. It is its own read so a
menu header does not carry ten thousand file names back, and it is asked only for
the menu actually opened.

## Short files

An upload commits once enough shards have landed to rebuild the file
([data pipeline](data-pipeline.md)). A cloud not answering at that moment costs
the file its spare shard and nothing visible: the upload succeeds, the file reads
back, and it stays one cloud short. `Stats.Degraded` counts these;
`Vault.Degraded` (`degraded.go`, `GET /api/degraded`) is the list behind the
number, with the same rule (fewer distinct shards recorded than the file's scheme
calls for) and the same scope (**the main vault alone**), so the figure clicked
and the list opened are the same set.

It reads the index and contacts no account, so the dialog can re-ask after every
repair. Whether accounts still hold what the index says is the health check's
question ([lost parts](vault.md#lost-parts-records-the-account-does-not-back-up)
for the whole vault).

- **Worst first.** Ordered by `Spare` (how many more clouds the file could lose),
  then shards missing, then path, so the order is stable between pages.
- **Paged.** An account down for an afternoon leaves everything uploaded that
  afternoon short, so the list is usually empty or large. `offset`/`limit`, 25 by
  default and 200 at most; `Total`, `Bytes` and `Unreadable` describe the whole
  list. An offset past the end is an empty page.

Repairs reuse what exists:

- **Rebuild onto other clouds.** Each row opens the same `POST /api/relocate` a
  file row's **Move to other clouds** uses; a short file takes relocation's
  rebuild branch, which writes the missing shard ([placement](placement.md)).
- **From the replica.** A file below its threshold has nothing to rebuild from
  unless it has a replica (the row carries `replica`); then `"source": "replica"`
  reads it from the replica and writes fresh parts (`RelocateFrom`), leaving the
  replica alone.
- **Reattach.** A shard that *was* written and has lost its record looks the same
  from here. **Look for the missing parts on** asks one cloud, or all,
  (`GET /api/vault/orphans/strays`) and opens the reattach panel aimed at that
  account ([vault.md](vault.md#mislaid-shards-reattaching)). A cloud that would
  not list, and a cloud where every part already has a record, are each said
  explicitly.
- **Rebuild a repository from upstream.** A tracked repository's bundle is marked
  (`DegradedFile.Tracked`) and can be rebuilt from its remote
  ([below](#repositories-kept-as-bundles)).

## A folder that looks after itself

The short-files list waits to be clicked. The failure worth designing for is the
silent one: a token that expired in March, a bucket that stopped answering, a
shard never written. A file at n−1 shards reads at full speed right up to the read
that needs the missing one.

So a folder can carry a standing instruction. `Automation` (`automation.go`)
lives in the manifest keyed by folder path, so it is encrypted, travels with a
rename and is dropped with a delete. It holds a cadence (`hourly`, or `daily` /
`weekly` at a local wall-clock time), a task, an action, the task's bounds and the
last 8 runs.

**The task is a choice; the schedule around it is not.** Two jobs share the
cadence, history, "run now" button and one-at-a-time rule:

| Task | Actions | Bounds |
|---|---|---|
| `shards`: are the parts still there | `check`, `rebalance` | `ShardPolicy`: `RebuildLimit`, `MaxRepairs`, `Narrow` |
| `git`: has the upstream moved | `check`, `pull` | `GitPolicy`: `MaxRepos`, `SizeLimit`, `Prune` |

`check` means look and change nothing for both; `Action.fits` refuses
combinations that do not exist. Each run record splits the same way: a common
half (folder, task, when, which accounts answered) and the task's counters.

**Per folder, not per vault.** The films are replaceable and the passports are
not, and the folder is where that distinction is already written down. A policy
covers its folder and everything under it, so `/` is a whole-vault policy. Where
policies overlap, `DueAutomations` orders them deepest first and one `seen` set
spans the tick, so a file is checked once, by the most specific policy.

### The shards sweep

`probeAccounts` pings every connected account once, concurrently, within each
backend's ping budget. Only then is each file walked, so a shard on an account
that failed the probe (or is no longer connected) is known unreachable without a
request; otherwise a folder of ten thousand files on a dead cloud would wait out
ten thousand timeouts. What remains is a `Stat` per shard per sampled chunk, the
sampling the file health check uses, a few files at a time.

Each file lands in one state against its own scheme: `whole`, `short` (at least
one missing, at least k present) or `at_risk` (fewer than k: unreadable until
something comes back). Only `short` is actionable, and the repair list is ordered
by spare shards ascending so a bounded run spends its budget on the files closest
to loss.

**A repair is always a rebuild.** Relocation's cheap path copies a shard from the
account holding it; every case here is one where there is nothing to copy from.
The planner normally decides "short" from index rows, which are exactly what is
wrong, so the sweep passes what it observed (`unreachable` accounts and `absent`
shards) and `countReachable` subtracts both. Hand-driven relocations pass the zero
value and behave as before: a move pointed at a dead cloud should fail visibly,
not silently rebuild. The rebuild is `migrateFile`, the same commit ordering as a
password change; old shards on the dead account become orphans, which the stray
scan finds.

**Three bounds**, because `migrateFile` gathers into memory and three in the
morning on a Raspberry Pi is not a watched password change:

- **`RebuildLimit`**: the largest file an unattended repair rebuilds, defaulting
  from the machine's tuning ([data pipeline](data-pipeline.md)). Larger files are
  counted `Deferred` and named, for a hand repair. Negative means no ceiling, a
  thing to choose rather than inherit.
- **`MaxRepairs`**: files rebuilt per run; the rest wait for the next run, worst
  first.
- **`Narrow`**, off by default, and the default is the important half.
  `rebalanceTarget` puts a file back over as many answering clouds as its scheme
  has shards, with the same k. With fewer answering clouds, an unattended job must
  not make a file permanently less durable and less secret because a cloud was down
  for an hour, so the file is left and reported. Turned on, `narrowScheme` scales k
  with the width (rounded, so 4-of-6 on four clouds is 3-of-4) and never allows
  k = n.

### The schedule

`Automation.Due` is a comparison, not a timer: now ≥ next slot after the last run
(or creation). Catch-up is free and idempotent (a machine off for three days owes
one sweep, not three), and a new policy waits for its first real slot. Wall-clock
times use `time.Local`, because "every day at 10 am" is about somebody's morning.

`Server.automationLoop` ticks every minute and contacts nothing unless something
is due. It runs **only while the vault is unlocked**: the schedules are in the
encrypted index and the files need its keys. A running sweep keeps the idle timer
off so the vault is not locked mid-rebuild; keeping the vault open overnight is
the operator's decision (`--idle-timeout`) and the security trade it looks like.

One sweep runs at a time, vault-wide (`ErrAutomationBusy`): two would be two
gathers in memory and two sets of uploads racing for the same accounts. Every run
is recorded, failures included (a fortnight of "no account answered" is what
someone needs to see). Warnings are capped at 20 per run and history at 8 runs,
because this goes into the vault file and every backup. A folder listing carries
`FolderAutomation`, the same record with history replaced by one `Trouble` flag.

`GET/POST/DELETE /api/automation`, `POST /api/automation/run`;
`sand automation list|set|remove|run`.

## Repositories kept as bundles

A git repository in the vault is **one file**: a bundle of its whole history.
That is forced by the storage model. A repository kept as a directory would be
thousands of loose objects, each becoming several cloud objects, and a repacking
fetch would re-scatter most of it every week. A bundle goes through the ordinary
upload path (`UploadStreamAt`), erasure code and accounts, and `gitrepo.go` needs
to know none of that. It is also the shape that outlives SAND Vault:
`git clone repo.bundle` is a complete repository with nothing to reimplement.

**The weekly check is nearly free, and that is the design.** `git ls-remote` is a
few kilobytes of ref advertisement and no objects; `git.Digest` reduces it and
compares with the digest stored beside the bundle. Equal means done: nothing
downloaded, uploaded or re-encrypted. Only a repository that moved costs anything,
and not a full clone: the stored bundle is turned back into a mirror
(`git.FromBundle`), `origin` is pointed at the real remote, and the fetch brings
the difference. `LsRemote` drops the advertised `HEAD` and peeled `^{}` entries;
keeping `HEAD` would make every stored repository differ from its upstream forever
and turn the cheap check into a weekly full fetch.

The digest is stored rather than the refs for size: four thousand tags would put
a quarter of a megabyte per repository into an index that is held in memory,
written on every change and replicated to every account. `GitSource` keeps
`CheckedAt` and `FetchedAt` separately; the gap between them is the good news.

**It borrows the machine's git.** `internal/git` shells out rather than speaking
the protocol, because the hard part is credentials: an agent-held SSH key, a
credential helper, a host alias in `~/.ssh/config`, a corporate CA. A second
implementation would reimplement that badly or reach no private repository. The
rule: a repository SAND Vault can see is one you can see from the same machine.
The cost is a runtime dependency; `git.Available()` gates the task and a fetching
policy is refused on a machine without git rather than failing at four in the
morning. (SFTP makes the opposite choice, for reasons given in
[sftp.md](sftp.md).)

**A repository URL is hostile input.** git's `ext::` transport hands its argument
to a shell, and a URL beginning with `-` is an option. Both are shut twice:
`ParseRemote` is an allow-list (https, http, ssh and scp-like `git@host:path`),
and every invocation also carries `-c protocol.ext.allow=never`. `ParseRemote` is
the only way to build the `git.Remote` every network-reaching function requires,
so no call site can skip it. **Local paths and `file://` are refused**: vault
access must not become arbitrary local file read. Submodules are never recursed,
nothing is checked out, and `GIT_TERMINAL_PROMPT=0` plus `ssh -o BatchMode=yes`
turn a missing credential into an immediate failure instead of a hang.

**A vanished upstream is not a deletion.** An outage, rename, revoked token and a
real deletion look identical, and only one is a reason to discard what may be the
last copy. So a silent upstream sets `GitSource.Gone` with the reason and the
bundle stays; `GitPolicy.Prune` exists and is off by default. A sweep still *asks*
every repository (the cheap half, which keeps the report honest); `MaxRepos` and
`SizeLimit` bound only what is fetched.

A refresh stores the new bundle with `Overwrite`, which files a new entry over
the old one, so the file gets a **new ID**; `refreshRepo` returns the repository
as it now stands.

**A bundle that cannot be read is rebuilt from upstream.** A refresh reads the
stored bundle back to fetch the difference, which fails when the bundle is the
problem (too few accounts answering, too few parts). `RebuildRepo` does what
tracking does (`git.Mirror` from upstream, bundle, upload with `Overwrite`)
pointed at the existing file, with chosen accounts and scheme, never reading the
stored parts. It mirrors rebuilding from a replica: a trusted copy written out as
fresh parts; only where the copy lives differs. The overwrite forgets the old
record in the same index write and erases its parts best-effort. It costs a full
clone, so the sweep never does it on its own. Listings mark tracked files
(`Listing.TrackedRepos`) so a row can offer it without fetching the repository
list.

`/api/git/*`; `sand git track|list|refresh|rebuild|untrack`. User steps are in
[repositories](../guide/repositories.md).
