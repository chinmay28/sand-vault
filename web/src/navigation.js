/* Where you are, and how you got there.

   A folder tree is walked, not read: you go in, look, and come back out. Until
   now the only way back was to aim at the right crumb in the trail, which is
   fine two levels deep and useless five — and there was no way at all to
   return to somewhere you had just left. So the current folder is kept as the
   trail of folders visited rather than as a single value, and Back, Forward
   and Up step along it the way they do in every other file manager.

   A step is a vault and a path, not a path alone. Sub vaults have roots of
   their own, so "/" is not one place — and stepping out of one is stepping
   back to a different tree, which the trail has to remember or Back would
   land you at the right path in the wrong vault.

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

/* A destination can be given as a bare path, meaning the vault you are already
   in, or as { vault, path } to cross into another. Most call sites are the
   first kind — walking into a folder does not change which vault you are in —
   and should not have to say so. */
function resolve(to, here) {
  if (typeof to === 'string') return { vault: here.vault, path: to }
  return { vault: to.vault ?? here.vault, path: to.path ?? here.path }
}

function same(a, b) {
  return a.vault === b.vault && a.path === b.path
}

export function useNavigator(root = '/') {
  const [trail, setTrail] = useState(() => ({ visited: [{ vault: '', path: root }], at: 0 }))
  const here = trail.visited[trail.at]

  /* Walking somewhere new throws away whatever Forward was holding — the same
     bargain a browser makes, and for the same reason: two futures cannot both
     be the one in front of you. Walking to where you already are does nothing
     at all, so re-tapping the folder you are in does not fill the trail with
     copies of it. */
  const navigate = useCallback((to) => {
    setTrail((current) => {
      const at = current.visited[current.at]
      const next = resolve(to, at)
      if (same(at, next)) return current
      const visited = [...current.visited.slice(0, current.at + 1), next]
      const over = Math.max(0, visited.length - TRAIL_LIMIT)
      return { visited: visited.slice(over), at: visited.length - 1 - over }
    })
  }, [])

  /* Swaps the folder you are standing in for another without recording the
     step. For the case where the folder went away underneath you: it is not
     somewhere you chose to leave, so Back should not lead there. It is also
     what a sub vault being locked needs — you did not walk out of it. */
  const replace = useCallback((to) => {
    setTrail((current) => {
      const at = current.visited[current.at]
      const next = resolve(to, at)
      if (same(at, next)) return current
      const visited = [...current.visited]
      visited[current.at] = next
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

  /* Up stops at the root of whichever vault you are in. A sub vault's root is
     not below the main vault's — it is a separate tree — so climbing out of one
     is something you do deliberately, not something Up does to you. */
  const up = useCallback(() => {
    setTrail((current) => {
      const at = current.visited[current.at]
      const parent = parentPath(at.path)
      if (parent === at.path) return current
      const visited = [...current.visited.slice(0, current.at + 1), { vault: at.vault, path: parent }]
      const over = Math.max(0, visited.length - TRAIL_LIMIT)
      return { visited: visited.slice(over), at: visited.length - 1 - over }
    })
  }, [])

  /* Drops every step that was inside a sub vault. Locking one has to take the
     trail with it — the folder names in it are that vault's index — and if you
     were standing in it, you are put back at the main root. */
  const leaveVault = useCallback((vault) => {
    setTrail((current) => {
      const kept = current.visited.filter((step) => step.vault !== vault)
      if (kept.length === current.visited.length) return current
      if (kept.length === 0) return { visited: [{ vault: '', path: root }], at: 0 }
      return { visited: kept, at: Math.min(current.at, kept.length - 1) }
    })
  }, [root])

  // Locking the vault has to take the trail with it: the folder names in it
  // are the file index, which is exactly what locking puts away.
  const reset = useCallback(() => setTrail({ visited: [{ vault: '', path: root }], at: 0 }), [root])

  return useMemo(() => ({
    path: here.path,
    vault: here.vault,
    navigate,
    replace,
    back,
    forward,
    up,
    reset,
    leaveVault,
    canBack: trail.at > 0,
    canForward: trail.at < trail.visited.length - 1,
    canUp: here.path !== '/',
    // What each arrow would land on, so the buttons can say so on hover
    // rather than leaving it to be discovered by pressing them.
    behind: trail.at > 0 ? trail.visited[trail.at - 1].path : null,
    ahead: trail.at < trail.visited.length - 1 ? trail.visited[trail.at + 1].path : null,
    parent: parentPath(here.path),
  }), [here, navigate, replace, back, forward, up, reset, leaveVault, trail])
}
