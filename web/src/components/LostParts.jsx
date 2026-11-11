import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { useLostRestore, useLostScan } from '../hooks'
import { claimRun } from '../runClaims'
import { Banner, Button, Modal, Spinner } from './ui'
import ScanProgress, { describeProgress } from './ScanProgress'

/* Parts a cloud should be holding, and is not.

   The stray-parts scan asks each cloud what it holds and subtracts what the
   index accounts for. This is that listing read the other way: what the index
   accounts for, less what each cloud holds. The remainder is parts that were
   written and recorded and have since gone from under SAND — a folder emptied
   by hand, a bucket restored from last month's backup, a sync client that
   tidied the files away.

   Nothing else says so. The file's badges still show a full set, and it still
   reads back — on its spares — until a second cloud is unavailable. So the
   scan is here to be asked for, and the app's own listing of the clouds says
   so in a banner when it happens to turn any up (see App).

   Putting one back is not a rebuild of the file. The lost object alone is cut
   again from the file's other parts and written to the cloud the index
   already records it on, under the name it had — so the index is not touched,
   nothing is erased, and what travels is the parts a read needs, down, and the
   one lost object, up.

   The one thing a listing cannot tell apart is a cloud that has been emptied
   and a cloud that is pointed at the wrong folder: both hold none of what is
   recorded there. That is said where it applies, and it is why each cloud can
   be left out of the restore.

   Both halves run on the server (see useServerJob), so the dialog can be
   closed over either; the strip above the file list carries a restore from
   there (BackgroundRuns). */

/* How long a finished scan is taken as the answer when the dialog opens.
   Short, and much shorter than the stray-parts panel allows itself: somebody
   opens this having just done something to a cloud's folder, and "nothing is
   lost", true ten minutes ago, is the one answer that must not be shown to
   them. Long enough only that closing the dialog as a scan lands and opening
   it again does not list every cloud twice. */
const LOST_SCAN_FRESH_MS = 60 * 1000

