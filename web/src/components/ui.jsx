import React, { useEffect, useId, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { COLORS, FONT } from '../theme'
import { useIsMobile } from '../hooks'

/* Whether a control should be lit, and the handlers that decide it.

   Only two things light a button: a pointer that can actually hover resting on
   it, and the keyboard landing on it. A finger is neither. A touch screen
   reports a tap as an enter — and Safari goes on reporting it until something
   else is tapped — so a button lit on `pointerenter` alone stays lit long after
   the finger has gone. On a strip of icons that is not a smudge you can ignore:
   it reads as a switch left on, which is exactly what one of those buttons
   means when it really is lit.

   Focus is held to `:focus-visible` for the same reason. A tap that focuses a
   button, which some browsers do and some do not, is a press and not a place
   the keyboard is sitting. */
function useHighlight() {
  const [lit, setLit] = useState(false)

  const handlers = {
    onPointerEnter: (e) => { if (e.pointerType === 'mouse') setLit(true) },
    onPointerLeave: () => setLit(false),
    // Anything that is not a mouse puts the light out on its way past, so a
    // browser that insists a tap is a hover is corrected rather than believed.
    onPointerDown: (e) => { if (e.pointerType !== 'mouse') setLit(false) },
    onPointerUp: (e) => { if (e.pointerType !== 'mouse') setLit(false) },
    onPointerCancel: () => setLit(false),
    onFocus: (e) => { if (keyboardFocused(e.currentTarget)) setLit(true) },
    onBlur: () => setLit(false),
  }

  return [lit, handlers]
}

/* A caller's own styles, minus the properties it had nothing to say about.

   `{ background: on ? tint : undefined }` reads as "tinted when it is on, and
   otherwise leave it alone" — but spread raw it does not leave it alone. The
   key is present, so it overwrites the value underneath, and React answers an
   undefined value by removing the property outright. The button loses the
   transparent background this file gave it and the browser paints its own
   default button chrome instead: a filled grey box with a border, which on a
   strip of flat glyphs is the one thing that looks switched on. Dropping the
   undefined entries makes the shorthand mean what it reads as. */
function opinions(style) {
  if (!style) return null
  return Object.fromEntries(Object.entries(style).filter(([, value]) => value !== undefined))
}

/* Browsers that never learned the selector throw on it rather than answering
   false, and a button that cannot be sure is better left unlit. */
function keyboardFocused(node) {
  try {
    return node.matches(':focus-visible')
  } catch {
    return false
  }
}

export function Button({ variant = 'default', size = 'md', style, disabled, ...props }) {
  const [hover, hoverHandlers] = useHighlight()
  // Every button gets a fingertip-sized height on a phone, from the toolbar
  // down to a "Disconnect" at the foot of the accounts drawer. Set here rather
  // than in the coarse-pointer rule so a narrow window is laid out the way a
  // phone is, and because an inline height would outrank that rule anyway.
  const mobile = useIsMobile()

  const palette = {
    primary: { bg: COLORS.accent, fg: COLORS.bg, border: COLORS.accent },
    danger: { bg: 'transparent', fg: COLORS.error, border: 'rgba(239,68,68,0.4)' },
    ghost: { bg: 'transparent', fg: COLORS.textDim, border: 'transparent' },
    default: { bg: COLORS.surfaceRaised, fg: COLORS.text, border: COLORS.border },
  }[variant]

  const padding = size === 'sm' ? '6px 10px' : '10px 16px'

  return (
    <button
      {...props}
      disabled={disabled}
      {...hoverHandlers}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: '6px',
        minHeight: mobile ? '44px' : undefined,
        padding,
        background: palette.bg,
        color: palette.fg,
        border: `1px solid ${palette.border}`,
        borderRadius: '6px',
        fontFamily: FONT.mono,
        fontSize: size === 'sm' ? '11px' : '12px',
        fontWeight: 600,
        letterSpacing: '0.5px',
        cursor: disabled ? 'not-allowed' : 'pointer',
        opacity: disabled ? 0.45 : 1,
        filter: hover && !disabled ? 'brightness(1.25)' : 'none',
        transition: 'filter 0.15s ease, border-color 0.15s ease',
        whiteSpace: 'nowrap',
        ...opinions(style),
      }}
    />
  )
}

