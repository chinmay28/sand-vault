import React from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { Modal } from './ui'
import { UsageBar, usageBreakdown } from './CloudStats'
import { Key, Section, Stat, noteStyle, percent } from './ReadStats'
import { useIsMobile } from '../hooks'

/* Where the room is, across every cloud at once.

   An account's own card says how full that account is. This adds them up: all
   the space the connected clouds have between them, how much of it is SAND's
   parts, how much is other people's files, and how much is left — as one pie,
   and then cloud by cloud, fullest first, since the cloud about to run out is
   the one that decides where the next upload can go.

   Nothing is fetched. The figures are the ones the accounts drawer already
   holds — what each account reported on its last ping, what a count of a
   bucket found, the quota somebody set, and what the index says SAND wrote —
   read through the same usageBreakdown every account card uses, so this page
   and the cards can never disagree. */

/* What the pie is made of, in the order it is drawn: each cloud's share of the
   parts in its own colour, then the regions that are not ours in neutrals,
   dimmest last, because the pie is read for what is filling it. */
const OTHER = { key: 'other', label: 'Other files', color: COLORS.textMuted,
  hint: 'Already on the accounts, and not SAND\'s: photos in the Drive, other things on the disk.' }
const FREE = { key: 'free', label: 'Free', color: COLORS.borderBright,
  hint: 'Room that can still be written to.' }
const RESERVED = { key: 'reserved', label: 'Out of reach', color: COLORS.border,
  hint: 'Neither used nor writable: a filesystem\'s root reserve, or the part of a disk outside a quota.' }

/* Past this an account is drawn as filling up. */
const NEARLY_FULL = 0.9

/* Totals across the accounts, for the pie and the figures over it.

   Only accounts that know how big they are go into the pie: a slice of a whole
   nobody can measure is not a slice of anything. The parts on the others are
   still SAND's and still counted in `stored`, and are named under the pie. */
export function storageTotals(providers) {
  const rows = (providers || []).map((p) => ({ provider: p, space: usageBreakdown(p) }))
  const measured = rows.filter((r) => r.space.known && r.space.total > 0)
  const unmeasured = rows.filter((r) => !(r.space.known && r.space.total > 0))

  const sum = (list, key) => list.reduce((total, r) => total + (r.space[key] || 0), 0)
  return {
    rows,
    measured,
    unmeasured,
    capacity: sum(measured, 'total'),
    sand: sum(measured, 'sand'),
    other: sum(measured, 'other'),
    free: sum(measured, 'free'),
    reserved: sum(measured, 'reserved'),
    // SAND's parts everywhere, including on accounts with no capacity to
    // measure them against.
    stored: rows.reduce((total, r) => total + Math.max(0, r.provider.stored || 0), 0),
  }
}

/* How full one account is, 0–1, or -1 when it cannot say: everything that
   cannot be written to any more, which includes a reserve nobody can use. It
   is the figure that decides whether the next upload fits, so it is what the
   rows are sorted and warned by — "used" above is only what is on it. */
function fullness(space) {
  if (!space.known || !space.total) return -1
  return Math.min(1, (space.total - space.free) / space.total)
}

