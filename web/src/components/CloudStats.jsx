import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'
import { SPACE_QUOTA, spaceOf, spaceTitle } from '../space'
import { useIsMobile } from '../hooks'

/* One account, taken apart.

   The card in the drawer says what an account is holding for SAND. That figure
   on its own is unreadable: 33.9 GB of parts means one thing on a 15 GB Drive
   and another on a 5 TB disk with a film library already on it. So everything
   here is drawn against the room the account actually has — what SAND put
   there, what else is on it, and what is left — and then broken down into what
   the parts belong to.

   The charts are laid out by hand, like everything else in this app: a bar is
   a box as wide as its share and a column is a box as tall as one. Nothing is
   fetched here, so there is no charting library to fetch. */

/* What an account's space is made of.

   `stored` is the vault's own accounting — the parts it wrote and their sizes.
   `usage` is what the account says about itself. SAND's share is the smaller of
   the two, because an account cannot be holding more of ours than it holds in
   total, and everything else that is used is somebody else's — the photos
   already in the Drive, the other things on the disk.

   A drive keeps a reserve only root may spend, and a quota can cut a share of
   a much larger disk down to size, so what is used and what can be written can
   fall well short of the whole between them. That difference is named — it is
   the fourth figure, `reserved` — rather than being folded into free space
   somebody cannot actually use.

   Where the account says nothing at all, the quota set on it answers instead —
   see `space.js`. That bar is a different picture and says so: it is not the
   account, it is SAND's own corner of it measured against the line somebody
   drew, and everything else in there is outside the frame. It is the only bar
   an account that reports nothing and cannot be counted will ever have. */
export function usageBreakdown(provider) {
  const usage = provider?.usage || {}
  const total = usage.total > 0 ? usage.total : 0
  const stored = Math.max(0, provider?.stored || 0)
  const quota = Math.max(0, provider?.quota || 0)
  // How far past the line the account is, whichever picture the bar is drawing.
  // A drive with room to spare can still be over a quota, and that is the half
  // of the account's state a usage bar cannot show.
  const over = quota > 0 ? Math.max(0, stored - quota) : 0
  // Where the two figures came from, which the panel says out loud. A bucket
  // has no quota call, so what is in it was counted by listing it and the
  // capacity it is drawn against was typed by whoever pays for the bucket —
  // both are true and neither is the account talking.
  const counted = Boolean(usage.measured)
  const declared = Boolean(usage.declared)
  const countedAt = usage.measured_at || ''

  if (!total) {
    // Nothing to measure the account against — but if somebody has said how
    // much of it SAND may fill, there is a line to measure *our* share against,
    // and that is a bar. What else is in there is not part of that picture:
    // a quota is about how much of somebody else's storage we are taking, not
    // about how full it is.
    if (quota > 0) {
      return {
        known: true,
        quota: true,
        counted,
        declared,
        countedAt,
        total: quota,
        used: Math.min(stored, quota),
        sand: Math.min(stored, quota),
        other: 0,
        free: Math.max(0, quota - stored),
        reserved: 0,
        over,
      }
    }

    // Counted but with nothing to measure it against: how full the account is
    // has no answer, and how much is on it does. Saying the second is the whole
    // point of having counted.
    const measured = counted ? Math.max(0, usage.used || 0) : 0
    return {
      known: false,
      quota: false,
      counted,
      declared,
      countedAt,
      total: 0,
      used: measured,
      sand: counted ? Math.min(stored, measured) : stored,
      other: counted ? Math.max(0, measured - stored) : 0,
      free: 0,
      reserved: 0,
      over,
    }
  }

  const used = Math.min(Math.max(0, usage.used || 0), total)
  const sand = Math.min(stored, used)
  const free = usage.free > 0 ? Math.min(usage.free, total - used) : Math.max(0, total - used)
  return {
    known: true,
    quota: false,
    counted,
    declared,
    countedAt,
    total,
    used,
    sand,
    other: Math.max(0, used - sand),
    free,
    reserved: Math.max(0, total - used - free),
    over,
  }
}

