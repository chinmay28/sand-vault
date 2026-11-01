import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Modal, Button, Input } from './ui'
import { Choice } from './CloudHealth'

/* How SAND is sized to the machine it runs on.

   Every number that decides how much is in flight or in memory at once — the
   chunk a file is cut into, how many chunks an upload keeps in the air, how
   far a download reads ahead, how much decrypted film the vault keeps for a
   player seeking around — was chosen for a Raspberry Pi. The same binary on
   a workstation with sixteen times the memory was being held to the same
   numbers: safe, and slow.

   So the server detects what it is running on and sizes itself to that,
   and this is where that is read and, when detection is wrong or the box
   has other work to leave room for, overruled. Three ways, in order of how
   much you have to know: pick a profile; describe the machine and let the
   formula run; or open Advanced and set any knob by hand, with the default
   beside it and a way back.

   The page knows nothing the server does not tell it. The knobs, their
   ranges and their hints all arrive with the reading, so a knob added on
   the server is a line here without a change here. */

const MIB = 1024 * 1024
const GIB = 1024 * MIB

/* The settings line's reading: which profile, and the machine it stands on. */
export function machineStatusLine(view) {
  if (!view) return null
  const profile = (view.profiles || []).find((p) => p.id === view.profile)
  const name = profile ? profile.label.split(' · ')[0] : view.profile
  const m = view.machine || {}
  const changed = Object.keys(view.overrides || {}).length
  const base = `${name} · ${m.cpus} CPU${m.cpus === 1 ? '' : 's'} · ${formatBytes(m.memory_bytes)}`
  return changed ? `${base} · ${changed} changed` : base
}

