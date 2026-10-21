import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, ConfirmDialog, Modal, Spinner } from './ui'

/* The re-encryption behind a password change, as something you can watch and
   steer rather than something you wait for.

   Changing the password rotates the key the stored parts are sealed under, and
   every file has to be gathered, re-encrypted and scattered again to move onto
   it. Done all at once on a large vault that is a day of saturated upload on
   every cloud. So the change can carry a plan instead — so much a day, these
   folders first — and the server moves files on it while the vault is open.
   This file is everything the browser shows and sets about that plan:

     PaceChooser      the budget, chosen in the password dialog
     FolderQueue      which folders go first, and whether the rest follows
     RekeyProgress    the panel: progress, pace, folders, pause, finish now
     RekeyCard        the sidebar's summary, with the two buttons that matter
     RekeyStripEntry  the "Previous key" entry at the top of the vault

   The vocabulary is deliberate. The old password opens nothing from the moment
   the change commits; what remains for a while is a set of files on the
   *previous key*, readable by anyone who holds the previous password and an old
   copy of the vault file. "Previous key" is what the panel, the card, the strip
   and the row marker all say, and none of them ever asks for a second password,
   because there is no second password to ask for. */

export const GB = 1024 ** 3

/* The budgets offered as chips, in gigabytes a day. Wide apart on purpose: the
   difference between 10 and 12 is not a decision anybody needs to make. */
export const PACE_PRESETS = [1, 2, 5, 10, 25, 50, 100]

/* What a paced change starts at when the dialog has no better idea. Ten
   gigabytes a day is a night of a home connection's upstream and well under
   every cloud's daily cap. */
export const DEFAULT_PACE = 10 * GB

/* How long the panel leaves between asking the server where the plan has got
   to. The server moves files on its own clock; this is only how fresh the
   figures on screen are. */
const POLL_MS = 15_000

/* A duration the way a person says one: "about 31 days", "2h 10m", "40 min". */
export function humanDuration(seconds) {
  if (!seconds || seconds <= 0) return 'now'
  const minutes = seconds / 60
  const hours = minutes / 60
  const days = hours / 24
  if (days >= 60) return `about ${Math.round(days / 30)} months`
  if (days >= 14) return `about ${Math.round(days / 7)} weeks`
  if (days >= 1.5) return `about ${Math.round(days)} days`
  if (hours >= 1) {
    const h = Math.floor(hours)
    const m = Math.round((hours - h) * 60)
    return m ? `${h}h ${m}m` : `${h}h`
  }
  if (minutes >= 1) return `${Math.round(minutes)} min`
  return 'under a minute'
}

/* How long `bytes` takes at `perDay`, as the sentence the dialog shows. */
export function describePace(bytes, perDay) {
  if (!perDay || perDay <= 0 || !bytes) return ''
  const seconds = (bytes / perDay) * 86400
  return `At ${formatBytes(perDay)} a day, ${formatBytes(bytes)} takes ${humanDuration(seconds)}.`
}

/* Polls the plan's status while `active`. The first read is immediate. */
export function useRekeyStatus(active, interval = POLL_MS) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)

  const refresh = useCallback(async () => {
    try {
      const fresh = await api.rekeyStatus()
      setStatus(fresh)
      setError(null)
      return fresh
    } catch (err) {
      setError(err.message)
      return null
    }
  }, [])

  useEffect(() => {
    if (!active) return undefined
    refresh()
    const timer = setInterval(refresh, interval)
    return () => clearInterval(timer)
  }, [active, interval, refresh])

  return { status, error, refresh, setStatus }
}

/* The folders the password dialog offers to do first: the top-level folders of
   the main vault with what each holds, largest first. Asked for when the
   chooser is open and not before — a dialog that is going to pick "all at
   once" has no use for the answer. */
export function useTopFolders(active) {
  const [folders, setFolders] = useState(null)

  useEffect(() => {
    if (!active || folders) return
    let cancelled = false
    ;(async () => {
      try {
        const root = await api.list('/')
        const stats = await Promise.all((root.folders || []).map(async (name) => {
          const path = `/${name}`
          try {
            const s = await api.folderStats(path)
            return { path, files: s.files, bytes: s.bytes }
          } catch {
            return { path, files: 0, bytes: 0 }
          }
        }))
        stats.sort((a, b) => b.bytes - a.bytes || a.path.localeCompare(b.path))
        if (!cancelled) setFolders(stats)
      } catch {
        if (!cancelled) setFolders([])
      }
    })()
    return () => { cancelled = true }
  }, [active, folders])

  return folders
}

