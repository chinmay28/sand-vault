import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { useIsMobile } from '../hooks'
import { api } from '../api'
import { Banner, Button, IconButton, Input, Modal, Spinner } from './ui'

/* What a film is, drawn from what the vault was told once.

   The vault holds the details in its own index and the poster as the file's
   thumbnail, so everything on this screen is served by the local server like
   the rest of the app — no image is fetched from anywhere else, no request
   leaves this machine to draw it. The only thing that ever does leave is a
   lookup, and a lookup happens when somebody asks for one: turning a folder on,
   sweeping it, or correcting a match. */

/* The wording the database asks anything using it to carry, and a fair thing to
   say in any case: the films, the artwork and the summaries are theirs. */
const ATTRIBUTION = 'Details and artwork come from The Movie Database (TMDB). ' +
  'This uses the TMDB API but is not endorsed or certified by TMDB.'

/* Where to get a key, said once and in one place. */
const KEY_HELP = 'A free personal key from themoviedb.org — Settings → API. ' +
  'Either the v3 key or the v4 read access token works.'

function runtimeLabel(minutes) {
  if (!minutes) return null
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  return hours ? `${hours}h ${rest}m` : `${rest}m`
}

/* Title and year, which is how a film is named everywhere in the app. */
export function filmLabel(film) {
  if (!film?.title) return null
  return film.year ? `${film.title} (${film.year})` : film.title
}

/* A line of small facts under the title: score, length, genres. Whichever of
   them the database actually had. */
function MetaLine({ film }) {
  const bits = [
    film.rating > 0 && `★ ${film.rating.toFixed(1)}${film.votes ? ` · ${film.votes.toLocaleString()} votes` : ''}`,
    runtimeLabel(film.runtime),
    film.genres?.length ? film.genres.join(', ') : null,
  ].filter(Boolean)

  if (!bits.length) return null

  return (
    <div style={{
      display: 'flex', flexWrap: 'wrap', gap: '4px 12px', marginTop: '6px',
      fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim,
    }}>
      {bits.map((bit, i) => <span key={i}>{bit}</span>)}
    </div>
  )
}

function Credits({ label, names, clamp }) {
  if (!names?.length) return null

  return (
    <div style={{ marginTop: '9px', fontFamily: FONT.sans, fontSize: '12px', lineHeight: 1.55 }}>
      <span style={{
        fontFamily: FONT.mono, fontSize: '9.5px', letterSpacing: '1.2px',
        textTransform: 'uppercase', color: COLORS.textMuted,
      }}>{label}</span>
      <div style={{
        color: COLORS.textDim, marginTop: '3px',
        ...(clamp ? {
          display: '-webkit-box', overflow: 'hidden',
          WebkitLineClamp: clamp, WebkitBoxOrient: 'vertical',
        } : null),
      }}>{names.join(' · ')}</div>
    </div>
  )
}

/* The poster, at the size the details view wants it. It is the file's stored
   thumbnail, so it falls back the way every other picture in the app does:
   to the icon, on any failure at all. */
function Poster({ id, version, width = 150 }) {
  const [failed, setFailed] = useState(false)

  useEffect(() => { setFailed(false) }, [id, version])

  const frame = {
    width: `${width}px`,
    // A poster is two by three nearly everywhere, and a frame of that shape
    // keeps the row from jumping when the picture finally decodes.
    aspectRatio: '2 / 3',
    flexShrink: 0,
    borderRadius: '6px',
    overflow: 'hidden',
    background: COLORS.bg,
    border: `1px solid ${COLORS.border}`,
  }

  if (failed) {
    return (
      <div style={{ ...frame, display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: '30px' }}>
        🎬
      </div>
    )
  }

  return (
    <img
      src={api.thumbURL(id, version)}
      alt=""
      decoding="async"
      onError={() => setFailed(true)}
      style={{ ...frame, objectFit: 'cover', display: 'block' }}
    />
  )
}