export default function MachineSettings({ zIndex, onClose, onChanged }) {
  const [view, setView] = useState(null)
  const [error, setError] = useState(null)
  const [saving, setSaving] = useState(false)
  const [advanced, setAdvanced] = useState(false)

  /* What is being edited and not yet saved: the custom machine, and the
     overrides. The profile chips save on the spot; these two wait for a
     button, since a number half-typed is not a setting. */
  const [custom, setCustom] = useState({ cpus: '', gb: '' })
  const [edits, setEdits] = useState({})

  const load = () => api.machine().then((v) => {
    setView(v)
    setEdits(bytesToFields(v.overrides || {}, v.knobs || []))
    const m = v.profile === 'custom' ? v.machine : (v.detected || v.machine)
    setCustom({ cpus: String(m?.cpus ?? ''), gb: String(Math.round((m?.memory_bytes || 0) / GIB)) })
  }).catch((err) => setError(err.message))
  useEffect(() => { load() }, [])

  const save = async (body) => {
    setSaving(true)
    setError(null)
    try {
      const next = await api.setMachine(body)
      setView(next)
      setEdits(bytesToFields(next.overrides || {}, next.knobs || []))
      if (onChanged) onChanged(next)
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  const choose = (profile) => {
    const body = { profile, overrides: fieldsToBytes(edits, view.knobs) }
    if (profile === 'custom') body.machine = customMachine(custom, view)
    save(body)
  }

  const applyCustom = () => save({
    profile: 'custom',
    machine: customMachine(custom, view),
    overrides: fieldsToBytes(edits, view.knobs),
  })

  const saveEdits = () => {
    const body = { profile: view.profile, overrides: fieldsToBytes(edits, view.knobs) }
    if (view.profile === 'custom') body.machine = view.machine
    save(body)
  }

  const reset = async () => {
    setSaving(true)
    setError(null)
    try {
      const next = await api.resetMachine()
      setView(next)
      setEdits({})
      if (onChanged) onChanged(next)
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  const dirty = view && JSON.stringify(fieldsToBytes(edits, view.knobs))
    !== JSON.stringify(view.overrides || {})
  const overridden = Object.keys(view?.overrides || {}).length

  return (
    <Modal
      title="This machine"
      subtitle="How SAND is sized to the box it runs on"
      onClose={onClose}
      width={560}
      zIndex={zIndex}
    >
      <p style={paragraph}>
        How big a piece a file is cut into, how many pieces an upload keeps in
        the air, how far a download reads ahead and how much of a film is kept
        for seeking all depend on what this machine has. A Raspberry Pi and a
        workstation should not run the same numbers, so they are taken from
        the machine unless you say otherwise here. Nothing here changes what is
        already stored.
      </p>

      {error && <p style={warning}>{error}</p>}

      {view?.pinned && (
        <p style={warning}>
          The profile is set to <b>{view.profile}</b> on the server&apos;s command
          line, so it cannot be changed here. The knobs under Advanced still can.
        </p>
      )}

      {!view && !error && <p style={muted}>Reading what this machine has…</p>}

      {view && (
        <>
          <div style={heading}>Sized as</div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px' }}>
            {view.profiles.map((p) => (
              <Choice
                key={p.id}
                label={p.label}
                on={view.profile === p.id}
                disabled={saving || view.pinned}
                onClick={() => choose(p.id)}
              />
            ))}
          </div>

          <p style={{ ...muted, margin: '10px 0 0' }}>
            Detected: {describe(view.detected)}.
            {view.profile !== 'auto' && <> Sized for: {describe(view.machine)}.</>}
          </p>

          {view.profile === 'custom' && (
            <div style={{ display: 'flex', gap: '12px', alignItems: 'flex-end', marginTop: '14px' }}>
              <div style={{ flex: 1 }}>
                <Input
                  label="CPUs"
                  type="number"
                  min={1}
                  value={custom.cpus}
                  onChange={(e) => setCustom({ ...custom, cpus: e.target.value })}
                  style={{ marginBottom: 0 }}
                />
              </div>
              <div style={{ flex: 1 }}>
                <Input
                  label="Memory (GB)"
                  type="number"
                  min={1}
                  value={custom.gb}
                  onChange={(e) => setCustom({ ...custom, gb: e.target.value })}
                />
              </div>
              <div style={{ marginBottom: '14px' }}>
                <Button onClick={applyCustom} disabled={saving || view.pinned}>Apply</Button>
              </div>
            </div>
          )}

          <div style={{ ...heading, marginTop: '18px', display: 'flex', alignItems: 'center', gap: '10px' }}>
            <span>Advanced</span>
            <button
              type="button"
              onClick={() => setAdvanced(!advanced)}
              style={linkButton}
            >
              {advanced ? 'Hide' : (overridden ? `Show · ${overridden} changed` : 'Show')}
            </button>
          </div>

          {advanced && (
            <>
              <p style={{ ...muted, margin: '0 0 10px' }}>
                Every knob, with what the profile would set it to. A changed
                one is kept when the profile changes; the arrow puts it back.
              </p>
              <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
                {view.knobs.map((k) => (
                  <KnobRow
                    key={k.name}
                    knob={k}
                    value={edits[k.name]}
                    defaultValue={view.defaults[k.name]}
                    effective={view.effective[k.name]}
                    disabled={saving}
                    onChange={(next) => setEdits({ ...edits, [k.name]: next })}
                    onReset={() => {
                      const next = { ...edits }
                      delete next[k.name]
                      setEdits(next)
                    }}
                  />
                ))}
              </div>

              <p style={{ ...muted, margin: '12px 0 0' }}>
                These settings commit about {formatBytes(view.footprint)} at once
                — one upload at full window, one download at full read-ahead and
                the cache full — of the {formatBytes(view.machine.memory_bytes)} this
                profile counts on.
              </p>

              <div style={{ display: 'flex', gap: '8px', marginTop: '14px', flexWrap: 'wrap' }}>
                <Button variant="primary" onClick={saveEdits} disabled={saving || !dirty}>Save</Button>
                <Button onClick={() => setEdits(bytesToFields(view.overrides || {}, view.knobs))} disabled={saving || !dirty}>Discard</Button>
                <div style={{ flex: 1 }} />
                <Button variant="danger" onClick={reset} disabled={saving}>Reset to system defaults</Button>
              </div>
            </>
          )}
        </>
      )}
    </Modal>
  )
}

/* One knob: its name and hint on the left, the field and the default on the
   right. A size is typed in MiB rather than bytes — nobody types 16777216 —
   and a level is a row of chips, since a scale of four with named ends is
   not a number anybody wants to type either. */
function KnobRow({ knob, value, defaultValue, effective, disabled, onChange, onReset }) {
  const changed = value !== undefined && value !== ''
  const shown = changed ? value : displayValue(knob, defaultValue)

  return (
    <div style={{
      display: 'flex',
      gap: '12px',
      alignItems: 'center',
      padding: '8px 10px',
      background: changed ? COLORS.surfaceHover : COLORS.surfaceRaised,
      border: `1px solid ${changed ? COLORS.accent : COLORS.border}`,
      borderRadius: '6px',
    }}>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ fontFamily: FONT.sans, fontSize: '12px', color: COLORS.text }}>{knob.label}</div>
        <div style={{ fontFamily: FONT.sans, fontSize: '10.5px', lineHeight: 1.5, color: COLORS.textMuted }}>
          {knob.hint}
          {knob.restart && ' — takes effect after a restart'}
        </div>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: '6px', flexShrink: 0 }}>
        {knob.unit === 'level' ? (
          [1, 2, 3, 4].map((level) => (
            <Choice
              key={level}
              label={String(level)}
              on={Number(shown) === level}
              disabled={disabled}
              onClick={() => onChange(String(level))}
            />
          ))
        ) : (
          <>
            <input
              type="number"
              inputMode="numeric"
              value={shown}
              min={displayValue(knob, knob.min)}
              max={displayValue(knob, knob.max)}
              step={knob.unit === 'bytes' ? Math.max(1, (knob.step || MIB) / MIB) : 1}
              disabled={disabled}
              onChange={(e) => onChange(e.target.value)}
              aria-label={knob.label}
              style={{
                width: '84px',
                padding: '6px 8px',
                background: COLORS.bg,
                border: `1px solid ${COLORS.border}`,
                borderRadius: '4px',
                color: COLORS.text,
                fontFamily: FONT.mono,
                fontSize: '12px',
                textAlign: 'right',
              }}
            />
            <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted, width: '28px' }}>
              {knob.unit === 'bytes' ? 'MiB' : ''}
            </span>
          </>
        )}
        <span
          title={`Default ${displayValue(knob, defaultValue)}${knob.unit === 'bytes' ? ' MiB' : ''}${changed ? '' : ' — in effect'}`}
          style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted, width: '56px', textAlign: 'right' }}
        >
          {changed ? `was ${compact(knob, defaultValue)}` : compact(knob, effective)}
        </span>
        <button
          type="button"
          onClick={onReset}
          disabled={disabled || !changed}
          title="Back to the profile's value"
          aria-label={`Reset ${knob.label}`}
          style={{ ...linkButton, opacity: changed ? 1 : 0.3, width: '20px' }}
        >↺</button>
      </div>
    </div>
  )
}