/* ---------------------------------------------------------------------------
   The budget
   --------------------------------------------------------------------------- */

/* Nearest preset to a budget, as the index the chips and the slider use. */
function presetIndex(perDay) {
  const gb = perDay / GB
  let best = 0
  PACE_PRESETS.forEach((p, i) => {
    if (Math.abs(Math.log(p) - Math.log(gb)) < Math.abs(Math.log(PACE_PRESETS[best]) - Math.log(gb))) best = i
  })
  return best
}

/* Chips for the budget and a field for anything the chips do not say. `value`
   is bytes a day. */
export function PaceChips({ value, onChange, disabled }) {
  const [custom, setCustom] = useState(() => (
    value <= 0 || PACE_PRESETS.some((p) => p * GB === value) ? '' : String(Math.round(value / GB))
  ))
  const chosen = PACE_PRESETS.find((p) => p * GB === value)

  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '6px' }} role="radiogroup" aria-label="How much to re-encrypt a day">
      {PACE_PRESETS.map((p) => {
        const on = chosen === p
        return (
          <button
            key={p}
            type="button"
            role="radio"
            aria-checked={on}
            disabled={disabled}
            onClick={() => { setCustom(''); onChange(p * GB) }}
            style={{
              ...chip,
              borderColor: on ? COLORS.accent : COLORS.borderBright,
              color: on ? COLORS.text : COLORS.textDim,
              background: on ? '#2a1c08' : 'transparent',
              cursor: disabled ? 'not-allowed' : 'pointer',
            }}
          >{p} GB</button>
        )
      })}
      <label style={{ ...chip, borderStyle: 'dashed', display: 'inline-flex', gap: '4px', alignItems: 'center', padding: '2px 6px 2px 10px' }}>
        <input
          type="number"
          min="1"
          step="1"
          inputMode="numeric"
          aria-label="A budget of your own, in gigabytes a day"
          placeholder="custom"
          value={custom}
          disabled={disabled}
          onChange={(e) => {
            setCustom(e.target.value)
            const n = Number(e.target.value)
            if (n > 0) onChange(Math.round(n * GB))
          }}
          style={{
            width: '58px', background: 'transparent', border: 'none', outline: 'none',
            fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.text, padding: '3px 0', minHeight: 0,
          }}
        />
        <span style={{ color: COLORS.textMuted }}>GB</span>
      </label>
      <span style={{ fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted, marginLeft: 'auto' }}>a day</span>
    </div>
  )
}

const chip = {
  border: `1px solid ${COLORS.borderBright}`,
  borderRadius: '999px',
  padding: '4px 10px',
  fontFamily: FONT.mono,
  fontSize: '10.5px',
  background: 'transparent',
  color: COLORS.textDim,
  minHeight: 0,
}

