import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Modal, PasswordInput, Spinner } from './ui'
import RekeyProgress, {
  DEFAULT_PACE, FolderQueue, GB, KeepUnlockedTick, PaceChooser, StatePill,
  humanDuration, useRekeyStatus, useTopFolders,
} from './Rekey'

/* Changing the password is not a metadata edit. The parts sitting on your cloud
   accounts are encrypted under a random key held inside the vault, not under
   the password, so a new password that left that key alone would change
   nothing about what an old password can still read. Instead a new key is
   generated and every stored file is rebuilt onto it — which is a download and
   an upload per file, and why this dialog talks about time rather than just
   asking for two strings.

   On a large vault that time is the problem rather than the inconvenience: a
   terabyte moved as fast as the clouds take it is a day of saturated upload on
   every account. So the dialog asks how fast, and a paced change leaves the
   files to a plan the server works through while the vault is open. See
   Rekey.jsx for the plan and everything that watches it.

   And it asks one change at a time. While files are still on the key the last
   change retired, the vault refuses another — two keys is what the file list,
   the panel and a person can keep straight — and the dialog shows where that
   change has got to instead of a form. */

/* Below this, "all at once" is the sensible default: the vault moves in the
   time it takes to make a coffee. Above it, pacing is. */
const PACE_ABOVE = 20 * GB

