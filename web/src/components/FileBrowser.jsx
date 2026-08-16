import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, fileIcon, formatBytes, formatDate, isPlayable } from '../theme'
import { api, joinPath } from '../api'
import { useDownload } from '../download'
import { ActionSheet, Banner, Button, ConfirmDialog, Empty, IconButton, Modal, Spinner } from './ui'
import StreamLink from './StreamLink'
import ConvertFile from './ConvertFile'
import { UploadDestination } from './CloudSelect'
import { makeThumbnail } from '../thumbs'

/* Name, size, modified, parts, actions. The four fixed columns come to nearly
   500px, which is why the phone layout stacks instead of shrinking them. */
const COLUMNS = 'minmax(0,1fr) 92px 150px 132px 108px'

/* How long to sit on a keystroke before asking the server. Long enough that
   typing a word is one query rather than six, short enough to feel live. */
const SEARCH_DEBOUNCE_MS = 180

export default function FileBrowser({
  path, listing, loading, error, providers, defaultAccounts, mobile,
  onNavigate, onRefresh, onPreview, onInspect, onError,
}) {
  const [dragging, setDragging] = useState(false)
  const [pending, setPending] = useState(null)
  const [uploads, setUploads] = useState([])
  const [warnings, setWarnings] = useState([])
  const [creatingFolder, setCreatingFolder] = useState(false)
  const [query, setQuery] = useState('')
  const [scoped, setScoped] = useState(true)
  const [results, setResults] = useState(null)
  const [searching, setSearching] = useState(false)
  const [searchError, setSearchError] = useState(null)
  const fileInput = useRef(null)
  const dragDepth = useRef(0)

  const canUpload = providers.length > 0
  const searchTerm = query.trim()
  // Searching from inside a folder looks there first; the results header can
  // widen it to the whole vault.
  const searchScope = scoped ? path : '/'

  // Walking into a folder is a different question from the one being asked, so
  // the search ends when navigation begins.
  useEffect(() => { setQuery('') }, [path])

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
      const thumbs = await Promise.all(
        [...files].map((file) => makeThumbnail(file, file.type, file.name)))

      const resp = await api.upload(files, path, {
        accounts,
        thumbs,
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

  const segments = path === '/' ? [] : path.slice(1).split('/')

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
        alignItems: 'center',
        gap: mobile ? '8px' : '10px',
        padding: mobile ? '10px 12px' : '14px 20px',
        borderBottom: `1px solid ${COLORS.border}`,
        flexWrap: 'wrap',
      }}>
        {/* On a phone the trail takes a row of its own and the two actions
            split the one below it, rather than all three fighting for it. */}
        <nav style={{
          flex: 1, minWidth: mobile ? '100%' : '180px', display: 'flex', alignItems: 'center',
          gap: '5px', fontFamily: FONT.mono, fontSize: '12px', flexWrap: 'wrap',
        }}>
          <Crumb label="▣ /" mobile={mobile} onClick={() => onNavigate('/')} active={path === '/'} />
          {segments.map((segment, i) => {
            const target = '/' + segments.slice(0, i + 1).join('/')
            return (
              <React.Fragment key={target}>
                <span style={{ color: COLORS.textMuted }}>/</span>
                <Crumb label={segment} mobile={mobile} onClick={() => onNavigate(target)} active={i === segments.length - 1} />
              </React.Fragment>
            )
          })}
        </nav>

        <SearchField
          value={query}
          busy={searching}
          mobile={mobile}
          onChange={setQuery}
        />

        {/* The two actions split the phone's row, each ending up wider than a
            thumb and taller than the 44px floor. */}
        <Button size={mobile ? 'md' : 'sm'} onClick={() => setCreatingFolder(true)}
          style={mobile ? { flex: 1, justifyContent: 'center', minHeight: '46px' } : null}>+ Folder</Button>
        <Button size={mobile ? 'md' : 'sm'} variant="primary" onClick={() => fileInput.current?.click()} disabled={!canUpload}
          style={mobile ? { flex: 1, justifyContent: 'center', minHeight: '46px' } : null}>
          ↑ Upload
        </Button>
        <input
          ref={fileInput}
          type="file"
          multiple
          style={{ display: 'none' }}
          onChange={(e) => { chooseFiles(Array.from(e.target.files || [])); e.target.value = '' }}
        />
      </div>

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
            onNavigate={onNavigate}
            onPreview={onPreview}
            onInspect={onInspect}
            onRefresh={refreshSearch}
            onError={onError}
          />
        ) : loading && !listing ? (
          <div style={{ padding: '48px', textAlign: 'center' }}><Spinner size={20} /></div>
        ) : (
          <FileTable
            path={path}
            listing={listing}
            canUpload={canUpload}
            mobile={mobile}
            onNavigate={onNavigate}
            onPreview={onPreview}
            onInspect={onInspect}
            onRefresh={onRefresh}
            onError={onError}
            onPickFiles={() => fileInput.current?.click()}
          />
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
    </main>
  )
}

