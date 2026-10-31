import React, { useEffect, useRef } from 'react'
import { COLORS, FONT } from '../theme'
import { Banner, Button, Spinner } from './ui'
import {
  eraseOutcomeText, eraseProgressText, eraseRunsLoaded, isEraseRunClaimed, stopErase, useEraseRuns,
} from '../erase'

/* Every delete the machine is carrying out, drawn above the file list — and
   what one lately came to, until it is dismissed.

   This is where a delete goes once it has left the dialog that asked for it.
   A row's delete never had a dialog to live in beyond the confirmation, and a
   bulk delete's can be closed partway; either way the run is still going on
   the machine, and this strip is its bar while it goes and its report once it
   is done — on this page, or the next one opened. A run a dialog is still
   drawing is left to that dialog.

   The listing behind it is drawn from the index, which a finished delete has
   changed, so one finishing is what refreshes it — whichever page or dialog
   started it. */
export default function EraseRuns({ mobile, onChanged }) {
  const runs = useEraseRuns()

  /* Which finished runs have already been accounted for. Seeded from the
     first answer rather than empty, so a page opened onto yesterday's
     results does not refresh a listing that was just drawn. */
  const known = useRef(null)
  useEffect(() => {
    if (known.current === null) {
      if (!eraseRunsLoaded()) return
      known.current = new Set(runs.filter((run) => run.done).map((run) => run.id))
      return
    }
    let arrived = false
    for (const run of runs) {
      if (!run.done || known.current.has(run.id)) continue
      known.current.add(run.id)
      // A dialog drawing the run refreshes on its own way out.
      if (!isEraseRunClaimed(run.id)) arrived = true
    }
    if (arrived) onChanged?.()
  }, [runs]) // eslint-disable-line react-hooks/exhaustive-deps

  const shown = runs.filter((run) => !isEraseRunClaimed(run.id))
  if (shown.length === 0) return null

  return (
    <div style={{ padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }}>
      {shown.map((run) => (run.done
        ? <FinishedErase key={run.id} run={run} onDismiss={() => stopErase(run.id).catch(() => {})} />
        : <EraseRunCard key={run.id} run={run} onStop={() => stopErase(run.id).catch(() => {})} />))}
    </div>
  )
}

/* One delete going: what, how far, and the one thing worth a button while it
   runs. A stop lands between files, not between parts — the few in flight
   finish their round, so nothing is left half erased. */
export function EraseRunCard({ run, onStop, stopping = false }) {
  const at = run.at || {}
  const total = at.total || 0
  const fraction = total > 0 ? Math.min(1, (at.done || 0) / total) : 0

  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '12px',
      padding: '9px 13px', marginBottom: '12px',
      background: COLORS.surface, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
    }}>
      <Spinner size={11} />
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{
          display: 'flex', justifyContent: 'space-between', gap: '10px', marginBottom: '6px',
          fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim,
        }}>
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {stopping ? 'Stopping' : run.waiting ? 'Waiting to delete' : 'Deleting'} {run.label}
          </span>
          <span style={{ flexShrink: 0, color: COLORS.textMuted }}>
            {run.waiting ? `${total} file${total === 1 ? '' : 's'}, after the one before it` : eraseProgressText(run)}
          </span>
        </div>
        <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden' }}>
          <div style={{
            height: '100%',
            width: `${Math.max(4, fraction * 100)}%`,
            background: COLORS.accent,
            transition: 'width 0.3s ease',
          }} />
        </div>
      </div>
      {onStop && (
        <Button size="sm" variant="ghost" disabled={stopping} onClick={onStop}>
          {stopping ? 'Stopping…' : 'Stop'}
        </Button>
      )}
    </div>
  )
}

/* What a delete came to, with the first few reasons anything was left behind
   — "parts could not be erased" without a which is a question, not a report. */
export function FinishedErase({ run, onDismiss }) {
  const warnings = run.report?.warnings || []
  const tone = run.error ? 'error' : run.cancelled || warnings.length ? 'warn' : 'success'
  return (
    <Banner tone={tone} onDismiss={onDismiss}>
      <div>{eraseOutcomeText(run)}</div>
      {run.error && (
        <div style={{ marginTop: '4px' }}>
          Every file that was erased is gone from the listing. Deleting the rest again picks up where this stopped.
        </div>
      )}
      {warnings.slice(0, 3).map((w, i) => (
        <div key={i} style={{ marginTop: '4px', fontSize: '11px' }}>{w}</div>
      ))}
      {warnings.length > 3 && (
        <div style={{ marginTop: '4px', fontSize: '11px', color: COLORS.textMuted }}>
          …and {warnings.length - 3} more part{warnings.length - 3 === 1 ? '' : 's'} left behind. The stray-parts scan in the accounts panel finds them.
        </div>
      )}
    </Banner>
  )
}