export default function StorageBreakdown({ providers = [], stats, onClose }) {
  const mobile = useIsMobile()
  const totals = storageTotals(providers)
  const used = totals.sand + totals.other
  const overhead = stats?.bytes > 0 && stats?.stored_bytes > 0 ? stats.stored_bytes / stats.bytes : 0

  const slices = [
    ...totals.measured
      .filter((r) => r.space.sand > 0)
      .map((r) => ({
        key: r.provider.id,
        label: r.provider.name || r.provider.id,
        color: accountColor(r.provider.id),
        bytes: r.space.sand,
        hint: `SAND's parts on ${r.provider.name}`,
      })),
    { ...OTHER, bytes: totals.other },
    { ...FREE, bytes: totals.free },
    { ...RESERVED, bytes: totals.reserved },
  ].filter((s) => s.bytes > 0)

  // Fullest first; the ones that cannot say go last, by name.
  const byFullness = [...totals.rows].sort((a, b) => {
    const fa = fullness(a.space)
    const fb = fullness(b.space)
    if (fa !== fb) return fb - fa
    return (a.provider.name || '').localeCompare(b.provider.name || '')
  })
  const nearlyFull = totals.measured.filter((r) => fullness(r.space) >= NEARLY_FULL).length

  return (
    <Modal
      title="Storage breakdown"
      subtitle="How much room the clouds have between them, and what is using it"
      onClose={onClose}
      width={620}
      zIndex={110}
    >
      {providers.length === 0 ? (
        <p style={noteStyle}>No clouds are connected, so there is no space to break down.</p>
      ) : (
        <>
          <div style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: mobile ? '16px 22px' : '18px 28px',
            paddingBottom: '4px',
          }}>
            <Stat value={totals.capacity ? formatBytes(totals.capacity) : '—'} label="capacity" />
            <Stat value={totals.capacity ? percent(used, totals.capacity) : '—'} label="used" />
            <Stat value={totals.capacity ? formatBytes(totals.free) : '—'} label="free" tone={COLORS.success} />
            <Stat value={formatBytes(totals.stored)} label="SAND parts" />
            {overhead > 0 && (
              <Stat value={`${overhead.toFixed(2)}×`} label="stored per byte" />
            )}
          </div>

          <Section
            title="Overall allocation"
            hint={`Every cloud that knows its own size, added together. Each coloured
                   slice is SAND's parts on one cloud; the greys are everything else
                   on them and the room that is left.`}
          >
            {totals.measured.length === 0 ? (
              <p style={{ ...noteStyle, margin: 0 }}>
                None of the connected clouds says how big it is, and no quota is set
                on any of them, so there is no whole to draw a share of. Edit an
                account to set its capacity or a quota.
              </p>
            ) : (
              <div style={{
                display: 'flex',
                flexDirection: mobile ? 'column' : 'row',
                alignItems: mobile ? 'center' : 'flex-start',
                gap: mobile ? '16px' : '24px',
              }}>
                <Donut
                  slices={slices}
                  total={totals.capacity}
                  centre={percent(used, totals.capacity)}
                  caption="used"
                />
                <div style={{ display: 'flex', flexDirection: 'column', gap: '7px', flex: 1, minWidth: 0, width: '100%' }}>
                  {slices.map((slice) => (
                    <div key={slice.key} title={slice.hint}>
                      <Key
                        color={slice.color}
                        label={slice.label}
                        value={formatBytes(slice.bytes)}
                        share={percent(slice.bytes, totals.capacity)}
                      />
                    </div>
                  ))}
                </div>
              </div>
            )}
            {totals.unmeasured.length > 0 && (
              <p style={{ ...noteStyle, margin: '12px 0 0' }}>
                Not in the pie: {totals.unmeasured.map((r) => r.provider.name || r.provider.id).join(', ')}
                {' '}— {totals.unmeasured.length === 1 ? 'it does' : 'they do'} not say how big{' '}
                {totals.unmeasured.length === 1 ? 'it is' : 'they are'}, though the parts on{' '}
                {totals.unmeasured.length === 1 ? 'it' : 'them'} are counted in SAND parts above.
              </p>
            )}
          </Section>

          <Section
            title="Where the parts are"
            hint={`SAND's own parts by the cloud holding them, on every account
                   whether or not it knows its size. An even spread is what keeps any
                   one cloud from filling up first.`}
          >
            <PartsShare rows={totals.rows} stored={totals.stored} />
          </Section>

          <Section
            title="Cloud by cloud"
            hint={`Fullest first. The bar is the account's own space: its colour is
                   SAND's parts, grey is everything else on it, and the dim end is
                   what is free.`}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: '14px' }}>
              {byFullness.map(({ provider, space }) => (
                <CloudRow key={provider.id} provider={provider} space={space} />
              ))}
            </div>
            {nearlyFull > 0 && (
              <p style={{ ...noteStyle, margin: '12px 0 0', color: COLORS.warn }}>
                {nearlyFull === 1 ? 'One cloud is' : `${nearlyFull} clouds are`} at least{' '}
                {Math.round(NEARLY_FULL * 100)}% full. Uploads that need {nearlyFull === 1 ? 'it' : 'them'}{' '}
                will run out of room there first.
              </p>
            )}
          </Section>
        </>
      )}
    </Modal>
  )
}

/* A donut, laid out by hand like every other chart in the app.

   Slices are separated by the surface showing through — a stroke in the
   surface colour — rather than by a border drawn round each, and a slice too
   thin to see keeps a sliver of a degree so it is never silently gone. The
   centre carries the one figure the pie is about. Every slice is named, with
   its figure, in the list beside it; the pie is the picture of that list. */
