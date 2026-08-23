import React, { useEffect, useMemo, useState } from 'react'
import {
  ACCOUNT_COLORS, ACCOUNT_COLOR_NAMES, ACCOUNT_PALETTE, COLORS, FONT, KIND_ICONS,
  accountColor, accountColorName, autoAccountColor, formatBytes, normalizeHex,
} from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import {
  callbackURL, forgetFlow, openAuthWindow, pendingOAuthFlow, rememberFlow, useSignInResult,
} from '../oauth'
import { Banner, Button, CopyField, Input, Modal, PasswordInput, Spinner } from './ui'
import SpecFields, { STORED_SECRET } from './SpecFields'

/* `size` items at a time, in order. */
function chunk(items, size) {
  const out = []
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size))
  return out
}

/* Editing an account, in two halves that are genuinely different things.

   One half is yours alone: what the account is called, what colour it wears,
   and — where the backend cannot say — how big it is. Nothing there reaches the
   cloud. Nothing is uploaded, downloaded or re-encrypted by renaming an
   account, and the credentials are not touched.

   The other half is how the account reaches the backend at all: its keys, its
   secrets, the bucket or folder it writes into, and for the clouds you sign in
   to, the consent behind the tokens. That half is the answer to an account card
   that has gone red — a rotated access key, a revoked consent, an OAuth client
   somebody deleted in a console. Before this dialog could change it, the only
   way to repair such an account was to disconnect it and connect it again,
   which means a new account with a new ID and every part it holds forgotten.
   Now it keeps its ID, its name, its colour and its parts, and only what it
   connects with changes.

   They are separate tabs rather than one long form because the promise the
   first half makes — nothing here touches the account — is one the second half
   cannot make, and a form that quietly means both is a form that means
   neither. */
export default function EditAccount({ provider, providers = [], initialTab, onClose, onChanged }) {
  const [tab, setTab] = useState(initialTab || 'look')
  // A save in flight is not a dialog to dismiss — least of all a reconnection,
  // which is a round trip to somebody else's cloud and comes back with an
  // answer worth reading. Held here rather than in the half doing the work, so
  // it can shut the modal's own close button as well as the form's.
  const [busy, setBusy] = useState(false)

  return (
    <Modal
      title="Edit account"
      subtitle={tab === 'look'
        ? 'What this cloud is called, the colour it wears here, how big it is where the backend cannot say, and how much of it SAND may fill. None of them touches its credentials or the parts stored on it.'
        : 'How this account reaches the backend. Changing any of it is checked against the cloud before it is stored, so settings SAND cannot connect with are refused rather than saved.'}
      onClose={busy ? undefined : onClose}
      width={480}
    >
      <Tabs value={tab} onChange={busy ? () => {} : setTab} />

      {tab === 'look' ? (
        <Appearance
          provider={provider}
          providers={providers}
          busy={busy}
          setBusy={setBusy}
          onClose={onClose}
          onChanged={onChanged}
        />
      ) : (
        <Connection
          provider={provider}
          busy={busy}
          setBusy={setBusy}
          onClose={onClose}
          onChanged={onChanged}
        />
      )}
    </Modal>
  )
}

/* The two halves, as a row of tabs. Same segmented control the read-stats panel
   uses for its spans: which one you are on changes what every control below
   means, so it is a thing you can see rather than a thing you have to open. */
function Tabs({ value, onChange }) {
  return (
    <div role="tablist" aria-label="What to edit" style={{
      display: 'flex',
      gap: '2px',
      padding: '2px',
      marginBottom: '16px',
      background: COLORS.surfaceRaised,
      borderRadius: '8px',
    }}>
      {[
        { key: 'look', label: 'How it looks' },
        { key: 'connection', label: 'How it connects' },
      ].map((option) => {
        const selected = option.key === value
        return (
          <button
            key={option.key}
            type="button"
            role="tab"
            aria-selected={selected}
            onClick={() => onChange(option.key)}
            style={{
              flex: 1,
              minHeight: '32px',
              padding: '6px 4px',
              border: 'none',
              borderRadius: '6px',
              cursor: 'pointer',
              fontFamily: FONT.mono,
              fontSize: '11px',
              letterSpacing: '0.4px',
              background: selected ? COLORS.surface : 'transparent',
              color: selected ? COLORS.text : COLORS.textMuted,
              boxShadow: selected ? `inset 0 0 0 1px ${COLORS.borderBright}` : 'none',
              transition: 'background 0.15s ease, color 0.15s ease',
            }}
          >{option.label}</button>
        )
      })}
    </div>
  )
}

