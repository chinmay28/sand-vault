/* Which reattach runs a dialog is drawing right now.

   A reattach runs on the server, not in the dialog that asked for it (see
   api.reattachStart), so the dialog can be closed while it goes. Two views
   can then be watching the same run: the dialog, and the strip over the file
   list that carries it once the dialog is gone. Whichever is drawing it owns
   its bar and its report, and refreshes the listing when it lands; a run a
   dialog has claimed is left alone by the strip, for the same reason a delete
   a dialog is drawing is left alone by EraseRuns.

   Module state, because the two views share nothing else, and an id is all
   either needs to know. */

const claimed = new Map()

/* Marks `id` as drawn by the caller until the function returned is called.
   Counted, so two dialogs on one run do not release each other's claim. */
export function claimReattachRun(id) {
  if (!id) return () => {}
  claimed.set(id, (claimed.get(id) || 0) + 1)
  let released = false
  return () => {
    if (released) return
    released = true
    const left = (claimed.get(id) || 1) - 1
    if (left > 0) claimed.set(id, left)
    else claimed.delete(id)
  }
}

export function isReattachRunClaimed(id) {
  return claimed.has(id)
}

/* "4 shards recorded again across 3 files." — the one sentence a report
   comes to, for whichever view is saying it. */
export function reattachOutcomeText(report) {
  const shards = report?.shards || 0
  const files = report?.files || 0
  if (shards === 0) return 'Nothing needed recording — every part found already had its record.'
  return `${shards} shard${shards === 1 ? '' : 's'} recorded again across ${files} file${files === 1 ? '' : 's'}.`
}
