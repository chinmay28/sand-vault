import React, { useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, IconButton, Spinner } from './ui'
import ConnectCloud, { pendingOAuthFlow } from './ConnectCloud'
import ChangePassword from './ChangePassword'
import { DefaultClouds, PARTS_PER_FILE } from './CloudSelect'
import { DevMark } from './Brand'

/* The sidebar: every cloud account SAND is wired into, whether it is answering,
   and how much of the vault it is carrying. Below the two-pane breakpoint the
   same panel becomes a drawer over the file browser — the file list is what you
   came for on a phone, and the accounts are a place you visit. */
export default function AccountsPanel({
  providers, loading, stats, mobile, open, onClose, onRefresh, onChanged,
}) {
  // A sign-in that took over the tab is still in flight when the app reloads:
  // reopen the dialog on it rather than making the user start again.
  const [connecting, setConnecting] = useState(() => Boolean(pendingOAuthFlow()))
  const [changingPassword, setChangingPassword] = useState(false)
  const [choosingDefaults, setChoosingDefaults] = useState(false)
  const [error, setError] = useState(null)

  const defaults = stats?.default_accounts || []

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
            <IconButton
              glyph="⟳"
              label="Re-check every account"
              tone="muted"
              size={mobile ? 44 : 28}
              onClick={onRefresh}
            />
          )}
          {mobile && (
            <IconButton glyph="✕" label="Close" tone="muted" size={44} onClick={onClose} />
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
          <AccountCard
            key={provider.id}
            provider={provider}
            isDefault={defaults.includes(provider.id)}
            onRemove={() => remove(provider)}
          />
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

        {/* Which clouds an upload starts on. Every upload can still choose its
            own, so this is the answer it opens with rather than a rule. */}
        {providers.length > 0 && (
          <div style={{ marginTop: '10px' }}>
            <div style={{
              fontFamily: FONT.mono,
              fontSize: '10px',
              color: COLORS.textMuted,
              lineHeight: 1.9,
            }}>
              {defaults.length > 0
                ? `default clouds: ${defaults.length} of ${providers.length}`
                : `default clouds: ${PARTS_PER_FILE} picked per upload`}
            </div>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setChoosingDefaults(true)}
              style={{ padding: '4px 0' }}
            >{defaults.length > 0 ? 'Change default clouds' : 'Set default clouds'}</Button>
          </div>
        )}

        {stats?.pending_migration > 0 && (
          <PendingMigration
            count={stats.pending_migration}
            onDone={onChanged}
            onError={setError}
          />
        )}

        {/* Vault-wide rather than account-wide, but this is where the vault's
            own state already lives, and the header has no room for it on a
            phone. */}
        <div style={{ marginTop: '12px', display: 'flex' }}>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setChangingPassword(true)}
            style={{ padding: '4px 0' }}
          >Change vault password</Button>
        </div>

        {/* Turned out of the header on a phone, it lands here. */}
        {mobile && (
          <div style={{ display: 'flex', justifyContent: 'center', marginTop: '14px' }}>
            <DevMark bare />
          </div>
        )}
      </div>

      {connecting && (
        <ConnectCloud
          onClose={() => setConnecting(false)}
          onConnected={() => { setConnecting(false); onChanged() }}
        />
      )}

      {choosingDefaults && (
        <DefaultClouds
          providers={providers}
          defaults={defaults}
          onClose={() => setChoosingDefaults(false)}
          onChanged={onChanged}
        />
      )}

      {changingPassword && (
        <ChangePassword
          stats={stats}
          onClose={() => setChangingPassword(false)}
          onChanged={onChanged}
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

/* A password change rotates the key the parts on the accounts are encrypted
   under, and each file has to be gathered and scattered again to move onto it.
   Anything left behind is still readable — and still readable with the old
   password, which is the whole reason to finish. */
function PendingMigration({ count, onDone, onError }) {
  const [running, setRunning] = useState(false)

  const finish = async () => {
    setRunning(true)
    onError(null)
    try {
      const report = await api.migrate()
      if (report.warnings?.length) onError(report.warnings.join('\n'))
      onDone()
    } catch (err) {
      onError(err.message)
    } finally {
      setRunning(false)
    }
  }

  return (
    <Banner tone="warn">
      {count} file{count === 1 ? ' is' : 's are'} still encrypted under your previous
      password&apos;s key.
      <Button
        size="sm"
        variant="ghost"
        onClick={finish}
        disabled={running}
        style={{ marginTop: '8px' }}
      >
        {running ? <Spinner size={10} /> : null}
        {running ? 'Re-encrypting…' : 'Finish re-encrypting'}
      </Button>
    </Banner>
  )
}

function AccountCard({ provider, isDefault, onRemove }) {
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
        {/* The stripe down the card's edge says the same thing, but it is easy
            to read as decoration. This is the part badge's own shape and
            colour, sitting next to the account's name — the two halves of the
            match, spelled out. */}
        <span
          title="Parts stored here carry this colour in the file list"
          style={{
            width: '14px',
            height: '14px',
            flexShrink: 0,
            borderRadius: '3px',
            background: color,
          }}
        />
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
        {isDefault && (
          <span
            title="Uploads go here unless they choose otherwise"
            style={{
              flexShrink: 0,
              padding: '1px 5px',
              borderRadius: '3px',
              border: `1px solid ${COLORS.accentDim}`,
              color: COLORS.accentBright,
              fontFamily: FONT.mono,
              fontSize: '8.5px',
              fontWeight: 700,
              letterSpacing: '0.8px',
              textTransform: 'uppercase',
            }}>default</span>
        )}
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
