import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { useLostRestore, useOrphanSweep, useReattach } from '../hooks'
import { isRunClaimed } from '../runClaims'
import { Banner, Spinner } from './ui'
import { describeProgress, formatElapsed } from './ScanProgress'
import { describeRestore, restoreOutcomeText } from './LostParts'

/* The housekeeping that carries on after its dialog is closed, drawn above
   the file list — and what each run came to, until that is dismissed.

   Putting mislaid parts back, erasing abandoned ones and remaking lost ones
   all run on the server (see useServerJob), so closing their dialog, or the
   page, stops nothing. This is where they show from then on: a bar while they go, and a
   line saying what they did once they land, on this page or the next one
   opened while they are still going. A run a dialog is still drawing is left
   to that dialog (see runClaims.js). */
export default function BackgroundRuns({ mobile, onChanged }) {
  return (
    <>
      <JobStrip
        kind="reattach"
        useJob={useReattach}
        title="Putting mislaid parts back"
        describe={(status, elapsed) => describeProgress(
          status, status.progress, elapsed, 'Asking the clouds what they are holding…',
        )}
        outcome={(run) => (run.error
          ? `Putting the mislaid parts back did not finish: ${run.error}`
          : `Mislaid parts put back. ${reattachOutcomeText(run.result)} No data was transferred.`
            + (run.result?.merged?.length ? ` ${mergedText(run.result.merged)}` : ''))}
        /* The index changed only if something was recorded. */
        changed={(run) => run.result?.shards > 0}
        mobile={mobile}
        onChanged={onChanged}
      />
      <JobStrip
        kind="sweep"
        useJob={useOrphanSweep}
        title="Erasing parts nothing points at"
        describe={(status, elapsed) => describeSweep(status.progress, elapsed)}
        outcome={(run) => (run.error
          ? `Erasing the abandoned parts did not finish: ${run.error}`
          : `Abandoned parts erased. ${sweepOutcomeText(run.result)}`)}
        /* Room on the clouds changed, which the accounts panel shows. */
        changed={(run) => run.result?.deleted > 0}
        mobile={mobile}
        onChanged={onChanged}
      />
      <JobStrip
        kind="lost-restore"
        useJob={useLostRestore}
        title="Putting lost parts back"
        describe={describeRestore}
        outcome={(run) => (run.error
          ? `Putting the lost parts back did not finish: ${run.error}`
          : restoreOutcomeText(run.result)
            + (run.result?.stopped ? ` It ended early — ${run.result.stopped_by || 'it was stopped'}.` : '')
            + (run.result?.failed > 0 ? ` ${run.result.failed} could not be put back — Settings → Lost parts says which.` : ''))}
        changed={(run) => run.result?.objects > 0}
        mobile={mobile}
        onChanged={onChanged}
      />
    </>
  )
}

/* "4 shards recorded again across 3 files." */
export function reattachOutcomeText(report) {
  const shards = report?.shards || 0
  const files = report?.files || 0
  if (shards === 0) return 'Nothing needed recording — every part found already had its record.'
  return `${shards} shard${shards === 1 ? '' : 's'} recorded again across ${files} file${files === 1 ? '' : 's'}.`
}

/* The disconnected accounts a reattach recognised in the ones the parts were
   found on, and what that did: their read history now counts under the
   account they came back as, rather than as a row of its own that only ever
   fails. "Elements" twice reads oddly but is the common case — the storage
   was connected again under the name it had. */
export function mergedText(merged) {
  const named = merged.map((m) => (m.from_name && m.from_name !== m.into_name
    ? `${m.from_name} is now ${m.into_name}`
    : `${m.into_name} is the ${m.into_name} that was disconnected`))
  return `${named.join('; ')} — its read history was carried over, so the read race shows it as one cloud.`
}

/* "12 objects erased across 3 archives, freeing 2.4 MB." */
export function sweepOutcomeText(report) {
  const deleted = report?.deleted || 0
  const archives = report?.archives || 0
  if (deleted === 0) return 'Nothing needed erasing — what was named had stopped being abandoned.'
  return `${deleted} object${deleted === 1 ? '' : 's'} erased across ${archives} archive`
    + `${archives === 1 ? '' : 's'}, freeing ${formatBytes(report.bytes || 0)}.`
}