/* The one control that reaches past the folder you are standing in. The index
   it queries only exists in the open vault, so this is also the only thing in
   the app that can answer "where did I put that?" at all. */
function SearchField({ value, busy, mobile, onChange }) {
  return (
    <div style={{
      position: 'relative',
      display: 'flex',
      alignItems: 'center',
      // On a phone the field takes a row of its own, above the two actions.
      flex: mobile ? '1 0 100%' : '0 1 240px',
      minWidth: mobile ? '100%' : '150px',
    }}>
      <span style={{
        position: 'absolute', left: mobile ? '11px' : '9px', color: COLORS.textMuted,
        fontSize: '13px', pointerEvents: 'none',
      }}>⌕</span>
      <input
        type="search"
        value={value}
        aria-label="Search files and folders"
        placeholder="Search files and folders"
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => { if (e.key === 'Escape') onChange('') }}
        style={{
          width: '100%',
          // Tall enough to be a target in its own right on a phone, where the
          // clear button beside it also has to clear 44px.
          minHeight: mobile ? '46px' : 0,
          padding: mobile ? '8px 48px 8px 30px' : '6px 28px 6px 24px',
          background: COLORS.bg,
          border: `1px solid ${COLORS.border}`,
          borderRadius: '6px',
          color: COLORS.text,
          fontFamily: FONT.mono,
          fontSize: mobile ? '13px' : '12px',
          outline: 'none',
          boxSizing: 'border-box',
          // Otherwise Safari renders a search field as its own rounded pill
          // and ignores most of the above. (Its clear button is dropped in
          // App.jsx, which is where a pseudo-element can be reached.)
          WebkitAppearance: 'none',
        }}
      />
      {busy && (
        <span style={{ position: 'absolute', right: mobile ? '14px' : '9px', display: 'flex' }}>
          <Spinner size={mobile ? 13 : 11} />
        </span>
      )}
      {!busy && value && (
        <span style={{ position: 'absolute', right: mobile ? '2px' : '4px', display: 'flex' }}>
          <IconButton
            glyph="✕"
            label="Clear the search"
            tone="muted"
            size={mobile ? 44 : 20}
            onClick={() => onChange('')}
            style={{ fontSize: mobile ? '13px' : '11px' }}
          />
        </span>
      )}
    </div>
  )
}

/* Search results are the same rows as a listing, with the folder each hit
   lives in spelled out — a name on its own means nothing once the answer can
   come from anywhere in the vault. */
function SearchResults({
  term, results, searching, error, path, scoped, mobile,
  onScopeChange, onNavigate, onPreview, onInspect, onRefresh, onError,
}) {
  if (error) return <Banner tone="error">{error}</Banner>
  if (!results) {
    return <div style={{ padding: '48px', textAlign: 'center' }}><Spinner size={20} /></div>
  }

  const hits = results.hits || []
  const thumbs = new Set(results.thumbs || [])
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
        <div style={{
          border: `1px solid ${COLORS.border}`,
          borderRadius: '8px',
          overflow: 'hidden',
          background: COLORS.surface,
        }}>
          {hits.map((hit) => (hit.type === 'folder' ? (
            <FolderRow
              key={`dir:${hit.path}`}
              name={hit.name}
              path={hit.path}
              location={hit.dir}
              mobile={mobile}
              onNavigate={onNavigate}
              onRefresh={onRefresh}
              onError={onError}
            />
          ) : (
            <FileRow
              key={hit.file.id}
              file={hit.file}
              location={hit.dir}
              mobile={mobile}
              hasThumb={thumbs.has(hit.file.id)}
              onPreview={() => onPreview(hit.file, thumbs.has(hit.file.id))}
              onInspect={() => onInspect(hit.file)}
              onRefresh={onRefresh}
              onError={onError}
            />
          )))}
        </div>
      )}
    </>
  )
}

