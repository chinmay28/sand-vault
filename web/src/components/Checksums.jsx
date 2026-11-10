import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, LoadProgress, Modal, Spinner } from './ui'
import { formatElapsed } from './ScanProgress'

/* Checksums: two jobs over a folder and everything under it, in one dialog.

   Every file has a checksum — the SHA-256 of its contents, taken as it is
   stored — and checksums.gz at the root of the vault lists them all.

     · verify  — read every file and hold it to its checksum. The quick
                 corruption check: it reads what a download reads, and looks
                 at nothing but the hash. "Find and fix corrupt files" is the
                 slow one that says which part on which cloud is wrong.
     · compute — read the files that have no checksum and record one. Nearly
                 every file has one from the moment it is stored; the ones
                 that do not were recovered without their index, or stored
                 long ago.

   Both are long reads of your clouds, so both run on the server — closing
   this stops nothing — and both are paced: a speed chosen before starting and
   changeable while it runs, a pause that keeps its place, and a stop that
   keeps what was done. See internal/vault/checksums.go. */

const MB = 1024 * 1024

/* Speeds on offer, in bytes a second. Zero is as fast as the clouds answer. */
const SPEEDS = [
  { rate: 0, label: 'Unlimited' },
  { rate: 50 * MB, label: '50 MB/s' },
  { rate: 20 * MB, label: '20 MB/s' },
  { rate: 10 * MB, label: '10 MB/s' },
  { rate: 5 * MB, label: '5 MB/s' },
  { rate: 2 * MB, label: '2 MB/s' },
  { rate: 1 * MB, label: '1 MB/s' },
  { rate: MB / 2, label: '512 KB/s' },
]

const TEXT = {
  verify: {
    title: 'Check files against their checksums',
    lead: 'Every file under this folder is read back from its clouds, as a download would read it, and held to the checksum taken when it was stored. Nothing is changed.',
    start: (n) => `Check ${n.toLocaleString()} file${n === 1 ? '' : 's'}`,
    verb: 'Checking',
  },
  compute: {
    title: 'Calculate missing checksums',
    lead: 'Every file under this folder that has no checksum is read back from its clouds and what it hashes to is recorded — in the index, and in checksums.gz at the root of the vault.',
    start: (n) => `Calculate ${n.toLocaleString()} checksum${n === 1 ? '' : 's'}`,
    verb: 'Reading',
  },
}

const STATES = {
  mismatch: { label: 'Does not match its checksum', tone: COLORS.error },
  unreadable: { label: 'Could not be read', tone: COLORS.error },
  skipped: { label: 'Not read', tone: COLORS.textDim },
}

const POLL_MS = 1200

