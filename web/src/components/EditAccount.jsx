import React, { useMemo, useState } from 'react'
import {
  ACCOUNT_COLORS, ACCOUNT_COLOR_NAMES, ACCOUNT_PALETTE, COLORS, FONT, KIND_ICONS,
  accountColor, accountColorName, autoAccountColor, formatBytes, normalizeHex,
} from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Input, Modal, Spinner } from './ui'

/* `size` items at a time, in order. */
function chunk(items, size) {
  const out = []
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size))
  return out
}

/* Editing an account.

   Everything else you can do to a connected cloud reaches the cloud: testing it
   pings it, disconnecting it forgets the parts it holds. Nothing here does. What
   an account is called and what colour it wears are how *you* tell it apart
   from the others, and a declared capacity is what *you* know about an account
   that cannot answer the question itself — nothing is uploaded, downloaded or
   re-encrypted by changing any of them, and the credentials are not touched.

   The colour is worth taking seriously even though it is only a colour. It is
   the same shade on the account's card here and on every part badge in the file
   list, which is what makes "which three clouds is this file on" a question you
   answer by eye rather than by opening an inspector. Left alone, the browser
   picks one and keeps it stable as accounts come and go; the point of choosing
   is that your Google Drive can be the blue one because that is what it is to
   you. */