function Crumb({ label, mobile, onClick, active }) {
  return (
    <button
      onClick={onClick}
      style={{
        background: 'none',
        border: 'none',
        // Walking back up the tree is a tap like any other, so the trail gets
        // room to be tapped rather than being treated as decoration.
        minHeight: mobile ? '44px' : 0,
        minWidth: mobile ? '44px' : 0,
        padding: mobile ? '4px 10px' : '2px 4px',
        borderRadius: '6px',
        cursor: 'pointer',
        fontFamily: FONT.mono,
        fontSize: mobile ? '13px' : '12px',
        color: active ? COLORS.accent : COLORS.textDim,
        fontWeight: active ? 700 : 400,
      }}
    >{label}</button>
  )
}

function FileTable({ path, listing, canUpload, mobile, onNavigate, onPreview, onInspect, onRefresh, onError, onPickFiles }) {
  const folders = listing?.folders || []
  const files = listing?.files || []
  // Which rows have a stored picture. The listing says so outright, so no row
  // asks for a thumbnail that was never made.
  const thumbs = new Set(listing?.thumbs || [])

  if (!folders.length && !files.length) {
    return (
      <Empty icon="◇" title={path === '/' ? 'Nothing stored yet' : 'This folder is empty'}>
        {canUpload
          ? 'Drop files anywhere in this pane, or use Upload. Each file is compressed, split into three encrypted parts, and each part goes to a different cloud account.'
          : 'Connect at least two cloud accounts first — SAND needs somewhere separate to put each part of a file.'}
        {canUpload && (
          <div style={{ marginTop: '16px' }}>
            <Button variant="primary" size="sm" onClick={onPickFiles}>↑ Choose files</Button>
          </div>
        )}
      </Empty>
    )
  }

  return (
    <div style={{
      border: `1px solid ${COLORS.border}`,
      borderRadius: '8px',
      overflow: 'hidden',
      background: COLORS.surface,
    }}>
      {/* Column headings only make sense over columns. */}
      {!mobile && (
        <div style={{
          display: 'grid',
          gridTemplateColumns: COLUMNS,
          gap: '12px',
          padding: '9px 14px',
          borderBottom: `1px solid ${COLORS.border}`,
          fontFamily: FONT.mono,
          fontSize: '9.5px',
          fontWeight: 700,
          letterSpacing: '1.2px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
        }}>
          <span>Name</span>
          <span>Size</span>
          <span>Modified</span>
          <span title="Which account holds each of the three parts">Parts</span>
          <span />
        </div>
      )}

      {folders.map((folder) => (
        <FolderRow
          key={`dir:${folder}`}
          name={folder}
          path={joinPath(path, folder)}
          mobile={mobile}
          onNavigate={onNavigate}
          onRefresh={onRefresh}
          onError={onError}
        />
      ))}

      {files.map((file) => (
        <FileRow
          key={file.id}
          file={file}
          mobile={mobile}
          hasThumb={thumbs.has(file.id)}
          onPreview={() => onPreview(file, thumbs.has(file.id))}
          onInspect={() => onInspect(file)}
          onRefresh={onRefresh}
          onError={onError}
        />
      ))}
    </div>
  )
}

