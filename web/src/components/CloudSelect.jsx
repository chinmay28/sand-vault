import React, { useMemo, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Choosing where a file's parts go.

   A file is split into three parts and each part goes to a different account,
   so "which clouds" is a choice of three — the vault's default, or something
   else for one upload. Both are made with the same list of rows, which is what
   lives here. */

export const PARTS_PER_FILE = 3
const MIN_ACCOUNTS = 2

/* The clouds an upload starts on: the vault's default, and with none set three
   picked at random — which is exactly what the picker then lets the user
   change. A default is taken as it stands rather than made up to three, the
   same way the server takes it, so a default of two clouds does not quietly
   become three.

   Reachable accounts are drawn first, so a random pick does not send parts at
   an account that is not answering while a working one sits idle. */
export function initialSelection(providers, defaults = []) {
  const connected = new Set(providers.map((p) => p.id))
  const preferred = (defaults || []).filter((id) => connected.has(id))
  if (preferred.length > 0) return preferred.slice(0, PARTS_PER_FILE)

  const pool = [...shuffle(providers.filter((p) => p.online)), ...shuffle(providers.filter((p) => !p.online))]
  return pool.slice(0, PARTS_PER_FILE).map((p) => p.id)
}

function shuffle(items) {
  const out = [...items]
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [out[i], out[j]] = [out[j], out[i]]
  }
  return out
}

/* The rows themselves. Selection is capped at three because a file has three
   parts: a fourth account would have nothing to hold. */
export function CloudChoice({ providers, selected, onChange }) {
  const full = selected.length >= PARTS_PER_FILE

  const toggle = (id) => {
    if (selected.includes(id)) onChange(selected.filter((s) => s !== id))
    else if (!full) onChange([...selected, id])
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
      {providers.map((provider) => {
        const chosen = selected.includes(provider.id)
        const part = chosen ? selected.indexOf(provider.id) + 1 : null
        return (
          <button
            key={provider.id}
            type="button"
            role="checkbox"
            aria-checked={chosen}
            onClick={() => toggle(provider.id)}
            disabled={!chosen && full}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '10px',
              width: '100%',
              padding: '10px 12px',
              textAlign: 'left',
              background: chosen ? COLORS.surfaceRaised : COLORS.bg,
              border: `1px solid ${chosen ? COLORS.accent : COLORS.border}`,
              borderLeft: `3px solid ${accountColor(provider.id)}`,
              borderRadius: '6px',
              color: COLORS.text,
              cursor: !chosen && full ? 'not-allowed' : 'pointer',
              opacity: !chosen && full ? 0.45 : 1,
            }}
          >
            {/* The badge doubles as the part number, so the row says both
                "chosen" and "this is where part 2 goes". */}
            <span style={{
              width: '20px',
              height: '20px',
              flexShrink: 0,
              borderRadius: '4px',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              fontFamily: FONT.mono,
              fontSize: '10px',
              fontWeight: 700,
              color: chosen ? COLORS.bg : COLORS.textMuted,
              background: chosen ? accountColor(provider.id) : 'transparent',
              border: chosen ? 'none' : `1px dashed ${COLORS.border}`,
            }}>{part || ''}</span>

            <span style={{ fontSize: '13px', flexShrink: 0 }}>{KIND_ICONS[provider.kind] || '☁'}</span>

            <span style={{ flex: 1, minWidth: 0 }}>
              <span style={{
                display: 'block',
                fontFamily: FONT.mono,
                fontSize: '12px',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
              }}>{provider.name}</span>
              <span style={{
                display: 'block',
                fontFamily: FONT.mono,
                fontSize: '10px',
                color: COLORS.textMuted,
                marginTop: '2px',
              }}>
                {provider.kind} · {provider.shards} part{provider.shards === 1 ? '' : 's'} · {formatBytes(provider.stored)}
              </span>
            </span>

            <span
              title={provider.online ? 'Reachable' : provider.error || 'Unreachable'}
              style={{
                width: '7px',
                height: '7px',
                borderRadius: '50%',
                flexShrink: 0,
                background: provider.online ? COLORS.success : COLORS.error,
              }}
            />
          </button>
        )
      })}

      {/* A row that cannot be clicked has to say why, or it reads as broken. */}
      {full && providers.length > PARTS_PER_FILE && (
        <p style={{
          margin: '2px 0 0',
          fontFamily: FONT.mono,
          fontSize: '10px',
          color: COLORS.textMuted,
        }}>
          {PARTS_PER_FILE} parts, {PARTS_PER_FILE} clouds — unpick one to swap it for another.
        </p>
      )}
    </div>
  )
}