/* All at once, or so much a day. `value` is { paced, perDay }. */
export function PaceChooser({ bytes, files, value, onChange, disabled }) {
  const option = (paced, title, body) => {
    const on = value.paced === paced
    return (
      <button
        type="button"
        role="radio"
        aria-checked={on}
        disabled={disabled}
        onClick={() => onChange({ ...value, paced })}
        style={{
          display: 'flex', gap: '10px', alignItems: 'flex-start', width: '100%', textAlign: 'left',
          padding: '10px 12px', marginBottom: '8px', borderRadius: '6px',
          border: `1px solid ${on ? COLORS.accent : COLORS.border}`,
          background: on ? '#15181f' : COLORS.bg,
          color: COLORS.text, cursor: disabled ? 'not-allowed' : 'pointer', minHeight: 0,
        }}
      >
        <span aria-hidden="true" style={{
          width: '15px', height: '15px', borderRadius: '50%', flexShrink: 0, marginTop: '2px',
          border: `2px solid ${on ? COLORS.accent : COLORS.borderBright}`,
          background: on ? `radial-gradient(circle, ${COLORS.accent} 45%, transparent 50%)` : 'transparent',
        }} />
        <span style={{ minWidth: 0, flex: 1 }}>
          <span style={{ display: 'block', fontFamily: FONT.sans, fontSize: '12px', fontWeight: 600 }}>{title}</span>
          <span style={{ display: 'block', fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.5, color: COLORS.textDim, marginTop: '3px' }}>{body}</span>
          {paced && on && (
            <span style={{ display: 'block', marginTop: '9px' }} onClick={(e) => e.stopPropagation()}>
              <PaceChips value={value.perDay} disabled={disabled} onChange={(perDay) => onChange({ ...value, perDay })} />
              <span style={{ display: 'block', marginTop: '8px', fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.55, color: COLORS.text }}>
                {describePace(bytes, value.perDay)}
                <span style={{ display: 'block', color: COLORS.textDim }}>
                  Until a file has moved, your previous password and an old copy of the vault file
                  or of a cloud backup can still read it. The file list marks those files.
                </span>
              </span>
            </span>
          )}
        </span>
      </button>
    )
  }

  const count = `${files} file${files === 1 ? '' : 's'} · ${formatBytes(bytes)}`
  return (
    <div role="radiogroup" aria-label="How to re-encrypt the files">
      <div style={sectionTitle}>Re-encrypt the files · {count}</div>
      {option(false, 'All at once',
        'Every file is gathered, re-encrypted and scattered again now, as fast as your clouds take it. Keep this tab open.')}
      {option(true, 'Gently, so much a day',
        'The password changes now. Files move in the background while the vault is open, smallest first, never more than this per day.')}
    </div>
  )
}

const sectionTitle = {
  fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '1px', textTransform: 'uppercase',
  color: COLORS.textDim, margin: '0 0 9px',
}

/* ---------------------------------------------------------------------------
   The folder queue
   --------------------------------------------------------------------------- */

/* Which folders go first, in the order they were ticked, and whether the rest
   of the vault follows. `folders` are { path, files, bytes } with, after the
   change, what is still pending in each; `queued` is the ordered list of
   paths; `rest` is the "then everything else" tick. */
export function FolderQueue({ folders, queued, rest, onToggle, onRest, disabled, pending = false }) {
  if (!folders) {
    return <div style={{ ...help, display: 'flex', gap: '8px', alignItems: 'center' }}><Spinner size={11} /> Looking at the folders…</div>
  }
  if (folders.length === 0) {
    return <div style={help}>No folders at the top of the vault — every file goes in turn, smallest first.</div>
  }

  const order = new Map(queued.map((p, i) => [p, i + 1]))
  // Queued folders first, in their order, then the rest as given.
  const rows = [
    ...queued.map((p) => folders.find((f) => f.path === p) || { path: p, files: 0, bytes: 0 }),
    ...folders.filter((f) => !order.has(f.path)),
  ]

  return (
    <div style={{ border: `1px solid ${COLORS.border}`, borderRadius: '6px', overflow: 'hidden' }}>
      {rows.map((f, i) => {
        const n = order.get(f.path)
        const done = pending && f.files === 0
        return (
          <label
            key={f.path}
            style={{
              display: 'grid', gridTemplateColumns: '22px 18px minmax(0,1fr) auto', gap: '8px',
              alignItems: 'center', padding: '8px 10px',
              borderTop: i === 0 ? 'none' : `1px solid ${COLORS.border}`,
              fontFamily: FONT.mono, fontSize: '11px', cursor: disabled ? 'not-allowed' : 'pointer',
            }}
          >
            <span aria-hidden="true" style={{
              width: '16px', height: '16px', borderRadius: '4px', display: 'flex', alignItems: 'center', justifyContent: 'center',
              fontSize: '9px', fontWeight: 700,
              background: n ? COLORS.accent : 'transparent', color: n ? COLORS.bg : COLORS.textMuted,
              border: n ? 'none' : `1px dashed ${COLORS.borderBright}`,
            }}>{n || ''}</span>
            <input
              type="checkbox"
              checked={!!n}
              disabled={disabled}
              onChange={(e) => onToggle(f.path, e.target.checked)}
              aria-label={`Re-encrypt ${f.path} first`}
              style={{ width: '14px', height: '14px', minHeight: 0, accentColor: COLORS.accent, margin: 0 }}
            />
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: COLORS.text }}>
              {f.path}
              {done && <span style={{ color: COLORS.success, marginLeft: '6px' }}>done</span>}
            </span>
            <span style={{ color: COLORS.textMuted, whiteSpace: 'nowrap' }}>
              {done ? '✓' : `${f.files} file${f.files === 1 ? '' : 's'} · ${formatBytes(f.bytes)}`}
            </span>
          </label>
        )
      })}
      <label style={{
        display: 'grid', gridTemplateColumns: '22px 18px minmax(0,1fr) auto', gap: '8px', alignItems: 'center',
        padding: '8px 10px', borderTop: `1px solid ${COLORS.border}`, background: COLORS.bg,
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, cursor: disabled ? 'not-allowed' : 'pointer',
      }}>
        <span />
        <input
          type="checkbox"
          checked={rest}
          disabled={disabled}
          onChange={(e) => onRest(e.target.checked)}
          aria-label="Then everything else"
          style={{ width: '14px', height: '14px', minHeight: 0, accentColor: COLORS.accent, margin: 0 }}
        />
        <span>Then everything else</span>
        <span style={{ color: COLORS.textMuted, whiteSpace: 'nowrap' }}>
          {rest ? '' : 'stops after the folders above'}
        </span>
      </label>
    </div>
  )
}

