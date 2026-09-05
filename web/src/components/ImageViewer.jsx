import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { COLORS, FONT } from '../theme'
import { useIsMobile } from '../hooks'
import { api } from '../api'
import { readToken } from '../readwatch'
import ReadStatus from './ReadStatus'
import { IconButton, Spinner } from './ui'

/* How long a slide stays up before the show moves on. Long enough to actually
   look at a photograph, short enough that a folder of forty is not a coffee
   break. */
const SLIDE_MS = 4000

/* Zoom stops. The floor is the fitted image — zooming out past what fits on
   the screen only makes the picture smaller and the letterboxing bigger. The
   ceiling is past any phone photo's pixels; beyond it every step is blur. */
const MIN_SCALE = 1
const MAX_SCALE = 8

/* Where a double tap lands: close enough to read a label or a face, far enough
   from the ceiling that pinching on from there still has somewhere to go. */
const TAP_SCALE = 2.5

/* A drag has to travel this far, and be clearly sideways, before letting go
   turns the page. Anything shorter snaps back — a wobbly tap is not a swipe. */
const SWIPE_PX = 60

const clamp = (value, lo, hi) => Math.min(hi, Math.max(lo, value))

/* One zoom step, kept pure so wheel, buttons, pinch and double tap all agree.
   (cx, cy) is the point to zoom around, measured from the stage's centre — the
   pixel under the cursor stays under the cursor. The pan is clamped so the
   image can always be dragged back on screen: at scale 1 that clamp is zero,
   which is also what recentres everything on the way back out. */
function applyZoom(view, nextScale, cx, cy, stage) {
  const scale = clamp(nextScale, MIN_SCALE, MAX_SCALE)
  if (scale === MIN_SCALE) return { scale: MIN_SCALE, x: 0, y: 0 }
  const ratio = scale / view.scale
  const maxX = stage ? ((scale - 1) * stage.clientWidth) / 2 : Infinity
  const maxY = stage ? ((scale - 1) * stage.clientHeight) / 2 : Infinity
  return {
    scale,
    x: clamp(cx - (cx - view.x) * ratio, -maxX, maxX),
    y: clamp(cy - (cy - view.y) * ratio, -maxY, maxY),
  }
}

/* The whole screen given to one image at a time: zoom by wheel, pinch, double
   tap or the buttons; pan by dragging; the folder's other images a swipe, an
   arrow key or a running slide show away.

   Not a Modal, deliberately. A modal is chrome around content; this is the
   opposite — as close to nothing but the photograph as the controls allow. It
   sits above the preview dialog that opened it, and it swallows its own keys
   in the capture phase so Escape closes the viewer and only the viewer rather
   than falling through and taking the dialog underneath with it. */
