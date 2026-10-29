import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'
import { humanDuration } from './Rekey'

/* The files still on the previous key, one by one.

   The progress panel counts them — seven left, seven under /Archives — and
   while files are moving, counts are what anyone wants. When they stop
   moving, a count is a question: which seven, and what is wrong with them?
   This is the answer, opened by tapping the count: every pending file in the
   order the plan takes them, with where each one stands — moving, waiting
   its turn, left for last, held for a cloud that is not answering, or tried
   and failed, with why and when it is tried again. Paged, because a vault
   that changed its password an hour ago has every file on this list. */

/* How many rows a page holds. A row is a path and a word, so this is about
   what fits on a screen. */
const PAGE_SIZE = 50

/* How often an open list asks again. The plan moves files on its own, and a
   list that showed a file as moving after it had moved would be telling the
   story the panel behind it has already stopped telling. */
const REFRESH_MS = 6_000

/* The state of a row, as a word and a tone. */
function describe(f) {
  switch (f.state) {
    case 'moving': return { word: 'moving', tone: COLORS.success }
    case 'deferred': return { word: 'left for last', tone: COLORS.textMuted }
    case 'held': return { word: `waiting for ${f.held_on}`, tone: COLORS.warn }
    case 'retrying': return { word: 'would not move', tone: COLORS.warn }
    case 'outside': return { word: 'after the folders', tone: COLORS.textMuted }
    default: return { word: 'in turn', tone: COLORS.textMuted }
  }
}

/* The sentence under a row that is not simply waiting its turn. */
function explain(f, now) {
  switch (f.state) {
    case 'held':
      return `${f.held_on} did not answer the last health check, and too few of this file's parts are elsewhere to rebuild it without it. It moves once the account is back.`
    case 'retrying': {
      const wait = Math.max(0, (new Date(f.retry_at).getTime() - now) / 1000)
      return `Tried again in ${humanDuration(wait)}. Last time: ${f.reason || 'it could not be moved'}`
    }
    case 'outside':
      return 'Outside the folders the plan stops at. Tick "Then everything else" to take it.'
    case 'deferred':
      return 'Skipped by you; it goes after everything else.'
    default:
      return ''
  }
}

/* The dialog. `folder` confines it to one folder — the one whose count was
   tapped — or "/" for the whole vault. `onChanged` is told when a row's
   skip or un-skip changed the plan, so the panel behind can refresh. */
