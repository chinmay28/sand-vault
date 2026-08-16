import React, { useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, IconButton, Spinner } from './ui'
import ConnectCloud, { pendingOAuthFlow } from './ConnectCloud'
import EditAccount from './EditAccount'
import ChangePassword from './ChangePassword'
import MountDrive from './MountDrive'
import { DefaultClouds, PARTS_PER_FILE } from './CloudSelect'
import { DevMark } from './Brand'

/* One number and the word for what it counts. The figure carries the weight —
   it is what someone glances down here to read — and the label under it is
   small enough to stay out of the way once you know which is which. */
function Figure({ value, label }) {
  return (
    <div>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '17px',
        fontWeight: 600,
        color: COLORS.text,
        lineHeight: 1.2,
        letterSpacing: '-0.5px',
      }}>{value}</div>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '9px',
        fontWeight: 600,
        letterSpacing: '1.2px',
        textTransform: 'uppercase',
        color: COLORS.textMuted,
        marginTop: '3px',
      }}>{label}</div>
    </div>
  )
}

/* A vault-wide action. Two lines: what it is, and what it does — the second
   line doubles as the place a status used to sit on its own above the button,
   so "3 per upload" is read off the thing it describes rather than from
   a line floating near it.

   It grows to fill half a row and takes the whole row when it is alone, so one,
   two or three of these all look deliberate. */
function ActionTile({ icon, label, hint, onClick }) {
  const [hover, setHover] = useState(false)

  return (
    <button
      type="button"
      onClick={onClick}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        flex: '1 1 calc(50% - 8px)',
        minWidth: '112px',
        // Comfortably past the 44px fingertip floor the rest of the app holds
        // to, since two lines of text need the room anyway.
        minHeight: '52px',
        display: 'flex',
        alignItems: 'center',
        gap: '9px',
        padding: '8px 10px',
        background: COLORS.surfaceRaised,
        border: `1px solid ${hover ? COLORS.borderBright : COLORS.border}`,
        borderRadius: '8px',
        cursor: 'pointer',
        textAlign: 'left',
        transition: 'border-color 0.15s ease, background 0.15s ease',
        ...(hover ? { background: COLORS.surfaceHover } : null),
      }}
    >
      {/* Hidden from the accessibility tree on purpose. The label beside it
          already says what the button is, so announcing the glyph as well would
          only be noise — and it would land in the button's accessible name,
          which is what a screen reader reads out and what the browser tests
          click by. */}
      <span aria-hidden="true" style={{
        fontSize: '15px',
        lineHeight: 1,
        flexShrink: 0,
        // Muted until hovered, so a row of tiles reads as text with marks
        // beside it rather than as a row of competing glyphs.
        opacity: hover ? 1 : 0.75,
        transition: 'opacity 0.15s ease',
      }}>{icon}</span>

      <span style={{ display: 'flex', flexDirection: 'column', gap: '2px', minWidth: 0 }}>
        <span style={{
          fontFamily: FONT.mono,
          fontSize: '11px',
          fontWeight: 600,
          letterSpacing: '0.5px',
          color: COLORS.text,
        }}>{label}</span>
        <span style={{
          fontFamily: FONT.mono,
          fontSize: '9px',
          color: COLORS.textMuted,
          lineHeight: 1.4,
        }}>{hint}</span>
      </span>
    </button>
  )
}

/* The sidebar: every cloud account SAND is wired into, whether it is answering,
   and how much of the vault it is carrying. Below the two-pane breakpoint the
   same panel becomes a drawer over the file browser — the file list is what you
   came for on a phone, and the accounts are a place you visit. */
