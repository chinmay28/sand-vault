/* Which background runs a dialog is drawing right now.

   The reattach and the orphan sweep run on the server, not in the dialog that
   asked for them (see useServerJob), so the dialog can be closed while they
   go. Two views can then be watching the same run: the dialog, and the strip
   over the file list that carries it once the dialog is gone. Whichever is
   drawing it owns its bar and its report, and refreshes the listing when it
   lands; a run a dialog has claimed is left alone by the strip, for the same
   reason a delete a dialog is drawing is left alone by EraseRuns.

   Module state, because the views share nothing else. A run is named by its
   kind ("reattach", "sweep") and id, since ids are only unique per job. */

const claimed = new Map()
const keyOf = (kind, id) => `${kind}\u0000${id}`

/* Marks the run as drawn by the caller until the function returned is called.
   Counted, so two dialogs on one run do not release each other's claim. A
   missing id claims nothing. */
export function claimRun(kind, id) {
  if (!id) return () => {}
  const key = keyOf(kind, id)
  claimed.set(key, (claimed.get(key) || 0) + 1)
  let released = false
  return () => {
    if (released) return
    released = true
    const left = (claimed.get(key) || 1) - 1
    if (left > 0) claimed.set(key, left)
    else claimed.delete(key)
  }
}

export function isRunClaimed(kind, id) {
  return claimed.has(keyOf(kind, id))
}
