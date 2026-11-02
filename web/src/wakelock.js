/* Keeping the screen on while something long is running in this tab.
 *
 * A folder of photographs takes an evening to upload, and an evening is
 * longer than any screen's own timeout. What happens then depends on the
 * machine: a desktop turns its display off and carries on, a laptop dims,
 * locks and then sleeps — and a sleeping laptop sends nothing, so the batch
 * in flight fails as a network error and the rest never leave. The Screen
 * Wake Lock API is the browser's way to ask the system not to do that while
 * the page is visible, and it is what every site that uploads for a living
 * asks for.
 *
 * It is a request, not a guarantee. A browser without the API (or on a plain
 * HTTP address, where the API is withheld) gives nothing, and the lock is
 * dropped the moment the tab is hidden; so this asks again every time the tab
 * comes back into view, and asks for nothing more than the system is willing
 * to give. Nothing here can stop a lid being closed.
 */

/* Holds the screen awake until the returned function is called. Safe to call
   anywhere: without the API it holds nothing and the release does nothing. */
export function keepScreenAwake() {
  if (typeof navigator === 'undefined' || !navigator.wakeLock
    || typeof document === 'undefined') {
    return () => {}
  }

  let lock = null
  let released = false

  const acquire = async () => {
    // The browser refuses a lock for a hidden page outright, so there is no
    // point asking until it is back in view.
    if (released || lock || document.visibilityState !== 'visible') return
    let held
    try {
      held = await navigator.wakeLock.request('screen')
    } catch {
      // Refused — on battery saver, say. The upload goes on; the screen is
      // the system's to manage.
      return
    }
    // Released while the request was out: let go of what just arrived.
    if (released) {
      held.release().catch(() => {})
      return
    }
    lock = held
    // The system takes the lock back when the tab is hidden, and says so
    // here; the visibility listener below asks again when it is shown.
    held.addEventListener('release', () => { if (lock === held) lock = null })
  }

  const onVisible = () => { if (document.visibilityState === 'visible') acquire() }
  document.addEventListener('visibilitychange', onVisible)
  acquire()

  return () => {
    if (released) return
    released = true
    document.removeEventListener('visibilitychange', onVisible)
    const held = lock
    lock = null
    if (held) held.release().catch(() => {})
  }
}