/* A percentage said the way a person would: never "0%" for something that is
   there, never a decimal point for something that fills the bar. */
function percent(part, whole) {
  if (!whole || part <= 0) return '0%'
  const share = (part / whole) * 100
  if (share >= 99.5) return '100%'
  if (share < 0.1) return '<0.1%'
  if (share < 10) return `${share.toFixed(1)}%`
  return `${Math.round(share)}%`
}

/* The account's space as one bar: SAND's parts, everything else on it, the
   room left, and — where a filesystem reserve or a quota puts part of the disk
   out of reach — the bit that is neither.

   Four regions and only one of them coloured. What SAND holds wears the
   account's own colour, the same colour its card and its part badges wear, so
   the sliver here and the badges in the file list are recognisably the same
   thing; everything else on the account is a neutral, because it is not ours
   and is not the subject; and free space is dimmer still, since a bar is read
   for what is filling it. Where two regions touch, a 2px gap in the track
   separates them rather than a border drawn round either. */
export function UsageBar({ provider, height = 6, gap = 2 }) {
  const space = usageBreakdown(provider)
  if (!space.known) return null

  const segments = [
    { key: 'sand', bytes: space.sand, color: accountColor(provider.id) },
    { key: 'other', bytes: space.other, color: COLORS.textMuted },
    { key: 'free', bytes: space.free, color: COLORS.borderBright },
  ].filter((segment) => segment.bytes > 0)

  return (
    <div
      style={{
        display: 'flex',
        height: `${height}px`,
        borderRadius: `${height / 2}px`,
        // Whatever is left over is the track, which is what a disk nobody can
        // write to looks like.
        background: COLORS.surfaceRaised,
        overflow: 'hidden',
      }}
    >
      {segments.map((segment, i) => (
        <span
          key={segment.key}
          style={{
            width: `${Math.min(100, (segment.bytes / space.total) * 100)}%`,
            // A share that rounds to less than a pixel is still a share that is
            // there: it keeps a hairline rather than disappearing. Corners are
            // left square — the bar's own rounding shapes the two ends, where a
            // radius on a two-pixel sliver would turn it into a dot.
            minWidth: '2px',
            flexShrink: 0,
            background: segment.color,
            marginLeft: i > 0 ? `${gap}px` : 0,
          }}
        />
      ))}
    </div>
  )
}

/* The line under the bar on an account card: how full the account is, and how
   much room is left on it.

   Being over a quota gets a line of its own, in the warning shade. It is the
   one thing here that is not a neutral fact about an account — a drive fills up
   on its own, but a quota is crossed, and the card should say so where the eye
   lands rather than leave it to be worked out from two figures. */
export function UsageLine({ provider }) {
  const { known, counted, quota, total, used, free, over } = usageBreakdown(provider)
  const crossed = over > 0 && (
    <div style={{ color: COLORS.warn }}>{formatBytes(over)} over the quota you set</div>
  )

  // A drive with room to spare can still be nearly out of the share of it SAND
  // was given, and then the drive's own free figure is the wrong answer to
  // "how much more fits here". Said as its own line rather than folded into the
  // one above, which is about the account and not about our corner of it.
  const bound = spaceOf(provider)
  const capped = !quota && over === 0 && bound.source === SPACE_QUOTA && (
    <div>{formatBytes(bound.free)} left under your quota</div>
  )

  if (!known) {
    // Counted, with no capacity to measure it against. It is still worth a
    // line: the account card's other figure is what SAND put there, and this
    // is what is there — the gap between the two is somebody else's files.
    if (!counted) return crossed || capped || null
    return <>{crossed}<div>{formatBytes(used)} on the account</div>{capped}</>
  }

  // What SAND's own share of that is stays on the line above, which already
  // says how many parts are here and what they weigh. Two figures fit the
  // drawer's width; a third wraps it onto a third line to say what the two
  // together already said.
  //
  // Against a quota it is the other way round: the figure above *is* the used
  // half, so the line names what it is being measured against rather than
  // repeating it.
  return (
    <>
      {crossed}
      <div>
        {quota
          ? `${formatBytes(used)} of your ${formatBytes(total)} quota`
          : `${formatBytes(used)} / ${formatBytes(total)} used`}
        {free > 0 ? ` · ${formatBytes(free)} free` : ''}
      </div>
      {capped}
    </>
  )
}

