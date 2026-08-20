import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, Spinner } from './ui'

/* A folder that looks after itself.

   Everything else in this app is something you do. This is something you say
   once, and it is the answer to the failure nothing else in the app can catch:
   an account whose token quietly expired in March, a part that never landed
   because the cloud holding it was refusing for the afternoon. Neither makes
   anything on any screen look wrong — a file missing one of its three parts
   reads back perfectly, at full speed, right up until the day a second cloud is
   unavailable and it does not read back at all.

   So the dialog is deliberately in two halves, and the top half is the one that
   matters. "What did the last run find" comes first, because a policy nobody
   ever reads the results of is a policy that is not doing anything. The
   schedule is underneath it.

   Two things are said plainly rather than buried, because both cost something
   real and both are surprising:

     · A repair is a rebuild. It cannot be anything else — a part on a cloud
       that is not answering cannot be copied off it, so the only way to put one
       back is to gather what can be read and cut the file again. That is the
       whole file down and up, and it is why there is a size ceiling.
     · The schedule is kept by the server while the vault is unlocked. A locked
       vault has no index to read and no keys to read it with. Nothing is lost —
       a missed slot comes up due the moment the vault is opened — but a machine
       meant to keep these needs to stay open. */

const CADENCES = [
  { key: 'hourly', label: 'Every hour' },
  { key: 'daily', label: 'Every day' },
  { key: 'weekly', label: 'Every week' },
]

const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

/* The button, beside the film and organizer ones, because all three are things
   done to the folder rather than to anything in it. Lit when the folder has a
   policy that is switched on — a folder looking after itself should say so at a
   glance, and a folder whose last run found something should say that too. */
export function AutomationButton({ automation, mobile, onOpen }) {
  const on = !!automation?.enabled
  /* One flag, worked out on the server from the last run: a listing carries the
     policy without its history, and "is anything wrong here" is the only thing
     about that history a folder's button needs. */
  const trouble = !!automation?.trouble

  const tint = trouble ? COLORS.warn : COLORS.accent
  const title = !automation
    ? 'Off. Have this folder checked on a schedule, and repaired'
    : !on
      ? 'This folder has a policy, switched off'
      : `${describeCadence(automation)} — ${automation.action === 'rebalance'
        ? 'anything missing is put back'
        : 'findings are written down, nothing is moved'}${trouble ? '. The last run found something.' : ''}`

  return (
    <button
      type="button"
      aria-label="Automation for this folder"
      title={title}
      onClick={onOpen}
      style={{
        width: mobile ? 44 : 32,
        height: mobile ? 44 : 32,
        display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
        flexShrink: 0,
        background: on ? `${tint}22` : 'transparent',
        border: `1px solid ${on ? tint : COLORS.border}`,
        borderRadius: '6px',
        color: COLORS.textDim,
        fontSize: mobile ? '15px' : '13px',
        cursor: 'pointer',
      }}
    >⏱</button>
  )
}

/* How a schedule reads in a sentence. */
function describeCadence(policy) {
  if (!policy) return ''
  if (policy.cadence === 'hourly') return 'Every hour'
  if (policy.cadence === 'weekly') return `Every ${DAYS[policy.weekday || 0]} at ${policy.at}`
  return `Every day at ${policy.at}`
}

/* When, written the way a person reads a date rather than an ISO string. */
function when(stamp) {
  if (!stamp) return '—'
  const at = new Date(stamp)
  if (Number.isNaN(at.getTime())) return '—'
  return at.toLocaleString(undefined, {
    weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit',
  })
}

/* The dialog. It loads the folder's full policy — the listing carries a version
   without the history — and everything below is a view of that one object. */
