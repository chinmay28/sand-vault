import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { useReattach } from '../hooks'
import { isReattachRunClaimed, reattachOutcomeText } from '../reattach'
import { Banner, Spinner } from './ui'
import { describeProgress, formatElapsed } from './ScanProgress'

/* A reattach still going after the dialog that started it was closed, drawn
   above the file list — and what it came to, until that is dismissed.

   The repair runs on the server (see api.reattachStart), so closing its dialog,
   or the page, stops nothing. This is where it shows from then on: a bar while
   it lists the clouds, and a line saying what it recorded once it lands, on
   this page or the next one opened while it is still going. A run a dialog is
   still drawing is left to that dialog (see reattach.js).

   The listing is drawn from the index, which a finished reattach has changed,
   so the run landing is what refreshes it — when no dialog was there to. */
export default function ReattachRuns({ mobile, onChanged }) {
  const { status } = useReattach()
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
    if (isReattachRunClaimed(status.id)) return
    setLanded(status)
    if (status.result?.shards > 0) onChanged?.()
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

  if (running && !isReattachRunClaimed(status.id)) {
    return <div style={pad}><RunningReattach status={status} /></div>
  }
  if (landed) {
    return (
      <div style={pad}>
        <Banner tone={landed.error ? 'error' : 'success'} onDismiss={() => setLanded(null)}>
          {landed.error
            ? `Putting the mislaid parts back did not finish: ${landed.error}`
            : `Mislaid parts put back. ${reattachOutcomeText(landed.result)} No data was transferred.`}
        </Banner>
      </div>
    )
  }
  return null
}

function RunningReattach({ status }) {
  const startedAt = status.started_at ? Date.parse(status.started_at) : NaN
  const elapsed = Number.isFinite(startedAt) ? formatElapsed(Date.now() - startedAt) : ''
  const { label, detail, fraction } = describeProgress(
    status, status.progress, elapsed, 'Asking the clouds what they are holding…',
  )
  const determinate = typeof fraction === 'number'

  return (
    <div
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={determinate ? Math.round(fraction * 100) : undefined}
      aria-valuetext={label}
      aria-label={`Putting mislaid parts back: ${label}`}
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
          Putting mislaid parts back
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
        {/* The count, and who is still being waited on, under the bar — on
            a phone there is no room for them beside the title. */}
        <div style={{
          marginTop: '5px', fontFamily: FONT.mono, fontSize: '10px', lineHeight: 1.45,
          color: COLORS.textMuted, wordBreak: 'break-word',
        }}>
          {label}{detail && detail !== elapsed ? ` · ${detail}` : ''}
        </div>
      </div>
    </div>
  )
}
