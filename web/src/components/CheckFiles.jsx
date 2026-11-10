import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, LoadProgress, Modal, Spinner } from './ui'
import { formatElapsed } from './ScanProgress'

/* Find the files that do not come back right, and fix the ones that can be.

   Everything else in the app that asks after a file's health asks whether its
   parts are there. This asks whether they are right: the server fetches every
   shard of every file under the folder, opens each one, rebuilds the file,
   holds it to the fingerprint taken when it was stored, and then looks at the
   result as a picture or a document or a film (internal/vault/verify.go).

   That is a download of everything chosen, so the dialog leads with the bill —
   how many files of each kind and how much they weigh — and runs as a job on
   the server: closing this stops nothing, and reopening it finds the check
   where it has got to or the report it left.

   What it finds comes in four sorts, and the difference between them is the
   whole value of the report:

     · can be repaired   — some shards are wrong but the file still reads.
                           Rebuilding writes a full fresh set.
     · from the replica  — the parts cannot give it back, the replica can.
     · cannot be read    — neither can. The report says which clouds hold what.
     · arrived damaged   — SAND returns exactly what it was given, and what it
                           was given is a broken file. Only the original helps.

   Nothing is changed by checking. Repair is its own button, over exactly the
   files listed. */

const KINDS = {
  images: 'Pictures',
  documents: 'Documents',
  video: 'Video',
  audio: 'Audio',
  archives: 'Archives',
  other: 'Everything else',
}

/* Pictures, documents and video are where a file can be looked at as what it
   is; they are ticked to begin with. */
const DEFAULT_KINDS = ['images', 'documents', 'video']

const STATES = {
  unreadable: { label: 'Cannot be read', tone: COLORS.error },
  altered: { label: 'Does not match what was stored', tone: COLORS.error },
  repairable: { label: 'Reads, with bad parts', tone: COLORS.warn },
  damaged_at_source: { label: 'Was damaged before it was stored', tone: COLORS.warn },
  incomplete: { label: 'Could not be fully checked', tone: COLORS.textDim },
  skipped: { label: 'Not checked', tone: COLORS.textDim },
}

const FAULTS = {
  missing: 'missing',
  damaged: 'damaged',
  mismatched: 'does not match the rest',
  unreachable: 'could not be reached',
}

const POLL_MS = 1500

