import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { downloadFile } from '../download'
import { useBatchEraseProgress, useEraseProgress } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'

/* What a handful of picked rows can be told to do at once.

   All of it is a loop over things the vault already does one at a time, run
   from the browser rather than added to the API. That is deliberate: erasing
   twelve files is twelve deletions whichever end of the wire the loop is on,
   and keeping it here means a stall on the fourth one is visible, the three
   that already went are still gone, and nothing needs a new endpoint that
   could half-succeed with no way to say which half.

   Hence one file at a time rather than all at once: a dozen parallel deletions
   against the same account is a way to be rate-limited, and a dozen parallel
   rebuilds is a way to run a Raspberry Pi out of memory. */

/* A run of one-at-a-time operations, with somewhere to say how far it has got.
   Left in whatever state it finished in — a partial failure is a normal
   outcome here and worth reading, not something to close over.

   What it runs over is taken once, when the dialog opens, and never read
   again. A finished run clears the selection it came from, and a dialog still
   reading that selection would answer "0 deleted" the instant it had deleted
   twelve things — the batch is what was picked, not what is picked now. */
export function useRun(chosen, perform, onFinished) {
  const [items] = useState(() => chosen)
  const [at, setAt] = useState(-1)
  const [done, setDone] = useState(null)
  /* A dialog closed halfway through must not leave a loop writing into a
     component that is no longer on screen. Set on the way in as well as
     cleared on the way out: React's strict mode mounts, unmounts and mounts
     again, and a flag only ever cleared would stay cleared for the mount that
     actually runs. */
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => { live.current = false }
  }, [])

  const running = at >= 0 && !done

  const start = async () => {
    const failures = []
    const warnings = []

    for (let i = 0; i < items.length; i++) {
      if (!live.current) return
      setAt(i)
      try {
        const notes = await perform(items[i])
        if (notes?.length) warnings.push(...notes.map((w) => `${items[i].name}: ${w}`))
      } catch (err) {
        failures.push(`${items[i].name}: ${err.message}`)
      }
    }

    if (!live.current) return
    setDone({ failures, warnings })
    onFinished?.()
  }

  return { items, at, running, done, start }
}

export function Progress({ items, at, verb, note }) {
  return <Meter count={at + 1} total={items.length} verb={verb} label={items[at]?.name} note={note} />
}

/* The bar itself: "Deleting 412 of 7364", a label for what it is standing on,
   and a note beside it. Progress above counts items; a batch delete counts
   files through it directly, because its unit of work is not its unit of
   progress. */