/* Bytes are shown and typed in MiB; everything else as it is. */
function displayValue(knob, bytes) {
  if (bytes === undefined || bytes === null) return ''
  if (knob.unit === 'bytes') return String(Math.round(bytes / MIB))
  return String(bytes)
}

function compact(knob, value) {
  if (value === undefined || value === null) return ''
  if (knob.unit === 'bytes') return formatBytes(value)
  return String(value)
}

/* The overrides as the server holds them, bytes and all, to what the fields
   show; and back, dropping the fields left empty. */
function bytesToFields(overrides, knobs) {
  const out = {}
  for (const k of knobs) {
    if (overrides[k.name] !== undefined) out[k.name] = displayValue(k, overrides[k.name])
  }
  return out
}

function fieldsToBytes(fields, knobs) {
  const out = {}
  for (const k of knobs || []) {
    const raw = fields[k.name]
    if (raw === undefined || raw === '') continue
    const n = Number(raw)
    if (!Number.isFinite(n)) continue
    out[k.name] = k.unit === 'bytes' ? Math.round(n) * MIB : Math.round(n)
  }
  return out
}

function customMachine(custom, view) {
  const cpus = Math.max(1, Math.round(Number(custom.cpus) || view?.detected?.cpus || 1))
  const gb = Math.max(1, Number(custom.gb) || Math.round((view?.detected?.memory_bytes || GIB) / GIB))
  return { cpus, memory_bytes: Math.round(gb * GIB) }
}

function describe(m) {
  if (!m) return 'nothing'
  return `${m.cpus} CPU${m.cpus === 1 ? '' : 's'}, ${formatBytes(m.memory_bytes)}`
}

const paragraph = {
  margin: '0 0 14px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textDim,
}

const muted = {
  margin: 0,
  fontFamily: FONT.sans,
  fontSize: '11px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}

const warning = {
  margin: '0 0 14px',
  padding: '10px 12px',
  fontFamily: FONT.sans,
  fontSize: '11.5px',
  lineHeight: 1.6,
  color: COLORS.warn,
  background: COLORS.surfaceRaised,
  border: `1px solid ${COLORS.border}`,
  borderRadius: '6px',
}

const heading = {
  fontFamily: FONT.mono,
  fontSize: '10px',
  fontWeight: 700,
  letterSpacing: '1.2px',
  textTransform: 'uppercase',
  color: COLORS.textMuted,
  marginBottom: '10px',
}

const linkButton = {
  background: 'none',
  border: 'none',
  padding: 0,
  cursor: 'pointer',
  fontFamily: FONT.mono,
  fontSize: '10px',
  letterSpacing: '1.2px',
  textTransform: 'uppercase',
  color: COLORS.accentBright,
}
