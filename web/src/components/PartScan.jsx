import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'
import { ago } from './CloudHealth'

/* The scan of the local disks.

   A disk on this machine can lose a part — deleted out from under SAND, or
   left as a zero-length file when the disk lost power after accepting the
   write — and nothing notices until something reads the file. For a cloud,
   looking at every part is a request per part; for a disk it is one
   directory listing. So the disks get a scan of their own: each one listed
   and held against the index, on a schedule, daily unless changed. This is
   that schedule, the last scan's findings, and the button that runs one now.
   What a scan finds goes to "Files missing a part", with the reason. */

/* The schedule's choices. A scan is a directory listing per disk, so even
   the shortest is cheap; the longest is for a machine whose disks are
   rarely touched. */
const INTERVALS = [
  { minutes: 6 * 60, label: '6 hours' },
  { minutes: 24 * 60, label: 'Daily' },
  { minutes: 7 * 24 * 60, label: 'Weekly' },
]

/* One line for the settings list: what the last scan found, or when the
   next one is. */
export function scanStatusLine(status) {
  if (!status) return ''
  if (status.local_accounts === 0) return 'No account is on a disk in this machine'
  const last = status.last
  if (last && last.lost > 0) {
    return `${last.lost} part${last.lost === 1 ? '' : 's'} missing on ${last.files} file${last.files === 1 ? '' : 's'} · scanned ${ago(last.ran_at)}`
  }
  const when = status.schedule.enabled
    ? `every ${INTERVALS.find((i) => i.minutes === status.schedule.interval_minutes)?.label.toLowerCase() || `${status.schedule.interval_minutes} min`}`
    : 'off'
  return last ? `All ${last.parts} parts in place · scanned ${ago(last.ran_at)} · ${when}` : `Scans ${when} · not scanned yet`
}

export function scanStatusTone(status) {
  if (status?.last?.lost > 0) return COLORS.warn
  return undefined
}

