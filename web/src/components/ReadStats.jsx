import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, ConfirmDialog, Empty, Modal, Spinner } from './ui'
import { useIsMobile } from '../hooks'

/* Which cloud is actually answering.

   Reading a file is a race. Every account holding a shard is asked at once and
   the first k to answer rebuild the file; the rest are cut off mid-download.
   That is what makes a wide code quick to read — 4-of-6 reads from whichever
   four clouds are fastest today — and it is also why a cloud can quietly stop
   pulling its weight without anything in the app looking wrong. Nothing gets
   slower. The others simply carry it, until the day two of them are offline
   and the passenger is suddenly load-bearing.

   So this is the race, kept, in three pictures and the figures behind them:
   who is carrying the reads, how long each account takes to answer, and what
   became of every shard it was asked for. Each answers a different question and
   none of them is the same question twice — the share says who is winning, the
   times say why, and the outcomes say whether losing was a fault or just the
   read path doing its job.

   The charts are laid out by hand, like every other one in this app: a bar is a
   box as wide as its share and a column is a box as tall as one. Nothing is
   fetched to draw them, so opening the vault still makes no third-party
   requests at all.

   The figures are kept by the day, so the tabs are additions over the same
   buckets: today, this month, this year, all of it. They survive a restart —
   sealed beside the vault under a key derived from its own, because when a
   vault is read and how much comes off each cloud is the same kind of thing
   the index is. */

/* The spans somebody asks about, in the order they widen. */
const WINDOWS = [
  { key: 'today', label: 'Today' },
  { key: 'month', label: 'Month' },
  { key: 'year', label: 'Year' },
  { key: 'all', label: 'All time' },
]

/* What became of a fetch, and what colour says so.

   A win wears the account's own colour — the same one its card, its shard
   badges and its slice of the share bar wear, so one account is one colour
   everywhere in the app. The two ways of not winning are deliberately grey:
   neither is a fault, and colouring them would make an account that is merely
   slower than four others look like one that is breaking. Only a genuine
   failure is red, which is the one thing here worth somebody's attention. */
const OUTCOMES = [
  { key: 'late', label: 'too late', color: COLORS.textDim,
    hint: 'It answered, but the rebuild already had enough shards.' },
  { key: 'aborted', label: 'cut off', color: COLORS.textMuted,
    hint: 'We cancelled it: enough shards had already arrived. Not a fault.' },
  { key: 'failures', label: 'failed', color: COLORS.error,
    hint: 'The account could not answer at all.' },
]

/* An account is expected to win about k/n of what it enters. Nobody here knows
   k and n — a vault holds files cut every which way — so the comparison is
   against the other accounts rather than against a number, and these two only
   decide when a row is coloured as a worry rather than as a fact. */
const STRUGGLING = 0.15
const IDLE_FLOOR = 4

