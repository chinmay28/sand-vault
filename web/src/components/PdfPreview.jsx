import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { useIsMobile } from '../hooks'
import { PDF_OPTIONS, loadPDFJS } from '../pdfjs'
import { Banner, Spinner } from './ui'

/* A PDF, drawn a page at a time onto a canvas.

   Handing the file to a framed browser viewer was the obvious thing and it
   only worked on half the devices: iOS Safari renders a framed PDF as a blank
   box or a single unscrollable page, so a phone got an apology and a download
   button instead of the document it asked for. pdf.js is already in this
   bundle — it is what makes a PDF's thumbnail its first page — so the same
   renderer draws the preview, and a phone and a desktop now show the same
   document with the same controls.

   Only the page being looked at is drawn, and pdf.js asks for the byte ranges
   that page needs rather than the whole file, so opening a 300-page scan costs
   the parts of it you actually read. */

/* How tall the page may get before the modal's own chrome and the buttons
   under it start being pushed off the screen. Past this the page scrolls. */
const VIEWER_HEIGHT = 'calc(var(--app-height) * 0.62)'

/* Zoom steps, as a multiple of "the page fits the viewer's width". Fit is
   where every page starts; the rest are for the phone, where a dense A4 page
   scaled to 360px is legible only as a shape. */
const ZOOMS = [1, 1.5, 2, 3]

/* A canvas past this many pixels is one a phone will accept, fail to allocate
   and hand back blank, so the device pixel ratio is a request rather than a
   promise. Roughly a 2000×2000 page — beyond what a screen resolves anyway. */
const MAX_CANVAS_PIXELS = 4e6

/* Every read here ends in round-trips to two cloud accounts, so pdf.js asking
   for one bigger range beats it asking for four of its default size. */
const RANGE_CHUNK = 256 * 1024

/* Below this much movement a resize is the scrollbar appearing or a phone's
   address bar sliding away, and re-rendering the page for it is wasted work. */
const RESIZE_SLOP = 4

/* The margin around the page, and so what "fits the width" has to leave room
   for: fitting to the viewer's full width instead puts the page's own edge
   under a scrollbar it caused. */
const GUTTER = 10

