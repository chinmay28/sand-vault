import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, LoadProgress, Modal, PasswordInput, Spinner } from './ui'
import { formatElapsed } from './ScanProgress'

/* Snapshots: this folder and everything under it as one sealed archive, kept
   somewhere of your choosing, and brought back from there.

     · take    — pack every file and folder under here into one archive,
                 compressed as hard as zstd will and encrypted under a password
                 typed here. It goes to one connected cloud or to a machine,
                 and past the size chosen it is cut into part files.
     · restore — pick a snapshot a cloud or a machine is keeping, give its
                 password, and unpack it into this folder. The part files are
                 put back together on the way; nothing is assembled by hand.

   Two things are said in the dialog because both are the opposite of what
   the rest of SAND does:

     · A snapshot is in one place. Every file in the vault is cut into parts
       that mean nothing alone; a snapshot is the whole folder on one account,
       with the password the only thing between that account and the files.
       That is the point of it — it is readable with no vault at all — and it
       is why the password should be a long one.
     · The password is not kept. Not in the vault, not anywhere: it is asked
       for again to restore, and a snapshot whose password is forgotten is
       noise.

   Both jobs run on the server, one at a time between them — closing this
   stops nothing. Stopping a snapshot throws it away, parts and all; stopping
   a restore keeps the files that had arrived, and running it again carries
   on. See internal/vault/snapshot.go. */

const MB = 1024 * 1024
const GB = 1024 * MB
const POLL_MS = 1200

/* Sizes on offer for one part file. A cloud always has a limit, because a
   part bound for one is held in memory whole on the way; a machine takes a
   part as a stream and need not have one. */
const CLOUD_SIZES = [16 * MB, 64 * MB, 128 * MB, 256 * MB]
const MACHINE_SIZES = [
  { size: 0, label: 'No limit — one file' },
  { size: 100 * MB, label: '100 MB' },
  { size: 650 * MB, label: '650 MB — a CD' },
  { size: 1 * GB, label: '1 GB' },
  { size: 2 * GB, label: '2 GB' },
  { size: 4 * GB - MB, label: '4 GB — the most FAT32 holds' },
  { size: 25 * GB, label: '25 GB' },
]

const TEXT = {
  take: {
    title: 'Take a snapshot of this folder',
    lead: 'Every file and folder under here is packed into one archive — compressed hard, then encrypted under a password you choose — and kept on one cloud or one machine. Nothing here is changed.',
  },
  restore: {
    title: 'Restore a snapshot into this folder',
    lead: 'Pick a snapshot, give its password, and everything in it is unpacked into this folder: stored like any other upload, on this vault’s own clouds.',
  },
}