export default function CheckFiles({ path, vault = '', onClose, onChanged }) {
  const [survey, setSurvey] = useState(null)
  const [kinds, setKinds] = useState(null)
  const [check, setCheck] = useState(null)
  const [repair, setRepair] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const live = useRef(true)

  const refresh = useCallback(async () => {
    const [c, r] = await Promise.all([api.checkStatus(), api.repairStatus()])
    if (!live.current) return
    setCheck(c)
    setRepair(r)
  }, [])

  useEffect(() => {
    live.current = true
    api.checkSurvey(path, vault)
      .then((s) => {
        if (!live.current) return
        setSurvey(s)
        const present = s.kinds.map((k) => k.kind)
        const preset = DEFAULT_KINDS.filter((k) => present.includes(k))
        setKinds(preset.length ? preset : present)
      })
      .catch((err) => { if (live.current) setError(err.message) })
    refresh().catch((err) => { if (live.current) setError(err.message) })
    return () => { live.current = false }
  }, [path, vault, refresh])

  /* Polled while either job runs. A finished repair has rewritten files, so
     the listing behind is told. */
  const running = !!check?.running || !!repair?.running
  const wasRepairing = useRef(false)
  useEffect(() => {
    if (wasRepairing.current && repair && !repair.running) onChanged?.()
    wasRepairing.current = !!repair?.running
  }, [repair, onChanged])
  useEffect(() => {
    if (!running) return undefined
    const timer = setInterval(() => { refresh().catch(() => {}) }, POLL_MS)
    return () => clearInterval(timer)
  }, [running, refresh])

  const act = async (fn) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
    } catch (err) {
      setError(err.message)
    }
    try { await refresh() } catch { /* the next poll will say */ }
    if (live.current) setBusy(false)
  }

  const chosen = survey && kinds
    ? survey.kinds.filter((k) => kinds.includes(k.kind))
    : []
  const files = chosen.reduce((n, k) => n + k.files, 0)
  const bytes = chosen.reduce((n, k) => n + k.bytes, 0)
  const all = survey && kinds && survey.kinds.every((k) => kinds.includes(k.kind))

  const report = !check?.running ? check?.result : null
  const repaired = !repair?.running ? repair?.result : null

  let body
  if (!survey || !check || !repair) {
    body = error ? null : <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>
  } else if (repair.running) {
    body = <RepairRunning status={repair} busy={busy} onStop={() => act(() => api.repairStop())} />
  } else if (check.running) {
    body = <CheckRunning status={check} busy={busy} onStop={() => act(() => api.checkStop())} />
  } else if (report) {
    body = (
      <Report
        report={report}
        busy={busy}
        onRepair={(ids) => act(() => api.repairStart(ids))}
        onAgain={() => setCheck({ running: false })}
      />
    )
  } else {
    body = (
      <>
        {repaired && <RepairOutcome report={repaired} />}
        <p style={para}>
          Every part of every file chosen is fetched from its cloud and opened, the file is rebuilt and
          held to the fingerprint taken when it was stored, and the result is looked at as the kind of
          file it is. Nothing is changed; what can be repaired is offered afterwards.
        </p>
        <div style={{ display: 'flex', flexDirection: 'column', gap: '2px', margin: '14px 0' }}>
          {survey.kinds.map((k) => (
            <label key={k.kind} style={kindRow}>
              <input
                type="checkbox"
                checked={kinds.includes(k.kind)}
                onChange={(e) => setKinds(e.target.checked
                  ? [...kinds, k.kind]
                  : kinds.filter((x) => x !== k.kind))}
              />
              <span style={{ flex: 1 }}>{KINDS[k.kind] || k.kind}</span>
              <span style={figure}>{k.files.toLocaleString()} file{k.files === 1 ? '' : 's'}</span>
              <span style={{ ...figure, width: '84px', textAlign: 'right' }}>{formatBytes(k.bytes)}</span>
            </label>
          ))}
          {survey.kinds.length === 0 && <p style={para}>There are no files under this folder.</p>}
        </div>
        {files > 0 && (
          <p style={{ ...para, color: COLORS.textMuted }}>
            This reads about {formatBytes(Math.round(bytes * 1.6))} from your clouds — every part, not only the
            ones a download needs — and keeps going if you close this.
          </p>
        )}
        <div style={footer}>
          <Button variant="ghost" onClick={onClose}>Close</Button>
          <Button
            variant="primary"
            disabled={busy || files === 0}
            onClick={() => act(() => api.checkStart(path, vault, all ? [] : kinds))}
          >Check {files.toLocaleString()} file{files === 1 ? '' : 's'}</Button>
        </div>
      </>
    )
  }

  return (
    <Modal title="Find and fix corrupt files" subtitle={path} onClose={onClose} width={640}>
      {error && <div style={{ marginBottom: '12px' }}><Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner></div>}
      {body}
    </Modal>
  )
}

function useElapsed(startedAt) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const started = startedAt ? Date.parse(startedAt) : NaN
  return Number.isFinite(started) ? formatElapsed(now - started) : ''
}

function CheckRunning({ status, busy, onStop }) {
  const at = status.progress || {}
  const elapsed = useElapsed(status.started_at)
  const flagged = at.flagged > 0 ? ` · ${at.flagged} flagged so far` : ''
  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'center', padding: '18px 2px 8px' }}>
        <LoadProgress
          label={at.files
            ? `Checking ${at.file} of ${at.files} · ${formatBytes(at.bytes || 0)} of ${formatBytes(at.total || 0)}${flagged}`
            : 'Asking every cloud whether it is there…'}
          detail={[at.path, elapsed].filter(Boolean).join(' · ')}
          fraction={at.total > 0 ? at.bytes / at.total : null}
          width={520}
        />
      </div>
      <p style={{ ...para, textAlign: 'center', color: COLORS.textMuted }}>
        This runs on the machine SAND is on. You can close this and come back to it.
      </p>
      <div style={footer}>
        <Button variant="ghost" disabled={busy} onClick={onStop}>Stop and show what was found</Button>
      </div>
    </>
  )
}