const help = {
  fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted, marginTop: '6px',
}

/* The tick that keeps the vault open while the plan runs, with the warning it
   needs. The same words wherever it appears. */
export function KeepUnlockedTick({ checked, onChange, disabled }) {
  return (
    <label style={{ display: 'flex', gap: '9px', alignItems: 'flex-start', marginTop: '10px', cursor: disabled ? 'not-allowed' : 'pointer' }}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        style={{ width: '14px', height: '14px', minHeight: 0, flexShrink: 0, marginTop: '2px', accentColor: COLORS.accent }}
      />
      <span style={{ fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.5, color: COLORS.textDim }}>
        <b style={{ color: COLORS.text, fontWeight: 600 }}>Keep the vault unlocked until it finishes.</b>{' '}
        Otherwise files move only while the vault is open, and the idle timer locks it.
        Leaving it unlocked for weeks means anyone at this machine can read your files for weeks.
      </span>
    </label>
  )
}

/* ---------------------------------------------------------------------------
   Progress
   --------------------------------------------------------------------------- */

function fraction(status) {
  if (!status || !status.total) return 0
  return Math.max(0, Math.min(1, (status.total - status.pending) / status.total))
}

function ProgressBar({ value, thin }) {
  return (
    <div
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(value * 100)}
      style={{
        height: thin ? '5px' : '8px', borderRadius: '4px', background: COLORS.bg,
        border: `1px solid ${COLORS.border}`, overflow: 'hidden',
      }}
    >
      <div style={{
        height: '100%', width: `${Math.round(value * 100)}%`,
        background: `linear-gradient(90deg, ${COLORS.accent}, ${COLORS.accentBright})`,
        transition: 'width 0.4s ease',
      }} />
    </div>
  )
}

/* running / paused / idle, as a pill. */
export function StatePill({ status }) {
  const plan = status?.plan
  let tone = COLORS.textMuted
  let text = 'not planned'
  if (plan?.paused) { tone = COLORS.warn; text = 'paused' }
  else if (plan?.bytes_per_day > 0 && status.idle) { tone = COLORS.info; text = 'idle' }
  else if (plan?.bytes_per_day > 0) { tone = COLORS.success; text = status.running ? 'moving' : 'running' }
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', gap: '5px', fontFamily: FONT.mono, fontSize: '9.5px',
      letterSpacing: '0.6px', textTransform: 'uppercase', padding: '2px 7px', borderRadius: '999px',
      border: `1px solid ${tone}66`, color: tone, whiteSpace: 'nowrap',
    }}>
      <span aria-hidden="true" style={{ width: '6px', height: '6px', borderRadius: '50%', background: tone }} />
      {text}
    </span>
  )
}

/* What the panel sends back when a control changes: the whole settings object,
   since a plan is one thing rather than a bag of patches. */
