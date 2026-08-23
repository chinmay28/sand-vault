import React, { useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Shards of your files, sitting on a cloud with nothing pointing at them.

   This is the good half of what a listing turns up, and the opposite of a
   sweep. Disconnecting a cloud drops the index records naming it — an index
   that still claimed them would be lying about what can be retrieved — and
   leaves the objects alone, because SAND has no business deleting from an
   account it is being told to stop using. Reconnect that storage and the two
   facts never meet again on their own: the account arrives with a new internal
   id, and finishing a recovery re-points records rather than inventing them, so
   there is nothing left to re-point. The file goes on reporting a missing spare
   part while the part sits on a connected cloud.

   Putting the record back moves no data at all, which is the thing worth
   saying loudest: a part's object key is derived from the archive id and the
   shard number, so the object is already exactly where the record says it is.
   That is why this has no per-row opt-out the way the sweep does — there is
   nothing to weigh up. It can only make files more recoverable than they are. */
export default function ReattachShards({ scan, zIndex = 100, onClose, onDone }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const rows = (scan?.strays || []).filter((row) => row.reattachable)
  const withheld = (scan?.strays || []).filter((row) => !row.reattachable)
  const bytes = rows.reduce((sum, row) => sum + (row.bytes || 0), 0)

  const run = async () => {
    setError(null)
    setBusy(true)
    try {
      setReport(await api.reattachShards())
      onDone?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Back where they belong" onClose={onClose} width={560} zIndex={zIndex}>
        <Banner tone="success">
          {report.shards} shard{report.shards === 1 ? '' : 's'} recorded again across{' '}
          {report.files} file{report.files === 1 ? '' : 's'}. No data was transferred —
          the parts were already on your clouds; only the index had forgotten them.
        </Banner>

        {report.restored?.length > 0 && (
          <>
            <SectionLabel>Back to full spread</SectionLabel>
            <div style={listStyle}>
              {report.restored.map((path) => (
                <div key={path} style={{ ...rowStyle, fontSize: '12px', color: COLORS.text }}>{path}</div>
              ))}
            </div>
          </>
        )}

        <Banner tone="info">
          Nothing here checked what the parts contain, any more than finishing a recovery does.
          Run a health check from a file&apos;s menu if you want the clouds asked.
        </Banner>

        {report.skipped?.length > 0 && (
          <Banner tone="warn">
            <span style={preText}>{report.skipped.join('\n')}</span>
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
      title="Parts your files have lost track of"
      subtitle="Still on your clouds, still readable — the index just stopped naming them."
      onClose={busy ? undefined : onClose}
      width={600}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={paragraph}>
        Disconnecting a cloud drops the records that named it, so the vault never claims to reach
        something it cannot. The parts themselves stay on the account — SAND does not delete from a
        cloud you are telling it to stop using. Connect that storage back and it arrives as a new
        account, with no way of knowing it is the one those records pointed at.
      </p>
      <p style={paragraph}>
        So the files below are short of a spare part while the spare sits on a cloud you are
        connected to. Recording them again <strong>moves no data at all</strong>: a part is stored
        under a name derived from the file it belongs to, so it is already exactly where the record
        would say it is.
      </p>

      <SectionLabel>
        {rows.length} shard{rows.length === 1 ? '' : 's'} · {scan?.stray_files || 0} file
        {(scan?.stray_files || 0) === 1 ? '' : 's'} · {formatBytes(bytes)}
        {scan?.strays_truncated > 0 ? ` (${scan.strays_truncated} more not listed)` : ''}
      </SectionLabel>
      <div style={listStyle}>
        {rows.map((row) => <StrayRow key={`${row.provider_id}:${row.archive_id}:${row.part}`} row={row} />)}
      </div>

      {withheld.length > 0 && (
        <>
          <SectionLabel>Left as they are</SectionLabel>
          <div style={listStyle}>
            {withheld.map((row) => (
              <StrayRow key={`${row.provider_id}:${row.archive_id}:${row.part}`} row={row} muted />
            ))}
          </div>
        </>
      )}

      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={onClose} disabled={busy}>Not now</Button>
        <Button variant="primary" onClick={run} disabled={busy || rows.length === 0}>
          {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
          {busy ? 'Recording…' : `Put ${rows.length} shard${rows.length === 1 ? '' : 's'} back`}
        </Button>
      </div>
    </Modal>
  )
}

function StrayRow({ row, muted }) {
  return (
    <div style={{ ...rowStyle, opacity: muted ? 0.6 : 1 }}>
      <span style={{
        width: '3px',
        alignSelf: 'stretch',
        borderRadius: '2px',
        background: accountColor(row.provider_id),
        flexShrink: 0,
      }} />
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{
          fontSize: '12px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>
          {/* A sub vault tells the main password its weight and nothing about
              what is inside it, so a file in one is named by its archive. */}
          {row.path || `a file in another vault · ${row.archive_id}`}
        </div>
        <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          part {row.part} on {row.provider_name} · {formatBytes(row.bytes)}
          {row.want ? ` · ${row.have} of ${row.want} parts recorded` : ''}
          {row.reason ? ` · ${row.reason}` : ''}
        </div>
      </div>
    </div>
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