function RepairRunning({ status, busy, onStop }) {
  const at = status.progress || {}
  const elapsed = useElapsed(status.started_at)
  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'center', padding: '18px 2px 8px' }}>
        <LoadProgress
          label={at.files ? `Rebuilding ${at.file} of ${at.files}` : 'Asking every cloud whether it is there…'}
          detail={[at.path, elapsed].filter(Boolean).join(' · ')}
          fraction={at.files > 0 ? at.done / at.files : null}
          width={520}
        />
      </div>
      <p style={{ ...para, textAlign: 'center', color: COLORS.textMuted }}>
        Each file is read from its good parts, written out as a full fresh set, and only then are the
        old parts erased. You can close this and come back to it.
      </p>
      <div style={footer}>
        <Button variant="ghost" disabled={busy} onClick={onStop}>Stop after this file</Button>
      </div>
    </>
  )
}

function RepairOutcome({ report }) {
  const failed = report.files.filter((f) => !f.repaired)
  return (
    <div style={{ marginBottom: '14px' }}>
      <Banner tone={failed.length ? 'warn' : 'success'}>
        {report.repaired} file{report.repaired === 1 ? '' : 's'} rebuilt
        {report.failed ? `, ${report.failed} could not be` : ''}
        {report.deferred ? `, ${report.deferred} left as they were` : ''}
        {report.stopped ? ' — stopped before the end' : ''}.
        {' '}Check again to confirm.
      </Banner>
      {failed.map((f) => (
        <div key={f.id} style={{ ...detail, marginTop: '6px' }}>{f.path}: {f.reason}</div>
      ))}
    </div>
  )
}

function Report({ report, busy, onRepair, onAgain }) {
  const fixable = report.flagged.filter((f) => f.fix)
  const tally = [
    [report.sound, 'sound'],
    [report.repairable, 'can be repaired'],
    [report.unreadable, 'cannot be read'],
    [report.altered, 'do not match what was stored'],
    [report.damaged_at_source, 'arrived damaged'],
    [report.incomplete, 'not fully checked'],
    [report.skipped, 'skipped'],
  ].filter(([n]) => n > 0).map(([n, what]) => `${n.toLocaleString()} ${what}`).join(' · ')

  return (
    <>
      <p style={{ ...para, color: COLORS.text }}>
        {report.checked.toLocaleString()} of {report.files.toLocaleString()} file{report.files === 1 ? '' : 's'} checked
        {' '}({formatBytes(report.bytes)}){report.stopped ? ', stopped before the end' : ''}.
      </p>
      <p style={{ ...para, fontFamily: FONT.mono, fontSize: '12px' }}>{tally || 'Nothing was checked.'}</p>

      {report.offline?.length > 0 && (
        <div style={{ margin: '10px 0' }}>
          <Banner tone="warn">
            {report.offline.join(', ')} did not answer, so the parts there were not looked at.
          </Banner>
        </div>
      )}
      {report.flagged.length === 0 && report.checked > 0 && (
        <div style={{ margin: '12px 0' }}>
          <Banner tone="success">
            Every file checked is intact: each part opens, and each file matches the fingerprint taken when it was stored.
          </Banner>
        </div>
      )}

      <div style={{ maxHeight: 'calc(var(--app-height) * 0.42)', overflowY: 'auto', margin: '12px 0' }}>
        {report.flagged.map((f) => <Finding key={f.id} file={f} />)}
        {report.truncated && <p style={para}>…and more. Fix these and check again to see the rest.</p>}
      </div>

      <div style={footer}>
        <Button variant="ghost" onClick={onAgain}>Check again</Button>
        {fixable.length > 0 && (
          <Button variant="primary" disabled={busy} onClick={() => onRepair(fixable.map((f) => f.id))}>
            Fix {fixable.length.toLocaleString()} file{fixable.length === 1 ? '' : 's'}
          </Button>
        )}
      </div>
    </>
  )
}

