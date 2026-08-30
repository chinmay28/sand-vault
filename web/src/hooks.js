import { useEffect, useState } from 'react'
import { api } from './api'

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

/* Where a recursive folder delete has got to, asked about once a second while
   one is running. The DELETE itself is a single request that answers only at
   the end, which for a folder of hundreds of files is minutes of a button
   saying "Deleting…" — indistinguishable from a hang. The server counts files
   beside the running request (see /api/folders/erasing), and this is the
   asking end.

   `active` is whether the caller has such a delete in flight: polling starts
   with it and stops with it, so nothing asks a question that has no answer.
   The count only ever moves forward here — the last poll can race the delete
   finishing and come back "not running", and a bar that has watched 76 of 79
   go should hold there rather than blink empty on the way out. */
export function useEraseProgress(path, vault, active) {
  const [at, setAt] = useState(null)

  useEffect(() => {
    setAt(null)
    if (!active) return undefined

    let live = true
    const ask = () => api.folderErasing(path, vault)
      .then((resp) => {
        if (live && resp.running) setAt({ done: resp.done, total: resp.total })
      })
      // Silence on purpose: this is the answer to a question nobody typed,
      // and the delete it watches reports its own failures.
      .catch(() => {})

    ask()
    const timer = setInterval(ask, 900)
    return () => { live = false; clearInterval(timer) }
  }, [path, vault, active])

  return at
}

/* The same window, opened onto the orphan sweep. Erasing the abandoned parts
   is one POST that answers only at the end — for a vault where somebody has
   been deleting films, minutes of a button saying "Erasing…" — and the server
   counts objects beside it (see /api/vault/orphans/erasing). A total of 0
   while running is its own kind of news: the sweep lists every account again
   before its first delete, so the button is not stuck, the clouds are being
   asked. The count holds rather than blinking empty when the last poll races
   the sweep finishing, for the reason above. */
export function useOrphanEraseProgress(active) {
  const [at, setAt] = useState(null)

  useEffect(() => {
    setAt(null)
    if (!active) return undefined

    let live = true
    const ask = () => api.orphanErasing()
      .then((resp) => {
        if (live && resp.running) setAt({ done: resp.done, total: resp.total })
      })
      // Silence on purpose, as above: the sweep reports its own failures.
      .catch(() => {})

    ask()
    const timer = setInterval(ask, 900)
    return () => { live = false; clearInterval(timer) }
  }, [active])

  return at
}

/* Where the two-pane layout gives up: a 286px sidebar plus a file table whose
   fixed columns already eat 480px leaves nothing for a filename below this. */
export const MOBILE_QUERY = '(max-width: 860px)'

export function useIsMobile() {
  return useMediaQuery(MOBILE_QUERY)
}