/* `trailing` is painted over the right-hand end of the field itself, so it
   stays put whatever the field's height turns out to be — which varies, since
   touch screens get a taller box and a bigger type size. */
export function Input({ label, help, trailing, id, style, ...props }) {
  const [focused, setFocused] = useState(false)
  // The label points at the field rather than wrapping it: `trailing` holds a
  // button, and a button inside a label ends up in the field's spoken name.
  const generated = useId()
  const fieldId = id || generated
  const helpId = `${fieldId}-help`

  return (
    <div style={{ marginBottom: '14px' }}>
      {label && (
        <label htmlFor={fieldId} style={{
          display: 'block',
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 600,
          letterSpacing: '1.5px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
          marginBottom: '6px',
        }}>{label}</label>
      )}
      <div style={{ position: 'relative' }}>
        <input
          {...props}
          id={fieldId}
          aria-describedby={help ? helpId : undefined}
          onFocus={(e) => { setFocused(true); props.onFocus?.(e) }}
          onBlur={(e) => { setFocused(false); props.onBlur?.(e) }}
          style={{
            width: '100%',
            padding: '10px 12px',
            background: COLORS.bg,
            border: `1px solid ${focused ? COLORS.accent : COLORS.border}`,
            borderRadius: '6px',
            color: COLORS.text,
            fontFamily: FONT.mono,
            fontSize: '13px',
            outline: 'none',
            boxSizing: 'border-box',
            transition: 'border-color 0.15s ease',
            ...style,
          }}
        />
        {trailing}
      </div>
      {help && (
        <span id={helpId} style={{
          display: 'block',
          marginTop: '5px',
          fontFamily: FONT.sans,
          fontSize: '11px',
          color: COLORS.textMuted,
          lineHeight: 1.45,
        }}>{help}</span>
      )}
    </div>
  )
}

/* A field whose value has newlines in it, which so far means a private key.

   Not a nicer-looking Input: an <input> drops the line breaks out of anything
   pasted into it, so a PEM block pasted into one arrives as a single line and
   does not parse. The field has to be a textarea or it cannot be filled in.

   Masking is deliberately not offered. A key is a secret, but one long enough
   to need a textarea is also one you check by looking at — the BEGIN line, the
   END line, that the middle is not truncated — and a wall of dots defeats the
   only reason the box is this big. `spellCheck` and the autocapitalise hints
   are off because a phone keyboard will otherwise helpfully capitalise
   base64. */
export function TextArea({ label, help, id, rows = 6, style, ...props }) {
  const [focused, setFocused] = useState(false)
  const generated = useId()
  const fieldId = id || generated
  const helpId = `${fieldId}-help`

  return (
    <div style={{ marginBottom: '14px' }}>
      {label && (
        <label htmlFor={fieldId} style={{
          display: 'block',
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 600,
          letterSpacing: '1.5px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
          marginBottom: '6px',
        }}>{label}</label>
      )}
      <textarea
        {...props}
        id={fieldId}
        rows={rows}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        aria-describedby={help ? helpId : undefined}
        onFocus={(e) => { setFocused(true); props.onFocus?.(e) }}
        onBlur={(e) => { setFocused(false); props.onBlur?.(e) }}
        style={{
          width: '100%',
          padding: '10px 12px',
          background: COLORS.bg,
          border: `1px solid ${focused ? COLORS.accent : COLORS.border}`,
          borderRadius: '6px',
          color: COLORS.text,
          fontFamily: FONT.mono,
          fontSize: '12px',
          lineHeight: 1.5,
          outline: 'none',
          boxSizing: 'border-box',
          resize: 'vertical',
          transition: 'border-color 0.15s ease',
          ...style,
        }}
      />
      {help && (
        <span id={helpId} style={{
          display: 'block',
          marginTop: '5px',
          fontFamily: FONT.sans,
          fontSize: '11px',
          color: COLORS.textMuted,
          lineHeight: 1.45,
        }}>{help}</span>
      )}
    </div>
  )
}