function Finding({ file }) {
  const state = STATES[file.state] || { label: file.state, tone: COLORS.textDim }
  return (
    <div style={{ padding: '10px 0', borderTop: `1px solid ${COLORS.border}` }}>
      <div style={{ display: 'flex', gap: '8px', alignItems: 'baseline', flexWrap: 'wrap' }}>
        <span style={{ fontFamily: FONT.mono, fontSize: '12.5px', color: COLORS.text, wordBreak: 'break-all' }}>{file.path}</span>
        <span style={{ fontFamily: FONT.mono, fontSize: '11px', color: state.tone }}>{state.label}</span>
        <span style={{ ...figure, marginLeft: 'auto' }}>
          {formatBytes(file.size)} · {file.good} of {file.total} parts good, {file.needed} needed
        </span>
      </div>
      <div style={detail}>{file.summary}</div>
      {(file.faults || []).map((fault) => (
        <div key={`${fault.part}-${fault.provider_id}`} style={{ ...detail, fontFamily: FONT.mono, fontSize: '11.5px' }}>
          ✗ part {fault.part} on {fault.provider_name} — {FAULTS[fault.kind] || fault.kind}
          {fault.chunks > 1 ? ` in ${fault.chunks} chunks` : ''}
          {fault.detail ? `: ${fault.detail.replace(/^the stored object is damaged: /, '')}` : ''}
        </div>
      ))}
      {file.advice && <div style={{ ...detail, color: COLORS.textDim }}>→ {file.advice}</div>}
    </div>
  )
}

/* What a deep check of one file came to, in the words the preview uses when
   a picture will not draw.

   The preview only asks once the browser has refused the bytes, so a file the
   check finds intact leaves the browser as the answer — and that is most of
   what this is ever asked: a photograph taken as HEIC opens in Safari and in
   nothing else, and "could not be rebuilt" was a wrong thing to say about a
   file that had just been rebuilt perfectly. */
const DRAWN_EVERYWHERE = new Set(['JPEG', 'PNG', 'GIF', 'WebP', 'BMP'])

export function describeCheck(check) {
  const format = check.content?.format
  const intact = check.state === 'sound' || check.state === 'incomplete' || check.state === 'repairable'
  if (!intact) {
    return { tone: 'error', text: `${check.summary}${check.advice ? ` ${check.advice}` : ''}` }
  }
  const verified = 'This file is intact: every part was fetched, and it matches the fingerprint taken when it was stored.'
  const browser = !format || DRAWN_EVERYWHERE.has(format)
    ? 'This browser could not draw it all the same — download it and open it with something else.'
    : `This browser cannot display ${format} pictures${format === 'HEIC' ? ' (Safari can)' : ''} — download it, or open it in a browser that does.`
  const parts = check.state === 'repairable'
    ? ' Some of its parts do need repair: Organize and automate → Find and fix corrupt files.'
    : ''
  return { tone: 'info', text: `${verified} ${browser}${parts}` }
}

const para = {
  margin: '0 0 8px', fontFamily: FONT.sans, fontSize: '13px', lineHeight: 1.6, color: COLORS.textDim,
}
const detail = {
  marginTop: '4px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.55,
  color: COLORS.textMuted, wordBreak: 'break-word',
}
const figure = { fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textMuted }
const kindRow = {
  display: 'flex', alignItems: 'center', gap: '10px', minHeight: '40px', padding: '0 4px',
  fontFamily: FONT.sans, fontSize: '13.5px', color: COLORS.text, cursor: 'pointer',
}
const footer = { display: 'flex', justifyContent: 'flex-end', gap: '8px', marginTop: '14px', flexWrap: 'wrap' }
