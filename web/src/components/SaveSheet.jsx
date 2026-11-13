import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { needsShareSheet, saveBlob, shareBlob } from '../download'
import { Banner, Button, Modal } from './ui'

/* A rebuilt file, held for the share sheet.

   On a phone a download is the same thing every other app's Share button is:
   the sheet that reaches WhatsApp, Mail, Drive, Files and the rest. The sheet
   opens only on a tap, though, and a file that took longer to rebuild than a
   tap lasts is held here under a button that has one. It is also where a
   dismissed sheet lands, so changing your mind does not mean rebuilding.

   In a browser tab the file can also be saved straight to the device, and
   that is offered beside the sheet — some phones' sheets have no "save" in
   them at all. Added to the home screen there is no such door: iOS ignores
   the download attribute and a blob opened in place is a dead end, so the
   sheet, with "Save to Files" in it, is the only way out. */
export function SaveSheet({ pending, onDone, zIndex }) {
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  if (!pending) return null

  const sheetOnly = needsShareSheet()

  const share = async () => {
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

  const save = () => {
    saveBlob(pending.blob, pending.name)
    onDone()
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
        {sheetOnly
          ? (
            <>
              Added to your home screen, SAND has no Downloads of its own — the share
              sheet is how a file reaches Files, Photos or another app. Pick
              <strong style={{ color: COLORS.text }}> Save to Files</strong> there.
            </>
          )
          : 'Send it to WhatsApp, Mail or any other app from the share sheet, or keep it in this device’s Downloads.'}
      </p>

      <div style={{ display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' }}>
        <Button variant="primary" onClick={share} disabled={busy}>
          {busy ? 'Opening…' : sheetOnly ? 'Save to Files…' : '↗ Share…'}
        </Button>
        {!sheetOnly && (
          <Button onClick={save} disabled={busy}>↓ Save to device</Button>
        )}
        <Button variant="ghost" onClick={onDone} disabled={busy}>Done</Button>
      </div>
    </Modal>
  )
}