export default function Snapshots({ mode, path, vault = '', providers = [], onClose, onChanged }) {
  const text = TEXT[mode]
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  /* A report on screen is the last run's, of either kind; one that is not
     this dialog's is not shown as its answer. */
  const [dismissed, setDismissed] = useState(false)
  const live = useRef(true)

  const refresh = useCallback(() => api.snapshotStatus()
    .then((s) => { if (live.current) setStatus(s) }), [])

  useEffect(() => {
    live.current = true
    refresh().catch((err) => { if (live.current) setError(err.message) })
    return () => { live.current = false }
  }, [refresh])

  const running = !!status?.running
  useEffect(() => {
    if (!running) return undefined
    const timer = setInterval(() => { refresh().catch(() => {}) }, POLL_MS)
    return () => clearInterval(timer)
  }, [running, refresh])

  /* A restore that has ended, however it ended, has put files in the folder
     behind this dialog. */
  const wasRunning = useRef(false)
  useEffect(() => {
    if (wasRunning.current && !running && status?.result?.kind === 'restore') onChanged?.()
    wasRunning.current = running
  }, [running, status, onChanged])

  const act = async (fn) => {
    setBusy(true)
    setError(null)
    try {
      const next = await fn()
      if (live.current && next) setStatus(next)
      if (live.current) setBusy(false)
      return true
    } catch (err) {
      if (live.current) { setError(err.message); setBusy(false) }
      refresh().catch(() => {})
      return false
    }
  }
  const start = (fn) => { setDismissed(false); return act(fn) }

  /* The last run's report is this dialog's to show only if it was this job
     over this folder: a restore into /photos is not the answer to opening
     the dialog on /taxes. */
  const mine = (r) => r && r.kind === mode && r.path === path
  const report = !running && !dismissed && mine(status?.result) ? status.result : null
  const failed = !running && !dismissed && !status?.result && status?.error ? status.error : null

  let body
  if (!status) {
    body = error ? null : <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>
  } else if (running) {
    body = (
      <Running
        status={status}
        mode={mode}
        busy={busy}
        onStop={() => act(() => api.snapshotStop())}
        onClose={onClose}
      />
    )
  } else if (report) {
    body = <Report report={report} onAgain={() => setDismissed(true)} onClose={onClose} />
  } else if (mode === 'take') {
    body = (
      <TakeForm
        path={path} vault={vault} providers={providers} busy={busy} lead={text.lead}
        onError={setError} onClose={onClose}
        onStart={(req) => start(() => api.snapshotTake({ path, vault, ...req }))}
      />
    )
  } else {
    body = (
      <RestoreForm
        path={path} vault={vault} providers={providers} busy={busy} lead={text.lead}
        onError={setError} onClose={onClose}
        onStart={(req) => start(() => api.snapshotRestore({ path, vault, ...req }))}
      />
    )
  }

  return (
    <Modal title={text.title} subtitle={path} onClose={onClose} width={620}>
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {failed && <Banner tone="error" onDismiss={() => setDismissed(true)}>The last run did not finish: {failed}</Banner>}
      {body}
    </Modal>
  )
}

/* --- Where ------------------------------------------------------------- */

/* The place a snapshot is kept, as the two forms share it: which kind, which
   one of that kind, and — on a machine — which folder under its root.

   The machines are the ones imports and exports already use. There is no
   form here to add one: that lives with the transfers, where the host key
   and the login are explained, and a second copy of it would be a second
   place for it to drift. */
function usePlace(providers, onError) {
  const [kind, setKind] = useState('cloud')
  const [account, setAccount] = useState(() => providers[0]?.id || '')
  const [machines, setMachines] = useState(null)
  const [machine, setMachine] = useState('')
  const [dir, setDir] = useState('sand-snapshots')

  useEffect(() => {
    let alive = true
    api.sources()
      .then((resp) => {
        if (!alive) return
        const list = resp.sources || []
        setMachines(list)
        setMachine((cur) => cur || list[0]?.id || '')
      })
      .catch((err) => { if (alive) { setMachines([]); onError(err.message) } })
    return () => { alive = false }
  }, [onError])

  useEffect(() => {
    if (!account && providers[0]) setAccount(providers[0].id)
  }, [providers, account])

  const target = kind === 'cloud'
    ? (account ? { account } : null)
    : (machine ? { machine, dir: dir.trim() } : null)

  return { kind, setKind, account, setAccount, machines, machine, setMachine, dir, setDir, target }
}