export function PasswordInput({ label, help, value, onChange, ...props }) {
  const [reveal, setReveal] = useState(false)

  return (
    <Input
      {...props}
      label={label}
      help={help}
      type={reveal ? 'text' : 'password'}
      value={value}
      onChange={onChange}
      /* Wide enough that the text never runs under the reveal button, which is
         itself a full touch target on a phone. */
      style={{ paddingRight: '52px' }}
      trailing={
        <button
          type="button"
          onClick={() => setReveal(!reveal)}
          aria-label={reveal ? 'Hide password' : 'Show password'}
          data-icon-button="true"
          style={{
            position: 'absolute',
            top: 0,
            bottom: 0,
            right: '4px',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            width: '34px',
            background: 'none',
            border: 'none',
            color: COLORS.textMuted,
            cursor: 'pointer',
            fontSize: '14px',
            padding: 0,
          }}
        >{reveal ? '◉' : '◎'}</button>
      }
    />
  )
}

/* Every open dialog, innermost last. */
const modalStack = []

/* `zIndex` is for the dialog a dialog opens — the folder picker over the
   connect form — which has to sit above the one that opened it rather than
   wherever the portal happened to put it. */
export function Modal({ title, subtitle, onClose, children, width = 520, zIndex = 100 }) {
  const mobile = useIsMobile()

  // Escape closes the dialog on top and only that one: the folder picker opens
  // over the connect form, and one keypress dismissing both would throw away a
  // half-filled form. Read through a ref so a re-render — which hands us a
  // fresh onClose every time — cannot reorder the stack underneath the dialog
  // that is actually in front.
  const closeRef = useRef(onClose)
  closeRef.current = onClose

  useEffect(() => {
    const token = {}
    modalStack.push(token)

    const onKey = (e) => {
      if (e.key !== 'Escape') return
      if (modalStack[modalStack.length - 1] !== token) return
      closeRef.current?.()
    }
    window.addEventListener('keydown', onKey)

    return () => {
      window.removeEventListener('keydown', onKey)
      const at = modalStack.indexOf(token)
      if (at !== -1) modalStack.splice(at, 1)
    }
  }, [])

  /* Rendered at the end of the document rather than where it was written: a
     modal opened from inside the accounts drawer would otherwise be trapped by
     the drawer's transform, which makes that element — not the viewport — what
     `position: fixed` measures against. */
  return createPortal(
    <div
      onClick={onClose}
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(3, 6, 12, 0.78)',
        backdropFilter: 'blur(3px)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: mobile ? '12px' : '24px',
        zIndex,
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
        style={{
          width: '100%',
          maxWidth: `${width}px`,
          // Measured against the visible viewport, not the one the address bar
          // is still counted in.
          maxHeight: `calc(var(--app-height) - ${mobile ? 24 : 48}px)`,
          overflowY: 'auto',
          background: COLORS.surface,
          border: `1px solid ${COLORS.borderBright}`,
          borderRadius: '10px',
          boxShadow: '0 24px 60px rgba(0,0,0,0.55)',
        }}
      >
        <div style={{
          display: 'flex',
          alignItems: 'flex-start',
          justifyContent: 'space-between',
          gap: mobile ? '10px' : '16px',
          padding: mobile ? '14px 14px 12px' : '18px 20px',
          borderBottom: `1px solid ${COLORS.border}`,
        }}>
          <div style={{ minWidth: 0 }}>
            <h2 style={{
              margin: 0,
              fontFamily: FONT.mono,
              fontSize: '14px',
              fontWeight: 700,
              letterSpacing: '1px',
              color: COLORS.text,
              wordBreak: 'break-word',
            }}>{title}</h2>
            {subtitle && (
              <p style={{
                margin: '6px 0 0',
                fontFamily: FONT.sans,
                fontSize: '12px',
                color: COLORS.textMuted,
                lineHeight: 1.5,
                // A file name has no spaces to break at, so without this it
                // runs straight under the close button.
                overflowWrap: 'anywhere',
              }}>{subtitle}</p>
            )}
          </div>
          <button
            onClick={onClose}
            aria-label="Close"
            data-icon-button="true"
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              background: 'none',
              border: 'none',
              color: COLORS.textMuted,
              fontSize: '18px',
              cursor: 'pointer',
              lineHeight: 1,
              flexShrink: 0,
              // Named here rather than left to the coarse-pointer floor: an
              // inline style outranks it, so a smaller number set here would
              // quietly shrink the target back on the devices that need it.
              minWidth: mobile ? '44px' : '32px',
              minHeight: mobile ? '44px' : '32px',
              padding: 0,
            }}
          >✕</button>
        </div>
        <div style={{ padding: mobile ? '16px 14px' : '20px' }}>{children}</div>
      </div>
    </div>,
    document.body,
  )
}