/* The film itself: poster on one side, what is known about it on the other.

   Lives apart from the dialog around it because two screens want it. The
   details view is one. The other is the preview — opening a film puts a
   video element on screen, and a video element that has not been played is a
   black rectangle with a triangle on it, which on a phone is half the screen
   saying nothing. This goes there instead, and the player takes over when
   somebody actually presses play. */
export function FilmSummary({ film, fileId, mobile, posterWidth, clamp, actions }) {
  const width = posterWidth || (mobile ? 112 : 150)

  return (
    <div style={{
      display: 'flex',
      alignItems: 'flex-start',
      gap: mobile ? '12px' : '16px',
    }}>
      {/* A poster is two-by-three and a summary is a paragraph, so the column
          under the picture runs out long before the column beside it does.
          Whatever the screen wants to do with the film goes in that gap rather
          than on rows of its own underneath — which is the difference, on a
          phone, between a dialog that fits and one that scrolls. */}
      <div style={{
        width: `${width}px`, flexShrink: 0,
        display: 'flex', flexDirection: 'column', gap: '6px',
      }}>
        <Poster id={fileId} version={film.matched_at} width={width} />
        {actions}
      </div>

      <div style={{ flex: 1, minWidth: 0 }}>
        {film.original && (
          <div style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted }}>
            {film.original}
          </div>
        )}
        {film.tagline && (
          <div style={{
            fontFamily: FONT.sans, fontSize: mobile ? '12px' : '12.5px', fontStyle: 'italic',
            color: COLORS.accent, marginTop: '2px',
          }}>{film.tagline}</div>
        )}

        <MetaLine film={film} />

        {film.overview && (
          <p style={{
            margin: '10px 0 0',
            fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6, color: COLORS.text,
            /* On a phone the summary sits beside a poster in a dialog that
               still has to show the player and its buttons, so it is trimmed
               to what fits rather than pushing them off. The details view
               passes no clamp and shows the lot. */
            ...(clamp ? {
              display: '-webkit-box', overflow: 'hidden',
              WebkitLineClamp: clamp, WebkitBoxOrient: 'vertical',
            } : null),
          }}>{film.overview}</p>
        )}

        <Credits label="Directed by" names={film.directors} />
        <Credits
          label="Starring"
          names={film.cast?.map((c) => (c.role ? `${c.name} as ${c.role}` : c.name))}
          /* Eight names is four lines of a phone. Trimmed to the top of the
             billing where the space is shared, spelled out in full where it is
             not — the same rule the summary above follows. */
          clamp={clamp ? 3 : 0}
        />
      </div>
    </div>
  )
}

/* Everything known about one file's film, and every way to change it.
   `file` is the listing's own record; nothing here needs the file's content. */
