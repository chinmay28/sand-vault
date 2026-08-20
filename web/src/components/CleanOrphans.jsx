import React, { useMemo, useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Parts left on a cloud that no file points at any more.

   They come from a delete that could not finish. Erasing a file erases its
   parts from the accounts holding them — the ones connected at the time. A
   cloud that is disconnected while files are deleted keeps its share of them,
   and connecting it back gives that account a brand new internal ID, so the
   vault has no way of knowing it is the account those parts were erased from.
   Nothing ever goes back for them: unreadable, unreferenced, and still counting
   against the quota.

   This is the panel that says so. It is deliberately per account and per
   archive rather than one number with one button, because the only honest thing
   that can be said about an abandoned archive is how big it is and where it is
   — what it used to be lived in the index that stopped mentioning it. So the
   figure somebody is agreeing to is the room they get back, and the rows are
   there to be un-ticked by anyone who would rather not.

   Everything the scan refuses to offer is shown all the same, greyed out and
   with its reason beside it. An account another vault has been writing to is
   the case that matters: its parts look exactly like orphans and are not. */
export default function CleanOrphans({ scan: initialScan, onClose, onSwept }) {
  const [scan, setScan] = useState(initialScan)
  const [excluded, setExcluded] = useState(() => new Set())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const offered = useMemo(() => (scan?.items || []).filter((item) => item.deletable), [scan])
  const withheld = useMemo(() => (scan?.items || []).filter((item) => !item.deletable), [scan])
  const key = (item) => `${item.provider_id}:${item.archive_id}`

  const chosen = offered.filter((item) => !excluded.has(key(item)))
  const chosenBytes = chosen.reduce((sum, item) => sum + (item.bytes || 0), 0)
  const chosenObjects = chosen.reduce((sum, item) => sum + (item.objects || 0), 0)

  const toggle = (item) => {
    setExcluded((current) => {
      const next = new Set(current)
      if (next.has(key(item))) next.delete(key(item))
      else next.add(key(item))
      return next
    })
  }

  const sweep = async () => {
    setError(null)
    setBusy(true)
    try {
      /* Nothing unticked means "all of it", and is sent as an empty list rather
         than as the rows on screen — the list is capped for reading, and naming
         only what fitted would quietly leave the rest behind. Untick anything
         and the request becomes exactly what is ticked. */
      const targets = excluded.size === 0
        ? []
        : chosen.map((item) => ({ provider_id: item.provider_id, archive_id: item.archive_id }))
      const result = await api.sweepOrphans({ targets })
      setReport(result)
      onSwept?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const rescan = async () => {
    setError(null)
    setBusy(true)
    try {
      const fresh = await api.orphanScan()
      setScan(fresh)
      setExcluded(new Set())
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Swept" onClose={onClose} width={560}>
        <Banner tone="success">
          {report.deleted} object{report.deleted === 1 ? '' : 's'} erased across{' '}
          {report.archives} archive{report.archives === 1 ? '' : 's'}, freeing{' '}
          {formatBytes(report.bytes)} on your clouds.
        </Banner>

        {report.skipped?.length > 0 && (
          <Banner tone="info">
            Some of what was named had stopped being abandoned by the time the sweep ran, and was
            left where it is:
            <span style={preText}>{report.skipped.join('\n')}</span>
          </Banner>
        )}
        {report.warnings?.length > 0 && (
          <Banner tone="warn">
            <span style={preText}>{report.warnings.join('\n')}</span>
          </Banner>
        )}

        <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button variant="primary" onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  return (
    <Modal
      title="Parts nothing points at"
      subtitle="Storage your clouds are holding for files this vault no longer has."
      onClose={busy ? undefined : onClose}
      width={620}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={paragraph}>
        Deleting a file erases its parts from the clouds holding them — the ones connected at the
        time. Disconnect a cloud, delete files without it, and its share of them stays put; connect
        it back and it arrives as a new account, so nothing ever goes looking again. These are what
        was left: encrypted, unreadable, referred to by nothing, and still taking up room.
      </p>

      <AccountBreakdown accounts={scan?.accounts || []} />

      {scan?.blocked?.length > 0 && (
        <Banner tone="warn">
          Nothing here is being offered for deletion:
          <span style={preText}>{scan.blocked.map((reason) => `• ${reason}`).join('\n')}</span>
        </Banner>
      )}

      {offered.length > 0 && (
        <>
          <SectionLabel>
            {offered.length} abandoned archive{offered.length === 1 ? '' : 's'}
            {scan?.items_truncated > 0 ? ` (${scan.items_truncated} more not listed)` : ''}
          </SectionLabel>
          <p style={{ ...paragraph, marginBottom: '10px' }}>
            One row per archive that was written and forgotten. There is nothing to say about what
            each one was — the file name, its folder and its date all lived in the index that
            stopped mentioning it. Untick anything you would rather keep.
          </p>
          <div style={listStyle}>
            {offered.map((item) => (
              <ArchiveRow
                key={key(item)}
                item={item}
                checked={!excluded.has(key(item))}
                onToggle={() => toggle(item)}
                disabled={busy}
              />
            ))}
          </div>
        </>
      )}

      {withheld.length > 0 && (
        <>
          <SectionLabel>Left alone</SectionLabel>
          <div style={listStyle}>
            {withheld.map((item) => (
              <ArchiveRow key={key(item)} item={item} checked={false} disabled />
            ))}
          </div>
        </>
      )}

      {scan?.warnings?.length > 0 && (
        <Banner tone="warn">
          <span style={preText}>{scan.warnings.join('\n')}</span>
        </Banner>
      )}

      {scan?.items_truncated > 0 && (
        <Banner tone="info">
          {scan.items_truncated} further archive{scan.items_truncated === 1 ? '' : 's'} were found
          and are not listed above — the list is capped for reading, not for counting.
          {excluded.size === 0
            ? ' Sweeping takes those too.'
            : ' Because you have unticked something, only the ticked rows above will be swept.'}
        </Banner>
      )}

      <div style={{ display: 'flex', justifyContent: 'space-between', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={rescan} disabled={busy}>Scan again</Button>
        <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>Not now</Button>
          <Button
            variant="primary"
            onClick={sweep}
            disabled={busy || chosen.length === 0 || scan?.blocked?.length > 0}
          >
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy
              ? 'Erasing…'
              : `Erase ${chosenObjects} object${chosenObjects === 1 ? '' : 's'} · ${formatBytes(chosenBytes)}`}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

/* What each account is carrying, and how much of it is dead weight. The
   denominator matters as much as the figure: 40 MB abandoned means one thing on
   an account holding 60 MB and another on one holding 400 GB. */
function AccountBreakdown({ accounts }) {
  const carrying = accounts.filter((account) => account.orphans > 0 || account.error)
  if (carrying.length === 0) return null

  return (
    <div style={{ ...listStyle, marginBottom: '16px' }}>
      {carrying.map((account) => (
        <div key={account.provider_id} style={rowStyle}>
          <span style={{
            width: '3px',
            alignSelf: 'stretch',
            borderRadius: '2px',
            background: accountColor(account.provider_id),
            flexShrink: 0,
          }} />
          <div style={{ minWidth: 0, flex: 1 }}>
            <div style={{ fontSize: '12px', color: COLORS.text }}>{account.name}</div>
            <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
              {account.error
                ? `could not be listed — ${account.error}`
                : `${account.orphans} of ${account.objects} part objects, `
                  + `${formatBytes(account.orphan_bytes)} of ${formatBytes(account.bytes)}`}
            </div>
          </div>
          {account.foreign && (
            <span style={tagStyle}>another vault&apos;s</span>
          )}
        </div>
      ))}
    </div>
  )
}

function ArchiveRow({ item, checked, onToggle, disabled }) {
  return (
    <label style={{ ...rowStyle, cursor: disabled ? 'default' : 'pointer', opacity: disabled ? 0.6 : 1 }}>
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        disabled={disabled}
        style={{ accentColor: COLORS.accent, flexShrink: 0 }}
      />
      <span style={{
        width: '3px',
        alignSelf: 'stretch',
        borderRadius: '2px',
        background: accountColor(item.provider_id),
        flexShrink: 0,
      }} />
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '11px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{item.archive_id}</div>
        <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {item.provider_name} · {item.objects} object{item.objects === 1 ? '' : 's'} ·{' '}
          {formatBytes(item.bytes)}
          {item.reason ? ` · ${item.reason}` : ''}
        </div>
      </div>
    </label>
  )
}

function SectionLabel({ children }) {
  return (
    <div style={{
      fontFamily: FONT.mono,
      fontSize: '9px',
      fontWeight: 700,
      letterSpacing: '1.2px',
      textTransform: 'uppercase',
      color: COLORS.textMuted,
      marginBottom: '6px',
    }}>{children}</div>
  )
}

const paragraph = {
  margin: '0 0 16px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}

const listStyle = {
  display: 'flex',
  flexDirection: 'column',
  gap: '4px',
  maxHeight: '240px',
  overflowY: 'auto',
  marginBottom: '16px',
}

const rowStyle = {
  display: 'flex',
  alignItems: 'center',
  gap: '10px',
  padding: '8px 10px',
  borderRadius: '6px',
  background: COLORS.surface,
  border: `1px solid ${COLORS.border}`,
}

const tagStyle = {
  fontFamily: FONT.mono,
  fontSize: '9px',
  letterSpacing: '0.6px',
  textTransform: 'uppercase',
  color: COLORS.textMuted,
  border: `1px solid ${COLORS.border}`,
  borderRadius: '4px',
  padding: '2px 6px',
  flexShrink: 0,
}

const preText = {
  display: 'block',
  marginTop: '6px',
  fontFamily: FONT.mono,
  fontSize: '11px',
  whiteSpace: 'pre-wrap',
}