/* A row of a breakdown: a label, a bar as long as its share, and the figure.

   One hue for every row, because these rows are a magnitude comparison and not
   a set of identities — the reader is asking which is biggest, and colouring
   each row differently would answer a question nobody asked while making the
   two smallest indistinguishable under a colour-vision deficiency. */
function BarRows({ rows, color, empty }) {
  const most = rows.reduce((max, row) => Math.max(max, row.bytes), 0)
  const total = rows.reduce((sum, row) => sum + row.bytes, 0)

  if (!rows.length) {
    return <p style={{ ...noteStyle, margin: 0 }}>{empty}</p>
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '9px' }}>
      {rows.map((row) => (
        <div key={row.label}>
          <div style={{
            display: 'flex',
            justifyContent: 'space-between',
            gap: '10px',
            fontFamily: FONT.mono,
            fontSize: '11px',
            color: COLORS.textDim,
            marginBottom: '4px',
          }}>
            <span style={{
              minWidth: 0,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}>{row.label}</span>
            <span style={{ flexShrink: 0, color: COLORS.textMuted }}>
              {formatBytes(row.bytes)} · {percent(row.bytes, total)}
            </span>
          </div>
          <div style={{ height: '6px', borderRadius: '3px', background: COLORS.surfaceRaised }}>
            <div
              title={`${row.label}: ${row.parts} part${row.parts === 1 ? '' : 's'}, ${formatBytes(row.bytes)}`}
              style={{
                width: `${most > 0 ? Math.max(2, (row.bytes / most) * 100) : 0}%`,
                height: '100%',
                borderRadius: '3px',
                background: color,
              }}
            />
          </div>
        </div>
      ))}
    </div>
  )
}

/* When the parts arrived. Columns rather than a line: each month is a discrete
   amount that landed, not a reading taken along a curve, and a month nothing
   arrived in is a gap you can see rather than a segment sloping through it.

   Only the biggest column is labelled. A number over every column is the thing
   that makes a small chart unreadable, and the rest are a hover away. */
function MonthColumns({ months, color, mobile }) {
  const most = months.reduce((max, m) => Math.max(max, m.bytes), 0)
  const peak = months.reduce((at, m, i) => (m.bytes > months[at].bytes ? i : at), 0)
  // Twelve labels do not fit on a phone, so every other one is dropped — the
  // last is always kept, since "up to when" is the half of the axis that is
  // read first.
  const step = mobile && months.length > 6 ? 2 : 1

  return (
    <div>
      <div style={{
        display: 'flex',
        alignItems: 'flex-end',
        gap: '4px',
        height: '96px',
        borderBottom: `1px solid ${COLORS.border}`,
        paddingBottom: '1px',
      }}>
        {months.map((month, i) => (
          <div key={month.month} style={{
            // Capped, so a vault one month old draws a column at the start of
            // the axis rather than one marooned in the middle of an empty one.
            flex: '1 1 0',
            maxWidth: '44px',
            display: 'flex',
            flexDirection: 'column',
            justifyContent: 'flex-end',
            alignItems: 'center',
            height: '100%',
            gap: '3px',
          }}>
            {i === peak && month.bytes > 0 && (
              <span style={{
                fontFamily: FONT.mono,
                fontSize: '9px',
                color: COLORS.textMuted,
                whiteSpace: 'nowrap',
              }}>{formatBytes(month.bytes)}</span>
            )}
            <div
              title={`${monthName(month.month, true)}: ${month.parts} part${month.parts === 1 ? '' : 's'}, ${formatBytes(month.bytes)}`}
              style={{
                width: '100%',
                maxWidth: '24px',
                // A month with nothing in it still gets a hairline, so the gap
                // reads as a quiet month rather than as a missing column.
                height: most > 0 ? `${Math.max(month.bytes > 0 ? 3 : 1, (month.bytes / most) * 100)}%` : '1px',
                background: month.bytes > 0 ? color : COLORS.border,
                borderRadius: '3px 3px 0 0',
              }}
            />
          </div>
        ))}
      </div>
      <div style={{ display: 'flex', gap: '4px', marginTop: '5px' }}>
        {months.map((month, i) => (
          <span key={month.month} style={{
            flex: '1 1 0',
            maxWidth: '44px',
            textAlign: 'center',
            fontFamily: FONT.mono,
            fontSize: '8.5px',
            letterSpacing: '0.3px',
            color: COLORS.textMuted,
            overflow: 'hidden',
            whiteSpace: 'nowrap',
          }}>
            {(months.length - 1 - i) % step === 0 ? monthName(month.month) : ''}
          </span>
        ))}
      </div>
    </div>
  )
}

