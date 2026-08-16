/* Where you are, and how you got there.

   A folder tree is walked, not read: you go in, look, and come back out. Until
   now the only way back was to aim at the right crumb in the trail, which is
   fine two levels deep and useless five — and there was no way at all to
   return to somewhere you had just left. So the current folder is kept as the
   trail of folders visited rather than as a single value, and Back, Forward
   and Up step along it the way they do in every other file manager.

   The trail lives in memory and nowhere else. Putting it in the browser's own
   history would mean writing the folder names you visited into an entry the
   browser keeps after the vault is locked, which is the one thing this app is
   careful never to leave lying about. */

import { useCallback, useMemo, useState } from 'react'
import { parentPath } from './api'

/* Far more than anyone walks through in a sitting, and bounded so a tab left
   open for a week does not grow a trail without end. Trimming from the front
   costs the oldest folders, which are the ones Back was never going to reach. */
const TRAIL_LIMIT = 100

export function useNavigator(root = '/') {
  const [trail, setTrail] = useState(() => ({ visited: [root], at: 0 }))
  const path = trail.visited[trail.at]

  /* Walking somewhere new throws away whatever Forward was holding — the same
     bargain a browser makes, and for the same reason: two futures cannot both
     be the one in front of you. Walking to where you already are does nothing
     at all, so re-tapping the folder you are in does not fill the trail with
     copies of it. */
  const navigate = useCallback((to) => {
    setTrail((current) => {
      if (current.visited[current.at] === to) return current
      const visited = [...current.visited.slice(0, current.at + 1), to]
      const over = Math.max(0, visited.length - TRAIL_LIMIT)
      return { visited: visited.slice(over), at: visited.length - 1 - over }
    })
  }, [])

  /* Swaps the folder you are standing in for another without recording the
     step. For the case where the folder went away underneath you: it is not
     somewhere you chose to leave, so Back should not lead there. */
  const replace = useCallback((to) => {
    setTrail((current) => {
      if (current.visited[current.at] === to) return current
      const visited = [...current.visited]
      visited[current.at] = to
      return { ...current, visited }
    })
  }, [])

  const back = useCallback(() => {
    setTrail((current) => (current.at > 0 ? { ...current, at: current.at - 1 } : current))
  }, [])

  const forward = useCallback(() => {
    setTrail((current) => (
      current.at < current.visited.length - 1 ? { ...current, at: current.at + 1 } : current))
  }, [])

  const up = useCallback(() => {
    setTrail((current) => {
      const here = current.visited[current.at]
      const parent = parentPath(here)
      if (parent === here) return current
      const visited = [...current.visited.slice(0, current.at + 1), parent]
      const over = Math.max(0, visited.length - TRAIL_LIMIT)
      return { visited: visited.slice(over), at: visited.length - 1 - over }
    })
  }, [])

  // Locking the vault has to take the trail with it: the folder names in it
  // are the file index, which is exactly what locking puts away.
  const reset = useCallback(() => setTrail({ visited: [root], at: 0 }), [root])

  return useMemo(() => ({
    path,
    navigate,
    replace,
    back,
    forward,
    up,
    reset,
    canBack: trail.at > 0,
    canForward: trail.at < trail.visited.length - 1,
    canUp: path !== '/',
    // What each arrow would land on, so the buttons can say so on hover
    // rather than leaving it to be discovered by pressing them.
    behind: trail.at > 0 ? trail.visited[trail.at - 1] : null,
    ahead: trail.at < trail.visited.length - 1 ? trail.visited[trail.at + 1] : null,
    parent: parentPath(path),
  }), [path, navigate, replace, back, forward, up, reset, trail])
}