function PlacePicker({ place, providers, verb }) {
  const { kind, setKind, account, setAccount, machines, machine, setMachine, dir, setDir } = place
  return (
    <div style={{ margin: '4px 0 14px' }}>
      <div style={label}>{verb}</div>
      <div role="radiogroup" aria-label={verb} style={{ display: 'flex', gap: '8px', marginBottom: '10px', flexWrap: 'wrap' }}>
        <Choice on={kind === 'cloud'} onClick={() => setKind('cloud')}>A connected cloud</Choice>
        <Choice on={kind === 'machine'} onClick={() => setKind('machine')}>A machine</Choice>
      </div>

      {kind === 'cloud' && (
        providers.length === 0
          ? <p style={{ ...para, color: COLORS.textMuted }}>No cloud is connected yet.</p>
          : (
            <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
              <span aria-hidden="true" style={{ width: '10px', height: '10px', borderRadius: '50%', background: accountColor(account), flex: 'none' }} />
              <select value={account} onChange={(e) => setAccount(e.target.value)} aria-label="Cloud" style={select}>
                {providers.map((p) => <option key={p.id} value={p.id}>{p.name} — {p.kind}</option>)}
              </select>
            </div>
          )
      )}

      {kind === 'machine' && (
        machines === null
          ? <Spinner size={14} />
          : machines.length === 0
            ? (
              <p style={{ ...para, color: COLORS.textMuted }}>
                No machine is set up yet. Add one from the toolbar’s “bring files in from a machine” button —
                any machine you have an SSH login on — and it will be offered here.
              </p>
            )
            : (
              <>
                <select value={machine} onChange={(e) => setMachine(e.target.value)} aria-label="Machine" style={{ ...select, marginBottom: '10px' }}>
                  {machines.map((m) => <option key={m.id} value={m.id}>{m.name} — {m.user}@{m.host}:{m.root}</option>)}
                </select>
                <Input
                  label="Folder on the machine"
                  value={dir}
                  onChange={(e) => setDir(e.target.value)}
                  placeholder="the machine’s own folder"
                  help="Under the folder this machine is scoped to. Made if it is not there."
                  autoComplete="off"
                  spellCheck={false}
                />
              </>
            )
      )}
    </div>
  )
}

function Choice({ on, onClick, children }) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={on}
      onClick={onClick}
      style={{
        minHeight: '38px', padding: '0 14px', borderRadius: '6px', cursor: 'pointer',
        fontFamily: FONT.mono, fontSize: '12px', fontWeight: 600,
        background: on ? `${COLORS.accent}22` : 'transparent',
        color: on ? COLORS.accent : COLORS.textDim,
        border: `1px solid ${on ? COLORS.accent : COLORS.border}`,
      }}
    >{children}</button>
  )
}

/* --- Take -------------------------------------------------------------- */