function Row({ children, mobile }) {
  const [hover, setHover] = useState(false)
  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        // Too narrow for columns, so the row becomes a stack: the name and its
        // menu on the first line, the details underneath. It is also roomier
        // than the desktop row — a row a fingertip has to hit needs the height
        // more than the screen needs to hold one more file.
        ...(mobile
          ? { display: 'flex', flexDirection: 'column', rowGap: '2px' }
          : { display: 'grid', gridTemplateColumns: COLUMNS, gap: '12px', alignItems: 'center' }),
        padding: mobile ? '6px 10px 8px' : '9px 14px',
        borderBottom: `1px solid ${COLORS.border}22`,
        background: hover ? COLORS.surfaceHover : 'transparent',
        fontFamily: FONT.mono,
        fontSize: '11.5px',
        color: COLORS.textDim,
        minHeight: mobile ? '64px' : '38px',
      }}
    >{children}</div>
  )
}

/* The picture in front of a file's name. It is a stored thumbnail — a small
   JPEG the vault keeps a folder at a time — so drawing one costs nothing like
   rebuilding the file it came from.

   `size` is the edge in pixels: 52 on a phone, where the row is a stack and
   the tile is the left column of it, and 26 on a desktop, where it stands in
   for the emoji inside the Name column without changing the row's height.

   It falls back to that same emoji, and does so on any failure — a file
   uploaded before thumbnails existed, an account that has gone quiet, a pack
   that could not be read. The list has always been readable without pictures. */
function Thumb({ id, icon, size, expected }) {
  const [failed, setFailed] = useState(false)

  // A new file in the same row position must not inherit the old one's state.
  useEffect(() => { setFailed(false) }, [id])

  if (!expected || failed) {
    return <span style={{ flexShrink: 0, fontSize: size >= 40 ? '26px' : '15px' }}>{icon}</span>
  }

  return (
    <img
      src={api.thumbURL(id)}
      alt=""
      width={size}
      height={size}
      loading="lazy"
      decoding="async"
      onError={() => setFailed(true)}
      style={{
        width: `${size}px`,
        height: `${size}px`,
        flexShrink: 0,
        objectFit: 'cover',
        borderRadius: size >= 40 ? '6px' : '4px',
        background: COLORS.surfaceRaised,
        border: `1px solid ${COLORS.border}`,
      }}
    />
  )
}

/* The tappable name. On a phone it claims the whole first line and a 44px
   height, so opening a file means hitting the row rather than the glyph.
   `location` is the folder the row was found in, which only a search result
   has to say. */
function NameButton({ mobile, icon, label, location, chevron, disabled, title, onClick }) {
  return (
    <button
      onClick={onClick}
      title={title}
      disabled={disabled}
      style={{
        display: 'flex', alignItems: 'center', gap: '9px',
        flex: 1, minWidth: 0,
        minHeight: mobile ? '44px' : 0,
        background: 'none', border: 'none',
        padding: mobile ? '0 2px' : 0,
        borderRadius: '8px',
        cursor: disabled ? 'not-allowed' : 'pointer',
        fontFamily: FONT.mono,
        fontSize: mobile ? '13.5px' : '12.5px',
        color: disabled ? COLORS.error : COLORS.text,
        overflow: 'hidden', textAlign: 'left',
      }}
    >
      {chevron !== undefined
        ? <span style={{ color: COLORS.accent, flexShrink: 0 }}>{chevron}</span>
        /* Lines file names up under the folder rows' ▸ chevron. */
        : <span style={{ width: '12px', flexShrink: 0 }} />}
      <span style={{ flexShrink: 0 }}>{icon}</span>
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{label}</span>
      {location && (
        /* Shrinks before the name does: a truncated name is worse than a
           truncated path. */
        <span
          title={location}
          style={{
            minWidth: 0, flexShrink: 1,
            color: COLORS.textMuted, fontSize: mobile ? '11px' : '10.5px',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
          }}
        >in {location}</span>
      )}
    </button>
  )
}

