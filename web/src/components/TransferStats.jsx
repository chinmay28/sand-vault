import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, ConfirmDialog, Empty, Modal, Spinner } from './ui'
import { Key, Section, Stat, Tabs, noteStyle, percent } from './ReadStats'
import { useIsMobile } from '../hooks'

/* How much has gone to each cloud, and how much has come back off it.

   Read speed is about who is quick. This is about who is busy: every object
   the vault has put on an account or fetched off one — uploads, the reads that
   rebuild a file, moves between clouds, conversions, the index backup — summed
   by account and by direction. It is the figure that matters at a provider
   that bills for egress or throttles an account that has moved too much today.

   In and out are named from the cloud's side, because the cloud is the row:
   *in* is what was sent to it, *out* is what was fetched off it. Only objects
   that arrived whole are counted; a fetch the read path cut off once it had
   enough shards moved some bytes, but nothing here can say how many.

   Same windows, same history and same forget as Read speed — the figures are
   kept in the same sealed file beside the vault. */

/* The two directions, and how each is drawn. Out wears the account's colour
   at full strength and in a lighter wash of it, so the two bars of one account
   are recognisably the same account and still told apart without a legend
   lookup — and each is labelled with an arrow besides, so the difference never
   rests on the shade alone. */
const DIRECTIONS = [
  { key: 'bytes_in', label: 'sent in', arrow: '↑', hint: 'Written to the cloud: uploads, moves onto it, the index backup.' },
  { key: 'bytes_out', label: 'fetched out', arrow: '↓', hint: 'Read off the cloud: rebuilding files, moves off it, conversions.' },
]

/* Lighter for "in", by mixing the account colour with the surface. A wash
   rather than opacity, so the bar does not take on whatever is behind it. */
const IN_ALPHA = '8c'

export default function TransferStats({ onClose }) {
  const [span, setSpan] = useState('today')
  const [board, setBoard] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [forgetting, setForgetting] = useState(false)
  const mobile = useIsMobile()

  const load = useCallback(async () => {
    try {
      const resp = await api.transferStats(span)
      setBoard(resp.transfers)
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [span])

  /* Polled while open, like Read speed: an upload or a film playing is moving
     the figures as you watch, and the call is a loopback read of counters. */
  useEffect(() => {
    setBoard(null)
    load()
    const timer = setInterval(load, 4000)
    return () => clearInterval(timer)
  }, [load])

  const forget = async () => {
    setBusy(true)
    try {
      await api.forgetReadStats(span)
      await load()
      setForgetting(false)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const accounts = board?.accounts || []
  const moved = (board?.bytes_in || 0) + (board?.bytes_out || 0)
  const objects = accounts.reduce((sum, a) => sum + a.puts + a.gets, 0)
  const failures = accounts.reduce((sum, a) => sum + a.put_failures + a.get_failures, 0)

  return (
    <Modal
      title="Transfers"
      subtitle="How much has been sent to each cloud and fetched back off it"
      onClose={onClose}
      width={620}
      zIndex={110}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <Tabs value={span} onChange={setSpan} />

      {!board && !error && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '18px' }}>
          <Spinner size={12} />
          <span style={noteStyle}>Adding it up…</span>
        </div>
      )}

      {board && moved === 0 && failures === 0 && (
        <Empty icon="⇅" title={emptyTitle(span)}>
          {span === 'all'
            ? `Nothing has been sent to a cloud or fetched off one since counting
               started. Upload something, or open a file, and the figures start here.`
            : `Nothing moved in this window. The wider ones above may have
               something in them.`}
        </Empty>
      )}

      {board && (moved > 0 || failures > 0) && (
        <>
          <div style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: mobile ? '16px 22px' : '18px 28px',
            paddingBottom: '4px',
          }}>
            <Stat value={`↑ ${formatBytes(board.bytes_in)}`} label="sent in" />
            <Stat value={`↓ ${formatBytes(board.bytes_out)}`} label="fetched out" />
            <Stat value={objects.toLocaleString()} label={objects === 1 ? 'object' : 'objects'} />
            {failures > 0 && (
              <Stat value={failures.toLocaleString()} label={failures === 1 ? 'failure' : 'failures'} tone={COLORS.error} />
            )}
          </div>

          <Section
            title="Share of the traffic"
            hint={`Each direction as one bar, cut into the accounts that carried it.
                   Under an even spread every account takes about the same share of
                   what goes in; what comes out leans towards the clouds that win the
                   read race.`}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: '18px' }}>
              {DIRECTIONS.map((dir) => (
                <DirectionShare key={dir.key} dir={dir} accounts={accounts} total={board[dir.key]} />
              ))}
            </div>
          </Section>

          {board.trend?.length >= 3 && (
            <Section
              title="Across the window"
              hint={`The whole vault's traffic span by span: what was sent in rises
                     above the line, what was fetched out hangs below it, on the same
                     scale.`}
            >
              <TrafficColumns trend={board.trend} mobile={mobile} />
            </Section>
          )}

          <Section
            title="By cloud"
            hint={`Both directions per account on one shared scale, so the longest
                   bar on the page is the busiest account in the busiest direction.`}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: '14px' }}>
              {accounts.map((account) => (
                <Row key={account.provider_id} account={account} scale={busiest(accounts)} />
              ))}
            </div>
          </Section>

          {failures > 0 && (
            <Banner tone="warn">
              {failures} transfer{failures === 1 ? '' : 's'} could not be completed. A failed
              send is retried or reported where it happened; this is the tally, so an account
              that keeps failing stands out here.
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
              ? `Counting since ${formatDate(board.since)}, kept with the read history and sealed with the vault.`
              : 'Nothing counted yet.'}
          </span>
          <Button size="sm" variant="ghost" onClick={() => setForgetting(true)} disabled={busy}>
            Forget history…
          </Button>
        </div>
      )}

      {forgetting && (
        <ConfirmDialog
          title="Forget the statistics?"
          subtitle="Read speed and transfers, every window, and the file they are kept in"
          confirmLabel="Forget it all"
          busy={busy}
          zIndex={130}
          onConfirm={forget}
          onClose={() => setForgetting(false)}
        >
          This erases everything counted since counting started — what was sent to
          and fetched from every account, and which of them answered the reads —
          and deletes the file it is kept in. No file moves and nothing stored on
          a cloud is touched. Counting starts again from the next transfer.
        </ConfirmDialog>
      )}
    </Modal>
  )
}