function TakeForm({ path, vault, providers, busy, lead, onError, onStart, onClose }) {
  const [plan, setPlan] = useState(null)
  const place = usePlace(providers, onError)
  const cloud = place.kind === 'cloud'
  const [cloudSize, setCloudSize] = useState(64 * MB)
  const [machineSize, setMachineSize] = useState(0)
  /* A size of one's own, in megabytes, for the limit no preset names. */
  const [custom, setCustom] = useState('')
  const [password, setPassword] = useState('')
  const [again, setAgain] = useState('')

  useEffect(() => {
    let alive = true
    api.snapshotPlan(path, vault)
      .then((p) => { if (alive) { setPlan(p); setCloudSize(p.cloud_default_part_size) } })
      .catch((err) => { if (alive) onError(err.message) })
    return () => { alive = false }
  }, [path, vault, onError])

  if (!plan) return <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>

  const preset = cloud ? cloudSize : machineSize
  const customBytes = Math.round(Number(custom) * MB)
  const partSize = preset === 'custom' ? customBytes : preset
  const max = cloud ? plan.cloud_max_part_size : Infinity
  const sizeProblem = preset !== 'custom' ? null
    : !Number.isFinite(customBytes) || customBytes < plan.min_part_size ? `At least ${plan.min_part_size / MB} MB.`
      : customBytes > max ? `At most ${max / MB} MB on a cloud — a part bound for one is held in memory whole.`
        : null

  const empty = plan.files === 0 && plan.folders === 0
  const mismatch = again !== '' && again !== password
  const ready = !busy && !empty && !plan.unconverted && place.target && !sizeProblem
    && password !== '' && password === again

  return (
    <>
      <p style={para}>{lead}</p>
      <div style={figures}>
        <span style={{ fontFamily: FONT.mono, fontSize: '13px', color: COLORS.text }}>
          {plan.files.toLocaleString()} file{plan.files === 1 ? '' : 's'} in {plan.folders.toLocaleString()} folder{plan.folders === 1 ? '' : 's'}
        </span>
        <span style={mono}>{formatBytes(plan.bytes)} before compression</span>
      </div>
      {empty && <Banner tone="info">This folder is empty, so there is nothing to snapshot.</Banner>}
      {plan.unconverted > 0 && (
        <Banner tone="warn">
          {plan.unconverted.toLocaleString()} file{plan.unconverted === 1 ? '' : 's'} under here {plan.unconverted === 1 ? 'is' : 'are'} still
          in the format SAND used before chunking and cannot be read as a stream. Convert {plan.unconverted === 1 ? 'it' : 'them'} first.
        </Banner>
      )}

      <PlacePicker place={place} providers={providers} verb="Keep it on" />

      <div style={label}>Largest file</div>
      <select
        value={preset}
        onChange={(e) => {
          const v = e.target.value === 'custom' ? 'custom' : Number(e.target.value)
          if (cloud) setCloudSize(v); else setMachineSize(v)
        }}
        aria-label="Largest file"
        style={{ ...select, marginBottom: '8px' }}
      >
        {cloud
          ? CLOUD_SIZES.filter((s) => s <= plan.cloud_max_part_size).map((s) => <option key={s} value={s}>{formatBytes(s)}</option>)
          : MACHINE_SIZES.map((s) => <option key={s.size} value={s.size}>{s.label}</option>)}
        <option value="custom">Another size…</option>
      </select>
      {preset === 'custom' && (
        <Input
          label="Size in MB"
          type="number"
          min={plan.min_part_size / MB}
          value={custom}
          onChange={(e) => setCustom(e.target.value)}
          help={sizeProblem || 'A megabyte here is 1024 × 1024 bytes.'}
        />
      )}
      <p style={{ ...para, color: COLORS.textMuted, fontSize: '12px' }}>
        {partSize > 0
          ? `A snapshot that comes out larger than this is cut into part files of at most ${formatBytes(partSize)} each, numbered in order. They are put back together when it is restored — keep them in the same folder.`
          : 'The snapshot is one file, however big it comes out.'}
      </p>

      <PasswordInput
        label="Password for the snapshot"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        autoComplete="new-password"
      />
      <PasswordInput
        label="The same again"
        value={again}
        onChange={(e) => setAgain(e.target.value)}
        autoComplete="new-password"
        help={mismatch ? 'These two do not match.' : undefined}
      />
      <Banner tone="warn">
        This password is the snapshot’s own, and SAND does not keep it. Whoever holds the snapshot and the password
        has the whole folder, with no vault needed — and without the password nobody has it, you included.
        {cloud ? ' Unlike everything else SAND stores, a snapshot is the whole folder on a single account, so make it a long one.' : ''}
      </Banner>

      <div style={footer}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        <Button
          variant="primary"
          disabled={!ready}
          onClick={() => onStart({ target: place.target, password, part_size: partSize })}
        >Take the snapshot</Button>
      </div>
    </>
  )
}

/* --- Restore ----------------------------------------------------------- */

