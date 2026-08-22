import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes, isPlayable, previewKind } from '../theme'
import { useIsMobile } from '../hooks'
import { api } from '../api'
import { useDownload } from '../download'
import { thumbnailFromElement } from '../thumbs'
import PdfPreview from './PdfPreview'
import StreamLink from './StreamLink'
import { RelocateClouds, fileScheme, schemeName, storedParts } from './CloudSelect'
import FilmDetails, { FilmSummary, filmLabel } from './FilmDetails'
import { Banner, Button, Modal, Spinner } from './ui'

/* How much of the visible viewport a preview may take before the modal's own
   chrome and the buttons under it start being pushed off. */
const PREVIEW_MAX = 'calc(var(--app-height) * 0.62)'

/* Opening a file here is the whole point of the design: the server gathers two
   of its three parts from separate accounts, rebuilds the plaintext in memory
   and streams it back. Nothing decrypted is ever written to disk. */
export default function PreviewModal({ file, hasThumb, film, onClose, onThumbStored, onFilmChanged }) {
  const kind = previewKind(file.mime, file.name)
  const url = api.contentURL(file.id)
  const mobile = useIsMobile()
  const captured = useRef(false)

  const [text, setText] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(kind === 'text')

  /* Kept apart from the preview's own error: a file whose download fails is
     still a file the preview above may be rendering perfectly well. */
  const [downloadError, setDownloadError] = useState(null)
  const [download, downloading] = useDownload(setDownloadError)

  /* The preview above is the browser's best attempt at the file. For a film
     that means one long inline fetch and whichever codecs the browser happens
     to have, which is exactly when handing it to a real player is the better
     answer — so the offer sits beside it rather than somewhere else. */
  const [streaming, setStreaming] = useState(null)
  const playable = isPlayable(file.mime, file.name)

  /* A film this file has already been matched to. Watching something is when
     "what is this, again?" gets asked, so the answer is here rather than only
     back in the list.

     `film` is the listing's own two fields — a title and a year — so the full
     record is fetched once the dialog is open. It arrives in well under the
     time anyone takes to decide whether to press play, and the title is on
     screen from the first frame either way. */
  const [details, setDetails] = useState(false)
  const [record, setRecord] = useState(null)

  useEffect(() => {
    if (!film) return undefined
    let cancelled = false
    api.movie(file.id)
      .then((resp) => { if (!cancelled) setRecord(resp.movie) })
      // The preview is what was asked for. Failing to decorate it is not worth
      // an error message over — the player and the buttons are all still here.
      .catch(() => {})
    return () => { cancelled = true }
  }, [film, file.id])

  /* Whether the player has been asked for.

     A video element that has not been played is a black rectangle with a
     triangle on it — iOS draws nothing else without a `poster`, and nothing
     makes a poster of a video at upload time. On a phone that is half the
     screen spent saying nothing, in front of a film whose summary, cast and
     artwork the vault is already holding. So a matched film opens on the film,
     and the player takes over when somebody presses play. */
  const [playing, setPlaying] = useState(false)

  useEffect(() => {
    if (kind !== 'text') return
    let cancelled = false

    fetch(url, { credentials: 'same-origin' })
      .then(async (resp) => {
        if (!resp.ok) throw new Error(`could not rebuild this file (${resp.status})`)
        // Cap what we render: a multi-megabyte log should not freeze the tab.
        const blob = await resp.blob()
        return blob.slice(0, 512 * 1024).text()
      })
      .then((body) => { if (!cancelled) { setText(body); setLoading(false) } })
      .catch((err) => { if (!cancelled) { setError(err.message); setLoading(false) } })

    return () => { cancelled = true }
  }, [url, kind])

  /* The three under the poster. Narrower than the toolbar's buttons because
     the column is, and shorter than the 44px floor a lone control gets: three
     stacked targets with nothing else near them are not the mis-tap that floor
     exists to prevent. */
  const action = { width: '100%', justifyContent: 'center', minHeight: '38px', padding: '6px 8px' }

  // Whether the film is what the preview is showing, which decides where the
  // controls live.
  const filmShown = kind === 'video' && !!record && !playing

  const parts = storedParts(file.shards)
  const accounts = new Set(file.shards.map((s) => s.provider_id)).size
  /* How many of those parts the rebuild actually needs. The count of parts and
     the count of clouds both say how far the file is spread; neither says how
     much of that spread has to answer for the file to come back, which is the
     number that decides whether it is still readable. */
  const needed = fileScheme(file).data

  /* A file uploaded before thumbnails existed — or from the command line —
     has no picture in the list. Opening it has just rebuilt and decoded the
     whole thing on screen, so taking one now costs nothing but a canvas: no
     second download, no second gather from the accounts. */
  const captureThumb = async (el) => {
    if (hasThumb || captured.current) return
    /* A video is asked repeatedly as it plays rather than once when it loads,
       and the first second of a film is a fade from black. Waiting a beat costs
       nothing and is the difference between a picture and a black square. */
    if (el.currentTime !== undefined && el.duration && el.currentTime < Math.min(1.5, el.duration / 4)) return
    captured.current = true

    const blob = await thumbnailFromElement(el)
    if (!blob) return
    try {
      await api.putThumb(file.id, blob)
      onThumbStored?.()
    } catch {
      // The preview is what was asked for and it is on screen. Failing to
      // keep a copy of it is not worth interrupting anyone over.
    }
  }

  return (
    <Modal
      title={filmLabel(record || film) || file.name}
      /* Where the film has taken the title, the file name comes here — and the
         line about the parts gets shorter on a phone, where the two of them
         together were three lines of header in front of a dialog trying to fit
         on one screen. It says the same thing either way. */
      subtitle={[
        film ? file.name : null,
        mobile
          ? `${formatBytes(file.size)} · ${parts} part${parts === 1 ? '' : 's'} · ${accounts} cloud${accounts === 1 ? '' : 's'} · any ${needed} rebuild it`
          : `${formatBytes(file.size)} · ${schemeName(fileScheme(file))} — ${parts} shard${parts === 1 ? '' : 's'} across ${accounts} account${accounts === 1 ? '' : 's'}, any ${needed} of them rebuild it`,
      ].filter(Boolean).join(' · ')}
      onClose={onClose}
      width={920}
    >
      <div style={{
        background: COLORS.bg,
        border: `1px solid ${COLORS.border}`,
        borderRadius: '8px',
        minHeight: '180px',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        overflow: 'hidden',
        marginBottom: '16px',
      }}>
        {error && <Banner tone="error">{error}</Banner>}

        {!error && kind === 'image' && (
          <img
            src={url}
            alt={file.name}
            style={{ maxWidth: '100%', maxHeight: PREVIEW_MAX, display: 'block' }}
            onLoad={(e) => captureThumb(e.currentTarget)}
            onError={() => setError('This file could not be rebuilt or is not a readable image.')}
          />
        )}

        {!error && kind === 'video' && (record && !playing ? (
          <div style={{ width: '100%', padding: mobile ? '12px' : '18px', boxSizing: 'border-box' }}>
            <FilmSummary
              film={record}
              fileId={file.id}
              mobile={mobile}
              /* Enough of the summary to know what it is, without pushing the
                 rest of the dialog off a phone. The details view, which has the
                 screen to itself, shows all of it. */
              clamp={mobile ? 6 : 0}
              /* Into the gap under the poster rather than onto rows of their
                 own. Everything a film is worth doing is here, so the footer
                 keeps only the one action that is about the file rather than
                 the film. */
              actions={(
                <>
                  <Button
                    size="sm"
                    variant="primary"
                    onClick={() => setPlaying(true)}
                    title="Play it in this page"
                    style={action}
                  >▶ Play</Button>
                  {/* Terse on a phone because the column is 112px wide, and
                      spelled out on a desk because there it is 150px and there
                      is no reason to abbreviate. */}
                  <Button
                    size="sm"
                    onClick={() => setStreaming('play')}
                    title="Open it in VLC, or copy the address"
                    style={action}
                  >{mobile ? '▶ VLC' : '▶ Stream in VLC'}</Button>
                  <Button size="sm" variant="ghost" onClick={onClose} style={action}>Close</Button>
                </>
              )}
            />
          </div>
        ) : (
          <video
            src={url}
            controls
            playsInline
            autoPlay={playing}
            /* The stored picture, which for a matched film is its poster and
               for anything else is a frame off the film itself. Without it iOS
               draws a black rectangle until the first frame decodes. */
            poster={hasThumb ? api.thumbURL(file.id) : undefined}
            preload="metadata"
            onTimeUpdate={(e) => captureThumb(e.currentTarget)}
            style={{ maxWidth: '100%', maxHeight: PREVIEW_MAX }}
          />
        ))}

        {!error && kind === 'audio' && (
          <audio src={url} controls style={{ width: '90%', margin: '32px 0' }} />
        )}

        {/* Drawn here rather than framed for the browser to deal with: a
            framed PDF is a blank box or one unscrollable page on iOS Safari,
            which used to leave a phone with an apology instead of the
            document. */}
        {!error && kind === 'pdf' && (
          <PdfPreview url={url} name={file.name} onFirstPage={captureThumb} />
        )}

        {!error && kind === 'text' && (
          loading ? <div style={{ padding: '40px' }}><Spinner size={20} /></div> : (
            <pre style={{
              margin: 0,
              padding: '16px',
              width: '100%',
              maxHeight: PREVIEW_MAX,
              overflow: 'auto',
              fontFamily: FONT.mono,
              fontSize: '12px',
              lineHeight: 1.6,
              color: COLORS.text,
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-word',
              boxSizing: 'border-box',
            }}>{text}</pre>
          )
        )}

        {!error && !kind && (
          <div style={{
            padding: '46px 24px',
            textAlign: 'center',
            fontFamily: FONT.sans,
            fontSize: '13px',
            color: COLORS.textMuted,
            lineHeight: 1.6,
          }}>
            <div style={{ fontSize: '30px', marginBottom: '10px', opacity: 0.5 }}>📦</div>
            No inline preview for {file.mime || 'this file type'}.<br />
            Download it to open with something else.
          </div>
        )}
      </div>

      {film && (
        <button
          onClick={() => setDetails(true)}
          title="The full summary and cast, how this file was matched, and how to correct it"
          style={{
            display: 'block', width: '100%',
            padding: '8px 2px', marginBottom: '10px',
            background: 'none', border: 'none', textAlign: mobile ? 'center' : 'right',
            minHeight: mobile ? '44px' : 0,
            cursor: 'pointer',
            fontFamily: FONT.mono, fontSize: '11px', color: COLORS.accent,
          }}
        >🎬 Film details, or fix the match →</button>
      )}

      {downloadError && (
        <Banner tone="error" onDismiss={() => setDownloadError(null)}>{downloadError}</Banner>
      )}

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
        {/* Close and the player are under the poster when there is one, so
            repeating them here would be two rows of the same three buttons. */}
        {!filmShown && (
          <Button variant="ghost" onClick={onClose}
            style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Close</Button>
        )}
        {/* A player for the files a player is for; for everything else the
            same dialog, opened for the address it is really wanted for. */}
        {!filmShown && (
          <Button
            onClick={() => setStreaming(playable ? 'play' : 'link')}
            title={playable
              ? 'Open this in VLC, or copy the address'
              : 'A link any player or app can open'}
            style={mobile ? { flex: 1, justifyContent: 'center' } : null}
          >{playable ? '▶ Stream in VLC' : '⧉ Copy the address'}</Button>
        )}
        <Button
          variant="primary"
          onClick={() => download(file)}
          disabled={downloading}
          title="Download the rebuilt, decrypted file"
          style={mobile ? { flex: 2, justifyContent: 'center' } : null}
        >
          {downloading
            ? <><Spinner size={12} color={COLORS.bg} /> Rebuilding…</>
            : '↓ Download decrypted'}
        </Button>
      </div>

      {streaming && (
        <StreamLink
          file={file}
          autoplay={streaming === 'play'}
          /* Opened from inside a dialog, so it has to sit above the one that
             opened it rather than wherever the portal happened to put it. */
          zIndex={120}
          onClose={() => setStreaming(null)}
        />
      )}

      {details && (
        <FilmDetails
          file={file}
          zIndex={120}
          onClose={() => setDetails(false)}
          onChanged={onFilmChanged}
        />
      )}
    </Modal>
  )
}

