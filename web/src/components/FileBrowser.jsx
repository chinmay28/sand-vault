import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api, joinPath } from '../api'
import { sortFiles, sortFolders, sortHits, useViewPrefs } from '../view'
import { Banner, Button, Empty, Modal, Spinner } from './ui'
import { UploadDestination, RelocateClouds } from './CloudSelect'
import { makeThumbnail } from '../thumbs'
import { COLUMNS, FileRow, FileTile, FolderRow, FolderTile, SelectBox } from './FileEntry'
import { Breadcrumbs, FolderHeader, NavCluster, SearchField, SelectionBar, ViewControls } from './Toolbar'
import { BulkDelete, BulkDownload } from './BulkActions'

/* How long to sit on a keystroke before asking the server. Long enough that
   typing a word is one query rather than six, short enough to feel live. */
const SEARCH_DEBOUNCE_MS = 180

/* How wide a tile is allowed to get before the grid grows another column. Two
   columns on the narrowest phone anyone still carries, and as many as fit on a
   desk — which is the whole reason to be looking at tiles rather than rows. */
const TILE_MIN_PX = { mobile: 108, desktop: 132 }

export default function FileBrowser({
  nav, listing, loading, error, providers, defaultAccounts, mobile,
  onRefresh, onPreview, onInspect, onError,
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
  const fileInput = useRef(null)
  const dragDepth = useRef(0)
  // Where the last tick was put, so a shift-click has a range to reach back to.
  const anchor = useRef(null)

  const path = nav.path
  const canUpload = providers.length > 0
  const searchTerm = query.trim()
  // Searching from inside a folder looks there first; the results header can
  // widen it to the whole vault.
  const searchScope = scoped ? path : '/'

  // Walking into a folder is a different question from the one being asked, so
  // the search ends when navigation begins — and on a phone the field it was
  // typed into gives the toolbar back.
  useEffect(() => { setQuery(''); setSearchOpen(false) }, [path])

  const runSearch = useCallback((signal) => api.search(searchTerm, { path: searchScope, signal })
    .then((resp) => { setResults(resp); setSearchError(null) })
    .catch((err) => {
      if (err.name === 'AbortError') return
      setResults(null)
      setSearchError(err.message)
    }), [searchTerm, searchScope])

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
    if (searchTerm) {
      return sortHits(results?.hits || [], prefs).map((hit) => (hit.type === 'folder'
        ? { kind: 'folder', key: `dir:${hit.path}`, name: hit.name, path: hit.path, location: hit.dir }
        : { kind: 'file', key: `file:${hit.file.id}`, name: hit.file.name, file: hit.file, location: hit.dir }))
    }
    return [
      ...sortFolders(listing?.folders, prefs).map((name) => ({
        kind: 'folder', key: `dir:${joinPath(path, name)}`, name, path: joinPath(path, name),
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

  /* A selection is about the things in front of you, so walking somewhere else
     — or asking a different question of the index — ends it rather than
     carrying a set of hidden rows along to be acted on by surprise. */
  useEffect(() => { setSelected(new Set()); anchor.current = null }, [path, searchTerm])

  const chosen = useMemo(
    () => entries.filter((entry) => selected.has(entry.key)), [entries, selected])
  const chosenFiles = chosen.filter((entry) => entry.kind !== 'folder')

  const selectAll = useCallback(() => {
    setSelected(new Set(entries.map((entry) => entry.key)))
  }, [entries])

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

  /* Files are held here until the clouds they are going to have been settled.
     Nothing is read or sent in the meantime — the picker is between choosing
     files and uploading them, not a step inside the upload. */
  const chooseFiles = useCallback((files) => {
    if (!files.length) return
    if (!canUpload) {
      onError('Connect a cloud account before uploading — there is nowhere to put the parts yet.')
      return
    }
    setPending(files)
  }, [canUpload, onError])

  const uploadFiles = useCallback(async (files, accounts) => {
    const batch = { id: Math.random().toString(36).slice(2), names: [...files].map((f) => f.name), progress: 0 }
    setUploads((prev) => [...prev, batch])

    try {
      /* Made here, before anything is sent, because this is the only place the
         plaintext file exists in a browser. Each one resolves to null rather
         than throwing, so a format we cannot draw never holds up its upload. */
      const thumbnails = await Promise.all(
        [...files].map((file) => makeThumbnail(file, file.type, file.name)))

      const resp = await api.upload(files, path, {
        accounts,
        thumbs: thumbnails,
        onProgress: (fraction) => setUploads((prev) =>
          prev.map((u) => (u.id === batch.id ? { ...u, progress: fraction } : u))),
      })

      const failures = (resp.results || []).filter((r) => !r.ok)
      const notes = (resp.results || []).flatMap((r) => (r.warnings || []).map((w) => `${r.name}: ${w}`))
      if (failures.length) {
        onError(failures.map((f) => `${f.name}: ${f.error}`).join('\n'))
      }
      if (notes.length) setWarnings((prev) => [...prev, ...notes])
      onRefresh()
    } catch (err) {
      onError(err.message)
    } finally {
      setUploads((prev) => prev.filter((u) => u.id !== batch.id))
    }
  }, [path, onError, onRefresh])

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
  const onDrop = (e) => {
    e.preventDefault()
    dragDepth.current = 0
    setDragging(false)
    chooseFiles(Array.from(e.dataTransfer.files || []))
  }

  const listProps = {
    entries,
    thumbs,
    prefs,
    mobile,
    providers,
    selecting,
    selected,
    onToggle: toggle,
    onNavigate: nav.navigate,
    onPreview,
    onInspect,
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
            stats={stats}
            prefs={prefs}
            view={view}
            selecting={selecting}
            canUpload={canUpload}
            onSelecting={(on) => { setSelecting(on); if (!on) setSelected(new Set()) }}
            onSearch={() => setSearchOpen(true)}
            onNewFolder={() => setCreatingFolder(true)}
            onUpload={() => fileInput.current?.click()}
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
              <Breadcrumbs path={path} mobile={false} onNavigate={nav.navigate} />
              <ViewControls
                mobile={false}
                prefs={prefs}
                view={view}
                selecting={selecting}
                onSelecting={(on) => { setSelecting(on); if (!on) setSelected(new Set()) }}
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
              <Button size="sm" variant="primary" onClick={() => fileInput.current?.click()} disabled={!canUpload}>
                ↑ Upload
              </Button>
            </div>
          </>
        )}

        <input
          ref={fileInput}
          type="file"
          multiple
          style={{ display: 'none' }}
          onChange={(e) => { chooseFiles(Array.from(e.target.files || [])); e.target.value = '' }}
        />
      </div>

      {selecting && (
        <SelectionBar
          mobile={mobile}
          count={chosen.length}
          total={entries.length}
          files={chosenFiles.length}
          allSelected={entries.length > 0 && chosen.length === entries.length}
          busy={!!bulk}
          onAll={selectAll}
          onNone={() => setSelected(new Set())}
          onDone={() => { setSelecting(false); setSelected(new Set()) }}
          onDownload={() => setBulk('download')}
          onMove={() => setBulk('move')}
          onDelete={() => setBulk('delete')}
        />
      )}

      <div style={{ flex: 1, overflowY: 'auto', padding: mobile ? '12px 12px 32px' : '16px 20px 40px' }}>
        {error && <Banner tone="error">{error}</Banner>}
        {warnings.length > 0 && (
          <Banner tone="warn" onDismiss={() => setWarnings([])}>
            {warnings.map((w, i) => <div key={i}>{w}</div>)}
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
                {upload.names.length === 1 ? upload.names[0] : `${upload.names.length} files`}
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
              ? 'Drop files anywhere in this pane, or use Upload. Each file is compressed, split into three encrypted parts, and each part goes to a different cloud account.'
              : 'Connect at least two cloud accounts first — SAND needs somewhere separate to put each part of a file.'}
            {canUpload && (
              <div style={{ marginTop: '16px' }}>
                <Button variant="primary" size="sm" onClick={() => fileInput.current?.click()}>↑ Choose files</Button>
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
              Drop to split, encrypt and scatter into {path}
            </div>
          </div>
        </div>
      )}

      {pending && (
        <UploadDestination
          files={pending}
          path={path}
          providers={providers}
          defaults={defaultAccounts}
          onClose={() => setPending(null)}
          onChanged={onRefresh}
          onUpload={(accounts) => { setPending(null); uploadFiles(pending, accounts) }}
        />
      )}

      {creatingFolder && (
        <NewFolderModal
          path={path}
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
          onDone={() => { setSelected(new Set()); listProps.onRefresh() }}
        />
      )}

      {bulk === 'download' && (
        <BulkDownload items={chosen} onClose={() => setBulk(null)} />
      )}

      {bulk === 'move' && (
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

/* The listing, as rows or as tiles. Which one is a preference and nothing
   more: both are drawn from the same ordered entries and offer the same things
   to do with each of them. */
function EntryList(props) {
  return props.prefs.view === 'grid' ? <EntryGrid {...props} /> : <EntryTable {...props} />
}

function EntryTable({
  entries, thumbs, mobile, providers, selecting, selected, onToggle, onSelectAll, onSelectNone,
  onNavigate, onPreview, onInspect, onRefresh, onError,
}) {
  const all = entries.length > 0 && entries.every((entry) => selected.has(entry.key))

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
              display: 'grid', gridTemplateColumns: COLUMNS, gap: '12px',
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
          name={entry.name}
          path={entry.path}
          location={entry.location}
          mobile={mobile}
          providers={providers}
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
          file={entry.file}
          location={entry.location}
          mobile={mobile}
          providers={providers}
          hasThumb={thumbs.has(entry.file.id)}
          selecting={selecting}
          selected={selected.has(entry.key)}
          onSelect={(checked, event) => onToggle(entry, checked, event)}
          onPreview={() => onPreview(entry.file, thumbs.has(entry.file.id))}
          onInspect={() => onInspect(entry.file)}
          onRefresh={onRefresh}
          onError={onError}
        />
      )))}
    </div>
  )
}

function EntryGrid({
  entries, thumbs, mobile, providers, selecting, selected, onToggle,
  onNavigate, onPreview, onInspect, onRefresh, onError,
}) {
  return (
    <div style={{
      display: 'grid',
      gridTemplateColumns: `repeat(auto-fill, minmax(${mobile ? TILE_MIN_PX.mobile : TILE_MIN_PX.desktop}px, 1fr))`,
      gap: mobile ? '8px' : '12px',
    }}>
      {entries.map((entry) => (entry.kind === 'folder' ? (
        <FolderTile
          key={entry.key}
          name={entry.name}
          path={entry.path}
          location={entry.location}
          mobile={mobile}
          providers={providers}
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
          file={entry.file}
          location={entry.location}
          mobile={mobile}
          providers={providers}
          hasThumb={thumbs.has(entry.file.id)}
          selecting={selecting}
          selected={selected.has(entry.key)}
          onSelect={(checked, event) => onToggle(entry, checked, event)}
          onPreview={() => onPreview(entry.file, thumbs.has(entry.file.id))}
          onInspect={() => onInspect(entry.file)}
          onRefresh={onRefresh}
          onError={onError}
        />
      )))}
    </div>
  )
}

function NewFolderModal({ path, onClose, onCreated }) {
  const [name, setName] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e) => {
    e.preventDefault()
    if (!name.trim()) return
    setBusy(true)
    setError(null)
    try {
      await api.createFolder(joinPath(path, name.trim()))
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