function RestoreForm({ path, providers, busy, lead, onError, onStart, onClose }) {
  const place = usePlace(providers, onError)
  const [stored, setStored] = useState(null)
  const [loading, setLoading] = useState(false)
  const [picked, setPicked] = useState('')
  const [password, setPassword] = useState('')
  const [replace, setReplace] = useState(false)
  /* What the picked snapshot said it holds, once its password has opened it.
     Starting a restore is offered only after that: a wrong password is then
     said beside the field that was wrong, rather than by a job failing. */
  const [info, setInfo] = useState(null)
  const [wrong, setWrong] = useState(null)
  const [opening, setOpening] = useState(false)
  const [removing, setRemoving] = useState('')

  const { kind, account, machine, dir } = place
  const target = place.target
  const targetKey = target ? JSON.stringify(target) : ''

  const load = useCallback(async () => {
    if (!targetKey) { setStored(null); return }
    setLoading(true)
    try {
      const resp = await api.snapshotsStored(JSON.parse(targetKey))
      setStored(resp.snapshots || [])
    } catch (err) {
      setStored([])
      onError(err.message)
    }
    setLoading(false)
  }, [targetKey, onError])

  /* A cloud is listed the moment it is picked. A machine's folder is typed,
     and listing on every keystroke would be a connection per letter — so it
     waits for the typing to stop. */
  useEffect(() => {
    setStored(null); setPicked(''); setInfo(null); setWrong(null)
    if (!targetKey) return undefined
    const timer = setTimeout(load, kind === 'machine' ? 600 : 0)
    return () => clearTimeout(timer)
  }, [targetKey, kind, account, machine, dir, load])

  useEffect(() => { setInfo(null); setWrong(null) }, [picked, password])

  const open = async () => {
    setOpening(true)
    setWrong(null)
    try {
      setInfo(await api.snapshotInspect(target, picked, password))
    } catch (err) {
      if (err.code === 'SNAPSHOT_PASSWORD') setWrong(err.message)
      else onError(err.message)
    }
    setOpening(false)
  }

  const remove = async (name) => {
    try {
      await api.snapshotDelete(target, name)
      if (picked === name) setPicked('')
      setRemoving('')
      await load()
    } catch (err) {
      onError(err.message)
    }
  }

  const chosen = stored?.find((s) => s.name === picked)
  const here = path === '/' ? 'at the top of the vault' : `in ${path}`

  return (
    <>
      <p style={para}>{lead}</p>
      <PlacePicker place={place} providers={providers} verb="It is kept on" />

      <div style={label}>Snapshots there</div>
      {(loading || (target && stored === null)) && <div style={{ padding: '14px', textAlign: 'center' }}><Spinner size={16} /></div>}
      {!loading && stored && stored.length === 0 && (
        <p style={{ ...para, color: COLORS.textMuted }}>None. A snapshot’s files are named snapshot-…​.sandsnap; if they are in another folder, name it above.</p>
      )}
      {!loading && stored && stored.length > 0 && (
        <div role="radiogroup" aria-label="Snapshots there" style={{ maxHeight: 'calc(var(--app-height) * 0.3)', overflowY: 'auto', marginBottom: '12px' }}>
          {stored.map((s) => (
            <SnapshotRow
              key={s.name}
              set={s}
              on={picked === s.name}
              onPick={() => s.complete && setPicked(s.name)}
              confirming={removing === s.name}
              onRemove={() => setRemoving(s.name)}
              onCancel={() => setRemoving('')}
              onConfirm={() => remove(s.name)}
            />
          ))}
        </div>
      )}

      {chosen && (
        <>
          <PasswordInput
            label="The snapshot’s password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="off"
          />
          {wrong && <p role="alert" style={{ ...para, marginTop: '-8px', color: COLORS.error }}>{wrong}</p>}
          {info && (
            <Banner tone="success">
              A snapshot of {info.path}, taken {new Date(info.taken_at).toLocaleString()}:{' '}
              {info.files.toLocaleString()} file{info.files === 1 ? '' : 's'} in {info.folders.toLocaleString()} folder{info.folders === 1 ? '' : 's'},{' '}
              {formatBytes(info.bytes)}. Its contents will land {here}.
            </Banner>
          )}
          <label style={check}>
            <input type="checkbox" checked={replace} onChange={(e) => setReplace(e.target.checked)} />
            <span>
              Replace files that differ
              <span style={{ display: 'block', color: COLORS.textMuted, fontSize: '12px' }}>
                A file already here with the same contents is always passed over. One with the same name and different
                contents is left alone and listed — unless this is ticked, and then the snapshot’s is written over it.
              </span>
            </span>
          </label>
        </>
      )}

      <div style={footer}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        {info
          ? (
            <Button
              variant="primary"
              disabled={busy}
              onClick={() => onStart({ target, name: picked, password, replace })}
            >Restore {info.files.toLocaleString()} file{info.files === 1 ? '' : 's'} into {path}</Button>
          )
          : (
            <Button variant="primary" disabled={!chosen || password === '' || opening} onClick={open}>
              {opening ? 'Opening…' : 'Open the snapshot'}
            </Button>
          )}
      </div>
    </>
  )
}

