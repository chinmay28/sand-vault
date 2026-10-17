/* Which data key the vault being browsed seals new files under.

   A password change rotates that key and re-encrypts the stored files onto it
   one at a time, so for a while some files are still sealed under the key the
   previous password protected. Each entry in a listing carries its own key_id,
   and the vault status carries the current one; the difference is the "prev
   key" marker a row draws. The current key is kept here, module-wide, the way
   the account colours are: every row needs it and none of them should have to
   be handed it. The app sets it whenever the status or the vault being browsed
   changes. */

let current = ''

export function setCurrentKey(id) {
  current = id || ''
}

/* Whether a file is still sealed under a key other than the current one. False
   when the current key is not known, so a vault that has never rotated — or a
   listing drawn before the status arrived — marks nothing. */
export function onPreviousKey(file) {
  if (!current || !file) return false
  return (file.key_id || '') !== current
}
