import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes, isPlayable, previewKind } from '../theme'
import { useIsMobile } from '../hooks'
import { api } from '../api'
import { useDownload } from '../download'
import { thumbnailFromElement } from '../thumbs'
import PdfPreview from './PdfPreview'
import StreamLink from './StreamLink'
import { RelocateClouds } from './CloudSelect'
import { Banner, Button, Modal, Spinner } from './ui'

/* How much of the visible viewport a preview may take before the modal's own
   chrome and the buttons under it start being pushed off. */
const PREVIEW_MAX = 'calc(var(--app-height) * 0.62)'

/* Opening a file here is the whole point of the design: the server gathers two
   of its three parts from separate accounts, rebuilds the plaintext in memory
   and streams it back. Nothing decrypted is ever written to disk. */
export default function PreviewModal({ file, hasThumb, onClose, onThumbStored }) {
  const kind = previewKind(file.mime, file.name)
  const url = api.contentURL(file.id)
  const mobile = useIsMobile()
  const captured = useRef(false)

  const [text, setText] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(kind === 'text')

  /* Kept apart from the preview's own error: a file whose download fails is
     still a file the preview above may be rendering perfectly well. */
  const [downloadError, setDownloadError] = useState(null)
  const [download, downloading] = useDownload(setDownloadError)

  /* The preview above is the browser's best attempt at the file. For a film
     that means one long inline fetch and whichever codecs the browser happens
     to have, which is exactly when handing it to a real player is the better
     answer — so the offer sits beside it rather than somewhere else. */
  const [streaming, setStreaming] = useState(null)
  const playable = isPlayable(file.mime, file.name)

  useEffect(() => {
    if (kind !== 'text') return
    let cancelled = false

    fetch(url, { credentials: 'same-origin' })
      .then(async (resp) => {
        if (!resp.ok) throw new Error(`could not rebuild this file (${resp.status})`)
        // Cap what we render: a multi-megabyte log should not freeze the tab.
        const blob = await resp.blob()
        return blob.slice(0, 512 * 1024).text()
      })
      .then((body) => { if (!cancelled) { setText(body); setLoading(false) } })
      .catch((err) => { if (!cancelled) { setError(err.message); setLoading(false) } })

    return () => { cancelled = true }
  }, [url, kind])

  /* A file uploaded before thumbnails existed — or from the command line —
     has no picture in the list. Opening it has just rebuilt and decoded the
     whole thing on screen, so taking one now costs nothing but a canvas: no
     second download, no second gather from the accounts. */
  const captureThumb = async (el) => {
    if (hasThumb || captured.current) return
    captured.current = true

    const blob = await thumbnailFromElement(el)
    if (!blob) return
    try {
      await api.putThumb(file.id, blob)
      onThumbStored?.()
    } catch {
      // The preview is what was asked for and it is on screen. Failing to
      // keep a copy of it is not worth interrupting anyone over.
    }
  }

  return (
    <Modal
      title={file.name}
      subtitle={`${formatBytes(file.size)} · rebuilt from ${file.shards.length} part${file.shards.length === 1 ? '' : 's'} across ${new Set(file.shards.map((s) => s.provider_id)).size} account(s)`}
      onClose={onClose}
      width={920}
    >
      <div style={{
        background: COLORS.bg,
        border: `1px solid ${COLORS.border}`,
        borderRadius: '8px',
        minHeight: '180px',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        overflow: 'hidden',
        marginBottom: '16px',
      }}>
        {error && <Banner tone="error">{error}</Banner>}

        {!error && kind === 'image' && (
          <img
            src={url}
            alt={file.name}
            style={{ maxWidth: '100%', maxHeight: PREVIEW_MAX, display: 'block' }}
            onLoad={(e) => captureThumb(e.currentTarget)}
            onError={() => setError('This file could not be rebuilt or is not a readable image.')}
          />
        )}

        {!error && kind === 'video' && (
          <video src={url} controls playsInline style={{ maxWidth: '100%', maxHeight: PREVIEW_MAX }} />
        )}

        {!error && kind === 'audio' && (
          <audio src={url} controls style={{ width: '90%', margin: '32px 0' }} />
        )}

        {/* Drawn here rather than framed for the browser to deal with: a
            framed PDF is a blank box or one unscrollable page on iOS Safari,
            which used to leave a phone with an apology instead of the
            document. */}
        {!error && kind === 'pdf' && (
          <PdfPreview url={url} name={file.name} onFirstPage={captureThumb} />
        )}

        {!error && kind === 'text' && (
          loading ? <div style={{ padding: '40px' }}><Spinner size={20} /></div> : (
            <pre style={{
              margin: 0,
              padding: '16px',
              width: '100%',
              maxHeight: PREVIEW_MAX,
              overflow: 'auto',
              fontFamily: FONT.mono,
              fontSize: '12px',
              lineHeight: 1.6,
              color: COLORS.text,
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-word',
              boxSizing: 'border-box',
            }}>{text}</pre>
          )
        )}

        {!error && !kind && (
          <div style={{
            padding: '46px 24px',
            textAlign: 'center',
            fontFamily: FONT.sans,
            fontSize: '13px',
            color: COLORS.textMuted,
            lineHeight: 1.6,
          }}>
            <div style={{ fontSize: '30px', marginBottom: '10px', opacity: 0.5 }}>📦</div>
            No inline preview for {file.mime || 'this file type'}.<br />
            Download it to open with something else.
          </div>
        )}
      </div>

      {downloadError && (
        <Banner tone="error" onDismiss={() => setDownloadError(null)}>{downloadError}</Banner>
      )}

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={onClose}
          style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Close</Button>
        {/* A player for the files a player is for; for everything else the
            same dialog, opened for the address it is really wanted for. */}
        <Button
          onClick={() => setStreaming(playable ? 'play' : 'link')}
          title={playable
            ? 'Open this in VLC, or copy the address'
            : 'A link any player or app can open'}
          style={mobile ? { flex: 1, justifyContent: 'center' } : null}
        >{playable ? '▶ Stream in VLC' : '⧉ Copy the address'}</Button>
        <Button
          variant="primary"
          onClick={() => download(file)}
          disabled={downloading}
          title="Download the rebuilt, decrypted file"
          style={mobile ? { flex: 2, justifyContent: 'center' } : null}
        >
          {downloading
            ? <><Spinner size={12} color={COLORS.bg} /> Rebuilding…</>
            : '↓ Download decrypted'}
        </Button>
      </div>

      {streaming && (
        <StreamLink
          file={file}
          autoplay={streaming === 'play'}
          /* Opened from inside a dialog, so it has to sit above the one that
             opened it rather than wherever the portal happened to put it. */
          zIndex={120}
          onClose={() => setStreaming(null)}
        />
      )}
    </Modal>
  )
}

