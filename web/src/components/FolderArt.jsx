import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, fileIcon } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Empty, Modal, Spinner } from './ui'
import { TILE_POSTER, TILE_SQUARE } from './FileEntry'

/* Choosing the picture a folder wears.

   A folder of films was a row of identical 📁, which is the same thing the
   files inside it looked like before they had posters. So a folder borrows one
   from what is inside it — and borrowing is all it does: what is stored is a
   file id, and the picture drawn is that file's own thumbnail, through the
   endpoint every other picture in the app comes through. Nothing is uploaded by
   choosing one, nothing is erased by changing it.

   Left alone, the vault picks a film from inside the folder and keeps picking
   the same one, so a folder does not change its face every time the listing
   refreshes. This is for when it picked the wrong one — which for a trilogy it
   half the time will, because there is no right one. */

export default function FolderArtPicker({ path, name, onClose, onDone }) {
  const mobile = useIsMobile()

  const [loading, setLoading] = useState(true)
  const [art, setArt] = useState(null)
  const [candidates, setCandidates] = useState([])
  const [truncated, setTruncated] = useState(false)
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState(null)
  // Whether anything was actually changed, so the listing behind is only asked
  // to redraw when there is something new to draw.
  const [changed, setChanged] = useState(false)

  useEffect(() => {
    let live = true
    api.folderArt(path)
      .then((resp) => {
        if (!live) return
        setArt(resp.art || null)
        setCandidates(resp.candidates || [])
        setTruncated(!!resp.truncated)
      })
      .catch((err) => { if (live) setError(err.message) })
      .finally(() => { if (live) setLoading(false) })
    return () => { live = false }
  }, [path])

  /* One shape for the whole grid, the way the file browser does it: a folder
     with films in it is a wall of posters, and a square crop of a poster eats
     the title band at the foot. */
  const aspect = candidates.some((c) => c.film) ? TILE_POSTER : TILE_SQUARE

  const choose = async (id) => {
    setBusy(id || 'auto')
    setError(null)
    try {
      const resp = await api.setFolderArt(path, id)
      setArt(resp.art || null)
      setChanged(true)
      close(true)
    } catch (err) {
      setError(err.message)
      setBusy(null)
    }
  }

  const close = (madeChange) => {
    if (madeChange || changed) onDone?.()
    onClose()
  }

  const chosen = art?.chosen ? art.id : null

  return (
    <Modal
      title="Folder picture"
      subtitle={`${name} — a picture of something inside it`}
      onClose={busy ? undefined : () => close(false)}
      width={560}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={{
        margin: '0 0 14px',
        fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.6,
      }}>
        Nothing is stored by choosing one. The folder points at a file that is already inside it and
        draws that file's own thumbnail, so changing its picture costs nothing and erases nothing.
      </p>

      {loading ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: '40px 0' }}><Spinner size={20} /></div>
      ) : candidates.length === 0 ? (
        <Empty icon="🖼" title="Nothing in here has a picture yet">
          A folder wears a picture of something inside it — a film's poster, or the thumbnail of a
          photograph or a PDF. Look the films up, or upload something with a picture, and this folder
          will have a face.
        </Empty>
      ) : (
        <>
          <div style={{
            display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap',
            marginBottom: '12px',
            fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
          }}>
            <span>
              {chosen
                ? 'Picked by hand'
                /* Says which pile it was drawn from, because a folder of films
                   and a folder of photographs are picked from differently: a
                   film's poster wins wherever there is one. */
                : `Picked by SAND, from the ${art?.film ? 'films' : 'pictures'} inside`}
            </span>
            {chosen && (
              <Button size="sm" variant="ghost" disabled={!!busy} onClick={() => choose('')}>
                {busy === 'auto' ? <Spinner size={10} /> : null}Let SAND choose
              </Button>
            )}
          </div>

          <div style={{
            display: 'grid',
            gridTemplateColumns: `repeat(auto-fill, minmax(${mobile ? 96 : 116}px, 1fr))`,
            gap: mobile ? '8px' : '10px',
            maxHeight: mobile ? '52vh' : '46vh',
            overflowY: 'auto',
            marginBottom: '14px',
          }}>
            {candidates.map((c) => (
              <Candidate
                key={c.id}
                candidate={c}
                aspect={aspect}
                current={art?.id === c.id}
                chosen={chosen === c.id}
                busy={busy === c.id}
                disabled={!!busy}
                onChoose={() => choose(c.id)}
              />
            ))}
          </div>

          {truncated && (
            <Banner tone="info">
              The first {candidates.length} are shown, films first. Everything else under this folder
              could stand for it too.
            </Banner>
          )}
        </>
      )}

      <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
        <Button variant="ghost" disabled={!!busy} onClick={() => close(false)}>Close</Button>
      </div>
    </Modal>
  )
}

/* One thing the folder could be drawn with. Captioned by the film's name where
   there is one, because that is what the picture is of — the file name is the
   hover text, the same bargain the grid of posters makes. */
function Candidate({ candidate, aspect, current, chosen, busy, disabled, onChoose }) {
  const [failed, setFailed] = useState(false)
  const [hover, setHover] = useState(false)

  const label = candidate.title || candidate.name
  const icon = useMemo(() => fileIcon('', candidate.name), [candidate.name])

  return (
    <button
      type="button"
      onClick={onChoose}
      disabled={disabled}
      title={candidate.title ? `${candidate.name} — in ${candidate.dir}` : `In ${candidate.dir}`}
      aria-label={`Draw this folder with ${label}`}
      onPointerEnter={() => setHover(true)}
      onPointerLeave={() => setHover(false)}
      style={{
        display: 'flex', flexDirection: 'column',
        padding: 0, overflow: 'hidden',
        borderRadius: '8px',
        textAlign: 'left',
        background: current ? `${COLORS.accent}1f` : COLORS.surface,
        border: `1px solid ${current ? COLORS.accent : hover ? COLORS.borderBright : COLORS.border}`,
        cursor: disabled ? 'wait' : 'pointer',
        opacity: disabled && !busy ? 0.6 : 1,
      }}
    >
      <span style={{
        position: 'relative',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        width: '100%', aspectRatio: aspect, overflow: 'hidden',
        background: COLORS.bg, borderBottom: `1px solid ${COLORS.border}`,
        fontSize: '28px',
      }}>
        {failed ? icon : (
          <img
            src={api.thumbURL(candidate.id)}
            alt=""
            loading="lazy"
            decoding="async"
            onError={() => setFailed(true)}
            style={{ width: '100%', height: '100%', objectFit: 'cover', display: 'block' }}
          />
        )}
        {busy && (
          <span style={{
            position: 'absolute', inset: 0,
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            background: 'rgba(10, 14, 23, 0.72)',
          }}><Spinner size={16} /></span>
        )}
      </span>

      <span style={{ display: 'block', padding: '6px 7px 7px', minWidth: 0 }}>
        <span style={{
          display: 'block',
          fontFamily: FONT.mono, fontSize: '10.5px',
          color: COLORS.text,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{label}</span>
        <span style={{
          display: 'block', marginTop: '2px',
          fontFamily: FONT.mono, fontSize: '9.5px', color: COLORS.textMuted,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>
          {chosen ? 'chosen' : current ? 'in use' : candidate.year || ''}
        </span>
      </span>
    </button>
  )
}