/* "2026-08" as a person writes it. The year comes along only where it is
   needed — in a tooltip, and on a January, which is where a run of months
   crosses into a new one. */
function monthName(key, full = false) {
  const [year, month] = key.split('-')
  const at = new Date(Number(year), Number(month) - 1, 1)
  if (Number.isNaN(at.getTime())) return key
  const name = at.toLocaleDateString(undefined, { month: 'short' })
  if (full) return `${name} ${year}`
  return month === '01' ? `${name} ${year.slice(2)}` : name
}

/* What a bucket that keeps every version is storing beneath the objects it
   shows, and the offer to erase it.

   Backblaze B2 does this out of the box: a write goes beneath the old version
   rather than over it, a delete leaves a marker on top, and every version is
   billed. SAND rewrites the index backup on every change and never reads an
   old one back, and every part it has deleted is still down there. None of it
   shows in the count above — a listing sees only the current version of each
   key — which is how the bar can say one thing and the provider's cap warning
   another.

   Asked when pressed rather than on the way in: the answer is a listing of
   every version on every account, and a panel opened to read a bar has no
   business starting one. The scan is vault-wide and this shows its row for
   the account in hand; erasing is aimed at this account alone. */
function StaleVersions({ provider, onChanged }) {
  const [account, setAccount] = useState(null)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState('')
  const [error, setError] = useState(null)
  const [erased, setErased] = useState(null)

  const look = useCallback(async () => {
    setBusy('looking')
    setError(null)
    try {
      const scan = await api.versionScan()
      const row = (scan.accounts || []).find((a) => a.provider_id === provider.id) || null
      setAccount(row)
      // Why nothing on this account may go, when nothing may: the reason is
      // on the rows, and one is enough to say.
      const held = (scan.items || []).find((item) => item.provider_id === provider.id && !item.deletable)
      setReason(row && row.deletable === 0 && held ? held.reason : '')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }, [provider.id])

  const erase = useCallback(async () => {
    setBusy('erasing')
    setError(null)
    try {
      const report = await api.sweepVersions({ accounts: [provider.id] })
      setErased(report)
      if (report.warnings?.length) setError(report.warnings.join(' '))
      onChanged?.()
      await look()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }, [provider.id, onChanged, look])

  let line
  if (!account) {
    line = 'A bucket that keeps every version stores what it shows and everything beneath it: '
      + 'every rewrite of the index backup, every part ever deleted. None of that is in the count above.'
  } else if (account.error) {
    line = `Could not list the versions here: ${account.error}`
  } else if (!account.versioned) {
    line = 'This account keeps no old versions.'
  } else if (account.stale === 0) {
    line = `Storing only the current version of each of its ${account.current} object${account.current === 1 ? '' : 's'}.`
  } else {
    line = `${account.stale} old version${account.stale === 1 ? '' : 's'} (${formatBytes(account.stale_bytes)}) `
      + `beneath ${account.current} current object${account.current === 1 ? '' : 's'} (${formatBytes(account.current_bytes)})`
      + (account.markers > 0 ? `, ${account.markers} of them delete marker${account.markers === 1 ? '' : 's'}` : '')
      + (account.other > 0 ? `, and ${formatBytes(account.other_bytes)} of history under files that are not SAND's, left alone` : '')
      + '.'
  }

  const offer = account && account.deletable > 0

  // What the schedule has done, when there is one: said beside the figures
  // so that a bucket somebody set to tidy itself reads as looked after rather
  // than as never looked at.
  let scheduled = ''
  if (provider.auto_prune) {
    const last = account?.last_prune
    scheduled = !last
      ? 'Erased daily; the first run is due shortly after the vault was unlocked.'
      : last.error
        ? `Erased daily; the last run, ${formatDate(last.at)}, failed: ${last.error}`
        : `Erased daily; the last run, ${formatDate(last.at)}, freed ${formatBytes(last.bytes)}.`
  }

  return (
    <Section
      title="Old versions"
      hint={'Only SAND\'s own objects are looked at, and the current version of every one of them stays.'
        + (provider.auto_prune ? '' : ' Edit account can make this happen daily.')}
    >
      {scheduled && <p style={{ ...noteStyle, margin: '0 0 8px', color: COLORS.textDim }}>{scheduled}</p>}
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {erased && !error && (
        <Banner tone="info" onDismiss={() => setErased(null)}>
          Erased {erased.deleted} old version{erased.deleted === 1 ? '' : 's'}, freeing {formatBytes(erased.bytes)}.
          The bucket keeps doing this: set its lifecycle to keep only the latest version to stop the next pile.
        </Banner>
      )}
      <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '10px' }}>
        <span style={{ ...noteStyle, flex: 1, minWidth: '160px' }}>
          {busy === 'looking' ? 'Listing every version on every account…'
            : busy === 'erasing' ? 'Erasing…'
              : line}
          {reason && !busy && ` Not erasing any of it: ${reason}`}
        </span>
        {offer ? (
          <Button size="sm" variant="danger" onClick={erase} disabled={Boolean(busy)}>
            {busy === 'erasing' ? <Spinner size={11} /> : `Erase ${formatBytes(account.deletable_bytes)}`}
          </Button>
        ) : (
          <Button size="sm" onClick={look} disabled={Boolean(busy)}>
            {busy === 'looking' ? <Spinner size={11} /> : account ? 'Look again' : 'Look'}
          </Button>
        )}
      </div>
    </Section>
  )
}