export default function ImageViewer({ images, start = 0, onClose, onShown }) {
  const mobile = useIsMobile()
  const count = images.length
  const [index, setIndex] = useState(() => clamp(start, 0, count - 1))
  const [view, setView] = useState({ scale: 1, x: 0, y: 0 })
  const [playing, setPlaying] = useState(false)
  const [loading, setLoading] = useState(true)
  /* Per file, not one flag: the show goes on past a broken image, and coming
     back to it should say "broken" again rather than spin. */
  const [failed, setFailed] = useState(() => new Set())

  const stageRef = useRef(null)
  /* The fingers currently down and what they are doing. Gesture state lives in
     refs because it changes on every pointer move and none of it is drawn —
     only the view it produces is. */
  const pointers = useRef(new Map())
  const gesture = useRef(null)
  const lastTap = useRef(null)

  /* Read through refs from the window-level listeners, so a re-render cannot
     leave a key press acting on a stale index or scale. */
  const viewRef = useRef(view)
  viewRef.current = view
  const indexRef = useRef(index)
  indexRef.current = index
  const closeRef = useRef(onClose)
  closeRef.current = onClose

  const entry = images[index]
  const broken = failed.has(entry.file.id)
  const zoomed = view.scale > 1.001

  /* A token per image, so the wait for each can be named — the folder's
     photos are on different clouds, and a slide show that stalls should say
     which one it stalled on. */
  const watch = useMemo(() => readToken(), [entry.file.id])

  const step = useCallback((delta) => {
    setIndex((i) => (i + delta + count) % count)
    setView({ scale: 1, x: 0, y: 0 })
    setLoading(true)
  }, [count])

  const zoomTo = useCallback((scale, cx = 0, cy = 0) => {
    setView((v) => applyZoom(v, scale, cx, cy, stageRef.current))
  }, [])

  const zoomBy = useCallback((factor, cx = 0, cy = 0) => {
    setView((v) => applyZoom(v, v.scale * factor, cx, cy, stageRef.current))
  }, [])

  /* Attached by hand rather than as onWheel: React registers wheel listeners
     passively, and a passive listener cannot preventDefault — so the page
     behind the viewer would scroll and pinch-zoom gestures that arrive as
     ctrl+wheel would zoom the browser instead of the photograph. */
  useEffect(() => {
    const stage = stageRef.current
    if (!stage) return undefined
    const onWheel = (e) => {
      e.preventDefault()
      const rect = stage.getBoundingClientRect()
      zoomBy(
        Math.exp(-e.deltaY * 0.0022),
        e.clientX - rect.left - rect.width / 2,
        e.clientY - rect.top - rect.height / 2,
      )
    }
    stage.addEventListener('wheel', onWheel, { passive: false })
    return () => stage.removeEventListener('wheel', onWheel)
  }, [zoomBy])

  /* Capture phase, and stopPropagation on anything handled: the preview dialog
     under this viewer is listening for Escape on the same window, and one key
     press must not close both. */
  useEffect(() => {
    const onKey = (e) => {
      let acted = true
      if (e.key === 'Escape') closeRef.current?.(indexRef.current)
      else if (e.key === 'ArrowRight' && count > 1) step(1)
      else if (e.key === 'ArrowLeft' && count > 1) step(-1)
      else if (e.key === '+' || e.key === '=') zoomBy(1.4)
      else if (e.key === '-' || e.key === '_') zoomBy(1 / 1.4)
      else if (e.key === '0') zoomTo(1)
      else if (e.key === ' ' && count > 1) setPlaying((p) => !p)
      else acted = false
      if (acted) { e.preventDefault(); e.stopPropagation() }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [count, step, zoomBy, zoomTo])

  /* The slide show: a timer per slide rather than an interval, so a manual
     step quietly restarts the clock instead of the next advance landing a
     moment after it. It holds while the image is zoomed — someone leaning in
     to read something is the one moment the show must not turn the page. */
  useEffect(() => {
    if (!playing || count < 2 || zoomed) return undefined
    const timer = setTimeout(() => step(1), SLIDE_MS)
    return () => clearTimeout(timer)
  }, [playing, count, zoomed, index, step])

  /* Fetch the neighbours while this one is being looked at. Each is a full
     rebuild on the server's side, which is exactly why doing it a slide early
     is the difference between a show and a loading spinner every four
     seconds. */
  useEffect(() => {
    if (count < 2) return
    new Image().src = api.contentURL(images[(index + 1) % count].file.id)
    if (count > 2) new Image().src = api.contentURL(images[(index - 1 + count) % count].file.id)
  }, [index, count, images])

  const onPointerDown = (e) => {
    if (e.pointerType === 'mouse' && e.button !== 0) return
    /* The arrows sit on the stage, and capturing their pointer here would
       steal the click they exist for. A press that starts on a button is that
       button's, not a gesture's. */
    if (e.target.closest?.('button')) return
    stageRef.current?.setPointerCapture?.(e.pointerId)
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY })

    if (pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      gesture.current = {
        type: 'pinch',
        startDist: Math.hypot(a.x - b.x, a.y - b.y),
        startScale: viewRef.current.scale,
      }
    } else if (pointers.current.size === 1) {
      gesture.current = {
        /* Zoomed in, a drag moves the image; fitted, it reaches for the next
           one. The same finger, two different questions. */
        type: viewRef.current.scale > 1.001 ? 'pan' : 'swipe',
        startX: e.clientX,
        startY: e.clientY,
        startView: viewRef.current,
        dx: 0,
        dy: 0,
        moved: false,
      }
    }
  }

  const onPointerMove = (e) => {
    if (!pointers.current.has(e.pointerId)) return
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY })
    const g = gesture.current
    if (!g) return

    if (g.type === 'pinch' && pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      const dist = Math.hypot(a.x - b.x, a.y - b.y)
      if (!dist || !g.startDist) return
      const rect = stageRef.current.getBoundingClientRect()
      g.moved = true
      zoomTo(
        g.startScale * (dist / g.startDist),
        (a.x + b.x) / 2 - rect.left - rect.width / 2,
        (a.y + b.y) / 2 - rect.top - rect.height / 2,
      )
    } else if (g.type === 'pan') {
      const dx = e.clientX - g.startX
      const dy = e.clientY - g.startY
      if (Math.hypot(dx, dy) > 4) g.moved = true
      setView((v) => applyZoom(
        { ...v, x: g.startView.x + dx, y: g.startView.y + dy },
        v.scale, 0, 0, stageRef.current,
      ))
    } else if (g.type === 'swipe') {
      g.dx = e.clientX - g.startX
      g.dy = e.clientY - g.startY
      if (Math.hypot(g.dx, g.dy) > 8) g.moved = true
      // The image follows the finger, so the gesture answers back before it
      // is finished — but only sideways, and only when there is a next page
      // for it to be reaching toward.
      if (count > 1) setView((v) => ({ ...v, x: g.dx }))
    }
  }

  const endPointer = (e) => {
    if (!pointers.current.delete(e.pointerId)) return
    const g = gesture.current

    if (g?.type === 'pinch') {
      if (pointers.current.size < 2) {
        gesture.current = null
        // A pinch that ends a hair over 1 leaves the image imperceptibly
        // zoomed — and a pan gesture where a swipe was wanted. Snap it home.
        setView((v) => (v.scale < 1.05 ? { scale: 1, x: 0, y: 0 } : v))
      }
      return
    }
    if (!g) return
    gesture.current = null

    if (g.type === 'swipe') {
      if (count > 1 && Math.abs(g.dx) > SWIPE_PX && Math.abs(g.dx) > Math.abs(g.dy) * 1.5) {
        step(g.dx < 0 ? 1 : -1)
        return
      }
      setView({ scale: 1, x: 0, y: 0 })
    }
    if (!g.moved && e.type !== 'pointercancel') tap(e)
  }

  /* Double tap — and double click, which arrives here the same way — toggles
     between fitted and leaned-in, around the spot that was tapped. */
  const tap = (e) => {
    const prev = lastTap.current
    lastTap.current = { t: e.timeStamp, x: e.clientX, y: e.clientY }
    if (!prev || e.timeStamp - prev.t > 320) return
    if (Math.hypot(e.clientX - prev.x, e.clientY - prev.y) > 40) return
    lastTap.current = null
    if (viewRef.current.scale > 1.001) {
      zoomTo(1)
    } else {
      const rect = stageRef.current.getBoundingClientRect()
      zoomTo(TAP_SCALE, e.clientX - rect.left - rect.width / 2, e.clientY - rect.top - rect.height / 2)
    }
  }

  /* The controls sit on a near-black ground, so the dim tones the app uses on
     its surfaces read fine here too. */
  const barText = {
    fontFamily: FONT.mono,
    fontSize: '12px',
    color: COLORS.textDim,
    whiteSpace: 'nowrap',
  }

  const arrow = (side, delta, label) => (
    <IconButton
      glyph={side === 'left' ? '‹' : '›'}
      label={label}
      size={44}
      onClick={() => step(delta)}
      style={{
        position: 'absolute',
        [side]: mobile ? '4px' : '14px',
        top: '50%',
        transform: 'translateY(-50%)',
        fontSize: '26px',
        color: COLORS.text,
        background: 'rgba(10, 14, 23, 0.55)',
        border: `1px solid ${COLORS.border}`,
        zIndex: 1,
      }}
    />
  )

  return createPortal(
    <div
      role="dialog"
      aria-modal="true"
      aria-label={entry.file.name}
      style={{
        position: 'fixed',
        inset: 0,
        zIndex: 130,
        display: 'flex',
        flexDirection: 'column',
        /* Opaque, unlike the dialogs' backdrops: this is a photo viewer, and
           the dialog's text ghosting through the letterbox reads as a defect
           in the photograph rather than as depth. */
        background: '#04070d',
      }}
    >
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: mobile ? '4px' : '10px',
        padding: `calc(6px + env(safe-area-inset-top, 0px)) ${mobile ? '8px' : '14px'} 6px`,
        flexShrink: 0,
      }}>
        <span style={{
          ...barText,
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          color: COLORS.text,
        }}>{entry.file.name}</span>
        {count > 1 && (
          <span style={barText} aria-live="polite">{index + 1} / {count}</span>
        )}

        <span style={{ flex: 1 }} />

        {/* A phone zooms by pinching and double-tapping; the buttons are for
            the pointer that cannot pinch, and for saying out loud that zoom
            is here at all. The readout doubles as the way back to fitted. */}
        {!mobile && (
          <>
            <IconButton glyph="−" label="Zoom out" onClick={() => zoomBy(1 / 1.4)} />
            <button
              onClick={() => zoomTo(1)}
              title="Back to the whole image"
              style={{
                ...barText,
                width: '48px',
                textAlign: 'center',
                background: 'none',
                border: 'none',
                padding: '6px 0',
                cursor: 'pointer',
              }}
            >{Math.round(view.scale * 100)}%</button>
            <IconButton glyph="+" label="Zoom in" onClick={() => zoomBy(1.4)} />
          </>
        )}
        {count > 1 && (
          <IconButton
            glyph={playing ? '⏸' : '▶'}
            label={playing ? 'Pause the slide show' : 'Play as a slide show'}
            size={mobile ? 44 : 34}
            onClick={() => setPlaying((p) => !p)}
            style={playing ? { color: COLORS.accent } : null}
          />
        )}
        <IconButton
          glyph="✕"
          label="Close full screen"
          size={mobile ? 44 : 34}
          onClick={() => onClose?.(index)}
        />
      </div>

      <div
        ref={stageRef}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endPointer}
        onPointerCancel={endPointer}
        style={{
          flex: 1,
          minHeight: 0,
          position: 'relative',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          overflow: 'hidden',
          /* The stage owns every touch on it — without this the browser pans
             and pinch-zooms the page and the gestures above never fire. */
          touchAction: 'none',
          cursor: zoomed ? 'grab' : 'zoom-in',
        }}
      >
        {broken ? (
          <div style={{
            padding: '24px',
            textAlign: 'center',
            fontFamily: FONT.sans,
            fontSize: '13px',
            color: COLORS.textMuted,
          }}>This file could not be rebuilt or is not a readable image.</div>
        ) : (
          <img
            /* Keyed so turning the page replaces the element: the old
               photograph must not sit there while the next one streams in
               behind the same src. */
            key={entry.file.id}
            src={api.contentURL(entry.file.id, { watch })}
            alt={entry.file.name}
            draggable={false}
            onLoad={(e) => { setLoading(false); onShown?.(entry, e.currentTarget) }}
            onError={() => {
              setLoading(false)
              setFailed((prev) => new Set(prev).add(entry.file.id))
            }}
            style={{
              maxWidth: '100%',
              maxHeight: '100%',
              transform: `translate(${view.x}px, ${view.y}px) scale(${view.scale})`,
              userSelect: 'none',
              /* The stage does the listening; an image that grabs its own
                 pointer events would start a native drag mid-pan. */
              pointerEvents: 'none',
            }}
          />
        )}

        {loading && !broken && (
          <div style={{ position: 'absolute', inset: 0, display: 'flex', alignItems: 'center', justifyContent: 'center', pointerEvents: 'none' }}>
            <Spinner size={22} />
          </div>
        )}
        {/* Under the spinner, once the wait is long enough to be worth a
            sentence. The server counts the bytes: an <img> says nothing
            until it has them all. */}
        {!broken && (
          <ReadStatus watch={watch} active={loading} size={entry.file.size} overlay untilDone />
        )}

        {count > 1 && arrow('left', -1, 'Previous image')}
        {count > 1 && arrow('right', 1, 'Next image')}
      </div>
    </div>,
    document.body,
  )
}