export default function ReadStats({ onClose }) {
  /* Opens on today, which is the question the panel is most often opened for —
     something is slow right now — and the one tab that moves while you watch
     it. The wider spans are a click away and say so when today is empty. */
  const [span, setSpan] = useState('today')
  const [board, setBoard] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [forgetting, setForgetting] = useState(false)
  const mobile = useIsMobile()

  const load = useCallback(async () => {
    try {
      const resp = await api.readStats(span)
      setBoard(resp.reads)
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [span])

  /* Refetched while the panel is open, because the thing it is about may be
     happening right now — someone opens this in the middle of a film to see
     which cloud is feeding it. The call reads counters already in memory on
     the machine this page came from, so the poll costs a loopback request and
     touches nobody's storage. */
  useEffect(() => {
    // Switching tabs asks again immediately rather than waiting out the poll,
    // and drops what was drawn so a wider window is never briefly shown with a
    // narrower one's figures under it.
    setBoard(null)
    load()
    const timer = setInterval(load, 4000)
    return () => clearInterval(timer)
  }, [load])

  const forget = async () => {
    setBusy(true)
    try {
      const resp = await api.forgetReadStats(span)
      setBoard(resp.reads)
      setError(null)
      setForgetting(false)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const accounts = board?.accounts || []
  const wins = accounts.reduce((sum, a) => sum + a.wins, 0)
  const delivered = accounts.reduce((sum, a) => sum + a.bytes, 0)
  /* The quickest of the accounts that have actually answered something. An
     account with no answers has no time, and a zero would win every time. */
  // Sorted so the two ends of the range can be named: the quickest is the
  // figure at the top of the panel, and the slowest sets the scale the latency
  // chart is drawn against.
  const timed = accounts.filter((a) => a.average_ms > 0)
    .sort((a, b) => a.average_ms - b.average_ms)
  const quickest = timed[0]
  const slowest = timed[timed.length - 1]

  return (
    <Modal
      title="Read speed"
      subtitle="Which cloud answers when a file is rebuilt, and how quickly"
      onClose={onClose}
      width={620}
      zIndex={110}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <Tabs value={span} onChange={setSpan} />

      {!board && !error && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '18px' }}>
          <Spinner size={12} />
          <span style={noteStyle}>Reading the score…</span>
        </div>
      )}

      {board && board.races === 0 && (
        <Empty icon="🏁" title={emptyTitle(span)}>
          {span === 'all'
            ? `Nothing has been rebuilt since counting started, so no cloud has had
               the chance to answer. Open a file, play something, or let a folder draw
               its thumbnails, and the accounts will start racing each other for it.`
            : `Nothing was read in this window. The wider ones above may have
               something in them — the figures go back as far as the vault has been
               open on this machine.`}
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
              value={quickest ? formatMS(quickest.average_ms) : '—'}
              label="quickest"
              tone={COLORS.success}
            />
            {board.shortfalls > 0 && (
              <Stat value={board.shortfalls.toLocaleString()} label="came up short" tone={COLORS.error} />
            )}
          </div>

          <Section
            title="Share of the reads"
            hint={`Every shard that went into a rebuild, by the account that supplied it.
                   An account holding one shard of a 4-of-6 file should be taking
                   something like two races in three; a sliver here is an account whose
                   parts nobody has been able to use.`}
          >
            <ShareBar accounts={accounts} wins={wins} />
          </Section>

          {slowest && (
            <Section
              title="How long an answer takes"
              hint={`The bar is the average and the wash behind it is the spread, from
                     that account's quickest answer to its slowest. A long bar is a cloud
                     that is losing races on speed; a long wash behind a short bar is one
                     that is usually fine and occasionally not. A wash that runs off the
                     end is marked — that account's slowest answer is past the scale the
                     rest of them fit on.`}
            >
              <LatencyBars accounts={accounts} slowest={slowest} />
            </Section>
          )}

          <Section
            title="Who answers"
            hint={`One bar per account, split by what became of every shard it was
                   asked for. Losing is three different things and only the red one is a
                   fault: the read path cancels the accounts it no longer needs, which
                   is the whole point of asking them all at once.`}
          >
            <OutcomeKey />
            <div style={{ display: 'flex', flexDirection: 'column', gap: '14px', marginTop: '14px' }}>
              {accounts.map((account) => (
                <Row key={account.provider_id} account={account} wins={wins} mobile={mobile} />
              ))}
            </div>
          </Section>

          {board.shortfalls > 0 && (
            <Banner tone="warn">
              {board.shortfalls} read{board.shortfalls === 1 ? '' : 's'} could not find enough
              shards to rebuild what was asked for. That is a file that did not open, not a
              cloud that was slow — check the accounts above with failures against them.
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
            {board.since
              ? `Counting since ${formatDate(board.since)}, kept beside the vault and sealed with it.`
              : 'Nothing counted yet.'}
            {board.days > 0 && ` Day by day for the last ${board.days === 1 ? 'day' : `${board.days} days`}.`}
          </span>
          {/* Named differently from the button inside the dialog it opens, so
              that "the one that asks" and "the one that does it" are never the
              same word twice. */}
          <Button size="sm" variant="ghost" onClick={() => setForgetting(true)} disabled={busy}>
            Forget history…
          </Button>
        </div>
      )}

      {forgetting && (
        <ConfirmDialog
          title="Forget the read history?"
          subtitle="Every window, and the file it is kept in"
          confirmLabel="Forget it all"
          busy={busy}
          zIndex={130}
          onConfirm={forget}
          onClose={() => setForgetting(false)}
        >
          This erases what every account has done since counting started — today,
          this month, this year and all of it — and deletes the file it is kept in.
          Nothing else is touched: no file moves, no account changes, and nothing
          stored on a cloud is affected. Counting starts again from the next read.
        </ConfirmDialog>
      )}
    </Modal>
  )
}

/* The four spans, as a row of tabs.

   A segmented control rather than a dropdown: there are four of them, they are
   ordered, and which one you are looking at is the single most important thing
   about every figure underneath — so it is a thing you can see rather than a
   thing you have to open. */
function Tabs({ value, onChange }) {
  return (
    <div role="tablist" aria-label="How far back" style={{
      display: 'flex',
      gap: '2px',
      padding: '2px',
      marginBottom: '18px',
      background: COLORS.surfaceRaised,
      borderRadius: '8px',
    }}>
      {WINDOWS.map((option) => {
        const selected = option.key === value
        return (
          <button
            key={option.key}
            type="button"
            role="tab"
            aria-selected={selected}
            onClick={() => onChange(option.key)}
            style={{
              flex: 1,
              // Past the fingertip floor, so the row is usable on a phone.
              minHeight: '32px',
              padding: '6px 4px',
              border: 'none',
              borderRadius: '6px',
              cursor: 'pointer',
              fontFamily: FONT.mono,
              fontSize: '11px',
              letterSpacing: '0.4px',
              background: selected ? COLORS.surface : 'transparent',
              color: selected ? COLORS.text : COLORS.textMuted,
              boxShadow: selected ? `inset 0 0 0 1px ${COLORS.borderBright}` : 'none',
              transition: 'background 0.15s ease, color 0.15s ease',
            }}
          >{option.label}</button>
        )
      })}
    </div>
  )
}

function emptyTitle(span) {
  switch (span) {
    case 'today': return 'Nothing read today'
    case 'month': return 'Nothing read this month'
    case 'year': return 'Nothing read this year'
    default: return 'No reads yet'
  }
}

/* Who carried the reads: one bar, cut into each account's share of the shards
   that were actually used.

   Part-to-whole, so one bar rather than one per account — the question is what
   fraction of the work each cloud did, and a row of separate bars makes that a
   subtraction. The legend under it names every slice with its figures, because
   a slice is identified by colour and colour alone is never enough: two
   accounts can wear neighbouring hues, and one of the two may be looking at
   them through a colour vision that makes them the same. */
function ShareBar({ accounts, wins }) {
  const winners = accounts.filter((a) => a.wins > 0)
  if (wins === 0 || winners.length === 0) {
    return <p style={{ ...noteStyle, margin: 0 }}>No shards have been used yet.</p>
  }

  return (
    <div>
      <div style={{
        display: 'flex',
        height: '14px',
        borderRadius: '7px',
        // Whatever is left over is the track, which nothing should be: the
        // slices are shares of the wins and they add up to all of them.
        background: COLORS.surfaceRaised,
        overflow: 'hidden',
      }}>
        {winners.map((account, i) => (
          <span
            key={account.provider_id}
            title={`${account.name}: ${account.wins} of ${wins} shards used`}
            style={{
              width: `${(account.wins / wins) * 100}%`,
              // A share that rounds to less than a pixel is still a share: it
              // keeps a hairline rather than disappearing. The gap between
              // slices is the surface showing through, not a border drawn
              // round them — a stroke would add ink that is not data.
              minWidth: '2px',
              flexShrink: 0,
              background: accountColor(account.provider_id),
              marginLeft: i > 0 ? '2px' : 0,
            }}
          />
        ))}
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: '7px', marginTop: '12px' }}>
        {winners.map((account) => (
          <Key
            key={account.provider_id}
            color={accountColor(account.provider_id)}
            label={account.name || account.provider_id}
            value={`${account.wins.toLocaleString()} shard${account.wins === 1 ? '' : 's'}`}
            share={percent(account.wins, wins)}
          />
        ))}
      </div>
    </div>
  )
}

/* How long each account takes to answer.

   Bars rather than columns, and one row each, because the accounts are named
   things rather than points along an axis: a name reads across a row and gets
   truncated under a column, and the figure belongs at the end of the bar it
   describes rather than balanced on top of it.

   The bar is the average. The wash behind it is that account's spread, quickest
   answer to slowest, which is the difference between a cloud that is steadily
   slow and one that is usually quick and occasionally stalls — two problems
   with different fixes.

   The axis is the slowest *average*, not the slowest single answer. One
   pathological fetch would otherwise set the scale and squash every account
   that is behaving into an indistinguishable stub. A spread that runs past the
   end is marked rather than quietly cut: the mark is the honest half of
   choosing a scale that something does not fit on. */
function LatencyBars({ accounts, slowest }) {
  const ceiling = slowest?.average_ms || 0
  if (ceiling <= 0) return null

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
      {accounts.map((account) => {
        const color = accountColor(account.provider_id)
        const answered = account.average_ms > 0
        const spreadFrom = Math.min(account.fastest_ms || 0, account.average_ms || 0)
        const spreadTo = Math.max(account.slowest_ms || 0, account.average_ms || 0)
        const clipped = spreadTo > ceiling

        return (
          <div
            key={account.provider_id}
            title={answered
              ? `${account.name}: ${formatMS(account.average_ms)} average, `
                + `${formatMS(account.fastest_ms)} at its quickest, ${formatMS(account.slowest_ms)} at its slowest`
              : `${account.name}: has not finished an answer`}
            style={{ display: 'flex', alignItems: 'center', gap: '8px' }}
          >
            <span style={{
              width: '84px',
              flexShrink: 0,
              fontFamily: FONT.mono,
              fontSize: '10px',
              color: COLORS.textMuted,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}>{account.name || account.provider_id}</span>

            <span style={{ position: 'relative', flex: 1, height: '10px', minWidth: 0 }}>
              {/* The spread, as a wash rather than a second bar: it is context
                  for the average in front of it, not a value competing with it
                  for the eye. */}
              {answered && spreadTo > spreadFrom && (
                <span aria-hidden="true" style={{
                  position: 'absolute',
                  top: '1px',
                  bottom: '1px',
                  left: `${Math.min(100, (spreadFrom / ceiling) * 100)}%`,
                  right: `${Math.max(0, 100 - Math.min(100, (spreadTo / ceiling) * 100))}%`,
                  background: `${color}2e`,
                  borderRadius: clipped ? '3px 0 0 3px' : '3px',
                }} />
              )}

              {/* The average. Square where it starts, rounded at the end that
                  carries the value. */}
              <span aria-hidden="true" style={{
                position: 'absolute',
                top: '2px',
                bottom: '2px',
                left: 0,
                width: answered ? `${Math.max(1.5, Math.min(100, (account.average_ms / ceiling) * 100))}%` : '2px',
                background: answered ? color : COLORS.border,
                borderRadius: '0 4px 4px 0',
              }} />

              {/* Where the spread leaves the chart. Two hairlines at the edge
                  rather than an arrowhead, which at this size is a smudge. */}
              {clipped && (
                <span aria-hidden="true" title={`slowest answer ${formatMS(account.slowest_ms)}, past the scale`} style={{
                  position: 'absolute',
                  top: 0,
                  bottom: 0,
                  right: '-4px',
                  width: '4px',
                  borderLeft: `1px solid ${color}`,
                  borderRight: `1px solid ${color}`,
                  opacity: 0.7,
                }} />
              )}
            </span>

            <span style={{
              width: '58px',
              flexShrink: 0,
              textAlign: 'right',
              fontFamily: FONT.mono,
              fontVariantNumeric: 'tabular-nums',
              fontSize: '10px',
              color: answered ? COLORS.textDim : COLORS.textMuted,
            }}>{answered ? formatMS(account.average_ms) : '—'}</span>
          </div>
        )
      })}
    </div>
  )
}

/* The legend for the outcome bars. The account's own colour stands for a win,
   which is why that entry has no swatch of its own — there are as many colours
   for it as there are accounts, and the bars below each wear theirs. */
function OutcomeKey() {
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px 14px' }}>
      <LegendItem
        color={null}
        label="won"
        title="The shard arrived in time to be part of the rebuild. Drawn in the account's own colour."
      />
      {OUTCOMES.map((outcome) => (
        <LegendItem key={outcome.key} color={outcome.color} label={outcome.label} title={outcome.hint} />
      ))}
    </div>
  )
}