export default function PdfPreview({ url, name, onFirstPage }) {
  const mobile = useIsMobile()
  const boxRef = useRef(null)
  const canvasRef = useRef(null)

  const [doc, setDoc] = useState(null)
  const [pages, setPages] = useState(0)
  const [page, setPage] = useState(1)
  const [zoom, setZoom] = useState(0)
  const [width, setWidth] = useState(0)
  const [error, setError] = useState(null)
  /* How much of the file has been gathered so far. Worth showing: the bytes
     are coming back off two cloud accounts, so the wait is a network away
     rather than a decode away. */
  const [progress, setProgress] = useState(null)
  const [drawn, setDrawn] = useState(false)

  /* Read through a ref: the dialog hands us a fresh callback on every render,
     and a new one in the render effect's dependencies would redraw the page
     that had just finished drawing, for ever. */
  const firstPageRef = useRef(onFirstPage)
  firstPageRef.current = onFirstPage

  /* The width the page is fitted to is the viewer's, measured rather than
     assumed: the modal is one width on a desktop and another on a phone, and
     both change when the window does. */
  useLayoutEffect(() => {
    const el = boxRef.current
    if (!el) return

    const measure = () => setWidth((prev) => (
      Math.abs(el.clientWidth - prev) < RESIZE_SLOP ? prev : el.clientWidth
    ))
    measure()

    const observer = new ResizeObserver(measure)
    observer.observe(el)
    return () => observer.disconnect()
  }, [])

  /* Open the document. */
  useEffect(() => {
    let cancelled = false
    let task = null

    setDoc(null)
    setPages(0)
    setPage(1)
    setError(null)
    setProgress(null)
    setDrawn(false)

    ;(async () => {
      const pdfjs = await loadPDFJS()
      if (cancelled) return

      task = pdfjs.getDocument({ ...PDF_OPTIONS, url, rangeChunkSize: RANGE_CHUNK })
      task.onProgress = ({ loaded, total }) => {
        if (!cancelled && total) setProgress(Math.min(1, loaded / total))
      }

      const opened = await task.promise
      if (cancelled) return
      setPages(opened.numPages)
      setDoc(opened)
    })().catch((err) => {
      // A viewer that has been closed has nothing to report to, and closing it
      // is itself what makes the load fail.
      if (cancelled || !boxRef.current) return
      setError(err?.message || 'this file could not be read as a PDF')
    })

    return () => {
      cancelled = true
      // Tears down the worker and the document with it, so a preview closed
      // mid-load stops pulling parts off the accounts.
      task?.destroy()
    }
  }, [url])

  /* Draw the page that is being looked at, and only that one. */
  useEffect(() => {
    if (!doc || !width) return
    let cancelled = false
    let task = null

    ;(async () => {
      const pdfPage = await doc.getPage(page)
      if (cancelled) return

      const base = pdfPage.getViewport({ scale: 1 })
      const cssWidth = Math.max(1, width - GUTTER * 2) * ZOOMS[zoom]
      const cssHeight = (cssWidth * base.height) / base.width

      // Draw at the screen's real pixels so the text is not soft, unless that
      // asks for a canvas the device will not give us.
      const ratio = Math.min(
        window.devicePixelRatio || 1,
        Math.sqrt(MAX_CANVAS_PIXELS / (cssWidth * cssHeight)),
      )
      const viewport = pdfPage.getViewport({ scale: (cssWidth / base.width) * ratio })

      const canvas = canvasRef.current
      if (!canvas) return
      canvas.width = Math.max(1, Math.round(viewport.width))
      canvas.height = Math.max(1, Math.round(viewport.height))
      canvas.style.width = `${Math.round(cssWidth)}px`
      canvas.style.height = `${Math.round(cssHeight)}px`

      const ctx = canvas.getContext('2d')
      // A page is transparent where it is blank, and this app's background is
      // nearly black. Paper is white.
      ctx.fillStyle = '#ffffff'
      ctx.fillRect(0, 0, canvas.width, canvas.height)

      task = pdfPage.render({ canvasContext: ctx, viewport })
      await task.promise
      if (cancelled) return

      setDrawn(true)
      // The first page is on screen and decoded, which is exactly the picture
      // the list wants for a file that has none — and it costs no second
      // gather from the accounts to take it.
      if (page === 1) firstPageRef.current?.(canvas)
    })().catch((err) => {
      // A render that was cancelled was replaced by the one that cancelled it,
      // and one that failed because the dialog closed under it has nobody left
      // to tell.
      if (cancelled || !boxRef.current) return
      if (err?.name === 'RenderingCancelledException') return
      setError(err?.message || 'this page could not be drawn')
    })

    return () => {
      cancelled = true
      task?.cancel()
    }
  }, [doc, page, zoom, width])

  /* A new page starts at the top of itself, not wherever the last one was
     scrolled to. */
  const turnTo = useCallback((next) => {
    const wanted = Math.min(Math.max(1, next), pages || 1)
    if (wanted === page) return
    setDrawn(false)
    if (boxRef.current) boxRef.current.scrollTop = 0
    setPage(wanted)
  }, [page, pages])

  /* Redrawing at a new scale blanks the canvas first, so the spinner covers it
     rather than the page appearing to have emptied itself. A resize does not
     go through here: the page is redrawn constantly while a window is being
     dragged, and blinking through every step of that is worse than a slightly
     stale page. */
  const zoomBy = useCallback((step) => {
    const wanted = Math.min(Math.max(0, zoom + step), ZOOMS.length - 1)
    if (wanted === zoom) return
    setDrawn(false)
    setZoom(wanted)
  }, [zoom])

  /* Arrow keys turn pages, which is how every other document viewer behaves.
     Not while something is being typed into, though — the address in the
     stream dialog is a field someone may be selecting their way along. */
  useEffect(() => {
    const onKey = (e) => {
      if (e.metaKey || e.ctrlKey || e.altKey) return
      const tag = e.target?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || e.target?.isContentEditable) return

      if (e.key === 'ArrowRight' || e.key === 'PageDown') turnTo(page + 1)
      else if (e.key === 'ArrowLeft' || e.key === 'PageUp') turnTo(page - 1)
      else return
      e.preventDefault()
    }

    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [page, turnTo])

  if (error) {
    return (
      <div style={{ width: '100%', padding: '16px', boxSizing: 'border-box' }}>
        <Banner tone="error">
          {name} could not be shown here — {error}. Download it below, or copy
          its address and open it with your own viewer.
        </Banner>
      </div>
    )
  }

  const waiting = !doc || !drawn

  return (
    <div style={{ width: '100%', minWidth: 0 }} data-pdf-preview="true">
      <div
        ref={boxRef}
        style={{
          position: 'relative',
          // Tall enough for the spinner, and otherwise as tall as the page
          // itself up to the ceiling: a landscape page should not sit in a
          // window sized for a portrait one.
          minHeight: '180px',
          maxHeight: VIEWER_HEIGHT,
          overflow: 'auto',
          // A phone scrolls a zoomed page around rather than reflowing it.
          WebkitOverflowScrolling: 'touch',
          background: COLORS.bg,
        }}
      >
        {/* Sized to the page when the page is the wider of the two, so a
            zoomed-in canvas scrolls into view instead of being centred with
            its left-hand edge cut off. */}
        <div style={{
          minWidth: 'fit-content',
          display: 'flex',
          justifyContent: 'center',
          padding: `${GUTTER}px`,
        }}>
          <canvas
            ref={canvasRef}
            aria-label={`${name}, page ${page}${pages ? ` of ${pages}` : ''}`}
            role="img"
            style={{ display: 'block', visibility: waiting ? 'hidden' : 'visible' }}
          />
        </div>

        {waiting && (
          <div style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            justifyContent: 'center',
            gap: '10px',
            fontFamily: FONT.mono,
            fontSize: '11px',
            color: COLORS.textMuted,
          }}>
            <Spinner size={20} />
            {progress !== null && progress < 1 && `Gathering… ${Math.round(progress * 100)}%`}
          </div>
        )}
      </div>

      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: '8px',
        padding: mobile ? '6px 8px' : '6px 10px',
        borderTop: `1px solid ${COLORS.border}`,
        background: COLORS.surface,
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          <PageButton label="Previous page" onClick={() => turnTo(page - 1)} disabled={page <= 1}>‹</PageButton>
          <span style={{
            fontFamily: FONT.mono,
            fontSize: '11px',
            color: COLORS.textDim,
            minWidth: '74px',
            textAlign: 'center',
            whiteSpace: 'nowrap',
          }}>{pages ? `Page ${page} / ${pages}` : '…'}</span>
          <PageButton label="Next page" onClick={() => turnTo(page + 1)} disabled={!pages || page >= pages}>›</PageButton>
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          <PageButton label="Zoom out" onClick={() => zoomBy(-1)} disabled={zoom === 0}>−</PageButton>
          <span style={{
            fontFamily: FONT.mono,
            fontSize: '11px',
            color: COLORS.textDim,
            minWidth: '44px',
            textAlign: 'center',
          }}>{zoom === 0 ? 'Fit' : `${ZOOMS[zoom]}×`}</span>
          <PageButton label="Zoom in" onClick={() => zoomBy(1)} disabled={zoom === ZOOMS.length - 1}>+</PageButton>
        </div>
      </div>
    </div>
  )
}

/* The viewer's own controls: a row of them sits under the page, so they are
   squarer and quieter than the dialog's buttons — and still a fingertip
   across, since turning a page is the most-tapped thing in this dialog. */
function PageButton({ label, onClick, disabled, children }) {
  const mobile = useIsMobile()

  return (
    <button
      onClick={onClick}
      disabled={disabled}
      aria-label={label}
      title={label}
      data-icon-button="true"
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        minWidth: mobile ? '44px' : '30px',
        minHeight: mobile ? '44px' : '30px',
        padding: 0,
        background: 'transparent',
        border: `1px solid ${disabled ? 'transparent' : COLORS.border}`,
        borderRadius: '6px',
        color: disabled ? COLORS.textMuted : COLORS.text,
        fontFamily: FONT.mono,
        fontSize: '16px',
        lineHeight: 1,
        cursor: disabled ? 'default' : 'pointer',
        opacity: disabled ? 0.4 : 1,
      }}
    >{children}</button>
  )
}