function emptyTitle(span) {
  switch (span) {
    case 'today': return 'Nothing moved today'
    case 'month': return 'Nothing moved this month'
    case 'year': return 'Nothing moved this year'
    default: return 'No transfers yet'
  }
}

/* The single largest figure on the board, in either direction — the scale every
   per-account bar is drawn against. */
export function busiest(accounts) {
  return accounts.reduce((max, a) => Math.max(max, a.bytes_in || 0, a.bytes_out || 0), 0)
}

/* One direction's traffic as a part-to-whole bar, with every slice named
   underneath. Same shape as Read speed's share bar, and for the same reason:
   the question is what fraction each cloud carried. */
function DirectionShare({ dir, accounts, total }) {
  const carriers = accounts.filter((a) => a[dir.key] > 0)
  const shade = (id) => (dir.key === 'bytes_in' ? `${accountColor(id)}${IN_ALPHA}` : accountColor(id))

  return (
    <div title={dir.hint}>
      <div style={{
        display: 'flex',
        alignItems: 'baseline',
        justifyContent: 'space-between',
        marginBottom: '6px',
        fontFamily: FONT.mono,
        fontSize: '10.5px',
        color: COLORS.textDim,
      }}>
        <span>{dir.arrow} {dir.label}</span>
        <span style={{ color: COLORS.textMuted }}>{formatBytes(total)}</span>
      </div>

      {carriers.length === 0 ? (
        <p style={{ ...noteStyle, margin: 0 }}>Nothing in this direction.</p>
      ) : (
        <>
          <div style={{
            display: 'flex',
            height: '12px',
            borderRadius: '6px',
            background: COLORS.surfaceRaised,
            overflow: 'hidden',
          }}>
            {carriers.map((account, i) => (
              <span
                key={account.provider_id}
                title={`${account.name}: ${formatBytes(account[dir.key])} ${dir.label}`}
                style={{
                  width: `${(account[dir.key] / total) * 100}%`,
                  minWidth: '2px',
                  flexShrink: 0,
                  background: shade(account.provider_id),
                  marginLeft: i > 0 ? '2px' : 0,
                }}
              />
            ))}
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '6px', marginTop: '10px' }}>
            {carriers.map((account) => (
              <Key
                key={account.provider_id}
                color={shade(account.provider_id)}
                label={account.name || account.provider_id}
                value={formatBytes(account[dir.key])}
                share={percent(account[dir.key], total)}
              />
            ))}
          </div>
        </>
      )}
    </div>
  )
}

/* The vault's traffic across the window: in above a shared baseline, out below
   it, one column pair per span. One axis for both, since both are bytes — a
   second scale would let either direction look as big as the other. Only the
   peak of each direction is labelled; the rest are a hover away. */