export default function ChangePassword({ stats, onClose, onChanged, zIndex }) {
  const mobile = useIsMobile()
  const files = stats?.files || 0
  const bytes = stats?.bytes || 0
  const blocked = (stats?.pending_migration || 0) > 0

  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [pace, setPace] = useState(() => ({ paced: bytes > PACE_ABOVE, perDay: DEFAULT_PACE }))
  const [queued, setQueued] = useState([])
  const [rest, setRest] = useState(true)
  const [keepUnlocked, setKeepUnlocked] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)
  const [progress, setProgress] = useState(false)

  // The folder queue is a desktop affordance: on a phone the dialog asks only
  // what has to be answered before the change, and the queue waits in the
  // panel, which opens from the report.
  const folders = useTopFolders(pace.paced && !mobile && !blocked && !report)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)

    if (!current || !next) return
    if (next !== confirm) {
      setError('The two new passwords do not match.')
      return
    }
    if (next === current) {
      setError('That is the password you already have.')
      return
    }

    const plan = pace.paced && files > 0 ? {
      bytes_per_day: pace.perDay,
      folders: queued,
      only_folders: queued.length > 0 && !rest,
      keep_unlocked: keepUnlocked,
    } : null

    setBusy(true)
    try {
      const result = await api.changePassword(current, next, { migrate: !plan, plan })
      setCurrent('')
      setNext('')
      setConfirm('')
      setReport({ ...result, plan })
      onChanged()
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD' ? 'That is not your current password.' : err.message)
    } finally {
      setBusy(false)
    }
  }

  if (blocked && !report) {
    return (
      <Blocked
        stats={stats}
        zIndex={zIndex}
        onClose={onClose}
        onChanged={onChanged}
      />
    )
  }

  if (report) {
    const plan = report.plan
    return (
      <Modal title="Password changed" onClose={onClose} zIndex={zIndex}>
        <Banner tone="success">
          Your vault now opens with the new password, and the old one opens nothing.
        </Banner>

        {plan ? (
          <p style={paragraph}>
            {report.remaining} file{report.remaining === 1 ? '' : 's'} ({formatBytes(bytes)}) will be
            re-encrypted at {formatBytes(plan.bytes_per_day)} a day while the vault is open,
            which takes {humanDuration((bytes / plan.bytes_per_day) * 86400)}.
            {plan.folders.length > 0 && ` ${plan.folders.join(', ')} first.`}
            {' '}Until a file has moved it stays readable, and marked, on the previous key.
          </p>
        ) : (
          <p style={paragraph}>
            {report.migrated > 0
              ? `${report.migrated} file${report.migrated === 1 ? '' : 's'} re-encrypted under the new key (${formatBytes(report.bytes)}). The parts the old key opened have been erased from your accounts.`
              : 'No files needed re-encrypting.'}
          </p>
        )}

        {!plan && report.remaining > 0 && (
          <Banner tone="warn">
            {report.remaining} file{report.remaining === 1 ? ' is' : 's are'} still on the previous
            key. Until they move, someone with your old password and an old copy of the vault
            file could still read their parts — finish it from the accounts panel whenever the
            accounts holding them are reachable.
          </Banner>
        )}

        {report.warnings?.length > 0 && (
          <Banner tone="warn">
            <span style={{ fontFamily: FONT.mono, fontSize: '11px', whiteSpace: 'pre-wrap' }}>
              {report.warnings.join('\n')}
            </span>
          </Banner>
        )}

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px' }}>
          {plan && <Button onClick={() => setProgress(true)}>Open progress</Button>}
          <Button variant="primary" onClick={onClose}>Done</Button>
        </div>
        {progress && <RekeyProgress onClose={() => setProgress(false)} onChanged={onChanged} zIndex={(zIndex || 100) + 10} />}
      </Modal>
    )
  }

  return (
    <Modal
      title="Change vault password"
      subtitle="Your files are encrypted under a key inside the vault, not under your password — so changing it means re-encrypting them."
      onClose={busy ? undefined : onClose}
      zIndex={zIndex}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        <PasswordInput
          label="Current password"
          value={current}
          autoFocus
          autoComplete="current-password"
          disabled={busy}
          onChange={(e) => setCurrent(e.target.value)}
        />
        <PasswordInput
          label="New password"
          value={next}
          autoComplete="new-password"
          placeholder="Choose a strong passphrase"
          disabled={busy}
          onChange={(e) => setNext(e.target.value)}
        />
        <PasswordInput
          label="Confirm new password"
          value={confirm}
          autoComplete="new-password"
          placeholder="Type it again"
          disabled={busy}
          help="There is no recovery. If you lose this password, nothing can rebuild your files."
          onChange={(e) => setConfirm(e.target.value)}
        />

        {files > 0 && (
          <div style={{ marginTop: '16px', borderTop: `1px solid ${COLORS.border}`, paddingTop: '14px' }}>
            <PaceChooser bytes={bytes} files={files} value={pace} onChange={setPace} disabled={busy} />

            {pace.paced && !mobile && (
              <>
                <div style={sectionTitle}>These folders first</div>
                <FolderQueue
                  folders={folders}
                  queued={queued}
                  rest={rest}
                  disabled={busy}
                  onToggle={(path, on) => setQueued((q) => (on ? [...q.filter((p) => p !== path), path] : q.filter((p) => p !== path)))}
                  onRest={setRest}
                />
                <KeepUnlockedTick checked={keepUnlocked} onChange={setKeepUnlocked} disabled={busy} />
              </>
            )}
            {pace.paced && mobile && (
              <p style={{ ...paragraph, marginTop: '10px', marginBottom: 0 }}>
                Folders to do first and the unlock setting are in the progress panel, which opens once the password has changed.
              </p>
            )}
          </div>
        )}

        <div style={{ display: 'flex', flexDirection: mobile ? 'column' : 'row', justifyContent: 'flex-end', gap: '8px', marginTop: '16px' }}>
          <Button type="submit" variant="primary" disabled={busy || !current || !next} style={{ justifyContent: 'center' }}>
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy
              ? (pace.paced || files === 0 ? 'Changing…' : 'Re-encrypting…')
              : 'Change password'}
          </Button>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy} style={{ justifyContent: 'center' }}>Cancel</Button>
        </div>

        {busy && !pace.paced && files > 0 && (
          <p style={{ ...paragraph, marginBottom: 0, marginTop: '14px' }}>
            Every file is committed as it moves, so closing this tab cannot leave one
            half-encrypted — it only leaves the rest for later.
          </p>
        )}
      </form>
    </Modal>
  )
}

/* The dialog when a previous change is still moving files: why nothing can be
   typed yet, where that change has got to, and the two ways out. */
