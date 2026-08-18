import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, Empty, Modal, Spinner } from './ui'
import { useIsMobile } from '../hooks'

/* Which cloud is actually answering.

   Reading a file is a race. Every account holding a shard is asked at once and
   the first k to answer rebuild the file; the rest are cut off mid-download.
   That is what makes a wide code quick to read — 4-of-6 reads from whichever
   four clouds are fastest today — and it is also why a cloud can quietly stop
   pulling its weight without anything in the app looking wrong. Nothing gets
   slower. The others simply carry it, until the day two of them are offline
   and the passenger is suddenly load-bearing.

   So this is the race, kept. One row per account: how many races it entered,
   how many it won, and how long its answers take. The bar is its share of the
   wins, which is the figure to read across accounts — an account holding one
   shard of a 4-of-6 file should be winning about two races in three, and one
   winning none is the finding.

   The figures are the server's, since it came up. Nothing is stored: counting
   reads into the vault file would mean a write on every chunk of every stream,
   to the one file everything else depends on. */

/* An account is expected to win about k/n of what it enters. Nobody here knows
   k and n — a vault holds files cut every which way — so the comparison is
   against the other accounts rather than against a number, and these two only
   decide when a row is coloured as a worry rather than as a fact. */
const STRUGGLING = 0.15
const IDLE_FLOOR = 4

export default function ReadStats({ onClose }) {
  const [board, setBoard] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const mobile = useIsMobile()

  const load = useCallback(async () => {
    try {
      const resp = await api.readStats()
      setBoard(resp.reads)
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [])

  /* Refetched while the panel is open, because the thing it is about may be
     happening right now — someone opens this in the middle of a film to see
     which cloud is feeding it. The call reads counters already in memory on
     the machine this page came from, so the poll costs a loopback request and
     touches nobody's storage. */
  useEffect(() => {
    load()
    const timer = setInterval(load, 4000)
    return () => clearInterval(timer)
  }, [load])

  const reset = async () => {
    setBusy(true)
    try {
      const resp = await api.resetReadStats()
      setBoard(resp.reads)
      setError(null)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const accounts = board?.accounts || []
  const wins = accounts.reduce((sum, a) => sum + a.wins, 0)
  const best = accounts.reduce((max, a) => Math.max(max, a.wins), 0)
  const delivered = accounts.reduce((sum, a) => sum + a.bytes, 0)
  /* The quickest of the accounts that have actually answered something. An
     account with no answers has no time, and a zero would win every time. */
  const quickest = accounts
    .filter((a) => a.average_ms > 0)
    .sort((a, b) => a.average_ms - b.average_ms)[0]

  return (
    <Modal
      title="Read speed"
      subtitle="Which cloud answers when a file is rebuilt, and how quickly"
      onClose={onClose}
      width={620}
      zIndex={110}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {!board && !error && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <Spinner size={12} />
          <span style={noteStyle}>Reading the score…</span>
        </div>
      )}

      {board && board.races === 0 && (
        <Empty icon="🏁" title="No reads yet">
          Nothing has been rebuilt since counting started, so no cloud has had
          the chance to answer. Open a file, play something, or let a folder draw
          its thumbnails, and the accounts will start racing each other for it.
        </Empty>
      )}

      {board && board.races > 0 && (
        <>
          <div style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: mobile ? '16px 22px' : '18px 28px',
            paddingBottom: '4px',
          }}>
            <Stat value={board.races.toLocaleString()} label={board.races === 1 ? 'race' : 'races'} />
            <Stat value={wins.toLocaleString()} label={wins === 1 ? 'shard used' : 'shards used'} />
            <Stat value={formatBytes(delivered)} label="delivered" />
            <Stat
              value={quickest ? `${formatMS(quickest.average_ms)}` : '—'}
              label="quickest"
              tone={COLORS.success}
            />
            {board.shortfalls > 0 && (
              <Stat value={board.shortfalls.toLocaleString()} label="came up short" tone={COLORS.error} />
            )}
          </div>

          <Section
            title="Who answers"
            hint={`A read asks every account holding a shard and rebuilds from the first
                   to answer, cutting off the rest — so the bar is each account's share of
                   the shards actually used. An account that enters races and wins none is
                   holding parts nobody has been able to use.`}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: '14px' }}>
              {accounts.map((account) => (
                <Row key={account.provider_id} account={account} best={best} wins={wins} />
              ))}
            </div>
          </Section>

          {board.shortfalls > 0 && (
            <Banner tone="warn">
              {board.shortfalls} read{board.shortfalls === 1 ? '' : 's'} could not find enough
              shards to rebuild what was asked for. That is a file that did not open, not a
              cloud that was slow — check the accounts below with failures against them.
            </Banner>
          )}
        </>
      )}

      {board && (
        <div style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          flexWrap: 'wrap',
          gap: '10px',
          marginTop: '22px',
          paddingTop: '14px',
          borderTop: `1px solid ${COLORS.border}`,
        }}>
          <span style={{ ...noteStyle, margin: 0 }}>
            Counting since {formatDate(board.since)}. Nothing is stored — a restart starts it again.
          </span>
          <Button size="sm" variant="ghost" onClick={reset} disabled={busy}>
            {busy ? 'Clearing…' : 'Start again'}
          </Button>
        </div>
      )}
    </Modal>
  )
}

/* One account's standing.

   The wins are the headline and the rest of the race is the line underneath,
   because "lost" is four different things and only one of them is a fault: an
   answer that arrived too late to be needed, a download we cut off ourselves
   once enough had arrived, and an account that could not answer at all. Only
   the last is worth anybody's attention, so only the last is coloured. */
