import React from 'react'
import { COLORS, FONT } from '../theme'
import { Banner, CopyField, Modal } from './ui'

/* The vault as a drive. `sand serve --webdav` puts the same files behind a
   WebDAV share, and this is where someone finds out how to open it.

   The address is built from the page's own origin rather than from anything the
   server reports about itself. Whatever host this app was reached on is the one
   that reaches it — a name the server guessed for itself might resolve nowhere
   else, and a tailnet or reverse proxy makes that near certain. */
export function mountURL(path) {
  if (!path) return ''
  return `${window.location.origin}${path}`
}

/* Whether the address someone is about to paste elsewhere sends the password in
   the clear. Loopback is exempt because nothing crosses a network to reach it,
   which is the same line browsers draw for a secure context. */
function isPlaintext(origin) {
  try {
    const url = new URL(origin)
    if (url.protocol === 'https:') return false
    return !['localhost', '127.0.0.1', '[::1]', '::1'].includes(url.hostname)
  } catch {
    return false
  }
}

const STEPS = [
  {
    what: 'macOS',
    how: 'Finder → Go → Connect to Server, paste the address, then Connect.',
  },
  {
    what: 'Windows',
    how: 'File Explorer → right-click This PC → Add a network location, paste the address.',
  },
  {
    what: 'Linux',
    how: 'Files → Other Locations → Connect to Server, or gio mount with the address as dav:// or davs://.',
  },
  {
    what: 'VLC',
    how: 'Open Network Stream, paste the address, and add the filename — or browse the share from the sidebar.',
  },
  {
    what: 'iOS / tvOS',
    how: 'VLC or Infuse → add a network share, choose WebDAV, and give it the address.',
  },
]

export default function MountDrive({ path, onClose, zIndex }) {
  const url = mountURL(path)
  const plaintext = isPlaintext(window.location.origin)

  return (
    <Modal
      title="Mount as a drive"
      subtitle="Open the vault in a file manager or a player instead of the browser"
      onClose={onClose}
      width={560}
      zIndex={zIndex}
    >
      <CopyField
        label="Address"
        value={url}
        help="Sign in with any username and your vault password."
      />

      {plaintext && (
        <Banner tone="warn">
          This address is plain HTTP, and a mounted share sends your vault
          password on <strong>every</strong> request rather than once at
          sign-in. Put TLS in front of SAND — Tailscale Serve, or the nginx
          template in the repo — and mount the <code>https://</code> address
          instead.
        </Banner>
      )}

      <div style={{ marginTop: '18px' }}>
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 700,
          letterSpacing: '1.5px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
          marginBottom: '10px',
        }}>Where to paste it</div>

        <dl style={{ margin: 0, display: 'grid', gap: '10px' }}>
          {STEPS.map(({ what, how }) => (
            <div key={what} style={{ display: 'flex', gap: '12px', alignItems: 'baseline' }}>
              <dt style={{
                fontFamily: FONT.mono,
                fontSize: '11px',
                color: COLORS.textDim,
                minWidth: '74px',
                flexShrink: 0,
              }}>{what}</dt>
              <dd style={{
                margin: 0,
                fontFamily: FONT.sans,
                fontSize: '12px',
                lineHeight: 1.6,
                color: COLORS.textMuted,
              }}>{how}</dd>
            </div>
          ))}
        </dl>
      </div>

      <p style={{
        fontFamily: FONT.sans,
        fontSize: '11px',
        lineHeight: 1.7,
        color: COLORS.textMuted,
        marginTop: '18px',
        marginBottom: 0,
      }}>
        Seeking works: a player asking for the middle of a film fetches only the
        parts covering that stretch. Copying files in and out streams rather
        than buffering, so size is bounded by disk rather than memory. Renaming
        and moving cost nothing — the stored parts never travel.
        {' '}
        <span style={{ color: COLORS.textDim }}>
          Media servers like Jellyfin want a real filesystem rather than an
          address, so they need a mount SAND does not offer yet.
        </span>
      </p>
    </Modal>
  )
}