function settingsOf(status) {
  const plan = status?.plan || {}
  return {
    bytes_per_day: plan.bytes_per_day || 0,
    folders: plan.folders || [],
    only_folders: !!plan.only_folders,
    paused: !!plan.paused,
    keep_unlocked: !!plan.keep_unlocked,
  }
}

/* The panel. Everything the plan knows and every control it has. */
export default function RekeyProgress({ onClose, onChanged, zIndex = 120 }) {
  const mobile = useIsMobile()
  const { status, error, refresh, setStatus } = useRekeyStatus(true)
  const [saving, setSaving] = useState(false)
  const [finishing, setFinishing] = useState(false)
  const [confirmFinish, setConfirmFinish] = useState(false)
  const [problem, setProblem] = useState(null)
  const changed = useRef(false)

  const apply = async (settings) => {
    setSaving(true)
    setProblem(null)
    try {
      const fresh = await api.setRekeyPlan(settings)
      setStatus(fresh)
      changed.current = true
    } catch (err) {
      setProblem(err.message)
    } finally {
      setSaving(false)
    }
  }

  const finishNow = async () => {
    setConfirmFinish(false)
    setFinishing(true)
    setProblem(null)
    try {
      const report = await api.migrate()
      if (report.warnings?.length) setProblem(report.warnings.join('\n'))
      changed.current = true
      await refresh()
    } catch (err) {
      setProblem(err.message)
    } finally {
      setFinishing(false)
    }
  }

  const close = () => {
    if (changed.current) onChanged?.()
    onClose()
  }

  const plan = status?.plan
  const settings = settingsOf(status)
  const done = status && status.pending === 0
  const perDay = plan?.bytes_per_day || 0
  const since = status?.since ? new Date(status.since) : null

  return (
    <Modal
      title="Re-encrypting"
      subtitle={since
        ? `Moving files off the key your previous password protected. Changed ${since.toLocaleDateString(undefined, { day: 'numeric', month: 'long' })}.`
        : 'Moving files off the key your previous password protected.'}
      onClose={finishing ? undefined : close}
      zIndex={zIndex}
      width={540}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {problem && <Banner tone="warn" onDismiss={() => setProblem(null)}>
        <span style={{ fontFamily: FONT.mono, fontSize: '11px', whiteSpace: 'pre-wrap' }}>{problem}</span>
      </Banner>}

      {!status && !error && <div style={{ display: 'flex', justifyContent: 'center', padding: '20px' }}><Spinner /></div>}

      {done && (
        <Banner tone="success">
          Every file is on the current key, and the previous key has been erased from the vault.
        </Banner>
      )}

      {status && !done && (
        <>
          <ProgressBar value={fraction(status)} />
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(96px, 1fr))', gap: '10px', margin: '12px 0 4px' }}>
            <Figure value={status.total - status.pending} label="files moved" />
            <Figure value={status.pending} label="files left" />
            <Figure value={formatBytes(status.pending_bytes)} label={`left of ${formatBytes(status.total_bytes)}`} />
            <Figure
              value={perDay > 0 && !plan?.paused ? humanDuration(status.eta_seconds) : '—'}
              label={perDay > 0 ? `at ${formatBytes(perDay)} a day` : 'no pace set'}
            />
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, marginTop: '10px', minWidth: 0 }}>
            <StatePill status={status} />
            {status.running && <Spinner size={11} />}
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1, minWidth: 0 }}>
              {status.running ? (
                <>
                  {status.current}
                  {status.current_size > 0 && (
                    <span style={{ color: COLORS.textMuted }}>
                      {' '}· {formatBytes(status.current_done)} of {formatBytes(status.current_size)}
                    </span>
                  )}
                </>
              )
                : status.next_in_seconds > 0 ? `next file in ${humanDuration(status.next_in_seconds)}`
                  : plan?.paused ? 'paused by you'
                    : status.idle && perDay > 0 ? 'the plan has done everything it was allowed to'
                      : perDay > 0 ? 'waiting for the next tick' : 'nothing moves until a pace is set'}
            </span>
            {/* The file being moved can be left for last: the move stops where
                it is, the file stays readable on the previous key, and it is
                taken up again after everything else. For a film on an account
                that keeps dropping, or a file that is simply in the way. */}
            {status.running && (
              <Button
                size="sm"
                variant="ghost"
                disabled={saving || finishing}
                title="Stop moving this file and leave it for last; it stays readable on the previous key"
                onClick={async () => {
                  setSaving(true)
                  setProblem(null)
                  try {
                    setStatus(await api.skipRekeyFile(status.current_id || ''))
                    changed.current = true
                  } catch (err) {
                    setProblem(err.message)
                  } finally {
                    setSaving(false)
                  }
                }}
              >Skip for now</Button>
            )}
          </div>

          {/* Pace */}
          <div style={section}>
            <div style={sectionTitle}>Pace</div>
            <input
              id="rekey-pace"
              type="range"
              min={0}
              max={PACE_PRESETS.length - 1}
              step={1}
              value={perDay > 0 ? presetIndex(perDay) : 0}
              disabled={saving || finishing}
              aria-label="Gigabytes a day"
              onChange={(e) => apply({ ...settings, bytes_per_day: PACE_PRESETS[Number(e.target.value)] * GB, paused: false })}
              style={{ width: '100%', accentColor: COLORS.accent, margin: '6px 0 2px' }}
            />
            <div style={{ display: 'flex', justifyContent: 'space-between', fontFamily: FONT.mono, fontSize: '9px', color: COLORS.textMuted }}>
              {PACE_PRESETS.map((p) => <span key={p}>{p}{p === PACE_PRESETS[PACE_PRESETS.length - 1] ? ' GB' : ''}</span>)}
            </div>
            <div style={{ marginTop: '10px' }}>
              <PaceChips
                value={perDay}
                disabled={saving || finishing}
                onChange={(bytes) => apply({ ...settings, bytes_per_day: bytes, paused: false })}
              />
            </div>
            {perDay > 0 && !plan?.paused && (
              <div style={{ ...help, fontFamily: FONT.mono, fontSize: '10px' }}>
                {describePace(status.pending_bytes, perDay)}
              </div>
            )}
          </div>

          {/* Folders */}
          <div style={section}>
            <div style={sectionTitle}>Folders</div>
            <FolderQueue
              pending
              folders={status.folders}
              queued={settings.folders}
              rest={!settings.only_folders}
              disabled={saving || finishing}
              onToggle={(path, on) => {
                const folders = on
                  ? [...settings.folders.filter((p) => p !== path), path]
                  : settings.folders.filter((p) => p !== path)
                apply({ ...settings, folders, only_folders: folders.length > 0 && settings.only_folders })
              }}
              onRest={(on) => apply({ ...settings, only_folders: !on })}
            />
            <div style={help}>
              Tick a folder to move it next; the numbers are the order. Smallest files go first within a folder.
            </div>
            {status.deferred?.length > 0 && (
              <div style={{ marginTop: '12px' }}>
                <div style={sectionTitle}>Left for last</div>
                <div style={{ border: `1px solid ${COLORS.border}`, borderRadius: '6px', overflow: 'hidden' }}>
                  {status.deferred.map((f, i) => (
                    <div
                      key={f.id}
                      style={{
                        display: 'flex', alignItems: 'center', gap: '8px', padding: '6px 10px',
                        borderTop: i === 0 ? 'none' : `1px solid ${COLORS.border}`,
                        fontFamily: FONT.mono, fontSize: '11px',
                      }}
                    >
                      <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{f.path}</span>
                      <span style={{ color: COLORS.textMuted, whiteSpace: 'nowrap' }}>{formatBytes(f.bytes)}</span>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={saving || finishing}
                        title="Put this file back in its turn"
                        onClick={async () => {
                          setSaving(true)
                          try {
                            setStatus(await api.skipRekeyFile(f.id, true))
                            changed.current = true
                          } catch (err) {
                            setProblem(err.message)
                          } finally {
                            setSaving(false)
                          }
                        }}
                      >Back in turn</Button>
                    </div>
                  ))}
                </div>
                <div style={help}>These move after everything else. A skipped file stays readable on the previous key meanwhile.</div>
              </div>
            )}
          </div>

          {/* While it runs */}
          <div style={section}>
            <div style={sectionTitle}>While it runs</div>
            <KeepUnlockedTick
              checked={settings.keep_unlocked}
              disabled={saving || finishing || perDay === 0}
              onChange={(on) => apply({ ...settings, keep_unlocked: on })}
            />
            {status.warnings?.length > 0 && (
              <div style={{ marginTop: '12px' }}>
                <Banner tone="warn">
                  <span style={{ fontFamily: FONT.mono, fontSize: '10.5px', whiteSpace: 'pre-wrap' }}>
                    {`${status.warnings.length} file${status.warnings.length === 1 ? '' : 's'} could not be read and will be tried again in an hour:\n${status.warnings.join('\n')}`}
                  </span>
                </Banner>
              </div>
            )}
          </div>
        </>
      )}

      <div style={{ display: 'flex', flexDirection: mobile ? 'column' : 'row', justifyContent: 'flex-end', gap: '8px', marginTop: '16px' }}>
        {status && !done && (
          <>
            <Button
              variant="danger"
              disabled={saving || finishing}
              onClick={() => setConfirmFinish(true)}
              style={{ marginRight: mobile ? 0 : 'auto', justifyContent: 'center' }}
            >
              {finishing ? <Spinner size={11} color={COLORS.error} /> : null}
              {finishing ? 'Finishing…' : `Finish now · ${formatBytes(status.pending_bytes)} at full speed`}
            </Button>
            {perDay > 0 && (
              <Button
                disabled={saving || finishing}
                onClick={() => apply({ ...settings, paused: !settings.paused })}
                style={{ justifyContent: 'center' }}
              >{settings.paused ? 'Resume' : 'Pause'}</Button>
            )}
          </>
        )}
        <Button variant="primary" onClick={close} disabled={finishing} style={{ justifyContent: 'center' }}>Done</Button>
      </div>

      {confirmFinish && (
        <ConfirmDialog
          title="Finish re-encrypting now?"
          confirmLabel="Finish now"
          onConfirm={finishNow}
          onClose={() => setConfirmFinish(false)}
          zIndex={zIndex + 10}
        >
          {formatBytes(status?.pending_bytes || 0)} in {status?.pending} file{status?.pending === 1 ? '' : 's'} will be gathered,
          re-encrypted and scattered again as fast as your clouds take it. Keep this tab open while it runs;
          every file is committed as it moves, so closing it only leaves the rest for later.
        </ConfirmDialog>
      )}
    </Modal>
  )
}

