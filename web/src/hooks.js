import { useCallback, useEffect, useState } from 'react'
import { api } from './api'
import { keepScreenAwake } from './wakelock'

/* Every style in this app is inline, so there is no stylesheet to hang an
   @media block off. Layout that has to change shape on a phone — the two-pane
   split, the file table's fixed columns — branches on this instead. Anything
   that only needs a different number (padding, font size) stays in CSS. */
export function useMediaQuery(query) {
  const [matches, setMatches] = useState(
    () => typeof window !== 'undefined' && window.matchMedia(query).matches,
  )

  useEffect(() => {
    const mql = window.matchMedia(query)
    const onChange = (e) => setMatches(e.matches)
    // The query can change between renders, and the viewport can have moved
    // since the initial state was computed.
    setMatches(mql.matches)

    // Safari only grew addEventListener on MediaQueryList in 14.
    if (mql.addEventListener) mql.addEventListener('change', onChange)
    else mql.addListener(onChange)

    return () => {
      if (mql.removeEventListener) mql.removeEventListener('change', onChange)
      else mql.removeListener(onChange)
    }
  }, [query])

  return matches
}

/* Where the orphan sweep has got to, asked about once a second while one is
   running. Erasing the abandoned parts is one POST that answers only at the end — for a vault where somebody has
   been deleting films, minutes of a button saying "Erasing…" — and the server
   counts objects beside it (see /api/vault/orphans/erasing). A total of 0
   while running is its own kind of news: the sweep lists every account again
   before its first delete, so the button is not stuck, the clouds are being
   asked. The count holds rather than blinking empty when the last poll races
   the sweep finishing, for the reason below. */
export function useOrphanEraseProgress(active) {
  return useErasing(() => api.orphanErasing(), active, [])
}

/* One poll of an erasing window, whichever request it is beside. `ask` is
   called every 900ms while active and answers {running, done, total}; the
   count is kept while it is running and left standing afterwards, so the last
   figure does not blink empty when the final poll races the request
   finishing. Silence on failure is on purpose: this is the answer to a
   question nobody typed, and the delete it watches reports its own failures. */
function useErasing(ask, active, deps) {
  const [at, setAt] = useState(null)

  useEffect(() => {
    setAt(null)
    if (!active) return undefined

    let live = true
    const poll = () => ask()
      .then((resp) => {
        if (live && resp.running) setAt({ done: resp.done, total: resp.total })
      })
      .catch(() => {})

    poll()
    const timer = setInterval(poll, 900)
    return () => { live = false; clearInterval(timer) }
  }, [active, ...deps])

  return at
}

/* How long a finished stray-parts scan is taken as the answer when the panel
   opens, rather than being run again. A page that was closed mid-scan and
   comes back wants the result it was waiting on, not a second listing of
   every cloud; a page opened an hour later wants to know what is true now. */
export const STRAY_SCAN_FRESH_MS = 15 * 60 * 1000

/* The stray-parts scan, as a page sees it: the run that is going, or the last
   one that ran, polled while it is going.

   The scan lives on the server rather than in a request (see
   api.strayScanStart), so this is a view of it and nothing more. `status` is
   null until the server has been asked once, then whatever it answered — an
   empty run when nothing has been asked since the vault was unlocked. It is
   re-asked every 800ms while `status.running`, and left standing once it is
   not. `start` begins a scan or joins the one already going; `refresh` asks
   again without starting anything. Failures of the polling itself are kept
   quiet — the scan reports its own in `status.error`, and a missed poll is
   answered by the next one. */
export function useStrayScan() {
  return useServerJob(api.strayScanStatus, api.strayScanStart, 'sand:stray-scan')
}

/* The reattach, as a page sees it — the same view as useStrayScan, of the
   repair that records mislaid parts back (see api.reattachStart). `start`
   takes {providerId}. */
export function useReattach() {
  return useServerJob(api.reattachStatus, api.reattachStart, 'sand:reattach')
}

/* A job the server runs apart from any request (see jobWatch on the server):
   `ask` answers its status, `begin` starts one or joins the one going. Both
   must be stable functions. `status` is null until the first answer, then
   whatever the server said — {id, running, scope, started_at, finished_at,
   progress, result, error}, or an empty run when nothing has been asked. It
   is re-asked every 800ms while running and left standing once it is not.
   `start(...args)` passes its arguments to `begin`. Failures of the polling
   itself are kept quiet; the job reports its own in `status.error`, and a
   failure to start is `error`.

   `channel` names a window event fired whenever one of the page's views
   starts a run, so every other view of the same job asks again and starts
   watching it — a strip over the file list hears about a run a dialog began,
   and goes on drawing it after the dialog is closed. */
function useServerJob(ask, begin, channel) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)

  const refresh = useCallback(async () => {
    try {
      setStatus(await ask())
    } catch (err) {
      setError(err.message)
    }
  }, [ask])

  const start = useCallback(async (...args) => {
    setError(null)
    try {
      setStatus(await begin(...args))
      window.dispatchEvent(new Event(channel))
    } catch (err) {
      setError(err.message)
      // Whatever is going on the server now is still worth showing — a
      // refused start is usually another run in the way.
      refresh()
    }
  }, [begin, refresh])

  useEffect(() => { refresh() }, [refresh])
  useEffect(() => {
    window.addEventListener(channel, refresh)
    return () => window.removeEventListener(channel, refresh)
  }, [channel, refresh])

  const running = !!status?.running
  const runID = status?.id
  useEffect(() => {
    if (!running) return undefined
    let live = true
    const poll = () => ask()
      .then((fresh) => { if (live) setStatus(fresh) })
      .catch(() => {})
    const timer = setInterval(poll, 800)
    return () => { live = false; clearInterval(timer) }
  }, [ask, running, runID])

  return { status, error, start, refresh }
}

/* Where the two-pane layout gives up: a 286px sidebar plus a file table whose
   fixed columns already eat 480px leaves nothing for a filename below this. */
export const MOBILE_QUERY = '(max-width: 860px)'

export function useIsMobile() {
  return useMediaQuery(MOBILE_QUERY)
}

/* Keeps the screen from timing out while `active` is true — for an upload,
   which outlasts any screen's own timeout and dies with the machine when a
   laptop goes to sleep. Nothing is held once it is false again, or once the
   component is gone; see wakelock.js for what is and is not promised. */
export function useScreenAwake(active) {
  useEffect(() => {
    if (!active) return undefined
    return keepScreenAwake()
  }, [active])
}