export default function Checksums({ kind, path, vault = '', onClose, onChanged }) {
  const text = TEXT[kind]
  const [survey, setSurvey] = useState(null)
  const [status, setStatus] = useState(null)
  const [rate, setRate] = useState(0)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  /* A report on screen is the last run's, of either kind and any folder; one
     that is not this dialog's is not shown as its answer. */
  const [dismissed, setDismissed] = useState(false)
  const live = useRef(true)

  const loadSurvey = useCallback(() => api.checksumSurvey(path, vault)
    .then((s) => { if (live.current) setSurvey(s) }), [path, vault])
  const refresh = useCallback(() => api.checksumStatus()
    .then((s) => { if (live.current) setStatus(s) }), [])

  useEffect(() => {
    live.current = true
    Promise.all([loadSurvey(), refresh()]).catch((err) => { if (live.current) setError(err.message) })
    return () => { live.current = false }
  }, [loadSurvey, refresh])

  const running = !!status?.running
  useEffect(() => {
    if (!running) return undefined
    const timer = setInterval(() => { refresh().catch(() => {}) }, POLL_MS)
    return () => clearInterval(timer)
  }, [running, refresh])

  /* When a run ends, what it left behind is news to two things: the survey
     here, and — for a compute, which wrote checksums.gz — the listing. */
  const wasRunning = useRef(false)
  useEffect(() => {
    if (wasRunning.current && !running) {
      loadSurvey().catch(() => {})
      if (status?.result?.kind === 'compute') onChanged?.()
    }
    wasRunning.current = running
  }, [running, status, loadSurvey, onChanged])

  const act = async (fn) => {
    setBusy(true)
    setError(null)
    try {
      const next = await fn()
      if (live.current && next) setStatus(next)
    } catch (err) {
      if (live.current) setError(err.message)
      refresh().catch(() => {})
    }
    if (live.current) setBusy(false)
  }

  const mine = (s) => s && s.kind === kind && s.path === path && (s.vault || '') === vault
  /* One job at a time, of either kind: a dialog opened while the other kind
     runs shows that run, and says it is not the one that was asked for. */
  const theirs = running && !!status.progress?.kind && status.progress.kind !== kind
  const report = !running && !dismissed && mine(status?.result) ? status.result : null

  const files = survey ? (kind === 'verify' ? survey.with : survey.without) : 0
  const bytes = survey ? (kind === 'verify' ? survey.with_bytes : survey.without_bytes) : 0

  let body
  if (!survey || !status) {
    body = error ? null : <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>
  } else if (running) {
    body = (
      <Running
        status={status}
        text={TEXT[status.progress?.kind] || text}
        other={theirs}
        busy={busy}
        onPause={() => act(() => api.checksumPause())}
        onResume={() => act(() => api.checksumResume())}
        onRate={(r) => act(() => api.checksumRate(r))}
        onStop={() => act(() => api.checksumStop())}
        onClose={onClose}
      />
    )
  } else if (report) {
    body = <Report report={report} onAgain={() => setDismissed(true)} onClose={onClose} />
  } else {
    body = (
      <>
        <p style={para}>{text.lead}</p>
        <div style={figures}>
          {kind === 'verify' ? (
            <>
              <Figure n={survey.with} what="with a checksum" bytes={survey.with_bytes} />
              {survey.without > 0 && <Figure n={survey.without} what="with none — not checked" bytes={survey.without_bytes} dim />}
            </>
          ) : (
            <>
              <Figure n={survey.without} what="with no checksum" bytes={survey.without_bytes} />
              <Figure n={survey.with} what="already have one" bytes={survey.with_bytes} dim />
            </>
          )}
        </div>
        {kind === 'compute' && survey.without === 0 && survey.files > 0 && (
          <Banner tone="success">Every file under this folder already has a checksum.</Banner>
        )}
        {kind === 'verify' && survey.without > 0 && (
          <p style={{ ...para, color: COLORS.textMuted }}>
            Files with no checksum cannot be checked this way. “Calculate missing checksums”, in the same menu, gives them one.
          </p>
        )}
        <Ledger ledger={survey.ledger} />

        <label style={speedRow}>
          <span>Speed</span>
          <SpeedSelect value={rate} onChange={setRate} />
        </label>
        {files > 0 && (
          <p style={{ ...para, color: COLORS.textMuted }}>
            {formatBytes(bytes)} to read{rate > 0 ? ` — about ${formatElapsed((bytes / rate) * 1000)} at this speed` : ''}.
            It runs in the background: you can close this, and pause, slow or stop it from here at any time.
          </p>
        )}
        <div style={footer}>
          <Button variant="ghost" onClick={onClose}>Close</Button>
          <Button
            variant="primary"
            disabled={busy || files === 0}
            onClick={() => { setDismissed(false); act(() => api.checksumStart(kind, path, vault, rate)) }}
          >{text.start(files)}</Button>
        </div>
      </>
    )
  }

  return (
    <Modal title={text.title} subtitle={path} onClose={onClose} width={600}>
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {body}
    </Modal>
  )
}