export default function FilmDetails({ file, zIndex, onClose, onChanged }) {
  const mobile = useIsMobile()
  const [state, setState] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [fixing, setFixing] = useState(false)
  const [note, setNote] = useState(null)

  const load = useCallback(() => api.movie(file.id)
    .then((resp) => { setState(resp); setError(null) })
    .catch((err) => setError(err.message)), [file.id])

  useEffect(() => { load() }, [load])

  const film = state?.movie
  const guess = state?.guess
  const lookup = state?.lookup

  /* Look this one file up, either on the guess off its name or on a film
     picked out of the candidate list. Anything that comes back is stored by
     the server before it answers, so the listing behind this dialog is stale
     the moment it does. */
  const match = async (options) => {
    setBusy(true)
    setError(null)
    setNote(null)
    try {
      const resp = await api.matchMovie(file.id, options)
      if (!resp.movie) {
        setNote(`Nothing came back for "${resp.guess?.title || guess?.title || file.name}". ` +
          'Try searching for it by name.')
        setFixing(true)
      } else {
        setState((current) => ({ ...current, movie: resp.movie }))
        setFixing(false)
        if (resp.warnings?.length) setNote(resp.warnings.join('\n'))
      }
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const forget = async () => {
    setBusy(true)
    try {
      await api.forgetMovie(file.id)
      setState((current) => ({ ...current, movie: null }))
      setNote(null)
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title={filmLabel(film) || file.name}
      subtitle={film ? file.name : 'Film details'}
      onClose={onClose}
      width={720}
      /* Opened from inside the preview as well as from a row, and a dialog
         over a dialog has to sit above the one that opened it. */
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {note && <Banner tone="info" onDismiss={() => setNote(null)}>{note}</Banner>}

      {!state && !error && <Spinner />}

      {state && film && !fixing && (
        <>
          <FilmSummary film={film} fileId={file.id} mobile={mobile} />

          {/* How this file came to be called this. It is the one thing that
              makes a wrong match obvious rather than puzzling — the name was
              read, a query was made, and here is what it was. */}
          <div style={{
            marginTop: '16px',
            padding: '9px 11px',
            background: COLORS.bg,
            border: `1px solid ${COLORS.border}`,
            borderRadius: '6px',
            fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted,
            lineHeight: 1.6, wordBreak: 'break-word',
          }}>
            {film.manual
              ? 'Chosen by hand, so sweeping this folder again will leave it alone.'
              : <>Matched from the file name by searching for <span style={{ color: COLORS.textDim }}>{film.query || '—'}</span>.</>}
            {film.tmdb_id ? <> TMDB #{film.tmdb_id}.</> : null}
            {film.imdb_id ? <> IMDb {film.imdb_id}.</> : null}
          </div>
        </>
      )}

      {state && !film && !fixing && (
        <div style={{
          padding: '24px 4px',
          textAlign: 'center',
          fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6, color: COLORS.textMuted,
        }}>
          <div style={{ fontSize: '30px', opacity: 0.5, marginBottom: '10px' }}>🎬</div>
          {lookup?.enabled ? (
            <>
              Nothing has been looked up for this file yet.
              {guess?.title
                ? <> Its name reads as <span style={{ color: COLORS.textDim }}>{guess.year ? `${guess.title} (${guess.year})` : guess.title}</span>.</>
                : <> Its name says nothing a search could use, so look it up by title instead.</>}
            </>
          ) : (
            <>Film lookup is off for {file.dir || 'this folder'}. Turn it on for the folder first —
              nothing is ever sent to the film database from a folder that has not asked for it.</>
          )}
        </div>
      )}

      {fixing && (
        <FixMatch
          file={file}
          guess={guess}
          busy={busy}
          onChoose={(id) => match({ tmdbId: id })}
        />
      )}

      <div style={{
        marginTop: '16px',
        fontFamily: FONT.sans, fontSize: '10.5px', lineHeight: 1.5, color: COLORS.textMuted,
      }}>{ATTRIBUTION}</div>

      <div style={{
        display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap', marginTop: '14px',
      }}>
        <Button variant="ghost" onClick={onClose}
          style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Close</Button>

        {film && !fixing && (
          <Button
            variant="ghost"
            disabled={busy}
            onClick={forget}
            title="Drop these details. The file and its poster stay where they are."
            style={mobile ? { flex: 1, justifyContent: 'center' } : null}
          >✕ Forget</Button>
        )}

        {lookup?.enabled && (
          fixing ? (
            <Button onClick={() => setFixing(false)} disabled={busy}
              style={mobile ? { flex: 1, justifyContent: 'center' } : null}>Back</Button>
          ) : (
            <Button
              onClick={() => setFixing(true)}
              disabled={busy}
              title="Search for the right film and pick it from the list"
              style={mobile ? { flex: 1, justifyContent: 'center' } : null}
            >⌕ {film ? 'Fix the match' : 'Search by title'}</Button>
          )
        )}

        {lookup?.enabled && !film && !fixing && guess?.title && (
          <Button
            variant="primary"
            disabled={busy}
            onClick={() => match({})}
            style={mobile ? { flex: 2, justifyContent: 'center' } : null}
          >{busy ? <><Spinner size={12} color={COLORS.bg} /> Looking up…</> : '🎬 Look it up'}</Button>
        )}
      </div>
    </Modal>
  )
}

/* The control that says whether the folder you are standing in has film lookup
   on, and opens the dialog that changes it. Lit when it is on, so a folder that
   talks to a third party is never doing so quietly. */
export function FilmButton({ lookup, mobile, onOpen }) {
  const on = !!lookup?.enabled

  return (
    <IconButton
      glyph="🎬"
      label="Film details for this folder"
      title={on
        ? `Film details are on${lookup.source && lookup.source !== '/' ? ` (set on ${lookup.source})` : ''} — look up new films, or turn it off`
        : 'Off. Match the videos here against the film database, like Plex or Jellyfin'}
      size={mobile ? 44 : 32}
      onClick={onOpen}
      style={{
        fontSize: mobile ? '15px' : '13px',
        background: on ? `${COLORS.accent}22` : undefined,
        borderColor: on ? COLORS.accent : undefined,
      }}
    />
  )
}

/* Turning film lookup on for a folder, and sweeping it.

   The two are deliberately separate buttons. Turning the switch on stores a
   setting and sends nothing; sweeping is what actually puts a list of your
   filenames in front of somebody else's server, and it should be a thing
   somebody pressed. */
export function FilmLookupSettings({ path, lookup, onClose, onChanged }) {
  const mobile = useIsMobile()
  const [settings, setSettings] = useState(null)
  const [key, setKey] = useState('')
  const [editingKey, setEditingKey] = useState(false)
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  useEffect(() => {
    api.movieSettings()
      .then((resp) => { setSettings(resp); setEditingKey(!resp.has_key) })
      .catch((err) => setError(err.message))
  }, [])

  const on = !!lookup?.enabled
  const inherited = on && lookup.source && lookup.source !== path
  const hasKey = !!settings?.has_key

  /* Storing a key and clearing one are the same call — the server checks
     whatever is not empty against the database before it keeps it, so a key
     that would fail on the first film fails here instead. */
  const storeKey = async (value) => {
    setBusy('key')
    setError(null)
    try {
      const resp = await api.setMovieKey(value)
      setSettings((current) => ({ ...current, has_key: resp.has_key }))
      setEditingKey(!resp.has_key)
      setKey('')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(null)
    }
  }

  const toggle = async (enabled) => {
    setBusy('toggle')
    setError(null)
    try {
      await api.setMovieLookup(path, enabled)
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(null)
    }
  }

  const scan = async (refresh) => {
    setBusy('scan')
    setError(null)
    setReport(null)
    try {
      setReport(await api.scanMovies(path, { refresh }))
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(null)
    }
  }

  return (
    <Modal
      title="Film details"
      subtitle={`For ${path} and everything inside it`}
      onClose={() => !busy && onClose()}
      width={560}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {/* Said before anything can be turned on, because it is the whole reason
          this is a switch rather than a feature. */}
      <Banner tone={on ? 'warn' : 'info'}>
        Everything else in SAND stays on this machine and your own accounts.
        This does not: looking a film up sends the title read off its file name,
        and this machine's address, to The Movie Database. Nothing else about
        the file goes — not its contents, not its size, not where its parts
        live — and what comes back is stored in the vault, so a film is looked
        up once and never again.
      </Banner>

      {!settings && !error && <Spinner />}

      {settings && (
        <>
          {/* The key first: nothing below it can do anything without one. */}
          {editingKey ? (
            <form onSubmit={(e) => { e.preventDefault(); storeKey(key.trim()) }}>
              <Input
                label={hasKey ? 'Replace the film database key' : 'Film database key'}
                value={key}
                autoFocus
                autoComplete="off"
                spellCheck="false"
                placeholder="Paste your TMDB key or read token"
                onChange={(e) => setKey(e.target.value)}
                help={KEY_HELP}
              />
              <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginBottom: '16px' }}>
                {hasKey && (
                  <Button type="button" variant="ghost" onClick={() => { setEditingKey(false); setKey('') }}>
                    Cancel
                  </Button>
                )}
                <Button type="submit" variant="primary" disabled={busy === 'key' || !key.trim()}>
                  {busy === 'key' ? <><Spinner size={12} color={COLORS.bg} /> Checking…</> : 'Save the key'}
                </Button>
              </div>
            </form>
          ) : (
            <div style={{
              display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap',
              padding: '10px 12px', marginBottom: '16px',
              background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
              fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim,
            }}>
              <span style={{ color: COLORS.success }}>✓</span>
              <span style={{ flex: 1, minWidth: 0 }}>
                A key is stored, sealed in the vault with everything else.
              </span>
              <Button size="sm" variant="ghost" onClick={() => setEditingKey(true)}>Change</Button>
              <Button
                size="sm"
                variant="ghost"
                disabled={busy === 'key'}
                title="Forget the key. Films already matched keep their details; nothing new can be looked up."
                onClick={() => storeKey('')}
              >Remove</Button>
            </div>
          )}

          <div style={{
            display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap',
            padding: '12px', marginBottom: '14px',
            background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
          }}>
            <div style={{ flex: 1, minWidth: '180px' }}>
              <div style={{ fontFamily: FONT.mono, fontSize: '12px', color: COLORS.text }}>
                {on ? 'On for this folder' : 'Off for this folder'}
              </div>
              <div style={{
                marginTop: '4px', fontFamily: FONT.sans, fontSize: '11.5px',
                lineHeight: 1.5, color: COLORS.textMuted,
              }}>
                {inherited
                  ? <>Turned on further up, at {lookup.source}, and inherited by everything under it. Turn it off there to stop it here.</>
                  : 'Videos in this folder and every folder inside it can be matched against the film database.'}
              </div>
            </div>
            <Button
              variant={on ? 'ghost' : 'primary'}
              disabled={!!busy || (!hasKey && !on) || inherited}
              title={inherited ? `The setting lives on ${lookup.source}` : undefined}
              onClick={() => toggle(!on)}
            >{busy === 'toggle' ? '…' : on ? 'Turn off' : 'Turn on'}</Button>
          </div>

          {on && (
            <>
              <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap', marginBottom: '4px' }}>
                <Button
                  variant="primary"
                  disabled={!!busy || !hasKey}
                  onClick={() => scan(false)}
                  style={mobile ? { flex: 1, justifyContent: 'center' } : null}
                >
                  {busy === 'scan'
                    ? <><Spinner size={12} color={COLORS.bg} /> Looking up…</>
                    : '🎬 Look up new films'}
                </Button>
                <Button
                  disabled={!!busy || !hasKey}
                  title="Look everything up again, including films that already have details. Matches you fixed by hand are left alone."
                  onClick={() => scan(true)}
                  style={mobile ? { flex: 1, justifyContent: 'center' } : null}
                >⟳ Look up everything again</Button>
              </div>
              <div style={{
                fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted,
              }}>
                One search and one poster per film, so a large folder takes a
                while. Leave this open — it is safe to stop and run again, and
                nothing is looked up twice.
              </div>
            </>
          )}

          {report && <ScanReport report={report} />}
        </>
      )}

      <div style={{
        marginTop: '16px',
        fontFamily: FONT.sans, fontSize: '10.5px', lineHeight: 1.5, color: COLORS.textMuted,
      }}>{ATTRIBUTION}</div>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '14px' }}>
        <Button variant="ghost" disabled={!!busy} onClick={onClose}>Close</Button>
      </div>
    </Modal>
  )
}

