import { formatBytes } from './theme'

/* How much more will fit on a cloud, said in a few words.

   The server works the figure out — see internal/vault/space.go — because it is
   the one place that has all three of the things that might answer: what the
   backend reported when it was last pinged, what a count of a bucket found in
   it, and what the index says SAND itself has written there. What is left here
   is putting it into the handful of words a row has space for, and deciding
   what that means for a file about to be split across several of them.

   `source` is where the number came from, and it is not a detail. A drive
   saying it has 40 GB left is a fact about the drive; a quota saying so is a
   line you drew yourself, and the two are fixed in completely different places
   when one of them looks wrong. */
export const SPACE_REPORTED = 'reported'
export const SPACE_DECLARED = 'declared'
export const SPACE_QUOTA = 'quota'

/* The server's answer, normalised so a provider from an older response — or one
   drawn before the first ping came back — reads as "nobody knows" rather than
   as an empty account. */
export function spaceOf(provider) {
  const space = provider?.space || {}
  const source = space.source || ''
  return {
    known: Boolean(source),
    source,
    free: Math.max(0, space.free || 0),
    used: Math.max(0, space.used || 0),
    total: Math.max(0, space.total || 0),
    over: Math.max(0, space.over || 0),
  }
}

/* Room left, in the four or five words a cloud's row has for it.

   An account past the quota set for it says so instead of saying "0 B free":
   zero free reads as a full drive, and this is not a full drive — it is a line
   somebody drew that has been crossed, which is a different thing and is fixed
   by raising the line or moving files off. */
export function spaceLabel(provider) {
  const space = spaceOf(provider)
  if (!space.known) return 'space unknown'
  if (space.over > 0) return `over quota by ${formatBytes(space.over)}`
  if (space.source === SPACE_QUOTA) return `${formatBytes(space.free)} free (quota)`
  return `${formatBytes(space.free)} free`
}

/* The same thing at length, for the row's tooltip — where the figure came from
   is worth one sentence somewhere, and a row has no space for it. */
export function spaceTitle(provider) {
  const space = spaceOf(provider)
  switch (space.source) {
    case SPACE_REPORTED:
      return `${formatBytes(space.free)} of ${formatBytes(space.total)} left, as the account reports it`
    case SPACE_DECLARED:
      return `${formatBytes(space.free)} left of the ${formatBytes(space.total)} capacity you set, `
        + 'measured against what a count of the account found in it'
    case SPACE_QUOTA:
      return space.over > 0
        ? `${formatBytes(space.used)} of parts here against the ${formatBytes(space.total)} quota `
          + `you set — ${formatBytes(space.over)} past it`
        : `${formatBytes(space.free)} left of the ${formatBytes(space.total)} quota you set for this account`
    default:
      return 'This account cannot say how much room is left, and no quota has been set for it. '
        + 'Edit the account to set one.'
  }
}

/* Roughly what one cloud receives when a file is cut k-of-n: a shard carries a
   kth of the file, whatever n is, which is exactly why widening a spread costs
   nothing per cloud and lowering k costs everything.

   Rounded up, and the chunk headers ignored — this is the figure a warning is
   drawn against, and a warning that fires a few kilobytes early is better than
   one that misses by the same margin. */
export function shardShare(bytes, scheme) {
  if (!scheme?.data || bytes <= 0) return 0
  return Math.ceil(bytes / scheme.data)
}

/* The chosen clouds this upload does not comfortably fit on, worst first.

   Only the ones that can actually say. A cloud with no reported figure, no
   count and no quota is not "fine" and is not "full" — it is unknown, and
   guessing either way would be inventing the answer the whole feature exists to
   stop inventing. Those are counted separately so the dialog can say how many
   it could not check. */
export function tightClouds(providers, selected, scheme, bytes) {
  const share = shardShare(bytes, scheme)
  const chosen = new Set(selected || [])
  const tight = []
  let unknown = 0

  for (const provider of providers || []) {
    if (!chosen.has(provider.id)) continue
    const space = spaceOf(provider)
    if (!space.known) {
      unknown++
      continue
    }
    if (share > 0 && space.free < share) tight.push({ provider, space })
  }

  tight.sort((a, b) => a.space.free - b.space.free)
  return { share, tight, unknown }
}