function Figure({ n, what, bytes, dim }) {
  return (
    <div style={{ display: 'flex', gap: '10px', alignItems: 'baseline', opacity: dim ? 0.7 : 1 }}>
      <span style={{ fontFamily: FONT.mono, fontSize: '13px', color: COLORS.text, minWidth: '64px' }}>
        {n.toLocaleString()} file{n === 1 ? '' : 's'}
      </span>
      <span style={{ flex: 1, fontFamily: FONT.sans, fontSize: '13px', color: COLORS.textDim }}>{what}</span>
      <span style={mono}>{formatBytes(bytes)}</span>
    </div>
  )
}

/* The ledger, in a line: that it is a real file in the vault, how many files
   it lists, and how fresh it is. */
function Ledger({ ledger }) {
  if (!ledger) return null
  const when = ledger.written_at ? new Date(ledger.written_at).toLocaleString() : ''
  return (
    <p style={{ ...para, color: COLORS.textMuted, fontSize: '12px' }}>
      {ledger.stored
        ? `${ledger.path} at the root of the vault lists every file with a checksum — a path and a SHA-256 a line, gzipped (${formatBytes(ledger.size)}, written ${when}). It is kept up to date as the vault changes.`
        : `${ledger.path} will be written at the root of the vault, on its default clouds, once there is a checksum to list.`}
    </p>
  )
}

function SpeedSelect({ value, onChange, disabled }) {
  /* A rate set some other way — the command line, another browser — is
     still shown as what it is. */
  const known = SPEEDS.some((s) => s.rate === value)
  return (
    <select
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(Number(e.target.value))}
      aria-label="Speed"
      style={{
        minHeight: '38px', padding: '0 10px', borderRadius: '6px',
        background: COLORS.bg, color: COLORS.text, border: `1px solid ${COLORS.border}`,
        fontFamily: FONT.mono, fontSize: '12.5px',
      }}
    >
      {!known && <option value={value}>{formatBytes(value)}/s</option>}
      {SPEEDS.map((s) => <option key={s.rate} value={s.rate}>{s.label}</option>)}
    </select>
  )
}

function Running({ status, text, other, busy, onPause, onResume, onRate, onStop, onClose }) {
  const at = status.progress || {}
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const started = status.started_at ? Date.parse(status.started_at) : NaN
  const elapsed = Number.isFinite(started) ? formatElapsed(now - started) : ''
  const flagged = at.flagged > 0 ? ` · ${at.flagged} flagged` : ''

  return (
    <>
      {other && (
        <Banner tone="info">A different checksum job is already running — one at a time. This is that one.</Banner>
      )}
      <div style={{ display: 'flex', justifyContent: 'center', padding: '16px 2px 8px' }}>
        <LoadProgress
          label={status.paused
            ? `Paused at ${at.file || 0} of ${at.files || 0} · ${formatBytes(at.bytes || 0)} of ${formatBytes(at.total || 0)}${flagged}`
            : at.files
              ? `${text.verb} ${at.file} of ${at.files} · ${formatBytes(at.bytes || 0)} of ${formatBytes(at.total || 0)}${flagged}`
              : 'Starting…'}
          detail={[at.path, elapsed].filter(Boolean).join(' · ')}
          fraction={at.total > 0 ? at.bytes / at.total : null}
          tone={status.paused ? 'warn' : 'info'}
          width={500}
        />
      </div>
      <label style={speedRow}>
        <span>Speed</span>
        <SpeedSelect value={status.rate || 0} onChange={onRate} disabled={busy} />
      </label>
      <p style={{ ...para, textAlign: 'center', color: COLORS.textMuted }}>
        {status.paused
          ? 'Held where it is. Nothing is being read; resume picks up at the same file. A paused job does not keep the vault from locking itself when idle, and locking ends it.'
          : 'Running on the machine SAND is on. You can close this and come back to it.'}
      </p>
      <div style={footer}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        <Button variant="ghost" disabled={busy} onClick={onStop}>Stop</Button>
        {status.paused
          ? <Button variant="primary" disabled={busy} onClick={onResume}>Resume</Button>
          : <Button disabled={busy} onClick={onPause}>Pause</Button>}
      </div>
    </>
  )
}