function Row({ account, best, wins }) {
  const color = accountColor(account.provider_id)
  const share = wins > 0 ? account.wins / wins : 0
  const raced = account.fetches > 0
  const struggling = raced && account.fetches >= IDLE_FLOOR && account.wins / account.fetches < STRUGGLING

  return (
    <div>
      <div style={{
        display: 'flex',
        alignItems: 'baseline',
        gap: '8px',
        marginBottom: '5px',
      }}>
        <span aria-hidden="true" style={{
          width: '8px',
          height: '8px',
          flexShrink: 0,
          borderRadius: '2px',
          background: color,
          alignSelf: 'center',
        }} />
        <span style={{
          fontFamily: FONT.mono,
          fontSize: '11.5px',
          color: COLORS.text,
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{account.name || account.provider_id}</span>
        <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {KIND_ICONS[account.kind] || '☁'}
          {!account.connected && ' · disconnected'}
        </span>
        <span style={{
          marginLeft: 'auto',
          flexShrink: 0,
          fontFamily: FONT.mono,
          fontSize: '11px',
          color: struggling ? COLORS.warn : COLORS.textDim,
        }}>
          {account.wins.toLocaleString()} won
          {raced && ` · ${percent(account.wins, account.fetches)} of its races`}
        </span>
      </div>

      <div
        title={`${account.name}: ${account.wins} of the ${wins} shards used`}
        style={{ height: '6px', borderRadius: '3px', background: COLORS.surfaceRaised }}
      >
        <div style={{
          width: `${best > 0 ? Math.max(account.wins > 0 ? 2 : 0, (account.wins / best) * 100) : 0}%`,
          height: '100%',
          borderRadius: '3px',
          background: color,
        }} />
      </div>

      <div style={{
        marginTop: '5px',
        fontFamily: FONT.mono,
        fontSize: '10px',
        color: COLORS.textMuted,
        display: 'flex',
        flexWrap: 'wrap',
        gap: '4px 10px',
      }}>
        {!raced && <span>Not asked yet — it holds no part of anything that has been read.</span>}
        {raced && (
          <>
            <span>{share > 0 ? `${percent(account.wins, wins)} of all shards used` : 'no shards used'}</span>
            {account.average_ms > 0 && (
              <span title={`fastest ${formatMS(account.fastest_ms)}, slowest ${formatMS(account.slowest_ms)}`}>
                {formatMS(account.average_ms)} average
              </span>
            )}
            {account.bytes > 0 && <span>{formatBytes(account.bytes)} delivered</span>}
            {account.late > 0 && <span>{account.late} too late</span>}
            {/* Named as ours rather than as theirs: the read path cancelled
                these the moment it had enough, and a cloud is not at fault for
                being the fifth of six to answer. */}
            {account.aborted > 0 && <span>{account.aborted} cut off</span>}
            {account.failures > 0 && (
              <span style={{ color: COLORS.error }}>{account.failures} failed</span>
            )}
          </>
        )}
      </div>

      {account.last_error && (
        <div style={{
          marginTop: '4px',
          fontFamily: FONT.mono,
          fontSize: '10px',
          color: COLORS.error,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }} title={account.last_error}>
          {account.last_error}
        </div>
      )}
    </div>
  )
}

/* A heading over one section of the panel. */
function Section({ title, hint, children }) {
  return (
    <section style={{ marginTop: '22px' }}>
      <h3 style={{
        margin: '0 0 3px',
        fontFamily: FONT.mono,
        fontSize: '10px',
        fontWeight: 700,
        letterSpacing: '1.4px',
        textTransform: 'uppercase',
        color: COLORS.textDim,
      }}>{title}</h3>
      {hint && <p style={{ ...noteStyle, margin: '0 0 12px' }}>{hint}</p>}
      {children}
    </section>
  )
}

/* One number and what it counts, for the row across the top. */
function Stat({ value, label, tone }) {
  return (
    <div style={{ minWidth: '72px' }}>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '16px',
        fontWeight: 600,
        letterSpacing: '-0.4px',
        color: tone || COLORS.text,
      }}>{value}</div>
      <div style={{
        marginTop: '3px',
        fontFamily: FONT.mono,
        fontSize: '9px',
        fontWeight: 600,
        letterSpacing: '1px',
        textTransform: 'uppercase',
        color: COLORS.textMuted,
      }}>{label}</div>
    </div>
  )
}

/* A cloud answers in tens or hundreds of milliseconds, a folder on the same
   disk in a fraction of one, and both belong on this list. So the unit follows
   the figure rather than the other way round: seconds at the slow end instead
   of five digits of milliseconds, and microseconds at the quick end instead of
   a local disk reading as 0.0 ms — which says "no time at all" where the whole
   point of the column is that some accounts take longer than others. */
function formatMS(ms) {
  if (!ms) return '—'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)} s`
  if (ms >= 10) return `${Math.round(ms)} ms`
  if (ms >= 1) return `${ms.toFixed(1)} ms`
  return `${Math.round(ms * 1000)} µs`
}

function percent(part, whole) {
  if (!whole) return '0%'
  const share = (part / whole) * 100
  if (share > 0 && share < 1) return '<1%'
  return `${Math.round(share)}%`
}

const noteStyle = {
  fontFamily: FONT.sans,
  fontSize: '11.5px',
  color: COLORS.textMuted,
  lineHeight: 1.6,
}