export default function LostParts({ zIndex = 100, onClose, onChanged }) {
  const { status: scanStatus, error: scanAskError, start: startScan } = useLostScan()
  const { status: restoreStatus, error: restoreAskError, start: startRestore, refresh: refreshRestore } = useLostRestore()
  const [starting, setStarting] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [skipped, setSkipped] = useState(() => new Set())

  /* Opening is "what is true?", not "scan": a scan still going is watched, a
     recent one is shown, and a fresh one is started only when there is
     neither. Decided once, and not while a restore is going — that lists the
     clouds itself, and its report is the news. */
  const decided = useRef(false)
  useEffect(() => {
    if (decided.current || scanStatus === null || restoreStatus === null) return
    decided.current = true
    if (scanStatus.running || restoreStatus.running) return
    const finished = scanStatus.finished_at ? Date.parse(scanStatus.finished_at) : NaN
    const fresh = scanStatus.result && Number.isFinite(finished) && Date.now() - finished < LOST_SCAN_FRESH_MS
    if (!fresh) startScan()
  }, [scanStatus, restoreStatus, startScan])

  /* The restore this dialog is drawing: any found running, including the one
     its own button started. Claimed while drawn, so the strip leaves it. */
  const [watching, setWatching] = useState(null)
  useEffect(() => {
    if (restoreStatus?.running && restoreStatus.id !== watching) setWatching(restoreStatus.id)
  }, [restoreStatus, watching])
  useEffect(() => claimRun('lost-restore', watching), [watching])

  const watched = watching && restoreStatus?.id === watching ? restoreStatus : null
  const restoring = !!watched?.running
  const report = watched && !restoring ? watched.result : null
  const restoreError = restoreAskError || (watched && !restoring ? watched.error : null)

  /* What the clouds hold changed, once, when the run this dialog drew landed. */
  const told = useRef(null)
  useEffect(() => {
    if (!report || told.current === watching) return
    told.current = watching
    if (report.objects > 0) onChanged?.()
  }, [report, watching, onChanged])

  const scanning = scanStatus === null || !!scanStatus.running
  const scan = !scanning ? scanStatus?.result : null
  const scanError = scanAskError || (!scanning ? scanStatus?.error : null)

  const accounts = (scan?.accounts || []).filter((account) => account.parts > 0)
  const unheard = (scan?.accounts || []).filter((account) => account.error)
  const chosen = accounts.filter((account) => account.restorable > 0 && !skipped.has(account.provider_id))
  const chosenParts = chosen.reduce((sum, account) => sum + account.restorable, 0)
  const everyCloud = chosen.length === accounts.filter((account) => account.restorable > 0).length
  const emptied = chosen.filter((account) => account.emptied)

  const toggle = (id) => setSkipped((prev) => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  })

  const restore = async () => {
    setStarting(true)
    try {
      await startRestore({ providerIds: everyCloud ? [] : chosen.map((account) => account.provider_id) })
    } finally {
      setStarting(false)
    }
  }

  const stop = async () => {
    setStopping(true)
    try {
      await api.lostRestoreStop()
      await refreshRestore()
    } catch {
      // The poll says what became of it either way.
    } finally {
      setStopping(false)
    }
  }

  const again = () => {
    setWatching(null)
    setSkipped(new Set())
    startScan()
  }

  if (restoring) {
    return (
      <Modal
        title="Putting the lost parts back"
        subtitle="Each one is remade from the file's other parts and written where it was."
        onClose={onClose}
        width={600}
        zIndex={zIndex}
      >
        <ScanProgress
          status={watched}
          describe={describeRestore}
          note="You can close this, or the page: it carries on, and the strip above your files says when it lands. Stopping keeps everything already put back."
        />
        <div style={footer}>
          <Button variant="ghost" onClick={stop} disabled={stopping}>
            {stopping ? 'Stopping…' : 'Stop'}
          </Button>
          <Button variant="primary" onClick={onClose}>Close</Button>
        </div>
      </Modal>
    )
  }

  if (report) {
    const clean = report.failed === 0 && report.withheld === 0 && !report.stopped
    return (
      <Modal title="Lost parts" subtitle="What the restore came to" onClose={onClose} width={600} zIndex={zIndex}>
        <Banner tone={report.parts > 0 ? 'success' : 'info'}>{restoreOutcomeText(report)}</Banner>

        {report.stopped && (
          <Banner tone="warn">
            It ended early — {report.stopped_by || 'it was stopped'}. Everything it had put back is
            in place; scanning again shows what is left.
          </Banner>
        )}

        {report.failures?.length > 0 && (
          <>
            <SectionLabel>
              Not put back · {report.failed + report.withheld}
              {report.failures_truncated > 0 ? ` (${report.failures_truncated} more not listed)` : ''}
            </SectionLabel>
            <div style={listStyle}>
              {report.failures.map((row) => (
                <div key={`${row.file_id}:${row.part}:${row.provider_name}`} style={rowStyle}>
                  <div style={{ minWidth: 0, flex: 1 }}>
                    <div style={rowTitle}>{row.path || 'a file in a sub vault'}</div>
                    <div style={rowDetail}>part {row.part} on {row.provider_name} · {row.reason}</div>
                  </div>
                </div>
              ))}
            </div>
          </>
        )}

        {report.warnings?.length > 0 && (
          <Banner tone="warn"><span style={preText}>{report.warnings.join('\n')}</span></Banner>
        )}

        {clean && report.parts > 0 && (
          <Banner tone="info">
            The index was not changed: it was right about where these parts belong. Run
            &ldquo;Find and fix corrupt files&rdquo; on a folder if you want what the clouds hold read
            back and checked.
          </Banner>
        )}

        <div style={footer}>
          <Button variant="ghost" onClick={again}>Scan again</Button>
          <Button variant="primary" onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  return (
    <Modal
      title="Lost parts"
      subtitle="What the index says a cloud is holding, and the cloud is not"
      onClose={onClose}
      width={600}
      zIndex={zIndex}
    >
      {scanError && <Banner tone="error">{scanError}</Banner>}
      {restoreError && <Banner tone="error">{restoreError}</Banner>}

      {scanning && (
        <ScanProgress
          status={scanStatus}
          note="Each cloud is asked for a listing of what it holds, and nothing is downloaded — so this takes about as long as the slowest of them. You can close this: the scan carries on, and what it finds will be here when you come back."
        />
      )}

      {scan && !scan.found && (
        <Banner tone={unheard.length > 0 ? 'info' : 'success'}>
          {unheard.length > 0
            ? 'Every part recorded on the clouds that answered is there.'
            : 'Every part the index records is on the cloud it is recorded on.'}
        </Banner>
      )}

      {scan?.found && (
        <>
          <p style={paragraph}>
            {partsOf(scan.parts, scan.files)} {scan.parts === 1 ? 'is' : 'are'} not on the cloud the
            index records {scan.parts === 1 ? 'it' : 'them'} on — about {formatBytes(scan.bytes)}.
            The files still read, on their other parts, but each has less to lose than it shows.
          </p>
          <p style={paragraph}>
            Putting a part back remakes <strong>only what is missing</strong>, from the file&apos;s
            other parts, and writes it where the index already says it is. Nothing is erased or
            overwritten, and the index is not changed.
          </p>

          <SectionLabel>By cloud</SectionLabel>
          <div style={listStyle}>
            {accounts.map((account) => (
              <AccountRow
                key={account.provider_id}
                account={account}
                checked={!skipped.has(account.provider_id)}
                onToggle={() => toggle(account.provider_id)}
              />
            ))}
          </div>

          {emptied.length > 0 && (
            <Banner tone="warn">
              {emptied.map((account) => account.name).join(', ')}{' '}
              {emptied.length === 1 ? 'holds' : 'hold'} none of the parts recorded there. That is
              what an emptied folder looks like — and also what a cloud pointed at the wrong
              folder looks like. If the folder moved, fix the account instead; untick it here and
              nothing is written to it.
            </Banner>
          )}

          <SectionLabel>
            {scan.parts} part{scan.parts === 1 ? '' : 's'}
            {scan.items_truncated > 0 ? ` (${scan.items_truncated} more not listed)` : ''}
          </SectionLabel>
          <div style={listStyle}>
            {scan.items.map((row) => (
              <PartRow key={`${row.provider_id}:${row.file_id}:${row.part}`} row={row} />
            ))}
          </div>

          {scan.parts > scan.restorable && (
            <Banner tone="info">
              {scan.parts - scan.restorable} of these cannot be remade from the parts that are
              left; each says why. A file with a replica, or one too short to read, is rebuilt
              from &ldquo;Find and fix corrupt files&rdquo; or its own Clouds dialog instead.
            </Banner>
          )}
        </>
      )}

      {unheard.length > 0 && !scanning && (
        <Banner tone="warn">
          {unheard.map((account) => account.name).join(', ')}{' '}
          {unheard.length === 1 ? 'was' : 'were'} not asked, so nothing recorded there was checked:{' '}
          {unheard[0].error}
        </Banner>
      )}

      <div style={footer}>
        {!scanning && <Button variant="ghost" onClick={again} disabled={starting}>Scan again</Button>}
        {scan?.found && scan.restorable > 0 ? (
          <Button variant="primary" onClick={restore} disabled={starting || chosenParts === 0}>
            {starting ? <Spinner size={12} color={COLORS.bg} /> : null}
            {starting ? 'Starting…' : `Put ${chosenParts} part${chosenParts === 1 ? '' : 's'} back`}
          </Button>
        ) : (
          <Button variant="primary" onClick={onClose}>{scanning ? 'Close' : 'Done'}</Button>
        )}
      </div>
    </Modal>
  )
}

/* "3 parts of 2 files" */
function partsOf(parts, files) {
  return `${parts} part${parts === 1 ? '' : 's'} of ${files} file${files === 1 ? '' : 's'}`
}

/* The sentence a finished restore comes to — the dialog's banner, and the
   strip's line when the dialog was closed over it. */
export function restoreOutcomeText(report) {
  const parts = report?.parts || 0
  if (parts === 0) {
    if (report?.already_there > 0) {
      return 'Nothing needed writing — the clouds were holding every part once each was asked for by name.'
    }
    return 'No parts were put back.'
  }
  const where = report.on?.length > 0 ? ` on ${report.on.join(', ')}` : ''
  return `${partsOf(parts, report.files || 0)} put back${where} — `
    + `${report.objects} object${report.objects === 1 ? '' : 's'}, ${formatBytes(report.bytes || 0)} written.`
}

/* The bar for a running restore: the clouds being listed first, then objects
   put back against objects lost. */
export function describeRestore(status, elapsed) {
  const at = status.progress
  if (!at || at.stage !== 'restoring') {
    return describeProgress(status, at?.scan, elapsed, 'Asking the clouds what they are holding…')
  }
  if (at.objects === 0) {
    return { label: 'Nothing to put back.', detail: elapsed, fraction: 1 }
  }
  return {
    label: `${at.done} of ${at.objects} object${at.objects === 1 ? '' : 's'} put back`
      + ` · ${formatBytes(at.bytes || 0)} written${elapsed ? ` · ${elapsed}` : ''}`,
    detail: at.path ? `${at.path} → ${at.cloud}` : (at.cloud || ''),
    fraction: at.done / at.objects,
  }
}

/* The banner's sentence, from the figures the stray-parts scan carries. */
export function lostNotice(lost) {
  const on = lost.on?.length > 0 ? lost.on.join(', ') : 'a cloud'
  return `${partsOf(lost.parts, lost.files)} ${lost.parts === 1 ? 'is' : 'are'} missing from ${on}, `
    + `where the index records ${lost.parts === 1 ? 'it' : 'them'}. `
    + (lost.restorable > 0
      ? 'The files still read; the missing parts can be remade from the others.'
      : 'The files that still read do so on their other parts.')
}

function AccountRow({ account, checked, onToggle }) {
  const can = account.restorable > 0
  return (
    <label style={{ ...rowStyle, cursor: can ? 'pointer' : 'default' }}>
      <span style={{
        width: '3px', alignSelf: 'stretch', borderRadius: '2px',
        background: accountColor(account.provider_id), flexShrink: 0,
      }} />
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={rowTitle}>{account.name}</div>
        <div style={rowDetail}>
          {partsOf(account.parts, account.files)} · {account.lost} of the {account.recorded}{' '}
          object{account.recorded === 1 ? '' : 's'} recorded there · {formatBytes(account.lost_bytes)}
          {account.emptied ? ' · holds none of them' : ''}
          {account.restorable < account.parts ? ` · ${account.restorable} can be put back` : ''}
        </div>
      </div>
      {can && (
        <input
          type="checkbox"
          checked={checked}
          onChange={onToggle}
          aria-label={`Put parts back on ${account.name}`}
          style={{ width: '16px', height: '16px', accentColor: COLORS.accent, flexShrink: 0 }}
        />
      )}
    </label>
  )
}

function PartRow({ row }) {
  return (
    <div style={{ ...rowStyle, opacity: row.restorable ? 1 : 0.6 }}>
      <span style={{
        width: '3px', alignSelf: 'stretch', borderRadius: '2px',
        background: accountColor(row.provider_id), flexShrink: 0,
      }} />
      <div style={{ minWidth: 0, flex: 1 }}>
        {/* A sub vault tells the main password its weight and nothing about
            what is inside it, so a file in one is named by its archive. */}
        <div style={rowTitle}>{row.path || `a file in a sub vault · ${row.archive_id}`}</div>
        <div style={rowDetail}>
          part {row.part} on {row.provider_name}
          {row.of > 1 ? ` · ${row.objects} of its ${row.of} objects missing` : ''}
          {' · '}{formatBytes(row.bytes)}
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
  margin: '0 0 14px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}

const listStyle = {
  display: 'flex',
  flexDirection: 'column',
  gap: '4px',
  maxHeight: '220px',
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

const rowTitle = {
  fontSize: '12px',
  fontFamily: FONT.sans,
  color: COLORS.text,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
}

const rowDetail = {
  fontFamily: FONT.mono,
  fontSize: '10px',
  lineHeight: 1.5,
  color: COLORS.textMuted,
  wordBreak: 'break-word',
}

const footer = { display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }

const preText = {
  display: 'block',
  fontFamily: FONT.mono,
  fontSize: '11px',
  whiteSpace: 'pre-wrap',
}