function Blocked({ stats, zIndex, onClose, onChanged }) {
  const { status } = useRekeyStatus(true)
  const [progress, setProgress] = useState(false)
  const [finishing, setFinishing] = useState(false)
  const [problem, setProblem] = useState(null)

  const since = status?.since ? new Date(status.since) : null
  const total = status?.total || stats.files
  const pending = status?.pending ?? stats.pending_migration
  const done = total ? Math.round(((total - pending) / total) * 100) : 0
  const plan = status?.plan
  const perDay = plan?.bytes_per_day || 0

  const finishNow = async () => {
    setFinishing(true)
    setProblem(null)
    try {
      const report = await api.migrate()
      if (report.warnings?.length) setProblem(report.warnings.join('\n'))
      onChanged()
    } catch (err) {
      setProblem(err.message)
    } finally {
      setFinishing(false)
    }
  }

  return (
    <Modal
      title="Change vault password"
      subtitle="A previous change is still taking effect."
      onClose={finishing ? undefined : onClose}
      zIndex={zIndex}
    >
      {problem && <Banner tone="warn" onDismiss={() => setProblem(null)}>
        <span style={{ fontFamily: FONT.mono, fontSize: '11px', whiteSpace: 'pre-wrap' }}>{problem}</span>
      </Banner>}
      <Banner tone="warn">
        The password you changed{since ? ` on ${since.toLocaleDateString(undefined, { day: 'numeric', month: 'long' })}` : ''} is in force,
        but <b>{pending} of {total} files</b> are still sealed under the key it replaced. The vault keeps
        two keys at most, the current one and the previous one, so another change has to wait until
        those files have moved.
      </Banner>

      <div role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={done} style={{
        height: '8px', borderRadius: '4px', background: COLORS.bg, border: `1px solid ${COLORS.border}`, overflow: 'hidden',
      }}>
        <div style={{ height: '100%', width: `${done}%`, background: `linear-gradient(90deg, ${COLORS.accent}, ${COLORS.accentBright})` }} />
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(96px, 1fr))', gap: '10px', margin: '12px 0 4px' }}>
        <Fig value={`${done}%`} label="re-encrypted" />
        <Fig value={status ? formatBytes(status.pending_bytes) : '…'} label="on previous key" />
        <Fig value={perDay > 0 ? formatBytes(perDay) : '—'} label={perDay > 0 ? 'a day' : 'no pace set'} />
        <Fig value={perDay > 0 && !plan?.paused && status?.eta_seconds ? humanDuration(status.eta_seconds) : '—'} label="at this pace" />
      </div>
      {status && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, marginTop: '10px' }}>
          <StatePill status={status} />
          {status.running && <Spinner size={11} />}
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {status.running ? status.current : status.next_in_seconds > 0 ? `next file in ${humanDuration(status.next_in_seconds)}` : ''}
          </span>
        </div>
      )}

      <p style={{ ...paragraph, marginTop: '12px' }}>
        Finishing now moves what is left at full speed and holds this tab open while it does.
        Adjusting the pace keeps the plan and changes how fast it runs.
      </p>

      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={onClose} disabled={finishing}>Close</Button>
        <Button onClick={() => setProgress(true)} disabled={finishing}>Adjust the pace</Button>
        <Button variant="primary" onClick={finishNow} disabled={finishing}>
          {finishing ? <Spinner size={12} color={COLORS.bg} /> : null}
          {finishing ? 'Re-encrypting…' : 'Finish now'}
        </Button>
      </div>
      {progress && <RekeyProgress onClose={() => setProgress(false)} onChanged={onChanged} zIndex={(zIndex || 100) + 10} />}
    </Modal>
  )
}

function Fig({ value, label }) {
  return (
    <div>
      <div style={{ fontFamily: FONT.mono, fontSize: '15px', fontWeight: 600, fontVariantNumeric: 'tabular-nums', color: COLORS.text }}>{value}</div>
      <div style={{ fontFamily: FONT.mono, fontSize: '9px', letterSpacing: '1px', textTransform: 'uppercase', color: COLORS.textMuted, marginTop: '2px' }}>{label}</div>
    </div>
  )
}

const paragraph = {
  margin: '0 0 16px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}

const sectionTitle = {
  fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '1px', textTransform: 'uppercase',
  color: COLORS.textDim, margin: '14px 0 9px',
}