function SnapshotRow({ set, on, onPick, confirming, onRemove, onCancel, onConfirm }) {
  const parts = set.parts.length
  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '10px', padding: '9px 8px',
      borderTop: `1px solid ${COLORS.border}`,
      background: on ? `${COLORS.accent}14` : 'transparent',
    }}>
      <button
        type="button"
        role="radio"
        aria-checked={on}
        disabled={!set.complete}
        onClick={onPick}
        style={{
          flex: 1, minWidth: 0, textAlign: 'left', background: 'none', border: 'none', padding: 0,
          cursor: set.complete ? 'pointer' : 'default', color: 'inherit',
        }}
      >
        <div style={{ fontFamily: FONT.sans, fontSize: '13px', color: set.complete ? COLORS.text : COLORS.textMuted }}>
          {new Date(set.taken_at).toLocaleString()}
          <span style={{ ...mono, marginLeft: '10px' }}>
            {formatBytes(set.bytes)} · {parts === 1 ? 'one file' : `${parts} part files`}
          </span>
          {!set.complete && <span style={{ ...mono, marginLeft: '10px', color: COLORS.warn }}>incomplete — parts are missing</span>}
        </div>
        <div style={{ ...mono, fontSize: '11px', wordBreak: 'break-all' }}>{set.name}</div>
      </button>
      {confirming
        ? (
          <>
            <Button size="sm" variant="danger" onClick={onConfirm}>Erase it</Button>
            <Button size="sm" variant="ghost" onClick={onCancel}>Keep</Button>
          </>
        )
        : <Button size="sm" variant="ghost" onClick={onRemove} aria-label={`Erase ${set.name}`}>Erase</Button>}
    </div>
  )
}

/* --- Running and done -------------------------------------------------- */

function Running({ status, mode, busy, onStop, onClose }) {
  const at = status.progress || {}
  /* A job says which it is with its first report, and until then it is the
     one this dialog just started. One at a time between the two: a dialog
     opened while the other kind runs shows that run, and says so. */
  const taking = (at.kind || mode) !== 'restore'
  const other = !!at.kind && at.kind !== mode
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const started = status.started_at ? Date.parse(status.started_at) : NaN
  const elapsed = Number.isFinite(started) ? formatElapsed(now - started) : ''

  let line
  if (at.stage === 'packing') {
    line = `Packing ${at.file} of ${at.files} · ${formatBytes(at.done || 0)} of ${formatBytes(at.bytes || 0)}`
      + ` → ${formatBytes(at.stored || 0)}${at.parts > 1 ? ` in ${at.parts} parts` : ''}`
  } else if (at.stage === 'restoring') {
    const kept = at.kept > 0 ? ` · ${at.kept} left alone` : ''
    const bad = at.failed > 0 ? ` · ${at.failed} failed` : ''
    line = `Restoring ${at.file} of ${at.files} · ${formatBytes(at.done || 0)} of ${formatBytes(at.bytes || 0)}${kept}${bad}`
  } else if (at.stage === 'opening') {
    line = 'Opening the snapshot…'
  } else {
    line = 'Connecting…'
  }

  return (
    <>
      {other && (
        <Banner tone="info">
          A snapshot is already being {taking ? 'taken' : 'restored'} — one job at a time between the two. This is that one.
        </Banner>
      )}
      <div style={{ display: 'flex', justifyContent: 'center', padding: '16px 2px 8px' }}>
        <LoadProgress
          label={line}
          detail={[at.path, elapsed].filter(Boolean).join(' · ')}
          fraction={at.bytes > 0 ? at.done / at.bytes : null}
          width={520}
        />
      </div>
      <p style={{ ...para, textAlign: 'center', color: COLORS.textMuted }}>
        Running on the machine SAND is on. You can close this and come back to it.{' '}
        {taking
          ? 'Stopping throws the snapshot away: what had been written of it is erased.'
          : 'Stopping keeps the files that have arrived, and restoring again carries on from there.'}
      </p>
      <div style={footer}>
        <Button variant="ghost" onClick={onClose}>Close</Button>
        <Button disabled={busy} onClick={onStop}>Stop</Button>
      </div>
    </>
  )
}

