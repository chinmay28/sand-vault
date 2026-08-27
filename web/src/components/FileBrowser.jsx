import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes, isPlayable } from '../theme'
import { api, joinPath } from '../api'
import { sortFiles, sortFolders, sortHits, useViewPrefs } from '../view'
import { ActionSheet, Banner, Button, Empty, Modal, Spinner } from './ui'
import { UploadDestination, RelocateClouds } from './CloudSelect'
import { makeThumbnails } from '../thumbs'
import {
  batchBytes, batchPicks, describePicks, emptyDirs, picksFromDrop, picksFromInput, totalBytes,
} from '../upload'
import {
  COLUMNS, FILM_COLUMNS, TILE_POSTER, TILE_SQUARE,
  FileRow, FileTile, FolderRow, FolderTile, SelectBox,
} from './FileEntry'
import { Breadcrumbs, FolderHeader, NavCluster, SearchField, SelectionBar, ViewControls } from './Toolbar'
import { BulkAssign, BulkDelete, BulkDownload } from './BulkActions'
import MoveToFolder from './MoveToFolder'
import { FilmButton, FilmLookupSettings } from './FilmDetails'
import { OrganizerButton, OrganizerMenu, OrganizerTool } from './Organizer'
import { ImportFromMachine } from './ImportFromMachine'

/* How long to sit on a keystroke before asking the server. Long enough that
   typing a word is one query rather than six, short enough to feel live. */
const SEARCH_DEBOUNCE_MS = 180

/* How wide a tile is allowed to get before the grid grows another column. Two
   columns on the narrowest phone anyone still carries, and as many as fit on a
   desk — which is the whole reason to be looking at tiles rather than rows. */
const TILE_MIN_PX = { mobile: 108, desktop: 132 }