/* A glyph-only control. A fingertip cannot aim at a 13px arrow, so the button
   carries its own square target rather than relying on the glyph's own size —
   `min-height`/`min-width` from the coarse-pointer rules widen it further on a
   touch screen.

   `label` is the spoken name and doubles as the tooltip; a control whose name
   has to name its own row ("Download hike.jpg", said once per row) can pass a
   `title` of its own to keep the hover text a sentence. */
export function IconButton({ glyph, label, title, tone = 'dim', size = 34, style, ...props }) {
  const [active, activeHandlers] = useHighlight()
  const color = { dim: COLORS.textDim, muted: COLORS.textMuted, danger: COLORS.error }[tone]

  return (
    <button
      {...props}
      data-icon-button="true"
      title={title || label}
      aria-label={label}
      {...activeHandlers}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        flexShrink: 0,
        width: `${size}px`,
        height: `${size}px`,
        padding: 0,
        background: active ? COLORS.surfaceRaised : 'transparent',
        border: `1px solid ${active ? COLORS.borderBright : 'transparent'}`,
        borderRadius: '8px',
        color,
        textDecoration: 'none',
        fontFamily: FONT.mono,
        fontSize: '15px',
        lineHeight: 1,
        cursor: 'pointer',
        transition: 'background 0.12s ease, border-color 0.12s ease',
        ...opinions(style),
      }}
    >{glyph}</button>
  )
}

/* The row-level menu. Two icons a few pixels apart — one of them a delete —
   is a mis-tap waiting to happen on a phone, so the row keeps a single button
   and the choices open down here, where each one is a full-width target with
   its name spelled out and the destructive one set apart at the bottom. */
