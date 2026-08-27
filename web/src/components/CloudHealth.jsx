import React, { useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Are the clouds still there?

   Every other ping in this app is one somebody started. The sidebar pings when
   it is drawn, Test pings one account, a folder's sweep pings them all before
   it checks anything under it — and each of those needs a person sitting in
   front of the app. The failure this is for is the one nobody is sitting in
   front of: a refresh token revoked in March, keys rotated by somebody else on
   the team, a NAS that has been off since the power cut. Nothing looks broken
   while it happens. Files still read, because a file only needs k of its n
   parts and the clouds still answering carry it — until a second one goes and
   the file does not come back at all.

   So the server asks them on a schedule, and this is where the answer is read:
   one line in the accounts drawer that says how many are not answering, and
   this panel behind it saying which, since when, and why.

   Two things it deliberately is not.

   It is not a check of what the clouds are *holding*. This is a ping — "answer
   me" and how long that took — because a check that walked a bucket would cost
   real money at the providers that bill for listing, every hour, forever.
   Whether the parts of a particular file are where the index says they went is
   a folder's automation policy, which reads the index and asks after them by
   name. Different job, different cost, and opt-in per folder for that reason.

   And it is not history. What a ping found is true for about as long as it
   takes to read, so it lives in the server's memory and goes when the vault
   locks. The one thing carried forward is how long an account has been
   failing, because "unreachable since Tuesday" and "unreachable just now" are
   not the same news. */

/* The intervals worth offering, and nothing in between.

   Not a number somebody types. The useful range is an hour either way and the
   figure means less the more precise it gets — 47 minutes is not a considered
   answer to anything — so this is the shape of the question: a few times a day,
   hourly, or a couple of times a week. */
const INTERVALS = [
  { minutes: 15, label: '15 min' },
  { minutes: 60, label: 'Hourly' },
  { minutes: 6 * 60, label: '6 hours' },
  { minutes: 24 * 60, label: 'Daily' },
]

/* How long ago, in the roughest form that is still true. Seconds matter for
   about a minute and never again, and a check that ran three days ago wants
   "3d" rather than a date somebody has to subtract from today. */
export function ago(iso) {
  if (!iso) return null
  const seconds = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000))
  if (seconds < 45) return 'just now'
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.round(minutes / 60)
  if (hours < 36) return `${hours}h ago`
  return `${Math.round(hours / 24)}d ago`
}

/* How long something has been true, for the outage that is still going on.
   "since 1m ago" is not English; "for 1m" is, and it is the same fact. */
export function lasting(iso) {
  if (!iso) return null
  const seconds = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000))
  if (seconds < 90) return null
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `for ${minutes}m`
  const hours = Math.round(minutes / 60)
  if (hours < 36) return `for ${hours}h`
  return `for ${Math.round(hours / 24)} days`
}

/* And the same in the other direction, for the check that has not happened
   yet. "in 43m" is a promise the panel can keep; a wall-clock time would be
   one the browser's own clock has to agree with. */
export function until(iso) {
  if (!iso) return null
  const seconds = Math.round((new Date(iso).getTime() - Date.now()) / 1000)
  if (seconds <= 60) return 'due now'
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `in ${minutes}m`
  const hours = Math.round(minutes / 60)
  if (hours < 36) return `in ${hours}h`
  return `in ${Math.round(hours / 24)}d`
}

/* The sentence the drawer shows, and the colour it is said in.

   It reports the worst true thing. An account that answered an hour ago and
   one that has never been asked are not the same claim, so a vault with
   something unchecked says so rather than calling itself healthy — the whole
   value of this line is that it can be believed. */
export function healthSummary(health) {
  if (!health || health.accounts === 0) return null

  // Short, because the space it lives in is the width of a drawer minus two
  // figures — about twenty characters. "1 of 17 unhealthy" says the whole thing
  // and stays on one line at seventeen accounts; the word it drops is the one
  // the drawer's own heading has been saying all along.
  if (health.unhealthy > 0) {
    return { tone: COLORS.error, line: `${health.unhealthy} of ${health.accounts} unhealthy` }
  }
  if (health.checked_at) {
    return {
      tone: COLORS.success,
      line: health.accounts === 1 ? '1 cloud healthy' : `${health.accounts} clouds healthy`,
      // An account connected since the last sweep is not a worry, but it is not
      // covered by the sentence either, so the line says so under its breath.
      note: health.unchecked > 0 ? `${health.unchecked} not checked yet` : null,
    }
  }
  return { tone: COLORS.textMuted, line: 'Not checked yet' }
}

/* The line itself, at the foot of the accounts drawer.

   A button, because there is always somewhere to go from it: which cloud, what
   it said, and how often this is asked. It reads as a status first and a
   control second — a dotted underline is the only mark of one — because most of
   the time nobody is going to press it, and it still has to be worth the space
   it takes. */