export function Meter({ count, total, verb, label, note }) {
  return (
    <div style={{ marginBottom: '16px' }}>
      <div style={{
        display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px',
        fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim,
      }}>
        <Spinner size={11} />
        <span>{verb} {count} of {total}</span>
        <span style={{
          flex: 1, minWidth: 0, color: COLORS.textMuted,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{label}</span>
        {/* Inside the item, when the item is itself a slow plural — a folder
            being deleted counts its files here, so the bar below standing
            still on one item for minutes has a number that is moving. */}
        {note && <span style={{ flexShrink: 0, color: COLORS.textMuted }}>{note}</span>}
      </div>
      <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden' }}>
        <div style={{
          height: '100%',
          width: `${Math.max(4, (count / Math.max(1, total)) * 100)}%`,
          background: COLORS.accent,
          transition: 'width 0.2s ease',
        }} />
      </div>
    </div>
  )
}

function Outcome({ done, total, verb }) {
  // A batch delete fails by the hundred under one line, so it says how many.
  const failed = done.failed ?? done.failures.length

  return (
    <>
      <Banner tone={failed ? 'warn' : 'success'}>
        {failed
          ? `${total - failed} of ${total} ${verb}. The rest are untouched — try them again once the accounts are answering.`
          : `${total} ${verb}.`}
      </Banner>
      {(done.failures.length > 0 || done.warnings.length > 0) && (
        <div style={{ maxHeight: '180px', overflowY: 'auto', marginBottom: '4px' }}>
          {done.failures.length > 0 && (
            <Banner tone="error">{done.failures.map((f, i) => <div key={i}>{f}</div>)}</Banner>
          )}
          {done.warnings.length > 0 && (
            <Banner tone="warn">{done.warnings.map((w, i) => <div key={i}>{w}</div>)}</Banner>
          )}
        </div>
      )}
    </>
  )
}

/* Erasing everything picked. A folder takes what is inside it, which is why
   the dialog counts the two kinds separately rather than saying "12 items" and
   leaving the folders to be discovered afterwards.

   Files do not go one request each. Deleting a file is a round of erasures
   bounded by the slowest account and then the whole index re-sealed and
   written, and seven thousand duplicates ticked in one go would pay both
   seven thousand times over. They go a few hundred to a request instead
   (api.deleteFiles), each request one index write, with the server erasing a
   few files abreast — the same pace a folder delete already runs at — and
   counting itself down through the same window. A folder is still a request
   of its own: the server takes its contents in one write already.

   Batches rather than one request for everything, so the bar moves, no single
   request outlives its timeout, and a dialog closed part-way through stops
   at the next batch rather than after all of them. */
export function BulkDelete({ items, vault = '', onClose, onDone }) {
  /* Taken once, when the dialog opens, for the reason useRun gives: a
     finished run clears the selection it came from, and a dialog still
     reading it would say "0 deleted" over the two things it just deleted. */
  const [batch] = useState(() => items)
  const [steps] = useState(() => planDeletes(batch))
  const run = useDeletes(steps, vault, onDone)

  const folders = batch.filter((i) => i.kind === 'folder')
  const files = batch.filter((i) => i.kind !== 'folder')
  const bytes = files.reduce((sum, f) => sum + (f.file.size || 0), 0)

  /* What the step in flight has done so far, read beside its request: a
     folder counts its files, a batch counts the files erased out of its
     own. Either way the bar below moves while one request runs for
     minutes. */
  const current = run.running ? steps[run.at] : null
  const folderInFlight = Boolean(current?.kind === 'folder')
  const batchInFlight = Boolean(current?.kind === 'files')
  const erasingFolder = useEraseProgress(folderInFlight ? current.path : '', vault, folderInFlight)
  const erasingBatch = useBatchEraseProgress(batchInFlight ? current.batch : '', batchInFlight)

  const close = () => { if (!run.running) onClose() }

  return (
    <Modal
      title={`Delete ${batch.length} item${batch.length === 1 ? '' : 's'}?`}
      subtitle={describe(folders.length, files.length, bytes)}
      onClose={run.running ? undefined : close}
      width={460}
    >
      {run.done ? (
        <>
          <Outcome done={run.done} total={batch.length} verb="deleted" />
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button variant="primary" onClick={close}>Done</Button>
          </div>
        </>
      ) : run.running ? (
        <Meter
          count={run.finished + (erasingBatch?.done || 0)}
          total={batch.length}
          verb="Deleting"
          label={folderInFlight ? current.name : ''}
          note={erasingFolder ? `${erasingFolder.done} of ${erasingFolder.total} files` : ''}
        />
      ) : (
        <>
          <div style={{
            marginBottom: '18px', fontFamily: FONT.sans, fontSize: '12.5px',
            lineHeight: 1.6, color: COLORS.textDim,
          }}>
            Every part of every file goes, erased from each account holding it.
            {folders.length > 0 && ' A folder takes everything inside it, however deep.'}
            {' '}This cannot be undone.
          </div>
          <div style={{ display: 'flex', gap: '10px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            <Button variant="danger" onClick={run.start}>Delete {batch.length}</Button>
            <Button variant="ghost" onClick={close}>Cancel</Button>
          </div>
        </>
      )}
    </Modal>
  )
}

/* How many files go in one request. Enough that a few thousand is a few
   dozen index writes rather than a few thousand; few enough that a request
   answers within minutes on slow accounts and a closed dialog does not have
   long to wait. */
export const DELETE_BATCH = 200

/* The requests a selection turns into: files a batch at a time, then each
   folder on its own. Files first, so a file ticked inside a folder that is
   also ticked is counted once as deleted rather than once as already gone.
   Every step says how many of the items it stands for, which is what the
   bar and the outcome count in. */
export function planDeletes(items) {
  const steps = []
  const files = items.filter((i) => i.kind !== 'folder')
  for (let i = 0; i < files.length; i += DELETE_BATCH) {
    const slice = files.slice(i, i + DELETE_BATCH)
    steps.push({
      kind: 'files',
      ids: slice.map((f) => f.file.id),
      count: slice.length,
      name: `${slice.length} file${slice.length === 1 ? '' : 's'}`,
      batch: `${Date.now().toString(36)}-${i}-${Math.random().toString(36).slice(2, 8)}`,
    })
  }
  for (const item of items) {
    if (item.kind !== 'folder') continue
    steps.push({ kind: 'folder', path: item.path, count: 1, name: item.name })
  }
  return steps
}

/* Running the plan, one request at a time, and keeping count in items rather
   than requests: `finished` is how many items the completed steps stood for,
   and the outcome says how many failed, however many lines that took.
   Left in whatever state it finished in, like useRun. */
function useDeletes(steps, vault, onFinished) {
  const [at, setAt] = useState(-1)
  const [finished, setFinished] = useState(0)
  const [done, setDone] = useState(null)
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => { live.current = false }
  }, [])

  const running = at >= 0 && !done

  const start = async () => {
    const failures = []
    const warnings = []
    let failed = 0
    let gone = 0
    let counted = 0

    for (let i = 0; i < steps.length; i++) {
      if (!live.current) return
      const step = steps[i]
      setAt(i)
      try {
        if (step.kind === 'folder') {
          const resp = await api.deleteFolder(step.path, true, vault)
          for (const w of resp?.warnings || []) warnings.push(`${step.name}: ${w}`)
        } else {
          const resp = await api.deleteFiles(step.ids, step.batch)
          warnings.push(...(resp?.warnings || []))
          gone += resp?.missing?.length || 0
        }
      } catch (err) {
        failures.push(`${step.name}: ${err.message}`)
        failed += step.count
      }
      counted += step.count
      setFinished(counted)
    }

    if (gone > 0) {
      warnings.push(`${gone} file${gone === 1 ? ' was' : 's were'} already gone before this reached ${gone === 1 ? 'it' : 'them'}.`)
    }
    if (!live.current) return
    setDone({ failures, warnings, failed })
    onFinished?.()
  }

  return { at, finished, running, done, start }
}

function describe(folders, files, bytes) {
  const parts = []
  if (folders) parts.push(`${folders} folder${folders === 1 ? '' : 's'}`)
  if (files) parts.push(`${files} file${files === 1 ? '' : 's'}, ${formatBytes(bytes)}`)
  return parts.join(' and ')
}

/* Handing several files back at once.

   Each one is gathered from its accounts and rebuilt before it can be saved,
   so this is minutes rather than seconds on anything large — and it starts the
   moment the dialog opens, because picking the files and pressing Download was
   already the decision. Nothing navigates: every file is saved as a blob under
   its own name, the way a single download is, so a vault opened from a phone's
   home screen stays where it is. */
export function BulkDownload({ items, onClose }) {
  // Folders are not rebuilt as one thing, so they are simply not part of this.
  const run = useRun(items.filter((i) => i.kind !== 'folder'),
    async (item) => { await downloadFile(item.file); return null })
  const files = run.items

  // Started here rather than behind a button: the dialog only opens because
  // Download was pressed, and asking twice for the same thing is a step.
  const started = useRef(false)
  useEffect(() => {
    if (started.current) return
    started.current = true
    run.start()
    // The run is stable for the life of the dialog; re-running it on a render
    // would download everything twice.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const bytes = files.reduce((sum, f) => sum + (f.file.size || 0), 0)

  return (
    <Modal
      title={`Download ${files.length} file${files.length === 1 ? '' : 's'}`}
      subtitle={`${formatBytes(bytes)} rebuilt one at a time — the parts of each are gathered from your accounts first`}
      onClose={run.done ? onClose : undefined}
      width={460}
    >
      {run.done ? (
        <>
          <Outcome done={run.done} total={files.length} verb="downloaded" />
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button variant="primary" onClick={onClose}>Done</Button>
          </div>
        </>
      ) : (
        <>
          <Progress items={files} at={Math.max(0, run.at)} verb="Rebuilding" />
          <p style={{
            margin: 0, fontFamily: FONT.sans, fontSize: '11.5px',
            color: COLORS.textMuted, lineHeight: 1.6,
          }}>
            Your browser may ask whether to allow several downloads at once. Leaving this dialog
            open is what keeps them coming.
          </p>
        </>
      )}
    </Modal>
  )
}

/* Moving a whole selection into a sub vault, or back out of one.

   Each item goes across on its own, so a failure part-way leaves the rest where
   they were rather than half a folder in each place — and the dialog says which
   ones landed. The move itself is an index change per item; nothing travels
   between clouds. */
export function BulkAssign({ items, from, targets, onClose, onDone, onError }) {
  const [to, setTo] = useState(targets[0]?.id ?? '')
  const [busy, setBusy] = useState(false)
  const [at, setAt] = useState(-1)
  const [done, setDone] = useState(null)

  const destination = targets.find((t) => t.id === to)

  const run = async () => {
    setBusy(true)
    const failed = []
    const warnings = []

    for (let i = 0; i < items.length; i++) {
      setAt(i)
      const entry = items[i]
      const target = entry.kind === 'folder' ? entry.path : entry.file.id
      try {
        const report = await api.assign({ target, from, to })
        if (report.warnings?.length) warnings.push(...report.warnings)
      } catch (err) {
        failed.push(`${entry.name}: ${err.message}`)
      }
    }

    if (warnings.length) onError(warnings.join('\n'))
    if (failed.length) onError(failed.join('\n'))
    // Once, after the whole batch, rather than per item: it walks the whole
    // destination either way.
    api.migrateVault(to).catch(() => {})
    setDone(items.length - failed.length)
    setBusy(false)
  }

  if (done !== null) {
    onDone()
    return null
  }

  return (
    <Modal
      title={from ? `Take ${items.length} item${items.length === 1 ? '' : 's'} out` : `Move ${items.length} item${items.length === 1 ? '' : 's'} into a sub vault`}
      onClose={busy ? undefined : onClose}
      width={460}
    >
      {busy ? (
        <Progress items={items} at={Math.max(0, at)} verb="Moving" />
      ) : (
        <>
          <p style={{
            margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '11.5px',
            color: COLORS.textMuted, lineHeight: 1.6,
          }}>
            The paths are kept and nothing travels between your clouds — this is an
            index change. Each is re-encrypted onto {destination?.label || 'the destination'}’s
            own key afterwards; until that finishes, the vault it is leaving can
            still read it.
          </p>

          {targets.length > 1 && (
            <select
              value={to}
              onChange={(e) => setTo(e.target.value)}
              style={{
                width: '100%', padding: '10px 12px', marginBottom: '14px',
                background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
                color: COLORS.text, fontFamily: FONT.mono, fontSize: '13px',
              }}
            >
              {targets.map((t) => <option key={t.id} value={t.id}>{t.label}</option>)}
            </select>
          )}

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
            <Button variant="primary" onClick={run}>Move</Button>
          </div>
        </>
      )}
    </Modal>
  )
}