const noteStyle = {
  fontFamily: FONT.sans,
  fontSize: '11.5px',
  color: COLORS.textMuted,
  lineHeight: 1.6,
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
      {hint && <p style={{ ...noteStyle, margin: '0 0 10px' }}>{hint}</p>}
      {!hint && <div style={{ height: '10px' }} />}
      {children}
    </section>
  )
}

/* One number and what it counts, for the row across the top. */
function Stat({ value, label, tone, title }) {
  return (
    <div title={title} style={{ minWidth: '72px' }}>
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

/* A swatch and a name, for the capacity bar's legend. Identity never rests on
   the colour alone: every segment is named here with its figure beside it, so
   the bar is a picture of something the panel has already said in words. */
function Key({ color, label, title, bytes, share, outline }) {
  return (
    <div title={title} style={{ display: 'flex', alignItems: 'baseline', gap: '7px' }}>
      <span style={{
        width: '9px',
        height: '9px',
        flexShrink: 0,
        borderRadius: '2px',
        background: color,
        border: outline ? `1px solid ${COLORS.borderBright}` : 'none',
      }} />
      <span style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim }}>
        {label}
      </span>
      <span style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.text, marginLeft: 'auto' }}>
        {formatBytes(bytes)}
      </span>
      <span style={{
        fontFamily: FONT.mono,
        fontSize: '10px',
        color: COLORS.textMuted,
        minWidth: '38px',
        textAlign: 'right',
      }}>{share}</span>
    </div>
  )
}

