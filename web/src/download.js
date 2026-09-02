/* Handing a rebuilt file back to the user.

   Added to a phone's home screen the vault runs standalone: no address bar, no
   back button, nothing around the page at all. That makes pointing the window
   at a file's content URL a trap — anything the OS will not render inline (an
   epub, a zip, an unrecognised type) becomes a bare document icon on a black
   screen with no way back, and the only escape left is force-quitting the app.

   So a download never navigates. The bytes are fetched with the session
   cookie, handed to the browser as a blob under the file's own name, and the
   app stays exactly where it was. Sending the URL to a new tab instead would
   not do either: the vault authenticates with a SameSite=Strict cookie that a
   home-screen app does not share with the browser it would hand the link to,
   so the tab would arrive signed out.

   A home-screen app on iOS is the one place even the blob does not work. It
   has no Downloads of its own, ignores the download attribute, and a new
   window opens an in-app Safari view that cannot save anything either. The
   one door out of it is the share sheet — "Save to Files" lives there — and
   the share sheet only opens on a tap, so a file is rebuilt first and offered
   under a button second. See needsShareSheet and shareBlob. */

import { useCallback, useState } from 'react'
import { api } from './api'
import { isStandalone } from './stream'

/* Safari reads the blob out of the object URL after the click handler has
   returned, so it cannot be revoked on the spot. A minute is far longer than
   that ever takes, and still bounds how long the plaintext stays reachable. */
const REVOKE_AFTER_MS = 60_000

/* Hand a blob to the browser as a file under a name of our choosing, without
   the page going anywhere. Used for a rebuilt file, and for the small playlist
   that starts a desktop player. */
export function saveBlob(blob, filename) {
  const url = URL.createObjectURL(blob)

  const link = document.createElement('a')
  link.href = url
  link.download = filename
  // Firefox only follows a synthetic click on an anchor that is in the document.
  document.body.appendChild(link)
  link.click()
  link.remove()

  setTimeout(() => URL.revokeObjectURL(url), REVOKE_AFTER_MS)
}

/* Whether this browser can put a file on the share sheet at all. Safari on
   iOS 15 and later can; a desktop browser mostly cannot, and does not need to. */
export function canShareFiles() {
  try {
    return typeof navigator.share === 'function'
      && typeof navigator.canShare === 'function'
      && navigator.canShare({ files: [new File(['x'], 'probe.bin', { type: 'application/octet-stream' })] })
  } catch {
    return false
  }
}

/* Whether a file has to go out through the share sheet here: a home-screen
   app that can share. A browser tab saves the blob directly and never needs
   this; a home-screen app that cannot share falls back to the blob and hopes. */
export function needsShareSheet() {
  return isStandalone() && canShareFiles()
}

/* Put a rebuilt file on the share sheet. Only from inside a tap: Safari opens
   the sheet on a user's gesture and refuses it a moment later, so this is
   never called at the end of a fetch — the fetch finishes first, and the
   button that calls this appears once it has. */
export async function shareBlob(blob, filename) {
  const file = new File([blob], filename, { type: blob.type || 'application/octet-stream' })
  try {
    await navigator.share({ files: [file], title: filename })
    return 'shared'
  } catch (err) {
    // The sheet was dismissed, which is not a failure to report.
    if (err && err.name === 'AbortError') return 'cancelled'
    throw err
  }
}

/* The whole file lands in memory, which is the bargain the server has already
   made on its side for a single file: it gathers the parts and rebuilds the
   plaintext to answer this very request. */
export async function fetchFileBlob(file) {
  const resp = await fetch(api.contentURL(file.id, { download: true }), {
    credentials: 'same-origin',
  })
  if (!resp.ok) throw new Error(await failureMessage(resp))
  return resp.blob()
}

export async function downloadFile(file) {
  saveBlob(await fetchFileBlob(file), file.name)
}

/* Save from an address the browser must not read into memory first.

   A folder as one zip can be far bigger than the page could hold, so it is not
   fetched as a blob the way a file is: the anchor points at the server's own
   streaming endpoint and the browser saves straight from it, spooling to disk
   as the bytes arrive. The address carries its own short-lived credential, so
   the session cookie is not needed for it to work. The response is an
   attachment, which is what keeps this from being the navigation trap above:
   a browser handed a download does not leave the page for it.

   Not for a home-screen app, which cannot save from an address at all — see
   FolderZip for what it does instead. */
export function downloadFromLink(url, filename) {
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.appendChild(link)
  link.click()
  link.remove()
}

/* Read a streamed address into memory, saying how much has arrived on the
   way. For the one case a streamed archive has to become a blob after all: a
   home-screen app, whose share sheet takes a file and not an address. */
export async function fetchToBlob(url, onProgress) {
  const resp = await fetch(url, { credentials: 'same-origin' })
  if (!resp.ok) throw new Error(await failureMessage(resp))
  const type = resp.headers.get('Content-Type') || 'application/octet-stream'
  if (!resp.body || typeof resp.body.getReader !== 'function') {
    return resp.blob()
  }
  const reader = resp.body.getReader()
  const chunks = []
  let got = 0
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    chunks.push(value)
    got += value.byteLength
    onProgress?.(got)
  }
  return new Blob(chunks, { type })
}

/* Rebuilding a file takes as long as the slowest account holding a part, so
   whatever control started it has to be able to say it is still working.

   On a home-screen app the rebuilt file is not saved but held — `pending` —
   for a button the caller draws, because the share sheet opens only on a tap
   and the tap that started the fetch is long spent by the time it is done.
   `dismiss` lets go of it. Everywhere else `pending` stays null and the file
   saves itself. */
export function useDownload(onError) {
  const [downloading, setDownloading] = useState(false)
  const [pending, setPending] = useState(null)

  const start = useCallback(async (file) => {
    setDownloading(true)
    try {
      if (needsShareSheet()) {
        const blob = await fetchFileBlob(file)
        setPending({ blob, name: file.name })
      } else {
        await downloadFile(file)
      }
    } catch (err) {
      onError(err.message)
    } finally {
      setDownloading(false)
    }
  }, [onError])

  const dismiss = useCallback(() => setPending(null), [])

  return [start, downloading, pending, dismiss]
}

/* A refused download carries the API's JSON error where the server got far
   enough to write one, and nothing but a status where it did not. */
async function failureMessage(resp) {
  if ((resp.headers.get('Content-Type') || '').includes('application/json')) {
    const payload = await resp.json().catch(() => null)
    if (payload?.error) return payload.error
  }
  return `could not rebuild this file (${resp.status})`
}