/* A read-out of exactly where a file's parts are and whether each one is still
   answering — the thing you want when an account starts misbehaving. */
export function ShardInspector({ file, providers = [], onClose, onChanged }) {
  const [health, setHealth] = useState(null)
  const [error, setError] = useState(null)
  const [relocating, setRelocating] = useState(false)

  useEffect(() => {
    let cancelled = false
    api.fileHealth(file.id)
      .then((resp) => { if (!cancelled) setHealth(resp) })
      .catch((err) => { if (!cancelled) setError(err.message) })
    return () => { cancelled = true }
  }, [file.id])

  return (
    <Modal
      title="Where this file lives"
      subtitle={`${file.name} — split into ${file.shards.length} encrypted part(s). Any two rebuild the original; any one on its own is noise.`}
      onClose={onClose}
      width={620}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {!health && !error && <Spinner />}

      {health && (
        <>
          <Banner tone={health.recoverable ? 'success' : 'error'}>
            {health.recoverable
              ? 'Enough parts are reachable to rebuild this file right now.'
              : 'Too few parts are reachable — this file cannot currently be rebuilt.'}
          </Banner>

          {health.shards.map((shard) => (
            <div key={shard.part} style={{
              display: 'flex',
              alignItems: 'center',
              gap: '12px',
              padding: '11px 13px',
              marginBottom: '8px',
              background: COLORS.bg,
              border: `1px solid ${shard.present ? COLORS.border : COLORS.error + '66'}`,
              borderRadius: '6px',
              fontFamily: FONT.mono,
              fontSize: '11.5px',
            }}>
              <span style={{
                color: shard.present ? COLORS.success : COLORS.error,
                fontSize: '13px',
              }}>{shard.present ? '✓' : '✗'}</span>

              {/* The account's colour, so this list and the row's part badges
                  can be read against each other. */}
              <span
                aria-hidden="true"
                style={{
                  width: '14px',
                  height: '14px',
                  flexShrink: 0,
                  borderRadius: '3px',
                  background: accountColor(shard.provider_id),
                }}
              />

              <span style={{ flex: 1, minWidth: 0 }}>
                <span style={{ color: COLORS.text }}>Part {shard.part}</span>
                <span style={{ color: COLORS.textMuted }}> on </span>
                <span style={{ color: COLORS.text }}>{shard.provider_name}</span>
                <span style={{ color: COLORS.textMuted }}> ({shard.provider_kind})</span>
                <div style={{
                  marginTop: '4px',
                  color: COLORS.textMuted,
                  fontSize: '10px',
                  wordBreak: 'break-all',
                }}>
                  {shard.key}
                  {shard.error ? ` — ${shard.error}` : ''}
                </div>
              </span>

              <span style={{ color: COLORS.textDim, flexShrink: 0, whiteSpace: 'nowrap' }}>
                {shard.present ? formatBytes(shard.observed_size) : '—'}
              </span>
            </div>
          ))}

          {file.shards.length < 3 && (
            <Banner tone="warn">
              Only {file.shards.length} of 3 parts were stored, so there is no spare copy.
              Connect another account and re-upload this file to restore full redundancy.
            </Banner>
          )}
        </>
      )}

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '4px' }}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        {/* The read-out above says where the parts are; this is how they go
            somewhere else. The row itself has no room for a fourth control,
            and this is the question it would have been answering anyway. */}
        <Button
          variant="primary"
          onClick={() => setRelocating(true)}
          disabled={providers.length === 0}
          title="Move the parts to other clouds"
        >⇄ Move to other clouds</Button>
      </div>

      {relocating && (
        <RelocateClouds
          target={{ id: file.id }}
          title={`Move ${file.name}`}
          subtitle={`${formatBytes(file.size)} — pick the clouds its ${file.shards.length} part(s) should live on`}
          /* Already selected: where the parts are now, so the dialog opens on
             the truth and a swap is one click rather than four. */
          current={file.shards.map((s) => s.provider_id)}
          providers={providers}
          onClose={() => setRelocating(false)}
          onDone={() => { onChanged?.(); onClose() }}
        />
      )}
    </Modal>
  )
}
