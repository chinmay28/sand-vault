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

/* Each connected account gets a stable colour so a file's three part badges
   read as "which account holds this" at a glance. */
const ACCOUNT_COLORS = [
  '#38bdf8', '#a78bfa', '#34d399', '#fb7185',
  '#fbbf24', '#22d3ee', '#f472b6', '#a3e635',
]

export function accountColor(id) {
  if (!id) return COLORS.textMuted
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0
  return ACCOUNT_COLORS[hash % ACCOUNT_COLORS.length]
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
