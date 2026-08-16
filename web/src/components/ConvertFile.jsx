import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'

/* Moving a file out of the format SAND used before chunking.

   Such a file has no seams — its parts are halves of one sealed blob — so
   reading a byte in the middle means rebuilding all of it. Nothing streams one
   for that reason: a player asking for a second of a film would cost the whole
   film in memory, which on a small machine is how the machine stops answering.

   So it is refused, and this is what the refusal offers instead. The warning is
   the point of the dialog rather than decoration on it: converting is minutes of
   transfer, it moves data on the accounts holding it, and someone who presses
   the button should already know both. */
export default function ConvertFile({ file, reason, onClose, onConverted }) {
  const mobile = useIsMobile()
  const [state, setState] = useState('asking') // asking · working · done
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const convert = async () => {
    setState('working')
    setError(null)
    try {
      const resp = await api.convertFile(file.id)
      setReport(resp)
      setState('done')
      onConverted?.()
    } catch (err) {
      setError(err.message)
      setState('asking')
    }
  }

  return (
    <Modal
      title="Convert to chunks"
      subtitle={file.name}
      onClose={state === 'working' ? undefined : onClose}
      width={520}
      zIndex={120}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {state === 'done' ? (
        <>
          <Banner tone="success">
            Converted{report?.chunk_count ? ` into ${report.chunk_count} chunks` : ''}. It
            streams now, without being rebuilt whole.
          </Banner>
          {report?.warnings?.map((w, i) => <Banner key={i} tone="warn">{w}</Banner>)}
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button variant="primary" onClick={onClose}
              style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Done</Button>
          </div>
        </>
      ) : (
        <>
          <Banner tone="warn">
            {reason || 'This file is stored in the format SAND used before chunked ' +
              'storage, which cannot be read a piece at a time.'}
          </Banner>

          <p style={{
            margin: '0 0 14px',
            fontFamily: FONT.sans,
            fontSize: '12.5px',
            lineHeight: 1.7,
            color: COLORS.textDim,
          }}>
            Converting reads {file.name} once and stores it again in chunks, after
            which it opens and seeks like anything else — in the browser, over the
            share, and in a player.
          </p>

          <dl style={{ margin: '0 0 18px', display: 'grid', gap: '8px' }}>
            {[
              ['Costs', `a download and an upload of all ${formatBytes(file.size)} — minutes, on a home connection`],
              ['Moves data', 'the new parts are written before the old ones are erased'],
              ['Safe to stop', 'an interrupted conversion leaves the file exactly as it is now'],
            ].map(([term, detail]) => (
              <div key={term} style={{ display: 'flex', gap: '12px', alignItems: 'baseline' }}>
                <dt style={{
                  fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim,
                  minWidth: '82px', flexShrink: 0,
                }}>{term}</dt>
                <dd style={{
                  margin: 0, fontFamily: FONT.sans, fontSize: '12px',
                  lineHeight: 1.6, color: COLORS.textMuted,
                }}>{detail}</dd>
              </div>
            ))}
          </dl>

          {state === 'working' && (
            <Banner tone="info">
              Converting. This holds the connection open until it finishes —
              leaving the page stops it, and the file stays as it was.
            </Banner>
          )}

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            <Button variant="ghost" onClick={onClose} disabled={state === 'working'}
              style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Cancel</Button>
            <Button
              variant="primary"
              onClick={convert}
              disabled={state === 'working'}
              style={mobile ? { flex: 2, justifyContent: 'center' } : null}
            >
              {state === 'working'
                ? <><Spinner size={12} color={COLORS.bg} /> Converting…</>
                : '◈ Convert'}
            </Button>
          </div>
        </>
      )}
    </Modal>
  )
}
