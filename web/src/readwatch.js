/* Saying what a slow open is waiting on.

   Opening a file is the server racing the accounts that hold its parts, then
   decrypting, then streaming — and from here all of it is a dark rectangle
   until the first byte lands. A photo on a cloud having a slow afternoon is
   several seconds of that, which reads as a hang. The server can say exactly
   what it is doing (see read_watch.go): the content request carries a token
   the browser minted, and the browser asks about that token while it waits.

   Two rules shape everything here. Nothing is shown for the first second:
   a file that arrives in 400ms should not flash a progress bar on its way in.
   And the sentence names things — "Waiting on Google Drive", "Decrypting",
   "Receiving, 43%" — because a spinner already says "wait" and the point is
   to say what for. */

import { useEffect, useState } from 'react'
import { api } from './api'
import { formatBytes } from './theme'

/* How long a wait may run before anything is said about it. */
export const SLOW_AFTER_MS = 1000

/* How often the server is asked. Twice a second is quick enough that a part
   arriving shows as it does, and cheap: the answer is a struct in memory. */
const POLL_MS = 500

/* A token for one read: something only this page could have minted, of a
   shape the server takes (see validReadToken). */
export function readToken() {
  const bytes = new Uint8Array(12)
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256)
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

/* Whether `active` has held for `after` milliseconds: the gate between a wait
   worth mentioning and one that is not. Goes back to false the moment the
   wait ends, so a second load on the same dialog starts its clock again. */
export function useSlow(active, after = SLOW_AFTER_MS) {
  const [slow, setSlow] = useState(false)
  useEffect(() => {
    if (!active) { setSlow(false); return undefined }
    const timer = setTimeout(() => setSlow(true), after)
    return () => clearTimeout(timer)
  }, [active, after])
  return slow && active
}

/* The server's account of a read, asked for twice a second while `active`,
   or null before it has answered. A 404 is "not yet" — the content request
   the token rides on may not have reached the server — and any other
   failure is silence: this answers a question nobody typed, and the fetch it
   watches reports its own failures. The last answer stands once polling
   stops, so a bar that reached the end does not blink empty on the way out. */
export function useReadWatch(token, active) {
  const [progress, setProgress] = useState(null)

  useEffect(() => {
    setProgress(null)
    if (!token || !active) return undefined

    let live = true
    const poll = () => api.readWatch(token)
      .then((resp) => { if (live && resp?.read) setProgress(resp.read) })
      .catch(() => {})

    poll()
    const timer = setInterval(poll, POLL_MS)
    return () => { live = false; clearInterval(timer) }
  }, [token, active])

  return progress
}

/* Whether the server's account of a read reached its end. */
export function readFinished(progress) {
  return progress?.phase === 'done' || progress?.phase === 'failed'
}

/* Names joined the way a sentence joins them: "Google Drive", "Google Drive
   and Dropbox", "Google Drive, Dropbox and Box". */
export function joinNames(names) {
  if (names.length <= 1) return names.join('')
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`
}

/* One line about where a read has got to, and how far along it is.

   `progress` is the server's account (or null before it has one), `received`
   how many bytes this page has taken in so far where it is reading the body
   itself, and `size` the file's length. The answer is a `label` to print and a
   `fraction` from 0 to 1 where one can honestly be given, or null where the
   only honest bar is an indeterminate one — nobody knows how long a cloud
   will take to answer. */
export function describeRead(progress, { received = null, size = 0 } = {}) {
  const chunks = progress?.chunks || 0
  const chunk = progress?.chunk || 0
  const many = chunks > 1
  const where = many ? `chunk ${chunk + 1} of ${chunks}` : ''
  const chunkFraction = many ? chunk / chunks : null

  if (!progress || progress.phase === 'opening') {
    return { label: 'Asking the vault…', fraction: null }
  }

  switch (progress.phase) {
    case 'gathering': {
      const accounts = progress.accounts || []
      const waiting = accounts.filter((a) => a.state === 'waiting').map((a) => a.name || a.kind)
      const have = progress.have || 0
      const needed = progress.needed || 0
      const label = waiting.length > 0
        ? `Waiting on ${joinNames(waiting)}…`
        : 'Gathering the parts…'
      const back = have > 0 && needed > 0 ? `${have} of ${needed} parts back` : ''
      return { label, detail: [back, where].filter(Boolean).join(' · '), fraction: chunkFraction }
    }
    case 'decrypting':
      return { label: 'Decrypting…', detail: where, fraction: chunkFraction }
    case 'sending':
    case 'done': {
      const total = size || progress.size || 0
      /* Whichever count is further along: the server's says what is on its
         way, this page's says what has landed, and the larger of the two is
         never a lie about the other. */
      const got = Math.max(received || 0, progress.sent || 0)
      if (total > 0) {
        const fraction = Math.min(1, got / total)
        return {
          label: `Receiving… ${Math.round(fraction * 100)}%`,
          detail: `${formatBytes(got)} of ${formatBytes(total)}`,
          fraction,
        }
      }
      return { label: `Receiving… ${formatBytes(got)}`, fraction: null }
    }
    case 'failed':
      return { label: 'This file could not be rebuilt.', detail: progress.error || '', fraction: null, failed: true }
    default:
      return { label: 'Working…', fraction: null }
  }
}