/* A read-out of exactly where a file's parts are and whether each one is still
   answering — the thing you want when an account starts misbehaving. */
export function ShardInspector({ file, providers = [], onClose, onChanged }) {
  const [health, setHealth] = useState(null)
  const [error, setError] = useState(null)
  const [relocating, setRelocating] = useState(false)

  useEffect(() => {
    let cancelled = false
    api.fileHealth(file.id)
      .then((resp) => { if (!cancelled) setHealth(resp) })
      .catch((err) => { if (!cancelled) setError(err.message) })
    return () => { cancelled = true }
  }, [file.id])

  /* A chunked file is not one object per part — it is one per part per chunk,
     all on the same account under the same name with the chunk number counting
     up. A 4 GB film is thousands of them, so the list below stays one row per
     part and names that part's first chunk; a page of `-c0000001-`,
     `-c0000002-` would say nothing the count does not.

     Which is only honest if the count is said out loud. A row showing
     `-c0000000-` and nothing else reads as if the part were a single object,
     and then the size beside it reads as the whole part when it is what the
     sample weighed. Both are said here instead. */
  const chunks = health?.chunk_count || file.chunk_count || 0
  const chunked = chunks > 1
  const sampled = health?.chunks_sampled || 0

  return (
    <Modal
      title="Where this file lives"
      subtitle={`${file.name} — cut ${schemeName(fileScheme(file))} into encrypted shards${
        chunked ? `, one set per chunk across ${chunks} chunks` : ''}. Any ${
        fileScheme(file).data} of them rebuild the original; fewer is noise.`}
      onClose={onClose}
      width={620}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {!health && !error && <Spinner />}

      {health && (
        <>
          <Banner tone={health.recoverable ? 'success' : 'error'}>
            {health.recoverable
              ? 'Enough parts are reachable to rebuild this file right now.'
              : 'Too few parts are reachable — this file cannot currently be rebuilt.'}
          </Banner>

          {chunked && (
            <Banner tone="info">
              {`This file is stored as ${chunks} chunks, each cut into the same shards. `
                + "One row per shard below, naming that shard's first chunk — the other "
                + `${chunks === 2 ? 'one sits' : `${chunks - 1} sit`} beside it on the `
                + 'same account, same name with the chunk number counting up. '
                + (sampled > 0 && sampled < chunks
                  ? `${sampled} of them, spread across the file, were checked just now `
                    + `rather than all ${chunks}: a shard is written to every chunk or to `
                    + 'none of them, so a handful catches one that never landed.'
                  : 'Every one of them was checked just now.')}
            </Banner>
          )}

          {health.shards.map((shard) => (
            /* Keyed by the copy and not the part: a file spread over six
                clouds has two rows for every part. */
            <div key={`${shard.part}-${shard.provider_id}`} style={{
              display: 'flex',
              alignItems: 'center',
              gap: '12px',
              padding: '11px 13px',
              marginBottom: '8px',
              background: COLORS.bg,
              border: `1px solid ${shard.present ? COLORS.border : COLORS.error + '66'}`,
              borderRadius: '6px',
              fontFamily: FONT.mono,
              fontSize: '11.5px',
            }}>
              <span style={{
                color: shard.present ? COLORS.success : COLORS.error,
                fontSize: '13px',
              }}>{shard.present ? '✓' : '✗'}</span>

              {/* The account's colour, so this list and the row's part badges
                  can be read against each other. */}
              <span
                aria-hidden="true"
                style={{
                  width: '14px',
                  height: '14px',
                  flexShrink: 0,
                  borderRadius: '3px',
                  background: accountColor(shard.provider_id),
                }}
              />

              <span style={{ flex: 1, minWidth: 0 }}>
                <span style={{ color: COLORS.text }}>Shard {shard.part}</span>
                <span style={{ color: COLORS.textMuted }}> on </span>
                <span style={{ color: COLORS.text }}>{shard.provider_name}</span>
                <span style={{ color: COLORS.textMuted }}> ({shard.provider_kind})</span>
                <div style={{
                  marginTop: '4px',
                  color: COLORS.textMuted,
                  fontSize: '10px',
                  wordBreak: 'break-all',
                }}>
                  {shard.key}
                  {chunked ? ` — chunk 1 of ${chunks}` : ''}
                  {shard.error ? ` — ${shard.error}` : ''}
                </div>
              </span>

              {/* The recorded figure for a chunked file, because the observed one
                  is what the sampled chunks weighed and would read as the whole
                  shard sitting there. For a file stored whole the two are the
                  same measurement, and the observed one was taken just now. */}
              <span
                title={chunked
                  ? `This shard across all ${chunks} chunks`
                  : 'Measured on the account just now'}
                style={{ color: COLORS.textDim, flexShrink: 0, whiteSpace: 'nowrap' }}
              >
                {shard.present ? formatBytes(chunked ? shard.size : shard.observed_size) : '—'}
              </span>
            </div>
          ))}

          {storedParts(file.shards) < fileScheme(file).total && (
            <Banner tone="warn">
              Only {storedParts(file.shards)} of {fileScheme(file).total} shards were stored, so the
              file has less margin than {schemeName(fileScheme(file))} allows for. Connect another
              account and re-upload it to restore the full spread.
            </Banner>
          )}

          {health.spare > 0 && (
            <Banner tone="success">
              Any {health.needed} of these shards rebuild the file, so {health.spare} more
              account{health.spare === 1 ? '' : 's'} could go dark before it was at risk.
            </Banner>
          )}
        </>
      )}

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '4px' }}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        {/* The read-out above says where the parts are; this is how they go
            somewhere else. The row itself has no room for a fourth control,
            and this is the question it would have been answering anyway. */}
        <Button
          variant="primary"
          onClick={() => setRelocating(true)}
          disabled={providers.length === 0}
          title="Move the shards to other clouds"
        >⇄ Move to other clouds</Button>
      </div>

      {relocating && (
        <RelocateClouds
          target={{ id: file.id }}
          title={`Move ${file.name}`}
          subtitle={`${formatBytes(file.size)} — ${schemeName(fileScheme(file))}; pick the clouds its shards should live on`}
          /* Already selected: where the parts are now, so the dialog opens on
             the truth and a swap is one click rather than four. */
          current={file.shards.map((s) => s.provider_id)}
          providers={providers}
          onClose={() => setRelocating(false)}
          onDone={() => { onChanged?.(); onClose() }}
        />
      )}
    </Modal>
  )
}