export default function FileBrowser({
  nav, listing, loading, error, providers, defaultAccounts, defaultScheme, mobile,
  subVaults = [], shownSubVaults = [], onOpenSubVault,
  onRefresh, onPreview, onInspect, onFilm, onError,
}) {
  const [dragging, setDragging] = useState(false)
  const [pending, setPending] = useState(null)
  const [uploads, setUploads] = useState([])
  const [warnings, setWarnings] = useState([])
  const [creatingFolder, setCreatingFolder] = useState(false)
  const [query, setQuery] = useState('')
  // Only a phone has this: there the field takes the whole toolbar while it is
  // open, so it has to be opened and closed rather than simply sitting there.
  const [searchOpen, setSearchOpen] = useState(false)
  const [scoped, setScoped] = useState(true)
  const [results, setResults] = useState(null)
  const [searching, setSearching] = useState(false)
  const [searchError, setSearchError] = useState(null)
  const [prefs, view] = useViewPrefs()
  const [selecting, setSelecting] = useState(false)
  const [selected, setSelected] = useState(() => new Set())
  const [bulk, setBulk] = useState(null)
  const [films, setFilms] = useState(false)
  /* Choosing between the folder's five tools, and then whichever was chosen.
     Two states rather than one: picking closes the sheet and opens the tool in
     the same click, and a single state would have the sheet's own dismissal
     land on the tool it had just opened. */
  const [organizing, setOrganizing] = useState(false)
  const [importing, setImporting] = useState(false)
  const [tool, setTool] = useState(null)
  /* The folder's standing instruction: its schedule, what the last few runs
     found, and the button that starts one now. */
  /* Files the organizer picked from below this folder, which have no row here
     to be ticked. They join the selection as items in their own right — see
     `chosen` — so the selection bar can move, download, scatter or erase a
     hundred subtitle files that are scattered over forty folders. */
  const [deeper, setDeeper] = useState([])
  const fileInput = useRef(null)
  /* A second input, because `webkitdirectory` is a property of the input and
     not of the click: an input can ask for files or for a folder, never for
     whichever the person turns out to want. */
  const dirInput = useRef(null)
  /* Which of the two the Upload button should open. Asked rather than guessed:
     both are ordinary things to want, and only one dialog can be opened. */
  const [choosing, setChoosing] = useState(false)
  /* Walking a dropped folder takes a moment on a large tree, and it happens
     before the destination dialog can open, so the drop needs something to say
     for itself in the meantime. */
  const [reading, setReading] = useState(false)
  const dragDepth = useRef(0)
  // Where the last tick was put, so a shift-click has a range to reach back to.
  const anchor = useRef(null)

  const path = nav.path
  // Which of the vaults inside the file we are standing in. Empty is the main
  // one, and every path-addressed call carries it.
  const vault = nav.vault
  const canUpload = providers.length > 0
  // What to call the root crumb. A sub vault's tree is not below the main
  // vault's, so the trail has to say which one you are walking.
  const vaultLabel = vault ? (subVaults.find((s) => s.id === vault)?.label || 'Sub vault') : ''
  /* Where a selection could be sent. Standing in the main vault that is every
     open sub vault; standing inside one it is the way back out. A locked sub
     vault is never a target — the move has to write into its index. */
  const assignTargets = useMemo(() => (vault
    ? [{ id: '', label: 'the main vault' }]
    : subVaults.filter((s) => s.unlocked).map((s) => ({ id: s.id, label: s.label }))
  ), [vault, subVaults])
  const searchTerm = query.trim()
  // Searching from inside a folder looks there first; the results header can
  // widen it to the whole vault.
  const searchScope = scoped ? path : '/'

  // Walking into a folder is a different question from the one being asked, so
  // the search ends when navigation begins — and on a phone the field it was
  // typed into gives the toolbar back.
  useEffect(() => { setQuery(''); setSearchOpen(false) }, [path, vault])

  /* A search never crosses out of the vault you are standing in. Answering one
     query from two trees would put a sub vault's filenames in front of someone
     searching the main vault, which is the disclosure a sub vault exists to
     prevent. */
  const runSearch = useCallback((signal) => api.search(searchTerm, { path: searchScope, vault, signal })
    .then((resp) => { setResults(resp); setSearchError(null) })
    .catch((err) => {
      if (err.name === 'AbortError') return
      setResults(null)
      setSearchError(err.message)
    }), [searchTerm, searchScope, vault])

  useEffect(() => {
    if (!searchTerm) {
      setResults(null)
      setSearchError(null)
      setSearching(false)
      return
    }

    // Every keystroke cancels the request the last one started, so a slow
    // answer can never arrive after — and overwrite — a newer one.
    const controller = new AbortController()
    setSearching(true)
    const timer = setTimeout(() => {
      runSearch(controller.signal).finally(() => {
        if (!controller.signal.aborted) setSearching(false)
      })
    }, SEARCH_DEBOUNCE_MS)

    return () => { clearTimeout(timer); controller.abort() }
  }, [searchTerm, runSearch])

  // A deleted result has to leave the results too, not just the listing.
  const refreshSearch = useCallback(() => {
    onRefresh()
    if (searchTerm) runSearch()
  }, [onRefresh, runSearch, searchTerm])

  /* One list of what is on screen, whether that came from the listing or from
     a search, already in the chosen order. Everything downstream — the rows,
     the tiles, select-all, a shift-click's range — works off this and so all
     of them agree about what "everything here" is and what order it is in. */
  const entries = useMemo(() => {
    /* Which file's thumbnail each folder is drawn with, by path. Worked out by
       the server in one walk of the index — a folder's picture comes from what
       is inside it, which a listing of the folder above it cannot see. */
    const art = (searchTerm ? results?.folder_art : listing?.folder_art) || {}

    if (searchTerm) {
      return sortHits(results?.hits || [], prefs).map((hit) => (hit.type === 'folder'
        ? {
          kind: 'folder', key: `dir:${hit.path}`, name: hit.name, path: hit.path,
          location: hit.dir, art: art[hit.path],
        }
        : { kind: 'file', key: `file:${hit.file.id}`, name: hit.file.name, file: hit.file, location: hit.dir }))
    }
    return [
      ...sortFolders(listing?.folders, prefs).map((name) => ({
        kind: 'folder',
        key: `dir:${joinPath(path, name)}`,
        name,
        path: joinPath(path, name),
        art: art[joinPath(path, name)],
      })),
      ...sortFiles(listing?.files, prefs).map((file) => ({
        kind: 'file', key: `file:${file.id}`, name: file.name, file,
      })),
    ]
  }, [searchTerm, results, listing, path, prefs])

  /* What this folder holds, for the heading above it on a phone: how much is
     here, and how many separate accounts it is spread over.

     Counted off the listing rather than asked of the server, because the
     listing is the answer — it arrived with every file's size and the accounts
     holding each of its parts already in it. The cloud count is of this folder
     alone, not of the vault: a folder whose files all landed on the same three
     accounts says three whether ten are connected or three are.

     Null until the listing arrives, so the line is blank for that moment
     rather than reading a zero that is about to be wrong. */
  const stats = useMemo(() => {
    if (!listing) return null

    const folders = listing.folders?.length || 0
    const files = listing.files || []
    const bytes = files.reduce((total, file) => total + (file.size || 0), 0)
    const clouds = new Set()
    for (const file of files) {
      for (const shard of file.shards || []) {
        if (shard.provider_id) clouds.add(shard.provider_id)
      }
    }

    /* What is worth a line here is what the listing underneath cannot say for
       itself. Folders are countable at a glance in the rows below, so they are
       named only when they are all there is; what a row cannot tell you is how
       much this folder comes to and how far it is spread — and on a 390px
       screen a fourth number is a fourth number that gets cut off. */
    const counted = files.length > 0
      ? [`${files.length} file${files.length === 1 ? '' : 's'}`, formatBytes(bytes)]
      : folders > 0 ? [`${folders} folder${folders === 1 ? '' : 's'}`] : []

    return {
      summary: counted.length ? counted.join(' · ') : 'Empty',
      clouds: clouds.size,
    }
  }, [listing])

  // Which rows have a stored picture. The listing says so outright, so no row
  // asks for a thumbnail that was never made.
  const thumbs = useMemo(
    () => new Set((searchTerm ? results?.thumbs : listing?.thumbs) || []),
    [searchTerm, results, listing])

  /* The films that have been matched, by file id — a title and a year each,
     which is what a tile is captioned with. The full record is a request per
     film, made when one is opened. */
  const movies = useMemo(
    () => (searchTerm ? results?.movies : listing?.movies) || {},
    [searchTerm, results, listing])

  /* Whether this folder's videos are matched at all. A search reaches across
     the whole vault, so it answers for the folder it was started from — which
     is the folder whose switch the toolbar button changes. */
  const lookup = listing?.movie_lookup
  /* The folder's standing instruction, minus its history — it rides along with
     the listing so that opening a folder does not cost a second request to be
     told the usual answer, which is that it has none. */
  const automation = listing?.automation
  const repoCount = listing?.repos || 0

  /* A selection is about the things in front of you, so walking somewhere else
     — or asking a different question of the index — ends it rather than
     carrying a set of hidden rows along to be acted on by surprise. */
  useEffect(() => {
    setSelected(new Set())
    setDeeper([])
    anchor.current = null
  }, [path, searchTerm])

  /* What is selected: the ticked rows, and whatever the organizer picked from
     under this folder. The two are kept apart because only one of them can be
     ticked — a file four folders down has no row here — and put together
     because everything downstream of a selection acts on files by ID and does
     not care which of the two a file came from. */
  const chosen = useMemo(() => ([
    ...entries.filter((entry) => selected.has(entry.key)),
    ...deeper.filter((entry) => selected.has(entry.key)),
  ]), [entries, deeper, selected])
  const chosenFiles = chosen.filter((entry) => entry.kind !== 'folder')

  const selectAll = useCallback(() => {
    setSelected(new Set([...entries, ...deeper].map((entry) => entry.key)))
  }, [entries, deeper])

  /* What picking by type hands back. Whatever is already a row here is ticked
     as if it had been clicked; the rest is carried alongside, counted on the
     bar so a selection is never larger than it looks without saying so. */
  const pickFiles = useCallback((files) => {
    const rows = new Set(entries.filter((e) => e.kind !== 'folder').map((e) => e.file.id))
    setDeeper(files.filter((file) => !rows.has(file.id)).map((file) => ({
      kind: 'file', key: `file:${file.id}`, name: file.name, file,
    })))
    setSelected(new Set(files.map((file) => `file:${file.id}`)))
    setSelecting(true)
  }, [entries])

  const stopSelecting = useCallback((on) => {
    setSelecting(on)
    if (!on) { setSelected(new Set()); setDeeper([]) }
  }, [])

  /* Ticking a box, with shift extending from the last one ticked — the range
     select every file manager has, and the only bearable way to pick forty
     photographs out of sixty. */
  const toggle = useCallback((entry, checked, event) => {
    const at = entries.findIndex((e) => e.key === entry.key)
    setSelected((current) => {
      const next = new Set(current)
      const from = anchor.current
      if (event?.shiftKey && from != null && from < entries.length && at >= 0) {
        const [lo, hi] = from <= at ? [from, at] : [at, from]
        for (let i = lo; i <= hi; i++) {
          if (checked) next.add(entries[i].key)
          else next.delete(entries[i].key)
        }
      } else if (checked) {
        next.add(entry.key)
      } else {
        next.delete(entry.key)
      }
      return next
    })
    anchor.current = at
  }, [entries])

  /* Ctrl+A is what a file manager answers to, and this one has a selection to
     answer it with. Not while a field has the focus, where the same keystroke
     means the text in it — and not on a page with nothing listed. */
  useEffect(() => {
    const onKey = (e) => {
      if (e.key !== 'a' && e.key !== 'A') return
      if (!(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey) return
      const el = e.target
      if (el instanceof HTMLElement && (el.isContentEditable
        || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName))) return
      if (!entries.length) return
      e.preventDefault()
      setSelecting(true)
      selectAll()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [entries.length, selectAll])

  /* Back and Forward on the keyboard as well as under the pointer. Alt is what
     a browser uses for the same pair, and the guard is the same one as above:
     inside a text field those keystrokes are how you move through the text. */
  useEffect(() => {
    const onKey = (e) => {
      if (!e.altKey || e.ctrlKey || e.metaKey) return
      const el = e.target
      if (el instanceof HTMLElement && (el.isContentEditable
        || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName))) return
      if (e.key === 'ArrowLeft') { e.preventDefault(); nav.back() }
      else if (e.key === 'ArrowRight') { e.preventDefault(); nav.forward() }
      else if (e.key === 'ArrowUp') { e.preventDefault(); nav.up() }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [nav])

  /* What was chosen is held here until the clouds it is going to have been
     settled. Nothing is read or sent in the meantime — the picker is between
     choosing and uploading, not a step inside the upload.

     A choice is files each with the path it had inside whatever was picked, so
     a folder can be rebuilt on the other side rather than tipped out flat; see
     upload.js. */
  const choosePicks = useCallback((picks) => {
    /* A folder with nothing in it is not an upload — no bytes, no clouds, no
       choice to make about where its parts go. It is a folder, so it is made
       rather than put through the picker and refused there for having no
       files. */
    if (!picks.files.length) {
      if (!picks.dirs.length) return
      Promise.all(picks.dirs.map((dir) => api.createFolder(joinPath(path, dir), vault)))
        .then(onRefresh)
        .catch((err) => onError(err.message))
      return
    }
    if (!canUpload) {
      onError('Connect a cloud account before uploading — there is nowhere to put the parts yet.')
      return
    }
    setPending(picks)
  }, [canUpload, onError, onRefresh, path, vault])

  /* Sends a choice, in as many requests as it takes.

     Everything picked used to go in one request, which for a folder meant a
     body of every byte in it: nothing was stored until all of it had arrived,
     one failure lost the lot, and the thumbnails for all of it were made at
     once before any of it was sent — enough, for a folder of photos, to take
     the tab down with no error anywhere, because there is no error to be had
     when the window itself is gone.

     So it goes in batches: each is bounded in bytes and in files, its pictures
     are made just before it leaves rather than all of them up front, and what
     it stored is listed while the next one is still going. A batch that fails
     is reported per file and the rest carry on — there is no sense in throwing
     away the ninety files that would have worked because the tenth would
     not. */
  const uploadFiles = useCallback(async (picks, accounts, scheme = '') => {
    const batches = batchPicks(picks.files)
    if (!batches.length) return

    // What is going up, said the way it was chosen: one file by its name, a
    // folder by the folder rather than by the four hundred files inside it.
    const card = {
      id: Math.random().toString(36).slice(2),
      label: describePicks(picks),
      progress: 0,
      note: batches.length > 1 ? `1 of ${batches.length}` : '',
    }
    setUploads((prev) => [...prev, card])

    const track = (fields) => setUploads((prev) =>
      prev.map((u) => (u.id === card.id ? { ...u, ...fields } : u)))

    // Bytes rather than batches, so the bar moves at the rate the network is
    // actually going and not in equal steps over unequal batches.
    const total = totalBytes(picks) || 1
    let done = 0
    // The corners of the tree no file would make on the way past. They ride
    // with the first request that lands, and are only let go once it has.
    let folders = emptyDirs(picks)
    const failures = []
    const notes = []

    try {
      for (let i = 0; i < batches.length; i++) {
        const group = batches[i]
        const weight = batchBytes(group)
        if (batches.length > 1) track({ note: `${i + 1} of ${batches.length}` })

        try {
          /* Made here, before this batch is sent and not before the whole
             upload, because this is the only place the plaintext file exists
             in a browser — and a few at a time, because decoding all of them
             at once is what made the tab disappear. Each resolves to null
             rather than throwing, so a format we cannot draw never holds up
             its upload. */
          const thumbnails = await makeThumbnails(group.map(({ file }) => file))

          const resp = await api.upload(group, path, {
            vault,
            accounts,
            scheme,
            thumbs: thumbnails,
            dirs: folders,
            onProgress: (fraction) => track({ progress: (done + fraction * weight) / total }),
          })
          folders = []

          const results = resp.results || []
          for (const r of results) {
            if (!r.ok) failures.push(`${r.name}: ${r.error}`)
            for (const w of r.warnings || []) notes.push(`${r.name}: ${w}`)
          }
          // Listed as it arrives: on a long upload the folder fills up while
          // the rest of it is still going, which is also the only sign from
          // out here that anything is happening at all.
          onRefresh()
        } catch (err) {
          // The request did not land, so nothing in it did — say so of each
          // file rather than once, since the rest of the upload continues and
          // the list would otherwise be the only record of what is missing.
          for (const { file, path: rel } of group) failures.push(`${rel || file.name}: ${err.message}`)
          // Unless it was never about this batch. A locked vault or a
          // cancelled request fails everything after it the same way, and
          // grinding through the remaining gigabyte to prove it helps nobody.
          if (err.code === 'ABORTED' || err.status === 401 || err.status === 403) {
            const left = batches.slice(i + 1).reduce((n, g) => n + g.length, 0)
            if (left) failures.push(`${left} more file${left === 1 ? '' : 's'} were not sent.`)
            break
          }
        }

        done += weight
        track({ progress: done / total })
      }

      if (failures.length) onError(failures.join('\n'))
      if (notes.length) setWarnings((prev) => [...prev, ...notes])
    } finally {
      setUploads((prev) => prev.filter((u) => u.id !== card.id))
    }
  }, [path, vault, onError, onRefresh])

  /* Drag counting: dragenter/dragleave fire for every child element, so a
     naive boolean flickers as the pointer crosses rows. */
  const onDragEnter = (e) => {
    e.preventDefault()
    dragDepth.current += 1
    if (e.dataTransfer?.types?.includes('Files')) setDragging(true)
  }
  const onDragLeave = (e) => {
    e.preventDefault()
    dragDepth.current -= 1
    if (dragDepth.current <= 0) { dragDepth.current = 0; setDragging(false) }
  }
  /* A drop may be a folder, and only the entries API can say so or look inside
     one — which it will only do while the drop event is still on the stack, so
     the reading starts here rather than after an await. */
  const onDrop = (e) => {
    e.preventDefault()
    dragDepth.current = 0
    setDragging(false)
    const walking = picksFromDrop(e.dataTransfer)
    setReading(true)
    walking
      .then(choosePicks)
      .catch((err) => onError(err.message))
      .finally(() => setReading(false))
  }

  const listProps = {
    entries,
    thumbs,
    movies,
    /* A row only offers a film lookup where one could happen: in a folder that
       has asked for it, and on a file a player would take. Everything else
       keeps the menu it has always had. */
    films: !!lookup?.enabled,
    prefs,
    mobile,
    providers,
    // Which vault every row belongs to, and where a row could be sent.
    vault,
    subVaults,
    selecting,
    selected,
    onToggle: toggle,
    onNavigate: nav.navigate,
    onPreview,
    onInspect,
    onFilm,
    onRefresh: searchTerm ? refreshSearch : onRefresh,
    onError,
  }

  return (
    <main
      onDragEnter={onDragEnter}
      onDragOver={(e) => e.preventDefault()}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
      style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', position: 'relative' }}
    >
      <div style={{
        display: 'flex',
        flexDirection: 'column',
        gap: mobile ? '8px' : '10px',
        padding: mobile ? '10px 12px' : '12px 20px',
        borderBottom: `1px solid ${COLORS.border}`,
        flexShrink: 0,
      }}>
        {/* A desk has room to lay the controls out and a phone does not, so
            they are not the same toolbar. On a desk: the arrows, the trail and
            the view controls across the top, the search and the two actions
            below. On a phone: the folder as a heading, saying what it holds and
            over how many clouds, with everything else on a strip beneath it —
            and search taking that strip's place while it is being typed into,
            because a field a folder name has to fit in cannot also share the
            line. */}
        {mobile ? (
          <FolderHeader
            nav={nav}
            path={path}
            vault={vaultLabel}
            stats={stats}
            prefs={prefs}
            view={view}
            selecting={selecting}
            canUpload={canUpload}
            onSelecting={stopSelecting}
            onSearch={() => setSearchOpen(true)}
            onNewFolder={() => setCreatingFolder(true)}
            onUpload={() => setChoosing(true)}
            onImport={() => setImporting(true)}
            /* Onto the strip under the heading, with the other controls that
               say how this folder is read rather than what is in it. Lit when
               the folder is opted in, since a folder that talks to a third
               party should never do so quietly. */
            film={<FilmButton lookup={lookup} mobile onOpen={() => setFilms(true)} />}
            /* Beside it, because both are things done to the folder rather
               than to a row in it. */
            organizer={(
              <>
                {/* One button for everything done to the folder rather than to
                    a row in it, standing instructions included — it lights when
                    this folder is being looked after and goes amber when the
                    last sweep found something. */}
                <OrganizerButton automation={automation} mobile onOpen={() => setOrganizing(true)} />
              </>
            )}
            /* Handed to the heading rather than put in its place: the field
               takes over the strip of icons underneath and the folder stays
               named above it, so a screen of results still says what was being
               searched and from where. */
            search={(searchOpen || searchTerm) && (
              <>
                <SearchField
                  value={query}
                  busy={searching}
                  mobile
                  autoFocus
                  onChange={setQuery}
                />
                <Button
                  size="md"
                  variant="ghost"
                  onClick={() => { setQuery(''); setSearchOpen(false) }}
                  style={{ flexShrink: 0 }}
                >Done</Button>
              </>
            )}
          />
        ) : (
          <>
            <div style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
              <NavCluster nav={nav} mobile={false} />
              <Breadcrumbs path={path} vault={vaultLabel} mobile={false} onNavigate={nav.navigate} />
              <FilmButton lookup={lookup} mobile={false} onOpen={() => setFilms(true)} />
              <OrganizerButton automation={automation} mobile={false} onOpen={() => setOrganizing(true)} />
              <ViewControls
                mobile={false}
                prefs={prefs}
                view={view}
                selecting={selecting}
                onSelecting={stopSelecting}
              />
            </div>

            <div style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
              <SearchField
                value={query}
                busy={searching}
                mobile={false}
                onChange={setQuery}
              />
              <Button size="sm" onClick={() => setCreatingFolder(true)}>+ Folder</Button>
              {/* Beside Upload rather than under the organizer, because it is
                  the same act: files arriving in this folder. The only
                  difference is that these come off a machine you have a login
                  on instead of off the device you are holding. */}
              <Button size="sm" onClick={() => setImporting(true)} disabled={!canUpload}>
                ↓ Import
              </Button>
              <Button size="sm" variant="primary" onClick={() => setChoosing(true)} disabled={!canUpload}>
                ↑ Upload
              </Button>
            </div>
          </>
        )}

        {/* Two inputs rather than one, because whether an input asks for files
            or for a folder is a property of the input and not of the click. */}
        <input
          ref={fileInput}
          type="file"
          multiple
          style={{ display: 'none' }}
          onChange={(e) => { choosePicks(picksFromInput(e.target.files)); e.target.value = '' }}
        />
        <input
          ref={dirInput}
          type="file"
          multiple
          /* Still prefixed in every browser that has it, Firefox and Safari
             included, and still the only way to ask for a folder. */
          webkitdirectory=""
          style={{ display: 'none' }}
          onChange={(e) => { choosePicks(picksFromInput(e.target.files)); e.target.value = '' }}
        />
      </div>

      {selecting && (
        <SelectionBar
          mobile={mobile}
          count={chosen.length}
          total={entries.length + deeper.length}
          files={chosenFiles.length}
          deeper={deeper.length}
          allSelected={entries.length + deeper.length > 0
            && chosen.length === entries.length + deeper.length}
          busy={!!bulk}
          onAll={selectAll}
          onNone={() => setSelected(new Set())}
          onDone={() => stopSelecting(false)}
          onDownload={() => setBulk('download')}
          onMoveTo={() => setBulk('folder')}
          onMove={() => setBulk('clouds')}
          onAssign={() => setBulk('assign')}
          onDelete={() => setBulk('delete')}
          vaultAction={assignTargets.length === 0 ? null : {
            label: vault ? 'Take out' : 'Into a sub vault',
            title: vault
              ? 'Move everything selected back into the main vault'
              : 'Move everything selected into a sub vault, at the paths it already has',
          }}
        />
      )}

      <div style={{ flex: 1, overflowY: 'auto', padding: mobile ? '12px 12px 32px' : '16px 20px 40px' }}>
        {error && <Banner tone="error">{error}</Banner>}
        {warnings.length > 0 && (
          <Banner tone="warn" onDismiss={() => setWarnings([])}>
            {warnings.map((w, i) => <div key={i}>{w}</div>)}
          </Banner>
        )}

        {reading && (
          <Banner tone="info">
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: '8px' }}>
              <Spinner size={11} /> Reading the folder — nothing has left the machine yet.
            </span>
          </Banner>
        )}

        {uploads.map((upload) => (
          <div key={upload.id} style={{
            padding: '11px 13px',
            marginBottom: '10px',
            background: COLORS.surface,
            border: `1px solid ${COLORS.border}`,
            borderRadius: '6px',
          }}>
            <div style={{
              display: 'flex', justifyContent: 'space-between', gap: '10px',
              fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, marginBottom: '8px',
            }}>
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {upload.label}
                {/* Which batch of how many, when it takes more than one — so a
                    folder that goes up in six requests reads as one upload
                    making its way through rather than six of them. */}
                {upload.note && <span style={{ color: COLORS.textMuted }}> · {upload.note}</span>}
              </span>
              <span>{upload.progress >= 1
                ? 'splitting, encrypting and scattering…'
                : `${Math.round(upload.progress * 100)}%`}</span>
            </div>
            <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden' }}>
              <div style={{
                height: '100%',
                width: `${Math.max(4, upload.progress * 100)}%`,
                background: COLORS.accent,
                transition: 'width 0.2s ease',
              }} />
            </div>
          </div>
        ))}

        {/* At the top of the main vault, and only the ones asked for: the
            vaults inside this one, locked ones included. Which ones is a
            per-sub-vault setting in the sub vaults panel, so this is handed
            the drawable ones rather than the whole list and a flag — the rest
            of this component still needs every sub vault, to name the one
            being stood in and to offer the open ones as move targets. They sit
            above the folders rather than among them, because they are not
            folders — a bulk delete must not be able to sweep one up, and a row
            that behaved like a folder would suggest it could be opened by
            being clicked. */}
        {!searchTerm && vault === '' && path === '/' && shownSubVaults.length > 0 && (
          <SubVaultStrip
            subVaults={shownSubVaults}
            mobile={mobile}
            onOpen={onOpenSubVault}
          />
        )}

        {searchTerm ? (
          <SearchResults
            term={searchTerm}
            results={results}
            searching={searching}
            error={searchError}
            path={path}
            scoped={scoped}
            mobile={mobile}
            onScopeChange={setScoped}
            listProps={listProps}
          />
        ) : loading && !listing ? (
          <div style={{ padding: '48px', textAlign: 'center' }}><Spinner size={20} /></div>
        ) : entries.length === 0 ? (
          <Empty icon="◇" title={path === '/' ? 'Nothing stored yet' : 'This folder is empty'}>
            {canUpload
              ? 'Drop files or a whole folder anywhere in this pane, or use Upload. A folder keeps its shape; each file inside it is compressed, split into three encrypted parts, and each part goes to a different cloud account.'
              : 'Connect at least two cloud accounts first — SAND needs somewhere separate to put each part of a file.'}
            {canUpload && (
              <div style={{ marginTop: '16px' }}>
                <Button variant="primary" size="sm" onClick={() => setChoosing(true)}>↑ Choose files or a folder</Button>
              </div>
            )}
          </Empty>
        ) : (
          <EntryList {...listProps} onSelectAll={selectAll} onSelectNone={() => setSelected(new Set())} />
        )}
      </div>

      {dragging && (
        <div style={{
          position: 'absolute',
          inset: '10px',
          border: `2px dashed ${COLORS.accent}`,
          borderRadius: '10px',
          background: 'rgba(217, 119, 6, 0.08)',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          pointerEvents: 'none',
          zIndex: 20,
        }}>
          <div style={{ textAlign: 'center', padding: '0 20px', fontFamily: FONT.mono, color: COLORS.accentBright }}>
            <div style={{ fontSize: '30px', marginBottom: '10px' }}>↓</div>
            <div style={{ fontSize: '13px', wordBreak: 'break-word' }}>
              Drop files or a folder to split, encrypt and scatter into {path}
            </div>
          </div>
        </div>
      )}

      {choosing && (
        <ActionSheet
          title="Upload into this folder"
          subtitle={`Whatever you pick is compressed, split and scattered into ${path === '/' ? 'the vault' : path}. A folder arrives as a folder, with everything inside it in the place it was.`}
          onClose={() => setChoosing(false)}
          items={[
            {
              key: 'files',
              glyph: '🗎',
              label: 'Files',
              hint: 'One or several, picked by hand',
              /* Opened from inside the click that chose it, because a file
                 dialog only opens while the click that asked for it is still
                 being handled. The sheet closes itself afterwards. */
              onSelect: () => fileInput.current?.click(),
            },
            {
              key: 'folder',
              glyph: '🗀',
              label: 'Folder',
              hint: 'Everything inside it, however deep, keeping its shape',
              onSelect: () => dirInput.current?.click(),
            },
          ]}
        />
      )}

      {pending && (
        <UploadDestination
          picks={pending}
          path={path}
          providers={providers}
          defaults={defaultAccounts}
          defaultScheme={defaultScheme}
          onClose={() => setPending(null)}
          onChanged={onRefresh}
          onUpload={(accounts, scheme) => { setPending(null); uploadFiles(pending, accounts, scheme) }}
        />
      )}

      {importing && (
        <ImportFromMachine
          path={path}
          vault={vault}
          onClose={() => setImporting(false)}
          /* What arrived is in this folder now, so the listing on screen is no
             longer what is there. */
          onChanged={onRefresh}
        />
      )}

      {organizing && (
        <OrganizerMenu
          path={path}
          automation={automation}
          repoCount={repoCount}
          onClose={() => setOrganizing(false)}
          onPick={setTool}
        />
      )}

      {tool && (
        <OrganizerTool
          tool={tool}
          path={path}
          vault={vault}
          onClose={() => setTool(null)}
          /* A flatten moves files out of folders, a prune removes the
             folders, and clearing duplicates erases files; whichever it was,
             what is on screen is no longer what is there. The selection goes
             with it — half of what was ticked may have moved or gone. */
          onDone={() => { setSelected(new Set()); setDeeper([]); onRefresh() }}
          onSelect={pickFiles}
        />
      )}

      {films && (
        <FilmLookupSettings
          path={path}
          lookup={lookup}
          onClose={() => setFilms(false)}
          /* Turning it on changes what the rows offer; a sweep changes their
             pictures and their captions. Both are the listing. */
          onChanged={onRefresh}
        />
      )}

      {creatingFolder && (
        <NewFolderModal
          path={path}
          vault={vault}
          onClose={() => setCreatingFolder(false)}
          onCreated={() => { setCreatingFolder(false); onRefresh() }}
        />
      )}

      {bulk === 'delete' && (
        <BulkDelete
          items={chosen}
          onClose={() => setBulk(null)}
          /* What was deleted cannot stay ticked, and what survived a partial
             run is still there to be tried again. */
          onDone={() => { setSelected(new Set()); setDeeper([]); listProps.onRefresh() }}
        />
      )}

      {bulk === 'download' && (
        <BulkDownload items={chosen} onClose={() => setBulk(null)} />
      )}

      {bulk === 'folder' && (
        <MoveToFolder
          items={chosen}
          onClose={() => setBulk(null)}
          /* What moved is not in this folder any more, so it cannot stay
             ticked; what a partial run left behind is still here to try
             again. */
          onDone={() => { setSelected(new Set()); setDeeper([]); listProps.onRefresh() }}
        />
      )}

      {bulk === 'assign' && (
        <BulkAssign
          items={chosen}
          from={vault}
          targets={assignTargets}
          onClose={() => setBulk(null)}
          onDone={() => { setBulk(null); setSelected(new Set()); setDeeper([]); onRefresh() }}
          onError={onError}
        />
      )}

      {bulk === 'clouds' && (
        <RelocateClouds
          targets={chosen.map((entry) => (
            entry.kind === 'folder' ? { path: entry.path } : { id: entry.file.id }))}
          title={`Move ${chosen.length} item${chosen.length === 1 ? '' : 's'}`}
          subtitle="Pick the clouds every part of everything selected should live on"
          /* Nothing preselected: a selection has as many placements as it has
             files, so there is no "where it is now" to open on. The estimate
             says how much of it is already in place as soon as there is
             something to price. */
          current={[]}
          providers={providers}
          onClose={() => setBulk(null)}
          onDone={listProps.onRefresh}
        />
      )}
    </main>
  )
}

/* Search results are the same rows as a listing, with the folder each hit
   lives in spelled out — a name on its own means nothing once the answer can
   come from anywhere in the vault. */
function SearchResults({ term, results, searching, error, path, scoped, mobile, onScopeChange, listProps }) {
  if (error) return <Banner tone="error">{error}</Banner>
  if (!results) {
    return <div style={{ padding: '48px', textAlign: 'center' }}><Spinner size={20} /></div>
  }

  const hits = results.hits || []
  const scopeNote = path !== '/' && (
    <button
      onClick={() => onScopeChange(!scoped)}
      style={{
        background: 'none', border: 'none', padding: mobile ? '6px 0' : 0, cursor: 'pointer',
        minHeight: mobile ? '44px' : 0,
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.accent, textDecoration: 'underline',
      }}
    >{scoped ? 'search the whole vault' : `search only ${path}`}</button>
  )

  return (
    <>
      <div style={{
        display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap',
        marginBottom: '10px', fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
      }}>
        <span>
          {hits.length === 0 ? 'No matches' : `${results.matched} match${results.matched === 1 ? '' : 'es'}`}
          {' for '}<span style={{ color: COLORS.textDim }}>{term}</span>
          {scoped && path !== '/' && <> in <span style={{ color: COLORS.textDim }}>{path}</span></>}
          {searching && ' …'}
        </span>
        {scopeNote}
      </div>

      {results.truncated && (
        <Banner tone="info">
          Showing the closest {hits.length} of {results.matched} matches. Narrow the search to see the rest.
        </Banner>
      )}

      {hits.length === 0 ? (
        <Empty icon="⌕" title={`Nothing matches "${term}"`}>
          Names are matched anywhere, ignoring case. Use <code>*</code> and <code>?</code> for wildcards
          ("*.jpg"), or include a "/" to match a whole path ("photos/2024").
        </Empty>
      ) : (
        <EntryList {...listProps} />
      )}
    </>
  )
}

/* Whether this listing is being read as films — which decides the shape of a
   tile and the width of a row's controls.

   The switch is not the whole answer. A folder somebody has since turned it off
   for still holds rows with films against them, and those keep their poster and
   their control, so what is drawn follows what is actually there. */
function showsFilms(films, movies, entries) {
  return films || entries.some((entry) => (
    (entry.file && movies[entry.file.id]) || entry.art?.film))
}

/* Whether a row should offer film details at all, and what happens if it is
   asked for.

   Two things earn it: the folder is opted in and the file is something a player
   would take, or the file already has a film against it — which it can, in a
   folder somebody has since turned the switch back off. Anything else gets no
   control and no menu entry rather than one that answers "not here". */
function filmAction(films, movies, file, onFilm) {
  if (!onFilm) return undefined
  if (!movies[file.id] && !(films && isPlayable(file.mime, file.name))) return undefined
  return () => onFilm(file)
}

/* The listing, as rows or as tiles. Which one is a preference and nothing
   more: both are drawn from the same ordered entries and offer the same things
   to do with each of them. */
function EntryList(props) {
  return props.prefs.view === 'grid' ? <EntryGrid {...props} /> : <EntryTable {...props} />
}

function EntryTable({
  entries, thumbs, movies, films, mobile, providers, selecting, selected,
  onToggle, onSelectAll, onSelectNone,
  onNavigate, onPreview, onInspect, onFilm, onRefresh, onError,
  vault, subVaults,
}) {
  const all = entries.length > 0 && entries.every((entry) => selected.has(entry.key))
  /* One template for the whole table, headings included: a film folder's rows
     carry an extra control and the column has to be the same width everywhere. */
  const columns = showsFilms(films, movies, entries) ? FILM_COLUMNS : COLUMNS

  return (
    <div style={{
      border: `1px solid ${COLORS.border}`,
      borderRadius: '8px',
      overflow: 'hidden',
      background: COLORS.surface,
    }}>
      {/* Column headings only make sense over columns. The tick above them
          picks the lot, which is the one control here that is worth having on
          a phone too — hence the heading row appearing at all while
          selecting. */}
      {(!mobile || selecting) && (
        <div style={{
          display: 'flex',
          alignItems: 'center',
          gap: selecting ? (mobile ? '4px' : '6px') : 0,
          padding: mobile ? '4px 10px' : '9px 14px',
          borderBottom: `1px solid ${COLORS.border}`,
          fontFamily: FONT.mono,
          fontSize: '9.5px',
          fontWeight: 700,
          letterSpacing: '1.2px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
        }}>
          {selecting && (
            <SelectBox
              mobile={mobile}
              checked={all}
              label={all ? 'Select none' : 'Select everything here'}
              onChange={(checked) => (checked ? onSelectAll?.() : onSelectNone?.())}
            />
          )}
          {mobile ? (
            <span>{all ? 'Everything here is selected' : 'Select everything here'}</span>
          ) : (
            <div style={{
              display: 'grid', gridTemplateColumns: columns, gap: '12px',
              flex: 1, minWidth: 0,
            }}>
              <span>Name</span>
              <span>Size</span>
              <span>Modified</span>
              <span title="Which account holds each of the three parts">Parts</span>
              <span />
            </div>
          )}
        </div>
      )}

      {entries.map((entry) => (entry.kind === 'folder' ? (
        <FolderRow
          key={entry.key}
          vault={vault}
          subVaults={subVaults}
          name={entry.name}
          path={entry.path}
          location={entry.location}
          art={entry.art}
          mobile={mobile}
          providers={providers}
          columns={columns}
          selecting={selecting}
          selected={selected.has(entry.key)}
          onSelect={(checked, event) => onToggle(entry, checked, event)}
          onNavigate={onNavigate}
          onRefresh={onRefresh}
          onError={onError}
        />
      ) : (
        <FileRow
          key={entry.key}
          vault={vault}
          subVaults={subVaults}
          file={entry.file}
          location={entry.location}
          mobile={mobile}
          providers={providers}
          hasThumb={thumbs.has(entry.file.id)}
          film={movies[entry.file.id]}
          columns={columns}
          selecting={selecting}
          selected={selected.has(entry.key)}
          onSelect={(checked, event) => onToggle(entry, checked, event)}
          onPreview={() => onPreview(entry.file, thumbs.has(entry.file.id), movies[entry.file.id])}
          onInspect={() => onInspect(entry.file)}
          onFilm={filmAction(films, movies, entry.file, onFilm)}
          onRefresh={onRefresh}
          onError={onError}
        />
      )))}
    </div>
  )
}

function EntryGrid({
  entries, thumbs, movies, films, mobile, providers, selecting, selected, onToggle,
  onNavigate, onPreview, onInspect, onFilm, onRefresh, onError,
  vault, subVaults,
}) {
  /* Posters are two-by-three and photographs are not, so the shape belongs to
     the folder rather than to the tile: one shape per grid keeps the rows level
     and the folders in step with the films beside them. */
  const aspect = showsFilms(films, movies, entries) ? TILE_POSTER : TILE_SQUARE

  return (
    <div style={{
      display: 'grid',
      gridTemplateColumns: `repeat(auto-fill, minmax(${mobile ? TILE_MIN_PX.mobile : TILE_MIN_PX.desktop}px, 1fr))`,
      gap: mobile ? '8px' : '12px',
    }}>
      {entries.map((entry) => (entry.kind === 'folder' ? (
        <FolderTile
          key={entry.key}
          vault={vault}
          subVaults={subVaults}
          name={entry.name}
          path={entry.path}
          location={entry.location}
          art={entry.art}
          mobile={mobile}
          providers={providers}
          aspect={aspect}
          selecting={selecting}
          selected={selected.has(entry.key)}
          onSelect={(checked, event) => onToggle(entry, checked, event)}
          onNavigate={onNavigate}
          onRefresh={onRefresh}
          onError={onError}
        />
      ) : (
        <FileTile
          key={entry.key}
          vault={vault}
          subVaults={subVaults}
          file={entry.file}
          location={entry.location}
          mobile={mobile}
          providers={providers}
          hasThumb={thumbs.has(entry.file.id)}
          film={movies[entry.file.id]}
          aspect={aspect}
          selecting={selecting}
          selected={selected.has(entry.key)}
          onSelect={(checked, event) => onToggle(entry, checked, event)}
          onPreview={() => onPreview(entry.file, thumbs.has(entry.file.id), movies[entry.file.id])}
          onInspect={() => onInspect(entry.file)}
          onFilm={filmAction(films, movies, entry.file, onFilm)}
          onRefresh={onRefresh}
          onError={onError}
        />
      )))}
    </div>
  )
}

function NewFolderModal({ path, vault, onClose, onCreated }) {
  const [name, setName] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e) => {
    e.preventDefault()
    if (!name.trim()) return
    setBusy(true)
    setError(null)
    try {
      await api.createFolder(joinPath(path, name.trim()), vault)
      onCreated()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal title="New folder" subtitle={`Inside ${path}`} onClose={onClose} width={400}>
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <input
          autoFocus
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Folder name"
          style={{
            width: '100%',
            padding: '10px 12px',
            marginBottom: '16px',
            background: COLORS.bg,
            border: `1px solid ${COLORS.border}`,
            borderRadius: '6px',
            color: COLORS.text,
            fontFamily: FONT.mono,
            fontSize: '13px',
            outline: 'none',
            boxSizing: 'border-box',
          }}
        />
        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
          <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy || !name.trim()}>Create</Button>
        </div>
      </form>
    </Modal>
  )
}

