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
   so the tab would arrive signed out. */

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

export async function downloadFile(file) {
  const resp = await fetch(api.contentURL(file.id, { download: true }), {
    credentials: 'same-origin',
  })
  if (!resp.ok) throw new Error(await failureMessage(resp))

  /* The whole file lands in memory, which is the bargain the server has
     already made on its side: it gathers the parts and rebuilds the plaintext
     in memory to answer this very request. */
  saveBlob(await resp.blob(), file.name)
}

/* Save from an address the browser must not read into memory first.

   A folder as one zip can be far bigger than the page could hold, so it is not
   fetched as a blob the way a file is: the anchor points at the server's own
   streaming endpoint and the browser saves straight from it, spooling to disk
   as the bytes arrive. The address carries its own short-lived credential, so
   the session cookie — which a home-screen app does not share with anything —
   is not needed for it to work.

   In a browser the response being an attachment is what keeps this from being
   the navigation trap above: a page handed a download does not leave for it.
   A home-screen app is the exception, and the one that bit: iOS ignores the
   download attribute there and points the app's own window at the archive,
   which it cannot show and cannot leave. So a standalone app hands the
   address to the system browser instead — a new window from a home-screen
   app opens in Safari — where Downloads can hold it and this app is still
   where it was when the user comes back. The address carries its own
   credential, so the browser needs no session to follow it.

   Returns where the download went: 'saved' when this window is saving it,
   'browser' when the system browser was handed the address. */
export function downloadFromLink(url, filename) {
  if (isStandalone()) {
    window.open(url, '_blank', 'noopener')
    return 'browser'
  }
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.appendChild(link)
  link.click()
  link.remove()
  return 'saved'
}

/* Rebuilding a file takes as long as the slowest account holding a part, so
   whatever control started it has to be able to say it is still working. */
export function useDownload(onError) {
  const [downloading, setDownloading] = useState(false)

  const start = useCallback(async (file) => {
    setDownloading(true)
    try {
      await downloadFile(file)
    } catch (err) {
      onError(err.message)
    } finally {
      setDownloading(false)
    }
  }, [onError])

  return [start, downloading]
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