const section = { marginTop: '16px', borderTop: `1px solid ${COLORS.border}`, paddingTop: '14px' }

function Figure({ value, label }) {
  return (
    <div>
      <div style={{ fontFamily: FONT.mono, fontSize: '15px', fontWeight: 600, fontVariantNumeric: 'tabular-nums', color: COLORS.text }}>{value}</div>
      <div style={{ fontFamily: FONT.mono, fontSize: '9px', letterSpacing: '1px', textTransform: 'uppercase', color: COLORS.textMuted, marginTop: '2px' }}>{label}</div>
    </div>
  )
}

/* ---------------------------------------------------------------------------
   The sidebar card
   --------------------------------------------------------------------------- */

/* The accounts drawer's summary of the plan: a bar, three figures and the two
   buttons that matter. Drawn only while something is on the previous key, and
   it polls on its own so the bar moves without anyone refreshing. */
export function RekeyCard({ stats, onChanged }) {
  const active = (stats?.pending_migration || 0) > 0
  const { status, setStatus } = useRekeyStatus(active, 30_000)
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)

  if (!active) return null

  const plan = status?.plan
  const perDay = plan?.bytes_per_day || 0
  const toggle = async () => {
    setBusy(true)
    try {
      setStatus(await api.setRekeyPlan({ ...settingsOf(status), paused: !plan?.paused }))
    } catch {
      // The panel says why; the card has no room to.
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{
      background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
      padding: '11px 12px', marginTop: '12px',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '8px', fontFamily: FONT.mono, fontSize: '11px', fontWeight: 600 }}>
        <span><span aria-hidden="true">🗝</span> Re-encrypting</span>
        {status && <StatePill status={status} />}
      </div>
      <div style={{ marginTop: '9px' }}><ProgressBar thin value={fraction(status)} /></div>
      <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted, marginTop: '7px', lineHeight: 1.6 }}>
        {status ? (
          <>
            {Math.round(fraction(status) * 100)}% · {formatBytes(status.pending_bytes)} left on the previous key
            <br />
            {plan?.paused ? 'Paused by you'
              : perDay > 0 ? `${formatBytes(perDay)} a day${status.eta_seconds ? ` · ${humanDuration(status.eta_seconds)}` : ''}`
                : 'No pace set — nothing moves until you choose one'}
          </>
        ) : `${stats.pending_migration} file${stats.pending_migration === 1 ? '' : 's'} on the previous key`}
      </div>
      <div style={{ display: 'flex', gap: '6px', marginTop: '9px' }}>
        {perDay > 0 && (
          <Button size="sm" disabled={busy} onClick={toggle} style={{ flex: 1, justifyContent: 'center' }}>
            {plan?.paused ? 'Resume' : 'Pause'}
          </Button>
        )}
        <Button size="sm" variant={perDay > 0 ? 'ghost' : 'default'} onClick={() => setOpen(true)} style={{ flex: 1, justifyContent: 'center' }}>
          Details ›
        </Button>
      </div>
      {open && (
        <RekeyProgress
          onClose={() => setOpen(false)}
          onChanged={onChanged}
        />
      )}
    </div>
  )
}