export default function EditAccount({ provider, providers = [], onClose, onChanged }) {
  const mobile = useIsMobile()
  const [name, setName] = useState(provider.name || '')
  // '' is a real value here rather than "unset": it is the account with no
  // colour of its own, which is what the Automatic swatch selects.
  const [color, setColor] = useState(() => normalizeHex(provider.color))
  // Typed rather than picked, and shown the way the rest of the app prints a
  // size, so what goes back in is what was read out. Empty is nobody declaring
  // one, which is where every account starts.
  const [capacity, setCapacity] = useState(
    () => (provider.capacity > 0 ? formatBytes(provider.capacity) : ''),
  )
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  // The full palette opens on its own when the account is already wearing a
  // shade the named row does not show — otherwise the dialog would open with
  // nothing selected and no sign of where the colour came from.
  const [showShades, setShowShades] = useState(
    () => Boolean(normalizeHex(provider.color)) &&
      !ACCOUNT_COLORS.includes(normalizeHex(provider.color)),
  )

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

  const hueColumns = mobile ? 6 : ACCOUNT_PALETTE.length
  const trimmed = name.trim()
  const preview = color || autoAccountColor(provider.id)
  // A capacity nobody has retyped is a capacity nobody is changing: the field
  // shows a rounded figure, and sending back "33.9 GB" for a capacity stored as
  // 36,401,835,212 bytes would quietly move it every time the dialog is opened.
  const declaredCapacity = provider.capacity > 0 ? formatBytes(provider.capacity) : ''
  const capacityChanged = capacity.trim() !== declaredCapacity
  const unchanged = trimmed === (provider.name || '')
    && color === normalizeHex(provider.color)
    && !capacityChanged

  const submit = async (e) => {
    e.preventDefault()
    if (!trimmed || busy) return

    setBusy(true)
    setError(null)
    try {
      await api.updateProvider(provider.id, {
        name: trimmed,
        color,
        ...(capacityChanged ? { capacity: capacity.trim() } : null),
      })
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
      subtitle="What this cloud is called, the colour it wears here, and — where the backend cannot say — how big it is. None of them touches its credentials or the parts stored on it."
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

        {/* How big the account is, for the backends that cannot say.

            A bucket has no quota call — S3 never had one and B2's own API does
            not add one — so an S3 account's card has always had a figure for
            what SAND put there and nothing to measure it against. This is that
            missing half, and it has to be typed because there is nowhere to
            read it from: the cap set in the provider's console, or simply how
            much of an unlimited bucket this vault is allowed to fill.

            Only offered where it does something. An account that reports its
            own quota is already answering the question, and one that can be
            neither asked nor counted would take the figure and still have no
            used to draw against it. */}
        {(provider.measurable || provider.capacity > 0) && (
          <Input
            label="Capacity"
            value={capacity}
            spellCheck={false}
            disabled={busy}
            maxLength={24}
            placeholder="10 GB"
            help={'What this account holds, as you know it — a bucket does not report a quota. ' +
              'Blank means nobody is saying, and the account goes back to showing no capacity. ' +
              'Nothing is enforced: this is what the usage bar is drawn against.'}
            onChange={(e) => setCapacity(e.target.value)}
          />
        )}

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

        {/* Twelve named colours is the shortlist, not the palette. Behind this
            is the whole thing — the same hues in three shades, laid out a hue
            per column so picking "the same blue but deeper" is a move
            downwards rather than a hunt. Kept shut by default: a wall of
            thirty-six squares is a worse first thing to meet than a row of
            twelve with names under them. */}
        <button
          type="button"
          onClick={() => setShowShades((open) => !open)}
          aria-expanded={showShades}
          disabled={busy}
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: '6px',
            padding: '4px 2px',
            marginBottom: showShades ? '8px' : '12px',
            background: 'none',
            border: 'none',
            color: COLORS.textDim,
            fontFamily: FONT.mono,
            fontSize: '10.5px',
            fontWeight: 600,
            letterSpacing: '0.5px',
            cursor: busy ? 'not-allowed' : 'pointer',
          }}
        >
          <span aria-hidden="true" style={{
            display: 'inline-block',
            fontSize: '9px',
            transform: showShades ? 'rotate(90deg)' : 'none',
            transition: 'transform 140ms ease',
          }}>▶</span>
          {showShades ? 'Fewer shades' : 'All shades'}
        </button>

        {showShades && (
          <div
            role="radiogroup"
            aria-label="Every shade"
            style={{
              display: 'flex',
              flexDirection: 'column',
              gap: '5px',
              padding: '10px',
              marginBottom: '14px',
              background: COLORS.bg,
              border: `1px solid ${COLORS.border}`,
              borderRadius: '6px',
            }}
          >
            {/* Twelve hues across is more than a phone has room for, so there
                it becomes two blocks of six rather than a grid that scrolls
                sideways — the columns are the point, and a column you have to
                drag into view is not one you can compare against its
                neighbour. */}
            {chunk(ACCOUNT_PALETTE, hueColumns).map((block, i) => (
              <div key={i} style={{
                display: 'grid',
                // A column per hue, its three shades stacked under one another.
                gridAutoFlow: 'column',
                gridTemplateRows: 'repeat(3, auto)',
                gridTemplateColumns: `repeat(${hueColumns}, minmax(0, 1fr))`,
                gap: '5px',
              }}>
                {block.flatMap(({ shades }) => shades.map((value) => {
                  const owner = wornByOthers.get(value)
                  const label = accountColorName(value)
                  return (
                    <ShadeSwatch
                      key={value}
                      color={value}
                      label={label}
                      title={owner ? `${label} — currently ${owner}'s` : label}
                      taken={Boolean(owner)}
                      selected={color === value}
                      disabled={busy}
                      onSelect={() => setColor(value)}
                    />
                  )
                }))}
              </div>
            ))}
          </div>
        )}

        {/* Thirty-six colours is still a palette; a cloud with a brand colour
            of its own is not in it, so the native picker takes any colour at
            all.
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

/* One tile in the full palette. No label — thirty-six of those would be a wall
   of text, and the grid's own shape says what each one is: a hue down a column,
   light at the top. The name lives in the tooltip and in what a screen reader
   announces, so it is never actually missing. */
function ShadeSwatch({ color, label, title, selected, taken, disabled, onSelect }) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      aria-label={taken ? `${label}, already used` : label}
      title={title}
      disabled={disabled}
      onClick={onSelect}
      style={{
        position: 'relative',
        // Square, and as wide as its column allows up to a fingertip's worth:
        // the grid hands out the width, the cap stops six columns on a phone
        // turning into six dinner plates.
        width: '100%',
        maxWidth: '44px',
        aspectRatio: '1 / 1',
        justifySelf: 'center',
        padding: 0,
        background: color,
        border: 'none',
        borderRadius: '5px',
        cursor: disabled ? 'not-allowed' : 'pointer',
        opacity: disabled ? 0.5 : 1,
        boxShadow: selected
          ? `0 0 0 2px ${COLORS.surface}, 0 0 0 4px ${COLORS.accent}`
          : 'none',
      }}
    >
      {taken && (
        <span aria-hidden="true" style={{
          position: 'absolute',
          right: '3px',
          bottom: '3px',
          width: '6px',
          height: '6px',
          borderRadius: '50%',
          background: COLORS.bg,
          opacity: 0.55,
        }} />
      )}
    </button>
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