/* What the Capacity section is looking at, in one sentence.

   Four different situations end up here and they are not the same claim. An
   account with a quota is quoting itself. A bucket with a declared capacity is
   quoting you, against a figure something counted. A bucket that has only been
   counted has no denominator at all. And a backend that can do neither is where
   this panel has always shrugged. */
function capacityHint(space, measurable) {
  if (space.quota) {
    return 'Measured against the quota you set for this account rather than against the '
      + 'account, which reports nothing to measure against. This is SAND\'s own corner of it: '
      + 'whatever else is in there is outside the frame, and crossing the line warns rather '
      + 'than refuses.'
  }
  if (space.known && space.declared) {
    return 'Measured against the capacity you set for this account, since a bucket reports none of its own — so the whole is your figure rather than the service\'s, and what is in it was counted by listing it.'
  }
  if (space.known) {
    return 'Not all of an account is SAND. Its parts are the coloured slice, the neutral one is whatever else already lives there, and what is left after both is room.'
  }
  if (space.counted) {
    return 'What is in the bucket, counted by listing it. How full that makes it has no answer until somebody says how big the bucket is — Edit the account to set a capacity, and this becomes a bar.'
  }
  if (measurable) {
    return 'A bucket reports no quota — S3 has never had a call for it — so what is in one has to be counted by listing it.'
  }
  return 'This backend reports no quota, so there is nothing to measure the parts against — a bucket '
    + 'or a share is as big as whoever runs it says. Edit the account to set a quota of your own, '
    + 'and what SAND has put here becomes a fraction rather than a bare figure.'
}