function Report({ report, onAgain, onClose }) {
  const verify = report.kind === 'verify'
  const tally = (verify
    ? [[report.matched, 'match'], [report.mismatched, 'do not match'], [report.unreadable, 'could not be read'], [report.skipped, 'not read']]
    : [[report.computed, 'recorded'], [report.unreadable, 'could not be read'], [report.skipped, 'not read']]
  ).filter(([n]) => n > 0).map(([n, what]) => `${n.toLocaleString()} ${what}`).join(' · ')
  const clean = report.findings.length === 0 && report.checked > 0 && !report.stopped

  return (
    <>
      <p style={{ ...para, color: COLORS.text }}>
        {report.checked.toLocaleString()} of {report.files.toLocaleString()} file{report.files === 1 ? '' : 's'} read
        {' '}({formatBytes(report.bytes)}){report.stopped ? ', stopped before the end' : ''}.
      </p>
      <p style={{ ...para, ...mono, fontSize: '12px' }}>{tally || 'Nothing was read.'}</p>

      {clean && (
        <Banner tone="success">
          {verify
            ? 'Every file read back to exactly the checksum taken when it was stored.'
            : 'Every file that had no checksum has one now.'}
        </Banner>
      )}
      {verify && report.no_checksum > 0 && (
        <Banner tone="info">
          {report.no_checksum.toLocaleString()} file{report.no_checksum === 1 ? ' has' : 's have'} no checksum and could not be checked —
          “Calculate missing checksums” gives {report.no_checksum === 1 ? 'it' : 'them'} one.
        </Banner>
      )}
      {report.ledger_error && <Banner tone="warn">checksums.gz could not be written: {report.ledger_error}</Banner>}
      {report.ledger?.written && (
        <p style={{ ...para, color: COLORS.textMuted }}>
          {report.ledger.path} was rewritten: {report.ledger.entries.toLocaleString()} file{report.ledger.entries === 1 ? '' : 's'} listed.
        </p>
      )}

      <div style={{ maxHeight: 'calc(var(--app-height) * 0.4)', overflowY: 'auto', margin: '10px 0' }}>
        {report.findings.map((f) => {
          const state = STATES[f.state] || { label: f.state, tone: COLORS.textDim }
          return (
            <div key={f.id} style={{ padding: '9px 0', borderTop: `1px solid ${COLORS.border}` }}>
              <div style={{ display: 'flex', gap: '8px', alignItems: 'baseline', flexWrap: 'wrap' }}>
                <span style={{ ...mono, fontSize: '12.5px', color: COLORS.text, wordBreak: 'break-all' }}>{f.path}</span>
                <span style={{ ...mono, fontSize: '11px', color: state.tone }}>{state.label}</span>
                <span style={{ ...mono, marginLeft: 'auto' }}>{formatBytes(f.size)}</span>
              </div>
              <div style={detail}>{f.detail}</div>
              {f.expected && (
                <div style={{ ...detail, ...mono, fontSize: '11px', wordBreak: 'break-all' }}>
                  on record {f.expected}<br />read back {f.got}
                </div>
              )}
            </div>
          )
        })}
        {report.truncated && <p style={para}>…and more than can be listed here.</p>}
      </div>

      <div style={footer}>
        <Button variant="ghost" onClick={onAgain}>Run again</Button>
        <Button variant="primary" onClick={onClose}>Done</Button>
      </div>
    </>
  )
}

const para = {
  margin: '0 0 10px', fontFamily: FONT.sans, fontSize: '13px', lineHeight: 1.6, color: COLORS.textDim,
}
const mono = { fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textMuted }
const detail = {
  marginTop: '4px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.55,
  color: COLORS.textMuted, wordBreak: 'break-word',
}
const figures = { display: 'flex', flexDirection: 'column', gap: '6px', margin: '4px 0 14px' }
const speedRow = {
  display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px', margin: '12px 0',
  fontFamily: FONT.sans, fontSize: '13px', color: COLORS.text,
}
const footer = { display: 'flex', justifyContent: 'flex-end', gap: '8px', marginTop: '14px', flexWrap: 'wrap' }
