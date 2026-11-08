import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { LoadProgress } from './ui'

/* How far a listing of the clouds has got, drawn from what the server last
   said about the job running it — the stray-parts scan, or the reattach that
   lists before it records (see useServerJob).

   The unit is a cloud, because that is the only unit there is: one account
   answers its listing all at once, so there is nothing finer to count, and a
   bar that crept along on a timer would be pretending to know how long the
   slowest cloud will take. What can be said honestly is how many clouds have
   answered, which are still being waited on, and how long it has been.

   `status` is the job's status, or null before the first answer. `note` is the
   paragraph under the bar — the place to say that closing the dialog stops
   nothing. `start` is the sentence for before the listing has begun. */
export default function ScanProgress({ status, note, start = 'Asking every connected cloud what it is holding…' }) {
  const at = status?.progress
  const startedAt = status?.started_at ? Date.parse(status.started_at) : NaN

  /* Ticks once a second, only so the elapsed time moves between polls. */
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const elapsed = Number.isFinite(startedAt) ? formatElapsed(now - startedAt) : ''

  const { label, detail, fraction } = describeProgress(status, at, elapsed, start)

  return (
    <div style={{
      display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '12px',
      padding: '18px 2px 22px',
    }}>
      <LoadProgress label={label} detail={detail} fraction={fraction} width={440} />
      {note && (
        <p style={{
          margin: 0,
          fontFamily: FONT.sans,
          fontSize: '12px',
          lineHeight: 1.6,
          color: COLORS.textMuted,
          textAlign: 'center',
          maxWidth: '440px',
        }}>{note}</p>
      )}
    </div>
  )
}

/* The bar's sentence, its smaller line, and how full it is, for each stage a
   listing job reports. */
export function describeProgress(status, at, elapsed, start) {
  if (!status) return { label: 'Asking what is already known…', detail: '', fraction: null }
  switch (at?.stage) {
    case 'pulling':
      return {
        label: 'Fetching the shared index from the other machines this vault is open on…',
        detail: elapsed,
        fraction: null,
      }
    case 'listing':
      if (at.roots > 0) {
        return {
          label: `${at.listed} of ${at.roots} cloud${at.roots === 1 ? '' : 's'} answered`
            + (elapsed ? ` · ${elapsed}` : ''),
          detail: at.waiting?.length > 0 ? `Waiting on ${nameSome(at.waiting)}` : '',
          fraction: at.listed / at.roots,
        }
      }
      break
    case 'judging':
      return {
        label: 'Every cloud has answered — weighing what they hold against the index…',
        detail: elapsed,
        fraction: 1,
      }
    case 'recording':
      return { label: 'Writing the records back into the index…', detail: elapsed, fraction: 1 }
    default:
  }
  return { label: start, detail: elapsed, fraction: null }
}

/* Up to three names, and a count for the rest: the bar has one line for this. */
function nameSome(names) {
  if (names.length <= 3) return names.join(', ')
  return `${names.slice(0, 3).join(', ')} and ${names.length - 3} more`
}

/* 0:07, 1:42, 1:03:09 — the shape a stopwatch shows, since that is what this is. */
export function formatElapsed(ms) {
  const total = Math.max(0, Math.floor(ms / 1000))
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const seconds = total % 60
  const pad = (n) => String(n).padStart(2, '0')
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${minutes}:${pad(seconds)}`
}
