/* One pdf.js, loaded once, for the two things that need it: the thumbnail a
   PDF gets when it is uploaded, and the pages the preview draws when it is
   opened.

   It is far larger than everything else in this bundle put together, so it is
   fetched only when a PDF is actually in front of someone — and from this
   origin, like every other asset here, so opening the vault still makes no
   third-party request. */

let pdfjsPromise = null

export function loadPDFJS() {
  if (!pdfjsPromise) {
    pdfjsPromise = (async () => {
      const [pdfjs, worker] = await Promise.all([
        import('pdfjs-dist'),
        import('pdfjs-dist/build/pdf.worker.min.mjs?url'),
      ])
      pdfjs.GlobalWorkerOptions.workerSrc = worker.default
      return pdfjs
    })().catch((err) => {
      // Let the next PDF try again rather than caching the failure forever.
      pdfjsPromise = null
      throw err
    })
  }
  return pdfjsPromise
}

/* What every getDocument() call here agrees on. A PDF is a document format
   with opinions about fetching things and running things; none of them are
   welcome in a vault that makes no third-party requests. */
export const PDF_OPTIONS = {
  isEvalSupported: false,
}