export default function PartScan({ zIndex = 130, onClose, onChanged }) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [saving, setSaving] = useState(false)
  const [scanning, setScanning] = useState(false)

  const load = () => api.partScanStatus().then(setStatus).catch((err) => setError(err.message))
  useEffect(() => { load() }, [])

  const setSchedule = async (change) => {
    setSaving(true)
    setError(null)
    try {
      setStatus(await api.setPartScanSchedule(change))
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  const scanNow = async () => {
    setScanning(true)
    setError(null)
    try {
      setStatus(await api.scanLocalDisks())
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setScanning(false)
    }
  }

  const schedule = status?.schedule || { enabled: true, interval_minutes: 24 * 60 }
  const last = status?.last
  const none = status && status.local_accounts === 0

  return (
    <Modal
      title="Local disks"
      subtitle="Each disk in this machine listed against the index, so a part that is gone or truncated is found before a read needs it"
      onClose={onClose}
      zIndex={zIndex}
      width={520}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {!status && !error && <div style={{ display: 'flex', justifyContent: 'center', padding: '20px' }}><Spinner /></div>}

      {none && (
        <Banner tone="info">
          No connected account is a folder on this machine, so there is nothing to scan. A cloud's
          parts are checked by a folder automation instead.
        </Banner>
      )}

      {status && !none && (
        <>
          <div style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
            <div style={{ flex: 1, minWidth: '200px', fontFamily: FONT.sans, fontSize: '12px', lineHeight: 1.55, color: COLORS.text }}>
              {last ? (
                <>
                  <b style={{ fontWeight: 600 }}>
                    {last.lost > 0
                      ? `${last.lost} part${last.lost === 1 ? '' : 's'} missing or truncated across ${last.files} file${last.files === 1 ? '' : 's'}.`
                      : `All ${last.parts} part${last.parts === 1 ? '' : 's'} where the index says.`}
                  </b>
                  <span style={{ display: 'block', color: COLORS.textDim }}>
                    Scanned {ago(last.ran_at)}
                    {last.recovered > 0 ? ` · ${last.recovered} part${last.recovered === 1 ? '' : 's'} back since the last look` : ''}
                    {last.unlisted > 0 ? ` · ${last.unlisted} disk${last.unlisted === 1 ? '' : 's'} could not be listed` : ''}
                    {last.lost > 0 ? ' · see Files missing a part' : ''}
                  </span>
                </>
              ) : (
                <span style={{ color: COLORS.textDim }}>
                  Not scanned yet{status.next_at ? ' — the first scan runs as soon as the vault is open under the server' : ''}.
                </span>
              )}
            </div>
            <Button disabled={scanning || status.running} onClick={scanNow} style={{ justifyContent: 'center' }}>
              {scanning || status.running ? <><Spinner size={11} /> Scanning…</> : 'Scan now'}
            </Button>
          </div>

          {last && last.accounts?.length > 0 && (
            <div style={{ marginTop: '12px', border: `1px solid ${COLORS.border}`, borderRadius: '6px', overflow: 'hidden' }}>
              {last.accounts.map((a, i) => (
                <div
                  key={a.id}
                  style={{
                    display: 'flex', alignItems: 'center', gap: '10px', padding: '7px 10px',
                    borderTop: i === 0 ? 'none' : `1px solid ${COLORS.border}`,
                    fontFamily: FONT.mono, fontSize: '11px',
                  }}
                >
                  <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: COLORS.text }}>{a.name}</span>
                  <span style={{ color: a.error ? COLORS.error : a.lost > 0 ? COLORS.warn : COLORS.textMuted, whiteSpace: 'nowrap' }}>
                    {a.error
                      ? 'could not be listed'
                      : a.lost > 0
                        ? `${a.lost} of ${a.parts} part${a.parts === 1 ? '' : 's'} missing`
                        : `${a.parts} part${a.parts === 1 ? '' : 's'} in place`}
                  </span>
                </div>
              ))}
            </div>
          )}

          <div style={{ marginTop: '18px', paddingTop: '14px', borderTop: `1px solid ${COLORS.border}` }}>
            <div style={{ fontFamily: FONT.mono, fontSize: '10px', fontWeight: 700, letterSpacing: '1.2px', textTransform: 'uppercase', color: COLORS.textMuted, marginBottom: '10px' }}>
              How often
            </div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px' }} role="radiogroup" aria-label="How often the local disks are scanned">
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
            <p style={{ margin: '10px 0 0', fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.6, color: COLORS.textMuted }}>
              A scan is one directory listing per disk and reads no part. It runs only while the
              vault is unlocked under the server; a slot that passes while it is locked comes round
              as soon as it is opened again. A missing part is reported, not repaired: a folder
              automation set to fix rebuilds the file on its own, or choose clouds for the file in
              Files missing a part.
            </p>
          </div>
        </>
      )}

      <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '16px' }}>
        <Button variant="primary" onClick={onClose} style={{ justifyContent: 'center' }}>Done</Button>
      </div>
    </Modal>
  )
}

/* The same chip the cloud health schedule uses, drawn here so neither panel
   owns the other. */
function Choice({ label, on, tone, disabled, onClick }) {
  const [hover, setHover] = useState(false)
  return (
    <button
      type="button"
      role="radio"
      aria-checked={on}
      onClick={onClick}
      disabled={disabled}
      onPointerEnter={(e) => { if (e.pointerType === 'mouse') setHover(true) }}
      onPointerLeave={() => setHover(false)}
      style={{
        minHeight: '34px', padding: '7px 12px',
        background: on ? COLORS.surfaceHover : COLORS.surfaceRaised,
        border: `1px solid ${on ? COLORS.accent : (hover ? COLORS.borderBright : COLORS.border)}`,
        borderRadius: '6px', cursor: disabled ? 'default' : 'pointer', opacity: disabled ? 0.6 : 1,
        fontFamily: FONT.mono, fontSize: '11px', fontWeight: on ? 600 : 400,
        color: on ? COLORS.accentBright : (tone || COLORS.textDim),
      }}
    >{label}</button>
  )
}
