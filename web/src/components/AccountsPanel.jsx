import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'
import { DevMark } from './Brand'

/* The sidebar: every cloud account SAND is wired into, whether it is answering,
   and how much of the vault it is carrying. Below the two-pane breakpoint the
   same panel becomes a drawer over the file browser — the file list is what you
   came for on a phone, and the accounts are a place you visit. */
export default function AccountsPanel({
  providers, loading, stats, mobile, open, onClose, onRefresh, onChanged,
}) {
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState(null)

  useEffect(() => {
    if (!mobile || !open) return
    const onKey = (e) => { if (e.key === 'Escape') onClose?.() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [mobile, open, onClose])

  const remove = async (provider) => {
    const confirmed = window.confirm(
      `Disconnect "${provider.name}"?\n\n` +
      `SAND will stop using it and will forget the ${provider.shards} part(s) it holds. ` +
      `The data itself stays on the account until you delete it there.`
    )
    if (!confirmed) return

    setError(null)
    try {
      await api.removeProvider(provider.id, false)
      onChanged()
    } catch (err) {
      // The server refuses when a file would be left unrecoverable; offer the
      // forced path only after saying plainly what it costs.
      const force = window.confirm(`${err.message}\n\nDisconnect anyway?`)
      if (!force) { setError(err.message); return }
      try {
        await api.removeProvider(provider.id, true)
        onChanged()
      } catch (forceErr) {
        setError(forceErr.message)
      }
    }
  }

  const enough = providers.length >= 2

  const panel = (
    <aside style={{
      width: mobile ? 'min(320px, 86vw)' : '286px',
      flexShrink: 0,
      borderRight: `1px solid ${COLORS.border}`,
      background: COLORS.surface,
      display: 'flex',
      flexDirection: 'column',
      overflow: 'hidden',
      ...(mobile ? {
        position: 'fixed',
        top: 0,
        bottom: 0,
        left: 0,
        zIndex: 90,
        transform: open ? 'translateX(0)' : 'translateX(-100%)',
        // Hidden rather than merely off-screen, so nothing in here is
        // reachable by tab while the drawer is shut. visibility holds its old
        // value for the whole transition, so the panel still slides out.
        visibility: open ? 'visible' : 'hidden',
        transition: 'transform 200ms ease, visibility 200ms',
        boxShadow: '0 0 40px rgba(0,0,0,0.5)',
      } : null),
    }}>
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: '8px',
        justifyContent: 'space-between',
        padding: mobile ? '10px 12px 10px 16px' : '16px 16px 12px',
        borderBottom: mobile ? `1px solid ${COLORS.border}` : 'none',
      }}>
        <span style={{
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 700,
          letterSpacing: '1.5px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
        }}>Connected clouds</span>

        <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          {loading ? <Spinner size={12} /> : (
            <button
              onClick={onRefresh}
              title="Re-check every account"
              aria-label="Re-check every account"
              style={{ background: 'none', border: 'none', color: COLORS.textMuted, cursor: 'pointer', fontSize: '13px', padding: '0 6px' }}
            >⟳</button>
          )}
          {mobile && (
            <button
              onClick={onClose}
              aria-label="Close"
              style={{ background: 'none', border: 'none', color: COLORS.textMuted, cursor: 'pointer', fontSize: '15px', padding: '0 6px' }}
            >✕</button>
          )}
        </span>
      </div>

      <div style={{ flex: 1, overflowY: 'auto', padding: '0 12px' }}>
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        {providers.length === 0 && !loading && (
          <p style={{
            fontFamily: FONT.sans,
            fontSize: '12px',
            color: COLORS.textMuted,
            lineHeight: 1.6,
            padding: '4px 4px 12px',
          }}>
            No accounts yet. SAND splits every file into three parts and needs
            somewhere separate to put each one.
          </p>
        )}

        {providers.map((provider) => (
          <AccountCard key={provider.id} provider={provider} onRemove={() => remove(provider)} />
        ))}

        {!enough && providers.length > 0 && (
          <Banner tone="warn">
            Connect at least {2 - providers.length} more account{providers.length === 1 ? '' : 's'} so
            no single one holds enough parts to rebuild a file.
          </Banner>
        )}
      </div>

      <div style={{ padding: '12px', borderTop: `1px solid ${COLORS.border}` }}>
        <Button
          variant="primary"
          onClick={() => setConnecting(true)}
          style={{ width: '100%', justifyContent: 'center' }}
        >+ Connect a cloud</Button>

        {stats && (
          <div style={{
            marginTop: '12px',
            fontFamily: FONT.mono,
            fontSize: '10px',
            color: COLORS.textMuted,
            lineHeight: 1.9,
          }}>
            <div>{stats.files} file{stats.files === 1 ? '' : 's'} · {formatBytes(stats.bytes)}</div>
            <div>{formatBytes(stats.stored_bytes)} stored across accounts</div>
            <div>placement: {stats.policy}</div>
            {stats.degraded > 0 && (
              <div style={{ color: COLORS.warn }}>{stats.degraded} file(s) missing a spare part</div>
            )}
          </div>
        )}

        {/* Turned out of the header on a phone, it lands here. */}
        {mobile && (
          <div style={{ display: 'flex', justifyContent: 'center', marginTop: '14px' }}>
            <DevMark bare />
          </div>
        )}
      </div>

      {connecting && (
        <ConnectModal
          onClose={() => setConnecting(false)}
          onConnected={() => { setConnecting(false); onChanged() }}
        />
      )}
    </aside>
  )

  if (!mobile) return panel

  return (
    <>
      <div
        onClick={onClose}
        aria-hidden="true"
        style={{
          position: 'fixed',
          inset: 0,
          zIndex: 85,
          background: 'rgba(3, 6, 12, 0.66)',
          opacity: open ? 1 : 0,
          visibility: open ? 'visible' : 'hidden',
          transition: 'opacity 200ms ease, visibility 200ms',
        }}
      />
      {panel}
    </>
  )
}

