/* HEIC, for the browsers that cannot draw it.

   A phone's camera writes HEIC, and Safari is the only browser that decodes
   it. Everywhere else an intact photograph was a dark box: the bytes arrived,
   matched their fingerprint, and the <img> refused them. So where the browser
   cannot, the page decodes the picture itself — libheif compiled to
   WebAssembly — and hands the browser a JPEG of it to draw.

   Three rules it keeps:

     · Only for drawing. What is decoded here is shown and thumbnailed and
       then let go of; downloading a file still fetches the stored bytes, so
       the HEIC that comes back is the HEIC that went in.
     · Only when needed. A browser that draws HEIC natively is asked first
       and, once it has said yes, never pays for the decoder at all.
     · From this origin. The decoder is three megabytes, so it is a chunk of
       its own fetched the first time a HEIC is in front of someone — like
       pdf.js — and, like everything else here, never from a third party. The
       build used is the one that needs no eval. */

let decoderPromise = null

function loadDecoder() {
  if (!decoderPromise) {
    decoderPromise = import('heic-to/csp').catch((err) => {
      // Let the next picture try again rather than caching the failure.
      decoderPromise = null
      throw err
    })
  }
  return decoderPromise
}

/* The brands an ISO media file opens with when it is a HEIF picture coded as
   HEVC. AVIF shares the container and is left alone: browsers draw it. */
const BRANDS = new Set(['heic', 'heix', 'hevc', 'hevx', 'heim', 'heis', 'mif1', 'msf1'])

/* Whether these bytes are a HEIC picture, read off the file itself rather
   than its name — a .HEIC that is really a JPEG needs no help, and a HEIC
   under another name does. */
export async function isHeic(blob) {
  if (!blob || blob.size < 12) return false
  const head = new Uint8Array(await blob.slice(0, 12).arrayBuffer())
  const text = (from) => String.fromCharCode(...head.slice(from, from + 4))
  return text(4) === 'ftyp' && BRANDS.has(text(8))
}

/* Whether this browser draws HEIC itself. Asked once, of a real picture, and
   remembered: the answer is a property of the browser. */
let native = null

async function drawsNatively(blob) {
  if (native !== null) return native
  try {
    const bitmap = await createImageBitmap(blob)
    bitmap.close?.()
    native = true
  } catch {
    native = false
  }
  return native
}

const JPEG_QUALITY = 0.92

/* A blob this browser can draw: the one given, unless it is a HEIC the
   browser cannot decode, in which case a JPEG decoded from it. Rejects only
   when it is a HEIC and the decoder could not open it either — which is then
   something worth saying about the file. */
export async function drawable(blob) {
  if (!(await isHeic(blob)) || (await drawsNatively(blob))) return blob
  const { heicTo } = await loadDecoder()
  return heicTo({ blob, type: 'image/jpeg', quality: JPEG_QUALITY })
}
