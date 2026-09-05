import React from 'react'
import { COLORS } from '../theme'
import { describeRead, readFinished, useReadWatch, useSlow } from '../readwatch'
import { LoadProgress } from './ui'

/* The sentence over a slow open: which cloud it is waiting on, that it is
   decrypting, how much has arrived. See readwatch.js for the rules.

   `watch` is the token the content request carries and `active` whether the
   wait is still on. Nothing is drawn for the first second of it. `received`
   is how many bytes this page has read itself, where it is reading the body
   rather than handing an address to an <img> or <video>; without it the
   server's own count of what it has sent stands in.

   `overlay` lays it over whatever is loading, letting pointer events through
   so a player's controls underneath still work; otherwise it sits in the flow
   where it is written. `untilDone` also takes it down once the server says
   the read is over, for the elements that give no reliable signal of their
   own — a <video> on iOS may never say it has a frame until it is played. */
export default function ReadStatus({
  watch, active, received = null, size = 0, overlay = false, untilDone = false,
}) {
  const slow = useSlow(active)
  const progress = useReadWatch(watch, active && slow)
  const finished = untilDone && readFinished(progress) && progress.phase !== 'failed'

  if (!slow || finished) return null

  const { label, detail, fraction, failed } = describeRead(progress, { received, size })
  const bar = (
    <LoadProgress label={label} detail={detail} fraction={fraction} tone={failed ? 'error' : 'info'} />
  )

  if (!overlay) {
    return <div style={{ display: 'flex', justifyContent: 'center', padding: '4px 0 10px' }}>{bar}</div>
  }
  return (
    <div style={{
      position: 'absolute',
      left: 0,
      right: 0,
      bottom: 0,
      display: 'flex',
      justifyContent: 'center',
      padding: '10px 16px 12px',
      /* Legible over a photograph's bright corner and over the black of a
         player alike, and gone from the touch layer entirely. */
      background: `linear-gradient(to top, ${COLORS.bg}f2, ${COLORS.bg}00)`,
      pointerEvents: 'none',
    }}>{bar}</div>
  )
}
