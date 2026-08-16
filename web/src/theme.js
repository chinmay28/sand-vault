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
  /* The wordmark's second half, written rather than set.

     Nefelibata Script first — a nefelibata is a cloud-walker, which is a fair
     description of a vault that lives on other people's clouds. It is a
     licensed font, so it is not in this repository: the build embeds it when a
     copy is sitting in web/fonts (see the README there), and a browser on a
     machine with the desktop font installed finds it by name either way.

     Everything after it ships with an operating system, so the wordmark is
     still written when the font is not there — nothing is ever fetched, which
     is the rule the whole app is held to. The stack walks the handwriting face
     each platform actually has and ends at the generic `cursive`, which every
     browser resolves to something joined. */
  script: "'Nefelibata Script', Nefelibata, 'Snell Roundhand', 'Apple Chancery', " +
    "'Segoe Script', 'Bradley Hand', 'Brush Script MT', 'Lucida Handwriting', " +
    "'URW Chancery L', cursive",
}

/* Each connected account wears a colour no other account wears: the same colour
   marks its card in the sidebar and every part badge for a file it holds, so
   "which three clouds is this file on" is a question you answer by eye.

   An account can be given a colour of its own in the edit menu, which the vault
   stores; this palette is what everything else is drawn from, and what the
   picker offers.

   Twelve hues, three shades each. Every one of them is light enough to carry
   the app's dark text on top, because that is what a part badge is — a number
   in `bg` on the account's colour — and dark enough to hold its own against the
   surface it sits on. Nothing here goes past that on either side: a palette
   with a navy and a pastel in it would offer two colours that cannot actually
   be used. */
export const ACCOUNT_PALETTE = [
  { name: 'Sky', shades: ['#7dd3fc', '#38bdf8', '#0ea5e9'] },
  { name: 'Cyan', shades: ['#67e8f9', '#22d3ee', '#06b6d4'] },
  { name: 'Mint', shades: ['#6ee7b7', '#34d399', '#10b981'] },
  { name: 'Green', shades: ['#86efac', '#4ade80', '#22c55e'] },
  { name: 'Lime', shades: ['#bef264', '#a3e635', '#84cc16'] },
  { name: 'Amber', shades: ['#fcd34d', '#fbbf24', '#f59e0b'] },
  { name: 'Orange', shades: ['#fdba74', '#fb923c', '#f97316'] },
  { name: 'Rose', shades: ['#fda4af', '#fb7185', '#f43f5e'] },
  { name: 'Pink', shades: ['#f9a8d4', '#f472b6', '#ec4899'] },
  { name: 'Orchid', shades: ['#f0abfc', '#e879f9', '#d946ef'] },
  { name: 'Violet', shades: ['#c4b5fd', '#a78bfa', '#8b5cf6'] },
  { name: 'Indigo', shades: ['#a5b4fc', '#818cf8', '#6366f1'] },
]

/* The middle shade of each hue: the twelve the automatic assignment hands out,
   and the row the picker opens on. */
export const ACCOUNT_COLORS = ACCOUNT_PALETTE.map((hue) => hue.shades[1])

/* Names for every swatch, so none of them is an unlabelled square — which is
   unreadable to a screen reader and unpointable over the phone to someone
   else. The middle shade is the hue's plain name; the other two say which way
   they lean. */
export const ACCOUNT_COLOR_NAMES = Object.fromEntries(
  ACCOUNT_PALETTE.flatMap(({ name, shades }) => [
    [shades[0], `${name} light`],
    [shades[1], name],
    [shades[2], `${name} deep`],
  ]),
)

/* What to call a colour that came from the native picker rather than the
   palette: its own hex, which is the only honest name for it. */
export function accountColorName(hex) {
  return ACCOUNT_COLOR_NAMES[hex] || hex
}

function preferredIndex(id) {
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0
  return hash % ACCOUNT_COLORS.length
}

/* A hex colour in the one form everything here compares and stores: lower-case
   "#rrggbb". "" — not a colour — is what an account with no choice of its own
   carries, and what clears a choice. The server normalizes the same way, so a
   colour typed here and a colour read back mean the same string. */
export function normalizeHex(value) {
  const hex = String(value == null ? '' : value).trim().replace(/^#/, '').toLowerCase()
  if (/^[0-9a-f]{6}$/.test(hex)) return `#${hex}`
  if (/^[0-9a-f]{3}$/.test(hex)) return `#${hex[0]}${hex[0]}${hex[1]}${hex[1]}${hex[2]}${hex[2]}`
  return ''
}

/* Account id → colour, rebuilt whenever the account list changes. */
let assigned = new Map()

/* Hand out the colours.

   A colour someone chose belongs to that account, so those are claimed first
   and the automatic assignment walks around them rather than sitting on one.
   Everything left starts at the colour its id hashes to — so a colour stays put
   as other accounts come and go — and anything that would land on a colour
   already spoken for walks forward to the next free one. Sorted by id first, so
   which account keeps a contested colour does not depend on the order the list
   happened to arrive in. */
export function assignAccountColors(providers) {
  const accounts = (providers || [])
    .filter((p) => p && p.id)
    .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
  const taken = new Set()
  const next = new Map()

  for (const account of accounts) {
    const chosen = normalizeHex(account.color)
    if (!chosen) continue
    next.set(account.id, chosen)
    taken.add(chosen)
  }

  for (const { id } of accounts) {
    if (next.has(id)) continue
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

/* Where the automatic assignment starts for an account: the colour its id
   hashes to. What it settles on depends on the whole list, so this is the
   swatch the picker shows against "Automatic" rather than a promise. */
export function autoAccountColor(id) {
  return id ? ACCOUNT_COLORS[preferredIndex(id)] : COLORS.textMuted
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