export function Donut({ slices, total, centre, caption, size = 148 }) {
  const stroke = 22
  const r = (size - stroke) / 2
  const c = size / 2
  const whole = total > 0 ? total : slices.reduce((sum, s) => sum + s.bytes, 0)
  const MIN_ANGLE = 0.012 // radians: about two-thirds of a degree

  let at = -Math.PI / 2
  const arcs = slices.map((slice) => {
    const angle = Math.max(MIN_ANGLE, (slice.bytes / whole) * Math.PI * 2)
    const from = at
    at += angle
    return { ...slice, from, to: Math.min(at, from + Math.PI * 2) }
  })

  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${size} ${size}`}
      role="img"
      aria-label={`${centre} ${caption}`}
      style={{ flexShrink: 0 }}
    >
      <circle cx={c} cy={c} r={r} fill="none" stroke={COLORS.surfaceRaised} strokeWidth={stroke} />
      {arcs.map((arc) => (
        <path
          key={arc.key}
          d={arcPath(c, r, arc.from, arc.to)}
          fill="none"
          stroke={arc.color}
          strokeWidth={stroke}
        >
          <title>{`${arc.label}: ${formatBytes(arc.bytes)} (${percent(arc.bytes, whole)})`}</title>
        </path>
      ))}
      {/* The gaps: a hairline of surface at every boundary between slices. */}
      {arcs.length > 1 && arcs.map((arc) => {
        const [x1, y1] = polar(c, r - stroke / 2 - 1, arc.from)
        const [x2, y2] = polar(c, r + stroke / 2 + 1, arc.from)
        return <line key={`gap-${arc.key}`} x1={x1} y1={y1} x2={x2} y2={y2} stroke={COLORS.surface} strokeWidth="2" />
      })}
      <text x={c} y={c - 2} textAnchor="middle" fontFamily={FONT.mono} fontSize="20" fontWeight="600" fill={COLORS.text}>
        {centre}
      </text>
      <text x={c} y={c + 15} textAnchor="middle" fontFamily={FONT.mono} fontSize="9" letterSpacing="1" fill={COLORS.textMuted}>
        {caption.toUpperCase()}
      </text>
    </svg>
  )
}

function polar(c, r, angle) {
  return [c + r * Math.cos(angle), c + r * Math.sin(angle)]
}

/* An arc along the ring from one angle to another. A full turn cannot be one
   arc — its two ends are the same point — so it is drawn as two halves. */
export function arcPath(c, r, from, to) {
  const sweep = to - from
  if (sweep >= Math.PI * 2 - 1e-6) {
    const [x1, y1] = polar(c, r, from)
    const [x2, y2] = polar(c, r, from + Math.PI)
    return `M ${x1} ${y1} A ${r} ${r} 0 1 1 ${x2} ${y2} A ${r} ${r} 0 1 1 ${x1} ${y1}`
  }
  const [x1, y1] = polar(c, r, from)
  const [x2, y2] = polar(c, r, to)
  const large = sweep > Math.PI ? 1 : 0
  return `M ${x1} ${y1} A ${r} ${r} 0 ${large} 1 ${x2} ${y2}`
}

/* SAND's parts as one part-to-whole bar across every account. */
function PartsShare({ rows, stored }) {
  const holders = rows.filter((r) => (r.provider.stored || 0) > 0)
    .sort((a, b) => b.provider.stored - a.provider.stored)
  if (stored <= 0 || holders.length === 0) {
    return <p style={{ ...noteStyle, margin: 0 }}>No parts are stored yet.</p>
  }
  return (
    <div>
      <div style={{
        display: 'flex', height: '12px', borderRadius: '6px', background: COLORS.surfaceRaised, overflow: 'hidden',
      }}>
        {holders.map(({ provider }, i) => (
          <span
            key={provider.id}
            title={`${provider.name}: ${formatBytes(provider.stored)} of parts`}
            style={{
              width: `${(provider.stored / stored) * 100}%`,
              minWidth: '2px',
              flexShrink: 0,
              background: accountColor(provider.id),
              marginLeft: i > 0 ? '2px' : 0,
            }}
          />
        ))}
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: '6px', marginTop: '10px' }}>
        {holders.map(({ provider }) => (
          <Key
            key={provider.id}
            color={accountColor(provider.id)}
            label={provider.name || provider.id}
            value={`${formatBytes(provider.stored)}${provider.shards ? ` · ${provider.shards.toLocaleString()} parts` : ''}`}
            share={percent(provider.stored, stored)}
          />
        ))}
      </div>
    </div>
  )
}

/* One account's space: name, how full, the same bar its card draws, and the
   figures under it. */
function CloudRow({ provider, space }) {
  const full = fullness(space)
  const filling = full >= NEARLY_FULL

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: '8px', marginBottom: '5px' }}>
        <span aria-hidden="true" style={{
          width: '8px', height: '8px', flexShrink: 0, borderRadius: '2px',
          background: accountColor(provider.id), alignSelf: 'center',
        }} />
        <span style={{
          fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text,
          minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{provider.name || provider.id}</span>
        <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {KIND_ICONS[provider.kind] || '☁'}
        </span>
        <span style={{
          marginLeft: 'auto', flexShrink: 0, fontFamily: FONT.mono, fontSize: '11px',
          color: filling ? COLORS.warn : COLORS.textDim,
        }}>
          {full >= 0 ? `${percent(space.total - space.free, space.total)} full` : 'size unknown'}
        </span>
      </div>

      {space.known ? (
        <UsageBar provider={provider} height={8} />
      ) : (
        <div style={{ height: '8px', borderRadius: '4px', background: COLORS.surfaceRaised }} />
      )}

      <div style={{
        marginTop: '5px', fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
        display: 'flex', flexWrap: 'wrap', gap: '4px 10px',
      }}>
        <span>{formatBytes(space.sand)} SAND</span>
        {space.other > 0 && <span>{formatBytes(space.other)} other</span>}
        {space.known && <span>{formatBytes(space.free)} free{space.quota ? ' under your quota' : ''}</span>}
        {space.known && <span>of {formatBytes(space.total)}{space.quota ? ' quota' : ''}</span>}
        {space.reserved > 0 && <span>{formatBytes(space.reserved)} out of reach</span>}
        {space.over > 0 && <span style={{ color: COLORS.warn }}>{formatBytes(space.over)} over quota</span>}
        {!space.known && <span>the account does not report its size</span>}
      </div>
    </div>
  )
}