/* What an account is called, the colour it wears, and how big its holder says
   it is.

   The colour is worth taking seriously even though it is only a colour. It is
   the same shade on the account's card in the sidebar and on every part badge
   in the file list, which is what makes "which three clouds is this file on" a
   question you answer by eye rather than by opening an inspector. Left alone,
   the browser picks one and keeps it stable as accounts come and go; the point
   of choosing is that your Google Drive can be the blue one because that is
   what it is to you. */
function Appearance({ provider, providers, busy, setBusy, onClose, onChanged }) {
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
  // The other figure a person types about an account, and a different question:
  // not how big it is, but how much of it is SAND's to fill. Offered on every
  // account rather than only the ones that cannot answer for themselves — a
  // Drive that reports 2 TB free still might not be somewhere you want two
  // terabytes of parts.
  const [quota, setQuota] = useState(
    () => (provider.quota > 0 ? formatBytes(provider.quota) : ''),
  )
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
  const setQuotaText = provider.quota > 0 ? formatBytes(provider.quota) : ''
  const quotaChanged = quota.trim() !== setQuotaText
  const unchanged = trimmed === (provider.name || '')
    && color === normalizeHex(provider.color)
    && !capacityChanged
    && !quotaChanged

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
        ...(quotaChanged ? { quota: quota.trim() } : null),
      })
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
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

      {/* How much of it is ours to fill.

          The account card and the upload picker both want to say how much more
          fits here, and on a good few backends there is nothing to say it
          with: a synced folder knows the disk underneath it, a Drive knows its
          quota, a bucket can be counted — but between them that still leaves
          accounts whose only known figure is what SAND itself wrote. A quota
          is what turns that figure into a fraction, and it is the reason the
          picker can rank clouds by room at all.

          Offered on every account, not only those. A cloud that reports two
          terabytes free is still a cloud you might only want to put two
          hundred gigabytes of parts on, and where both figures exist the room
          left is whichever leaves less. */}
      <Input
        label="Quota"
        value={quota}
        spellCheck={false}
        disabled={busy}
        maxLength={24}
        placeholder="200 GB"
        help={'How much of this account SAND may fill, as against how big it is. '
          + 'It is what says how much more fits where the backend cannot. '
          + 'Nothing is refused for crossing it — an upload past it stores and warns, '
          + 'and the account reads as over until you raise the line or move parts off. '
          + 'Blank means nobody is watching this account\'s share.'}
        onChange={(e) => setQuota(e.target.value)}
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
  )
}

/* How the account reaches the backend.

   The form is generated from the backend's own field specs, exactly as the
   connect dialog's is, and filled in from what the account is currently
   connected with — minus the secrets, which the server never sends back. A
   secret box left alone therefore means "keep the one you have"; only a box
   somebody typed into becomes a new credential.

   For a cloud you sign in to there is a shorter way, and it is the one at the
   top: put the account back through the provider's own consent screen and let
   SAND take the tokens again. That is what answers a revoked consent or an
   expired refresh token, neither of which anybody can retype. */