export function ActionSheet({ title, subtitle, figures, note, items, onClose }) {
  const mobile = useIsMobile()
  const first = useRef(null)

  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') onClose?.() }
    window.addEventListener('keydown', onKey)
    first.current?.focus()
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const rows = items.filter(Boolean)
  const stats = (figures || []).filter(Boolean)

  return createPortal(
    <div
      onClick={onClose}
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(3, 6, 12, 0.7)',
        backdropFilter: 'blur(2px)',
        display: 'flex',
        alignItems: mobile ? 'flex-end' : 'center',
        justifyContent: 'center',
        padding: mobile ? 0 : '24px',
        zIndex: 110,
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
        className="sand-sheet"
        style={{
          width: '100%',
          maxWidth: mobile ? 'none' : '380px',
          maxHeight: 'calc(var(--app-height) - 40px)',
          overflowY: 'auto',
          background: COLORS.surface,
          border: `1px solid ${COLORS.borderBright}`,
          borderRadius: mobile ? '16px 16px 0 0' : '12px',
          boxShadow: '0 -12px 44px rgba(0,0,0,0.5)',
          // Clear of the home indicator, so the last row is not half a swipe.
          paddingBottom: mobile ? 'calc(10px + env(safe-area-inset-bottom, 0px))' : '10px',
        }}
      >
        {mobile && (
          <div style={{ display: 'flex', justifyContent: 'center', padding: '8px 0 2px' }}>
            <span style={{ width: '36px', height: '4px', borderRadius: '2px', background: COLORS.borderBright }} />
          </div>
        )}

        <div style={{ padding: mobile ? '8px 18px 12px' : '14px 18px 12px', borderBottom: `1px solid ${COLORS.border}` }}>
          {/* The name on the left and the figures on the right: the space beside
              a two-word title is the one place in the sheet where something can
              be said about the thing without pushing a choice further down. */}
          <div style={{ display: 'flex', alignItems: 'flex-start', gap: '14px' }}>
            <div style={{ minWidth: 0, flex: 1 }}>
              <div style={{
                fontFamily: FONT.mono, fontSize: '13px', color: COLORS.text,
                overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              }}>{title}</div>
              {subtitle && (
                <div style={{ marginTop: '4px', fontFamily: FONT.sans, fontSize: '11.5px', color: COLORS.textMuted }}>
                  {subtitle}
                </div>
              )}
            </div>

            {stats.length > 0 && (
              <div style={{ display: 'flex', gap: '14px', flexShrink: 0, textAlign: 'right' }}>
                {stats.map((f) => (
                  <div key={f.key || f.label} title={f.title}>
                    <div style={{
                      fontFamily: FONT.mono, fontSize: '15px', fontWeight: 600,
                      color: COLORS.text, lineHeight: 1.2, letterSpacing: '-0.5px',
                      whiteSpace: 'nowrap',
                    }}>{f.value}</div>
                    <div style={{
                      marginTop: '3px',
                      fontFamily: FONT.mono, fontSize: '9px', fontWeight: 600,
                      letterSpacing: '1.2px', textTransform: 'uppercase',
                      color: COLORS.textMuted, whiteSpace: 'nowrap',
                    }}>{f.label}</div>
                  </div>
                ))}
              </div>
            )}
          </div>

          {note && (
            <div style={{
              marginTop: '8px',
              fontFamily: FONT.mono, fontSize: '10px', lineHeight: 1.6,
              color: COLORS.textMuted,
            }}>{note}</div>
          )}
        </div>

        <div style={{ padding: '6px 0' }}>
          {rows.map((item, i) => (
            <SheetRow
              key={item.key || i}
              ref={i === 0 ? first : null}
              {...item}
              onSelect={() => { item.onSelect?.(); if (!item.keepOpen) onClose?.() }}
            />
          ))}
        </div>

        <div style={{ padding: '6px 12px 4px', borderTop: `1px solid ${COLORS.border}` }}>
          <button
            onClick={onClose}
            style={{
              width: '100%',
              minHeight: '48px',
              background: 'transparent',
              border: 'none',
              borderRadius: '10px',
              color: COLORS.textDim,
              fontFamily: FONT.mono,
              fontSize: '12.5px',
              fontWeight: 600,
              letterSpacing: '0.5px',
              cursor: 'pointer',
            }}
          >Cancel</button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

const SheetRow = React.forwardRef(function SheetRow(
  { glyph, label, hint, tint, danger, disabled, onSelect }, ref,
) {
  const [active, setActive] = useState(false)

  return (
    <button
      ref={ref}
      onClick={onSelect}
      disabled={disabled}
      onPointerDown={() => setActive(true)}
      onPointerUp={() => setActive(false)}
      onPointerLeave={() => setActive(false)}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: '14px',
        width: '100%',
        minHeight: '54px',
        padding: '8px 18px',
        background: active ? COLORS.surfaceHover : 'transparent',
        border: 'none',
        color: danger ? COLORS.error : COLORS.text,
        textAlign: 'left',
        textDecoration: 'none',
        fontFamily: FONT.mono,
        fontSize: '13px',
        cursor: disabled ? 'not-allowed' : 'pointer',
        opacity: disabled ? 0.4 : 1,
      }}
    >
      {/* tint colours the glyph alone, so a row can carry a state — a folder
          whose last sweep found something — without a second badge or a
          differently coloured label shouting over the four rows beside it. */}
      <span style={{
        width: '20px', textAlign: 'center', fontSize: '15px', flexShrink: 0,
        color: tint || undefined,
      }}>{glyph}</span>
      <span style={{ minWidth: 0 }}>
        <span style={{ display: 'block' }}>{label}</span>
        {hint && (
          <span style={{
            display: 'block', marginTop: '3px',
            fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted, lineHeight: 1.4,
          }}>{hint}</span>
        )}
      </span>
    </button>
  )
})

