/* What was chosen to be uploaded, before any of it is sent.
 *
 * A browser will not hand over a folder. Both ways of choosing one — the
 * picker with `webkitdirectory` set, and dropping it on the window — hand over
 * the files inside it instead, and it is up to us to keep the shape they came
 * in. So a choice is not a list of files here but a list of files each with the
 * path it had inside whatever was chosen, plus the folders themselves, because
 * a folder holding nothing has no file to carry it and would otherwise be the
 * one part of a dropped tree that did not arrive.
 *
 * A file picked on its own is the same thing with a path that is just its name,
 * which is why nothing downstream needs to know which of the two it was.
 */

/* A ceiling on how much one choice can be. Not a policy about what is worth
   uploading — a guard on the walk, so a folder pointed at a home directory
   fails saying so rather than filling memory while nothing appears to happen. */
export const MAX_FILES = 20000

export class TooManyFiles extends Error {
  constructor(limit) {
    super(`That folder holds more than ${limit.toLocaleString()} files — upload it a part at a time.`)
    this.name = 'TooManyFiles'
  }
}

/* From the file picker. `webkitRelativePath` is set on every file when the
   input asked for a directory and empty when it asked for files, so the two
   cases are the same code. Empty folders never reach us here: the picker only
   reports files, which is a browser limit and not one worth pretending away. */
export function picksFromInput(fileList) {
  const files = Array.from(fileList || []).map((file) => ({
    file,
    path: file.webkitRelativePath || file.name,
  }))
  return { files, dirs: [] }
}

/* From a drop. Anything dropped may be a folder, and only the entries API can
   say so or look inside one.
 *
 * The entries have to be taken out of the DataTransfer synchronously — the list
 * is emptied when the drop event finishes, so grabbing them after the first
 * await gets nothing. That is why this reads the items first and only then
 * starts walking.
 */
export async function picksFromDrop(dataTransfer) {
  const items = Array.from(dataTransfer?.items || [])
  const entries = items
    .filter((item) => item.kind === 'file')
    .map((item) => (item.webkitGetAsEntry ? item.webkitGetAsEntry() : null))
    .filter(Boolean)
  // Taken now for the same reason, so the fallback below still has something
  // to fall back to.
  const dropped = Array.from(dataTransfer?.files || [])

  // A browser without the entries API, or a drag that carried no real entry:
  // whatever files came through, as files. Folders are silently absent there,
  // which is what dropping a folder has always done.
  if (!entries.length) return picksFromInput(dropped)

  const out = { files: [], dirs: [] }
  for (const entry of entries) await walk(entry, '', out)
  return out
}

async function walk(entry, prefix, out) {
  if (out.files.length > MAX_FILES) throw new TooManyFiles(MAX_FILES)

  if (entry.isFile) {
    const file = await new Promise((resolve, reject) => entry.file(resolve, reject))
    out.files.push({ file, path: prefix + entry.name })
    return
  }
  if (!entry.isDirectory) return

  const dir = prefix + entry.name
  out.dirs.push(dir)
  const reader = entry.createReader()
  /* readEntries hands back a page at a time and signals the end with an empty
     page, so it has to be asked until it gives one — a directory of 300 files
     arrives in chunks of 100 and reading it once would lose two thirds. */
  for (;;) {
    const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject))
    if (!batch.length) break
    for (const child of batch) await walk(child, `${dir}/`, out)
  }
}

/* The folders a choice needs that no file would make on its own. Sending every
   folder would be sending most of them twice, since a file's own path already
   names the ones above it. */
export function emptyDirs({ files, dirs }) {
  const held = new Set()
  for (const { path } of files) {
    const parts = path.split('/')
    for (let i = 1; i < parts.length; i++) held.add(parts.slice(0, i).join('/'))
  }
  return dirs.filter((dir) => !held.has(dir))
}

/* How to say what is about to go up. One file is its name; a folder is the
   folder, because "photos and 419 others" is not what was chosen. */
export function describePicks({ files, dirs }) {
  const roots = new Set()
  for (const { path } of files) roots.add(path.includes('/') ? path.split('/')[0] : null)
  for (const dir of dirs) roots.add(dir.split('/')[0])

  const folders = [...roots].filter(Boolean)
  const loose = files.filter(({ path }) => !path.includes('/'))

  if (!folders.length) {
    return loose.length === 1 ? loose[0].file.name : `${loose.length} files`
  }
  const folder = folders.length === 1 ? folders[0] : `${folders.length} folders`
  if (!loose.length) return folder
  return `${folder} and ${loose.length} file${loose.length === 1 ? '' : 's'}`
}

/* How to say what was not sent because the destination already had it. Each
   skipped file matters to whoever chose it, but four hundred of them are a
   wall of text, not a notice — so a few are named and the rest are counted. */
export function describeSkips(skipped) {
  const paths = skipped.map(({ path }) => path)
  if (paths.length === 1) {
    return `Skipped ${paths[0]} — already stored here with the same name and size.`
  }
  const named = paths.slice(0, 3).join(', ')
  const more = paths.length - 3
  return `Skipped ${paths.length} files already stored here with the same name and size: `
    + `${named}${more > 0 ? ` and ${more} more` : ''}.`
}

/* The total, which is the file bytes: a folder costs nothing of its own. */
export function totalBytes({ files }) {
  return files.reduce((sum, { file }) => sum + file.size, 0)
}

/* How much of a choice one request carries.
 *
 * Everything picked used to go up as a single multipart request, which is fine
 * for a file and quietly wrong for a folder: a hundred photos and a gigabyte
 * and a half became one body that had to arrive whole before any of it was
 * stored, spooled to a temp file on the server before a byte of it was looked
 * at, and failed as one thing when it failed at all. Sending it in batches
 * bounds all three — what has arrived is stored and listed while the rest is
 * still going, and what fails takes only its own batch down with it.
 *
 * The byte budget is well under the server's own ceiling on a request
 * (DefaultMaxUploadSize), so a batch is never refused for its size; the file
 * count keeps a batch of small files from being a thousand of them. */
export const BATCH_BYTES = 256 * 1024 * 1024
export const BATCH_FILES = 24

/* Cuts the files of a choice into the requests that will carry them, in the
   order they were chosen so a folder arrives roughly top-down.

   A file bigger than the whole budget still goes — alone, in a batch of its
   own, because splitting a single file across requests is not something this
   endpoint can do and refusing it here would be refusing the one upload most
   worth having. */
export function batchPicks(files, { bytes = BATCH_BYTES, count = BATCH_FILES } = {}) {
  const batches = []
  let batch = []
  let size = 0

  for (const pick of files) {
    const cost = pick.file?.size || 0
    if (batch.length && (batch.length >= count || size + cost > bytes)) {
      batches.push(batch)
      batch = []
      size = 0
    }
    batch.push(pick)
    size += cost
  }
  if (batch.length) batches.push(batch)
  return batches
}

/* What a batch weighs, for working out how far along a run of them is. */
export function batchBytes(batch) {
  return batch.reduce((sum, { file }) => sum + (file?.size || 0), 0)
}