function LegendItem({ color, label, title }) {
  return (
    <span title={title} style={{ display: 'flex', alignItems: 'center', gap: '5px' }}>
      <span aria-hidden="true" style={{
        width: '8px',
        height: '8px',
        flexShrink: 0,
        borderRadius: '2px',
        // The win swatch is the one that cannot be a colour, so it is drawn as
        // the outline of one.
        background: color || 'transparent',
        border: color ? 'none' : `1px solid ${COLORS.borderBright}`,
      }} />
      <span style={{ fontFamily: FONT.mono, fontSize: '9.5px', color: COLORS.textMuted }}>{label}</span>
    </span>
  )
}

/* One account's standing: the figures, and a bar cut into what became of every
   shard it was asked for.

   The bar is that account's own fetches rather than a share of everybody's, so
   a quiet account is not squeezed into a sliver — the question this one answers
   is "of what this cloud was asked, how much did it deliver", and that is the
   same question whether it was asked twice or two thousand times. Who did the
   most work is the chart at the top. */
function Row({ account, wins, mobile }) {
  const color = accountColor(account.provider_id)
  const raced = account.fetches > 0
  const struggling = raced && account.fetches >= IDLE_FLOOR && account.wins / account.fetches < STRUGGLING

  const segments = raced
    ? [
      { key: 'wins', label: 'won', color, count: account.wins },
      ...OUTCOMES.map((o) => ({ key: o.key, label: o.label, color: o.color, count: account[o.key] })),
    ].filter((segment) => segment.count > 0)
    : []

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: '8px', marginBottom: '5px' }}>
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
        {/* How that share has moved across the window, where the window is
            long enough to have a shape. It is the one thing here that answers
            "is this getting worse" rather than "how is it now", which is the
            question somebody opens a year's worth of figures to ask. */}
        {!mobile && account.trend?.length >= 3 && (
          <TrendLine trend={account.trend} color={color} name={account.name} />
        )}

        <span style={{
          marginLeft: mobile || !(account.trend?.length >= 3) ? 'auto' : '8px',
          flexShrink: 0,
          fontFamily: FONT.mono,
          fontSize: '11px',
          color: struggling ? COLORS.warn : COLORS.textDim,
        }}>
          {account.wins.toLocaleString()} won
          {raced && ` · ${percent(account.wins, account.fetches)} of its races`}
        </span>
      </div>

      {/* Eight pixels rather than six: four segments and three gaps have to be
          told apart inside it, and two of the four are greys a hairline would
          lose against the track. */}
      <div style={{
        display: 'flex',
        height: '8px',
        borderRadius: '4px',
        background: COLORS.surfaceRaised,
        overflow: 'hidden',
      }}>
        {segments.map((segment, i) => (
          <span
            key={segment.key}
            title={`${account.name}: ${segment.count} ${segment.label}`}
            style={{
              width: `${(segment.count / account.fetches) * 100}%`,
              minWidth: '2px',
              flexShrink: 0,
              background: segment.color,
              marginLeft: i > 0 ? '2px' : 0,
            }}
          />
        ))}
      </div>

      {/* The figures the bars are drawn from, written out — which is also the
          table view for anybody the colours are not working for. */}
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
            <span>{account.wins > 0 ? `${percent(account.wins, wins)} of all shards used` : 'no shards used'}</span>
            {account.average_ms > 0 && (
              <span>
                {formatMS(account.average_ms)} average
                {account.slowest_ms > account.fastest_ms &&
                  ` (${formatMS(account.fastest_ms)}–${formatMS(account.slowest_ms)})`}
              </span>
            )}
            {account.bytes > 0 && <span>{formatBytes(account.bytes)} delivered</span>}
            {account.late > 0 && <span>{account.late} too late</span>}
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