/* The bar's sentence and how full it is, for a sweep. The first stretch has
   no denominator — the sweep lists every account again before its first
   delete, so that what it erases is what is abandoned now — and saying so
   beats a bar pretending to know. */
export function describeSweep(at, elapsed) {
  if (at?.total > 0) {
    return {
      label: `Erased ${at.done} of ${at.total} object${at.total === 1 ? '' : 's'}`
        + (elapsed ? ` · ${elapsed}` : ''),
      detail: '',
      fraction: Math.min(1, at.done / at.total),
    }
  }
  return {
    label: 'Checking every cloud once more, so what goes is what is abandoned now…',
    detail: elapsed,
    fraction: null,
  }
}

/* One background job's strip. `useJob` is the hook that views it (constant
   for the life of the strip); `describe(status, elapsed)` gives the bar's
   {label, detail, fraction}; `outcome(run)` the sentence a finished run comes
   to; `changed(run)` whether it changed what the page is showing. */
function JobStrip({ kind, useJob, title, describe, outcome, changed, mobile, onChanged }) {
  const { status } = useJob()
  const [, rerender] = useState(0)

  /* The ids of runs this strip saw running, so a page opened onto a report
     from before it was loaded does not announce it, and one it saw land is
     announced once. */
  const seenRunning = useRef(new Set())
  const [landed, setLanded] = useState(null)
  useEffect(() => {
    if (!status?.id) return
    if (status.running) {
      seenRunning.current.add(status.id)
      return
    }
    if (!seenRunning.current.has(status.id)) return
    seenRunning.current.delete(status.id)
    // A dialog drawing the run says what it came to and refreshes on its own.
    if (isRunClaimed(kind, status.id)) return
    setLanded(status)
    if (changed(status)) onChanged?.()
  }, [status]) // eslint-disable-line react-hooks/exhaustive-deps

  /* The claim is module state nothing announces, so a dialog closing over a
     running run is noticed on the next tick of this clock rather than the next
     poll — and the clock is what moves the elapsed time between polls. */
  const running = !!status?.running
  useEffect(() => {
    if (!running) return undefined
    const timer = setInterval(() => rerender((n) => n + 1), 1000)
    return () => clearInterval(timer)
  }, [running])

  const pad = { padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }

  if (running && !isRunClaimed(kind, status.id)) {
    const startedAt = status.started_at ? Date.parse(status.started_at) : NaN
    const elapsed = Number.isFinite(startedAt) ? formatElapsed(Date.now() - startedAt) : ''
    const { label, detail, fraction } = describe(status, elapsed)
    return (
      <div style={pad}>
        <RunCard title={title} label={label} detail={detail !== elapsed ? detail : ''} fraction={fraction} />
      </div>
    )
  }
  if (landed) {
    return (
      <div style={pad}>
        <Banner tone={landed.error ? 'error' : 'success'} onDismiss={() => setLanded(null)}>
          {outcome(landed)}
        </Banner>
      </div>
    )
  }
  return null
}

/* A running job: a title, a 3px bar, and the count — and who is still being
   waited on — under the bar, since on a phone there is no room beside the
   title. A null fraction is a bar that only says "still going". */
function RunCard({ title, label, detail, fraction }) {
  const determinate = typeof fraction === 'number'
  return (
    <div
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={determinate ? Math.round(fraction * 100) : undefined}
      aria-valuetext={label}
      aria-label={`${title}: ${label}`}
      style={{
        display: 'flex', alignItems: 'center', gap: '12px',
        padding: '9px 13px', marginBottom: '12px',
        background: COLORS.surface, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
      }}
    >
      <Spinner size={11} />
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{
          marginBottom: '6px', fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>
          {title}
        </div>
        <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden', position: 'relative' }}>
          <div style={{
            position: 'absolute', top: 0, bottom: 0,
            left: determinate ? 0 : undefined,
            width: determinate ? `${Math.max(4, fraction * 100)}%` : '38%',
            background: COLORS.accent,
            transition: determinate ? 'width 0.3s ease' : undefined,
            animation: determinate ? undefined : 'sand-sweep 1.3s ease-in-out infinite',
          }} />
        </div>
        <div style={{
          marginTop: '5px', fontFamily: FONT.mono, fontSize: '10px', lineHeight: 1.45,
          color: COLORS.textMuted, wordBreak: 'break-word',
        }}>
          {label}{detail ? ` · ${detail}` : ''}
        </div>
      </div>
    </div>
  )
}