function FileRow({ file, location, mobile, hasThumb, onPreview, onInspect, onRefresh, onError }) {
  const [busy, setBusy] = useState(false)
  const [download, downloading] = useDownload(onError)
  const [menu, setMenu] = useState(false)
  const [confirming, setConfirming] = useState(false)
  /* null, or 'play' when the stream dialog should reach for VLC on the way in
     and 'link' when it should just show the address. */
  const [streaming, setStreaming] = useState(null)
  const [converting, setConverting] = useState(null)
  /* A file stored before chunked storage existed. It cannot be read at an
     offset, so nothing opens or streams it until it has been converted — the
     row says so rather than letting a click fail. */
  const legacy = !file.chunk_count
  const degraded = file.shards.length < 3
  const dead = file.shards.length < 2
  // Only what a player is any use for gets offered one.
  const playable = isPlayable(file.mime, file.name)

  const remove = async () => {
    setBusy(true)
    try {
      const resp = await api.deleteFile(file.id)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      setConfirming(false)
      onRefresh()
    } catch (err) {
      onError(err.message)
      setConfirming(false)
    } finally {
      setBusy(false)
    }
  }

  const icon = fileIcon(file.mime, file.name)
  const openTitle = dead
    ? 'Too few parts remain to rebuild this file'
    : legacy ? 'Stored in the old format — convert it before it can be opened' : 'Open'

  const name = (
    <NameButton
      mobile={mobile}
      /* On a desktop the picture stands exactly where the emoji did, so the
         Name column keeps its width and the row its height. */
      icon={<Thumb id={file.id} icon={icon} size={26} expected={hasThumb} />}
      label={file.name}
      location={location}
      disabled={dead}
      title={openTitle}
      onClick={legacy ? () => setConverting('open') : onPreview}
    />
  )

  /* On a phone the badges are a read-out and nothing more. A third target in a
     row that already has a name and a menu would have to be either too small
     to hit or tall enough to push the next file off the screen — and the menu
     already offers the same inspector by name. On a desktop the badges stay
     the shortcut they have always been. */
  const partsBadges = (
    <>
      {[1, 2, 3].map((part) => {
        const shard = file.shards.find((s) => s.part === part)
        return (
          <span
            key={part}
            title={shard ? `Part ${part} on ${shard.provider_name}` : `Part ${part} not stored`}
            style={{
              /* The badges share the phone's second line with the size and
                 the date now that the picture has taken the left column, so
                 they are the desktop's width there too — that is the
                 difference between reading the date and truncating it. */
              width: '19px',
              height: mobile ? '16px' : '15px',
              borderRadius: '3px',
              fontFamily: FONT.mono,
              fontSize: mobile ? '9.5px' : '8.5px',
              fontWeight: 700,
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              flexShrink: 0,
              color: shard ? COLORS.bg : COLORS.textMuted,
              background: shard ? accountColor(shard.provider_id) : 'transparent',
              border: shard ? 'none' : `1px dashed ${COLORS.border}`,
            }}
          >{part}</span>
        )
      })}
      {degraded && (
        <span style={{ marginLeft: '3px', color: dead ? COLORS.error : COLORS.warn, fontSize: '11px' }}>
          {dead ? '✗' : '!'}
        </span>
      )}
    </>
  )

  const parts = mobile ? (
    <span
      title={`${file.shards.length} of 3 parts stored`}
      style={{ display: 'flex', alignItems: 'center', gap: '4px', flexShrink: 0 }}
    >{partsBadges}</span>
  ) : (
    <button
      onClick={onInspect}
      title="Where the parts live"
      aria-label="Where the parts live"
      style={{
        display: 'flex', alignItems: 'center', gap: '3px',
        background: 'none', border: 'none', padding: 0,
        borderRadius: '6px', cursor: 'pointer', flexShrink: 0,
      }}
    >{partsBadges}</button>
  )

  /* A pointer can pick between two 34px squares. A fingertip cannot, and one
     of them deletes the file everywhere, so the phone gets a single menu
     button instead and spells the choices out in a sheet. */
  const actions = mobile ? (
    <IconButton
      glyph="⋯"
      label={`Actions for ${file.name}`}
      onClick={() => setMenu(true)}
      size={44}
      style={{ background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, fontSize: '18px' }}
    />
  ) : (
    <span style={{ display: 'flex', gap: '2px', justifyContent: 'flex-end', flexShrink: 0 }}>
      {/* Three controls is what the column has room for, so this one is on the
          rows it means something on: a player is no use on a PDF. */}
      {legacy ? (
        <IconButton
          glyph="◈"
          label={`Convert ${file.name}`}
          title="Stored in the old format — convert it to open or stream it"
          tone="muted"
          onClick={() => setConverting('open')}
        />
      ) : playable && !dead && (
        <IconButton
          glyph="▶"
          label={`Stream ${file.name}`}
          title="Open in VLC, or copy the address"
          onClick={() => setStreaming('play')}
        />
      )}
      {/* Every row carries this control, so the spoken name says which file it
          belongs to rather than repeating the same sentence down the list. */}
      <IconButton
        glyph={downloading ? '…' : '↓'}
        label={`Download ${file.name}`}
        title="Download the rebuilt, decrypted file"
        disabled={downloading}
        onClick={() => download(file)}
      />
      <IconButton
        glyph={busy ? '…' : '✕'}
        label="Delete everywhere"
        tone="muted"
        disabled={busy}
        onClick={() => setConfirming(true)}
      />
    </span>
  )

  const dialogs = (
    <>
      {menu && (
        <ActionSheet
          title={file.name}
          subtitle={`${formatBytes(file.size)} · ${formatDate(file.modified_at)}`}
          onClose={() => setMenu(false)}
          items={[
            legacy ? {
              key: 'convert',
              glyph: '◈',
              label: 'Convert to chunks',
              hint: 'Stored in the old format, which cannot be opened or streamed until converted',
              disabled: dead,
              onSelect: () => setConverting('open'),
            } : {
              key: 'open',
              glyph: '◱',
              label: 'Open',
              hint: dead ? 'Too few parts remain to rebuild this file' : 'Gather the parts and rebuild it here',
              disabled: dead,
              onSelect: onPreview,
            },
            // Sits under Open because it is the same intent aimed elsewhere:
            // watch this, but in the player that can actually seek it.
            playable && !legacy && {
              key: 'stream',
              glyph: '▶',
              label: 'Stream in VLC',
              hint: dead ? 'Too few parts remain to rebuild this file' : 'Open it in VLC and start playing',
              disabled: dead,
              onSelect: () => setStreaming('play'),
            },
            !legacy && {
              key: 'copy-link',
              glyph: '⧉',
              label: 'Copy the address',
              hint: 'A link any player or app can open',
              disabled: dead,
              onSelect: () => setStreaming('link'),
            },
            !legacy && {
              key: 'download',
              glyph: '↓',
              // The sheet closes on the way out and the fetch carries on
              // behind it — a home-screen app has no tab to park a download
              // in, so nothing here may navigate.
              label: downloading ? 'Downloading…' : 'Download',
              hint: 'Save the rebuilt, decrypted file',
              disabled: downloading,
              onSelect: () => download(file),
            },
            {
              key: 'parts',
              glyph: '◈',
              label: 'Where the parts live',
              hint: `${file.shards.length} of 3 parts stored`,
              onSelect: onInspect,
            },
            {
              key: 'delete',
              glyph: '✕',
              label: 'Delete',
              hint: 'Erases every part, everywhere',
              danger: true,
              onSelect: () => setConfirming(true),
            },
          ]}
        />
      )}

      {streaming && (
        <StreamLink
          file={file}
          autoplay={streaming === 'play'}
          onClose={() => setStreaming(null)}
        />
      )}

      {converting && (
        <ConvertFile
          file={file}
          onClose={() => setConverting(null)}
          onConverted={onRefresh}
        />
      )}

      {confirming && (
        <ConfirmDialog
          title={`Delete ${file.name}?`}
          busy={busy}
          onConfirm={remove}
          onClose={() => !busy && setConfirming(false)}
        >
          Every part is erased from the accounts holding it. This cannot be undone.
        </ConfirmDialog>
      )}
    </>
  )

  if (mobile) {
    /* The picture is the row's left column and the two lines of text sit
       beside it, rather than the name being indented over a line of detail.
       Six pixels taller than the row it replaces, and the tile is inside the
       same tap target as the name — pointing at the photo is how anyone would
       expect to open it. */
    return (
      <Row mobile>
        <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
          <button
            onClick={legacy ? () => setConverting('open') : onPreview}
            disabled={dead}
            title={openTitle}
            style={{
              display: 'flex', alignItems: 'center', gap: '10px',
              flex: 1, minWidth: 0, minHeight: '52px',
              background: 'none', border: 'none', padding: '0 2px',
              borderRadius: '8px', textAlign: 'left',
              cursor: dead ? 'not-allowed' : 'pointer',
            }}
          >
            <Thumb id={file.id} icon={icon} size={52} expected={hasThumb} />

            <span style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: '3px' }}>
              <span style={{
                fontFamily: FONT.mono, fontSize: '13.5px',
                color: dead ? COLORS.error : COLORS.text,
                overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              }}>{file.name}</span>

              {/* Size, date and the part badges share the second line — the
                  columns they came from are gone, so they carry their own
                  separators. */}
              {/* This line carries the part badges as well as the size and the
                  date, and on a 390px screen it has 224px to do it in. Hence
                  no "·" separator, unlike everywhere else, and hence the
                  badges are pushed right with a margin rather than a spacer
                  element — a spacer would cost a flex gap of its own, which is
                  the difference between reading the time and truncating it. */}
              <span style={{
                display: 'flex', alignItems: 'center', gap: '6px',
                fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
              }}>
                <span style={{ whiteSpace: 'nowrap', flexShrink: 0 }}>{formatBytes(file.size)}</span>
                <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {formatDate(file.modified_at)}
                </span>
                {location && (
                  <span
                    title={location}
                    style={{ minWidth: 0, flexShrink: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                  >in {location}</span>
                )}
                <span style={{ marginLeft: 'auto', display: 'flex', flexShrink: 0 }}>{parts}</span>
              </span>
            </span>
          </button>

          {actions}
        </div>
        {dialogs}
      </Row>
    )
  }

  return (
    <Row>
      {name}
      <span>{formatBytes(file.size)}</span>
      <span>{formatDate(file.modified_at)}</span>
      {parts}
      {actions}
      {dialogs}
    </Row>
  )
}

