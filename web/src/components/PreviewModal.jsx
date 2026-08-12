import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes, previewKind } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Opening a file here is the whole point of the design: the server gathers two
   of its three parts from separate accounts, rebuilds the plaintext in memory
   and streams it back. Nothing decrypted is ever written to disk. */
export default function PreviewModal({ file, onClose }) {
  const kind = previewKind(file.mime, file.name)
  const url = api.contentURL(file.id)

  const [text, setText] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(kind === 'text')

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
            style={{ maxWidth: '100%', maxHeight: '62vh', display: 'block' }}
            onError={() => setError('This file could not be rebuilt or is not a readable image.')}
          />
        )}

        {!error && kind === 'video' && (
          <video src={url} controls style={{ maxWidth: '100%', maxHeight: '62vh' }} />
        )}

        {!error && kind === 'audio' && (
          <audio src={url} controls style={{ width: '90%', margin: '32px 0' }} />
        )}

        {!error && kind === 'pdf' && (
          <iframe
            src={url}
            title={file.name}
            style={{ width: '100%', height: '68vh', border: 'none', background: '#fff' }}
          />
        )}

        {!error && kind === 'text' && (
          loading ? <div style={{ padding: '40px' }}><Spinner size={20} /></div> : (
            <pre style={{
              margin: 0,
              padding: '16px',
              width: '100%',
              maxHeight: '62vh',
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

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        <Button
          variant="primary"
          as="a"
          onClick={() => { window.location.href = api.contentURL(file.id, { download: true }) }}
        >↓ Download decrypted</Button>
      </div>
    </Modal>
  )
}

/* A read-out of exactly where a file's parts are and whether each one is still
   answering — the thing you want when an account starts misbehaving. */
export function ShardInspector({ file, onClose }) {
  const [health, setHealth] = useState(null)
  const [error, setError] = useState(null)

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

              <span style={{ color: COLORS.textDim }}>
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

      <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '4px' }}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
      </div>
    </Modal>
  )
}
