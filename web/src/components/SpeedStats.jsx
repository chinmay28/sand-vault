import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Empty, Modal, Spinner } from './ui'
import { Section, Stat, Tabs, noteStyle } from './ReadStats'
import { useIsMobile } from '../hooks'

/* How fast the vault is, in Mbps.

   Three speeds, each answering a different question (internal/vault/
   throughput.go has the long version):

   - the vault's: a file's bytes over the time it took to open or store it,
     end to end. What somebody waiting on a file sees.
   - by kind of file: the same, split into images, videos, documents and the
     rest, because a photo is one small read and a film is a stream of chunks.
   - each cloud's: its own calls, one object at a time. A cloud's link, not the
     vault's — a rebuild waits on the k-th quickest of them, not the sum.

   Megabits per second, the unit a connection is sold in, so the figures can
   be held up against what the ISP and the provider promise. Every speed is
   total bytes over total time, never an average of rates. */

/* The kinds the server names, in the words a person uses. */
const KIND_LABELS = {
  images: { label: 'Images', icon: '🖼' },
  video: { label: 'Videos', icon: '🎬' },
  documents: { label: 'Documents', icon: '📄' },
  audio: { label: 'Audio', icon: '🎵' },
  archives: { label: 'Archives', icon: '🗜' },
  other: { label: 'Other files', icon: '📦' },
  thumbnails: { label: 'Thumbnails', icon: '▦' },
}

/* Down (opening, fetching) is the direction a person waits on, so it wears the
   strong colour; up is the lighter wash of the same, as in Transfers. Each bar
   is labelled with its arrow as well, so the two never rest on shade alone. */
const UP_ALPHA = '8c'

export default function SpeedStats({ onClose }) {
  const [span, setSpan] = useState('today')
  const [board, setBoard] = useState(null)
  const [error, setError] = useState(null)
  const mobile = useIsMobile()

  const load = useCallback(async () => {
    try {
      const resp = await api.throughput(span)
      setBoard(resp.throughput)
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [span])

  useEffect(() => {
    setBoard(null)
    load()
    const timer = setInterval(load, 4000)
    return () => clearInterval(timer)
  }, [load])

  const kinds = board?.kinds || []
  const accounts = board?.accounts || []
  const anything = board && (board.open.count > 0 || board.store.count > 0
    || accounts.some((a) => a.up.count > 0 || a.down.count > 0))
  const kindScale = kinds.reduce((max, k) => Math.max(max, k.open.mbps, k.store.mbps), 0)
  const cloudScale = accounts.reduce((max, a) => Math.max(max, a.up.mbps, a.down.mbps), 0)

  return (
    <Modal
      title="Speeds"
      subtitle="How fast files open and store, by kind of file and by cloud, in Mbps"
      onClose={onClose}
      width={620}
      zIndex={110}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <Tabs value={span} onChange={setSpan} />

      {!board && !error && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '18px' }}>
          <Spinner size={12} />
          <span style={noteStyle}>Timing it…</span>
        </div>
      )}

      {board && !anything && (
        <Empty icon="⚡" title={emptyTitle(span)}>
          Nothing was opened or stored in this window, so there is nothing to time.
          Open a file or upload one and the speeds start here.
        </Empty>
      )}

      {board && anything && (
        <>
          <div style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: mobile ? '16px 22px' : '18px 28px',
            paddingBottom: '4px',
          }}>
            <Stat value={`↓ ${formatMbps(board.open.mbps)}`} label="opening files" />
            <Stat value={`↑ ${formatMbps(board.store.mbps)}`} label="storing files" />
            {fastestDown(accounts) && (
              <Stat
                value={formatMbps(fastestDown(accounts).down.mbps)}
                label={`fastest · ${fastestDown(accounts).name}`}
                tone={COLORS.success}
              />
            )}
          </div>
          <p style={{ ...noteStyle, margin: '6px 0 0' }}>
            The vault&apos;s speed: a file&apos;s size over the time from asking for it to having
            it rebuilt, or from handing it over to the last part written.
            {board.open.count > 0 && ` ${board.open.count.toLocaleString()} opened`}
            {board.open.count > 0 && board.store.count > 0 && ','}
            {board.store.count > 0 && ` ${board.store.count.toLocaleString()} stored`}
            {(board.open.count > 0 || board.store.count > 0) && '.'}
          </p>

          <Section
            title="By kind of file"
            hint={`The vault's speed again, split by what was moving. A film is opened a
                   chunk at a time and each chunk counts as one open; a photo is one small
                   read, where the wait for the first answer is most of the time.`}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: '14px' }}>
              {kinds.map((kind) => (
                <SpeedRow
                  key={kind.kind}
                  icon={KIND_LABELS[kind.kind]?.icon || '📦'}
                  name={KIND_LABELS[kind.kind]?.label || kind.kind}
                  color={COLORS.accent}
                  down={{ ...kind.open, label: 'opening', noun: 'open' }}
                  up={{ ...kind.store, label: 'storing', noun: 'store' }}
                  scale={kindScale}
                  idle={`No ${(KIND_LABELS[kind.kind]?.label || kind.kind).toLowerCase()} opened or stored in this window.`}
                />
              ))}
            </div>
          </Section>

          <Section
            title="By cloud"
            hint={`Each account's own link: every object sent to it or fetched from it,
                   one at a time. Fastest download first. Several clouds are asked at once,
                   so the vault can open a file faster than any one of them.`}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: '14px' }}>
              {accounts.map((account) => (
                <SpeedRow
                  key={account.provider_id}
                  swatch={accountColor(account.provider_id)}
                  icon={KIND_ICONS[account.kind] || '☁'}
                  name={account.name || account.provider_id}
                  aside={!account.connected ? 'disconnected' : ''}
                  color={accountColor(account.provider_id)}
                  down={{ ...account.down, label: 'fetching', noun: 'fetch', plural: 'fetches' }}
                  up={{ ...account.up, label: 'sending', noun: 'send' }}
                  scale={cloudScale}
                  idle="Nothing sent to it or fetched from it in this window."
                />
              ))}
            </div>
          </Section>
        </>
      )}

      {board && (
        <div style={{
          marginTop: '22px',
          paddingTop: '14px',
          borderTop: `1px solid ${COLORS.border}`,
        }}>
          <span style={{ ...noteStyle, margin: 0 }}>
            {board.since
              ? `Counting since ${formatDate(board.since)}, kept with the read history. `
                + 'Forget history in Read speed or Transfers clears it too.'
              : 'Nothing counted yet.'}
          </span>
        </div>
      )}
    </Modal>
  )
}