function Connection({ provider, busy, setBusy, onClose, onChanged }) {
  const [spec, setSpec] = useState(null)
  const [values, setValues] = useState({})
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [error, setError] = useState(null)

  useEffect(() => {
    let dropped = false
    api.providerSpecs()
      .then((resp) => {
        if (dropped) return
        const match = (resp.specs || []).find((s) => s.kind === provider.kind)
        setSpec(match || null)
        // Secrets start blank, because a blank is the truth: the browser was
        // never given the stored one. Everything else opens on what the account
        // is actually connected with, so an edit is a change to something
        // visible rather than a form filled in from memory.
        const initial = {}
        for (const field of match?.fields || []) {
          initial[field.key] = field.secret ? '' : (provider.options?.[field.key] || '')
        }
        setValues(initial)
      })
      .catch((err) => !dropped && setError(err.message))
    return () => { dropped = true }
  }, [provider.kind])

  // Only what somebody touched. A secret box left empty is a secret left alone;
  // every other field is sent as it stands, empty included, since clearing an
  // optional setting is a real edit.
  const edits = useMemo(() => {
    const out = {}
    for (const field of spec?.fields || []) {
      const value = values[field.key] ?? ''
      if (field.secret) {
        if (value !== '') out[field.key] = value
        continue
      }
      if (value !== (provider.options?.[field.key] || '')) out[field.key] = value
    }
    return out
  }, [spec, values, provider.options])

  const save = async (e) => {
    e.preventDefault()
    if (busy || Object.keys(edits).length === 0) return

    setBusy(true)
    setError(null)
    try {
      await api.updateProvider(provider.id, { options: edits })
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  if (!spec) {
    return error
      ? <Banner tone="error">{error}</Banner>
      : <Spinner />
  }

  const plain = spec.fields.filter((field) => !field.advanced)
  const advanced = spec.fields.filter((field) => field.advanced)

  return (
    <>
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {/* Whatever the account last said when it was asked. It is the reason
          somebody is on this tab, and reading it beside the fields that might
          fix it beats remembering it from the card behind the dialog. */}
      {!provider.online && provider.error && (
        <Banner tone="error">
          <strong>{provider.name} is not answering.</strong>{' '}
          <span style={{ wordBreak: 'break-word' }}>{provider.error}</span>
        </Banner>
      )}

      {/* Outside the form below on purpose. A sign-in is its own errand with
          its own buttons, and a button inside a form is a submit button unless
          it says otherwise — "Continue with Google" saving the settings form on
          its way to the provider is not a mistake worth leaving available. */}
      {(spec.oauth || spec.sign_in_link) && (
        <Reauthorize
          provider={provider}
          spec={spec}
          disabled={busy}
          onDone={() => { onChanged(); onClose() }}
        />
      )}

      <form onSubmit={save}>
        <SpecFields
          fields={plain}
          values={values}
          disabled={busy}
          secretPlaceholder="unchanged"
          keyName={provider.name}
          onChange={(key, value) => setValues((current) => ({ ...current, [key]: value }))}
        />

        {/* The fields a sign-in fills in for you. Kept behind a disclosure for
            the same reason the connect form keeps them there: an OAuth client ID
            and a refresh token are things you paste when the button above will
            not do, not things you edit on the way past. */}
        {advanced.length > 0 && (
          <>
            <button
              type="button"
              onClick={() => setShowAdvanced((open) => !open)}
              aria-expanded={showAdvanced}
              disabled={busy}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: '6px',
                padding: '4px 2px',
                marginBottom: showAdvanced ? '10px' : '14px',
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
                transform: showAdvanced ? 'rotate(90deg)' : 'none',
                transition: 'transform 140ms ease',
              }}>▶</span>
              {showAdvanced ? 'Hide the tokens' : 'Set the tokens by hand'}
            </button>

            {showAdvanced && (
              <div style={{
                padding: '12px 12px 2px',
                marginBottom: '14px',
                background: COLORS.bg,
                border: `1px solid ${COLORS.border}`,
                borderRadius: '6px',
              }}>
                <SpecFields
                  fields={advanced}
                  values={values}
                  disabled={busy}
                  secretPlaceholder="unchanged"
                  keyName={provider.name}
                  onChange={(key, value) => setValues((current) => ({ ...current, [key]: value }))}
                />
                {advanced.some((field) => field.secret) && (
                  <p style={{
                    margin: '0 0 12px',
                    fontFamily: FONT.sans,
                    fontSize: '11px',
                    color: COLORS.textMuted,
                    lineHeight: 1.5,
                  }}>
                    {storedSecrets(provider, advanced).length > 0
                      ? `SAND is holding ${listOf(storedSecrets(provider, advanced))} for this account. ` +
                        'They are never sent to this page, so a box left empty keeps the one it has.'
                      : 'A box left empty keeps whatever the account already has.'}
                  </p>
                )}
              </div>
            )}
          </>
        )}

        {/* Settings that decide where parts go are not settings that move them.
            Only worth saying to an account that is actually holding something —
            on an empty one there is nothing to leave behind. */}
        {provider.shards > 0 && (
          <p style={{
            margin: '0 0 14px',
            fontFamily: FONT.sans,
            fontSize: '11px',
            color: COLORS.textMuted,
            lineHeight: 1.55,
          }}>
            Pointing this account somewhere else — another bucket, another folder,
            another account entirely — does not carry the {provider.shards} part
            {provider.shards === 1 ? '' : 's'} already on it across. SAND will look
            in the new place and not find them, and the part badges in the file
            list are where that shows up. Signing back in above is the safe kind
            of change: it replaces the credentials and nothing else.
          </p>
        )}

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            disabled={busy || Object.keys(edits).length === 0}
          >
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy ? 'Connecting…' : 'Save and reconnect'}
          </Button>
        </div>
      </form>
    </>
  )
}