function AccountCard({ provider, onRemove }) {
  const [testing, setTesting] = useState(false)
  const [result, setResult] = useState(null)
  const color = accountColor(provider.id)

  const test = async () => {
    setTesting(true)
    setResult(null)
    try {
      const resp = await api.testProvider(provider.id)
      setResult(resp.online ? { ok: true } : { ok: false, error: resp.error })
    } catch (err) {
      setResult({ ok: false, error: err.message })
    } finally {
      setTesting(false)
    }
  }

  const online = result ? result.ok : provider.online
  const errorText = result && !result.ok ? result.error : provider.error
  const quota = provider.usage && provider.usage.total > 0
    ? `${formatBytes(provider.usage.used)} / ${formatBytes(provider.usage.total)} used`
    : null

  return (
    <div style={{
      padding: '11px 12px',
      marginBottom: '8px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderLeft: `3px solid ${color}`,
      borderRadius: '6px',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
        <span style={{ fontSize: '13px' }}>{KIND_ICONS[provider.kind] || '☁'}</span>
        <span style={{
          flex: 1,
          minWidth: 0,
          fontFamily: FONT.mono,
          fontSize: '12px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{provider.name}</span>
        <span
          title={online ? 'Reachable' : errorText || 'Unreachable'}
          style={{
            width: '7px',
            height: '7px',
            borderRadius: '50%',
            flexShrink: 0,
            background: online ? COLORS.success : COLORS.error,
          }}
        />
      </div>

      <div style={{
        marginTop: '6px',
        fontFamily: FONT.mono,
        fontSize: '10px',
        color: COLORS.textMuted,
        lineHeight: 1.7,
      }}>
        <div>{provider.kind} · {provider.shards} part{provider.shards === 1 ? '' : 's'} · {formatBytes(provider.stored)}</div>
        {quota && <div>{quota}</div>}
        {!online && errorText && (
          <div style={{ color: COLORS.error, wordBreak: 'break-word' }}>{errorText}</div>
        )}
      </div>

      <div style={{ display: 'flex', gap: '6px', marginTop: '8px' }}>
        <Button size="sm" variant="ghost" onClick={test} disabled={testing}>
          {testing ? <Spinner size={10} /> : null}{testing ? 'Testing' : 'Test'}
        </Button>
        <Button size="sm" variant="ghost" onClick={onRemove}
          style={{ color: COLORS.error }}>Disconnect</Button>
      </div>
    </div>
  )
}

/* The connect form is generated from the backend's own field specs, so a new
   provider kind appears here without any frontend change. */
function ConnectModal({ onClose, onConnected }) {
  const [specs, setSpecs] = useState([])
  const [kind, setKind] = useState(null)
  const [name, setName] = useState('')
  const [values, setValues] = useState({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  useEffect(() => {
    api.providerSpecs()
      .then((resp) => setSpecs(resp.specs || []))
      .catch((err) => setError(err.message))
  }, [])

  const spec = useMemo(() => specs.find((s) => s.kind === kind), [specs, kind])

  const choose = (nextSpec) => {
    setKind(nextSpec.kind)
    setName(nextSpec.label)
    setError(null)
    const defaults = {}
    for (const field of nextSpec.fields) {
      if (field.default) defaults[field.key] = field.default
    }
    setValues(defaults)
  }

  const submit = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.addProvider(kind, name, values)
      onConnected()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (!spec) {
    return (
      <Modal
        title="Connect a cloud account"
        subtitle="Pick where SAND should put one of the parts. Each account holds an encrypted fragment that is useless on its own."
        onClose={onClose}
      >
        {error && <Banner tone="error">{error}</Banner>}
        {specs.length === 0 && !error && <Spinner />}
        {specs.map((option) => (
          <button
            key={option.kind}
            onClick={() => choose(option)}
            style={{
              display: 'block',
              width: '100%',
              textAlign: 'left',
              padding: '13px 14px',
              marginBottom: '9px',
              background: COLORS.bg,
              border: `1px solid ${COLORS.border}`,
              borderRadius: '7px',
              cursor: 'pointer',
              color: COLORS.text,
            }}
          >
            <div style={{
              display: 'flex', alignItems: 'center', gap: '9px',
              fontFamily: FONT.mono, fontSize: '13px', marginBottom: '5px',
            }}>
              <span>{KIND_ICONS[option.kind] || '☁'}</span>{option.label}
            </div>
            <div style={{
              fontFamily: FONT.sans, fontSize: '11.5px',
              color: COLORS.textMuted, lineHeight: 1.55,
            }}>{option.description}</div>
          </button>
        ))}
      </Modal>
    )
  }

  return (
    <Modal
      title={`Connect ${spec.label}`}
      subtitle={spec.description}
      onClose={onClose}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        <Input
          label="Display name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="How this account appears in the sidebar"
        />

        {spec.fields.map((field) => {
          const Control = field.secret ? PasswordInput : Input
          return (
            <Control
              key={field.key}
              label={field.label + (field.required ? ' *' : '')}
              help={field.help}
              placeholder={field.placeholder}
              value={values[field.key] || ''}
              onChange={(e) => setValues({ ...values, [field.key]: e.target.value })}
            />
          )
        })}

        {spec.docs_url && (
          <p style={{
            fontFamily: FONT.sans, fontSize: '11px',
            color: COLORS.textMuted, marginBottom: '14px',
          }}>
            Need credentials?{' '}
            <a href={spec.docs_url} target="_blank" rel="noreferrer"
              style={{ color: COLORS.accent }}>Provider documentation ↗</a>
          </p>
        )}

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
          <Button type="button" variant="ghost" onClick={() => setKind(null)}>← Back</Button>
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy ? 'Testing connection…' : 'Connect'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}
