/* Where a file's shards are, answered from the index before any cloud has
   been asked.

   The index already records every shard's account, key and size — that is
   how the listing draws its badges. What it cannot say is whether each one
   is still answering, which takes a round trip per account. The dialog
   that shows a file's placement used to wait for that round trip before
   drawing anything, as if the locations were part of the answer rather
   than already known. These helpers shape the index's record the way the
   health reply is shaped, so the dialog draws it at once and the probe
   fills in reachability when it lands.

   A shard with `present: null` is one nobody has asked about yet; the
   health reply says true or false. A placement with `pending: true` has
   no verdict on whether it can be rebuilt. */

/* The code a placement was cut with, from its own record or a default. */
function schemeOf(data, total, fallback) {
  if (data > 0 && total > 0) return { data, total }
  return fallback
}

/* One placement — the parts or the replica — as the index has it. `scheme`
   is `{ data, total }`; `chunkCount` is 0 or 1 for a file stored whole. */
export function indexPlacement(shards, scheme, chunkCount = 0) {
  const list = Array.isArray(shards) ? shards : []
  const chunked = chunkCount > 1
  return {
    pending: true,
    recoverable: null,
    shards: list.map((s) => ({ ...s, present: null, observed_size: null })),
    scheme: scheme ? `${scheme.data}-of-${scheme.total}` : '',
    needed: scheme ? scheme.data : 0,
    spare: 0,
    chunks_sampled: 0,
    chunk_count: chunked ? chunkCount : 0,
  }
}

/* The file's replica as the index has it, in the shape of the health
   reply's `replica`, or null for a file without one. */
export function indexReplica(replica) {
  if (!replica) return null
  const scheme = schemeOf(replica.data_shards, replica.total_shards, null)
  return {
    ...indexPlacement(replica.shards, scheme, replica.chunk_count),
    whole: replica.total_shards === 1,
    previous_key: false,
    created_at: replica.created_at || null,
  }
}

/* What one row knows about its shard: `present`, `missing`, `checking`
   while the probe is still out, or `unknown` once it has failed without
   saying. */
export function shardState(shard, checking) {
  if (shard.present === true) return 'present'
  if (shard.present === false) return 'missing'
  return checking ? 'checking' : 'unknown'
}

/* The machine an account lives on when it is not this one, from the account
   list's own say-so (`elsewhere` on each account): empty for every cloud and
   for every disk that is plugged into this machine. A row and the inspector
   both read it to say where a shard is rather than that it is missing. */
export function homeOf(providers, id) {
  return providers?.find((p) => p.id === id)?.elsewhere || ''
}
