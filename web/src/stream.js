/* Handing a file to a player that is not this browser.

   Opening a film in VLC means giving VLC an address, and until now that meant
   reading one out of the mount dialog, adding the filename by hand, and typing
   the result into Open Network Stream. The server will mint a link for one file
   instead (see internal/server/stream.go); this is the half that gets it into
   VLC without anyone transcribing anything.

   Three platforms, three different answers, and none of them is a plain link:
   a media player is not a web page, so a browser will not navigate to one. */

/* Turn the server's relative path into the address a player will use. Built
   against the origin this page was reached on rather than anything the server
   says about itself — whatever host got you here is the host that will work
   from wherever the player is running, and a tailnet or a reverse proxy makes
   any name the server guessed for itself very likely to resolve nowhere. */
export function absoluteURL(path) {
  return new URL(path, window.location.href).href
}

/* iPadOS 13 and later report themselves as a Mac, so the platform string alone
   is not enough; a Mac with a touchscreen is the thing that does not exist. */
export function isIOS() {
  const ua = navigator.userAgent || ''
  if (/iPad|iPhone|iPod/.test(ua)) return true
  return /Mac/.test(ua) && navigator.maxTouchPoints > 1
}

/* Added to a home screen, the vault runs as its own window: no address bar,
   no tabs, no back button — and, on iOS, no downloads either, so anything the
   window is pointed at that it cannot show becomes a dead end with nothing to
   press. Safari's own flag and the display-mode query between them cover
   every platform that does this. */
export function isStandalone() {
  if (typeof navigator !== 'undefined' && navigator.standalone) return true
  return typeof window !== 'undefined'
    && typeof window.matchMedia === 'function'
    && window.matchMedia('(display-mode: standalone)').matches
}

/* How the handoff is made on this device.

   iOS has a documented entry point — vlc-x-callback://x-callback-url/stream —
   which takes the address as a parameter, so nothing has to be guessed about
   how VLC will parse what it is given.

   Android has no such thing, but an intent: URL names the target package
   outright and carries the scheme in a field of its own. A bare vlc:// address
   does not: VLC strips its own scheme and assumes http for whatever is left, so
   an https server handed over that way would be fetched in the clear.

   A desktop has neither, and this is the one case where guessing is avoidable
   altogether. VLC registers itself for playlist files on every desktop it
   installs on, so the handoff there is a two-line .m3u naming the address:
   saved, then opened like any other download. */
export function vlcHandoff(url) {
  if (isIOS()) {
    return {
      kind: 'deeplink',
      href: `vlc-x-callback://x-callback-url/stream?url=${encodeURIComponent(url)}`,
    }
  }

  if (/Android/.test(navigator.userAgent || '')) {
    const parsed = new URL(url)
    return {
      kind: 'deeplink',
      href: `intent://${url.slice(parsed.protocol.length + 2)}` +
        `#Intent;scheme=${parsed.protocol.slice(0, -1)};package=org.videolan.vlc;end`,
    }
  }

  return { kind: 'playlist' }
}

/* A playlist holding the one address, which is what a desktop opens VLC with.

   #EXTINF carries the name so the player has something to show while it works
   out what it is playing; -1 is "length unknown", which is honest — the length
   is the file's business, not the playlist's. */
export function playlistFor(url, name) {
  const title = String(name || 'stream').replace(/[\r\n]+/g, ' ')
  return new Blob([`#EXTM3U\n#EXTINF:-1,${title}\n${url}\n`], { type: 'audio/x-mpegurl' })
}

/* How long to wait before deciding the handoff went nowhere. */
const HANDOFF_GRACE_MS = 1600

/* Follow a scheme this page cannot navigate to.

   An anchor click rather than assigning to location: a custom scheme assigned
   to location.href replaces the document on some browsers before the OS gets a
   chance to intercept it, and this app is often running from a home screen with
   no back button to recover with. */
function followScheme(href) {
  const link = document.createElement('a')
  link.href = href
  link.style.display = 'none'
  // Firefox only follows a synthetic click on an anchor that is in the document.
  document.body.appendChild(link)
  link.click()
  link.remove()
}

/* Attempt the handoff, and report whether an app appears to have taken it.

   Nothing tells a page that a URL scheme went nowhere — an unhandled scheme is
   a navigation that quietly does not happen. What does change when another app
   opens is that this page stops being the visible one, so its still being
   visible a moment later is the only evidence available that VLC is not
   installed. It is evidence and not proof, which is why the caller offers the
   address either way rather than insisting nothing happened. */
export function attemptDeepLink(href) {
  return new Promise((resolve) => {
    let left = document.visibilityState === 'hidden'
    const onHidden = () => { if (document.visibilityState === 'hidden') left = true }
    const onLeave = () => { left = true }

    document.addEventListener('visibilitychange', onHidden)
    window.addEventListener('pagehide', onLeave)

    followScheme(href)

    window.setTimeout(() => {
      document.removeEventListener('visibilitychange', onHidden)
      window.removeEventListener('pagehide', onLeave)
      resolve(left)
    }, HANDOFF_GRACE_MS)
  })
}
