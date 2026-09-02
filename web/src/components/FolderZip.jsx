import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { downloadFromLink } from '../download'
import { absoluteURL } from '../stream'
import { Banner, Button, CopyField, Modal, Spinner } from './ui'

/* A folder handed back as one zip.

   A file is downloaded by fetching it and handing the browser a blob, which
   costs the file in memory and is fine for a file. A folder is not fetched
   that way, because a folder can be far larger than a page can hold: the
   server mints a short-lived address, and the browser saves straight from it
   as the archive streams. The archive is built as it downloads — each file
   gathered from the clouds and written straight in — so a folder far bigger
   than the machine SAND runs on still leaves it, one piece at a time.

   The address is shown as well as followed, for the case a phone is the wrong
   place to receive 40 GB: paste it into a download manager, or curl, on a
   machine with the disk. It carries its own credential, lasts twelve hours
   without being used — sliding forward while a download runs — and dies the
   moment the vault locks. */
export function FolderZip({ path, name, vault = '', onClose }) {
  const [link, setLink] = useState(null)
  const [error, setError] = useState(null)
  /* null until Save is pressed, then where the download went — see
     downloadFromLink. A home-screen app cannot save a file itself, so it
     hands the address to the browser and says so. */
  const [started, setStarted] = useState(null)

  useEffect(() => {
    let live = true
    api.folderZipLink(path, vault)
      .then((resp) => { if (live) setLink({ ...resp, address: absoluteURL(resp.url) }) })
      .catch((err) => { if (live) setError(err) })
    return () => { live = false }
  }, [path, vault])

  const save = () => {
    if (!link) return
    setStarted(downloadFromLink(link.url, link.name))
  }

  return (
    <Modal title={`Download ${name}`} subtitle={path} onClose={onClose} width={520}>
      {error && (
        <Banner tone={error.code === 'NEEDS_CONVERSION' ? 'warn' : 'error'}>
          {error.code === 'NEEDS_CONVERSION'
            ? (
              <>
                <div>Some of the files in here are still stored in the format SAND used
                  before chunking, which cannot be streamed into an archive.</div>
                <div style={{ marginTop: '4px' }}>
                  Convert them first — each one's own menu offers it — and this will
                  work. {error.message}
                </div>
              </>
            )
            : error.message}
        </Banner>
      )}

      {!error && !link && (
        <div style={{ padding: '32px', textAlign: 'center' }}><Spinner size={20} /></div>
      )}

      {link && (
        <>
          <div style={{
            display: 'flex', gap: '18px', flexWrap: 'wrap', marginBottom: '14px',
            fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim,
          }}>
            <span><strong style={{ color: COLORS.text }}>{link.files}</strong> file{link.files === 1 ? '' : 's'}</span>
            <span><strong style={{ color: COLORS.text }}>{formatBytes(link.bytes)}</strong> in here</span>
            {link.folders > 0 && (
              <span><strong style={{ color: COLORS.text }}>{link.folders}</strong> folder{link.folders === 1 ? '' : 's'} inside</span>
            )}
          </div>

          <div style={{ display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '14px' }}>
            <Button variant="primary" onClick={save}>
              {started ? 'Save it again' : `↓ Save ${link.name}`}
            </Button>
            <Button variant="ghost" onClick={onClose}>{started ? 'Done' : 'Cancel'}</Button>
          </div>

          {started === 'saved' && (
            <Banner tone="info">
              Your browser is saving it. There is nothing to wait for here — the
              archive is built as it downloads, and closing this leaves the
              download running.
            </Banner>
          )}
          {started === 'browser' && (
            <Banner tone="info">
              Handed to your browser, which can save it where this app cannot:
              tap Download there and it goes to your Downloads. Come back here
              whenever you like — nothing is waiting on this screen.
            </Banner>
          )}

          <CopyField
            label="Or save it somewhere else"
            value={link.address}
            help="Good for twelve hours, or until the vault locks, and for this one folder. Paste it into a download manager, or curl -O it, on a machine with the disk for it — no sign-in needed, the address is the key."
          />

          <p style={{
            margin: '12px 0 0', paddingTop: '12px', borderTop: `1px solid ${COLORS.border}`,
            fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
          }}>
            Built as it downloads: each file is gathered from your clouds and written
            straight into the archive, so a folder far bigger than the memory of the
            machine SAND runs on still leaves it, a piece at a time. The archive is
            stored rather than compressed — the files already were, before they were
            split — so there is no total to show until it is done, and your browser
            counts bytes rather than a percentage.
          </p>
        </>
      )}
    </Modal>
  )
}