export function CloudHealthLine({ health, onClick }) {
  const [hover, setHover] = useState(false)
  const summary = healthSummary(health)
  if (!summary) return null

  const checked = ago(health.checked_at)

  return (
    <button
      type="button"
      onClick={onClick}
      onPointerEnter={(e) => { if (e.pointerType === 'mouse') setHover(true) }}
      onPointerLeave={() => setHover(false)}
      onFocus={() => setHover(true)}
      onBlur={() => setHover(false)}
      title="Which clouds answered, and how often SAND asks"
      style={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-end',
        gap: '3px',
        padding: 0,
        background: 'none',
        border: 'none',
        cursor: 'pointer',
        textAlign: 'right',
        filter: hover ? 'brightness(1.25)' : 'none',
        transition: 'filter 0.15s ease',
      }}
    >
      <span style={{
        display: 'flex',
        alignItems: 'center',
        gap: '5px',
        fontFamily: FONT.mono,
        fontSize: '10px',
        fontWeight: 600,
        // One line or none: a sentence this short broken across two, with a
        // dotted underline trailing off the end of the first, reads as a
        // mistake rather than as a status.
        whiteSpace: 'nowrap',
        color: summary.tone,
        textDecoration: 'underline',
        textDecorationStyle: 'dotted',
        textUnderlineOffset: '3px',
      }}>
        {/* The same dot each account card wears, meaning the same thing, for
            all of them at once. */}
        <span aria-hidden="true" style={{
          width: '7px',
          height: '7px',
          flexShrink: 0,
          borderRadius: '50%',
          background: summary.tone,
        }} />
        {summary.line}
      </span>

      <span style={{
        fontFamily: FONT.mono,
        fontSize: '8.5px',
        whiteSpace: 'nowrap',
        color: COLORS.textMuted,
      }}>
        {summary.note
          || (health.schedule && !health.schedule.enabled ? 'checks off'
            : checked ? `checked ${checked}` : 'checking hourly')}
      </span>
    </button>
  )
}

/* The panel behind that line: every cloud, worst first. */
export default function CloudHealth({ health, zIndex, onClose, onChanged }) {
  const [checking, setChecking] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState(null)

  const schedule = health?.schedule || { enabled: true, interval_minutes: 60 }
  const clouds = health?.clouds || []

  const check = async () => {
    setChecking(true)
    setError(null)
    try {
      const resp = await api.checkClouds()
      onChanged(resp.health)
    } catch (err) {
      setError(err.message)
    } finally {
      setChecking(false)
    }
  }

  const setSchedule = async (next) => {
    setSaving(true)
    setError(null)
    try {
      const resp = await api.setCloudHealthSchedule(next)
      if (resp.health) onChanged(resp.health)
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  const summary = healthSummary(health)

  return (
    <Modal
      title="Cloud health"
      subtitle="SAND asks every connected cloud whether it is still answering, on its own schedule, whether or not anybody is looking"
      width={520}
      zIndex={zIndex}
      onClose={onClose}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {/* Where it stands, and the two things to do about it: ask again now, and
          change how often it asks. */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: '12px',
        flexWrap: 'wrap',
        padding: '12px',
        marginBottom: '12px',
        background: COLORS.surfaceRaised,
        border: `1px solid ${COLORS.border}`,
        borderRadius: '8px',
      }}>
        <div style={{ flex: 1, minWidth: '180px' }}>
          <div style={{
            fontFamily: FONT.mono,
            fontSize: '13px',
            fontWeight: 600,
            color: summary?.tone || COLORS.textMuted,
          }}>{summary?.line || 'No clouds connected'}</div>
          <div style={{
            marginTop: '4px',
            fontFamily: FONT.mono,
            fontSize: '10px',
            color: COLORS.textMuted,
            lineHeight: 1.6,
          }}>
            {health?.checked_at ? `Last checked ${ago(health.checked_at)}` : 'Not checked yet'}
            {schedule.enabled && health?.next_check_at ? ` · next ${until(health.next_check_at)}` : ''}
            {!schedule.enabled ? ' · scheduled checks are off' : ''}
          </div>
        </div>

        <Button size="sm" onClick={check} disabled={checking || clouds.length === 0}>
          {checking ? <Spinner size={11} /> : null}
          {checking ? 'Checking…' : 'Check now'}
        </Button>
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
        {clouds.map((cloud) => <CloudRow key={cloud.id} cloud={cloud} />)}
      </div>

      {/* How often, or never.

          Off is a real answer rather than a hidden one: somebody metered by the
          request, or running SAND on a laptop that is asleep most of the day,
          should be able to say so — and the drawer stops claiming a freshness
          nothing is maintaining. The clouds are still pinged whenever the
          accounts panel is refreshed, which is what keeps the figure honest
          rather than absent. */}
      <div style={{ marginTop: '18px', paddingTop: '14px', borderTop: `1px solid ${COLORS.border}` }}>
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 700,
          letterSpacing: '1.2px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
          marginBottom: '10px',
        }}>How often</div>

        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px' }}>
          {INTERVALS.map((choice) => (
            <Choice
              key={choice.minutes}
              label={choice.label}
              on={schedule.enabled && schedule.interval_minutes === choice.minutes}
              disabled={saving}
              onClick={() => setSchedule({ enabled: true, minutes: choice.minutes })}
            />
          ))}
          <Choice
            label="Off"
            on={!schedule.enabled}
            disabled={saving}
            tone={COLORS.textMuted}
            onClick={() => setSchedule({ enabled: false })}
          />
        </div>

        <p style={{
          margin: '10px 0 0',
          fontFamily: FONT.sans,
          fontSize: '11px',
          lineHeight: 1.6,
          color: COLORS.textMuted,
        }}>
          A check is one small request per cloud and moves no data. It runs only
          while the vault is unlocked — a slot that passes while it is locked is
          not lost, and comes round as soon as it is opened again.
        </p>
      </div>
    </Modal>
  )
}

