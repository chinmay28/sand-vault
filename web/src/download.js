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

/* Safari reads the blob out of the object URL after the click handler has
   returned, so it cannot be revoked on the spot. A minute is far longer than
   that ever takes, and still bounds how long the plaintext stays reachable. */
const REVOKE_AFTER_MS = 60_000

export async function downloadFile(file) {
  const resp = await fetch(api.contentURL(file.id, { download: true }), {
    credentials: 'same-origin',
  })
  if (!resp.ok) throw new Error(await failureMessage(resp))

  /* The whole file lands in memory, which is the bargain the server has
     already made on its side: it gathers the parts and rebuilds the plaintext
     in memory to answer this very request. */
  const url = URL.createObjectURL(await resp.blob())

  const link = document.createElement('a')
  link.href = url
  link.download = file.name
  // Firefox only follows a synthetic click on an anchor that is in the document.
  document.body.appendChild(link)
  link.click()
  link.remove()

  setTimeout(() => URL.revokeObjectURL(url), REVOKE_AFTER_MS)
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
