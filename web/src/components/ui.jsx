import React, { useEffect, useId, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { COLORS, FONT } from '../theme'
import { useIsMobile } from '../hooks'

export function Button({ variant = 'default', size = 'md', style, disabled, ...props }) {
  const [hover, setHover] = useState(false)

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
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: '6px',
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
        ...style,
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

export function Modal({ title, subtitle, onClose, children, width = 520 }) {
  const mobile = useIsMobile()

  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') onClose?.() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

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
        zIndex: 100,
      }}
    >
      <div
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
              minWidth: '32px',
              minHeight: '32px',
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
  const [active, setActive] = useState(false)
  const color = { dim: COLORS.textDim, muted: COLORS.textMuted, danger: COLORS.error }[tone]

  return (
    <button
      {...props}
      data-icon-button="true"
      title={title || label}
      aria-label={label}
      onPointerEnter={() => setActive(true)}
      onPointerLeave={() => setActive(false)}
      onBlur={() => setActive(false)}
      onFocus={() => setActive(true)}
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
        ...style,
      }}
    >{glyph}</button>
  )
}

/* The row-level menu. Two icons a few pixels apart — one of them a delete —
   is a mis-tap waiting to happen on a phone, so the row keeps a single button
   and the choices open down here, where each one is a full-width target with
   its name spelled out and the destructive one set apart at the bottom. */
export function ActionSheet({ title, subtitle, items, onClose }) {
  const mobile = useIsMobile()
  const first = useRef(null)

  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') onClose?.() }
    window.addEventListener('keydown', onKey)
    first.current?.focus()
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const rows = items.filter(Boolean)

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
  { glyph, label, hint, danger, disabled, onSelect }, ref,
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
      <span style={{ width: '20px', textAlign: 'center', fontSize: '15px', flexShrink: 0 }}>{glyph}</span>
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
export function ConfirmDialog({ title, subtitle, children, confirmLabel = 'Delete', busy, onConfirm, onClose }) {
  const mobile = useIsMobile()

  return (
    <Modal title={title} subtitle={subtitle} onClose={onClose} width={420}>
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