function FolderRow({ name, path, location, mobile, onNavigate, onRefresh, onError }) {
  const [menu, setMenu] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)

  const open = () => onNavigate(path)

  const remove = async () => {
    setBusy(true)
    try {
      const resp = await api.deleteFolder(path, true)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      setConfirming(false)
      onRefresh()
    } catch (err) {
      onError(err.message)
      setConfirming(false)
    } finally {
      setBusy(false)
    }
  }

  const nameButton = (
    <NameButton mobile={mobile} icon="📁" label={name} location={location} chevron="▸" title="Open folder" onClick={open} />
  )

  const actions = mobile ? (
    <IconButton
      glyph="⋯"
      label={`Actions for ${name}`}
      onClick={() => setMenu(true)}
      size={44}
      style={{ background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, fontSize: '18px' }}
    />
  ) : (
    <span style={{ display: 'flex', justifyContent: 'flex-end' }}>
      <IconButton glyph="✕" label="Delete folder" tone="muted" onClick={() => setConfirming(true)} />
    </span>
  )

  const dialogs = (
    <>
      {menu && (
        <ActionSheet
          title={name}
          subtitle="Folder"
          onClose={() => setMenu(false)}
          items={[
            { key: 'open', glyph: '▸', label: 'Open folder', onSelect: open },
            {
              key: 'delete',
              glyph: '✕',
              label: 'Delete folder',
              hint: 'Erases everything inside it, everywhere',
              danger: true,
              onSelect: () => setConfirming(true),
            },
          ]}
        />
      )}

      {confirming && (
        <ConfirmDialog
          title={`Delete ${name}?`}
          subtitle={path}
          busy={busy}
          confirmLabel="Delete folder"
          onConfirm={remove}
          onClose={() => !busy && setConfirming(false)}
        >
          The folder and everything inside it goes: all parts are erased from every account. This cannot be undone.
        </ConfirmDialog>
      )}
    </>
  )

  if (mobile) {
    return (
      <Row mobile>
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', minHeight: '48px' }}>
          {nameButton}
          {actions}
        </div>
        {dialogs}
      </Row>
    )
  }

  return (
    <Row>
      {nameButton}
      {/* The three empty middle cells only exist to fill the grid. */}
      <span /><span /><span />
      {actions}
      {dialogs}
    </Row>
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
