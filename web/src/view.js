/* How a folder is drawn, and in what order.

   Both are preferences rather than state: whoever wants a wall of thumbnails
   sorted newest-first wants it again tomorrow, so the answer is kept in the
   browser rather than re-chosen every session. What is kept is three words —
   the view, the sort key and its direction — and nothing whatever about the
   vault, so a locked vault leaves nothing readable behind here either.

   The sorting itself is done in the browser rather than asked of the server.
   A listing is already in hand, complete and small; sending it back to be
   reordered would be a round-trip to answer a question the page can answer
   without one. */

import { useCallback, useEffect, useState } from 'react'

export const VIEWS = ['list', 'grid']

/* What can be sorted on. A folder carries only its name, so the other three
   fall back to the name for folders — see sortFolders. */
export const SORT_KEYS = [
  { key: 'name', label: 'Name', up: 'A to Z', down: 'Z to A' },
  { key: 'size', label: 'Size', up: 'Smallest first', down: 'Largest first' },
  { key: 'modified', label: 'Modified', up: 'Oldest first', down: 'Newest first' },
  { key: 'kind', label: 'Kind', up: 'Grouped by extension', down: 'Grouped by extension, reversed' },
]

/* Which way round each one starts. Nobody asks for a folder sorted by date
   because they want the oldest thing in it, and nobody sorts by size to find
   the smallest — so those two open descending and the two alphabetical ones
   open ascending, which is what every file manager does and what the arrow in
   the menu then makes obvious. */
const NATURAL_DIRECTION = { name: 'asc', size: 'desc', modified: 'desc', kind: 'asc' }

export function naturalDirection(key) {
  return NATURAL_DIRECTION[key] || 'asc'
}

export const DEFAULT_PREFS = { view: 'list', key: 'name', dir: 'asc' }

const STORE_KEY = 'sand.view'

/* Private browsing, a blocked origin, a quota that is already full: any of
   them makes localStorage throw rather than return nothing, and none of them
   is a reason for the file browser not to open. */
function readPrefs() {
  try {
    const stored = JSON.parse(window.localStorage.getItem(STORE_KEY) || 'null')
    if (!stored) return DEFAULT_PREFS
    return {
      view: VIEWS.includes(stored.view) ? stored.view : DEFAULT_PREFS.view,
      key: SORT_KEYS.some((s) => s.key === stored.key) ? stored.key : DEFAULT_PREFS.key,
      dir: stored.dir === 'desc' ? 'desc' : 'asc',
    }
  } catch {
    return DEFAULT_PREFS
  }
}

export function useViewPrefs() {
  const [prefs, setPrefs] = useState(readPrefs)

  useEffect(() => {
    try {
      window.localStorage.setItem(STORE_KEY, JSON.stringify(prefs))
    } catch { /* nothing worth failing an interaction over */ }
  }, [prefs])

  const setView = useCallback((view) => setPrefs((p) => ({ ...p, view })), [])

  /* Choosing the column you are already sorted on flips the direction, which
     is how a column heading behaves everywhere; choosing a different one
     starts it the way round that column is normally wanted. */
  const setSort = useCallback((key) => setPrefs((p) => (
    p.key === key
      ? { ...p, dir: p.dir === 'asc' ? 'desc' : 'asc' }
      : { ...p, key, dir: naturalDirection(key) })), [])

  return [prefs, { setView, setSort }]
}

export function sortDirectionLabel(key, dir) {
  const spec = SORT_KEYS.find((s) => s.key === key)
  if (!spec) return ''
  return dir === 'asc' ? spec.up : spec.down
}

/* Numeric so "clip2" comes before "clip10" rather than after it, and
   base-sensitive so a capital letter does not sort a name into its own group. */
const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

/* Folders are ordered by name whatever the sort is. The listing carries a
   folder as a name and nothing else — there is no size or date to sort it on —
   and inventing one by walking the tree would cost a request per folder to
   answer a question nobody asked. */
export function sortFolders(folders, { dir }) {
  const sign = dir === 'desc' ? -1 : 1
  return [...(folders || [])].sort((a, b) => sign * collator.compare(a, b))
}

export function sortFiles(files, { key, dir }) {
  const sign = dir === 'desc' ? -1 : 1
  return [...(files || [])].sort((a, b) => sign * compareFiles(a, b, key))
}

/* Search results are one list of both kinds, ranked by closeness before they
   ever reached the browser. Folders still lead, and the rest is the chosen
   order — the ranking has already done its work by deciding which hits are in
   the list at all, which is what a truncated result set turns on. */
export function sortHits(hits, prefs) {
  const sign = prefs.dir === 'desc' ? -1 : 1
  // filter() hands back a new array either way, so both sorts are in place on
  // a copy rather than on the results the search returned.
  const folders = (hits || []).filter((h) => h.type === 'folder')
    // Two folders of the same name in different places is ordinary, so the
    // path settles it rather than leaving the pair in whatever order they
    // happened to arrive in.
    .sort((a, b) => sign * (collator.compare(a.name, b.name) || collator.compare(a.path, b.path)))
  const files = (hits || []).filter((h) => h.type !== 'folder')
    .sort((a, b) => sign * compareFiles(a.file, b.file, prefs.key))
  return [...folders, ...files]
}

function compareFiles(a, b, key) {
  switch (key) {
    case 'size':
      return (a.size - b.size) || collator.compare(a.name, b.name)
    case 'modified':
      return (stamp(a.modified_at) - stamp(b.modified_at)) || collator.compare(a.name, b.name)
    case 'kind':
      return collator.compare(extension(a.name), extension(b.name)) || collator.compare(a.name, b.name)
    default:
      return collator.compare(a.name, b.name)
  }
}

// A file with no readable date sorts as the oldest thing there is, rather than
// making every comparison against it a NaN and the whole order arbitrary.
function stamp(iso) {
  const at = Date.parse(iso || '')
  return Number.isNaN(at) ? 0 : at
}

function extension(name = '') {
  const dot = name.lastIndexOf('.')
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : ''
}
