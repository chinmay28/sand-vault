import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { shareBlob } from '../download'
import { Banner, Button, Modal } from './ui'

/* The one door out of a home-screen app.

   Added to the home screen, SAND has no Downloads of its own: iOS ignores the
   download attribute, a blob opened in place is a dead end, and a new window
   is an in-app Safari view that cannot save anything either. What it does
   have is the share sheet, and "Save to Files" lives there. So on a
   home-screen app a rebuilt file is not saved but held, and this sheet offers
   it under a button — because the share sheet opens only on a tap, and the tap
   that started the rebuild is long spent by the time the file is ready. */
export function SaveSheet({ pending, onDone, zIndex }) {
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  if (!pending) return null

  const save = async () => {
    setBusy(true)
    setError(null)
    try {
      if (await shareBlob(pending.blob, pending.name) === 'shared') onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title={`Save ${pending.name}`}
      subtitle={`${formatBytes(pending.blob.size)} · rebuilt and ready`}
      onClose={onDone}
      width={460}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <p style={{
        margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6,
        color: COLORS.textDim,
      }}>
        Added to your home screen, SAND has no Downloads of its own — the share
        sheet is how a file reaches Files, Photos or another app. Pick
        <strong style={{ color: COLORS.text }}> Save to Files</strong> there.
      </p>

      <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
        <Button variant="primary" onClick={save} disabled={busy}>
          {busy ? 'Opening…' : 'Save to Files…'}
        </Button>
        <Button variant="ghost" onClick={onDone} disabled={busy}>Done</Button>
      </div>
    </Modal>
  )
}