export default function AccountsPanel({
  providers, loading, stats, webdav, mobile, open, onClose, onRefresh, onChanged,
}) {
  // A sign-in that took over the tab is still in flight when the app reloads:
  // reopen the dialog on it rather than making the user start again.
  const [connecting, setConnecting] = useState(() => Boolean(pendingOAuthFlow()))
  const [changingPassword, setChangingPassword] = useState(false)
  const [mounting, setMounting] = useState(false)
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
            providers={providers}
            isDefault={defaults.includes(provider.id)}
            onRemove={() => remove(provider)}
            onChanged={onChanged}
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

        {/* What the vault weighs, as two figures rather than four sentences.
            The numbers are the thing worth seeing from across the room; the
            labels under them are only there to say which number is which. */}
        {stats && (
          <div style={{ marginTop: '16px' }}>
            <div style={{ display: 'flex', gap: '18px' }}>
              <Figure value={stats.files} label={stats.files === 1 ? 'file' : 'files'} />
              <Figure value={formatBytes(stats.bytes)} label="in the vault" />
            </div>
            <div style={{
              marginTop: '8px',
              fontFamily: FONT.mono,
              fontSize: '10px',
              color: COLORS.textMuted,
              lineHeight: 1.7,
            }}>
              {formatBytes(stats.stored_bytes)} across accounts · {stats.policy}
            </div>
            {stats.degraded > 0 && (
              <div style={{
                marginTop: '6px',
                fontFamily: FONT.mono,
                fontSize: '10px',
                color: COLORS.warn,
                lineHeight: 1.7,
              }}>{stats.degraded} file{stats.degraded === 1 ? '' : 's'} missing a spare part</div>
            )}
          </div>
        )}

        {stats?.pending_migration > 0 && (
          <PendingMigration
            count={stats.pending_migration}
            onDone={onChanged}
            onError={setError}
          />
        )}

        {/* The three things you do to the vault itself rather than to an
            account. They were ghost buttons in a column, which read as three
            more lines of the grey text above them — nothing said they could be
            pressed. As tiles they group, they say what they are for on a second
            line, and they are a fingertip tall. */}
        <div style={{
          marginTop: '16px',
          display: 'flex',
          flexWrap: 'wrap',
          gap: '8px',
        }}>
          {/* Absent unless the server was started with --webdav: telling
              someone to mount a share that is not being served is worse than
              not mentioning it. */}
          {webdav?.path && (
            <ActionTile
              icon="💾"
              label="Mount"
              hint="as a drive"
              onClick={() => setMounting(true)}
            />
          )}
          {providers.length > 0 && (
            <ActionTile
              icon="☁️"
              label="Defaults"
              hint={defaults.length > 0
                ? `${defaults.length} of ${providers.length} clouds`
                : `${PARTS_PER_FILE} per upload`}
              onClick={() => setChoosingDefaults(true)}
            />
          )}
          <ActionTile
            icon="🔑"
            label="Password"
            hint="change it"
            onClick={() => setChangingPassword(true)}
          />
        </div>

        {/* Turned out of the header on a phone, it lands here. */}
        {mobile && (
          <div style={{ display: 'flex', justifyContent: 'center', marginTop: '18px' }}>
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

      {mounting && (
        <MountDrive path={webdav?.path} onClose={() => setMounting(false)} />
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

function AccountCard({ provider, providers, isDefault, onRemove, onChanged }) {
  const [testing, setTesting] = useState(false)
  const [result, setResult] = useState(null)
  const [editing, setEditing] = useState(false)
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

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px', marginTop: '8px' }}>
        <Button size="sm" variant="ghost" onClick={() => setEditing(true)}>Edit</Button>
        <Button size="sm" variant="ghost" onClick={test} disabled={testing}>
          {testing ? <Spinner size={10} /> : null}{testing ? 'Testing' : 'Test'}
        </Button>
        <Button size="sm" variant="ghost" onClick={onRemove}
          style={{ color: COLORS.error }}>Disconnect</Button>
      </div>

      {editing && (
        <EditAccount
          provider={provider}
          providers={providers}
          onClose={() => setEditing(false)}
          onChanged={onChanged}
        />
      )}
    </div>
  )
}
