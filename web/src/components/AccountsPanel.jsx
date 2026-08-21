import React, { useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { pendingOAuthFlow } from '../oauth'
import { Banner, Button, IconButton, Spinner } from './ui'
import CloudStats, { UsageBar, UsageLine, usageBreakdown } from './CloudStats'
import ConnectCloud from './ConnectCloud'
import EditAccount from './EditAccount'
import { DisconnectIcon, EditIcon, StatsIcon, TestIcon } from './Icons'
import MissingParts from './MissingParts'
import ReadStats from './ReadStats'
import ReclaimVault from './ReclaimVault'
import VaultSettings from './VaultSettings'
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

/* The count of files missing a part, as the way into them.

   It reads as the sentence it always was — the underline and the ▸ are the
   only marks of a control on it — because the line has to keep working as a
   read-out for the people who never click it. Warn-coloured like the text it
   replaces, and lit rather than recoloured on hover, so nothing about the
   figure changes meaning when a pointer happens to rest on it. */
function DegradedLink({ count, onClick }) {
  const [hover, setHover] = useState(false)

  return (
    <button
      type="button"
      onClick={onClick}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      onFocus={() => setHover(true)}
      onBlur={() => setHover(false)}
      title="See which files, and choose other clouds for them"
      style={{
        display: 'block',
        width: '100%',
        marginTop: '6px',
        padding: 0,
        background: 'none',
        border: 'none',
        textAlign: 'left',
        cursor: 'pointer',
        fontFamily: FONT.mono,
        fontSize: '10px',
        color: COLORS.warn,
        lineHeight: 1.7,
        textDecoration: 'underline',
        textDecorationStyle: 'dotted',
        textUnderlineOffset: '3px',
        filter: hover ? 'brightness(1.25)' : 'none',
        transition: 'filter 0.15s ease',
      }}
    >{count} file{count === 1 ? '' : 's'} missing a spare part <span aria-hidden="true">▸</span></button>
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
  providers, loading, status, stats, webdav, mobile, open,
  subVaults = [], showSubVaults, subVaultShown, onToggleSubVaults, onToggleSubVault, onOpenSubVault,
  onClose, onRefresh, onChanged,
}) {
  // A sign-in that took over the tab is still in flight when the app reloads:
  // reopen the dialog on it rather than making the user start again. Which
  // dialog depends on what the sign-in was for — a new account goes back to the
  // connect one, and an account signing back in goes back to its own edit
  // dialog, where the tokens replace the ones it already has.
  const [pending] = useState(pendingOAuthFlow)
  const [connecting, setConnecting] = useState(() => Boolean(pending && !pending.provider_id))
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [readsOpen, setReadsOpen] = useState(false)
  const [missingOpen, setMissingOpen] = useState(false)
  const [reclaiming, setReclaiming] = useState(false)
  const [error, setError] = useState(null)

  const defaults = stats?.default_accounts || []

  useEffect(() => {
    if (!mobile || !open) return
    const onKey = (e) => { if (e.key === 'Escape') onClose?.() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [mobile, open, onClose])

  /* Disconnecting is not quite as final as it reads, and saying so is the
     difference between somebody thinking they have lost redundancy and knowing
     how to get it back. The records naming this account go — an index still
     claiming them would be lying about what can be reached — but the parts
     themselves stay put, and connecting the same storage again is enough to
     re-record them without moving a byte. See the reattach panel. */
  const remove = async (provider) => {
    const confirmed = window.confirm(
      `Disconnect "${provider.name}"?\n\n` +
      `SAND will forget the ${provider.shards} part(s) it holds, so any file that had one ` +
      `will show a missing spare part.\n\n` +
      `The parts themselves stay on the account. Connect this same storage again and SAND ` +
      `offers to put those records back — no data is moved. Delete them at the provider if ` +
      `you want the room instead.`
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
            resuming={pending?.provider_id === provider.id}
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
            {/* The one figure down here that names something to do rather than
                something to know: a file short a part stays short until
                somebody sends it somewhere else. So it is a button, and the
                list behind it is the list of files it has been counting all
                along — with the clouds each of them is on, and the choice of
                different ones, on the row. */}
            {stats.degraded > 0 && (
              <DegradedLink count={stats.degraded} onClick={() => setMissingOpen(true)} />
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

        {/* A recovery adopts the key of the vault it rebuilt, so until these
            files are re-encrypted the old password still opens their parts.
            Said here rather than only at the end of the recovery: the transfer
            is the whole vault twice over, and the moment to do it is rarely the
            moment the recovery finishes. */}
        {stats?.inherited_key && (
          <Banner tone="warn">
            This vault is still using the key it recovered, so the lost vault&apos;s password
            opens every part on your clouds — including anything you upload until you change it.
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setReclaiming(true)}
              style={{ marginTop: '8px' }}
            >Re-encrypt under your key</Button>
          </Banner>
        )}

        {/* One door to everything the vault itself is set to.

            This was a row of tiles, one per setting, which worked at two and
            stopped working at four: the drawer is where your accounts live, and
            a grid of little boxes for the password, the default clouds, the
            film key and the drive was eating the bottom half of it — while
            still only offering whichever settings happened to fit. A list
            behind one button holds as many as the vault grows, and gives each
            of them room to say what it is currently set to. */}
        {/* Two tiles, half the row each: what the vault is set to, and what the
            clouds have been doing with it. The second is not a setting — it
            changes nothing — but it is the other question somebody standing in
            front of their accounts asks, and it is about all of them at once
            rather than about the one card they happen to be looking at. */}
        <div style={{ marginTop: '16px', display: 'flex', gap: '8px' }}>
          <ActionTile
            icon="⚙️"
            label="Vault settings"
            /* Three of the list's items, and the recovery kit is one of them
               because it is the one somebody has to be told exists. Nobody
               goes looking for a backup they have never heard of. */
            hint="password · clouds · recovery kit"
            onClick={() => setSettingsOpen(true)}
          />
          <ActionTile
            icon="⏱️"
            label="Read speed"
            hint="who answers when a file is rebuilt"
            onClick={() => setReadsOpen(true)}
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

      {readsOpen && <ReadStats onClose={() => setReadsOpen(false)} />}

      {missingOpen && (
        <MissingParts
          providers={providers}
          onClose={() => setMissingOpen(false)}
          onChanged={onChanged}
        />
      )}

      {settingsOpen && (
        <VaultSettings
          providers={providers}
          stats={stats}
          webdav={webdav}
          subVaults={subVaults}
          showSubVaults={showSubVaults}
          subVaultShown={subVaultShown}
          onToggleSubVaults={onToggleSubVaults}
          onToggleSubVault={onToggleSubVault}
          onOpenSubVault={onOpenSubVault}
          onClose={() => setSettingsOpen(false)}
          onChanged={onChanged}
        />
      )}

      {reclaiming && (
        <ReclaimVault
          stats={stats}
          providers={providers}
          onClose={() => setReclaiming(false)}
          onDone={onChanged}
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

function AccountCard({ provider, providers, isDefault, resuming, onRemove, onChanged }) {
  const [testing, setTesting] = useState(false)
  const [result, setResult] = useState(null)
  // `resuming` is a sign-in this account started that took the whole tab over.
  // The dialog reopens on the connection tab, which is where it left off.
  const [editing, setEditing] = useState(Boolean(resuming))
  const [showing, setShowing] = useState(false)
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
  const space = usageBreakdown(provider)

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
        {/* How full the account is, and how much of that is ours. A local drive
            answers this as well as a cloud does now — the disk it sits on is
            shared with everything else on the machine, and a folder holding
            34 GB of parts says nothing about whether there is room for more. */}
        {space.known && (
          <div style={{ margin: '5px 0 4px' }}>
            <UsageBar provider={provider} />
          </div>
        )}
        <UsageLine provider={provider} />
        {!online && errorText && (
          <div style={{ color: COLORS.error, wordBreak: 'break-word' }}>{errorText}</div>
        )}
      </div>

      {/* Four words in a row, all of them bare text, one of them destructive:
          the only thing separating "Test" from "Disconnect" was a gap the width
          of a space. They are real buttons now, sharing the card's width — but
          the drawer is only 286px wide, and four labels with an icon beside
          each would need half again as much. So the icon sits above its word
          instead: the tile ends up narrower than the label-and-icon row it
          replaces, and taller, which is the shape a fingertip wants anyway.

          Disconnect keeps the red outline the app uses everywhere for a step
          that cannot be taken back, and stands off from the other three by a
          wider gap than they leave between themselves. */}
      <div style={{ display: 'flex', gap: '5px', marginTop: '10px' }}>
        {[
          { key: 'stats', label: 'Stats', icon: <StatsIcon size={15} />, onClick: () => setShowing(true) },
          { key: 'edit', label: 'Edit', icon: <EditIcon size={15} />, onClick: () => setEditing(true) },
          {
            key: 'test',
            label: testing ? 'Testing' : 'Test',
            icon: testing ? <Spinner size={13} /> : <TestIcon size={15} />,
            onClick: test,
            disabled: testing,
          },
          {
            key: 'disconnect',
            label: 'Disconnect',
            icon: <DisconnectIcon size={15} />,
            onClick: onRemove,
            variant: 'danger',
            apart: true,
          },
        ].map((action) => (
          <Button
            key={action.key}
            size="sm"
            variant={action.variant}
            onClick={action.onClick}
            disabled={action.disabled}
            style={{
              /* Grow to fill the row, never shrink below the word inside —
                 "Disconnect" is twice the length of "Edit" and takes the width
                 it needs, rather than every tile being cut to its size. */
              flex: '1 0 auto',
              flexDirection: 'column',
              justifyContent: 'center',
              gap: '4px',
              padding: '7px 5px',
              fontSize: '9px',
              letterSpacing: '0.4px',
              marginLeft: action.apart ? '5px' : undefined,
            }}
          >{action.icon}{action.label}</Button>
        ))}
      </div>

      {showing && (
        <CloudStats provider={provider} onClose={() => setShowing(false)} onChanged={onChanged} />
      )}

      {editing && (
        <EditAccount
          provider={provider}
          providers={providers}
          initialTab={resuming ? 'connection' : 'look'}
          onClose={() => setEditing(false)}
          onChanged={onChanged}
        />
      )}
    </div>
  )
}