function emptyTitle(span) {
  switch (span) {
    case 'today': return 'Nothing timed today'
    case 'month': return 'Nothing timed this month'
    case 'year': return 'Nothing timed this year'
    default: return 'Nothing timed yet'
  }
}

/* The connected cloud with the quickest downloads, if any has downloaded. */
function fastestDown(accounts) {
  return accounts.filter((a) => a.down.mbps > 0)
    .reduce((best, a) => (!best || a.down.mbps > best.down.mbps ? a : best), null)
}

/* Megabits per second, to the precision the figure deserves: a local disk
   runs in thousands, a struggling link in fractions of one. */
export function formatMbps(mbps) {
  if (!mbps || mbps <= 0) return '—'
  if (mbps >= 1000) return `${(mbps / 1000).toFixed(1)} Gbps`
  if (mbps >= 100) return `${Math.round(mbps)} Mbps`
  if (mbps >= 10) return `${mbps.toFixed(1)} Mbps`
  if (mbps >= 0.01) return `${mbps.toFixed(2)} Mbps`
  return '<0.01 Mbps'
}

/* One row: a name, a bar per direction on the section's shared scale, and the
   figures they are drawn from written out underneath. */
function SpeedRow({ swatch, icon, name, aside, color, down, up, scale, idle }) {
  const quiet = down.count === 0 && up.count === 0
  const lines = [
    { key: 'down', arrow: '↓', speed: down, fill: color },
    { key: 'up', arrow: '↑', speed: up, fill: `${color}${UP_ALPHA}` },
  ]

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: '8px', marginBottom: '6px' }}>
        {swatch && (
          <span aria-hidden="true" style={{
            width: '8px', height: '8px', flexShrink: 0, borderRadius: '2px', background: swatch, alignSelf: 'center',
          }} />
        )}
        <span aria-hidden="true" style={{ fontSize: '11px', flexShrink: 0 }}>{icon}</span>
        <span style={{
          fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text,
          minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{name}</span>
        {aside && <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>· {aside}</span>}
      </div>

      {quiet ? (
        <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>{idle}</div>
      ) : (
        lines.map((line) => (
          <div
            key={line.key}
            title={line.speed.count > 0
              ? `${name}, ${line.speed.label}: ${formatMbps(line.speed.mbps)} — ${formatBytes(line.speed.bytes)} `
                + `in ${line.speed.seconds.toFixed(1)} s over ${line.speed.count.toLocaleString()} `
                + `${plural(line.speed, line.speed.count)}`
              : `${name}: nothing ${line.speed.label} in this window`}
            style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '3px' }}
          >
            <span aria-hidden="true" style={{
              width: '12px', flexShrink: 0, fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
            }}>{line.arrow}</span>
            <span style={{ flex: 1, height: '6px', borderRadius: '3px', background: COLORS.surfaceRaised, minWidth: 0 }}>
              <span style={{
                display: 'block',
                height: '100%',
                width: line.speed.mbps > 0 && scale > 0 ? `${Math.max(1.5, (line.speed.mbps / scale) * 100)}%` : 0,
                borderRadius: '3px',
                background: line.fill,
              }} />
            </span>
            <span style={{
              width: '84px', flexShrink: 0, textAlign: 'right', fontFamily: FONT.mono,
              fontVariantNumeric: 'tabular-nums', fontSize: '10px',
              color: line.speed.mbps > 0 ? COLORS.textDim : COLORS.textMuted,
            }}>{formatMbps(line.speed.mbps)}</span>
          </div>
        ))
      )}

      {!quiet && (
        <div style={{
          marginTop: '5px', fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
          display: 'flex', flexWrap: 'wrap', gap: '4px 10px',
        }}>
          {lines.filter((l) => l.speed.count > 0).map((l) => (
            <span key={l.key}>
              {l.arrow} {l.speed.count.toLocaleString()} {plural(l.speed, l.speed.count)} · {formatBytes(l.speed.bytes)}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

function plural(speed, n) {
  if (n === 1) return speed.noun
  return speed.plural || `${speed.noun}s`
}
