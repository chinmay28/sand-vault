import React, { useEffect, useId, useState } from 'react'
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
      style={{ paddingRight: '42px' }}
      trailing={
        <button
          type="button"
          onClick={() => setReveal(!reveal)}
          aria-label={reveal ? 'Hide password' : 'Show password'}
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
            style={{
              background: 'none',
              border: 'none',
              color: COLORS.textMuted,
              fontSize: '18px',
              cursor: 'pointer',
              lineHeight: 1,
              flexShrink: 0,
              padding: '0 4px',
            }}
          >✕</button>
        </div>
        <div style={{ padding: mobile ? '16px 14px' : '20px' }}>{children}</div>
      </div>
    </div>,
    document.body,
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
        <button onClick={onDismiss} style={{
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