/* One cloud's standing. The account's own colour down the edge, because that
   is how an account is identified everywhere else in the app. */
function CloudRow({ cloud }) {
  const tone = !cloud.checked ? COLORS.textMuted : cloud.healthy ? COLORS.success : COLORS.error
  const took = cloud.took_ms > 0 ? `${cloud.took_ms < 1000
    ? `${cloud.took_ms}ms`
    : `${(cloud.took_ms / 1000).toFixed(1)}s`}` : null

  return (
    <div style={{
      display: 'flex',
      alignItems: 'flex-start',
      gap: '9px',
      padding: '9px 11px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderLeft: `3px solid ${accountColor(cloud.id)}`,
      borderRadius: '6px',
    }}>
      <span aria-hidden="true" style={{
        width: '7px',
        height: '7px',
        marginTop: '5px',
        borderRadius: '50%',
        flexShrink: 0,
        background: tone,
      }} />
      <span style={{ fontSize: '13px', lineHeight: 1.3 }}>{KIND_ICONS[cloud.kind] || '☁'}</span>

      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '12px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{cloud.name}</div>

        <div style={{
          marginTop: '3px',
          fontFamily: FONT.mono,
          fontSize: '9.5px',
          color: tone,
          lineHeight: 1.5,
        }}>
          {!cloud.checked ? 'Not checked yet'
            : cloud.healthy ? `Answering${took ? ` in ${took}` : ''}`
              /* How long it has been down, not merely that it is: one is a
                 blip somebody can wait out and the other is a cloud that has
                 quietly stopped being one of the places their files live. An
                 outage a minute old says nothing after "not answering", which
                 is why the duration can come back empty. */
              : ['Not answering', lasting(cloud.failing_since)].filter(Boolean).join(' ')}
        </div>

        {cloud.error && (
          <div style={{
            marginTop: '4px',
            fontFamily: FONT.mono,
            fontSize: '9.5px',
            color: COLORS.textDim,
            wordBreak: 'break-word',
            lineHeight: 1.5,
          }}>{cloud.error}</div>
        )}
      </div>

      <span style={{
        flexShrink: 0,
        fontFamily: FONT.mono,
        fontSize: '9px',
        color: COLORS.textMuted,
      }}>{ago(cloud.checked_at)}</span>
    </div>
  )
}

/* One interval, picked. A radio row written as chips: they are short, there are
   five of them, and a select would hide four of the five behind a tap. */
function Choice({ label, on, tone, disabled, onClick }) {
  const [hover, setHover] = useState(false)

  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      aria-pressed={on}
      onPointerEnter={(e) => { if (e.pointerType === 'mouse') setHover(true) }}
      onPointerLeave={() => setHover(false)}
      style={{
        minHeight: '34px',
        padding: '7px 12px',
        background: on ? COLORS.surfaceHover : COLORS.surfaceRaised,
        border: `1px solid ${on ? COLORS.accent : (hover ? COLORS.borderBright : COLORS.border)}`,
        borderRadius: '6px',
        cursor: disabled ? 'default' : 'pointer',
        opacity: disabled ? 0.6 : 1,
        fontFamily: FONT.mono,
        fontSize: '11px',
        fontWeight: on ? 600 : 400,
        color: on ? COLORS.accentBright : (tone || COLORS.textDim),
        transition: 'border-color 0.15s ease, background 0.15s ease',
      }}
    >{label}</button>
  )
}
