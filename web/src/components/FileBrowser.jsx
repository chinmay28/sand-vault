import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, fileIcon, formatBytes, formatDate } from '../theme'
import { api, joinPath } from '../api'
import { useDownload } from '../download'
import { Banner, Button, Empty, Modal, Spinner } from './ui'

/* Name, size, modified, parts, actions. The four fixed columns come to nearly
   500px, which is why the phone layout stacks instead of shrinking them. */
const COLUMNS = 'minmax(0,1fr) 92px 150px 132px 108px'

export default function FileBrowser({
  path, listing, loading, error, providers, mobile,
  onNavigate, onRefresh, onPreview, onInspect, onError,
}) {
  const [dragging, setDragging] = useState(false)
  const [uploads, setUploads] = useState([])
  const [warnings, setWarnings] = useState([])
  const [creatingFolder, setCreatingFolder] = useState(false)
  const fileInput = useRef(null)
  const dragDepth = useRef(0)

  const canUpload = providers.length > 0

  const uploadFiles = useCallback(async (files) => {
    if (!files.length) return
    if (!canUpload) {
      onError('Connect a cloud account before uploading — there is nowhere to put the parts yet.')
      return
    }

    const batch = { id: Math.random().toString(36).slice(2), names: [...files].map((f) => f.name), progress: 0 }
    setUploads((prev) => [...prev, batch])

    try {
      const resp = await api.upload(files, path, {
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
  }, [path, canUpload, onError, onRefresh])

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
    uploadFiles(Array.from(e.dataTransfer.files || []))
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
          <Crumb label="▣ /" onClick={() => onNavigate('/')} active={path === '/'} />
          {segments.map((segment, i) => {
            const target = '/' + segments.slice(0, i + 1).join('/')
            return (
              <React.Fragment key={target}>
                <span style={{ color: COLORS.textMuted }}>/</span>
                <Crumb label={segment} onClick={() => onNavigate(target)} active={i === segments.length - 1} />
              </React.Fragment>
            )
          })}
        </nav>

        <Button size="sm" onClick={() => setCreatingFolder(true)}
          style={mobile ? { flex: 1, justifyContent: 'center' } : null}>+ Folder</Button>
        <Button size="sm" variant="primary" onClick={() => fileInput.current?.click()} disabled={!canUpload}
          style={mobile ? { flex: 1, justifyContent: 'center' } : null}>
          ↑ Upload
        </Button>
        <input
          ref={fileInput}
          type="file"
          multiple
          style={{ display: 'none' }}
          onChange={(e) => { uploadFiles(Array.from(e.target.files || [])); e.target.value = '' }}
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

        {loading && !listing ? (
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

function Crumb({ label, onClick, active }) {
  return (
    <button
      onClick={onClick}
      style={{
        background: 'none',
        border: 'none',
        padding: '2px 4px',
        cursor: 'pointer',
        fontFamily: FONT.mono,
        fontSize: '12px',
        color: active ? COLORS.accent : COLORS.textDim,
        fontWeight: active ? 700 : 400,
      }}
    >{label}</button>
  )
}

function FileTable({ path, listing, canUpload, mobile, onNavigate, onPreview, onInspect, onRefresh, onError, onPickFiles }) {
  const folders = listing?.folders || []
  const files = listing?.files || []

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
        <Row key={`dir:${folder}`} mobile={mobile}>
          <button
            onClick={() => onNavigate(joinPath(path, folder))}
            style={{
              display: 'flex', alignItems: 'center', gap: '9px',
              flex: 1, minWidth: 0,
              background: 'none', border: 'none', padding: 0, cursor: 'pointer',
              fontFamily: FONT.mono, fontSize: '12.5px', color: COLORS.text,
              overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              textAlign: 'left',
            }}
          >
            <span style={{ color: COLORS.accent }}>▸</span>
            <span>📁</span>
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{folder}</span>
          </button>
          {/* The three empty middle cells only exist to fill the grid. */}
          {!mobile && <><span /><span /><span /></>}
          <FolderActions path={joinPath(path, folder)} onRefresh={onRefresh} onError={onError} />
        </Row>
      ))}

      {files.map((file) => (
        <FileRow
          key={file.id}
          file={file}
          mobile={mobile}
          onPreview={() => onPreview(file)}
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
        // Too narrow for columns, so the row becomes a strip: name on the
        // left, actions on the right, and everything else on a second line.
        ...(mobile
          ? { display: 'flex', flexWrap: 'wrap', rowGap: '6px', columnGap: '8px' }
          : { display: 'grid', gridTemplateColumns: COLUMNS, gap: '12px' }),
        alignItems: 'center',
        padding: mobile ? '8px 12px' : '9px 14px',
        borderBottom: `1px solid ${COLORS.border}22`,
        background: hover ? COLORS.surfaceHover : 'transparent',
        fontFamily: FONT.mono,
        fontSize: '11.5px',
        color: COLORS.textDim,
        minHeight: '38px',
      }}
    >{children}</div>
  )
}

function FileRow({ file, mobile, onPreview, onInspect, onRefresh, onError }) {
  const [busy, setBusy] = useState(false)
  const [download, downloading] = useDownload(onError)
  const degraded = file.shards.length < 3
  const dead = file.shards.length < 2

  const remove = async () => {
    if (!window.confirm(`Delete "${file.name}"?\n\nEvery part is erased from the accounts holding it. This cannot be undone.`)) return
    setBusy(true)
    try {
      const resp = await api.deleteFile(file.id)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      onRefresh()
    } catch (err) {
      onError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const name = (
    <button
      onClick={onPreview}
      title={dead ? 'Too few parts remain to rebuild this file' : 'Open'}
      style={{
        display: 'flex', alignItems: 'center', gap: '9px',
        flex: 1, minWidth: 0,
        background: 'none', border: 'none', padding: 0,
        cursor: dead ? 'not-allowed' : 'pointer',
        fontFamily: FONT.mono, fontSize: '12.5px',
        color: dead ? COLORS.error : COLORS.text,
        overflow: 'hidden', textAlign: 'left',
      }}
      disabled={dead}
    >
      {/* Lines the name up under the folder rows' ▸ chevron. */}
      {!mobile && <span style={{ width: '12px' }} />}
      <span>{fileIcon(file.mime, file.name)}</span>
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{file.name}</span>
    </button>
  )

  const parts = (
    <button
      onClick={onInspect}
      title="Where the parts live"
      aria-label="Where the parts live"
      style={{ display: 'flex', alignItems: 'center', gap: '3px', background: 'none', border: 'none', padding: 0, cursor: 'pointer' }}
    >
      {[1, 2, 3].map((part) => {
        const shard = file.shards.find((s) => s.part === part)
        return (
          <span
            key={part}
            title={shard ? `Part ${part} on ${shard.provider_name}` : `Part ${part} not stored`}
            style={{
              width: '19px',
              height: '15px',
              borderRadius: '3px',
              fontFamily: FONT.mono,
              fontSize: '8.5px',
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
    </button>
  )

  const actions = (
    <span style={{ display: 'flex', gap: '4px', justifyContent: 'flex-end', flexShrink: 0 }}>
      {/* Every row carries this control, so the spoken name says which file it
          belongs to rather than repeating the same sentence down the list. */}
      <button
        onClick={() => download(file)}
        disabled={downloading}
        title="Download the rebuilt, decrypted file"
        aria-label={`Download ${file.name}`}
        style={{
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          background: 'none', border: 'none', cursor: downloading ? 'default' : 'pointer',
          color: COLORS.textDim, fontFamily: FONT.mono, fontSize: '13px', padding: '2px 5px',
        }}
      >{downloading ? '…' : '↓'}</button>
      <button
        onClick={remove}
        disabled={busy}
        title="Delete everywhere"
        aria-label="Delete everywhere"
        style={{ background: 'none', border: 'none', color: COLORS.textMuted, cursor: 'pointer', fontSize: '12px', padding: '2px 5px' }}
      >{busy ? '…' : '✕'}</button>
    </span>
  )

  if (mobile) {
    return (
      <Row mobile>
        {name}
        {actions}
        {/* Size, date and the part badges share the second line — the columns
            they came from are gone, so they carry their own separators. */}
        <div style={{
          display: 'flex', alignItems: 'center', gap: '8px',
          width: '100%', fontSize: '10.5px', color: COLORS.textMuted,
        }}>
          <span style={{ whiteSpace: 'nowrap' }}>{formatBytes(file.size)}</span>
          <span>·</span>
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {formatDate(file.modified_at)}
          </span>
          <span style={{ flex: 1 }} />
          {parts}
        </div>
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
    </Row>
  )
}

function FolderActions({ path, onRefresh, onError }) {
  const remove = async () => {
    if (!window.confirm(`Delete "${path}" and everything inside it?\n\nAll parts are erased from every account. This cannot be undone.`)) return
    try {
      const resp = await api.deleteFolder(path, true)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      onRefresh()
    } catch (err) {
      onError(err.message)
    }
  }

  return (
    <span style={{ display: 'flex', justifyContent: 'flex-end' }}>
      <button
        onClick={remove}
        title="Delete folder"
        style={{ background: 'none', border: 'none', color: COLORS.textMuted, cursor: 'pointer', fontSize: '12px', padding: '2px 5px' }}
      >✕</button>
    </span>
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