function TrafficColumns({ trend, mobile }) {
  const peak = trend.reduce((max, p) => Math.max(max, p.bytes_in, p.bytes_out), 0)
  if (peak <= 0) return <p style={{ ...noteStyle, margin: 0 }}>Nothing moved in this window.</p>

  const half = 48
  const peakIn = trend.reduce((at, p, i) => (p.bytes_in > trend[at].bytes_in ? i : at), 0)
  const peakOut = trend.reduce((at, p, i) => (p.bytes_out > trend[at].bytes_out ? i : at), 0)
  const height = (bytes) => (bytes > 0 ? Math.max(2, (bytes / peak) * half) : 0)

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'stretch', gap: mobile ? '2px' : '3px' }}>
        {trend.map((point, i) => (
          <div
            key={point.from}
            title={`${spanLabel(point)}: ↑ ${formatBytes(point.bytes_in)} sent in, ↓ ${formatBytes(point.bytes_out)} fetched out`}
            style={{ flex: '1 1 0', maxWidth: '28px', display: 'flex', flexDirection: 'column', alignItems: 'center' }}
          >
            <div style={{ height: `${half}px`, width: '100%', display: 'flex', flexDirection: 'column', justifyContent: 'flex-end', alignItems: 'center' }}>
              {i === peakIn && point.bytes_in > 0 && <PeakLabel bytes={point.bytes_in} />}
              <span style={{
                width: '100%',
                height: `${height(point.bytes_in)}px`,
                background: `${COLORS.accent}${IN_ALPHA}`,
                borderRadius: '3px 3px 0 0',
              }} />
            </div>
            <div style={{ height: '1px', width: '100%', background: COLORS.border, margin: '1px 0' }} />
            <div style={{ height: `${half}px`, width: '100%', display: 'flex', flexDirection: 'column', alignItems: 'center' }}>
              <span style={{
                width: '100%',
                height: `${height(point.bytes_out)}px`,
                background: COLORS.accent,
                borderRadius: '0 0 3px 3px',
              }} />
              {i === peakOut && point.bytes_out > 0 && <PeakLabel bytes={point.bytes_out} />}
            </div>
          </div>
        ))}
      </div>
      <div style={{
        display: 'flex',
        justifyContent: 'space-between',
        marginTop: '6px',
        fontFamily: FONT.mono,
        fontSize: '9.5px',
        color: COLORS.textMuted,
      }}>
        <span>{spanLabel(trend[0])}</span>
        <span>↑ sent in · ↓ fetched out</span>
        <span>{spanLabel(trend[trend.length - 1])}</span>
      </div>
    </div>
  )
}

function PeakLabel({ bytes }) {
  return (
    <span style={{
      fontFamily: FONT.mono,
      fontSize: '9px',
      color: COLORS.textMuted,
      whiteSpace: 'nowrap',
      margin: '2px 0',
    }}>{formatBytes(bytes)}</span>
  )
}

function spanLabel(point) {
  const from = new Date(point.from)
  const opts = { month: 'short', day: 'numeric' }
  if (point.days <= 1) return from.toLocaleDateString(undefined, opts)
  const to = new Date(new Date(point.to).getTime() - 1)
  return `${from.toLocaleDateString(undefined, opts)}–${to.toLocaleDateString(undefined, opts)}`
}

/* One account: its name, a bar for each direction on the board's shared scale,
   and the figures the bars are drawn from written out underneath. */
function Row({ account, scale }) {
  const color = accountColor(account.provider_id)
  const fails = account.put_failures + account.get_failures
  const idle = account.bytes_in === 0 && account.bytes_out === 0 && fails === 0

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: '8px', marginBottom: '6px' }}>
        <span aria-hidden="true" style={{
          width: '8px', height: '8px', flexShrink: 0, borderRadius: '2px', background: color, alignSelf: 'center',
        }} />
        <span style={{
          fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text,
          minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{account.name || account.provider_id}</span>
        <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {KIND_ICONS[account.kind] || '☁'}
          {!account.connected && ' · disconnected'}
        </span>
        <span style={{ marginLeft: 'auto', flexShrink: 0, fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim }}>
          {formatBytes(account.bytes_in + account.bytes_out)}
        </span>
      </div>

      {!idle && DIRECTIONS.map((dir) => (
        <div key={dir.key} style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '3px' }}>
          <span aria-hidden="true" style={{ width: '12px', flexShrink: 0, fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
            {dir.arrow}
          </span>
          <span style={{ flex: 1, height: '6px', borderRadius: '3px', background: COLORS.surfaceRaised, minWidth: 0 }}>
            <span
              title={`${account.name}: ${formatBytes(account[dir.key])} ${dir.label}`}
              style={{
                display: 'block',
                height: '100%',
                width: account[dir.key] > 0 && scale > 0 ? `${Math.max(1.5, (account[dir.key] / scale) * 100)}%` : 0,
                borderRadius: '3px',
                background: dir.key === 'bytes_in' ? `${color}${IN_ALPHA}` : color,
              }}
            />
          </span>
          <span style={{
            width: '64px', flexShrink: 0, textAlign: 'right', fontFamily: FONT.mono,
            fontVariantNumeric: 'tabular-nums', fontSize: '10px', color: COLORS.textDim,
          }}>{formatBytes(account[dir.key])}</span>
        </div>
      ))}

      <div style={{
        marginTop: '5px', fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
        display: 'flex', flexWrap: 'wrap', gap: '4px 10px',
      }}>
        {idle && <span>Nothing sent to it or fetched from it in this window.</span>}
        {!idle && (
          <>
            <span>{account.puts.toLocaleString()} sent · {account.gets.toLocaleString()} fetched</span>
            {account.put_failures > 0 && <span style={{ color: COLORS.error }}>{account.put_failures} sends failed</span>}
            {account.get_failures > 0 && <span style={{ color: COLORS.error }}>{account.get_failures} fetches failed</span>}
            {account.last_at && <span>last {formatDate(account.last_at)}</span>}
          </>
        )}
      </div>
    </div>
  )
}