/* One account's win rate across the window, as a sparkline.

   The scale is fixed from none of its races to all of them, rather than fitted
   to what this account happens to have done. A sparkline that rescales itself
   draws the same picture for a cloud sliding from 70% to 65% as for one falling
   off a cliff, and the two are not the same news. Nothing is labelled: the
   figures are in the row it sits in, and the line is here to say which way they
   have been going.

   A span nothing was read in is a gap rather than a zero — nobody asked that
   account for anything, which is not the same as it failing to answer — so the
   line is drawn in pieces and each piece needs two points to exist. */
function TrendLine({ trend, color, name }) {
  const width = 68
  const height = 16
  const step = trend.length > 1 ? width / (trend.length - 1) : 0

  const pieces = []
  let run = []
  trend.forEach((point, i) => {
    if (!point.fetches) {
      if (run.length > 1) pieces.push(run)
      run = []
      return
    }
    const rate = point.wins / point.fetches
    run.push([i * step, height - 1 - rate * (height - 2)])
  })
  if (run.length > 1) pieces.push(run)
  if (pieces.length === 0) return null

  const last = pieces[pieces.length - 1][pieces[pieces.length - 1].length - 1]

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      aria-hidden="true"
      focusable="false"
      // Centred rather than sat on the text baseline the row aligns to: a line
      // chart has no baseline of its own and hanging it from one leaves it
      // floating above the figures it belongs to.
      style={{ flexShrink: 0, marginLeft: 'auto', alignSelf: 'center', overflow: 'visible' }}
    >
      <title>{`${name}: share of its own races won, across the window`}</title>
      {pieces.map((piece, i) => (
        <polyline
          key={i}
          points={piece.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ')}
          fill="none"
          stroke={color}
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          opacity="0.9"
        />
      ))}
      {/* Where it stands now, which is the end the eye goes to. */}
      <circle cx={last[0]} cy={last[1]} r="2" fill={color} />
    </svg>
  )
}

/* A swatch and a name, for the share bar's legend. Identity never rests on the
   colour alone: every slice is named here with its figures beside it, so the
   bar is a picture of something the panel has already said in words. */
function Key({ color, label, value, share }) {
  return (
    <div style={{ display: 'flex', alignItems: 'baseline', gap: '7px' }}>
      <span aria-hidden="true" style={{
        width: '9px',
        height: '9px',
        flexShrink: 0,
        borderRadius: '2px',
        background: color,
        alignSelf: 'center',
      }} />
      <span style={{
        fontFamily: FONT.mono,
        fontSize: '11px',
        color: COLORS.textDim,
        minWidth: 0,
        overflow: 'hidden',
        textOverflow: 'ellipsis',
        whiteSpace: 'nowrap',
      }}>{label}</span>
      <span style={{
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.text, marginLeft: 'auto', flexShrink: 0,
      }}>{value}</span>
      <span style={{
        fontFamily: FONT.mono,
        fontSize: '10px',
        color: COLORS.textMuted,
        minWidth: '38px',
        textAlign: 'right',
        flexShrink: 0,
      }}>{share}</span>
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
