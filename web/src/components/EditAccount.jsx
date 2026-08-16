import React, { useMemo, useState } from 'react'
import {
  ACCOUNT_COLORS, ACCOUNT_COLOR_NAMES, COLORS, FONT, KIND_ICONS,
  accountColor, autoAccountColor, normalizeHex,
} from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, Spinner } from './ui'

/* Editing an account.

   Everything else you can do to a connected cloud reaches the cloud: testing it
   pings it, disconnecting it forgets the parts it holds. These two do not. What
   an account is called and what colour it wears are how *you* tell it apart
   from the others — nothing is uploaded, downloaded or re-encrypted by changing
   either, and the credentials are not touched.

   The colour is worth taking seriously even though it is only a colour. It is
   the same shade on the account's card here and on every part badge in the file
   list, which is what makes "which three clouds is this file on" a question you
   answer by eye rather than by opening an inspector. Left alone, the browser
   picks one and keeps it stable as accounts come and go; the point of choosing
   is that your Google Drive can be the blue one because that is what it is to
   you. */
export default function EditAccount({ provider, providers = [], onClose, onChanged }) {
  const [name, setName] = useState(provider.name || '')
  // '' is a real value here rather than "unset": it is the account with no
  // colour of its own, which is what the Automatic swatch selects.
  const [color, setColor] = useState(() => normalizeHex(provider.color))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  // Which colours the other accounts are wearing, so a swatch can say whose it
  // already is instead of letting two clouds quietly end up the same blue.
  const wornByOthers = useMemo(() => {
    const out = new Map()
    for (const other of providers) {
      if (!other || other.id === provider.id) continue
      out.set(accountColor(other.id), other.name)
    }
    return out
  }, [providers, provider.id])

  const trimmed = name.trim()
  const preview = color || autoAccountColor(provider.id)
  const unchanged = trimmed === (provider.name || '') && color === normalizeHex(provider.color)

  const submit = async (e) => {
    e.preventDefault()
    if (!trimmed || busy) return

    setBusy(true)
    setError(null)
    try {
      await api.updateProvider(provider.id, { name: trimmed, color })
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Edit account"
      subtitle="What this cloud is called, and the colour it wears here. Neither touches its credentials or the parts stored on it."
      onClose={busy ? undefined : onClose}
      width={480}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        <Input
          label="Name"
          value={name}
          autoFocus
          spellCheck={false}
          disabled={busy}
          maxLength={64}
          placeholder="drive-personal"
          help="Yours alone — the provider never sees it, and no two accounts may share one."
          onChange={(e) => setName(e.target.value)}
        />

        <span style={{
          display: 'block',
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 600,
          letterSpacing: '1.5px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
          marginBottom: '8px',
        }} id="account-colour-label">Colour</span>

        <div
          role="radiogroup"
          aria-labelledby="account-colour-label"
          style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: '8px',
            marginBottom: '10px',
          }}
        >
          <Swatch
            color={autoAccountColor(provider.id)}
            label="Automatic"
            title="Let the browser pick, and keep it stable as accounts come and go"
            selected={color === ''}
            auto
            disabled={busy}
            onSelect={() => setColor('')}
          />
          {ACCOUNT_COLORS.map((value) => {
            const owner = wornByOthers.get(value)
            return (
              <Swatch
                key={value}
                color={value}
                label={ACCOUNT_COLOR_NAMES[value] || value}
                title={owner ? `${ACCOUNT_COLOR_NAMES[value] || value} — currently ${owner}'s` : undefined}
                taken={Boolean(owner)}
                selected={color === value}
                disabled={busy}
                onSelect={() => setColor(value)}
              />
            )
          })}
        </div>

        {/* The palette covers twelve accounts; past that, or when a cloud has a
            brand colour of its own, the native picker takes any colour at all.
            It is a colour input rather than a hex field because a phone gives
            you a real picker for one and a keyboard for the other. */}
        <label style={{
          display: 'flex',
          alignItems: 'center',
          gap: '10px',
          padding: '9px 11px',
          marginBottom: '14px',
          background: COLORS.bg,
          border: `1px solid ${COLORS.border}`,
          borderRadius: '6px',
          cursor: busy ? 'not-allowed' : 'pointer',
        }}>
          <input
            type="color"
            value={preview}
            disabled={busy}
            aria-label="Any other colour"
            onChange={(e) => setColor(normalizeHex(e.target.value))}
            style={{
              width: '30px',
              height: '30px',
              flexShrink: 0,
              padding: 0,
              background: 'none',
              border: `1px solid ${COLORS.border}`,
              borderRadius: '5px',
              cursor: busy ? 'not-allowed' : 'pointer',
            }}
          />
          <span style={{ fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textDim }}>
            Any other colour
            <span style={{
              display: 'block',
              marginTop: '2px',
              fontFamily: FONT.mono,
              fontSize: '10px',
              color: COLORS.textMuted,
            }}>{color || `${preview} · picked for you`}</span>
          </span>
        </label>

        {/* The same two shapes the colour actually appears in: the stripe down
            the card in the sidebar, and a part badge in the file list. Shown
            here so the choice is made against what it will look like rather
            than against a square in a dialog. */}
        <div style={{
          display: 'flex',
          alignItems: 'center',
          gap: '10px',
          padding: '10px 12px',
          marginBottom: '16px',
          background: COLORS.bg,
          border: `1px solid ${COLORS.border}`,
          borderLeft: `3px solid ${preview}`,
          borderRadius: '6px',
        }}>
          <span aria-hidden="true" style={{ fontSize: '13px' }}>{KIND_ICONS[provider.kind] || '☁'}</span>
          <span style={{
            flex: 1,
            minWidth: 0,
            fontFamily: FONT.mono,
            fontSize: '12px',
            color: trimmed ? COLORS.text : COLORS.textMuted,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}>{trimmed || 'an account needs a name'}</span>
          <span aria-hidden="true" style={{ display: 'flex', gap: '3px', flexShrink: 0 }}>
            {[1, 2, 3].map((part) => (
              <span key={part} style={{
                width: '16px',
                height: '16px',
                borderRadius: '3px',
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                fontFamily: FONT.mono,
                fontSize: '9px',
                fontWeight: 700,
                color: part === 2 ? COLORS.bg : COLORS.textMuted,
                background: part === 2 ? preview : 'transparent',
                border: part === 2 ? 'none' : `1px dashed ${COLORS.border}`,
              }}>{part}</span>
            ))}
          </span>
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy || !trimmed || unchanged}>
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy ? 'Saving…' : 'Save'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/* One colour to choose. A fingertip target with the colour filling it, its name
   underneath — and a dot when another account is already wearing it, which is
   worth knowing before you make two clouds the same shade rather than after. */
function Swatch({ color, label, title, selected, taken, auto, disabled, onSelect }) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      aria-label={taken ? `${label}, already used` : label}
      title={title || label}
      disabled={disabled}
      onClick={onSelect}
      style={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        gap: '4px',
        // Wide enough for "Automatic" to be spelled out under its swatch, and
        // past the 44px a fingertip needs either way.
        width: '58px',
        padding: '6px 2px',
        background: selected ? COLORS.surfaceRaised : 'transparent',
        border: `1px solid ${selected ? COLORS.accent : 'transparent'}`,
        borderRadius: '7px',
        cursor: disabled ? 'not-allowed' : 'pointer',
        opacity: disabled ? 0.5 : 1,
      }}
    >
      <span style={{
        position: 'relative',
        width: '28px',
        height: '28px',
        borderRadius: '6px',
        background: color,
        // The automatic swatch shows the colour it would start from, so it has
        // to say it is a choice about picking rather than a thirteenth colour.
        border: auto ? `2px dashed ${COLORS.bg}` : 'none',
        boxShadow: selected ? `0 0 0 2px ${COLORS.accent}` : 'none',
      }}>
        {taken && (
          <span aria-hidden="true" style={{
            position: 'absolute',
            right: '-2px',
            bottom: '-2px',
            width: '8px',
            height: '8px',
            borderRadius: '50%',
            background: COLORS.textMuted,
            border: `1.5px solid ${COLORS.surface}`,
          }} />
        )}
      </span>
      <span style={{
        fontFamily: FONT.mono,
        fontSize: '8.5px',
        letterSpacing: '0.3px',
        color: selected ? COLORS.text : COLORS.textMuted,
        maxWidth: '100%',
        overflow: 'hidden',
        textOverflow: 'ellipsis',
        whiteSpace: 'nowrap',
      }}>{label}</span>
    </button>
  )
}