/* Putting a connected account back through the provider's consent screen.

   One button, most of the time: the account was connected through an OAuth app
   whose details are in the vault, so SAND reuses them rather than asking for
   them again — which it could not do anyway, since the app's secret has never
   been sent to this page. The only case that still asks is an account whose
   stored app has stopped working, which is exactly what "the OAuth client was
   deleted" means, and then a new client ID goes in here.

   What comes back replaces the credentials on this account. It does not make a
   second one: same ID, same name, same colour, same parts, and the file index
   goes on pointing at it. */
function Reauthorize({ provider, spec, disabled, onDone }) {
  // 'idle' until somebody asks for it; then 'waiting' while the provider has
  // the browser, and 'ready' once the server is holding tokens that have not
  // been spent on the account yet.
  const [step, setStep] = useState('idle')
  const [flow, setFlow] = useState(null)
  const [account, setAccount] = useState('')
  const [client, setClient] = useState({ clientId: '', clientSecret: '' })
  const [needClient, setNeedClient] = useState(false)
  const [pasted, setPasted] = useState('')
  const [showPaste, setShowPaste] = useState(false)

  // The link a sign-in that cannot redirect produces, once its client is up.
  const [signInURL, setSignInURL] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  /* A sign-in that took over the tab rather than opening a window: the sidebar
     reopened this dialog on it, and this is where it is picked back up. */
  useEffect(() => {
    const pending = pendingOAuthFlow()
    if (!pending || pending.provider_id !== provider.id) return
    setFlow(pending)
    setStep('waiting')
  }, [provider.id])

  const begin = async () => {
    setBusy(true)
    setError(null)
    setSignInURL('')
    try {
      /* A link-style sign-in reuses the account's own settings — its folder,
         its client, its state directory — so the browser sends nothing but the
         account's ID and the server reads the rest off the account. */
      if (spec.sign_in_link) {
        const resp = await api.protonSignIn({ providerId: provider.id })
        const started = { id: resp.flow_id, kind: provider.kind, provider_id: provider.id }
        rememberFlow(started)
        setFlow(started)
        setStep('waiting')
        return
      }

      const resp = await api.oauthStart(provider.kind, {
        providerId: provider.id,
        clientId: client.clientId,
        clientSecret: client.clientSecret,
        redirectUri: callbackURL(),
      })
      const started = {
        id: resp.flow_id,
        kind: provider.kind,
        redirect_uri: resp.redirect_uri,
        provider_id: provider.id,
      }
      rememberFlow(started)
      setFlow(started)
      setStep('waiting')
      openAuthWindow(resp.auth_url)
    } catch (err) {
      // The app this account was connected through is gone or was never
      // stored, so there is nothing to reuse and a new one has to be named.
      if (err.code === 'OAUTH_NO_CLIENT') setNeedClient(true)
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  useSignInResult({
    flowId: flow?.id,
    active: step === 'waiting',
    onReady: (resp) => {
      setAccount(resp.account || '')
      setStep('ready')
    },
    onFailed: (message) => {
      setError(message)
      setSignInURL('')
      setStep('idle')
    },
    onPending: (resp) => {
      if (resp.sign_in_url) setSignInURL(resp.sign_in_url)
    },
  })

  const finishByHand = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.oauthExchange(flow.id, pasted)
      forgetFlow()
      setAccount(resp.account || '')
      setStep('ready')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const apply = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.oauthReauthorize(flow.id, provider.id)
      forgetFlow()
      onDone()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  const cancel = () => {
    forgetFlow()
    setFlow(null)
    setSignInURL('')
    setStep('idle')
    setPasted('')
    setShowPaste(false)
  }

  return (
    <div style={{
      padding: '13px 13px 3px',
      marginBottom: '16px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '7px',
    }}>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '10px',
        fontWeight: 700,
        letterSpacing: '1.5px',
        textTransform: 'uppercase',
        color: COLORS.textMuted,
        marginBottom: '9px',
      }}>Sign in again</div>

      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {step === 'idle' && (
        <>
          <p style={{
            margin: '0 0 12px',
            fontFamily: FONT.sans,
            fontSize: '12px',
            color: COLORS.textDim,
            lineHeight: 1.6,
          }}>
            {spec.sign_in_link
              ? <>Signs in to {spec.label} again and replaces the session SAND
                holds for this account. It stays the same account throughout —
                same name, same colour, same
                {provider.shards === 1 ? ' part' : ' parts'}.</>
              : <>Sends you back to {spec.label} to approve access again, and
                replaces the tokens SAND holds for this account with the new
                ones. It stays the same account throughout — same name, same
                colour, same{provider.shards === 1 ? ' part' : ' parts'}.</>}
          </p>

          {needClient && spec.oauth && (
            <>
              <p style={{
                margin: '0 0 10px',
                fontFamily: FONT.sans,
                fontSize: '11.5px',
                color: COLORS.textMuted,
                lineHeight: 1.55,
              }}>
                The {spec.label} app this account was connected through is no
                longer usable. Register another one and paste its details here —
                the redirect URI to give it is{' '}
                <code style={{ color: COLORS.textDim, wordBreak: 'break-all' }}>{callbackURL()}</code>.
                {spec.oauth.console_url && (
                  <>
                    {' '}
                    <a href={spec.oauth.console_url} target="_blank" rel="noreferrer"
                      style={{ color: COLORS.accent }}>Open the developer console ↗</a>
                  </>
                )}
              </p>
              <Input
                label="Client ID *"
                value={client.clientId}
                spellCheck={false}
                disabled={disabled || busy}
                placeholder="from the developer console"
                onChange={(e) => setClient({ ...client, clientId: e.target.value })}
              />
              <PasswordInput
                label={`Client secret${spec.oauth.secret_required ? ' *' : ''}`}
                help={spec.oauth.secret_required ? undefined : 'Leave blank for a public client.'}
                value={client.clientSecret}
                disabled={disabled || busy}
                onChange={(e) => setClient({ ...client, clientSecret: e.target.value })}
              />
            </>
          )}

          <Button
            type="button"
            variant="primary"
            onClick={begin}
            disabled={disabled || busy || (needClient && !client.clientId.trim())}
            style={{ width: '100%', justifyContent: 'center', marginBottom: '13px' }}
          >
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy ? 'Opening…' : (spec.sign_in_link?.sign_in_label || spec.oauth.sign_in_label)}
          </Button>
        </>
      )}

      {step === 'waiting' && (
        <>
          <div style={{
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
            padding: '4px 0 12px',
            fontFamily: FONT.sans,
            fontSize: '12.5px',
            color: COLORS.textDim,
          }}>
            <Spinner size={14} />
            {/* "Hand the account back" is redirect wording, and a link-style
                sign-in never redirects: nothing is coming back, somebody has to
                go and follow a link. */}
            {spec.sign_in_link
              ? (signInURL
                ? <>Waiting for you to sign in to {spec.label}…</>
                : <>Starting {spec.label}&apos;s client…</>)
              : <>Waiting for {spec.label} to hand the account back…</>}
          </div>

          {/* A link-style sign-in has no window that opened and no redirect to
              paste. What it has is a link, worth copying rather than only
              clicking: the device meant to follow it is often not this one. */}
          {spec.sign_in_link && signInURL && (
            <>
              <p style={{
                margin: '0 0 10px',
                fontFamily: FONT.sans, fontSize: '11.5px',
                color: COLORS.textMuted, lineHeight: 1.6,
              }}>
                Open this on any device. Sign in as the same account
                {' '}{provider.name} already holds its parts on — signing in as a
                different one leaves them where SAND cannot reach them.
              </p>
              <CopyField value={signInURL} />
              <Button
                type="button"
                variant="primary"
                onClick={() => window.open(signInURL, '_blank', 'noopener,noreferrer')}
                style={{ width: '100%', justifyContent: 'center', margin: '10px 0 12px' }}
              >Open in this browser</Button>
            </>
          )}

          {!spec.sign_in_link && !showPaste && (
            <button
              type="button"
              onClick={() => setShowPaste(true)}
              style={{
                background: 'none', border: 'none', cursor: 'pointer', padding: 0,
                fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted,
                textDecoration: 'underline', marginBottom: '12px',
              }}
            >The page did not come back — paste the URL instead</button>
          )}

          {!spec.sign_in_link && showPaste && (
            <>
              {/* The way back in when the redirect cannot reach this server —
                  a vault on localhost being driven from a phone, most often. */}
              <p style={{
                margin: '0 0 10px',
                fontFamily: FONT.sans, fontSize: '11.5px',
                color: COLORS.textMuted, lineHeight: 1.6,
              }}>
                Copy the whole URL the provider landed on — the one starting{' '}
                <code style={{ color: COLORS.textDim, wordBreak: 'break-all' }}>
                  {flow?.redirect_uri || callbackURL()}
                </code>{' '}
                — and paste it here.
              </p>
              <Input
                label="Redirect URL"
                value={pasted}
                disabled={busy}
                onChange={(e) => setPasted(e.target.value)}
                placeholder="http://…/api/providers/oauth/callback?code=…&state=…"
              />
              <Button
                type="button"
                variant="primary"
                onClick={finishByHand}
                disabled={busy || !pasted.trim()}
                style={{ width: '100%', justifyContent: 'center', marginBottom: '12px' }}
              >{busy ? 'Finishing…' : 'Finish sign-in'}</Button>
            </>
          )}

          <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: '10px' }}>
            <Button type="button" size="sm" variant="ghost" onClick={cancel}>Cancel</Button>
          </div>
        </>
      )}

      {step === 'ready' && (
        <>
          <Banner tone={account ? 'warn' : 'success'}>
            {account
              ? <>Signed in as <strong>{account}</strong>. Make sure that is the account
                {' '}{provider.name} holds its parts on — signing in as a different one
                leaves them where SAND can no longer reach them.</>
              : <>Authorized. SAND holds the new tokens; they never reach this page.</>}
          </Banner>

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginBottom: '11px' }}>
            <Button type="button" variant="ghost" onClick={cancel} disabled={busy}>Cancel</Button>
            <Button type="button" variant="primary" onClick={apply} disabled={busy}>
              {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
              {busy ? 'Reconnecting…' : `Reconnect ${provider.name}`}
            </Button>
          </div>
        </>
      )}
    </div>
  )
}

/* The secret settings this account actually has one stored for. The server
   substitutes a placeholder for every one of them on the way out, so their
   presence is readable from here even though their values are not. */
function storedSecrets(provider, fields) {
  return fields
    .filter((field) => field.secret && provider.options?.[field.key] === STORED_SECRET)
    .map((field) => field.label.toLowerCase())
}

/* "a and b", "a, b and c" — a list read out rather than punctuated. */
function listOf(items) {
  if (items.length <= 1) return items.join('')
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`
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
