import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { downloadFromLink, fetchToBlob, needsShareSheet } from '../download'
import { SaveSheet } from './SaveSheet'
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
   machine with the disk. It carries its own credential, lasts a few hours
   without being used — three unless the vault's settings say otherwise,
   sliding forward while a download runs — and dies the moment the vault
   locks.

   A home-screen app cannot save from an address at all: iOS ignores the
   download attribute there and a new window is an in-app Safari view that
   cannot save either. Its one door is the share sheet, which takes a file
   and not an address — so there, and only there, the archive is read into
   memory after all and offered to the sheet, up to a size a phone can hold.
   Past that the honest answer is the address, on a machine with the disk. */

/* How much archive a home-screen app will read into memory to share. A phone
   has room for this; it does not have room for a film library, and a page that
   tried would be killed partway with nothing to show for it. */
const SHARE_LIMIT = 512 << 20

/* A link's remaining life in the roundest words that are still true. */
function describeLifetime(seconds) {
  const hours = Math.round((seconds || 0) / 3600)
  if (hours < 1) return 'under an hour'
  if (hours === 1) return 'an hour'
  if (hours === 24) return 'a day'
  if (hours % 24 === 0) return `${hours / 24} days`
  return `${hours} hours`
}
export function FolderZip({ path, name, vault = '', onClose }) {
  const [link, setLink] = useState(null)
  const [error, setError] = useState(null)
  /* null until Save is pressed, then where the download went — see
     downloadFromLink. A home-screen app cannot save a file itself, so it
     hands the address to the browser and says so. */
  const [started, setStarted] = useState(null)
  /* The home-screen route: how much of the archive has arrived, and the blob
     once all of it has, held for the share sheet's button. */
  const [got, setGot] = useState(0)
  const [pending, setPending] = useState(null)
  const sheet = needsShareSheet()

  useEffect(() => {
    let live = true
    api.folderZipLink(path, vault)
      .then((resp) => { if (live) setLink({ ...resp, address: absoluteURL(resp.url) }) })
      .catch((err) => { if (live) setError(err) })
    return () => { live = false }
  }, [path, vault])

  const save = async () => {
    if (!link) return
    if (!sheet) {
      downloadFromLink(link.url, link.name)
      setStarted('saved')
      return
    }
    if (link.bytes > SHARE_LIMIT) {
      setStarted('too-big')
      return
    }
    setStarted('fetching')
    setGot(0)
    try {
      const blob = await fetchToBlob(link.url, setGot)
      setPending({ blob, name: link.name })
      setStarted('ready')
    } catch (err) {
      setError(err)
      setStarted(null)
    }
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
            <Button variant="primary" onClick={save} disabled={started === 'fetching'}>
              {started === 'fetching'
                ? 'Rebuilding…'
                : started === 'ready' || started === 'saved' || started === 'browser'
                  ? 'Save it again'
                  : `↓ Save ${link.name}`}
            </Button>
            <Button variant="ghost" onClick={onClose} disabled={started === 'fetching'}>
              {started && started !== 'fetching' ? 'Done' : 'Cancel'}
            </Button>
          </div>

          {started === 'fetching' && (
            <Banner tone="info">
              Rebuilding the archive here first — {formatBytes(got)} of about {formatBytes(link.bytes)} so far.
              Added to your home screen, SAND can only hand a file to the share
              sheet, and the sheet takes a file rather than an address.
            </Banner>
          )}
          {started === 'too-big' && (
            <Banner tone="warn">
              Too big to hold on a phone: a home-screen app can only save through the
              share sheet, which needs the whole archive in memory first. Open SAND in
              Safari and save it from there, or paste the address below into a
              download manager on a machine with the disk for it.
            </Banner>
          )}
          <SaveSheet
            pending={pending}
            zIndex={140}
            onDone={() => { setPending(null); setStarted('shared') }}
          />

          {started === 'saved' && (
            <Banner tone="info">
              Your browser is saving it. There is nothing to wait for here — the
              archive is built as it downloads, and closing this leaves the
              download running.
            </Banner>
          )}
          {started === 'shared' && (
            <Banner tone="info">
              Handed to the share sheet. If you picked Save to Files, it is in Files now.
            </Banner>
          )}

          <CopyField
            label="Or save it somewhere else"
            value={link.address}
            help={`Good for ${describeLifetime(link.expires_in)}, or until the vault locks, and for this one folder. Paste it into a download manager, or curl -O it, on a machine with the disk for it — no sign-in needed, the address is the key. How long a link lasts is in Vault settings.`}
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