/* Deleting scatters erasures across several accounts and cannot be undone, so
   it gets a real dialog rather than the browser's confirm() — which on a phone
   puts its two buttons side by side at the top of the screen, out of reach and
   a few pixels apart. */
/* zIndex is forwarded for the same reason every other nested dialog takes one:
   a confirmation opened from inside a panel has to sit above the panel that
   asked for it. Left at the Modal default it lands underneath, and the button
   that opened it reads as broken — the dialog is there, behind the backdrop. */
export function ConfirmDialog({
  title, subtitle, children, confirmLabel = 'Delete', busy, onConfirm, onClose, zIndex,
}) {
  const mobile = useIsMobile()

  return (
    <Modal title={title} subtitle={subtitle} onClose={onClose} width={420} zIndex={zIndex}>
      {children && (
        <div style={{
          marginBottom: '18px',
          fontFamily: FONT.sans,
          fontSize: '12.5px',
          lineHeight: 1.6,
          color: COLORS.textDim,
        }}>{children}</div>
      )}
      {/* Stacked and full width on a phone, with the destructive choice on top
          where the thumb already is — and Cancel the wider, safer default. */}
      <div style={{
        display: 'flex',
        flexDirection: mobile ? 'column' : 'row',
        gap: '10px',
        justifyContent: 'flex-end',
      }}>
        <Button
          variant="danger"
          disabled={busy}
          onClick={onConfirm}
          style={mobile ? { justifyContent: 'center', minHeight: '48px' } : null}
        >{busy ? 'Deleting…' : confirmLabel}</Button>
        <Button
          variant={mobile ? 'default' : 'ghost'}
          onClick={onClose}
          disabled={busy}
          style={mobile ? { justifyContent: 'center', minHeight: '48px' } : null}
        >Cancel</Button>
      </div>
    </Modal>
  )
}

export function Banner({ tone = 'info', children, onDismiss }) {
  const color = { info: COLORS.info, warn: COLORS.warn, error: COLORS.error, success: COLORS.success }[tone]

  return (
    <div style={{
      display: 'flex',
      alignItems: 'flex-start',
      gap: '10px',
      padding: '10px 12px',
      marginBottom: '14px',
      background: `${color}14`,
      border: `1px solid ${color}55`,
      borderRadius: '6px',
      fontFamily: FONT.sans,
      fontSize: '12px',
      lineHeight: 1.5,
      color: COLORS.text,
    }}>
      <span style={{ color, fontFamily: FONT.mono, fontWeight: 700 }}>
        {{ info: 'ℹ', warn: '⚠', error: '✗', success: '✓' }[tone]}
      </span>
      <div style={{ flex: 1, minWidth: 0, wordBreak: 'break-word' }}>{children}</div>
      {onDismiss && (
        <button onClick={onDismiss} aria-label="Dismiss" data-icon-button="true" style={{
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          minWidth: '30px', minHeight: '30px', padding: 0, flexShrink: 0,
          background: 'none', border: 'none', color: COLORS.textMuted, cursor: 'pointer',
        }}>✕</button>
      )}
    </div>
  )
}

export function Spinner({ size = 16, color = COLORS.accent }) {
  return (
    <span
      style={{
        display: 'inline-block',
        width: `${size}px`,
        height: `${size}px`,
        border: `2px solid ${color}33`,
        borderTopColor: color,
        borderRadius: '50%',
        animation: 'sand-spin 0.7s linear infinite',
      }}
    />
  )
}

