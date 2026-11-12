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

/* The scan for parts a cloud should be holding and is not (see
   api.lostScanStart), and the run that puts them back (api.lostRestoreStart;
   `start` takes {providerIds}). */
export function useLostScan() {
  return useServerJob(api.lostScanStatus, api.lostScanStart, 'sand:lost-scan')
}

export function useLostRestore() {
  return useServerJob(api.lostRestoreStatus, api.lostRestoreStart, 'sand:lost-restore')
}

/* The orphan sweep, the same way (see api.sweepStart). `start` takes
   {targets}. */
export function useOrphanSweep() {
  return useServerJob(api.sweepStatus, api.sweepStart, 'sand:orphan-sweep')
}

/* The sweep of a folder for film details (see api.filmScanStart). `start`
   takes (path, {refresh, vault}). */
export function useFilmScan() {
  return useServerJob(api.filmScanStatus, api.filmScanStart, 'sand:film-scan')
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