export default function CloudStats({ provider, onClose, onChanged }) {
  const [stats, setStats] = useState(null)
  const [error, setError] = useState(null)
  // A measurement taken from this panel, which outranks whatever the stats
  // call came back with — that one is as fresh as the last count, this one is
  // the count.
  const [counted, setCounted] = useState(null)
  const [counting, setCounting] = useState(false)
  const [countError, setCountError] = useState(null)
  const mobile = useIsMobile()

  useEffect(() => {
    let live = true
    setError(null)
    api.providerStats(provider.id)
      .then((resp) => { if (live) setStats(resp.stats) })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [provider.id])

  /* Counting what is in a bucket, for the backends that answer no other way.

     It costs a listing — a request per thousand objects, billed at some
     providers — so it is taken once and kept: the first time this panel is
     opened for an account nobody has counted, and by the button after that.
     Repeat visits read back what the last count found rather than paying for
     the same answer twice.

     The drawer behind is told, because the server keeps the figure too and the
     account's card can draw a bar from it the moment there is a capacity to
     draw it against. */
  const measure = useCallback(async () => {
    setCounting(true)
    setCountError(null)
    try {
      const resp = await api.measureProvider(provider.id)
      setCounted(resp.usage)
      onChanged?.()
    } catch (err) {
      setCountError(err.message)
    } finally {
      setCounting(false)
    }
  }, [provider.id, onChanged])

  useEffect(() => {
    // Not at an account that is not answering: the ping behind the card
    // already failed, and a listing would fail the same way with a longer
    // wait and a second error to dismiss.
    if (!provider.measurable || provider.usage?.measured || provider.online === false) return
    measure()
    // Once per account, on the way in. `measure` is stable for an account and
    // the guard above is what stops a second run: a count that has happened is
    // a count this panel does not repeat.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [provider.id])

  const color = accountColor(provider.id)
  // The panel re-pings the account as it opens, so what it answers with is
  // fresher than the card that was clicked; until it does, the card's own
  // figures stand in and nothing jumps.
  const account = counted
    ? { ...(stats || provider), usage: counted }
    : (stats || provider)
  const space = usageBreakdown(account)
  const room = spaceOf(account)

  return (
    <Modal
      title={provider.name}
      subtitle={`${provider.kind} · ${account.shards || 0} part${account.shards === 1 ? '' : 's'} · ${formatBytes(account.stored || 0)} stored by SAND`}
      onClose={onClose}
      width={620}
      zIndex={120}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <div style={{
        display: 'flex',
        flexWrap: 'wrap',
        gap: mobile ? '16px 22px' : '18px 28px',
        paddingBottom: '4px',
      }}>
        <Stat value={`${KIND_ICONS[provider.kind] || '☁'} ${account.online ? 'online' : 'offline'}`}
          label="status" tone={account.online ? COLORS.success : COLORS.error} />
        <Stat value={stats ? stats.files : '—'} label="files here" />
        <Stat
          value={stats && stats.vault_stored > 0 ? percent(stats.stored, stats.vault_stored) : '—'}
          label="of the vault"
        />
        {/* The binding figure rather than the bar's: an account can be a long
            way from full and still have nothing left of the quota set on it,
            and this is the number that decides whether the next file fits. */}
        <Stat
          value={room.known ? formatBytes(room.free) : '—'}
          label="room left"
          title={spaceTitle(account)}
        />
      </div>

      {!stats && !error && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '20px' }}>
          <Spinner size={12} />
          <span style={noteStyle}>Asking the account where it stands…</span>
        </div>
      )}

      {account.error && !account.online && (
        <Banner tone="error">{account.error}</Banner>
      )}

      <Section
        title="Capacity"
        hint={capacityHint(space, provider.measurable)}
      >
        {/* A line crossed rather than a drive filled, so it is said in words
            before the bar rather than left to be read off it — and it is said
            wherever it is true, including on an account whose own figures are
            perfectly healthy. */}
        {space.over > 0 && (
          <Banner tone="warn">
            {formatBytes(account.stored || 0)} of parts are on this account, against the{' '}
            {formatBytes(account.quota || 0)} quota you set for it — {formatBytes(space.over)} past
            it. Nothing was refused: uploads past a quota store and warn. Raise the line from
            Edit account, or move files to another cloud.
          </Banner>
        )}

        {space.known ? (
          <>
            <UsageBar provider={account} height={14} />
            <div style={{ display: 'flex', flexDirection: 'column', gap: '7px', marginTop: '12px' }}>
              <Key color={color} label="SAND's parts" bytes={space.sand} share={percent(space.sand, space.total)} />
              {!space.quota && (
                <Key color={COLORS.textMuted} label="everything else on it" bytes={space.other} share={percent(space.other, space.total)} />
              )}
              <Key color={COLORS.borderBright} label={space.quota ? 'left under your quota' : 'free'}
                bytes={space.free} share={percent(space.free, space.total)} />
              {space.reserved > space.total * 0.01 && (
                <Key color={COLORS.surfaceRaised} outline label="reserved"
                  title="A filesystem reserve, or a quota that cuts this account down from the disk it sits on — space the account has but cannot write to."
                  bytes={space.reserved} share={percent(space.reserved, space.total)} />
              )}
            </div>
          </>
        ) : space.counted ? (
          /* Counted, with nothing to measure it against. The bar wants a
             denominator and there is none, so the two figures are said instead:
             what is in the bucket, and how much of that is ours. */
          <div style={{ display: 'flex', flexDirection: 'column', gap: '7px' }}>
            <Key color={color} label="SAND's parts" bytes={space.sand}
              share={percent(space.sand, space.used)} />
            <Key color={COLORS.textMuted} label="everything else on it" bytes={space.other}
              share={percent(space.other, space.used)} />
          </div>
        ) : (
          <p style={{ ...noteStyle, margin: 0, color: COLORS.textDim }}>
            {formatBytes(account.stored || 0)} of parts across {account.shards || 0} object
            {account.shards === 1 ? '' : 's'}.
          </p>
        )}

        {/* Where the figures came from, and the offer to take them again. Only
            the backends that can be counted get either — for a Drive this
            section is the account's own answer and there is nothing to press. */}
        {provider.measurable && (
          <div style={{
            display: 'flex',
            alignItems: 'center',
            flexWrap: 'wrap',
            gap: '10px',
            marginTop: '14px',
          }}>
            <span style={{ ...noteStyle, flex: 1, minWidth: '160px' }}>
              {counting
                ? 'Counting what is in the bucket…'
                : space.counted
                  ? `Counted ${formatDate(space.countedAt)}${space.declared ? ', against the capacity you set for this account' : ''}.`
                  : 'Nothing has counted this bucket yet.'}
            </span>
            <Button size="sm" onClick={measure} disabled={counting}>
              {counting ? <Spinner size={11} /> : space.counted ? 'Count again' : 'Count it'}
            </Button>
          </div>
        )}
        {countError && <Banner tone="error" onDismiss={() => setCountError(null)}>{countError}</Banner>}
      </Section>

      {/* Only the backends that can be counted can be asked for versions —
          today both are the S3 face — and a folder on a disk has nothing
          beneath its files to ask about. */}
      {provider.measurable && (
        <StaleVersions provider={provider} onChanged={onChanged} />
      )}

      {stats && (
        <>
          <Section
            title="What SAND keeps here"
            hint="By what the files are, weighed by what their parts weigh on this account rather than by the files' own size."
          >
            <BarRows rows={stats.kinds || []} color={color}
              empty="Nothing of the vault has landed on this account yet." />
          </Section>

          {stats.folders?.length > 0 && (
            <Section title="Where it comes from" hint="The folders leaning hardest on this account.">
              <BarRows rows={stats.folders} color={color} empty="" />
            </Section>
          )}

          {stats.months?.length > 0 && (
            <Section title="When it arrived" hint="Parts landing here, by the month the file was added.">
              <MonthColumns months={stats.months} color={color} mobile={mobile} />
            </Section>
          )}

          {stats.largest?.length > 0 && (
            <Section title="Heaviest files" hint="Measured by what each one left here.">
              <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
                {stats.largest.map((file) => (
                  <div key={file.path} style={{
                    display: 'flex',
                    alignItems: 'baseline',
                    gap: '10px',
                    fontFamily: FONT.mono,
                    fontSize: '11px',
                  }}>
                    <span style={{
                      flex: 1,
                      minWidth: 0,
                      color: COLORS.textDim,
                      overflow: 'hidden',
                      textOverflow: 'ellipsis',
                      whiteSpace: 'nowrap',
                    }} title={file.path}>{file.path}</span>
                    <span style={{ flexShrink: 0, color: COLORS.text }}>{formatBytes(file.bytes)}</span>
                    <span style={{ flexShrink: 0, color: COLORS.textMuted, minWidth: '52px', textAlign: 'right' }}>
                      of {formatBytes(file.size)}
                    </span>
                  </div>
                ))}
              </div>
            </Section>
          )}

          {stats.sub_vaults?.parts > 0 && (
            <Section
              title="Sub vaults"
              hint="Counted in the figures above and left out of the breakdowns: what is inside a vault within the vault is its own password's business, not this panel's."
            >
              <p style={{ ...noteStyle, margin: 0, color: COLORS.textDim }}>
                {stats.sub_vaults.label} put {stats.sub_vaults.parts} part
                {stats.sub_vaults.parts === 1 ? '' : 's'} here, weighing {formatBytes(stats.sub_vaults.bytes)}.
              </p>
            </Section>
          )}

          {stats.sole > 0 && (
            <Banner tone="warn">
              {stats.sole} file{stats.sole === 1 ? '' : 's'} could not be rebuilt without this
              account — no other connected account holds enough of {stats.sole === 1 ? 'it' : 'them'}.
              Spreading {stats.sole === 1 ? 'it' : 'them'} wider is what a move to other clouds is for.
            </Banner>
          )}
        </>
      )}
    </Modal>
  )
}
