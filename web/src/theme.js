/* Shared visual language for the SAND browser. */

export const COLORS = {
  bg: '#0a0e17',
  surface: '#111827',
  surfaceHover: '#161f2f',
  surfaceRaised: '#1a2332',
  border: '#1e2d3d',
  borderBright: '#2b3f55',
  text: '#e2e8f0',
  textDim: '#94a3b8',
  textMuted: '#64748b',
  accent: '#d97706',
  accentBright: '#f59e0b',
  accentDim: '#92400e',
  error: '#ef4444',
  warn: '#eab308',
  success: '#22c55e',
  info: '#38bdf8',
}

/* System font stacks only — nothing is fetched from a CDN, so opening the
   vault makes no third-party requests at all. */
export const FONT = {
  mono: "ui-monospace, 'SF Mono', 'JetBrains Mono', 'Fira Code', Menlo, Consolas, monospace",
  sans: "system-ui, -apple-system, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif",
}

/* Each connected account wears a colour no other account wears: the same colour
   marks its card in the sidebar and every part badge for a file it holds, so
   "which three clouds is this file on" is a question you answer by eye. */
const ACCOUNT_COLORS = [
  '#38bdf8', '#a78bfa', '#34d399', '#fb7185',
  '#fbbf24', '#22d3ee', '#f472b6', '#a3e635',
  '#818cf8', '#4ade80', '#e879f9', '#fb923c',
]

function preferredIndex(id) {
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0
  return hash % ACCOUNT_COLORS.length
}

/* Account id → colour, rebuilt whenever the account list changes. */
let assigned = new Map()

/* Hand out the colours. Every account starts at the one its id hashes to — so
   a colour stays put as other accounts come and go — and anything that would
   land on a colour already spoken for walks forward to the next free one.
   Sorted by id first, so which account keeps a contested colour does not depend
   on the order the list happened to arrive in. */
export function assignAccountColors(providers) {
  const ids = (providers || []).map((p) => p.id).filter(Boolean).sort()
  const taken = new Set()
  const next = new Map()

  for (const id of ids) {
    const start = preferredIndex(id)
    // Past the palette's length every colour is taken and the walk finds
    // nothing free; the hashed colour repeats rather than a badge going blank.
    let color = ACCOUNT_COLORS[start]
    for (let step = 0; step < ACCOUNT_COLORS.length; step++) {
      const candidate = ACCOUNT_COLORS[(start + step) % ACCOUNT_COLORS.length]
      if (!taken.has(candidate)) { color = candidate; break }
    }
    taken.add(color)
    next.set(id, color)
  }

  assigned = next
}

export function accountColor(id) {
  if (!id) return COLORS.textMuted
  // A part can name an account that is no longer connected, and the first
  // render happens before the account list has arrived. Both fall back to the
  // hashed colour, which is where the assignment above starts from anyway.
  return assigned.get(id) || ACCOUNT_COLORS[preferredIndex(id)]
}

export const KIND_ICONS = {
  local: '🖴',
  s3: '☁',
  webdav: '🌐',
  gdrive: '▲',
  dropbox: '◈',
  onedrive: '⬡',
  box: '▣',
  proton: '◉',
}

export function formatBytes(bytes) {
  if (!bytes) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(k)), sizes.length - 1)
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`
}

export function formatDate(iso) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const now = new Date()
  const sameYear = d.getFullYear() === now.getFullYear()
  return d.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' }),
  }) + ' ' + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

/* Which preview a MIME type gets, if any. */
export function previewKind(mime = '', name = '') {
  const type = mime.split(';')[0].trim().toLowerCase()
  if (type.startsWith('image/') && type !== 'image/svg+xml') return 'image'
  if (type.startsWith('video/')) return 'video'
  if (type.startsWith('audio/')) return 'audio'
  if (type === 'application/pdf') return 'pdf'
  if (type.startsWith('text/') || TEXTUAL.has(type)) return 'text'
  if (/\.(txt|md|json|ya?ml|toml|ini|conf|log|csv|tsv|go|js|jsx|ts|tsx|py|rs|c|h|cpp|java|rb|sh)$/i.test(name)) {
    return 'text'
  }
  return null
}

/* Which files are worth handing to a media player.

   Deliberately wider than previewKind, and on a different question. That one
   asks what this browser can draw, and it has to be strict — it is choosing
   between a <video> element and an honest "no preview here". This one asks what
   VLC could play, and VLC's answer is nearly everything.

   Hence the extension test rather than the type alone: a .mkv usually arrives
   with no recognised type at all, since Go's built-in table stops at the
   handful of web formats and a server without an /etc/mime.types has nothing
   else to consult. That is precisely the file a browser cannot play and a
   player can. */
export function isPlayable(mime = '', name = '') {
  const type = mime.split(';')[0].trim().toLowerCase()
  if (type.startsWith('video/') || type.startsWith('audio/')) return true
  return /\.(mp4|m4v|mov|mkv|webm|avi|wmv|flv|mpe?g|m2ts|ts|ogv|3gp|mp3|m4a|aac|flac|wav|ogg|oga|opus|wma)$/i
    .test(name)
}

const TEXTUAL = new Set([
  'application/json',
  'application/x-yaml',
  'application/toml',
  'application/x-sh',
])

export function fileIcon(mime = '', name = '') {
  switch (previewKind(mime, name)) {
    case 'image': return '🖼'
    case 'video': return '🎬'
    case 'audio': return '🎵'
    case 'pdf': return '📕'
    case 'text': return '📄'
    default: return '📦'
  }
}