export function Empty({ icon, title, children }) {
  const mobile = useIsMobile()

  return (
    <div style={{
      padding: mobile ? '40px 16px' : '56px 24px',
      textAlign: 'center',
      color: COLORS.textMuted,
      fontFamily: FONT.sans,
      fontSize: '13px',
      lineHeight: 1.6,
    }}>
      <div style={{ fontSize: '30px', opacity: 0.5, marginBottom: '12px' }}>{icon}</div>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '13px',
        color: COLORS.textDim,
        marginBottom: '6px',
      }}>{title}</div>
      <div style={{ maxWidth: '420px', margin: '0 auto' }}>{children}</div>
    </div>
  )
}

/* Put text on the clipboard from wherever the app happens to be running.

   navigator.clipboard only exists on a secure origin, and SAND is normally
   reached over plain HTTP at a LAN or tailnet address — so the async API is
   the shortcut here, not the mechanism. */
async function writeToClipboard(text) {
  try {
    // Throws outright when the API is missing, rejects when it is blocked.
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return execCommandCopy(text)
  }
}

/* The pre-clipboard-API copy: deprecated, but the only one that works on
   http://. The text goes into a throwaway node rather than the field itself so
   the page keeps its own selection, focus and scroll position. */
function execCommandCopy(text) {
  const scratch = document.createElement('textarea')
  scratch.value = text
  // Read-only keeps iOS from opening a keyboard over the dialog, and is what
  // makes a selection stick there at all.
  scratch.setAttribute('readonly', '')
  scratch.setAttribute('aria-hidden', 'true')
  // Rendered but out of the way — display:none or visibility:hidden would
  // leave nothing to select.
  scratch.style.cssText =
    'position:fixed;top:0;left:0;width:1px;height:1px;padding:0;border:0;opacity:0;'
  document.body.appendChild(scratch)

  const restore = document.activeElement
  try {
    scratch.focus()
    scratch.select()
    scratch.setSelectionRange(0, text.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    scratch.remove()
    if (restore instanceof HTMLElement) restore.focus()
  }
}

/* A read-only value with a copy button, for the things that have to be
   transcribed exactly somewhere else — an OAuth redirect URI into someone
   else's console, a mount address into a file manager. */
export function CopyField({ label, value, help }) {
  // 'manual' is the last resort: nothing could reach the clipboard, so the
  // value is selected and the user is told to copy it themselves. A button
  // that silently does nothing is the one outcome worth avoiding.
  const [state, setState] = useState('idle')
  const fieldId = useId()
  const flashTimer = useRef(null)

  useEffect(() => () => window.clearTimeout(flashTimer.current), [])

  const flash = (next) => {
    setState(next)
    window.clearTimeout(flashTimer.current)
    flashTimer.current = window.setTimeout(() => setState('idle'), 2400)
  }

  const copy = async () => {
    if (await writeToClipboard(value)) {
      flash('copied')
      return
    }
    const field = document.getElementById(fieldId)
    field?.focus()
    try { field?.setSelectionRange(0, value.length) } catch { /* not selectable */ }
    flash('manual')
  }

  return (
    <Input
      id={fieldId}
      label={label}
      value={value}
      readOnly
      help={state === 'manual'
        ? 'This browser will not let the page copy for you — the address is selected, copy it by hand.'
        : help}
      onFocus={(e) => e.target.select()}
      style={{ paddingRight: '62px', color: COLORS.textDim }}
      trailing={
        <button
          type="button"
          onClick={copy}
          style={{
            position: 'absolute', top: 0, bottom: 0, right: '4px',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            width: '54px', background: 'none', border: 'none',
            color: state === 'copied' ? COLORS.success : COLORS.textMuted,
            cursor: 'pointer', fontFamily: FONT.mono, fontSize: '10px',
            letterSpacing: '1px', padding: 0,
          }}
        >{state === 'copied' ? 'COPIED' : 'COPY'}</button>
      }
    />
  )
}