export function AutomationSettings({ path, vault = '', onClose, onChanged }) {
  const [policy, setPolicy] = useState(null)
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [run, setRun] = useState(null)

  useEffect(() => {
    let live = true
    api.automation(path, vault)
      .then((resp) => {
        if (!live) return
        setPolicy(resp.automation || blankPolicy())
        setLoaded(true)
      })
      .catch((err) => { if (live) { setError(err.message); setLoaded(true) } })
    return () => { live = false }
  }, [path, vault])

  const stored = !!policy?.created_at

  const save = async (next) => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.setAutomation({
        vault,
        path,
        enabled: !!next.enabled,
        cadence: next.cadence,
        at: next.at,
        weekday: Number(next.weekday) || 0,
        action: next.action,
        narrow: !!next.narrow,
        max_repairs: Number(next.max_repairs) || 0,
        rebuild_limit: Number(next.rebuild_limit) || 0,
      })
      setPolicy(resp.automation)
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.removeAutomation(path, vault)
      setPolicy(blankPolicy())
      setRun(null)
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  /* Running it now is the whole reason somebody opens this dialog the day they
     reconnect a cloud: they would rather know than wait until ten tomorrow. It
     can take minutes and it can rebuild files, so the button says so and the
     dialog will not close under it. */
  const runNow = async () => {
    setBusy(true)
    setError(null)
    setRun(null)
    try {
      const resp = await api.runAutomation(path, vault)
      setRun(resp.run)
      const fresh = await api.automation(path, vault)
      setPolicy(fresh.automation || blankPolicy())
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Look after this folder"
      subtitle={path === '/' ? 'The whole vault, and everything under it' : `${path}, and everything under it`}
      onClose={() => !busy && onClose()}
      width={600}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {!loaded && <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>}

      {loaded && policy && (
        <>
          {run && <RunReport run={run} />}
          {!run && stored && <LastRuns policy={policy} />}

          <PolicyForm policy={policy} stored={stored} busy={busy} onChange={setPolicy} />

          <div style={{
            display: 'flex', gap: '8px', justifyContent: 'flex-end',
            flexWrap: 'wrap', marginTop: '14px',
          }}>
            {stored && (
              <Button
                variant="ghost"
                disabled={busy}
                title="Forget the schedule and everything it has found. The files are untouched."
                onClick={remove}
              >Remove</Button>
            )}
            {stored && (
              <Button
                variant="ghost"
                disabled={busy}
                title="Check the folder now, without waiting for its next slot"
                onClick={runNow}
              >{busy ? <><Spinner size={12} /> Checking…</> : 'Run it now'}</Button>
            )}
            <Button variant="primary" disabled={busy} onClick={() => save(policy)}>
              {stored ? 'Save' : 'Start looking after it'}
            </Button>
          </div>

          <Footnotes action={policy.action} />
        </>
      )}
    </Modal>
  )
}

/* What a folder is given when it has never had a policy: nightly, and looking
   only. Looking only, because letting a schedule rewrite somebody's files is
   not a thing to switch on for them — it is one line below, and it is theirs to
   turn on. */
function blankPolicy() {
  return {
    enabled: true,
    cadence: 'daily',
    at: '10:00',
    weekday: 0,
    action: 'check',
    narrow: false,
    max_repairs: 0,
    rebuild_limit: 0,
  }
}

/* The schedule and what to do about what it finds. */
function PolicyForm({ policy, stored, busy, onChange }) {
  const set = (patch) => onChange({ ...policy, ...patch })

  return (
    <div style={{ marginTop: stored ? '18px' : 0 }}>
      <Label>When</Label>
      <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap', marginBottom: '10px' }}>
        {CADENCES.map((c) => (
          <Choice
            key={c.key}
            on={policy.cadence === c.key}
            disabled={busy}
            onClick={() => set({ cadence: c.key })}
          >{c.label}</Choice>
        ))}
      </div>

      {policy.cadence === 'weekly' && (
        <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap', marginBottom: '10px' }}>
          {DAYS.map((day, index) => (
            <Choice
              key={day}
              on={Number(policy.weekday) === index}
              disabled={busy}
              onClick={() => set({ weekday: index })}
            >{day.slice(0, 3)}</Choice>
          ))}
        </div>
      )}

      {policy.cadence !== 'hourly' && (
        <Input
          label="At"
          type="time"
          value={policy.at || '10:00'}
          disabled={busy}
          onChange={(e) => set({ at: e.target.value })}
          help="This machine's own clock, so it stays at the hour you meant across the change of season."
        />
      )}

      <Label>What to do about what it finds</Label>
      <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap', marginBottom: '10px' }}>
        <Choice on={policy.action === 'check'} disabled={busy} onClick={() => set({ action: 'check' })}>
          Just tell me
        </Choice>
        <Choice
          on={policy.action === 'rebalance'}
          disabled={busy}
          onClick={() => set({ action: 'rebalance' })}
        >Put it back</Choice>
      </div>
      <Note>
        {policy.action === 'rebalance'
          ? 'Files that cannot be read whole are rebuilt onto the clouds that answered, keeping each file cut exactly as it is now.'
          : 'Nothing is moved and no byte leaves any account. The run is written down and shown here.'}
      </Note>

      {policy.action === 'rebalance' && (
        <div style={{ marginTop: '12px' }}>
          <Toggle
            on={!!policy.narrow}
            disabled={busy}
            onClick={() => set({ narrow: !policy.narrow })}
            label="Cut files narrower when there are too few clouds answering"
            hint="Off, a file whose code will not fit is left alone and named in the report. On, a 4-of-6 file may come back as 3-of-4 — which cannot be undone without another full rebuild."
          />
          <Input
            label="Rebuild at most"
            type="number"
            min="0"
            value={policy.max_repairs || 0}
            disabled={busy}
            onChange={(e) => set({ max_repairs: e.target.value })}
            help="Files per run, 0 for no bound. What is left over is picked up next time, worst first."
          />
          <Input
            label="Largest file to rebuild unattended, in MB"
            type="number"
            min="0"
            value={Math.round((Number(policy.rebuild_limit) || 0) / (1024 * 1024))}
            disabled={busy}
            onChange={(e) => set({ rebuild_limit: Number(e.target.value) * 1024 * 1024 })}
            help={`0 keeps the default of ${formatBytes(1024 * 1024 * 1024)}. A rebuild holds the whole file in memory, so a film left to a schedule at three in the morning is how a small machine stops answering.`}
          />
        </div>
      )}

      <Toggle
        on={!!policy.enabled}
        disabled={busy}
        onClick={() => onChange({ ...policy, enabled: !policy.enabled })}
        label="Run it on this schedule"
        hint="Off keeps the policy and its history and simply never comes round. 'Run it now' still works."
      />
    </div>
  )
}

/* What one run came to, in the shape somebody scans rather than reads. */
function RunReport({ run }) {
  const clean = !run.error && !run.short && !run.at_risk && !(run.offline || []).length

  return (
    <div style={{
      padding: '12px',
      background: COLORS.bg,
      border: `1px solid ${run.error ? COLORS.error : clean ? COLORS.success : COLORS.warn}`,
      borderRadius: '6px',
      marginBottom: '14px',
    }}>
      <div style={{
        fontFamily: FONT.mono, fontSize: '11px', letterSpacing: '1px',
        textTransform: 'uppercase', color: COLORS.textMuted, marginBottom: '8px',
      }}>{when(run.finished_at)}</div>

      {run.error
        ? <div style={{ fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.error }}>{run.error}</div>
        : (
          <div style={{ display: 'flex', gap: '18px', flexWrap: 'wrap' }}>
            <Figure label="checked" value={run.checked} />
            <Figure label="whole" value={run.whole} tone={run.whole ? COLORS.success : undefined} />
            <Figure label="short" value={run.short} tone={run.short ? COLORS.warn : undefined} />
            <Figure label="past repairing" value={run.at_risk} tone={run.at_risk ? COLORS.error : undefined} />
            {run.action === 'rebalance' && (
              <Figure label="rebuilt" value={run.repaired} tone={run.repaired ? COLORS.accent : undefined} />
            )}
            {run.action === 'rebalance' && !!run.deferred && <Figure label="left for later" value={run.deferred} />}
            {!!run.bytes && <Figure label="moved" value={formatBytes(run.bytes)} />}
          </div>
        )}

      {!!(run.offline || []).length && (
        <div style={{
          marginTop: '8px', fontFamily: FONT.sans, fontSize: '12px', color: COLORS.warn,
        }}>No answer from {run.offline.join(', ')}.</div>
      )}

      {!!(run.warnings || []).length && (
        <ul style={{
          margin: '10px 0 0', paddingLeft: '18px',
          fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.5, color: COLORS.textDim,
          maxHeight: '180px', overflowY: 'auto',
        }}>
          {run.warnings.slice(0, 40).map((warning, i) => <li key={i}>{warning}</li>)}
        </ul>
      )}
    </div>
  )
}

/* The last few runs, which is what makes a schedule trustworthy: a fortnight of
   "every part where it should be" is the only evidence that it is working. */
function LastRuns({ policy }) {
  const history = policy.history || []

  return (
    <div style={{ marginBottom: '4px' }}>
      <Label>What it has found</Label>
      {history.length === 0 ? (
        <Note>
          It has not run yet. Next {policy.next_run_at ? when(policy.next_run_at) : 'when it is switched on'}.
        </Note>
      ) : (
        <>
          <RunReport run={history[0]} />
          {history.length > 1 && (
            <div style={{
              fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted, lineHeight: 1.7,
            }}>
              {history.slice(1).map((run, i) => (
                <div key={i} style={{ display: 'flex', gap: '10px', flexWrap: 'wrap' }}>
                  <span style={{ minWidth: '150px' }}>{when(run.finished_at)}</span>
                  <span>
                    {run.error
                      ? run.error
                      : `${run.checked} checked, ${run.short} short${run.repaired ? `, ${run.repaired} rebuilt` : ''}`}
                  </span>
                </div>
              ))}
            </div>
          )}
          {policy.next_run_at && (
            <Note>Next {when(policy.next_run_at)}.</Note>
          )}
        </>
      )}
    </div>
  )
}

function Figure({ label, value, tone }) {
  return (
    <div>
      <div style={{
        fontFamily: FONT.mono, fontSize: '17px', color: tone || COLORS.text,
      }}>{value ?? 0}</div>
      <div style={{
        fontFamily: FONT.mono, fontSize: '9.5px', letterSpacing: '1px',
        textTransform: 'uppercase', color: COLORS.textMuted,
      }}>{label}</div>
    </div>
  )
}

function Label({ children }) {
  return (
    <div style={{
      fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
      textTransform: 'uppercase', color: COLORS.textMuted, margin: '0 0 6px',
    }}>{children}</div>
  )
}

function Note({ children }) {
  return (
    <div style={{
      fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.5,
      color: COLORS.textMuted, marginTop: '6px',
    }}>{children}</div>
  )
}

function Choice({ on, disabled, onClick, children }) {
  return (
    <button
      type="button"
      aria-pressed={on}
      disabled={disabled}
      onClick={onClick}
      style={{
        padding: '7px 12px',
        background: on ? `${COLORS.accent}22` : COLORS.bg,
        border: `1px solid ${on ? COLORS.accent : COLORS.border}`,
        borderRadius: '6px',
        color: on ? COLORS.text : COLORS.textDim,
        fontFamily: FONT.mono, fontSize: '12px',
        cursor: disabled ? 'default' : 'pointer',
        opacity: disabled ? 0.6 : 1,
      }}
    >{children}</button>
  )
}

function Toggle({ on, disabled, onClick, label, hint }) {
  return (
    <div style={{ margin: '12px 0' }}>
      <button
        type="button"
        role="switch"
        aria-checked={on}
        disabled={disabled}
        onClick={onClick}
        style={{
          display: 'flex', alignItems: 'center', gap: '10px',
          width: '100%', padding: '9px 11px', textAlign: 'left',
          background: COLORS.bg,
          border: `1px solid ${on ? COLORS.accent : COLORS.border}`,
          borderRadius: '6px',
          color: COLORS.text,
          fontFamily: FONT.sans, fontSize: '12.5px',
          cursor: disabled ? 'default' : 'pointer',
          opacity: disabled ? 0.6 : 1,
        }}
      >
        <span style={{
          width: '15px', height: '15px', flexShrink: 0,
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          border: `1px solid ${on ? COLORS.accent : COLORS.borderBright}`,
          borderRadius: '4px',
          background: on ? COLORS.accent : 'transparent',
          color: COLORS.bg, fontSize: '10px',
        }}>{on ? '✓' : ''}</span>
        <span>{label}</span>
      </button>
      {hint && <Note>{hint}</Note>}
    </div>
  )
}

/* The two things that cost something, said once, at the bottom, where somebody
   who has already decided will still read them. */
function Footnotes({ action }) {
  return (
    <div style={{
      marginTop: '16px', paddingTop: '12px',
      borderTop: `1px solid ${COLORS.border}`,
      fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
    }}>
      {action === 'rebalance' && (
        <p style={{ margin: '0 0 8px' }}>
          Putting a part back is always a rebuild, never a copy: a part on a cloud
          that is not answering cannot be read off it, so the file is gathered
          from what can be read and cut again. That is the whole file down and up.
        </p>
      )}
      <p style={{ margin: 0 }}>
        Schedules are kept while the vault is unlocked — a locked vault has no
        index to read. Nothing is lost when it locks: a missed slot comes up due
        the moment the vault is opened again.
      </p>
    </div>
  )
}
