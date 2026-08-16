import React, { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { COLORS, FONT } from '../theme'
import { APP_VERSION } from '../version'

/* The brand lockup: mark, then the name over the running version. The version
   belongs here rather than in a footer — it's the first thing you want when
   something looks wrong, and it tells you at a glance whether the build you're
   looking at is the one you just deployed. */
export function Brand({ size = 'md' }) {
  const large = size === 'lg'

  return (
    <span style={{
      display: 'inline-flex',
      // Two-line lockup, so hang everything from the top rather than centring
      // against the taller text block.
      alignItems: 'flex-start',
      gap: large ? '12px' : '9px',
    }}>
      <img
        src="/icon.svg"
        alt=""
        aria-hidden="true"
        style={{
          width: large ? '44px' : '26px',
          height: large ? '44px' : '26px',
          borderRadius: large ? '10px' : '6px',
          display: 'block',
        }}
      />
      <span style={{ display: 'flex', flexDirection: 'column', lineHeight: 1.15 }}>
        {/* Two-tone wordmark, in two hands. SAND keeps the tracked-out
            monospace and the accent it has always had — the machine half of the
            name. Vault is written, in the handwriting face the platform has, and
            sits back in the text colour: the pair reads as one name, and the
            join between them is the point rather than a coincidence.

            A script is drawn joined, so the tracking that spaces SAND out has
            to come off it — letters set 5px apart stop being handwriting. It is
            set larger to compensate for a script's smaller x-height, and hangs
            on the same baseline via `alignItems: baseline` rather than being
            nudged with a magic number.

            The wordmark never wraps, so on a narrow screen it is the thing
            that decides how wide the header has to be — size and tracking come
            down with the viewport rather than pushing everything else off. */}
        <span style={{
          display: 'inline-flex',
          alignItems: 'baseline',
          fontSize: large ? 'clamp(20px, 6.4vw, 26px)' : 'clamp(13.5px, 4.4vw, 17px)',
          whiteSpace: 'nowrap',
        }}>
          <span style={{
            fontFamily: FONT.mono,
            fontWeight: 700,
            letterSpacing: large ? 'clamp(4px, 1.7vw, 7px)' : 'clamp(2px, 1vw, 4px)',
            color: COLORS.accent,
          }}>SAND</span>
          <span style={{
            fontFamily: FONT.script,
            // Scripts sit small for their point size, and a swash can start
            // left of where the glyph is measured from — hence the size bump
            // and the room on the left for it to lean into. Tuned against the
            // face in web/fonts, where this lands the script's x-height on
            // SAND's cap height and lets the ascenders rise past it. It is the
            // one number to touch if the face ever changes.
            fontSize: '1.65em',
            fontWeight: 500,
            // Deliberately not italic. Every face in the stack is already
            // written on a slant, so asking for italic on top of that makes
            // the browser synthesise a second one — a script sheared over
            // again, which is exactly as bad as it sounds.
            letterSpacing: '0.01em',
            paddingLeft: '0.08em',
            marginLeft: large ? '5px' : '3px',
            color: COLORS.textDim,
          }}>Vault</span>
        </span>
        <span style={{
          fontFamily: FONT.mono,
          // Tabular figures so the number doesn't shimmy as the patch count
          // rolls over.
          fontVariantNumeric: 'tabular-nums',
          fontSize: large ? '11px' : '10px',
          fontWeight: 500,
          letterSpacing: '0.05em',
          color: COLORS.textMuted,
          marginTop: '2px',
        }}>{APP_VERSION}</span>
      </span>
    </span>
  )
}

/** How long the developer badge stays up when the header mark is tapped.
 *  Kept in sync with the fade/scale animation below — the CSS runs on its own
 *  clock, this unmounts it. */
const DEV_FLASH_MS = 3000

/* The developer credit mark. The artwork is a dark badge in its own right, so
   it stays dark regardless of surroundings and hangs off a hairline divider. */
export function DevMark({ bare = false }) {
  const [flash, setFlash] = useState(false)
  const [hover, setHover] = useState(false)

  useEffect(() => {
    if (!flash) return

    const timer = window.setTimeout(() => setFlash(false), DEV_FLASH_MS)
    // Nobody should be stuck waiting out an animation — Escape ends it early,
    // as does a click anywhere on the overlay.
    const onKey = (e) => { if (e.key === 'Escape') setFlash(false) }
    window.addEventListener('keydown', onKey)

    return () => {
      window.clearTimeout(timer)
      window.removeEventListener('keydown', onKey)
    }
  }, [flash])

  return (
    <>
      <button
        type="button"
        aria-label="Show the developer badge"
        onClick={() => setFlash(true)}
        onMouseEnter={() => setHover(true)}
        onMouseLeave={() => setHover(false)}
        style={{
          display: 'inline-flex',
          flex: 'none',
          alignItems: 'center',
          justifyContent: 'center',
          // The badge itself stops reading below 36px, so the target grows
          // around it rather than the artwork growing with it.
          minWidth: '44px',
          minHeight: '44px',
          border: 0,
          // In the header the mark hangs off the nav on a hairline; standing
          // on its own there is nothing to divide it from, so drop the rule.
          borderLeft: bare ? 0 : `1px solid ${COLORS.border}`,
          borderRadius: 0,
          background: 'none',
          padding: bare ? 0 : '0 0 0 12px',
          marginLeft: bare ? 0 : '4px',
          cursor: 'pointer',
          WebkitTapHighlightColor: 'transparent',
        }}
      >
        {/* The button carries the label; the image would only repeat it. */}
        <img
          src="/dev-badge.png"
          alt=""
          aria-hidden="true"
          style={{
            display: 'block',
            // 36px is the smallest size the badge's wordmark still reads at —
            // below it the mark collapses into an anonymous dark dot.
            width: '36px',
            height: '36px',
            borderRadius: '50%',
            background: '#101010',
            boxShadow: hover
              ? `0 0 0 1px ${COLORS.accent}8c`
              : `0 0 0 1px ${COLORS.border}`,
            opacity: hover ? 1 : 0.8,
            transition: 'opacity 140ms ease, box-shadow 140ms ease',
          }}
        />
      </button>

      {flash && <DevFlash onDismiss={() => setFlash(false)} />}
    </>
  )
}

/* The badge thrown up full screen for a beat. Everything here is on one clock:
   the veil fades, the lockup lands with a small overshoot and drifts out.

   Portalled to the body so "full screen" stays true when the mark is tapped
   from inside the accounts drawer, whose transform would otherwise become what
   this measures itself against. */
function DevFlash({ onDismiss }) {
  return createPortal(
    <div
      onClick={onDismiss}
      className="sand-dev-veil"
      style={{
        position: 'fixed',
        inset: 0,
        // Above every other layer, modals included.
        zIndex: 200,
        display: 'grid',
        placeItems: 'center',
        padding: '32px',
        cursor: 'pointer',
        background: 'rgba(10, 14, 23, 0.78)',
        backdropFilter: 'saturate(1.2) blur(14px)',
        animation: 'sand-dev-veil 3s ease-in-out forwards',
      }}
    >
      <div
        className="sand-dev-lockup"
        style={{
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: '22px',
          // The scale lives on the lockup so the badge and handle never drift
          // apart mid-animation.
          animation: 'sand-dev-badge 3s cubic-bezier(0.22, 1, 0.36, 1) forwards',
        }}
      >
        <img
          src="/dev-badge-full.png"
          alt="Built by CM Hegday — 0x434d"
          style={{
            width: 'min(64vmin, 26rem)',
            height: 'min(64vmin, 26rem)',
            borderRadius: '50%',
            boxShadow: `0 0 0 1px ${COLORS.border}, 0 24px 60px rgba(0,0,0,0.55)`,
          }}
        />
        {/* Monospace to echo the badge's own wordmark, tracked out so it reads
            as a signature under the mark rather than a line of body copy. */}
        <span style={{
          fontFamily: FONT.mono,
          fontSize: 'clamp(0.85rem, 3.2vmin, 1.1rem)',
          letterSpacing: '0.08em',
          color: COLORS.textMuted,
        }}>github.com/chinmay28</span>
      </div>
    </div>,
    document.body,
  )
}