/* The vaults inside this one, drawn at the root of the main vault.

   A locked one is still listed. That is the point of showing them at all: you
   are meant to see that there is a place called Taxes and be asked for a
   password, rather than have it be invisible until you remember to go looking
   in settings. Which ones get here at all is settled before this — a sub vault
   left unticked in the panel is not passed in. */
function SubVaultStrip({ subVaults, mobile, onOpen }) {
  return (
    <div style={{ marginBottom: '14px' }}>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '9px',
        fontWeight: 600,
        letterSpacing: '1.2px',
        textTransform: 'uppercase',
        color: COLORS.textMuted,
        margin: '0 0 7px 2px',
      }}>Sub vaults</div>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '8px' }}>
        {subVaults.map((sub) => (
          <button
            key={sub.id}
            type="button"
            onClick={() => onOpen(sub)}
            title={sub.unlocked
              ? `Open ${sub.label}`
              : `${sub.label} is locked — opening it asks for its own password`}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '8px',
              padding: '9px 12px',
              minHeight: mobile ? '46px' : '38px',
              flex: mobile ? '1 1 calc(50% - 8px)' : '0 0 auto',
              background: COLORS.surfaceRaised,
              border: `1px dashed ${sub.unlocked ? COLORS.borderBright : COLORS.border}`,
              borderRadius: '8px',
              cursor: 'pointer',
              textAlign: 'left',
              color: COLORS.text,
              fontFamily: FONT.mono,
              fontSize: '12px',
            }}
          >
            <span aria-hidden="true" style={{ opacity: sub.unlocked ? 1 : 0.6 }}>
              {sub.unlocked ? '🔓' : '🔒'}
            </span>
            <span style={{ minWidth: 0 }}>
              <span style={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {sub.label}
              </span>
              <span style={{ display: 'block', fontSize: '9px', color: COLORS.textMuted, marginTop: '1px' }}>
                {sub.files} file{sub.files === 1 ? '' : 's'}{sub.unlocked ? '' : ' · locked'}
              </span>
            </span>
          </button>
        ))}
      </div>
    </div>
  )
}
