/* Thumbnails are made here, in the tab that is uploading the file, and not on
   the server — because this is the only place they can be made at all.

   The browser decodes HEIC, which most photos taken on a phone are and which
   Go cannot read; it honours the EXIF orientation that would otherwise leave a
   third of them lying on their side; and with pdf.js it renders the first page
   of a PDF, which nothing in a static Go binary can do without a C library.

   What leaves here is a small JPEG. The server decodes and re-encodes it
   before storing, so nothing downstream depends on this having been honest. */

import { PDF_OPTIONS, loadPDFJS } from './pdfjs'
import { previewKind } from './theme'

/* The longest edge of a stored thumbnail. Matches internal/thumb.Size — the
   server re-encodes to the same number, so producing anything larger only
   wastes the upload. */
export const THUMB_EDGE = 256

const JPEG_QUALITY = 0.8

/* Past this, decoding the source costs more than the picture is worth on a
   phone. The file still uploads; it just keeps its icon. */
const MAX_SOURCE_BYTES = 80 * 1024 * 1024

/* Which files get a picture. Everything else — audio, text, archives — is
   better served by the glyph it already has. */
export function thumbnailKind(mime = '', name = '') {
  const kind = previewKind(mime, name)
  return kind === 'image' || kind === 'pdf' ? kind : null
}

/* Renders a file to a small JPEG, or resolves null when it cannot. A missing
   thumbnail is never an error: the list falls back to the icon, so nothing
   here is allowed to fail an upload. */
export async function makeThumbnail(blob, mime, name) {
  const kind = thumbnailKind(mime || blob?.type || '', name || '')
  if (!kind || !blob || blob.size > MAX_SOURCE_BYTES) return null

  try {
    return kind === 'pdf' ? await fromPDF(blob) : await fromImage(blob)
  } catch {
    // An undecodable format, a corrupt file, a browser without the codec.
    return null
  }
}

/* The backfill path: an element that is already on screen has been decoded and
   is same-origin, so a thumbnail can be taken from it without fetching the file
   a second time.

   An <img> reports its size as naturalWidth and a <video> as videoWidth, and a
   video is worth taking one from: nothing makes a picture of a video at upload
   time — the browser would have to decode a film to do it — so the frame
   somebody is already watching is the only cheap one there is. */
export async function thumbnailFromElement(el) {
  const w = el.naturalWidth || el.videoWidth || el.width
  const h = el.naturalHeight || el.videoHeight || el.height
  if (!w || !h) return null
  try {
    return await draw(el, w, h)
  } catch {
    return null
  }
}

async function fromImage(blob) {
  let bitmap
  try {
    // Without this the picture is drawn as stored, and a photo taken in
    // portrait arrives on its side.
    bitmap = await createImageBitmap(blob, { imageOrientation: 'from-image' })
  } catch {
    bitmap = await createImageBitmap(blob)
  }

  try {
    return await draw(bitmap, bitmap.width, bitmap.height)
  } finally {
    bitmap.close?.()
  }
}

async function fromPDF(blob) {
  const pdfjs = await loadPDFJS()
  const doc = await pdfjs.getDocument({
    ...PDF_OPTIONS,
    data: new Uint8Array(await blob.arrayBuffer()),
    disableAutoFetch: true,
  }).promise

  try {
    const page = await doc.getPage(1)
    const base = page.getViewport({ scale: 1 })
    const scale = THUMB_EDGE / Math.max(base.width, base.height)
    const viewport = page.getViewport({ scale })

    const canvas = document.createElement('canvas')
    canvas.width = Math.max(1, Math.round(viewport.width))
    canvas.height = Math.max(1, Math.round(viewport.height))

    const ctx = canvas.getContext('2d')
    // A page is transparent where it is blank, and a transparent JPEG is a
    // black one. Paper is white.
    ctx.fillStyle = '#ffffff'
    ctx.fillRect(0, 0, canvas.width, canvas.height)

    await page.render({ canvasContext: ctx, viewport }).promise
    return await encode(canvas)
  } finally {
    doc.destroy()
  }
}

/* Scales a decoded source down to fit THUMB_EDGE and encodes it. */
function draw(source, width, height) {
  const scale = Math.min(1, THUMB_EDGE / Math.max(width, height))
  const canvas = document.createElement('canvas')
  canvas.width = Math.max(1, Math.round(width * scale))
  canvas.height = Math.max(1, Math.round(height * scale))

  const ctx = canvas.getContext('2d')
  ctx.imageSmoothingEnabled = true
  ctx.imageSmoothingQuality = 'high'
  ctx.drawImage(source, 0, 0, canvas.width, canvas.height)
  return encode(canvas)
}

function encode(canvas) {
  return new Promise((resolve, reject) => {
    canvas.toBlob(
      (blob) => (blob ? resolve(blob) : reject(new Error('could not encode the thumbnail'))),
      'image/jpeg',
      JPEG_QUALITY,
    )
  })
}

/* How many files are decoded at once when a whole folder was chosen.
 *
 * Making a thumbnail decodes the source, and a decoded image is its pixels, not
 * its file: a 20 MB photo off a modern phone is a couple of hundred megabytes
 * once it is a bitmap. Asking for all of them at the same time — which is what
 * mapping an upload straight through Promise.all does — is asking the tab to
 * hold every one of those bitmaps at once, and a folder of a hundred photos
 * killed the tab outright, which from the other side of the screen is the
 * window simply disappearing with nothing said. A few lanes at a time is as
 * fast in practice, since the work is decode-bound either way. */
export const THUMBNAIL_LANES = 3

/* Thumbnails for a list of files, in the same order, a few at a time. Each is
   null where no picture could be made, exactly as makeThumbnail leaves it. */
export async function makeThumbnails(files, lanes = THUMBNAIL_LANES) {
  const out = new Array(files.length).fill(null)
  let next = 0

  const lane = async () => {
    for (;;) {
      const i = next
      next += 1
      if (i >= files.length) return
      const file = files[i]
      out[i] = await makeThumbnail(file, file?.type, file?.name)
    }
  }

  await Promise.all(Array.from({ length: Math.min(lanes, files.length) }, lane))
  return out
}