/* ---------------------------------------------------------------------------
   The entry at the top of the vault
   --------------------------------------------------------------------------- */

/* Drawn beside the sub vaults, in their chrome — dashed, above the folders,
   not among them — because it is the same kind of thing to the eye: a part of
   the vault that is not a folder. It is not a sub vault and asks for no
   password; it opens the panel. */
export function RekeyStripEntry({ pending, mobile }) {
  const { status } = useRekeyStatus(pending > 0, 30_000)
  const [open, setOpen] = useState(false)
  if (!pending) return null

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        title="Files still sealed under the key your previous password protected — open to see where the re-encryption has got to"
        style={{
          display: 'flex', alignItems: 'center', gap: '8px', padding: '9px 12px',
          minHeight: mobile ? '46px' : '38px', flex: mobile ? '1 1 calc(50% - 8px)' : '0 0 auto',
          background: COLORS.surfaceRaised, border: `1px dashed ${COLORS.accent}`, borderRadius: '8px',
          cursor: 'pointer', textAlign: 'left', color: COLORS.text, fontFamily: FONT.mono, fontSize: '12px',
        }}
      >
        <span aria-hidden="true">🗝</span>
        <span style={{ minWidth: 0 }}>
          <span style={{ display: 'block' }}>Previous key</span>
          <span style={{ display: 'block', fontSize: '9px', color: COLORS.textDim, marginTop: '1px', whiteSpace: 'nowrap' }}>
            {status
              ? `${status.pending} file${status.pending === 1 ? '' : 's'} · ${formatBytes(status.pending_bytes)}${status.eta_seconds && !status.plan?.paused ? ` · ${humanDuration(status.eta_seconds)}` : ''}`
              : `${pending} file${pending === 1 ? '' : 's'}`}
          </span>
          {status && <span style={{ display: 'block', width: '90px', marginTop: '4px' }}><ProgressBar thin value={fraction(status)} /></span>}
        </span>
      </button>
      {open && <RekeyProgress onClose={() => setOpen(false)} />}
    </>
  )
}

/* The marker a file row wears while it is still on the previous key. Quiet on
   purpose: the file is not broken, and not at risk from anyone who does not
   already hold the previous password and an old copy of the vault file. */
export function PreviousKeyPill({ tiny }) {
  return (
    <span
      title="Still sealed under the key your previous password protected. It moves when the re-encryption reaches it."
      style={{
        marginLeft: tiny ? '4px' : '6px', fontFamily: FONT.mono, fontSize: tiny ? '8px' : '9px', letterSpacing: '0.3px',
        color: COLORS.textDim, border: `1px dashed ${COLORS.borderBright}`, borderRadius: '999px',
        padding: tiny ? '0 4px' : '1px 6px', whiteSpace: 'nowrap', flexShrink: 0,
      }}
    >prev key</span>
  )
}
