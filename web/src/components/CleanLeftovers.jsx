import React, { useMemo, useState } from 'react'
import { COLORS, FONT, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Working files SAND left in its own folder, and the room they are taking.

   The other two panels are about clouds. This one is about the disk the vault
   file itself is on — /var/lib/sand on a server, ~/.sand on a desktop — and it
   is here because the same sentence is true of it: something is being held that
   no file in this vault needs.

   The one that matters is the upload spool. A stream cannot say its own hash
   until its last byte and every chunk of a stored file has to carry that hash,
   so an upload arriving over the network is written to disk in full before a
   byte of it is sent. That file is removed on every path out of an upload
   including failure — but the process being killed is not a path out of it, and
   neither is the power going. What is left behind is the whole file that was
   being uploaded, at full size, in a folder nobody thinks to look in. Four
   interrupted films is thirty gigabytes of a disk that was probably chosen for
   being small.

   Rows rather than one number and one button, for the reason the sweep panel
   gives rows: these are named files on a disk somebody can go and look at, and
   the honest thing is to say which ones and how big rather than to ask for
   agreement to a total. Anything the scan will not offer is shown all the same,
   greyed out with its reason — a spool that was written to in the last hour is
   the case that matters, because that is what an upload running right now in
   another window looks like. */
export default function CleanLeftovers({ scan: initialScan, zIndex = 100, onClose, onSwept }) {
  const [scan, setScan] = useState(initialScan)
  const [excluded, setExcluded] = useState(() => new Set())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const offered = useMemo(() => (scan?.items || []).filter((item) => item.deletable), [scan])
  const withheld = useMemo(() => (scan?.items || []).filter((item) => !item.deletable), [scan])

  const chosen = offered.filter((item) => !excluded.has(item.name))
  const chosenBytes = chosen.reduce((sum, item) => sum + (item.bytes || 0), 0)

  const toggle = (item) => {
    setExcluded((current) => {
      const next = new Set(current)
      if (next.has(item.name)) next.delete(item.name)
      else next.add(item.name)
      return next
    })
  }

  const sweep = async () => {
    setError(null)
    setBusy(true)
    try {
      /* Nothing unticked means "all of it", sent as an empty list rather than
         as the rows on screen — the list is capped for reading, and naming only
         what fitted would quietly leave the rest behind. */
      const names = excluded.size === 0 ? [] : chosen.map((item) => item.name)
      const result = await api.sweepLeftovers({ names })
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
      setScan(fresh?.leftovers || null)
      setExcluded(new Set())
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Tidied" onClose={onClose} width={560} zIndex={zIndex}>
        <Banner tone="success">
          {report.deleted} working file{report.deleted === 1 ? '' : 's'} erased, freeing{' '}
          {formatBytes(report.bytes)} on this machine.
        </Banner>

        {report.skipped?.length > 0 && (
          <Banner tone="info">
            Something had started writing to these again by the time the sweep ran, so they were
            left where they are:
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
      title="Working files left behind"
      subtitle={scan?.dir ? `In ${scan.dir}, on this machine` : 'In this vault’s own folder'}
      onClose={busy ? undefined : onClose}
      width={620}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={paragraph}>
        An upload is written to disk in full before any of it is sent, because every chunk of a
        stored file has to carry the whole file’s hash and a stream only gives that up at its last
        byte. The copy is deleted the moment the upload ends, however it ends — but a process that
        is killed, or a machine that loses power, never gets to that line. What is left is the file
        that was being uploaded, at full size, in the folder your vault lives in. Nothing goes back
        for it on its own.
      </p>

      {offered.length > 0 && (
        <>
          <SectionLabel>
            {offered.length} file{offered.length === 1 ? '' : 's'} nothing is using
            {scan?.items_truncated > 0 ? ` (${scan.items_truncated} more not listed)` : ''}
          </SectionLabel>
          <p style={{ ...paragraph, marginBottom: '10px' }}>
            Erasing these frees room on this machine and touches nothing in your vault: they are
            SAND’s own scratch copies, not your files. Untick anything you would rather keep.
          </p>
          <div style={listStyle}>
            {offered.map((item) => (
              <LeftoverRow
                key={item.name}
                item={item}
                checked={!excluded.has(item.name)}
                onToggle={() => toggle(item)}
                disabled={busy}
              />
            ))}
          </div>
        </>
      )}

      {withheld.length > 0 && (
        <>
          <SectionLabel>Left alone for now</SectionLabel>
          <p style={{ ...paragraph, marginBottom: '10px' }}>
            Something wrote to these recently, which is what an upload that is still running looks
            like from the outside. They are offered once they have been quiet for an hour.
          </p>
          <div style={listStyle}>
            {withheld.map((item) => (
              <LeftoverRow key={item.name} item={item} checked={false} disabled />
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
          {scan.items_truncated} further file{scan.items_truncated === 1 ? '' : 's'} were found and
          are not listed above — the list is capped for reading, not for counting.
          {excluded.size === 0
            ? ' Erasing takes those too.'
            : ' Because you have unticked something, only the ticked rows above will go.'}
        </Banner>
      )}

      <div style={{ display: 'flex', justifyContent: 'space-between', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={rescan} disabled={busy}>Scan again</Button>
        <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>Not now</Button>
          <Button variant="primary" onClick={sweep} disabled={busy || chosen.length === 0}>
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy
              ? 'Erasing…'
              : `Erase ${chosen.length} file${chosen.length === 1 ? '' : 's'} · ${formatBytes(chosenBytes)}`}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

/* One file, with the two things worth knowing about it: what it was for, and
   when anything last wrote to it. The second is the one that answers "is this
   really finished with" — an upload in progress is being written to constantly,
   and a spool nothing has touched since Tuesday is not an upload in progress. */
function LeftoverRow({ item, checked, onToggle, disabled }) {
  return (
    <label style={{ ...rowStyle, cursor: disabled ? 'default' : 'pointer', opacity: disabled ? 0.6 : 1 }}>
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        disabled={disabled}
        style={{ accentColor: COLORS.accent, flexShrink: 0 }}
      />
      <span aria-hidden="true" style={{ fontSize: '14px', flexShrink: 0 }}>
        {item.dir ? '📁' : '📄'}
      </span>
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '11px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{item.name}</div>
        <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {formatBytes(item.bytes)} · last written {formatDate(item.modified)}
        </div>
        <div style={{ fontFamily: FONT.sans, fontSize: '10px', color: COLORS.textMuted }}>
          {item.reason || item.what}
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

const preText = {
  display: 'block',
  marginTop: '6px',
  fontFamily: FONT.mono,
  fontSize: '11px',
  whiteSpace: 'pre-wrap',
}
