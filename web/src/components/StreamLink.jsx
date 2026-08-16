import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { saveBlob } from '../download'
import { absoluteURL, attemptDeepLink, playlistFor, vlcHandoff } from '../stream'
import { Banner, Button, CopyField, Modal, Spinner } from './ui'

/* Play a stored file somewhere that is not this browser.

   Two things were being done by hand and are done here instead: opening VLC on
   the file, and getting the file's address onto the clipboard. They are one
   dialog because they are one question — the address is what VLC is being
   handed, so a handoff that does not land leaves you looking at exactly the
   thing you would have gone looking for.

   `autoplay` is the difference between the two ways in. "Stream in VLC" opens
   this already reaching for the player; "Copy link" opens it sitting still. */
export default function StreamLink({ file, autoplay, zIndex, onClose }) {
  const mobile = useIsMobile()
  const [link, setLink] = useState(null)
  const [error, setError] = useState(null)
  /* idle · handing (waiting to see whether an app took it) · gone (one did,
     as far as anything here can tell) · no-app (nothing answered). */
  const [state, setState] = useState('idle')
  const started = useRef(false)

  useEffect(() => {
    let cancelled = false
    api.streamLink(file.id)
      .then((resp) => { if (!cancelled) setLink({ ...resp, address: absoluteURL(resp.url) }) })
      .catch((err) => { if (!cancelled) setError(err.message) })
    return () => { cancelled = true }
  }, [file.id])

  // Held steady across renders, so the effect below is not woken by a fresh
  // object describing the same device.
  const handoff = useMemo(() => (link ? vlcHandoff(link.address) : null), [link])

  const play = useCallback(async () => {
    if (!link) return

    /* A desktop is handed a playlist rather than a scheme, and saving a file
       nobody asked for is not something to do on a dialog opening — so only
       the deep link is ever taken automatically, and the button below is how a
       desktop starts one. */
    if (handoff.kind === 'playlist') {
      saveBlob(playlistFor(link.address, file.name), `${file.name}.m3u`)
      setState('gone')
      return
    }

    setState('handing')
    setState(await attemptDeepLink(handoff.href) ? 'gone' : 'no-app')
  }, [file.name, handoff, link])

  // Once, on the first link to arrive, and never again for the life of the
  // dialog: re-running this on a re-render would fire VLC at the file twice.
  useEffect(() => {
    if (!autoplay || !handoff || started.current) return
    if (handoff.kind !== 'deeplink') return
    started.current = true
    play()
  }, [autoplay, handoff, play])

  return (
    <Modal
      title="Stream to a player"
      subtitle={file.name}
      onClose={onClose}
      width={540}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {!error && !link && (
        <div style={{ padding: '32px', textAlign: 'center' }}><Spinner size={20} /></div>
      )}

      {link && (
        <>
          {state === 'handing' && <Banner tone="info">Opening VLC…</Banner>}
          {state === 'gone' && (
            <Banner tone="success">
              {handoff.kind === 'playlist'
                ? <>Playlist saved as <code>{file.name}.m3u</code> — open it and VLC starts on this file.</>
                : 'Handed to VLC.'}
            </Banner>
          )}
          {state === 'no-app' && (
            <Banner tone="warn">
              VLC did not answer. Install it, or paste the address below into
              VLC's <strong>Open Network Stream</strong> — the link works in any
              player that takes a URL.
            </Banner>
          )}

          <CopyField
            label="Stream address"
            value={link.address}
            help="Plays on its own — a player following this needs no password."
          />

          <div style={{
            display: 'flex',
            gap: '8px',
            justifyContent: 'flex-end',
            flexWrap: 'wrap',
            marginTop: '4px',
          }}>
            <Button variant="ghost" onClick={onClose}
              style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Close</Button>

            {/* A desktop already has the playlist behind the primary button;
                offering it twice would be the same button beside itself. */}
            {handoff.kind === 'deeplink' && (
              <Button
                onClick={() => {
                  saveBlob(playlistFor(link.address, file.name), `${file.name}.m3u`)
                }}
                title="A playlist file, for a player that did not take the handoff"
                style={mobile ? { flex: 1, justifyContent: 'center' } : null}
              >⤓ .m3u</Button>
            )}

            <Button
              variant="primary"
              onClick={play}
              disabled={state === 'handing'}
              title={handoff.kind === 'playlist'
                ? 'Save a playlist naming this address — opening it starts VLC'
                : 'Hand this address to VLC'}
              style={mobile ? { flex: 2, justifyContent: 'center' } : null}
            >{state === 'handing' ? <><Spinner size={12} color={COLORS.bg} /> Opening…</> : '▶ Open in VLC'}</Button>
          </div>

          <p style={{
            margin: '16px 0 0',
            fontFamily: FONT.sans,
            fontSize: '11px',
            lineHeight: 1.7,
            color: COLORS.textMuted,
          }}>
            Seeking works: a player asking for the middle of a film fetches only
            the parts covering that stretch, not the whole file.
            {' '}
            <span style={{ color: COLORS.textDim }}>
              The address stands for this one file and carries its own
              credential, so anyone holding it can play the file without your
              vault password. It stops working {expiryPhrase(link.expires_in)}{' '}
              after it was last used, and immediately when the vault locks.
            </span>
          </p>
        </>
      )}
    </Modal>
  )
}

/* How long a link lasts, in words. Rounded rather than exact — the deadline
   slides forward every time the link is used, so a number to the minute would
   be a precise answer to the wrong question. */
function expiryPhrase(seconds) {
  const hours = Math.round((seconds || 0) / 3600)
  if (hours >= 2) return `${hours} hours`
  if (hours === 1) return 'an hour'
  return 'shortly'
}
