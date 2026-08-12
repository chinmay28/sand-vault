import { useEffect, useState } from 'react'

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

/* Where the two-pane layout gives up: a 286px sidebar plus a file table whose
   fixed columns already eat 480px leaves nothing for a filename below this. */
export const MOBILE_QUERY = '(max-width: 860px)'

export function useIsMobile() {
  return useMediaQuery(MOBILE_QUERY)
}