/* What a sweep did. The misses matter more than the hits — those are the files
   that want a match picked by hand — so they are listed by name rather than
   counted. */
function ScanReport({ report }) {
  const nothing = report.considered === 0

  return (
    <div style={{ marginTop: '14px' }}>
      <Banner tone={nothing ? 'info' : report.matched > 0 || report.skipped > 0 ? 'success' : 'warn'}>
        {nothing
          ? 'No videos here to look up.'
          : `${report.matched} matched, ${report.skipped} already had details, ` +
            `${report.unmatched?.length || 0} left unmatched — out of ${report.considered} video(s). ` +
            `${report.artwork} poster(s) stored.`}
      </Banner>

      {report.warnings?.map((warning, i) => (
        <Banner key={i} tone="warn">{warning}</Banner>
      ))}

      {report.unmatched?.length > 0 && (
        <div style={{
          maxHeight: '180px', overflowY: 'auto',
          padding: '10px 12px',
          background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
          fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, lineHeight: 1.7,
        }}>
          <div style={{ color: COLORS.textMuted, marginBottom: '4px' }}>
            Nothing came back for these — open one and search for it by title:
          </div>
          {report.unmatched.map((miss) => (
            <div key={miss.id} style={{ wordBreak: 'break-all' }}>
              {miss.name}
              {miss.query ? <span style={{ color: COLORS.textMuted }}> — searched for "{miss.query}"</span> : null}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

/* Searching the database for the right film, and choosing it.

   Nothing is stored until one is chosen, so this is safe to reopen and retype
   as often as it takes — and a film chosen here is recorded as chosen by hand,
   which is what stops the next sweep of the folder undoing the correction. */
function FixMatch({ file, guess, busy, onChoose }) {
  const [query, setQuery] = useState(guess?.title || '')
  const [candidates, setCandidates] = useState(null)
  const [searching, setSearching] = useState(false)
  const [error, setError] = useState(null)

  const search = useCallback(async (term) => {
    const wanted = (term ?? query).trim()
    if (!wanted) return
    setSearching(true)
    setError(null)
    try {
      const resp = await api.movieCandidates(file.id, wanted)
      setCandidates(resp.candidates || [])
    } catch (err) {
      setError(err.message)
      setCandidates(null)
    } finally {
      setSearching(false)
    }
  }, [file.id, query])

  // Opens on the guess already searched for: the common correction is picking
  // the other film of the same name, and that list is one the app can have
  // waiting rather than making somebody ask for it.
  useEffect(() => { if (guess?.title) search(guess.title) }, []) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div>
      <form onSubmit={(e) => { e.preventDefault(); search() }}>
        <Input
          label="Search the film database"
          value={query}
          autoFocus
          placeholder="Film title"
          onChange={(e) => setQuery(e.target.value)}
          help={`Searching sends this title — and nothing else about ${file.name} — to the film database.`}
          trailing={
            <span style={{ position: 'absolute', top: 0, bottom: 0, right: '6px', display: 'flex', alignItems: 'center' }}>
              {searching
                ? <Spinner size={13} />
                : <IconButton glyph="⌕" label="Search" size={28} onClick={() => search()} />}
            </span>
          }
        />
      </form>

      {error && <Banner tone="error">{error}</Banner>}

      {candidates?.length === 0 && (
        <Banner tone="info">Nothing matched "{query}". Try the original title, or add the year.</Banner>
      )}

      <div style={{ maxHeight: 'calc(var(--app-height) * 0.4)', overflowY: 'auto' }}>
        {(candidates || []).map((c) => (
          <button
            key={c.tmdb_id}
            onClick={() => onChoose(c.tmdb_id)}
            disabled={busy}
            style={{
              display: 'block', width: '100%', textAlign: 'left',
              padding: '10px 12px', marginBottom: '6px',
              background: COLORS.bg,
              border: `1px solid ${COLORS.border}`,
              borderRadius: '6px',
              cursor: busy ? 'wait' : 'pointer',
              color: COLORS.text,
            }}
          >
            <span style={{ display: 'flex', alignItems: 'baseline', gap: '8px' }}>
              <span style={{ fontFamily: FONT.mono, fontSize: '12.5px' }}>{c.title}</span>
              <span style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted }}>
                {c.year || '—'}
              </span>
              {c.rating > 0 && (
                <span style={{ marginLeft: 'auto', fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textDim }}>
                  ★ {c.rating.toFixed(1)}
                </span>
              )}
            </span>
            {c.overview && (
              <span style={{
                marginTop: '4px',
                fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.5, color: COLORS.textMuted,
                /* Two lines of the summary and no more: the list is for
                   telling four films of the same name apart, not for reading. */
                display: '-webkit-box', overflow: 'hidden',
                WebkitLineClamp: 2, WebkitBoxOrient: 'vertical',
              }}>{c.overview}</span>
            )}
          </button>
        ))}
      </div>
    </div>
  )
}