export default function RekeyFiles({ folder = '/', onClose, onChanged, zIndex = 130 }) {
  const mobile = useIsMobile()
  const [offset, setOffset] = useState(0)
  const [page, setPage] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(null)
  const [now, setNow] = useState(() => Date.now())

  const load = useCallback(async (at, quiet = false) => {
    if (!quiet) setLoading(true)
    try {
      const answer = await api.rekeyFiles({ folder, offset: at, limit: PAGE_SIZE })
      /* A file that moved while the list was open shortens it, so the last
         page can empty out. Fall back a page rather than show nothing over
         a "51–50 of 50". */
      if (answer.files.length === 0 && at > 0 && answer.total > 0) {
        const back = Math.max(0, Math.floor((answer.total - 1) / PAGE_SIZE) * PAGE_SIZE)
        if (back !== at) { setOffset(back); return }
      }
      setPage(answer)
      setNow(Date.now())
      setError(null)
    } catch (err) {
      setError(err.message)
    } finally {
      if (!quiet) setLoading(false)
    }
  }, [folder])

  useEffect(() => { load(offset) }, [load, offset])
  useEffect(() => {
    const timer = setInterval(() => load(offset, true), REFRESH_MS)
    return () => clearInterval(timer)
  }, [load, offset])

  const skip = async (f, undo) => {
    setBusy(f.id)
    try {
      await api.skipRekeyFile(f.id, undo)
      onChanged?.()
      await load(offset, true)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(null)
    }
  }

  const total = page?.total || 0
  const shown = page?.files?.length || 0
  const first = total === 0 ? 0 : offset + 1
  const last = offset + shown
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const atPage = Math.floor(offset / PAGE_SIZE) + 1
  const where = folder === '/' ? 'the vault' : folder

  return (
    <Modal
      title="Still on the previous key"
      subtitle={page
        ? total > 0
          ? `${total} file${total === 1 ? '' : 's'} · ${formatBytes(page.bytes)} in ${where}, in the order the plan takes them`
          : `Nothing in ${where} is on the previous key any more.`
        : `The files in ${where} the re-encryption has not reached yet.`}
      onClose={onClose}
      zIndex={zIndex}
      width={620}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {!page && !error && <div style={{ display: 'flex', justifyContent: 'center', padding: '20px' }}><Spinner /></div>}

      {page && total > 0 && (
        <div style={{ border: `1px solid ${COLORS.border}`, borderRadius: '6px', overflow: 'hidden', opacity: loading ? 0.6 : 1 }}>
          {page.files.map((f, i) => {
            const { word, tone } = describe(f)
            const why = explain(f, now)
            const dir = f.dir === '/' ? '/' : `${f.dir}/`
            const action = f.state === 'deferred' ? (
              <Button size="sm" variant="ghost" disabled={busy === f.id} title="Put this file back in its turn" onClick={() => skip(f, true)}>Back in turn</Button>
            ) : f.state !== 'outside' ? (
              <Button
                size="sm"
                variant="ghost"
                disabled={busy === f.id}
                title={f.state === 'moving'
                  ? 'Stop moving this file and leave it for last; it stays readable on the previous key'
                  : 'Leave this file for last; it stays readable on the previous key meanwhile'}
                onClick={() => skip(f, false)}
              >Skip for now</Button>
            ) : null
            const pill = (
              <span style={{
                fontFamily: FONT.mono, fontSize: '9.5px', letterSpacing: '0.4px', textTransform: 'uppercase',
                padding: '1px 7px', borderRadius: '999px', border: `1px solid ${tone}66`, color: tone, whiteSpace: 'nowrap',
              }}>{word}</span>
            )
            const ellipsis = { minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }
            /* On a phone: the number, the name and the button on one line,
               the folder, size and state under the name. Wider: one line,
               with the folder dim before the name and the size, the state
               and the button in their own columns. The reason, when there
               is one, is a line of its own under the name either way. */
            return (
              <div
                key={f.id}
                style={{
                  display: 'grid',
                  gridTemplateColumns: mobile ? '28px minmax(0,1fr) auto' : '36px minmax(0,1fr) auto auto auto',
                  gap: '3px 10px', alignItems: 'center', padding: '7px 10px',
                  borderTop: i === 0 ? 'none' : `1px solid ${COLORS.border}`,
                  fontFamily: FONT.mono, fontSize: '11px',
                }}
              >
                <span style={{ color: COLORS.textMuted, fontSize: '10px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{offset + i + 1}</span>
                <span style={{ ...ellipsis, color: COLORS.text }} title={f.path}>
                  {!mobile && <span style={{ color: COLORS.textDim }}>{dir}</span>}
                  {f.name}
                </span>
                {!mobile && <span style={{ color: COLORS.textMuted, whiteSpace: 'nowrap' }}>{formatBytes(f.bytes)}</span>}
                {!mobile && pill}
                <span style={{ display: 'flex', justifyContent: 'flex-end', minHeight: '26px', alignItems: 'center' }}>{action}</span>
                {mobile && (
                  <span style={{ ...ellipsis, gridColumn: '2 / -1', color: COLORS.textMuted, fontSize: '10px' }} title={f.path}>
                    {dir} · {formatBytes(f.bytes)} · <span style={{ color: tone }}>{word}</span>
                  </span>
                )}
                {why && (
                  <span style={{ gridColumn: '2 / -1', fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.45, color: tone === COLORS.warn ? COLORS.textDim : COLORS.textMuted, overflowWrap: 'anywhere' }}>
                    {why}
                  </span>
                )}
              </div>
            )
          })}
        </div>
      )}

      {page && total > PAGE_SIZE && (
        <div style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '10px',
          marginTop: '14px', paddingTop: '12px', borderTop: `1px solid ${COLORS.border}`,
        }}>
          <Button size="sm" disabled={offset === 0 || loading} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>‹ Previous</Button>
          <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted, textAlign: 'center', lineHeight: 1.5 }}>
            {first}–{last} of {total}
            <br />
            page {atPage} of {pages}
          </span>
          <Button size="sm" disabled={last >= total || loading} onClick={() => setOffset(offset + PAGE_SIZE)}>Next ›</Button>
        </div>
      )}

      <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '16px' }}>
        <Button variant="primary" onClick={onClose} style={{ justifyContent: 'center' }}>Done</Button>
      </div>
    </Modal>
  )
}