/* What a selection means, said before the upload rather than after it. */
function SelectionNote({ providers, selected }) {
  if (selected.length < MIN_ACCOUNTS) {
    return (
      <Banner tone="error">
        Choose at least {MIN_ACCOUNTS} clouds. Any two parts rebuild a file, so one cloud on its
        own could not — and would be the only thing standing between you and losing it.
      </Banner>
    )
  }
  if (selected.length < PARTS_PER_FILE) {
    return (
      <Banner tone="warn">
        With {selected.length} clouds only {selected.length} of the {PARTS_PER_FILE} parts are
        stored. The file is recoverable and still unreadable to either cloud alone, but it has no
        spare part if one of them goes away.
      </Banner>
    )
  }
  const offline = providers.filter((p) => selected.includes(p.id) && !p.online)
  if (offline.length) {
    return (
      <Banner tone="warn">
        {offline.map((p) => p.name).join(', ')} {offline.length === 1 ? 'is' : 'are'} not answering.
        The upload will go ahead if the others accept their parts, and that part will be missing.
      </Banner>
    )
  }
  return null
}

/* The dialog every upload passes through: which files are going up, which three
   clouds they are being scattered over, and the chance to change that before a
   single byte leaves the machine. */
export function UploadDestination({ files, path, providers, defaults, onUpload, onClose, onChanged }) {
  const [selected, setSelected] = useState(() => initialSelection(providers, defaults))
  const [remember, setRemember] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const usingDefault = useMemo(() => {
    const set = new Set(defaults || [])
    return set.size > 0 && selected.length === set.size && selected.every((id) => set.has(id))
  }, [defaults, selected])

  const names = files.map((f) => f.name)
  const total = files.reduce((sum, f) => sum + f.size, 0)

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      // Saved first: an upload that fails should not also lose the choice the
      // user just made about where their files live from now on.
      if (remember) {
        await api.setDefaultAccounts(selected)
        onChanged?.()
      }
      onUpload(selected)
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={names.length === 1 ? `Upload ${names[0]}` : `Upload ${names.length} files`}
      subtitle={`${formatBytes(total)} into ${path} — split into ${PARTS_PER_FILE} encrypted parts, one per cloud`}
      onClose={busy ? undefined : onClose}
      width={460}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <p style={{
        margin: '0 0 12px',
        fontFamily: FONT.sans,
        fontSize: '12px',
        color: COLORS.textMuted,
        lineHeight: 1.6,
      }}>
        {usingDefault
          ? 'Your default clouds are selected. Change them for this upload if you want.'
          : (defaults?.length
            ? 'Chosen for this upload, in place of your default clouds.'
            : `No default is set, so ${PARTS_PER_FILE} clouds were picked at random. Change them if you want.`)}
      </p>

      <CloudChoice providers={providers} selected={selected} onChange={setSelected} />

      <div style={{ marginTop: '14px' }}>
        <SelectionNote providers={providers} selected={selected} />
      </div>

      <label style={{
        display: 'flex',
        alignItems: 'center',
        gap: '8px',
        margin: '10px 0 16px',
        fontFamily: FONT.mono,
        fontSize: '11px',
        color: COLORS.textDim,
        cursor: 'pointer',
      }}>
        <input
          type="checkbox"
          checked={remember}
          onChange={(e) => setRemember(e.target.checked)}
          disabled={selected.length < MIN_ACCOUNTS}
          style={{ accentColor: COLORS.accent, width: '15px', height: '15px', minHeight: 0 }}
        />
        Make this the default for the whole vault
      </label>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button
          type="button"
          variant="primary"
          onClick={submit}
          disabled={busy || selected.length < MIN_ACCOUNTS}
        >
          {busy ? <Spinner size={10} /> : null}
          ↑ Upload to {selected.length} cloud{selected.length === 1 ? '' : 's'}
        </Button>
      </div>
    </Modal>
  )
}

/* The same choice, made once for the whole vault. */
export function DefaultClouds({ providers, defaults, onClose, onChanged }) {
  const [selected, setSelected] = useState(() => (defaults || []).filter(
    (id) => providers.some((p) => p.id === id)))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const save = async (accounts) => {
    setBusy(true)
    setError(null)
    try {
      await api.setDefaultAccounts(accounts)
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Default clouds"
      subtitle="Where uploads go unless they choose otherwise. Every upload can still pick its own."
      onClose={busy ? undefined : onClose}
      width={460}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <CloudChoice providers={providers} selected={selected} onChange={setSelected} />

      {selected.length > 0 && selected.length < MIN_ACCOUNTS && (
        <div style={{ marginTop: '14px' }}>
          <Banner tone="error">
            Choose at least {MIN_ACCOUNTS} clouds, or none at all to let every upload pick its own.
          </Banner>
        </div>
      )}

      <p style={{
        margin: '14px 0 16px',
        fontFamily: FONT.sans,
        fontSize: '12px',
        color: COLORS.textMuted,
        lineHeight: 1.6,
      }}>
        With no default, each file picks {PARTS_PER_FILE} clouds of its own at random — which is
        what spreads a vault evenly over more than {PARTS_PER_FILE} accounts.
      </p>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
        <Button type="button" variant="ghost" onClick={() => save([])} disabled={busy}>
          Pick per upload
        </Button>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button
          type="button"
          variant="primary"
          onClick={() => save(selected)}
          disabled={busy || selected.length < MIN_ACCOUNTS}
        >
          {busy ? <Spinner size={10} /> : null}Save default
        </Button>
      </div>
    </Modal>
  )
}