function Report({ report, onAgain, onClose }) {
  if (report.kind === 'take') {
    const parts = report.parts || []
    const saved = report.bytes > 0 ? Math.round((1 - report.stored / report.bytes) * 100) : 0
    return (
      <>
        <Banner tone="success">
          {report.files.toLocaleString()} file{report.files === 1 ? '' : 's'} in {report.folders.toLocaleString()} folder{report.folders === 1 ? '' : 's'},{' '}
          {formatBytes(report.bytes)}, packed into {formatBytes(report.stored)}
          {saved > 0 ? ` — ${saved}% smaller` : ''} and kept on {report.where}.
        </Banner>
        <p style={para}>
          {parts.length === 1
            ? 'It is one file:'
            : `It is ${parts.length} part files of at most ${formatBytes(report.part_size)} each. All of them are needed to restore it, in the same folder:`}
        </p>
        <div style={{ maxHeight: 'calc(var(--app-height) * 0.3)', overflowY: 'auto', margin: '0 0 10px' }}>
          {parts.map((name) => (
            <div key={name} style={{ ...mono, fontSize: '11.5px', color: COLORS.textDim, padding: '3px 0', wordBreak: 'break-all' }}>{name}</div>
          ))}
        </div>
        <p style={{ ...para, color: COLORS.textMuted, fontSize: '12px' }}>
          The password is not stored anywhere. Restore it from this menu on any vault, or with <span style={mono}>sand snapshot restore</span>.
        </p>
        <div style={footer}>
          <Button variant="ghost" onClick={onAgain}>Take another</Button>
          <Button variant="primary" onClick={onClose}>Done</Button>
        </div>
      </>
    )
  }

  const tally = [
    [report.restored, 'restored'], [report.skipped, 'already here'],
    [report.kept, 'left alone'], [report.failed, 'failed'],
  ].filter(([n]) => n > 0).map(([n, what]) => `${n.toLocaleString()} ${what}`).join(' · ')
  const clean = !report.stopped && !report.failed && !report.kept
  const lines = report.results || []

  return (
    <>
      {clean
        ? <Banner tone="success">Everything in the snapshot is in {report.path}.</Banner>
        : report.stopped
          ? <Banner tone="warn">Stopped before the end of the snapshot. What arrived is whole; restore it again to carry on.</Banner>
          : <Banner tone="warn">The snapshot was read to its end, and not everything in it was restored.</Banner>}
      <p style={{ ...para, ...mono, fontSize: '12px' }}>{tally || 'Nothing was restored.'}</p>
      {report.of && (
        <p style={{ ...para, color: COLORS.textMuted }}>
          From the snapshot of {report.of.path} taken {new Date(report.of.taken_at).toLocaleString()}, kept on {report.where}.
        </p>
      )}
      <div style={{ maxHeight: 'calc(var(--app-height) * 0.35)', overflowY: 'auto', margin: '10px 0' }}>
        {lines.map((l) => (
          <div key={l.path} style={{ padding: '8px 0', borderTop: `1px solid ${COLORS.border}` }}>
            <div style={{ ...mono, fontSize: '12.5px', color: COLORS.text, wordBreak: 'break-all' }}>{l.path}</div>
            <div style={{ ...detail, color: l.error ? COLORS.error : COLORS.textMuted }}>{l.error || l.reason}</div>
          </div>
        ))}
        {report.omitted > 0 && <p style={para}>…and {report.omitted.toLocaleString()} more.</p>}
      </div>
      <div style={footer}>
        <Button variant="ghost" onClick={onAgain}>Restore another</Button>
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
  marginTop: '4px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.55, wordBreak: 'break-word',
}
const label = {
  fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
  textTransform: 'uppercase', color: COLORS.textMuted, marginBottom: '6px',
}
const select = {
  width: '100%', minHeight: '38px', padding: '0 10px', borderRadius: '6px',
  background: COLORS.bg, color: COLORS.text, border: `1px solid ${COLORS.border}`,
  fontFamily: FONT.mono, fontSize: '12.5px',
}
const figures = {
  display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: '10px',
  flexWrap: 'wrap', margin: '4px 0 14px',
}
const check = {
  display: 'flex', gap: '10px', alignItems: 'flex-start', margin: '4px 0 12px',
  fontFamily: FONT.sans, fontSize: '13px', lineHeight: 1.5, color: COLORS.text, cursor: 'pointer',
}
const footer = { display: 'flex', justifyContent: 'flex-end', gap: '8px', marginTop: '14px', flexWrap: 'wrap' }
